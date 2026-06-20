package protocol

import "testing"

func TestCapabilityBits(t *testing.T) {
	if Capability_All != 0x7ff {
		t.Fatalf("All = %#x, want 0x7ff", Capability_All)
	}
	if Capability_None != 0 {
		t.Fatalf("None = %#x, want 0", Capability_None)
	}
	// all must be exactly the OR of the individual bits.
	or := Capability_Spawn | Capability_Cancel | Capability_ExecAttach |
		Capability_FileRead | Capability_FileWrite | Capability_ForwardLocal |
		Capability_ForwardRemote | Capability_Notify | Capability_Prune |
		Capability_RunnerAdmin | Capability_InfoGlobal
	if or != Capability_All {
		t.Fatalf("OR of bits = %#x, want All = %#x", or, Capability_All)
	}
}

func TestRequestedCapsRoundTrip(t *testing.T) {
	in := SubmitRequest{}
	in.RequestedCaps = Capability_Spawn | Capability_FileRead // 0x9, not a single member
	b := in.MustAppend(nil)
	var out SubmitRequest
	if err := out.DecodeExact(b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.RequestedCaps != in.RequestedCaps {
		t.Fatalf("round-trip caps = %#x, want %#x", out.RequestedCaps, in.RequestedCaps)
	}
}
