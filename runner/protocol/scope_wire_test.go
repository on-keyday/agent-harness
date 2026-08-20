package protocol

import "testing"

// The scope fields are appended to formats that were already on the wire, and
// the schema has no optional-field mechanism: DecodeExact rejects both a short
// buffer and leftover bytes. These tests assert that skew is a hard, visible
// failure rather than a silent misparse, in both directions.

// A pre-scope SubmitRequest payload is one TaskScope short. An empty TaskScope
// encodes as base(1) + vis_base(1) + flags(1) + vis_ids_len(2) + ids_len(2) =
// seven bytes, plus the overrides_len(1) that rides beside it — so trimming
// eight reproduces what a client from before the per-capability change sends.
// (It was three bytes when TaskScope was base + ids_len alone.)
func TestSubmitRequestRejectsPreScopePayload(t *testing.T) {
	var req SubmitRequest
	req.SetRepoPath([]byte("/r"))
	req.SetPrompt([]byte("p"))
	buf, err := req.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if len(buf) < 9 {
		t.Fatalf("encoded payload is implausibly short (%d bytes)", len(buf))
	}
	var got SubmitRequest
	if err := got.DecodeExact(buf[:len(buf)-8]); err == nil {
		t.Fatal("a pre-scope payload decoded; want a short-buffer error")
	}
}

// The other direction: a new payload reaching an older decoder leaves bytes
// over. DecodeExact's leftover check is what makes that loud.
func TestSubmitRequestRejectsTrailingBytes(t *testing.T) {
	var req SubmitRequest
	req.SetRepoPath([]byte("/r"))
	req.SetPrompt([]byte("p"))
	buf, err := req.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	var got SubmitRequest
	if err := got.DecodeExact(append(buf, 0, 0, 0)); err == nil {
		t.Fatal("trailing bytes accepted; want an exact-length error")
	}
}

// Enum VALUE additions are decode-safe: the generated decoder assigns the raw
// byte with no is_defined check, and String falls back to the numeric form. An
// old client receiving scope_not_permitted therefore renders it, not fails.
func TestUnknownStatusValuesDecodeAndRender(t *testing.T) {
	if got := SubmitStatus(200).String(); got != "SubmitStatus(200)" {
		t.Errorf("SubmitStatus(200).String() = %q, want the numeric fallback", got)
	}
	if got := OpenInteractiveStatus(200).String(); got != "OpenInteractiveStatus(200)" {
		t.Errorf("OpenInteractiveStatus(200).String() = %q, want the numeric fallback", got)
	}
	resp := SubmitResponse{Status: SubmitStatus_ScopeNotPermitted}
	buf, err := resp.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	var got SubmitResponse
	if err := got.DecodeExact(buf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Status != SubmitStatus_ScopeNotPermitted {
		t.Errorf("status = %v, want scope_not_permitted", got.Status)
	}
}

// CancelStatus grew a typed status. ok must still be the same single zero byte
// every pre-change reply wrote.
func TestCancelStatusOkIsOneZeroByte(t *testing.T) {
	cs := CancelStatus{Status: CancelResult_Ok}
	buf, err := cs.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if len(buf) != 1 || buf[0] != 0 {
		t.Fatalf("encoded %v, want a single zero byte", buf)
	}
}

// subtree is the zero value so that an absent scope, a legacy WAL record and a
// zero-valued struct all read as the default rather than as the strictest set.
func TestZeroTaskScopeIsSubtree(t *testing.T) {
	if (TaskScope{}).Base != ScopeBase_Subtree {
		t.Fatal("the zero TaskScope must be subtree")
	}
}

// The presence bits pack into one byte and must round-trip independently —
// caps_present set with scope_present clear is the "--caps only" call.
func TestSetCapsRequestPresenceBitsRoundTrip(t *testing.T) {
	var req SetCapsRequest
	req.TaskId = TaskID{Id: [16]byte{9}}
	req.Caps = Capability_Spawn
	req.Scope = TaskScope{Base: ScopeBase_None}
	req.SetCapsPresent(true)
	req.SetScopePresent(false)
	req.SetCascade(true)
	req.SetKeepConns(false)
	buf, err := req.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	var got SetCapsRequest
	if err := got.DecodeExact(buf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.CapsPresent() || got.ScopePresent() || !got.Cascade() || got.KeepConns() {
		t.Fatalf("bits: caps=%v scope=%v cascade=%v keep=%v, want true/false/true/false",
			got.CapsPresent(), got.ScopePresent(), got.Cascade(), got.KeepConns())
	}
	if got.Caps != Capability_Spawn || got.Scope.Base != ScopeBase_None {
		t.Fatalf("payload = caps %v scope %v", got.Caps, got.Scope.Base)
	}
}

// Every field added by the per-capability change must have a zero value that
// reproduces the pre-change reading: visibility follows the action base, self
// is in the action set, both id lists empty. An all-zeros TaskScope is the old
// default exactly, which is what makes legacy WAL records and absent scopes
// legal without a migration.
func TestTaskScopeZeroValueIsPreChangeDefault(t *testing.T) {
	var s TaskScope
	if s.Base != ScopeBase_Subtree {
		t.Errorf("Base = %v, want subtree", s.Base)
	}
	if s.VisBasePresent() {
		t.Error("VisBasePresent = true; visibility must follow the action base")
	}
	if s.ExcludeSelf() {
		t.Error("ExcludeSelf = true; self was unconditional before this change")
	}
	if len(s.Ids) != 0 || len(s.VisIds) != 0 {
		t.Errorf("ids = %d, vis_ids = %d, want both empty", len(s.Ids), len(s.VisIds))
	}
}

// The two new flags share a byte with the reserved bits and must round-trip
// independently — the same property the SetCapsRequest presence bits have.
func TestTaskScopeVisibilityFlagsRoundTrip(t *testing.T) {
	in := TaskScope{Base: ScopeBase_None, VisBase: ScopeBase_Global}
	in.SetVisBasePresent(true)
	in.SetExcludeSelf(true)
	buf, err := in.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	var got TaskScope
	if err := got.DecodeExact(buf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Base != ScopeBase_None || got.VisBase != ScopeBase_Global {
		t.Errorf("bases = %v / %v, want none / global", got.Base, got.VisBase)
	}
	if !got.VisBasePresent() || !got.ExcludeSelf() {
		t.Errorf("flags = vis_present %v, exclude_self %v; both should survive",
			got.VisBasePresent(), got.ExcludeSelf())
	}
}

// An override is a capability MASK plus a scope. Two bits in one entry is the
// point — "every write-ish bit gets the same narrowing" must cost one entry,
// not one per bit.
func TestScopeOverrideCarriesAMask(t *testing.T) {
	in := ScopeOverride{
		Caps: Capability_ExecCowrite | Capability_FileWrite,
		Base: ScopeBase_Subtree,
	}
	in.SetExcludeSelf(true)
	buf, err := in.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	var got ScopeOverride
	if err := got.DecodeExact(buf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Caps != (Capability_ExecCowrite | Capability_FileWrite) {
		t.Errorf("caps = %v, want both bits", got.Caps)
	}
	if got.Base != ScopeBase_Subtree || !got.ExcludeSelf() {
		t.Errorf("base = %v, exclude_self = %v", got.Base, got.ExcludeSelf())
	}
}

// The override list rides beside the scope on every format that carries one.
func TestSubmitRequestCarriesOverrides(t *testing.T) {
	var req SubmitRequest
	req.SetRepoPath([]byte("/r"))
	req.SetPrompt([]byte("p"))
	req.Scope = TaskScope{Base: ScopeBase_Subtree}
	req.Overrides = []ScopeOverride{{Caps: Capability_Cancel, Base: ScopeBase_None}}
	req.OverridesLen = uint8(len(req.Overrides))

	buf, err := req.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	var got SubmitRequest
	if err := got.DecodeExact(buf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.OverridesLen != 1 || len(got.Overrides) != 1 {
		t.Fatalf("overrides = %d / %d, want one", got.OverridesLen, len(got.Overrides))
	}
	if got.Overrides[0].Caps != Capability_Cancel {
		t.Errorf("override caps = %v, want cancel", got.Overrides[0].Caps)
	}
}

// A TaskScope with ids must survive the length-prefixed array round trip.
func TestTaskScopeIDsRoundTrip(t *testing.T) {
	in := TaskScope{Base: ScopeBase_Subtree, IdsLen: 2, Ids: []TaskID{
		{Id: [16]byte{1}}, {Id: [16]byte{2}},
	}}
	buf, err := in.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	var got TaskScope
	if err := got.DecodeExact(buf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.IdsLen != 2 || len(got.Ids) != 2 || got.Ids[1].Id[0] != 2 {
		t.Fatalf("round trip = %+v", got)
	}
}

// Disjointness and the empty mask are validated where the value is WRITTEN
// (server: validateScope; client: MergeScopeOverride), not at decode. This
// pins that split rather than leaving a reader to assume the wire rejects
// them: an override list that violates either still decodes cleanly, and the
// gate is the handler.
func TestScopeOverrideDisjointnessIsNotAWireConcern(t *testing.T) {
	in := SubmitRequest{}
	in.SetRepoPath([]byte("/r"))
	in.SetPrompt([]byte("p"))
	in.Overrides = []ScopeOverride{
		{Caps: Capability_Cancel | Capability_Purge},
		{Caps: Capability_Purge}, // intersects the first
		{Caps: Capability_None},  // empty mask
	}
	in.OverridesLen = uint8(len(in.Overrides))

	buf, err := in.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	var got SubmitRequest
	if err := got.DecodeExact(buf); err != nil {
		t.Fatalf("decode: %v — the wire must carry what it is given; the REJECTION "+
			"belongs to the handler, which can name the offending bit", err)
	}
	if got.OverridesLen != 3 {
		t.Errorf("overrides = %d, want all three carried through", got.OverridesLen)
	}
}

// The visibility rank and its presence bit share a byte with exclude_self and
// the reserved bits. A canonical zero must stay a canonical zero across the
// wire, or the rule that one authority has one encoding is unenforceable.
func TestTaskScopeCanonicalZeroSurvivesTheWire(t *testing.T) {
	in := TaskScope{Base: ScopeBase_None}
	buf, err := in.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	var got TaskScope
	if err := got.DecodeExact(buf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.VisBasePresent() || got.VisBase != ScopeBase(0) || got.ExcludeSelf() {
		t.Errorf("a canonical zero came back non-canonical: present=%v vis=%v exclude=%v",
			got.VisBasePresent(), got.VisBase, got.ExcludeSelf())
	}
}
