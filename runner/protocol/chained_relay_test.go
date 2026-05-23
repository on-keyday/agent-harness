package protocol

import (
	"testing"
)

func TestRequestChainedRelayRoundTrip(t *testing.T) {
	orig := RequestChainedRelay{SlotId: 0xABCD}
	buf, err := orig.Append(nil)
	if err != nil {
		t.Fatal(err)
	}
	var got RequestChainedRelay
	if _, err := got.Decode(buf); err != nil {
		t.Fatal(err)
	}
	if got.SlotId != orig.SlotId {
		t.Errorf("SlotId mismatch: got 0x%x want 0x%x", got.SlotId, orig.SlotId)
	}
}

func TestChainedRelayResponseRoundTrip(t *testing.T) {
	for _, st := range []ChainedRelayStatus{
		ChainedRelayStatus_Ok,
		ChainedRelayStatus_Direct,
		ChainedRelayStatus_SlotCollision,
		ChainedRelayStatus_HopSetupFailed,
		ChainedRelayStatus_ChainUnwalkable,
		ChainedRelayStatus_AnotherInFlight,
	} {
		orig := ChainedRelayResponse{Status: st}
		buf, err := orig.Append(nil)
		if err != nil {
			t.Fatalf("status %v: Append: %v", st, err)
		}
		var got ChainedRelayResponse
		if _, err := got.Decode(buf); err != nil {
			t.Fatalf("status %v: Decode: %v", st, err)
		}
		if got.Status != orig.Status {
			t.Errorf("status %v: roundtrip mismatch: got %v", st, got.Status)
		}
	}
}

func TestRunnerMessage_RequestChainedRelay_Variant(t *testing.T) {
	inner := RequestChainedRelay{SlotId: 42}
	msg := RunnerMessage{Kind: RunnerMessageType_RequestChainedRelay}
	if !msg.SetRequestChainedRelay(inner) {
		t.Fatal("SetRequestChainedRelay returned false")
	}
	buf, err := msg.Append(nil)
	if err != nil {
		t.Fatal(err)
	}
	var got RunnerMessage
	if _, err := got.Decode(buf); err != nil {
		t.Fatal(err)
	}
	if got.Kind != RunnerMessageType_RequestChainedRelay {
		t.Fatalf("kind mismatch: got %v want RequestChainedRelay", got.Kind)
	}
	rcr := got.RequestChainedRelay()
	if rcr == nil {
		t.Fatal("RequestChainedRelay() returned nil after decode")
	}
	if rcr.SlotId != 42 {
		t.Errorf("SlotId mismatch: got %d want 42", rcr.SlotId)
	}
}

func TestRunnerRequest_ChainedRelayResponse_Variant(t *testing.T) {
	inner := ChainedRelayResponse{Status: ChainedRelayStatus_HopSetupFailed}
	req := RunnerRequest{Kind: RunnerRequestType_ChainedRelayResponse}
	if !req.SetChainedRelayResponse(inner) {
		t.Fatal("SetChainedRelayResponse returned false")
	}
	buf, err := req.Append(nil)
	if err != nil {
		t.Fatal(err)
	}
	var got RunnerRequest
	if _, err := got.Decode(buf); err != nil {
		t.Fatal(err)
	}
	if got.Kind != RunnerRequestType_ChainedRelayResponse {
		t.Fatalf("kind mismatch: got %v want ChainedRelayResponse", got.Kind)
	}
	crr := got.ChainedRelayResponse()
	if crr == nil {
		t.Fatal("ChainedRelayResponse() returned nil after decode")
	}
	if crr.Status != ChainedRelayStatus_HopSetupFailed {
		t.Errorf("Status mismatch: got %v want HopSetupFailed", crr.Status)
	}
}
