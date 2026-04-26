package cli

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/topics"
	"github.com/on-keyday/agent-harness/trsf"
)

// Watch subscribes to tasks.status and runners.status topics and prints a
// human-readable line to out for each event. Returns when ctx is cancelled.
//
// Each JOIN carries a unique request_id (managed by pubsub.Client); the
// broker echoes it back in the PubSubResponse along with the StreamId of the
// stream it created. Both subscribers are correlated independently, so
// concurrent JOIN ordering on the wire does not affect which goroutine reads
// which stream.
func Watch(ctx context.Context, peerCID objproto.ConnectionID, out io.Writer) error {
	c, err := Dial(ctx, peerCID)
	if err != nil {
		return err
	}
	defer c.Close()

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

	// consumeTasks drains a stream carrying TaskStatusEvent payloads.
	consumeTasks := func(st trsf.BidirectionalStream) {
		var buf []byte
		for {
			data, eof, err := st.ReadDirect(4096)
			if err != nil {
				return
			}
			if len(data) > 0 {
				buf = append(buf, data...)
				buf = drainTaskEvents(buf, out, &mu)
			}
			if eof {
				return
			}
		}
	}

	// consumeRunners drains a stream carrying RunnerStatusEvent payloads.
	consumeRunners := func(st trsf.BidirectionalStream) {
		var buf []byte
		for {
			data, eof, err := st.ReadDirect(4096)
			if err != nil {
				return
			}
			if len(data) > 0 {
				buf = append(buf, data...)
				buf = drainRunnerEvents(buf, out, &mu)
			}
			if eof {
				return
			}
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); consumeTasks(tasksStream) }()
	go func() { defer wg.Done(); consumeRunners(runnersStream) }()

	<-ctx.Done()
	return ctx.Err()
}

// drainTaskEvents decodes as many TaskStatusEvent records as possible from buf,
// prints each one, and returns the unconsumed tail.
func drainTaskEvents(buf []byte, out io.Writer, mu *sync.Mutex) []byte {
	for {
		ev := &protocol.TaskStatusEvent{}
		rest, err := ev.Decode(buf)
		if err != nil {
			// Not enough data yet; wait for more bytes.
			break
		}
		mu.Lock()
		fmt.Fprintf(out, "task %x kind=%s status=%s ts=%d exit=%d\n",
			ev.TaskId.Id[:6], ev.Kind, ev.TaskStatus, ev.Ts, ev.ExitCode)
		mu.Unlock()
		buf = rest
	}
	return buf
}

// drainRunnerEvents decodes as many RunnerStatusEvent records as possible from buf,
// prints each one, and returns the unconsumed tail.
func drainRunnerEvents(buf []byte, out io.Writer, mu *sync.Mutex) []byte {
	for {
		ev := &protocol.RunnerStatusEvent{}
		rest, err := ev.Decode(buf)
		if err != nil {
			// Not enough data yet; wait for more bytes.
			break
		}
		mu.Lock()
		fmt.Fprintf(out, "runner kind=%s status=%s ts=%d\n",
			ev.Kind, ev.RunnerStatus, ev.Ts)
		mu.Unlock()
		buf = rest
	}
	return buf
}
