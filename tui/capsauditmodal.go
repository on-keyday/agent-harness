package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// auditLine is one line of the caps audit modal: a capability, or (holder !=
// nil) one of its holders under an expanded capability.
type auditLine struct {
	cap    protocol.Capability
	holder *cli.AuditHolder
}

// CapsAuditModal is the TUI face of `caps audit`: one line per capability,
// strongest first, from cli.CapsAudit. Opened with `i` or the `caps audit`
// command; enter expands a capability or, on a holder, hands the task to the
// re-grant picker.
type CapsAuditModal struct {
	open                       bool
	tasks                      []protocol.TaskInfo
	includeEnded, includeEmpty bool
	expanded                   map[protocol.Capability]bool
	rows                       []cli.AuditRow
	lines                      []auditLine
	cursor                     int
	width, height              int
}

func NewCapsAuditModal() CapsAuditModal {
	return CapsAuditModal{expanded: map[protocol.Capability]bool{}}
}

func (m *CapsAuditModal) IsOpen() bool { return m.open }
func (m *CapsAuditModal) Close()       { m.open = false }

func (m *CapsAuditModal) SetSize(w, h int) { m.width, m.height = w, h }

// Open shows the audit of tasks. focus, when non-zero, starts with that
// capability expanded and under the cursor.
func (m *CapsAuditModal) Open(tasks []protocol.TaskInfo, includeEnded, includeEmpty bool, focus protocol.Capability) {
	m.open, m.includeEnded, m.includeEmpty = true, includeEnded, includeEmpty
	m.expanded = map[protocol.Capability]bool{}
	m.cursor = 0
	if focus != 0 {
		m.expanded[focus] = true
	}
	m.SetTasks(tasks)
	for i, l := range m.lines {
		if l.holder == nil && l.cap == focus {
			m.cursor = i
		}
	}
}

// SetTasks rebuilds from a fresh listing, keeping the cursor on the same line
// where it still exists.
func (m *CapsAuditModal) SetTasks(tasks []protocol.TaskInfo) {
	m.tasks = tasks
	m.rebuild()
}

func (m *CapsAuditModal) rebuild() {
	// Remember WHICH line the cursor is on, not its index: a holder ending
	// above it shifts every line below, and enter must not then land on a
	// different task.
	var keep auditLine
	var keepHolder string
	had := m.cursor < len(m.lines)
	if had {
		keep = m.lines[m.cursor]
		if keep.holder != nil {
			keepHolder = keep.holder.TaskID
		}
	}
	m.rows = cli.CapsAudit(m.tasks, m.includeEnded, m.includeEmpty)
	m.lines = m.lines[:0]
	for i := range m.rows {
		r := &m.rows[i]
		m.lines = append(m.lines, auditLine{cap: r.Cap})
		if m.expanded[r.Cap] {
			for j := range r.Holders {
				m.lines = append(m.lines, auditLine{cap: r.Cap, holder: &r.Holders[j]})
			}
		}
	}
	if had {
		// The same line if it is still there; else its capability's line, so
		// a holder that went never hands the cursor to its neighbour.
		capLine := -1
		found := false
		for i, l := range m.lines {
			if l.cap != keep.cap {
				continue
			}
			if l.holder == nil {
				capLine = i
			}
			if keepHolder == "" && l.holder == nil || l.holder != nil && l.holder.TaskID == keepHolder {
				m.cursor, found = i, true
				break
			}
		}
		if !found && capLine >= 0 {
			m.cursor = capLine
		}
	}
	if m.cursor >= len(m.lines) {
		m.cursor = len(m.lines) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	if had && m.cursor < len(m.lines) && m.lines[m.cursor].holder != nil &&
		m.lines[m.cursor].holder.TaskID != keepHolder {
		// Its whole capability folded away: back up to a capability line,
		// so enter never re-grants a task the cursor was not on.
		for m.cursor > 0 && m.lines[m.cursor].holder != nil {
			m.cursor--
		}
	}
}

// Move shifts the cursor by delta lines.
func (m *CapsAuditModal) Move(delta int) {
	m.cursor += delta
	if m.cursor >= len(m.lines) {
		m.cursor = len(m.lines) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

// Enter toggles the capability under the cursor, or returns the holder's task
// id when the cursor is on a holder.
func (m *CapsAuditModal) Enter() (taskID string, ok bool) {
	if m.cursor >= len(m.lines) {
		return "", false
	}
	l := m.lines[m.cursor]
	if l.holder != nil {
		return l.holder.TaskID, true
	}
	m.expanded[l.cap] = !m.expanded[l.cap]
	m.rebuild()
	return "", false
}

// SelectedCap is the capability of the line under the cursor, a bit line or
// one of its holders alike.
func (m *CapsAuditModal) SelectedCap() (protocol.Capability, bool) {
	if m.cursor >= len(m.lines) {
		return 0, false
	}
	return m.lines[m.cursor].cap, true
}

func (m *CapsAuditModal) ToggleEnded() { m.includeEnded = !m.includeEnded; m.rebuild() }
func (m *CapsAuditModal) ToggleEmpty() { m.includeEmpty = !m.includeEmpty; m.rebuild() }

func (m CapsAuditModal) View() string {
	which := "live tasks"
	if m.includeEnded {
		which = "live and ended tasks"
	}
	// The frame's width is the terminal's, never the content's: expanding a
	// bit lengthens lines, and each is cut to the inner width instead.
	inner := m.width - 4 // border + padding
	clip := lipgloss.NewStyle().MaxWidth(inner)
	header := clip.Render(HeaderStyle.Render("capabilities — who holds them (" + which + ")"))
	footer := clip.Render(FooterStyle.Render("enter: expand / show in tree · t: mark bit in tree · a: ended tasks · e: unheld bits · Esc: close"))
	box := PanelStyleFocused.Padding(0, 1).Width(m.width - 2)
	if len(m.lines) == 0 {
		return box.Render(header + "\n" + "no task holds a capability (a: include ended tasks)" + "\n" + footer)
	}
	var rows []string
	for i, l := range m.lines {
		var s string
		if l.holder == nil {
			s = m.capLine(l.cap)
		} else {
			s = "    " + l.holder.Label + "  " + auditScopeCell(*l.holder) + "  parent=" + l.holder.Parent + "  auth=" + l.holder.Authority
			if !l.holder.Live {
				s += " (ended)"
			}
		}
		if i == m.cursor {
			s = CursorStyle.Render(s)
		}
		rows = append(rows, clip.Render(s))
	}
	rows = visibleWindow(rows, m.cursor, m.height-6)
	return box.Render(header + "\n" + strings.Join(rows, "\n") + "\n" + footer)
}

func (m CapsAuditModal) capLine(c protocol.Capability) string {
	for _, r := range m.rows {
		if r.Cap != c {
			continue
		}
		mark := "▸"
		if m.expanded[c] {
			mark = "▾"
		}
		var ids []string
		for _, h := range r.Holders {
			ids = append(ids, h.TaskID[:8])
		}
		s := fmt.Sprintf("%s %-15s %s %2d  %s", mark, c.String(), r.Tier.Label(), len(r.Holders), strings.Join(ids, " "))
		if len(r.ImpliedBy) > 0 {
			var names []string
			for _, b := range r.ImpliedBy {
				names = append(names, b.String())
			}
			s += MutedStyle.Render("  (also: " + strings.Join(names, ", ") + ")")
		}
		return s
	}
	return c.String()
}

// auditScopeCell colours a holder's scope by breadth: red global, yellow
// subtree, green narrow. A bit no task scope bounds reaches the server's shared
// resources, so it is red too.
func auditScopeCell(h cli.AuditHolder) string {
	if h.ScopeKind == verb.ScopeNone {
		return ErrorStyle.Render("[" + cli.UnboundedScopeLabel + "]")
	}
	s := "[" + h.Scope
	if h.Override {
		s += "*"
	}
	s += "]"
	switch h.Breadth {
	case cli.BreadthGlobal:
		return ErrorStyle.Render(s)
	case cli.BreadthSubtree:
		return WarnStyle.Render(s)
	default:
		return OKStyle.Render(s)
	}
}

// visibleWindow keeps the cursor's line on screen when the list is taller
// than the space for it.
func visibleWindow(lines []string, cursor, room int) []string {
	if room <= 0 || len(lines) <= room {
		return lines
	}
	start := cursor - room/2
	if start < 0 {
		start = 0
	}
	if start+room > len(lines) {
		start = len(lines) - room
	}
	return lines[start : start+room]
}
