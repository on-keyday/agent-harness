package agent

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/appwire"
)

// Wait blocks until a message arrives on the given topic, or until --timeout.
// Output: JSON Lines on stdout (1 line per delivered message). When --since-last
// is used, the cursor file under $XDG_CACHE_HOME/harness/ is updated on success.
func Wait(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("agent wait", flag.ContinueOnError)
	serverCID := fs.String("server-cid", "", "server ConnectionID (env: HARNESS_SERVER_CID)")
	taskID := fs.String("task-id", "", "(debug) task id hex (env: HARNESS_TASK_ID)")
	runnerID := fs.String("runner-id", "", "(debug) runner id (env: HARNESS_RUNNER_ID)")
	topic := fs.String("topic", "", "topic to wait on")
	sinceLast := fs.Bool("since-last", false, "use the persisted cursor")
	since := fs.Uint64("since", 0, "cursor to wait beyond (ignored if --since-last)")
	timeout := fs.Duration("timeout", 5*time.Minute, "max block duration")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *topic == "" {
		return errors.New("--topic required")
	}

	conn, err := ConnectAgent(ctx, Flags{
		ServerCID: *serverCID,
		TaskID:    *taskID,
		RunnerID:  *runnerID,
	})
	if err != nil {
		return err
	}
	defer conn.Close()

	cursor := *since
	var oldLive uint64
	if *sinceLast {
		live, _, err := LoadCursor(hexTaskID(conn.TaskID()))
		if err == nil {
			oldLive = live
			cursor = live
		}
	}

	reqID := rand.Uint32()
	respCh := make(chan agentboard.WaitResponse, 1)
	conn.SetOnControl(func(kind appwire.AppKind, p []byte) {
		if kind != appwire.AppKind_AgentMessage {
			return
		}
		msg := &agentboard.AgentMessage{}
		if _, err := msg.Decode(p); err != nil {
			return
		}
		if msg.Kind == agentboard.AgentMessageKind_WaitResponse {
			r := msg.WaitResponse()
			if r != nil && r.RequestId == reqID {
				select {
				case respCh <- *r:
				default:
				}
			}
		}
	})

	wr := agentboard.WaitRequest{
		RequestId: reqID,
		Since:     cursor,
		TimeoutMs: uint32(timeout.Milliseconds()),
	}
	wr.SetPattern([]byte(*topic))

	waitMsg := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_Wait}
	if !waitMsg.SetWait(wr) {
		return errors.New("agent: SetWait failed")
	}
	if err := conn.SendRaw(waitMsg); err != nil {
		return err
	}

	select {
	case r := <-respCh:
		for _, m := range r.Msgs {
			payload, perr := conn.FetchDeliveredPayload(ctx, m.PayloadStreamId)
			if perr != nil {
				return fmt.Errorf("fetch payload seq=%d: %w", m.Seq, perr)
			}
			emitMessageLine(stdout, m.Seq, string(m.Topic), payload, m.FromRunnerId, m.FromTaskId, string(m.FromHostname))
		}
		if *sinceLast {
			_ = SaveCursor(hexTaskID(conn.TaskID()), r.NextCursor, oldLive)
		}
		if r.TimedOut == 1 && len(r.Msgs) == 0 {
			return errors.New("timeout")
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
