package server

import (
	"bytes"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The control stream double is port_forward_test.go's recordingBidiStream: it
// captures writes and records end-of-stream, which is exactly what an ExecEvent
// assertion needs. It is a bidi double standing in for a SendStream — the real
// control stream is unidirectional on purpose (see handleOpenExecRun).

// The exit code reaches the client as ONE ExecEvent on the control stream, and
// the registration is dropped in the same step: an entry lives exactly as long
// as its exec, so a finished one must not linger in `exec ls`.
func TestExecRunFinishedPushesEventAndDrops(t *testing.T) {
	h := &TaskHandler{}
	ctrl := newRecordingBidiStream(1)
	id := h.execs().add(&execRun{taskIDHex: "aaaa", argv: []string{"false"}, control: ctrl})

	h.onExecRunFinished(&protocol.ExecRunFinished{
		ExecId: id, ExitCode: 1, Kind: protocol.ExecEventKind_Exited,
	})

	if _, ok := h.execs().get(id); ok {
		t.Error("a finished exec must be dropped from the registry")
	}
	var ev protocol.ExecEvent
	if err := ev.DecodeExactCopy(ctrl.Written()); err != nil {
		t.Fatalf("control stream did not carry exactly one ExecEvent: %v", err)
	}
	if ev.Kind != protocol.ExecEventKind_Exited || ev.ExitCode != 1 {
		t.Errorf("event = kind %v code %d, want exited/1", ev.Kind, ev.ExitCode)
	}
	if !ctrl.Ended() {
		t.Error("the control stream must end after its one event — the EOF is what tells the client the outcome is complete")
	}
}

// A failure carries -1 and its reason, not an invented exit code: 127 is a
// shell's convention and there is no shell in this path.
func TestExecRunFinishedFailedCarriesDetail(t *testing.T) {
	h := &TaskHandler{}
	ctrl := newRecordingBidiStream(1)
	id := h.execs().add(&execRun{taskIDHex: "aaaa", control: ctrl})

	h.onExecRunFinished(&protocol.ExecRunFinished{
		ExecId: id, ExitCode: -1, Kind: protocol.ExecEventKind_Failed,
		Detail: []byte(`exec "nope": executable file not found in $PATH`),
	})

	var ev protocol.ExecEvent
	if err := ev.DecodeExactCopy(ctrl.Written()); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ev.Kind != protocol.ExecEventKind_Failed || ev.ExitCode != -1 {
		t.Errorf("event = kind %v code %d, want failed/-1", ev.Kind, ev.ExitCode)
	}
	if !bytes.Contains(ev.Detail, []byte("executable file not found")) {
		t.Errorf("detail = %q, want the OS error", ev.Detail)
	}
}

// An unknown exec id is a no-op, not a panic: a runner can report a finish for
// an exec whose client already went away and took the registration with it.
func TestExecRunFinishedUnknownIDIsIgnored(t *testing.T) {
	h := &TaskHandler{}
	h.onExecRunFinished(&protocol.ExecRunFinished{ExecId: 999, Kind: protocol.ExecEventKind_Exited})
}

// Every field of the client's request must survive the relay to the runner —
// one relay site for a growing struct is the shape that has silently dropped a
// new field on this project before.
func TestRunnerExecRunRequestCarriesEveryField(t *testing.T) {
	var argv protocol.ExecArgv
	for _, a := range []string{"sh", "-c", "echo hi"} {
		var one protocol.ExecArg
		one.SetArg([]byte(a))
		argv.Argv = append(argv.Argv, one)
	}
	argv.ArgvLen = uint16(len(argv.Argv))

	in := &protocol.ExecRunRequest{TaskId: protocol.TaskID{Id: [16]byte{9}}, Argv: argv}
	in.SetStdinEnabled(true)

	out := runnerExecRunRequest(in, 7, "/repo", 42)
	if out.ExecId != 7 || out.StreamId != 42 {
		t.Errorf("exec id / stream id = %d / %d, want 7 / 42", out.ExecId, out.StreamId)
	}
	if string(out.RepoPath) != "/repo" {
		t.Errorf("repo path = %q, want /repo — a terminal task's worktree cannot be resolved without it", out.RepoPath)
	}
	if out.TaskId.Id != in.TaskId.Id {
		t.Error("task id did not survive the relay")
	}
	if out.Argv.ArgvLen != 3 || string(out.Argv.Argv[2].Arg) != "echo hi" {
		t.Errorf("argv did not survive the relay: %+v", out.Argv)
	}
	if !out.StdinEnabled() {
		t.Error("stdin_enabled did not survive the relay")
	}
}

// An empty argv is refused before anything is allocated: there is no command
// to run and no reason to open three streams to discover that.
func TestOpenExecRunRefusesEmptyArgv(t *testing.T) {
	h := &TaskHandler{}
	resp := h.handleOpenExecRun(nil, &protocol.ExecRunRequest{})
	if resp.Status != protocol.ExecRunStatus_EmptyArgv {
		t.Errorf("status = %v, want empty_argv", resp.Status)
	}
}

// The listing renders what the operator needs to tell two identical commands
// apart: whose it is, and against which task.
func TestExecRunInfoCarriesOrigin(t *testing.T) {
	e := &execRun{
		execID:     3,
		taskIDHex:  "00112233445566778899aabbccddeeff",
		argv:       []string{"make", "test"},
		clientCID:  "ws:127.0.0.1:8539-7",
		clientKind: protocol.ClientKind_Tui,
	}
	info := execRunInfo(e)
	if info.ExecId != 3 {
		t.Errorf("ExecId = %d, want 3", info.ExecId)
	}
	if info.OriginKind != protocol.ClientKind_Tui || string(info.OriginCid) != "ws:127.0.0.1:8539-7" {
		t.Errorf("origin = %v %q, want tui and the cid", info.OriginKind, info.OriginCid)
	}
	if info.Argv.ArgvLen != 2 || string(info.Argv.Argv[1].Arg) != "test" {
		t.Errorf("argv = %+v, want [make test]", info.Argv)
	}
	if got := info.TaskId.Id[0]; got != 0x00 {
		t.Errorf("task id first byte = %#x", got)
	}
	if info.TaskId.Id[15] != 0xff {
		t.Error("task id did not decode from hex")
	}
}
