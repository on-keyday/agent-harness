package server

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// A3 (A-D29): `session new --oneshot`.

func withOneshot(oi *protocol.OpenInteractiveRequest) *protocol.OpenInteractiveRequest {
	oi.SetExtPresent(true)
	var ext protocol.OpenInteractiveExt
	if e := oi.Ext(); e != nil {
		ext = *e
	}
	ext.SetOneshot(true)
	oi.SetExt(ext)
	return oi
}

// oneshotRunner registers a runner on /r whose hello did (or did not) say it
// runs --oneshot, and returns its connection.
func oneshotRunner(h *TaskHandler, name string, serves bool) *fakeConn {
	now := time.Now()
	rc := &fakeConn{id: objproto.MustParseConnectionID(fmt.Sprintf("ws:127.0.0.1:%d-1", 9170+len(name))), nextStreamID: 42, autoSendStreams: true}
	h.Registry.Add(&RunnerEntry{
		ID: tcid(name), Identity: testRunnerID(name), Hostname: "h", AllowedRoots: []string{"/r"}, MaxTasks: 4,
		ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastTaskActivity: now, Conn: rc,
		AgentProfiles: []string{"claude"}, OneshotSessions: serves,
	})
	return rc
}

func sentOpenExec(t *testing.T, rc *fakeConn) *protocol.OpenExecRunnerRequest {
	t.Helper()
	for _, raw := range rc.sent {
		rr := &protocol.RunnerRequest{}
		if _, err := rr.Decode(raw[1:]); err == nil && rr.Kind == protocol.RunnerRequestType_OpenExec {
			return rr.OpenExec()
		}
	}
	t.Fatal("no OpenExec sent")
	return nil
}

func TestOneshotOpenRelaysBit(t *testing.T) {
	h := newTestHandler(t)
	rc := oneshotRunner(h, "A", true)
	oi := withOneshot(detachedOpen(40, 120))
	oi.SetAgentProfile([]byte("claude"))
	resp, _ := h.handleOpenInteractive("", nil, oi, protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All, "answer this")
	if resp.Status != protocol.OpenInteractiveStatus_Ok {
		t.Fatalf("status %v %q", resp.Status, resp.ErrorMsg)
	}
	if oer := sentOpenExec(t, rc); !oer.Oneshot() || !oer.PromptPresent() {
		t.Fatalf("OpenExec oneshot=%v prompt=%v", oer.Oneshot(), oer.PromptPresent())
	}
	if e := taskOf(t, h, resp); !e.Oneshot || e.Kind != protocol.TaskKind_Interactive {
		t.Fatalf("task oneshot=%v kind=%v, want an interactive task marked oneshot", e.Oneshot, e.Kind)
	}
}

func TestOneshotOpenNeedsPrompt(t *testing.T) {
	h := newTestHandler(t)
	oneshotRunner(h, "A", true)
	fg := withOneshot(detachedOpen(40, 120))
	fg.SetAgentProfile([]byte("claude"))
	if resp, _ := h.handleOpenInteractive("", nil, fg, protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All, ""); resp.Status != protocol.OpenInteractiveStatus_PromptRequired {
		t.Fatalf("foreground without a prompt: %v", resp.Status)
	}
	// A background open already needs one; the rule holds there too.
	if r := openBG(h, withOneshot(bgOpen("claude")), ""); r.Status != protocol.OpenInteractiveStatus_PromptRequired {
		t.Fatalf("background without a prompt: %v", r.Status)
	}
}

// Review Focus 1: a runner that did not say it runs --oneshot would ignore the
// bit and start the interactive argv. It never gets one.
func TestOneshotOpenSkipsRunnersWithoutHelloBit(t *testing.T) {
	h := newTestHandler(t)
	old := oneshotRunner(h, "OL", false)
	fg := withOneshot(detachedOpen(40, 120))
	fg.SetAgentProfile([]byte("claude"))
	resp, _ := h.handleOpenInteractive("", nil, fg, protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All, "p")
	if resp.Status != protocol.OpenInteractiveStatus_ProfileUnavailable {
		t.Fatalf("foreground on an old runner only: %v, want profile_unavailable", resp.Status)
	}
	if len(old.sent) != 0 {
		t.Fatalf("the old runner was sent %d messages", len(old.sent))
	}

	bg := openBG(h, withOneshot(bgOpen("claude")), "p")
	if bg.Status != protocol.OpenInteractiveStatus_Ok || !bg.Queued() {
		t.Fatalf("background: %v queued=%v", bg.Status, bg.Queued())
	}
	id := taskOf(t, h, bg).ID
	var started []string
	s := NewScheduler(h.Registry, h.Tasks, func(_ objproto.ConnectionID, tID string) error { started = append(started, tID); return nil })
	s.Tick()
	if len(started) != 0 {
		t.Fatalf("Tick placed a --oneshot task on a runner without the bit: %v", started)
	}
	if w := h.waitReasonFor(taskOf(t, h, bg)); w != protocol.WaitReason_NoCandidate {
		t.Fatalf("wait reason %v, want no_candidate", w)
	}
	oneshotRunner(h, "NEW", true)
	s.Tick()
	if len(started) != 1 || started[0] != id {
		t.Fatalf("after a runner with the bit connected, started %v", started)
	}
}

func TestRunnerEntryRecordsOneshotSessionsFromHello(t *testing.T) {
	var hello protocol.RunnerHello
	hello.SetOneshotSessions(true)
	if e := runnerEntryFromHello(tcid("R"), hello, 1, time.Now()); !e.OneshotSessions {
		t.Fatal("RunnerEntry.OneshotSessions not set from the hello")
	}
}

// A queued --oneshot task keeps the flag across a restart, so Tick still
// starts it with the oneshot argv.
func TestOneshotFlagSurvivesReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.log")
	w, err := OpenWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	store := NewTaskStore()
	store.SetWAL(w)
	id, err := store.CreateWithinLimits("/r", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Cli,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{}, "claude",
		RunSpec{Mode: protocol.RunMode_Background, Cond: RunConditions{Deadline: time.Hour}, Oneshot: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	events, _, err := ReadWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	r := NewTaskStore()
	r.ReplayEvents(events)
	if e, _ := r.Get(id); !e.Oneshot {
		t.Fatal("oneshot lost across replay")
	}
}

// Checkpoint review I1: --oneshot is a PTY session (A-D29). With --stream the
// open is refused here — the WebUI can send both — rather than created as a
// stream task the runner refuses later, or queued forever in background.
func TestOneshotWithStreamRefused(t *testing.T) {
	h := newTestHandler(t)
	oneshotRunner(h, "A", true)
	fg := withOneshot(detachedOpen(40, 120))
	fg.SetAgentProfile([]byte("claude"))
	fg.SetEventStream(true)
	if resp, _ := h.handleOpenInteractive("", nil, fg, protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All, "p"); resp.Status != protocol.OpenInteractiveStatus_OneshotStream {
		t.Fatalf("foreground: %v", resp.Status)
	}
	bg := withOneshot(bgOpen("claude"))
	bg.SetEventStream(true)
	if r := openBG(h, bg, "p"); r.Status != protocol.OpenInteractiveStatus_OneshotStream {
		t.Fatalf("background: %v", r.Status)
	}
	if n := len(h.Tasks.List(0)); n != 0 {
		t.Fatalf("%d tasks created", n)
	}
}
