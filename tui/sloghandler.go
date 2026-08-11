package tui

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// LogTailMsg is dispatched into the tea.Program for each slog record.
// app.go renders it into the cmdresult panel with a dim "[log]" prefix.
type LogTailMsg struct {
	Line string
}

// tailChanCap bounds the handoff between record production and program.Send.
const tailChanCap = 256

// SlogTailHandler is a slog.Handler that forwards each record as LogTailMsg.
// Before BindProgram is called (e.g. during early startup), records are
// buffered up to bufCap entries; BindProgram drains them and switches to
// dispatch through a single goroutine.
//
// Handle NEVER blocks its caller. bubbletea's program.Send writes to an
// UNBUFFERED msgs channel (tea.go) and the event loop runs tea.Exec — an
// attached session, or an external editor — SYNCHRONOUSLY, so for the whole
// suspension nothing drains msgs and a direct Send parks its caller until the
// user comes back. The callers here are arbitrary background goroutines:
// cli.Client dials with slog.Default() (this handler), peer.Dial hands that
// logger to trsf.NewStreams, so the trsf run loop logs through here. A single
// Error-level record from trsf's packet demux during an attach therefore froze
// the entire stream plane — attach stopped rendering and every newly opened
// stream stayed invisible ("stream N not visible"), because a stream only
// materializes when the run loop demuxes its first frame — while the objproto
// control plane (TaskControl RPC, ping/pong) kept answering on its own
// goroutine, so nothing detected the stall and the connection was never
// dropped. Detaching released the parked Send, which is why re-attaching
// worked.
//
// Same shape as forwardStatusLogf (portforward.go): non-blocking send onto a
// buffered channel, drop on overflow, one drain goroutine owning program.Send.
// Unlike cosmetic forward status, dropped records are counted and announced
// once the UI comes back, so a suspension cannot silently eat the very errors
// that explain the next stall.
type SlogTailHandler struct {
	mu      sync.Mutex
	program *tea.Program
	buf     []string
	bufCap  int
	level   slog.Level

	// ch is nil until BindProgram; while nil, records go to buf instead.
	ch      chan string
	dropped atomic.Uint64
}

// NewSlogTailHandler creates a handler that filters records below `level`.
func NewSlogTailHandler(level slog.Level) *SlogTailHandler {
	return &SlogTailHandler{level: level, bufCap: 256}
}

func (h *SlogTailHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *SlogTailHandler) Handle(_ context.Context, r slog.Record) error {
	line := formatSlogRecord(r)
	h.mu.Lock()
	ch := h.ch
	if ch == nil { // not bound yet — hold for the BindProgram drain
		if len(h.buf) >= h.bufCap {
			h.buf = h.buf[1:]
		}
		h.buf = append(h.buf, line)
		h.mu.Unlock()
		return nil
	}
	h.mu.Unlock()
	select {
	case ch <- line:
	default: // UI suspended (tea.Exec) and the handoff is full
		h.dropped.Add(1)
	}
	return nil
}

func (h *SlogTailHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *SlogTailHandler) WithGroup(_ string) slog.Handler      { return h }

// BindProgram drains buffered records into the program and switches to
// dispatch through the pump goroutine. Safe to call multiple times; the pump
// re-reads the current program each record, so a rebind takes effect without
// starting a second pump.
func (h *SlogTailHandler) BindProgram(p *tea.Program) {
	h.mu.Lock()
	h.program = p
	drain := h.buf
	h.buf = nil
	if h.ch == nil {
		h.ch = make(chan string, tailChanCap)
		go h.pump()
	}
	ch := h.ch
	h.mu.Unlock()
	for _, line := range drain {
		select {
		case ch <- line:
		default:
			h.dropped.Add(1)
		}
	}
}

// pump is the only goroutine that calls program.Send. It blocks for the whole
// of a tea.Exec suspension; that is expected and harmless, because producers
// hand off to ch without waiting for it.
func (h *SlogTailHandler) pump() {
	for line := range h.ch {
		h.mu.Lock()
		p := h.program
		h.mu.Unlock()
		if p == nil {
			continue
		}
		if n := h.dropped.Swap(0); n > 0 {
			p.Send(LogTailMsg{Line: fmt.Sprintf("... %d log line(s) dropped while the UI was suspended", n)})
		}
		p.Send(LogTailMsg{Line: line})
	}
}

func formatSlogRecord(r slog.Record) string {
	ts := r.Time.Format(time.Kitchen)
	level := r.Level.String()
	out := fmt.Sprintf("%s %s %s", ts, level, r.Message)
	r.Attrs(func(a slog.Attr) bool {
		out += fmt.Sprintf(" %s=%v", a.Key, a.Value.Any())
		return true
	})
	return out
}
