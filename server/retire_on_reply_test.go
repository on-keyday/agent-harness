package server

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// retireFixture builds a board plus the two identities the rule is about: an
// author who sends an instruction, and the recipient who answers it.
type retireFixture struct {
	srv       *Server
	board     *agentboard.Board
	author    protocol.TaskID
	recipient protocol.TaskID
	selfTopic string
}

func newRetireFixture(t *testing.T) *retireFixture {
	t.Helper()
	b := agentboard.New(agentboard.Config{RingN: 8, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	t.Cleanup(b.Close)
	var author, recipient protocol.TaskID
	author.Id[0] = 0xA1
	recipient.Id[0] = 0xB2
	return &retireFixture{
		srv:       &Server{Board: b},
		board:     b,
		author:    author,
		recipient: recipient,
		selfTopic: agentboard.SelfTopic(recipient),
	}
}

func (f *retireFixture) send(t *testing.T, topic string, opts ...agentboard.SendOption) uint64 {
	t.Helper()
	seq, _, err := f.board.Send(topic, []byte("do the thing"), protocol.RunnerID{}, f.author, "h", "", 0, opts...)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	return seq
}

func (f *retireFixture) live(topic string) int {
	msgs, _ := f.board.ListRetained(topic)
	return len(msgs)
}

// TestRetireOnReply_PointToPoint is the intended path: an instruction sent to a
// task's own inbound topic is withdrawn the moment that task answers it, with
// nobody having to remember to retract.
func TestRetireOnReply_PointToPoint(t *testing.T) {
	f := newRetireFixture(t)
	seq := f.send(t, f.selfTopic)

	f.srv.retireRepliedParent(seq, f.recipient)

	if n := f.live(f.selfTopic); n != 0 {
		t.Errorf("live ring = %d after the recipient replied, want 0", n)
	}
	withdrawn, _ := f.board.ListRetracted(f.selfTopic)
	if len(withdrawn) != 1 {
		t.Fatalf("withdrawn = %d, want 1 (kept for the operator)", len(withdrawn))
	}
	if withdrawn[0].RetractedAt.IsZero() {
		t.Error("RetractedAt not stamped by the reply-retire path")
	}
}

// TestRetireOnReply_OptOut: an author that asked for the message to survive
// being answered keeps it, which is the whole point of the negative bit.
func TestRetireOnReply_OptOut(t *testing.T) {
	f := newRetireFixture(t)
	seq := f.send(t, f.selfTopic, agentboard.NoRetireOnReply())

	f.srv.retireRepliedParent(seq, f.recipient)

	if n := f.live(f.selfTopic); n != 1 {
		t.Errorf("live ring = %d, want the opted-out message still there", n)
	}
}

// TestRetireOnReply_SharedTopicNeverFires guards the decision that cost the
// most thought: one subscriber answering says nothing about whether the others
// have read it, so a shared topic is never auto-retired however the flag is
// set. Those senders retract explicitly.
func TestRetireOnReply_SharedTopicNeverFires(t *testing.T) {
	f := newRetireFixture(t)
	seq := f.send(t, "t.broadcast")

	f.srv.retireRepliedParent(seq, f.recipient)

	if n := f.live("t.broadcast"); n != 1 {
		t.Errorf("live ring = %d, want a shared-topic message untouched by one reply", n)
	}
	// The author can still withdraw it deliberately.
	if _, ok := f.board.RetractSeq(seq, f.author); !ok {
		t.Error("explicit retract must still work on a shared topic")
	}
}

// TestRetireOnReply_SelfReplyDoesNotFire: a task answering itself on its own
// topic must not erase its own message.
func TestRetireOnReply_SelfReplyDoesNotFire(t *testing.T) {
	f := newRetireFixture(t)
	// Author publishes to its OWN inbound topic, then "replies" to itself.
	ownTopic := agentboard.SelfTopic(f.author)
	seq, _, err := f.board.Send(ownTopic, []byte("note to self"), protocol.RunnerID{}, f.author, "h", "", 0)
	if err != nil {
		t.Fatal(err)
	}

	f.srv.retireRepliedParent(seq, f.author)

	if n := f.live(ownTopic); n != 1 {
		t.Errorf("live ring = %d, want a self-reply to leave the message alone", n)
	}
}

// TestRetireOnReply_UnknownParentIsNoOp: a parent already withdrawn, purged or
// rotated out is nothing to do — not an error, and not something that may
// disturb any other message.
func TestRetireOnReply_UnknownParentIsNoOp(t *testing.T) {
	f := newRetireFixture(t)
	seq := f.send(t, f.selfTopic)

	f.srv.retireRepliedParent(seq+9999, f.recipient)
	if n := f.live(f.selfTopic); n != 1 {
		t.Errorf("live ring = %d after retiring an unknown seq, want 1 untouched", n)
	}

	// Idempotent: retiring twice is the second one finding nothing live.
	f.srv.retireRepliedParent(seq, f.recipient)
	f.srv.retireRepliedParent(seq, f.recipient)
	if withdrawn, _ := f.board.ListRetracted(f.selfTopic); len(withdrawn) != 1 {
		t.Errorf("withdrawn = %d after two retires, want exactly 1", len(withdrawn))
	}

	// A zero replier id (an unauthenticated connection) matches nobody.
	seq2 := f.send(t, f.selfTopic)
	f.srv.retireRepliedParent(seq2, protocol.TaskID{})
	if n := f.live(f.selfTopic); n != 1 {
		t.Errorf("live ring = %d, want a zero-id replier to retire nothing", n)
	}
}
