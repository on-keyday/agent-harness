package protocol

import "testing"

func TestCapabilityBits(t *testing.T) {
	// Formatted through uint32, never the Capability itself: Capability has a
	// String method, and fmt applies %x to the STRING for a Stringer — so
	// %#x of Capability_All printed 0x616c6c ("all" in ASCII) and hid the
	// actual value behind a plausible-looking hex number.
	if Capability_All != 0xffff {
		t.Fatalf("All = %#x, want 0xffff", uint32(Capability_All))
	}
	if Capability_None != 0 {
		t.Fatalf("None = %#x, want 0", uint32(Capability_None))
	}
	// all must be exactly the OR of the individual bits.
	or := Capability_Spawn | Capability_Cancel | Capability_ExecControl |
		Capability_FileRead | Capability_FileWrite | Capability_ForwardLocal |
		Capability_ForwardRemote | Capability_Notify | Capability_Prune |
		Capability_RunnerAdmin | Capability_BoardObserve | Capability_Purge |
		Capability_ExecRun |
		Capability_ExecView | Capability_ExecCowrite | Capability_ExecResize
	if or != Capability_All {
		t.Fatalf("OR of bits = %#x, want All = %#x", uint32(or), uint32(Capability_All))
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

func TestResumeConversationRoundTrip(t *testing.T) {
	submit := SubmitRequest{}
	submit.SetResumeCapsOverride(true)
	submit.SetResumeConversation(true)
	submitBytes := submit.MustAppend(nil)
	var submitOut SubmitRequest
	if err := submitOut.DecodeExact(submitBytes); err != nil {
		t.Fatal(err)
	}
	if !submitOut.ResumeCapsOverride() {
		t.Fatalf("SubmitRequest.ResumeCapsOverride = false, want true")
	}
	if !submitOut.ResumeConversation() {
		t.Fatalf("SubmitRequest.ResumeConversation = false, want true")
	}

	open := OpenInteractiveRequest{}
	open.SetX11Enabled(true)
	open.SetResumeCapsOverride(true)
	open.SetResumeConversation(true)
	open.SetX11(X11Forward{Display: 10})
	openBytes := open.MustAppend(nil)
	var openOut OpenInteractiveRequest
	if err := openOut.DecodeExact(openBytes); err != nil {
		t.Fatal(err)
	}
	if !openOut.X11Enabled() || !openOut.ResumeCapsOverride() || !openOut.ResumeConversation() {
		t.Fatalf("OpenInteractive flags lost: x11=%v caps=%v conversation=%v",
			openOut.X11Enabled(), openOut.ResumeCapsOverride(), openOut.ResumeConversation())
	}

	assign := AssignTaskBody{}
	assign.SetResumeConversation(true)
	assignBytes := assign.MustAppend(nil)
	var assignOut AssignTaskBody
	if err := assignOut.DecodeExact(assignBytes); err != nil {
		t.Fatal(err)
	}
	if !assignOut.ResumeConversation() {
		t.Fatalf("AssignTaskBody.ResumeConversation = false, want true")
	}

	exec := OpenExecRunnerRequest{}
	exec.SetX11Enabled(true)
	exec.SetResumeConversation(true)
	exec.SetX11(X11Forward{Display: 11})
	execBytes := exec.MustAppend(nil)
	var execOut OpenExecRunnerRequest
	if err := execOut.DecodeExact(execBytes); err != nil {
		t.Fatal(err)
	}
	if !execOut.X11Enabled() || !execOut.ResumeConversation() {
		t.Fatalf("OpenExec flags lost: x11=%v conversation=%v",
			execOut.X11Enabled(), execOut.ResumeConversation())
	}
}
