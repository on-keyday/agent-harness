package agentboard

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func subscribed(b *Board, c *ConnState, topic string) bool {
	return slices.Contains(b.ListSubscriptions(c), topic)
}

func TestMembership_JoinGrantsReadWriteOnOneTopic(t *testing.T) {
	r := newFakeReach()
	me := tidN(1)
	r.add(me, protocol.TaskID{}, 0)
	b := reachBoard(t, r)
	c := b.Attach(protocol.RunnerID{}, me, "h", "bash")
	if b.Publishable(c, "grp") {
		t.Fatal("a task with no bit may publish to a free-form topic before joining")
	}
	if err := b.Join(me, "grp"); err != nil {
		t.Fatal(err)
	}
	if !b.Publishable(c, "grp") || !b.Readable(c, "grp") {
		t.Errorf("member of grp: Publishable %v, Readable %v; want both", b.Publishable(c, "grp"), b.Readable(c, "grp"))
	}
	if b.Publishable(c, "other") {
		t.Error("joining grp opened another free-form topic")
	}
	if !b.PublishableBy(me, "grp") {
		t.Error("PublishableBy ignores membership")
	}
	if !subscribed(b, c, "grp") || !b.IsMember(me, "grp") {
		t.Error("join did not subscribe the task / record it")
	}
}

func TestMembership_RefusesChatAndForeignTree(t *testing.T) {
	r := newFakeReach()
	root, me, other := tidN(1), tidN(2), tidN(3)
	r.add(root, protocol.TaskID{}, 0)
	r.add(me, root, protocol.Capability_BoardObserve)
	r.add(other, protocol.TaskID{}, 0)
	b := reachBoard(t, r)
	for _, topic := range []string{"chat.x", SelfTopic(other), OperatorTopic, "", treeOf(other)} {
		if err := b.Join(me, topic); !errors.Is(err, ErrNotJoinable) {
			t.Errorf("Join(%q) = %v, want ErrNotJoinable", topic, err)
		}
	}
	if err := b.Join(me, treeOf(root)); err != nil {
		t.Errorf("Join of the task's own lineage tree = %v", err)
	}
}

func TestMembership_SubscribeTaskGrantsNothing(t *testing.T) {
	r := newFakeReach()
	me, other := tidN(1), tidN(2)
	r.add(me, protocol.TaskID{}, 0)
	r.add(other, protocol.TaskID{}, 0)
	b := reachBoard(t, r)
	c := b.Attach(protocol.RunnerID{}, me, "h", "bash")
	if err := b.SubscribeTask(me, "grp"); err != nil {
		t.Fatal(err)
	}
	if !subscribed(b, c, "grp") {
		t.Error("SubscribeTask did not subscribe")
	}
	if b.Publishable(c, "grp") || b.IsMember(me, "grp") {
		t.Error("SubscribeTask granted publishing / membership")
	}
	if err := b.SubscribeTask(me, SelfTopic(other)); !errors.Is(err, ErrNotPermitted) {
		t.Errorf("SubscribeTask on another task's chat = %v, want ErrNotPermitted", err)
	}
	if err := b.SubscribeTask(me, OperatorTopic); !errors.Is(err, ErrReservedTopic) {
		t.Errorf("SubscribeTask on chat.operator = %v, want ErrReservedTopic", err)
	}
	b.UnsubscribeTask(me, "grp")
	if subscribed(b, c, "grp") {
		t.Error("UnsubscribeTask left the subscription")
	}
}

func TestMembership_NotRunning(t *testing.T) {
	r := newFakeReach()
	me := tidN(1)
	r.add(me, protocol.TaskID{}, 0)
	b := reachBoard(t, r)
	if err := b.SubscribeTask(me, "grp"); !errors.Is(err, ErrNotRunning) {
		t.Errorf("SubscribeTask with no board state = %v, want ErrNotRunning", err)
	}
	if err := b.Join(me, "grp"); err != nil {
		t.Fatalf("Join with no board state = %v", err)
	}
	b.RegisterTask(protocol.RunnerID{}, me, [16]byte{}, "bash")
	c := b.Attach(protocol.RunnerID{}, me, "h", "bash")
	if !subscribed(b, c, "grp") {
		t.Error("RegisterTask did not subscribe a joined topic")
	}
}

func TestMembership_BothRunnerStates(t *testing.T) {
	r := newFakeReach()
	me := tidN(1)
	r.add(me, protocol.TaskID{}, 0)
	b := reachBoard(t, r)
	var ra, rb protocol.RunnerID
	ra.Id[0], rb.Id[0] = 1, 2
	ca := b.Attach(ra, me, "a", "bash")
	cb := b.Attach(rb, me, "b", "bash")
	if err := b.SubscribeTask(me, "sub"); err != nil {
		t.Fatal(err)
	}
	if err := b.Join(me, "grp"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []*ConnState{ca, cb} {
		if !subscribed(b, c, "sub") || !subscribed(b, c, "grp") {
			t.Errorf("a runner's state misses a subscription: %v", b.ListSubscriptions(c))
		}
	}
	b.Leave(me, "grp")
	for _, c := range []*ConnState{ca, cb} {
		if subscribed(b, c, "grp") {
			t.Error("Leave left a runner's state subscribed")
		}
	}
}

func TestMembership_SelfUnsubscribeLeaves(t *testing.T) {
	r := newFakeReach()
	me := tidN(1)
	r.add(me, protocol.TaskID{}, 0)
	b := reachBoard(t, r)
	c := b.Attach(protocol.RunnerID{}, me, "h", "bash")
	if err := b.Join(me, "grp"); err != nil {
		t.Fatal(err)
	}
	b.Unsubscribe(c, "grp")
	if b.IsMember(me, "grp") || b.Publishable(c, "grp") {
		t.Error("the task's own unsubscribe did not end its membership")
	}
}

func TestMembership_ReplyRouteDefault(t *testing.T) {
	b := New(Config{RingN: 8, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	t.Cleanup(b.Close)
	me, other := tidN(1), tidN(2)
	if err := b.Join(me, "grp"); err != nil {
		t.Fatal(err)
	}
	seq, _, _ := b.Send("grp", []byte("q"), protocol.RunnerID{}, me, "h", "", 0)
	if m, _ := b.Retained(seq); m.ReplyToTopic != "grp" {
		t.Errorf("member's publish reply_to = %q, want grp", m.ReplyToTopic)
	}
	seq, _, _ = b.Send("grp", []byte("q"), protocol.RunnerID{}, me, "h", "", 0, WithReplyTo(SelfTopic(me)))
	if m, _ := b.Retained(seq); m.ReplyToTopic != SelfTopic(me) {
		t.Errorf("explicit reply_to overridden: %q", m.ReplyToTopic)
	}
	seq, _, _ = b.Send("grp", []byte("q"), protocol.RunnerID{}, other, "h", "", 0)
	if m, _ := b.Retained(seq); m.ReplyToTopic != "" {
		t.Errorf("non-member's publish reply_to = %q, want empty", m.ReplyToTopic)
	}
}

func TestMembership_RetainedSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	me := tidN(1)
	b, s := newRetainBoard(t, Config{}, dir)
	if err := b.Retain("keep", Retention{Forever: true}); err != nil {
		t.Fatal(err)
	}
	for _, topic := range []string{"keep", "temp"} {
		if err := b.Join(me, topic); err != nil {
			t.Fatal(err)
		}
	}
	b.Close()
	_ = s.Close()

	b2, s2 := newRetainBoard(t, Config{}, dir)
	defer b2.Close()
	defer s2.Close()
	if !b2.IsMember(me, "keep") || b2.IsMember(me, "temp") {
		t.Fatalf("after restart: keep %v (want true), temp %v (want false)", b2.IsMember(me, "keep"), b2.IsMember(me, "temp"))
	}
	b2.RegisterTask(protocol.RunnerID{}, me, [16]byte{}, "bash")
	if c := b2.Attach(protocol.RunnerID{}, me, "h", "bash"); !subscribed(b2, c, "keep") {
		t.Error("re-registration did not subscribe the restored membership")
	}

	// A leave is persisted too: the task is not re-subscribed after the next restart.
	b2.Leave(me, "keep")
	b2.Close()
	_ = s2.Close()
	b3, s3 := newRetainBoard(t, Config{}, dir)
	defer b3.Close()
	defer s3.Close()
	if b3.IsMember(me, "keep") {
		t.Error("a leave on a retained topic did not survive the restart")
	}
}

func TestMembership_RetainAfterJoinPersists(t *testing.T) {
	dir := t.TempDir()
	me := tidN(1)
	b, s := newRetainBoard(t, Config{}, dir)
	if err := b.Join(me, "grp"); err != nil {
		t.Fatal(err)
	}
	if err := b.Retain("grp", Retention{Forever: true}); err != nil {
		t.Fatal(err)
	}
	b.Close()
	_ = s.Close()
	b2, s2 := newRetainBoard(t, Config{}, dir)
	defer b2.Close()
	defer s2.Close()
	if !b2.IsMember(me, "grp") {
		t.Error("a membership made before the retain was not written with it")
	}
}

func TestMembership_PurgeKeepsClearDrops(t *testing.T) {
	dir := t.TempDir()
	me := tidN(1)
	b, s := newRetainBoard(t, Config{}, dir)
	for _, topic := range []string{"purged", "cleared"} {
		if err := b.Retain(topic, Retention{Forever: true}); err != nil {
			t.Fatal(err)
		}
		if err := b.Join(me, topic); err != nil {
			t.Fatal(err)
		}
		publish(t, b, topic, "m")
	}
	b.PurgeTopic("purged")
	if err := b.ClearRetain("cleared"); err != nil {
		t.Fatal(err)
	}
	if !b.IsMember(me, "purged") || !b.IsMember(me, "cleared") {
		t.Error("purge / clear dropped the in-memory membership")
	}
	b.Close()
	_ = s.Close()
	b2, s2 := newRetainBoard(t, Config{}, dir)
	defer b2.Close()
	defer s2.Close()
	if !b2.IsMember(me, "purged") {
		t.Error("purging a retained topic lost its persisted membership")
	}
	if b2.IsMember(me, "cleared") {
		t.Error("clearing the retention kept the persisted membership")
	}
}

func TestListSubscribers_MarksJoined(t *testing.T) {
	b := New(Config{RingN: 8, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	t.Cleanup(b.Close)
	me := tidN(1)
	b.Attach(protocol.RunnerID{}, me, "h", "bash")
	if err := b.SubscribeTask(me, "sub"); err != nil {
		t.Fatal(err)
	}
	if err := b.Join(me, "grp"); err != nil {
		t.Fatal(err)
	}
	rows := b.ListSubscribers("")
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	joined := map[string]bool{}
	for _, p := range rows[0].Patterns {
		joined[p.Name] = p.Joined
	}
	if !joined["grp"] || joined["sub"] {
		t.Errorf("Joined = %v, want grp only", joined)
	}
}
