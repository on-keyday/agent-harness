//go:build integration

package integration

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/topics"
	"github.com/on-keyday/agent-harness/tui"
)

// An exec whose argv spans several segments reaches an execs.status
// subscriber as whole events — the TUI logged
// `not enough data to read for field "ExecArg::Arg"` and dropped them.
//
// The subscriber is on UDP: over loopback ws the whole event arrived in one
// read and the unframed reader passed; over UDP it failed with that error on
// every run.
func TestPubsubFraming_LongArgvExecEventReachesTheTUIWhole(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("the command is POSIX shell — skipping on Windows")
	}
	clearAgentEnv(t)

	wsCID, udpCID, _ := startDualStackServer(t)
	repo := tempRepo(t)
	startRunner(t, wsCID, runnerOpts{MaxTasks: 2, Roots: []string{repo}, ClaudeBin: fakeClaudeSlowPath(t)})
	c := dialClient(t, udpCID)
	taskID := openLiveSession(t, c, repo)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := c.Peer().JoinAndGetStream(ctx, "test", topics.ExecsStatus())
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu      sync.Mutex
		started []protocol.ExecStatusEvent
	)
	go tui.PumpEvents(ctx, st, topics.ExecsStatus(), func(p []byte) tea.Msg {
		ev, err := tui.DecodeExecStatus(p)
		if err != nil {
			t.Errorf("decode: %v", err)
			return nil
		}
		return tui.ExecStatusMsg{Event: ev}
	}, func(m tea.Msg) {
		ev := m.(tui.ExecStatusMsg).Event
		if ev.Kind == protocol.StatusEventKind_ExecStarted {
			mu.Lock()
			started = append(started, ev)
			mu.Unlock()
		}
	})

	line := "echo " + strings.Repeat("a", 8192) + " END"
	if _, err := c.ExecRun(ctx, taskID, []string{line}, cli.ExecRunOpts{ShellLine: true}); err != nil {
		t.Fatalf("ExecRun: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(started)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(started) != 1 {
		t.Fatalf("got %d exec_started events, want 1", len(started))
	}
	if a := started[0].Info.Argv.Argv; len(a) == 0 || !strings.HasSuffix(string(a[len(a)-1].Arg), " END") {
		t.Fatal("exec_started argv does not carry the whole line")
	}
}
