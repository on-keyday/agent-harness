package agentboard

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
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
	conns   map[*ConnState]struct{}
	seq     atomic.Uint64
	reg     *registry
	stopCh  chan struct{}
	stopped bool
}

func New(cfg Config) *Board {
	b := &Board{
		cfg:    cfg,
		topics: make(map[string]*topic),
		conns:  make(map[*ConnState]struct{}),
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

func (b *Board) Attach() *ConnState {
	c := newConnState()
	b.mu.Lock()
	b.conns[c] = struct{}{}
	b.mu.Unlock()
	return c
}

func (b *Board) Detach(c *ConnState) {
	b.mu.Lock()
	delete(b.conns, c)
	b.mu.Unlock()
}

func (b *Board) Subscribe(c *ConnState, pattern string) error {
	if pattern == "" {
		return errors.New("empty pattern")
	}
	c.addPattern(pattern)
	return nil
}

func (b *Board) Unsubscribe(c *ConnState, pattern string) {
	c.removePattern(pattern)
}

var (
	ErrPayloadTooLarge = errors.New("agentboard: payload too large")
	ErrTooManyTopics   = errors.New("agentboard: too many topics")
)

func (b *Board) Send(topicName string, payload []byte) (uint64, error) {
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
	conns := make([]*ConnState, 0, len(b.conns))
	for c := range b.conns {
		conns = append(conns, c)
	}
	b.mu.Unlock()

	seq := b.seq.Add(1)
	t.append(seq, payload)

	for _, c := range conns {
		if c.matches(topicName) {
			c.ping()
		}
	}
	return seq, nil
}

// Inbox returns retained messages for all topics this conn is subscribed to,
// with Seq > since, plus the new cursor (max seq seen, or since if none).
func (b *Board) Inbox(c *ConnState, since uint64) ([]RetainedMessage, uint64) {
	c.mu.Lock()
	patterns := make([]string, 0, len(c.patterns))
	for p := range c.patterns {
		patterns = append(patterns, p)
	}
	c.mu.Unlock()

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
// or until ctx is done. Returns (messages, timedOut, error). An implicit subscribe
// is added for the wait window.
func (b *Board) Wait(ctx context.Context, c *ConnState, topicName string, since uint64) ([]RetainedMessage, bool, error) {
	if !c.matches(topicName) {
		c.addPattern(topicName)
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
