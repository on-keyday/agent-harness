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

func (c *captureWriter) lastWrite() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.writes) == 0 {
		return ""
	}
	return string(c.writes[len(c.writes)-1])
}

func (c *captureWriter) writeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.writes)
}

func TestSession_WakeStdin_WritesMarker(t *testing.T) {
	s := &Session{Now: time.Now}
	cw := &captureWriter{}
	s.mu.Lock()
	s.tasks = map[string]*taskEntry{
		"abc": {wakeWrite: cw.write},
	}
	s.mu.Unlock()

	s.WakeStdin("abc")

	if cw.lastWrite() != wakeMarker {
		t.Errorf("wrote %q, want %q", cw.lastWrite(), wakeMarker)
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

	if got := cw.writeCount(); got != 1 {
		t.Errorf("debounce broken: writeCount=%d, want 1", got)
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

	if got := cw.writeCount(); got != 2 {
		t.Errorf("post-window wake suppressed: writeCount=%d, want 2", got)
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

func TestSession_WakeStdin_WriteError_DoesNotAdvanceCursor(t *testing.T) {
	s := &Session{Now: time.Now}
	cw := &captureWriter{failNext: errors.New("pipe closed")}
	s.mu.Lock()
	s.tasks = map[string]*taskEntry{
		"abc": {wakeWrite: cw.write},
	}
	s.mu.Unlock()

	s.WakeStdin("abc") // fails — writeCount stays 0
	if cw.writeCount() != 0 {
		t.Errorf("expected failed write to not be recorded")
	}
	// Second call should still try (lastWakeAt not advanced because write
	// failed). It succeeds this time and writes once.
	s.WakeStdin("abc")
	if cw.writeCount() != 1 {
		t.Errorf("retry after failure: writeCount=%d, want 1", cw.writeCount())
	}
}
