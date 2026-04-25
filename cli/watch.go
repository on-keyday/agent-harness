package cli

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/pubsub"
	pubsubproto "github.com/on-keyday/agent-harness/pubsub/protocol"
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
func Watch(ctx context.Context, addr string, out io.Writer) error {
	c, err := Dial(ctx, addr)
	if err != nil {
		return err
	}
	defer c.Close()

	tasksTopic := topics.TasksStatus()
	runnersTopic := topics.RunnersStatus()

	tasksStream, err := joinAndGetStream(ctx, c.Conn(), c.Transport(), c.Pubsub(), tasksTopic)
	if err != nil {
		return fmt.Errorf("join %s: %w", tasksTopic, err)
	}
	runnersStream, err := joinAndGetStream(ctx, c.Conn(), c.Transport(), c.Pubsub(), runnersTopic)
	if err != nil {
		return fmt.Errorf("join %s: %w", runnersTopic, err)
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

// joinAndGetStream issues a JOIN on topic via pubClient and returns the
// broker-created stream once the response correlates back. The topic-header
// line written by the broker as the first chunk is consumed before returning.
func joinAndGetStream(ctx context.Context, conn objproto.Connection, p trsf.Transport, pubClient *pubsub.Client, topic string) (trsf.BidirectionalStream, error) {
	respCh := make(chan *pubsubproto.PubSubResponse, 1)
	joinBytes := pubClient.JoinTopic("watch", topic, func(r *pubsubproto.PubSubResponse) { respCh <- r })
	if joinBytes == nil {
		return nil, fmt.Errorf("encode JOIN failed (nickname too long?)")
	}
	if _, _, err := conn.SendMessage(joinBytes); err != nil {
		return nil, fmt.Errorf("send JOIN: %w", err)
	}
	var resp *pubsubproto.PubSubResponse
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resp = <-respCh:
	}
	if resp.Status != pubsubproto.Status_Ok {
		return nil, fmt.Errorf("JOIN rejected: status %v", resp.Status)
	}
	st := waitForStream(ctx, p, trsf.StreamID(resp.StreamId))
	if st == nil {
		return nil, fmt.Errorf("stream %d not visible after JOIN", resp.StreamId)
	}
	if err := readUntilNewline(ctx, st); err != nil {
		return nil, fmt.Errorf("read topic header: %w", err)
	}
	return st, nil
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
