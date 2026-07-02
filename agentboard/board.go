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
func (b *Board) RegisterTask(rid protocol.RunnerID, tid protocol.TaskID, ticket [16]byte) {
	b.reg.Register(rid, tid, ticket)
	key := ticketKey{runner: runnerIDStringProto(rid), task: hexTaskIDProto(tid)}
	b.mu.Lock()
	ts, ok := b.tasks[key]
	if !ok {
		ts = newTaskState()
		b.tasks[key] = ts
	}
	b.mu.Unlock()
	ts.setIdentity(rid, tid, "")
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
// the taskState if this is the first agent connecting under that ticket. hostname is
// captured into the taskState so Board.Send can attach sender attestation to every
// message published by this (rid, tid).
func (b *Board) Attach(rid RunnerID, tid TaskID, hostname string) *ConnState {
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
	ts.setIdentity(protoRid, protoTid, hostname)
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
			if _, ok := b.topics[p]; ok && !b.anyTaskMatchesLocked(p) {
				delete(b.topics, p)
			}
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

// Send appends a message to topicName attributed to the given (rid, tid, hostname).
// The caller (server agent_handler) is responsible for passing the *authenticated*
// sender — taken from the calling ConnState's taskState — so agents cannot spoof
// the from_* fields.
func (b *Board) Send(topicName string, payload []byte, fromRid protocol.RunnerID, fromTid protocol.TaskID, fromHost string) (uint64, error) {
	if len(payload) > b.cfg.MaxPayload {
		return 0, ErrPayloadTooLarge
	}
	b.mu.Lock()
	t, ok := b.topics[topicName]
	if !ok {
		if len(b.topics) >= b.cfg.MaxTopics {
			b.evictOldestTopicLocked()
			if len(b.topics) >= b.cfg.MaxTopics {
				b.mu.Unlock()
				return 0, ErrTooManyTopics
			}
		}
		t = newTopic(topicName, b.cfg.RingN)
		b.topics[topicName] = t
	}
	fromKey := ticketKey{runner: runnerIDStringProto(fromRid), task: hexTaskIDProto(fromTid)}
	targets := make([]*taskState, 0)
	var selfTs *taskState
	for k, ts := range b.tasks {
		if ts.matches(topicName) {
			targets = append(targets, ts)
			if k == fromKey {
				selfTs = ts
			}
		}
	}
	b.mu.Unlock()

	seq := b.seq.Add(1)
	t.append(seq, payload, fromRid, fromTid, fromHost)

	b.mu.Lock()
	fn := b.onDeliver
	b.mu.Unlock()
	for _, ts := range targets {
		// ping self too — supports loopback waits where the same (rid, tid)
		// has one connection waiting and another publishing (e.g. concurrent
		// `harness-cli agent wait` + `agent send`). Cheap and harmless.
		for _, c := range ts.snapshotConns() {
			c.ping()
		}
		// Skip onDeliver for the publisher's own taskState — otherwise the
		// server's wake hook would emit task_wake to the publisher's runner
		// for a message the publisher just sent itself, injecting a spurious
		// <harness:agentboard-wake> into the publisher's own stdin.
		if fn != nil && ts != selfTs {
			rid, tid, _ := ts.identity()
			fn(rid, tid)
		}
	}
	return seq, nil
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
// or until ctx is done. Returns (messages, timedOut, error). Implicitly adds
// topicName to the persistent (rid, tid) subscription set — Wait callers
// inherit Subscribe's persistence semantics. Disable this side-effect by
// pre-Subscribing then Unsubscribing if undesired.
func (b *Board) Wait(ctx context.Context, c *ConnState, topicName string, since uint64) ([]RetainedMessage, bool, error) {
	if c == nil || c.task == nil {
		return nil, false, errors.New("not attached")
	}
	if !c.task.matches(topicName) {
		c.task.addPattern(topicName)
	}
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
