package agentboard

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// fakeReach is a mutable task table: tests change a parent or a cap between
// calls to show the board reads it on every check, not at subscribe time.
type fakeReach struct {
	creator map[protocol.TaskID]protocol.TaskID
	caps    map[protocol.TaskID]protocol.Capability
}

func newFakeReach() *fakeReach {
	return &fakeReach{creator: map[protocol.TaskID]protocol.TaskID{}, caps: map[protocol.TaskID]protocol.Capability{}}
}

func (f *fakeReach) add(tid, creator protocol.TaskID, caps protocol.Capability) {
	f.creator[tid], f.caps[tid] = creator, caps
}

func (f *fakeReach) CreatorOf(t protocol.TaskID) (protocol.TaskID, bool) {
	c, ok := f.creator[t]
	return c, ok
}

func (f *fakeReach) CapsOf(t protocol.TaskID) (protocol.Capability, bool) {
	c, ok := f.caps[t]
	return c, ok
}

func (f *fakeReach) HasChild(p protocol.TaskID, short string) bool {
	for t, c := range f.creator {
		if c == p && hexTaskIDProto(t)[:selfTopicShortLen] == short {
			return true
		}
	}
	return false
}

func (f *fakeReach) Len() int { return len(f.creator) }

func tidN(b byte) protocol.TaskID {
	var t protocol.TaskID
	t.Id[0], t.Id[15] = b, b
	return t
}

func treeOf(t protocol.TaskID) string { return "tree." + hex.EncodeToString(t.Id[:]) + ".x" }

func reachBoard(t *testing.T, r *fakeReach) *Board {
	t.Helper()
	b := New(Config{RingN: 8, TopicTTL: time.Hour, MaxTopics: 64, MaxPayload: 1024})
	t.Cleanup(func() { b.Close() })
	b.SetReach(r)
	return b
}

func TestReadableNamespaces(t *testing.T) {
	r := newFakeReach()
	root, me, peer := tidN(1), tidN(2), tidN(3)
	r.add(root, protocol.TaskID{}, 0)
	r.add(me, root, 0)
	r.add(peer, protocol.TaskID{}, 0)
	b := reachBoard(t, r)
	c := b.Attach(protocol.RunnerID{}, me, "h", "bash")

	for _, tc := range []struct {
		topic string
		want  bool
	}{
		{SelfTopic(me), true},
		{SelfTopic(me) + ".side", true},
		{SelfTopic(peer), false},
		{SelfTopic(peer) + ".side", false},
		{OperatorTopic, false},
		{treeOf(root), true}, // my ancestor
		{treeOf(me), true},   // myself
		{treeOf(peer), false},
		{"tree.notahex.x", true}, // not a tree namespace: free-form
		{"build.events", true},
	} {
		if got := b.Readable(c, tc.topic); got != tc.want {
			t.Errorf("Readable(%s) = %v, want %v", tc.topic, got, tc.want)
		}
	}
	r.caps[me] = protocol.Capability_BoardObserve
	for _, topic := range []string{SelfTopic(peer), OperatorTopic, treeOf(peer)} {
		if !b.Readable(c, topic) {
			t.Errorf("with board_observe, Readable(%s) = false", topic)
		}
	}
}

func TestMembershipFollowsTheLiveParent(t *testing.T) {
	r := newFakeReach()
	s, w := tidN(1), tidN(2)
	r.add(s, protocol.TaskID{}, 0)
	r.add(w, s, 0)
	b := reachBoard(t, r)
	cw := b.Attach(protocol.RunnerID{}, w, "h", "bash")
	if !b.Readable(cw, treeOf(s)) {
		t.Fatal("worker cannot read its supervisor's tree")
	}
	// set-parent --swap: W becomes S's parent.
	r.creator[w], r.creator[s] = protocol.TaskID{}, w
	if b.Readable(cw, treeOf(s)) {
		t.Error("worker still reads tree.S after leaving it")
	}
	cs := b.Attach(protocol.RunnerID{}, s, "h", "bash")
	if !b.Readable(cs, treeOf(w)) {
		t.Error("supervisor cannot read tree.W after joining it")
	}
}

func TestMembershipEndsAtAPruneHole(t *testing.T) {
	r := newFakeReach()
	root, mid, leaf := tidN(1), tidN(2), tidN(3)
	r.add(root, protocol.TaskID{}, 0)
	r.add(leaf, mid, 0) // mid is not in the table: pruned
	b := reachBoard(t, r)
	if b.Readable(b.Attach(protocol.RunnerID{}, leaf, "h", "bash"), treeOf(root)) {
		t.Error("membership crossed a pruned task (D9)")
	}
}

func TestMembershipWalkIsBounded(t *testing.T) {
	r := newFakeReach()
	a, c := tidN(1), tidN(2)
	r.add(a, c, 0)
	r.add(c, a, 0) // a cycle set_parent refuses; the walk must still stop
	b := reachBoard(t, r)
	if b.Readable(b.Attach(protocol.RunnerID{}, a, "h", "bash"), treeOf(tidN(9))) {
		t.Error("a cycle reported membership")
	}
}

func TestUnknownTaskFailsClosed(t *testing.T) {
	b := reachBoard(t, newFakeReach())
	c := b.Attach(protocol.RunnerID{}, tidN(7), "h", "bash")
	if b.Readable(c, SelfTopic(tidN(8))) {
		t.Error("a task missing from the store read a peer's topic")
	}
	if !b.Readable(c, SelfTopic(tidN(7))) {
		t.Error("a task missing from the store cannot read its own topic")
	}
}

func TestNilReachEnforcesNothing(t *testing.T) {
	b := New(Config{RingN: 8, TopicTTL: time.Hour, MaxTopics: 64, MaxPayload: 1024})
	defer b.Close()
	c := b.Attach(protocol.RunnerID{}, tidN(1), "h", "bash")
	if !b.Readable(c, SelfTopic(tidN(2))) {
		t.Error("a board with no reach source refused a read")
	}
}
