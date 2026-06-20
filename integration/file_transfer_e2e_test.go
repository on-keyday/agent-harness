package integration

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/agent-harness/runner"
	"github.com/on-keyday/agent-harness/server"
)

// clearAgentEnv unsets the harness agent identity env vars for the duration
// of t. Integration tests that connect as operators (ClientKind_Cli) must call
// this: buildMergedClientHello auto-upgrades to Agent when all three vars are
// set, which causes capability denials when the test server has no matching
// task entry.
func clearAgentEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HARNESS_RUNNER_ID", "")
	t.Setenv("HARNESS_TASK_ID", "")
	t.Setenv("HARNESS_AUTH_TICKET", "")
}

// TestFileTransferE2E exercises the full client → server → runner splice
// path: push a file into a running task's worktree, ls to verify it appears,
// pull it back, then check the negative paths (already_exists, not_found,
// path escape).
//
// Uses fake-claude-slow.sh so the task stays Running for the duration of
// the test (runner only accepts file ops while the task is live).
func TestFileTransferE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}
	clearAgentEnv(t)

	repo := initRepo(t)
	fakeClaude, err := filepath.Abs("../testdata/fake-claude-slow.sh")
	if err != nil {
		t.Fatal(err)
	}

	addr := "127.0.0.1:18545"
	peerCID, err := objproto.ParseConnectionID("ws:"+addr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse server cid: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	s := server.New(server.Config{Addr: addr, DataDir: t.TempDir()})
	serverDone := make(chan error, 1)
	go func() { serverDone <- s.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)

	runnerDone := make(chan error, 1)
	go func() {
		runnerDone <- runner.Run(ctx, runner.Config{
			ServerCID:    peerCID,
			AllowedRoots: []string{repo},
			ClaudeBin:    fakeClaude,
		})
	}()
	time.Sleep(500 * time.Millisecond)

	taskID, err := cli.Submit(ctx, peerCID, repo, "long-running")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	t.Logf("submitted task %s", taskID)

	// Poll until the worktree exists (signals the runner has started
	// executing the task).
	worktree := filepath.Join(repo, ".harness-worktrees", taskID)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(worktree); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("worktree did not appear: %v", err)
	}

	// Open a CLI client. File ops do not require SayHello.
	c, err := cli.Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	// 1. PUSH: write a local file, push it into the worktree, verify it
	//    landed.
	srcPath := filepath.Join(t.TempDir(), "src.bin")
	if err := os.WriteFile(srcPath, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.FilePush(ctx, taskID, srcPath, "uploaded.bin", false); err != nil {
		t.Fatalf("push: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(worktree, "uploaded.bin"))
	if err != nil {
		t.Fatalf("read pushed file: %v", err)
	}
	if string(got) != "hello world" {
		t.Errorf("pushed content = %q want %q", got, "hello world")
	}

	// 2. LS: confirm uploaded.bin is in the listing.
	var lsBuf bytes.Buffer
	if err := c.FileLs(ctx, taskID, "", &lsBuf); err != nil {
		t.Fatalf("ls: %v", err)
	}
	if !strings.Contains(lsBuf.String(), "uploaded.bin") {
		t.Errorf("ls output missing uploaded.bin:\n%s", lsBuf.String())
	}

	// 3. PULL: copy the file back; verify content matches.
	dstPath := filepath.Join(t.TempDir(), "dst.bin")
	if err := c.FilePull(ctx, taskID, "uploaded.bin", dstPath, false); err != nil {
		t.Fatalf("pull: %v", err)
	}
	pulled, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(pulled) != "hello world" {
		t.Errorf("pulled content = %q want %q", pulled, "hello world")
	}

	// 4. PUSH AGAIN: same path → already_exists.
	if err := c.FilePush(ctx, taskID, srcPath, "uploaded.bin", false); err == nil {
		t.Errorf("second push should fail with already_exists")
	} else if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("second push error mismatch: %v", err)
	}

	// 5. PULL MISSING: not_found.
	if err := c.FilePull(ctx, taskID, "nope.bin", dstPath, false); err == nil {
		t.Errorf("pull of missing file should fail")
	}

	// 6. PATH ESCAPE: push with .. → path_invalid.
	if err := c.FilePush(ctx, taskID, srcPath, "../escape.bin", false); err == nil {
		t.Errorf("escape push should fail")
	}

	// Tear down.
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

func TestFileDirTransferE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}
	clearAgentEnv(t)

	repo := initRepo(t)
	fakeClaude, err := filepath.Abs("../testdata/fake-claude-slow.sh")
	if err != nil {
		t.Fatal(err)
	}

	addr := "127.0.0.1:18546"
	peerCID, err := objproto.ParseConnectionID("ws:"+addr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse server cid: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	s := server.New(server.Config{Addr: addr, DataDir: t.TempDir()})
	serverDone := make(chan error, 1)
	go func() { serverDone <- s.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)

	runnerDone := make(chan error, 1)
	go func() {
		runnerDone <- runner.Run(ctx, runner.Config{
			ServerCID:    peerCID,
			AllowedRoots: []string{repo},
			ClaudeBin:    fakeClaude,
		})
	}()
	time.Sleep(500 * time.Millisecond)

	taskID, err := cli.Submit(ctx, peerCID, repo, "long-running")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	worktree := filepath.Join(repo, ".harness-worktrees", taskID)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(worktree); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("worktree did not appear: %v", err)
	}

	c, err := cli.Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	// 1. Build a local source directory tree.
	localSrc := filepath.Join(t.TempDir(), "src-tree")
	if err := os.MkdirAll(filepath.Join(localSrc, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localSrc, "a.txt"), []byte("AA"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localSrc, "sub", "b.txt"), []byte("BBB"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 2. Push the directory.
	if err := c.FilePushDir(ctx, taskID, localSrc, "incoming", false); err != nil {
		t.Fatalf("dir push: %v", err)
	}

	// 3. Verify it landed.
	if got, err := os.ReadFile(filepath.Join(worktree, "incoming", "a.txt")); err != nil || string(got) != "AA" {
		t.Errorf("incoming/a.txt = %q err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(worktree, "incoming", "sub", "b.txt")); err != nil || string(got) != "BBB" {
		t.Errorf("incoming/sub/b.txt = %q err=%v", got, err)
	}

	// 4. Push again without --force: must fail.
	if err := c.FilePushDir(ctx, taskID, localSrc, "incoming", false); err == nil {
		t.Errorf("second push without --force should fail")
	}

	// 5. Push with --force: must replace.
	localSrc2 := filepath.Join(t.TempDir(), "src-tree-2")
	if err := os.MkdirAll(localSrc2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localSrc2, "fresh.txt"), []byte("NEW"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.FilePushDir(ctx, taskID, localSrc2, "incoming", true); err != nil {
		t.Fatalf("force push: %v", err)
	}
	if _, err := os.Stat(filepath.Join(worktree, "incoming", "a.txt")); !os.IsNotExist(err) {
		t.Errorf("old a.txt should be gone after force replace; err=%v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(worktree, "incoming", "fresh.txt")); string(got) != "NEW" {
		t.Errorf("fresh.txt = %q", got)
	}

	// 6. Pull the directory back.
	localDst := filepath.Join(t.TempDir(), "pulled-tree")
	if err := c.FilePullDir(ctx, taskID, "incoming", localDst, false); err != nil {
		t.Fatalf("dir pull: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(localDst, "fresh.txt")); string(got) != "NEW" {
		t.Errorf("pulled fresh.txt = %q", got)
	}

	// 7. Pull again without --force: must fail (dest exists).
	if err := c.FilePullDir(ctx, taskID, "incoming", localDst, false); err == nil {
		t.Errorf("second pull without --force should fail")
	}

	cancel()
	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
	}
	select {
	case <-runnerDone:
	case <-time.After(2 * time.Second):
	}
}
