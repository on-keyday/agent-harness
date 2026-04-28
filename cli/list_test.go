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

	// Use a placeholder IPv4 so shortHex doesn't render "-"
	r.Id.SetIpAddr([]byte{127, 0, 0, 1})
	r.Id.IpAddrLen = 4

	return r
}

// TestLsRunnerColumnsHostTasksRoots verifies the RUNNERS section renders
// host=, tasks=N/M, and roots= columns.
func TestLsRunnerColumnsHostTasksRoots(t *testing.T) {
	roots := []string{"/home/user/project"}
	r := makeRunnerInfo("gmkhost", protocol.RunnerStatus_Idle, 4, roots, 2)
	lr := &protocol.ListResult{
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
	lr := &protocol.ListResult{
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
	lr := &protocol.ListResult{}

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

	lr := &protocol.ListResult{
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
}

// TestLsShortHex verifies shortHex behaviour.
func TestLsShortHex(t *testing.T) {
	// All zero = "-"
	if got := shortHex([]byte{0, 0, 0, 0}); got != "-" {
		t.Errorf("shortHex(zeros) = %q, want -", got)
	}
	// Non-zero = hex prefix
	got := shortHex([]byte{0xde, 0xad, 0xbe, 0xef})
	if got != "deadbeef" {
		t.Errorf("shortHex = %q, want deadbeef", got)
	}
}
