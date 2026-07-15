package server

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
	"github.com/on-keyday/objtrsf/objproto"
)

// stubConn is a minimal ConnHandle used to mark a RunnerEntry as connected
// without wiring real network I/O. It no-ops all methods.
type stubConn struct{}

func (stubConn) ConnectionID() objproto.ConnectionID                           { return objproto.ConnectionID{} }
func (stubConn) SendMessage([]byte) (int, uint64, error)                       { return 0, 0, nil }
func (stubConn) CreateSendStream() trsf.SendStream                             { return nil }
func (stubConn) GetReceiveStream(trsf.StreamID) trsf.ReceiveStream             { return nil }
func (stubConn) CreateBidirectionalStream() trsf.BidirectionalStream           { return nil }
func (stubConn) GetBidirectionalStream(trsf.StreamID) trsf.BidirectionalStream { return nil }

// newTestHandler returns a *TaskHandler with an empty Registry and TaskStore,
// suitable for unit-testing handleSubmit and handleOpenInteractive.
func newTestHandler(t *testing.T) *TaskHandler {
	t.Helper()
	return &TaskHandler{
		Tasks:    NewTaskStore(),
		Registry: NewRegistry(),
		// Every interactive open registers a SessionMux, so a wired Sessions
		// registry is part of the minimal production-shaped handler.
		Sessions: NewSessionRegistry(),
	}
}

// mustHostname is declared in taskstore_test.go (same package).

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

	// Register a runner that covers /repo so handleSubmit succeeds.
	reg.Add(&RunnerEntry{
		ID:           "r-test-1",
		Hostname:     "testhost",
		AllowedRoots: []string{"/"},
		MaxTasks:     1,
		ActiveTasks:  map[string]struct{}{},
		ConnectedAt:  time.Now(),
		LastSeen:     time.Now(),
		Conn:         stubConn{},
	})

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
	// Allocate a fake send stream so handleList can write its body onto it
	// and the test can decode and verify.
	fc.nextSendStreamID = 42

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
		Conn:         stubConn{},
	})

	// Pre-populate TaskStore with 1 Queued task on "/x".
	taskID := tasks.Create("/x", "list-prompt", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, "")

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
	if listResult.StreamId != 42 {
		t.Errorf("expected StreamId=42, got %d", listResult.StreamId)
	}

	// Body is on the recorded send stream, not in the SendMessage payload.
	if len(fc.sendStreams) != 1 {
		t.Fatalf("expected 1 send stream, got %d", len(fc.sendStreams))
	}
	ss := fc.sendStreams[0]
	if !ss.eofSent {
		t.Errorf("expected EOF on list stream")
	}
	var body protocol.ListResultBody
	if err := body.DecodeExact(ss.bytes); err != nil {
		t.Fatalf("decode ListResultBody (%d bytes): %v", len(ss.bytes), err)
	}
	if body.RunnersLen != 1 {
		t.Errorf("expected RunnersLen=1, got %d", body.RunnersLen)
	}
	if body.TasksLen != 1 {
		t.Errorf("expected TasksLen=1, got %d", body.TasksLen)
	}
	if len(body.Runners) > 0 && string(body.Runners[0].Hostname) != "h1" {
		t.Errorf("expected runner Hostname h1, got %q", string(body.Runners[0].Hostname))
	}
	if len(body.Tasks) > 0 && string(body.Tasks[0].Prompt) != "list-prompt" {
		t.Errorf("expected task Prompt 'list-prompt', got %q", string(body.Tasks[0].Prompt))
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

// ---------------------------------------------------------------------------
// Task 4.1: handleSubmit synchronous error codes
// ---------------------------------------------------------------------------

func TestHandleSubmitNoRunnerForRepo(t *testing.T) {
	h := newTestHandler(t) // empty registry

	req := &protocol.SubmitRequest{}
	req.SetRepoPath([]byte("/x/repo"))
	req.SetPrompt([]byte("p"))
	// Selector is zero value == RunnerSelectorKind_Any (no pinning)

	resp := h.handleSubmit(req, protocol.ClientKind_Unspecified, protocol.TaskID{}, protocol.Capability_All)
	if resp.Status != protocol.SubmitStatus_NoRunner {
		t.Fatalf("status=%v want NoRunner", resp.Status)
	}
}

func TestHandleSubmitAmbiguousRunner(t *testing.T) {
	h := newTestHandler(t)
	now := time.Now()
	h.Registry.Add(&RunnerEntry{ID: "A", Hostname: "h1", AllowedRoots: []string{"/shared"}, MaxTasks: 1, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now, Conn: stubConn{}})
	h.Registry.Add(&RunnerEntry{ID: "B", Hostname: "h2", AllowedRoots: []string{"/shared"}, MaxTasks: 1, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now, Conn: stubConn{}})

	req := &protocol.SubmitRequest{}
	req.SetRepoPath([]byte("/shared/repo"))
	req.SetPrompt([]byte("p"))

	resp := h.handleSubmit(req, protocol.ClientKind_Unspecified, protocol.TaskID{}, protocol.Capability_All)
	if resp.Status != protocol.SubmitStatus_AmbiguousRunner {
		t.Fatalf("status=%v want AmbiguousRunner", resp.Status)
	}
	if !bytes.Contains(resp.ErrorMsg, []byte("h1")) || !bytes.Contains(resp.ErrorMsg, []byte("h2")) {
		t.Fatalf("error_msg lacks hostnames: %q", resp.ErrorMsg)
	}
}

func TestHandleSubmitPinnedNotFound(t *testing.T) {
	h := newTestHandler(t)
	now := time.Now()
	h.Registry.Add(&RunnerEntry{ID: "A", Hostname: "gmkhost", AllowedRoots: []string{"/x"}, MaxTasks: 1, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now, Conn: stubConn{}})

	sel := protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_ByHostname}
	sel.SetHostname(mustHostname(t, "raspi")) // hostname not present
	req := &protocol.SubmitRequest{Selector: sel}
	req.SetRepoPath([]byte("/x/repo"))
	req.SetPrompt([]byte("p"))

	resp := h.handleSubmit(req, protocol.ClientKind_Unspecified, protocol.TaskID{}, protocol.Capability_All)
	if resp.Status != protocol.SubmitStatus_PinnedNotFound {
		t.Fatalf("status=%v want PinnedNotFound", resp.Status)
	}
}

func TestHandleSubmitOK(t *testing.T) {
	h := newTestHandler(t)
	now := time.Now()
	h.Registry.Add(&RunnerEntry{ID: "A", Hostname: "runner-a", AllowedRoots: []string{"/x"}, MaxTasks: 1, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now, Conn: stubConn{}})

	req := &protocol.SubmitRequest{}
	req.SetRepoPath([]byte("/x/repo"))
	req.SetPrompt([]byte("p"))

	resp := h.handleSubmit(req, protocol.ClientKind_Unspecified, protocol.TaskID{}, protocol.Capability_All)
	if resp.Status != protocol.SubmitStatus_Ok {
		t.Fatalf("status=%v want Ok", resp.Status)
	}
	zeroID := protocol.TaskID{}
	if resp.TaskId == zeroID {
		t.Fatal("expected non-zero TaskId in SubmitResponse")
	}
	taskIDHex := hex.EncodeToString(resp.TaskId.Id[:])
	got, ok := h.Tasks.Get(taskIDHex)
	if !ok {
		t.Fatalf("task %q not found in store", taskIDHex)
	}
	if got.BoundRunnerID != "A" {
		t.Fatalf("BoundRunnerID=%q want A", got.BoundRunnerID)
	}
	if got.Selector.Kind != protocol.RunnerSelectorKind_Any {
		t.Fatalf("Selector.Kind=%v want Any", got.Selector.Kind)
	}
}

// ---------------------------------------------------------------------------
// Task 4: submit-path agent-profile filter + resolution
// ---------------------------------------------------------------------------

func TestSubmitProfileUnavailable(t *testing.T) {
	h := newTestHandler(t) // one runner advertising ["claude"]
	now := time.Now()
	h.Registry.Add(&RunnerEntry{ID: "A", Hostname: "h1", AllowedRoots: []string{"/"}, MaxTasks: 1, AgentProfiles: []string{"claude"}, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now, Conn: stubConn{}})

	req := &protocol.SubmitRequest{}
	req.SetRepoPath([]byte("/repo"))
	req.SetPrompt([]byte("x"))
	req.SetAgentProfile([]byte("codex"))
	resp := h.handleSubmit(req, protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All)
	if resp.Status != protocol.SubmitStatus_ProfileUnavailable {
		t.Fatalf("got %v", resp.Status)
	}
	if !bytes.Contains(resp.ErrorMsg, []byte("claude")) {
		t.Fatalf("error_msg lacks advertised profile name: %q", resp.ErrorMsg)
	}
}

func TestSubmitEmptyProfileUsesDefault(t *testing.T) {
	h := newTestHandler(t) // runner advertising ["claude","codex"]
	now := time.Now()
	h.Registry.Add(&RunnerEntry{ID: "A", Hostname: "h1", AllowedRoots: []string{"/"}, MaxTasks: 1, AgentProfiles: []string{"claude", "codex"}, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now, Conn: stubConn{}})

	req := &protocol.SubmitRequest{}
	req.SetRepoPath([]byte("/repo"))
	req.SetPrompt([]byte("x")) // no agent_profile
	resp := h.handleSubmit(req, protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All)
	if resp.Status != protocol.SubmitStatus_Ok {
		t.Fatalf("got %v", resp.Status)
	}
	taskIDHex := hex.EncodeToString(resp.TaskId.Id[:])
	got, ok := h.Tasks.Get(taskIDHex)
	if !ok {
		t.Fatalf("task %q not found in store", taskIDHex)
	}
	if got.AgentProfile != "claude" {
		t.Fatalf("AgentProfile=%q want %q (default/first)", got.AgentProfile, "claude")
	}
}

func TestSubmitProfileFilterNarrowsAmbiguity(t *testing.T) {
	h := newTestHandler(t)
	now := time.Now()
	// Two runners serve the same repo; only B advertises "codex". Without the
	// profile filter this would be AmbiguousRunner.
	h.Registry.Add(&RunnerEntry{ID: "A", Hostname: "h1", AllowedRoots: []string{"/shared"}, MaxTasks: 1, AgentProfiles: []string{"claude"}, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now, Conn: stubConn{}})
	h.Registry.Add(&RunnerEntry{ID: "B", Hostname: "h2", AllowedRoots: []string{"/shared"}, MaxTasks: 1, AgentProfiles: []string{"codex"}, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now, Conn: stubConn{}})

	req := &protocol.SubmitRequest{}
	req.SetRepoPath([]byte("/shared/repo"))
	req.SetPrompt([]byte("p"))
	req.SetAgentProfile([]byte("codex"))
	resp := h.handleSubmit(req, protocol.ClientKind_Unspecified, protocol.TaskID{}, protocol.Capability_All)
	if resp.Status != protocol.SubmitStatus_Ok {
		t.Fatalf("status=%v want Ok", resp.Status)
	}
	taskIDHex := hex.EncodeToString(resp.TaskId.Id[:])
	got, ok := h.Tasks.Get(taskIDHex)
	if !ok {
		t.Fatalf("task not found")
	}
	if got.BoundRunnerID != "B" {
		t.Fatalf("BoundRunnerID=%q want B (only codex-advertising runner)", got.BoundRunnerID)
	}
	if got.AgentProfile != "codex" {
		t.Fatalf("AgentProfile=%q want codex", got.AgentProfile)
	}
}

func TestSubmitLegacyRunnerNoProfilesFallsBackToAgentBin(t *testing.T) {
	h := newTestHandler(t)
	now := time.Now()
	// Legacy runner: no AgentProfiles advertised at all, only AgentBin.
	h.Registry.Add(&RunnerEntry{ID: "A", Hostname: "h1", AllowedRoots: []string{"/"}, MaxTasks: 1, AgentBin: "claude", ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now, Conn: stubConn{}})

	req := &protocol.SubmitRequest{}
	req.SetRepoPath([]byte("/repo"))
	req.SetPrompt([]byte("x")) // no agent_profile
	resp := h.handleSubmit(req, protocol.ClientKind_Unspecified, protocol.TaskID{}, protocol.Capability_All)
	if resp.Status != protocol.SubmitStatus_Ok {
		t.Fatalf("got %v", resp.Status)
	}
	taskIDHex := hex.EncodeToString(resp.TaskId.Id[:])
	got, ok := h.Tasks.Get(taskIDHex)
	if !ok {
		t.Fatalf("task not found")
	}
	if got.AgentProfile != "claude" {
		t.Fatalf("AgentProfile=%q want %q (AgentBin fallback)", got.AgentProfile, "claude")
	}
}

func TestSubmitResumeProfileUnavailableWhenRunnerLostProfile(t *testing.T) {
	h := newTestHandler(t)
	now := time.Now()

	// Original task bound to profile "codex" on runner "A" (test-only shortcut
	// via TaskStore.Create; equivalent to what handleSubmit would have stored).
	taskIDHex := h.Tasks.Create("/repo", "orig", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, protocol.TaskID{}, "A", protocol.RunnerSelector{}, nil, protocol.Capability_All, "codex")
	h.Tasks.Assign(taskIDHex, "A", "/wt")
	h.Tasks.Finish(taskIDHex, 0, nil)

	// Runner "A" is (re)registered but now only advertises "claude" — the
	// profile the task needs is no longer available on it.
	h.Registry.Add(&RunnerEntry{ID: "A", Hostname: "h1", AllowedRoots: []string{"/"}, MaxTasks: 1, AgentProfiles: []string{"claude"}, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now, Conn: stubConn{}})

	var tid protocol.TaskID
	raw, _ := hex.DecodeString(taskIDHex)
	copy(tid.Id[:], raw)

	req := &protocol.SubmitRequest{ResumeTaskId: tid}
	req.SetPrompt([]byte("resume")) // no agent_profile -> falls back to "codex"

	resp := h.handleSubmit(req, protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All)
	if resp.Status != protocol.SubmitStatus_ProfileUnavailable {
		t.Fatalf("status=%v want ProfileUnavailable", resp.Status)
	}
}

func TestSubmitResumeEmptyProfileReusesOriginal(t *testing.T) {
	h := newTestHandler(t)
	now := time.Now()

	taskIDHex := h.Tasks.Create("/repo", "orig", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, protocol.TaskID{}, "A", protocol.RunnerSelector{}, nil, protocol.Capability_All, "codex")
	h.Tasks.Assign(taskIDHex, "A", "/wt")
	h.Tasks.Finish(taskIDHex, 0, nil)

	// Runner still advertises "codex" — resume should succeed and stay bound
	// to the original profile without the caller repeating it.
	h.Registry.Add(&RunnerEntry{ID: "A", Hostname: "h1", AllowedRoots: []string{"/"}, MaxTasks: 1, AgentProfiles: []string{"claude", "codex"}, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now, Conn: stubConn{}})

	var tid protocol.TaskID
	raw, _ := hex.DecodeString(taskIDHex)
	copy(tid.Id[:], raw)

	req := &protocol.SubmitRequest{ResumeTaskId: tid}
	req.SetPrompt([]byte("resume"))

	resp := h.handleSubmit(req, protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All)
	if resp.Status != protocol.SubmitStatus_Ok {
		t.Fatalf("status=%v want Ok", resp.Status)
	}
	got, ok := h.Tasks.Get(taskIDHex)
	if !ok {
		t.Fatalf("task not found")
	}
	if got.AgentProfile != "codex" {
		t.Fatalf("AgentProfile=%q want codex (unchanged across resume)", got.AgentProfile)
	}
}

// ---------------------------------------------------------------------------
// Task 4.2: handleOpenInteractive synchronous error codes
// ---------------------------------------------------------------------------

func TestHandleOpenInteractiveNoRunnerForRepo(t *testing.T) {
	h := newTestHandler(t) // empty registry

	req := &protocol.OpenInteractiveRequest{}
	req.SetRepoPath([]byte("/x/repo"))

	resp := h.handleOpenInteractive(nil, req, protocol.ClientKind_Unspecified, protocol.TaskID{}, protocol.Capability_All)
	if resp.Status != protocol.OpenInteractiveStatus_NoRunnerForRepo {
		t.Fatalf("status=%v want NoRunnerForRepo", resp.Status)
	}
}

func TestHandleOpenInteractiveBusy(t *testing.T) {
	h := newTestHandler(t)
	now := time.Now()
	// Runner is at capacity (MaxTasks=1, 1 active task).
	h.Registry.Add(&RunnerEntry{ID: "A", Hostname: "h", AllowedRoots: []string{"/x"}, MaxTasks: 1,
		ActiveTasks: map[string]struct{}{"existing": {}}, ConnectedAt: now, LastSeen: now, Conn: stubConn{}})

	req := &protocol.OpenInteractiveRequest{}
	req.SetRepoPath([]byte("/x/repo"))

	resp := h.handleOpenInteractive(nil, req, protocol.ClientKind_Unspecified, protocol.TaskID{}, protocol.Capability_All)
	if resp.Status != protocol.OpenInteractiveStatus_RunnerBusy {
		t.Fatalf("status=%v want RunnerBusy", resp.Status)
	}
}

func TestHandleOpenInteractiveAmbiguous(t *testing.T) {
	h := newTestHandler(t)
	now := time.Now()
	h.Registry.Add(&RunnerEntry{ID: "ws:10.0.0.1:1-1", Hostname: "h1", AllowedRoots: []string{"/shared"}, MaxTasks: 8, ActiveTasks: map[string]struct{}{"t": {}}, ConnectedAt: now, LastSeen: now, Conn: stubConn{}})
	h.Registry.Add(&RunnerEntry{ID: "ws:10.0.0.2:1-1", Hostname: "h2", AllowedRoots: []string{"/shared"}, MaxTasks: 8, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now, Conn: stubConn{}})

	req := &protocol.OpenInteractiveRequest{}
	req.SetRepoPath([]byte("/shared/repo"))

	resp := h.handleOpenInteractive(nil, req, protocol.ClientKind_Unspecified, protocol.TaskID{}, protocol.Capability_All)
	if resp.Status != protocol.OpenInteractiveStatus_AmbiguousRunner {
		t.Fatalf("status=%v want AmbiguousRunner", resp.Status)
	}
	if len(*resp.Candidates()) != 2 {
		t.Fatalf("candidates=%d want 2", len(*resp.Candidates()))
	}
	byCid := map[string]protocol.RunnerCandidate{}
	for _, c := range *resp.Candidates() {
		byCid[string(c.Cid)] = c
	}
	a, ok := byCid["ws:10.0.0.1:1-1"]
	if !ok {
		t.Fatalf("missing candidate h1; got %v", byCid)
	}
	if string(a.Hostname) != "h1" || string(a.MatchedRoot) != "/shared" || a.ActiveTasks != 1 || a.MaxTasks != 8 {
		t.Fatalf("h1 candidate mismatch: %+v", a)
	}
}

func TestHandleOpenInteractivePinnedNotFound(t *testing.T) {
	h := newTestHandler(t)
	now := time.Now()
	h.Registry.Add(&RunnerEntry{ID: "A", Hostname: "gmkhost", AllowedRoots: []string{"/x"}, MaxTasks: 1, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now, Conn: stubConn{}})

	sel := protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_ByHostname}
	sel.SetHostname(mustHostname(t, "raspi")) // hostname not present
	req := &protocol.OpenInteractiveRequest{Selector: sel}
	req.SetRepoPath([]byte("/x/repo"))

	resp := h.handleOpenInteractive(nil, req, protocol.ClientKind_Unspecified, protocol.TaskID{}, protocol.Capability_All)
	if resp.Status != protocol.OpenInteractiveStatus_PinnedNotFound {
		t.Fatalf("status=%v want PinnedNotFound", resp.Status)
	}
}

// TestHandleOpenInteractiveOkSetsRepoPathOnOpenExec verifies the Ok path of
// handleOpenInteractive emits a RunnerControl/OpenExec message that carries
// the cleaned RepoPath. Regression guard: the original implementation only
// set TaskId/StreamId on the OpenExecRunnerRequest, leaving RepoPath empty,
// which made the runner's AllowedRoots gate reject every interactive task
// with TaskFinished exit=-1.
func TestHandleOpenInteractiveOkSetsRepoPathOnOpenExec(t *testing.T) {
	h := newTestHandler(t)
	now := time.Now()

	runnerConn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9100-1"), nextStreamID: 42}
	h.Registry.Add(&RunnerEntry{
		ID: "A", Hostname: "h", AllowedRoots: []string{"/shared"}, MaxTasks: 1,
		ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now,
		Conn: runnerConn,
	})

	tuiConn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9101-2"), nextStreamID: 7}

	req := &protocol.OpenInteractiveRequest{}
	req.SetRepoPath([]byte("/shared/repo"))

	resp := h.handleOpenInteractive(tuiConn, req, protocol.ClientKind_Unspecified, protocol.TaskID{}, protocol.Capability_All)
	if resp.Status != protocol.OpenInteractiveStatus_Ok {
		t.Fatalf("status=%v want Ok", resp.Status)
	}

	// Find the OpenExec message captured on the runner conn. The handler also
	// sends nothing else on this conn during Ok, so sent[0] is the target.
	if len(runnerConn.sent) != 1 {
		t.Fatalf("runner conn: want 1 sent message, got %d", len(runnerConn.sent))
	}
	raw := runnerConn.sent[0]
	if raw[0] != byte(appwire.AppKind_RunnerControl) {
		t.Fatalf("first byte=%d, want RunnerControl kind", raw[0])
	}

	rr := &protocol.RunnerRequest{}
	if _, err := rr.Decode(raw[1:]); err != nil {
		t.Fatalf("decode RunnerRequest: %v", err)
	}
	if rr.Kind != protocol.RunnerRequestType_OpenExec {
		t.Fatalf("kind=%v want OpenExec", rr.Kind)
	}
	oer := rr.OpenExec()
	if oer == nil {
		t.Fatal("OpenExec variant nil")
	}
	if got, want := string(oer.RepoPath), "/shared/repo"; got != want {
		t.Fatalf("OpenExec.RepoPath=%q want %q", got, want)
	}
	if oer.StreamId != 42 {
		t.Fatalf("OpenExec.StreamId=%d want 42 (runner-side stream)", oer.StreamId)
	}
}

// ---------------------------------------------------------------------------
// Task 6: handleOpenInteractive session wiring
// ---------------------------------------------------------------------------

// TestHandleOpenInteractive_RegistersSessionMux verifies that every open
// creates and registers a SessionMux (all interactive sessions are
// detachable) and returns Ok with the TUI stream ID.
func TestHandleOpenInteractive_RegistersSessionMux(t *testing.T) {
	h := newTestHandler(t)
	h.Sessions = NewSessionRegistry()
	now := time.Now()

	runnerConn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9200-1"), nextStreamID: 55}
	h.Registry.Add(&RunnerEntry{
		ID: "A", Hostname: "h", AllowedRoots: []string{"/repo"}, MaxTasks: 1,
		ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now,
		Conn: runnerConn,
	})

	tuiConn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9201-2"), nextStreamID: 11}

	req := &protocol.OpenInteractiveRequest{}
	req.SetRepoPath([]byte("/repo"))

	resp := h.handleOpenInteractive(tuiConn, req, protocol.ClientKind_Unspecified, protocol.TaskID{}, protocol.Capability_All)
	if resp.Status != protocol.OpenInteractiveStatus_Ok {
		t.Fatalf("status=%v want Ok", resp.Status)
	}

	// TUI stream ID should match the tuiConn's stream.
	if resp.StreamId != 11 {
		t.Fatalf("StreamId=%d want 11 (tui-side stream)", resp.StreamId)
	}

	// Task ID must be non-zero.
	taskIDHex := hex.EncodeToString(resp.TaskId.Id[:])
	if taskIDHex == "00000000000000000000000000000000" {
		t.Fatal("expected non-zero TaskId")
	}

	// SessionMux must be registered in Sessions.
	mux := h.Sessions.Get(taskIDHex)
	if mux == nil {
		t.Fatalf("Sessions.Get(%q) = nil, want non-nil SessionMux", taskIDHex)
	}

	entry, ok := h.Tasks.Get(taskIDHex)
	if !ok {
		t.Fatalf("task %q not found in store", taskIDHex)
	}

	// Task status should be Running (Assign was called).
	if entry.Status != protocol.TaskStatus_Running {
		t.Errorf("task.Status=%v want Running", entry.Status)
	}

	// OpenExec must have reached the runner.
	if len(runnerConn.sent) != 1 {
		t.Fatalf("runner conn: want 1 sent message, got %d", len(runnerConn.sent))
	}
	raw := runnerConn.sent[0]
	rr := &protocol.RunnerRequest{}
	if _, err := rr.Decode(raw[1:]); err != nil {
		t.Fatalf("decode RunnerRequest: %v", err)
	}
	if rr.OpenExec() == nil {
		t.Fatal("OpenExec variant nil")
	}
}

// ---------------------------------------------------------------------------
// Task 7: handleAttachSession error paths and Ok path
// ---------------------------------------------------------------------------

// makeSessionTask is a helper that injects an interactive task entry directly
// into the TaskStore with the given status. Returns the hex task ID.
func makeSessionTask(t *testing.T, tasks *TaskStore, status protocol.TaskStatus) string {
	t.Helper()
	var rawID [16]byte
	rawID[0] = 0xDE
	rawID[1] = 0x7A
	rawID[14] = byte(status)
	rawID[15] = 0xCC
	id := hex.EncodeToString(rawID[:])
	tasks.mu.Lock()
	tasks.tasks[id] = &TaskEntry{
		ID:       id,
		RepoPath: "/repo",
		Status:   status,
		Kind:     protocol.TaskKind_Interactive,
	}
	tasks.order = append(tasks.order, id)
	tasks.mu.Unlock()
	return id
}

// taskIDFromHexStr converts a hex string to protocol.TaskID for test requests.
func taskIDFromHexStr(t *testing.T, idHex string) protocol.TaskID {
	t.Helper()
	var tid protocol.TaskID
	raw, err := hex.DecodeString(idHex)
	if err != nil {
		t.Fatalf("hex.DecodeString(%q): %v", idHex, err)
	}
	copy(tid.Id[:], raw)
	return tid
}

// TestHandleAttachSession_NotFound: unknown task ID → NotFound.
func TestHandleAttachSession_NotFound(t *testing.T) {
	h := newTestHandler(t)
	h.Sessions = NewSessionRegistry()

	var rawID [16]byte
	rawID[0] = 0xFF
	req := &protocol.AttachSessionRequest{TaskId: protocol.TaskID{Id: rawID}}

	resp := h.handleAttachSession(&fakeConn{}, req)
	if resp.Status != protocol.AttachSessionStatus_NotFound {
		t.Fatalf("status=%v want NotFound", resp.Status)
	}
}

// TestHandleAttachSession_NotInteractive: oneshot task → NotInteractive.
func TestHandleAttachSession_NotInteractive(t *testing.T) {
	h := newTestHandler(t)
	h.Sessions = NewSessionRegistry()

	var rawID [16]byte
	rawID[0] = 0xA1
	id := hex.EncodeToString(rawID[:])
	h.Tasks.mu.Lock()
	h.Tasks.tasks[id] = &TaskEntry{
		ID:       id,
		RepoPath: "/repo",
		Status:   protocol.TaskStatus_Running,
		Kind:     protocol.TaskKind_Oneshot,
	}
	h.Tasks.order = append(h.Tasks.order, id)
	h.Tasks.mu.Unlock()

	req := &protocol.AttachSessionRequest{TaskId: taskIDFromHexStr(t, id)}
	resp := h.handleAttachSession(&fakeConn{}, req)
	if resp.Status != protocol.AttachSessionStatus_NotInteractive {
		t.Fatalf("status=%v want NotInteractive", resp.Status)
	}
}

// TestHandleAttachSession_AlreadyTerminal: Cancelled task → AlreadyTerminal.
func TestHandleAttachSession_AlreadyTerminal(t *testing.T) {
	h := newTestHandler(t)
	h.Sessions = NewSessionRegistry()

	id := makeSessionTask(t, h.Tasks, protocol.TaskStatus_Cancelled)

	req := &protocol.AttachSessionRequest{TaskId: taskIDFromHexStr(t, id)}
	resp := h.handleAttachSession(&fakeConn{}, req)
	if resp.Status != protocol.AttachSessionStatus_AlreadyTerminal {
		t.Fatalf("status=%v want AlreadyTerminal", resp.Status)
	}
}

// TestHandleAttachSession_RunnerUnreachable: detachable session but no SessionMux → RunnerUnreachable.
func TestHandleAttachSession_RunnerUnreachable(t *testing.T) {
	h := newTestHandler(t)
	h.Sessions = NewSessionRegistry()

	id := makeSessionTask(t, h.Tasks, protocol.TaskStatus_Running)
	// Do NOT register a SessionMux for this task.

	req := &protocol.AttachSessionRequest{TaskId: taskIDFromHexStr(t, id)}
	resp := h.handleAttachSession(&fakeConn{}, req)
	if resp.Status != protocol.AttachSessionStatus_RunnerUnreachable {
		t.Fatalf("status=%v want RunnerUnreachable", resp.Status)
	}
}

// TestHandleAttachSession_Ok_FromDetached: detachable, Detached state, SessionMux present → Ok.
// The task is created as Running then transitioned to Detached via SetDetached,
// which matches the canonical reattach-from-detached scenario (client disconnect
// fires onDetach → Tasks.SetDetached).
func TestHandleAttachSession_Ok_FromDetached(t *testing.T) {
	h := newTestHandler(t)
	h.Sessions = NewSessionRegistry()

	id := makeSessionTask(t, h.Tasks, protocol.TaskStatus_Running)
	if err := h.Tasks.SetDetached(id); err != nil {
		t.Fatalf("SetDetached: %v", err)
	}

	// Build a minimal SessionMux with a fakeBidiStream as the runner side so
	// that runnerPump blocks (noopBidiStream returns EOF immediately, which
	// would race with Attach and report "session_mux: stopped").
	runnerStream := newFakeStream(t)
	ring := NewRingBuffer(4096)
	// Pre-populate the ring with some data so replay_bytes is non-zero.
	ring.Append([]byte("hello from runner"))
	mux := NewSessionMux(context.Background(), id, runnerStream, ring, SessionHooks{})
	h.Sessions.Add(id, mux)
	defer func() {
		runnerStream.CloseRead()
		mux.Stop()
	}()

	tuiConn := &fakeConn{
		id:           objproto.MustParseConnectionID("ws:127.0.0.1:9400-1"),
		nextStreamID: trsf.StreamID(33),
	}

	req := &protocol.AttachSessionRequest{TaskId: taskIDFromHexStr(t, id)}
	resp := h.handleAttachSession(tuiConn, req)
	if resp.Status != protocol.AttachSessionStatus_Ok {
		t.Fatalf("status=%v want Ok", resp.Status)
	}

	// StreamId must match the stream allocated by tuiConn.
	if resp.StreamId != 33 {
		t.Fatalf("StreamId=%d want 33", resp.StreamId)
	}

	// ReplayBytes must reflect the ring content written before Attach.
	if resp.ReplayBytes == 0 {
		t.Errorf("ReplayBytes=0, want >0 (ring had data before Attach)")
	}

	// tuiConn should have had its stream consumed (nextStreamID reset to 0).
	if tuiConn.nextStreamID != 0 {
		t.Errorf("nextStreamID=%d want 0 (stream was allocated)", tuiConn.nextStreamID)
	}
}

// View mode must succeed without taking the writer slot and without flipping
// the task to Running (it must register a viewer instead).
func TestHandleAttachSession_ViewMode_NoWriterTakeover(t *testing.T) {
	h := newTestHandler(t)
	h.Sessions = NewSessionRegistry()

	id := makeSessionTask(t, h.Tasks, protocol.TaskStatus_Running)
	if err := h.Tasks.SetDetached(id); err != nil {
		t.Fatalf("SetDetached: %v", err)
	}

	runnerStream := newFakeStream(t)
	ring := NewRingBuffer(4096)
	ring.Append([]byte("hello from runner"))
	mux := NewSessionMux(context.Background(), id, runnerStream, ring, SessionHooks{})
	h.Sessions.Add(id, mux)
	defer func() {
		runnerStream.CloseRead()
		mux.Stop()
	}()

	tuiConn := &fakeConn{
		id:           objproto.MustParseConnectionID("ws:127.0.0.1:9400-1"),
		nextStreamID: trsf.StreamID(33),
	}

	req := &protocol.AttachSessionRequest{TaskId: taskIDFromHexStr(t, id), Mode: protocol.AttachMode_View}
	resp := h.handleAttachSession(tuiConn, req)
	if resp.Status != protocol.AttachSessionStatus_Ok {
		t.Fatalf("status=%v want Ok", resp.Status)
	}
	if mux.IsAttached() {
		t.Fatal("view attach must NOT occupy the writer slot")
	}
	waitFor(t, func() bool { return mux.ViewerCount() == 1 })
}

func TestToTaskInfoCapabilities(t *testing.T) {
	want := protocol.Capability_Spawn | protocol.Capability_FileRead
	entry := TaskEntry{
		ID:           "0000000000000000000000000000000000000000000000000000000000000001",
		Status:       protocol.TaskStatus_Queued,
		Capabilities: want,
		CreatedAt:    time.Now(),
	}
	info := toTaskInfo(entry)
	if info.Capabilities != want {
		t.Fatalf("toTaskInfo Capabilities = %#x, want %#x", info.Capabilities, want)
	}
}
