package cli

import (
	"encoding/hex"
	"sort"
	"strings"

	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// ScopeBreadth is how far a resolved scope reaches, for colouring a holder in
// the capability audit.
type ScopeBreadth int

const (
	BreadthGlobal ScopeBreadth = iota
	BreadthSubtree
	BreadthNarrow
)

// AuditHolder is one task holding one bit.
type AuditHolder struct {
	TaskID, Label, Parent string
	Live                  bool
	ScopeKind             verb.CapScopeKind
	Scope                 string // the bit's resolved scope; "" for ScopeNone
	Breadth               ScopeBreadth
	Override              bool // Scope came from a --scope-for override
	TaskScope             string
}

// AuditRow is one capability and every task holding it literally.
type AuditRow struct {
	Cap       protocol.Capability
	Tier      verb.CapTier
	ImpliedBy []protocol.Capability
	Holders   []AuditHolder
}

// CapsAudit builds the per-capability holder rows every audit surface renders:
// the CLI's `caps audit`, the TUI `i` modal and the WebUI 権限 tab. Rows follow
// verb.AuditOrder. Ended tasks are left out unless includeEnded, and bits nobody
// holds unless includeEmpty.
func CapsAudit(tasks []protocol.TaskInfo, includeEnded, includeEmpty bool) []AuditRow {
	var rows []AuditRow
	for _, c := range verb.AuditOrder() {
		row := AuditRow{Cap: c, Tier: verb.CapTierOf(c), ImpliedBy: verb.CapImpliedBy(c)}
		for _, t := range tasks {
			if t.Capabilities&c != c {
				continue
			}
			live := !isTerminalStatus(t.Status)
			if !live && !includeEnded {
				continue
			}
			row.Holders = append(row.Holders, auditHolder(t, c, live))
		}
		if len(row.Holders) == 0 && !includeEmpty {
			continue
		}
		sort.SliceStable(row.Holders, func(i, j int) bool {
			a, b := row.Holders[i], row.Holders[j]
			if a.ScopeKind != b.ScopeKind {
				return a.ScopeKind < b.ScopeKind
			}
			if a.Breadth != b.Breadth {
				return a.Breadth < b.Breadth
			}
			return a.TaskID < b.TaskID
		})
		rows = append(rows, row)
	}
	return rows
}

func auditHolder(t protocol.TaskInfo, c protocol.Capability, live bool) AuditHolder {
	h := AuditHolder{
		TaskID:    hex.EncodeToString(t.Id.Id[:]),
		Label:     AuditLabel(t),
		Parent:    "operator",
		Live:      live,
		ScopeKind: verb.CapScopeKindOf(c),
		TaskScope: ScopeLabel(t.Scope),
	}
	if t.CreatorTaskId.Id != ([16]byte{}) {
		h.Parent = hex.EncodeToString(t.CreatorTaskId.Id[:])[:8]
	}
	if h.ScopeKind == verb.ScopeNone {
		return h
	}
	h.Scope = ResolvedScopeByCap(t.Capabilities, t.Scope, t.Overrides)[c.String()]
	h.Breadth = ScopeBreadthOf(h.Scope)
	for _, o := range t.Overrides {
		if o.Caps&c != 0 {
			h.Override = true
		}
	}
	return h
}

// ScopeBreadthOf classifies a scope label by its ACTION half: a
// "<visibility>/<action>" label is coloured by what the task may act on.
func ScopeBreadthOf(scope string) ScopeBreadth {
	if i := strings.LastIndex(scope, "/"); i >= 0 {
		scope = scope[i+1:]
	}
	switch {
	case strings.HasPrefix(scope, "global"):
		return BreadthGlobal
	case strings.HasPrefix(scope, "subtree"), strings.HasPrefix(scope, "descendants"):
		return BreadthSubtree
	default:
		return BreadthNarrow
	}
}

// AuditLabel names a task the way the TUI picker's rows do: id8, agent, and
// the head of the prompt, with empty parts dropped — a redacted parent row has
// neither agent nor prompt.
func AuditLabel(t protocol.TaskInfo) string {
	parts := []string{hex.EncodeToString(t.Id.Id[:])[:8]}
	if a := string(t.AgentProfile); a != "" {
		parts = append(parts, a)
	}
	if p := strings.Join(strings.Fields(string(t.Prompt)), " "); p != "" {
		if r := []rune(p); len(r) > 24 {
			p = string(r[:24])
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, " ")
}

func isTerminalStatus(s protocol.TaskStatus) bool {
	switch s {
	case protocol.TaskStatus_Succeeded, protocol.TaskStatus_Failed, protocol.TaskStatus_Cancelled:
		return true
	}
	return false
}
