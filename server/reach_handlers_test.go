package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// reachHandler is newAgentTaskHandler with a real store behind the board's
// reach check, the way SetBoard wires it.
func reachHandler(t *testing.T, caps protocol.Capability) (*TaskHandler, *fakeConn, *agentboard.ConnState, protocol.TaskID) {
	t.Helper()
	h, conn := newBoardTestHandler(t)
	h.Board.SetReach(storeReach{h.Tasks})
	idHex := h.Tasks.Create("repo", "", protocol.TaskKind_Interactive, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, caps, Scope{}, "bash")
	tid := hexToTaskID(t, idHex)
	st := h.Board.Attach(protocol.RunnerID{}, tid, "h", "bash")
	h.BoardConnState = func(ConnHandle) *agentboard.ConnState { return st }
	return h, conn, st, tid
}

func TestStoreReach(t *testing.T) {
	s := NewTaskStore()
	parentHex := s.Create("r", "", protocol.TaskKind_Interactive, protocol.ClientKind_Agent, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, protocol.Capability_BoardPeer, Scope{}, "bash")
	parent := hexToTaskID(t, parentHex)
	childHex := s.Create("r", "", protocol.TaskKind_Interactive, protocol.ClientKind_Agent, parent, "",
		protocol.RunnerSelector{}, nil, 0, Scope{}, "bash")
	child := hexToTaskID(t, childHex)
	r := storeReach{s}
	if c, ok := r.CreatorOf(child); !ok || c != parent {
		t.Errorf("CreatorOf(child) = %x, %v", c.Id, ok)
	}
	if caps, ok := r.CapsOf(parent); !ok || caps != protocol.Capability_BoardPeer {
		t.Errorf("CapsOf(parent) = %v, %v", caps, ok)
	}
	if !r.HasChild(parent, childHex[:8]) || r.HasChild(child, parentHex[:8]) {
		t.Error("HasChild got the direction wrong")
	}
	if _, ok := r.CreatorOf(protocol.TaskID{Id: [16]byte{0xee}}); ok {
		t.Error("an unknown task resolved")
	}
	if r.Len() != 2 {
		t.Errorf("Len = %d, want 2", r.Len())
	}
}

func TestSetBoardWiresReach(t *testing.T) {
	s := New(Config{})
	b := newTestBoard(t)
	s.SetBoard(b)
	c := b.Attach(protocol.RunnerID{}, protocol.TaskID{Id: [16]byte{1}}, "h", "bash")
	if b.Readable(c, agentboard.SelfTopic(protocol.TaskID{Id: [16]byte{2}})) {
		t.Error("SetBoard did not install a reach source: an unknown task read a peer's topic")
	}
}

func TestAgentSubscribeAnswersNotPermitted(t *testing.T) {
	h, conn, _, _ := reachHandler(t, 0)
	h.handleAgentSubscribe(conn, 1, "chat.deadbeef", false)
	resp := lastTaskControlResponse(t, conn)
	if got := resp.AgentSubscribe(); got == nil || got.Status != protocol.SubscribeStatus_NotPermitted {
		t.Fatalf("status = %+v, want not_permitted", got)
	}
}

func TestAgentWaitAnswersNotPermitted(t *testing.T) {
	h, conn, _, _ := reachHandler(t, 0)
	r := &protocol.AgentWaitRequest{TimeoutMs: 50}
	r.SetPattern([]byte(agentboard.OperatorTopic))
	h.handleAgentWait(conn, 1, r)
	resp := lastTaskControlResponse(t, conn)
	got := resp.AgentWait()
	if got == nil || got.Status != protocol.WaitStatus_NotPermitted || got.TimedOut != 0 {
		t.Fatalf("wait = %+v, want not_permitted and not timed out", got)
	}
}

// D15: on a topic the caller cannot read, list_retained lists only what the
// caller published there, and says so.
func TestAgentListRetainedOnAPeerTopicListsOwnRowsOnly(t *testing.T) {
	h, conn, _, me := reachHandler(t, 0)
	var other protocol.TaskID
	other.Id[0] = 0x77
	h.Board.Send("chat.deadbeef", []byte("mine"), protocol.RunnerID{}, me, "h", "", 0)
	h.Board.Send("chat.deadbeef", []byte("theirs"), protocol.RunnerID{}, other, "h", "", 0)
	r := &protocol.AgentListRetainedRequest{}
	r.SetTopic([]byte("chat.deadbeef"))
	h.handleAgentListRetained(conn, 1, r)
	resp := lastTaskControlResponse(t, conn)
	got := resp.AgentListRetained()
	if got == nil || got.Status != protocol.BoardStatus_Ok || !got.OwnOnly() {
		t.Fatalf("list_retained = %+v, want ok with own_only", got)
	}
	var body protocol.RetainedBody
	conn.streamBody(t, got.StreamId, &body)
	if len(body.Metas) != 1 || body.Metas[0].FromTask != me {
		t.Fatalf("metas = %+v, want only the caller's one row", body.Metas)
	}
}

// On its own topic the list is whole, and own_only stays 0.
func TestAgentListRetainedOnItsOwnTopicIsWhole(t *testing.T) {
	h, conn, _, me := reachHandler(t, 0)
	var other protocol.TaskID
	other.Id[0] = 0x77
	h.Board.Send(agentboard.SelfTopic(me), []byte("x"), protocol.RunnerID{}, other, "h", "", 0)
	r := &protocol.AgentListRetainedRequest{}
	r.SetTopic([]byte(agentboard.SelfTopic(me)))
	h.handleAgentListRetained(conn, 1, r)
	resp := lastTaskControlResponse(t, conn)
	got := resp.AgentListRetained()
	if got == nil || got.OwnOnly() {
		t.Fatalf("list_retained = %+v, want a whole list", got)
	}
	var body protocol.RetainedBody
	conn.streamBody(t, got.StreamId, &body)
	if len(body.Metas) != 1 {
		t.Fatalf("metas = %d, want 1", len(body.Metas))
	}
}

// D4: the check runs on the topic the reply resolves to. A seq on someone
// else's topic, never handed to the replier, resolves to a route the replier
// has no contact for.
func TestAgentSendRefusesAGuessedReplyRoute(t *testing.T) {
	h, _, st, _ := reachHandler(t, 0)
	var stranger protocol.TaskID
	stranger.Id[0] = 0x77
	seq, _, err := h.Board.Send("chat.elsewhere", []byte("q"), protocol.RunnerID{}, stranger, "h", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	dest, status := resolveReplyTarget(h.Board, "", seq)
	if status != protocol.SendStatus_Ok || dest != agentboard.SelfTopic(stranger) {
		t.Fatalf("resolve = %q, %v", dest, status)
	}
	if h.Board.Publishable(st, dest) {
		t.Fatal("a reply to a never-delivered seq passed the reach check")
	}
	// Handed to the replier, the same route is a contact.
	h.Board.NoteDelivered(st, []agentboard.RetainedMessage{{FromTask: stranger, SenderKind: protocol.SenderKind_Agent}})
	if !h.Board.Publishable(st, dest) {
		t.Error("a reply to a delivered message was refused")
	}
}

// The rows wait / inbox / inbox_advance / read_seq hand over become contacts.
func TestDeliveredRowsBecomeContacts(t *testing.T) {
	h, conn, st, me := reachHandler(t, 0)
	var peer protocol.TaskID
	peer.Id[0] = 0x55
	// Attach does not seed chat.<id> (RegisterTask does); subscribe it here.
	if err := h.Board.Subscribe(st, agentboard.SelfTopic(me)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.Board.Send(agentboard.SelfTopic(me), []byte("hi"), protocol.RunnerID{}, peer, "h", "", 0); err != nil {
		t.Fatal(err)
	}
	if h.Board.Publishable(st, agentboard.SelfTopic(peer)) {
		t.Fatal("peer reachable before its message was read")
	}
	h.handleAgentInboxAdvance(conn, 1)
	if !h.Board.Publishable(st, agentboard.SelfTopic(peer)) {
		t.Error("reading the peer's message through inbox_advance did not make it a contact")
	}
}
