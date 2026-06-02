package protocol

import "testing"

func TestOpenPortForwardRequest_RoundTrip(t *testing.T) {
	req := OpenPortForwardRequest{
		TaskId:     TaskID{Id: [16]byte{1, 2, 3}},
		Direction:  PortForwardDirection_Local,
		RemotePort: 3000,
	}
	req.SetRemoteHost([]byte("127.0.0.1"))
	enc, err := req.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	got := &OpenPortForwardRequest{}
	if _, err := got.Decode(enc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.RemotePort != 3000 || string(got.RemoteHost) != "127.0.0.1" ||
		got.Direction != PortForwardDirection_Local {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}
