package protocol

import "testing"

// A3 (A-D29): --oneshot rides one bit on the open request's flag byte and one
// on OpenExecRunnerRequest, and a runner that runs it says so in its hello.
// Each is zero unless set, so an older peer's bytes read as "not oneshot".
func TestOneshotBitsRoundTrip(t *testing.T) {
	var ext OpenInteractiveExt
	ext.SetOneshot(true)
	ext.SetRecordSet(true)
	var extOut OpenInteractiveExt
	if err := extOut.DecodeExact(ext.MustAppend(nil)); err != nil {
		t.Fatal(err)
	}
	if !extOut.Oneshot() || !extOut.RecordSet() || extOut.Record() {
		t.Fatalf("ext bits: oneshot=%v record_set=%v record=%v", extOut.Oneshot(), extOut.RecordSet(), extOut.Record())
	}

	var oe OpenExecRunnerRequest
	oe.SetOneshot(true)
	var oeOut OpenExecRunnerRequest
	if err := oeOut.DecodeExact(oe.MustAppend(nil)); err != nil {
		t.Fatal(err)
	}
	if !oeOut.Oneshot() || oeOut.PromptPresent() {
		t.Fatalf("OpenExec bits: oneshot=%v prompt_present=%v", oeOut.Oneshot(), oeOut.PromptPresent())
	}
	var zero OpenExecRunnerRequest
	if err := oeOut.DecodeExact(zero.MustAppend(nil)); err != nil || oeOut.Oneshot() {
		t.Fatalf("zero OpenExec decodes oneshot=%v err=%v", oeOut.Oneshot(), err)
	}

	var h RunnerHello
	h.SetOneshotSessions(true)
	var hOut RunnerHello
	if err := hOut.DecodeExact(h.MustAppend(nil)); err != nil {
		t.Fatal(err)
	}
	if !hOut.OneshotSessions() || hOut.Transcripts() {
		t.Fatalf("hello bits: oneshot_sessions=%v transcripts=%v", hOut.OneshotSessions(), hOut.Transcripts())
	}
}
