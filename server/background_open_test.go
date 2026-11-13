package server

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/trsf"
)

// bgOpen builds a detached background open on /r with the agent and a 1 h
// deadline; callers strip what a case needs missing.
func bgOpen(agent string) *protocol.OpenInteractiveRequest {
	oi := detachedOpen(40, 120)
	oi.SetRunModeSet(true)
	oi.SetBackground(true)
	oi.SetAgentProfile([]byte(agent))
	oi.SetConditionsPresent(true)
	oi.SetConditions(protocol.BackgroundConditions{DeadlineMs: 3600000})
	return oi
}

func openBG(h *TaskHandler, oi *protocol.OpenInteractiveRequest, prompt string) protocol.OpenInteractiveResponse {
	resp, _ := h.handleOpenInteractive("", nil, oi, protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All, prompt)
	return resp
}

func taskOf(t *testing.T, h *TaskHandler, resp protocol.OpenInteractiveResponse) TaskEntry {
	t.Helper()
	e, ok := h.Tasks.Get(hex.EncodeToString(resp.TaskId.Id[:]))
	if !ok {
		t.Fatalf("no task for response %+v", resp)
	}
	return e
}

func TestBackgroundOpenWithNoRunnerIsQueued(t *testing.T) {
	h := newTestHandler(t)
	resp := openBG(h, bgOpen("claude"), "triage")
	if resp.Status != protocol.OpenInteractiveStatus_Ok || !resp.Queued() || resp.Attached() {
		t.Fatalf("status %v queued %v attached %v", resp.Status, resp.Queued(), resp.Attached())
	}
	e := taskOf(t, h, resp)
	if e.Status != protocol.TaskStatus_Queued || e.RunMode != protocol.RunMode_Background || e.Prompt != "triage" ||
		e.Cond.Deadline != time.Hour || e.Cond.Quiet != 5*time.Minute || e.Cond.Rows != 40 || e.Cond.Cols != 120 || e.AgentProfile != "claude" {
		t.Fatalf("entry %+v", e)
	}
}

func TestBackgroundOpenOnFullRunnerIsQueued(t *testing.T) {
	h := newTestHandler(t)
	now := time.Now()
	h.Registry.Add(&RunnerEntry{ID: tcid("R"), Identity: testRunnerID("R"), Hostname: "h", AllowedRoots: []string{"/r"},
		AgentProfiles: []string{"claude"}, MaxTasks: 1, ActiveTasks: map[string]struct{}{"busy": {}},
		ConnectedAt: now, LastTaskActivity: now, Conn: &fakeConn{}})
	resp := openBG(h, bgOpen("claude"), "triage")
	if resp.Status != protocol.OpenInteractiveStatus_Ok || !resp.Queued() || taskOf(t, h, resp).Status != protocol.TaskStatus_Queued {
		t.Fatalf("status %v queued %v", resp.Status, resp.Queued())
	}
}

func TestBackgroundCreateRequiresProfileDeadlinePrompt(t *testing.T) {
	h := newTestHandler(t)
	if r := openBG(h, bgOpen(""), "p"); r.Status != protocol.OpenInteractiveStatus_ProfileRequired {
		t.Errorf("no profile: %v", r.Status)
	}
	noDeadline := bgOpen("claude")
	noDeadline.SetConditions(protocol.BackgroundConditions{})
	if r := openBG(h, noDeadline, "p"); r.Status != protocol.OpenInteractiveStatus_DeadlineRequired {
		t.Errorf("no deadline: %v", r.Status)
	}
	if r := openBG(h, bgOpen("claude"), ""); r.Status != protocol.OpenInteractiveStatus_PromptRequired {
		t.Errorf("no prompt: %v", r.Status)
	}
	if n := len(h.Tasks.List(0)); n != 0 {
		t.Errorf("a refused open left %d tasks", n)
	}
}

// Review M3: background never takes a seat; an attached background request is
// refused rather than queued with no size and no client stream.
func TestBackgroundOpenMustBeDetached(t *testing.T) {
	h := newTestHandler(t)
	oi := &protocol.OpenInteractiveRequest{}
	oi.SetRepoPath([]byte("/r"))
	oi.SetRunModeSet(true)
	oi.SetBackground(true)
	oi.SetAgentProfile([]byte("claude"))
	oi.SetConditionsPresent(true)
	oi.SetConditions(protocol.BackgroundConditions{DeadlineMs: 1000})
	if r := openBG(h, oi, "p"); r.Status != protocol.OpenInteractiveStatus_DetachRequired {
		t.Fatalf("status %v, want detach_required", r.Status)
	}
}

// endedBG creates a background task and cancels it, ready to resume.
func endedBG(t *testing.T, h *TaskHandler, kind protocol.TaskKind) string {
	t.Helper()
	id, err := h.Tasks.CreateWithinLimits("/r", "first", kind, protocol.ClientKind_Cli, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, authority.Grant{}, "codex",
		RunSpec{Mode: protocol.RunMode_Background, Cond: RunConditions{Deadline: time.Hour, Quiet: time.Minute}})
	if err != nil {
		t.Fatal(err)
	}
	h.Tasks.Cancel(id, protocol.EndReason_Cancelled, protocol.TaskID{})
	return id
}

func resumeReq(id string, oi *protocol.OpenInteractiveRequest) *protocol.OpenInteractiveRequest {
	raw, _ := hex.DecodeString(id)
	copy(oi.ResumeTaskId.Id[:], raw)
	oi.SetRepoPath(nil)
	return oi
}

func TestBackgroundResumeTakesRecordedProfile(t *testing.T) {
	h := newTestHandler(t)
	id := endedBG(t, h, protocol.TaskKind_Interactive)
	r := openBG(h, resumeReq(id, bgOpen("")), "second")
	if r.Status != protocol.OpenInteractiveStatus_Ok || !r.Queued() {
		t.Fatalf("status %v", r.Status)
	}
	e, _ := h.Tasks.Get(id)
	if e.AgentProfile != "codex" || e.Prompt != "second" || e.Cond.Quiet != 5*time.Minute || e.Status != protocol.TaskStatus_Queued {
		t.Fatalf("resumed %+v", e)
	}
}

func TestBackgroundResumeRequiresDeadlineAndPrompt(t *testing.T) {
	h := newTestHandler(t)
	id := endedBG(t, h, protocol.TaskKind_Interactive)
	// No run mode named: the task's background applies, and so do its rules.
	plain := resumeReq(id, detachedOpen(0, 0))
	if r := openBG(h, plain, "p"); r.Status != protocol.OpenInteractiveStatus_DeadlineRequired {
		t.Errorf("no deadline: %v", r.Status)
	}
	if r := openBG(h, resumeReq(id, bgOpen("")), ""); r.Status != protocol.OpenInteractiveStatus_PromptRequired {
		t.Errorf("no prompt: %v", r.Status)
	}
}

func TestBackgroundStreamRefusesQuietTurnEnd(t *testing.T) {
	h := newTestHandler(t)
	for _, te := range []protocol.TurnEndSignal{protocol.TurnEndSignal_Quiet, protocol.TurnEndSignal_Program} {
		oi := bgOpen("claude")
		oi.SetEventStream(true)
		oi.SetConditions(protocol.BackgroundConditions{DeadlineMs: 1000, TurnEnd: te})
		if r := openBG(h, oi, "p"); r.Status != protocol.OpenInteractiveStatus_TurnEndUnsupported {
			t.Errorf("%v: %v", te, r.Status)
		}
	}
	oi := bgOpen("claude")
	oi.SetEventStream(true)
	r := openBG(h, oi, "p")
	if r.Status != protocol.OpenInteractiveStatus_Ok || taskOf(t, h, r).Cond.TurnEnd != protocol.TurnEndSignal_Lifecycle {
		t.Fatalf("stream default: %v %+v", r.Status, taskOf(t, h, r).Cond)
	}
}

func TestResumeWithoutRunModeKeepsBackground(t *testing.T) {
	h := newTestHandler(t)
	id := endedBG(t, h, protocol.TaskKind_Interactive)
	oi := resumeReq(id, bgOpen(""))
	oi.SetRunModeSet(false)
	oi.SetBackground(false)
	if r := openBG(h, oi, "p"); r.Status != protocol.OpenInteractiveStatus_Ok || !r.Queued() {
		t.Fatalf("status %v queued %v", r.Status, r.Queued())
	}
	if e, _ := h.Tasks.Get(id); e.RunMode != protocol.RunMode_Background {
		t.Fatalf("mode %v", e.RunMode)
	}
}

func TestResumeNamingForegroundSwitchesIt(t *testing.T) {
	h := newTestHandler(t)
	openRunner(t, h)
	id := endedBG(t, h, protocol.TaskKind_Interactive)
	oi := resumeReq(id, detachedOpen(0, 0))
	oi.SetRunModeSet(true)
	oi.SetBackground(false)
	oi.SetAgentProfile([]byte(""))
	r := openBG(h, oi, "")
	if r.Status != protocol.OpenInteractiveStatus_Ok || r.Queued() {
		t.Fatalf("status %v queued %v", r.Status, r.Queued())
	}
	if e, _ := h.Tasks.Get(id); e.RunMode != protocol.RunMode_Foreground || e.Status != protocol.TaskStatus_Detached {
		t.Fatalf("mode %v status %v", e.RunMode, e.Status)
	}
}

func TestSubmitResumeOfBackgroundIsModeMismatch(t *testing.T) {
	h := newTestHandler(t)
	id := endedBG(t, h, protocol.TaskKind_Interactive)
	sub := protocol.SubmitRequest{}
	raw, _ := hex.DecodeString(id)
	copy(sub.ResumeTaskId.Id[:], raw)
	sub.SetPrompt([]byte("x"))
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_Submit, RequestId: 3}}
	req.SetSubmit(sub)
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9150-1")}
	h.Handle(conn, encodeTaskControlRequest(t, req))
	resp := lastTaskControlResponse(t, conn)
	if s := resp.Submit(); s == nil || s.Status != protocol.SubmitStatus_ModeMismatch {
		t.Fatalf("submit resume of a background task = %+v", s)
	}
}

func TestForegroundOpenPassesPromptToRunner(t *testing.T) {
	h := newTestHandler(t)
	st := newFakeStream(t)
	rc := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9151-1"), nextBidi: st, autoSendStreams: true}
	now := time.Now()
	h.Registry.Add(&RunnerEntry{ID: tcid("R"), Identity: testRunnerID("R"), Hostname: "h", AllowedRoots: []string{"/r"},
		MaxTasks: 4, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastTaskActivity: now, Conn: rc})
	r := openBG(h, detachedOpen(0, 0), "hello")
	if r.Status != protocol.OpenInteractiveStatus_Ok || r.Queued() {
		t.Fatalf("status %v queued %v", r.Status, r.Queued())
	}
	if e := taskOf(t, h, r); e.Prompt != "hello" || e.RunMode != protocol.RunMode_Foreground {
		t.Fatalf("entry prompt %q mode %v", e.Prompt, e.RunMode)
	}
	var body protocol.PromptBody
	if len(rc.sendStreams) != 1 || body.DecodeExact(rc.sendStreams[0].bytes) != nil || string(body.Prompt) != "hello" {
		t.Fatalf("prompt stream: %d streams, body %q", len(rc.sendStreams), body.Prompt)
	}
}

// promptConn is a fakeConn whose one client-initiated receive stream carries a
// prompt body.
type promptConn struct {
	*fakeConn
	st *fakeRecv
}

func (c promptConn) GetReceiveStream(trsf.StreamID) trsf.ReceiveStream { return c.st }

// Review Focus 2: through the dispatch, a prompt over the cap is refused at
// open time with the cap named, and its stream is cancelled.
func TestPromptOverCapRefused(t *testing.T) {
	h := newTestHandler(t)
	body := protocol.PromptBody{}
	body.SetPrompt(bytes.Repeat([]byte("a"), protocol.PromptMax+1))
	rs := &fakeRecv{data: body.MustAppend(nil)}
	conn := promptConn{fakeConn: &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9152-1")}, st: rs}
	oi := bgOpen("claude")
	oi.SetPromptPresent(true)
	oi.SetPromptStreamId(5)
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_OpenInteractive, RequestId: 4}}
	req.SetOpenInteractive(*oi)
	h.Handle(conn, encodeTaskControlRequest(t, req))
	deadline := time.Now().Add(2 * time.Second)
	for len(conn.Sent()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	last := lastTaskControlResponse(t, conn.fakeConn)
	o := last.OpenInteractive()
	if o == nil || o.Status != protocol.OpenInteractiveStatus_PromptTooLarge || !strings.Contains(string(o.ErrorMsg), "16384") || !rs.cancelled {
		t.Fatalf("response %+v cancelled %v", o, rs.cancelled)
	}
	if n := len(h.Tasks.List(0)); n != 0 {
		t.Fatalf("a refused open left %d tasks", n)
	}
}

// A prompt within the cap travels through the dispatch into the task.
func TestPromptThroughDispatchReachesTheTask(t *testing.T) {
	h := newTestHandler(t)
	body := protocol.PromptBody{}
	body.SetPrompt([]byte("triage new issues"))
	conn := promptConn{fakeConn: &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9153-1")}, st: &fakeRecv{data: body.MustAppend(nil)}}
	oi := bgOpen("claude")
	oi.SetPromptPresent(true)
	oi.SetPromptStreamId(5)
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_OpenInteractive, RequestId: 5}}
	req.SetOpenInteractive(*oi)
	h.Handle(conn, encodeTaskControlRequest(t, req))
	deadline := time.Now().Add(2 * time.Second)
	for len(conn.Sent()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	last := lastTaskControlResponse(t, conn.fakeConn)
	o := last.OpenInteractive()
	if o == nil || o.Status != protocol.OpenInteractiveStatus_Ok {
		t.Fatalf("response %+v", o)
	}
	if e, _ := h.Tasks.Get(hex.EncodeToString(o.TaskId.Id[:])); e.Prompt != "triage new issues" {
		t.Fatalf("prompt %q", e.Prompt)
	}
}

func TestListShowsWaitReason(t *testing.T) {
	h := newTestHandler(t)
	resp := openBG(h, bgOpen("claude"), "p")
	id := hex.EncodeToString(resp.TaskId.Id[:])
	if w := waitReasonOf(t, h, id); w != protocol.WaitReason_NoCandidate {
		t.Fatalf("no runner: %v", w)
	}
	now := time.Now()
	h.Registry.Add(&RunnerEntry{ID: tcid("R"), Identity: testRunnerID("R"), Hostname: "h", AllowedRoots: []string{"/r"},
		AgentProfiles: []string{"claude"}, MaxTasks: 1, ActiveTasks: map[string]struct{}{"busy": {}},
		ConnectedAt: now, LastTaskActivity: now, Conn: &fakeConn{}})
	if w := waitReasonOf(t, h, id); w != protocol.WaitReason_AllBusy {
		t.Fatalf("full runner: %v", w)
	}
}

func waitReasonOf(t *testing.T, h *TaskHandler, id string) protocol.WaitReason {
	t.Helper()
	e, ok := h.Tasks.Get(id)
	if !ok {
		t.Fatalf("task %s not found", id)
	}
	return h.waitReasonFor(e)
}

// Review I1: a cancel makes the task terminal at once, but the runner reports
// the old run finished only after the agent answers its hangup (up to ~2 s).
// A resume in that window would be finished, or have its session torn down,
// by the old run's late report, so it is refused until the runner has let go.
func TestResumeIsRefusedWhileTheOldRunIsStillBound(t *testing.T) {
	h := newTestHandler(t)
	now := time.Now()
	id := endedBG(t, h, protocol.TaskKind_Interactive)
	h.Registry.Add(&RunnerEntry{ID: tcid("R"), Identity: testRunnerID("R"), Hostname: "h", AllowedRoots: []string{"/r"},
		AgentProfiles: []string{"codex"}, MaxTasks: 2, ActiveTasks: map[string]struct{}{id: {}},
		ConnectedAt: now, LastTaskActivity: now, Conn: &fakeConn{}})

	r := openBG(h, resumeReq(id, bgOpen("")), "second")
	if r.Status != protocol.OpenInteractiveStatus_ResumeNotTerminal {
		t.Fatalf("resume while the old run is bound: status %v, want resume_not_terminal", r.Status)
	}
	if e, _ := h.Tasks.Get(id); e.Status != protocol.TaskStatus_Cancelled {
		t.Fatalf("a refused resume changed the task: %v", e.Status)
	}

	h.Registry.UnbindTask(tcid("R"), id)
	if r := openBG(h, resumeReq(id, bgOpen("")), "second"); r.Status != protocol.OpenInteractiveStatus_Ok {
		t.Fatalf("resume after the runner let go: status %v", r.Status)
	}
}
