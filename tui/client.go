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
	"github.com/on-keyday/agent-harness/transport"
	"github.com/on-keyday/agent-harness/trsf"
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
// pubsub subscriptions (via trsf streams) and ad-hoc TaskControl round-trips.
// Caller is responsible for cancelling ctx; the goroutines exit on cancel.
func Connect(ctx context.Context, addr string) (objproto.Connection, trsf.Transport, error) {
	sess, err := transport.WebSocketSession(slog.Default(), addr, nil, objproto.SessionModeClient)
	if err != nil {
		return nil, nil, fmt.Errorf("ws session: %w", err)
	}
	cid := objproto.MustParseConnectionID(fmt.Sprintf("ws:127.0.0.1:%s-3333", portFrom(addr)))
	conn, err := objproto.DoECDHHandshake(ctx, sess, cid, ecdh.P521(), objproto.AES128GCM)
	if err != nil {
		return nil, nil, fmt.Errorf("ecdh: %w", err)
	}
	p := trsf.NewStreams(ctx, false, trsf.DefaultInitialMTU, trsf.DefaultMaxMTU, conn, slog.Default())
	go trsf.AutoSend(ctx, p, conn, nil)
	go trsf.AutoReceive(ctx, p, conn, func(*objproto.Message, error) {})
	go trsf.AutoPing(ctx, conn, 30*time.Second)
	return conn, p, nil
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
