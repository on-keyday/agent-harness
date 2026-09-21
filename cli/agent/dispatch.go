package agent

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/on-keyday/agent-harness/cli/cliopts"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Dispatch publishes to --topic and blocks until a message ANSWERING that
// publish arrives, all over one Hello'd connection. JSON-Lines output.
//
// WHERE the reply goes, and where this waits: its own chat.<short-id> by
// default. --reply-to changes BOTH at once — it rides on the publish as
// reply_to_topic, so the server routes the reply there (resolveReplyTarget in
// server/agent_handler.go reads it off the parent), and it is what this call
// waits on.
//
// ONE flag sets both deliberately. The removed --reply-topic set only the wait
// and told the peer nothing, so a peer answering with --in-reply-to had its
// reply routed to the caller's OWN chat.<short-id> — the inbox a caller names a
// reply topic to keep clean — while this call waited out its timeout somewhere
// else. Splitting the two is the failure, not the feature.
//
// The peer needs no change and no knowledge: it answers with --in-reply-to
// alone and the destination comes off the parent, server-side.
//
// WHAT satisfies it, on either topic: a message above the seq this call
// published AND carrying that seq as its in_reply_to. Before, it waited from
// Since:0 with no correlation, so every message already retained on the named
// topic satisfied it — including an answer to somebody else's question.
//
// The correlation costs nothing on the --reply-to path: a reply reaches a
// declared destination BY the server resolving in_reply_to against the parent,
// so one without in_reply_to could not arrive there in the first place.
//
// replyDeadlineMargin is the slice of the budget reserved for the server's
// timed-out WaitResponse to travel back before the local deadline fires. It
// buys the caller a message naming what happened ("dispatch reply timeout")
// instead of a bare context error, which on a timeout is the whole content of
// the answer.
const replyDeadlineMargin = 500 * time.Millisecond

// deadlineOf returns ctx's deadline, or now + a long fallback when it has
// none. Dispatch always sets one, so the fallback is unreachable there; it
// exists so this helper cannot silently produce a negative budget if a future
// caller passes a deadline-less context.
func deadlineOf(ctx context.Context) time.Time {
	if d, ok := ctx.Deadline(); ok {
		return d
	}
	return time.Now().Add(5 * time.Minute)
}

// --timeout bounds the WHOLE call, not just the reply. It used to bound only
// the reply: the server-side wait got it as WaitRequest.TimeoutMs while the
// publish-acknowledgement wait above had no deadline of its own and ran until
// the process context died. `--timeout 8s` could therefore return well after
// 8s, which is not something a caller can infer from a flag named "timeout".
//
// This is a shell-level tool for scripting OUTSIDE an agent's turn loop, for
// the same reason `agent wait` is.
func Dispatch(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	// Same declaration as `agent send`: these two verbs publish to one board
	// through one flag surface, so a positional that means the body on one and
	// nothing on the other is a trap. `dispatch --topic T 'question'` used to
	// ignore the word and block on a stdin nobody was writing to.
	sp, _ := verb.Lookup("agent", "dispatch")
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
	return DispatchWith(ctx, a, stdin, stdout)
}

// DispatchWith is Dispatch for a caller that already has the parsed action --
// the generated CLI dispatch, which parses from the declaration itself.
func DispatchWith(ctx context.Context, a verb.AgentSendAction, stdin io.Reader, stdout io.Writer) error {
	serverCID, topic, replyTo := &a.ServerCID, &a.Topic, &a.ReplyTo
	data, timeout := &a.Data, &a.Timeout
	if *topic == "" {
		return errors.New("--topic required")
	}

	// One deadline for everything below, so the flag means what it says.
	ctx, cancelTimeout := context.WithTimeout(ctx, *timeout)
	defer cancelTimeout()

	// Same resolver `agent send` uses. These two verbs publish to one board
	// through one identical flag surface, so a positional that means the body
	// on one and nothing at all on the other is a trap, not a distinction:
	// `dispatch --topic T 'question'` used to ignore the word and block on a
	// stdin nobody was writing to.
	payload, source, err := resolvePayloadFrom(a.DataSet, *data, a.Positional, stdin)
	if err != nil {
		return err
	}

	if err := refuseIfOwnTicket(payload); err != nil {
		return err
	}

	selfTid, err := cliopts.ResolveTaskID("")
	if err != nil {
		return err
	}
	c, err := connectClient(ctx, *serverCID)
	if err != nil {
		return err
	}
	defer c.Close()

	// Send: the payload travels on a client-initiated send-stream (UDP MTU).
	sendStream := c.Transport().CreateSendStream()
	if sendStream == nil {
		return errors.New("agent: failed to allocate payload stream")
	}
	sr := protocol.AgentSendRequest{PayloadStreamId: uint64(sendStream.ID())}
	sr.SetTopic([]byte(*topic))
	if *replyTo != "" {
		if !sr.SetReplyToTopic([]byte(*replyTo)) {
			return errors.New("agent: --reply-to too long")
		}
	}
	sendReq := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AgentSend}
	sendReq.SetAgentSend(sr)
	// Announce BEFORE writing the body, as `agent send` does and for its
	// reason: the request names the stream, so until the server has read it
	// nobody drains that stream and a body past the peer's receive window
	// blocks forever. This path used to write the whole body first, which is
	// the same deadlock waiting for a large enough payload.
	sendCh, berr := c.BeginTaskControl(sendReq)
	if berr != nil {
		return berr
	}
	if werr := sendStream.AppendDataContext(ctx, false, payload); werr != nil {
		return fmt.Errorf("agent: payload stream write: %w", werr)
	}
	if werr := sendStream.AppendDataContext(ctx, true); werr != nil {
		return fmt.Errorf("agent: payload stream EOF: %w", werr)
	}

	var publishedSeq uint64
	select {
	case res := <-sendCh:
		if res.Err != nil {
			return res.Err
		}
		if kerr := expectKind(res.Resp, protocol.TaskControlKind_AgentSend); kerr != nil {
			return kerr
		}
		r := res.Resp.AgentSend()
		if r == nil {
			return errors.New("agent: send response variant is nil")
		}
		if r.Status != protocol.SendStatus_Ok {
			return fmt.Errorf("send failed: %v (%d bytes from %s)", r.Status, len(payload), source)
		}
		publishedSeq = r.Seq
		// What went out, before this blocks for an answer. stdout here is the
		// REPLY stream (JSON-Lines), so the summary goes to stderr the way
		// `session send`'s does — and it has to be printed BEFORE the wait,
		// because a body that went out empty or truncated otherwise shows up
		// only as a timeout minutes later, which names nothing.
		fmt.Fprintf(os.Stderr, "agent dispatch: published %d bytes from %s as seq %d, delivered_to %d\n",
			len(payload), source, r.Seq, r.DeliveredTo)
	case <-ctx.Done():
		return ctx.Err()
	}

	// The server-side wait gets what is LEFT of the budget after the publish
	// round trip, minus a margin. The margin is not padding: without it the
	// server's timed-out answer and the local deadline fire together, and the
	// caller gets `context deadline exceeded` instead of the message that says
	// what actually happened. Reserving it means the friendly error wins the
	// race while `--timeout` stays the hard bound on the call.
	remaining := time.Until(deadlineOf(ctx)) - replyDeadlineMargin
	if remaining <= 0 {
		return errors.New("dispatch reply timeout")
	}

	// Wait for the reply on OUR own topic — where the server routes it — above
	// our own publish, and only for messages answering that seq.
	wr := protocol.AgentWaitRequest{
		Since:     publishedSeq,
		TimeoutMs: uint32(remaining.Milliseconds()),
		InReplyTo: publishedSeq,
	}
	// Wait where we told the peer's reply to go. ONE flag sets both halves on
	// purpose: the removed --reply-topic set only the wait, so a peer answering
	// with --in-reply-to had its reply routed to our own inbox while this call
	// waited out its timeout somewhere else.
	waitOn := SelfTopic(selfTid)
	if *replyTo != "" {
		waitOn = *replyTo
	}
	wr.SetPattern([]byte(waitOn))
	waitReq := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AgentWait}
	waitReq.SetAgentWait(wr)

	waitResp, err := c.RoundTripTaskControl(ctx, waitReq)
	if err != nil {
		return err
	}
	if err := expectKind(waitResp, protocol.TaskControlKind_AgentWait); err != nil {
		return err
	}
	r := waitResp.AgentWait()
	if r == nil {
		return errors.New("agent: wait response variant is nil")
	}
	for _, m := range r.Msgs {
		body, perr := fetchDeliveredPayload(ctx, c.Transport(), m.PayloadStreamId)
		if perr != nil {
			return fmt.Errorf("fetch payload seq=%d: %w", m.Seq, perr)
		}
		emitMessageLine(stdout, m, body)
	}
	if r.TimedOut == 1 && len(r.Msgs) == 0 {
		return errors.New("dispatch reply timeout")
	}
	return nil
}
