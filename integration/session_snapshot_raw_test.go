//go:build integration

package integration

import (
	"bytes"
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// fakeClaudeAnsiPath returns the absolute path to fake-claude-ansi.sh, which
// emits an SGR-colored line then sleeps.
func fakeClaudeAnsiPath(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("../testdata/fake-claude-ansi.sh")
	if err != nil {
		t.Fatalf("resolve fake-claude-ansi.sh: %v", err)
	}
	return abs
}

// TestSessionSnapshotRaw_PreservesEscapes verifies that SessionSnapshotRaw
// returns the verbatim PTY replay burst (escape sequences intact), in contrast
// to SessionSnapshot, whose headless VT render flattens SGR away.
func TestSessionSnapshotRaw_PreservesEscapes(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("fake-claude scripts require bash — skipping on Windows")
	}

	serverCID := startServer(t)
	repo := tempRepo(t)
	startRunner(t, serverCID, runnerOpts{
		MaxTasks:  1,
		Roots:     []string{repo},
		ClaudeBin: fakeClaudeAnsiPath(t),
	})

	c1 := dialClient(t, serverCID)
	sel := protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any}
	stream1, taskIDHex, err := c1.OpenInteractive(context.Background(), repo, cli.SessionOpts{Selector: sel})
	if err != nil {
		t.Fatalf("OpenInteractiveWithSelectorAndArgs: %v", err)
	}

	// Drain client 1's stdout so the controlling stream never backpressures the
	// PTY; the server's ring buffer is what the snapshots replay from.
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, e := stream1.Stdout().Read(buf); e != nil {
				return
			}
		}
	}()

	eventually(t, func() bool {
		ti := getTask(t, c1, taskIDHex)
		return ti.Status == protocol.TaskStatus_Running
	}, 10*time.Second, 100*time.Millisecond, "task to reach Running")

	settle := 1500 * time.Millisecond

	raw, synth, err := c1.SessionSnapshotRaw(context.Background(), taskIDHex, settle, false)
	if err != nil {
		t.Fatalf("SessionSnapshotRaw: %v", err)
	}
	// A real server replays a mode preamble and a screen repaint alongside the
	// PTY bytes, and --raw withholds both. Asserting the count is non-zero is
	// what keeps this an END-TO-END check that the separation happens on the
	// wire: against a server that still sent them as Stdout frames, they would
	// be counted as PTY output and this would read zero.
	if synth == 0 {
		t.Fatalf("no synthesised replay bytes were reported; the server is not framing " +
			"its own replay additions as Synth, so --raw cannot tell them from PTY output")
	}
	if bytes.Contains(raw, []byte("\x1b[?25")) {
		t.Fatalf("raw output carries a mode preamble the PTY never emitted; got %q", raw)
	}
	if !bytes.Contains(raw, []byte("\x1b[31m")) {
		t.Fatalf("raw snapshot missing ESC[31m sequence; got %q", raw)
	}
	if !bytes.Contains(raw, []byte("REDLINE")) {
		t.Fatalf("raw snapshot missing payload text; got %q", raw)
	}

	text, err := c1.SessionSnapshot(context.Background(), taskIDHex, 40, 120, settle)
	if err != nil {
		t.Fatalf("SessionSnapshot: %v", err)
	}
	if !strings.Contains(text, "REDLINE") {
		t.Fatalf("rendered snapshot missing payload text; got %q", text)
	}
	if strings.IndexByte(text, 0x1b) >= 0 {
		t.Fatalf("rendered snapshot leaked a raw ESC byte; got %q", text)
	}
}
