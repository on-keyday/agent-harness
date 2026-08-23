package agentboard

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

type Config struct {
	RingN      int
	TopicTTL   time.Duration
	MaxTopics  int
	MaxPayload int
	// SeqSeed is the starting value for the board-global publish sequence
	// counter (b.seq). It exists to keep seq monotonic ACROSS server
	// restarts: b.seq lives only in memory, so a bare restart would reset it
	// to 0 and re-issue low seqs — but consumer `--since-last` cursors are
	// persisted on disk (~/.cache/harness/agent-cursor-<task>) and survive
	// the restart. A cursor left above the post-restart seq range then
	// filters out every new message (seq > cursor is false), silently
	// wedging the auto-inbox hook. The server seeds this with a
	// strictly-increasing boot epoch (wall-clock ms << 20) so every restart
	// begins in a higher range than any prior boot's cursors. Zero (the
	// default, used by tests) preserves the legacy seq=1,2,3… behavior.
	SeqSeed uint64
}

type Board struct {
	cfg     Config
	mu      sync.Mutex
	topics  map[string]*topic
	tasks   map[ticketKey]*taskState // per-(runner_id, task_id) persistent state
	seq     atomic.Uint64
	reg     *registry
	stopCh  chan struct{}
	stopped bool

	// onDeliver, if non-nil, is invoked once per subscriber that Send delivers
	// to (i.e. once per (rid, tid) whose subscription set matches the
	// published topic). Used by the server to emit task_wake to the runners
	// hosting those tasks. Called outside b.mu.
	onDeliver func(protocol.RunnerID, protocol.TaskID)
}

func New(cfg Config) *Board {
	b := &Board{
		cfg:    cfg,
		topics: make(map[string]*topic),
		tasks:  make(map[ticketKey]*taskState),
		reg:    newRegistry(),
		stopCh: make(chan struct{}),
	}
	// Seed the publish sequence so it stays monotonic across restarts; see
	// Config.SeqSeed. The first published message gets SeqSeed+1 (b.seq.Add(1)).
	b.seq.Store(cfg.SeqSeed)
	go b.evictLoop()
	return b
}

func (b *Board) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped {
		return
	}
	b.stopped = true
	close(b.stopCh)
}

// Registry returns the ticket registry for server lifecycle code (TryDispatch / TaskFinished).
func (b *Board) Registry() *registry { return b.reg }

// RegisterTask stores the task's auth ticket and seeds its id-directed inbound
// topic. This is the server-side equivalent of the historical
// `harness-cli agent subscribe --self` SessionStart hook, but it runs when the
// task is assigned to a runner, before any runtime-specific agent hook exists.
func (b *Board) RegisterTask(rid protocol.RunnerID, tid protocol.TaskID, ticket [16]byte, agentProfile string) {
	b.reg.Register(rid, tid, ticket)
	key := ticketKey{runner: runnerIDStringProto(rid), task: hexTaskIDProto(tid)}
	b.mu.Lock()
	ts, ok := b.tasks[key]
	if !ok {
		ts = newTaskState()
		b.tasks[key] = ts
	}
	b.mu.Unlock()
	ts.setIdentity(rid, tid, "", agentProfile)
	ts.addPattern(SelfTopic(tid))
}

// SetOnDeliver registers a callback fired once per matched subscriber
// after Send has appended the message to the topic ring. Non-blocking
// expected; runs on the publisher's goroutine. Safe to call once at
// startup before any Send.
func (b *Board) SetOnDeliver(fn func(protocol.RunnerID, protocol.TaskID)) {
	b.mu.Lock()
	b.onDeliver = fn
	b.mu.Unlock()
}

// Attach is called from the agent_message Hello handler after Validate(rid, tid, ticket)
// returns Ok. It returns a ConnState bound to the (rid, tid) taskState, lazy-creating
// the taskState if this is the first agent connecting under that ticket. hostname
// and agentProfile are captured into the taskState so Board.Send can attach sender
// attestation to every message published by this (rid, tid). agentProfile is the
// server-resolved agent profile for the task; empty means "not attributed".
func (b *Board) Attach(rid RunnerID, tid TaskID, hostname, agentProfile string) *ConnState {
	key := ticketKey{runner: runnerIDStringBoard(rid), task: hexTaskIDBoard(tid)}
	b.mu.Lock()
	ts, ok := b.tasks[key]
	if !ok {
		ts = newTaskState()
		b.tasks[key] = ts
	}
	b.mu.Unlock()
	// Convert agentboard.RunnerID / TaskID → protocol.RunnerID / TaskID for identity storage.
	var protoRid protocol.RunnerID
	protoRid.SetTransport(rid.Transport)
	protoRid.SetIpAddr(rid.IpAddr)
	protoRid.Port = rid.Port
	protoRid.UniqueNumber = rid.UniqueNumber
	var protoTid protocol.TaskID
	copy(protoTid.Id[:], tid.Id[:])
	ts.setIdentity(protoRid, protoTid, hostname, agentProfile)
	c := newConnState(ts)
	ts.attachConn(c)
	return c
}

// Detach removes a ConnState from its taskState's attached set. The taskState
// itself is preserved so subscriptions survive across reconnects; it is only
// destroyed by Revoke (TaskFinished).
func (b *Board) Detach(c *ConnState) {
	if c == nil || c.task == nil {
		return
	}
	c.task.detachConn(c)
}

// Revoke removes the ticket and destroys the (rid, tid) taskState. Called by the
// server runner_handler on TaskFinished and by dispatch on send-failure rollback.
// Topics that were exclusively subscribed by this task are deleted immediately
// rather than waiting for TTL eviction.
func (b *Board) Revoke(rid protocol.RunnerID, tid protocol.TaskID) {
	b.reg.Revoke(rid, tid)
	key := ticketKey{runner: runnerIDStringProto(rid), task: hexTaskIDProto(tid)}
	b.mu.Lock()
	ts := b.tasks[key]
	delete(b.tasks, key)
	if ts != nil {
		for _, p := range ts.snapshotPatterns() {
			t, ok := b.topics[p]
			if !ok || b.anyTaskMatchesLocked(p) {
				continue
			}
			// A topic still holding WITHDRAWN messages outlives its last
			// subscriber. Otherwise the audit trail retract exists to preserve
			// would vanish at exactly the moment it matters most: a worker's
			// chat.<short-id> is subscribed by that one task, so finishing the
			// task destroyed every instruction it had retracted — including the
			// ones an operator had not read yet. TTL still takes it, which is
			// the bound the operator already lives with.
			//
			// Lock order is b.mu -> t.mu; no topic method reaches back into the
			// board, so the nesting cannot invert.
			if t.hasRetracted() {
				continue
			}
			delete(b.topics, p)
		}
	}
	b.mu.Unlock()
}

// anyTaskMatchesLocked returns true if at least one taskState in b.tasks subscribes
// to topic. Must be called with b.mu held.
func (b *Board) anyTaskMatchesLocked(topic string) bool {
	for _, ts := range b.tasks {
		if ts.matches(topic) {
			return true
		}
	}
	return false
}

// Subscribe records a topic pattern in the taskState shared by all ConnStates
// of the same (rid, tid). Persists across reconnects until Revoke.
func (b *Board) Subscribe(c *ConnState, pattern string) error {
	if pattern == "" {
		return errors.New("empty pattern")
	}
	if c == nil || c.task == nil {
		return errors.New("not attached")
	}
	c.task.addPattern(pattern)
	return nil
}

func (b *Board) Unsubscribe(c *ConnState, pattern string) {
	if c == nil || c.task == nil {
		return
	}
	c.task.removePattern(pattern)
}

var (
	ErrPayloadTooLarge = errors.New("agentboard: payload too large")
	ErrTooManyTopics   = errors.New("agentboard: too many topics")
)

// Send appends a message to topicName attributed to the given
// (rid, tid, hostname, agentProfile). The caller (server agent_handler) is
// responsible for passing the *authenticated* sender — taken from the calling
// ConnState's taskState — so agents cannot spoof the from_* fields. fromProfile is
// frozen into the ring entry rather than resolved on read, because a task id can be
// resumed under a different agent profile while its topic keeps the older messages.
//
// inReplyTo is the parent message's seq, or 0. Send does NOT validate it —
// resolution is the caller's job (server/agent_handler.go, via LookupSeq),
// because rejecting a send is a protocol-level decision and the board is the
// storage layer.
// MaxPayload is the per-message byte limit Send enforces. Exposed so the
// transport layer can stop reading an over-long body instead of buffering it
// whole and having Send reject it afterwards.
func (b *Board) MaxPayload() int { return b.cfg.MaxPayload }

// Send publishes to topicName and returns the assigned seq plus deliveredTo —
// how many subscribers matched. The count is built here anyway (targets below)
// and used to be discarded, which left every caller unable to distinguish a
// delivered publish from one into a topic nobody holds.
func (b *Board) Send(topicName string, payload []byte, fromRid protocol.RunnerID, fromTid protocol.TaskID, fromHost, fromProfile string, inReplyTo uint64, opts ...SendOption) (uint64, int, error) {
	if len(payload) > b.cfg.MaxPayload {
		return 0, 0, ErrPayloadTooLarge
	}
	var cfg sendConfig
	for _, o := range opts {
		o(&cfg)
	}
	b.mu.Lock()
	t, ok := b.topics[topicName]
	if !ok {
		if len(b.topics) >= b.cfg.MaxTopics {
			b.evictOldestTopicLocked()
			if len(b.topics) >= b.cfg.MaxTopics {
				b.mu.Unlock()
				return 0, 0, ErrTooManyTopics
			}
		}
		t = newTopic(topicName, b.cfg.RingN)
		b.topics[topicName] = t
	}
	targets := make([]*taskState, 0)
	for _, ts := range b.tasks {
		if ts.matches(topicName) {
			targets = append(targets, ts)
		}
	}
	b.mu.Unlock()

	seq := b.seq.Add(1)
	t.append(seq, payload, fromRid, fromTid, fromHost, fromProfile, inReplyTo, cfg)

	b.mu.Lock()
	fn := b.onDeliver
	b.mu.Unlock()
	// Subscription is the opt-in: every matching subscriber is pinged AND
	// gets onDeliver (hence a task_wake) — the publisher's own taskState
	// included, so a send to one's own chat.<short-id> is a working
	// self-ping. This also makes the (rid, tid)-keyed publisher case
	// consistent with publishes that self-wake by construction anyway:
	// server-originated ones (await-idle) carry a placeholder RunnerID and
	// never matched a publisher skip. Loop pressure is bounded elsewhere —
	// the runner debounces wake injections per task (wakeDebounceWindow),
	// and agents are told to subscribe only to topics they receive on.
	for _, ts := range targets {
		for _, c := range ts.snapshotConns() {
			c.ping()
		}
		if fn != nil {
			rid, tid, _, _ := ts.identity()
			fn(rid, tid)
		}
	}
	return seq, len(targets), nil
}

// Inbox returns retained messages for all topics the (rid, tid) taskState is
// subscribed to, with Seq > since, plus the new cursor (max seq seen, or
// since if none).
func (b *Board) Inbox(c *ConnState, since uint64) ([]RetainedMessage, uint64) {
	if c == nil || c.task == nil {
		return nil, since
	}
	patterns := c.task.snapshotPatterns()

	b.mu.Lock()
	all := make([]RetainedMessage, 0)
	for _, p := range patterns {
		if t, ok := b.topics[p]; ok {
			all = append(all, t.since(since)...)
		}
	}
	b.mu.Unlock()

	max := since
	for _, m := range all {
		if m.Seq > max {
			max = m.Seq
		}
	}
	return all, max
}

// Wait blocks until at least one message arrives on topicName with seq > since,
// or until ctx is done. Returns (messages, timedOut, error).
//
// For the duration of the call the topic is subscribed: beginWait/endWait
// refcount it and taskState.matches is the union of the persistent pattern set
// and the topics under a live wait. A wait therefore leaves no subscription
// behind, and does not remove one the task already held. It used to call
// addPattern and never undo it, so one wait on a peer's chat.<short-id>
// subscribed the task to it for the rest of that task's life.
func (b *Board) Wait(ctx context.Context, c *ConnState, topicName string, since uint64) ([]RetainedMessage, bool, error) {
	if c == nil || c.task == nil {
		return nil, false, errors.New("not attached")
	}
	c.task.beginWait(topicName)
	defer c.task.endWait(topicName)
	for {
		b.mu.Lock()
		var msgs []RetainedMessage
		if t, ok := b.topics[topicName]; ok {
			msgs = t.since(since)
		}
		b.mu.Unlock()
		if len(msgs) > 0 {
			return msgs, false, nil
		}
		select {
		case <-c.notify:
			continue
		case <-ctx.Done():
			return nil, true, nil
		case <-b.stopCh:
			return nil, false, errors.New("board closed")
		}
	}
}

func (b *Board) evictLoop() {
	interval := b.cfg.TopicTTL / 6
	if interval <= 0 {
		interval = time.Minute
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-b.stopCh:
			return
		case <-tick.C:
			b.evictExpiredTopics()
		}
	}
}

func (b *Board) evictExpiredTopics() {
	cutoff := time.Now().Add(-b.cfg.TopicTTL)
	b.mu.Lock()
	defer b.mu.Unlock()
	for name, t := range b.topics {
		// NOTE: t.lastPublishedAt is read here without t.mu held (b.mu is held instead).
		// topic.append writes lastPublishedAt under t.mu. This is a known approximate-read
		// v1 race: a torn timestamp read at worst delays eviction by one tick. Not a
		// correctness issue; acceptable for v1.
		if t.lastPublishedAt.Before(cutoff) {
			delete(b.topics, name)
		}
	}
}

// evictOldestTopicLocked frees a slot when MaxTopics is reached. Unlike Revoke
// it does NOT spare topics holding withdrawn messages: this runs under capacity
// pressure, where something has to go, and a rule that can refuse every
// candidate would turn a full board into a permanent publish failure.
func (b *Board) evictOldestTopicLocked() {
	var oldestName string
	var oldestT time.Time
	for n, t := range b.topics {
		// Same approximate-read caveat as evictExpiredTopics above.
		if oldestName == "" || t.lastPublishedAt.Before(oldestT) {
			oldestName, oldestT = n, t.lastPublishedAt
		}
	}
	if oldestName != "" {
		delete(b.topics, oldestName)
	}
}

// PurgeTopic destroys a single topic's retained-message ring, removing the
// topic from the board entirely (the same operation as TTL / Revoke eviction).
// Returns the number of retained messages dropped and whether the topic
// existed. Subscriptions live on each taskState's pattern set, not on the
// topic, so a later publish recreates the topic fresh; the seq counter is
// board-global (b.seq), so it is unaffected by deletion and consumer cursors
// stay valid across a purge (a post-purge message gets a strictly higher seq).
func (b *Board) PurgeTopic(name string) (purged int, found bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.topics[name]
	if !ok {
		return 0, false
	}
	_, _, n := t.summary()
	delete(b.topics, name)
	return n, true
}

// PurgeSeq drops a single retained message (by seq) from a topic's ring,
// leaving every other message and the topic itself intact. found reports
// whether the topic existed; removed reports whether a message with that seq
// was present and dropped. A topic left empty is harmless — it TTL-evicts like
// any quiet topic.
func (b *Board) PurgeSeq(name string, seq uint64) (removed, found bool) {
	b.mu.Lock()
	t, ok := b.topics[name]
	b.mu.Unlock()
	if !ok {
		return false, false
	}
	return t.removeSeq(seq), true
}

// ListRetained returns metadata for every message in a topic's ring (no
// payload bytes are surfaced by callers). found reports whether the topic
// exists. This is the content-blind targeting step for PurgeSeq.
func (b *Board) ListRetained(name string) (msgs []RetainedMessage, found bool) {
	b.mu.Lock()
	t, ok := b.topics[name]
	b.mu.Unlock()
	if !ok {
		return nil, false
	}
	return t.snapshot(), true
}

// SendOption records a per-publish choice that is not part of the message
// body. They are options rather than more positional parameters because Send
// already takes seven and is called from ~50 places, nearly all of them tests
// that have no opinion about any of this.
type SendOption func(*sendConfig)

type sendConfig struct {
	noRetireOnReply bool
}

// NoRetireOnReply marks the message as one that must survive being answered:
// the reply-retire rule (see Server.retireRepliedParent) will skip it. The
// option is the negative because the DEFAULT is to retire — a caller that
// passes no options gets the behaviour that makes a spent instruction go away
// on its own.
func NoRetireOnReply() SendOption {
	return func(c *sendConfig) { c.noRetireOnReply = true }
}

// ListRetracted returns the topic's withdrawn messages — the ones an author
// retracted. found reports whether the topic exists.
//
// It is a separate call from ListRetained rather than a flag on it because the
// board is the storage layer and deciding who may see a message is a
// protocol-level call, the same split Retained and Send already use. The
// operator handlers ask for both and merge; every agent-facing handler asks
// for neither, and gets the live ring through ListRetained.
func (b *Board) ListRetracted(name string) (msgs []RetainedMessage, found bool) {
	b.mu.Lock()
	t, ok := b.topics[name]
	b.mu.Unlock()
	if !ok {
		return nil, false
	}
	return t.snapshotRetracted(), true
}

// RetractedCount reports how many withdrawn messages a topic holds. found
// reports whether the topic exists.
func (b *Board) RetractedCount(name string) (n int, found bool) {
	b.mu.Lock()
	t, ok := b.topics[name]
	b.mu.Unlock()
	if !ok {
		return 0, false
	}
	return t.retractedCount(), true
}

// RetractSeq withdraws the message with this seq when by published it: the
// entry leaves the live ring — and so every agent-facing read — and moves to
// the topic's withdrawn list, where only the operator surfaces look.
//
// It returns the topic the message was on, for logging. ok is false when no
// live ring holds that seq (never published, rotated out, already withdrawn,
// purged) AND when the seq belongs to somebody else; callers must answer both
// with the same status, or the reply confirms the existence of any seq on any
// ring the caller cannot name.
//
// Like LookupSeq this is a full scan of every ring, and for the same reason: a
// seq -> topic index would have to be invalidated in seven places now, and a
// missed one desynchronizes it intermittently. See LookupSeq for the cost
// bound.
func (b *Board) RetractSeq(seq uint64, by protocol.TaskID) (topicName string, ok bool) {
	if seq == 0 || by.Id == ([16]byte{}) {
		return "", false
	}
	now := time.Now()
	b.mu.Lock()
	names := make([]string, 0, len(b.topics))
	tps := make([]*topic, 0, len(b.topics))
	for n, t := range b.topics {
		names = append(names, n)
		tps = append(tps, t)
	}
	b.mu.Unlock()

	for i, t := range tps {
		if t.retract(seq, by, now) {
			return names[i], true
		}
	}
	return "", false
}

// LookupSeq resolves a published seq to the topic whose ring still retains it
// and the task that published it. ok is false for seq 0, for a seq that was
// never published, and for one whose entry has since left its ring (count
// overflow, TTL eviction, purge, or Revoke) — the three are indistinguishable
// and are treated alike: the message is not on the board.
//
// This is a full scan of every ring, deliberately: the alternative is a
// seq -> topic index that must be invalidated in six places (ring overflow in
// topic.append, removeSeq, PurgeTopic, PurgeSeq, both evict paths, and the
// topic deletion in Revoke), and a missed one desynchronizes it from the rings
// intermittently. The scan holds no derived state so it cannot desynchronize.
// Cost is bounded by MaxTopics * RingN — 1024 * 64 with the shipped defaults —
// against a publish rate driven by agent turns. Raising either bound by an
// order of magnitude is the trigger to reconsider the index.
//
// A message evicted between the snapshot and its ring's scan is missed,
// yielding a spurious "not found". That is the same approximate-read tradeoff
// already accepted in evictExpiredTopics.
// Subscribes reports whether the (rid, tid) behind c has topicName in its
// subscription set. It is the scope check Retained's callers owe: subscribing
// requires knowing a topic NAME, and that requirement — not a capability — is
// what keeps the rings from being browsable, so a lookup keyed on a global
// consecutive seq has to be re-narrowed to the same set.
func (b *Board) Subscribes(c *ConnState, topicName string) bool {
	if c == nil || c.task == nil {
		return false
	}
	return c.task.matches(topicName)
}

// Retained returns the retained message with this seq, from whichever topic
// holds it. It is deliberately unscoped — the board is the storage layer, and
// deciding who may see a message is a protocol-level call, the same split
// Send uses for in_reply_to. Callers exposing this to an agent MUST check the
// returned Topic against that agent's subscriptions: an unscoped read by seq
// would let anyone enumerate every ring without ever learning a topic name,
// and knowing the name is the whole price of entry today.
func (b *Board) Retained(seq uint64) (RetainedMessage, bool) {
	if seq == 0 {
		return RetainedMessage{}, false
	}
	b.mu.Lock()
	tps := make([]*topic, 0, len(b.topics))
	for _, t := range b.topics {
		tps = append(tps, t)
	}
	b.mu.Unlock()

	for _, t := range tps {
		for _, m := range t.snapshot() {
			if m.Seq == seq {
				return m, true
			}
		}
	}
	return RetainedMessage{}, false
}

func (b *Board) LookupSeq(seq uint64) (string, protocol.TaskID, bool) {
	if seq == 0 {
		return "", protocol.TaskID{}, false
	}
	b.mu.Lock()
	names := make([]string, 0, len(b.topics))
	tps := make([]*topic, 0, len(b.topics))
	for n, t := range b.topics {
		names = append(names, n)
		tps = append(tps, t)
	}
	b.mu.Unlock()

	for i, t := range tps {
		for _, m := range t.snapshot() {
			if m.Seq == seq {
				return names[i], m.FromTask, true
			}
		}
	}
	return "", protocol.TaskID{}, false
}

// BoardTopicSummary is one row of ListTopics output. It uses Go-native types
// (string, time.Time, int) and is distinct from the generated wire type TopicSummary.
type BoardTopicSummary struct {
	Name            string
	LastSeq         uint64
	LastPublishedAt time.Time
	MsgCount        int
}

// ListSubscriptions returns the registered patterns for the (rid, tid) bound
// to c. Order is unspecified. Returns nil for a nil/unattached ConnState.
func (b *Board) ListSubscriptions(c *ConnState) []string {
	if c == nil || c.task == nil {
		return nil
	}
	return c.task.snapshotPatterns()
}

// SubscriberRow is one task's subscription set together with the identity
// captured on its taskState. Hostname is empty for a task that has been
// registered (and so has its chat.<short-id> seeded) but has not yet run a
// harness-cli command — Attach is what fills it in. That is a real state, not
// missing data.
type SubscriberRow struct {
	Task         protocol.TaskID
	Hostname     string
	AgentProfile string
	Patterns     []string
}

// ListSubscribers returns one row per task known to the board. A non-empty
// topic narrows the result to the tasks a publish to that topic would reach;
// Patterns still holds each returned row's full set. Order is unspecified.
//
// The filter calls taskState.matches — the same predicate Board.Send uses to
// pick delivery targets — rather than reimplementing the comparison, so this
// view cannot claim a different set of recipients than delivery actually uses
// if matching ever gains wildcards.
//
// Deliberately absent: the attached-connection count. harness-cli is a
// short-lived process per subcommand, so a healthy agent has zero attached
// connections almost all of the time; reporting that number would read as
// "nobody is connected" and mislead exactly the diagnosis this exists for.
func (b *Board) ListSubscribers(topic string) []SubscriberRow {
	b.mu.Lock()
	states := make([]*taskState, 0, len(b.tasks))
	for _, ts := range b.tasks {
		states = append(states, ts)
	}
	b.mu.Unlock()

	out := make([]SubscriberRow, 0, len(states))
	for _, ts := range states {
		if topic != "" && !ts.matches(topic) {
			continue
		}
		_, tid, host, profile := ts.identity()
		out = append(out, SubscriberRow{
			Task:         tid,
			Hostname:     host,
			AgentProfile: profile,
			Patterns:     ts.snapshotPatterns(),
		})
	}
	return out
}

// ListTopics returns a snapshot of every topic currently retained on the board.
// Order is unspecified.
func (b *Board) ListTopics() []BoardTopicSummary {
	b.mu.Lock()
	names := make([]string, 0, len(b.topics))
	tps := make([]*topic, 0, len(b.topics))
	for n, t := range b.topics {
		names = append(names, n)
		tps = append(tps, t)
	}
	b.mu.Unlock()

	out := make([]BoardTopicSummary, 0, len(names))
	for i, n := range names {
		ls, lp, c := tps[i].summary()
		out = append(out, BoardTopicSummary{
			Name:            n,
			LastSeq:         ls,
			LastPublishedAt: lp,
			MsgCount:        c,
		})
	}
	return out
}
