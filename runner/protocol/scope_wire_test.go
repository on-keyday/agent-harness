package protocol

import "testing"

// The scope fields are appended to formats that were already on the wire, and
// the schema has no optional-field mechanism: DecodeExact rejects both a short
// buffer and leftover bytes. These tests assert that skew is a hard, visible
// failure rather than a silent misparse, in both directions.

// A pre-scope SubmitRequest payload is one TaskScope short. TaskScope encodes
// as base(1) + ids_len(2) with no ids, so trimming three bytes reproduces what
// an older client would send.
func TestSubmitRequestRejectsPreScopePayload(t *testing.T) {
	var req SubmitRequest
	req.SetRepoPath([]byte("/r"))
	req.SetPrompt([]byte("p"))
	buf, err := req.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if len(buf) < 4 {
		t.Fatalf("encoded payload is implausibly short (%d bytes)", len(buf))
	}
	var got SubmitRequest
	if err := got.DecodeExact(buf[:len(buf)-3]); err == nil {
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
