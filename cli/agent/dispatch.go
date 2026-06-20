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

// Dispatch sends a message to --topic, then blocks waiting for a reply on
// --reply-topic, all over one Hello'd connection. JSON-Lines output of the
// reply messages.
func Dispatch(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("agent dispatch", flag.ContinueOnError)
	serverCID := fs.String("server-cid", "", "")
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
	})
	if err != nil {
		return err
	}
	defer conn.Close()

	sendID := rand.Uint32()
	waitID := rand.Uint32()
	sendCh := make(chan agentboard.SendResponse, 1)
	waitCh := make(chan agentboard.WaitResponse, 1)
	conn.SetOnControl(func(kind appwire.AppKind, p []byte) {
		if kind != appwire.AppKind_AgentMessage {
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

	// Send: payload travels on a client-initiated send-stream (UDP MTU fix).
	sendStream := conn.PC().Transport().CreateSendStream()
	if sendStream == nil {
		return errors.New("agent: failed to allocate payload stream")
	}
	if werr := sendStream.AppendData(false, payload); werr != nil {
		return fmt.Errorf("agent: payload stream write: %w", werr)
	}
	if werr := sendStream.AppendData(true); werr != nil {
		return fmt.Errorf("agent: payload stream EOF: %w", werr)
	}
	sr := agentboard.SendRequest{RequestId: sendID, PayloadStreamId: uint64(sendStream.ID())}
	sr.SetTopic([]byte(*topic))
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
			payload, perr := conn.FetchDeliveredPayload(ctx, m.PayloadStreamId)
			if perr != nil {
				return fmt.Errorf("fetch payload seq=%d: %w", m.Seq, perr)
			}
			emitMessageLine(stdout, m.Seq, string(m.Topic), payload, m.FromRunnerId, m.FromTaskId, string(m.FromHostname))
		}
		if r.TimedOut == 1 && len(r.Msgs) == 0 {
			return errors.New("dispatch reply timeout")
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
