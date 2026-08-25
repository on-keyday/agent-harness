package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestExecRunDir(t *testing.T) {
	got := execRunDir("/repo", "0123456789abcdef0123456789abcdef")
	want := filepath.Join("/repo", ".harness-worktrees", "0123456789abcdef0123456789abcdef")
	if got != want {
		t.Errorf("execRunDir = %q, want %q", got, want)
	}
}

// The gate is the DIRECTORY existing, not the task's status: a task that ended
// dirty keeps its worktree and is a valid target, while one that ended clean
// had it removed and is not. Both reduce to "does this path exist".
func TestExecRunDirExists(t *testing.T) {
	repo := t.TempDir()
	id := "0123456789abcdef0123456789abcdef"
	if execRunDirExists(repo, id) {
		t.Fatal("a repo with no worktrees must report none")
	}
	if err := os.MkdirAll(execRunDir(repo, id), 0o755); err != nil {
		t.Fatal(err)
	}
	if !execRunDirExists(repo, id) {
		t.Error("an existing worktree directory must be reported")
	}
}

// A plain FILE at the worktree path is not a worktree. Reporting it as one
// would send the runner into an exec with a cwd that cannot be entered, and
// the failure would surface as an opaque chdir error instead of no_worktree.
func TestExecRunDirExistsRejectsAFile(t *testing.T) {
	repo := t.TempDir()
	id := "ffffffffffffffffffffffffffffffff"
	if err := os.MkdirAll(filepath.Dir(execRunDir(repo, id)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(execRunDir(repo, id), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if execRunDirExists(repo, id) {
		t.Error("a plain file must not count as a worktree")
	}
}

// take() must hand the cancel over exactly once: a second close_exec_run for
// the same id — the operator pressing kill twice — must not cancel whatever
// reused that id later.
func TestExecCancelsTakeIsOnce(t *testing.T) {
	var c execCancels
	calls := 0
	c.put(7, func() { calls++ })

	if cancel := c.take(7); cancel == nil {
		t.Fatal("take on a live id must return the cancel")
	} else {
		cancel()
	}
	if cancel := c.take(7); cancel != nil {
		t.Error("take twice must return nil the second time")
	}
	if calls != 1 {
		t.Errorf("cancel called %d times, want 1", calls)
	}
}

func TestExecCancelsUnknownIDIsNil(t *testing.T) {
	var c execCancels
	if cancel := c.take(1); cancel != nil {
		t.Error("take on an id that was never put must return nil")
	}
	// And the same through the handler, which is what a kill arriving after
	// the exec already finished hits.
	s := &Session{}
	s.handleCloseExecRun(&protocol.CloseExecRunRequest{ExecId: 1})
}

func TestExecCancelsCancelsTheContext(t *testing.T) {
	var c execCancels
	ctx, cancel := context.WithCancel(context.Background())
	c.put(3, cancel)
	c.take(3)()
	select {
	case <-ctx.Done():
	default:
		t.Error("the stored cancel did not cancel its context")
	}
}
