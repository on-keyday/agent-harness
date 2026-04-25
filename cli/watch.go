package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/pubsub"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/topics"
	"github.com/on-keyday/agent-harness/trsf"
)

// Watch subscribes to tasks.status and runners.status topics and prints a
// human-readable line to out for each event. Returns when ctx is cancelled.
//
// Implementation note: two separate JOIN messages are sent and two streams are
// accepted, one per topic. Each stream goroutine knows which topic it handles
// (by the order of JOIN / Accept), so it decodes the correct event type without
// ambiguity. The decode-twice heuristic from the original design is avoided.
func Watch(ctx context.Context, addr string, out io.Writer) error {
	c, err := Dial(ctx, addr)
	if err != nil {
		return err
	}
	defer c.Close()

	conn := c.Conn()
	p := trsf.NewStreams(ctx, false, trsf.DefaultInitialMTU, trsf.DefaultMaxMTU, conn, slog.Default())
	go trsf.AutoSend(ctx, p, conn, nil)
	go trsf.AutoReceive(ctx, p, conn, func(*objproto.Message, error) {})
	// Keep the objproto session alive — server's AutoGarbageCollect drops idle sessions
	// after 1 minute, and Watch sits waiting for events indefinitely.
	go trsf.AutoPing(ctx, conn, 30*time.Second)

	// Send JOIN for tasks.status first, then runners.status.
	// The broker opens streams in the order it receives JOINs, so Accept order
	// corresponds to JOIN order.
	tasksTopic := topics.TasksStatus()
	runnersTopic := topics.RunnersStatus()

	if _, _, err := conn.SendMessage(pubsub.JoinTopic("watch", tasksTopic)); err != nil {
		return fmt.Errorf("send JOIN %s: %w", tasksTopic, err)
	}
	if _, _, err := conn.SendMessage(pubsub.JoinTopic("watch", runnersTopic)); err != nil {
		return fmt.Errorf("send JOIN %s: %w", runnersTopic, err)
	}

	// Accept the two streams in the same order as JOIN.
	tasksStream, err := p.AcceptBidirectionalStream(ctx)
	if err != nil {
		return fmt.Errorf("accept tasks.status stream: %w", err)
	}
	runnersStream, err := p.AcceptBidirectionalStream(ctx)
	if err != nil {
		return fmt.Errorf("accept runners.status stream: %w", err)
	}

	var mu sync.Mutex

	// consumeTasks drains a stream carrying TaskStatusEvent payloads.
	consumeTasks := func(st trsf.BidirectionalStream) {
		// Discard topic-name header line.
		if err := readUntilNewline(ctx, st); err != nil {
			return
		}
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
		// Discard topic-name header line.
		if err := readUntilNewline(ctx, st); err != nil {
			return
		}
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
