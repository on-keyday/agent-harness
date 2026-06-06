package server

import (
	"encoding/hex"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TestHandleOpenPortForward_NoSuchTask exercises the early-exit branch
// when the requested task id is not present in the TaskStore (or is not
// in the Running/Detached state). The handler must return NoSuchTask without
// touching streams — passing nil for ConnHandle is safe here precisely
// because the lookup fails before any stream-allocation step runs.
func TestHandleOpenPortForward_NoSuchTask(t *testing.T) {
	h := &TaskHandler{
		Tasks:    NewTaskStore(),
		Registry: NewRegistry(),
	}
	req := &protocol.OpenPortForwardRequest{TaskId: protocol.TaskID{Id: [16]byte{9, 9, 9}}}
	req.SetRemoteHost([]byte("127.0.0.1"))
	resp := h.handleOpenPortForward(nil, req)
	if resp.Status != protocol.OpenPortForwardStatus_NoSuchTask {
		t.Fatalf("got status %v, want NoSuchTask", resp.Status)
	}
}

// TestHandleOpenPortForward_DetachedTaskAccepted verifies that the status
// gate accepts Detached as well as Running. Mirrors the equivalent test in
// file_transfer_test.go. The runner is intentionally NOT registered: the
// expected outcome is RunnerOffline (proving the status gate let us through),
// not NoSuchTask (which would prove the gate rejected Detached).
func TestHandleOpenPortForward_DetachedTaskAccepted(t *testing.T) {
	h := &TaskHandler{
		Tasks:    NewTaskStore(),
		Registry: NewRegistry(),
	}
	var rawID [16]byte
	rawID[0] = 0xD3
	idHex := hex.EncodeToString(rawID[:])
	h.Tasks.mu.Lock()
	h.Tasks.tasks[idHex] = &TaskEntry{
		ID:         idHex,
		RepoPath:   "/repo",
		Status:     protocol.TaskStatus_Detached,
		Kind:       protocol.TaskKind_Interactive,
		Detachable: true,
		AssignedTo: "fake-runner-id",
	}
	h.Tasks.order = append(h.Tasks.order, idHex)
	h.Tasks.mu.Unlock()

	req := &protocol.OpenPortForwardRequest{TaskId: protocol.TaskID{Id: rawID}}
	req.SetRemoteHost([]byte("127.0.0.1"))
	req.RemotePort = 8080
	resp := h.handleOpenPortForward(nil, req)
	if resp.Status == protocol.OpenPortForwardStatus_NoSuchTask {
		t.Fatalf("detached task must not be rejected as NoSuchTask")
	}
	// Runner not registered → RunnerOffline.
	if resp.Status != protocol.OpenPortForwardStatus_RunnerOffline {
		t.Fatalf("expected RunnerOffline (no runner registered), got %v", resp.Status)
	}
}

// TestHandleOpenPortForward_RemoteRegisters verifies the ssh -R registration:
// the server creates a control stream (returned as StreamId), assigns a
// ForwardId, stores the registration, and sends the runner a listen request.
func TestHandleOpenPortForward_RemoteRegisters(t *testing.T) {
	h := &TaskHandler{Tasks: NewTaskStore(), Registry: NewRegistry()}
	var rawID [16]byte
	rawID[0] = 0x5A
	idHex := hex.EncodeToString(rawID[:])
	h.Tasks.mu.Lock()
	h.Tasks.tasks[idHex] = &TaskEntry{ID: idHex, Status: protocol.TaskStatus_Running, AssignedTo: "runner-1"}
	h.Tasks.order = append(h.Tasks.order, idHex)
	h.Tasks.mu.Unlock()

	runnerConn := &fakeConn{}
	h.Registry.Add(&RunnerEntry{ID: "runner-1", Conn: runnerConn})

	clientConn := &fakeConn{nextStreamID: 555}
	req := &protocol.OpenPortForwardRequest{
		TaskId:     protocol.TaskID{Id: rawID},
		Direction:  protocol.PortForwardDirection_Remote,
		RemotePort: 5432,
		BindPort:   15432,
	}
	req.SetRemoteHost([]byte("127.0.0.1"))
	req.SetBindAddr([]byte("127.0.0.1"))

	resp := h.handleOpenPortForward(clientConn, req)
	if resp.Status != protocol.OpenPortForwardStatus_Ok {
		t.Fatalf("status = %v, want Ok", resp.Status)
	}
	if resp.ForwardId == 0 {
		t.Fatal("ForwardId should be non-zero")
	}
	if resp.StreamId != 555 {
		t.Fatalf("StreamId = %d, want control stream id 555", resp.StreamId)
	}
	if _, ok := h.rforwards().get(resp.ForwardId); !ok {
		t.Fatal("registration not stored")
	}
	if len(runnerConn.sent) == 0 {
		t.Fatal("no listen request sent to runner")
	}
	var rr protocol.RunnerRequest
	if _, err := rr.Decode(runnerConn.sent[0][1:]); err != nil { // strip ApplicationPayloadKind byte
		t.Fatalf("decode runner req: %v", err)
	}
	if rr.Kind != protocol.RunnerRequestType_OpenPortForward {
		t.Fatalf("runner req kind = %v", rr.Kind)
	}
	body := rr.OpenPortForward()
	if body == nil || body.Direction != protocol.PortForwardDirection_Remote ||
		body.BindPort != 15432 || body.ForwardId != resp.ForwardId {
		t.Fatalf("runner req body = %+v", body)
	}
}
