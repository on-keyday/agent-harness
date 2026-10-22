package cli

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// AuthorityTasks converts a listing into the view package authority reads.
func AuthorityTasks(tasks []protocol.TaskInfo) []authority.Task {
	out := make([]authority.Task, 0, len(tasks))
	for _, t := range tasks {
		at := authority.Task{
			ID:            hex.EncodeToString(t.Id.Id[:]),
			Caps:          t.Capabilities,
			DelegateCaps:  t.DelegateCaps,
			Scope:         authority.ScopeFromWire(t.Scope, t.Overrides),
			DelegateScope: authority.ScopeFromWire(t.DelegateScope, nil),
		}
		if t.CreatorTaskId.Id != ([16]byte{}) {
			at.Creator = hex.EncodeToString(t.CreatorTaskId.Id[:])
		}
		out = append(out, at)
	}
	return out
}

// ReachLine is one entry of a reach report: a capability, or view / delegate.
type ReachLine struct {
	Cap string
	authority.ReachResult
}

// ReachReport is `caps reach`: what one task reaches per bit it exercises, what
// it sees, what it may hand down, and how it is wider than its parent. Every
// surface renders it through WriteReachReport.
type ReachReport struct {
	TaskID   string
	Granter  string
	Bits     []ReachLine
	View     ReachLine
	Delegate ReachLine
	// DelegateCaps is what a child can actually be given: the task's caps
	// inside its bound (Granter.ChildCaps of all), not the bound as written.
	DelegateCaps protocol.Capability
	Wider        authority.Wider
	// WiderComputed is false over a partial listing, where Wider is zero and
	// means nothing (see BuildReachReport).
	WiderComputed bool
}

// BuildReachReport reads the report for id off the caller's listing. focus,
// when non-zero, keeps one bit; otherwise every exercised bit, strongest
// first.
//
// complete says the listing is everything (an operator's). A confined
// caller's listing has gaps — tasks it cannot see between its parent and its
// targets — and a subtree walk stops at a gap, so comparing with the parent
// over it can report a widening that does not exist. Over a partial listing
// the comparison is not made.
func BuildReachReport(tasks []protocol.TaskInfo, id string, focus protocol.Capability, complete bool) (ReachReport, error) {
	view := AuthorityTasks(tasks)
	var info *protocol.TaskInfo
	for i := range tasks {
		if hex.EncodeToString(tasks[i].Id.Id[:]) == id {
			info = &tasks[i]
		}
	}
	if info == nil {
		return ReachReport{}, fmt.Errorf("caps reach: no such task %s", shortID(id))
	}
	r := ReachReport{
		TaskID:        id,
		Granter:       AuthorityLabel(*info),
		View:          ReachLine{Cap: "view", ReachResult: authority.ReachOf(view, id, protocol.Capability_None)},
		Delegate:      ReachLine{Cap: "delegate", ReachResult: authority.DelegableOf(view, id)},
		DelegateCaps:  info.Capabilities & info.DelegateCaps,
		WiderComputed: complete,
	}
	if complete {
		r.Wider = WiderOf(view, id)
	}
	for _, c := range ReachBits(info.Capabilities) {
		if focus != 0 && c != focus {
			continue
		}
		r.Bits = append(r.Bits, ReachLine{Cap: c.String(), ReachResult: authority.ReachOf(view, id, c)})
	}
	return r, nil
}

// ReachBits are the bits a holder of caps reaches TASKS through: every bit it
// exercises, strongest first, less those that name no task (spawn, notify,
// runner_admin, board_observe, board_send — `global（固定）` in caps audit),
// which have no per-task reach to draw.
func ReachBits(caps protocol.Capability) []protocol.Capability {
	exercised := authority.ExercisedBits(caps)
	var out []protocol.Capability
	for _, c := range verb.AuditOrder() {
		if exercised&c != 0 && verb.CapScopeKindOf(c) != verb.ScopeNone {
			out = append(out, c)
		}
	}
	return out
}

// WiderOf is authority.WiderThanParent restricted to the bits that name tasks
// (ReachBits): a child is not wider through spawn, which targets nobody.
func WiderOf(view []authority.Task, id string) authority.Wider {
	w := authority.WiderThanParent(view, id)
	var targeted protocol.Capability
	for _, c := range verb.AuditOrder() {
		if verb.CapScopeKindOf(c) != verb.ScopeNone {
			targeted |= c
		}
	}
	w.Bits &= targeted
	return w
}

// WiderLine is the one rendering of "wider than parent", for the report and
// the WebUI tree's mark alike. "" when the task is not wider.
func WiderLine(w authority.Wider, granter string) string {
	if w.Empty() {
		return ""
	}
	var parts []string
	if w.Bits != 0 {
		parts = append(parts, CapsLabel(w.Bits))
	}
	if w.Sees {
		parts = append(parts, "+vis")
	}
	return "wider than parent: " + strings.Join(parts, " ") + " (granter=" + granter + ")"
}

const unlisted = "global (+ tasks you cannot list)"

func idList(ids []string) string {
	var b strings.Builder
	for _, id := range ids {
		b.WriteString(" " + shortID(id))
	}
	return b.String()
}

// reachBody renders one line's act / see halves; a line with no action half
// (view) prints only what it sees.
func reachBody(r authority.ReachResult, withAct bool) string {
	see := func() string {
		if r.SeeAll {
			return "see " + unlisted
		}
		if !withAct {
			s := fmt.Sprintf("see %d", len(r.See))
			if len(r.See) > 0 {
				s += ":" + idList(r.See)
			}
			return s
		}
		s := fmt.Sprintf("see +%d", len(r.See))
		if len(r.See) > 0 {
			s += ":" + idList(r.See)
		}
		return s
	}
	if !withAct {
		return see()
	}
	if r.ActAll {
		return "act " + unlisted
	}
	act := fmt.Sprintf("act %d", len(r.Act))
	if len(r.Act) > 0 {
		act += ":" + idList(r.Act)
	}
	return act + "   " + see()
}

type reachLineJSON struct {
	Cap    string   `json:"cap"`
	ActAll bool     `json:"act_all"`
	Act    []string `json:"act"`
	SeeAll bool     `json:"see_all"`
	See    []string `json:"see"`
	Caps   string   `json:"caps,omitempty"`
}

func lineJSON(l ReachLine) reachLineJSON {
	j := reachLineJSON{Cap: l.Cap, ActAll: l.ActAll, Act: l.Act, SeeAll: l.SeeAll, See: l.See}
	if j.Act == nil {
		j.Act = []string{}
	}
	if j.See == nil {
		j.See = []string{}
	}
	return j
}

// WriteReachReport prints r as text (ids short) or as one JSON object (ids
// full).
func WriteReachReport(w io.Writer, r ReachReport, asJSON bool) error {
	if asJSON {
		bits := make([]reachLineJSON, 0, len(r.Bits))
		for _, l := range r.Bits {
			bits = append(bits, lineJSON(l))
		}
		deleg := lineJSON(r.Delegate)
		deleg.Caps = CapsLabel(r.DelegateCaps)
		return json.NewEncoder(w).Encode(map[string]any{
			"task": r.TaskID, "granter": r.Granter, "bits": bits,
			"view": lineJSON(r.View), "delegate": deleg,
			"wider": map[string]any{"caps": CapsLabel(r.Wider.Bits), "sees": r.Wider.Sees, "computed": r.WiderComputed},
		})
	}
	row := func(name, body string) string { return fmt.Sprintf("  %-14s %s\n", name, body) }
	var b strings.Builder
	fmt.Fprintf(&b, "caps reach %s  granter=%s\n", shortID(r.TaskID), r.Granter)
	for _, l := range r.Bits {
		b.WriteString(row(l.Cap, reachBody(l.ReachResult, true)))
	}
	b.WriteString(row("view", reachBody(r.View.ReachResult, false)))
	b.WriteString(row("delegate", "caps="+CapsLabel(r.DelegateCaps)+"  "+reachBody(r.Delegate.ReachResult, true)))
	if !r.WiderComputed {
		b.WriteString("  wider than parent: not computed (your listing is partial)\n")
	} else if l := WiderLine(r.Wider, r.Granter); l != "" {
		b.WriteString("  " + l + "\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}
