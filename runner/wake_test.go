package runner

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
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
	// One fire emits two writes: the marker text, then a lone Enter byte
	// after wakeSubmitDelay. The split is required because Ink-based
	// claude code TUI treats a single combined write as paste content
	// (trailing Enter becomes literal newline) — see session.go comments.
	s := &Session{Now: time.Now}
	cw := &captureWriter{}
	s.mu.Lock()
	s.tasks = map[string]*taskEntry{
		"abc": {wakeWrite: cw.write},
	}
	s.mu.Unlock()

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
	s.mu.Lock()
	s.tasks = map[string]*taskEntry{
		"abc": {wakeWrite: cw.write},
	}
	s.mu.Unlock()

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
	s.mu.Lock()
	s.tasks = map[string]*taskEntry{
		"abc": {wakeWrite: cw.write},
	}
	s.mu.Unlock()

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
	s.mu.Lock()
	s.tasks = map[string]*taskEntry{}
	s.mu.Unlock()
	// Should not panic on unknown task.
	s.WakeStdin("missing")
}

func TestSession_WakeStdin_TextWriteError_DoesNotAdvanceCursor(t *testing.T) {
	// When the first (text) write fails, the submit byte is not sent,
	// and lastWakeAt is not advanced — so a follow-up call within the
	// debounce window can still try.
	s := &Session{Now: time.Now}
	cw := &captureWriter{failNext: errors.New("pipe closed")}
	s.mu.Lock()
	s.tasks = map[string]*taskEntry{
		"abc": {wakeWrite: cw.write},
	}
	s.mu.Unlock()

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
			setup: func(s *Session, _ *captureWriter) { s.tasks = map[string]*taskEntry{} },
			task:  "missing",
			want:  "task not known to this runner",
		},
		{
			name: "no stdin writer",
			setup: func(s *Session, _ *captureWriter) {
				s.tasks = map[string]*taskEntry{"abc": {}}
			},
			task: "abc",
			want: "no stdin writer",
		},
		{
			name: "debounced",
			setup: func(s *Session, cw *captureWriter) {
				s.tasks = map[string]*taskEntry{
					// Inside wakeDebounceWindow, so the second fire is dropped.
					"abc": {wakeWrite: cw.write, lastWakeAt: s.Now()},
				}
			},
			task: "abc",
			want: "debounced",
		},
		{
			name: "written",
			setup: func(s *Session, cw *captureWriter) {
				s.tasks = map[string]*taskEntry{"abc": {wakeWrite: cw.write}}
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
