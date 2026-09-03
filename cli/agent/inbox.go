package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/appwire"
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
	serverCID := &a.ServerCID
	since := &a.Since
	asJSON := &a.JSON
	promptHook := &a.UserPromptSubmitHook
	inReplyTo := &a.InReplyTo
	_ = asJSON // currently always JSON Lines

	conn, err := ConnectAgent(ctx, Flags{
		ServerCID: *serverCID,
	})
	if err != nil {
		return err
	}
	defer conn.Close()

	reqID := rand.Uint32()
	// Both response kinds carry the same body, so one channel of messages
	// serves either read; which one arrives is decided by what was sent.
	respCh := make(chan []agentboard.DeliveredMessage, 1)
	conn.SetOnControl(func(kind appwire.AppKind, p []byte) {
		if kind != appwire.AppKind_AgentMessage {
			return
		}
		msg := &agentboard.AgentMessage{}
		if _, err := msg.Decode(p); err != nil {
			return
		}
		switch msg.Kind {
		case agentboard.AgentMessageKind_InboxResponse:
			if r := msg.InboxResponse(); r != nil && r.RequestId == reqID {
				select {
				case respCh <- r.Msgs:
				default:
				}
			}
		case agentboard.AgentMessageKind_InboxAdvanceResponse:
			if r := msg.InboxAdvanceResponse(); r != nil && r.RequestId == reqID {
				select {
				case respCh <- r.Msgs:
				default:
				}
			}
		}
	})

	var msg *agentboard.AgentMessage
	if *promptHook {
		msg = &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_InboxAdvance}
		msg.SetInboxAdvance(agentboard.InboxAdvanceRequest{RequestId: reqID})
	} else {
		msg = &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_Inbox}
		msg.SetInbox(agentboard.InboxRequest{RequestId: reqID, Since: *since})
	}
	if err := conn.SendRaw(msg); err != nil {
		return err
	}

	select {
	case msgs := <-respCh:
		// Fetch all payloads up front so a write-time decode error doesn't
		// leave half the inbox emitted.
		payloads := make([][]byte, len(msgs))
		for i, m := range msgs {
			p, perr := conn.FetchDeliveredPayload(ctx, m.PayloadStreamId)
			if perr != nil {
				return fmt.Errorf("fetch payload seq=%d: %w", m.Seq, perr)
			}
			payloads[i] = p
		}
		// Only the hook mode gets the inline guard: its output is spliced into
		// the agent's next prompt, so an oversize body is context the agent
		// never agreed to spend. A plain read hands the record to a caller that
		// can redirect it, and `agent read <seq>` is where the guarded record
		// points for the full body.
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
	case <-ctx.Done():
		return ctx.Err()
	}
}
