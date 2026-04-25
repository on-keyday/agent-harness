package tui

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf"
)

// --- tea.Cmd factories using the persistent cli.Client ---

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

		req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_Submit}
		sub := protocol.SubmitRequest{}
		sub.SetRepoPath([]byte(repo))
		sub.SetPrompt([]byte(prompt))
		req.SetSubmit(sub)
		resp, err := c.RoundTripTaskControl(ctx, req)
		if err != nil {
			return SubmitResultMsg{Err: err, Echo: echo}
		}
		s := resp.Submit()
		if s == nil || resp.Kind != protocol.TaskControlKind_Submit {
			return SubmitResultMsg{Err: fmt.Errorf("unexpected submit response"), Echo: echo}
		}
		return SubmitResultMsg{TaskID: hex.EncodeToString(s.TaskId.Id[:]), Echo: echo}
	}
}

// DoCancel issues a Cancel RPC over the existing persistent client.
// resolved is the full hex id (callers resolve prefixes against tasksByID).
func DoCancel(c *cli.Client, idPrefix, resolved string) tea.Cmd {
	return func() tea.Msg {
		if resolved == "" {
			return CancelResultMsg{IDPrefix: idPrefix, Err: fmt.Errorf("no task matching prefix %q", idPrefix)}
		}
		raw, err := hex.DecodeString(resolved)
		if err != nil {
			return CancelResultMsg{IDPrefix: idPrefix, Err: fmt.Errorf("invalid id %q: %w", resolved, err)}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		var tid protocol.TaskID
		copy(tid.Id[:], raw)
		req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_Cancel}
		req.SetCancel(protocol.CancelTask{TaskId: tid})
		_, err = c.RoundTripTaskControl(ctx, req)
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

		raw, err := hex.DecodeString(taskID)
		if err != nil {
			return LogHistoryMsg{TaskID: taskID, Err: err}
		}
		var tid protocol.TaskID
		copy(tid.Id[:], raw)
		req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_GetTaskLog}
		req.SetGetLog(protocol.GetTaskLogRequest{TaskId: tid})
		resp, err := c.RoundTripTaskControl(ctx, req)
		if err != nil {
			return LogHistoryMsg{TaskID: taskID, Err: err}
		}
		gl := resp.GetLog()
		if gl == nil {
			return LogHistoryMsg{TaskID: taskID, Err: fmt.Errorf("expected GetTaskLog response, got kind=%v", resp.Kind)}
		}
		if gl.Found == 0 {
			return LogHistoryMsg{TaskID: taskID, Found: false}
		}
		st := waitForReceiveStream(ctx, c.Transport(), trsf.StreamID(gl.StreamId))
		if st == nil {
			return LogHistoryMsg{TaskID: taskID, Found: true, Err: fmt.Errorf("stream %d not visible", gl.StreamId)}
		}
		var out []byte
		for {
			data, eof, err := st.ReadDirect(64 * 1024)
			if err != nil {
				return LogHistoryMsg{TaskID: taskID, Found: true, Content: out, Err: err}
			}
			if len(data) > 0 {
				out = append(out, data...)
			}
			if eof {
				return LogHistoryMsg{TaskID: taskID, Found: true, Content: out}
			}
			if ctx.Err() != nil {
				return LogHistoryMsg{TaskID: taskID, Found: true, Content: out, Err: ctx.Err()}
			}
		}
	}
}

// DoPruneTasks asks the server to forget terminal tasks older than `before`.
func DoPruneTasks(c *cli.Client, before time.Duration) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cutoff := time.Now().Add(-before)
		req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_PruneTasks}
		req.SetPrune(protocol.PruneTasksRequest{BeforeTs: uint64(cutoff.UnixNano())})
		resp, err := c.RoundTripTaskControl(ctx, req)
		if err != nil {
			return PruneResultMsg{Err: err}
		}
		pr := resp.Prune()
		if pr == nil {
			return PruneResultMsg{Err: fmt.Errorf("unexpected prune response kind: %v", resp.Kind)}
		}
		return PruneResultMsg{Removed: pr.Removed}
	}
}

// RefreshSnapshot calls List over the persistent client.
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

// waitForReceiveStream polls until p.GetReceiveStream(id) returns non-nil
// or ctx / 2s elapses. Server-initiated send-stream creation can race the
// response message (the response goes via objproto.SendMessage; the
// stream-create frame travels via trsf.AutoSend).
func waitForReceiveStream(ctx context.Context, p trsf.Transport, id trsf.StreamID) trsf.ReceiveStream {
	if st := p.GetReceiveStream(id); st != nil {
		return st
	}
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-deadline.C:
			return nil
		case <-tick.C:
			if st := p.GetReceiveStream(id); st != nil {
				return st
			}
		}
	}
}
