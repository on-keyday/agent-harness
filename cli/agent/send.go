package agent

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// Send is the entry for `harness-cli agent send`.
func Send(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("agent send", flag.ContinueOnError)
	serverCID := fs.String("server-cid", "", "server ConnectionID (env: HARNESS_SERVER_CID)")
	taskID := fs.String("task-id", "", "(debug) task id hex (env: HARNESS_TASK_ID)")
	runnerID := fs.String("runner-id", "", "(debug) runner id (env: HARNESS_RUNNER_ID)")
	topic := fs.String("topic", "", "agentboard topic")
	data := fs.String("data", "-", `payload string, or "-" to read stdin`)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *topic == "" {
		return errors.New("--topic required")
	}

	var payload []byte
	if *data == "-" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		payload = b
	} else {
		payload = []byte(*data)
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

	reqID := rand.Uint32()
	respCh := make(chan agentboard.SendResponse, 1)
	conn.SetOnControl(func(kind wire.ApplicationPayloadKind, p []byte) {
		if kind != wire.ApplicationPayloadKind_AgentMessage {
			return
		}
		msg := &agentboard.AgentMessage{}
		if _, err := msg.Decode(p); err != nil {
			return
		}
		if msg.Kind == agentboard.AgentMessageKind_SendResponse {
			r := msg.SendResponse()
			if r != nil && r.RequestId == reqID {
				select {
				case respCh <- *r:
				default:
				}
			}
		}
	})

	req := agentboard.SendRequest{RequestId: reqID}
	req.SetTopic([]byte(*topic))
	req.SetPayload(payload)

	msg := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_Send}
	if !msg.SetSend(req) {
		return errors.New("agent: SetSend failed")
	}
	if err := conn.SendRaw(msg); err != nil {
		return err
	}

	select {
	case resp := <-respCh:
		if resp.Status != agentboard.SendStatus_Ok {
			return fmt.Errorf("send rejected: %v", resp.Status)
		}
		out, _ := json.Marshal(map[string]any{"seq": resp.Seq, "status": "ok"})
		fmt.Fprintln(stdout, string(out))
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
