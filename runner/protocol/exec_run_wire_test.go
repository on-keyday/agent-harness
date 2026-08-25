package protocol

import "testing"

// Every new format round-trips, and argv survives its element boundaries — the
// length-prefixed-list shape is where an off-by-one shows up as a silently
// truncated command rather than as an error.
func TestExecRunRequestRoundTrip(t *testing.T) {
	var argv ExecArgv
	for _, a := range []string{"sh", "-c", "echo hi && echo bye 1>&2"} {
		var e ExecArg
		if !e.SetArg([]byte(a)) {
			t.Fatalf("SetArg(%q) failed", a)
		}
		argv.Argv = append(argv.Argv, e)
	}
	argv.ArgvLen = uint16(len(argv.Argv))

	in := ExecRunRequest{TaskId: TaskID{Id: [16]byte{1, 2, 3}}, Argv: argv}
	in.SetStdinEnabled(true)

	buf, err := in.Append(nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	var out ExecRunRequest
	if err := out.DecodeExactCopy(buf); err != nil {
		t.Fatalf("DecodeExactCopy: %v", err)
	}
	if out.Argv.ArgvLen != 3 {
		t.Fatalf("argv len = %d, want 3", out.Argv.ArgvLen)
	}
	if got := string(out.Argv.Argv[2].Arg); got != "echo hi && echo bye 1>&2" {
		t.Errorf("argv[2] = %q, want the third argument intact", got)
	}
	if got := string(out.Argv.Argv[0].Arg); got != "sh" {
		t.Errorf("argv[0] = %q, want sh", got)
	}
	if !out.StdinEnabled() {
		t.Error("stdin_enabled did not survive")
	}
	if out.TaskId.Id != in.TaskId.Id {
		t.Error("task id did not survive")
	}
}

func TestExecEventRoundTrip(t *testing.T) {
	in := ExecEvent{Kind: ExecEventKind_Exited, ExitCode: 3}
	buf, err := in.Append(nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	var out ExecEvent
	if err := out.DecodeExactCopy(buf); err != nil {
		t.Fatalf("DecodeExactCopy: %v", err)
	}
	if out.Kind != ExecEventKind_Exited || out.ExitCode != 3 {
		t.Errorf("got kind=%v code=%d, want exited/3", out.Kind, out.ExitCode)
	}
}

// A NEGATIVE code must survive. -1 is what killed and failed report, and an
// unsigned field would deliver it as 4294967295 — which reads as a plausible
// exit code rather than as a decoding bug.
func TestExecEventNegativeExitCode(t *testing.T) {
	in := ExecEvent{Kind: ExecEventKind_Killed, ExitCode: -1}
	in.Detail = []byte("killed by exec kill")
	buf, err := in.Append(nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	var out ExecEvent
	if err := out.DecodeExactCopy(buf); err != nil {
		t.Fatalf("DecodeExactCopy: %v", err)
	}
	if out.ExitCode != -1 {
		t.Errorf("exit code = %d, want -1", out.ExitCode)
	}
	if string(out.Detail) != "killed by exec kill" {
		t.Errorf("detail = %q, want it intact", out.Detail)
	}
}

func TestExecRunFinishedRoundTrip(t *testing.T) {
	in := ExecRunFinished{ExecId: 42, ExitCode: 7, Kind: ExecEventKind_Exited}
	buf, err := in.Append(nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	var out ExecRunFinished
	if err := out.DecodeExactCopy(buf); err != nil {
		t.Fatalf("DecodeExactCopy: %v", err)
	}
	if out.ExecId != 42 || out.ExitCode != 7 || out.Kind != ExecEventKind_Exited {
		t.Errorf("got %+v, want exec 42 / code 7 / exited", out)
	}
}

// `all` must cover the new bit, and the new bit must not collide with any
// existing one. `all` is a literal in the schema, so this is the test that
// fails when a bit is added and the literal is not widened.
func TestExecRunCapabilityBit(t *testing.T) {
	if Capability_ExecRun&Capability_All != Capability_ExecRun {
		t.Error("Capability_All does not include exec_run")
	}
	for _, other := range []Capability{
		Capability_Spawn, Capability_Cancel, Capability_ExecControl,
		Capability_FileRead, Capability_FileWrite, Capability_ForwardLocal,
		Capability_ForwardRemote, Capability_Notify, Capability_Prune,
		Capability_RunnerAdmin, Capability_BoardObserve, Capability_Purge,
		Capability_ExecView, Capability_ExecCowrite, Capability_ExecResize,
	} {
		if Capability_ExecRun&other != 0 {
			t.Errorf("exec_run collides with %v", other)
		}
	}
}

// exec_count is appended to TaskInfo, so every field before it must keep its
// meaning: a shifted offset would corrupt the whole row rather than just the
// new field.
func TestTaskInfoExecCountRoundTrip(t *testing.T) {
	in := TaskInfo{ExecCount: 2, Viewers: 1, Cowriters: 3, ExitCode: -1}
	buf, err := in.Append(nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	var out TaskInfo
	if err := out.DecodeExactCopy(buf); err != nil {
		t.Fatalf("DecodeExactCopy: %v", err)
	}
	if out.ExecCount != 2 {
		t.Errorf("ExecCount = %d, want 2", out.ExecCount)
	}
	if out.Viewers != 1 || out.Cowriters != 3 || out.ExitCode != -1 {
		t.Errorf("neighbouring fields moved: %+v", out)
	}
}
