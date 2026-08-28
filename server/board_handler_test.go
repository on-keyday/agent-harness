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
	h.Board.Send("chat.aaa", []byte("x"), protocol.RunnerID{}, protocol.TaskID{}, "h", "", 0)
	h.Board.Send("chat.bbb", []byte("y"), protocol.RunnerID{}, protocol.TaskID{}, "h", "", 0)

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
	s1, _, _ := h.Board.Send("chat.p", []byte("a"), protocol.RunnerID{}, protocol.TaskID{}, "h", "", 0)
	h.Board.Send("chat.p", []byte("b"), protocol.RunnerID{}, protocol.TaskID{}, "h", "", 0)

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
	h.Board.Send("chat.r", []byte("alpha"), protocol.RunnerID{}, protocol.TaskID{}, "h", "", 0)
	h.Board.Send("chat.r", []byte("bravo"), protocol.RunnerID{}, protocol.TaskID{}, "h", "", 0)

	// Configure the send stream ID so handleBoardRead gets a non-nil stream.
	// (fakeConn.CreateSendStream returns nil when nextSendStreamID==0.)
	conn.nextSendStreamID = 5

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

func TestHandleBoardRead_CarriesInReplyTo(t *testing.T) {
	h, conn := newBoardTestHandler(t)
	parent, _, err := h.Board.Send("chat.r", []byte("q"), protocol.RunnerID{}, protocol.TaskID{}, "h", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.Board.Send("chat.r", []byte("a"), protocol.RunnerID{}, protocol.TaskID{}, "h", "", parent); err != nil {
		t.Fatal(err)
	}

	h.handleBoardRead(conn, 1, "chat.r")

	resp := lastTaskControlResponse(t, conn)
	br := resp.BoardRead()
	if br == nil || br.Status != protocol.BoardStatus_Ok {
		t.Fatalf("board read = %+v, want ok", br)
	}
	if len(br.Msgs) != 2 {
		t.Fatalf("msgs = %d, want 2", len(br.Msgs))
	}
	if br.Msgs[0].InReplyTo != 0 {
		t.Errorf("parent row InReplyTo = %d, want 0", br.Msgs[0].InReplyTo)
	}
	if br.Msgs[1].InReplyTo != parent {
		t.Errorf("reply row InReplyTo = %d, want %d", br.Msgs[1].InReplyTo, parent)
	}
}

func TestHandleBoardSubscribers_NoFilterAndFilter(t *testing.T) {
	h, conn := newBoardTestHandler(t)

	var rid protocol.RunnerID
	rid.SetTransport([]byte("ws"))
	rid.SetIpAddr([]byte{1, 2, 3, 4})
	var listener protocol.TaskID
	listener.Id[0] = 1
	var bystander protocol.TaskID
	bystander.Id[0] = 2

	c := h.Board.Attach(boardRunnerIDFromProto(rid), boardTaskIDFromProto(listener), "host-A", "claude")
	if err := h.Board.Subscribe(c, "rr.dec-019"); err != nil {
		t.Fatal(err)
	}
	h.Board.Attach(boardRunnerIDFromProto(rid), boardTaskIDFromProto(bystander), "host-B", "codex")

	h.handleBoardSubscribers(conn, 1, "")
	allResp := lastTaskControlResponse(t, conn)
	all := allResp.BoardSubscribers()
	if all == nil || len(all.Rows) != 2 {
		t.Fatalf("no-filter rows = %+v, want 2", all)
	}

	h.handleBoardSubscribers(conn, 2, "rr.dec-019")
	filteredResp := lastTaskControlResponse(t, conn)
	filtered := filteredResp.BoardSubscribers()
	if filtered == nil || len(filtered.Rows) != 1 {
		t.Fatalf("filtered rows = %+v, want 1", filtered)
	}
	if filtered.Rows[0].Task.Id != listener.Id {
		t.Errorf("task = %x, want %x", filtered.Rows[0].Task.Id, listener.Id)
	}
	if string(filtered.Rows[0].Hostname) != "host-A" {
		t.Errorf("hostname = %q, want host-A", string(filtered.Rows[0].Hostname))
	}
	if string(filtered.Rows[0].AgentProfile) != "claude" {
		t.Errorf("agent = %q, want claude", string(filtered.Rows[0].AgentProfile))
	}
	// The row reports the task's full pattern set: its server-seeded
	// chat.<short-id> plus the explicit subscription.
	if len(filtered.Rows[0].Patterns) != 1 {
		t.Errorf("patterns = %d, want 1 (only the explicit subscribe; Attach does not seed)", len(filtered.Rows[0].Patterns))
	}
}

// The operator surface is the only place the declared destination is visible.
// Without it, "where did the answer to #N go" has no answer at all: the server
// resolves the route off the parent's retained entry, and neither the ask nor
// the reply mentions it in its text.
func TestHandleBoardRead_CarriesReplyToTopic(t *testing.T) {
	h, conn := newBoardTestHandler(t)
	if _, _, err := h.Board.Send("chat.r", []byte("q"), protocol.RunnerID{}, protocol.TaskID{}, "h", "", 0,
		agentboard.WithReplyTo("rr.dec-019")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.Board.Send("chat.r", []byte("plain"), protocol.RunnerID{}, protocol.TaskID{}, "h", "", 0); err != nil {
		t.Fatal(err)
	}

	h.handleBoardRead(conn, 1, "chat.r")

	resp := lastTaskControlResponse(t, conn)
	br := resp.BoardRead()
	if br == nil || len(br.Msgs) != 2 {
		t.Fatalf("board read = %+v, want 2 msgs", br)
	}
	if got := string(br.Msgs[0].ReplyToTopic); got != "rr.dec-019" {
		t.Errorf("declared row ReplyToTopic = %q, want rr.dec-019", got)
	}
	// Empty, not the sender's own topic: the row reports what was DECLARED, and
	// substituting the fallback here would make "declared nothing" and
	// "declared its own inbox" the same reading.
	if got := string(br.Msgs[1].ReplyToTopic); got != "" {
		t.Errorf("undeclared row ReplyToTopic = %q, want empty", got)
	}
}

// boardRetractResp reads the last response as a board_retract one. The
// generated accessor is a pointer method, so the response has to be a variable
// before it can be asked — the sibling tests above spell that out inline.
func boardRetractResp(t *testing.T, conn *fakeConn) *protocol.BoardRetractResponse {
	t.Helper()
	resp := lastTaskControlResponse(t, conn)
	return resp.BoardRetract()
}

// boardReadResp is the same for board_read.
func boardReadResp(t *testing.T, conn *fakeConn) *protocol.BoardReadResponse {
	t.Helper()
	resp := lastTaskControlResponse(t, conn)
	return resp.BoardRead()
}

// TestHandleBoardRetract_WithdrawsAndKeepsItReadable is the whole point of the
// verb in one test: the message stops being deliverable and stays in the
// operator's read.
func TestHandleBoardRetract_WithdrawsAndKeepsItReadable(t *testing.T) {
	h, conn := newBoardTestHandler(t)
	var author protocol.TaskID
	author.Id[0] = 7
	var caller protocol.TaskID
	caller.Id[0] = 9
	seq, _, err := h.Board.Send("chat.w", []byte("inconvenient"), protocol.RunnerID{}, author, "h", "", 0)
	if err != nil {
		t.Fatal(err)
	}

	h.handleBoardRetract(conn, 1, "chat.w", seq, caller)
	if got := boardRetractResp(t, conn); got == nil || got.Status != protocol.BoardStatus_Ok {
		t.Fatalf("retract = %+v, want ok", got)
	}

	// Gone from the live ring — which is every agent-facing read.
	if msgs, _ := h.Board.ListRetained("chat.w"); len(msgs) != 0 {
		t.Errorf("live ring still holds %d message(s)", len(msgs))
	}

	// Still in the operator's read, payload included, marked with who took it.
	conn.nextSendStreamID = 5
	h.handleBoardRead(conn, 2, "chat.w")
	br := boardReadResp(t, conn)
	if br == nil || len(br.Msgs) != 1 {
		t.Fatalf("board read = %+v, want 1 msg", br)
	}
	row := br.Msgs[0]
	if !row.Retracted() {
		t.Error("row is not marked retracted")
	}
	if row.RetractedAtUnixMs == 0 {
		t.Error("row carries no retracted_at stamp")
	}
	if row.RetractedBy != protocol.RetractedBy_PurgeCap {
		t.Errorf("retracted_by = %v, want purge_cap", row.RetractedBy)
	}
	if row.RetractedByTask.Id != caller.Id {
		t.Errorf("retracted_by_task = %x, want the caller %x", row.RetractedByTask.Id, caller.Id)
	}
	if row.FromTask.Id != author.Id {
		t.Errorf("from_task = %x, want the author %x", row.FromTask.Id, author.Id)
	}
	if got := conn.sendStreamBytes(t, br.StreamId); string(got) != "inconvenient" {
		t.Errorf("withdrawn payload = %q, want the original bytes", got)
	}
}

// TestHandleBoardRetract_NotFoundCases: the four ways to miss all answer the
// same status. Distinguishing them would confirm what a topic holds to a caller
// that only guessed at a seq.
func TestHandleBoardRetract_NotFoundCases(t *testing.T) {
	h, conn := newBoardTestHandler(t)
	var caller protocol.TaskID
	caller.Id[0] = 9
	seq, _, err := h.Board.Send("chat.nf", []byte("x"), protocol.RunnerID{}, protocol.TaskID{}, "h", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		topic string
		seq   uint64
	}{
		{"unknown topic", "nope", seq},
		{"unknown seq", "chat.nf", seq + 9999},
		{"seq 0 is never a message", "chat.nf", 0},
	} {
		h.handleBoardRetract(conn, 1, tc.topic, tc.seq, caller)
		if got := boardRetractResp(t, conn); got.Status != protocol.BoardStatus_NotFound {
			t.Errorf("%s: status = %v, want not_found", tc.name, got.Status)
		}
	}
	// And once withdrawn, again.
	h.handleBoardRetract(conn, 2, "chat.nf", seq, caller)
	if got := boardRetractResp(t, conn); got.Status != protocol.BoardStatus_Ok {
		t.Fatalf("first retract = %v, want ok", got.Status)
	}
	h.handleBoardRetract(conn, 3, "chat.nf", seq, caller)
	if got := boardRetractResp(t, conn); got.Status != protocol.BoardStatus_NotFound {
		t.Errorf("second retract = %v, want not_found", got.Status)
	}
}

// TestHandleBoardRetract_OperatorClientHasNoTaskID: an operator surface holds
// its capabilities directly and has no principal task, so the recorded caller
// is the zero id. The row must still read as purge_cap — an operator retract
// must not be renderable as the author having done it.
func TestHandleBoardRetract_OperatorClientHasNoTaskID(t *testing.T) {
	h, conn := newBoardTestHandler(t)
	var author protocol.TaskID
	author.Id[0] = 7
	seq, _, err := h.Board.Send("chat.op", []byte("x"), protocol.RunnerID{}, author, "h", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	h.handleBoardRetract(conn, 1, "chat.op", seq, protocol.TaskID{})
	if got := boardRetractResp(t, conn); got.Status != protocol.BoardStatus_Ok {
		t.Fatalf("retract = %v, want ok", got.Status)
	}
	conn.nextSendStreamID = 5
	h.handleBoardRead(conn, 2, "chat.op")
	br := boardReadResp(t, conn)
	if br == nil || len(br.Msgs) != 1 {
		t.Fatalf("board read = %+v, want 1 msg", br)
	}
	if br.Msgs[0].RetractedBy != protocol.RetractedBy_PurgeCap {
		t.Errorf("retracted_by = %v, want purge_cap", br.Msgs[0].RetractedBy)
	}
	if br.Msgs[0].RetractedByTask.Id != ([16]byte{}) {
		t.Errorf("retracted_by_task = %x, want the zero id", br.Msgs[0].RetractedByTask.Id)
	}
}

// TestBoardRetractNeedsPurge pins the gate at the one place it is declared. It
// reuses purge's bit deliberately: purge already reaches the same message and
// destroys it outright, so this grants nothing new — but that is an argument
// about a specific bit, and the map is where it is stated.
func TestBoardRetractNeedsPurge(t *testing.T) {
	if got := requiredCap[protocol.TaskControlKind_BoardRetract]; got != protocol.Capability_Purge {
		t.Errorf("board_retract required cap = %v, want purge", got)
	}
}
