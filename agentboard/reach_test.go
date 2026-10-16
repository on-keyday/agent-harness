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
	// peerAll / peerIDs stand in for the board_peer scope: global, or the
	// explicit target set. Neither set is a board_peer scope covering only
	// the task itself.
	peerAll map[protocol.TaskID]bool
	peerIDs map[protocol.TaskID][]protocol.TaskID
}

func newFakeReach() *fakeReach {
	return &fakeReach{
		creator: map[protocol.TaskID]protocol.TaskID{}, caps: map[protocol.TaskID]protocol.Capability{},
		peerAll: map[protocol.TaskID]bool{}, peerIDs: map[protocol.TaskID][]protocol.TaskID{},
	}
}

func (f *fakeReach) PeerGlobal(t protocol.TaskID) bool { return f.peerAll[t] }

func (f *fakeReach) PeerCovers(t protocol.TaskID, short string) bool {
	if f.peerAll[t] {
		return true
	}
	for _, id := range f.peerIDs[t] {
		if hexTaskIDProto(id)[:selfTopicShortLen] == short {
			return true
		}
	}
	return false
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

	// D16: board_peer opens what its scope covers. Held with a scope that
	// covers nobody else, it opens nothing beyond the base set.
	r.caps[me] = protocol.Capability_BoardPeer
	if b.Publishable(c, SelfTopic(peer)) || b.Publishable(c, "build.events") {
		t.Error("board_peer with a scope covering no peer opened a peer / free-form topic")
	}
	// Scoped to one task: that task's chat.<id> and side channels, and nothing
	// free-form.
	r.peerIDs[me] = []protocol.TaskID{peer}
	if !b.Publishable(c, SelfTopic(peer)) || !b.Publishable(c, SelfTopic(peer)+".side") {
		t.Error("board_peer scoped to the peer did not open the peer's chat.<id>")
	}
	if b.Publishable(c, "build.events") {
		t.Error("an ids-scoped board_peer opened a free-form topic")
	}
	// Global: every peer and every free-form topic, still no foreign tree.
	r.peerAll[me] = true
	if !b.Publishable(c, "build.events") {
		t.Error("a global board_peer did not open a free-form topic")
	}
	if b.Publishable(c, treeOf(peer)) {
		t.Error("board_peer opened a tree the task is not in (D7)")
	}
	// The scope alone is not the grant.
	r.caps[me] = 0
	if b.Publishable(c, SelfTopic(peer)) {
		t.Error("a board_peer scope without the bit opened a peer")
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
		{Topic: SelfTopic(me), FromTask: peer, SenderKind: protocol.SenderKind_Agent},
		{Topic: SelfTopic(me), FromTask: peer, SenderKind: protocol.SenderKind_Agent, ReplyToTopic: "review.results"},
		{Topic: SelfTopic(me), SenderKind: protocol.SenderKind_Operator}, // no route, nothing added
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
		ts.addContact(fmt.Sprintf("t.%d", i), 0)
	}
	if ts.hasContact("t.0") {
		t.Error("the oldest contact survived past the bound")
	}
	if !ts.hasContact(fmt.Sprintf("t.%d", maxContacts)) {
		t.Error("the newest contact is missing")
	}
}

// A correspondent who keeps writing must not be the one the bound drops: a
// re-add moves it to the newest end, so the eviction takes the quietest route.
func TestContactsEvictTheLeastRecentlyAdded(t *testing.T) {
	ts := newTaskState()
	for i := 0; i < maxContacts; i++ {
		ts.addContact(fmt.Sprintf("t.%d", i), 0)
	}
	ts.addContact("t.0", 0)
	ts.addContact("t.new", 0)
	if !ts.hasContact("t.0") {
		t.Error("a contact re-added just before the overflow was evicted")
	}
	if ts.hasContact("t.1") {
		t.Error("the least recently added contact survived the overflow")
	}
	got := ts.snapshotContacts()
	if len(got) != maxContacts || got[0] != "t.new" || got[1] != "t.0" {
		t.Errorf("snapshot = %d entries starting %q, want %d newest first [t.new t.0 …]", len(got), got[:2], maxContacts)
	}
}

func TestContactsAndSubscriberRowsShowTheList(t *testing.T) {
	r := newFakeReach()
	me, peer := tidN(2), tidN(4)
	r.add(me, protocol.TaskID{}, 0)
	r.add(peer, protocol.TaskID{}, 0)
	b := reachBoard(t, r)
	c := b.Attach(protocol.RunnerID{}, me, "h", "bash")
	if got := b.Contacts(c); len(got) != 0 {
		t.Fatalf("contacts before any message = %v, want none", got)
	}
	b.NoteDelivered(c, []RetainedMessage{
		{Topic: SelfTopic(me), FromTask: peer, SenderKind: protocol.SenderKind_Agent},
		{Topic: SelfTopic(me), FromTask: peer, SenderKind: protocol.SenderKind_Agent, ReplyToTopic: "review.results"},
	})
	want := []string{"review.results", SelfTopic(peer)}
	if got := b.Contacts(c); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Contacts = %v, want %v", got, want)
	}
	found := false
	for _, row := range b.ListSubscribers("") {
		if row.Task != me {
			continue
		}
		found = true
		if fmt.Sprint(row.Contacts) != fmt.Sprint(want) {
			t.Errorf("subscriber row contacts = %v, want %v", row.Contacts, want)
		}
	}
	if !found {
		t.Error("no subscriber row for the task")
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

// A contact is the right to answer someone who addressed YOU: a message on
// your own chat.<id> / chat.<id>.*, or a reply to a message you sent. Reading
// a group topic, or another task's topic with board_observe, adds nobody —
// otherwise a read power turns into a publish power, and a busy group topic
// pushes real correspondents out of the 64-entry set.
func TestContactsOnlyFromMessagesAddressedToTheReader(t *testing.T) {
	r := newFakeReach()
	me, peer, other := tidN(2), tidN(4), tidN(5)
	r.add(me, protocol.TaskID{}, protocol.Capability_BoardObserve)
	r.add(peer, protocol.TaskID{}, protocol.Capability_BoardPeer)
	r.add(other, protocol.TaskID{}, protocol.Capability_BoardPeer)
	b := reachBoard(t, r)
	c := b.Attach(protocol.RunnerID{}, me, "h", "bash")

	// A question I asked on a group topic, and a peer's answer to it there.
	mine, _, err := b.Send("rr.q", []byte("q?"), protocol.RunnerID{}, me, "h", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	b.NoteDelivered(c, []RetainedMessage{
		{Topic: "build.events", FromTask: other, SenderKind: protocol.SenderKind_Agent},            // group chatter
		{Topic: SelfTopic(other), FromTask: tidN(6), SenderKind: protocol.SenderKind_Agent},        // observed DM
		{Topic: "rr.q", FromTask: peer, SenderKind: protocol.SenderKind_Agent, InReplyTo: mine},    // answer to me
		{Topic: SelfTopic(me) + ".side", FromTask: tidN(7), SenderKind: protocol.SenderKind_Agent}, // my side channel
	})
	if b.Publishable(c, SelfTopic(other)) {
		t.Error("reading a group topic made its poster a contact")
	}
	if b.Publishable(c, SelfTopic(tidN(6))) {
		t.Error("observing another task's direct message made its sender a contact")
	}
	if !b.Publishable(c, SelfTopic(peer)) {
		t.Error("a reply to my own message, delivered on a group topic, did not make its sender a contact")
	}
	if !b.Publishable(c, SelfTopic(tidN(7))) {
		t.Error("a message on my own side channel did not make its sender a contact")
	}
}

// The operator forgets one route, or every route, of a task's contacts. A
// forgotten route closes again for a task without board_peer.
func TestClearContactsOneAndAll(t *testing.T) {
	r := newFakeReach()
	me, p1, p2 := tidN(2), tidN(4), tidN(5)
	r.add(me, protocol.TaskID{}, 0)
	b := reachBoard(t, r)
	c := b.Attach(protocol.RunnerID{}, me, "h", "bash")
	b.NoteDelivered(c, []RetainedMessage{
		{Topic: SelfTopic(me), FromTask: p1, SenderKind: protocol.SenderKind_Agent},
		{Topic: SelfTopic(me), FromTask: p2, SenderKind: protocol.SenderKind_Agent},
	})

	if n, found := b.ClearContacts(me, SelfTopic(p1)); !found || n != 1 {
		t.Fatalf("clear one = %d, %v; want 1, true", n, found)
	}
	if b.Publishable(c, SelfTopic(p1)) {
		t.Error("a forgotten contact still opens its route")
	}
	if !b.Publishable(c, SelfTopic(p2)) {
		t.Error("clearing one contact took another with it")
	}
	if n, found := b.ClearContacts(me, SelfTopic(p1)); !found || n != 0 {
		t.Errorf("clearing a route that is not a contact = %d, %v; want 0, true", n, found)
	}
	if n, found := b.ClearContacts(me, ""); !found || n != 1 {
		t.Errorf("clear all = %d, %v; want 1, true", n, found)
	}
	if got := b.Contacts(c); len(got) != 0 {
		t.Errorf("contacts after clear all = %v", got)
	}
	if _, found := b.ClearContacts(tidN(9), ""); found {
		t.Error("a task the board holds no state for reported found")
	}
}

// A plain inbox re-reads the whole ring, so the message that opened a route is
// handed over again after the operator forgot it. Forgetting must hold until
// a message published AFTER it arrives — found driving a dummy harness, where
// the next `agent inbox` brought the route straight back.
func TestForgottenContactIsNotRevivedByARetainedMessage(t *testing.T) {
	r := newFakeReach()
	me, peer := tidN(2), tidN(4)
	r.add(me, protocol.TaskID{}, 0)
	b := reachBoard(t, r)
	c := b.Attach(protocol.RunnerID{}, me, "h", "bash")
	if err := b.Subscribe(c, SelfTopic(me)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Send(SelfTopic(me), []byte("hi"), protocol.RunnerID{}, peer, "h", "", 0, WithNoWake()); err != nil {
		t.Fatal(err)
	}
	read := func() {
		msgs, _ := b.Inbox(c, 0)
		b.NoteDelivered(c, msgs)
	}
	read()
	if !b.Publishable(c, SelfTopic(peer)) {
		t.Fatal("the first delivery did not open the route")
	}
	for _, topic := range []string{SelfTopic(peer), ""} { // one route, then all
		if _, found := b.ClearContacts(me, topic); !found {
			t.Fatal("task not found")
		}
		read() // the same retained message, handed over again
		if b.Publishable(c, SelfTopic(peer)) {
			t.Fatalf("clear(%q): re-reading a message older than the clear revived the route", topic)
		}
	}
	if _, _, err := b.Send(SelfTopic(me), []byte("again"), protocol.RunnerID{}, peer, "h", "", 0, WithNoWake()); err != nil {
		t.Fatal(err)
	}
	read()
	if !b.Publishable(c, SelfTopic(peer)) {
		t.Error("a message published after the clear did not reopen the route")
	}
}
