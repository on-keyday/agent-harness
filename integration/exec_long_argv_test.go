//go:build integration

package integration

import (
	"bytes"
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// An exec's command rides a stream on both hops, so a command line far past
// one datagram works from a client on either leg, against a runner on UDP.
// Zed's remote terminal is a 2802-byte line and hung when it rode inline.
//
// Loopback does not drop oversize datagrams (MTU 65536), so this proves the
// stream path carries the command — not that the old inline path would have
// been dropped on a real link.
func TestExecRunLongCommandOverBothTransports(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("the command is POSIX shell — skipping on Windows")
	}
	clearAgentEnv(t)

	wsCID, udpCID, _ := startDualStackServer(t)
	repo := tempRepo(t)
	startRunner(t, udpCID, runnerOpts{MaxTasks: 2, Roots: []string{repo}, ClaudeBin: fakeClaudeSlowPath(t)})

	wsClient := dialClient(t, wsCID)
	taskID := openLiveSession(t, wsClient, repo)
	line := "echo " + strings.Repeat("a", 8192) + " END"

	for _, leg := range []struct {
		name string
		c    *cli.Client
	}{{"ws", wsClient}, {"udp", dialClient(t, udpCID)}} {
		t.Run(leg.name, func(t *testing.T) {
			var out bytes.Buffer
			res, err := leg.c.ExecRun(context.Background(), taskID, []string{line},
				cli.ExecRunOpts{ShellLine: true, Stdout: &out})
			if err != nil {
				t.Fatalf("ExecRun: %v", err)
			}
			if res.Kind != protocol.ExecEventKind_Exited || res.ExitCode != 0 {
				t.Fatalf("result = %+v, want exited/0", res)
			}
			if !strings.HasSuffix(strings.TrimSpace(out.String()), "END") || out.Len() < 8192 {
				t.Errorf("stdout is %d bytes ending %q, want the 8 KiB line back", out.Len(), tail(out.Bytes(), 20))
			}
		})
	}
}
