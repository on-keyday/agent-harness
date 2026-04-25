package tui

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	pubsubproto "github.com/on-keyday/agent-harness/pubsub/protocol"
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

// subscribeAndStream sends a JOIN with a unique request_id (managed by
// cli.Client.Pubsub()), waits for the matching response, looks up the
// broker-created stream by its StreamId, validates the topic-header line,
// then delivers payload chunks via fn(payload) → program.Send.
func subscribeAndStream(ctx context.Context, c *cli.Client, topic string, program *tea.Program, fn func([]byte) tea.Msg) {
	respCh := make(chan *pubsubproto.PubSubResponse, 1)
	joinBytes := c.Pubsub().JoinTopic("tui", topic, func(r *pubsubproto.PubSubResponse) {
		respCh <- r
	})
	if joinBytes == nil {
		slog.Warn("JOIN encode failed (nickname too long?)", "topic", topic)
		return
	}
	if _, _, err := c.Conn().SendMessage(joinBytes); err != nil {
		slog.Warn("JOIN send failed", "topic", topic, "err", err)
		return
	}

	var resp *pubsubproto.PubSubResponse
	select {
	case <-ctx.Done():
		return
	case resp = <-respCh:
	}
	if resp.Status != pubsubproto.Status_Ok {
		slog.Warn("JOIN rejected", "topic", topic, "status", resp.Status)
		return
	}

	// Find the broker-created stream. The response may arrive before the
	// stream-creation trsf frame on the wire, so poll briefly if absent.
	st := waitForStream(ctx, c.Transport(), trsf.StreamID(resp.StreamId))
	if st == nil {
		slog.Warn("stream not visible after JOIN", "topic", topic, "stream_id", resp.StreamId)
		return
	}

	// Read + validate the topic-header line written by the broker as the first chunk.
	var headerBuf []byte
	for {
		data, eof, err := st.ReadDirect(1)
		if err != nil || eof {
			return
		}
		if len(data) > 0 {
			if data[0] == '\n' {
				break
			}
			headerBuf = append(headerBuf, data[0])
		}
	}
	if got := string(headerBuf); got != topic {
		slog.Warn("topic mismatch on stream", "want", topic, "got", got)
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

// waitForStream returns p.GetBidirectionalStream(id), polling briefly if the
// stream isn't yet visible (the JOIN response may race ahead of the
// stream-creation trsf frame on the wire). Returns nil on ctx cancellation
// or if the stream doesn't appear within ~2s.
func waitForStream(ctx context.Context, p trsf.Transport, id trsf.StreamID) trsf.BidirectionalStream {
	if st := p.GetBidirectionalStream(id); st != nil {
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
			if st := p.GetBidirectionalStream(id); st != nil {
				return st
			}
		}
	}
}

// FormatTaskID returns the hex string for a TaskID, exposed for app.go.
func FormatTaskID(t protocol.TaskID) string {
	return hex.EncodeToString(t.Id[:])
}
