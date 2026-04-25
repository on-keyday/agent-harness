package tui

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/pubsub"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/topics"
	"github.com/on-keyday/agent-harness/trsf"
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

type ConnectionMsg struct {
	Connected bool
	Err       error
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

// SubscribeTaskStatus issues a JOIN for tasks.status, accepts the resulting
// stream, and forwards each decoded event as TaskEventMsg via program.Send.
// Returns once ctx is cancelled or the stream errors out.
func SubscribeTaskStatus(ctx context.Context, conn objproto.Connection, p trsf.Transport, program *tea.Program) {
	subscribeAndStream(ctx, conn, p, topics.TasksStatus(), program, func(payload []byte) tea.Msg {
		ev, err := DecodeTaskStatus(payload)
		if err != nil {
			slog.Warn("decode task event", "err", err)
			return nil
		}
		return TaskEventMsg{Event: ev}
	})
}

// SubscribeRunnerStatus mirror of SubscribeTaskStatus for runners.status.
func SubscribeRunnerStatus(ctx context.Context, conn objproto.Connection, p trsf.Transport, program *tea.Program) {
	subscribeAndStream(ctx, conn, p, topics.RunnersStatus(), program, func(payload []byte) tea.Msg {
		ev, err := DecodeRunnerStatus(payload)
		if err != nil {
			slog.Warn("decode runner event", "err", err)
			return nil
		}
		return RunnerEventMsg{Event: ev}
	})
}

// SubscribeTaskLog joins task.<taskID>.log and forwards each chunk as
// LogChunkMsg{TaskID: taskID}. Caller is expected to filter on TaskID at the
// consumer side because rapid tab-switching may interleave streams briefly.
func SubscribeTaskLog(ctx context.Context, conn objproto.Connection, p trsf.Transport, program *tea.Program, taskID string) {
	subscribeAndStream(ctx, conn, p, topics.TaskLog(taskID), program, func(payload []byte) tea.Msg {
		chunk := make([]byte, len(payload))
		copy(chunk, payload)
		return LogChunkMsg{TaskID: taskID, Chunk: chunk}
	})
}

// subscribeAndStream sends a JOIN, accepts the next stream, discards the topic
// header, then delivers payload chunks via fn(payload) → program.Send.
func subscribeAndStream(ctx context.Context, conn objproto.Connection, p trsf.Transport, topic string, program *tea.Program, fn func([]byte) tea.Msg) {
	joinBytes := pubsub.JoinTopic("tui", topic)
	if _, _, err := conn.SendMessage(joinBytes); err != nil {
		slog.Warn("JOIN failed", "topic", topic, "err", err)
		return
	}
	st, err := p.AcceptBidirectionalStream(ctx)
	if err != nil {
		slog.Warn("accept stream failed", "topic", topic, "err", err)
		return
	}
	// Topic-header line: byte-by-byte until '\n'.
	for {
		data, eof, err := st.ReadDirect(1)
		if err != nil || eof {
			return
		}
		if len(data) > 0 && data[0] == '\n' {
			break
		}
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
