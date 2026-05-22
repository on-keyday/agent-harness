package protocol

import (
	"testing"
)

func TestEstablishRelayRequestRoundTrip(t *testing.T) {
	var inner EstablishRelayRequest
	inner.Target.SetTransport([]byte("ws"))
	inner.Target.SetIpAddr([]byte{10, 0, 0, 5})
	inner.Target.Port = 8540
	inner.Target.UniqueNumber = 0xABCD
	inner.SlotId = 0x1234

	var req RunnerRequest
	req.Kind = RunnerRequestType_EstablishRelay
	req.SetEstablishRelay(inner)

	buf, err := req.Append(nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	var got RunnerRequest
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Kind != RunnerRequestType_EstablishRelay {
		t.Errorf("kind: got %v want EstablishRelay", got.Kind)
	}
	er := got.EstablishRelay()
	if er == nil {
		t.Fatal("EstablishRelay variant nil after decode")
	}
	if er.SlotId != 0x1234 {
		t.Errorf("slot_id: got %x", er.SlotId)
	}
	if string(er.Target.Transport) != "ws" {
		t.Errorf("transport: got %q", er.Target.Transport)
	}
}

func TestEstablishRelayResponseRoundTrip(t *testing.T) {
	inner := EstablishRelayResponse{Status: EstablishRelayStatus_SlotCollision}

	var msg RunnerMessage
	msg.Kind = RunnerMessageType_EstablishRelayResponse
	msg.SetEstablishRelayResponse(inner)

	buf, err := msg.Append(nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	var got RunnerMessage
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Kind != RunnerMessageType_EstablishRelayResponse {
		t.Errorf("kind: got %v", got.Kind)
	}
	er := got.EstablishRelayResponse()
	if er == nil {
		t.Fatal("EstablishRelayResponse variant nil")
	}
	if er.Status != EstablishRelayStatus_SlotCollision {
		t.Errorf("status: got %v", er.Status)
	}
}

func TestDialRunnerRequestWithViaRoundTrip(t *testing.T) {
	var req DialRunnerRequest
	req.Target.SetTransport([]byte("ws"))
	req.Target.SetIpAddr([]byte{10, 0, 0, 9})
	req.Target.Port = 8540
	req.Via.SetTransport([]byte("ws"))
	req.Via.SetIpAddr([]byte{192, 168, 3, 14})
	req.Via.Port = 52036
	req.Via.UniqueNumber = 51357

	buf, err := req.Append(nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	var got DialRunnerRequest
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if string(got.Via.Transport) != "ws" {
		t.Errorf("via.transport: got %q", got.Via.Transport)
	}
	if got.Via.Port != 52036 || got.Via.UniqueNumber != 51357 {
		t.Errorf("via fields: got port=%d uniq=%d", got.Via.Port, got.Via.UniqueNumber)
	}
}

func TestDialRunnerRequestViaEmptyRoundTrip(t *testing.T) {
	// transport_len == 0 means "no via" (direct dial backward compat)
	var req DialRunnerRequest
	req.Target.SetTransport([]byte("ws"))
	req.Target.SetIpAddr([]byte{10, 0, 0, 9})
	req.Target.Port = 8540
	// Via fields all zero

	buf, err := req.Append(nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	var got DialRunnerRequest
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(got.Via.Transport) != 0 {
		t.Errorf("via.transport should be empty, got %q", got.Via.Transport)
	}
}
