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
	// hOffset is how many display cells to skip from the left of each line.
	// Adjusted via ←/→ (cells) and Shift+←/→ (one viewport-width page).
	hOffset int

	// Substring filter applied to each logical line. Empty means "show all".
	// During interactive editing (after `/`), filterDraft is used for live
	// preview while filter holds the previously-committed pattern.
	filter         string
	filterDraft    string
	editingFilter  bool
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

// visibleLines returns the logical lines after splitting the chunk buffer
// on '\n' and applying the active filter. While the user is editing the
// filter (after `/`), filterDraft is the active filter so the user gets a
// live preview; otherwise the committed filter is used. Empty filter →
// everything passes through.
func (m *LogsModel) visibleLines() []string {
	full := strings.Join(m.lines, "")
	parts := strings.Split(full, "\n")
	pat := m.filter
	if m.editingFilter {
		pat = m.filterDraft
	}
	if pat == "" {
		return parts
	}
	kept := make([]string, 0, len(parts))
	for _, line := range parts {
		if strings.Contains(line, pat) {
			kept = append(kept, line)
		}
	}
	return kept
}

// maxHOffset returns the largest useful hOffset for the *currently visible*
// content + viewport width: scrolling past it would slide the longest
// visible line entirely off the left edge with no new content appearing
// on the right. Cell-aware; honors the active filter.
func (m *LogsModel) maxHOffset() int {
	width := m.vp.Width
	if width <= 0 {
		width = 80
	}
	maxCells := 0
	for _, line := range m.visibleLines() {
		w := runewidth.StringWidth(line)
		if w > maxCells {
			maxCells = w
		}
	}
	if maxCells <= width {
		return 0
	}
	return maxCells - width
}

// applyContent rebuilds the viewport content from the currently visible
// lines (post-filter), applying the current horizontal offset (in display
// cells) and clipping each logical line at the viewport width so the
// terminal does not soft-wrap onto the next row. Both ends are cell-aware
// so multibyte / east-asian-wide chars don't get sliced mid-codepoint.
// hOffset is clamped to the max so that content shrinking (e.g. a tighter
// filter producing only short lines) doesn't leave the viewport stuck at
// an empty offset.
func (m *LogsModel) applyContent() {
	width := m.vp.Width
	if width <= 0 {
		width = 80
	}
	if max := m.maxHOffset(); m.hOffset > max {
		m.hOffset = max
	}
	parts := m.visibleLines()
	var b strings.Builder
	for i, line := range parts {
		b.WriteString(clipLine(line, m.hOffset, width))
		if i < len(parts)-1 {
			b.WriteByte('\n')
		}
	}
	m.vp.SetContent(b.String())
}

// Filter returns the committed substring filter. Empty means "show all".
func (m *LogsModel) Filter() string { return m.filter }

// FilterDraft returns the in-progress draft pattern while editingFilter
// is true. When not editing, returns "".
func (m *LogsModel) FilterDraft() string { return m.filterDraft }

// IsEditingFilter reports whether the user is currently typing a filter
// pattern after pressing `/`. The app footer uses this to render the
// `/<draft>_` prompt.
func (m *LogsModel) IsEditingFilter() bool { return m.editingFilter }

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
// Intercepts:
//   - ←/→  : horizontal scroll by hScrollStep cells
//   - Shift+←/→ : horizontal page jump (one viewport-width)
//   - 0 / $ : jump to leftmost / rightmost edge (vim-style)
//   - /    : enter filter edit mode (live preview while typing)
//   - Esc  : clear committed filter (when not editing)
// While editingFilter is true, all printable runes feed filterDraft;
// Enter commits, Esc cancels, Backspace trims one rune (UTF-8 aware).
// Other keys (↑/↓/PgUp/PgDn/g/G) are handled by the viewport for vertical
// scroll. stickToBottom is updated on each viewport pass: any user
// scroll that leaves the bottom flips it false; returning (End / G)
// flips it true again so live chunks resume tailing.
func (m LogsModel) Update(msg tea.Msg) (LogsModel, tea.Cmd) {
	if !m.focused {
		return m, nil
	}
	if k, ok := msg.(tea.KeyMsg); ok {
		// Edit mode: capture printable input + control keys for the draft.
		// We deliberately swallow everything else so the viewport doesn't
		// react to e.g. PgDn while the user is composing a pattern.
		if m.editingFilter {
			switch k.Type {
			case tea.KeyEnter:
				m.filter = m.filterDraft
				m.editingFilter = false
				m.applyContent()
				if m.stickToBottom {
					m.vp.GotoBottom()
				}
				return m, nil
			case tea.KeyEsc:
				m.filterDraft = ""
				m.editingFilter = false
				m.applyContent()
				return m, nil
			case tea.KeyBackspace:
				if len(m.filterDraft) > 0 {
					_, size := utf8.DecodeLastRuneInString(m.filterDraft)
					m.filterDraft = m.filterDraft[:len(m.filterDraft)-size]
					m.applyContent()
				}
				return m, nil
			case tea.KeySpace:
				m.filterDraft += " "
				m.applyContent()
				return m, nil
			case tea.KeyRunes:
				m.filterDraft += string(k.Runes)
				m.applyContent()
				return m, nil
			}
			return m, nil
		}
		// Normal mode.
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
			max := m.maxHOffset()
			if m.hOffset >= max {
				return m, nil
			}
			m.hOffset += hScrollStep
			if m.hOffset > max {
				m.hOffset = max
			}
			m.applyContent()
			return m, nil
		case tea.KeyShiftLeft:
			page := m.vp.Width
			if page <= 0 {
				page = 80
			}
			m.hOffset -= page
			if m.hOffset < 0 {
				m.hOffset = 0
			}
			m.applyContent()
			return m, nil
		case tea.KeyShiftRight:
			page := m.vp.Width
			if page <= 0 {
				page = 80
			}
			max := m.maxHOffset()
			if m.hOffset >= max {
				return m, nil
			}
			m.hOffset += page
			if m.hOffset > max {
				m.hOffset = max
			}
			m.applyContent()
			return m, nil
		case tea.KeyEsc:
			if m.filter != "" {
				m.filter = ""
				m.applyContent()
			}
			return m, nil
		case tea.KeyRunes:
			if len(k.Runes) == 1 {
				switch k.Runes[0] {
				case '/':
					m.editingFilter = true
					m.filterDraft = m.filter
					m.applyContent()
					return m, nil
				case '0':
					if m.hOffset != 0 {
						m.hOffset = 0
						m.applyContent()
					}
					return m, nil
				case '$':
					max := m.maxHOffset()
					if m.hOffset != max {
						m.hOffset = max
						m.applyContent()
					}
					return m, nil
				}
			}
		}
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	m.stickToBottom = m.vp.AtBottom()
	return m, cmd
}

func (m LogsModel) View() string { return m.vp.View() }
