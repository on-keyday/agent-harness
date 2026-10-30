package server

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// storeDirs remembers each test board's store directory, for tests that
// reach the log file itself.
var storeDirs = map[*agentboard.Board]string{}

func boardLogPath(t *testing.T, h *TaskHandler, topic string) string {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(storeDirs[h.Board], "*.log"))
	if len(matches) != 1 {
		t.Fatalf("want one log in the store, found %v", matches)
	}
	return matches[0]
}

func attachTestStore(t *testing.T, b *agentboard.Board) {
	t.Helper()
	dir := t.TempDir()
	storeDirs[b] = dir
	s, err := agentboard.OpenFileStore(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := b.AttachStore(s); err != nil {
		t.Fatal(err)
	}
}

// retainHandler is a handler whose caller is either the operator (caps =
// nil) or a task holding caps, with the board's reach and store wired.
func retainHandler(t *testing.T, caps *protocol.Capability) (*TaskHandler, *fakeConn, protocol.TaskID) {
	t.Helper()
	h, conn := newBoardTestHandler(t)
	h.Board.SetReach(storeReach{h.Tasks})
	attachTestStore(t, h.Board)
	if caps == nil {
		return h, conn, protocol.TaskID{}
	}
	idHex := h.Tasks.Create("repo", "", protocol.TaskKind_Interactive, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: *caps, Scope: authority.Scope{}},
		"bash")
	tid := hexToTaskID(t, idHex)
	h.principals = map[string]protocol.TaskID{conn.ConnectionID().String(): tid}
	return h, conn, tid
}

func retain(t *testing.T, h *TaskHandler, conn *fakeConn, topic string, ttl time.Duration, forever, clear bool) protocol.TaskControlResponse {
	t.Helper()
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_BoardRetain, RequestId: 7}
	r := protocol.BoardRetainRequest{TtlMs: uint64(ttl.Milliseconds())}
	r.SetTopic([]byte(topic))
	r.SetForever(forever)
	r.SetClear(clear)
	req.SetBoardRetain(r)
	h.Handle(conn, encodeTaskControlRequest(t, req))
	return lastTaskControlResponse(t, conn)
}

func retainStatus(t *testing.T, resp protocol.TaskControlResponse) protocol.BoardRetainStatus {
	t.Helper()
	got := resp.BoardRetain()
	if got == nil {
		t.Fatalf("response kind %v, want board_retain", resp.Kind)
	}
	return got.Status
}

func topicsBody(t *testing.T, h *TaskHandler, conn *fakeConn) protocol.BoardTopicsBody {
	t.Helper()
	h.handleBoardTopics(conn, 1)
	resp := lastTaskControlResponse(t, conn)
	var body protocol.BoardTopicsBody
	conn.streamBody(t, resp.BoardTopics().StreamId, &body)
	return body
}

func TestBoardRetain_OperatorAnyTopic(t *testing.T) {
	h, conn, _ := retainHandler(t, nil)
	if st := retainStatus(t, retain(t, h, conn, "free.x", 0, true, false)); st != protocol.BoardRetainStatus_Ok {
		t.Fatalf("status %v, want ok", st)
	}
	body := topicsBody(t, h, conn)
	if len(body.Retention) != 1 {
		t.Fatalf("retention list = %+v, want free.x", body.Retention)
	}
	r := body.Retention[0]
	if string(r.Name) != "free.x" || !r.Forever() || r.SetBy != (protocol.TaskID{}) {
		t.Fatalf("retention row = %+v", r)
	}
	if st := retainStatus(t, retain(t, h, conn, "free.x", 0, false, true)); st != protocol.BoardRetainStatus_Ok {
		t.Fatalf("clear status %v", st)
	}
	if body := topicsBody(t, h, conn); len(body.Retention) != 0 {
		t.Fatalf("retention after clear = %+v", body.Retention)
	}
}

func TestBoardRetain_TaskNeedsBit(t *testing.T) {
	none := protocol.Capability_None
	h, conn, _ := retainHandler(t, &none)
	resp := retain(t, h, conn, "free.x", time.Hour, false, false)
	if resp.Kind != protocol.TaskControlKind_PermissionDenied || resp.PermissionDenied().RequiredCap != protocol.Capability_BoardPersist {
		t.Fatalf("response %v, want permission denied naming board_persist", resp.Kind)
	}
}

func TestBoardRetain_TaskNeedsPublishable(t *testing.T) {
	caps := protocol.Capability_BoardPersist
	h, conn, tid := retainHandler(t, &caps)
	if st := retainStatus(t, retain(t, h, conn, "free.x", time.Hour, false, false)); st != protocol.BoardRetainStatus_NotPublishable {
		t.Fatalf("free-form topic without board_peer: %v, want not_publishable", st)
	}
	own := fmt.Sprintf("tree.%s.d", hex.EncodeToString(tid.Id[:]))
	if st := retainStatus(t, retain(t, h, conn, own, time.Hour, false, false)); st != protocol.BoardRetainStatus_Ok {
		t.Fatalf("own tree topic: %v, want ok", st)
	}
	body := topicsBody(t, h, conn)
	if len(body.Retention) != 1 || body.Retention[0].SetBy != tid || body.Retention[0].TtlMs != uint64(time.Hour.Milliseconds()) {
		t.Fatalf("retention = %+v, want own topic set by the task", body.Retention)
	}
}

func TestBoardRetain_ChatRefused(t *testing.T) {
	h, conn, _ := retainHandler(t, nil)
	if st := retainStatus(t, retain(t, h, conn, "chat.0123abcd", time.Hour, false, false)); st != protocol.BoardRetainStatus_NotRetainable {
		t.Fatalf("status %v, want not_retainable", st)
	}
	if st := retainStatus(t, retain(t, h, conn, "free.y", 0, false, false)); st != protocol.BoardRetainStatus_BadRequest {
		t.Fatalf("no ttl, no forever, no clear: %v, want bad_request", st)
	}
	// A ttl_ms past what a Duration holds would wrap negative and expire at once.
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_BoardRetain, RequestId: 8}
	r := protocol.BoardRetainRequest{TtlMs: 1 << 63}
	r.SetTopic([]byte("free.z"))
	req.SetBoardRetain(r)
	h.Handle(conn, encodeTaskControlRequest(t, req))
	if st := retainStatus(t, lastTaskControlResponse(t, conn)); st != protocol.BoardRetainStatus_BadRequest {
		t.Fatalf("overflowing ttl_ms: %v, want bad_request", st)
	}
}

func TestBoardRead_Pages(t *testing.T) {
	h, conn, _ := retainHandler(t, nil)
	_ = retain(t, h, conn, "paged", 0, true, false)
	for i := 0; i < 100; i++ {
		h.Board.Send("paged", []byte("m"), protocol.RunnerID{}, protocol.TaskID{}, "h", "", 0)
	}
	read := func(before uint64, limit uint16) []protocol.BoardMessageRow {
		t.Helper()
		req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_BoardRead, RequestId: 9}
		r := protocol.BoardReadRequest{BeforeSeq: before, Limit: limit}
		r.SetTopic([]byte("paged"))
		req.SetBoardRead(r)
		h.Handle(conn, encodeTaskControlRequest(t, req))
		resp := lastTaskControlResponse(t, conn)
		return boardReadRows(t, conn, resp.BoardRead())
	}
	page := read(0, 10)
	if len(page) != 10 || page[0].Seq <= page[9].Seq {
		t.Fatalf("first page = %d rows, want 10 newest first", len(page))
	}
	older := read(page[9].Seq, 200)
	if len(older) != 90 || older[0].Seq >= page[9].Seq {
		t.Fatalf("second page = %d rows, want the 90 older ones", len(older))
	}
	// limit 0 is the old answer: the ring (8 in the test board), oldest first.
	ring := read(0, 0)
	if len(ring) != 8 || ring[0].Seq >= ring[7].Seq {
		t.Fatalf("limit 0 = %d rows, want the 8-entry ring oldest first", len(ring))
	}
}

// A paged read whose log cannot be read answers an error status, not ok with
// zero rows — an empty history and a failed read must not look alike.
func TestBoardRead_PageFailureIsNotEmpty(t *testing.T) {
	h, conn, _ := retainHandler(t, nil)
	_ = retain(t, h, conn, "unreadable", 0, true, false)
	h.Board.Send("unreadable", []byte("m"), protocol.RunnerID{}, protocol.TaskID{}, "h", "", 0)
	// Flush it to disk, then make the log unreadable.
	if _, err := h.Board.Page("unreadable", 0, 1); err != nil {
		t.Fatal(err)
	}
	path := boardLogPath(t, h, "unreadable")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_BoardRead, RequestId: 9}
	r := protocol.BoardReadRequest{Limit: 10}
	r.SetTopic([]byte("unreadable"))
	req.SetBoardRead(r)
	h.Handle(conn, encodeTaskControlRequest(t, req))
	resp := lastTaskControlResponse(t, conn)
	if st := resp.BoardRead().Status; st != protocol.BoardStatus_StoreError {
		t.Fatalf("status %v, want store_error", st)
	}
}
