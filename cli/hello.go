package cli

import (
	"context"
	"fmt"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// SayHello announces this client's kind to the server immediately after Dial,
// so server-side observability (slog) can attribute connections to cli / tui /
// webui processes. Long-lived clients (tui, wasm) should call SayHello once
// per process, right after Dial and before any other RPC.
//
// Short-lived per-call dial-close consumers (harness-cli subcommands) do not
// call SayHello: each invocation would log a fresh "client hello" line per
// subcommand, which is noisy for what is supposed to be a quiet observability
// signal. The per-call cost is also wasted for processes that immediately
// tear the connection down.
//
// Returns an error if the round-trip fails, the response kind is not
// ClientHello, the variant is missing, or the status is not Ok.
func (c *Client) SayHello(ctx context.Context, kind protocol.ClientKind) error {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_ClientHello}
	hello := protocol.ClientHello{Kind: kind}
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
