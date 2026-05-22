package protocol

import (
	"bytes"
	"testing"
)

func TestProxyControlRequestRoundTrip(t *testing.T) {
	var inner ProxyRequest
	copy(inner.TaskId.Id[:], []byte("0123456789abcdef"))

	var pc ProxyControl
	pc.Kind = ProxyControlKind_Request
	pc.SetRequest(inner)

	buf, err := pc.Append(nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	var got ProxyControl
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Kind != ProxyControlKind_Request {
		t.Errorf("kind: got %v", got.Kind)
	}
	req := got.Request()
	if req == nil {
		t.Fatal("Request() returned nil")
	}
	if !bytes.Equal(req.TaskId.Id[:], inner.TaskId.Id[:]) {
		t.Errorf("task_id: got %x want %x", req.TaskId.Id, inner.TaskId.Id)
	}
}

func TestProxyControlEstablishResponseRoundTrip(t *testing.T) {
	resp := ProxyEstablishResponse{Status: ProxyEstablishStatus_IdCollision}

	var pc ProxyControl
	pc.Kind = ProxyControlKind_EstablishResponse
	pc.SetEstablishResponse(resp)

	buf, err := pc.Append(nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	var got ProxyControl
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	er := got.EstablishResponse()
	if er == nil {
		t.Fatal("EstablishResponse() returned nil")
	}
	if er.Status != ProxyEstablishStatus_IdCollision {
		t.Errorf("status: got %v", er.Status)
	}
}

func TestDialGreetingRoundTrip(t *testing.T) {
	orig := DialGreeting{Version: 1}
	buf, err := orig.Append(nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	var got DialGreeting
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Version != orig.Version {
		t.Errorf("version: got %d want %d", got.Version, orig.Version)
	}
}
