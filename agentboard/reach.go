package agentboard

import (
	"encoding/hex"
	"errors"
	"strings"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// ErrNotPermitted is a board operation the caller's reach does not cover.
// See docs/superpowers/specs/2026-10-16-agentboard-reach-design.md.
var ErrNotPermitted = errors.New("agentboard: not permitted")

// ReachSource is what the board needs from the task store to decide reach.
// The server implements it over its TaskStore (server/taskstore_reach.go).
type ReachSource interface {
	CreatorOf(tid protocol.TaskID) (creator protocol.TaskID, ok bool)
	CapsOf(tid protocol.TaskID) (protocol.Capability, bool)
	// HasChild reports whether a live task whose creator is parent has a
	// task id beginning with short (8 hex).
	HasChild(parent protocol.TaskID, short string) bool
	// Len bounds the membership walk: no chain is longer than the store.
	Len() int
	// PeerCovers reports whether tid's board_peer target set — its scope
	// for that bit — holds a task whose id begins with short (8 hex). True
	// for a global scope. PeerGlobal reports that global scope, the one form
	// that opens free-form topics (reach spec D16). Neither checks the bit.
	PeerCovers(tid protocol.TaskID, short string) bool
	PeerGlobal(tid protocol.TaskID) bool
}

// SetReach installs the source every reach check reads. A board without one
// enforces nothing; the server installs one in SetBoard, and the ring and
// delivery tests construct bare boards.
func (b *Board) SetReach(r ReachSource) {
	b.mu.Lock()
	b.reach = r
	b.mu.Unlock()
}

func (b *Board) reachSource() ReachSource {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reach
}

const treeTopicPrefix = "tree."

// parseTreeTopic returns X for tree.<32 hex>.<suffix>. Anything else under
// tree. is a free-form name.
func parseTreeTopic(topic string) (protocol.TaskID, bool) {
	rest, ok := strings.CutPrefix(topic, treeTopicPrefix)
	if !ok || len(rest) < 34 || rest[32] != '.' {
		return protocol.TaskID{}, false
	}
	raw, err := hex.DecodeString(rest[:32])
	if err != nil {
		return protocol.TaskID{}, false
	}
	var x protocol.TaskID
	copy(x.Id[:], raw)
	return x, true
}

// isOwnChat reports whether topic is chat.<self short id> or chat.<short>.*.
func isOwnChat(self protocol.TaskID, topic string) bool {
	own := SelfTopic(self)
	return topic == own || strings.HasPrefix(topic, own+".")
}

// member is the spec's §4: x is caller, or x is on caller's creator chain. The
// walk stops at the root, at a task the store no longer has (D9), or after Len
// steps.
func (b *Board) member(r ReachSource, caller, x protocol.TaskID) bool {
	cur := caller
	for step := 0; step <= r.Len(); step++ {
		if cur == x {
			return true
		}
		next, ok := r.CreatorOf(cur)
		if !ok || next == (protocol.TaskID{}) {
			return false
		}
		cur = next
	}
	return false
}

func hasCap(r ReachSource, tid protocol.TaskID, c protocol.Capability) bool {
	caps, ok := r.CapsOf(tid)
	return ok && caps&c != 0
}

// Readable is the spec's §5: may c's task read topic? Called with no board or
// task lock held — it takes the task store's.
func (b *Board) Readable(c *ConnState, topic string) bool {
	r := b.reachSource()
	if r == nil {
		return true
	}
	if c == nil || c.task == nil {
		return false
	}
	_, self, _, _ := c.task.identity()
	return b.readableFor(r, self, topic)
}

func (b *Board) readableFor(r ReachSource, self protocol.TaskID, topic string) bool {
	if strings.HasPrefix(topic, SelfTopicPrefix) {
		if topic != OperatorTopic && isOwnChat(self, topic) {
			return true
		}
		return hasCap(r, self, protocol.Capability_BoardObserve)
	}
	if x, ok := parseTreeTopic(topic); ok {
		return b.member(r, self, x) || hasCap(r, self, protocol.Capability_BoardObserve)
	}
	return true
}

// Publishable is the spec's §2: may c's task publish to topic? The server asks
// it with the topic resolveReplyTarget returned, so a reply is checked where
// it will actually land (D4).
func (b *Board) Publishable(c *ConnState, topic string) bool {
	r := b.reachSource()
	if r == nil {
		return true
	}
	if c == nil || c.task == nil {
		return false
	}
	_, self, _, _ := c.task.identity()
	if x, ok := parseTreeTopic(topic); ok {
		return b.member(r, self, x) // no bit opens a tree the task is not in (D7)
	}
	if isOwnChat(self, topic) || topic == OperatorTopic {
		return true
	}
	if creator, ok := r.CreatorOf(self); ok && creator != (protocol.TaskID{}) && topic == SelfTopic(creator) {
		return true
	}
	if short, ok := strings.CutPrefix(topic, SelfTopicPrefix); ok && len(short) == selfTopicShortLen && r.HasChild(self, short) {
		return true
	}
	if c.task.hasContact(topic) {
		return true
	}
	if !hasCap(r, self, protocol.Capability_BoardPeer) {
		return false
	}
	// D16: the bit opens what its scope covers. A task's chat.<id> namespace
	// is covered when that task is in the target set; anything else names no
	// task, so only the unbounded form opens it.
	if short, ok := chatShortID(topic); ok {
		return r.PeerCovers(self, short)
	}
	return r.PeerGlobal(self)
}

// chatShortID returns <id> for chat.<id> and chat.<id>.<suffix> when <id> is a
// task's 8-hex short id.
func chatShortID(topic string) (string, bool) {
	rest, ok := strings.CutPrefix(topic, SelfTopicPrefix)
	if !ok || len(rest) < selfTopicShortLen {
		return "", false
	}
	short := rest[:selfTopicShortLen]
	if len(rest) > selfTopicShortLen && rest[selfTopicShortLen] != '.' {
		return "", false
	}
	if _, err := hex.DecodeString(short); err != nil {
		return "", false
	}
	return short, true
}

// NoteDelivered records the reply route of every message handed to c's task
// that was addressed to it (the spec's §3, as amended): one on its own
// chat.<id> / chat.<id>.*, or a reply to a message it sent. A message read off
// a group topic, or off another task's topic with board_observe, adds nobody —
// a read power must not become a publish power, and a busy group topic would
// otherwise push real correspondents out of the bounded set. The server calls
// it from the one place wait, inbox, inbox_advance and read_seq build their
// rows.
func (b *Board) NoteDelivered(c *ConnState, msgs []RetainedMessage) {
	if c == nil || c.task == nil {
		return
	}
	_, self, _, _ := c.task.identity()
	for _, m := range msgs {
		if !b.addressedTo(self, m) {
			continue
		}
		switch {
		case m.ReplyToTopic != "":
			c.task.addContact(m.ReplyToTopic, m.Seq)
		case m.SenderKind == protocol.SenderKind_Agent:
			c.task.addContact(SelfTopic(m.FromTask), m.Seq)
		}
	}
}

// Contacts returns c's task's contacts, newest first: the routes it may
// answer without board_peer because someone addressed it from there.
func (b *Board) Contacts(c *ConnState) []string {
	if c == nil || c.task == nil {
		return nil
	}
	return c.task.snapshotContacts()
}

// ClearContacts forgets topic from tid's contacts, or every contact when topic
// is empty (the operator's board clear-contacts). found is false when the board
// holds no state for tid. The route stays forgotten until a message published
// after this call is addressed to the task from it. A task resumed on another runner has one state per
// runner, so every one of them is cleared.
func (b *Board) ClearContacts(tid protocol.TaskID, topic string) (removed int, found bool) {
	want := hexTaskIDProto(tid)
	b.mu.Lock()
	var states []*taskState
	for k, ts := range b.tasks {
		if k.task == want {
			states = append(states, ts)
		}
	}
	b.mu.Unlock()
	floor := b.seq.Load()
	for _, ts := range states {
		removed += ts.forgetContacts(topic, floor)
	}
	return removed, len(states) > 0
}

// readableTargets keeps the tasks that may read topic. Called after b.mu is
// released: the predicate takes the task store's lock (reach spec §5, Lock
// order).
func (b *Board) readableTargets(targets []*taskState, topic string) []*taskState {
	r := b.reachSource()
	if r == nil {
		return targets
	}
	kept := targets[:0]
	for _, ts := range targets {
		_, tid, _, _ := ts.identity()
		if b.readableFor(r, tid, topic) {
			kept = append(kept, ts)
		}
	}
	return kept
}

func (b *Board) readableTopic(ts *taskState, topic string) bool {
	r := b.reachSource()
	if r == nil {
		return true
	}
	_, tid, _, _ := ts.identity()
	return b.readableFor(r, tid, topic)
}

// readablePatterns drops the subscriptions ts may no longer read. The
// patterns stay in ts; granting the bit back restores them.
func (b *Board) readablePatterns(ts *taskState, patterns []string) []string {
	kept := patterns[:0]
	for _, p := range patterns {
		if b.readableTopic(ts, p) {
			kept = append(kept, p)
		}
	}
	return kept
}

// addressedTo reports whether m was meant for self: delivered on self's own
// chat.<id> / chat.<id>.*, or answering a message self published.
func (b *Board) addressedTo(self protocol.TaskID, m RetainedMessage) bool {
	if isOwnChat(self, m.Topic) {
		return true
	}
	if m.InReplyTo == 0 {
		return false
	}
	parent, ok := b.ReplyParent(m.InReplyTo)
	return ok && parent.FromTask == self
}
