package runner

import (
	"errors"
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
