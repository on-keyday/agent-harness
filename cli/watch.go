package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/topics"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/trsf"
)

// Watch subscribes to tasks.status and runners.status topics and prints a
// human-readable line to out for each event. Returns when ctx is cancelled or a stream fails.
//
// Method form: callable on an existing *Client without re-dialing. Long-lived
// consumers (tui, wasm) should use this directly.
//
// Each JOIN carries a unique request_id (managed by pubsub.Client); the
// broker echoes it back in the PubSubResponse along with the StreamId of the
// stream it created. Both subscribers are correlated independently, so
// concurrent JOIN ordering on the wire does not affect which goroutine reads
// which stream.
func (c *Client) Watch(ctx context.Context, out io.Writer) error {
	tasksTopic := topics.TasksStatus()
	runnersTopic := topics.RunnersStatus()

	tasksStream, err := c.Peer().JoinAndGetStream(ctx, "watch", tasksTopic)
	if err != nil {
		return fmt.Errorf("join %s: %w", tasksTopic, err)
	}
	runnersStream, err := c.Peer().JoinAndGetStream(ctx, "watch", runnersTopic)
	if err != nil {
		return fmt.Errorf("join %s: %w", runnersTopic, err)
	}

	var mu sync.Mutex

	// consumeTasks reads a stream carrying TaskStatusEvent messages.
	consumeTasks := func(st trsf.BidirectionalStream) error {
		return readFrames(st, tasksTopic, func(p []byte) {
			ev := &protocol.TaskStatusEvent{}
			if _, err := ev.Decode(p); err != nil {
				slog.Warn("decode task event", "err", err)
				return
			}
			mu.Lock()
			fmt.Fprintf(out, "task %x kind=%s status=%s ts=%d exit=%d\n",
				ev.TaskId.Id[:6], ev.Kind, ev.TaskStatus, ev.Ts, ev.ExitCode)
			mu.Unlock()
		})
	}

	// consumeRunners reads a stream carrying RunnerStatusEvent messages.
	consumeRunners := func(st trsf.BidirectionalStream) error {
		return readFrames(st, runnersTopic, func(p []byte) {
			ev := &protocol.RunnerStatusEvent{}
			if _, err := ev.Decode(p); err != nil {
				slog.Warn("decode runner event", "err", err)
				return
			}
			mu.Lock()
			fmt.Fprintf(out, "runner kind=%s status=%s ts=%d\n", ev.Kind, ev.RunnerStatus, ev.Ts)
			mu.Unlock()
		})
	}

	return watchFrames(ctx,
		func() error { return consumeTasks(tasksStream) },
		func() error { return consumeRunners(runnersStream) },
	)
}

// Watch (package-level) is a thin wrapper that opens a fresh Client per call.
// Suitable for short-lived CLI processes (harness-cli). Long-lived consumers
// should hold a *Client and call (*Client).Watch instead.
func Watch(ctx context.Context, peerCID objproto.ConnectionID, out io.Writer) error {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Watch(ctx, out)
}
