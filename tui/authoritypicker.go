package tui

import (
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// AuthorityPickerModel is the selection UI behind re-granting a task's
// authority (tasks-pane `a`) and editing the session-default authority
// (`caps` / `scope` with no argument). It is one flat scrollable list —
// capability rows, one base row, task rows, and (re-grant only)
// cascade/keep-conns rows — so there is no intra-popup focus management.
// On apply the scope selection is serialized to the same `--scope` grammar
// string the typed forms use; nothing downstream changes.
type AuthorityPickerModel struct {
	open      bool
	mode      AuthorityPickerMode
	targetHex string

	rows   []pickerRow
	cursor int

	caps      protocol.Capability
	base      protocol.ScopeBase
	selected  map[string]bool
	cascade   bool
	keepConns bool

	w, h int
}

type AuthorityPickerMode int

const (
	// PickerModeRegrant edits a live task's caps+scope (plus cascade /
	// keep-conns) and applies via set_caps.
	PickerModeRegrant AuthorityPickerMode = iota
	// PickerModeSession edits the session-default caps+scope for spawns.
	PickerModeSession
)

type pickerRowKind int

const (
	rowCap pickerRowKind = iota
	rowBase
	rowTask
	rowCascade
	rowKeepConns
)

type pickerRow struct {
	kind  pickerRowKind
	label string
	bit   protocol.Capability // rowCap
	idHex string              // rowTask
}

// OpenRegrant opens the picker on a target task, prefilled from its stored
// authority. tasks is the full task-table snapshot; the target row itself is
// omitted — {self} is unconditionally in scope, so listing it is noise.
func (m *AuthorityPickerModel) OpenRegrant(target protocol.TaskInfo, tasks []protocol.TaskInfo) {
	m.reset(PickerModeRegrant, target.Capabilities, target.Scope)
	m.targetHex = FormatTaskID(target.Id)
	m.buildRows(tasks)
}

// OpenSession opens the picker prefilled with the session defaults.
func (m *AuthorityPickerModel) OpenSession(caps protocol.Capability, scope protocol.TaskScope, tasks []protocol.TaskInfo) {
	m.reset(PickerModeSession, caps, scope)
	m.buildRows(tasks)
}

func (m *AuthorityPickerModel) reset(mode AuthorityPickerMode, caps protocol.Capability, scope protocol.TaskScope) {
	m.open = true
	m.mode = mode
	m.targetHex = ""
	m.cursor = 0
	m.caps = caps
	m.base = scope.Base
	m.selected = map[string]bool{}
	for _, id := range scope.Ids {
		m.selected[FormatTaskID(id)] = true
	}
	m.cascade = false
	m.keepConns = false
}

func (m *AuthorityPickerModel) buildRows(tasks []protocol.TaskInfo) {
	m.rows = m.rows[:0]
	for _, c := range cli.CapsCatalog() {
		// The catalog includes the none/all pseudo entries; the picker
		// toggles real bits only.
		if c.Bit == 0 || protocol.Capability(c.Bit) == protocol.Capability_All {
			continue
		}
		m.rows = append(m.rows, pickerRow{kind: rowCap, label: c.Name, bit: protocol.Capability(c.Bit)})
	}
	m.rows = append(m.rows, pickerRow{kind: rowBase, label: "base:"})
	for _, t := range tasks {
		idHex := FormatTaskID(t.Id)
		if idHex == m.targetHex {
			continue
		}
		label := idHex[:8] + " " + taskStatusStr(t.Status) + " " + string(t.AgentProfile)
		if p := string(t.Prompt); p != "" {
			label += " " + runewidth.Truncate(p, 24, "…")
		}
		m.rows = append(m.rows, pickerRow{kind: rowTask, label: label, idHex: idHex})
	}
	if m.mode == PickerModeRegrant {
		m.rows = append(m.rows, pickerRow{kind: rowCascade, label: "--cascade (clamp descendants too)"})
		m.rows = append(m.rows, pickerRow{kind: rowKeepConns, label: "--keep-conns (do not drop their connections)"})
	}
}

func (m *AuthorityPickerModel) IsOpen() bool { return m.open }

func (m *AuthorityPickerModel) Close() { m.open = false }

func (m *AuthorityPickerModel) Mode() AuthorityPickerMode { return m.mode }

// TargetID is the re-grant target's hex id, "" in session mode.
func (m *AuthorityPickerModel) TargetID() string { return m.targetHex }

// Move moves the cursor by delta, wrapping, skipping disabled rows (task
// rows while base == global — the grammar has no global+ids form).
func (m *AuthorityPickerModel) Move(delta int) {
	if len(m.rows) == 0 {
		return
	}
	step := 1
	if delta < 0 {
		step = -1
		delta = -delta
	}
	for i := 0; i < delta; i++ {
		for {
			m.cursor = (m.cursor + step + len(m.rows)) % len(m.rows)
			if !m.rowDisabled(m.rows[m.cursor]) {
				break
			}
		}
	}
}

func (m *AuthorityPickerModel) rowDisabled(r pickerRow) bool {
	return r.kind == rowTask && m.base == protocol.ScopeBase_Global
}

// SetAllCaps sets every granular capability bit on (all=true) or off
// (all=false) — the TUI counterpart of the WebUI chip row's [all] / [none]
// quick-set buttons.
func (m *AuthorityPickerModel) SetAllCaps(all bool) {
	if !all {
		m.caps = protocol.Capability_None
		return
	}
	m.caps = protocol.Capability_None
	for _, r := range m.rows {
		if r.kind == rowCap {
			m.caps |= r.bit
		}
	}
}

// Toggle flips the current row; on the base row it cycles
// subtree → none → global → subtree.
func (m *AuthorityPickerModel) Toggle() {
	if len(m.rows) == 0 {
		return
	}
	switch r := m.rows[m.cursor]; r.kind {
	case rowCap:
		m.caps ^= r.bit
	case rowBase:
		switch m.base {
		case protocol.ScopeBase_Subtree:
			m.base = protocol.ScopeBase_None
		case protocol.ScopeBase_None:
			m.base = protocol.ScopeBase_Global
		default:
			m.base = protocol.ScopeBase_Subtree
		}
	case rowTask:
		m.selected[r.idHex] = !m.selected[r.idHex]
	case rowCascade:
		m.cascade = !m.cascade
	case rowKeepConns:
		m.keepConns = !m.keepConns
	}
}

// Result returns the selection, with the scope serialized to the grammar
// string. Re-grant specs are always explicit (a re-grant apply sends both
// fields); session mode returns "" for base-subtree-no-ids, the spawn
// default. Selected ids are ignored under base == global.
func (m *AuthorityPickerModel) Result() (caps protocol.Capability, scopeSpec string, cascade, keepConns bool) {
	var ids []string
	if m.base != protocol.ScopeBase_Global {
		for id, on := range m.selected {
			if on {
				ids = append(ids, id)
			}
		}
	}
	spec := scopeSpecFor(m.base, ids, m.mode == PickerModeSession)
	if m.mode == PickerModeSession {
		return m.caps, spec, false, false
	}
	return m.caps, spec, m.cascade, m.keepConns
}

// scopeSpecFor serializes a base + id set to the `--scope` grammar.
func scopeSpecFor(base protocol.ScopeBase, ids []string, sessionMode bool) string {
	sort.Strings(ids)
	switch base {
	case protocol.ScopeBase_Global:
		return "global"
	case protocol.ScopeBase_None:
		if len(ids) == 0 {
			return "none"
		}
		return "ids:" + strings.Join(ids, ",")
	default: // subtree
		if len(ids) == 0 {
			if sessionMode {
				return "" // the spawn default
			}
			return "subtree"
		}
		return "subtree+ids:" + strings.Join(ids, ",")
	}
}

func (m *AuthorityPickerModel) SetSize(w, h int) { m.w, m.h = w, h }

var (
	pickerBorderStyle   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
	pickerCursorStyle   = lipgloss.NewStyle().Reverse(true)
	pickerDisabledStyle = lipgloss.NewStyle().Faint(true)
)

// View renders the popup: title, a scroll window of rows following the
// cursor, and a footer with the key hints plus the grammar echo.
func (m *AuthorityPickerModel) View() string {
	if !m.open {
		return ""
	}
	title := "session default authority"
	if m.mode == PickerModeRegrant {
		title = "re-grant " + m.targetHex[:8]
	}

	// Rows visible: popup height minus title + footer (2 lines each side of
	// the border are added by lipgloss).
	visible := m.h - 4
	if visible < 3 {
		visible = 3
	}
	start := 0
	if m.cursor >= visible {
		start = m.cursor - visible + 1
	}
	end := start + visible
	if end > len(m.rows) {
		end = len(m.rows)
	}

	var b strings.Builder
	b.WriteString(title + "\n")
	for i := start; i < end; i++ {
		r := m.rows[i]
		line := "[ ] "
		switch r.kind {
		case rowCap:
			if m.caps&r.bit == r.bit {
				line = "[x] "
			}
		case rowBase:
			line = "    "
		case rowTask:
			if m.selected[r.idHex] {
				line = "[x] "
			}
		case rowCascade:
			if m.cascade {
				line = "[x] "
			}
		case rowKeepConns:
			if m.keepConns {
				line = "[x] "
			}
		}
		label := r.label
		if r.kind == rowBase {
			label = "base: " + cli.ScopeLabel(protocol.TaskScope{Base: m.base})
		}
		line += label
		maxW := m.w - 6
		if maxW > 8 {
			line = runewidth.Truncate(line, maxW, "…")
		}
		switch {
		case i == m.cursor:
			line = pickerCursorStyle.Render(line)
		case m.rowDisabled(r):
			line = pickerDisabledStyle.Render(line)
		}
		b.WriteString(line + "\n")
	}

	_, spec, _, _ := m.Result()
	if spec == "" {
		spec = "(default subtree)"
	}
	footer := "space toggle · A/N all/none caps · enter apply · esc cancel · scope=" + spec
	maxW := m.w - 6
	if maxW > 8 {
		footer = runewidth.Truncate(footer, maxW, "…")
	}
	b.WriteString(footer)
	return pickerBorderStyle.Render(b.String())
}
