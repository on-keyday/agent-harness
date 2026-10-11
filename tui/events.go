package tui

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/pubsub"
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

// ForwardStatusMsg / ExecStatusMsg carry one decoded registry event.
type ForwardStatusMsg struct {
	Event protocol.ForwardStatusEvent
}

type ExecStatusMsg struct {
	Event protocol.ExecStatusEvent
}

// DecodeForwardStatus decodes a ForwardStatusEvent payload.
func DecodeForwardStatus(payload []byte) (protocol.ForwardStatusEvent, error) {
	var ev protocol.ForwardStatusEvent
	if _, err := ev.Decode(payload); err != nil {
		return protocol.ForwardStatusEvent{}, fmt.Errorf("decode ForwardStatusEvent: %w", err)
	}
	return ev, nil
}

// SubscribeForwardStatus mirrors SubscribeConnStatus for forwards.status.
//
// Why forwards need this and did not before: the pane used to hold pure
// configuration, so one fetch when it opened was the whole truth. It now holds
// counters, and a fetch-once pane shows an idle 0/0 while bytes cross — which
// reads as an answer rather than as stale data.
func SubscribeForwardStatus(ctx context.Context, c *cli.Client, program *tea.Program) {
	subscribeAndStream(ctx, c, topics.ForwardsStatus(), program, func(payload []byte) tea.Msg {
		ev, err := DecodeForwardStatus(payload)
		if err != nil {
			slog.Warn("decode forward event", "err", err)
			return nil
		}
		return ForwardStatusMsg{Event: ev}
	})
}

// DecodeExecStatus decodes an ExecStatusEvent payload.
func DecodeExecStatus(payload []byte) (protocol.ExecStatusEvent, error) {
	var ev protocol.ExecStatusEvent
	if _, err := ev.Decode(payload); err != nil {
		return protocol.ExecStatusEvent{}, fmt.Errorf("decode ExecStatusEvent: %w", err)
	}
	return ev, nil
}

// SubscribeExecStatus is its sibling for execs.status. The exec pane had the
// same fetch-once staleness; it is less loud because an exec row is discrete,
// but it is the same defect and gets the same fix in the same change.
func SubscribeExecStatus(ctx context.Context, c *cli.Client, program *tea.Program) {
	subscribeAndStream(ctx, c, topics.ExecsStatus(), program, func(payload []byte) tea.Msg {
		ev, err := DecodeExecStatus(payload)
		if err != nil {
			slog.Warn("decode exec event", "err", err)
			return nil
		}
		return ExecStatusMsg{Event: ev}
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
// NotifyEvent as NotifyEventMsg. The ring replay arrives first, as ordinary
// messages.
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

// subscribeAndStream joins topic and pumps each message through fn →
// program.Send via PumpEvents. Runs until ctx is cancelled; stream death
// resubscribes via resubscribeLoop.
func subscribeAndStream(ctx context.Context, c *cli.Client, topic string, program *tea.Program, fn func([]byte) tea.Msg) {
	resubscribeLoop(ctx, c, topic, program, func(st trsf.BidirectionalStream) {
		_ = PumpEvents(ctx, st, topic, fn, program.Send)
	})
}

// PumpEvents reads topic's subscriber stream one message at a time and sends
// fn's result for each. It returns the reader's error — io.EOF when the stream
// ended cleanly — or ctx.Err().
func PumpEvents(ctx context.Context, st pubsub.StreamReader, topic string, fn func([]byte) tea.Msg, send func(tea.Msg)) error {
	fr := pubsub.NewFrameReader(st)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		payload, err := fr.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				slog.Warn("subscription stream", "topic", topic, "err", err)
			}
			return err
		}
		if msg := fn(payload); msg != nil {
			send(msg)
		}
	}
}

// FormatTaskID returns the hex string for a TaskID, exposed for app.go.
func FormatTaskID(t protocol.TaskID) string {
	return hex.EncodeToString(t.Id[:])
}

// ParseTaskID is FormatTaskID's inverse: it turns a 32-hex id back into the
// wire type. The picker needs it to rebuild a TaskScope from the ids it holds
// as strings; every other caller in this repo hex-decodes inline because it is
// going straight into a request.
func ParseTaskID(s string) (protocol.TaskID, error) {
	raw, err := hex.DecodeString(s)
	if err != nil {
		return protocol.TaskID{}, fmt.Errorf("task id %q is not hex: %w", s, err)
	}
	if len(raw) != 16 {
		return protocol.TaskID{}, fmt.Errorf("task id %q: want 16 bytes, got %d", s, len(raw))
	}
	var out protocol.TaskID
	copy(out.Id[:], raw)
	return out, nil
}
