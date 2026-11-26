package authority

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Grant conditions (docs/superpowers/specs/2026-11-26-grant-conditions-and-bound-enrollment-design.md,
// C4-C7, C9, C14): a condition on a ScopeOverride bounds WHICH OBJECT inside a
// target the override's bits reach — which action exec_run may run, which port
// a forward may use. Attenuation is containment (C5): a child's condition must
// be inside its granter's for the same bit.

// Limits on one request's body (C6).
const (
	MaxGrantConditions  = 8
	MaxConditionActions = 32
	MaxConditionSpans   = 8
)

// PortSpan is one inclusive port range, Lo <= Hi.
type PortSpan struct{ Lo, Hi uint16 }

// Condition is one override's condition. A *Condition is never mutated after
// it is built, so overrides copied by value may share one.
//
// Kind 0 is a condition whose body is unknown: a marker that arrived without a
// body, or a stored record whose kind this build does not know. It allows
// nothing and contains nothing (C7), so it can never read as unconditioned.
type Condition struct {
	Kind    protocol.ConditionKind
	Actions [][16]byte // action_set: sorted, unique
	Spans   []PortSpan // port_range: sorted, overlapping and adjacent spans merged
}

// Object is what an object-aware gate asks a condition about.
type Object struct {
	Kind   protocol.ConditionKind
	Action [16]byte
	Port   uint16
}

// ApplicableBits is the mask a condition kind may be attached to (C9, C14).
func ApplicableBits(k protocol.ConditionKind) protocol.Capability {
	switch k {
	case protocol.ConditionKind_ActionSet:
		return protocol.Capability_ExecRun
	case protocol.ConditionKind_PortRange:
		return protocol.Capability_ForwardLocal | protocol.Capability_ForwardRemote
	}
	return protocol.Capability_None
}

// NewCondition normalises a condition: actions sorted and deduplicated, spans
// sorted and merged, so Contains is exact and two equal conditions have one
// form. It refuses what Validate refuses for a condition on its own.
func NewCondition(kind protocol.ConditionKind, actions [][16]byte, spans []PortSpan) (*Condition, error) {
	c := &Condition{Kind: kind}
	switch kind {
	case protocol.ConditionKind_ActionSet:
		if len(spans) != 0 {
			return nil, fmt.Errorf("an action_set condition carries port spans")
		}
		c.Actions = normalizeActions(actions)
	case protocol.ConditionKind_PortRange:
		if len(actions) != 0 {
			return nil, fmt.Errorf("a port_range condition carries action ids")
		}
		for _, s := range spans {
			if s.Lo > s.Hi {
				return nil, fmt.Errorf("port span %d-%d: low end above high end", s.Lo, s.Hi)
			}
		}
		c.Spans = mergeSpans(spans)
	default:
		return nil, fmt.Errorf("unknown condition kind %d", kind)
	}
	if err := c.check(); err != nil {
		return nil, err
	}
	return c, nil
}

// check is the per-condition half of Validate: a known kind and the limits.
// Kind 0 passes: it is a state (body unknown), not something a writer sent.
func (c *Condition) check() error {
	switch c.Kind {
	case 0:
		return nil
	case protocol.ConditionKind_ActionSet:
		if len(c.Actions) > MaxConditionActions {
			return fmt.Errorf("action_set names %d actions; at most %d", len(c.Actions), MaxConditionActions)
		}
	case protocol.ConditionKind_PortRange:
		if len(c.Spans) > MaxConditionSpans {
			return fmt.Errorf("port_range names %d spans; at most %d", len(c.Spans), MaxConditionSpans)
		}
		for _, s := range c.Spans {
			if s.Lo > s.Hi {
				return fmt.Errorf("port span %d-%d: low end above high end", s.Lo, s.Hi)
			}
		}
	default:
		return fmt.Errorf("unknown condition kind %d", c.Kind)
	}
	return nil
}

func normalizeActions(in [][16]byte) [][16]byte {
	if len(in) == 0 {
		return nil
	}
	out := append([][16]byte(nil), in...)
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i][:], out[j][:]) < 0 })
	w := 1
	for i := 1; i < len(out); i++ {
		if out[i] != out[w-1] {
			out[w] = out[i]
			w++
		}
	}
	return out[:w]
}

func mergeSpans(in []PortSpan) []PortSpan {
	if len(in) == 0 {
		return nil
	}
	s := append([]PortSpan(nil), in...)
	sort.Slice(s, func(i, j int) bool { return s[i].Lo < s[j].Lo })
	out := []PortSpan{s[0]}
	for _, x := range s[1:] {
		last := &out[len(out)-1]
		if uint32(x.Lo) <= uint32(last.Hi)+1 {
			if x.Hi > last.Hi {
				last.Hi = x.Hi
			}
			continue
		}
		out = append(out, x)
	}
	return out
}

// Allows reports whether the condition admits o. A nil condition is
// unconditioned and admits everything; kind 0 admits nothing.
func (c *Condition) Allows(o Object) bool {
	if c == nil {
		return true
	}
	if c.Kind == 0 || c.Kind != o.Kind {
		return false
	}
	switch c.Kind {
	case protocol.ConditionKind_ActionSet:
		for _, a := range c.Actions {
			if a == o.Action {
				return true
			}
		}
	case protocol.ConditionKind_PortRange:
		for _, s := range c.Spans {
			if s.Lo <= o.Port && o.Port <= s.Hi {
				return true
			}
		}
	}
	return false
}

// Contains reports whether every object d admits, c admits too. nil (no
// condition) contains everything and is contained only by nil; kind 0
// contains nothing and is contained only by nil.
func (c *Condition) Contains(d *Condition) bool {
	if c == nil {
		return true
	}
	if d == nil || c.Kind == 0 || d.Kind == 0 || c.Kind != d.Kind {
		return false
	}
	switch c.Kind {
	case protocol.ConditionKind_ActionSet:
		for _, a := range d.Actions {
			if !c.Allows(Object{Kind: c.Kind, Action: a}) {
				return false
			}
		}
		return true
	case protocol.ConditionKind_PortRange:
		// c's spans are merged, so a span of d is inside c only if it is
		// inside one span of c.
		for _, x := range d.Spans {
			in := false
			for _, s := range c.Spans {
				if s.Lo <= x.Lo && x.Hi <= s.Hi {
					in = true
					break
				}
			}
			if !in {
				return false
			}
		}
		return true
	}
	return false
}

// Intersect is the condition admitting what both admit. nil is the identity.
// Kind 0, or two different kinds, give kind 0 (admits nothing); an empty
// overlap keeps the kind with no members, which also admits nothing.
func (c *Condition) Intersect(d *Condition) *Condition {
	if c == nil {
		return d.Clone()
	}
	if d == nil {
		return c.Clone()
	}
	if c.Kind == 0 || d.Kind == 0 || c.Kind != d.Kind {
		return &Condition{}
	}
	out := &Condition{Kind: c.Kind}
	switch c.Kind {
	case protocol.ConditionKind_ActionSet:
		for _, a := range c.Actions {
			if d.Allows(Object{Kind: c.Kind, Action: a}) {
				out.Actions = append(out.Actions, a)
			}
		}
	case protocol.ConditionKind_PortRange:
		for _, x := range c.Spans {
			for _, y := range d.Spans {
				lo, hi := max(x.Lo, y.Lo), min(x.Hi, y.Hi)
				if lo <= hi {
					out.Spans = append(out.Spans, PortSpan{lo, hi})
				}
			}
		}
		out.Spans = mergeSpans(out.Spans)
	}
	return out
}

// Clone returns a copy that shares no slice with c.
func (c *Condition) Clone() *Condition {
	if c == nil {
		return nil
	}
	return &Condition{
		Kind:    c.Kind,
		Actions: append([][16]byte(nil), c.Actions...),
		Spans:   append([]PortSpan(nil), c.Spans...),
	}
}

// Equal reports whether the two admit the same objects (both normalised).
func (c *Condition) Equal(d *Condition) bool { return c.key() == d.key() }

// key is a canonical string for grouping and comparing; "" for nil.
func (c *Condition) key() string {
	if c == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(strconv.Itoa(int(c.Kind)))
	for _, a := range c.Actions {
		b.WriteString(":" + hex.EncodeToString(a[:]))
	}
	for _, s := range c.Spans {
		fmt.Fprintf(&b, ":%d-%d", s.Lo, s.Hi)
	}
	return b.String()
}

// Label renders the condition for display: "actions:<hex>,…",
// "ports:8000-8100,9000", or "(condition unknown)" for kind 0.
func (c *Condition) Label() string {
	if c == nil {
		return ""
	}
	switch c.Kind {
	case protocol.ConditionKind_ActionSet:
		parts := make([]string, 0, len(c.Actions))
		for _, a := range c.Actions {
			parts = append(parts, hex.EncodeToString(a[:]))
		}
		return "actions:" + strings.Join(parts, ",")
	case protocol.ConditionKind_PortRange:
		parts := make([]string, 0, len(c.Spans))
		for _, s := range c.Spans {
			if s.Lo == s.Hi {
				parts = append(parts, strconv.Itoa(int(s.Lo)))
			} else {
				parts = append(parts, fmt.Sprintf("%d-%d", s.Lo, s.Hi))
			}
		}
		return "ports:" + strings.Join(parts, ",")
	}
	return "(condition unknown)"
}

// CondFor is the condition on the override covering bit, or nil. The base
// clause carries none: a condition attaches to an override only.
func (s Scope) CondFor(bit protocol.Capability) *Condition {
	for _, o := range s.Overrides {
		if o.Caps&bit != 0 {
			return o.Cond
		}
	}
	return nil
}

// NamesAction reports whether any action_set condition in s lists id: s's
// holder is then one of the action's holders (C11), whose runs an edit of it
// changes.
func (s Scope) NamesAction(id [16]byte) bool {
	for _, o := range s.Overrides {
		if o.Cond == nil || o.Cond.Kind != protocol.ConditionKind_ActionSet {
			continue
		}
		for _, a := range o.Cond.Actions {
			if a == id {
				return true
			}
		}
	}
	return false
}

// validateCond is Validate's check of one override's condition.
func validateCond(o ScopeOverride) error {
	if o.Cond == nil {
		return nil
	}
	if err := o.Cond.check(); err != nil {
		return fmt.Errorf("condition on %s: %w", CapsLabel(o.Caps), err)
	}
	if o.Cond.Kind != 0 {
		if extra := o.Caps &^ ApplicableBits(o.Cond.Kind); extra != 0 {
			return fmt.Errorf("a %s condition does not apply to %s", o.Cond.Kind, CapsLabel(extra))
		}
	}
	return nil
}

// WiderConditionError is a child condition not contained in the granter's
// for the same bit (C5).
type WiderConditionError struct{ Caps protocol.Capability }

func (e *WiderConditionError) Error() string {
	return "the condition on " + CapsLabel(e.Caps) + " is wider than yours"
}

// widerPrefix marks Attenuate's offender string for a wider condition.
const widerPrefix = "wider condition: "

// applyConds rewrites s so that every exercised bit carries final(bit, the
// condition its entry carries now). An entry whose bits come out with
// different conditions is split by condition; a base-clause bit that comes
// out conditioned moves into an override with the base clause's rank,
// exclude_self and ids, since a condition attaches to an override only.
// Bits not exercised keep their entry's condition. A refusal from final
// returns the refused bit.
func applyConds(s Scope, exercised protocol.Capability, final func(bit protocol.Capability, cur *Condition) (*Condition, bool)) (Scope, protocol.Capability, bool) {
	type group struct {
		mask protocol.Capability
		cond *Condition
	}
	var refused protocol.Capability
	regroup := func(bits protocol.Capability, cur *Condition, keepNil bool) []group {
		var gs []group
		idx := map[string]int{}
		for bit := protocol.Capability(1); bit != 0 && bit <= protocol.Capability_All; bit <<= 1 {
			if bits&bit == 0 {
				continue
			}
			c := cur
			if exercised&bit != 0 {
				var ok bool
				if c, ok = final(bit, cur); !ok {
					refused = bit
					return nil
				}
			}
			if c == nil && !keepNil {
				continue
			}
			k := c.key()
			if i, ok := idx[k]; ok {
				gs[i].mask |= bit
				continue
			}
			idx[k] = len(gs)
			gs = append(gs, group{bit, c})
		}
		return gs
	}

	out := s
	out.Overrides = nil
	var overridden protocol.Capability
	for _, o := range s.Overrides {
		overridden |= o.Caps
		gs := regroup(o.Caps, o.Cond, true)
		if refused != 0 {
			return Scope{}, refused, false
		}
		for _, g := range gs {
			p := o
			p.Caps, p.Cond = g.mask, g.cond
			p.IDs = append([]string(nil), o.IDs...)
			out.Overrides = append(out.Overrides, p)
		}
	}
	gs := regroup(exercised&^overridden&^protocol.Capability_Spawn, nil, false)
	if refused != 0 {
		return Scope{}, refused, false
	}
	for _, g := range gs {
		out.Overrides = append(out.Overrides, ScopeOverride{
			Caps: g.mask, Base: s.Base, ExcludeSelf: s.ExcludeSelf,
			IDs: append([]string(nil), s.IDs...), Cond: g.cond,
		})
	}
	return out, 0, true
}

// ConditionsFromWire attaches a request's bodies to the overrides
// ScopeFromWire marked (Cond kind 0). It refuses, whole: more than
// MaxGrantConditions bodies; a body whose mask is not exactly one marked
// override's; two bodies for one override; a body that does not normalise or
// whose kind does not apply to its bits; and a marked override left without a
// body (C6). A refused scope must not be used.
func ConditionsFromWire(body protocol.GrantConditions, s *Scope) error {
	if len(body.Conditions) > MaxGrantConditions {
		return fmt.Errorf("%d grant conditions; at most %d", len(body.Conditions), MaxGrantConditions)
	}
	for _, w := range body.Conditions {
		i := -1
		for j, o := range s.Overrides {
			if o.Caps == w.Caps {
				i = j
				break
			}
		}
		if i < 0 || s.Overrides[i].Cond == nil {
			return fmt.Errorf("a grant condition names %s, which is not a conditioned --scope-for override", CapsLabel(w.Caps))
		}
		if s.Overrides[i].Cond.Kind != 0 {
			return fmt.Errorf("two grant conditions name %s", CapsLabel(w.Caps))
		}
		c, err := ConditionFromWire(w)
		if err != nil {
			return err
		}
		s.Overrides[i].Cond = c
	}
	for _, o := range s.Overrides {
		if o.Cond != nil && o.Cond.Kind == 0 {
			return fmt.Errorf("the override for %s is marked conditioned but no condition body names it", CapsLabel(o.Caps))
		}
	}
	return nil
}

// ConditionFromWire normalises one body and checks that its kind applies to
// every bit of its mask (C9).
func ConditionFromWire(w protocol.GrantCondition) (*Condition, error) {
	var acts [][16]byte
	var spans []PortSpan
	if a := w.ActionSet(); a != nil {
		for _, id := range a.Actions {
			acts = append(acts, id.Id)
		}
	}
	if p := w.PortRange(); p != nil {
		for _, sp := range p.Spans {
			spans = append(spans, PortSpan{sp.Lo, sp.Hi})
		}
	}
	c, err := NewCondition(w.Kind, acts, spans)
	if err != nil {
		return nil, fmt.Errorf("grant condition on %s: %w", CapsLabel(w.Caps), err)
	}
	if extra := w.Caps &^ ApplicableBits(w.Kind); extra != 0 {
		return nil, fmt.Errorf("a %s condition does not apply to %s", w.Kind, CapsLabel(extra))
	}
	return c, nil
}

// ConditionsToWire is the bodies of s's conditioned overrides, for the
// streamed reads (TaskInfo, ScheduleDetail) and for a client's request. A kind
// 0 condition has no body to send: its override's marker alone says
// "conditioned, body unknown".
func (s Scope) ConditionsToWire() []protocol.GrantCondition {
	var out []protocol.GrantCondition
	for _, o := range s.Overrides {
		if o.Cond == nil || o.Cond.Kind == 0 {
			continue
		}
		out = append(out, o.Cond.ToWire(o.Caps))
	}
	return out
}

// ToWire encodes c as the body of the override whose mask is caps.
func (c *Condition) ToWire(caps protocol.Capability) protocol.GrantCondition {
	w := protocol.GrantCondition{Caps: caps, Kind: c.Kind}
	switch c.Kind {
	case protocol.ConditionKind_ActionSet:
		var a protocol.ActionSetCondition
		ids := make([]protocol.ActionID, 0, len(c.Actions))
		for _, id := range c.Actions {
			ids = append(ids, protocol.ActionID{Id: id})
		}
		a.SetActions(ids)
		w.SetActionSet(a)
	case protocol.ConditionKind_PortRange:
		var p protocol.PortRangeCondition
		spans := make([]protocol.PortSpan, 0, len(c.Spans))
		for _, s := range c.Spans {
			spans = append(spans, protocol.PortSpan{Lo: s.Lo, Hi: s.Hi})
		}
		p.SetSpans(spans)
		w.SetPortRange(p)
	}
	return w
}

// capsFromLabel inverts CapsLabel, for the offender string Attenuate returns.
func capsFromLabel(label string) protocol.Capability {
	var m protocol.Capability
	for _, part := range strings.Split(label, ",") {
		for bit := protocol.Capability(1); bit != 0 && bit <= protocol.Capability_All; bit <<= 1 {
			if bit.String() == part {
				m |= bit
			}
		}
	}
	return m
}
