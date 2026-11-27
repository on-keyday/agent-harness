package tui

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Per-capability overrides in the authority picker: each one a section of
// rows — its capability bits (a bit another override holds is disabled: the
// server refuses overlapping masks), a base and exclude-self, the task ids,
// and its grant condition where one kind applies to every bit of the row
// (exec_run alone: a checklist of actions by name; forward bits: a typed port
// list). The WebUI's buildOverrideRows has the same controls.
//
// What the picker must keep: an override the operator did not touch goes
// back exactly as it came, with its condition body — overrides travel with
// the scope under one presence bit, so a re-grant that dropped one would
// erase the target's rule, and one that sent a marker without its body would
// be refused (grant conditions C6, C7). Only a removed override disappears,
// and Removed names it for the result line.

// pickerOverride is one override as the picker edits it. orig / origCond are
// what the picker opened with; dirty says the controls now differ from them.
type pickerOverride struct {
	orig        protocol.ScopeOverride
	origCond    *protocol.GrantCondition
	dirty       bool
	caps        protocol.Capability
	base        protocol.ScopeBase
	excludeSelf bool
	selected    map[string]bool
	actions     []string // exec_run: the checked action ids, in order
	ports       string   // forward bits: the list after "ports:"
	// unknown: marked conditioned with no body in hand. It reaches nothing
	// (C7), so it is not editable here — an edit would drop the marker and
	// widen it; x removes it, --grant-condition sets one.
	unknown bool
}

const forwardBits = protocol.Capability_ForwardLocal | protocol.Capability_ForwardRemote

// condKind is the condition kind that applies to every bit of caps: "actions"
// for exactly exec_run, "ports" for forward bits only, "" otherwise.
func condKind(caps protocol.Capability) string {
	switch {
	case caps == protocol.Capability_ExecRun:
		return "actions"
	case caps != 0 && caps&^forwardBits == 0:
		return "ports"
	}
	return ""
}

func newPickerOverride(o protocol.ScopeOverride, conds []protocol.GrantCondition) pickerOverride {
	p := pickerOverride{orig: o, caps: o.Caps, base: o.Base, excludeSelf: o.ExcludeSelf(), selected: map[string]bool{}}
	for _, id := range o.Ids {
		p.selected[FormatTaskID(id)] = true
	}
	for i := range conds {
		if conds[i].Caps != o.Caps {
			continue
		}
		c := conds[i]
		p.origCond = &c
		if a := c.ActionSet(); a != nil {
			for _, id := range a.Actions {
				p.actions = append(p.actions, hex.EncodeToString(id.Id[:]))
			}
		}
		if c.PortRange() != nil {
			if w, err := authority.ConditionFromWire(c); err == nil {
				p.ports = strings.TrimPrefix(w.Label(), "ports:")
			}
		}
	}
	p.unknown = o.Conditioned() && p.origCond == nil
	return p
}

// wire is the override as the picker now holds it.
func (p *pickerOverride) wire() protocol.ScopeOverride {
	if !p.dirty {
		return p.orig
	}
	w := protocol.ScopeOverride{Caps: p.caps, Base: p.base}
	w.SetExcludeSelf(p.excludeSelf)
	var ids []protocol.TaskID
	for _, h := range selectedIDs(p.selected, p.base) {
		if id, err := ParseTaskID(h); err == nil {
			ids = append(ids, id)
		}
	}
	w.SetIds(ids)
	if c, _ := p.condition(); c != nil {
		w.SetConditioned(true)
	}
	return w
}

// condition is the override's grant condition body, nil when it has none; an
// error when the typed port list does not parse.
func (p *pickerOverride) condition() (*protocol.GrantCondition, error) {
	if !p.dirty {
		return p.origCond, nil
	}
	switch condKind(p.caps) {
	case "actions":
		if len(p.actions) == 0 {
			return nil, nil
		}
		g, err := verb.ParseGrantCondition(cli.CapsLabel(p.caps) + "=actions:" + strings.Join(p.actions, ","))
		return &g, err
	case "ports":
		if strings.TrimSpace(p.ports) == "" {
			return nil, nil
		}
		g, err := verb.ParseGrantCondition(cli.CapsLabel(p.caps) + "=ports:" + p.ports)
		if err != nil {
			return nil, err
		}
		return &g, nil
	}
	return nil, nil
}

// claimedElsewhere is every bit the other overrides hold.
func (m *AuthorityPickerModel) claimedElsewhere(ov int) protocol.Capability {
	var c protocol.Capability
	for i := range m.ovs {
		if i != ov {
			c |= m.ovs[i].caps
		}
	}
	return c
}

// SetActionViews hands the picker the action list its checklists show.
func (m *AuthorityPickerModel) SetActionViews(v []cli.ActionView) {
	m.actionViews, m.actionErr = v, nil
	m.rebuildKeepCursor()
}

// SetActionViewsErr says the action list could not be read: the checklist
// then says so, rather than calling the condition's ids deleted.
func (m *AuthorityPickerModel) SetActionViewsErr(err error) {
	m.actionViews, m.actionErr = nil, err
	m.rebuildKeepCursor()
}

// AddOverride appends an empty override and puts the cursor on it.
func (m *AuthorityPickerModel) AddOverride() {
	m.ovs = append(m.ovs, pickerOverride{dirty: true, base: protocol.ScopeBase_Subtree, selected: map[string]bool{}})
	m.rebuildKeepCursor()
	for i, r := range m.rows {
		if r.kind == rowOvHead && r.ov == len(m.ovs)-1 {
			m.cursor = i
		}
	}
}

// RemoveOverride drops the override the cursor is in.
func (m *AuthorityPickerModel) RemoveOverride() {
	if len(m.rows) == 0 {
		return
	}
	r := m.rows[m.cursor]
	if !isOverrideRow(r.kind) || r.ov < 0 || r.ov >= len(m.ovs) {
		return
	}
	o := m.ovs[r.ov]
	if o.caps != 0 {
		m.removed = append(m.removed, cli.OverridesLabel([]protocol.ScopeOverride{o.wire()}))
	}
	m.ovs = append(m.ovs[:r.ov], m.ovs[r.ov+1:]...)
	m.rebuildKeepCursor()
}

// Removed names the overrides the operator removed, for the result line.
func (m *AuthorityPickerModel) Removed() []string { return m.removed }

// Problem is why the picker cannot be applied yet: a port list that does not
// parse. "" when it can.
func (m *AuthorityPickerModel) Problem() string {
	for i := range m.ovs {
		if _, err := m.ovs[i].condition(); err != nil {
			return fmt.Sprintf("override %d: %v", i+1, err)
		}
	}
	return ""
}

func isOverrideRow(k pickerRowKind) bool {
	switch k {
	case rowOvHead, rowOvCap, rowOvBase, rowOvExcludeSelf, rowOvTask, rowOvAction, rowOvPorts:
		return true
	}
	return false
}

// appendOverrides appends the overrides section: a heading, each override's
// rows, and the row that adds one.
func (m *AuthorityPickerModel) appendOverrides() {
	m.rows = append(m.rows, pickerRow{kind: rowSection, ov: -1,
		label: "— per-capability overrides: space toggles · x removes the override under the cursor —"})
	for i := range m.ovs {
		o := &m.ovs[i]
		m.rows = append(m.rows, pickerRow{kind: rowOvHead, ov: i})
		for _, c := range cli.CapsCatalog() {
			if c.Bit == 0 || protocol.Capability(c.Bit) == protocol.Capability_All {
				continue
			}
			m.rows = append(m.rows, pickerRow{kind: rowOvCap, ov: i, label: c.Name, bit: protocol.Capability(c.Bit)})
		}
		m.rows = append(m.rows, pickerRow{kind: rowOvBase, ov: i}, pickerRow{kind: rowOvExcludeSelf, ov: i,
			label: "exclude self (subtree→descendants; none→the empty set)"})
		if o.base != protocol.ScopeBase_Global {
			for _, t := range m.tasks {
				idHex := FormatTaskID(t.Id)
				if idHex == m.targetHex {
					continue
				}
				m.rows = append(m.rows, pickerRow{kind: rowOvTask, ov: i, idHex: idHex, label: idHex[:8] + " " + taskStatusStr(t.Status) + " " + taskAgentCell(t)})
			}
		}
		switch condKind(o.caps) {
		case "actions":
			known := map[string]string{}
			for _, v := range m.actionViews {
				known[v.ID] = v.Name
			}
			listed := map[string]bool{}
			for _, id := range o.actions {
				label := "deleted " + id[:8]
				if n, ok := known[id]; ok {
					label = n + " (" + id[:8] + ")"
				} else if m.actionErr != nil {
					label = id[:8] + " (action list unavailable)"
				}
				m.rows = append(m.rows, pickerRow{kind: rowOvAction, ov: i, idHex: id, label: label})
				listed[id] = true
			}
			for _, v := range m.actionViews {
				if !listed[v.ID] {
					m.rows = append(m.rows, pickerRow{kind: rowOvAction, ov: i, idHex: v.ID, label: v.Name + " (" + v.ID[:8] + ")"})
				}
			}
			switch {
			case m.actionErr != nil:
				m.rows = append(m.rows, pickerRow{kind: rowSection, ov: i, label: "  action list unavailable: " + m.actionErr.Error()})
			case len(m.actionViews) == 0 && len(o.actions) == 0:
				m.rows = append(m.rows, pickerRow{kind: rowSection, ov: -1, label: "  no actions — define one in Actions (M)"})
			}
		case "ports":
			m.rows = append(m.rows, pickerRow{kind: rowOvPorts, ov: i})
		}
	}
	// A hint, not a toggle: adding grows the list, and space on every row in
	// turn must stay a walk that ends.
	m.rows = append(m.rows, pickerRow{kind: rowSection, ov: -1, label: "  o adds an override"})
}

// rebuildKeepCursor rebuilds the rows after an edit that changes which rows
// exist, leaving the cursor on the same row — found by what it is, not by
// index, since rows can appear above it (an action list arriving late) — or,
// when that row is gone, on the same index.
func (m *AuthorityPickerModel) rebuildKeepCursor() {
	at := m.cursor
	var was pickerRow
	had := at >= 0 && at < len(m.rows)
	if had {
		was = m.rows[at]
	}
	m.buildRows(m.tasks)
	if had {
		for i, r := range m.rows {
			if r.kind == was.kind && r.deleg == was.deleg && r.ov == was.ov && r.bit == was.bit &&
				r.idHex == was.idHex && r.label == was.label {
				m.cursor = i
				return
			}
		}
	}
	if at >= len(m.rows) {
		at = len(m.rows) - 1
	}
	if at < 0 {
		at = 0
	}
	m.cursor = at
}

// toggleOverride is Toggle on an override row.
func (m *AuthorityPickerModel) toggleOverride(r pickerRow) {
	if r.ov < 0 || r.ov >= len(m.ovs) {
		return
	}
	o := &m.ovs[r.ov]
	if o.unknown {
		return
	}
	switch r.kind {
	case rowOvCap:
		if m.claimedElsewhere(r.ov)&r.bit != 0 {
			return
		}
		before := condKind(o.caps)
		o.caps ^= r.bit
		if condKind(o.caps) != before {
			o.actions, o.ports = nil, ""
		}
	case rowOvBase:
		switch o.base {
		case protocol.ScopeBase_Subtree:
			o.base = protocol.ScopeBase_None
		case protocol.ScopeBase_None:
			o.base = protocol.ScopeBase_Global
		default:
			o.base = protocol.ScopeBase_Subtree
		}
	case rowOvExcludeSelf:
		o.excludeSelf = !o.excludeSelf
	case rowOvTask:
		o.selected[r.idHex] = !o.selected[r.idHex]
	case rowOvAction:
		at := -1
		for i, id := range o.actions {
			if id == r.idHex {
				at = i
			}
		}
		if at >= 0 {
			o.actions = append(o.actions[:at], o.actions[at+1:]...)
		} else {
			o.actions = append(o.actions, r.idHex)
		}
	default:
		return
	}
	o.dirty = true
	m.rebuildKeepCursor()
}

// typeOnPorts edits the port list on a ports row: digits, '-' and ','.
func (m *AuthorityPickerModel) typeOnPorts(ch rune, back bool) bool {
	if len(m.rows) == 0 {
		return false
	}
	r := m.rows[m.cursor]
	if r.kind != rowOvPorts || r.ov < 0 || r.ov >= len(m.ovs) {
		return false
	}
	o := &m.ovs[r.ov]
	switch {
	case back:
		if o.ports != "" {
			o.ports = o.ports[:len(o.ports)-1]
		}
	case (ch >= '0' && ch <= '9') || ch == '-' || ch == ',':
		o.ports += string(ch)
	default:
		return true
	}
	o.dirty = true
	return true
}

// overrideLine renders an override row's checkbox and label.
func (m *AuthorityPickerModel) overrideLine(r pickerRow) string {
	if r.ov < 0 || r.ov >= len(m.ovs) {
		return "    " + r.label
	}
	o := &m.ovs[r.ov]
	box := func(on bool) string {
		if on {
			return "[x] "
		}
		return "[ ] "
	}
	switch r.kind {
	case rowOvHead:
		w := o.wire()
		head := fmt.Sprintf("override %d: caps=%s", r.ov+1, cli.CapsLabel(o.caps))
		if ov := cli.OverridesLabel([]protocol.ScopeOverride{w}); ov != "" {
			head = fmt.Sprintf("override %d: %s", r.ov+1, ov)
		}
		if c, err := o.condition(); err == nil && c != nil {
			if cw, err := authority.ConditionFromWire(*c); err == nil {
				head += " condition=" + cw.Label()
			}
		}
		if o.unknown {
			head += " condition=(unknown: reaches nothing; x removes it, --grant-condition sets one)"
		}
		return "    " + head
	case rowOvCap:
		return "  " + box(o.caps&r.bit == r.bit) + r.label
	case rowOvBase:
		bare := protocol.TaskScope{Base: o.base}
		bare.SetExcludeSelf(o.excludeSelf)
		return "      base: " + cli.ScopeLabel(bare)
	case rowOvExcludeSelf:
		return "  " + box(o.excludeSelf) + r.label
	case rowOvTask:
		return "  " + box(o.selected[r.idHex]) + r.label
	case rowOvAction:
		on := false
		for _, id := range o.actions {
			on = on || id == r.idHex
		}
		return "  " + box(on) + "action " + r.label
	case rowOvPorts:
		return "      ports: " + o.ports + "▏ (type digits , -)"
	}
	return "    " + r.label
}
