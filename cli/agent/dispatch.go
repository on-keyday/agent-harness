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
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// Dispatch sends a message to --topic, then blocks waiting for a reply on
// --reply-topic, all over one Hello'd connection. JSON-Lines output of the
// reply messages.
func Dispatch(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("agent dispatch", flag.ContinueOnError)
	serverCID := fs.String("server-cid", "", "")
	taskID := fs.String("task-id", "", "")
	runnerID := fs.String("runner-id", "", "")
	topic := fs.String("topic", "", "topic to send to")
	replyTopic := fs.String("reply-topic", "", "topic to wait for reply on")
	data := fs.String("data", "-", `payload string or "-" for stdin`)
	timeout := fs.Duration("timeout", 5*time.Minute, "max wait")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *topic == "" || *replyTopic == "" {
		return errors.New("--topic and --reply-topic required")
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

	sendID := rand.Uint32()
	waitID := rand.Uint32()
	sendCh := make(chan agentboard.SendResponse, 1)
	waitCh := make(chan agentboard.WaitResponse, 1)
	conn.SetOnControl(func(kind wire.ApplicationPayloadKind, p []byte) {
		if kind != wire.ApplicationPayloadKind_AgentMessage {
			return
		}
		msg := &agentboard.AgentMessage{}
		if _, err := msg.Decode(p); err != nil {
			return
		}
		switch msg.Kind {
		case agentboard.AgentMessageKind_SendResponse:
			r := msg.SendResponse()
			if r != nil && r.RequestId == sendID {
				select {
				case sendCh <- *r:
				default:
				}
			}
		case agentboard.AgentMessageKind_WaitResponse:
			r := msg.WaitResponse()
			if r != nil && r.RequestId == waitID {
				select {
				case waitCh <- *r:
				default:
				}
			}
		}
	})

	// Send
	sr := agentboard.SendRequest{RequestId: sendID}
	sr.SetTopic([]byte(*topic))
	sr.SetPayload(payload)
	sendMsg := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_Send}
	if !sendMsg.SetSend(sr) {
		return errors.New("agent: SetSend failed")
	}
	if err := conn.SendRaw(sendMsg); err != nil {
		return err
	}
	select {
	case r := <-sendCh:
		if r.Status != agentboard.SendStatus_Ok {
			return fmt.Errorf("send failed: %v", r.Status)
		}
	case <-ctx.Done():
		return ctx.Err()
	}

	// Wait for reply
	wr := agentboard.WaitRequest{
		RequestId: waitID,
		Since:     0,
		TimeoutMs: uint32(timeout.Milliseconds()),
	}
	wr.SetPattern([]byte(*replyTopic))
	waitMsg := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_Wait}
	if !waitMsg.SetWait(wr) {
		return errors.New("agent: SetWait failed")
	}
	if err := conn.SendRaw(waitMsg); err != nil {
		return err
	}
	select {
	case r := <-waitCh:
		for _, m := range r.Msgs {
			emitMessageLine(stdout, m.Seq, string(m.Topic), m.Payload)
		}
		if r.TimedOut == 1 && len(r.Msgs) == 0 {
			return errors.New("dispatch reply timeout")
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
