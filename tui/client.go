package tui

import (
	"context"
	"crypto/ecdh"
	"fmt"
	"log/slog"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/pubsub"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/transport"
	"github.com/on-keyday/agent-harness/trsf"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// portFrom returns the port part of "host:port".
func portFrom(addr string) string {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return addr
	}
	return addr[i+1:]
}

// Connect dials the harness-server and returns a connection ready for both
// pubsub subscriptions (via trsf streams) and ad-hoc TaskControl round-trips,
// along with a pubsub.Client that correlates broker responses by request_id.
// The AutoReceive goroutine routes Pubsub-kind messages into the Client's
// HandleResponse so subscribeAndStream can pick up the StreamId for its JOIN.
// Caller is responsible for cancelling ctx; the goroutines exit on cancel.
func Connect(ctx context.Context, addr string) (objproto.Connection, trsf.Transport, *pubsub.Client, error) {
	sess, err := transport.WebSocketSession(slog.Default(), addr, nil, objproto.SessionModeClient)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("ws session: %w", err)
	}
	cid := objproto.MustParseConnectionID(fmt.Sprintf("ws:127.0.0.1:%s-3333", portFrom(addr)))
	conn, err := objproto.DoECDHHandshake(ctx, sess, cid, ecdh.P521(), objproto.AES128GCM)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("ecdh: %w", err)
	}
	p := trsf.NewStreams(ctx, false, trsf.DefaultInitialMTU, trsf.DefaultMaxMTU, conn, slog.Default())
	pubClient := pubsub.NewClient()
	go trsf.AutoSend(ctx, p, conn, nil)
	go trsf.AutoReceive(ctx, p, conn, func(msg *objproto.Message, err error) {
		if err != nil || len(msg.Data) == 0 {
			return
		}
		if wire.ApplicationPayloadKind(msg.Data[0]) == wire.ApplicationPayloadKind_Pubsub {
			pubClient.HandleResponse(msg.Data[1:])
		}
		// Other kinds are not consumed by the TUI's read path (TaskControl
		// responses go through cli.Client's synchronous ReceiveMessage call).
	})
	go trsf.AutoPing(ctx, conn, 30*time.Second)
	return conn, p, pubClient, nil
}

// --- tea.Cmd factories backed by short-lived cli.* helpers ---

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

// DoSubmit returns a tea.Cmd that calls cli.Submit and returns SubmitResultMsg.
func DoSubmit(addr, repo, prompt string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		echo := fmt.Sprintf("submit --repo %q %q", repo, prompt)
		id, err := cli.Submit(ctx, addr, repo, prompt)
		return SubmitResultMsg{TaskID: id, Err: err, Echo: echo}
	}
}

// DoCancel: the caller resolves the id-prefix to a full id BEFORE invoking
// (lookup happens in app.go using the local tasks snapshot). If resolved == ""
// the action returns an error message without contacting the server.
func DoCancel(addr, idPrefix, resolved string) tea.Cmd {
	return func() tea.Msg {
		if resolved == "" {
			return CancelResultMsg{IDPrefix: idPrefix, Err: fmt.Errorf("no task matching prefix %q", idPrefix)}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := cli.Cancel(ctx, addr, resolved)
		return CancelResultMsg{IDPrefix: idPrefix, Resolved: resolved, Err: err}
	}
}

// DoGetTaskLog fetches the historical log for taskID from the server and
// dispatches a LogHistoryMsg. The fetch always uses its own short-lived
// connection so the TUI's persistent pubsub conn is unaffected.
func DoGetTaskLog(addr, taskID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		content, found, err := cli.GetTaskLog(ctx, addr, taskID)
		return LogHistoryMsg{TaskID: taskID, Content: content, Found: found, Err: err}
	}
}

// DoPruneTasks asks the server to forget terminal tasks older than `before`.
func DoPruneTasks(addr string, before time.Duration) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cutoff := time.Now().Add(-before)
		removed, err := cli.PruneTasks(ctx, addr, cutoff)
		return PruneResultMsg{Removed: removed, Err: err}
	}
}

// RefreshSnapshot calls List on the server (via a short-lived cli connection)
// and dispatches a SnapshotMsg.
func RefreshSnapshot(addr string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c, err := cli.Dial(ctx, addr)
		if err != nil {
			return SnapshotMsg{Err: err}
		}
		defer c.Close()

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
