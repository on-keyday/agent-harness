package agent

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/cli/verb"
)

// sendTargetArgs validates the destination pair and returns the topic to put on
// the wire. An empty topic is legal only alongside a non-zero in-reply-to, in
// which case the server derives the destination from the parent's authenticated
// sender; the schema encodes the same rule (SendRequest asserts
// topic_len != 0 || in_reply_to != 0), this is the early, legible error.
func sendTargetArgs(topic string, inReplyTo uint64) (string, error) {
	if topic == "" && inReplyTo == 0 {
		return "", errors.New("--topic required (or --in-reply-to, to reply to the sender of that message)")
	}
	return topic, nil
}

// Send is the entry for `harness-cli agent send`.
func Send(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	// Parsed from the declaration (cli/verb), which knows this verb takes a
	// joined-positional payload -- something cli/flagorder_test.go's guard
	// could not see, because the read happens inside resolvePayload rather
	// than off the FlagSet, so `agent send` was never on its allowlist.
	sp, _ := verb.Lookup("agent", "send")
	sp = sp.For(verb.CLI)
	fs := sp.NewFlagSet(flag.ContinueOnError)
	b, perr := sp.Parse(fs, args)
	if perr != nil {
		return perr
	}
	act, berr := sp.Build(b)
	if berr != nil {
		return berr
	}
	a := act.(verb.AgentSendAction)
	serverCID, topic, data := &a.ServerCID, &a.Topic, &a.Data
	inReplyTo, replyTo := &a.InReplyTo, &a.ReplyTo
	noRetireOnReply := &a.NoRetireOnReply
	wireTopic, err := sendTargetArgs(*topic, *inReplyTo)
	if err != nil {
		return err
	}

	payload, source, err := resolvePayloadFrom(a.DataSet, *data, a.Positional, stdin)
	if err != nil {
		return err
	}

	if err := refuseIfOwnTicket(payload); err != nil {
		return err
	}

	conn, cerr := ConnectAgent(ctx, Flags{
		ServerCID: *serverCID,
	})
	if cerr != nil {
		return cerr
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

	req := agentboard.SendRequest{RequestId: reqID, PayloadStreamId: uint64(stream.ID()), InReplyTo: *inReplyTo}
	// Negative on the wire too, so the zero value means the default. Only set
	// it when the caller asked to opt out.
	if *noRetireOnReply {
		req.SetNoRetireOnReply(true)
	}
	// An empty topic is the wire's "derive the destination from the parent"; the
	// schema assertion guarantees it can only be empty on a reply.
	req.SetTopic([]byte(wireTopic))
	// Where REPLIES to this message go. The server records it on the retained
	// entry and resolveReplyTarget reads it back, so the peer answers with
	// --in-reply-to alone and never has to learn the topic.
	if *replyTo != "" {
		if !req.SetReplyToTopic([]byte(*replyTo)) {
			return errors.New("agent: --reply-to too long")
		}
	}

	msg := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_Send}
	if !msg.SetSend(req) {
		return errors.New("agent: SetSend failed")
	}
	// The request goes out BEFORE the body. It names the stream id, so until
	// the server has it, nobody drains the payload stream: AppendData blocks
	// once the send buffer fills (1MB) and stays blocked once the peer's
	// receive window (16MB) is exhausted, so a body past the window used to
	// deadlock here with the request still unsent. Announcing first gives the
	// server a reader — which is also what lets it stop an over-long body
	// mid-flight instead of discovering the length after the fact. The server
	// polls briefly for the stream to become visible, so arriving first is
	// expected (server/agent_handler.go, readAgentPayloadStream).
	if err := conn.SendRaw(msg); err != nil {
		return err
	}

	// AppendDataContext, not AppendData: the latter passes context.Background()
	// internally, so a stalled write would ignore this command's deadline and
	// hang instead of failing.
	writeErr := stream.AppendDataContext(ctx, false, payload)
	if writeErr == nil {
		writeErr = stream.AppendDataContext(ctx, true)
	}
	if writeErr != nil {
		// The server tears the payload stream down when it refuses the body,
		// which surfaces here as a bare io.EOF. The actionable reason is in
		// its SendResponse, so wait briefly and prefer that; "EOF" tells the
		// sender nothing about what to do differently.
		select {
		case resp := <-respCh:
			return sendResult(resp, *inReplyTo, len(payload), source, stdout)
		case <-time.After(payloadErrGrace):
			return fmt.Errorf("agent: payload stream write: %w", writeErr)
		case <-ctx.Done():
			return fmt.Errorf("agent: payload stream write: %w", writeErr)
		}
	}

	select {
	case resp := <-respCh:
		return sendResult(resp, *inReplyTo, len(payload), source, stdout)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// payloadErrGrace bounds how long a failed payload write waits for the
// server's explanation before reporting the local error instead.
const payloadErrGrace = 2 * time.Second

// sendResult renders a SendResponse: the ok line on stdout, or the error the
// status stands for. n and source describe the body that was published.
func sendResult(resp agentboard.SendResponse, inReplyTo uint64, n int, source string, stdout io.Writer) error {
	if resp.Status == agentboard.SendStatus_UnknownInReplyTo {
		return fmt.Errorf("send rejected: --in-reply-to %d is not on the board "+
			"(evicted past the topic's ring or TTL, or purged). "+
			"Drop --in-reply-to to send this as an ordinary message", inReplyTo)
	}
	if resp.Status != agentboard.SendStatus_Ok {
		// The size belongs on the rejection too: PayloadTooLarge is the status
		// whose only remedy is splitting the body, and the sender cannot pick a
		// split without knowing what it just tried to publish.
		return fmt.Errorf("send rejected: %v (%d bytes from %s)", resp.Status, n, source)
	}
	// delivered_to is the point of the line for a sender debugging silence:
	// status ok with 0 means the topic exists but nobody holds it (typo'd or
	// stale chat.<short-id> is the usual cause), which is otherwise
	// indistinguishable from a delivered send.
	//
	// bytes and source answer the question one step earlier: whether what went
	// out is what the caller meant to send. A shell that swallowed the pipe, a
	// heredoc that expanded to nothing, `--data -` typed as a positional — all
	// of them publish successfully, and until the count was on this line the
	// only way to notice was to go read the message back.
	out, _ := json.Marshal(map[string]any{
		"seq": resp.Seq, "status": "ok", "delivered_to": resp.DeliveredTo,
		"bytes": n, "source": source,
	})
	fmt.Fprintln(stdout, string(out))
	return nil
}
