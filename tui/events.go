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
	"github.com/on-keyday/objtrsf/trsf"
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

// ConnStatusMsg carries a decoded ConnStatusEvent from the conns.status topic.
type ConnStatusMsg struct {
	Event protocol.ConnStatusEvent
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

// DecodeConnStatus decodes a ConnStatusEvent payload.
func DecodeConnStatus(payload []byte) (protocol.ConnStatusEvent, error) {
	var ev protocol.ConnStatusEvent
	if _, err := ev.Decode(payload); err != nil {
		return protocol.ConnStatusEvent{}, fmt.Errorf("decode ConnStatusEvent: %w", err)
	}
	return ev, nil
}

// SubscribeConnStatus mirrors SubscribeTaskStatus for the conns.status topic.
// It forwards each decoded ConnStatusEvent as a ConnStatusMsg via program.Send.
func SubscribeConnStatus(ctx context.Context, c *cli.Client, program *tea.Program) {
	subscribeAndStream(ctx, c, topics.ConnsStatus(), program, func(payload []byte) tea.Msg {
		ev, err := DecodeConnStatus(payload)
		if err != nil {
			slog.Warn("decode conn event", "err", err)
			return nil
		}
		return ConnStatusMsg{Event: ev}
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

// drainNotifyEvents decodes every complete NotifyEvent in buf, calling emit for
// each (in order), and returns the undrained remainder. Handles coalesced reads
// (multiple events in one payload) and split reads (a partial event at the tail).
func drainNotifyEvents(buf []byte, emit func(protocol.NotifyEvent)) []byte {
	for {
		var ev protocol.NotifyEvent
		rest, err := ev.Decode(buf)
		if err != nil {
			break
		}
		emit(ev)
		buf = rest
	}
	return buf
}

// SubscribeNotifications joins the notifications topic and forwards each decoded
// NotifyEvent as NotifyEventMsg via program.Send. Unlike subscribeAndStream, this
// accumulates bytes across ReadDirect calls and drains ALL complete events per read,
// correctly handling the server-side ring replay path that may coalesce multiple
// AppendData calls into a single ReadDirect payload. The accumulation buffer is
// reset per (re)subscribe: a new stream starts at a ring-replay boundary, so a
// partial event left over from a dead stream must not prefix it.
func SubscribeNotifications(ctx context.Context, c *cli.Client, program *tea.Program) {
	resubscribeLoop(ctx, c, topics.Notifications(), program, func(stream trsf.BidirectionalStream) {
		var buf []byte
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			data, eof, rerr := stream.ReadDirect(64 * 1024)
			if rerr != nil {
				return
			}
			if len(data) > 0 {
				buf = append(buf, data...)
				buf = drainNotifyEvents(buf, func(ev protocol.NotifyEvent) {
					program.Send(NotifyEventMsg{Event: ev})
				})
			}
			if eof {
				return
			}
		}
	})
}

// SubscribedMsg reports a successful JOIN of a pubsub topic. Resubscribed is
// true when this join replaced an earlier stream that died mid-connection —
// the App uses that to gap-fill state the dead stream missed (snapshot
// refresh for status topics, history re-fetch for a followed task log).
type SubscribedMsg struct {
	Topic        string
	Resubscribed bool
}

const (
	resubscribeInitialBackoff = 500 * time.Millisecond
	resubscribeMaxBackoff     = 10 * time.Second
)

// resubscribeLoop joins topic and runs pump on the stream, rejoining with
// exponential backoff whenever the join fails or the pump returns (stream
// error / EOF) while ctx is still live. Every successful join dispatches a
// SubscribedMsg. This is what keeps event delivery robust across mid-
// connection stream death: a dead stream costs one backoff round, not the
// rest of the session (a full disconnect cancels ctx via PersistLoop and
// ends the loop; the reconnect spawns fresh subscriptions).
func resubscribeLoop(ctx context.Context, c *cli.Client, topic string, program *tea.Program, pump func(trsf.BidirectionalStream)) {
	backoff := resubscribeInitialBackoff
	joined := false
	for {
		if ctx.Err() != nil {
			return
		}
		st, err := c.Peer().JoinAndGetStream(ctx, "tui", topic)
		if err != nil {
			// context.Canceled is the normal path when the user switches
			// tasks (followTask cancels the prior subscription's ctx) or the
			// connection drops — don't log, don't retry.
			if ctx.Err() != nil {
				return
			}
			slog.Warn("subscribe", "topic", topic, "err", err, "retry_in", backoff)
		} else {
			program.Send(SubscribedMsg{Topic: topic, Resubscribed: joined})
			joined = true
			backoff = resubscribeInitialBackoff
			pump(st)
			if ctx.Err() != nil {
				return
			}
			slog.Warn("subscription stream ended; rejoining", "topic", topic, "retry_in", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > resubscribeMaxBackoff {
			backoff = resubscribeMaxBackoff
		}
	}
}

// subscribeAndStream uses peer.Conn.JoinAndGetStream to do the JOIN+lookup+
// header-discard dance, then pumps payload chunks through fn → program.Send.
// Runs until ctx is cancelled; stream death resubscribes via resubscribeLoop.
func subscribeAndStream(ctx context.Context, c *cli.Client, topic string, program *tea.Program, fn func([]byte) tea.Msg) {
	resubscribeLoop(ctx, c, topic, program, func(st trsf.BidirectionalStream) {
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
	})
}

// FormatTaskID returns the hex string for a TaskID, exposed for app.go.
func FormatTaskID(t protocol.TaskID) string {
	return hex.EncodeToString(t.Id[:])
}
