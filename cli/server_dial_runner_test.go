package cli

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// fakeTaskControlClient captures the outgoing TaskControlRequest and feeds
// back a pre-canned TaskControlResponse. Implements the taskControlClient
// interface defined in server_dial_runner.go.
type fakeTaskControlClient struct {
	lastRequest    *protocol.TaskControlRequest
	responseStatus protocol.DialRunnerStatus
	responseErr    error
	// forceKind, when non-nil, overrides the response Kind on the reply
	// (used to verify unexpected-kind error paths). Pointer-typed because
	// TaskControlKind_Submit == 0 collides with the zero value.
	forceKind *protocol.TaskControlKind
	// dropDialRunner forces a response with no DialRunner variant set.
	dropDialRunner bool
}

func (f *fakeTaskControlClient) RoundTripTaskControl(_ context.Context, req *protocol.TaskControlRequest) (*protocol.TaskControlResponse, error) {
	f.lastRequest = req
	if f.responseErr != nil {
		return nil, f.responseErr
	}
	kind := protocol.TaskControlKind_DialRunner
	if f.forceKind != nil {
		kind = *f.forceKind
	}
	resp := &protocol.TaskControlResponse{
		Kind:      kind,
		RequestId: req.RequestId,
	}
	if !f.dropDialRunner {
		resp.SetDialRunner(protocol.DialRunnerResponse{Status: f.responseStatus})
	}
	return resp, nil
}

func makeTestTarget(t *testing.T) protocol.RunnerID {
	t.Helper()
	var target protocol.RunnerID
	target.SetTransport([]byte("ws"))
	target.SetIpAddr([]byte{192, 168, 3, 10})
	target.Port = 8540
	target.UniqueNumber = 0xabcd
	return target
}

func TestServerDialRunnerSendsRequestAndDecodesResponse(t *testing.T) {
	target := makeTestTarget(t)
	fc := &fakeTaskControlClient{responseStatus: protocol.DialRunnerStatus_Ok}

	resp, err := ServerDialRunnerWith(context.Background(), fc, target, protocol.RunnerID{})
	if err != nil {
		t.Fatalf("ServerDialRunnerWith: %v", err)
	}
	if resp.Status != protocol.DialRunnerStatus_Ok {
		t.Errorf("status: got %v want Ok", resp.Status)
	}
	if fc.lastRequest == nil {
		t.Fatal("client never saw a request")
	}
	if fc.lastRequest.Kind != protocol.TaskControlKind_DialRunner {
		t.Errorf("request kind: got %v want DialRunner", fc.lastRequest.Kind)
	}
	dr := fc.lastRequest.DialRunner()
	if dr == nil {
		t.Fatal("DialRunner variant nil in captured request")
	}
	if string(dr.Target.Transport) != "ws" {
		t.Errorf("target transport: got %q want %q", dr.Target.Transport, "ws")
	}
	if dr.Target.Port != 8540 {
		t.Errorf("target port: got %d want %d", dr.Target.Port, 8540)
	}
	if dr.Target.UniqueNumber != 0xabcd {
		t.Errorf("target unique: got %x want %x", dr.Target.UniqueNumber, 0xabcd)
	}
}

func TestServerDialRunnerPropagatesNonOkStatus(t *testing.T) {
	fc := &fakeTaskControlClient{responseStatus: protocol.DialRunnerStatus_DialFailed}
	resp, err := ServerDialRunnerWith(context.Background(), fc, makeTestTarget(t), protocol.RunnerID{})
	if err != nil {
		t.Fatalf("ServerDialRunnerWith: %v", err)
	}
	if resp.Status != protocol.DialRunnerStatus_DialFailed {
		t.Errorf("status: got %v want DialFailed", resp.Status)
	}
}

func TestServerDialRunnerPropagatesRoundTripErr(t *testing.T) {
	wantErr := errors.New("boom")
	fc := &fakeTaskControlClient{responseErr: wantErr}
	_, err := ServerDialRunnerWith(context.Background(), fc, makeTestTarget(t), protocol.RunnerID{})
	if !errors.Is(err, wantErr) {
		t.Errorf("err: got %v want %v", err, wantErr)
	}
}

func TestServerDialRunnerRejectsUnexpectedKind(t *testing.T) {
	kind := protocol.TaskControlKind_Submit
	fc := &fakeTaskControlClient{forceKind: &kind}
	_, err := ServerDialRunnerWith(context.Background(), fc, makeTestTarget(t), protocol.RunnerID{})
	if err == nil {
		t.Fatal("expected error for non-DialRunner response kind")
	}
}

func TestServerDialRunnerRejectsMissingVariant(t *testing.T) {
	fc := &fakeTaskControlClient{dropDialRunner: true}
	_, err := ServerDialRunnerWith(context.Background(), fc, makeTestTarget(t), protocol.RunnerID{})
	if err == nil {
		t.Fatal("expected error when DialRunner variant is nil")
	}
}

// TestServerDialRunnerWithVia: verifies that a non-zero viaCID populates
// DialRunnerRequest.Via in the wire payload.
func TestServerDialRunnerWithVia(t *testing.T) {
	fakeServerCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort("192.168.1.10:8540"), 12345)
	fakeViaCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort("192.168.1.20:8550"), 51357)

	fc := &fakeTaskControlClient{responseStatus: protocol.DialRunnerStatus_Ok}

	resp, err := ServerDialRunnerWith(context.Background(), fc,
		protocol.ConnIDToRunnerID(fakeServerCID),
		protocol.ConnIDToRunnerID(fakeViaCID))
	if err != nil {
		t.Fatalf("ServerDialRunnerWith: %v", err)
	}
	if resp.Status != protocol.DialRunnerStatus_Ok {
		t.Errorf("status: got %v want Ok", resp.Status)
	}
	if fc.lastRequest == nil {
		t.Fatal("client did not see a DialRunner request")
	}
	dr := fc.lastRequest.DialRunner()
	if dr == nil {
		t.Fatal("DialRunner variant nil")
	}
	if string(dr.Via.Transport) != "ws" {
		t.Errorf("via.transport: got %q", dr.Via.Transport)
	}
	if dr.Via.Port != 8550 {
		t.Errorf("via.port: got %d want 8550", dr.Via.Port)
	}
	if dr.Via.UniqueNumber != 51357 {
		t.Errorf("via.unique_number: got %d want 51357", dr.Via.UniqueNumber)
	}
}

// TestServerDialRunnerWithoutVia: zero viaCID should leave DialRunnerRequest.Via
// with empty Transport (the "no via" marker for backward compat).
func TestServerDialRunnerWithoutVia(t *testing.T) {
	fakeServerCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort("192.168.1.10:8540"), 12345)

	fc := &fakeTaskControlClient{responseStatus: protocol.DialRunnerStatus_Ok}

	_, err := ServerDialRunnerWith(context.Background(), fc,
		protocol.ConnIDToRunnerID(fakeServerCID),
		protocol.RunnerID{})
	if err != nil {
		t.Fatalf("ServerDialRunnerWith: %v", err)
	}
	if fc.lastRequest == nil {
		t.Fatal("client did not see a DialRunner request")
	}
	dr := fc.lastRequest.DialRunner()
	if dr == nil {
		t.Fatal("DialRunner variant nil")
	}
	if len(dr.Via.Transport) != 0 {
		t.Errorf("via.transport should be empty for direct dial, got %q", dr.Via.Transport)
	}
}
