//go:build integration

package integration

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner"
	"github.com/on-keyday/agent-harness/server"
)

// initRepo creates a tempdir git repo with a single commit.
func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README")
	run("commit", "-m", "init")
	return dir
}

func waitForFile(t *testing.T, path string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func TestSubmitFakeClaudeE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}

	repo := initRepo(t)
	fakeClaude, err := filepath.Abs("../testdata/fake-claude.sh")
	if err != nil {
		t.Fatal(err)
	}

	// Random-ish port to avoid collision when this test is run repeatedly.
	addr := "localhost:18539"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start server.
	s := server.New(server.Config{
		Addr:    addr,
		DataDir: t.TempDir(),
	})
	serverDone := make(chan error, 1)
	go func() { serverDone <- s.Run(ctx) }()

	// Give the server a moment to start listening.
	time.Sleep(300 * time.Millisecond)

	// Start runner.
	runnerDone := make(chan error, 1)
	go func() {
		runnerDone <- runner.Run(ctx, runner.Config{
			ServerAddr: addr,
			RepoPath:   repo,
			ClaudeBin:  fakeClaude,
		})
	}()

	// Give the runner time to connect, hello, and become Idle.
	time.Sleep(500 * time.Millisecond)

	// Submit a task.
	taskID, err := cli.Submit(ctx, addr, repo, "do-the-thing")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	t.Logf("submitted task %s", taskID)

	// Wait for the worktree's `hello.txt` to appear (fake-claude.sh creates it).
	wt := filepath.Join(repo, ".harness-worktrees", taskID, "hello.txt")
	if !waitForFile(t, wt, 15*time.Second) {
		t.Fatalf("hello.txt did not appear at %s within timeout", wt)
	}
	t.Logf("worktree artifact present at %s", wt)

	// Verify task status: poll List until the task shows Succeeded or we time out.
	deadline := time.Now().Add(10 * time.Second)
	var lastOutput string
	for time.Now().Before(deadline) {
		var buf bytes.Buffer
		if err := cli.List(ctx, addr, &buf); err != nil {
			t.Fatalf("list: %v", err)
		}
		lastOutput = buf.String()
		if strings.Contains(lastOutput, "Succeeded") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !strings.Contains(lastOutput, "Succeeded") {
		t.Fatalf("task never reached Succeeded; last list output:\n%s", lastOutput)
	}
	if !strings.Contains(lastOutput, taskID[:12]) {
		t.Fatalf("task id prefix not in list output:\n%s", lastOutput)
	}

	// Cancel runs to clean up goroutines.
	cancel()
	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Log("server did not exit within 2s of cancel — leaking goroutine")
	}
	select {
	case <-runnerDone:
	case <-time.After(2 * time.Second):
		t.Log("runner did not exit within 2s of cancel — leaking goroutine")
	}
}
