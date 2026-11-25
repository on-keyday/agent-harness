package protocol

import "testing"

// Grant conditions (docs/superpowers/specs/2026-11-26-grant-conditions-and-bound-enrollment-design.md,
// C6/C9): bodies travel on streams, an override carries only the conditioned
// marker inline.

func actionID(b byte) ActionID {
	var id ActionID
	for i := range id.Id {
		id.Id[i] = b
	}
	return id
}

// Both kinds round-trip, at the limits' worst case (8 conditions, 32 action
// ids in one, 8 spans in another).
func TestGrantConditionsRoundTrip(t *testing.T) {
	var body GrantConditions
	var conds []GrantCondition
	var as GrantCondition
	as.Caps = Capability_ExecRun
	as.Kind = ConditionKind_ActionSet
	var set ActionSetCondition
	ids := make([]ActionID, 32)
	for i := range ids {
		ids[i] = actionID(byte(i + 1))
	}
	if !set.SetActions(ids) {
		t.Fatal("SetActions(32) refused")
	}
	if !as.SetActionSet(set) {
		t.Fatal("SetActionSet refused")
	}
	conds = append(conds, as)
	for i := 0; i < 7; i++ {
		var pr GrantCondition
		pr.Caps = Capability_ForwardLocal
		pr.Kind = ConditionKind_PortRange
		var r PortRangeCondition
		spans := make([]PortSpan, 8)
		for j := range spans {
			spans[j] = PortSpan{Lo: uint16(1000*j + i), Hi: uint16(1000*j + i + 10)}
		}
		if !r.SetSpans(spans) {
			t.Fatal("SetSpans(8) refused")
		}
		if !pr.SetPortRange(r) {
			t.Fatal("SetPortRange refused")
		}
		conds = append(conds, pr)
	}
	if !body.SetConditions(conds) {
		t.Fatal("SetConditions(8) refused")
	}
	buf, err := body.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	var got GrantConditions
	if err := got.DecodeExact(buf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Conditions) != 8 {
		t.Fatalf("conditions = %d, want 8", len(got.Conditions))
	}
	a := got.Conditions[0]
	if a.Caps != Capability_ExecRun || a.Kind != ConditionKind_ActionSet || a.ActionSet() == nil {
		t.Fatalf("condition 0 = %+v, want an action_set on exec_run", a)
	}
	if n := len(a.ActionSet().Actions); n != 32 || a.ActionSet().Actions[31] != actionID(32) {
		t.Fatalf("action ids: %d, last %v", n, a.ActionSet().Actions[n-1])
	}
	if a.PortRange() != nil {
		t.Fatal("an action_set condition answers PortRange()")
	}
	p := got.Conditions[7]
	if p.Kind != ConditionKind_PortRange || p.PortRange() == nil {
		t.Fatalf("condition 7 = %+v, want a port_range", p)
	}
	if sp := p.PortRange().Spans; len(sp) != 8 || sp[7] != (PortSpan{Lo: 7006, Hi: 7016}) {
		t.Fatalf("spans = %v", sp)
	}
}

// The marker takes one bit of ScopeOverride's former reserved byte: the
// override's size does not change and exclude_self is untouched.
func TestScopeOverrideConditionedBitRoundTrip(t *testing.T) {
	for _, tc := range []struct{ excl, cond bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		o := ScopeOverride{Caps: Capability_ForwardLocal, Base: ScopeBase_Global}
		o.SetExcludeSelf(tc.excl)
		o.SetConditioned(tc.cond)
		buf, err := o.Append(nil)
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		// caps u64 + base u8 + flag byte + ids_len u16, no ids.
		if len(buf) != 12 {
			t.Fatalf("encoded %d bytes, want 12 (the size before the marker)", len(buf))
		}
		var got ScopeOverride
		if err := got.DecodeExact(buf); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.ExcludeSelf() != tc.excl || got.Conditioned() != tc.cond {
			t.Fatalf("excl=%v cond=%v, want %v %v", got.ExcludeSelf(), got.Conditioned(), tc.excl, tc.cond)
		}
	}
	// The zero value is unconditioned: every record from before the marker.
	if zero := (ScopeOverride{}); zero.Conditioned() {
		t.Fatal("the zero ScopeOverride reads as conditioned")
	}
}

// The open names its grant-conditions stream in the ext byte's block; absent
// when the bit is clear (no invisible bytes).
func TestOpenInteractiveExtCarriesGrantConditionsStream(t *testing.T) {
	var ext OpenInteractiveExt
	plain, err := ext.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if len(plain) != 1 {
		t.Fatalf("ext without grant conditions = %d bytes, want 1", len(plain))
	}
	ext.SetGrantConditionsPresent(true)
	ext.SetGrantConditionsStreamId(77)
	buf, err := ext.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if len(buf) != 9 {
		t.Fatalf("ext with grant conditions = %d bytes, want 9", len(buf))
	}
	var got OpenInteractiveExt
	if err := got.DecodeExact(buf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.GrantConditionsPresent() || got.GrantConditionsStreamId() == nil || *got.GrantConditionsStreamId() != 77 {
		t.Fatalf("got present=%v id=%v", got.GrantConditionsPresent(), got.GrantConditionsStreamId())
	}
}

// The other four grant-writing requests carry the stream id unconditionally
// (0 = none); the two streamed reads carry the bodies.
func TestGrantWritersAndReadersCarryConditions(t *testing.T) {
	sc := SetCapsRequest{GrantConditionsStreamId: 5}
	ci := CredentialIssueRequest{GrantConditionsStreamId: 6}
	cr := CredentialRekeyRequest{GrantConditionsStreamId: 7}
	ss := ScheduleSpec{GrantConditionsStreamId: 8}
	for _, m := range []interface {
		Append([]byte) ([]byte, error)
	}{&sc, &ci, &cr, &ss} {
		if _, err := m.Append(nil); err != nil {
			t.Fatalf("append %T: %v", m, err)
		}
	}
	var spec ScheduleSpec
	b, _ := ss.Append(nil)
	if err := spec.DecodeExact(b); err != nil || spec.GrantConditionsStreamId != 8 {
		t.Fatalf("ScheduleSpec stream id = %d (%v)", spec.GrantConditionsStreamId, err)
	}

	pr := GrantCondition{Caps: Capability_ForwardRemote, Kind: ConditionKind_PortRange}
	var r PortRangeCondition
	r.SetSpans([]PortSpan{{Lo: 22, Hi: 22}})
	pr.SetPortRange(r)
	var ti TaskInfo
	if !ti.SetGrantConditions([]GrantCondition{pr}) {
		t.Fatal("TaskInfo.SetGrantConditions refused")
	}
	tb, err := ti.Append(nil)
	if err != nil {
		t.Fatalf("TaskInfo append: %v", err)
	}
	var tgot TaskInfo
	if err := tgot.DecodeExact(tb); err != nil || len(tgot.GrantConditions) != 1 {
		t.Fatalf("TaskInfo grant conditions = %d (%v)", len(tgot.GrantConditions), err)
	}
	var sd ScheduleDetail
	if !sd.SetGrantConditions([]GrantCondition{pr}) {
		t.Fatal("ScheduleDetail.SetGrantConditions refused")
	}
	db, err := sd.Append(nil)
	if err != nil {
		t.Fatalf("ScheduleDetail append: %v", err)
	}
	var dgot ScheduleDetail
	if err := dgot.DecodeExact(db); err != nil || len(dgot.GrantConditions) != 1 {
		t.Fatalf("ScheduleDetail grant conditions = %d (%v)", len(dgot.GrantConditions), err)
	}

	for _, s := range []string{
		OpenInteractiveStatus_BadGrantConditions.String(),
		CredentialStatus_BadGrantConditions.String(),
		SetCapsStatus_BadGrantConditions.String(),
	} {
		if s != "bad_grant_conditions" {
			t.Errorf("status string = %q, want bad_grant_conditions", s)
		}
	}
	// ScheduleStatus's values carry no string form; its String is the Go name.
	if s := ScheduleStatus_BadGrantConditions.String(); s != "BadGrantConditions" {
		t.Errorf("ScheduleStatus string = %q", s)
	}
}
