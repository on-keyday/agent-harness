package tui

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// A tea.Program that was created but never Run has an unbuffered msgs channel
// with nobody selecting on it — the same state the event loop is in for the
// whole of a tea.Exec suspension (an attached session, an external editor).
// This is the situation the handler must survive, not the drained one.
func unrunProgram() *tea.Program { return tea.NewProgram(New(Config{})) }

// The trsf run loop logs through slog.Default(), which cmd/harness-tui sets to
// this handler. When a direct program.Send parked that goroutine, the whole
// stream plane stopped: attach froze and freshly opened streams never became
// visible. Handle must return regardless of whether the UI is draining.
func TestSlogTailHandlerHandleDoesNotBlockWhileUISuspended(t *testing.T) {
	h := NewSlogTailHandler(slog.LevelInfo)
	h.BindProgram(unrunProgram())
	log := slog.New(h)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < tailChanCap*4; i++ {
			log.Error("received cancel for unknown stream", "stream_id", i)
		}
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("slog.Error blocked while the tea.Program was not draining msgs")
	}

	// More records than the handoff can hold, with the UI suspended, must be
	// dropped rather than queued without bound — and counted, so the drop is
	// announced instead of silently swallowing the errors.
	if h.dropped.Load() == 0 {
		t.Errorf("expected overflow records to be counted as dropped, got 0")
	}
}

// Records emitted before BindProgram still have to survive to the drain, since
// early-startup dial/handshake errors are logged before the program exists.
func TestSlogTailHandlerBuffersUntilBindProgram(t *testing.T) {
	h := NewSlogTailHandler(slog.LevelInfo)
	slog.New(h).Error("pre-bind failure")

	h.mu.Lock()
	n := len(h.buf)
	h.mu.Unlock()
	if n != 1 {
		t.Fatalf("pre-bind buffer len = %d, want 1", n)
	}

	h.BindProgram(unrunProgram())

	h.mu.Lock()
	n = len(h.buf)
	ch := h.ch
	h.mu.Unlock()
	if n != 0 {
		t.Errorf("buffer not drained by BindProgram: len = %d", n)
	}
	if ch == nil {
		t.Fatal("BindProgram did not create the handoff channel")
	}
}

// Below-level records must not even be formatted, let alone dispatched.
func TestSlogTailHandlerLevelFilter(t *testing.T) {
	h := NewSlogTailHandler(slog.LevelInfo)
	log := slog.New(h)
	log.Debug("trsf chatter")

	h.mu.Lock()
	n := len(h.buf)
	h.mu.Unlock()
	if n != 0 {
		t.Errorf("debug record was buffered despite LevelInfo filter: len = %d", n)
	}
}

// recorderModel quits as soon as it sees the LogTailMsg it is waiting for, so
// the test fails by timeout if the pump never delivers.
type recorderModel struct {
	want string
	got  chan string
}

func (m recorderModel) Init() tea.Cmd { return nil }
func (m recorderModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if lm, ok := msg.(LogTailMsg); ok && strings.Contains(lm.Line, m.want) {
		select {
		case m.got <- lm.Line:
		default:
		}
		return m, tea.Quit
	}
	return m, nil
}
func (m recorderModel) View() string { return "" }

// The point of the handoff is decoupling, not discarding: once the UI is
// draining again, records must still arrive. Without this the pump could be
// silently dead and every other test here would still pass.
func TestSlogTailHandlerDeliversOnceUIDrains(t *testing.T) {
	m := recorderModel{want: "delivered through the pump", got: make(chan string, 1)}
	p := tea.NewProgram(m, tea.WithInput(nil), tea.WithOutput(io.Discard))

	h := NewSlogTailHandler(slog.LevelInfo)
	h.BindProgram(p)
	slog.New(h).Error("delivered through the pump")

	done := make(chan error, 1)
	go func() { _, err := p.Run(); done <- err }()

	select {
	case line := <-m.got:
		if !strings.Contains(line, "ERROR") {
			t.Errorf("delivered line lost its level: %q", line)
		}
	case <-time.After(10 * time.Second):
		p.Kill()
		t.Fatal("record never reached the program: the pump is not delivering")
	}
	<-done
}
