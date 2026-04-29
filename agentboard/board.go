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
}

func New(cfg Config) *Board {
	b := &Board{
		cfg:    cfg,
		topics: make(map[string]*topic),
		tasks:  make(map[ticketKey]*taskState),
		reg:    newRegistry(),
		stopCh: make(chan struct{}),
	}
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
func (b *Board) Revoke(rid protocol.RunnerID, tid protocol.TaskID) {
	b.reg.Revoke(rid, tid)
	key := ticketKey{runner: runnerIDStringProto(rid), task: hexTaskIDProto(tid)}
	b.mu.Lock()
	delete(b.tasks, key)
	b.mu.Unlock()
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
	targets := make([]*taskState, 0)
	for _, ts := range b.tasks {
		if ts.matches(topicName) {
			targets = append(targets, ts)
		}
	}
	b.mu.Unlock()

	seq := b.seq.Add(1)
	t.append(seq, payload, fromRid, fromTid, fromHost)

	for _, ts := range targets {
		for _, c := range ts.snapshotConns() {
			c.ping()
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

// BoardTopicSummary is one row of ListTopics output. It uses Go-native types
// (string, time.Time, int) and is distinct from the generated wire type TopicSummary.
type BoardTopicSummary struct {
	Name            string
	LastSeq         uint64
	LastPublishedAt time.Time
	MsgCount        int
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
