package server

import (
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// exactRunner registers a runner serving /shared that advertises profiles; a
// nil conn is one any number of opens can start a session on.
func exactRunner(t *testing.T, h *TaskHandler, name string, conn ConnHandle, profiles ...string) {
	t.Helper()
	now := time.Now()
	if conn == nil {
		conn = newOpenableConn(t)
	}
	h.Registry.Add(&RunnerEntry{
		ID: tcid(name), Identity: testRunnerID(name), Hostname: "h-" + name,
		AllowedRoots: []string{"/shared/repo"}, MaxTasks: 4, AgentProfiles: profiles,
		ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastTaskActivity: now, Conn: conn,
	})
}

// recordedTask creates a finished task last run on runner `on`.
func recordedTask(t *testing.T, h *TaskHandler, kind protocol.TaskKind, prompt string, args []string, profile, on string) string {
	t.Helper()
	id := h.Tasks.Create("/shared/repo", prompt, kind, protocol.ClientKind_Cli, protocol.TaskID{}, on,
		protocol.RunnerSelector{}, args, authority.Grant{Caps: protocol.Capability_None, Scope: authority.DefaultScope()},
		profile)

	h.Tasks.Assign(id, testRunnerID(on), "/wt", false)
	h.Tasks.Finish(id, 0, nil)
	return id
}

// exactOpen is a detached exact-resume open of id.
func exactOpen(t *testing.T, id string) *protocol.OpenInteractiveRequest {
	t.Helper()
	oi := &protocol.OpenInteractiveRequest{ResumeTaskId: hexToTaskID(t, id), Kind: protocol.TaskKind_Interactive}
	oi.SetResumeExact(true)
	oi.SetDetached(true)
	oi.SetInitialRows(0)
	oi.SetInitialCols(0)
	return oi
}

func openExact(h *TaskHandler, oi *protocol.OpenInteractiveRequest) (protocol.OpenInteractiveResponse, *protocol.RunnerCandidatesBody) {
	return h.handleOpenInteractive(objproto.ConnectionID{}, nil, oi, protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All, "")
}

func TestExactMismatchEachField(t *testing.T) {
	for _, c := range []struct {
		field string
		set   func(*protocol.OpenInteractiveRequest)
	}{
		{"--agent-arg", func(r *protocol.OpenInteractiveRequest) { r.ExtraArgs = protocol.AgentArgsFromStrings([]string{"--y"}) }},
		{"--agent", func(r *protocol.OpenInteractiveRequest) { r.SetAgentProfile([]byte("bash")) }},
		{"--caps", func(r *protocol.OpenInteractiveRequest) { r.SetResumeCapsOverride(true) }},
		{"--scope", func(r *protocol.OpenInteractiveRequest) { r.SetScopePresent(true) }},
	} {
		t.Run(c.field, func(t *testing.T) {
			h := newTestHandler(t)
			exactRunner(t, h, "A", nil, "bash")
			id := recordedTask(t, h, protocol.TaskKind_Interactive, "", nil, "bash", "A")
			req := exactOpen(t, id)
			c.set(req)
			resp, _ := openExact(h, req)
			if resp.Status != protocol.OpenInteractiveStatus_ResumeExactMismatch {
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

	stream := recordedTask(t, h, protocol.TaskKind_Stream, "", nil, "bash", "A")
	oi := &protocol.OpenInteractiveRequest{ResumeTaskId: hexToTaskID(t, stream), Kind: protocol.TaskKind_Interactive}
	oi.SetResumeExact(true)
	oresp, _ := h.handleOpenInteractive(objproto.ConnectionID{}, nil, oi, protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All, "")
	if oresp.Status != protocol.OpenInteractiveStatus_ResumeExactMismatch {
		t.Fatalf("open of a stream task with kind=interactive: status %v, want resume_exact_mismatch", oresp.Status)
	}

	// Explicit kind=stream against a recorded interactive task mismatches.
	inter := recordedTask(t, h, protocol.TaskKind_Interactive, "", nil, "bash", "A")
	oi3 := &protocol.OpenInteractiveRequest{ResumeTaskId: hexToTaskID(t, inter), Kind: protocol.TaskKind_Stream}
	oi3.SetResumeExact(true)
	if r, _ := h.handleOpenInteractive(objproto.ConnectionID{}, nil, oi3, protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All, ""); r.Status != protocol.OpenInteractiveStatus_ResumeExactMismatch {
		t.Fatalf("open of an interactive task with kind=stream: status %v, want resume_exact_mismatch", r.Status)
	}
}

func TestOpenInteractiveKindUnsupported(t *testing.T) {
	badKinds := []struct {
		name string
		kind protocol.TaskKind
	}{
		{"credential", protocol.TaskKind_Credential},
		{"unknown_99", protocol.TaskKind(99)},
	}

	t.Run("foreground", func(t *testing.T) {
		h := newTestHandler(t)
		exactRunner(t, h, "A", nil, "bash")
		for _, tc := range badKinds {
			oi := &protocol.OpenInteractiveRequest{Kind: tc.kind}
			oi.SetRepoPath([]byte("/shared/repo"))
			oi.SetDetached(true)
			oi.SetInitialRows(0)
			oi.SetInitialCols(0)
			resp, _ := h.handleOpenInteractive(objproto.ConnectionID{}, nil, oi, protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All, "")
			if resp.Status != protocol.OpenInteractiveStatus_KindUnsupported {
				t.Errorf("%s (%v): status %v msg %q, want kind_unsupported", tc.name, tc.kind, resp.Status, resp.ErrorMsg)
			}
		}
		if n := len(h.Tasks.List(0)); n != 0 {
			t.Fatalf("refused foreground opens created %d tasks", n)
		}
		// Control: interactive succeeds through the same path.
		okReq := &protocol.OpenInteractiveRequest{Kind: protocol.TaskKind_Interactive}
		okReq.SetRepoPath([]byte("/shared/repo"))
		okReq.SetDetached(true)
		okReq.SetInitialRows(0)
		okReq.SetInitialCols(0)
		if resp, _ := h.handleOpenInteractive(objproto.ConnectionID{}, nil, okReq, protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All, ""); resp.Status != protocol.OpenInteractiveStatus_Ok {
			t.Fatalf("control interactive: status %v msg %q, want ok", resp.Status, resp.ErrorMsg)
		}
	})

	t.Run("background", func(t *testing.T) {
		h := newTestHandler(t)
		for _, tc := range badKinds {
			oi := bgOpen("claude")
			oi.Kind = tc.kind
			// Test both via handleOpenInteractive and direct openBackground.
			if r := openBG(h, oi, "p"); r.Status != protocol.OpenInteractiveStatus_KindUnsupported {
				t.Errorf("openBG %s (%v): status %v msg %q, want kind_unsupported", tc.name, tc.kind, r.Status, r.ErrorMsg)
			}
			if r := h.openBackground(backgroundOpen{req: oi, repo: "/r", prompt: "p"}); r.Status != protocol.OpenInteractiveStatus_KindUnsupported {
				t.Errorf("openBackground %s (%v): status %v msg %q, want kind_unsupported", tc.name, tc.kind, r.Status, r.ErrorMsg)
			}
		}
		if n := len(h.Tasks.List(0)); n != 0 {
			t.Fatalf("refused background opens created %d tasks", n)
		}
		// Control: interactive and oneshot succeed through the same path.
		for _, k := range []protocol.TaskKind{protocol.TaskKind_Interactive, protocol.TaskKind_Oneshot} {
			oi := bgOpen("claude")
			oi.Kind = k
			if r := h.openBackground(backgroundOpen{req: oi, repo: "/r", prompt: "p"}); r.Status != protocol.OpenInteractiveStatus_Ok || !r.Queued() {
				t.Fatalf("control background %v: status %v queued %v msg %q, want ok+queued", k, r.Status, r.Queued(), r.ErrorMsg)
			}
		}
	})
}

func TestExactWithoutResumeID(t *testing.T) {
	h := newTestHandler(t)
	exactRunner(t, h, "A", nil, "bash")
	oi := &protocol.OpenInteractiveRequest{Kind: protocol.TaskKind_Interactive}
	oi.SetRepoPath([]byte("/shared/repo"))
	oi.SetResumeExact(true)
	if resp, _ := h.handleOpenInteractive(objproto.ConnectionID{}, nil, oi, protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All, ""); resp.Status != protocol.OpenInteractiveStatus_ResumeExactMismatch {
		t.Fatalf("open: status %v, want resume_exact_mismatch", resp.Status)
	}
}

func TestExactKeepsRecordedSettings(t *testing.T) {
	h := newTestHandler(t)
	exactRunner(t, h, "A", nil, "bash")
	id := recordedTask(t, h, protocol.TaskKind_Interactive, "", []string{"--x"}, "bash", "A")
	resp, _ := openExact(h, exactOpen(t, id))
	if resp.Status != protocol.OpenInteractiveStatus_Ok {
		t.Fatalf("exact resume: status %v msg %q", resp.Status, resp.ErrorMsg)
	}
	e, _ := h.Tasks.Get(id)
	if len(e.ExtraArgs) != 1 || e.ExtraArgs[0] != "--x" || e.AgentProfile != "bash" {
		t.Fatalf("after exact resume: args=%v profile=%q, want the record", e.ExtraArgs, e.AgentProfile)
	}

	// The contrast, pinned: a plain resume with no args clears them.
	other := recordedTask(t, h, protocol.TaskKind_Interactive, "", []string{"--x"}, "bash", "A")
	plain := exactOpen(t, other)
	plain.SetResumeExact(false)
	if resp, _ := openExact(h, plain); resp.Status != protocol.OpenInteractiveStatus_Ok {
		t.Fatalf("plain resume: status %v msg %q", resp.Status, resp.ErrorMsg)
	}
	if e, _ := h.Tasks.Get(other); len(e.ExtraArgs) != 0 {
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

	oi := &protocol.OpenInteractiveRequest{ResumeTaskId: hexToTaskID(t, id), Kind: protocol.TaskKind_Interactive}
	oi.SetResumeExact(true)
	resp, _ := h.handleOpenInteractive(objproto.ConnectionID{}, tuiConn, oi, protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All, "")
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

	// Each case resumes its own task recorded on B, so no case has to wind
	// down the session the one before it started.
	id := recordedTask(t, h, protocol.TaskKind_Interactive, "", nil, "bash", "B")
	resp, _ := openExact(h, exactOpen(t, id))
	if resp.Status != protocol.OpenInteractiveStatus_Ok {
		t.Fatalf("two candidates, last runner B present: status %v, want ok on B", resp.Status)
	}
	if e, _ := h.Tasks.Get(id); e.BoundRunnerID != tcid("B").String() {
		t.Fatalf("bound to %q, want B", e.BoundRunnerID)
	}

	// B gone, one candidate left: Any picks it.
	h.Registry.Remove(tcid("B"))
	id = recordedTask(t, h, protocol.TaskKind_Interactive, "", nil, "bash", "B")
	if resp, _ := openExact(h, exactOpen(t, id)); resp.Status != protocol.OpenInteractiveStatus_Ok {
		t.Fatalf("B gone, A left: status %v, want ok", resp.Status)
	}
	if e, _ := h.Tasks.Get(id); e.BoundRunnerID != tcid("A").String() {
		t.Fatalf("bound to %q, want A", e.BoundRunnerID)
	}

	// B gone, two candidates left: Any is ambiguous, as for any resume.
	exactRunner(t, h, "C", nil, "bash")
	id = recordedTask(t, h, protocol.TaskKind_Interactive, "", nil, "bash", "B")
	if resp, _ := openExact(h, exactOpen(t, id)); resp.Status != protocol.OpenInteractiveStatus_AmbiguousRunner {
		t.Fatalf("B gone, A and C left: status %v, want ambiguous_runner", resp.Status)
	}
}

// Review Focus 2: the recorded profile is the setting, so a runner that no
// longer advertises it is not a fallback.
func TestExactProfileGone(t *testing.T) {
	h := newTestHandler(t)
	exactRunner(t, h, "A", nil, "claude")
	id := recordedTask(t, h, protocol.TaskKind_Interactive, "", nil, "bash", "A")
	resp, _ := openExact(h, exactOpen(t, id))
	if resp.Status != protocol.OpenInteractiveStatus_ProfileUnavailable {
		t.Fatalf("status %v, want profile_unavailable", resp.Status)
	}
}

// The recorded profile filters the candidates before the last runner is
// preferred: a last runner that no longer offers the profile is not a reason
// to fail when another one does.
func TestExactPrefersProfileOverLastRunner(t *testing.T) {
	h := newTestHandler(t)
	exactRunner(t, h, "A", nil, "claude") // last runner, lost the profile
	bConn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9122-1"), nextBidi: newFakeStream(t)}
	exactRunner(t, h, "B", bConn, "bash")
	inter := recordedTask(t, h, protocol.TaskKind_Interactive, "", nil, "bash", "A")
	oi := &protocol.OpenInteractiveRequest{ResumeTaskId: hexToTaskID(t, inter), Kind: protocol.TaskKind_Interactive}
	oi.SetResumeExact(true)
	oi.SetDetached(true)
	oi.SetInitialRows(0)
	oi.SetInitialCols(0)
	resp, _ := h.handleOpenInteractive(objproto.ConnectionID{}, nil, oi, protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All, "")
	if resp.Status != protocol.OpenInteractiveStatus_Ok {
		t.Fatalf("open: status %v, want ok on B", resp.Status)
	}
	if e, _ := h.Tasks.Get(inter); e.BoundRunnerID != tcid("B").String() {
		t.Fatalf("open bound to %q, want B", e.BoundRunnerID)
	}
}

// Prompt is a setting like the rest; kind must match the recorded kind.
func TestExactRefusesPromptAndOneshot(t *testing.T) {
	t.Run("prompt", func(t *testing.T) {
		h := newTestHandler(t)
		oneshotRunner(h, "A", true)
		id := recordedOneshot(t, h, "P", "A")
		req := withOneshot(exactOpen(t, id))
		resp, _ := h.handleOpenInteractive(objproto.ConnectionID{}, nil, req, protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All, "anything")
		if resp.Status != protocol.OpenInteractiveStatus_ResumeExactMismatch || !strings.Contains(string(resp.ErrorMsg), "prompt") {
			t.Fatalf("status = %v (%q), want resume_exact_mismatch naming prompt", resp.Status, resp.ErrorMsg)
		}
	})
	t.Run("kind", func(t *testing.T) {
		h := newTestHandler(t)
		oneshotRunner(h, "A", true)
		id := recordedTask(t, h, protocol.TaskKind_Interactive, "", nil, "claude", "A")
		req := withOneshot(exactOpen(t, id))
		resp, _ := h.handleOpenInteractive(objproto.ConnectionID{}, nil, req, protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All, "")
		if resp.Status != protocol.OpenInteractiveStatus_ResumeExactMismatch || !strings.Contains(string(resp.ErrorMsg), "kind") {
			t.Fatalf("status = %v (%q), want resume_exact_mismatch naming kind", resp.Status, resp.ErrorMsg)
		}
	})
}

// recordedOneshot creates a finished task on /r whose last run was a
// oneshot with prompt, run on runner `on`.
func recordedOneshot(t *testing.T, h *TaskHandler, prompt, on string) string {
	t.Helper()
	id, err := h.Tasks.CreateWithinLimits("/r", prompt, protocol.TaskKind_Oneshot, protocol.ClientKind_Cli,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_None, Scope: authority.DefaultScope()},
		"claude", RunSpec{Mode: protocol.RunMode_Foreground})
	if err != nil {
		t.Fatal(err)
	}
	h.Tasks.Assign(id, testRunnerID(on), "/wt", false)
	h.Tasks.Finish(id, 0, nil)
	return id
}

// An exact resume runs what was recorded: the last run's prompt and its
// oneshot kind reach the runner when the client sends a plain exact resume
// (Kind=Interactive, no --oneshot, no prompt).
func TestExactRunsRecordedPromptAndOneshot(t *testing.T) {
	h := newTestHandler(t)
	rc := oneshotRunner(h, "A", true)
	id := recordedOneshot(t, h, "P", "A")
	resp, _ := openExact(h, exactOpen(t, id))
	if resp.Status != protocol.OpenInteractiveStatus_Ok {
		t.Fatalf("status %v %q", resp.Status, resp.ErrorMsg)
	}
	oer := sentOpenExec(t, rc)
	if !oer.Oneshot() || !oer.PromptPresent() {
		t.Fatalf("OpenExec oneshot=%v prompt=%v, want the recorded --oneshot with its prompt", oer.Oneshot(), oer.PromptPresent())
	}
	var body protocol.PromptBody
	if len(rc.sendStreams) != 1 || body.DecodeExact(rc.sendStreams[0].bytes) != nil || string(body.Prompt) != "P" {
		t.Fatalf("prompt stream: %d streams, body %q, want the recorded P", len(rc.sendStreams), body.Prompt)
	}
	if e, _ := h.Tasks.Get(id); e.Kind != protocol.TaskKind_Oneshot || e.Prompt != "P" {
		t.Fatalf("after exact resume: kind=%v prompt=%q, want oneshot + P", e.Kind, e.Prompt)
	}
}

// A background open needs a prompt; an exact one has the recorded prompt and
// adopts the recorded oneshot kind from a plain exact resume request.
func TestExactBackgroundUsesRecordedPrompt(t *testing.T) {
	h := newTestHandler(t)
	oneshotRunner(h, "A", true)
	id := recordedOneshot(t, h, "P", "A")
	req := exactOpen(t, id)
	req.SetRunModeSet(true)
	req.SetBackground(true)
	req.SetConditionsPresent(true)
	req.SetConditions(protocol.BackgroundConditions{DeadlineMs: 3600000})
	resp, _ := openExact(h, req)
	if resp.Status != protocol.OpenInteractiveStatus_Ok || !resp.Queued() {
		t.Fatalf("status %v (%q) queued %v, want queued", resp.Status, resp.ErrorMsg, resp.Queued())
	}
	if e, _ := h.Tasks.Get(id); e.Prompt != "P" || e.Kind != protocol.TaskKind_Oneshot || e.RunMode != protocol.RunMode_Background {
		t.Fatalf("queued: prompt %q kind %v mode %v, want the recorded P, oneshot, background", e.Prompt, e.Kind, e.RunMode)
	}
}
