package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/cli/verb"
)

// Wait blocks until a matching message arrives on the given topic, or until
// --timeout. Output: JSON Lines on stdout, one line per delivered message.
//
// This is a shell-level tool for scripting OUTSIDE an agent's turn loop. An
// agent must not call it from inside a turn: it holds the process for the whole
// timeout, during which it can neither reason nor send to anyone else, and
// replies arrive through the inbox hook regardless.
//
// It is "take everything after the cursor, block only if there is nothing" —
// NOT "wait for the next message". Board.Wait scans the retained ring before it
// blocks, so anything already there above --since returns at once, as a batch.
// With --since omitted the cursor is 0 and the whole ring is above it: on a
// topic that already holds messages this returns instantly with old ones.
//
// A caller meaning "what happens NEXT" must therefore pass --since. Omitting it
// does not look like a mistake — something is returned, so the call reads as a
// successful wait for a message that actually predates it.
//
// --since is the CALLER's own resume position (the previous response's
// next_cursor). Nothing is persisted. It used to have a --since-last that read
// and wrote the hook's cursor file, which was a board-global watermark over
// every subscribed topic: advancing it from the max seq of the ONE topic being
// waited on skipped unread messages on the others, and the hook never delivered
// them.
//
// --in-reply-to narrows the wait to the answer to one seq; a non-matching
// publish on the topic does not end it.
func Wait(ctx context.Context, args []string, stdout io.Writer) error {
	a, perr := parseAgentVerb("wait", args)
	if perr != nil {
		return perr
	}
	return WaitWith(ctx, a, stdout)
}

// WaitWith is Wait for a caller that already has the parsed action --
// the generated CLI dispatch, which parses from the declaration itself.
func WaitWith(ctx context.Context, a verb.AgentAction, stdout io.Writer) error {
	serverCID := &a.ServerCID
	topic := &a.Topic
	since := &a.Since
	inReplyTo := &a.InReplyTo
	timeout := &a.Timeout
	if *topic == "" {
		return errors.New("--topic required")
	}

	conn, err := ConnectAgent(ctx, Flags{
		ServerCID: *serverCID,
	})
	if err != nil {
		return err
	}
	defer conn.Close()

	reqID := rand.Uint32()
	respCh := make(chan agentboard.WaitResponse, 1)
	conn.SetOnControl(func(kind appwire.AppKind, p []byte) {
		if kind != appwire.AppKind_AgentMessage {
			return
		}
		msg := &agentboard.AgentMessage{}
		if _, err := msg.Decode(p); err != nil {
			return
		}
		if msg.Kind == agentboard.AgentMessageKind_WaitResponse {
			r := msg.WaitResponse()
			if r != nil && r.RequestId == reqID {
				select {
				case respCh <- *r:
				default:
				}
			}
		}
	})

	wr := agentboard.WaitRequest{
		RequestId: reqID,
		Since:     *since,
		TimeoutMs: uint32(timeout.Milliseconds()),
		InReplyTo: *inReplyTo,
	}
	wr.SetPattern([]byte(*topic))

	waitMsg := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_Wait}
	if !waitMsg.SetWait(wr) {
		return errors.New("agent: SetWait failed")
	}
	if err := conn.SendRaw(waitMsg); err != nil {
		return err
	}

	select {
	case r := <-respCh:
		for _, m := range r.Msgs {
			payload, perr := conn.FetchDeliveredPayload(ctx, m.PayloadStreamId)
			if perr != nil {
				return fmt.Errorf("fetch payload seq=%d: %w", m.Seq, perr)
			}
			emitMessageLine(stdout, m, payload)
		}
		if r.TimedOut == 1 && len(r.Msgs) == 0 {
			return errors.New("timeout")
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
