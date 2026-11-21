package agentboard

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// AttachTicketless (credential-tasks D10): the attach that creates the state
// seeds the task's chat.<id> and its member topics as RegisterTask would; a
// later attach seeds nothing; nothing is ever woken for the state; Revoke
// drops it and the next attach seeds afresh.
func TestAttachTicketlessSeedsInboundAndNeverWakes(t *testing.T) {
	r := newFakeReach()
	me := tidN(1)
	r.add(me, protocol.TaskID{}, 0)
	b := reachBoard(t, r)
	if err := b.Join(me, "grp"); err != nil {
		t.Fatal(err)
	}
	var woken []protocol.TaskID
	b.SetOnDeliver(func(_ protocol.RunnerID, tid protocol.TaskID) { woken = append(woken, tid) })

	c := b.AttachTicketless(protocol.RunnerID{}, me)
	if !subscribed(b, c, SelfTopic(me)) || !subscribed(b, c, "grp") {
		t.Fatalf("first attach subscribed %v, want its chat and its member topic", b.ListSubscriptions(c))
	}

	if _, delivered, err := b.Send(SelfTopic(me), []byte("x"), protocol.RunnerID{}, protocol.TaskID{}, "", "", 0); err != nil || delivered != 1 {
		t.Fatalf("send to its chat: delivered %d, err %v", delivered, err)
	}
	if n := b.Wake(SelfTopic(me)); n != 0 {
		t.Errorf("Wake counted %d for a state with no process", n)
	}
	if len(woken) != 0 {
		t.Errorf("onDeliver called for a state with no process: %v", woken)
	}

	b.Unsubscribe(c, SelfTopic(me))
	again := b.AttachTicketless(protocol.RunnerID{}, me)
	if subscribed(b, again, SelfTopic(me)) {
		t.Error("a later attach re-seeded a topic the task unsubscribed from")
	}

	b.Revoke(protocol.RunnerID{}, me)
	fresh := b.AttachTicketless(protocol.RunnerID{}, me)
	if !subscribed(b, fresh, SelfTopic(me)) {
		t.Error("the attach after Revoke did not seed the task's chat")
	}
}

// A credential member's own publish on a member topic is answered there, as
// an agent member's is (PublishedByTask).
func TestCredentialMemberPublishIsAnsweredOnTheTopic(t *testing.T) {
	r := newFakeReach()
	me := tidN(1)
	r.add(me, protocol.TaskID{}, 0)
	b := reachBoard(t, r)
	if err := b.Join(me, "grp"); err != nil {
		t.Fatal(err)
	}
	seq, _, err := b.Send("grp", []byte("x"), protocol.RunnerID{}, me, "", "", 0, WithSenderKind(protocol.SenderKind_Credential))
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := b.Retained(seq); !ok || m.ReplyToTopic != "grp" {
		t.Fatalf("reply_to_topic %q, want grp", m.ReplyToTopic)
	}
}
