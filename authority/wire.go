package authority

import (
	"encoding/hex"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

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
	return NormalizeIDs(out)
}

// idsToWire skips ids that are not valid task-id hex: an unencodable target is
// a client bug, not a reason to fail the whole response.
func idsToWire(in []string) []protocol.TaskID {
	var out []protocol.TaskID
	for _, h := range in {
		raw, err := hex.DecodeString(h)
		if err != nil || len(raw) != TaskIDHexLen/2 {
			continue
		}
		var tid protocol.TaskID
		copy(tid.Id[:], raw)
		out = append(out, tid)
	}
	return out
}

// ScopeFromWire converts a decoded TaskScope plus its sibling override list
// into the server-side form. The override list is a separate wire field rather
// than a member of TaskScope, so it has to be passed in alongside — every call
// site that decodes one must pass the other.
func ScopeFromWire(w protocol.TaskScope, ov []protocol.ScopeOverride) Scope {
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

// ToWire converts back for TaskInfo / WhoAmIResponse.
//
// It returns the scope only. The override list is a SIBLING field on every
// format that carries a scope, so a mapper has to set both — see
// OverridesToWire. That split is why server/mapper_completeness_test.go exists:
// it fails when a mapper populates one and forgets the other.
func (s Scope) ToWire() protocol.TaskScope {
	out := protocol.TaskScope{Base: s.Base, VisBase: s.VisBase}
	out.SetVisBasePresent(s.VisBasePresent)
	out.SetExcludeSelf(s.ExcludeSelf)
	out.Ids = idsToWire(s.IDs)
	out.IdsLen = uint16(len(out.Ids))
	out.VisIds = idsToWire(s.VisIDs)
	out.VisIdsLen = uint16(len(out.VisIds))
	return out
}

// OverridesToWire is the other half of ToWire. Kept as its own method rather
// than folded into a struct return so the call sites read as two assignments
// to two wire fields, which is what they are.
func (s Scope) OverridesToWire() []protocol.ScopeOverride {
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
