package tui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// ServerDialResultMsg is delivered to App.Update after a `server dial-runner`
// command completes. Status carries the wire-level DialRunnerStatus; Err is
// non-nil for transport / parse failures.
type ServerDialResultMsg struct {
	RunnerCID string
	Status    protocol.DialRunnerStatus
	Err       error
}

// DoServerDialRunner asks the server to dial out to runnerCIDStr (a Listen-mode
// runner) via the existing TUI-server connection.
func DoServerDialRunner(serverCID objproto.ConnectionID, runnerCIDStr string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		targetCID, err := objproto.ParseConnectionID(runnerCIDStr,
			objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
		if err != nil {
			return ServerDialResultMsg{RunnerCID: runnerCIDStr, Err: err}
		}
		resp, err := cli.ServerDialRunner(ctx, serverCID, targetCID)
		if err != nil {
			return ServerDialResultMsg{RunnerCID: runnerCIDStr, Err: err}
		}
		return ServerDialResultMsg{RunnerCID: runnerCIDStr, Status: resp.Status}
	}
}
