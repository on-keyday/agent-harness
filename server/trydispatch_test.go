package server

import (
	"errors"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// errConnHandle is a fakeConn variant whose SendMessage always errors.
type errConnHandle struct {
	fakeConn
}

func (e *errConnHandle) SendMessage(_ []byte) (int, uint64, error) {
	return 0, 0, errors.New("send failed")
}

// newTestDispatcher builds a Dispatcher wired to fresh Registry and TaskStore.
func newTestDispatcher() (*Dispatcher, *Registry, *TaskStore) {
	reg := NewRegistry()
	tasks := NewTaskStore()
	d := &Dispatcher{
		Registry: reg,
		Tasks:    tasks,
	}
	return d, reg, tasks
}

// registerRunner adds a runner entry with the given conn to the registry.
func registerRunner(reg *Registry, id string, conn ConnHandle, roots []string, maxTasks int) {
	reg.Add(&RunnerEntry{
		ID:           id,
		Hostname:     "host",
		AllowedRoots: roots,
		MaxTasks:     maxTasks,
		ActiveTasks:  map[string]struct{}{},
		ConnectedAt:  time.Unix(1, 0),
		LastSeen:     time.Unix(1, 0),
		Conn:         conn,
	})
}

// TestTryDispatch_HappyPath verifies that tryDispatch binds the runner, sends an
// AssignTask wire message, and transitions the task to Running.
func TestTryDispatch_HappyPath(t *testing.T) {
	d, reg, tasks := newTestDispatcher()
	fc := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:8539-10")}
	runnerID := fc.id.String()
	registerRunner(reg, runnerID, fc, []string{"/repo"}, 2)

	taskID := tasks.Create("/repo", "do work", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, "", protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any})
	task, _ := tasks.Get(taskID)

	ok := d.TryDispatch(task)
	if !ok {
		t.Fatal("expected TryDispatch to return true on happy path")
	}

	// Runner must have the task bound.
	entry, _ := reg.Get(runnerID)
	if _, has := entry.ActiveTasks[taskID]; !has {
		t.Errorf("expected task %q in runner ActiveTasks, got %v", taskID, entry.ActiveTasks)
	}

	// Task must be Running.
	te, _ := tasks.Get(taskID)
	if te.Status != protocol.TaskStatus_Running {
		t.Errorf("expected task status Running, got %v", te.Status)
	}
	if te.AssignedTo != runnerID {
		t.Errorf("expected AssignedTo=%q, got %q", runnerID, te.AssignedTo)
	}

	// Exactly one message must have been sent, prefixed with RunnerControl kind.
	if len(fc.sent) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(fc.sent))
	}
	if fc.sent[0][0] != byte(wire.ApplicationPayloadKind_RunnerControl) {
		t.Errorf("expected RunnerControl prefix byte, got %d", fc.sent[0][0])
	}

	// Decode and verify it is an AssignTask with correct repo_path.
	var req protocol.RunnerRequest
	if _, err := req.Decode(fc.sent[0][1:]); err != nil {
		t.Fatalf("decode RunnerRequest: %v", err)
	}
	if req.Kind != protocol.RunnerRequestType_AssignTask {
		t.Errorf("expected AssignTask kind, got %v", req.Kind)
	}
	at := req.AssignTask()
	if at == nil {
		t.Fatal("AssignTask() returned nil")
	}
	if string(at.Prompt) != "do work" {
		t.Errorf("prompt mismatch: got %q", at.Prompt)
	}
	if string(at.RepoPath) != "/repo" {
		t.Errorf("repo_path mismatch: got %q", at.RepoPath)
	}
}

// TestTryDispatch_NoCapacity verifies that tryDispatch returns false when all
// candidates are at capacity, and sends no messages.
func TestTryDispatch_NoCapacity(t *testing.T) {
	d, reg, tasks := newTestDispatcher()
	fc := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:8539-11")}
	runnerID := fc.id.String()

	// Runner at full capacity (MaxTasks=1, 1 active task).
	reg.Add(&RunnerEntry{
		ID:           runnerID,
		Hostname:     "host",
		AllowedRoots: []string{"/repo"},
		MaxTasks:     1,
		ActiveTasks:  map[string]struct{}{"existingtask": {}},
		ConnectedAt:  time.Unix(1, 0),
		LastSeen:     time.Unix(1, 0),
		Conn:         fc,
	})

	taskID := tasks.Create("/repo", "do work", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, "", protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any})
	task, _ := tasks.Get(taskID)

	ok := d.TryDispatch(task)
	if ok {
		t.Fatal("expected TryDispatch to return false when no capacity")
	}
	if len(fc.sent) != 0 {
		t.Errorf("expected no messages sent, got %d", len(fc.sent))
	}
	// Task must remain Queued.
	te, _ := tasks.Get(taskID)
	if te.Status != protocol.TaskStatus_Queued {
		t.Errorf("expected task to remain Queued, got %v", te.Status)
	}
}

// TestTryDispatch_SendError verifies that tryDispatch rolls back the BindTask
// reservation when SendMessage fails, and returns false.
func TestTryDispatch_SendError(t *testing.T) {
	d, reg, tasks := newTestDispatcher()

	fc := &errConnHandle{
		fakeConn: fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:8539-12")},
	}
	runnerID := fc.id.String()
	registerRunner(reg, runnerID, fc, []string{"/repo"}, 2)

	taskID := tasks.Create("/repo", "work", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, "", protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any})
	task, _ := tasks.Get(taskID)

	ok := d.TryDispatch(task)
	if ok {
		t.Fatal("expected TryDispatch to return false on send error")
	}

	// Runner slot must have been rolled back (UnbindTask called).
	entry, _ := reg.Get(runnerID)
	if _, has := entry.ActiveTasks[taskID]; has {
		t.Errorf("expected task to be unbound after send error, ActiveTasks=%v", entry.ActiveTasks)
	}

	// Task must remain Queued (not Running).
	te, _ := tasks.Get(taskID)
	if te.Status != protocol.TaskStatus_Queued {
		t.Errorf("expected task to remain Queued after send error, got %v", te.Status)
	}
}
