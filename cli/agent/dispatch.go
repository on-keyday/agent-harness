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

// Dispatch publishes to --topic and blocks until a message ANSWERING that
// publish arrives, all over one Hello'd connection. JSON-Lines output.
//
// There is no --reply-topic. A reply carrying --in-reply-to and no topic is
// routed by the server to the ORIGINAL SENDER's own chat.<short-id> (see
// resolveReplyTarget in server/agent_handler.go), so the reply topic is not a
// caller's choice: a supplied one could only disagree with where the reply
// actually lands.
//
// The wait is bounded below by the seq this call published AND filtered to
// replies to it. Before, it waited on a caller-named topic from Since:0 with no
// correlation, so every message already retained there satisfied it — including
// an answer to somebody else's question.
//
// This is a shell-level tool for scripting OUTSIDE an agent's turn loop, for
// the same reason `agent wait` is.
func Dispatch(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("agent dispatch", flag.ContinueOnError)
	serverCID := fs.String("server-cid", "", "")
	topic := fs.String("topic", "", "topic to send to")
	data := fs.String("data", "-", `payload string or "-" for stdin`)
	timeout := fs.Duration("timeout", 5*time.Minute, "max wait")
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

	if err := refuseIfOwnTicket(payload); err != nil {
		return err
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
	var publishedSeq uint64
	select {
	case r := <-sendCh:
		if r.Status != agentboard.SendStatus_Ok {
			return fmt.Errorf("send failed: %v", r.Status)
		}
		publishedSeq = r.Seq
	case <-ctx.Done():
		return ctx.Err()
	}

	// Wait for the reply on OUR own topic — where the server routes it — above
	// our own publish, and only for messages answering that seq.
	wr := agentboard.WaitRequest{
		RequestId: waitID,
		Since:     publishedSeq,
		TimeoutMs: uint32(timeout.Milliseconds()),
		InReplyTo: publishedSeq,
	}
	wr.SetPattern([]byte(agentboard.SelfTopic(conn.TaskID())))
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
			emitMessageLine(stdout, m.Seq, string(m.Topic), payload, m.FromRunnerId, m.FromTaskId, string(m.FromHostname), string(m.FromAgentProfile), m.InReplyTo)
		}
		if r.TimedOut == 1 && len(r.Msgs) == 0 {
			return errors.New("dispatch reply timeout")
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
