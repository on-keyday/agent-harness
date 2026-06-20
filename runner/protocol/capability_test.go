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

func TestTaskInfoCapabilitiesRoundTrip(t *testing.T) {
	in := TaskInfo{}
	in.Capabilities = Capability_Spawn | Capability_FileRead
	b := in.MustAppend(nil)
	var out TaskInfo
	if err := out.DecodeExact(b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Capabilities != in.Capabilities {
		t.Fatalf("round-trip Capabilities = %#x, want %#x", out.Capabilities, in.Capabilities)
	}
}

func TestResumeCapsOverrideRoundTrip(t *testing.T) {
	// override = true: bit survives encode/decode
	in := SubmitRequest{}
	in.SetResumeCapsOverride(true)
	in.RequestedCaps = Capability_Spawn
	b := in.MustAppend(nil)
	var out SubmitRequest
	if err := out.DecodeExact(b); err != nil {
		t.Fatal(err)
	}
	if !out.ResumeCapsOverride() {
		t.Fatalf("ResumeCapsOverride = false, want true")
	}

	// override = false (zero value): bit is clear after round-trip
	in2 := SubmitRequest{}
	in2.RequestedCaps = Capability_Spawn
	b2 := in2.MustAppend(nil)
	var out2 SubmitRequest
	if err := out2.DecodeExact(b2); err != nil {
		t.Fatal(err)
	}
	if out2.ResumeCapsOverride() {
		t.Fatalf("ResumeCapsOverride = true, want false")
	}
}
