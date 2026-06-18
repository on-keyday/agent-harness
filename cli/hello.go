package cli

import (
	"context"
	"fmt"

	"github.com/on-keyday/agent-harness/cli/cliopts"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// sendClientHello performs the ClientHello round-trip with the given hello
// value and checks the server's response status.
func (c *Client) sendClientHello(ctx context.Context, hello protocol.ClientHello) error {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_ClientHello}
	req.SetClientHello(hello)

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return fmt.Errorf("client hello: %w", err)
	}
	r := resp.ClientHello()
	if resp.Kind != protocol.TaskControlKind_ClientHello || r == nil {
		return fmt.Errorf("client hello: unexpected response kind=%v", resp.Kind)
	}
	if r.Status != protocol.ClientHelloStatus_Ok {
		return fmt.Errorf("client hello: server returned status=%v", r.Status)
	}
	return nil
}

// SayHello announces this client's kind to the server immediately after Dial,
// so server-side observability (slog) can attribute connections to cli / tui /
// webui processes. Long-lived clients (tui, wasm) should call SayHello once
// per process, right after Dial and before any other RPC.
//
// Operator surfaces (tui, wasm) call SayHello directly with their specific
// kind. In-task harness-cli subcommands (submit, interactive, file-transfer,
// port-forward) call SayHelloAuto, which auto-selects kind=agent when the
// agent env (HARNESS_RUNNER_ID / HARNESS_TASK_ID / HARNESS_AUTH_TICKET) is
// present, or the given operator kind otherwise.
//
// Returns an error if the round-trip fails, the response kind is not
// ClientHello, the variant is missing, or the status is not Ok.
func (c *Client) SayHello(ctx context.Context, kind protocol.ClientKind) error {
	return c.sendClientHello(ctx, protocol.ClientHello{Kind: kind})
}

// SayHelloAuto sends a ClientHello. When the in-task agent env (HARNESS_RUNNER_ID
// / HARNESS_TASK_ID / HARNESS_AUTH_TICKET) is present it announces kind=agent
// with the credential so the server can attribute and verify the principal;
// otherwise it announces the given operator kind (cli/tui/webui). Reuses the same
// env resolution as the agentboard client (cli/cliopts).
func (c *Client) SayHelloAuto(ctx context.Context, operatorKind protocol.ClientKind) error {
	hello := protocol.ClientHello{Kind: operatorKind}
	if rid, err := cliopts.ResolveRunnerID(""); err == nil {
		if tid, err := cliopts.ResolveTaskID(""); err == nil {
			if ticket, err := cliopts.ResolveAuthTicket(); err == nil {
				info := protocol.AgentInfo{RunnerId: rid, TaskId: tid, AuthTicket: ticket}
				info.SetHostname([]byte(cliopts.ResolveString("", "HARNESS_HOSTNAME")))
				hello.Kind = protocol.ClientKind_Agent
				hello.SetAgentInfo(info)
			}
		}
	}
	return c.sendClientHello(ctx, hello)
}
