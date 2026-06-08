package tui

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/topics"
)

// Messages dispatched into the tea.Program from the pubsub bridge.
type SnapshotMsg struct {
	Runners []protocol.RunnerInfo
	Tasks   []protocol.TaskInfo
	Err     error
}

type TaskEventMsg struct {
	Event protocol.TaskStatusEvent
}

type RunnerEventMsg struct {
	Event protocol.RunnerStatusEvent
}

type LogChunkMsg struct {
	TaskID string // hex
	Chunk  []byte
}

// ConnectionMsg notifies the App of a connection state change driven by
// cli.PersistLoop. Connected and Reconnecting are mutually exclusive.
//   - Connected=true             → freshly bound to a live client
//   - Reconnecting=true          → between attempts, NextRetry counts down
//   - Connected=false, Reconnecting=false → terminal disconnect (Err set)
type ConnectionMsg struct {
	Connected    bool
	Reconnecting bool
	Attempt      int
	NextRetry    time.Duration
	Err          error
}

// BindClientMsg carries a fresh *cli.Client into the App via the bubbletea
// Update loop, so client-pointer reads/writes stay on a single goroutine
// (see persist-reconnect spec §6).
type BindClientMsg struct {
	Client *cli.Client
}

// DecodeTaskStatus decodes a TaskStatusEvent payload.
func DecodeTaskStatus(payload []byte) (protocol.TaskStatusEvent, error) {
	var ev protocol.TaskStatusEvent
	if _, err := ev.Decode(payload); err != nil {
		return protocol.TaskStatusEvent{}, fmt.Errorf("decode TaskStatusEvent: %w", err)
	}
	return ev, nil
}

// DecodeRunnerStatus decodes a RunnerStatusEvent payload.
func DecodeRunnerStatus(payload []byte) (protocol.RunnerStatusEvent, error) {
	var ev protocol.RunnerStatusEvent
	if _, err := ev.Decode(payload); err != nil {
		return protocol.RunnerStatusEvent{}, fmt.Errorf("decode RunnerStatusEvent: %w", err)
	}
	return ev, nil
}

// SubscribeTaskStatus issues a JOIN for tasks.status, finds the broker-created
// stream via the response's StreamId, and forwards each decoded event as
// TaskEventMsg via program.Send. Returns once ctx is cancelled or the stream
// errors out.
func SubscribeTaskStatus(ctx context.Context, c *cli.Client, program *tea.Program) {
	subscribeAndStream(ctx, c, topics.TasksStatus(), program, func(payload []byte) tea.Msg {
		ev, err := DecodeTaskStatus(payload)
		if err != nil {
			slog.Warn("decode task event", "err", err)
			return nil
		}
		return TaskEventMsg{Event: ev}
	})
}

// SubscribeRunnerStatus mirror of SubscribeTaskStatus for runners.status.
func SubscribeRunnerStatus(ctx context.Context, c *cli.Client, program *tea.Program) {
	subscribeAndStream(ctx, c, topics.RunnersStatus(), program, func(payload []byte) tea.Msg {
		ev, err := DecodeRunnerStatus(payload)
		if err != nil {
			slog.Warn("decode runner event", "err", err)
			return nil
		}
		return RunnerEventMsg{Event: ev}
	})
}

// SubscribeTaskLog joins task.<taskID>.log and forwards each chunk as
// LogChunkMsg{TaskID: taskID}.
func SubscribeTaskLog(ctx context.Context, c *cli.Client, program *tea.Program, taskID string) {
	subscribeAndStream(ctx, c, topics.TaskLog(taskID), program, func(payload []byte) tea.Msg {
		chunk := make([]byte, len(payload))
		copy(chunk, payload)
		return LogChunkMsg{TaskID: taskID, Chunk: chunk}
	})
}

// NotifyEventMsg carries a decoded NotifyEvent dispatched from SubscribeNotifications.
type NotifyEventMsg struct {
	Event protocol.NotifyEvent
}

// SubscribeNotifications joins the notifications topic and forwards each decoded
// NotifyEvent as NotifyEventMsg via program.Send. Mirrors SubscribeTaskStatus.
func SubscribeNotifications(ctx context.Context, c *cli.Client, program *tea.Program) {
	subscribeAndStream(ctx, c, topics.Notifications(), program, func(payload []byte) tea.Msg {
		var ev protocol.NotifyEvent
		if _, err := ev.Decode(payload); err != nil {
			slog.Warn("decode notify event", "err", err)
			return nil
		}
		return NotifyEventMsg{Event: ev}
	})
}

// subscribeAndStream uses peer.Conn.JoinAndGetStream to do the JOIN+lookup+
// header-discard dance, then pumps payload chunks through fn → program.Send
// until ctx is cancelled or the stream EOFs.
func subscribeAndStream(ctx context.Context, c *cli.Client, topic string, program *tea.Program, fn func([]byte) tea.Msg) {
	st, err := c.Peer().JoinAndGetStream(ctx, "tui", topic)
	if err != nil {
		// context.Canceled is the normal path when the user switches tasks
		// (followTask cancels the prior subscription's ctx) — don't log.
		if ctx.Err() == nil {
			slog.Warn("subscribe", "topic", topic, "err", err)
		}
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		data, eof, err := st.ReadDirect(64 * 1024)
		if err != nil {
			return
		}
		if len(data) > 0 {
			if msg := fn(data); msg != nil {
				program.Send(msg)
			}
		}
		if eof {
			return
		}
	}
}

// FormatTaskID returns the hex string for a TaskID, exposed for app.go.
func FormatTaskID(t protocol.TaskID) string {
	return hex.EncodeToString(t.Id[:])
}
