package tui

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
)

// --- tea.Cmd factories using the persistent cli.Client ---
//
// These wrap (*cli.Client) methods with the tea.Msg shapes the TUI's update
// loop expects (echo strings, prefix/resolved IDs for cancel, structured
// snapshots for List). The methods themselves are defined in package cli;
// we only own the result-message wrapping here.

type SubmitResultMsg struct {
	TaskID string
	Err    error
	Echo   string // human-readable echo of the request, e.g. "submit --repo /r \"prompt\""
}

type CancelResultMsg struct {
	IDPrefix string
	Resolved string
	Err      error
}

type PruneResultMsg struct {
	Removed uint32
	Err     error
}

// LogHistoryMsg carries the historical content of a task log fetched from the
// server's on-disk store. app.go appends it into the LogsModel when the task
// id matches the currently-followed one (a switch can happen between fetch
// and arrival).
type LogHistoryMsg struct {
	TaskID  string
	Content []byte
	Found   bool
	Err     error
}

// DoSubmit issues a Submit RPC over the existing persistent client.
func DoSubmit(c *cli.Client, repo, prompt string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		echo := fmt.Sprintf("submit --repo %q %q", repo, prompt)
		id, err := c.Submit(ctx, repo, prompt)
		if err != nil {
			return SubmitResultMsg{Err: err, Echo: echo}
		}
		return SubmitResultMsg{TaskID: id, Echo: echo}
	}
}

// DoCancel issues a Cancel RPC over the existing persistent client.
// resolved is the full hex id (callers resolve prefixes against tasksByID).
func DoCancel(c *cli.Client, idPrefix, resolved string) tea.Cmd {
	return func() tea.Msg {
		if resolved == "" {
			return CancelResultMsg{IDPrefix: idPrefix, Err: fmt.Errorf("no task matching prefix %q", idPrefix)}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := c.Cancel(ctx, resolved)
		return CancelResultMsg{IDPrefix: idPrefix, Resolved: resolved, Err: err}
	}
}

// DoGetTaskLog fetches the historical log via the persistent client. The
// stream-pointer response is read off the same trsf transport the client
// already runs.
func DoGetTaskLog(c *cli.Client, taskID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		content, found, err := c.GetTaskLog(ctx, taskID)
		return LogHistoryMsg{TaskID: taskID, Content: content, Found: found, Err: err}
	}
}

// DoPruneTasks asks the server to forget terminal tasks older than `before`.
func DoPruneTasks(c *cli.Client, before time.Duration) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cutoff := time.Now().Add(-before)
		removed, err := c.PruneTasks(ctx, cutoff)
		if err != nil {
			return PruneResultMsg{Err: err}
		}
		return PruneResultMsg{Removed: removed}
	}
}

// RefreshSnapshot wraps (*cli.Client).Snapshot with the SnapshotMsg envelope
// the TUI's update loop expects. The RoundTripTaskControl + decode lives in
// the cli package so the wasm bridge and other consumers can share it.
func RefreshSnapshot(c *cli.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		lr, err := c.Snapshot(ctx)
		if err != nil {
			return SnapshotMsg{Err: err}
		}
		return SnapshotMsg{Runners: lr.Runners, Tasks: lr.Tasks}
	}
}
