package protocol

import (
	"bytes"
	"testing"
)

func TestDialRunnerRequestRoundTrip(t *testing.T) {
	var orig DialRunnerRequest
	orig.Target.SetTransport([]byte("ws"))
	orig.Target.SetIpAddr([]byte{192, 168, 3, 10})
	orig.Target.Port = 8540
	orig.Target.UniqueNumber = 0xabcd

	buf, err := orig.Append(nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	var got DialRunnerRequest
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !bytes.Equal(got.Target.Transport, orig.Target.Transport) {
		t.Errorf("transport: got %q want %q", got.Target.Transport, orig.Target.Transport)
	}
	if got.Target.Port != orig.Target.Port {
		t.Errorf("port: got %d want %d", got.Target.Port, orig.Target.Port)
	}
	if got.Target.UniqueNumber != orig.Target.UniqueNumber {
		t.Errorf("unique_number: got %d want %d", got.Target.UniqueNumber, orig.Target.UniqueNumber)
	}
}

func TestDialRunnerResponseRoundTrip(t *testing.T) {
	orig := DialRunnerResponse{Status: DialRunnerStatus_DialFailed}
	buf, err := orig.Append(nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	var got DialRunnerResponse
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Status != orig.Status {
		t.Errorf("status: got %v want %v", got.Status, orig.Status)
	}
}
