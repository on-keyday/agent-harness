package tui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// GitResultMsg carries one git query's answer back to App.Update. Kind tells
// the modal whether this refreshes the commit list, the status summary, or the
// content pane.
type GitResultMsg struct {
	TaskID string
	Kind   protocol.GitQueryKind
	Result *cli.GitResult
	Err    error
}

// gitCmdTimeout is generous because a diff against a distant baseline in a
// large repository is the slow case, and the runner caps its own git at 30s
// anyway — this only has to outlast that plus the round trip.
const gitCmdTimeout = 60 * time.Second

// Each Do* takes the caller's long-lived *cli.Client, the same threading
// DoFileLs uses. There is deliberately no cli.Dial here: the TUI already holds
// a client and a fresh dial would throw away its handshake.

func DoGitLog(c *cli.Client, taskID, baseRev string, max uint32) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), gitCmdTimeout)
		defer cancel()
		res, err := c.GitLog(ctx, taskID, baseRev, "", max)
		return GitResultMsg{TaskID: taskID, Kind: protocol.GitQueryKind_Log, Result: res, Err: err}
	}
}

func DoGitDiff(c *cli.Client, taskID, baseRev string, target protocol.GitDiffTarget, targetRev, path string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), gitCmdTimeout)
		defer cancel()
		res, err := c.GitDiff(ctx, taskID, baseRev, target, targetRev, path, 0)
		return GitResultMsg{TaskID: taskID, Kind: protocol.GitQueryKind_Diff, Result: res, Err: err}
	}
}

func DoGitShow(c *cli.Client, taskID, rev, path string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), gitCmdTimeout)
		defer cancel()
		res, err := c.GitShow(ctx, taskID, rev, path, 0)
		return GitResultMsg{TaskID: taskID, Kind: protocol.GitQueryKind_Show, Result: res, Err: err}
	}
}

func DoGitStatus(c *cli.Client, taskID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), gitCmdTimeout)
		defer cancel()
		res, err := c.GitStatus(ctx, taskID, "")
		return GitResultMsg{TaskID: taskID, Kind: protocol.GitQueryKind_Status, Result: res, Err: err}
	}
}
