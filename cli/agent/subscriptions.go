package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/cli/verb"
)

// Subscriptions fetches the calling task's subscription pattern list and emits
// one JSON Lines record per subscription.
func Subscriptions(ctx context.Context, args []string, stdout io.Writer) error {
	a, perr := parseAgentVerb("subscriptions", args)
	if perr != nil {
		return perr
	}
	return SubscriptionsWith(ctx, a, stdout)
}

// SubscriptionsWith is Subscriptions for a caller that already has the parsed action --
// the generated CLI dispatch, which parses from the declaration itself.
func SubscriptionsWith(ctx context.Context, a verb.AgentAction, stdout io.Writer) error {
	serverCID := &a.ServerCID

	conn, err := ConnectAgent(ctx, Flags{
		ServerCID: *serverCID,
	})
	if err != nil {
		return err
	}
	defer conn.Close()

	reqID := rand.Uint32()
	respCh := make(chan agentboard.ListSubscriptionsResponse, 1)
	conn.SetOnControl(func(kind appwire.AppKind, p []byte) {
		if kind != appwire.AppKind_AgentMessage {
			return
		}
		msg := &agentboard.AgentMessage{}
		if _, err := msg.Decode(p); err != nil {
			return
		}
		if msg.Kind == agentboard.AgentMessageKind_ListSubscriptionsResponse {
			r := msg.ListSubscriptionsResponse()
			if r != nil && r.RequestId == reqID {
				select {
				case respCh <- *r:
				default:
				}
			}
		}
	})

	msg := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_ListSubscriptions}
	msg.SetListSubscriptions(agentboard.ListSubscriptionsRequest{RequestId: reqID})
	if err := conn.SendRaw(msg); err != nil {
		return err
	}

	select {
	case r := <-respCh:
		for _, s := range r.Subscriptions {
			rec := map[string]any{"pattern": string(s.Pattern)}
			line, _ := json.Marshal(rec)
			fmt.Fprintln(stdout, string(line))
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
