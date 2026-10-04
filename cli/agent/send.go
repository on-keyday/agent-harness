package agent

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
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
	act, berr := sp.BuildFunc()(b)
	if berr != nil {
		return berr
	}
	a := act.(verb.AgentSendAction)
	return SendWith(ctx, a, stdin, stdout)
}

// SendWith is Send for a caller that already has the parsed action --
// the generated CLI dispatch, which parses from the declaration itself.
func SendWith(ctx context.Context, a verb.AgentSendAction, stdin io.Reader, stdout io.Writer) error {
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

	c, cerr := connectClient(ctx, *serverCID)
	if cerr != nil {
		return cerr
	}
	defer c.Close()

	r, err := c.TaskControlWithPayload(ctx, func(streamID uint64) (*protocol.TaskControlRequest, error) {
		req := protocol.AgentSendRequest{PayloadStreamId: streamID, InReplyTo: *inReplyTo}
		// Negative on the wire too, so the zero value means the default. Only
		// set it when the caller asked to opt out.
		if *noRetireOnReply {
			req.SetNoRetireOnReply(true)
		}
		// An empty topic is the wire's "derive the destination from the
		// parent"; the schema assertion guarantees it can only be empty on a
		// reply.
		req.SetTopic([]byte(wireTopic))
		// Where REPLIES to this message go. The server records it on the
		// retained entry and resolveReplyTarget reads it back, so the peer
		// answers with --in-reply-to alone and never has to learn the topic.
		if *replyTo != "" {
			if !req.SetReplyToTopic([]byte(*replyTo)) {
				return nil, errors.New("--reply-to too long")
			}
		}
		tcReq := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AgentSend}
		tcReq.SetAgentSend(req)
		return tcReq, nil
	}, payload)
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	return sendResult(r, *inReplyTo, len(payload), source, stdout)
}

// sendResult renders a SendResponse: the ok line on stdout, or the error the
// status stands for. n and source describe the body that was published.
func sendResult(r cli.TaskControlResult, inReplyTo uint64, n int, source string, stdout io.Writer) error {
	if r.Err != nil {
		return r.Err
	}
	if err := expectKind(r.Resp, protocol.TaskControlKind_AgentSend); err != nil {
		return err
	}
	resp := r.Resp.AgentSend()
	if resp == nil {
		return errors.New("agent: send response variant is nil")
	}
	if resp.Status == protocol.SendStatus_UnknownInReplyTo {
		return fmt.Errorf("send rejected: --in-reply-to %d is not on the board "+
			"(evicted past the topic's ring or TTL, or purged). "+
			"Drop --in-reply-to to send this as an ordinary message", inReplyTo)
	}
	if resp.Status == protocol.SendStatus_NoReplyRoute {
		return &cli.NoReplyRouteError{InReplyTo: inReplyTo}
	}
	if resp.Status != protocol.SendStatus_Ok {
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
