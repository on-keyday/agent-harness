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
// It answers a permission denial FIRST, and by the capability's own name off
// the wire: PermissionDeniedResponse carries required_cap, so no caller here
// spells a capability out. Two verbs used to, and the two strings they held
// differed from the operator surface's wording for the same refusal.
func expectKind(resp *protocol.TaskControlResponse, want protocol.TaskControlKind) error {
	if resp == nil {
		return errors.New("agent: empty response")
	}
	if resp.Kind == protocol.TaskControlKind_PermissionDenied {
		return permissionDenied(resp.PermissionDenied())
	}
	if resp.Kind != want {
		return fmt.Errorf("agent: unexpected response kind=%v (want %v)", resp.Kind, want)
	}
	return nil
}

// permissionDenied renders a refusal naming the capability the server asked
// for. The wording matches the operator surface's, because it is now the same
// response format answering both.
func permissionDenied(pd *protocol.PermissionDeniedResponse) error {
	if pd == nil {
		return errors.New("permission denied")
	}
	return fmt.Errorf("permission denied: %v requires capability %v", pd.RequestedKind, pd.RequiredCap)
}
