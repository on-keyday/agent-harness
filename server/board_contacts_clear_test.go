package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// board_contacts_clear names a task, so it is gated on the purge bit's scope as
// well as on the bit: a subtree-scoped caller forgets contacts of its own
// descendants and gets not_found — not permission_denied — for anyone else.
func TestBoardContactsClearIsScopeGated(t *testing.T) {
	h, _, c, g, u := scopeFixture(t)
	h.Board = newTestBoard(t)
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9702-1")}
	h.principals = map[string]protocol.TaskID{conn.ConnectionID().String(): hexToTaskID(t, c)}

	var peerA, peerB protocol.TaskID
	peerA.Id[0], peerB.Id[0] = 0xA1, 0xB2
	seed := func(taskHex string) *agentboard.ConnState {
		tid := hexToTaskID(t, taskHex)
		st := h.Board.Attach(protocol.RunnerID{}, tid, "h", "bash")
		h.Board.NoteDelivered(st, []agentboard.RetainedMessage{
			{Topic: agentboard.SelfTopic(tid), FromTask: peerA, SenderKind: protocol.SenderKind_Agent},
			{Topic: agentboard.SelfTopic(tid), FromTask: peerB, SenderKind: protocol.SenderKind_Agent},
		})
		return st
	}
	gst, ust := seed(g), seed(u)

	clear := func(t *testing.T, taskHex, topic string) protocol.BoardContactsClearResponse {
		t.Helper()
		req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_BoardContactsClear, RequestId: 1}
		r := protocol.BoardContactsClearRequest{Task: hexToTaskID(t, taskHex)}
		r.SetTopic([]byte(topic))
		req.SetBoardContactsClear(r)
		h.Handle(conn, encodeTaskControlRequest(t, req))
		resp := lastTaskControlResponse(t, conn)
		got := resp.BoardContactsClear()
		if got == nil {
			t.Fatalf("response kind %v, want board_contacts_clear", resp.Kind)
		}
		return *got
	}

	if got := clear(t, u, ""); got.Status != protocol.BoardStatus_NotFound {
		t.Errorf("out-of-scope task: status %v, want not_found", got.Status)
	}
	if n := len(h.Board.Contacts(ust)); n != 2 {
		t.Errorf("out-of-scope task kept %d contacts, want 2", n)
	}
	if got := clear(t, g, agentboard.SelfTopic(peerA)); got.Status != protocol.BoardStatus_Ok || got.Removed != 1 {
		t.Errorf("one contact of a descendant: %+v, want ok removed=1", got)
	}
	if got := clear(t, g, ""); got.Status != protocol.BoardStatus_Ok || got.Removed != 1 {
		t.Errorf("all contacts of a descendant: %+v, want ok removed=1", got)
	}
	if n := len(h.Board.Contacts(gst)); n != 0 {
		t.Errorf("descendant has %d contacts after clear all", n)
	}
}
