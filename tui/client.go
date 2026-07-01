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
func DoSubmit(c *cli.Client, repo, prompt string, caps protocol.Capability) tea.Cmd {
	return DoSubmitWithOpts(c, repo, prompt, "", nil, "", caps, false, false)
}

// DoSubmitWithOpts issues a Submit RPC with an optional hostname pin,
// optional per-task extra claude args, and optional resume target id. When
// host is non-empty a ByHostname selector is built; otherwise Any is used.
// extraArgs are forwarded verbatim to the runner and appended to its
// --claude-args baseline at exec time. resumeTaskID, when non-empty, asks
// the server to reuse that terminal task's id and worktree branch.
// caps sets RequestedCaps on the wire request; pass protocol.Capability_All
// for the inherit-all behaviour.
// resumeCapsOverride, when true, signals the server to re-grant caps from
// the resumer's caps rather than keeping the persisted caps of the resumed task.
// Ignored (no-op) when resumeTaskID is empty (fresh submit).
func DoSubmitWithOpts(c *cli.Client, repo, prompt, host string, extraArgs []string, resumeTaskID string, caps protocol.Capability, resumeCapsOverride bool, resumeConversation bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		echo := buildSubmitEcho(repo, prompt, host, extraArgs, resumeTaskID)
		sel, err := cli.BuildSelector(cli.SelectorOpts{Host: host})
		if err != nil {
			return SubmitResultMsg{Err: fmt.Errorf("selector: %w", err), Echo: echo}
		}
		id, err := c.SubmitWithSelectorArgsAndCaps(ctx, repo, prompt, sel, extraArgs, resumeTaskID, caps, resumeCapsOverride, resumeConversation)
		if err != nil {
			return SubmitResultMsg{Err: err, Echo: echo}
		}
		return SubmitResultMsg{TaskID: id, Echo: echo}
	}
}

// buildSubmitEcho formats the human-readable echo string for the cmdline
// result panel. Annotations are added only when the corresponding option is
// set so the common case (just repo + prompt) stays readable.
func buildSubmitEcho(repo, prompt, host string, extraArgs []string, resumeTaskID string) string {
	annot := ""
	if host != "" {
		annot += fmt.Sprintf(" --host %q", host)
	}
	if len(extraArgs) > 0 {
		annot += fmt.Sprintf(" (+%d claude-args)", len(extraArgs))
	}
	if resumeTaskID != "" {
		annot += fmt.Sprintf(" --resume %s", shortTaskID(resumeTaskID))
	}
	return fmt.Sprintf("submit --repo %q%s %q", repo, annot, prompt)
}

// shortTaskID truncates a 32-char hex task id to its first 12 for display.
// Falls back to the original string when the input is shorter than expected.
func shortTaskID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
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
		res, err := c.PruneTasks(ctx, cutoff, nil, false)
		if err != nil {
			return PruneResultMsg{Err: err}
		}
		return PruneResultMsg{Removed: res.Removed}
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

// SessionListMsg carries the result of a session ls command: a slice of
// interactive+detachable tasks, or an error if the snapshot failed.
type SessionListMsg struct {
	Tasks []protocol.TaskInfo
	Err   error
}

// DoSessionList fetches a snapshot and returns only interactive+detachable
// tasks in a SessionListMsg.
func DoSessionList(c *cli.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		lr, err := c.Snapshot(ctx)
		if err != nil {
			return SessionListMsg{Err: err}
		}
		var sessions []protocol.TaskInfo
		for _, t := range lr.Tasks {
			if t.Kind == protocol.TaskKind_Interactive && t.Detachable() {
				sessions = append(sessions, t)
			}
		}
		return SessionListMsg{Tasks: sessions}
	}
}
