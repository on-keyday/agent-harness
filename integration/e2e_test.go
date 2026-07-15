////go:build integration

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
	"github.com/on-keyday/objtrsf/objproto"
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
	clearAgentEnv(t)

	repo := initRepo(t)
	fakeClaude, err := filepath.Abs("../testdata/fake-claude.sh")
	if err != nil {
		t.Fatal(err)
	}

	// Random-ish port to avoid collision when this test is run repeatedly.
	// Use 127.0.0.1 explicitly: "localhost" resolves to ::1 on systems where
	// IPv6 is preferred, but the http server only listens on IPv4 — the dial
	// would then connect-refuse on [::1]:18539.
	addr := "127.0.0.1:18539"
	peerCID, err := objproto.ParseConnectionID("ws:"+addr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse server cid: %v", err)
	}

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
			ServerCID:    peerCID,
			AllowedRoots: []string{repo},
			Profiles:     singleAgentProfile(fakeClaude),
		})
	}()

	// Give the runner time to connect, hello, and become Idle.
	time.Sleep(500 * time.Millisecond)

	// Submit a task.
	taskID, err := cli.Submit(ctx, peerCID, repo, "do-the-thing")
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
		if err := cli.List(ctx, peerCID, &buf); err != nil {
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

// TestSubmitFakeClaudeE2E_NoWorktree mirrors TestSubmitFakeClaudeE2E but starts
// the runner with NoWorktree=true over a non-git tempdir. Verifies the full
// server↔runner↔CLI flow works end-to-end without git worktree machinery.
func TestSubmitFakeClaudeE2E_NoWorktree(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}
	clearAgentEnv(t)

	// Plain tempdir — no git init.
	repo := t.TempDir()
	fakeClaude, err := filepath.Abs("../testdata/fake-claude.sh")
	if err != nil {
		t.Fatal(err)
	}

	addr := "127.0.0.1:18540" // distinct port from TestSubmitFakeClaudeE2E
	peerCID, err := objproto.ParseConnectionID("ws:"+addr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse server cid: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := server.New(server.Config{
		Addr:    addr,
		DataDir: t.TempDir(),
	})
	serverDone := make(chan error, 1)
	go func() { serverDone <- s.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)

	runnerDone := make(chan error, 1)
	go func() {
		runnerDone <- runner.Run(ctx, runner.Config{
			ServerCID:    peerCID,
			AllowedRoots: []string{repo},
			Profiles:     singleAgentProfile(fakeClaude),
			NoWorktree:   true,
		})
	}()
	time.Sleep(500 * time.Millisecond)

	taskID, err := cli.Submit(ctx, peerCID, repo, "nw-e2e")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	t.Logf("submitted task %s", taskID)

	// fake-claude.sh creates hello.txt in cwd. With NoWorktree, cwd == repo.
	helloPath := filepath.Join(repo, "hello.txt")
	if !waitForFile(t, helloPath, 15*time.Second) {
		t.Fatalf("hello.txt did not appear at %s within timeout", helloPath)
	}
	t.Logf("artifact present at %s", helloPath)

	// Verify task reached Succeeded.
	deadline := time.Now().Add(10 * time.Second)
	var lastOutput string
	for time.Now().Before(deadline) {
		var buf bytes.Buffer
		if err := cli.List(ctx, peerCID, &buf); err != nil {
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

	// .harness-worktrees must not exist.
	if _, err := os.Stat(filepath.Join(repo, ".harness-worktrees")); !os.IsNotExist(err) {
		t.Errorf(".harness-worktrees should not exist in NoWorktree mode; stat err=%v", err)
	}

	cancel()
	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Log("server did not exit within 2s of cancel")
	}
	select {
	case <-runnerDone:
	case <-time.After(2 * time.Second):
		t.Log("runner did not exit within 2s of cancel")
	}
}
