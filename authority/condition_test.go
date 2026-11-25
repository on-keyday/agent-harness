package authority

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func aid(b byte) [16]byte {
	var id [16]byte
	for i := range id {
		id[i] = b
	}
	return id
}

func actions(ids ...byte) *Condition {
	c := &Condition{Kind: protocol.ConditionKind_ActionSet}
	for _, b := range ids {
		c.Actions = append(c.Actions, aid(b))
	}
	return c
}

func ports(spans ...PortSpan) *Condition {
	return &Condition{Kind: protocol.ConditionKind_PortRange, Spans: spans}
}

const (
	execRun = protocol.Capability_ExecRun
	fwdL    = protocol.Capability_ForwardLocal
	fwdR    = protocol.Capability_ForwardRemote
)

func TestConditionContains(t *testing.T) {
	unknown := &Condition{}
	for _, tc := range []struct {
		name string
		c, d *Condition
		want bool
	}{
		{"{a,b} ⊇ {a}", actions(1, 2), actions(1), true},
		{"{a} ⊉ {a,b}", actions(1), actions(1, 2), false},
		{"8000-8100 ⊇ 8050-8060", ports(PortSpan{8000, 8100}), ports(PortSpan{8050, 8060}), true},
		{"8000-8100 ⊉ 7999-8000", ports(PortSpan{8000, 8100}), ports(PortSpan{7999, 8000}), false},
		{"nil contains everything", nil, actions(1), true},
		{"nil contains nil", nil, nil, true},
		{"non-nil does not contain nil", actions(1, 2), nil, false},
		{"kind 0 contains nothing", unknown, actions(1), false},
		{"kind 0 does not contain kind 0", unknown, &Condition{}, false},
		{"kind 0 is contained by nil", nil, unknown, true},
		{"kind 0 is contained by nothing else", actions(1), unknown, false},
		{"kinds never contain each other", actions(1), ports(PortSpan{1, 2}), false},
		{"empty set is contained", actions(1), actions(), true},
	} {
		if got := tc.c.Contains(tc.d); got != tc.want {
			t.Errorf("%s: Contains = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestConditionIntersect(t *testing.T) {
	got := actions(1, 2, 3).Intersect(actions(2, 3, 4))
	if !got.Contains(actions(2, 3)) || !actions(2, 3).Contains(got) {
		t.Errorf("actions intersect = %+v, want {2,3}", got)
	}
	sp := ports(PortSpan{8000, 8100}, PortSpan{9000, 9000}).Intersect(ports(PortSpan{8050, 9500}))
	if !sp.Contains(ports(PortSpan{8050, 8100}, PortSpan{9000, 9000})) || sp.Allows(Object{Kind: protocol.ConditionKind_PortRange, Port: 8500}) {
		t.Errorf("ports intersect = %+v, want 8050-8100 and 9000", sp)
	}
	empty := actions(1).Intersect(actions(2))
	if empty == nil || empty.Kind != protocol.ConditionKind_ActionSet || len(empty.Actions) != 0 {
		t.Errorf("disjoint intersect = %+v, want an action_set with no members", empty)
	}
	if empty.Allows(Object{Kind: protocol.ConditionKind_ActionSet, Action: aid(1)}) {
		t.Error("an empty set allows an action")
	}
	if c := (*Condition)(nil).Intersect(actions(1)); c == nil || !c.Contains(actions(1)) {
		t.Errorf("nil ∩ {1} = %+v, want {1}", c)
	}
	if c := (&Condition{}).Intersect(actions(1)); c == nil || c.Allows(Object{Kind: protocol.ConditionKind_ActionSet, Action: aid(1)}) {
		t.Errorf("unknown ∩ {1} = %+v, want something that allows nothing", c)
	}
}

func TestConditionAllows(t *testing.T) {
	if (&Condition{}).Allows(Object{}) || (&Condition{}).Allows(Object{Kind: protocol.ConditionKind_PortRange, Port: 1}) {
		t.Error("kind 0 allowed an object")
	}
	p := ports(PortSpan{8000, 8100})
	for port, want := range map[uint16]bool{7999: false, 8000: true, 8100: true, 8101: false} {
		if got := p.Allows(Object{Kind: protocol.ConditionKind_PortRange, Port: port}); got != want {
			t.Errorf("port %d: %v, want %v", port, got, want)
		}
	}
	if !actions(1).Allows(Object{Kind: protocol.ConditionKind_ActionSet, Action: aid(1)}) ||
		actions(1).Allows(Object{Kind: protocol.ConditionKind_ActionSet, Action: aid(2)}) {
		t.Error("action membership wrong")
	}
	if p.Allows(Object{Kind: protocol.ConditionKind_ActionSet, Action: aid(1)}) {
		t.Error("a port range allowed an action")
	}
	if !(*Condition)(nil).Allows(Object{Kind: protocol.ConditionKind_PortRange, Port: 1}) {
		t.Error("nil (unconditioned) refused an object")
	}
}

func TestValidateConditionApplicability(t *testing.T) {
	ok := Scope{Overrides: []ScopeOverride{
		{Caps: execRun, Cond: actions(1)},
		{Caps: fwdL | fwdR, Cond: ports(PortSpan{1, 2})},
	}}
	if err := Validate(ok); err != nil {
		t.Fatalf("applicable conditions refused: %v", err)
	}
	many := make([]byte, MaxConditionActions+1)
	for i := range many {
		many[i] = byte(i + 1)
	}
	spans := make([]PortSpan, MaxConditionSpans+1)
	for i := range spans {
		spans[i] = PortSpan{uint16(10 * i), uint16(10*i + 1)}
	}
	for name, s := range map[string]Scope{
		"action_set on exec_run|cancel": {Overrides: []ScopeOverride{{Caps: execRun | protocol.Capability_Cancel, Cond: actions(1)}}},
		"port_range on forward_link":    {Overrides: []ScopeOverride{{Caps: protocol.Capability_ForwardLink, Cond: ports(PortSpan{1, 2})}}},
		"port_range on exec_run":        {Overrides: []ScopeOverride{{Caps: execRun, Cond: ports(PortSpan{1, 2})}}},
		"too many actions":              {Overrides: []ScopeOverride{{Caps: execRun, Cond: actions(many...)}}},
		"too many spans":                {Overrides: []ScopeOverride{{Caps: fwdL, Cond: ports(spans...)}}},
		"lo > hi":                       {Overrides: []ScopeOverride{{Caps: fwdL, Cond: ports(PortSpan{9, 8})}}},
		"unknown kind":                  {Overrides: []ScopeOverride{{Caps: fwdL, Cond: &Condition{Kind: 9}}}},
	} {
		if err := Validate(s); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

const (
	creatorHex = "00112233445566778899aabbccddeeff"
	targetHex  = "ffeeddccbbaa99887766554433221100"
)

// A conditioned override split by BoundRanks keeps its condition on every
// piece: a dropped one would turn the bit unconditioned (anti-goal 1).
func TestBoundRanksKeepsConditionOnEveryPiece(t *testing.T) {
	creator := NewReach(creatorHex, Scope{Base: protocol.ScopeBase_Global, Overrides: []ScopeOverride{
		{Caps: fwdR, Base: protocol.ScopeBase_Subtree},
	}}, nil)
	cond := ports(PortSpan{8000, 8100})
	child := Scope{Base: protocol.ScopeBase_Global, Overrides: []ScopeOverride{
		{Caps: fwdL | fwdR, Base: protocol.ScopeBase_Global, Cond: cond},
	}}
	out := creator.BoundRanks(child, fwdL|fwdR)
	if len(out.Overrides) != 2 {
		t.Fatalf("overrides = %+v, want two pieces", out.Overrides)
	}
	for _, o := range out.Overrides {
		if o.Cond == nil || !o.Cond.Contains(cond) || !cond.Contains(o.Cond) {
			t.Errorf("piece %s lost its condition: %+v", CapsLabel(o.Caps), o.Cond)
		}
	}
}

func TestAttenuateRefusesWiderCondition(t *testing.T) {
	creator := NewReach(creatorHex, Scope{Base: protocol.ScopeBase_Global, Overrides: []ScopeOverride{
		{Caps: execRun, Base: protocol.ScopeBase_Global, Cond: actions(1)},
	}}, nil)
	req := Scope{Base: protocol.ScopeBase_Subtree, Overrides: []ScopeOverride{
		{Caps: execRun, Base: protocol.ScopeBase_Subtree, Cond: actions(1, 2)},
	}}
	_, off, ok := Attenuate(req, execRun, creator)
	if ok || !strings.Contains(off, "exec_run") {
		t.Fatalf("ok=%v offender=%q, want refused naming exec_run", ok, off)
	}
	var cw *WiderConditionError
	if err := attenuateError(off); !asWider(err, &cw) || cw.Caps != execRun {
		t.Fatalf("attenuateError(%q) = %v, want a WiderConditionError on exec_run", off, err)
	}
	// A narrower one passes as written.
	req.Overrides[0].Cond = actions(1)
	if out, off, ok := Attenuate(req, execRun, creator); !ok || out.CondFor(execRun) == nil {
		t.Fatalf("equal condition: ok=%v off=%q out=%+v", ok, off, out)
	}
}

func asWider(err error, out **WiderConditionError) bool {
	w, ok := err.(*WiderConditionError)
	if ok {
		*out = w
	}
	return ok
}

func TestAttenuateInheritsConditionIntoOverride(t *testing.T) {
	cond := actions(1)
	creator := NewReach(creatorHex, Scope{Base: protocol.ScopeBase_Global, Overrides: []ScopeOverride{
		{Caps: execRun, Base: protocol.ScopeBase_Global, Cond: cond},
	}}, nil)
	req := Scope{Base: protocol.ScopeBase_Subtree, Overrides: []ScopeOverride{
		{Caps: execRun | protocol.Capability_Cancel, Base: protocol.ScopeBase_Subtree},
	}}
	out, off, ok := Attenuate(req, execRun|protocol.Capability_Cancel, creator)
	if !ok {
		t.Fatalf("refused: %s", off)
	}
	if c := out.CondFor(execRun); c == nil || !c.Contains(cond) || !cond.Contains(c) {
		t.Fatalf("exec_run condition = %+v, want the creator's", c)
	}
	if out.CondFor(protocol.Capability_Cancel) != nil {
		t.Fatal("cancel picked up exec_run's condition")
	}
	if err := Validate(out); err != nil {
		t.Fatalf("result invalid: %v", err)
	}
}

// A child asking exec_run in its BASE clause under a creator whose exec_run is
// conditioned comes out with an override carrying the creator's condition.
func TestAttenuateInheritsConditionIntoBaseBits(t *testing.T) {
	cond := actions(1)
	creator := NewReach(creatorHex, Scope{Base: protocol.ScopeBase_Global, Overrides: []ScopeOverride{
		{Caps: execRun, Base: protocol.ScopeBase_Global, Cond: cond},
	}}, nil)
	req := Scope{Base: protocol.ScopeBase_Subtree, IDs: []string{targetHex}}
	out, off, ok := Attenuate(req, execRun|protocol.Capability_FileRead, creator)
	if !ok {
		t.Fatalf("refused: %s", off)
	}
	c := out.CondFor(execRun)
	if c == nil || !c.Contains(cond) || !cond.Contains(c) {
		t.Fatalf("exec_run condition = %+v, want the creator's (overrides %+v)", c, out.Overrides)
	}
	base, _, ids := out.ForCap(execRun)
	if base != protocol.ScopeBase_Subtree || len(ids) != 1 || ids[0] != targetHex {
		t.Fatalf("exec_run reach = %v %v, want the base clause's", base, ids)
	}
	if out.CondFor(protocol.Capability_FileRead) != nil {
		t.Fatal("file_read became conditioned")
	}
	if err := Validate(out); err != nil {
		t.Fatalf("result invalid: %v", err)
	}
}

func TestClampUnderIntersectsConditions(t *testing.T) {
	parent := NewReach(creatorHex, Scope{Base: protocol.ScopeBase_Global, Overrides: []ScopeOverride{
		{Caps: execRun, Base: protocol.ScopeBase_Global, Cond: actions(1)},
	}}, nil)
	child := Scope{Base: protocol.ScopeBase_Subtree, Overrides: []ScopeOverride{
		{Caps: execRun, Base: protocol.ScopeBase_Subtree, Cond: actions(1, 2)},
	}}
	out := ClampUnder(child, execRun, parent)
	c := out.CondFor(execRun)
	if c == nil || !c.Contains(actions(1)) || !actions(1).Contains(c) {
		t.Fatalf("clamped condition = %+v, want {1}", c)
	}
	// An unconditioned descendant under a conditioned parent takes the parent's.
	out = ClampUnder(Scope{Base: protocol.ScopeBase_Subtree}, execRun, parent)
	if c := out.CondFor(execRun); c == nil || !c.Contains(actions(1)) || !actions(1).Contains(c) {
		t.Fatalf("unconditioned descendant = %+v, want {1}", c)
	}
}

func TestIsNarrowingSeesACondition(t *testing.T) {
	with := func(c *Condition) Scope {
		return Scope{Base: protocol.ScopeBase_Global, Overrides: []ScopeOverride{{Caps: fwdL, Base: protocol.ScopeBase_Global, Cond: c}}}
	}
	for _, tc := range []struct {
		name          string
		before, after *Condition
		want          bool
	}{
		{"added", nil, ports(PortSpan{1, 9}), true},
		{"narrowed", ports(PortSpan{1, 9}), ports(PortSpan{1, 5}), true},
		{"widened", ports(PortSpan{1, 5}), ports(PortSpan{1, 9}), false},
		{"removed", ports(PortSpan{1, 5}), nil, false},
		{"same", ports(PortSpan{1, 5}), ports(PortSpan{1, 5}), false},
	} {
		got := narrow(entry{fwdL, with(tc.before)}, entry{fwdL, with(tc.after)})
		if got != tc.want {
			t.Errorf("%s: IsNarrowing = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestConditionsWireRoundTrip(t *testing.T) {
	s := Scope{Base: protocol.ScopeBase_Subtree, Overrides: []ScopeOverride{
		{Caps: execRun, Base: protocol.ScopeBase_Global, Cond: actions(2, 1)},
		{Caps: fwdL, Base: protocol.ScopeBase_None, IDs: []string{targetHex}, Cond: ports(PortSpan{9000, 9010}, PortSpan{8000, 8100})},
		{Caps: protocol.Capability_Cancel, Base: protocol.ScopeBase_Global},
	}}
	ov := s.OverridesToWire()
	if !ov[0].Conditioned() || !ov[1].Conditioned() || ov[2].Conditioned() {
		t.Fatalf("markers = %v %v %v", ov[0].Conditioned(), ov[1].Conditioned(), ov[2].Conditioned())
	}
	body := protocol.GrantConditions{Conditions: s.ConditionsToWire()}
	got := ScopeFromWire(s.ToWire(), ov)
	if err := ConditionsFromWire(body, &got); err != nil {
		t.Fatalf("ConditionsFromWire: %v", err)
	}
	for _, o := range s.Overrides {
		g := got.CondFor(o.Caps)
		if (o.Cond == nil) != (g == nil) || (g != nil && (!g.Contains(o.Cond) || !o.Cond.Contains(g))) {
			t.Errorf("%s: got %+v, want %+v", CapsLabel(o.Caps), g, o.Cond)
		}
	}
	// Normalised: sorted.
	if a := got.CondFor(execRun).Actions; a[0] != aid(1) {
		t.Errorf("actions not sorted: %v", a)
	}

	// A marker with no body is refused.
	bare := ScopeFromWire(s.ToWire(), ov)
	if err := ConditionsFromWire(protocol.GrantConditions{}, &bare); err == nil {
		t.Error("marked overrides with no body accepted")
	}
	// Before ConditionsFromWire a marked override is "reaches nothing".
	if c := bare.CondFor(execRun); c == nil || c.Kind != 0 {
		t.Errorf("marked override before its body = %+v, want kind 0", c)
	}
	// A body for an unmarked override is refused.
	plain := ScopeFromWire(s.ToWire(), Scope{Overrides: []ScopeOverride{{Caps: execRun}}}.OverridesToWire())
	if err := ConditionsFromWire(protocol.GrantConditions{Conditions: s.ConditionsToWire()[:1]}, &plain); err == nil {
		t.Error("a body for an unmarked override accepted")
	}
	// A body whose kind does not apply to its bits is refused.
	bad := ScopeFromWire(s.ToWire(), Scope{Overrides: []ScopeOverride{{Caps: execRun, Cond: &Condition{}}}}.OverridesToWire())
	wrong := protocol.GrantCondition{Caps: execRun, Kind: protocol.ConditionKind_PortRange}
	var pr protocol.PortRangeCondition
	pr.SetSpans([]protocol.PortSpan{{Lo: 1, Hi: 2}})
	wrong.SetPortRange(pr)
	if err := ConditionsFromWire(protocol.GrantConditions{Conditions: []protocol.GrantCondition{wrong}}, &bad); err == nil {
		t.Error("port_range on exec_run accepted")
	}
	// More than MaxGrantConditions is refused.
	many := make([]protocol.GrantCondition, MaxGrantConditions+1)
	if err := ConditionsFromWire(protocol.GrantConditions{Conditions: many}, &Scope{}); err == nil {
		t.Error("over-limit body accepted")
	}
}

// A task whose condition on a bit is wider than its creator's — none at all
// under a conditioned creator, as an operator's caps set can leave it — is
// wider on that bit, though its target set is the same.
func TestWiderThanParentSeesACondition(t *testing.T) {
	cond := func(c *Condition) Scope {
		return Scope{Base: protocol.ScopeBase_Global, Overrides: []ScopeOverride{{Caps: fwdL, Base: protocol.ScopeBase_Global, Cond: c}}}
	}
	tasks := []Task{
		{ID: creatorHex, Caps: fwdL, Scope: cond(ports(PortSpan{8000, 8100}))},
		{ID: targetHex, Creator: creatorHex, Caps: fwdL, Scope: Scope{Base: protocol.ScopeBase_Global}},
	}
	if w := WiderThanParent(tasks, targetHex); w.Bits != fwdL {
		t.Fatalf("unconditioned under conditioned: wider bits = %s, want forward_local", CapsLabel(w.Bits))
	}
	tasks[1].Scope = cond(ports(PortSpan{8000, 8050}))
	if w := WiderThanParent(tasks, targetHex); w.Bits != 0 {
		t.Fatalf("narrower condition: wider bits = %s, want none", CapsLabel(w.Bits))
	}
}
