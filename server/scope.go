package server

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Scope is the server-side form of protocol.TaskScope plus its override list:
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
// exactly taskIDHexLen characters. Anything else is dropped on ingest rather
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
// An override narrows on the BASE axis only. It may name ids outside the base
// set — those were disclosed by the granter and join the visible set — while a
// wider base would reach targets nobody named, which is the enumeration leak
// the rank check refuses.
type ScopeOverride struct {
	Caps        protocol.Capability
	Base        protocol.ScopeBase
	ExcludeSelf bool
	IDs         []string
}

// taskIDHexLen is the hex width of a protocol.TaskID (16 bytes).
const taskIDHexLen = 32

// defaultScope is what a task gets when no scope is requested: self plus
// descendants, which is the visibility rule the server applied before scopes
// existed. It is also the zero value, deliberately — see ScopeBase in the
// schema.
func defaultScope() Scope { return Scope{Base: protocol.ScopeBase_Subtree} }

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

// capsLabelForMask renders a capability mask for error text, so a rejection
// names bits rather than a number. cli.CapsLabel does the same job for the
// operator surfaces; duplicating the walk here keeps server/ from importing
// cli/ for one string.
func capsLabelForMask(m protocol.Capability) string {
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

// validateScope enforces the rules that make "no capability acts outside what
// ls shows" hold by construction. Two of them are about authority and one is
// about encoding:
//
//   - The action base, and every override's base, must rank at or below the
//     visibility rank. Only the BASE axis is restricted, because only the base
//     reaches targets nobody named — an override may carry ids outside the
//     base, since a granted id was disclosed by the granter and joins the
//     visible set.
//   - Override masks are non-empty and pairwise disjoint, which is what keeps
//     ForCap a lookup.
//   - VisBase must be zero when VisBasePresent is not set. That one is wire
//     hygiene rather than authority: otherwise one authority has two
//     encodings, which compare unequal and render differently.
//
// Ids are deliberately NOT checked here. Their bound is the parent's effective
// set at grant time (attenuateScope), not the task's own base.
func validateScope(s Scope) error {
	vis := s.VisRank()
	if scopeBaseRank(s.Base) > scopeBaseRank(vis) {
		return fmt.Errorf("scope base %s outranks visibility %s", s.Base, vis)
	}
	if !s.VisBasePresent && s.VisBase != protocol.ScopeBase(0) {
		return fmt.Errorf("vis_base %s set while vis_base_present is 0 (non-canonical encoding)", s.VisBase)
	}
	var seen protocol.Capability
	for _, o := range s.Overrides {
		if o.Caps == protocol.Capability_None {
			return fmt.Errorf("scope override with an empty capability mask")
		}
		if overlap := seen & o.Caps; overlap != 0 {
			return fmt.Errorf("scope override masks intersect at %s", capsLabelForMask(overlap))
		}
		seen |= o.Caps
		if scopeBaseRank(o.Base) > scopeBaseRank(vis) {
			return fmt.Errorf("scope override for %s: base %s outranks visibility %s",
				capsLabelForMask(o.Caps), o.Base, vis)
		}
	}
	return nil
}

// scopeBaseRank orders bases by permissiveness. It is NOT the numeric value:
// subtree is 0 so that a zero reads as the default, which puts the numbers out
// of order. An unrecognised base — a byte from a peer built against a newer
// schema — ranks as none, so an unknown widening fails closed.
func scopeBaseRank(b protocol.ScopeBase) int {
	switch b {
	case protocol.ScopeBase_Subtree:
		return 1
	case protocol.ScopeBase_Global:
		return 2
	default: // ScopeBase_None and anything unrecognised
		return 0
	}
}

// minScopeBase returns the less permissive of the two. Ties keep a, so an
// unrecognised base is preserved rather than being rewritten to none — the
// rank already makes it behave as none, and keeping the byte means `ls` shows
// the operator what the task actually asked for.
func minScopeBase(a, b protocol.ScopeBase) protocol.ScopeBase {
	if scopeBaseRank(b) < scopeBaseRank(a) {
		return b
	}
	return a
}

// normalizeScopeIDs lower-cases, validates, sorts and dedupes id hexes.
func normalizeScopeIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.ToLower(strings.TrimSpace(id))
		if len(id) != taskIDHexLen {
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

// idsFromWire / idsToWire are the one place task-id hex crosses the wire
// boundary, so the three id lists (action, view-only, per-override) cannot
// drift in how they normalise.
func idsFromWire(in []protocol.TaskID) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, id := range in {
		out = append(out, hex.EncodeToString(id.Id[:]))
	}
	return normalizeScopeIDs(out)
}

// idsToWire skips ids that are not valid task-id hex: an unencodable target is
// a client bug, not a reason to fail the whole response.
func idsToWire(in []string) []protocol.TaskID {
	var out []protocol.TaskID
	for _, h := range in {
		raw, err := hex.DecodeString(h)
		if err != nil || len(raw) != taskIDHexLen/2 {
			continue
		}
		var tid protocol.TaskID
		copy(tid.Id[:], raw)
		out = append(out, tid)
	}
	return out
}

// scopeFromWire converts a decoded TaskScope plus its sibling override list
// into the server-side form. The override list is a separate wire field rather
// than a member of TaskScope, so it has to be passed in alongside — every call
// site that decodes one must pass the other.
func scopeFromWire(w protocol.TaskScope, ov []protocol.ScopeOverride) Scope {
	s := Scope{
		Base:           w.Base,
		IDs:            idsFromWire(w.Ids),
		VisBase:        w.VisBase,
		VisBasePresent: w.VisBasePresent(),
		ExcludeSelf:    w.ExcludeSelf(),
		VisIDs:         idsFromWire(w.VisIds),
	}
	for _, o := range ov {
		so := ScopeOverride{Caps: o.Caps, Base: o.Base, ExcludeSelf: o.ExcludeSelf(), IDs: idsFromWire(o.Ids)}
		s.Overrides = append(s.Overrides, so)
	}
	return s
}

// toWire converts back for TaskInfo / WhoAmIResponse.
//
// It returns the scope only. The override list is a SIBLING field on every
// format that carries a scope, so a mapper has to set both — see
// overridesToWire. That split is why server/mapper_completeness_test.go exists:
// it fails when a mapper populates one and forgets the other.
func (s Scope) toWire() protocol.TaskScope {
	out := protocol.TaskScope{Base: s.Base, VisBase: s.VisBase}
	out.SetVisBasePresent(s.VisBasePresent)
	out.SetExcludeSelf(s.ExcludeSelf)
	out.Ids = idsToWire(s.IDs)
	out.IdsLen = uint16(len(out.Ids))
	out.VisIds = idsToWire(s.VisIDs)
	out.VisIdsLen = uint16(len(out.VisIds))
	return out
}

// overridesToWire is the other half of toWire. Kept as its own method rather
// than folded into a struct return so the call sites read as two assignments
// to two wire fields, which is what they are.
func (s Scope) overridesToWire() []protocol.ScopeOverride {
	var out []protocol.ScopeOverride
	for _, o := range s.Overrides {
		w := protocol.ScopeOverride{Caps: o.Caps, Base: o.Base}
		w.SetExcludeSelf(o.ExcludeSelf)
		w.Ids = idsToWire(o.IDs)
		w.IdsLen = uint16(len(w.Ids))
		out = append(out, w)
	}
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

// applyToWAL / scopeFromWAL are the WAL projection, kept next to the type so
// the two directions cannot drift apart.
//
// applyToWAL writes into the event rather than returning a tuple: with six
// fields to carry, a caller that populated some and forgot the rest would
// produce a record that replays as a DIFFERENT authority, and nothing would
// say so.
func (s Scope) applyToWAL(ev *WALEvent) {
	ev.ScopeBase = uint8(s.Base)
	ev.ScopeIDs = s.IDs
	ev.ScopeVisBase = uint8(s.VisBase)
	ev.ScopeVisBasePresent = s.VisBasePresent
	ev.ScopeExcludeSelf = s.ExcludeSelf
	ev.ScopeVisIDs = s.VisIDs
	ev.ScopeOverrides = nil
	for _, o := range s.Overrides {
		ev.ScopeOverrides = append(ev.ScopeOverrides, WALScopeOverride{
			Caps: uint32(o.Caps), Base: uint8(o.Base), ExcludeSelf: o.ExcludeSelf, IDs: o.IDs,
		})
	}
}

// scopeFromWAL reconstructs a Scope from a record.
//
// The migration here is ADDITIVE. A legacy record whose caps carry
// board_observe (formerly info_global, which gated task visibility as well)
// gains global visibility; its capability mask is never touched. Clearing the
// bit would silently revoke board observation at the moment of a restart, with
// no operator action requesting it and no way afterwards to tell the result
// apart from a deliberate narrowing.
//
// The condition is !VisBasePresent, so it fires only for records written
// before the axis existed — a task explicitly granted a visibility rank keeps
// the one it was given.
func scopeFromWAL(ev WALEvent) Scope {
	s := Scope{
		Base:           protocol.ScopeBase(ev.ScopeBase),
		IDs:            normalizeScopeIDs(ev.ScopeIDs),
		VisBase:        protocol.ScopeBase(ev.ScopeVisBase),
		VisBasePresent: ev.ScopeVisBasePresent,
		ExcludeSelf:    ev.ScopeExcludeSelf,
		VisIDs:         normalizeScopeIDs(ev.ScopeVisIDs),
	}
	for _, o := range ev.ScopeOverrides {
		s.Overrides = append(s.Overrides, ScopeOverride{
			Caps: protocol.Capability(o.Caps), Base: protocol.ScopeBase(o.Base),
			ExcludeSelf: o.ExcludeSelf, IDs: normalizeScopeIDs(o.IDs),
		})
	}
	if !s.VisBasePresent && protocol.Capability(ev.Capabilities)&protocol.Capability_BoardObserve != 0 {
		s.VisBasePresent = true
		s.VisBase = protocol.ScopeBase_Global
	}
	return s
}
