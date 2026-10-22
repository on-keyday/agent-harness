package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// WhatIfReport is `caps what-if`: every task a change would alter, and the
// command that makes the change. Every surface renders it through
// WriteWhatIf.
type WhatIfReport struct {
	Diffs   []authority.TaskDiff
	Command string
	// RunBy is a spawn's creator: the command is that task's to run.
	RunBy string
	// Partial says the listing was a confined caller's, so tasks it cannot
	// list are missing from the diff (BuildReachReport's complete).
	Partial bool
}

// WhatIfChange is the change a parsed `caps what-if` line describes. The
// positional is the target of a re-grant or reparent and the creator of a
// spawn.
func WhatIfChange(a verb.CapsWhatIfAction) authority.Change {
	if a.ParentID != "" || a.ParentNone {
		return authority.Reparent{Target: a.TaskID, NewParent: a.ParentID}
	}
	if a.Spawn {
		s := authority.Spawn{Creator: a.TaskID}
		var scope, bound protocol.TaskScope
		if a.Caps != nil {
			s.Caps = *a.Caps
		}
		if a.Scope != nil {
			scope = *a.Scope
		}
		if a.DelegateCaps != nil {
			s.DelegateCaps = *a.DelegateCaps
		}
		if a.DelegateScope != nil {
			bound = *a.DelegateScope
		}
		s.Scope = authority.ScopeFromWire(scope, a.Overrides)
		s.DelegateScope = authority.ScopeFromWire(bound, nil)
		return s
	}
	r := authority.Regrant{Target: a.TaskID, Caps: a.Caps, DelegateCaps: a.DelegateCaps, Cascade: a.Cascade}
	if a.Scope != nil {
		s := authority.ScopeFromWire(*a.Scope, a.Overrides)
		r.Scope = &s
	}
	if a.DelegateScope != nil {
		s := authority.ScopeFromWire(*a.DelegateScope, nil)
		r.DelegateScope = &s
	}
	return r
}

// BuildWhatIf applies c to the caller's listing. A refusal is the server's
// own text: the change would be refused the same way when sent.
func BuildWhatIf(tasks []protocol.TaskInfo, c authority.Change) (WhatIfReport, error) {
	diffs, err := authority.WhatIf(AuthorityTasks(tasks), c)
	if err != nil {
		return WhatIfReport{}, fmt.Errorf("caps what-if: %w", err)
	}
	r := WhatIfReport{Command: WhatIfCommand(c)}
	if s, ok := c.(authority.Spawn); ok {
		r.RunBy = s.Creator
	}
	// A task whose only change is on a bit naming no task (spawn, notify, …)
	// has no line to show, as caps reach draws none for those bits.
	for _, d := range diffs {
		if d.New || len(diffHead(d)) > 0 || len(WhatIfBits(d)) > 0 {
			r.Diffs = append(r.Diffs, d)
		}
	}
	return r, nil
}

func scopeWithOverrides(s authority.Scope) string {
	l := ScopeLabel(s.ToWire())
	if ov := OverridesLabel(s.OverridesToWire()); ov != "" {
		l += " +" + ov
	}
	return l
}

// grantParts are a grant's four halves under the keys ls prints them with.
func grantParts(g authority.Grant) [4][2]string {
	return [4][2]string{
		{"caps", CapsLabel(g.Caps)},
		{"scope", scopeWithOverrides(g.Scope)},
		{"delegate", CapsLabel(g.DelegateCaps)},
		{"delegate-scope", ScopeLabel(g.DelegateScope.ToWire())},
	}
}

// diffHead is the changed halves, `key=before → after`; a new task's are all
// of them, `key=value`.
func diffHead(d authority.TaskDiff) []string {
	after := grantParts(d.After)
	var out []string
	if d.New {
		for _, p := range after {
			out = append(out, p[0]+"="+p[1])
		}
		return out
	}
	before := grantParts(d.Before)
	for i := range after {
		if before[i][1] != after[i][1] {
			out = append(out, after[i][0]+"="+before[i][1]+" → "+after[i][1])
		}
	}
	return out
}

// WhatIfBit is one bit's change on one task: the tasks it gains and loses.
type WhatIfBit struct {
	Cap            string
	Added, Removed []string
}

// WhatIfBits are the bits that name tasks, strongest first, then view — as
// caps reach orders them — each with the tasks gained and lost.
func WhatIfBits(d authority.TaskDiff) []WhatIfBit {
	var out []WhatIfBit
	add := func(name string, c protocol.Capability) {
		if len(d.Added[c]) > 0 || len(d.Removed[c]) > 0 {
			out = append(out, WhatIfBit{Cap: name, Added: d.Added[c], Removed: d.Removed[c]})
		}
	}
	for _, c := range verb.AuditOrder() {
		if verb.CapScopeKindOf(c) != verb.ScopeNone {
			add(c.String(), c)
		}
	}
	add("view", protocol.Capability_None)
	return out
}

// WhatIfCommand is the command line that makes c, in the grammar the CLI
// parses back: caps set, submit (run by the creator) or caps set-parent.
func WhatIfCommand(c authority.Change) string {
	scopeFlags := func(s authority.Scope) []string {
		out := []string{"--scope", ScopeLabel(s.ToWire())}
		for _, o := range s.OverridesToWire() {
			out = append(out, "--scope-for", OverridesLabel([]protocol.ScopeOverride{o}))
		}
		return out
	}
	var tok []string
	switch c := c.(type) {
	case authority.Regrant:
		tok = []string{"harness-cli", "caps", "set", c.Target}
		if c.Caps != nil {
			tok = append(tok, "--caps", CapsLabel(*c.Caps))
		}
		if c.Scope != nil {
			tok = append(tok, scopeFlags(*c.Scope)...)
		}
		if c.DelegateCaps != nil {
			tok = append(tok, "--delegate-caps", CapsLabel(*c.DelegateCaps))
		}
		if c.DelegateScope != nil {
			tok = append(tok, "--delegate-scope", ScopeLabel(c.DelegateScope.ToWire()))
		}
		if c.Cascade {
			tok = append(tok, "--cascade")
		}
	case authority.Spawn:
		tok = append([]string{"harness-cli", "submit", "--caps", CapsLabel(c.Caps)}, scopeFlags(c.Scope)...)
		// The bound halves default to none / subtree, so they are named only
		// when they say something else.
		if c.DelegateCaps != 0 {
			tok = append(tok, "--delegate-caps", CapsLabel(c.DelegateCaps))
		}
		if l := ScopeLabel(c.DelegateScope.ToWire()); l != ScopeLabel(protocol.TaskScope{}) {
			tok = append(tok, "--delegate-scope", l)
		}
	case authority.Reparent:
		tok = []string{"harness-cli", "caps", "set-parent", c.Target}
		if c.NewParent == "" {
			tok = append(tok, "--none")
		} else {
			tok = append(tok, "--parent", c.NewParent)
		}
	}
	return strings.Join(tok, " ")
}

// WhatIfHead is a diff's head line: the id (`new` for a spawn's child), then
// the changed halves.
func WhatIfHead(d authority.TaskDiff) string {
	id := shortID(d.ID)
	if d.New {
		id = d.ID
	}
	return strings.Join(append([]string{id}, diffHead(d)...), "  ")
}

// Line is the bit's line: its name, then +N and -M with the ids (short).
func (b WhatIfBit) Line() string {
	var parts []string
	if len(b.Added) > 0 {
		parts = append(parts, fmt.Sprintf("+%d:%s", len(b.Added), idList(b.Added)))
	}
	if len(b.Removed) > 0 {
		parts = append(parts, fmt.Sprintf("-%d:%s", len(b.Removed), idList(b.Removed)))
	}
	return fmt.Sprintf("%-14s %s", b.Cap, strings.Join(parts, "  "))
}

// WriteWhatIf prints r as text (ids short, the command's full) or as one JSON
// object (ids full).
func WriteWhatIf(w io.Writer, r WhatIfReport, asJSON bool) error {
	if asJSON {
		type bitJSON struct {
			Cap     string   `json:"cap"`
			Added   []string `json:"added"`
			Removed []string `json:"removed"`
		}
		grant := func(g authority.Grant) map[string]string {
			out := map[string]string{}
			for _, p := range grantParts(g) {
				out[strings.ReplaceAll(p[0], "-", "_")] = p[1]
			}
			return out
		}
		diffs := make([]map[string]any, 0, len(r.Diffs))
		for _, d := range r.Diffs {
			bits := []bitJSON{}
			for _, b := range WhatIfBits(d) {
				j := bitJSON{Cap: b.Cap, Added: b.Added, Removed: b.Removed}
				if j.Added == nil {
					j.Added = []string{}
				}
				if j.Removed == nil {
					j.Removed = []string{}
				}
				bits = append(bits, j)
			}
			m := map[string]any{"task": d.ID, "new": d.New, "after": grant(d.After), "bits": bits}
			if !d.New {
				m["before"] = grant(d.Before)
			}
			diffs = append(diffs, m)
		}
		return json.NewEncoder(w).Encode(map[string]any{
			"diffs": diffs, "command": r.Command, "run_by": r.RunBy, "partial": r.Partial,
		})
	}
	var b strings.Builder
	if r.Partial {
		b.WriteString("(over your listing only)\n")
	}
	if len(r.Diffs) == 0 {
		b.WriteString("  nothing changes\n")
		_, err := io.WriteString(w, b.String())
		return err
	}
	for _, d := range r.Diffs {
		b.WriteString("  " + WhatIfHead(d) + "\n")
		for _, bit := range WhatIfBits(d) {
			b.WriteString("      " + bit.Line() + "\n")
		}
	}
	if r.RunBy != "" {
		fmt.Fprintf(&b, "  command (run by %s): %s\n", shortID(r.RunBy), r.Command)
	} else {
		b.WriteString("  command: " + r.Command + "\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}
