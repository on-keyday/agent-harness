package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/hostlog"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

func newHostLogFixture(t *testing.T, serverReader *hostlog.Reader, runnerHostLogs bool) (*TaskHandler, *fakeConn) {
	t.Helper()
	h := newTestHandler(t)
	h.HostLog = serverReader
	rc := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9140-1"), nextStreamID: 100}
	now := time.Now()
	h.Registry.Add(&RunnerEntry{
		ID: tcid("R"), Identity: testRunnerID("R"), Hostname: "h", AllowedRoots: []string{"/r"}, MaxTasks: 4,
		ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastTaskActivity: now, Conn: rc,
		HostLogs: runnerHostLogs,
	})
	return h, rc
}

func sendHostLogReq(t *testing.T, h *TaskHandler, conn *fakeConn, body protocol.HostLogRequest) protocol.TaskControlResponse {
	t.Helper()
	req := &protocol.TaskControlRequest{
		Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_HostLog, RequestId: 7},
	}
	req.SetHostLog(body)
	conn.nextStreamID = 200
	h.Handle(conn, encodeTaskControlRequest(t, req))
	return lastTaskControlResponse(t, conn)
}

func TestHostLogCapabilityGate(t *testing.T) {
	ring := hostlog.NewRing(1024)
	_, _ = ring.Write([]byte("hello server log\n"))
	h, _ := newHostLogFixture(t, hostlog.NewRingReader(ring), true)

	// Agent without host_log is denied centrally.
	deniedID := h.Tasks.Create("/r", "no-cap", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_FileRead}, "")
	deniedConn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9151-1"), nextStreamID: 200}
	h.principals = map[objproto.ConnectionID]protocol.TaskID{
		deniedConn.ConnectionID(): hexToTaskID(t, deniedID),
	}
	resp := sendHostLogReq(t, h, deniedConn, protocol.HostLogRequest{Target: protocol.HostLogTarget_Server})
	if resp.Kind != protocol.TaskControlKind_PermissionDenied {
		t.Fatalf("without host_log: got kind %v, want PermissionDenied", resp.Kind)
	}

	// Agent with host_log is allowed.
	allowedID := h.Tasks.Create("/r", "with-cap", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_HostLog}, "")
	allowedConn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9152-1"), nextStreamID: 200}
	h.principals[allowedConn.ConnectionID()] = hexToTaskID(t, allowedID)
	resp = sendHostLogReq(t, h, allowedConn, protocol.HostLogRequest{Target: protocol.HostLogTarget_Server})
	if resp.Kind != protocol.TaskControlKind_HostLog || resp.HostLog() == nil || resp.HostLog().Status != protocol.HostLogStatus_Ok {
		t.Fatalf("with host_log: got kind=%v body=%+v, want HostLog ok", resp.Kind, resp.HostLog())
	}
}

func TestHostLogServerFileAndUnavailable(t *testing.T) {
	// Unavailable when HostLog is nil.
	hNil, _ := newHostLogFixture(t, nil, true)
	op := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9160-1"), nextStreamID: 200}
	resp := sendHostLogReq(t, hNil, op, protocol.HostLogRequest{Target: protocol.HostLogTarget_Server})
	if resp.HostLog() == nil || resp.HostLog().Status != protocol.HostLogStatus_Unavailable {
		t.Fatalf("nil HostLog: got %+v, want unavailable", resp.HostLog())
	}

	// File-backed server log answers ok with a non-zero stream id.
	p := filepath.Join(t.TempDir(), "server.log")
	if err := os.WriteFile(p, []byte("boot ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hFile, _ := newHostLogFixture(t, hostlog.NewFileReader(p), true)
	resp = sendHostLogReq(t, hFile, op, protocol.HostLogRequest{Target: protocol.HostLogTarget_Server})
	if resp.HostLog() == nil || resp.HostLog().Status != protocol.HostLogStatus_Ok || resp.HostLog().StreamId == 0 {
		t.Fatalf("file HostLog: got %+v, want ok with stream_id", resp.HostLog())
	}
}

func TestHostLogRunnerForwardAndUnsupported(t *testing.T) {
	op := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9161-1"), nextStreamID: 200}

	// Runner that predates host_logs (HostLogs == false) is never sent a request.
	hOld, rcOld := newHostLogFixture(t, nil, false)
	resp := sendHostLogReq(t, hOld, op, protocol.HostLogRequest{
		Target:   protocol.HostLogTarget_Runner,
		RunnerId: testRunnerID("R"),
	})
	if resp.HostLog() == nil || resp.HostLog().Status != protocol.HostLogStatus_RunnerUnsupported {
		t.Fatalf("old runner: got %+v, want runner_unsupported", resp.HostLog())
	}
	if reqs := runnerRequests(t, rcOld); len(reqs) != 0 {
		t.Fatalf("old runner received %d requests, want 0", len(reqs))
	}

	// Unknown / offline runner answers runner_offline.
	resp = sendHostLogReq(t, hOld, op, protocol.HostLogRequest{
		Target:   protocol.HostLogTarget_Runner,
		RunnerId: testRunnerID("X"),
	})
	if resp.HostLog() == nil || resp.HostLog().Status != protocol.HostLogStatus_RunnerOffline {
		t.Fatalf("offline runner: got %+v, want runner_offline", resp.HostLog())
	}

	// Runner with HostLogs == true receives RunnerRequestType_HostLog.
	hNew, rcNew := newHostLogFixture(t, nil, true)
	resp = sendHostLogReq(t, hNew, op, protocol.HostLogRequest{
		Target:   protocol.HostLogTarget_Runner,
		RunnerId: testRunnerID("R"),
		Mode:     protocol.HostLogMode_Range,
		Offset:   128,
		MaxBytes: 4096,
	})
	if resp.HostLog() == nil || resp.HostLog().Status != protocol.HostLogStatus_Ok || resp.HostLog().StreamId == 0 {
		t.Fatalf("new runner: got %+v, want ok with stream_id", resp.HostLog())
	}
	var got *protocol.RunnerHostLogRequest
	for _, rr := range runnerRequests(t, rcNew) {
		if rr.Kind == protocol.RunnerRequestType_HostLog && rr.HostLog() != nil {
			hl := *rr.HostLog()
			got = &hl
		}
	}
	if got == nil {
		t.Fatal("runner did not receive RunnerRequestType_HostLog")
	}
	if got.Mode != protocol.HostLogMode_Range || got.Offset != 128 || got.MaxBytes != 4096 || got.StreamId == 0 {
		t.Fatalf("forwarded RunnerHostLogRequest mismatch: %+v", *got)
	}
}
