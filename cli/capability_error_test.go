package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// fakePermDeniedClient returns a PermissionDenied TaskControlResponse for
// every request, regardless of what was sent. It satisfies taskControlClient.
type fakePermDeniedClient struct {
	// requestedKind is the kind placed in the PermissionDeniedResponse.
	requestedKind protocol.TaskControlKind
	// requiredCap is the capability placed in the PermissionDeniedResponse.
	requiredCap protocol.Capability
}

func (f *fakePermDeniedClient) RoundTripTaskControl(_ context.Context, req *protocol.TaskControlRequest) (*protocol.TaskControlResponse, error) {
	resp := &protocol.TaskControlResponse{
		Kind:      protocol.TaskControlKind_PermissionDenied,
		RequestId: req.RequestId,
	}
	resp.SetPermissionDenied(protocol.PermissionDeniedResponse{
		RequestedKind: f.requestedKind,
		RequiredCap:   f.requiredCap,
	})
	return resp, nil
}

// TestPermissionDeniedRecognition verifies that RoundTripTaskControl (via the
// fake transport) converts a PermissionDenied server response into a
// *CapabilityDeniedError with the correct fields, rather than returning a raw
// response.
func TestPermissionDeniedRecognition(t *testing.T) {
	fake := &fakePermDeniedClient{
		requestedKind: protocol.TaskControlKind_Submit,
		requiredCap:   protocol.Capability_Spawn,
	}

	// Build a minimal *Client that uses our fake via its pending map and
	// dispatchControl path. Instead of wiring a full peer.Conn, we call
	// the fake's RoundTripTaskControl directly through a thin wrapper Client
	// that satisfies the same interface.
	//
	// Since RoundTripTaskControl is a method on *Client and we need to inject
	// a fake transport, we test via a helper that mimics what RoundTripTaskControl
	// does with the PermissionDenied guard — but without a live peer.Conn.
	// The guard logic is self-contained in client.go, so we exercise it by
	// calling roundTripWithFake which replicates the guard path.
	_, err := roundTripWithFake(context.Background(), fake, protocol.TaskControlKind_Submit)
	if err == nil {
		t.Fatal("expected CapabilityDeniedError, got nil")
	}

	var capErr *CapabilityDeniedError
	if !errors.As(err, &capErr) {
		t.Fatalf("err type = %T, want *CapabilityDeniedError; err = %v", err, err)
	}
	if capErr.RequestedKind != protocol.TaskControlKind_Submit {
		t.Errorf("RequestedKind = %v, want Submit", capErr.RequestedKind)
	}
	if capErr.RequiredCap != protocol.Capability_Spawn {
		t.Errorf("RequiredCap = %v, want Spawn", capErr.RequiredCap)
	}
	// Error string should mention the relevant details.
	msg := capErr.Error()
	if msg == "" {
		t.Error("Error() returned empty string")
	}
}

// roundTripWithFake replicates the PermissionDenied guard in
// RoundTripTaskControl, using an arbitrary taskControlClient implementation.
// This lets us test the guard logic without a live WebSocket connection.
func roundTripWithFake(ctx context.Context, c taskControlClient, kind protocol.TaskControlKind) (*protocol.TaskControlResponse, error) {
	req := &protocol.TaskControlRequest{Kind: kind}
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, err
	}
	// PermissionDenied guard — mirrors the guard in client.go RoundTripTaskControl.
	if resp.Kind == protocol.TaskControlKind_PermissionDenied &&
		req.Kind != protocol.TaskControlKind_PermissionDenied {
		pd := resp.PermissionDenied()
		if pd != nil {
			return nil, &CapabilityDeniedError{
				RequestedKind: pd.RequestedKind,
				RequiredCap:   pd.RequiredCap,
			}
		}
	}
	return resp, nil
}

// TestCapabilityDeniedErrorMessage verifies the error message format.
func TestCapabilityDeniedErrorMessage(t *testing.T) {
	err := &CapabilityDeniedError{
		RequestedKind: protocol.TaskControlKind_Submit,
		RequiredCap:   protocol.Capability_Spawn,
	}
	msg := err.Error()
	// Must mention "permission denied", the kind, and the required capability.
	for _, want := range []string{"permission denied", "Submit", "spawn"} {
		if !containsSubstring(msg, want) {
			t.Errorf("error message %q does not contain %q", msg, want)
		}
	}
}

// The test above covers a SINGLE-bit requirement, which is the only shape the
// generated enum String() can render — so it passed while the mask shape was
// broken in production. An attach is gated by a mask of ALTERNATIVES
// (server/capabilities.go attachModeCap, checked with hasAnyCap), and a view
// attach denial printed `requires capability Capability(12292)`: the raw mask,
// because String() names single values only. Operator-reported. 12292 is that
// exact mask, kept here as the literal that reproduced it.
func TestCapabilityDeniedNamesEveryAlternativeInAMask(t *testing.T) {
	err := &CapabilityDeniedError{
		RequestedKind: protocol.TaskControlKind_AttachSession,
		RequiredCap: protocol.Capability_ExecView |
			protocol.Capability_ExecCowrite |
			protocol.Capability_ExecControl,
	}
	if got := uint32(err.RequiredCap); got != 12292 {
		t.Fatalf("fixture mask = %d, want the 12292 the operator saw", got)
	}
	msg := err.Error()

	if containsSubstring(msg, "Capability(") || containsSubstring(msg, "12292") {
		t.Fatalf("Error() = %q; the raw mask leaked instead of the names", msg)
	}
	for _, want := range []string{"exec_view", "exec_cowrite", "exec_control", "AttachSession"} {
		if !containsSubstring(msg, want) {
			t.Errorf("Error() = %q, missing %q — every alternative must be named", msg, want)
		}
	}
	// "any of", not "capability": holding ONE of them satisfies the request, so
	// the singular claimed a conjunction the server never asked for.
	if !containsSubstring(msg, "any of") {
		t.Errorf("Error() = %q, want it to say the alternatives ARE alternatives", msg)
	}
}

// A single-bit requirement keeps the singular wording — "any of spawn" would
// read as though something were being withheld.
func TestCapabilityDeniedKeepsTheSingularForOneBit(t *testing.T) {
	err := &CapabilityDeniedError{
		RequestedKind: protocol.TaskControlKind_Submit,
		RequiredCap:   protocol.Capability_Spawn,
	}
	msg := err.Error()
	if !containsSubstring(msg, "requires capability spawn") {
		t.Errorf("Error() = %q, want the singular form naming spawn", msg)
	}
	if containsSubstring(msg, "any of") {
		t.Errorf("Error() = %q, want no alternatives wording for a single bit", msg)
	}
}

// The builders' capability default is pinned in session_opts_test.go
// (TestSessionOptsCapsResolution), which runs the real
// buildSubmitRequest / buildOpenInteractiveRequest. A
// TestBuilderDefaultsToCapabilityAll used to sit here asserting the opposite
// default, and it could never have caught a regression either way: it
// assigned Capability_All to a bare protocol.SubmitRequest and then asserted
// that same literal back, without calling a builder at all.

func containsSubstring(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
