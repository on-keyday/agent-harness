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

func TestOpenPortForwardRequest_RemoteRoundTrip(t *testing.T) {
	req := OpenPortForwardRequest{
		TaskId:     TaskID{Id: [16]byte{9, 9}},
		Direction:  PortForwardDirection_Remote,
		RemotePort: 5432,
		BindPort:   15432,
	}
	req.SetRemoteHost([]byte("127.0.0.1"))
	req.SetBindAddr([]byte("0.0.0.0"))
	enc, err := req.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	got := &OpenPortForwardRequest{}
	if _, err := got.Decode(enc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Direction != PortForwardDirection_Remote || got.RemotePort != 5432 ||
		got.BindPort != 15432 ||
		string(got.RemoteHost) != "127.0.0.1" || string(got.BindAddr) != "0.0.0.0" {
		t.Fatalf("remote round-trip mismatch: %+v", got)
	}
}

func TestOpenPortForwardResponse_ForwardIdRoundTrip(t *testing.T) {
	resp := OpenPortForwardResponse{Status: OpenPortForwardStatus_Ok, StreamId: 5, ForwardId: 9}
	enc, err := resp.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	got := &OpenPortForwardResponse{}
	if _, err := got.Decode(enc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ForwardId != 9 || got.StreamId != 5 || got.Status != OpenPortForwardStatus_Ok {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestRemoteForwardConn_RoundTrip(t *testing.T) {
	in := RemoteForwardConn{ForwardId: 9, StreamId: 42}
	enc, err := in.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	got := &RemoteForwardConn{}
	if _, err := got.Decode(enc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ForwardId != 9 || got.StreamId != 42 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestRemoteForwardConnNotify_RoundTrip(t *testing.T) {
	in := RemoteForwardConnNotify{StreamId: 1234}
	enc, err := in.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if len(enc) != 8 {
		t.Fatalf("notify encodes to %d bytes, want 8 (plan assumes fixed 8)", len(enc))
	}
	got := &RemoteForwardConnNotify{}
	if _, err := got.Decode(enc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.StreamId != 1234 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestClosePortForwardRequest_RoundTrip(t *testing.T) {
	in := ClosePortForwardRequest{ForwardId: 7}
	enc, err := in.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	got := &ClosePortForwardRequest{}
	if _, err := got.Decode(enc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ForwardId != 7 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestRemoteForwardBindResult_RoundTrip(t *testing.T) {
	for _, ok := range []bool{true, false} {
		in := RemoteForwardBindResult{ForwardId: 13}
		in.SetOk(ok)
		enc, err := in.Append(nil)
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		got := &RemoteForwardBindResult{}
		if _, err := got.Decode(enc); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.ForwardId != 13 || got.Ok() != ok {
			t.Fatalf("round-trip mismatch: forwardId=%d ok=%v want ok=%v", got.ForwardId, got.Ok(), ok)
		}
	}
}
