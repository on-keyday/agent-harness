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
// capability rows, one base row, task rows, then the same controls again for
// the delegation bound, and (re-grant only) cascade/keep-conns rows — so
// there is no intra-popup focus management. On apply each scope selection is
// serialized to the same `--scope` grammar string the typed forms use;
// nothing downstream changes.
type AuthorityPickerModel struct {
	open      bool
	mode      AuthorityPickerMode
	targetHex string

	rows   []pickerRow
	cursor int

	// The task's own authority. Embedded, so the section's fields read as the
	// picker's own, as they did before the delegation section existed.
	pickerScope
	// deleg is the delegation bound, edited with the same controls: what the
	// task may hand to the tasks it creates. delegOpened is its value when
	// the picker opened, so a re-grant sends the bound only if it changed.
	deleg       pickerScope
	delegOpened string

	// overrides is CARRIED, not edited. The picker edits the base scope and
	// the id set; per-capability narrowings are typed on the cmdline. It has
	// to be kept because overrides travel with the scope under one presence
	// bit — a re-grant that returned an empty list would silently erase the
	// target's per-capability rules, which is the same defect the cascade had.
	overrides []protocol.ScopeOverride
	cascade   bool
	keepConns bool

	w, h int
}

// pickerScope is one caps + scope selection: the controls a section of the
// picker edits.
type pickerScope struct {
	caps protocol.Capability
	base protocol.ScopeBase
	// excludeSelf is the base's other half. The six scopes are three ranks
	// times this flag (subtree/descendants, none/none-self, global/global-self),
	// so as a fourth base value it could only ever have meant "subtree without
	// self" and the other two ranks could not say it at all.
	excludeSelf bool
	// The visibility half, EDITED — it used to be carried untouched because the
	// picker had no control for it, which kept a `global/subtree` task from
	// collapsing to `subtree` on apply but also meant three of the ranks could
	// only ever be typed on the cmdline.
	//
	// visBasePresent is a real third state, not a nil-check: an unstated rank
	// means "visibility follows the action base", and the zero ScopeBase is
	// itself a legal rank (subtree), so dropping the bit would promote every
	// default to an explicit rank the first time anyone opened the picker.
	visBase        protocol.ScopeBase
	visBasePresent bool
	// visSelected is the +vis-ids: set — tasks this one may SEE without being
	// able to act on them. Separate from `selected` because they are separate
	// clauses of the grammar; a task can be in either, both, or neither.
	visSelected map[string]bool
	selected    map[string]bool
}

// newPickerScope reads a wire selection into the controls' state.
func newPickerScope(caps protocol.Capability, scope protocol.TaskScope) pickerScope {
	s := pickerScope{
		caps: caps, base: scope.Base, excludeSelf: scope.ExcludeSelf(),
		visBase: scope.VisBase, visBasePresent: scope.VisBasePresent(),
		selected: map[string]bool{}, visSelected: map[string]bool{},
	}
	for _, id := range scope.Ids {
		s.selected[FormatTaskID(id)] = true
	}
	for _, id := range scope.VisIds {
		s.visSelected[FormatTaskID(id)] = true
	}
	return s
}

type AuthorityPickerMode int

const (
	// PickerModeRegrant edits a live task's caps+scope (plus cascade /
	// keep-conns) and applies via set_caps.
	PickerModeRegrant AuthorityPickerMode = iota
	// PickerModeSession edits the session-default caps+scope for spawns.
	PickerModeSession
	// PickerModeParent picks a new parent for a live task and applies via
	// set_parent. Single-choice: Enter applies the CURSOR row (root / swap /
	// a task); there is no toggling and no caps/base/cascade rows.
	PickerModeParent
)

type pickerRowKind int

const (
	rowCap pickerRowKind = iota
	rowBase
	rowExcludeSelf
	rowVisBase
	rowTask
	rowCascade
	rowKeepConns
	rowParentRoot // parent mode: detach to the operator root
	rowParentSwap // parent mode: swap with the current parent
	rowSection    // a heading; never selectable
)

type pickerRow struct {
	kind  pickerRowKind
	label string
	bit   protocol.Capability // rowCap
	idHex string              // rowTask
	// deleg marks a row of the delegation section: it edits the bound, not
	// the task's own authority.
	deleg bool
}

// section is the selection a row edits.
func (m *AuthorityPickerModel) section(r pickerRow) *pickerScope {
	if r.deleg {
		return &m.deleg
	}
	return &m.pickerScope
}

// OpenRegrant opens the picker on a target task, prefilled from its stored
// authority. tasks is the full task-table snapshot; the target row itself is
// omitted — {self} is unconditionally in scope, so listing it is noise.
func (m *AuthorityPickerModel) OpenRegrant(target protocol.TaskInfo, tasks []protocol.TaskInfo) {
	m.reset(PickerModeRegrant, target.Capabilities, target.Scope)
	m.setDeleg(target.DelegateCaps, target.DelegateScope)
	m.overrides = target.Overrides
	m.targetHex = FormatTaskID(target.Id)
	m.buildRows(tasks)
}

// OpenSession opens the picker prefilled with the session defaults.
func (m *AuthorityPickerModel) OpenSession(defaults Authority, tasks []protocol.TaskInfo) {
	m.reset(PickerModeSession, defaults.Caps, defaults.Scope)
	m.setDeleg(defaults.DelegateCaps, defaults.DelegateScope)
	m.overrides = defaults.Overrides
	m.buildRows(tasks)
}

func (m *AuthorityPickerModel) setDeleg(caps protocol.Capability, scope protocol.TaskScope) {
	m.deleg = newPickerScope(caps, scope)
	m.delegOpened = m.delegLabel()
}

// OpenParent opens the picker as a parent chooser for target: a detach row, a
// swap row when the target currently has a parent, then every other task as a
// candidate. Single-choice — Enter applies the cursor row via ParentChoice.
func (m *AuthorityPickerModel) OpenParent(target protocol.TaskInfo, tasks []protocol.TaskInfo) {
	m.reset(PickerModeParent, 0, protocol.TaskScope{})
	m.targetHex = FormatTaskID(target.Id)
	parentHex := ""
	if target.CreatorTaskId.Id != ([16]byte{}) {
		parentHex = FormatTaskID(target.CreatorTaskId)
	}
	m.rows = m.rows[:0]
	m.rows = append(m.rows, pickerRow{kind: rowParentRoot, label: "(root — detach)"})
	if parentHex != "" {
		m.rows = append(m.rows, pickerRow{kind: rowParentSwap,
			label: "(swap with " + parentHex[:8] + " — take its place, it becomes the child)"})
	}
	for _, t := range tasks {
		idHex := FormatTaskID(t.Id)
		if idHex == m.targetHex {
			continue
		}
		label := idHex[:8] + " " + taskStatusStr(t.Status) + " " + taskAgentCell(t) +
			" " + truncateLeft(string(t.RepoPath), 20)
		if p := string(t.Prompt); p != "" {
			label += " " + runewidth.Truncate(p, 24, "…")
		}
		if idHex == parentHex {
			label += "  ← current parent"
		}
		m.rows = append(m.rows, pickerRow{kind: rowTask, label: label, idHex: idHex})
	}
}

// ParentChoice reads the cursor row as the parent-mode selection. ok is false
// outside parent mode or on a row kind that cannot occur there.
func (m *AuthorityPickerModel) ParentChoice() (parentHex string, detach, swap, ok bool) {
	if m.mode != PickerModeParent || len(m.rows) == 0 {
		return "", false, false, false
	}
	switch r := m.rows[m.cursor]; r.kind {
	case rowParentRoot:
		return "", true, false, true
	case rowParentSwap:
		return "", false, true, true
	case rowTask:
		return r.idHex, false, false, true
	default:
		return "", false, false, false
	}
}

func (m *AuthorityPickerModel) reset(mode AuthorityPickerMode, caps protocol.Capability, scope protocol.TaskScope) {
	m.open = true
	m.mode = mode
	m.targetHex = ""
	m.cursor = 0
	m.pickerScope = newPickerScope(caps, scope)
	m.deleg = newPickerScope(0, protocol.TaskScope{})
	m.delegOpened = ""
	m.overrides = nil
	m.cascade = false
	m.keepConns = false
}

func (m *AuthorityPickerModel) buildRows(tasks []protocol.TaskInfo) {
	m.rows = m.rows[:0]
	m.appendSection(tasks, false)
	// The bound is edited with the same controls, so it reads the same way:
	// a cap set, a base, exclude-self, a visibility rank, and the two id sets.
	m.rows = append(m.rows, pickerRow{kind: rowSection,
		label: "— delegation: what this task may hand to the tasks it creates —"})
	m.appendSection(tasks, true)
	if m.mode == PickerModeRegrant {
		m.rows = append(m.rows, pickerRow{kind: rowCascade, label: "--cascade (clamp descendants too)"})
		m.rows = append(m.rows, pickerRow{kind: rowKeepConns, label: "--keep-conns (do not drop their connections)"})
	}
}

// appendSection appends one section's rows: every capability, the base and
// its exclude-self twin, the visibility rank, and one row per task.
func (m *AuthorityPickerModel) appendSection(tasks []protocol.TaskInfo, deleg bool) {
	for _, c := range cli.CapsCatalog() {
		// The catalog includes the none/all pseudo entries; the picker
		// toggles real bits only.
		if c.Bit == 0 || protocol.Capability(c.Bit) == protocol.Capability_All {
			continue
		}
		m.rows = append(m.rows, pickerRow{kind: rowCap, label: c.Name, bit: protocol.Capability(c.Bit), deleg: deleg})
	}
	m.rows = append(m.rows, pickerRow{kind: rowBase, label: "base:", deleg: deleg})
	m.rows = append(m.rows, pickerRow{kind: rowExcludeSelf,
		label: "exclude self (subtree→descendants; none→the empty set)", deleg: deleg})
	// Directly under its sibling: the visibility rank is the same three words
	// with one fewer state (self is always visible, so no exclude-self twin).
	// Putting it anywhere else is how the WebUI ended up with the control on
	// the override rows and not on the row above them.
	m.rows = append(m.rows, pickerRow{kind: rowVisBase, label: "see:", deleg: deleg})
	for _, t := range tasks {
		idHex := FormatTaskID(t.Id)
		if idHex == m.targetHex {
			continue
		}
		label := idHex[:8] + " " + taskStatusStr(t.Status) + " " + taskAgentCell(t) +
			" " + truncateLeft(string(t.RepoPath), 20)
		if p := string(t.Prompt); p != "" {
			label += " " + runewidth.Truncate(p, 24, "…")
		}
		m.rows = append(m.rows, pickerRow{kind: rowTask, label: label, idHex: idHex, deleg: deleg})
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

// effectiveVisBase is the rank visibility actually runs at: the stated one, or
// the action base when it is unstated (the grammar's "omitting it means
// visibility follows the action base").
func (m *pickerScope) effectiveVisBase() protocol.ScopeBase {
	if m.visBasePresent {
		return m.visBase
	}
	return m.base
}

// A task row carries two checkboxes now, so it is only dead when NEITHER can
// use it: global already covers every task on both axes.
func (m *AuthorityPickerModel) rowDisabled(r pickerRow) bool {
	if r.kind == rowSection {
		return true
	}
	s := m.section(r)
	return r.kind == rowTask &&
		s.base == protocol.ScopeBase_Global &&
		s.effectiveVisBase() == protocol.ScopeBase_Global
}

// ToggleVisID flips the cursor task row's +vis-ids: membership — the second
// checkbox on the same row, on its own key so the common case (space) still
// means the action set. A no-op off a task row and in parent mode.
//
// It is NOT gated on the visibility rank: a global rank makes the clause
// redundant, not illegal, and a key that silently does nothing under a state
// the operator cannot see is worse than a redundant selection they can.
func (m *AuthorityPickerModel) ToggleVisID() {
	if len(m.rows) == 0 || m.mode == PickerModeParent {
		return
	}
	if r := m.rows[m.cursor]; r.kind == rowTask {
		s := m.section(r)
		s.visSelected[r.idHex] = !s.visSelected[r.idHex]
	}
}

// SetAllCaps sets every granular capability bit on (all=true) or off
// (all=false) in the section the cursor is in — the TUI counterpart of the
// WebUI chip row's [all] / [none] quick-set buttons.
func (m *AuthorityPickerModel) SetAllCaps(all bool) {
	deleg := len(m.rows) > 0 && m.rows[m.cursor].deleg
	s := m.section(pickerRow{deleg: deleg})
	s.caps = protocol.Capability_None
	if !all {
		return
	}
	for _, r := range m.rows {
		if r.kind == rowCap && r.deleg == deleg {
			s.caps |= r.bit
		}
	}
}

// Toggle flips the current row; on the base row it cycles
// subtree → none → global → subtree.
func (m *AuthorityPickerModel) Toggle() {
	if len(m.rows) == 0 || m.mode == PickerModeParent {
		// Parent mode is single-choice: the cursor is the selection and Enter
		// applies it; there is nothing to toggle.
		return
	}
	r := m.rows[m.cursor]
	s := m.section(r)
	switch r.kind {
	case rowCap:
		s.caps ^= r.bit
	case rowBase:
		switch s.base {
		case protocol.ScopeBase_Subtree:
			s.base = protocol.ScopeBase_None
		case protocol.ScopeBase_None:
			s.base = protocol.ScopeBase_Global
		default:
			s.base = protocol.ScopeBase_Subtree
		}
	case rowExcludeSelf:
		s.excludeSelf = !s.excludeSelf
	case rowVisBase:
		// Four states, because "not stated" is one of them and is the default.
		// Cycling back to it is the only way to UNSET a rank once set, so it
		// stays in the ring rather than being an initial value you fall out of.
		switch {
		case !s.visBasePresent:
			s.visBase, s.visBasePresent = protocol.ScopeBase_Subtree, true
		case s.visBase == protocol.ScopeBase_Subtree:
			s.visBase = protocol.ScopeBase_None
		case s.visBase == protocol.ScopeBase_None:
			s.visBase = protocol.ScopeBase_Global
		default:
			s.visBasePresent = false
		}
	case rowTask:
		s.selected[r.idHex] = !s.selected[r.idHex]
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
// Overrides returns the per-capability narrowings the picker is carrying
// unchanged, so the caller can send them back with the scope rather than
// clearing them.
func (m *AuthorityPickerModel) Overrides() []protocol.ScopeOverride { return m.overrides }

// Delegation returns the delegation bound's selection: its caps, its scope in
// the grammar (always explicit — the bound has no "not named" form here), and
// whether either half differs from what the picker opened with, so a
// re-grant writes the bound only when the operator changed it.
func (m *AuthorityPickerModel) Delegation() (caps protocol.Capability, scopeSpec string, changed bool) {
	return m.deleg.caps, cli.ScopeLabel(m.deleg.scope()), m.delegLabel() != m.delegOpened
}

func (m *AuthorityPickerModel) delegLabel() string {
	return cli.CapsLabel(m.deleg.caps) + " " + cli.ScopeLabel(m.deleg.scope())
}

func (m *AuthorityPickerModel) Result() (caps protocol.Capability, scopeSpec string, cascade, keepConns bool) {
	spec := m.scopeSpec()
	if m.mode == PickerModeSession {
		return m.caps, spec, false, false
	}
	return m.caps, spec, m.cascade, m.keepConns
}

// selectedIDs reads one of the two id sets back as sorted hex.
//
// dropAtGlobal applies to the ACTION set only, and is the grammar's rule rather
// than a tidiness one: `global+ids:` does not parse, so serializing it would
// produce a string nothing accepts. `global/…+vis-ids:` DOES parse — redundant,
// since a global rank already sees everything, but legal — so the see-only set
// is never dropped. Dropping it would make opening a `global/subtree+vis-ids:X`
// task and pressing apply erase the clause, which is the silent narrowing this
// dialog exists to have stopped doing.
func selectedIDs(set map[string]bool, dropAtGlobal protocol.ScopeBase) []string {
	if dropAtGlobal == protocol.ScopeBase_Global {
		return nil
	}
	var ids []string
	for id, on := range set {
		if on {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// scope assembles a section's selection into a wire TaskScope: both halves,
// each from its own controls.
func (m *pickerScope) scope() protocol.TaskScope {
	var sc protocol.TaskScope
	sc.Base = m.base
	sc.SetExcludeSelf(m.excludeSelf)
	sc.VisBase = m.visBase
	sc.SetVisBasePresent(m.visBasePresent)

	appendIDs := func(dst *[]protocol.TaskID, ids []string) {
		for _, id := range ids {
			tid, err := ParseTaskID(id)
			if err != nil {
				continue // rows are built from formatted ids; a bad one cannot select
			}
			*dst = append(*dst, tid)
		}
	}
	appendIDs(&sc.Ids, selectedIDs(m.selected, m.base))
	sc.IdsLen = uint16(len(sc.Ids))
	// Never dropped: see selectedIDs. The zero rank passed here is "no rank
	// suppresses this set", not a claim about the visibility base.
	appendIDs(&sc.VisIds, selectedIDs(m.visSelected, protocol.ScopeBase_Subtree))
	sc.VisIdsLen = uint16(len(sc.VisIds))
	return sc
}

// scopeSpec serializes that scope to the `--scope` grammar with cli.ScopeLabel
// — the same function `ls` renders with, so there is ONE serializer rather
// than a picker-local copy. The copy this replaced knew only three of the six
// bases and neither half of the visibility pair, so a picker apply silently
// turned `descendants` into `subtree` and dropped `global/…` entirely.
//
// Session mode returns "" for the plain default, which is what "--scope not
// named" means on a spawn; a re-grant is always explicit.
func (m *AuthorityPickerModel) scopeSpec() string {
	sc := m.scope()
	if m.mode == PickerModeSession && len(sc.Ids) == 0 && !m.excludeSelf &&
		!sc.VisBasePresent() && len(sc.VisIds) == 0 && m.base == protocol.ScopeBase_Subtree {
		return ""
	}
	return cli.ScopeLabel(sc)
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
	switch m.mode {
	case PickerModeRegrant:
		title = "re-grant " + m.targetHex[:8]
	case PickerModeParent:
		title = "set parent of " + m.targetHex[:8]
	}

	// The popup's width depends only on the terminal, never on the content:
	// sizing to the longest line makes the frame jump every time a selected
	// id lengthens the footer echo. Lines are truncated AND padded to inner.
	inner := m.w * 3 / 5
	if inner > 100 {
		inner = 100
	}
	if inner < 44 {
		inner = 44
	}
	if inner > m.w-4 {
		inner = m.w - 4
	}
	fit := func(s string) string {
		s = runewidth.Truncate(s, inner, "…")
		return s + strings.Repeat(" ", inner-runewidth.StringWidth(s))
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
	b.WriteString(fit(title) + "\n")
	for i := start; i < end; i++ {
		r := m.rows[i]
		line := "[ ] "
		if m.mode == PickerModeParent {
			// Single-choice: the cursor IS the selection, so checkboxes would
			// suggest a multi-select that does not exist.
			line = "  "
		}
		s := m.section(r)
		switch r.kind {
		case rowCap:
			if s.caps&r.bit == r.bit {
				line = "[x] "
			}
		case rowBase, rowVisBase, rowSection:
			line = "    "
		case rowTask:
			if m.mode == PickerModeParent {
				break
			}
			// Two boxes on one row: the action set (space) and the see-only
			// set (v). Rendering them side by side is what makes the second
			// key discoverable — a row that looked identical either way would
			// leave `v` as folklore in the footer.
			act, see := "[ ]", "[ ]"
			if s.selected[r.idHex] {
				act = "[x]"
			}
			if s.visSelected[r.idHex] {
				see = "[v]"
			}
			line = act + see + " "
		case rowExcludeSelf:
			if s.excludeSelf {
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
		switch r.kind {
		case rowBase:
			// A bare action scope through the SAME serializer, so this row
			// spells `descendants` exactly the way ls does rather than growing
			// a picker-local word list. The vis prefix and the ids belong to
			// their own rows and the footer echo.
			bare := protocol.TaskScope{Base: s.base}
			bare.SetExcludeSelf(s.excludeSelf)
			label = "base: " + cli.ScopeLabel(bare)
		case rowVisBase:
			// "(follows base)" rather than the resolved word: showing the
			// action rank here would make the unstated default look like an
			// explicit choice, which is the one distinction this row exists
			// to expose.
			see := "(follows base)"
			if s.visBasePresent {
				see = s.visBase.String()
			}
			label = "see:  " + see
		}
		line = fit(line + label)
		switch {
		case i == m.cursor:
			line = pickerCursorStyle.Render(line)
		case m.rowDisabled(r):
			line = pickerDisabledStyle.Render(line)
		}
		b.WriteString(line + "\n")
	}

	if m.mode == PickerModeParent {
		b.WriteString(fit("j/k move · enter apply · esc cancel"))
		return pickerBorderStyle.Render(b.String())
	}
	_, spec, _, _ := m.Result()
	if spec == "" {
		spec = "(default subtree)"
	}
	_, dspec, _ := m.Delegation()
	b.WriteString(fit("space toggle · v see-only · A/N all/none caps · enter apply · esc cancel · scope=" + spec +
		" delegate=" + cli.CapsLabel(m.deleg.caps) + " delegate-scope=" + dspec))
	return pickerBorderStyle.Render(b.String())
}
