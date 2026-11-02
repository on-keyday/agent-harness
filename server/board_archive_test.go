package server

import (
	"encoding/hex"
	"fmt"
	"regexp"
	"testing"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func boardReq(t *testing.T, h *TaskHandler, conn *fakeConn, req *protocol.TaskControlRequest) protocol.TaskControlResponse {
	t.Helper()
	h.Handle(conn, encodeTaskControlRequest(t, req))
	return lastTaskControlResponse(t, conn)
}

func flush(t *testing.T, h *TaskHandler, conn *fakeConn, topic string) protocol.BoardFlushResponse {
	t.Helper()
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_BoardFlush, RequestId: 7}
	var r protocol.BoardFlushRequest
	r.SetTopic([]byte(topic))
	req.SetBoardFlush(r)
	resp := boardReq(t, h, conn, req)
	got := resp.BoardFlush()
	if got == nil {
		t.Fatalf("response kind %v, want board_flush", resp.Kind)
	}
	return *got
}

func archive(t *testing.T, h *TaskHandler, conn *fakeConn, topic string) protocol.BoardArchiveResponse {
	t.Helper()
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_BoardArchive, RequestId: 7}
	var r protocol.BoardArchiveRequest
	r.SetTopic([]byte(topic))
	req.SetBoardArchive(r)
	resp := boardReq(t, h, conn, req)
	got := resp.BoardArchive()
	if got == nil {
		t.Fatalf("response kind %v, want board_archive", resp.Kind)
	}
	return *got
}

func sendN(t *testing.T, h *TaskHandler, topic string, n int) []uint64 {
	t.Helper()
	var seqs []uint64
	for i := 0; i < n; i++ {
		seq, _, err := h.Board.Send(topic, []byte(fmt.Sprintf("m%d", i)), protocol.RunnerID{}, protocol.TaskID{}, "h", "", 0)
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, seq)
	}
	return seqs
}

func TestBoardFlush_OperatorAndStatuses(t *testing.T) {
	h, conn, _ := retainHandler(t, nil)
	if err := h.Board.Retain("keep", agentboard.Retention{Forever: true}); err != nil {
		t.Fatal(err)
	}
	seqs := sendN(t, h, "keep", 20)
	got := flush(t, h, conn, "keep")
	if got.Status != protocol.BoardFlushStatus_Ok || got.ThroughSeq != seqs[19] || got.Dropped == 0 {
		t.Fatalf("flush = %+v", got)
	}
	if msgs, _ := h.Board.ListRetained("keep"); len(msgs) != 0 {
		t.Fatalf("ring holds %d after flush", len(msgs))
	}
	body := topicsBody(t, h, conn)
	if len(body.Retention) != 1 || body.Retention[0].RingFloor != seqs[19] {
		t.Fatalf("retention rows = %+v, want ring_floor %d", body.Retention, seqs[19])
	}
	sendN(t, h, "plain", 1)
	if st := flush(t, h, conn, "plain").Status; st != protocol.BoardFlushStatus_NotRetained {
		t.Errorf("plain: %v", st)
	}
	if st := flush(t, h, conn, "missing").Status; st != protocol.BoardFlushStatus_NotFound {
		t.Errorf("missing: %v", st)
	}
}

func TestBoardFlush_TaskNeedsBitAndPublishable(t *testing.T) {
	none := protocol.Capability_None
	h, conn, _ := retainHandler(t, &none)
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_BoardFlush, RequestId: 7}
	var r protocol.BoardFlushRequest
	r.SetTopic([]byte("x"))
	req.SetBoardFlush(r)
	if resp := boardReq(t, h, conn, req); resp.Kind != protocol.TaskControlKind_PermissionDenied ||
		resp.PermissionDenied().RequiredCap != protocol.Capability_BoardPersist {
		t.Fatalf("no bit: %v", resp.Kind)
	}

	caps := protocol.Capability_BoardPersist
	h, conn, tid := retainHandler(t, &caps)
	if err := h.Board.Retain("free.x", agentboard.Retention{Forever: true}); err != nil {
		t.Fatal(err)
	}
	sendN(t, h, "free.x", 1)
	if st := flush(t, h, conn, "free.x").Status; st != protocol.BoardFlushStatus_NotPublishable {
		t.Errorf("free-form topic without board_peer: %v", st)
	}
	own := fmt.Sprintf("tree.%s.d", hex.EncodeToString(tid.Id[:]))
	if err := h.Board.Retain(own, agentboard.Retention{Forever: true}); err != nil {
		t.Fatal(err)
	}
	sendN(t, h, own, 1)
	if st := flush(t, h, conn, own).Status; st != protocol.BoardFlushStatus_Ok {
		t.Errorf("own tree topic: %v", st)
	}
}

func TestBoardArchive_StatusMapping(t *testing.T) {
	h, conn, _ := retainHandler(t, nil)
	sendN(t, h, "chat.0123abcd", 1)
	if st := archive(t, h, conn, "chat.0123abcd").Status; st != protocol.BoardArchiveStatus_NotArchivable {
		t.Errorf("chat: %v", st)
	}
	if st := archive(t, h, conn, "missing").Status; st != protocol.BoardArchiveStatus_NotFound {
		t.Errorf("missing: %v", st)
	}
	if err := h.Board.Retain("empty", agentboard.Retention{Forever: true}); err != nil {
		t.Fatal(err)
	}
	if st := archive(t, h, conn, "empty").Status; st != protocol.BoardArchiveStatus_Empty {
		t.Errorf("empty: %v", st)
	}
	seqs := sendN(t, h, "plain", 3)
	got := archive(t, h, conn, "plain")
	if got.Status != protocol.BoardArchiveStatus_Ok || !regexp.MustCompile(`^[0-9a-f]{12}$`).Match(got.Id) ||
		got.ThroughSeq != seqs[2] || got.Count != 3 {
		t.Fatalf("archive = %+v", got)
	}
}

func archivesRows(t *testing.T, h *TaskHandler, conn *fakeConn, kind protocol.TaskControlKind, topic string) (protocol.BoardStatus, []protocol.BoardArchiveRow) {
	t.Helper()
	req := &protocol.TaskControlRequest{Kind: kind, RequestId: 7}
	if kind == protocol.TaskControlKind_BoardArchives {
		r := protocol.BoardArchivesRequest{RequestId: 7}
		r.SetTopic([]byte(topic))
		req.SetBoardArchives(r)
	} else {
		r := protocol.AgentArchivesRequest{RequestId: 7}
		r.SetTopic([]byte(topic))
		req.SetAgentArchives(r)
	}
	resp := boardReq(t, h, conn, req)
	var got *protocol.BoardArchivesResponse
	if kind == protocol.TaskControlKind_BoardArchives {
		got = resp.BoardArchives()
	} else {
		got = resp.AgentArchives()
	}
	if got == nil {
		t.Fatalf("response kind %v", resp.Kind)
	}
	if got.Status != protocol.BoardStatus_Ok {
		if got.StreamId != 0 {
			t.Fatalf("refusal with stream %d", got.StreamId)
		}
		return got.Status, nil
	}
	var body protocol.BoardArchivesBody
	conn.streamBody(t, got.StreamId, &body)
	return got.Status, body.Rows
}

func TestBoardArchives_ListAndRead(t *testing.T) {
	h, conn, _ := retainHandler(t, nil)
	if err := h.Board.Retain("keep", agentboard.Retention{Forever: true}); err != nil {
		t.Fatal(err)
	}
	seqs := sendN(t, h, "keep", 30)
	// The test board's ring is short; withdraw one it still holds.
	if _, ok := h.Board.RetireSeq(seqs[27], protocol.TaskID{}); !ok {
		t.Fatal("retract")
	}
	id := string(archive(t, h, conn, "keep").Id)
	sendN(t, h, "other", 1)
	archive(t, h, conn, "other")

	if _, rows := archivesRows(t, h, conn, protocol.TaskControlKind_BoardArchives, ""); len(rows) != 2 {
		t.Fatalf("all archives = %d rows", len(rows))
	}
	_, rows := archivesRows(t, h, conn, protocol.TaskControlKind_BoardArchives, "keep")
	if len(rows) != 1 || string(rows[0].Id) != id || rows[0].Count != 30 || rows[0].ThroughSeq != seqs[29] || rows[0].DiskBytes == 0 {
		t.Fatalf("keep archives = %+v", rows)
	}

	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_BoardArchiveRead, RequestId: 7}
	r := protocol.BoardArchiveReadRequest{RequestId: 7, BeforeSeq: seqs[29], Limit: 5}
	r.SetId([]byte(id))
	req.SetBoardArchiveRead(r)
	rr := boardReq(t, h, conn, req)
	resp := rr.BoardArchiveRead()
	if resp == nil || resp.Status != protocol.BoardStatus_Ok {
		t.Fatalf("read = %+v", resp)
	}
	var body protocol.BoardReadBody
	conn.streamBody(t, resp.StreamId, &body)
	if len(body.Msgs) != 5 || body.Msgs[0].Seq != seqs[28] || !body.Msgs[1].Retracted() {
		t.Fatalf("page = %d rows, first %d", len(body.Msgs), body.Msgs[0].Seq)
	}

	r.SetId([]byte("000000000000"))
	req.SetBoardArchiveRead(r)
	rr = boardReq(t, h, conn, req)
	if resp := rr.BoardArchiveRead(); resp == nil || resp.Status != protocol.BoardStatus_NotFound || resp.StreamId != 0 {
		t.Fatalf("unknown id = %+v", resp)
	}
}

func TestBoardArchivePurge_SeqAndWhole(t *testing.T) {
	h, conn, _ := retainHandler(t, nil)
	seqs := sendN(t, h, "plain", 3)
	id := archive(t, h, conn, "plain").Id
	purge := func(seq uint64) protocol.BoardPurgeResponse {
		req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_BoardArchivePurge, RequestId: 7}
		r := protocol.BoardArchivePurgeRequest{RequestId: 7, Seq: seq}
		r.SetId(id)
		req.SetBoardArchivePurge(r)
		rr := boardReq(t, h, conn, req)
		got := rr.BoardArchivePurge()
		if got == nil {
			t.Fatal("no board_archive_purge variant")
		}
		return *got
	}
	if got := purge(seqs[1]); got.Status != protocol.BoardStatus_Ok || got.Purged != 1 {
		t.Fatalf("purge seq = %+v", got)
	}
	if got := purge(seqs[1]); got.Status != protocol.BoardStatus_NotFound {
		t.Fatalf("purge seq again = %+v", got)
	}
	if got := purge(0); got.Status != protocol.BoardStatus_Ok || got.Purged != 2 {
		t.Fatalf("purge whole = %+v", got)
	}
	if got := purge(0); got.Status != protocol.BoardStatus_NotFound {
		t.Fatalf("purge whole again = %+v", got)
	}
}

func agentArchiveRead(t *testing.T, h *TaskHandler, conn *fakeConn, id string) (protocol.AgentReadPageStatus, []protocol.DeliveredMessage) {
	t.Helper()
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AgentArchiveRead, RequestId: 7}
	r := protocol.AgentArchiveReadRequest{RequestId: 7, Limit: 100}
	r.SetId([]byte(id))
	req.SetAgentArchiveRead(r)
	rr := boardReq(t, h, conn, req)
	got := rr.AgentArchiveRead()
	if got == nil {
		t.Fatal("no agent_archive_read variant")
	}
	if got.Status != protocol.AgentReadPageStatus_Ok {
		if got.StreamId != 0 {
			t.Fatalf("refusal with stream %d", got.StreamId)
		}
		return got.Status, nil
	}
	var body protocol.DeliveredBody
	conn.streamBody(t, got.StreamId, &body)
	return got.Status, body.Msgs
}

// An agent reads an archive of a topic it may read, live messages only; one
// of another task's tree, and an id that does not exist, answer the same
// not_found with no stream.
func TestAgentArchiveRead_ReachMergedNotFound(t *testing.T) {
	h, conn, _ := agentPageHandler(t, "group", false)
	seqs := sendN(t, h, "group", 5)
	if _, ok := h.Board.RetireSeq(seqs[2], protocol.TaskID{}); !ok {
		t.Fatal("retract")
	}
	open, err := h.Board.Archive("group", protocol.TaskID{})
	if err != nil {
		t.Fatal(err)
	}
	sendN(t, h, "tree.00112233445566778899aabbccddeeff.d", 2)
	closed, err := h.Board.Archive("tree.00112233445566778899aabbccddeeff.d", protocol.TaskID{})
	if err != nil {
		t.Fatal(err)
	}

	st, msgs := agentArchiveRead(t, h, conn, open.ID)
	if st != protocol.AgentReadPageStatus_Ok || len(msgs) != 4 {
		t.Fatalf("readable archive: %v, %d rows, want 4 live", st, len(msgs))
	}
	for _, m := range msgs {
		if m.Seq == seqs[2] {
			t.Fatal("withdrawn message on an agent path")
		}
	}
	if st, _ := agentArchiveRead(t, h, conn, closed.ID); st != protocol.AgentReadPageStatus_NotFound {
		t.Errorf("another task's tree: %v", st)
	}
	if st, _ := agentArchiveRead(t, h, conn, "000000000000"); st != protocol.AgentReadPageStatus_NotFound {
		t.Errorf("unknown id: %v", st)
	}

	if st, rows := archivesRows(t, h, conn, protocol.TaskControlKind_AgentArchives, "group"); st != protocol.BoardStatus_Ok || len(rows) != 1 {
		t.Errorf("agent archives of a readable topic: %v %d", st, len(rows))
	}
	if st, _ := archivesRows(t, h, conn, protocol.TaskControlKind_AgentArchives, "tree.00112233445566778899aabbccddeeff.d"); st != protocol.BoardStatus_NotFound {
		t.Errorf("agent archives of another task's tree: %v", st)
	}
	if st, _ := archivesRows(t, h, conn, protocol.TaskControlKind_AgentArchives, ""); st != protocol.BoardStatus_NotFound {
		t.Errorf("agent archives without a topic: %v", st)
	}
}
