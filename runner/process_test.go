package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClaudePath returns the absolute path to testdata/fake-claude.sh from the runner package directory.
func fakeClaudePath(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("../testdata/fake-claude.sh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("fake-claude.sh missing: %v", err)
	}
	return abs
}

func TestRunClaudeWithExtraArgs(t *testing.T) {
	// Verify ExtraArgs are inserted before "-p <prompt>". fake-claude.sh prints all its
	// args via "$*", so we can search the captured stdout for the extra flag.
	repo := initRepo(t)
	wm := &WorktreeManager{Repo: repo}
	dir, _ := wm.Create("extra-args")

	var mu sync.Mutex
	var chunks [][]byte
	sink := func(data []byte) {
		mu.Lock()
		chunks = append(chunks, append([]byte{}, data...))
		mu.Unlock()
	}
	p := &Process{
		ClaudeBin: fakeClaudePath(t),
		CWD:       dir,
		Timeout:   5 * time.Second,
		ExtraArgs: []string{"--dangerously-skip-permissions"},
	}
	exit, err := p.Run(context.Background(), "hi", sink)
	if err != nil || exit != 0 {
		t.Fatalf("run: exit=%d err=%v", exit, err)
	}
	mu.Lock()
	defer mu.Unlock()
	var combined strings.Builder
	for _, c := range chunks {
		combined.Write(c)
	}
	got := combined.String()
	if !strings.Contains(got, "--dangerously-skip-permissions") {
		t.Errorf("extra arg not forwarded; got: %q", got)
	}
	// Confirm order: --dangerously-skip-permissions appears before -p in fake-claude's "$*" echo.
	idxExtra := strings.Index(got, "--dangerously-skip-permissions")
	idxP := strings.Index(got, "-p")
	if idxExtra < 0 || idxP < 0 || idxExtra > idxP {
		t.Errorf("extra arg should precede -p; got: %q", got)
	}
}

func TestRunClaudeStreamsLogs(t *testing.T) {
	repo := initRepo(t)
	wm := &WorktreeManager{Repo: repo}
	dir, _ := wm.Create("t1")

	var mu sync.Mutex
	var chunks [][]byte
	sink := func(data []byte) {
		mu.Lock()
		chunks = append(chunks, append([]byte{}, data...))
		mu.Unlock()
	}

	p := &Process{
		ClaudeBin: fakeClaudePath(t),
		CWD:       dir,
		Timeout:   5 * time.Second,
	}
	exit, err := p.Run(context.Background(), "hello", sink)
	if err != nil {
		t.Fatal(err)
	}
	if exit != 0 {
		t.Fatalf("exit=%d", exit)
	}

	mu.Lock()
	defer mu.Unlock()
	var combined strings.Builder
	for _, c := range chunks {
		combined.Write(c)
	}
	text := combined.String()
	if !strings.Contains(text, "[out]") {
		t.Errorf("missing [out] prefix in: %q", text)
	}
	if !strings.Contains(text, "[err]") {
		t.Errorf("missing [err] prefix in: %q", text)
	}
	if !strings.Contains(text, "stdout: prompt=-p hello") && !strings.Contains(text, "stdout: prompt=hello") {
		t.Errorf("missing prompt echo in: %q", text)
	}
	if !strings.Contains(text, "stderr line") {
		t.Errorf("missing stderr line in: %q", text)
	}
}

func TestRunClaudeNonZeroExit(t *testing.T) {
	repo := initRepo(t)
	wm := &WorktreeManager{Repo: repo}
	dir, _ := wm.Create("t2")
	abs, err := filepath.Abs("../testdata/fake-claude-fail.sh")
	if err != nil {
		t.Fatal(err)
	}
	p := &Process{ClaudeBin: abs, CWD: dir, Timeout: 5 * time.Second}
	exit, err := p.Run(context.Background(), "x", func([]byte) {})
	if err != nil {
		t.Fatal(err)
	}
	if exit != 3 {
		t.Fatalf("expected exit=3, got %d", exit)
	}
}

func TestRunClaudeTimeout(t *testing.T) {
	repo := initRepo(t)
	wm := &WorktreeManager{Repo: repo}
	dir, _ := wm.Create("t3")

	// Write a slow wrapper script that sleeps for 60s.
	sleepWrapper := filepath.Join(dir, "slow.sh")
	os.WriteFile(sleepWrapper, []byte("#!/bin/bash\nsleep 60\n"), 0o755)

	p := &Process{
		ClaudeBin: sleepWrapper,
		CWD:       dir,
		Timeout:   500 * time.Millisecond,
	}
	start := time.Now()
	exit, err := p.Run(context.Background(), "x", func([]byte) {})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if exit != -1 {
		t.Errorf("expected exit=-1 (killed), got %d", exit)
	}
	if elapsed > 10*time.Second {
		t.Errorf("timeout took too long: %v", elapsed)
	}
}
