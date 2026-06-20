//go:build !js

package cli

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// makeRunnerInfo is a helper that constructs a minimal RunnerInfo for tests.
func makeRunnerInfo(hostname string, status protocol.RunnerStatus, maxTasks int, roots []string, activeCount int) protocol.RunnerInfo {
	r := protocol.RunnerInfo{}
	r.SetHostname([]byte(hostname))
	r.Status = status
	r.MaxTasks = uint16(maxTasks)

	ar := make([]protocol.AllowedRoot, len(roots))
	for i, root := range roots {
		ar[i].SetPath([]byte(root))
	}
	r.SetAllowedRoots(ar)

	active := make([]protocol.ActiveTaskRef, activeCount)
	for i := range active {
		active[i].SetRepoPath([]byte(roots[0]))
	}
	r.ActiveTasks = active
	r.ActiveTasksLen = uint16(activeCount)

	// Use a placeholder IPv4 so taskIDStr doesn't render "-"
	r.Id.SetIpAddr([]byte{127, 0, 0, 1})
	r.Id.IpAddrLen = 4

	return r
}

// TestLsRunnerColumnsHostTasksRoots verifies the RUNNERS section renders
// host=, tasks=N/M, and roots= columns.
func TestLsRunnerColumnsHostTasksRoots(t *testing.T) {
	roots := []string{"/home/user/project"}
	r := makeRunnerInfo("gmkhost", protocol.RunnerStatus_Idle, 4, roots, 2)
	lr := &protocol.ListResultBody{
		Runners: []protocol.RunnerInfo{r},
	}
	lr.RunnersLen = 1

	var out strings.Builder
	renderList(lr, &out)
	s := out.String()

	if !strings.Contains(s, "host=gmkhost") {
		t.Errorf("expected host=gmkhost in output:\n%s", s)
	}
	if !strings.Contains(s, "tasks=2/4") {
		t.Errorf("expected tasks=2/4 in output:\n%s", s)
	}
	if !strings.Contains(s, "roots=/home/user/project") {
		t.Errorf("expected roots=/home/user/project in output:\n%s", s)
	}
	// Must NOT contain old repo= column
	if strings.Contains(s, "repo=") {
		t.Errorf("unexpected repo= column in output:\n%s", s)
	}
}

// TestLsRunnerStatusStrings verifies Idle/Busy/Offline status strings.
func TestLsRunnerStatusStrings(t *testing.T) {
	tests := []struct {
		status protocol.RunnerStatus
		want   string
	}{
		{protocol.RunnerStatus_Idle, "Idle"},
		{protocol.RunnerStatus_Busy, "Busy"},
	}
	for _, tc := range tests {
		got := runnerStatusStr(tc.status)
		if !strings.Contains(got, tc.want) {
			t.Errorf("runnerStatusStr(%v) = %q, want contains %q", tc.status, got, tc.want)
		}
	}
}

// TestLsMultipleRoots verifies multiple roots are comma-separated.
func TestLsMultipleRoots(t *testing.T) {
	roots := []string{"/repo/a", "/repo/b"}
	r := makeRunnerInfo("srv", protocol.RunnerStatus_Idle, 2, roots, 0)
	lr := &protocol.ListResultBody{
		Runners:    []protocol.RunnerInfo{r},
		RunnersLen: 1,
	}

	var out strings.Builder
	renderList(lr, &out)
	s := out.String()

	if !strings.Contains(s, "/repo/a,/repo/b") && !strings.Contains(s, "/repo/a") {
		t.Errorf("expected both roots in output:\n%s", s)
	}
}

// TestLsNoRunners verifies the "(none)" placeholder.
func TestLsNoRunners(t *testing.T) {
	lr := &protocol.ListResultBody{}

	var out strings.Builder
	renderList(lr, &out)
	s := out.String()

	if !strings.Contains(s, "(none)") {
		t.Errorf("expected (none) when no runners:\n%s", s)
	}
}

// TestLsTaskRow verifies a task row is rendered with repo= and status.
func TestLsTaskRow(t *testing.T) {
	var taskID protocol.TaskID
	taskID.Id[0] = 0xab
	task := protocol.TaskInfo{
		Id:       taskID,
		Status:   protocol.TaskStatus_Running,
		RepoPath: []byte("/home/user/myrepo"),
		Prompt:   []byte("fix the bug"),
	}
	task.RepoPathLen = uint16(len(task.RepoPath))
	task.PromptLen = uint32(len(task.Prompt))

	lr := &protocol.ListResultBody{
		Tasks:    []protocol.TaskInfo{task},
		TasksLen: 1,
	}

	var out strings.Builder
	renderList(lr, &out)
	s := out.String()

	if !strings.Contains(s, "Running") {
		t.Errorf("expected Running in output:\n%s", s)
	}
	if !strings.Contains(s, "/home/user/myrepo") {
		t.Errorf("expected repo path in output:\n%s", s)
	}
	// Zero-value kind renders as the oneshot column.
	if !strings.Contains(s, "oneshot") {
		t.Errorf("expected oneshot kind column in output:\n%s", s)
	}
	// No error / no exit code → the optional suffixes must be absent.
	if strings.Contains(s, "err=") || strings.Contains(s, "exit=") {
		t.Errorf("unexpected err=/exit= suffix on a clean running task:\n%s", s)
	}
}

// TestLsTaskKindInteractive verifies an interactive (session) task renders
// the interactive kind column, explaining its empty prompt.
func TestLsTaskKindInteractive(t *testing.T) {
	var taskID protocol.TaskID
	taskID.Id[0] = 0xcd
	task := protocol.TaskInfo{
		Id:     taskID,
		Status: protocol.TaskStatus_Detached,
		Kind:   protocol.TaskKind_Interactive,
	}

	lr := &protocol.ListResultBody{
		Tasks:    []protocol.TaskInfo{task},
		TasksLen: 1,
	}

	var out strings.Builder
	renderList(lr, &out)
	s := out.String()

	if !strings.Contains(s, "interactive") {
		t.Errorf("expected interactive kind column in output:\n%s", s)
	}
}

// TestLsTaskErrorAndExitSuffix verifies err= renders for a recorded failure
// reason (e.g. runner_disconnected) and exit= for a non-zero exit code on a
// finished task.
func TestLsTaskErrorAndExitSuffix(t *testing.T) {
	var taskID protocol.TaskID
	taskID.Id[0] = 0xef
	task := protocol.TaskInfo{
		Id:       taskID,
		Status:   protocol.TaskStatus_Failed,
		Kind:     protocol.TaskKind_Interactive,
		EndedAt:  1,
		ExitCode: 137,
	}
	task.SetErrorMessage([]byte("runner_disconnected"))

	lr := &protocol.ListResultBody{
		Tasks:    []protocol.TaskInfo{task},
		TasksLen: 1,
	}

	var out strings.Builder
	renderList(lr, &out)
	s := out.String()

	if !strings.Contains(s, `err="runner_disconnected"`) {
		t.Errorf("expected err= suffix in output:\n%s", s)
	}
	if !strings.Contains(s, "exit=137") {
		t.Errorf("expected exit=137 suffix in output:\n%s", s)
	}
}

// TestLsTaskCapsSegment verifies the caps= segment appears in a task row.
// Spawn|FileRead renders as "spawn,file_read"; zero-value Capability renders
// as "none" (Capability_None == 0).
func TestLsTaskCapsSegment(t *testing.T) {
	var taskID protocol.TaskID
	taskID.Id[0] = 0x11
	task := protocol.TaskInfo{
		Id:           taskID,
		Status:       protocol.TaskStatus_Running,
		Capabilities: protocol.Capability_Spawn | protocol.Capability_FileRead,
	}
	task.SetRepoPath([]byte("/repo"))
	task.SetPrompt([]byte("do stuff"))

	lr := &protocol.ListResultBody{
		Tasks:    []protocol.TaskInfo{task},
		TasksLen: 1,
	}

	var out strings.Builder
	renderList(lr, &out)
	s := out.String()

	if !strings.Contains(s, "caps=spawn,file_read") {
		t.Errorf("expected caps=spawn,file_read in output:\n%s", s)
	}
}

// TestLsTaskCapsNone verifies that a zero-value Capabilities renders as "none".
func TestLsTaskCapsNone(t *testing.T) {
	var taskID protocol.TaskID
	taskID.Id[0] = 0x22
	task := protocol.TaskInfo{
		Id:     taskID,
		Status: protocol.TaskStatus_Queued,
		// Capabilities zero-value == Capability_None
	}
	task.SetRepoPath([]byte("/repo"))

	lr := &protocol.ListResultBody{
		Tasks:    []protocol.TaskInfo{task},
		TasksLen: 1,
	}

	var out strings.Builder
	renderList(lr, &out)
	s := out.String()

	if !strings.Contains(s, "caps=none") {
		t.Errorf("expected caps=none in output:\n%s", s)
	}
}

// TestTaskIDStr verifies taskIDStr behaviour: full-length hex with "-" for
// the all-zero placeholder.
func TestTaskIDStr(t *testing.T) {
	if got := taskIDStr([]byte{0, 0, 0, 0}); got != "-" {
		t.Errorf("taskIDStr(zeros) = %q, want -", got)
	}
	got := taskIDStr([]byte{0xde, 0xad, 0xbe, 0xef})
	if got != "deadbeef" {
		t.Errorf("taskIDStr(4 bytes) = %q, want deadbeef", got)
	}
	// Full 16-byte task id renders 32 hex chars.
	full := make([]byte, 16)
	for i := range full {
		full[i] = byte(i)
	}
	if got := taskIDStr(full); got != "000102030405060708090a0b0c0d0e0f" {
		t.Errorf("taskIDStr(16 bytes) = %q", got)
	}
}
