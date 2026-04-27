package server

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// encodeTaskControlRequest encodes a TaskControlRequest to its wire form (including Kind byte).
func encodeTaskControlRequest(t *testing.T, req *protocol.TaskControlRequest) []byte {
	t.Helper()
	b, err := req.Append(nil)
	if err != nil {
		t.Fatalf("failed to encode TaskControlRequest: %v", err)
	}
	return b
}

func TestSubmitCreatesTaskAndReplies(t *testing.T) {
	fc := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9001-1")}
	tasks := NewTaskStore()
	reg := NewRegistry()
	changeCalled := 0

	h := &TaskHandler{
		Tasks:    tasks,
		Registry: reg,
		OnChange: func() { changeCalled++ },
	}

	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_Submit}
	sub := protocol.SubmitRequest{}
	sub.SetRepoPath([]byte("/repo"))
	sub.SetPrompt([]byte("do stuff"))
	req.SetSubmit(sub)

	payload := encodeTaskControlRequest(t, req)
	h.Handle(fc, payload)

	// Assert exactly 1 message was sent.
	if len(fc.sent) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(fc.sent))
	}

	// The first byte should be ApplicationPayloadKind_TaskControl (7).
	msg := fc.sent[0]
	if len(msg) < 2 {
		t.Fatalf("response message too short: %d bytes", len(msg))
	}
	// Decode the TaskControlResponse (skip the leading kind byte).
	var resp protocol.TaskControlResponse
	if err := resp.DecodeExact(msg[1:]); err != nil {
		t.Fatalf("failed to decode TaskControlResponse: %v", err)
	}
	if resp.Kind != protocol.TaskControlKind_Submit {
		t.Errorf("expected response Kind=Submit, got %v", resp.Kind)
	}
	submitResp := resp.Submit()
	if submitResp == nil {
		t.Fatal("expected non-nil Submit() in response")
	}
	// TaskId should be non-zero (16 bytes).
	allZero := true
	for _, b := range submitResp.TaskId.Id {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Error("expected non-zero TaskId in SubmitResponse")
	}

	// TaskStore should have exactly 1 task.
	taskList := tasks.List(100)
	if len(taskList) != 1 {
		t.Fatalf("expected 1 task in store, got %d", len(taskList))
	}
	if taskList[0].RepoPath != "/repo" {
		t.Errorf("expected RepoPath /repo, got %q", taskList[0].RepoPath)
	}
	if taskList[0].Prompt != "do stuff" {
		t.Errorf("expected Prompt 'do stuff', got %q", taskList[0].Prompt)
	}

	// OnChange should have been called once.
	if changeCalled != 1 {
		t.Errorf("expected OnChange called 1 time, got %d", changeCalled)
	}
}

func TestListReturnsRunnersAndTasks(t *testing.T) {
	fc := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9001-2")}
	tasks := NewTaskStore()
	reg := NewRegistry()
	changeCalled := 0

	h := &TaskHandler{
		Tasks:    tasks,
		Registry: reg,
		OnChange: func() { changeCalled++ },
	}

	// Pre-populate Registry with 1 Idle runner serving "/x".
	reg.Add(&RunnerEntry{
		ID:           "runner-1",
		Hostname:     "h1",
		AllowedRoots: []string{"/x"},
		MaxTasks:     1,
		ActiveTasks:  map[string]struct{}{},
		ConnectedAt:  time.Now(),
		LastSeen:     time.Now(),
	})

	// Pre-populate TaskStore with 1 Queued task on "/x".
	taskID := tasks.Create("/x", "list-prompt", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified)

	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_List}
	req.SetList(protocol.ListQuery{})

	payload := encodeTaskControlRequest(t, req)
	h.Handle(fc, payload)

	if len(fc.sent) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(fc.sent))
	}

	msg := fc.sent[0]
	var resp protocol.TaskControlResponse
	if err := resp.DecodeExact(msg[1:]); err != nil {
		t.Fatalf("failed to decode TaskControlResponse: %v", err)
	}
	if resp.Kind != protocol.TaskControlKind_List {
		t.Errorf("expected response Kind=List, got %v", resp.Kind)
	}
	listResult := resp.List()
	if listResult == nil {
		t.Fatal("expected non-nil List() in response")
	}
	if listResult.RunnersLen != 1 {
		t.Errorf("expected RunnersLen=1, got %d", listResult.RunnersLen)
	}
	if listResult.TasksLen != 1 {
		t.Errorf("expected TasksLen=1, got %d", listResult.TasksLen)
	}
	if len(listResult.Runners) > 0 && string(listResult.Runners[0].Hostname) != "h1" {
		t.Errorf("expected runner Hostname h1, got %q", string(listResult.Runners[0].Hostname))
	}
	if len(listResult.Tasks) > 0 && string(listResult.Tasks[0].Prompt) != "list-prompt" {
		t.Errorf("expected task Prompt 'list-prompt', got %q", string(listResult.Tasks[0].Prompt))
	}

	// OnChange must NOT be called for List.
	if changeCalled != 0 {
		t.Errorf("expected OnChange NOT called for List, got %d", changeCalled)
	}

	_ = taskID // used to create the task
}

func TestCancelMarksTaskCancelled(t *testing.T) {
	fc := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9001-3")}
	tasks := NewTaskStore()
	reg := NewRegistry()
	changeCalled := 0

	h := &TaskHandler{
		Tasks:    tasks,
		Registry: reg,
		OnChange: func() { changeCalled++ },
	}

	// Pre-populate a Running task.
	var rawID [16]byte
	rawID[0] = 0xCA
	rawID[15] = 0xFE
	taskID := hex.EncodeToString(rawID[:])

	tasks.mu.Lock()
	tasks.tasks[taskID] = &TaskEntry{
		ID:       taskID,
		RepoPath: "/cancel-repo",
		Prompt:   "cancel me",
		Status:   protocol.TaskStatus_Running,
	}
	tasks.order = append(tasks.order, taskID)
	tasks.mu.Unlock()

	// Encode TaskControlRequest with Cancel for that taskID.
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_Cancel}
	cancel := protocol.CancelTask{}
	cancel.TaskId.Id = rawID
	req.SetCancel(cancel)

	payload := encodeTaskControlRequest(t, req)
	h.Handle(fc, payload)

	if len(fc.sent) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(fc.sent))
	}

	msg := fc.sent[0]
	var resp protocol.TaskControlResponse
	if err := resp.DecodeExact(msg[1:]); err != nil {
		t.Fatalf("failed to decode TaskControlResponse: %v", err)
	}
	if resp.Kind != protocol.TaskControlKind_Cancel {
		t.Errorf("expected response Kind=Cancel, got %v", resp.Kind)
	}
	cancelStatus := resp.Cancel()
	if cancelStatus == nil {
		t.Fatal("expected non-nil Cancel() in response")
	}
	if cancelStatus.Status != 0 {
		t.Errorf("expected CancelStatus.Status=0, got %d", cancelStatus.Status)
	}

	// Task should be Cancelled.
	entry, ok := tasks.Get(taskID)
	if !ok {
		t.Fatalf("task %q not found after Cancel", taskID)
	}
	if entry.Status != protocol.TaskStatus_Cancelled {
		t.Errorf("expected task Status=Cancelled, got %v", entry.Status)
	}

	// OnChange should have been called once.
	if changeCalled != 1 {
		t.Errorf("expected OnChange called 1 time, got %d", changeCalled)
	}
}

// TestMalformedPayloadIsIgnoredTask passes garbage bytes.
// bgn-generated code makes it impossible to construct a Submit-without-payload
// (the encoder returns "invalid union type for encoding" and the variant internal
// type asserts fail), so we test the malformed-payload path instead.
func TestMalformedPayloadIsIgnoredTask(t *testing.T) {
	fc := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9001-4")}
	tasks := NewTaskStore()
	reg := NewRegistry()
	changeCalled := 0

	h := &TaskHandler{
		Tasks:    tasks,
		Registry: reg,
		OnChange: func() { changeCalled++ },
	}

	// Garbage bytes — should not panic, no response, no OnChange.
	h.Handle(fc, []byte{0xFF, 0xFF, 0xDE, 0xAD})

	if len(fc.sent) != 0 {
		t.Errorf("expected no response for malformed payload, got %d messages", len(fc.sent))
	}
	if changeCalled != 0 {
		t.Errorf("expected OnChange NOT called for malformed payload, got %d", changeCalled)
	}
}
