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
	// Via is an IDENTITY, unlike Target beside it: it names a registered proxy
	// runner the server resolves, not an address the server dials.
	req.Via.Id = [16]byte{0xC0, 0xA8, 0x03, 0x0E, 9, 8, 7, 6, 5, 4, 3, 2, 1, 0, 0xCA, 0xFE}

	buf, err := req.Append(nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	var got DialRunnerRequest
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Via != req.Via {
		t.Errorf("via: got %s want %s", got.Via.Hex(), req.Via.Hex())
	}
	if got.Target.Port != 8540 || string(got.Target.Transport) != "ws" {
		t.Errorf("target: got transport=%q port=%d", got.Target.Transport, got.Target.Port)
	}
}

func TestDialRunnerRequestViaEmptyRoundTrip(t *testing.T) {
	// A zero Via means "no via" — the direct-dial path. It used to be spelled
	// transport_len == 0, which was the same statement while an identity was an
	// address; IsZero is what says it now.
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
	if !got.Via.IsZero() {
		t.Errorf("via should be absent, got %s", got.Via.Hex())
	}
}
