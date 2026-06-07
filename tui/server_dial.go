package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/objtrsf/objproto"
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

// DoServerDialRunner asks the server (via the TUI's existing *cli.Client) to
// dial out to runnerCIDStr (a Listen-mode runner). viaCIDStr, when non-empty,
// requests the server to relay through the named runner (Phase C).
//
// Reuses the long-lived client established by App.BindClient instead of opening
// a fresh peer.Conn per call — every other TUI action (submit/cancel/interactive
// /file/session) follows this pattern.
func DoServerDialRunner(c *cli.Client, runnerCIDStr, viaCIDStr string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		targetCID, err := objproto.ParseConnectionID(runnerCIDStr,
			objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
		if err != nil {
			return ServerDialResultMsg{RunnerCID: runnerCIDStr, Err: err}
		}
		var viaCID objproto.ConnectionID
		if v := strings.TrimSpace(viaCIDStr); v != "" {
			viaCID, err = objproto.ParseConnectionID(v,
				objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
			if err != nil {
				return ServerDialResultMsg{RunnerCID: runnerCIDStr, Err: fmt.Errorf("--via: %w", err)}
			}
		}
		resp, err := cli.ServerDialRunnerWith(ctx, c,
			protocol.ConnIDToRunnerID(targetCID),
			protocol.ConnIDToRunnerID(viaCID))
		if err != nil {
			return ServerDialResultMsg{RunnerCID: runnerCIDStr, Err: err}
		}
		return ServerDialResultMsg{RunnerCID: runnerCIDStr, Status: resp.Status}
	}
}
