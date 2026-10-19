package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// WriteCapsAudit renders the audit rows as text: one line per bit with its
// tier, holder count and holders. With focus set, only that bit, one holder
// per line, each followed by the `caps set` line that would take the bit away
// (`--caps` alone keeps the task's scope). tasks supplies the current masks
// for those lines.
func WriteCapsAudit(w io.Writer, rows []AuditRow, focus protocol.Capability, tasks []protocol.TaskInfo) error {
	if focus != 0 {
		return writeCapsAuditFocus(w, rows, focus, tasks)
	}
	if len(rows) == 0 {
		_, err := fmt.Fprintln(w, "no task holds a capability (use --all to include ended tasks)")
		return err
	}
	for _, r := range rows {
		holders := make([]string, len(r.Holders))
		for i, h := range r.Holders {
			holders[i] = auditHolderText(h)
		}
		line := fmt.Sprintf("%-15s %s  %d  %s", r.Cap.String(), r.Tier.Label(), len(r.Holders), strings.Join(holders, "  "))
		if note := impliedNote(r.ImpliedBy); note != "" {
			line += "  " + note
		}
		if _, err := fmt.Fprintln(w, strings.TrimRight(line, " ")); err != nil {
			return err
		}
	}
	return nil
}

func writeCapsAuditFocus(w io.Writer, rows []AuditRow, focus protocol.Capability, tasks []protocol.TaskInfo) error {
	caps := map[string]protocol.Capability{}
	for _, t := range tasks {
		caps[fmt.Sprintf("%x", t.Id.Id[:])] = t.Capabilities
	}
	for _, r := range rows {
		if r.Cap != focus {
			continue
		}
		fmt.Fprintf(w, "%s %s  %d holder(s)  %s\n", r.Cap.String(), r.Tier.Label(), len(r.Holders), impliedNote(r.ImpliedBy))
		for _, h := range r.Holders {
			fmt.Fprintf(w, "  %s  parent=%s\n", auditHolderText(h), h.Parent)
			fmt.Fprintf(w, "    revoke: harness-cli caps set %s --caps %s\n", h.TaskID, CapsLabel(caps[h.TaskID]&^focus))
		}
		return nil
	}
	_, err := fmt.Fprintf(w, "no task holds %s (use --all to include ended tasks)\n", focus.String())
	return err
}

func auditHolderText(h AuditHolder) string {
	s := h.Label + "[" + auditScopeText(h) + "]"
	if !h.Live {
		s += "(ended)"
	}
	return s
}

func auditScopeText(h AuditHolder) string {
	if h.ScopeKind == verb.ScopeNone {
		return "scope 対象外"
	}
	if h.Override {
		return h.Scope + "*"
	}
	return h.Scope
}

func impliedNote(by []protocol.Capability) string {
	if len(by) == 0 {
		return ""
	}
	names := make([]string, len(by))
	for i, c := range by {
		names[i] = c.String()
	}
	return "(also: " + strings.Join(names, ", ") + ")"
}

// AuditRowJSON is one row as the CLI's --json and the WebUI bridge carry it.
type AuditRowJSON struct {
	Cap       string            `json:"cap"`
	Tier      string            `json:"tier"`
	ImpliedBy []string          `json:"implied_by"`
	Holders   []AuditHolderJSON `json:"holders"`
}

// AuditHolderJSON is one holder in AuditRowJSON.
type AuditHolderJSON struct {
	TaskID    string `json:"task_id"`
	Label     string `json:"label"`
	Parent    string `json:"parent"`
	Live      bool   `json:"live"`
	ScopeKind string `json:"scope_kind"` // "own" | "none"
	Scope     string `json:"scope"`
	Breadth   string `json:"breadth"` // "global" | "subtree" | "narrow" | "" for scope_kind none
	Override  bool   `json:"override"`
	TaskScope string `json:"task_scope"`
}

// AuditRowsJSON converts rows to their JSON form.
func AuditRowsJSON(rows []AuditRow) []AuditRowJSON {
	out := make([]AuditRowJSON, len(rows))
	for i, r := range rows {
		j := AuditRowJSON{Cap: r.Cap.String(), Tier: r.Tier.Label(), ImpliedBy: []string{}, Holders: []AuditHolderJSON{}}
		for _, c := range r.ImpliedBy {
			j.ImpliedBy = append(j.ImpliedBy, c.String())
		}
		for _, h := range r.Holders {
			hj := AuditHolderJSON{TaskID: h.TaskID, Label: h.Label, Parent: h.Parent, Live: h.Live,
				ScopeKind: "own", Scope: h.Scope, Override: h.Override, TaskScope: h.TaskScope}
			if h.ScopeKind == verb.ScopeNone {
				hj.ScopeKind = "none"
			} else {
				hj.Breadth = [...]string{"global", "subtree", "narrow"}[h.Breadth]
			}
			j.Holders = append(j.Holders, hj)
		}
		out[i] = j
	}
	return out
}

// WriteCapsAuditJSON writes one JSON object per row (JSON Lines), the form
// the other listing verbs use.
func WriteCapsAuditJSON(w io.Writer, rows []AuditRow) error {
	enc := json.NewEncoder(w)
	for _, r := range AuditRowsJSON(rows) {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}
