package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Inbox returns the JSON-Lines dump of messages on subscribed topics.
//
// Two reads exist, and the flags pick between them:
//
//   - plain (the default): every retained message above --since, which defaults
//     to 0 — the whole ring. Idempotent; it moves nothing. This is what an
//     agent runs by hand, and what a runtime with no UserPromptSubmit hook
//     (codex, bash, …) polls.
//   - advancing (--user-prompt-submit-hook): the messages the automatic
//     injection path has not yet been given, marked as given by the SERVER in
//     the same operation. Only the runner-injected hook sends it — see
//     runner/settings.go.
//
// There is deliberately no flag that advances without also producing the hook
// envelope. The position used to be a client-side cursor file driven by a
// --since-last/--commit pair, and "never pass --commit by hand" was an
// instruction in a skill file; now advancing requires claiming to be the one
// hook the runner installs, whose output is an envelope no human reads.
//
// --in-reply-to filters the emitted records to replies to that seq. It is
// presentational only: the advancing read still marks every message the server
// returned, so a filtered run does not re-deliver what it hid.
func Inbox(ctx context.Context, args []string, stdout io.Writer) error {
	a, perr := parseAgentVerb("inbox", args)
	if perr != nil {
		return perr
	}
	return InboxWith(ctx, a, stdout)
}

// InboxWith is Inbox for a caller that already has the parsed action --
// the generated CLI dispatch, which parses from the declaration itself.
func InboxWith(ctx context.Context, a verb.AgentAction, stdout io.Writer) error {
	serverCID := &a.ServerCID
	since := &a.Since
	asJSON := &a.JSON
	promptHook := &a.UserPromptSubmitHook
	inReplyTo := &a.InReplyTo
	_ = asJSON // currently always JSON Lines

	c, err := connectClient(ctx, *serverCID)
	if err != nil {
		return err
	}
	defer c.Close()

	// The advancing read is its own KIND, not a flag on the plain one. Only the
	// runner-injected hook may send it, and a kind selected by
	// --user-prompt-submit-hook — whose output is a hook envelope — makes that
	// the shape of the CLI rather than an instruction in a skill file.
	kind := protocol.TaskControlKind_AgentInbox
	req := &protocol.TaskControlRequest{Kind: kind}
	if *promptHook {
		kind = protocol.TaskControlKind_AgentInboxAdvance
		req.Kind = kind
		req.SetAgentInboxAdvance(protocol.AgentInboxAdvanceRequest{})
	} else {
		req.SetAgentInbox(protocol.AgentInboxRequest{Since: *since})
	}

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return err
	}
	if err := expectKind(resp, kind); err != nil {
		return err
	}
	// Both answers carry the same row type under their own union field, so the
	// accessor has to follow the kind that was asked for.
	var msgs []protocol.DeliveredMessage
	if *promptHook {
		r := resp.AgentInboxAdvance()
		if r == nil {
			return fmt.Errorf("inbox: advance response variant is nil")
		}
		msgs = r.Msgs
	} else {
		r := resp.AgentInbox()
		if r == nil {
			return fmt.Errorf("inbox: response variant is nil")
		}
		msgs = r.Msgs
	}

	// Fetch all payloads up front so a write-time decode error doesn't leave
	// half the inbox emitted.
	payloads := make([][]byte, len(msgs))
	for i, m := range msgs {
		p, perr := fetchDeliveredPayload(ctx, c.Transport(), m.PayloadStreamId)
		if perr != nil {
			return fmt.Errorf("fetch payload seq=%d: %w", m.Seq, perr)
		}
		payloads[i] = p
	}
	// Only the hook mode gets the inline guard: its output is spliced into the
	// agent's next prompt, so an oversize body is context the agent never agreed
	// to spend. A plain read hands the record to a caller that can redirect it,
	// and `agent read <seq>` is where the guarded record points for the full
	// body.
	emit := emitMessageLine
	if *promptHook {
		emit = emitMessageLineForHook
	}
	var body bytes.Buffer
	for i, m := range msgs {
		if *inReplyTo != 0 && m.InReplyTo != *inReplyTo {
			continue
		}
		emit(&body, m, payloads[i])
	}
	if *promptHook {
		emitUserPromptSubmitHookOutput(stdout, body.String())
		return nil
	}
	if _, err := stdout.Write(body.Bytes()); err != nil {
		return err
	}
	return nil
}
