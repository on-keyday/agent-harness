package server

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/topics"
	"github.com/on-keyday/objtrsf/exec/frame"
	"github.com/on-keyday/objtrsf/objproto"
)

// A-D33: the PTY is an axis of its own. The server resolves the open's choice
// — --pty / --no-pty, else the mode's default — and sends the final answer.

// withPty sets the open's PTY choice: --pty (true) or --no-pty (false).
func withPty(oi *protocol.OpenInteractiveRequest, pty bool) *protocol.OpenInteractiveRequest {
	oi.SetExtPresent(true)
	var ext protocol.OpenInteractiveExt
	if e := oi.Ext(); e != nil {
		ext = *e
	}
	ext.SetPtySet(true)
	ext.SetPty(pty)
	oi.SetExt(ext)
	return oi
}

func TestPtyResolvedPerMode(t *testing.T) {
	for _, c := range []struct {
		name      string
		oneshot   bool
		pty       *bool
		wantNoPTY bool
	}{
		{"interactive default", false, nil, false},
		{"oneshot default", true, nil, true},
		{"interactive --no-pty", false, boolp(false), true},
		{"oneshot --pty", true, boolp(true), false},
		{"interactive --pty", false, boolp(true), false},
		{"oneshot --no-pty", true, boolp(false), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newTestHandler(t)
			rc := oneshotRunner(h, "A", true)
			oi := detachedOpen(40, 120)
			oi.SetAgentProfile([]byte("claude"))
			if c.oneshot {
				withOneshot(oi)
			}
			if c.pty != nil {
				withPty(oi, *c.pty)
			}
			resp, _ := h.handleOpenInteractive(objproto.ConnectionID{}, nil, oi, protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All, "p")
			if resp.Status != protocol.OpenInteractiveStatus_Ok {
				t.Fatalf("status %v %q", resp.Status, resp.ErrorMsg)
			}
			if oer := sentOpenExec(t, rc); oer.NoPty() != c.wantNoPTY {
				t.Fatalf("OpenExec no_pty=%v, want %v", oer.NoPty(), c.wantNoPTY)
			}
			if e := taskOf(t, h, resp); e.NoPTY != c.wantNoPTY {
				t.Fatalf("task NoPTY=%v, want %v", e.NoPTY, c.wantNoPTY)
			}
		})
	}
}

func boolp(b bool) *bool { return &b }

// A stream session has no PTY: asking for one is refused, in either run mode,
// before any task exists.
func TestPtyWithStreamRefused(t *testing.T) {
	h := newTestHandler(t)
	oneshotRunner(h, "A", true)
	fg := withPty(detachedOpen(40, 120), true)
	fg.SetAgentProfile([]byte("claude"))
	fg.SetEventStream(true)
	if resp, _ := h.handleOpenInteractive(objproto.ConnectionID{}, nil, fg, protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All, "p"); resp.Status != protocol.OpenInteractiveStatus_PtyStream {
		t.Fatalf("foreground: %v", resp.Status)
	}
	bg := withPty(bgOpen("claude"), true)
	bg.SetEventStream(true)
	if r := openBG(h, bg, "p"); r.Status != protocol.OpenInteractiveStatus_PtyStream {
		t.Fatalf("background: %v", r.Status)
	}
	if n := len(h.Tasks.List(0)); n != 0 {
		t.Fatalf("%d tasks created", n)
	}
}

// A queued background run is started later by Tick, so the resolved choice is
// stored on the task and sent when it is placed.
func TestPtyQueuedRunStartsWithStoredChoice(t *testing.T) {
	h := newTestHandler(t)
	resp := openBG(h, withOneshot(bgOpen("claude")), "p")
	if resp.Status != protocol.OpenInteractiveStatus_Ok || !resp.Queued() {
		t.Fatalf("status %v queued %v", resp.Status, resp.Queued())
	}
	e := taskOf(t, h, resp)
	if !e.NoPTY {
		t.Fatal("a queued --oneshot did not store its default, no PTY")
	}
	rc := oneshotRunner(h, "A", true)
	runner, _ := h.Registry.Get(tcid("A"))
	if !h.Registry.BindTask(runner.ID, e.ID) {
		t.Fatal("bind")
	}
	h.Tasks.Assign(e.ID, runner.Identity, "", false)
	if err := h.StartQueuedSession(runner.ID, e.ID); err != nil {
		t.Fatal(err)
	}
	if oer := sentOpenExec(t, rc); !oer.NoPty() || !oer.Oneshot() {
		t.Fatalf("placed OpenExec no_pty=%v oneshot=%v", oer.NoPty(), oer.Oneshot())
	}
}

func TestPtyChoiceSurvivesReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.log")
	w, err := OpenWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	store := NewTaskStore()
	store.SetWAL(w)
	id, err := store.CreateWithinLimits("/r", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Cli,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{}, "claude",
		RunSpec{Mode: protocol.RunMode_Background, Cond: RunConditions{Deadline: time.Hour}, NoPTY: true})
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
	if e, _ := r.Get(id); !e.NoPTY {
		t.Fatal("no_pty lost across replay")
	}
}

// recordedNoPTY creates a finished task on /r whose last run had no PTY.
func recordedNoPTY(t *testing.T, h *TaskHandler, on string) string {
	t.Helper()
	id, err := h.Tasks.CreateWithinLimits("/r", "", protocol.TaskKind_Interactive, protocol.ClientKind_Cli,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_None, Scope: authority.DefaultScope()},
		"claude", RunSpec{Mode: protocol.RunMode_Foreground, NoPTY: true})
	if err != nil {
		t.Fatal(err)
	}
	h.Tasks.Assign(id, testRunnerID(on), "/wt", false)
	h.Tasks.Finish(id, 0, nil)
	return id
}

// An exact resume runs as the last run did, its PTY choice included; the PTY
// is a terminal-shape setting like -d and the size, so an exact resume may
// also state it.
func TestExactResumePtyChoice(t *testing.T) {
	t.Run("recorded", func(t *testing.T) {
		h := newTestHandler(t)
		rc := oneshotRunner(h, "A", true)
		id := recordedNoPTY(t, h, "A")
		if resp, _ := openExact(h, exactOpen(t, id)); resp.Status != protocol.OpenInteractiveStatus_Ok {
			t.Fatalf("status %v %q", resp.Status, resp.ErrorMsg)
		}
		if oer := sentOpenExec(t, rc); !oer.NoPty() {
			t.Fatal("exact resume dropped the recorded no-PTY")
		}
	})
	t.Run("stated", func(t *testing.T) {
		h := newTestHandler(t)
		rc := oneshotRunner(h, "A", true)
		id := recordedNoPTY(t, h, "A")
		if resp, _ := openExact(h, withPty(exactOpen(t, id), true)); resp.Status != protocol.OpenInteractiveStatus_Ok {
			t.Fatalf("status %v %q", resp.Status, resp.ErrorMsg)
		}
		if oer := sentOpenExec(t, rc); oer.NoPty() {
			t.Fatal("exact resume with --pty ran without one")
		}
		if e, _ := h.Tasks.Get(id); e.NoPTY {
			t.Fatal("the task still records no PTY")
		}
	})
}

// A plain resume reuses nothing (A-D7): an interactive resume of a no-PTY
// task gets a PTY unless it says otherwise.
func TestPlainResumeTakesModeDefault(t *testing.T) {
	h := newTestHandler(t)
	rc := oneshotRunner(h, "A", true)
	id := recordedNoPTY(t, h, "A")
	oi := &protocol.OpenInteractiveRequest{ResumeTaskId: hexToTaskID(t, id)}
	oi.SetDetached(true)
	oi.SetInitialRows(40)
	oi.SetInitialCols(120)
	oi.SetAgentProfile([]byte("claude"))
	if resp, _ := h.handleOpenInteractive(objproto.ConnectionID{}, nil, oi, protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All, ""); resp.Status != protocol.OpenInteractiveStatus_Ok {
		t.Fatalf("status %v %q", resp.Status, resp.ErrorMsg)
	}
	if oer := sentOpenExec(t, rc); oer.NoPty() {
		t.Fatal("a plain resume reused the last run's no-PTY")
	}
}

// The listing and every task event carry the run's choice: the kind alone
// cannot tell an interactive session on pipes from one in a PTY.
func TestListAndEventsCarryNoPTY(t *testing.T) {
	h := newTestHandler(t)
	mk := func(noPTY bool) string {
		id, err := h.Tasks.CreateWithinLimits("/r", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Cli,
			protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_All}, "claude",
			RunSpec{Mode: protocol.RunMode_Foreground, NoPTY: noPTY})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	pipeID, ptyID := mk(true), mk(false)
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_List}}
	req.SetList(defaultListQuery())
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9831-1"), nextSendStreamID: 10}
	h.Handle(conn, encodeTaskControlRequest(t, req))
	got := map[string]bool{}
	for _, ti := range decodeListBody(t, conn.sendStreams[0].bytes).Tasks {
		got[fmt.Sprintf("%x", ti.Id.Id[:])] = ti.NoPty()
	}
	if !got[pipeID] || got[ptyID] {
		t.Fatalf("listed no_pty: pipe=%v pty=%v", got[pipeID], got[ptyID])
	}

	s := New(Config{})
	getEvents := captureTaskEvents(s, topics.TasksStatus())
	id, err := s.tasks.CreateWithinLimits("/repo", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Cli,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_All}, "claude",
		RunSpec{Mode: protocol.RunMode_Foreground, NoPTY: true})
	if err != nil {
		t.Fatal(err)
	}
	s.tasks.Assign(id, testRunnerID("ws:127.0.0.1:8539-40"), "", false)
	s.tasks.MarkFailed(id, "runner_disconnected")
	seen := false
	for _, ev := range getEvents() {
		if fmt.Sprintf("%x", ev.TaskId.Id[:]) == id {
			seen = true
			if !ev.NoPty() {
				t.Fatalf("event %v carries no_pty=false", ev.Kind)
			}
		}
	}
	if !seen {
		t.Fatal("no event for the task")
	}
}

// pipeMux is a mux for a session whose child runs on pipes.
func pipeMux(t *testing.T) (*SessionMux, *fakeBidiStream, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	runner := newFakeStream(t)
	return newSessionMux(ctx, "task", protocol.TaskKind_Interactive, true, runner, NewRingBuffer(1<<20), SessionHooks{}), runner, ctx
}

func screenLinesOf(m *SessionMux) []string {
	m.screenMu.Lock()
	defer m.screenMu.Unlock()
	return m.screen.Lines()
}

// A child on pipes writes bare LF, which a terminal's ONLCR would have turned
// into CRLF. The screen model takes it as a terminal would have shown it — two
// lines, both at column 0, not a staircase — while the ring and the record
// keep the bytes the child wrote.
func TestNoPTYScreenTranslatesLFAndRecordKeepsRaw(t *testing.T) {
	mux, runner, _ := pipeMux(t)
	sink := newMemSink(1 << 20)
	if err := mux.StartRecording(sink, recordFresh); err != nil {
		t.Fatal(err)
	}
	runner.QueueRead(encodeFrame(frame.FrameType_Stdout, []byte("a\nb\n")))
	waitFor(t, func() bool { return len(screenLinesOf(mux)) > 1 && strings.TrimRight(screenLinesOf(mux)[1], " ") != "" })
	lines := screenLinesOf(mux)
	if got0, got1 := strings.TrimRight(lines[0], " "), strings.TrimRight(lines[1], " "); got0 != "a" || got1 != "b" {
		t.Fatalf("screen lines %q %q, want \"a\" \"b\" at column 0", got0, got1)
	}
	waitFor(t, func() bool { return len(sink.all()) > 0 })
	for _, e := range sink.all() {
		if e.FrameType == uint8(frame.FrameType_Stdout) && string(e.Payload) != "a\nb\n" {
			t.Fatalf("record payload %q, want the raw bytes", e.Payload)
		}
	}
	if !strings.Contains(string(mux.ring.Snapshot()), "a\nb\n") {
		t.Fatal("the ring does not hold the raw bytes")
	}
}

// What a client receives is what a terminal would have shown: an attached
// client and an observer get the LF as CRLF, on the replay and live, so the
// operator's own terminal does not staircase. Their input gets the terminal's
// other translation: Enter (CR) reaches the child's pipe as LF.
func TestNoPTYClientsGetTerminalTranslation(t *testing.T) {
	mux, runner, ctx := pipeMux(t)
	runner.QueueRead(encodeFrame(frame.FrameType_Stdout, []byte("one\n")))
	waitFor(t, func() bool { return mux.RingAppendCount() == 1 })

	control := newFakeStream(t)
	if err := mux.Attach(ctx, control); err != nil {
		t.Fatal(err)
	}
	viewer := newFakeStream(t)
	if err := mux.AttachViewer(ctx, viewer, 0, false); err != nil {
		t.Fatal(err)
	}
	runner.QueueRead(encodeFrame(frame.FrameType_Stdout, []byte("two\n")))
	for name, s := range map[string]*fakeBidiStream{"control": control, "viewer": viewer} {
		waitFor(t, func() bool { return strings.Contains(string(s.Written()), "two") })
		got := string(s.Written())
		if !strings.Contains(got, "one\r\n") || !strings.Contains(got, "two\r\n") || strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") {
			t.Fatalf("%s received %q, want every LF as CRLF", name, got)
		}
	}

	control.QueueRead(encodeFrame(frame.FrameType_Stdin, []byte("echo hi\r")))
	waitFor(t, func() bool { return strings.Contains(string(runner.Written()), "echo hi") })
	if got := string(runner.Written()); !strings.Contains(got, "echo hi\n") || strings.Contains(got, "\r") {
		t.Fatalf("runner received %q, want the CR as LF", got)
	}
}

// A PTY session's bytes pass untouched both ways: the terminal is the PTY.
func TestPTYSessionIsNotTranslated(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", protocol.TaskKind_Interactive, runner, NewRingBuffer(1<<20), SessionHooks{})
	control := newFakeStream(t)
	if err := mux.Attach(ctx, control); err != nil {
		t.Fatal(err)
	}
	runner.QueueRead(encodeFrame(frame.FrameType_Stdout, []byte("x\n")))
	waitFor(t, func() bool { return strings.Contains(string(control.Written()), "x") })
	if strings.Contains(string(control.Written()), "x\r\n") {
		t.Fatal("a PTY session's output was translated")
	}
	control.QueueRead(encodeFrame(frame.FrameType_Stdin, []byte("y\r")))
	waitFor(t, func() bool { return strings.Contains(string(runner.Written()), "y") })
	if !strings.Contains(string(runner.Written()), "y\r") {
		t.Fatal("a PTY session's input was translated")
	}
}

// The mux a session start builds is the one that stands in for the line
// discipline, so the resolved choice has to reach it, not only the runner's
// OpenExec (A-D33): server/session_start.go hands ss.NoPTY to newSessionMux.
func TestSessionStartGivesTheMuxItsPipeMode(t *testing.T) {
	for _, noPTY := range []bool{true, false} {
		t.Run(fmt.Sprintf("no_pty=%v", noPTY), func(t *testing.T) {
			h := newTestHandler(t)
			rc := oneshotRunner(h, "A", true)
			rc.nextBidi = newFakeStream(t) // a stream that stays open, so the mux outlives the open
			oi := withPty(detachedOpen(40, 120), !noPTY)
			oi.SetAgentProfile([]byte("claude"))
			resp, _ := h.handleOpenInteractive(objproto.ConnectionID{}, nil, oi, protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All, "p")
			if resp.Status != protocol.OpenInteractiveStatus_Ok {
				t.Fatalf("status %v %q", resp.Status, resp.ErrorMsg)
			}
			mux := h.Sessions.Get(taskOf(t, h, resp).ID)
			if mux == nil {
				t.Fatal("no session mux registered")
			}
			if mux.pipe != noPTY {
				t.Fatalf("mux.pipe=%v, want %v", mux.pipe, noPTY)
			}
		})
	}
}
