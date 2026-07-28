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

// writeFakeClaude creates a temporary executable script with the given body and returns its path.
func writeFakeClaude(t *testing.T, body string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "fake-claude.sh")
	fullBody := "#!/usr/bin/env bash\nset -e\n" + body + "\nexit 0\n"
	if err := os.WriteFile(script, []byte(fullBody), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
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

func TestProcess_RunSetsEnv(t *testing.T) {
	fake := writeFakeClaude(t, `echo "TASK_ID=$HARNESS_TASK_ID"`)
	p := &Process{
		ClaudeBin: fake,
		CWD:       t.TempDir(),
		Env:       []string{"HARNESS_TASK_ID=deadbeef"},
	}
	var out []byte
	sink := func(data []byte) { out = append(out, data...) }
	code, err := p.Run(context.Background(), "ignored", sink)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(string(out), "TASK_ID=deadbeef") {
		t.Errorf("env not propagated; output = %q", out)
	}
}

func TestProcessDecodesStdoutAndLeavesStderrRaw(t *testing.T) {
	// A fake agent that prints two claude stream-json lines on stdout and one
	// plain line on stderr, then exits.
	script := filepath.Join(t.TempDir(), "fake-agent.sh")
	body := "#!/bin/sh\n" +
		"printf '%s\\n' '{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"hello\"}]}}'\n" +
		"printf '%s\\n' 'boom' >&2\n" +
		"printf '%s\\n' '{\"type\":\"result\",\"duration_ms\":12,\"total_cost_usd\":0}'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	p := &Process{
		ClaudeBin:           script,
		CWD:                 t.TempDir(),
		Timeout:             30 * time.Second,
		OneshotArgvTemplate: []string{"{args}", "{prompt}"},
		LogFormat:           "claude-stream-json",
	}
	var mu sync.Mutex
	var lines []string
	exit, err := p.Run(context.Background(), "ignored", func(b []byte) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, string(b))
	})
	if err != nil || exit != 0 {
		t.Fatalf("Run: exit=%d err=%v", exit, err)
	}

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(lines, "")
	if !strings.Contains(joined, "[out]hello\n") {
		t.Errorf("stdout was not decoded; got:\n%s", joined)
	}
	if !strings.Contains(joined, "[out]✓ 12ms\n") {
		t.Errorf("result event was not rendered; got:\n%s", joined)
	}
	if strings.Contains(joined, `"type":"assistant"`) {
		t.Errorf("raw JSON leaked into the log; got:\n%s", joined)
	}
	if !strings.Contains(joined, "[err]boom\n") {
		t.Errorf("stderr must stay verbatim; got:\n%s", joined)
	}
}

func TestProcessPassthroughWhenNoLogFormat(t *testing.T) {
	script := filepath.Join(t.TempDir(), "fake-agent.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'plain output\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &Process{
		ClaudeBin:           script,
		CWD:                 t.TempDir(),
		Timeout:             30 * time.Second,
		OneshotArgvTemplate: []string{"{args}", "{prompt}"},
	}
	var mu sync.Mutex
	var got string
	if _, err := p.Run(context.Background(), "ignored", func(b []byte) {
		mu.Lock()
		defer mu.Unlock()
		got += string(b)
	}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got != "[out]plain output\n" {
		t.Fatalf("got %q, want the line unchanged", got)
	}
}

// TestProcessStdoutVerbatimWhenNoDecoderApplies covers what
// TestProcessPassthroughWhenNoLogFormat cannot: a CRLF-terminated line and a
// final line with no trailing newline at all. Routing stdout through
// decode+render — even via agentlog's passthrough decoder — would corrupt
// both: passthrough.Decode trims "\r\n" (so a CRLF line loses its "\r"), and
// emit always synthesizes exactly one trailing "\n" on the rendered text (so
// a truly unterminated final line would gain a newline it never had). Both
// an empty LogFormat and an unrecognised one resolve to "no decoder", and
// per the task requirement they must forward stdout byte-for-byte identical
// to the pre-decoding behaviour — this test checks both resolve identically.
func TestProcessStdoutVerbatimWhenNoDecoderApplies(t *testing.T) {
	for _, format := range []string{"", "not-a-real-format"} {
		t.Run("format="+format, func(t *testing.T) {
			script := filepath.Join(t.TempDir(), "fake-agent.sh")
			body := "#!/bin/sh\n" +
				"printf 'crlf line\\r\\n'\n" +
				"printf '%s' 'trailing partial'\n"
			if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
			p := &Process{
				ClaudeBin:           script,
				CWD:                 t.TempDir(),
				Timeout:             30 * time.Second,
				OneshotArgvTemplate: []string{"{args}", "{prompt}"},
				LogFormat:           format,
			}
			var mu sync.Mutex
			var got string
			if _, err := p.Run(context.Background(), "ignored", func(b []byte) {
				mu.Lock()
				defer mu.Unlock()
				got += string(b)
			}); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			want := "[out]crlf line\r\n[out]trailing partial"
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}

// TestProcessDecodesFinalUnterminatedLine guards the ReadBytes/EOF ordering
// in scanDecoded: bufio.Reader.ReadBytes('\n') returns the trailing bytes
// together with a non-nil error when the stream ends without a newline, and
// the `if len(line) > 0 { ... }` decode step must run before the `if err !=
// nil { return }` check, or the last event of every real agent run (usually
// its "result"/finish line) would be silently dropped. This is the decoded
// counterpart of the unterminated-line case in
// TestProcessStdoutVerbatimWhenNoDecoderApplies.
func TestProcessDecodesFinalUnterminatedLine(t *testing.T) {
	script := filepath.Join(t.TempDir(), "fake-agent.sh")
	body := "#!/bin/sh\n" +
		"printf '%s' '{\"type\":\"result\",\"duration_ms\":12,\"total_cost_usd\":0}'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &Process{
		ClaudeBin:           script,
		CWD:                 t.TempDir(),
		Timeout:             30 * time.Second,
		OneshotArgvTemplate: []string{"{args}", "{prompt}"},
		LogFormat:           "claude-stream-json",
	}
	var mu sync.Mutex
	var got string
	exit, err := p.Run(context.Background(), "ignored", func(b []byte) {
		mu.Lock()
		defer mu.Unlock()
		got += string(b)
	})
	if err != nil || exit != 0 {
		t.Fatalf("Run: exit=%d err=%v", exit, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got != "[out]✓ 12ms\n" {
		t.Fatalf("got %q, want the finish event rendered", got)
	}
}

func TestProcessLeavesStdinClosed(t *testing.T) {
	// A oneshot agent must see EOF on stdin immediately. codex blocks waiting
	// for it; a never-EOF pipe hung every codex task until the timeout.
	script := filepath.Join(t.TempDir(), "reads-stdin.sh")
	body := "#!/bin/sh\n" +
		"cat > /dev/null\n" + // returns only at EOF
		"printf 'saw eof\\n'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &Process{
		ClaudeBin:           script,
		CWD:                 t.TempDir(),
		Timeout:             10 * time.Second,
		OneshotArgvTemplate: []string{"{args}", "{prompt}"},
	}
	var mu sync.Mutex
	var got string
	exit, err := p.Run(context.Background(), "ignored", func(b []byte) {
		mu.Lock()
		defer mu.Unlock()
		got += string(b)
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exit != 0 {
		t.Fatalf("exit = %d, want 0 (a non-zero exit here means the process was killed at the timeout)", exit)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(got, "saw eof") {
		t.Fatalf("agent never saw stdin EOF; got %q", got)
	}
}
