package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// agentPageHandler is a board with a store and one attached agent, which
// subscribes to topic. The topic is retained when retained is set.
func agentPageHandler(t *testing.T, topic string, retained bool) (*TaskHandler, *fakeConn, *agentboard.ConnState) {
	t.Helper()
	h, conn, _ := retainHandler(t, nil)
	var rid protocol.RunnerID
	var tid protocol.TaskID
	rid.Id[0], tid.Id[0] = 0xAA, 0xBB
	st := h.Board.Attach(rid, tid, "testhost", "bash")
	h.BoardConnState = func(ConnHandle) *agentboard.ConnState { return st }
	if retained {
		if err := h.Board.Retain(topic, agentboard.Retention{Forever: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.Board.Subscribe(st, topic); err != nil {
		t.Fatal(err)
	}
	return h, conn, st
}

func readPage(t *testing.T, h *TaskHandler, conn *fakeConn, topic string, before uint64, limit uint16) (protocol.AgentReadPageStatus, []protocol.DeliveredMessage) {
	t.Helper()
	r := &protocol.AgentReadPageRequest{BeforeSeq: before, Limit: limit}
	r.SetTopic([]byte(topic))
	h.handleAgentReadPage(conn, 1, r)
	resp := lastTaskControlResponse(t, conn)
	got := resp.AgentReadPage()
	if got == nil {
		t.Fatal("no agent_read_page variant")
	}
	if got.Status != protocol.AgentReadPageStatus_Ok {
		if got.StreamId != 0 {
			t.Fatalf("status %v with stream %d: a refusal must not open a stream", got.Status, got.StreamId)
		}
		return got.Status, nil
	}
	var body protocol.DeliveredBody
	conn.streamBody(t, got.StreamId, &body)
	return got.Status, body.Msgs
}

// A subscriber pages a retained topic past its ring (8 in the test board)
// into the disk log, newest first, and --before moves the window.
func TestAgentReadPage_ReachesPastTheRing(t *testing.T) {
	h, conn, _ := agentPageHandler(t, "hist", true)
	var seqs []uint64
	for i := 0; i < 30; i++ {
		seq, _, err := h.Board.Send("hist", []byte("m"), protocol.RunnerID{}, protocol.TaskID{}, "h", "", 0)
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, seq)
	}

	st, page := readPage(t, h, conn, "hist", 0, 10)
	if st != protocol.AgentReadPageStatus_Ok || len(page) != 10 || page[0].Seq != seqs[29] || page[9].Seq != seqs[20] {
		t.Fatalf("first page: status %v, %d rows, want the 10 newest first", st, len(page))
	}
	_, older := readPage(t, h, conn, "hist", page[9].Seq, 100)
	if len(older) != 20 || older[0].Seq != seqs[19] || older[19].Seq != seqs[0] {
		t.Fatalf("second page = %d rows, want the 20 older ones down to the first", len(older))
	}
	if _, ok := h.Board.Retained(seqs[0]); ok {
		t.Fatal("the oldest message is still in the ring; the test does not exercise the disk log")
	}
	_, none := readPage(t, h, conn, "hist", seqs[0], 10)
	if len(none) != 0 {
		t.Fatalf("before the first message: %d rows, want 0", len(none))
	}
}

// Withdrawn messages are left out, and the page is still filled from older
// rows rather than coming back short.
func TestAgentReadPage_LeavesOutWithdrawnAndRefills(t *testing.T) {
	h, conn, st := agentPageHandler(t, "hist", true)
	_, self, _, _ := st.Identity()
	var seqs []uint64
	for i := 0; i < 20; i++ {
		seq, _, err := h.Board.Send("hist", []byte("m"), protocol.RunnerID{}, self, "h", "", 0)
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, seq)
	}
	for _, i := range []int{19, 18, 17} {
		if _, ok := h.Board.RetractSeq(seqs[i], self); !ok {
			t.Fatalf("retract seq %d failed", seqs[i])
		}
	}
	_, page := readPage(t, h, conn, "hist", 0, 5)
	if len(page) != 5 || page[0].Seq != seqs[16] || page[4].Seq != seqs[12] {
		t.Fatalf("page = %d rows starting at %v, want 5 live rows from seq %d", len(page), page, seqs[16])
	}
}

// A topic the caller does not subscribe to answers not_found, exactly as one
// that does not exist.
func TestAgentReadPage_NotSubscribedIsNotFound(t *testing.T) {
	h, conn, _ := agentPageHandler(t, "mine", true)
	if err := h.Board.Retain("other", agentboard.Retention{Forever: true}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.Board.Send("other", []byte("m"), protocol.RunnerID{}, protocol.TaskID{}, "h", "", 0); err != nil {
		t.Fatal(err)
	}
	if st, _ := readPage(t, h, conn, "other", 0, 10); st != protocol.AgentReadPageStatus_NotFound {
		t.Fatalf("unsubscribed topic: %v, want not_found", st)
	}
	if st, _ := readPage(t, h, conn, "nosuch", 0, 10); st != protocol.AgentReadPageStatus_NotFound {
		t.Fatalf("unknown topic: %v, want not_found", st)
	}
}

// A topic that is not retained pages its ring.
func TestAgentReadPage_UnretainedPagesTheRing(t *testing.T) {
	h, conn, _ := agentPageHandler(t, "plain", false)
	for i := 0; i < 12; i++ {
		if _, _, err := h.Board.Send("plain", []byte("m"), protocol.RunnerID{}, protocol.TaskID{}, "h", "", 0); err != nil {
			t.Fatal(err)
		}
	}
	_, page := readPage(t, h, conn, "plain", 0, 100)
	if len(page) != 8 || page[0].Seq <= page[7].Seq {
		t.Fatalf("page = %d rows, want the 8-entry ring newest first", len(page))
	}
}
