package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// newBoardTestHandler returns a *TaskHandler with a Board wired, an empty Registry
// and TaskStore, and a recording fakeConn whose caller is treated as operator
// (no principals entry → callerCaps = Capability_All).
func newBoardTestHandler(t *testing.T) (*TaskHandler, *fakeConn) {
	t.Helper()
	board := newTestBoard(t)
	h := &TaskHandler{
		Tasks:    NewTaskStore(),
		Registry: NewRegistry(),
		Board:    board,
	}
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9950-1")}
	return h, conn
}

func TestHandleBoardTopics_ListsTopics(t *testing.T) {
	h, conn := newBoardTestHandler(t) // helper: TaskHandler w/ Board + recording ConnHandle, operator caller
	// seed two topics via the board
	h.Board.Send("chat.aaa", []byte("x"), protocol.RunnerID{}, protocol.TaskID{}, "h")
	h.Board.Send("chat.bbb", []byte("y"), protocol.RunnerID{}, protocol.TaskID{}, "h")

	h.handleBoardTopics(conn, 1)

	topicsResp := lastTaskControlResponse(t, conn)
	if topicsResp.Kind != protocol.TaskControlKind_BoardTopics {
		t.Fatalf("kind = %v", topicsResp.Kind)
	}
	bt := topicsResp.BoardTopics()
	if bt == nil || bt.TopicsLen != 2 {
		t.Fatalf("topics = %+v, want 2", bt)
	}
}

func TestHandleBoardPurge_WholeAndSeq(t *testing.T) {
	h, conn := newBoardTestHandler(t)
	s1, _ := h.Board.Send("chat.p", []byte("a"), protocol.RunnerID{}, protocol.TaskID{}, "h")
	h.Board.Send("chat.p", []byte("b"), protocol.RunnerID{}, protocol.TaskID{}, "h")

	// seq purge drops exactly one
	h.handleBoardPurge(conn, 2, "chat.p", s1)
	resp2 := lastTaskControlResponse(t, conn)
	r := resp2.BoardPurge()
	if r.Status != protocol.BoardStatus_Ok || r.Purged != 1 {
		t.Fatalf("seq purge = %+v, want ok/1", r)
	}
	// whole purge drops the remainder
	h.handleBoardPurge(conn, 3, "chat.p", 0)
	resp3 := lastTaskControlResponse(t, conn)
	r = resp3.BoardPurge()
	if r.Status != protocol.BoardStatus_Ok || r.Purged != 1 {
		t.Fatalf("whole purge = %+v, want ok/1", r)
	}
	// unknown topic → not_found
	h.handleBoardPurge(conn, 4, "nope", 0)
	resp4 := lastTaskControlResponse(t, conn)
	r = resp4.BoardPurge()
	if r.Status != protocol.BoardStatus_NotFound {
		t.Fatalf("unknown purge = %+v, want not_found", r)
	}
	_ = agentboard.RetainedMessage{} // keep import if unused otherwise
}

func TestHandleBoardRead_StreamsPayloadsInOrder(t *testing.T) {
	h, conn := newBoardTestHandler(t)
	h.Board.Send("chat.r", []byte("alpha"), protocol.RunnerID{}, protocol.TaskID{}, "h")
	h.Board.Send("chat.r", []byte("bravo"), protocol.RunnerID{}, protocol.TaskID{}, "h")

	h.handleBoardRead(conn, 1, "chat.r")

	resp := conn.lastTaskControlResponse(t)
	br := resp.BoardRead()
	if br == nil || br.Status != protocol.BoardStatus_Ok || br.MsgsLen != 2 {
		t.Fatalf("board_read resp = %+v, want ok/2", br)
	}
	if br.Msgs[0].Size != 5 || br.Msgs[1].Size != 5 {
		t.Fatalf("sizes = %d,%d want 5,5", br.Msgs[0].Size, br.Msgs[1].Size)
	}
	// The recording conn captures the send-stream bytes; concatenation is row order.
	got := conn.sendStreamBytes(t, br.StreamId)
	if string(got) != "alphabravo" {
		t.Fatalf("stream payload = %q, want alphabravo", got)
	}

	// unknown topic → not_found, stream_id 0
	h.handleBoardRead(conn, 2, "nope")
	br = conn.lastTaskControlResponse(t).BoardRead()
	if br.Status != protocol.BoardStatus_NotFound || br.StreamId != 0 {
		t.Fatalf("unknown read = %+v, want not_found/0", br)
	}
}
