package cli

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// A capability names a verb; a scope names which tasks that verb may be
// pointed at. The grammar is deliberately the same in both directions, so what
// `ls` prints can be pasted straight back into a `--scope`:
//
//	(omitted)                 self + descendants — the default
//	subtree                   the same, written out
//	none                      self only
//	global                    every task on the server
//	ids:<id>[,<id>]           self + exactly those tasks
//	subtree+ids:<id>[,<id>]   self + descendants + those tasks
//
// A bare ids: list implies base none, so `ids:X` and `none+ids:X` parse
// identically. `global+ids:` is rejected rather than accepted-and-ignored: a
// scope the user wrote and the server silently drops is the invisible
// divergence this design refuses everywhere else.

const scopeIDHexLen = 32

// ScopeGrammar is the one-line syntax summary, shared by flag help and
// `harness-cli caps`.
const ScopeGrammar = "subtree (default) | none | global | [subtree+]ids:<task-id>[,<task-id>]"

// ParseScope converts a --scope value into a wire TaskScope. Empty is the
// subtree default, which is also the zero value.
func ParseScope(s string) (protocol.TaskScope, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return protocol.TaskScope{Base: protocol.ScopeBase_Subtree}, nil
	}

	base, idList, hasIDs := strings.Cut(s, "ids:")
	base = strings.TrimSuffix(strings.TrimSpace(base), "+")

	var scopeBase protocol.ScopeBase
	switch base {
	case "", "none":
		// A bare "ids:…" has no written base; none is the implied one.
		scopeBase = protocol.ScopeBase_None
		if !hasIDs && base == "" {
			return protocol.TaskScope{}, fmt.Errorf("empty scope (valid: %s)", ScopeGrammar)
		}
	case "subtree":
		scopeBase = protocol.ScopeBase_Subtree
	case "global":
		if hasIDs {
			return protocol.TaskScope{}, fmt.Errorf(
				"scope %q: ids are meaningless under global, which already covers every task", s)
		}
		scopeBase = protocol.ScopeBase_Global
	default:
		return protocol.TaskScope{}, fmt.Errorf("unknown scope base %q (valid: %s)", base, ScopeGrammar)
	}
	if !hasIDs {
		return protocol.TaskScope{Base: scopeBase}, nil
	}

	seen := make(map[string]bool)
	var ids []string
	for _, raw := range strings.Split(idList, ",") {
		id := strings.ToLower(strings.TrimSpace(raw))
		if id == "" {
			continue
		}
		if len(id) != scopeIDHexLen {
			return protocol.TaskScope{}, fmt.Errorf(
				"scope id %q: want %d hex characters, got %d", raw, scopeIDHexLen, len(id))
		}
		if _, err := hex.DecodeString(id); err != nil {
			return protocol.TaskScope{}, fmt.Errorf("scope id %q is not hex", raw)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return protocol.TaskScope{}, fmt.Errorf("scope %q: ids: with no ids after it", s)
	}
	sort.Strings(ids)

	out := protocol.TaskScope{Base: scopeBase}
	for _, id := range ids {
		raw, _ := hex.DecodeString(id)
		var tid protocol.TaskID
		copy(tid.Id[:], raw)
		out.Ids = append(out.Ids, tid)
	}
	out.IdsLen = uint16(len(out.Ids))
	return out, nil
}

// ScopeLabel renders a TaskScope in the grammar ParseScope accepts. The zero
// value renders as "subtree".
func ScopeLabel(s protocol.TaskScope) string {
	base := s.Base.String()
	if len(s.Ids) == 0 {
		return base
	}
	ids := make([]string, 0, len(s.Ids))
	for _, id := range s.Ids {
		ids = append(ids, hex.EncodeToString(id.Id[:]))
	}
	sort.Strings(ids)
	list := "ids:" + strings.Join(ids, ",")
	if s.Base == protocol.ScopeBase_None {
		return list
	}
	return base + "+" + list
}

// IsDefaultScope reports whether s is the plain subtree default, so operator
// surfaces can leave the common case out of a crowded row.
func IsDefaultScope(s protocol.TaskScope) bool {
	return s.Base == protocol.ScopeBase_Subtree && len(s.Ids) == 0
}
