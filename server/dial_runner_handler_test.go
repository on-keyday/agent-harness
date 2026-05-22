package server

import (
	"context"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/transport"
)

// TestDialRunnerInvalidTargetTransport covers empty-transport early return.
func TestDialRunnerInvalidTargetTransport(t *testing.T) {
	h := &DialRunnerHandler{
		Logger:   slog.Default(),
		Endpoint: nil, // not reached when validation fails first
	}
	var bad protocol.RunnerID
	// Leave transport empty (SetTransport with empty slice produces TransportLen=0)
	bad.SetTransport([]byte{})
	bad.SetIpAddr([]byte{127, 0, 0, 1})
	bad.Port = 8540

	resp := h.Handle(context.Background(), bad)
	if resp.Status != protocol.DialRunnerStatus_InvalidTarget {
		t.Errorf("status: got %v, want InvalidTarget", resp.Status)
	}
}

// TestDialRunnerDialFailsUnboundedPort covers a real DoECDHHandshake that hits a
// nothing-listening port. Uses a real WebSocket Client endpoint so we exercise
// the actual dial path. The test must complete quickly via DialTimeout.
func TestDialRunnerDialFailsUnboundedPort(t *testing.T) {
	ep := buildTestClientEndpoint(t)
	h := &DialRunnerHandler{
		Logger:      slog.Default(),
		Endpoint:    ep,
		DialTimeout: 500 * time.Millisecond,
	}
	var target protocol.RunnerID
	target.SetTransport([]byte("ws"))
	target.SetIpAddr([]byte{127, 0, 0, 1})
	target.Port = 1 // nothing listens here

	resp := h.Handle(context.Background(), target)
	if resp.Status != protocol.DialRunnerStatus_DialFailed {
		t.Errorf("status: got %v, want DialFailed", resp.Status)
	}
}

// buildTestClientEndpoint returns a WS-only Client endpoint for tests.
// Client mode is sufficient because DialRunnerHandler only initiates outbound
// ECDH handshakes; it never accepts inbound connections.
func buildTestClientEndpoint(t *testing.T) objproto.Endpoint {
	t.Helper()
	ep, err := transport.WebSocketEndpoint(nil, transport.WebSocketConfig{
		Logger: slog.Default(),
		Path:   "/ws",
		Mode:   objproto.EndpointModeClient,
	})
	if err != nil {
		t.Fatalf("buildTestClientEndpoint: %v", err)
	}
	return ep
}

// TestDialRunnerTaskHandlerCase verifies that the TaskHandler dispatches
// DialRunner correctly: when Endpoint is nil the handler returns InvalidTarget
// for a bad target (before reaching any dial), and the response is encoded/decoded
// correctly.
func TestDialRunnerTaskHandlerCase(t *testing.T) {
	fc := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9001-50")}

	h := &TaskHandler{
		Tasks:    NewTaskStore(),
		Registry: NewRegistry(),
		// Endpoint is nil — the handler will fail on validation before using it
	}

	var req protocol.TaskControlRequest
	req.Kind = protocol.TaskControlKind_DialRunner
	// Target has empty transport → InvalidTarget
	var dr protocol.DialRunnerRequest
	dr.Target.SetIpAddr([]byte{127, 0, 0, 1})
	dr.Target.Port = 9001
	req.SetDialRunner(dr)

	payload, err := req.Append(nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	h.Handle(fc, payload)

	if len(fc.sent) == 0 {
		t.Fatal("expected a response message, got none")
	}
	// Strip the ApplicationPayloadKind byte
	raw := fc.sent[0]
	if len(raw) < 1 {
		t.Fatal("response too short")
	}
	var resp protocol.TaskControlResponse
	if err := resp.DecodeExact(raw[1:]); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Kind != protocol.TaskControlKind_DialRunner {
		t.Errorf("kind: got %v, want DialRunner", resp.Kind)
	}
	dr2 := resp.DialRunner()
	if dr2 == nil {
		t.Fatal("DialRunner response variant is nil")
	}
	if dr2.Status != protocol.DialRunnerStatus_InvalidTarget {
		t.Errorf("status: got %v, want InvalidTarget", dr2.Status)
	}
}

// TestDialRunnerRequestRoundTrip verifies wire encode/decode symmetry for
// DialRunnerRequest embedded in TaskControlRequest.
func TestDialRunnerRequestRoundTrip(t *testing.T) {
	var req protocol.TaskControlRequest
	req.Kind = protocol.TaskControlKind_DialRunner
	req.RequestId = 42
	var dr protocol.DialRunnerRequest
	dr.Target.SetTransport([]byte("ws"))
	dr.Target.SetIpAddr([]byte{10, 0, 0, 1})
	dr.Target.Port = 8540
	dr.Target.UniqueNumber = 7
	req.SetDialRunner(dr)

	encoded, err := req.Append(nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	var got protocol.TaskControlRequest
	if err := got.DecodeExact(encoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Kind != protocol.TaskControlKind_DialRunner {
		t.Errorf("kind: got %v", got.Kind)
	}
	if got.RequestId != 42 {
		t.Errorf("request_id: got %d", got.RequestId)
	}
	drGot := got.DialRunner()
	if drGot == nil {
		t.Fatal("DialRunner variant nil after decode")
	}
	if string(drGot.Target.Transport) != "ws" {
		t.Errorf("transport: got %q", drGot.Target.Transport)
	}
	if drGot.Target.Port != 8540 {
		t.Errorf("port: got %d", drGot.Target.Port)
	}
	if drGot.Target.UniqueNumber != 7 {
		t.Errorf("unique_number: got %d", drGot.Target.UniqueNumber)
	}
}

// TestDialRunnerResponseRoundTrip verifies wire encode/decode for TaskControlResponse
// with DialRunner payload.
func TestDialRunnerResponseRoundTrip(t *testing.T) {
	var resp protocol.TaskControlResponse
	resp.Kind = protocol.TaskControlKind_DialRunner
	resp.RequestId = 99
	resp.SetDialRunner(protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_Ok})

	encoded, err := resp.Append(nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	var got protocol.TaskControlResponse
	if err := got.DecodeExact(encoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Kind != protocol.TaskControlKind_DialRunner {
		t.Errorf("kind: got %v", got.Kind)
	}
	if got.RequestId != 99 {
		t.Errorf("request_id: got %d", got.RequestId)
	}
	dr := got.DialRunner()
	if dr == nil {
		t.Fatal("DialRunner variant nil after decode")
	}
	if dr.Status != protocol.DialRunnerStatus_Ok {
		t.Errorf("status: got %v", dr.Status)
	}
}

// Ensure unused http import is used (for Client endpoint test helper).
var _ = http.DefaultServeMux
