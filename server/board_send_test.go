package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestBoardPublish_StampsOperatorAndPrincipal(t *testing.T) {
	h, _ := newBoardTestHandler(t)
	var by protocol.TaskID
	by.Id[0] = 5
	seq, _, st := h.boardPublish(by, "chat.op1", []byte("hi"), 0, "", false, false)
	if st != protocol.SendStatus_Ok {
		t.Fatalf("status = %v", st)
	}
	m, _ := h.Board.Retained(seq)
	if m.SenderKind != protocol.SenderKind_Operator || m.FromTask != by {
		t.Fatalf("kind=%v from=%x, want operator/%x", m.SenderKind, m.FromTask.Id, by.Id)
	}
}

func TestBoardPublish_RepliesToAgentWithoutTopic(t *testing.T) {
	h, _ := newBoardTestHandler(t)
	var agentTid protocol.TaskID
	agentTid.Id[0] = 0xab
	parent, _, _ := h.Board.Send("shared", []byte("q"), protocol.RunnerID{}, agentTid, "h", "claude", 0)
	seq, _, st := h.boardPublish(protocol.TaskID{}, "", []byte("a"), parent, agentboard.OperatorTopic, false, false)
	if st != protocol.SendStatus_Ok {
		t.Fatalf("status = %v", st)
	}
	m, _ := h.Board.Retained(seq)
	if m.Topic != agentboard.SelfTopic(agentTid) {
		t.Errorf("reply landed on %q, want the author's chat topic", m.Topic)
	}
	if m.ReplyToTopic != agentboard.OperatorTopic {
		t.Errorf("ReplyToTopic = %q, want %q", m.ReplyToTopic, agentboard.OperatorTopic)
	}
	// An operator reply never retires the parent: the operator has no
	// chat.<short-id> for the parent to sit on.
	if p, ok := h.Board.Retained(parent); !ok || p.Seq != parent {
		t.Errorf("parent %d was retired by an operator reply", parent)
	}
}

func TestResolveReplyTarget_OperatorParentWithoutRouteIsRefused(t *testing.T) {
	h, _ := newBoardTestHandler(t)
	parent, _, _ := h.Board.Send("chat.abababab", []byte("from op"), protocol.RunnerID{}, protocol.TaskID{}, "", "", 0,
		agentboard.WithSenderKind(protocol.SenderKind_Operator))
	if _, st := resolveReplyTarget(h.Board, "", parent); st != protocol.SendStatus_NoReplyRoute {
		t.Fatalf("status = %v, want no_reply_route", st)
	}
	// An explicit topic is unchanged.
	if dest, st := resolveReplyTarget(h.Board, "elsewhere", parent); st != protocol.SendStatus_Ok || dest != "elsewhere" {
		t.Fatalf("explicit topic = %q/%v", dest, st)
	}
}

func TestResolveReplyTarget_OperatorParentWithRouteGoesThere(t *testing.T) {
	h, _ := newBoardTestHandler(t)
	parent, _, _ := h.Board.Send("chat.abababab", []byte("from op"), protocol.RunnerID{}, protocol.TaskID{}, "", "", 0,
		agentboard.WithSenderKind(protocol.SenderKind_Operator), agentboard.WithReplyTo(agentboard.OperatorTopic))
	dest, st := resolveReplyTarget(h.Board, "", parent)
	if st != protocol.SendStatus_Ok || dest != agentboard.OperatorTopic {
		t.Fatalf("dest = %q/%v, want chat.operator/ok", dest, st)
	}
}

func TestResolveReplyTarget_ServerParentWithoutRouteIsRefused(t *testing.T) {
	h, _ := newBoardTestHandler(t)
	var requester protocol.TaskID
	requester.Id[0] = 3
	parent, _, _ := h.Board.Send("chat.03000000", []byte(`{"kind":"session_idle"}`), protocol.RunnerID{}, requester, "server", "", 0,
		agentboard.WithSenderKind(protocol.SenderKind_Server))
	if _, st := resolveReplyTarget(h.Board, "", parent); st != protocol.SendStatus_NoReplyRoute {
		t.Fatalf("status = %v, want no_reply_route", st)
	}
}

func TestHandleBoardWake_ReportsWoken(t *testing.T) {
	h, conn := newBoardTestHandler(t)
	h.handleBoardWake(conn, 7, "chat.none")
	resp := lastTaskControlResponse(t, conn)
	w := resp.BoardWake()
	if resp.Kind != protocol.TaskControlKind_BoardWake || w == nil || w.Woken != 0 {
		t.Fatalf("resp = %v %+v, want board_wake woken=0", resp.Kind, w)
	}
}

func TestBoardSendAndWakeNeedBoardSend(t *testing.T) {
	for _, k := range []protocol.TaskControlKind{protocol.TaskControlKind_BoardSend, protocol.TaskControlKind_BoardWake} {
		if got := requiredCap[k]; got != protocol.Capability_BoardSend {
			t.Errorf("%v required cap = %v, want board_send", k, got)
		}
	}
}

func TestFireIdleBoardStampsServer(t *testing.T) {
	h, _ := newBoardTestHandler(t)
	h.fireIdleBoard("chat.1d1e0000", "00", protocol.TaskID{}, false, 0)
	rows, _ := h.Board.ListRetained("chat.1d1e0000")
	if len(rows) != 1 || rows[0].SenderKind != protocol.SenderKind_Server {
		t.Fatalf("rows = %+v, want one server-kind message", rows)
	}
}

func TestHandleBoardRead_CarriesSenderKind(t *testing.T) {
	h, conn := newBoardTestHandler(t)
	if _, _, err := h.Board.Send("chat.sk", []byte("op"), protocol.RunnerID{}, protocol.TaskID{}, "", "", 0,
		agentboard.WithSenderKind(protocol.SenderKind_Operator)); err != nil {
		t.Fatal(err)
	}
	h.handleBoardRead(conn, 1, "chat.sk")
	br := boardReadResp(t, conn)
	if br == nil || len(br.Msgs) != 1 || br.Msgs[0].SenderKind != protocol.SenderKind_Operator {
		t.Fatalf("board read = %+v, want one operator row", br)
	}
}
