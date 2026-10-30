package agentboard

import (
	"errors"
	"strings"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Membership is a task's standing on one topic, granted from outside the task
// (board join): the task is subscribed to it and may publish to it without
// board_peer. It is kept per task id at the board level, not on a taskState,
// so it outlives Revoke and reaches every runner a task is resumed on. See
// docs/superpowers/specs/2026-10-31-board-membership-design.md.

var (
	// ErrNotJoinable is a topic membership is refused on: an empty name, any
	// chat.* topic, or a tree.<X>.* topic outside the task's lineage. Join
	// never widens a namespace the reach rules keep closed.
	ErrNotJoinable = errors.New("agentboard: topic cannot be joined")
	// ErrNotRunning is a subscription for a task the board holds no state
	// for: nothing would carry it, and nothing would bring it back later.
	ErrNotRunning = errors.New("agentboard: task holds no board state")
)

// statesOf returns every taskState held under tid — one per runner the task
// is attached through.
func (b *Board) statesOf(tid protocol.TaskID) []*taskState {
	want := hexTaskIDProto(tid)
	b.mu.Lock()
	defer b.mu.Unlock()
	var states []*taskState
	for k, ts := range b.tasks {
		if k.task == want {
			states = append(states, ts)
		}
	}
	return states
}

// SubscribeTask subscribes tid to topic on the operator's behalf. It passes
// the check tid's own subscribe would, and grants nothing beyond reading.
func (b *Board) SubscribeTask(tid protocol.TaskID, topic string) error {
	if topic == OperatorTopic {
		return ErrReservedTopic
	}
	if topic == "" {
		return ErrNotPermitted
	}
	if r := b.reachSource(); r != nil && !b.readableFor(r, tid, topic) {
		return ErrNotPermitted
	}
	states := b.statesOf(tid)
	if len(states) == 0 {
		return ErrNotRunning
	}
	for _, ts := range states {
		ts.addPattern(topic)
	}
	return nil
}

// UnsubscribeTask removes topic from every state of tid. A membership stays:
// the task keeps its publish right and is subscribed again on its next
// registration.
func (b *Board) UnsubscribeTask(tid protocol.TaskID, topic string) {
	for _, ts := range b.statesOf(tid) {
		ts.removePattern(topic)
	}
}

func (b *Board) joinable(tid protocol.TaskID, topic string) bool {
	if topic == "" || strings.HasPrefix(topic, SelfTopicPrefix) {
		return false
	}
	if x, ok := parseTreeTopic(topic); ok {
		r := b.reachSource()
		return r == nil || b.member(r, tid, x)
	}
	return true
}

// Join makes tid a member of topic and subscribes every state it holds. A task
// with no state yet is subscribed when it registers.
func (b *Board) Join(tid protocol.TaskID, topic string) error {
	if !b.joinable(tid, topic) {
		return ErrNotJoinable
	}
	b.mu.Lock()
	b.addMemberLocked(topic, tid)
	store := b.retainedStoreLocked(topic)
	b.mu.Unlock()
	for _, ts := range b.statesOf(tid) {
		ts.addPattern(topic)
	}
	if store != nil {
		storeLog("member", topic, store.SetMember(topic, tid, true))
	}
	return nil
}

// Leave ends tid's membership of topic and its subscription to it.
func (b *Board) Leave(tid protocol.TaskID, topic string) {
	b.mu.Lock()
	_, was := b.members[topic][tid]
	if was {
		delete(b.members[topic], tid)
		if len(b.members[topic]) == 0 {
			delete(b.members, topic)
		}
	}
	store := b.retainedStoreLocked(topic)
	b.mu.Unlock()
	for _, ts := range b.statesOf(tid) {
		ts.removePattern(topic)
	}
	if was && store != nil {
		storeLog("member", topic, store.SetMember(topic, tid, false))
	}
}

// IsMember reports whether tid is a member of topic.
func (b *Board) IsMember(tid protocol.TaskID, topic string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.isMemberLocked(tid, topic)
}

func (b *Board) addMemberLocked(topic string, tid protocol.TaskID) {
	if b.members == nil {
		b.members = make(map[string]map[protocol.TaskID]struct{})
	}
	if b.members[topic] == nil {
		b.members[topic] = make(map[protocol.TaskID]struct{})
	}
	b.members[topic][tid] = struct{}{}
}

func (b *Board) isMemberLocked(tid protocol.TaskID, topic string) bool {
	_, ok := b.members[topic][tid]
	return ok
}

// memberTopicsLocked returns every topic tid is a member of.
func (b *Board) memberTopicsLocked(tid protocol.TaskID) []string {
	var out []string
	for topic, tasks := range b.members {
		if _, ok := tasks[tid]; ok {
			out = append(out, topic)
		}
	}
	return out
}

// retainedStoreLocked returns the store when topic is retained, else nil: only
// a retained topic's membership is written down.
func (b *Board) retainedStoreLocked(topic string) Store {
	if t, ok := b.topics[topic]; ok && t.retention != nil {
		return b.store
	}
	return nil
}
