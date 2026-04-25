package tui

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// LogTailMsg is dispatched into the tea.Program for each slog record.
// app.go renders it into the cmdresult panel with a dim "[log]" prefix.
type LogTailMsg struct {
	Line string
}

// SlogTailHandler is a slog.Handler that forwards each record as LogTailMsg
// to the bound tea.Program AND optionally mirrors the formatted line to an
// io.Writer (typically a debug log file). Before BindProgram is called the
// records are buffered up to bufCap entries; BindProgram drains them and
// switches to direct dispatch.
type SlogTailHandler struct {
	mu      sync.Mutex
	program *tea.Program
	mirror  io.Writer // optional; nil means no mirror
	buf     []string
	bufCap  int
	level   slog.Level
}

// NewSlogTailHandler creates a handler that filters records below `level`.
func NewSlogTailHandler(level slog.Level) *SlogTailHandler {
	return &SlogTailHandler{level: level, bufCap: 256}
}

// SetMirror enables mirroring of every formatted record to w. Pass nil to
// disable. Safe to call before or after BindProgram.
func (h *SlogTailHandler) SetMirror(w io.Writer) {
	h.mu.Lock()
	h.mirror = w
	h.mu.Unlock()
}

func (h *SlogTailHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *SlogTailHandler) Handle(_ context.Context, r slog.Record) error {
	line := formatSlogRecord(r)
	h.mu.Lock()
	prog := h.program
	mirror := h.mirror
	if prog == nil {
		if len(h.buf) >= h.bufCap {
			h.buf = h.buf[1:]
		}
		h.buf = append(h.buf, line)
	}
	h.mu.Unlock()
	if mirror != nil {
		_, _ = io.WriteString(mirror, line+"\n")
	}
	if prog != nil {
		prog.Send(LogTailMsg{Line: line})
	}
	return nil
}

func (h *SlogTailHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *SlogTailHandler) WithGroup(_ string) slog.Handler      { return h }

// BindProgram drains buffered records into the program and switches to
// direct dispatch. Safe to call multiple times.
func (h *SlogTailHandler) BindProgram(p *tea.Program) {
	h.mu.Lock()
	h.program = p
	drain := h.buf
	h.buf = nil
	h.mu.Unlock()
	for _, line := range drain {
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
