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
