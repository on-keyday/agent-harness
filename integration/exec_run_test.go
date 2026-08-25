//go:build integration

package integration

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TestExecRunE2E drives `exec` end to end against a live server + runner + a
// task with a worktree.
func TestExecRunE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("the commands here are POSIX shell — skipping on Windows")
	}
	clearAgentEnv(t)

	serverCID := startServer(t)
	repo := tempRepo(t)
	startRunner(t, serverCID, runnerOpts{
		// Four: the main task, plus the dirty and clean ones the terminal-task
		// cases open. At two, the last openLiveSession got runner_busy and the
		// case failed for capacity rather than for what it asserts.
		MaxTasks:  4,
		Roots:     []string{repo},
		ClaudeBin: fakeClaudeSlowPath(t),
	})

	c := dialClient(t, serverCID)
	taskID := openLiveSession(t, c, repo)
	worktree := filepath.Join(repo, ".harness-worktrees", taskID)

	t.Run("streams_are_separate", func(t *testing.T) {
		// The defect this whole feature exists to avoid is the two streams
		// merging. A test that read one combined buffer would pass on
		// `session exec`, which is the thing being replaced.
		var out, errb bytes.Buffer
		res, err := c.ExecRun(context.Background(), taskID,
			[]string{"sh", "-c", "echo OUT; echo ERR 1>&2"},
			cli.ExecRunOpts{Stdout: &out, Stderr: &errb})
		if err != nil {
			t.Fatalf("ExecRun: %v", err)
		}
		if res.Kind != protocol.ExecEventKind_Exited || res.ExitCode != 0 {
			t.Fatalf("result = %+v, want exited/0", res)
		}
		if !strings.Contains(out.String(), "OUT") {
			t.Errorf("stdout = %q, want OUT", out.String())
		}
		if strings.Contains(out.String(), "ERR") {
			t.Errorf("stdout = %q, must NOT carry the stderr line", out.String())
		}
		if !strings.Contains(errb.String(), "ERR") {
			t.Errorf("stderr = %q, want ERR", errb.String())
		}
		if strings.Contains(errb.String(), "OUT") {
			t.Errorf("stderr = %q, must NOT carry the stdout line", errb.String())
		}
	})

	t.Run("exit_code", func(t *testing.T) {
		res, err := c.ExecRun(context.Background(), taskID,
			[]string{"sh", "-c", "exit 7"}, cli.ExecRunOpts{})
		if err != nil {
			t.Fatalf("ExecRun: %v", err)
		}
		if res.Kind != protocol.ExecEventKind_Exited || res.ExitCode != 7 {
			t.Errorf("result = %+v, want exited with 7", res)
		}
	})

	t.Run("missing_binary_is_failed_not_exited", func(t *testing.T) {
		res, err := c.ExecRun(context.Background(), taskID,
			[]string{"definitely-not-a-real-binary-xyz"}, cli.ExecRunOpts{})
		if err != nil {
			t.Fatalf("ExecRun: %v", err)
		}
		// NOT exited with an invented 127: there is no shell here to have that
		// convention, and reporting one would be a lie about what happened.
		if res.Kind != protocol.ExecEventKind_Failed {
			t.Errorf("kind = %v, want failed", res.Kind)
		}
		if res.Detail == "" {
			t.Error("a failed exec must say why")
		}
	})

	t.Run("cwd_is_the_worktree", func(t *testing.T) {
		var out bytes.Buffer
		if _, err := c.ExecRun(context.Background(), taskID,
			[]string{"pwd"}, cli.ExecRunOpts{Stdout: &out}); err != nil {
			t.Fatalf("ExecRun: %v", err)
		}
		got := strings.TrimSpace(out.String())
		// Compare the resolved paths: a repo under /tmp is a symlink on macOS.
		wantResolved, _ := filepath.EvalSymlinks(worktree)
		gotResolved, _ := filepath.EvalSymlinks(got)
		if gotResolved != wantResolved {
			t.Errorf("pwd = %q, want the task's worktree %q", got, worktree)
		}
	})

	t.Run("stdin_is_carried", func(t *testing.T) {
		var out bytes.Buffer
		if _, err := c.ExecRun(context.Background(), taskID,
			[]string{"cat"},
			cli.ExecRunOpts{Stdin: strings.NewReader("hello-stdin\n"), Stdout: &out}); err != nil {
			t.Fatalf("ExecRun: %v", err)
		}
		if !strings.Contains(out.String(), "hello-stdin") {
			t.Errorf("stdout = %q, want what was written to stdin", out.String())
		}
	})

	// A caller with NO stdin must still leave the child with one that is at
	// EOF. This is the TUI's and the WebUI's path — neither passes a Stdin —
	// and it hung forever against a live runner: `exec <task> -- bash` sat with
	// the shell waiting on an EOF nobody was going to send, the child still
	// alive minutes later and the row still in `exec ls`.
	//
	// The wire says as much (stdin_enabled=0) and nothing on the runner reads
	// that flag, so the close comes from the client. A timeout, not a plain
	// call: the failure mode is a hang, which without one takes the whole
	// package's deadline instead of this case.
	t.Run("no_stdin_gives_the_child_dev_null", func(t *testing.T) {
		var out bytes.Buffer
		done := make(chan cli.ExecRunResult, 1)
		go func() {
			// The child asserts and reports which assertion failed as its exit
			// code. Asserting only that it TERMINATES would pass on a pipe the
			// client closed a few ms later, which is the weaker thing this
			// replaced: that leaves a window where stdin is open and empty.
			res, err := c.ExecRun(context.Background(), taskID,
				[]string{"sh", "-c", `
					cat; [ $? = 0 ] || exit 21
					[ -r /dev/stdin ] || exit 22
					[ -t 0 ] && exit 23
					readlink /proc/self/fd/0
					exit 0`},
				cli.ExecRunOpts{Stdout: &out})
			if err != nil {
				t.Errorf("ExecRun: %v", err)
			}
			done <- res
		}()
		select {
		case res := <-done:
			if res.Kind != protocol.ExecEventKind_Exited || res.ExitCode != 0 {
				t.Fatalf("result = %+v (21=cat failed, 22=fd0 not readable, 23=fd0 is a tty)", res)
			}
			// Linux-only detail, so it is checked when it is there rather than
			// asserted unconditionally: the exit-code checks above carry the
			// property on every platform.
			if got := strings.TrimSpace(out.String()); got != "" && got != os.DevNull {
				t.Errorf("the child's stdin is %q, want %s", got, os.DevNull)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("a command that reads stdin never ended: its stdin was left open")
		}
	})

	t.Run("ls_shows_a_running_exec_and_the_task_reports_it", func(t *testing.T) {
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = c.ExecRun(context.Background(), taskID,
				[]string{"sh", "-c", "sleep 5"}, cli.ExecRunOpts{})
		}()

		var execID uint64
		eventually(t, func() bool {
			execs, err := c.ExecRunListWith(context.Background(), "")
			if err != nil || len(execs) == 0 {
				return false
			}
			execID = execs[0].ExecId
			return true
		}, 10*time.Second, 100*time.Millisecond, "the running exec to appear in exec ls")

		if n := getTask(t, c, taskID).ExecCount; n != 1 {
			t.Errorf("TaskInfo.ExecCount = %d, want 1 while an exec runs", n)
		}

		if err := c.ExecRunKillWith(context.Background(), execID); err != nil {
			t.Fatalf("ExecRunKill: %v", err)
		}
		<-done

		eventually(t, func() bool {
			execs, err := c.ExecRunListWith(context.Background(), "")
			return err == nil && len(execs) == 0
		}, 10*time.Second, 100*time.Millisecond, "the killed exec to leave exec ls")
	})

	t.Run("exec_count_is_zero_not_absent", func(t *testing.T) {
		// Zero is a measurement. A row that elided it would read as "this row
		// does not report execs", which is the ambiguity the count exists to
		// remove — and a non-zero-only assertion cannot tell the two apart.
		if n := getTask(t, c, taskID).ExecCount; n != 0 {
			t.Errorf("ExecCount = %d with nothing running, want 0", n)
		}
	})

	t.Run("a_second_client_sees_it", func(t *testing.T) {
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = c.ExecRun(context.Background(), taskID,
				[]string{"sh", "-c", "sleep 5"}, cli.ExecRunOpts{})
		}()
		other := dialClient(t, serverCID)

		var execID uint64
		eventually(t, func() bool {
			execs, err := other.ExecRunListWith(context.Background(), "")
			if err != nil || len(execs) == 0 {
				return false
			}
			execID = execs[0].ExecId
			return true
		}, 10*time.Second, 100*time.Millisecond, "the exec to be visible to a client that did not start it")

		// And it can stop it: the registry is shared, so `exec kill` reaches
		// another client's exec exactly as `forward kill` does.
		if err := other.ExecRunKillWith(context.Background(), execID); err != nil {
			t.Errorf("a second client must be able to kill it: %v", err)
		}
		<-done
	})

	t.Run("terminal_task_that_ended_DIRTY_still_runs", func(t *testing.T) {
		// The gate is the worktree, not the status. A task that ended with
		// uncommitted work keeps its tree, and running git status or make test
		// in it is exactly what an operator wants then.
		dirtyID := openLiveSession(t, c, repo)
		dirtyWT := filepath.Join(repo, ".harness-worktrees", dirtyID)
		if err := os.WriteFile(filepath.Join(dirtyWT, "uncommitted.txt"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := c.Cancel(context.Background(), dirtyID); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		eventually(t, func() bool {
			return isTerminalStatus(getTask(t, c, dirtyID).Status)
		}, 15*time.Second, 200*time.Millisecond, "the dirty task to reach a terminal status")

		// The setup only means something if the worktree actually survived; a
		// removed one would make the assertion below pass for the wrong reason.
		eventually(t, func() bool {
			st, err := os.Stat(dirtyWT)
			return err == nil && st.IsDir()
		}, 10*time.Second, 200*time.Millisecond, "the dirty worktree to be retained")

		var out bytes.Buffer
		res, err := c.ExecRun(context.Background(), dirtyID,
			[]string{"cat", "uncommitted.txt"}, cli.ExecRunOpts{Stdout: &out})
		if err != nil {
			t.Fatalf("exec against a terminal-but-dirty task must run: %v", err)
		}
		if res.Kind != protocol.ExecEventKind_Exited || res.ExitCode != 0 {
			t.Errorf("result = %+v, want exited/0", res)
		}
		if !strings.Contains(out.String(), "x") {
			t.Errorf("stdout = %q, want the file the task left behind", out.String())
		}
	})

	t.Run("a_task_with_no_worktree_is_refused", func(t *testing.T) {
		cleanID := openLiveSession(t, c, repo)
		cleanWT := filepath.Join(repo, ".harness-worktrees", cleanID)
		if err := c.Cancel(context.Background(), cleanID); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		eventually(t, func() bool {
			_, err := os.Stat(cleanWT)
			return os.IsNotExist(err)
		}, 20*time.Second, 200*time.Millisecond, "the clean worktree to be removed")

		// The refusal arrives as a FAILED outcome, not as a transport error and
		// not as a status on the open response: the server has no way to know
		// whether the tree is on disk — that is the runner's host — so the gate
		// necessarily runs there and reports back the same way a child that
		// could not start does.
		res, err := c.ExecRun(context.Background(), cleanID, []string{"pwd"}, cli.ExecRunOpts{})
		if err != nil {
			t.Fatalf("ExecRun: %v", err)
		}
		if res.Kind != protocol.ExecEventKind_Failed {
			t.Errorf("kind = %v, want failed for a task with no worktree", res.Kind)
		}
		if !strings.Contains(res.Detail, "no worktree") {
			t.Errorf("detail = %q, want it to name the missing worktree", res.Detail)
		}
	})
}

// openLiveSession opens a detachable session and waits for it to run, so its
// worktree exists on disk. The stream is drained in the background; closing it
// would detach, which is not what these cases are about.
func openLiveSession(t *testing.T, c *cli.Client, repo string) string {
	t.Helper()
	sel := protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any}
	stream, taskID, err := c.OpenInteractive(context.Background(), repo, cli.SessionOpts{
		Selector: sel, InitialRows: 24, InitialCols: 80,
	})
	if err != nil {
		t.Fatalf("OpenInteractive: %v", err)
	}
	go func() { _, _ = io.Copy(io.Discard, stream.Stdout()) }()
	t.Cleanup(func() { _ = stream.Close() })

	eventually(t, func() bool {
		return getTask(t, c, taskID).Status == protocol.TaskStatus_Running
	}, 20*time.Second, 100*time.Millisecond, "the session to reach Running")

	// Running is reported by the runner and the worktree appears alongside it,
	// so a caller that writes into the tree the instant Running lands can lose
	// the race — which it did, as "uncommitted.txt: no such file or directory".
	eventually(t, func() bool {
		st, err := os.Stat(filepath.Join(repo, ".harness-worktrees", taskID))
		return err == nil && st.IsDir()
	}, 20*time.Second, 100*time.Millisecond, "the task's worktree to exist on disk")
	return taskID
}

func isTerminalStatus(s protocol.TaskStatus) bool {
	switch s {
	case protocol.TaskStatus_Succeeded, protocol.TaskStatus_Failed, protocol.TaskStatus_Cancelled:
		return true
	}
	return false
}
