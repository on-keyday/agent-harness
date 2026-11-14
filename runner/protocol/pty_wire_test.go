package protocol

import "testing"

// A-D33: the PTY is its own axis. The open request says whether it asked
// (pty_set) and what (pty); OpenExec carries the server's resolved answer as
// no_pty, so an older server's zero means a PTY as before; TaskInfo and
// TaskStatusEvent report it the same way round.
func TestPtyBitsRoundTrip(t *testing.T) {
	var ext OpenInteractiveExt
	ext.SetPtySet(true)
	ext.SetOneshot(true)
	var extOut OpenInteractiveExt
	if err := extOut.DecodeExact(ext.MustAppend(nil)); err != nil {
		t.Fatal(err)
	}
	if !extOut.PtySet() || extOut.Pty() || !extOut.Oneshot() || extOut.RecordSet() {
		t.Fatalf("ext bits: pty_set=%v pty=%v oneshot=%v record_set=%v", extOut.PtySet(), extOut.Pty(), extOut.Oneshot(), extOut.RecordSet())
	}
	ext.SetPty(true)
	if err := extOut.DecodeExact(ext.MustAppend(nil)); err != nil || !extOut.Pty() {
		t.Fatalf("ext pty=%v err=%v", extOut.Pty(), err)
	}

	var oe OpenExecRunnerRequest
	oe.SetNoPty(true)
	var oeOut OpenExecRunnerRequest
	if err := oeOut.DecodeExact(oe.MustAppend(nil)); err != nil {
		t.Fatal(err)
	}
	if !oeOut.NoPty() || oeOut.Oneshot() {
		t.Fatalf("OpenExec bits: no_pty=%v oneshot=%v", oeOut.NoPty(), oeOut.Oneshot())
	}
	var zero OpenExecRunnerRequest
	if err := oeOut.DecodeExact(zero.MustAppend(nil)); err != nil || oeOut.NoPty() {
		t.Fatalf("zero OpenExec decodes no_pty=%v err=%v", oeOut.NoPty(), err)
	}

	var ti TaskInfo
	ti.SetNoPty(true)
	var tiOut TaskInfo
	if err := tiOut.DecodeExact(ti.MustAppend(nil)); err != nil {
		t.Fatal(err)
	}
	if !tiOut.NoPty() || tiOut.Oneshot() {
		t.Fatalf("TaskInfo bits: no_pty=%v oneshot=%v", tiOut.NoPty(), tiOut.Oneshot())
	}

	var ev TaskStatusEvent
	ev.SetNoPty(true)
	var evOut TaskStatusEvent
	if err := evOut.DecodeExact(ev.MustAppend(nil)); err != nil {
		t.Fatal(err)
	}
	if !evOut.NoPty() || evOut.Oneshot() {
		t.Fatalf("TaskStatusEvent bits: no_pty=%v oneshot=%v", evOut.NoPty(), evOut.Oneshot())
	}
}
