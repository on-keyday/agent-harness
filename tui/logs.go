package tui

import (
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"
)

// clipLine returns the substring of line whose display rendering starts
// at column `from` and is at most `width` cells wide. Honors UTF-8 and
// east-asian width (e.g. Japanese chars are 2 cells). When `from` lands
// in the middle of a wide rune, the rune is skipped entirely (we never
// emit a partial wide rune). If line is shorter than `from`, returns "".
func clipLine(line string, from, width int) string {
	if width <= 0 {
		return ""
	}
	skipped := 0
	rest := line
	for skipped < from && len(rest) > 0 {
		r, size := utf8.DecodeRuneInString(rest)
		w := runewidth.RuneWidth(r)
		skipped += w
		rest = rest[size:]
	}
	if len(rest) == 0 {
		return ""
	}
	consumed := 0
	var b strings.Builder
	for len(rest) > 0 {
		r, size := utf8.DecodeRuneInString(rest)
		w := runewidth.RuneWidth(r)
		if consumed+w > width {
			break
		}
		b.WriteString(rest[:size])
		rest = rest[size:]
		consumed += w
	}
	return b.String()
}

type LogsModel struct {
	vp      viewport.Model
	taskID  string
	lines   []string
	focused bool
	// stickToBottom is true when the user has not manually scrolled away from
	// the tail. New chunks/Prepends auto-scroll to bottom in that mode. Once
	// the user scrolls up (with arrows / PgUp / k), it flips to false so live
	// chunks don't yank the view back to the bottom.
	stickToBottom bool
	// hOffset is how many bytes of each line to skip from the left.
	// Adjusted via ←/→ when the panel is focused. byte-based, not rune-aware.
	hOffset int
}

// hScrollStep is the per-keypress horizontal scroll distance, in display cells.
const hScrollStep = 16

func NewLogs() LogsModel {
	vp := viewport.New(80, 10)
	vp.SetContent("(no task selected)")
	return LogsModel{vp: vp, stickToBottom: true}
}

func (m *LogsModel) Focus() { m.focused = true }
func (m *LogsModel) Blur() {
	m.focused = false
}
func (m *LogsModel) IsFocused() bool { return m.focused }

func (m *LogsModel) SetSize(w, h int) {
	m.vp.Width = w
	m.vp.Height = h
	if len(m.lines) > 0 {
		m.applyContent()
	}
}

// Reset clears the viewport and sets the task ID we're following.
// taskID == "" means no task selected. Resets sticky-tail and h-scroll.
func (m *LogsModel) Reset(taskID string) {
	m.taskID = taskID
	m.lines = nil
	m.stickToBottom = true
	m.hOffset = 0
	if taskID == "" {
		m.vp.SetContent("(no task selected)")
	} else {
		m.vp.SetContent("(following " + taskID[:12] + "…)")
	}
}

// applyContent rebuilds the viewport content from m.lines, applying the
// current horizontal offset (in display cells) and clipping each logical
// line at the viewport width so the terminal does not soft-wrap onto the
// next row. Both ends are cell-aware so multibyte / east-asian-wide chars
// don't get sliced mid-codepoint.
func (m *LogsModel) applyContent() {
	full := strings.Join(m.lines, "")
	width := m.vp.Width
	if width <= 0 {
		width = 80
	}
	var b strings.Builder
	parts := strings.Split(full, "\n")
	for i, line := range parts {
		b.WriteString(clipLine(line, m.hOffset, width))
		if i < len(parts)-1 {
			b.WriteByte('\n')
		}
	}
	m.vp.SetContent(b.String())
}

// TaskID returns which task we're currently following, or "" if none.
func (m *LogsModel) TaskID() string { return m.taskID }

// Append appends a chunk of bytes (already prefixed by the runner with [out]/[err]).
// Chunks may contain partial lines; we keep them as-is. When the user has
// scrolled up (stickToBottom == false), we don't yank the viewport back to
// the bottom — the new content is still in the buffer and will be visible
// when the user scrolls down.
func (m *LogsModel) Append(chunk []byte) {
	if m.taskID == "" {
		return
	}
	m.lines = append(m.lines, string(chunk))
	if len(m.lines) > 1000 {
		m.lines = m.lines[len(m.lines)-1000:]
	}
	m.applyContent()
	if m.stickToBottom {
		m.vp.GotoBottom()
	}
}

// Prepend inserts content before any already-appended live chunks. Used to
// fold the historical log file (fetched via GetTaskLog) in front of pubsub
// chunks that may have started arriving while the fetch was in flight.
func (m *LogsModel) Prepend(content []byte) {
	if m.taskID == "" || len(content) == 0 {
		return
	}
	m.lines = append([]string{string(content)}, m.lines...)
	if len(m.lines) > 1000 {
		m.lines = m.lines[len(m.lines)-1000:]
	}
	m.applyContent()
	if m.stickToBottom {
		m.vp.GotoBottom()
	}
}

// Update forwards key/mouse events to the embedded viewport when focused.
// Intercepts ←/→ for horizontal scroll (byte-step hScrollStep). Other keys
// (↑/↓/PgUp/PgDn/g/G) are handled by viewport for vertical scroll.
// We also update stickToBottom: any user-initiated scroll that takes us
// off the bottom flips it false; returning to the bottom (e.g. End / G)
// flips it true again so live chunks resume tailing.
func (m LogsModel) Update(msg tea.Msg) (LogsModel, tea.Cmd) {
	if !m.focused {
		return m, nil
	}
	if k, ok := msg.(tea.KeyMsg); ok {
		switch k.Type {
		case tea.KeyLeft:
			if m.hOffset > 0 {
				m.hOffset -= hScrollStep
				if m.hOffset < 0 {
					m.hOffset = 0
				}
				m.applyContent()
			}
			return m, nil
		case tea.KeyRight:
			m.hOffset += hScrollStep
			m.applyContent()
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	m.stickToBottom = m.vp.AtBottom()
	return m, cmd
}

func (m LogsModel) View() string { return m.vp.View() }
