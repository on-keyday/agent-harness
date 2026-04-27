package server

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// classifyMsg inspects the first byte of a message to determine its
// ApplicationPayloadKind, then decodes the RunnerRequest to check the
// RunnerRequestType. Returns ("assign"|"cancel"|"unknown") for test assertions.
func classifyRunnerRequest(msg []byte) string {
	if len(msg) == 0 {
		return "empty"
	}
	if wire.ApplicationPayloadKind(msg[0]) != wire.ApplicationPayloadKind_RunnerControl {
		return "not-runner-control"
	}
	var req protocol.RunnerRequest
	if _, err := req.Decode(msg[1:]); err != nil {
		return "decode-error"
	}
	switch req.Kind {
	case protocol.RunnerRequestType_AssignTask:
		return "assign"
	case protocol.RunnerRequestType_CancelTask:
		return "cancel"
	default:
		return "unknown"
	}
}

// TestDispatcherOnCancel verifies that Dispatcher.OnCancel sends a CancelTask
// message to the runner identified by AssignedTo (the running runner) when the
// task is in Running state.
func TestDispatcherOnCancel_ForwardsToRunner(t *testing.T) {
	reg := NewRegistry()
	tasks := NewTaskStore()
	d := &Dispatcher{Registry: reg, Tasks: tasks}

	fc := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:8539-20")}
	runnerID := fc.id.String()
	reg.Add(&RunnerEntry{
		ID:           runnerID,
		Hostname:     "host",
		AllowedRoots: []string{"/repo"},
		MaxTasks:     2,
		ActiveTasks:  map[string]struct{}{},
		ConnectedAt:  time.Unix(1, 0),
		LastSeen:     time.Unix(1, 0),
		Conn:         fc,
	})

	// Create a task and manually assign it (simulating TryDispatch success).
	taskID := tasks.Create("/repo", "work", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, "", protocol.RunnerSelector{})
	tasks.Assign(taskID, runnerID, "")
	// Also bind it in the registry (as TryDispatch would have done).
	reg.BindTask(runnerID, taskID)

	// Call OnCancel.
	d.OnCancel(taskID)

	// The runner must have received a CancelTask message.
	if len(fc.sent) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(fc.sent))
	}
	if classifyRunnerRequest(fc.sent[0]) != "cancel" {
		t.Errorf("expected cancel message, got %q", classifyRunnerRequest(fc.sent[0]))
	}

	// Capacity must NOT be released (UnbindTask must not have been called).
	entry, _ := reg.Get(runnerID)
	if _, has := entry.ActiveTasks[taskID]; !has {
		t.Errorf("expected task still bound after OnCancel (capacity released only via TaskFinished), ActiveTasks=%v", entry.ActiveTasks)
	}
}

// TestDispatcherOnCancel_NoRunner verifies that OnCancel is a no-op when the
// task has no assigned runner (e.g. still Queued or never dispatched).
func TestDispatcherOnCancel_NoRunner(t *testing.T) {
	reg := NewRegistry()
	tasks := NewTaskStore()
	d := &Dispatcher{Registry: reg, Tasks: tasks}

	taskID := tasks.Create("/repo", "work", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, "", protocol.RunnerSelector{})

	// Should not panic; no runner to forward to.
	d.OnCancel(taskID)
}

// TestDispatcherOnCancel_WiredViaTaskStoreCallback verifies that the server.go
// wiring causes a Cancel on the TaskStore to invoke Dispatcher.OnCancel, which
// in turn sends a CancelTask to the running runner. This tests the full chain:
// tasks.Cancel(id) → tasks.OnCancel callback → d.OnCancel(id) → runner.
func TestDispatcherOnCancel_WiredViaTaskStoreCallback(t *testing.T) {
	reg := NewRegistry()
	tasks := NewTaskStore()
	d := &Dispatcher{Registry: reg, Tasks: tasks}

	fc := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:8539-21")}
	runnerID := fc.id.String()
	reg.Add(&RunnerEntry{
		ID:           runnerID,
		Hostname:     "host",
		AllowedRoots: []string{"/repo"},
		MaxTasks:     2,
		ActiveTasks:  map[string]struct{}{},
		ConnectedAt:  time.Unix(1, 0),
		LastSeen:     time.Unix(1, 0),
		Conn:         fc,
	})

	taskID := tasks.Create("/repo", "work", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, "", protocol.RunnerSelector{})
	tasks.Assign(taskID, runnerID, "")
	reg.BindTask(runnerID, taskID)

	// Wire the OnCancel callback as server.go should (chain with existing publish).
	cancelEvents := 0
	tasks.OnCancel = func(id string) {
		cancelEvents++
		d.OnCancel(id)
	}

	// Trigger cancel via TaskStore.
	tasks.Cancel(taskID)

	if cancelEvents != 1 {
		t.Errorf("expected OnCancel callback called once, got %d", cancelEvents)
	}
	if len(fc.sent) != 1 {
		t.Fatalf("expected 1 message forwarded to runner, got %d", len(fc.sent))
	}
	if classifyRunnerRequest(fc.sent[0]) != "cancel" {
		t.Errorf("expected cancel message kind, got %q", classifyRunnerRequest(fc.sent[0]))
	}
}
