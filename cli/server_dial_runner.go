package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// taskControlClient is the minimal interface ServerDialRunnerWith needs
// from a TaskControl-capable peer. Satisfied by *Client; mocked in tests.
//
// We deliberately reuse the existing RoundTripTaskControl signature rather
// than introducing a new SendTaskControlRequest wrapper: the existing
// per-request-id correlator already lives there, and Submit/List/Cancel
// all funnel through it. Defining a fresh interface (instead of leaning on
// a shared one in client.go) keeps the test surface tiny and avoids
// coupling this helper to the full *Client API.
type taskControlClient interface {
	RoundTripTaskControl(ctx context.Context, req *protocol.TaskControlRequest) (*protocol.TaskControlResponse, error)
}

// ServerDialRunner is the high-level entry used by
//
//	harness-cli server dial-runner <runner-cid>
//
// It dials the server, sends a DialRunnerRequest and returns the server's
// decoded response. The target is an ADDRESS (the runner is not registered yet
// — the server dials it); via names a REGISTERED proxy runner, so it is an
// identity and the server resolves it.
//
// Short-lived processes (harness-cli) should use this form; long-lived
// embedders that already hold a *Client should call ServerDialRunnerWith
// directly to skip the redundant Dial/Close.
// ParseDialVia parses the optional --via value into a proxy-runner IDENTITY.
// Empty means "no relay" and yields the zero RunnerID, which is what the server
// reads as Phase A direct.
//
// It exists so the CLI, the TUI and the wasm bridge share one parse: --via used
// to take a connection id, all three hand-rolled the same ParseConnectionID
// call, and a surface that kept doing so would silently accept a string the
// server can no longer resolve.
func ParseDialVia(s string) (protocol.RunnerID, error) {
	if strings.TrimSpace(s) == "" {
		return protocol.RunnerID{}, nil
	}
	rid, err := protocol.RunnerIDFromHex(strings.TrimSpace(s))
	if err != nil {
		return protocol.RunnerID{}, fmt.Errorf("--via wants a 32-hex runner id (the id= column of `ls`): %w", err)
	}
	return rid, nil
}

func ServerDialRunner(ctx context.Context, serverCID objproto.ConnectionID, targetCID objproto.ConnectionID, via protocol.RunnerID) (protocol.DialRunnerResponse, error) {
	c, err := Dial(ctx, serverCID, protocol.ClientKind_Cli)
	if err != nil {
		return protocol.DialRunnerResponse{}, fmt.Errorf("dial server: %w", err)
	}
	defer c.Close()
	return ServerDialRunnerWith(ctx, c, protocol.ConnIDFromObjproto(targetCID), via)
}

// ServerDialRunnerWith is the lower-level form that operates on an already-
// connected taskControlClient. Exposed for callers that hold a long-lived
// *Client and want to avoid re-dialing.
func ServerDialRunnerWith(ctx context.Context, c taskControlClient, target protocol.ConnID, via protocol.RunnerID) (protocol.DialRunnerResponse, error) {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_DialRunner}
	req.SetDialRunner(protocol.DialRunnerRequest{Target: target, Via: via})

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return protocol.DialRunnerResponse{}, err
	}
	if resp.Kind != protocol.TaskControlKind_DialRunner {
		return protocol.DialRunnerResponse{}, fmt.Errorf("unexpected response kind: %v (want DialRunner)", resp.Kind)
	}
	dr := resp.DialRunner()
	if dr == nil {
		return protocol.DialRunnerResponse{}, fmt.Errorf("response missing DialRunner variant")
	}
	return *dr, nil
}
