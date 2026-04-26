package tui

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
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

// RefreshSnapshot calls List over the persistent client. We do NOT delegate
// to (*cli.Client).List here because that method writes a human-readable
// summary to an io.Writer, whereas the TUI needs the structured Runners /
// Tasks slices. Keep the bespoke RoundTripTaskControl call here.
func RefreshSnapshot(c *cli.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_List}
		req.SetList(protocol.ListQuery{})
		resp, err := c.RoundTripTaskControl(ctx, req)
		if err != nil {
			return SnapshotMsg{Err: err}
		}
		lr := resp.List()
		if lr == nil {
			return SnapshotMsg{Err: fmt.Errorf("empty list response")}
		}
		runners := make([]protocol.RunnerInfo, len(lr.Runners))
		copy(runners, lr.Runners)
		tasks := make([]protocol.TaskInfo, len(lr.Tasks))
		copy(tasks, lr.Tasks)
		return SnapshotMsg{Runners: runners, Tasks: tasks}
	}
}
