package runner

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The gate is the DIRECTORY existing, not the task's status: a task that ended
// dirty keeps its worktree and is a valid target, while one that ended clean
// had it removed and is not.
func TestExecRunDir(t *testing.T) {
	s := &Session{}
	repo := t.TempDir()
	id := "0123456789abcdef0123456789abcdef"
	wt := filepath.Join(repo, ".harness-worktrees", id)

	dir, ok := s.execRunDir(repo, id)
	if dir != wt {
		t.Errorf("dir = %q, want the task's worktree %q", dir, wt)
	}
	if ok {
		t.Fatal("a repo with no worktrees must report none")
	}

	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory alone is NOT enough: an orphan left by a crashed cleanup
	// exists and holds no .git, and a command run in it would act on something
	// that is not the task's tree while reporting success. Same predicate
	// git_query uses (isWorktreeRoot).
	if _, ok := s.execRunDir(repo, id); ok {
		t.Error("a directory with no .git entry must not count as a worktree")
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.execRunDir(repo, id); !ok {
		t.Error("a real worktree checkout must be reported")
	}
}

// A plain FILE at the worktree path is not a worktree. Reporting it as one
// would send the runner into an exec with a cwd that cannot be entered, and
// the failure would surface as an opaque chdir error instead of no_worktree.
func TestExecRunDirRejectsAFile(t *testing.T) {
	s := &Session{}
	repo := t.TempDir()
	id := "ffffffffffffffffffffffffffffffff"
	wt := filepath.Join(repo, ".harness-worktrees", id)
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wt, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.execRunDir(repo, id); ok {
		t.Error("a plain file must not count as a worktree")
	}
}

// Under NoWorktree the runner never creates one and every task runs directly in
// the repo, so the repo IS the task's working directory. A runner configured
// that way refused every exec until this branch existed — found by running the
// verb against the dummy harness, whose runner is exactly that shape.
func TestExecRunDirNoWorktreeMode(t *testing.T) {
	repo := t.TempDir()
	id := "0123456789abcdef0123456789abcdef"
	s := &Session{NoWorktree: true}

	dir, ok := s.execRunDir(repo, id)
	if dir != repo {
		t.Errorf("dir = %q, want the repo itself %q", dir, repo)
	}
	if !ok {
		t.Error("the repo exists, so an exec has somewhere to run")
	}

	// No .git required here: the repo is the agent's cwd in this mode whether
	// or not it is a checkout, and demanding one would refuse the very setup
	// the mode exists for.
	if _, ok := (&Session{NoWorktree: true}).execRunDir(filepath.Join(repo, "nope"), id); ok {
		t.Error("a repo path that does not exist must still be refused")
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

// shell_line hands the runner ONE line for ITS OWN shell. The choice is made
// here because this is the only party that knows what the far side has: the ssh
// gateway hard-coded `sh -c` and it worked on one Windows box only because Git
// for Windows had put sh on PATH.
func TestShellLineArgvPicksThePlatformShell(t *testing.T) {
	got, err := shellLineArgv([]string{"echo hi | wc -l"}, false)
	if err != nil {
		t.Fatalf("shellLineArgv: %v", err)
	}
	var want []string
	if runtime.GOOS == "windows" {
		want = []string{"cmd", "/c", "echo hi | wc -l"}
	} else {
		want = []string{"sh", "-c", "echo hi | wc -l"}
	}
	if len(got) != len(want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv = %q, want %q", got, want)
		}
	}
}

// The line is passed WHOLE. Splitting it would defeat the point: the caller
// asked for shell interpretation precisely because quotes, pipes and
// redirections are in there.
func TestShellLineArgvDoesNotSplitTheLine(t *testing.T) {
	got, err := shellLineArgv([]string{`echo "one two" > out.txt`}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got[len(got)-1] != `echo "one two" > out.txt` {
		t.Errorf("last element = %q, want the line intact", got[len(got)-1])
	}
}

// Anything but one element is a caller that built an argv AND asked for shell
// interpretation, which cannot both be meant. Refused rather than guessed at:
// picking either reading silently would run something the caller did not write.
func TestShellLineArgvRefusesAnythingButOneElement(t *testing.T) {
	for _, argv := range [][]string{nil, {}, {"sh", "-c", "true"}, {"a", "b"}} {
		if _, err := shellLineArgv(argv, false); err == nil {
			t.Errorf("shellLineArgv(%q) = nil error, want a refusal", argv)
		}
	}
}

// sshd_parent renames the SHELL, so off Windows there is nothing it can mean.
// Refused rather than ignored: a runner that ran the command anyway would
// report success for a property the command does not have, and the caller —
// a bootstrap that checks its own ancestry — would fail later and somewhere
// else.
func TestShellLineArgvRefusesSshdParentOffWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Windows path stages a shim instead of refusing")
	}
	_, err := shellLineArgv([]string{"echo hi"}, true)
	if err == nil {
		t.Fatal("shellLineArgv(sshdParent=true) = nil error, want a refusal naming the platform")
	}
	if !strings.Contains(err.Error(), runtime.GOOS) {
		t.Errorf("error %q does not name the platform it refused on", err)
	}
}

// sshd_parent must not disturb the ordinary path. A runner that started
// staging a shim for every shell line would put an unnecessary file copy in
// front of every `exec --shell`.
func TestShellLineArgvWithoutSshdParentIsUnchanged(t *testing.T) {
	got, err := shellLineArgv([]string{"echo hi"}, false)
	if err != nil {
		t.Fatalf("shellLineArgv: %v", err)
	}
	want := "sh"
	if runtime.GOOS == "windows" {
		want = "cmd"
	}
	if got[0] != want {
		t.Errorf("shell = %q, want %q", got[0], want)
	}
}
