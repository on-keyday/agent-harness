package authority

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Scope is the in-memory form of protocol.TaskScope plus its override list:
// which tasks a task's capabilities may be pointed at. Capability says what
// verbs; Scope says what targets, and since the per-capability change it says
// so per verb.
//
//	visRank        = VisBasePresent ? VisBase : Base
//	visible        = {self} ∪ baseSet(visRank) ∪ VisIDs ∪ IDs ∪ ⋃ Overrides[].IDs
//	effective(cap) = (excludeSelf ? ∅ : {self}) ∪ baseSet(base) ∪ ids
//	                 taking base/excludeSelf/ids from the override covering cap,
//	                 else from this struct
//
// Two properties the field choices encode, both load-bearing:
//
//   - Every zero value is the pre-change reading. VisBasePresent = false makes
//     visibility follow Base, which pins the rank pair to the diagonal, and
//     ExcludeSelf = false keeps the unconditional self the base spec had. A
//     legacy WAL record and a zero struct therefore mean what they always did.
//   - Action IDs are visible without being repeated in VisIDs. An id written
//     into a grant was disclosed by the granter, so hiding it from ls protects
//     nothing — and it is what keeps every action set inside the visible set
//     while an override is still allowed to name targets outside the base.
//
// IDs are task-id hex, normalised to lower case, sorted and deduped, and
// exactly TaskIDHexLen characters. Anything else is dropped on ingest rather
// than carried as a target nothing can ever match.
type Scope struct {
	Base protocol.ScopeBase
	IDs  []string

	// VisBase is read only when VisBasePresent is set; it must be the zero
	// value otherwise, so one authority has exactly one encoding.
	VisBase        protocol.ScopeBase
	VisBasePresent bool
	// ExcludeSelf removes self from the ACTION set only. Self stays in the
	// visible set either way: seeing your own row is orientation, not
	// authority.
	ExcludeSelf bool
	// VisIDs are view-only extras — seen, never actionable.
	VisIDs []string
	// Overrides carry one scope per capability mask. Masks are pairwise
	// disjoint, validated where the value is written, so the entry covering a
	// capability is unique and lookup needs no precedence rule.
	Overrides []ScopeOverride
}

// ScopeOverride is one entry of Scope.Overrides: a capability MASK and the
// scope those bits resolve through. A mask rather than a single bit because
// "every write-ish bit gets the same narrowing" is the common case and would
// otherwise cost one entry per bit.
//
// An override may be wider OR narrower than the base, on any axis. In
// particular its rank is not bounded by the visibility rank: `scope=none
// +exec_view:global` is a task that can enumerate nothing and look at anything
// it is handed an id for, which is the shape a board-driven observer wants.
// See Validate for why the bound that used to exist was dropped.
//
// Ids named here join the VISIBLE set (see VisibleSet) even though the rank
// does not: an id written into a grant was disclosed by the granter, so hiding
// it from ls would protect nothing.
type ScopeOverride struct {
	Caps        protocol.Capability
	Base        protocol.ScopeBase
	ExcludeSelf bool
	IDs         []string
}

// TaskIDHexLen is the hex width of a protocol.TaskID (16 bytes).
const TaskIDHexLen = 32

// DefaultScope is what a task gets when no scope is requested: self plus
// descendants, which is the visibility rule the server applied before scopes
// existed. It is also the zero value, deliberately — see ScopeBase in the
// schema.
func DefaultScope() Scope { return Scope{Base: protocol.ScopeBase_Subtree} }

// VisRank is the visibility rank: VisBase when the presence bit is set, the
// action Base otherwise. The absent case pins the rank pair to the diagonal,
// which is why the zero value and every pre-change record are legal without a
// check of their own.
func (s Scope) VisRank() protocol.ScopeBase {
	if s.VisBasePresent {
		return s.VisBase
	}
	return s.Base
}

// ForCap resolves the scope ONE capability sees: the override covering it, or
// the base scope. Masks are validated pairwise disjoint where the value is
// written, so at most one entry matches and the first hit is the only hit —
// there is no precedence rule to get wrong.
//
// A mask may name bits the task does not hold. Those stay inert but retained,
// so a grant template survives reuse and a bit granted later by caps set picks
// its override up. Safe by construction: an override only ever narrows.
func (s Scope) ForCap(c protocol.Capability) (base protocol.ScopeBase, excludeSelf bool, ids []string) {
	for _, o := range s.Overrides {
		if o.Caps&c != 0 {
			return o.Base, o.ExcludeSelf, o.IDs
		}
	}
	return s.Base, s.ExcludeSelf, s.IDs
}

// CapsLabel renders a capability mask for error text, so a rejection
// names bits rather than a number.
func CapsLabel(m protocol.Capability) string {
	var parts []string
	for bit := protocol.Capability(1); bit != 0 && bit <= protocol.Capability_All; bit <<= 1 {
		if m&bit != 0 {
			parts = append(parts, bit.String())
		}
	}
	if len(parts) == 0 {
		return protocol.Capability_None.String()
	}
	return strings.Join(parts, ",")
}

// Validate checks a scope for internal CONSISTENCY. Nothing here is about
// how much authority the value grants — that is the operator's call, bounded
// at spawn by Attenuate and not at all for an operator's own set_caps.
//
//   - Override masks are non-empty and pairwise disjoint, which is what keeps
//     ForCap a lookup rather than a precedence walk.
//   - VisBase must be zero when VisBasePresent is not set, so one authority
//     has exactly one encoding.
//
// **The action rank is deliberately NOT compared against the visibility rank.**
// An earlier version required base <= visRank on the reasoning that `ls` should
// be a complete statement of what a task can reach. Dropped, because that rule
// conflated two properties this design elsewhere insists are different:
// un-enumerable and invisible.
//
// The shape it forbade is a real one and the operator wanted it: an agent that
// acts only on ids handed to it over the agentboard — snapshot this session,
// look at that task — and must NOT be able to enumerate the server. Written
// `scope=none +exec_view:global`, that is exactly "cannot list anything, can
// look at what it is told about". The old rule made the only way to get it a
// global visibility rank, which grants the enumeration the operator was trying
// to withhold.
//
// Nor did the rule protect much. Task ids are 128 random bits, so a wide action
// rank buys no discovery; it only lets a task act on ids it already holds. And
// the operator's view of the authority is the scope column, which states it in
// full either way — it was the TASK's own ls that stopped bounding its reach,
// which is introspection rather than confinement, and `whoami --json` answers
// that precisely with scope_by_cap.
//
// Ids are not checked here either. Their bound is the parent's effective set at
// grant time (Attenuate), not the task's own base.
func Validate(s Scope) error {
	if !s.VisBasePresent && s.VisBase != protocol.ScopeBase(0) {
		return fmt.Errorf("vis_base %s set while vis_base_present is 0 (non-canonical encoding)", s.VisBase)
	}
	var seen protocol.Capability
	for _, o := range s.Overrides {
		if o.Caps == protocol.Capability_None {
			return fmt.Errorf("scope override with an empty capability mask")
		}
		// spawn creates tasks and names none, so an override for it would be
		// accepted and bound nothing.
		if o.Caps&protocol.Capability_Spawn != 0 {
			return fmt.Errorf("scope override names spawn: spawn names no task; narrow resume / resume_exact instead")
		}
		if overlap := seen & o.Caps; overlap != 0 {
			return fmt.Errorf("scope override masks intersect at %s", CapsLabel(overlap))
		}
		seen |= o.Caps
	}
	return nil
}

// BaseRank orders bases by permissiveness. It is NOT the numeric value:
// subtree is 0 so that a zero reads as the default, which puts the numbers out
// of order. An unrecognised base — a byte from a peer built against a newer
// schema — ranks as none, so an unknown widening fails closed.
func BaseRank(b protocol.ScopeBase) int {
	switch b {
	case protocol.ScopeBase_Subtree:
		return 1
	case protocol.ScopeBase_Global:
		return 2
	default: // ScopeBase_None and anything unrecognised
		return 0
	}
}

// MinBase returns the less permissive of the two. Ties keep a, so an
// unrecognised base is preserved rather than being rewritten to none — the
// rank already makes it behave as none, and keeping the byte means `ls` shows
// the operator what the task actually asked for.
func MinBase(a, b protocol.ScopeBase) protocol.ScopeBase {
	if BaseRank(b) < BaseRank(a) {
		return b
	}
	return a
}

// NormalizeIDs lower-cases, validates, sorts and dedupes id hexes.
func NormalizeIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.ToLower(strings.TrimSpace(id))
		if len(id) != TaskIDHexLen {
			continue
		}
		if _, err := hex.DecodeString(id); err != nil {
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// String renders the scope in the same grammar the --scope flag accepts, so
// what `ls` prints can be pasted back into a spawn.
func (s Scope) String() string {
	base := s.Base.String()
	if len(s.IDs) == 0 {
		return base
	}
	ids := "ids:" + strings.Join(s.IDs, ",")
	if s.Base == protocol.ScopeBase_None {
		// none is the implied base of a bare id list; writing it would be noise.
		return ids
	}
	return base + "+" + ids
}
