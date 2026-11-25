package protocol

import "testing"

// A3 (A-D29): --oneshot rides one bit on the open request's flag byte and one
// on OpenExecRunnerRequest, and a runner that runs it says so in its hello.
// Each is zero unless set, so an older peer's bytes read as "not oneshot".
func TestOneshotBitsRoundTrip(t *testing.T) {
	var oi OpenInteractiveRequest
	oi.Kind = TaskKind_Oneshot
	oi.SetExtPresent(true)
	var ext OpenInteractiveExt
	ext.SetRecordSet(true)
	oi.SetExt(ext)
	var oiOut OpenInteractiveRequest
	if err := oiOut.DecodeExact(oi.MustAppend(nil)); err != nil {
		t.Fatal(err)
	}
	if oiOut.Kind != TaskKind_Oneshot || oiOut.Ext() == nil || !oiOut.Ext().RecordSet() || oiOut.Ext().Record() {
		t.Fatalf("open: kind=%v ext=%+v", oiOut.Kind, oiOut.Ext())
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
