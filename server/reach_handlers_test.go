package server

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// reachHandler is newAgentTaskHandler with a real store behind the board's
// reach check, the way SetBoard wires it.
func reachHandler(t *testing.T, caps protocol.Capability) (*TaskHandler, *fakeConn, *agentboard.ConnState, protocol.TaskID) {
	t.Helper()
	h, conn := newBoardTestHandler(t)
	h.Board.SetReach(storeReach{h.Tasks})
	idHex := h.Tasks.Create("repo", "", protocol.TaskKind_Interactive, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, caps, authority.Scope{}, "bash")
	tid := hexToTaskID(t, idHex)
	st := h.Board.Attach(protocol.RunnerID{}, tid, "h", "bash")
	h.BoardConnState = func(ConnHandle) *agentboard.ConnState { return st }
	return h, conn, st, tid
}

func TestStoreReach(t *testing.T) {
	s := NewTaskStore()
	parentHex := s.Create("r", "", protocol.TaskKind_Interactive, protocol.ClientKind_Agent, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, protocol.Capability_BoardPeer, authority.Scope{}, "bash")
	parent := hexToTaskID(t, parentHex)
	childHex := s.Create("r", "", protocol.TaskKind_Interactive, protocol.ClientKind_Agent, parent, "",
		protocol.RunnerSelector{}, nil, 0, authority.Scope{}, "bash")
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

// Reach refusals are the ordinary capability denial (reach spec D14, as
// reversed): one response every surface already renders.
func TestAgentSubscribeIsDeniedBoardObserve(t *testing.T) {
	h, conn, _, _ := reachHandler(t, 0)
	h.handleAgentSubscribe(conn, 7, "chat.deadbeef", false)
	assertPermissionDenied(t, conn, 7, protocol.Capability_BoardObserve)
}

func TestAgentWaitIsDeniedBoardObserve(t *testing.T) {
	h, conn, _, _ := reachHandler(t, 0)
	r := &protocol.AgentWaitRequest{RequestId: 8, TimeoutMs: 50}
	r.SetPattern([]byte(agentboard.OperatorTopic))
	h.handleAgentWait(conn, 8, r)
	assertPermissionDenied(t, conn, 8, protocol.Capability_BoardObserve)
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
	h, _, st, me := reachHandler(t, 0)
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
	// Addressed to the replier and handed to it, the same route is a contact.
	h.Board.NoteDelivered(st, []agentboard.RetainedMessage{{Topic: agentboard.SelfTopic(me), FromTask: stranger, SenderKind: protocol.SenderKind_Agent}})
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

// sendThroughHandler drives handleAgentSend with a one-byte body and waits for
// its answer, which the handler sends from its own goroutine.
func sendThroughHandler(t *testing.T, h *TaskHandler, conn *fakeConn, requestID uint32, r *protocol.AgentSendRequest) protocol.TaskControlResponse {
	t.Helper()
	before := len(conn.Sent())
	r.RequestId = requestID
	r.PayloadStreamId = 1
	rc := &recvStreamConn{fakeConn: conn, st: &countingRecvStream{remaining: 1}}
	h.handleAgentSend(rc, requestID, r)
	deadline := time.Now().Add(2 * time.Second)
	for len(conn.Sent()) == before {
		if time.Now().After(deadline) {
			t.Fatal("handleAgentSend sent no response")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return lastTaskControlResponse(t, conn)
}

// A declared reply route becomes the recipient's contact (reach spec §3), so a
// sender may only declare one it could publish to itself. Otherwise a task
// with no bits sends itself a message with --reply-to <anything>, reads it,
// and holds that topic as a contact: board_peer bypassed alone.
func TestAgentSendRefusesAReplyToBeyondItsOwnReach(t *testing.T) {
	h, conn, st, me := reachHandler(t, 0)
	if err := h.Board.Subscribe(st, agentboard.SelfTopic(me)); err != nil {
		t.Fatal(err)
	}
	for _, victim := range []string{"chat.deadbeef", "build.events"} {
		r := &protocol.AgentSendRequest{}
		r.SetTopic([]byte(agentboard.SelfTopic(me)))
		r.SetReplyToTopic([]byte(victim))
		resp := sendThroughHandler(t, h, conn, 9, r)
		if resp.Kind != protocol.TaskControlKind_PermissionDenied || resp.PermissionDenied().RequiredCap != protocol.Capability_BoardPeer {
			t.Errorf("self-send with reply_to %s answered %v, want the board_peer denial", victim, resp.Kind)
		}
		h.handleAgentInboxAdvance(conn, 10)
		if h.Board.Publishable(st, victim) {
			t.Errorf("%s became reachable through a self-declared reply route", victim)
		}
	}
	// Declaring a route the sender can reach itself is fine: its own side channel.
	r := &protocol.AgentSendRequest{}
	r.SetTopic([]byte(agentboard.SelfTopic(me)))
	r.SetReplyToTopic([]byte(agentboard.SelfTopic(me) + ".side"))
	if resp := sendThroughHandler(t, h, conn, 11, r); resp.Kind != protocol.TaskControlKind_AgentSend || resp.AgentSend().Status != protocol.SendStatus_Ok {
		t.Errorf("self-send with reply_to on its own side channel answered %v", resp.Kind)
	}
}

// await-idle's board sink publishes a session_idle row stamped with the
// requester and wakes the topic's subscribers. It is a publish, so it obeys
// the requester's reach like agent send does; an operator connection (no
// principal task) is not reach-checked, like the rest of the operator face.
func TestAwaitIdleBoardSinkObeysTheRequestersReach(t *testing.T) {
	h, conn, _, me := reachHandler(t, 0)
	h.principals = map[string]protocol.TaskID{conn.ConnectionID().String(): me}

	arm := func(topic string) protocol.TaskControlResponse {
		t.Helper()
		req := protocol.AwaitIdleRequest{TaskId: me, ThresholdMs: 50, Sink: protocol.AwaitSink_Board}
		req.SetTopic([]byte(topic))
		tcr := protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AwaitIdle, RequestId: 7}
		tcr.SetAwaitIdle(req)
		before := len(conn.Sent())
		h.Handle(conn, tcr.MustAppend(nil))
		waitFor(t, func() bool { return len(conn.Sent()) > before })
		return lastTaskControlResponse(t, conn)
	}

	if resp := arm("chat.deadbeef"); resp.Kind != protocol.TaskControlKind_PermissionDenied ||
		resp.PermissionDenied().RequiredCap != protocol.Capability_BoardPeer {
		t.Errorf("arming a board sink on a peer's topic answered %v, want the board_peer denial", resp.Kind)
	}
	// Its own topic is in reach; the arm then proceeds (and finds no session).
	if resp := arm(agentboard.SelfTopic(me)); resp.Kind == protocol.TaskControlKind_PermissionDenied {
		t.Error("arming a board sink on its own topic was denied")
	}
}

// D16: board_peer opens what its scope covers, by the same rules every other
// bit follows — the task's scope, narrowed per bit by --scope-for.
func TestBoardPeerFollowsScope(t *testing.T) {
	h, _, st, me := reachHandler(t, protocol.Capability_BoardPeer)
	meHex := hex.EncodeToString(me.Id[:])
	mk := func(creator protocol.TaskID) (string, protocol.TaskID) {
		idHex := h.Tasks.Create("repo", "", protocol.TaskKind_Interactive, protocol.ClientKind_Agent,
			creator, "", protocol.RunnerSelector{}, nil, 0, authority.Scope{}, "bash")
		return idHex, hexToTaskID(t, idHex)
	}
	peerHex, peer := mk(protocol.TaskID{})
	_, child := mk(me)
	_, grandchild := mk(child)
	can := func(topic string) bool { return h.Board.Publishable(st, topic) }

	// The default scope is subtree: beyond the base set, only the subtree.
	if !can(agentboard.SelfTopic(grandchild)) {
		t.Error("subtree board_peer did not open a grandchild")
	}
	if can(agentboard.SelfTopic(peer)) || can("build.events") {
		t.Error("subtree board_peer opened a peer or a free-form topic")
	}

	// --scope-for board_peer=none+ids:<peer>: that peer, nothing free-form.
	setScope(t, h, meHex, authority.Scope{Base: protocol.ScopeBase_Subtree, Overrides: []authority.ScopeOverride{{
		Caps: protocol.Capability_BoardPeer, Base: protocol.ScopeBase_None, IDs: []string{peerHex},
	}}})
	if !can(agentboard.SelfTopic(peer)) || !can(agentboard.SelfTopic(peer)+".side") {
		t.Error("board_peer scoped to the peer did not open its chat.<id>")
	}
	if can(agentboard.SelfTopic(grandchild)) || can("build.events") {
		t.Error("board_peer scoped to one peer opened something else")
	}

	// --scope-for board_peer=global: today's reach.
	setScope(t, h, meHex, authority.Scope{Base: protocol.ScopeBase_Subtree, Overrides: []authority.ScopeOverride{{
		Caps: protocol.Capability_BoardPeer, Base: protocol.ScopeBase_Global,
	}}})
	if !can(agentboard.SelfTopic(peer)) || !can("build.events") {
		t.Error("global board_peer did not open a peer and a free-form topic")
	}
}
