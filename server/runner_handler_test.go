package server

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// encodeRunnerMessage encodes a RunnerMessage to its wire form (including Kind byte).
func encodeRunnerMessage(t *testing.T, msg *protocol.RunnerMessage) []byte {
	t.Helper()
	b, err := msg.Append(nil)
	if err != nil {
		t.Fatalf("failed to encode RunnerMessage: %v", err)
	}
	return b
}

func TestHelloRegistersRunner(t *testing.T) {
	reg := NewRegistry()
	tasks := NewTaskStore()
	changeCalled := 0
	h := &RunnerHandler{
		Registry: reg,
		Tasks:    tasks,
		Now:      time.Now,
		OnChange: func() { changeCalled++ },
	}

	fc := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:8539-1")}

	msg := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_Hello}
	hello := protocol.RunnerHello{Version: 1}
	hello.SetRepoPath([]byte("/foo"))
	msg.SetHello(hello)

	payload := encodeRunnerMessage(t, msg)
	h.Handle(fc, payload)

	runnerID := fc.ConnectionID().String()
	entry, ok := reg.Get(runnerID)
	if !ok {
		t.Fatalf("expected runner entry for ID %q, not found", runnerID)
	}
	if entry.RepoPath != "/foo" {
		t.Errorf("expected RepoPath /foo, got %q", entry.RepoPath)
	}
	if entry.Status != protocol.RunnerStatus_Idle {
		t.Errorf("expected Status Idle, got %v", entry.Status)
	}
	if changeCalled != 1 {
		t.Errorf("expected OnChange called 1 time, got %d", changeCalled)
	}
}

func TestTaskFinishedUpdatesStore(t *testing.T) {
	reg := NewRegistry()
	tasks := NewTaskStore()
	changeCalled := 0
	h := &RunnerHandler{
		Registry: reg,
		Tasks:    tasks,
		Now:      time.Now,
		OnChange: func() { changeCalled++ },
	}

	fc := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:8539-2")}
	runnerID := fc.ConnectionID().String()

	// Use a known 16-byte task ID.
	var rawID [16]byte
	rawID[15] = 0x42
	taskID := hex.EncodeToString(rawID[:])

	// Pre-populate Registry with a Busy runner whose CurrentTask matches.
	reg.Add(&RunnerEntry{
		ID:          runnerID,
		RepoPath:    "/repo",
		Status:      protocol.RunnerStatus_Busy,
		CurrentTask: taskID,
		ConnectedAt: time.Now(),
		LastSeen:    time.Now(),
	})

	// Pre-populate TaskStore with a Running task.
	// We use internal mutation here since TaskStore.Create generates a random ID;
	// instead, manually inject the task.
	tasks.mu.Lock()
	tasks.tasks[taskID] = &TaskEntry{
		ID:       taskID,
		RepoPath: "/repo",
		Prompt:   "test",
		Status:   protocol.TaskStatus_Running,
	}
	tasks.order = append(tasks.order, taskID)
	tasks.mu.Unlock()

	tf := protocol.TaskFinished{
		ExitCode: 0,
	}
	tf.TaskId.Id = rawID

	msg := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_TaskFinished}
	msg.SetTaskFinished(tf)

	payload := encodeRunnerMessage(t, msg)
	h.Handle(fc, payload)

	// Verify task status.
	taskEntry, ok := tasks.Get(taskID)
	if !ok {
		t.Fatalf("task %q not found after Handle", taskID)
	}
	if taskEntry.Status != protocol.TaskStatus_Succeeded {
		t.Errorf("expected task Status Succeeded, got %v", taskEntry.Status)
	}
	if taskEntry.ExitCode == nil || *taskEntry.ExitCode != 0 {
		t.Errorf("expected ExitCode 0, got %v", taskEntry.ExitCode)
	}

	// Verify runner status.
	runnerEntry, ok := reg.Get(runnerID)
	if !ok {
		t.Fatalf("runner %q not found after Handle", runnerID)
	}
	if runnerEntry.Status != protocol.RunnerStatus_Idle {
		t.Errorf("expected runner Status Idle, got %v", runnerEntry.Status)
	}
	if runnerEntry.CurrentTask != "" {
		t.Errorf("expected runner CurrentTask empty, got %q", runnerEntry.CurrentTask)
	}

	if changeCalled != 1 {
		t.Errorf("expected OnChange called 1 time, got %d", changeCalled)
	}
}

func TestTaskStartedSetsWorktreeDir(t *testing.T) {
	reg := NewRegistry()
	tasks := NewTaskStore()
	changeCalled := 0
	h := &RunnerHandler{
		Registry: reg,
		Tasks:    tasks,
		Now:      time.Now,
		OnChange: func() { changeCalled++ },
	}

	fc := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:8539-3")}
	runnerID := fc.ConnectionID().String()

	var rawID [16]byte
	rawID[0] = 0xAB
	taskID := hex.EncodeToString(rawID[:])

	// Pre-populate Registry.
	reg.Add(&RunnerEntry{
		ID:          runnerID,
		RepoPath:    "/repo",
		Status:      protocol.RunnerStatus_Busy,
		CurrentTask: taskID,
		ConnectedAt: time.Now(),
		LastSeen:    time.Now(),
	})

	// Pre-populate TaskStore with a Running task.
	tasks.mu.Lock()
	tasks.tasks[taskID] = &TaskEntry{
		ID:       taskID,
		RepoPath: "/repo",
		Prompt:   "test",
		Status:   protocol.TaskStatus_Running,
	}
	tasks.order = append(tasks.order, taskID)
	tasks.mu.Unlock()

	ts := protocol.TaskStarted{}
	ts.TaskId.Id = rawID
	ts.SetWorktreeDir([]byte("/some/wt"))

	msg := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_TaskStarted}
	msg.SetTaskStarted(ts)

	payload := encodeRunnerMessage(t, msg)
	h.Handle(fc, payload)

	taskEntry, ok := tasks.Get(taskID)
	if !ok {
		t.Fatalf("task %q not found after Handle", taskID)
	}
	if taskEntry.WorktreeDir != "/some/wt" {
		t.Errorf("expected WorktreeDir /some/wt, got %q", taskEntry.WorktreeDir)
	}

	if changeCalled != 1 {
		t.Errorf("expected OnChange called 1 time, got %d", changeCalled)
	}
}

func TestHeartbeatUpdatesLastSeen(t *testing.T) {
	reg := NewRegistry()
	tasks := NewTaskStore()
	changeCalled := 0

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Second)

	nowFn := func() time.Time { return t1 }

	h := &RunnerHandler{
		Registry: reg,
		Tasks:    tasks,
		Now:      nowFn,
		OnChange: func() { changeCalled++ },
	}

	fc := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:8539-4")}
	runnerID := fc.ConnectionID().String()

	// Pre-populate registry with LastSeen at t0.
	reg.Add(&RunnerEntry{
		ID:          runnerID,
		RepoPath:    "/repo",
		Status:      protocol.RunnerStatus_Idle,
		ConnectedAt: t0,
		LastSeen:    t0,
	})

	msg := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_Heartbeat}

	payload := encodeRunnerMessage(t, msg)
	h.Handle(fc, payload)

	entry, ok := reg.Get(runnerID)
	if !ok {
		t.Fatalf("runner %q not found after Handle", runnerID)
	}
	if !entry.LastSeen.Equal(t1) {
		t.Errorf("expected LastSeen %v, got %v", t1, entry.LastSeen)
	}

	if changeCalled != 1 {
		t.Errorf("expected OnChange called 1 time, got %d", changeCalled)
	}
}

func TestMalformedPayloadIsIgnored(t *testing.T) {
	reg := NewRegistry()
	tasks := NewTaskStore()
	changeCalled := 0
	h := &RunnerHandler{
		Registry: reg,
		Tasks:    tasks,
		Now:      time.Now,
		OnChange: func() { changeCalled++ },
	}

	fc := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:8539-5")}

	// Garbage bytes — should not panic and should not call OnChange.
	h.Handle(fc, []byte{0xFF, 0xFF})

	if changeCalled != 0 {
		t.Errorf("expected OnChange not called for malformed payload, got %d", changeCalled)
	}
}
