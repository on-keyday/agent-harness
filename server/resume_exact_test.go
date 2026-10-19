package server

import (
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// exactRunner registers a runner serving /shared that advertises profiles.
func exactRunner(t *testing.T, h *TaskHandler, name string, conn ConnHandle, profiles ...string) {
	t.Helper()
	now := time.Now()
	if conn == nil {
		conn = stubConn{}
	}
	h.Registry.Add(&RunnerEntry{
		ID: tcid(name), Identity: testRunnerID(name), Hostname: "h-" + name,
		AllowedRoots: []string{"/shared"}, MaxTasks: 4, AgentProfiles: profiles,
		ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastTaskActivity: now, Conn: conn,
	})
}

// recordedTask creates a finished task last run on runner `on`.
func recordedTask(t *testing.T, h *TaskHandler, kind protocol.TaskKind, prompt string, args []string, profile, on string) string {
	t.Helper()
	id := h.Tasks.Create("/shared/repo", prompt, kind, protocol.ClientKind_Cli, protocol.TaskID{}, on,
		protocol.RunnerSelector{}, args, protocol.Capability_None, defaultScope(), profile)
	h.Tasks.Assign(id, testRunnerID(on), "/wt", false)
	h.Tasks.Finish(id, 0, nil)
	return id
}

func exactSubmit(t *testing.T, id string) *protocol.SubmitRequest {
	t.Helper()
	req := &protocol.SubmitRequest{ResumeTaskId: hexToTaskID(t, id)}
	req.SetResumeExact(true)
	return req
}

func TestExactMismatchEachField(t *testing.T) {
	for _, c := range []struct {
		field string
		set   func(*protocol.SubmitRequest)
	}{
		{"prompt", func(r *protocol.SubmitRequest) { r.SetPrompt([]byte("new")) }},
		{"--agent-arg", func(r *protocol.SubmitRequest) { r.ExtraArgs = protocol.AgentArgsFromStrings([]string{"--y"}) }},
		{"--agent", func(r *protocol.SubmitRequest) { r.SetAgentProfile([]byte("bash")) }},
		{"--caps", func(r *protocol.SubmitRequest) { r.SetResumeCapsOverride(true) }},
		{"--scope", func(r *protocol.SubmitRequest) { r.SetScopePresent(true) }},
	} {
		t.Run(c.field, func(t *testing.T) {
			h := newTestHandler(t)
			exactRunner(t, h, "A", nil, "bash")
			id := recordedTask(t, h, protocol.TaskKind_Oneshot, "p", nil, "bash", "A")
			req := exactSubmit(t, id)
			c.set(req)
			resp, _ := h.handleSubmit("", req, protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All)
			if resp.Status != protocol.SubmitStatus_ResumeExactMismatch {
				t.Fatalf("status = %v, want resume_exact_mismatch", resp.Status)
			}
			if !strings.Contains(string(resp.ErrorMsg), c.field) {
				t.Fatalf("error_msg %q does not name %s", resp.ErrorMsg, c.field)
			}
		})
	}
}

func TestExactKindMismatch(t *testing.T) {
	h := newTestHandler(t)
	exactRunner(t, h, "A", nil, "bash")
	inter := recordedTask(t, h, protocol.TaskKind_Interactive, "", nil, "bash", "A")
	resp, _ := h.handleSubmit("", exactSubmit(t, inter), protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All)
	if resp.Status != protocol.SubmitStatus_ResumeExactMismatch || !strings.Contains(string(resp.ErrorMsg), "kind") {
		t.Fatalf("submit of an interactive task: status %v msg %q, want a kind mismatch", resp.Status, resp.ErrorMsg)
	}

	stream := recordedTask(t, h, protocol.TaskKind_Stream, "", nil, "bash", "A")
	oi := &protocol.OpenInteractiveRequest{ResumeTaskId: hexToTaskID(t, stream)}
	oi.SetResumeExact(true) // event_stream = 0, recorded Stream
	oresp, _ := h.handleOpenInteractive("", nil, oi, protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All)
	if oresp.Status != protocol.OpenInteractiveStatus_ResumeExactMismatch {
		t.Fatalf("open of a stream task without event_stream: status %v, want resume_exact_mismatch", oresp.Status)
	}
}

func TestExactWithoutResumeID(t *testing.T) {
	h := newTestHandler(t)
	exactRunner(t, h, "A", nil, "bash")
	sub := &protocol.SubmitRequest{}
	sub.SetRepoPath([]byte("/shared/repo"))
	sub.SetResumeExact(true)
	if resp, _ := h.handleSubmit("", sub, protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All); resp.Status != protocol.SubmitStatus_ResumeExactMismatch {
		t.Fatalf("submit: status %v, want resume_exact_mismatch", resp.Status)
	}
	oi := &protocol.OpenInteractiveRequest{}
	oi.SetRepoPath([]byte("/shared/repo"))
	oi.SetResumeExact(true)
	if resp, _ := h.handleOpenInteractive("", nil, oi, protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All); resp.Status != protocol.OpenInteractiveStatus_ResumeExactMismatch {
		t.Fatalf("open: status %v, want resume_exact_mismatch", resp.Status)
	}
}

func TestExactKeepsRecordedSettings(t *testing.T) {
	h := newTestHandler(t)
	exactRunner(t, h, "A", nil, "bash")
	id := recordedTask(t, h, protocol.TaskKind_Oneshot, "p", []string{"--x"}, "bash", "A")
	resp, _ := h.handleSubmit("", exactSubmit(t, id), protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All)
	if resp.Status != protocol.SubmitStatus_Ok {
		t.Fatalf("exact resume: status %v msg %q", resp.Status, resp.ErrorMsg)
	}
	e, _ := h.Tasks.Get(id)
	if e.Prompt != "p" || len(e.ExtraArgs) != 1 || e.ExtraArgs[0] != "--x" || e.AgentProfile != "bash" {
		t.Fatalf("after exact resume: prompt=%q args=%v profile=%q, want the record", e.Prompt, e.ExtraArgs, e.AgentProfile)
	}

	// The contrast, pinned: a plain resume with no args clears them (today's
	// behaviour, unchanged).
	h.Tasks.Finish(id, 0, nil)
	plain := &protocol.SubmitRequest{ResumeTaskId: hexToTaskID(t, id)}
	plain.SetPrompt([]byte("q"))
	if resp, _ := h.handleSubmit("", plain, protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All); resp.Status != protocol.SubmitStatus_Ok {
		t.Fatalf("plain resume: status %v", resp.Status)
	}
	if e, _ := h.Tasks.Get(id); len(e.ExtraArgs) != 0 {
		t.Fatalf("plain resume kept args %v; it replaces them", e.ExtraArgs)
	}
}

func TestExactOpenSendsRecordedArgsToRunner(t *testing.T) {
	h := newTestHandler(t)
	runnerConn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9120-1"), nextStreamID: 42}
	exactRunner(t, h, "A", runnerConn, "bash")
	id := recordedTask(t, h, protocol.TaskKind_Interactive, "", []string{"--x"}, "bash", "A")
	tuiConn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9121-2"), nextStreamID: 7}
	// Blocking streams on both sides: noopBidiStream EOFs at once, and the
	// handler's teardown then races this test (the open-interactive flake family).
	runnerConn.nextBidi = newFakeStream(t)
	tuiConn.nextBidi = newFakeStream(t)

	oi := &protocol.OpenInteractiveRequest{ResumeTaskId: hexToTaskID(t, id)}
	oi.SetResumeExact(true)
	resp, _ := h.handleOpenInteractive("", tuiConn, oi, protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All)
	if resp.Status != protocol.OpenInteractiveStatus_Ok {
		t.Fatalf("status %v", resp.Status)
	}
	if len(runnerConn.sent) != 1 || runnerConn.sent[0][0] != byte(appwire.AppKind_RunnerControl) {
		t.Fatalf("runner conn: want one RunnerControl message, got %d", len(runnerConn.sent))
	}
	rr := &protocol.RunnerRequest{}
	if _, err := rr.Decode(runnerConn.sent[0][1:]); err != nil {
		t.Fatalf("decode: %v", err)
	}
	oer := rr.OpenExec()
	if oer == nil {
		t.Fatal("not an OpenExec")
	}
	if args := oer.ExtraArgs.AsStrings(); len(args) != 1 || args[0] != "--x" {
		t.Fatalf("OpenExec.ExtraArgs = %v, want the recorded [--x]", args)
	}
	if string(oer.AgentProfile) != "bash" {
		t.Fatalf("OpenExec.AgentProfile = %q, want bash", oer.AgentProfile)
	}
}

func TestExactPinsLastRunner(t *testing.T) {
	h := newTestHandler(t)
	exactRunner(t, h, "A", nil, "bash")
	exactRunner(t, h, "B", nil, "bash")
	id := recordedTask(t, h, protocol.TaskKind_Oneshot, "p", nil, "bash", "B")

	resp, _ := h.handleSubmit("", exactSubmit(t, id), protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All)
	if resp.Status != protocol.SubmitStatus_Ok {
		t.Fatalf("two candidates, last runner B present: status %v, want ok on B", resp.Status)
	}
	if e, _ := h.Tasks.Get(id); e.BoundRunnerID != tcid("B").String() {
		t.Fatalf("bound to %q, want B", e.BoundRunnerID)
	}

	// B gone, one candidate left: Any picks it.
	h.Tasks.Assign(id, testRunnerID("B"), "/wt", false)
	h.Tasks.Finish(id, 0, nil)
	h.Registry.Remove(tcid("B"))
	if resp, _ := h.handleSubmit("", exactSubmit(t, id), protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All); resp.Status != protocol.SubmitStatus_Ok {
		t.Fatalf("B gone, A left: status %v, want ok", resp.Status)
	}
	if e, _ := h.Tasks.Get(id); e.BoundRunnerID != tcid("A").String() {
		t.Fatalf("bound to %q, want A", e.BoundRunnerID)
	}

	// B gone, two candidates left: Any is ambiguous, as for any resume.
	h.Tasks.Assign(id, testRunnerID("B"), "/wt", false)
	h.Tasks.Finish(id, 0, nil)
	exactRunner(t, h, "C", nil, "bash")
	if resp, _ := h.handleSubmit("", exactSubmit(t, id), protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All); resp.Status != protocol.SubmitStatus_AmbiguousRunner {
		t.Fatalf("B gone, A and C left: status %v, want ambiguous_runner", resp.Status)
	}
}

// Review Focus 2: the recorded profile is the setting, so a runner that no
// longer advertises it is not a fallback.
func TestExactProfileGone(t *testing.T) {
	h := newTestHandler(t)
	exactRunner(t, h, "A", nil, "claude")
	id := recordedTask(t, h, protocol.TaskKind_Oneshot, "p", nil, "bash", "A")
	resp, _ := h.handleSubmit("", exactSubmit(t, id), protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All)
	if resp.Status != protocol.SubmitStatus_ProfileUnavailable {
		t.Fatalf("status %v, want profile_unavailable", resp.Status)
	}
}
