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
	hello := protocol.RunnerHello{Version: 1, MaxTasks: 2}
	hello.SetHostname([]byte("myhost"))
	ar := protocol.AllowedRoot{}
	ar.SetPath([]byte("/foo"))
	hello.SetAllowedRoots([]protocol.AllowedRoot{ar})
	msg.SetHello(hello)

	payload := encodeRunnerMessage(t, msg)
	h.Handle(fc, payload)

	runnerID := fc.ConnectionID().String()
	entry, ok := reg.Get(runnerID)
	if !ok {
		t.Fatalf("expected runner entry for ID %q, not found", runnerID)
	}
	if entry.Hostname != "myhost" {
		t.Errorf("expected Hostname myhost, got %q", entry.Hostname)
	}
	if len(entry.AllowedRoots) == 0 || entry.AllowedRoots[0] != "/foo" {
		t.Errorf("expected AllowedRoots[/foo], got %v", entry.AllowedRoots)
	}
	if entry.MaxTasks != 2 {
		t.Errorf("expected MaxTasks 2, got %d", entry.MaxTasks)
	}
	if entry.Status() != protocol.RunnerStatus_Idle {
		t.Errorf("expected Status Idle (has Conn, no active tasks), got %v", entry.Status())
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

	// Pre-populate Registry with a Busy runner that has the task bound.
	reg.Add(&RunnerEntry{
		ID:           runnerID,
		Hostname:     "h",
		AllowedRoots: []string{"/repo"},
		MaxTasks:     1,
		ActiveTasks:  map[string]struct{}{taskID: {}},
		ConnectedAt:  time.Now(),
		LastSeen:     time.Now(),
		Conn:         fc,
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

	// Verify runner slot was released.
	runnerEntry, ok := reg.Get(runnerID)
	if !ok {
		t.Fatalf("runner %q not found after Handle", runnerID)
	}
	if len(runnerEntry.ActiveTasks) != 0 {
		t.Errorf("expected runner ActiveTasks empty after TaskFinished, got %v", runnerEntry.ActiveTasks)
	}
	if runnerEntry.Status() != protocol.RunnerStatus_Idle {
		t.Errorf("expected runner Status Idle after TaskFinished, got %v", runnerEntry.Status())
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
		ID:           runnerID,
		Hostname:     "h",
		AllowedRoots: []string{"/repo"},
		MaxTasks:     1,
		ActiveTasks:  map[string]struct{}{taskID: {}},
		ConnectedAt:  time.Now(),
		LastSeen:     time.Now(),
		Conn:         fc,
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
		ID:           runnerID,
		Hostname:     "h",
		AllowedRoots: []string{"/repo"},
		MaxTasks:     1,
		ActiveTasks:  map[string]struct{}{},
		ConnectedAt:  t0,
		LastSeen:     t0,
		Conn:         fc,
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

func TestTaskAcceptedUpdatesLastSeen(t *testing.T) {
	reg := NewRegistry()
	tasks := NewTaskStore()
	changeCalled := 0

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(5 * time.Second)

	nowFn := func() time.Time { return t1 }

	h := &RunnerHandler{
		Registry: reg,
		Tasks:    tasks,
		Now:      nowFn,
		OnChange: func() { changeCalled++ },
	}

	fc := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:8539-6")}
	runnerID := fc.ConnectionID().String()

	var rawID [16]byte
	rawID[0] = 0x99
	taskID := hex.EncodeToString(rawID[:])

	// Register the runner with LastSeen at t0 and an active task.
	reg.Add(&RunnerEntry{
		ID:           runnerID,
		Hostname:     "h",
		AllowedRoots: []string{"/repo"},
		MaxTasks:     1,
		ActiveTasks:  map[string]struct{}{taskID: {}},
		ConnectedAt:  t0,
		LastSeen:     t0,
		Conn:         fc,
	})

	ta := protocol.TaskAccepted{}
	ta.TaskId.Id = rawID

	msg := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_TaskAccepted}
	msg.SetTaskAccepted(ta)

	payload := encodeRunnerMessage(t, msg)
	h.Handle(fc, payload)

	entry, ok := reg.Get(runnerID)
	if !ok {
		t.Fatalf("runner %q not found after Handle", runnerID)
	}
	if !entry.LastSeen.Equal(t1) {
		t.Errorf("expected LastSeen %v after TaskAccepted, got %v", t1, entry.LastSeen)
	}

	if changeCalled != 1 {
		t.Errorf("expected OnChange called 1 time, got %d", changeCalled)
	}
}

func TestTaskAcceptedMismatchStillUpdatesLastSeen(t *testing.T) {
	reg := NewRegistry()
	tasks := NewTaskStore()
	changeCalled := 0

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(5 * time.Second)

	nowFn := func() time.Time { return t1 }

	h := &RunnerHandler{
		Registry: reg,
		Tasks:    tasks,
		Now:      nowFn,
		OnChange: func() { changeCalled++ },
	}

	fc := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:8539-7")}
	runnerID := fc.ConnectionID().String()

	var expectedRawID [16]byte
	expectedRawID[0] = 0xAA
	expectedTaskID := hex.EncodeToString(expectedRawID[:])

	// Register the runner with LastSeen at t0 and an active task.
	reg.Add(&RunnerEntry{
		ID:           runnerID,
		Hostname:     "h",
		AllowedRoots: []string{"/repo"},
		MaxTasks:     1,
		ActiveTasks:  map[string]struct{}{expectedTaskID: {}},
		ConnectedAt:  t0,
		LastSeen:     t0,
		Conn:         fc,
	})

	// Send TaskAccepted with a DIFFERENT TaskID (all 0xFF bytes).
	var differentRawID [16]byte
	for i := range differentRawID {
		differentRawID[i] = 0xFF
	}

	ta := protocol.TaskAccepted{}
	ta.TaskId.Id = differentRawID

	msg := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_TaskAccepted}
	msg.SetTaskAccepted(ta)

	payload := encodeRunnerMessage(t, msg)
	h.Handle(fc, payload)

	// Assert: LastSeen was updated to t1 despite the mismatch warning.
	entry, ok := reg.Get(runnerID)
	if !ok {
		t.Fatalf("runner %q not found after Handle", runnerID)
	}
	if !entry.LastSeen.Equal(t1) {
		t.Errorf("expected LastSeen %v after TaskAccepted (mismatch case), got %v", t1, entry.LastSeen)
	}

	// The mismatch warning does not prevent OnChange from being called.
	if changeCalled != 1 {
		t.Errorf("expected OnChange called 1 time, got %d", changeCalled)
	}
}

// TestRunnerHandlerTaskFinishedReleasesCapacity verifies that receiving a
// TaskFinished message causes UnbindTask to remove the task from the runner's
// ActiveTasks, releasing the capacity slot for future dispatch.
func TestRunnerHandlerTaskFinishedReleasesCapacity(t *testing.T) {
	reg := NewRegistry()
	tasks := NewTaskStore()
	h := &RunnerHandler{
		Registry: reg,
		Tasks:    tasks,
		Now:      time.Now,
		OnChange: func() {},
	}

	fc := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:8539-10")}
	runnerID := fc.ConnectionID().String()

	// Step 1: a runner with MaxTasks=1 and ActiveTasks={"t1"}.
	// Use a fixed 16-byte raw ID to represent task "t1".
	var rawID [16]byte
	rawID[0] = 0x01
	taskIDHex := hex.EncodeToString(rawID[:])

	reg.Add(&RunnerEntry{
		ID:          runnerID,
		Hostname:    "host",
		AllowedRoots: []string{"/repo"},
		MaxTasks:    1,
		ActiveTasks: map[string]struct{}{taskIDHex: {}},
		ConnectedAt: time.Now(),
		LastSeen:    time.Now(),
		Conn:        fc,
	})

	// Step 2: add + MarkRunning task t1 in the TaskStore.
	tasks.mu.Lock()
	tasks.tasks[taskIDHex] = &TaskEntry{
		ID:       taskIDHex,
		RepoPath: "/repo",
		Prompt:   "t1",
		Status:   protocol.TaskStatus_Running,
	}
	tasks.order = append(tasks.order, taskIDHex)
	tasks.mu.Unlock()

	// Confirm runner is Busy before the message.
	entry, ok := reg.Get(runnerID)
	if !ok {
		t.Fatal("runner not found before TaskFinished")
	}
	if entry.Status() != protocol.RunnerStatus_Busy {
		t.Fatalf("expected runner Busy before TaskFinished, got %v", entry.Status())
	}

	// Step 3: construct and dispatch a TaskFinished message.
	tf := protocol.TaskFinished{ExitCode: 0}
	tf.TaskId.Id = rawID
	msg := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_TaskFinished}
	msg.SetTaskFinished(tf)
	h.Handle(fc, encodeRunnerMessage(t, msg))

	// Step 4: assert ActiveTasks no longer contains taskIDHex.
	entry, ok = reg.Get(runnerID)
	if !ok {
		t.Fatal("runner not found after TaskFinished")
	}
	if _, stillBound := entry.ActiveTasks[taskIDHex]; stillBound {
		t.Errorf("expected ActiveTasks to not contain %q after TaskFinished, got %v", taskIDHex, entry.ActiveTasks)
	}
	if entry.Status() != protocol.RunnerStatus_Idle {
		t.Errorf("expected runner Status Idle after TaskFinished, got %v", entry.Status())
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
