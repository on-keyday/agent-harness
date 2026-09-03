package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/cli/cliopts"
	"github.com/on-keyday/agent-harness/cli/verb"
)

func subscribeOrUnsub(ctx context.Context, args []string, stdout io.Writer, kind agentboard.AgentMessageKind) error {
	sub := "subscribe"
	if kind == agentboard.AgentMessageKind_Unsubscribe {
		sub = "unsubscribe"
	}
	// The --self / --topic exclusion lives in the verb's Build now, so both
	// spellings of this one parser get it from the same place.
	a, perr := parseAgentVerb(sub, args)
	if perr != nil {
		return perr
	}
	return subscribeOrUnsubWith(ctx, a, stdout, kind)
}

// subscribeOrUnsubWith is subscribeOrUnsub for a caller that already has the
// parsed action -- the generated CLI dispatch.
func subscribeOrUnsubWith(ctx context.Context, a verb.AgentAction, stdout io.Writer, kind agentboard.AgentMessageKind) error {
	serverCID, pattern, self := &a.ServerCID, &a.Topic, &a.Self
	if *self {
		tid, err := cliopts.ResolveTaskID("")
		if err != nil {
			return err
		}
		t := SelfTopic(tid)
		pattern = &t
	}
	if *pattern == "" {
		return errors.New("--topic or --self required")
	}

	conn, err := ConnectAgent(ctx, Flags{
		ServerCID: *serverCID,
	})
	if err != nil {
		return err
	}
	defer conn.Close()

	reqID := rand.Uint32()
	respCh := make(chan agentboard.SubscribeResponse, 1)
	conn.SetOnControl(func(k appwire.AppKind, p []byte) {
		if k != appwire.AppKind_AgentMessage {
			return
		}
		msg := &agentboard.AgentMessage{}
		if _, err := msg.Decode(p); err != nil {
			return
		}
		if msg.Kind == agentboard.AgentMessageKind_SubscribeResponse {
			r := msg.SubscribeResponse()
			if r != nil && r.RequestId == reqID {
				select {
				case respCh <- *r:
				default:
				}
			}
		}
	})

	msg := &agentboard.AgentMessage{Kind: kind}
	if kind == agentboard.AgentMessageKind_Subscribe {
		req := agentboard.SubscribeRequest{RequestId: reqID}
		req.SetPattern([]byte(*pattern))
		msg.SetSubscribe(req)
	} else {
		req := agentboard.UnsubscribeRequest{RequestId: reqID}
		req.SetPattern([]byte(*pattern))
		msg.SetUnsubscribe(req)
	}
	if err := conn.SendRaw(msg); err != nil {
		return err
	}

	select {
	case r := <-respCh:
		if r.Status != agentboard.SubscribeStatus_Ok {
			return fmt.Errorf("subscribe failed: %v", r.Status)
		}
		fmt.Fprintln(stdout, `{"status":"ok"}`)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Subscribe is the entry for `harness-cli agent subscribe`.
func Subscribe(ctx context.Context, args []string, stdout io.Writer) error {
	return subscribeOrUnsub(ctx, args, stdout, agentboard.AgentMessageKind_Subscribe)
}

// Unsubscribe is the entry for `harness-cli agent unsubscribe`.
func Unsubscribe(ctx context.Context, args []string, stdout io.Writer) error {
	return subscribeOrUnsub(ctx, args, stdout, agentboard.AgentMessageKind_Unsubscribe)
}

// SubscribeWith and UnsubscribeWith are Subscribe / Unsubscribe for a caller
// that already has the parsed action -- the generated CLI dispatch.
func SubscribeWith(ctx context.Context, a verb.AgentAction, stdout io.Writer) error {
	return subscribeOrUnsubWith(ctx, a, stdout, agentboard.AgentMessageKind_Subscribe)
}

func UnsubscribeWith(ctx context.Context, a verb.AgentAction, stdout io.Writer) error {
	return subscribeOrUnsubWith(ctx, a, stdout, agentboard.AgentMessageKind_Unsubscribe)
}
