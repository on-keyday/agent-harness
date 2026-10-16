package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/cliopts"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// connectClient dials the server as an ordinary task-control client.
//
// The agent verbs used to open their own connection (ConnectAgent) because
// they spoke a different frame family and had to own its demux. They do not
// any more, and no handshake is lost with it: cli.Dial runs the same merged
// PSK+identity exchange, and buildMergedClientHello upgrades the announced
// kind to Agent with AgentInfo whenever HARNESS_TASK_ID / HARNESS_RUNNER_ID /
// HARNESS_AUTH_TICKET are populated — which, for a verb that only works
// inside a task, is always.
//
// ClientKind_Cli is what ConnectAgent announced too. It is the fallback the
// hello uses when the agent env is absent, not a claim to be an operator.
func connectClient(ctx context.Context, serverCID string) (*cli.Client, error) {
	cid, err := cliopts.ResolveServerCID(serverCID)
	if err != nil {
		return nil, err
	}
	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err != nil {
		return nil, fmt.Errorf("agent dial: %w", err)
	}
	return c, nil
}

// expectKind checks a response envelope before its variant is read.
//
// It does NOT handle a permission denial, and that absence is the point: both
// RoundTripTaskControl and BeginTaskControl already turn one into a
// *cli.CapabilityDeniedError, which names the capability through CapsLabel —
// the single renderer every other surface uses. A denial therefore never
// reaches here, and no caller in this package spells a capability out. Two
// used to (`"board_observe"` in topics.go, `"purge"` in purge.go), each with
// wording the operator surface did not share.
func expectKind(resp *protocol.TaskControlResponse, want protocol.TaskControlKind) error {
	if resp == nil {
		return errors.New("agent: empty response")
	}
	if resp.Kind != want {
		return fmt.Errorf("agent: unexpected response kind=%v (want %v)", resp.Kind, want)
	}
	return nil
}

// readDelivered reads the messages wait / inbox / inbox_advance / read_seq
// return. They arrive as one DeliveredBody on the stream the response names,
// bodies inside their rows.
func readDelivered(ctx context.Context, c *cli.Client, streamID uint64) ([]protocol.DeliveredMessage, error) {
	var body protocol.DeliveredBody
	if err := cli.ReadResponseBody(ctx, c.Transport(), streamID, &body); err != nil {
		return nil, err
	}
	return body.Msgs, nil
}
