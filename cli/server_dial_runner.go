package cli

import (
	"context"
	"fmt"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
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
// It dials the server, converts the runner CID to a RunnerID, sends a
// DialRunnerRequest and returns the server's decoded response.
//
// Short-lived processes (harness-cli) should use this form; long-lived
// embedders that already hold a *Client should call ServerDialRunnerWith
// directly to skip the redundant Dial/Close.
func ServerDialRunner(ctx context.Context, serverCID objproto.ConnectionID, targetCID objproto.ConnectionID) (protocol.DialRunnerResponse, error) {
	c, err := Dial(ctx, serverCID)
	if err != nil {
		return protocol.DialRunnerResponse{}, fmt.Errorf("dial server: %w", err)
	}
	defer c.Close()
	return ServerDialRunnerWith(ctx, c, protocol.ConnIDToRunnerID(targetCID))
}

// ServerDialRunnerWith is the lower-level form that operates on an already-
// connected taskControlClient. Exposed for callers that hold a long-lived
// *Client and want to avoid re-dialing.
func ServerDialRunnerWith(ctx context.Context, c taskControlClient, target protocol.RunnerID) (protocol.DialRunnerResponse, error) {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_DialRunner}
	req.SetDialRunner(protocol.DialRunnerRequest{Target: target})

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
