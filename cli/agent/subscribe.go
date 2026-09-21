package agent

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/on-keyday/agent-harness/cli/cliopts"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func subscribeOrUnsub(ctx context.Context, args []string, stdout io.Writer, remove bool) error {
	sub := "subscribe"
	if remove {
		sub = "unsubscribe"
	}
	// The --self / --topic exclusion lives in the verb's Build now, so both
	// spellings of this one parser get it from the same place.
	a, perr := parseAgentVerb(sub, args)
	if perr != nil {
		return perr
	}
	return subscribeOrUnsubWith(ctx, a, stdout, remove)
}

// subscribeOrUnsubWith is subscribeOrUnsub for a caller that already has the
// parsed action -- the generated CLI dispatch.
func subscribeOrUnsubWith(ctx context.Context, a verb.AgentAction, stdout io.Writer, remove bool) error {
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

	c, err := connectClient(ctx, *serverCID)
	if err != nil {
		return err
	}
	defer c.Close()

	kind := protocol.TaskControlKind_AgentSubscribe
	req := &protocol.TaskControlRequest{Kind: kind}
	if remove {
		kind = protocol.TaskControlKind_AgentUnsubscribe
		req.Kind = kind
		ur := protocol.AgentUnsubscribeRequest{}
		ur.SetPattern([]byte(*pattern))
		req.SetAgentUnsubscribe(ur)
	} else {
		sr := protocol.AgentSubscribeRequest{}
		sr.SetPattern([]byte(*pattern))
		req.SetAgentSubscribe(sr)
	}

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return err
	}
	if err := expectKind(resp, kind); err != nil {
		return err
	}
	// Same format, different union field per kind — read the one that matches
	// the kind asked for, or the variant accessor returns nil.
	r := resp.AgentSubscribe()
	if remove {
		r = resp.AgentUnsubscribe()
	}
	if r == nil {
		return errors.New("agent: subscribe response variant is nil")
	}
	if r.Status != protocol.SubscribeStatus_Ok {
		return fmt.Errorf("subscribe failed: %v", r.Status)
	}
	fmt.Fprintln(stdout, `{"status":"ok"}`)
	return nil
}

// Subscribe is the entry for `harness-cli agent subscribe`.
func Subscribe(ctx context.Context, args []string, stdout io.Writer) error {
	return subscribeOrUnsub(ctx, args, stdout, false)
}

// Unsubscribe is the entry for `harness-cli agent unsubscribe`.
func Unsubscribe(ctx context.Context, args []string, stdout io.Writer) error {
	return subscribeOrUnsub(ctx, args, stdout, true)
}

// SubscribeWith and UnsubscribeWith are Subscribe / Unsubscribe for a caller
// that already has the parsed action -- the generated CLI dispatch.
func SubscribeWith(ctx context.Context, a verb.AgentAction, stdout io.Writer) error {
	return subscribeOrUnsubWith(ctx, a, stdout, false)
}

func UnsubscribeWith(ctx context.Context, a verb.AgentAction, stdout io.Writer) error {
	return subscribeOrUnsubWith(ctx, a, stdout, true)
}
