package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
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
	c, err := connectClient(ctx, a.ServerCID)
	if err != nil {
		return err
	}
	defer c.Close()

	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AgentListSubscriptions}
	req.SetAgentListSubscriptions(protocol.AgentListSubscriptionsRequest{})

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return err
	}
	if err := expectKind(resp, protocol.TaskControlKind_AgentListSubscriptions); err != nil {
		return err
	}
	r := resp.AgentListSubscriptions()
	if r == nil {
		return errors.New("agent: subscriptions response variant is nil")
	}
	var body protocol.SubscriptionsBody
	if err := cli.ReadResponseBody(ctx, c.Transport(), r.StreamId, &body); err != nil {
		return fmt.Errorf("subscriptions: %w", err)
	}
	for _, s := range body.Subscriptions {
		rec := map[string]any{"pattern": string(s.Pattern), "joined": s.Joined()}
		line, _ := json.Marshal(rec)
		fmt.Fprintln(stdout, string(line))
	}
	return nil
}
