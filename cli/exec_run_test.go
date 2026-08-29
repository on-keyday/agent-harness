//go:build !js

package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestBuildExecArgv(t *testing.T) {
	got := buildExecArgv([]string{"sh", "-c", "echo 'a b'"})
	if got.ArgvLen != 3 {
		t.Fatalf("ArgvLen = %d, want 3", got.ArgvLen)
	}
	if s := string(got.Argv[2].Arg); s != "echo 'a b'" {
		t.Errorf("argv[2] = %q, want the whole third argument", s)
	}
	if got.Argv[0].ArgLen != 2 {
		t.Errorf("argv[0].ArgLen = %d, want 2", got.Argv[0].ArgLen)
	}
}

func TestBuildExecArgvEmpty(t *testing.T) {
	if got := buildExecArgv(nil); got.ArgvLen != 0 {
		t.Errorf("ArgvLen = %d, want 0 for no argv", got.ArgvLen)
	}
}

// The listing is what an operator reads to decide what to kill, so a two-word
// argument must not read as two arguments.
func TestExecRunArgvStringQuotesSpaces(t *testing.T) {
	got := ExecRunArgvString(buildExecArgv([]string{"sh", "-c", "echo hi"}))
	if got != `sh -c "echo hi"` {
		t.Errorf("ExecRunArgvString = %q, want the third argument quoted", got)
	}
	plain := ExecRunArgvString(buildExecArgv([]string{"make", "test"}))
	if plain != "make test" {
		t.Errorf("ExecRunArgvString = %q, want no quoting when none is needed", plain)
	}
}

// An empty listing says so rather than printing a bare header: "no running
// execs" is an answer, an empty table is ambiguous with a broken query.
func TestExecRunInfoLinesEmpty(t *testing.T) {
	lines := ExecRunInfoLines(nil)
	if len(lines) != 1 || !strings.Contains(lines[0], "no running execs") {
		t.Errorf("lines = %v, want one line saying there are none", lines)
	}
}

// The JSON form carries what the text form abbreviates: the FULL task id and
// the argv as a list, so a consumer never has to re-split a rendered string.
func TestExecRunInfoJSONLineCarriesTheFullRow(t *testing.T) {
	e := protocol.ExecRunInfo{
		ExecId:        4,
		StartedUnixMs: 1700000000000,
		Argv:          buildExecArgv([]string{"sh", "-c", "echo hi"}),
		OriginKind:    protocol.ClientKind_Cli,
	}
	e.TaskId.Id = [16]byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}
	e.SetOriginCid([]byte("ws:127.0.0.1:8539-3"))

	line := ExecRunInfoJSONLine(&e)
	for _, want := range []string{
		`"exec_id":4`,
		`"task_id":"00112233445566778899aabbccddeeff"`,
		`"argv":["sh","-c","echo hi"]`,
		`"origin_cid":"ws:127.0.0.1:8539-3"`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("JSON line %s is missing %s", line, want)
		}
	}
}

func TestExecRunStatusErrorNamesTheReason(t *testing.T) {
	if err := execRunStatusError("abc", protocol.ExecRunStatus_Ok); err != nil {
		t.Errorf("ok must be no error, got %v", err)
	}
	// no_worktree is the one an operator will actually hit, and the message has
	// to say WHICH tasks still have one — otherwise it reads as "finished tasks
	// are unreachable", which is false.
	err := execRunStatusError("abc", protocol.ExecRunStatus_NoWorktree)
	if err == nil || !strings.Contains(err.Error(), "uncommitted work") {
		t.Errorf("no_worktree error = %v, want it to explain which tasks keep a worktree", err)
	}
	if err := execRunStatusError("abc", protocol.ExecRunStatus_Denied); err == nil ||
		!strings.Contains(err.Error(), "exec_run") {
		t.Errorf("denied error = %v, want it to name the capability", err)
	}
}

// SshdParent renames the shell, so a caller that supplied its own argv has
// asked for something with no shell in it to rename. Caught HERE rather than
// on the runner, which cannot tell that case from a caller that simply forgot
// ShellLine, and would have to guess which one was meant.
func TestExecRunRefusesSshdParentWithoutShellLine(t *testing.T) {
	c := &Client{}
	_, err := c.ExecRun(context.Background(),
		"0123456789abcdef0123456789abcdef",
		[]string{"powershell", "-Command", "echo hi"},
		ExecRunOpts{SshdParent: true})
	if err == nil {
		t.Fatal("ExecRun(SshdParent, no ShellLine) = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "ShellLine") {
		t.Errorf("error %q does not name the flag that is missing", err)
	}
}

// The two new flags reach the wire. A setter that was declared but never
// called is invisible from the caller's side: the request encodes, the exec
// runs, and the property the caller asked for is simply absent.
func TestExecRunOptsReachTheRequestFlags(t *testing.T) {
	var body protocol.ExecRunRequest
	body.SetShellLine(true)
	body.SetDetached(true)
	body.SetSshdParent(true)
	if !body.ShellLine() || !body.Detached() || !body.SshdParent() {
		t.Fatalf("flags did not round-trip: shell=%v detached=%v sshd=%v",
			body.ShellLine(), body.Detached(), body.SshdParent())
	}
	// The bits share a byte with stdin_enabled, and a new flag declared BEFORE
	// an existing one shifts every bit behind it — an old peer then reads
	// shell_line as stdin_enabled, which decodes cleanly and means something
	// else. Setting one must not disturb another.
	body.SetStdinEnabled(false)
	if !body.ShellLine() || !body.Detached() || !body.SshdParent() {
		t.Error("setting stdin_enabled disturbed the flags declared after it")
	}
}
