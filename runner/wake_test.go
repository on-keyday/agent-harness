package runner

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/runner/streamagent"
	"github.com/on-keyday/agent-harness/termmode"
)

// captureWriter records every byte slice written to it.
type captureWriter struct {
	mu       sync.Mutex
	writes   [][]byte
	failNext error
}

func (c *captureWriter) write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failNext != nil {
		err := c.failNext
		c.failNext = nil
		return 0, err
	}
	cp := make([]byte, len(p))
	copy(cp, p)
	c.writes = append(c.writes, cp)
	return len(p), nil
}

func (c *captureWriter) writeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.writes)
}

func (c *captureWriter) writeAt(i int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if i < 0 || i >= len(c.writes) {
		return ""
	}
	return string(c.writes[i])
}

func TestSession_WakeStdin_SplitWrite(t *testing.T) {
	// Without bracketed paste, one fire emits two writes: the marker text,
	// then a lone Enter byte after wakeSubmitDelay. Read together they are
	// taken as pasted content and the Enter becomes a literal newline — see
	// the session.go comments for why the delay narrows that and the paste
	// does not have it.
	s := &Session{Now: time.Now}
	cw := &captureWriter{}
	s.reg = NewTaskRegistry()
	s.reg.put("abc", &taskEntry{wake: ptyWake(cw.write, termmode.New())})

	s.WakeStdin("abc")

	if got := cw.writeCount(); got != 2 {
		t.Fatalf("writeCount = %d, want 2 (text + lone Enter)", got)
	}
	if cw.writeAt(0) != wakeMarker {
		t.Errorf("write[0] = %q, want %q", cw.writeAt(0), wakeMarker)
	}
	if cw.writeAt(1) != "\r" {
		t.Errorf("write[1] = %q, want %q", cw.writeAt(1), "\r")
	}
}

func TestSession_WakeStdin_Debounce(t *testing.T) {
	s := &Session{Now: time.Now}
	cw := &captureWriter{}
	s.reg = NewTaskRegistry()
	s.reg.put("abc", &taskEntry{wake: ptyWake(cw.write, termmode.New())})

	s.WakeStdin("abc")
	s.WakeStdin("abc")
	s.WakeStdin("abc")

	// Three rapid calls collapse to one fire (= 2 writes: text + Enter).
	if got := cw.writeCount(); got != 2 {
		t.Errorf("debounce broken: writeCount=%d, want 2", got)
	}
}

func TestSession_WakeStdin_AfterWindow(t *testing.T) {
	now := time.Now()
	cur := now
	s := &Session{Now: func() time.Time { return cur }}
	cw := &captureWriter{}
	s.reg = NewTaskRegistry()
	s.reg.put("abc", &taskEntry{wake: ptyWake(cw.write, termmode.New())})

	s.WakeStdin("abc")
	cur = now.Add(wakeDebounceWindow + 100*time.Millisecond)
	s.WakeStdin("abc")

	// Two fires in two windows = 4 writes (2 per fire).
	if got := cw.writeCount(); got != 4 {
		t.Errorf("post-window wake suppressed: writeCount=%d, want 4", got)
	}
}

func TestSession_WakeStdin_UnknownTask(t *testing.T) {
	s := &Session{Now: time.Now}
	s.reg = NewTaskRegistry()
	// Should not panic on unknown task.
	s.WakeStdin("missing")
}

func TestSession_WakeStdin_TextWriteError_DoesNotAdvanceCursor(t *testing.T) {
	// When the first (text) write fails, the submit byte is not sent,
	// and lastWakeAt is not advanced — so a follow-up call within the
	// debounce window can still try.
	s := &Session{Now: time.Now}
	cw := &captureWriter{failNext: errors.New("pipe closed")}
	s.reg = NewTaskRegistry()
	s.reg.put("abc", &taskEntry{wake: ptyWake(cw.write, termmode.New())})

	s.WakeStdin("abc") // text write fails — writeCount stays 0
	if cw.writeCount() != 0 {
		t.Errorf("expected failed text write to not be recorded, got %d", cw.writeCount())
	}
	// Second call should still try. Both writes succeed; total = 2.
	s.WakeStdin("abc")
	if cw.writeCount() != 2 {
		t.Errorf("retry after failure: writeCount=%d, want 2", cw.writeCount())
	}
}

// Every path out of WakeStdin says which one it took.
//
// A wake that is dropped is otherwise indistinguishable from a wake that was
// written and ignored by the agent, and on 2026-08-29 that ambiguity cost a
// full investigation: a message showed shown_to=1/1 on the board while no turn
// ever started, and nothing on either side recorded which half had failed. The
// value of these lines is precisely that they are all present, so this asserts
// the set rather than any one of them.
func TestSession_WakeStdin_LogsWhichGateItTook(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*Session, *captureWriter)
		task  string
		want  string
	}{
		{
			name:  "unknown task",
			setup: func(s *Session, _ *captureWriter) { s.reg = NewTaskRegistry() },
			task:  "missing",
			want:  "task not known to this runner",
		},
		{
			name: "no stdin writer",
			setup: func(s *Session, _ *captureWriter) {
				s.reg = NewTaskRegistry()
				s.reg.put("abc", &taskEntry{})
			},
			task: "abc",
			want: "no stdin writer",
		},
		{
			name: "debounced",
			setup: func(s *Session, cw *captureWriter) {
				s.reg = NewTaskRegistry()
				// Inside wakeDebounceWindow, so the second fire is dropped.
				s.reg.put("abc", &taskEntry{wake: ptyWake(cw.write, termmode.New()), lastWakeAt: s.Now()})
			},
			task: "abc",
			want: "debounced",
		},
		{
			name: "written",
			setup: func(s *Session, cw *captureWriter) {
				s.reg = NewTaskRegistry()
				s.reg.put("abc", &taskEntry{wake: ptyWake(cw.write, termmode.New())})
			},
			task: "abc",
			want: "wake written",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			s := &Session{
				Now:    time.Now,
				Logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})),
			}
			cw := &captureWriter{}
			s.mu.Lock()
			tc.setup(s, cw)
			s.mu.Unlock()

			s.WakeStdin(tc.task)

			if got := buf.String(); !strings.Contains(got, tc.want) {
				t.Errorf("log did not name the path taken\n want substring: %q\n got: %s", tc.want, got)
			}
			// The task id is what makes a line usable when several tasks share
			// a runner, which is the ordinary case.
			if got := buf.String(); !strings.Contains(got, tc.task) {
				t.Errorf("log line does not carry the task id: %s", got)
			}
		})
	}
}

// An agent that asked for bracketed paste gets the marker as one paste and the
// Enter after it, in ONE write. That is the point: the split write's delay is
// the writer's, and an agent that reads late takes text and "\r" together and
// turns the "\r" into a newline. With the paste end explicit, one read is fine.
func TestSession_WakeStdin_BracketedPasteIsOneWrite(t *testing.T) {
	modes := termmode.New()
	modes.Feed([]byte("\x1b[?2004h"))
	s := &Session{Now: time.Now}
	cw := &captureWriter{}
	s.reg = NewTaskRegistry()
	s.reg.put("abc", &taskEntry{wake: ptyWake(cw.write, modes)})

	s.WakeStdin("abc")

	if got := cw.writeCount(); got != 1 {
		t.Fatalf("writeCount = %d, want 1 (paste and Enter together)", got)
	}
	if want := "\x1b[200~" + wakeMarker + "\x1b[201~\r"; cw.writeAt(0) != want {
		t.Errorf("write = %q, want %q", cw.writeAt(0), want)
	}
}

// `wake written` names the form the wake took. An idle agent submits either PTY
// form, so nothing on its screen says which one the runner chose; without the
// log, "did this runner see bracketed paste" has no answer short of stopping
// the agent to make the split one fail.
func TestSession_WakeStdin_LogsTheForm(t *testing.T) {
	pasting := termmode.New()
	pasting.Feed([]byte("\x1b[?2004h"))
	cases := []struct {
		name string
		wake func(write func([]byte) (int, error)) func() (wakeForm, error)
		want string
	}{
		{"pty without bracketed paste", func(w func([]byte) (int, error)) func() (wakeForm, error) {
			return ptyWake(w, termmode.New())
		}, "form=split"},
		{"pty with bracketed paste", func(w func([]byte) (int, error)) func() (wakeForm, error) {
			return ptyWake(w, pasting)
		}, "form=paste"},
		{"event stream", streamWake, "form=turn"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			s := &Session{Now: time.Now, Logger: slog.New(slog.NewTextHandler(&buf, nil))}
			cw := &captureWriter{}
			s.reg = NewTaskRegistry()
			s.reg.put("abc", &taskEntry{wake: tc.wake(cw.write)})

			s.WakeStdin("abc")

			if got := buf.String(); !strings.Contains(got, "wake written") || !strings.Contains(got, tc.want) {
				t.Errorf("want a `wake written` line with %s, got: %s", tc.want, got)
			}
		})
	}
}

// The mode is read at each wake, not when the session starts: an agent turns
// bracketed paste on after its first output, and can turn it off again.
func TestPtyWake_FollowsTheModeAsItChanges(t *testing.T) {
	modes := termmode.New()
	cw := &captureWriter{}
	wake := ptyWake(cw.write, modes)

	if _, err := wake(); err != nil {
		t.Fatal(err)
	}
	modes.Feed([]byte("\x1b[?2004h"))
	if _, err := wake(); err != nil {
		t.Fatal(err)
	}
	modes.Feed([]byte("\x1b[?2004l"))
	if _, err := wake(); err != nil {
		t.Fatal(err)
	}

	// split (2) + paste (1) + split (2)
	if got := cw.writeCount(); got != 5 {
		t.Fatalf("writeCount = %d, want 5", got)
	}
	if !strings.HasPrefix(cw.writeAt(2), "\x1b[200~") {
		t.Errorf("the wake after ESC[?2004h was not a paste: %q", cw.writeAt(2))
	}
	if cw.writeAt(3) != wakeMarker {
		t.Errorf("the wake after ESC[?2004l was not the plain text: %q", cw.writeAt(3))
	}
}

// Wakes run off the receive loop, so two for one task can overlap, and the
// debounce cannot stop the second: lastWakeAt is set only once a write has
// finished. The second is dropped while the first is still writing.
func TestSession_WakeStdin_OneInFlight(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	writes := 0
	blocking := func() (wakeForm, error) {
		mu.Lock()
		writes++
		mu.Unlock()
		close(entered)
		<-release
		return wakeSplit, nil
	}
	var buf bytes.Buffer
	var bufMu sync.Mutex
	s := &Session{
		Now:    time.Now,
		Logger: slog.New(slog.NewTextHandler(lockedWriter{&bufMu, &buf}, nil)),
	}
	s.reg = NewTaskRegistry()
	s.reg.put("abc", &taskEntry{wake: blocking})

	done := make(chan struct{})
	go func() { s.WakeStdin("abc"); close(done) }()
	<-entered
	s.WakeStdin("abc") // must return, without writing, while the first blocks
	close(release)
	<-done

	mu.Lock()
	defer mu.Unlock()
	if writes != 1 {
		t.Fatalf("writes = %d, want 1", writes)
	}
	bufMu.Lock()
	defer bufMu.Unlock()
	if !strings.Contains(buf.String(), "in flight") {
		t.Errorf("the dropped wake did not say why: %s", buf.String())
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// A TaskWake must not hold the receive loop while its write is blocked — the
// rule TestRebindSessionDoesNotBlockTheReceiveLoop pins for a rebind. An agent
// that leaves its input unread blocks the stdin write, and the loop behind it
// carries every control message for every task on the runner.
func TestTaskWakeDoesNotBlockTheReceiveLoop(t *testing.T) {
	const id = "91560a69a243764d181ed3f6bd7f76cf"
	release := make(chan struct{})
	defer close(release)
	reg := NewTaskRegistry()
	reg.put(id, &taskEntry{wake: func() (wakeForm, error) { <-release; return wakeSplit, nil }})
	s := &Session{reg: reg, Sender: nopSender{}, Now: time.Now}

	req := &protocol.RunnerRequest{Kind: protocol.RunnerRequestType_TaskWake}
	req.SetTaskWake(protocol.TaskWakeRequest{TaskId: taskIDOf(t, id)})
	payload, err := req.Append(nil)
	if err != nil {
		t.Fatal(err)
	}

	// Unlike a rebind, a blocked wake never gives up, so the dispatch runs on a
	// goroutine and the test fails at a deadline instead of hanging with it.
	done := make(chan struct{})
	go func() {
		dispatchRunnerRequest(context.Background(), s, s.logger(), appwire.AppKind_RunnerControl, payload)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatching a TaskWake held the receive loop while the wake's write was blocked")
	}
}

// A stream session's wake is one write of one protocol line: a user turn whose
// text is the marker. The adapter reads lines, so anything else — the PTY
// path's bare text and lone CR — would be a protocol error there.
func TestSession_WakeStdin_StreamIsOneUserTurnLine(t *testing.T) {
	s := &Session{Now: time.Now}
	cw := &captureWriter{}
	s.reg = NewTaskRegistry()
	s.reg.put("abc", &taskEntry{wake: streamWake(cw.write)})

	s.WakeStdin("abc")

	if got := cw.writeCount(); got != 1 {
		t.Fatalf("writeCount = %d, want 1 (one whole line)", got)
	}
	line := cw.writeAt(0)
	if !strings.HasSuffix(line, "\n") || strings.Count(line, "\n") != 1 {
		t.Fatalf("write is not exactly one line: %q", line)
	}
	m, err := streamagent.DecodeMsg([]byte(strings.TrimSuffix(line, "\n")))
	if err != nil {
		t.Fatalf("not a protocol line: %v (%q)", err, line)
	}
	if m.Kind != streamagent.KindUser || m.User == nil || m.User.Text != wakeMarker {
		t.Errorf("got %+v, want a user turn carrying the marker", m)
	}
}
