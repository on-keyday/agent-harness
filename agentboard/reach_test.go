package agentboard

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
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

func TestPublishableBaseSet(t *testing.T) {
	r := newFakeReach()
	sup, me, child, peer := tidN(1), tidN(2), tidN(3), tidN(4)
	r.add(sup, protocol.TaskID{}, 0)
	r.add(me, sup, 0)
	r.add(child, me, 0)
	r.add(peer, protocol.TaskID{}, 0)
	b := reachBoard(t, r)
	c := b.Attach(protocol.RunnerID{}, me, "h", "bash")

	for _, tc := range []struct {
		name, topic string
		want        bool
	}{
		{"self", SelfTopic(me), true},
		{"self side channel", SelfTopic(me) + ".x", true},
		{"operator", OperatorTopic, true},
		{"creator", SelfTopic(sup), true},
		{"child", SelfTopic(child), true},
		{"peer", SelfTopic(peer), false},
		{"free-form", "build.events", false},
		{"own tree", treeOf(me), true},
		{"ancestor tree", treeOf(sup), true},
		{"foreign tree", treeOf(peer), false},
	} {
		if got := b.Publishable(c, tc.topic); got != tc.want {
			t.Errorf("%s: Publishable(%s) = %v, want %v", tc.name, tc.topic, got, tc.want)
		}
	}

	r.caps[me] = protocol.Capability_BoardPeer
	if !b.Publishable(c, SelfTopic(peer)) || !b.Publishable(c, "build.events") {
		t.Error("board_peer did not open a peer / free-form topic")
	}
	if b.Publishable(c, treeOf(peer)) {
		t.Error("board_peer opened a tree the task is not in (D7)")
	}
}

func TestContactsOpenTheReplyRoute(t *testing.T) {
	r := newFakeReach()
	me, peer := tidN(2), tidN(4)
	r.add(me, protocol.TaskID{}, 0)
	r.add(peer, protocol.TaskID{}, protocol.Capability_BoardPeer)
	b := reachBoard(t, r)
	c := b.Attach(protocol.RunnerID{}, me, "h", "bash")
	if b.Publishable(c, SelfTopic(peer)) {
		t.Fatal("peer reachable before it said anything")
	}
	b.NoteDelivered(c, []RetainedMessage{
		{FromTask: peer, SenderKind: protocol.SenderKind_Agent},
		{FromTask: peer, SenderKind: protocol.SenderKind_Agent, ReplyToTopic: "review.results"},
		{SenderKind: protocol.SenderKind_Operator}, // no route, nothing added
	})
	if !b.Publishable(c, SelfTopic(peer)) {
		t.Error("the sender's chat.<id> did not become a contact")
	}
	if !b.Publishable(c, "review.results") {
		t.Error("a declared reply_to did not become a contact")
	}
	if b.Publishable(c, SelfTopic(protocol.TaskID{})) {
		t.Error("an operator message without a reply route opened the zero task's topic")
	}
}

func TestContactsAreBounded(t *testing.T) {
	ts := newTaskState()
	for i := 0; i < maxContacts+1; i++ {
		ts.addContact(fmt.Sprintf("t.%d", i))
	}
	if ts.hasContact("t.0") {
		t.Error("the oldest contact survived past the bound")
	}
	if !ts.hasContact(fmt.Sprintf("t.%d", maxContacts)) {
		t.Error("the newest contact is missing")
	}
}

func TestSendFanoutSkipsAnUnreadableSubscriber(t *testing.T) {
	r := newFakeReach()
	owner, snoop := tidN(1), tidN(2)
	r.add(owner, protocol.TaskID{}, 0)
	r.add(snoop, protocol.TaskID{}, protocol.Capability_BoardObserve)
	b := reachBoard(t, r)
	var woke []protocol.TaskID
	b.SetOnDeliver(func(_ protocol.RunnerID, tid protocol.TaskID) { woke = append(woke, tid) })
	co := b.Attach(protocol.RunnerID{}, owner, "h", "bash")
	cs := b.Attach(protocol.RunnerID{}, snoop, "h", "bash")
	if err := b.Subscribe(co, SelfTopic(owner)); err != nil {
		t.Fatal(err)
	}
	if err := b.Subscribe(cs, SelfTopic(owner)); err != nil { // allowed: holds board_observe
		t.Fatal(err)
	}
	r.caps[snoop] = 0 // caps set takes the bit away
	if _, delivered, err := b.Send(SelfTopic(owner), []byte("x"), protocol.RunnerID{}, tidN(9), "h", "", 0); err != nil || delivered != 1 {
		t.Fatalf("delivered = %d, %v; want 1 (the owner only)", delivered, err)
	}
	for _, w := range woke {
		if w == snoop {
			t.Error("a task without board_observe was woken for another task's topic")
		}
	}
}

func TestRevokedObserveHidesAndRestores(t *testing.T) {
	r := newFakeReach()
	owner, snoop := tidN(1), tidN(2)
	r.add(owner, protocol.TaskID{}, 0)
	r.add(snoop, protocol.TaskID{}, protocol.Capability_BoardObserve)
	b := reachBoard(t, r)
	cs := b.Attach(protocol.RunnerID{}, snoop, "h", "bash")
	if err := b.Subscribe(cs, SelfTopic(owner)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Send(SelfTopic(owner), []byte("x"), protocol.RunnerID{}, tidN(9), "h", "", 0, WithNoWake()); err != nil {
		t.Fatal(err)
	}
	r.caps[snoop] = 0
	if msgs, _ := b.Inbox(cs, 0); len(msgs) != 0 {
		t.Errorf("inbox after revoke returned %d rows (D11: already-retained must hide)", len(msgs))
	}
	if msgs := b.InboxAdvance(cs); len(msgs) != 0 {
		t.Errorf("advance after revoke returned %d rows", len(msgs))
	}
	if b.Subscribes(cs, SelfTopic(owner)) {
		t.Error("Subscribes (read_seq's check) still answers yes after revoke")
	}
	r.caps[snoop] = protocol.Capability_BoardObserve
	if msgs := b.InboxAdvance(cs); len(msgs) != 1 {
		t.Errorf("advance after re-grant returned %d rows, want 1: the revoked read moved the mark", len(msgs))
	}
}

func TestSubscribeAndWaitRefuseAnUnreadableTopic(t *testing.T) {
	r := newFakeReach()
	me, peer := tidN(1), tidN(2)
	r.add(me, protocol.TaskID{}, 0)
	r.add(peer, protocol.TaskID{}, 0)
	b := reachBoard(t, r)
	c := b.Attach(protocol.RunnerID{}, me, "h", "bash")
	if err := b.Subscribe(c, SelfTopic(peer)); !errors.Is(err, ErrNotPermitted) {
		t.Errorf("subscribe to a peer's topic: %v, want ErrNotPermitted", err)
	}
	if err := b.Subscribe(c, OperatorTopic); !errors.Is(err, ErrReservedTopic) {
		t.Errorf("subscribe to chat.operator: %v, want ErrReservedTopic (unchanged)", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := b.Wait(ctx, c, OperatorTopic, 0, 0); !errors.Is(err, ErrNotPermitted) {
		t.Errorf("wait on chat.operator: %v, want ErrNotPermitted", err)
	}
	if err := b.Subscribe(c, "build.events"); err != nil {
		t.Errorf("subscribe to a group topic: %v, want nil", err)
	}
}

func TestOperatorPublishStillReachesAConfinedTask(t *testing.T) {
	r := newFakeReach()
	me := tidN(1)
	r.add(me, protocol.TaskID{}, 0)
	b := reachBoard(t, r)
	b.SetOnDeliver(func(protocol.RunnerID, protocol.TaskID) {}) // Wake counts only emitted wakes
	c := b.Attach(protocol.RunnerID{}, me, "h", "bash")
	if err := b.Subscribe(c, SelfTopic(me)); err != nil {
		t.Fatal(err)
	}
	_, delivered, err := b.Send(SelfTopic(me), []byte("op"), protocol.RunnerID{}, protocol.TaskID{}, "", "", 0,
		WithSenderKind(protocol.SenderKind_Operator))
	if err != nil || delivered != 1 {
		t.Fatalf("operator publish: delivered=%d err=%v, want 1", delivered, err)
	}
	if n := b.Wake(SelfTopic(me)); n != 1 {
		t.Errorf("operator wake reached %d tasks, want 1", n)
	}
}

func TestListSubscribersShowsOnlyWhoAPublishReaches(t *testing.T) {
	r := newFakeReach()
	owner, snoop := tidN(1), tidN(2)
	r.add(owner, protocol.TaskID{}, 0)
	r.add(snoop, protocol.TaskID{}, protocol.Capability_BoardObserve)
	b := reachBoard(t, r)
	co := b.Attach(protocol.RunnerID{}, owner, "h", "bash")
	cs := b.Attach(protocol.RunnerID{}, snoop, "h", "bash")
	for _, c := range []*ConnState{co, cs} {
		if err := b.Subscribe(c, SelfTopic(owner)); err != nil {
			t.Fatal(err)
		}
	}
	r.caps[snoop] = 0
	if rows := b.ListSubscribers(SelfTopic(owner)); len(rows) != 1 {
		t.Errorf("subscribers = %d, want 1: the list must match what a publish reaches", len(rows))
	}
}
