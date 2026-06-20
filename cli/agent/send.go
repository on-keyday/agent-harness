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
	"github.com/on-keyday/agent-harness/appwire"
)

// Send is the entry for `harness-cli agent send`.
func Send(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("agent send", flag.ContinueOnError)
	serverCID := fs.String("server-cid", "", "server ConnectionID (env: HARNESS_SERVER_CID)")
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
	})
	if err != nil {
		return err
	}
	defer conn.Close()

	reqID := rand.Uint32()
	respCh := make(chan agentboard.SendResponse, 1)
	conn.SetOnControl(func(kind appwire.AppKind, p []byte) {
		if kind != appwire.AppKind_AgentMessage {
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

	// Allocate a client-initiated send-stream for the payload; the server
	// reads from the matching receive stream until EOF and treats those
	// bytes as the publish body. Streaming the payload (instead of stuffing
	// it into the SendRequest envelope) keeps the envelope inside path MTU
	// on UDP transport.
	stream := conn.PC().Transport().CreateSendStream()
	if stream == nil {
		return errors.New("agent: failed to allocate payload stream")
	}
	if werr := stream.AppendData(false, payload); werr != nil {
		return fmt.Errorf("agent: payload stream write: %w", werr)
	}
	if werr := stream.AppendData(true); werr != nil {
		return fmt.Errorf("agent: payload stream EOF: %w", werr)
	}

	req := agentboard.SendRequest{RequestId: reqID, PayloadStreamId: uint64(stream.ID())}
	req.SetTopic([]byte(*topic))

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
