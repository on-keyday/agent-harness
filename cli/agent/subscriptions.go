package agent

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// Subscriptions fetches the calling task's subscription pattern list and emits
// one JSON Lines record per subscription.
func Subscriptions(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("agent subscriptions", flag.ContinueOnError)
	serverCID := fs.String("server-cid", "", "")
	taskID := fs.String("task-id", "", "")
	runnerID := fs.String("runner-id", "", "")
	if err := fs.Parse(args); err != nil {
		return err
	}

	conn, err := ConnectAgent(ctx, Flags{
		ServerCID: *serverCID,
		TaskID:    *taskID,
		RunnerID:  *runnerID,
	})
	if err != nil {
		return err
	}
	defer conn.Close()

	reqID := rand.Uint32()
	respCh := make(chan agentboard.ListSubscriptionsResponse, 1)
	conn.SetOnControl(func(kind wire.ApplicationPayloadKind, p []byte) {
		if kind != wire.ApplicationPayloadKind_AgentMessage {
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
