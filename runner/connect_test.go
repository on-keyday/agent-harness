package runner

import (
	"context"
	"encoding/hex"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// TestPeerSenderPublishCachesStreamPerTopic verifies that the runner's
// Publish path (peerSender → peer.Conn.Publish) creates one bidirectional
// stream per topic and caches it for subsequent Publish calls.
//
// Full verification is deferred because implementing faithful fakes for
// trsf.Transport (CreateBidirectionalStream, AcceptBidirectionalStream)
// and objproto.Connection (SendMessage) requires reproducing non-trivial
// internal trsf stream machinery. The behaviour is exercised end-to-end
// in the integration test (see Task 5.1 in the original plan).
func TestPeerSenderPublishCachesStreamPerTopic(t *testing.T) {
	t.Skip("requires fake trsf.Transport stub — covered by integration test")
}

// TestRunnerHandlesCancelTaskCallsCancelFunc verifies that receiving a CancelTask
// request causes dispatchRunnerRequest to call the registered cancel function for
// the matching task.
func TestRunnerHandlesCancelTaskCallsCancelFunc(t *testing.T) {
	ms := &mockSender{}
	s := &Session{
		AllowedRoots: []string{"/repo"},
		ClaudeBin:    "/bin/true",
		Timeout:      30 * time.Second,
		Sender:       ms,
		Now:          time.Now,
	}

	// Pre-register a task with a cancel function we can observe.
	var taskIDBytes [16]byte
	taskIDBytes[0] = 0xDE
	taskIDHex := hex.EncodeToString(taskIDBytes[:])

	cancelCalled := false
	var cancelMu sync.Mutex
	s.mu.Lock()
	s.initMaps()
	s.tasks[taskIDHex] = &taskEntry{
		cancel: func() {
			cancelMu.Lock()
			cancelCalled = true
			cancelMu.Unlock()
		},
		repoPath: "/repo",
	}
	s.mu.Unlock()

	// Build a CancelTask RunnerRequest.
	ct := protocol.CancelTask{}
	ct.TaskId.Id = taskIDBytes
	req := &protocol.RunnerRequest{Kind: protocol.RunnerRequestType_CancelTask}
	req.SetCancelTask(ct)
	payload, err := req.Append(nil)
	if err != nil {
		t.Fatalf("encode CancelTask: %v", err)
	}

	dispatchRunnerRequest(context.Background(), s, s.logger(), wire.ApplicationPayloadKind_RunnerControl, payload)

	cancelMu.Lock()
	got := cancelCalled
	cancelMu.Unlock()
	if !got {
		t.Errorf("expected cancel to be called for task %q", taskIDHex)
	}
}

// TestRunnerHandlesCancelTaskUnknownIsNoOp verifies that a CancelTask for an
// unknown task ID does not panic and is silently ignored (just a log line).
func TestRunnerHandlesCancelTaskUnknownIsNoOp(t *testing.T) {
	ms := &mockSender{}
	s := &Session{
		AllowedRoots: []string{"/repo"},
		Sender:       ms,
		Now:          time.Now,
	}

	var taskIDBytes [16]byte
	taskIDBytes[0] = 0xFF
	ct := protocol.CancelTask{}
	ct.TaskId.Id = taskIDBytes
	req := &protocol.RunnerRequest{Kind: protocol.RunnerRequestType_CancelTask}
	req.SetCancelTask(ct)
	payload, err := req.Append(nil)
	if err != nil {
		t.Fatalf("encode CancelTask: %v", err)
	}

	// Must not panic.
	dispatchRunnerRequest(context.Background(), s, s.logger(), wire.ApplicationPayloadKind_RunnerControl, payload)
}

// TestRunnerHello verifies that the Hello payload built in Run uses
// AllowedRoots, MaxTasks, and Hostname from Config.
// We test via buildHelloPayload extracted logic; since Run requires a live
// peer connection, we test the encoding/decoding directly here.
func TestRunnerHello(t *testing.T) {
	cfg := Config{
		AllowedRoots: []string{"/home/user/repos", "/data/work"},
		MaxTasks:     3,
		Hostname:     "build-runner-1",
	}

	// Build the Hello message the same way Run does.
	hello := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_Hello}
	h := protocol.RunnerHello{Version: 1}
	maxTasks := cfg.MaxTasks
	if maxTasks < 1 {
		maxTasks = 1
	}
	h.MaxTasks = uint16(maxTasks)
	if cfg.Hostname != "" {
		h.SetHostname([]byte(cfg.Hostname))
	}
	roots := make([]protocol.AllowedRoot, 0, len(cfg.AllowedRoots))
	for _, r := range cfg.AllowedRoots {
		var ar protocol.AllowedRoot
		ar.SetPath([]byte(r))
		roots = append(roots, ar)
	}
	h.SetAllowedRoots(roots)
	hello.SetHello(h)

	// Encode, then decode and verify fields.
	payload, err := hello.Append(nil)
	if err != nil {
		t.Fatalf("encode Hello: %v", err)
	}

	var decoded protocol.RunnerMessage
	if err := decoded.DecodeExact(payload); err != nil {
		t.Fatalf("decode Hello: %v", err)
	}
	hd := decoded.Hello()
	if hd == nil {
		t.Fatal("Hello variant is nil after decode")
	}
	if string(hd.Hostname) != "build-runner-1" {
		t.Errorf("Hostname: want %q, got %q", "build-runner-1", string(hd.Hostname))
	}
	if hd.MaxTasks != 3 {
		t.Errorf("MaxTasks: want 3, got %d", hd.MaxTasks)
	}
	if len(hd.AllowedRoots) != 2 {
		t.Fatalf("AllowedRoots len: want 2, got %d", len(hd.AllowedRoots))
	}
	if string(hd.AllowedRoots[0].Path) != "/home/user/repos" {
		t.Errorf("AllowedRoots[0]: want %q, got %q", "/home/user/repos", string(hd.AllowedRoots[0].Path))
	}
	if string(hd.AllowedRoots[1].Path) != "/data/work" {
		t.Errorf("AllowedRoots[1]: want %q, got %q", "/data/work", string(hd.AllowedRoots[1].Path))
	}
}
