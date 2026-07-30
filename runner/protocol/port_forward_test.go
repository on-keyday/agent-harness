package protocol

import "testing"

// TestOpenPortForwardRequest_RoundTrip covers the slimmed message: it now
// means exactly "open the data stream for one accepted local-forward
// connection", so direction/bind_addr/bind_port are gone (moved to
// RegisterPortForwardRequest, see TestRegisterPortForwardRoundTrip below).
func TestOpenPortForwardRequest_RoundTrip(t *testing.T) {
	req := OpenPortForwardRequest{
		TaskId:     TaskID{Id: [16]byte{1, 2, 3}},
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
	if got.RemotePort != 3000 || string(got.RemoteHost) != "127.0.0.1" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

// TestOpenPortForwardResponse_RoundTrip covers the slimmed message: forward_id
// is gone (it was the -R registration handle, now returned by
// RegisterPortForwardResponse).
func TestOpenPortForwardResponse_RoundTrip(t *testing.T) {
	resp := OpenPortForwardResponse{Status: OpenPortForwardStatus_Ok, StreamId: 5}
	enc, err := resp.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	got := &OpenPortForwardResponse{}
	if _, err := got.Decode(enc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.StreamId != 5 || got.Status != OpenPortForwardStatus_Ok {
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

func TestRegisterPortForwardRoundTrip(t *testing.T) {
	var req RegisterPortForwardRequest
	req.TaskId = TaskID{Id: [16]byte{1, 2, 3}}
	req.Direction = PortForwardDirection_Local
	req.SetBindAddr([]byte("127.0.0.1"))
	req.BindPort = 8080
	req.SetTargetHost([]byte("db.internal"))
	req.TargetPort = 5432

	buf := req.MustAppend(nil)
	var got RegisterPortForwardRequest
	if err := got.DecodeExact(buf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Direction != PortForwardDirection_Local ||
		string(got.BindAddr) != "127.0.0.1" || got.BindPort != 8080 ||
		string(got.TargetHost) != "db.internal" || got.TargetPort != 5432 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestPortForwardEventVariants(t *testing.T) {
	var notify PortForwardEvent
	notify.Kind = PortForwardEventKind_ConnNotify
	notify.SetConnNotify(RemoteForwardConnNotify{StreamId: 42})
	buf := notify.MustAppend(nil)

	var closed PortForwardEvent
	closed.Kind = PortForwardEventKind_Closed
	closed.SetClosed(PortForwardClosed{Reason: PortForwardCloseReason_Killed})
	buf = closed.MustAppend(buf)

	// Two events, back to back, decoded from one buffer.
	var a PortForwardEvent
	rest, err := a.Decode(buf)
	if err != nil {
		t.Fatalf("decode first: %v", err)
	}
	if a.Kind != PortForwardEventKind_ConnNotify || a.ConnNotify().StreamId != 42 {
		t.Fatalf("first event wrong: %+v", a)
	}
	var b PortForwardEvent
	if _, err := b.Decode(rest); err != nil {
		t.Fatalf("decode second: %v", err)
	}
	if b.Kind != PortForwardEventKind_Closed ||
		b.Closed().Reason != PortForwardCloseReason_Killed {
		t.Fatalf("second event wrong: %+v", b)
	}
}

func TestPortForwardListBodyRoundTrip(t *testing.T) {
	var info PortForwardInfo
	info.ForwardId = 7
	info.Direction = PortForwardDirection_Remote
	info.SetBindAddr([]byte("127.0.0.1"))
	info.BindPort = 6001
	info.SetTargetHost([]byte("localhost"))
	info.TargetPort = 6000
	info.OriginKind = ClientKind_Tui
	info.SetOriginCid([]byte("ws:127.0.0.1:1234-ab"))

	var body PortForwardListResultBody
	body.SetForwards([]PortForwardInfo{info})
	buf := body.MustAppend(nil)

	var got PortForwardListResultBody
	if err := got.DecodeExact(buf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Forwards) != 1 || got.Forwards[0].ForwardId != 7 ||
		string(got.Forwards[0].OriginCid) != "ws:127.0.0.1:1234-ab" {
		t.Fatalf("round-trip mismatch: %+v", got.Forwards)
	}
}

// TestRegisterPortForwardRequest_InProcessRoundTrip covers the endpoint-kind
// field. A local in-process registration has no client-side bind address, so
// the bind pair round-trips as empty/0 rather than carrying a placeholder.
func TestRegisterPortForwardRequest_InProcessRoundTrip(t *testing.T) {
	req := RegisterPortForwardRequest{
		TaskId:         TaskID{Id: [16]byte{0x11}},
		Direction:      PortForwardDirection_Local,
		TargetPort:     3000,
		ClientEndpoint: ClientEndpointKind_InProcess,
	}
	req.SetTargetHost([]byte("127.0.0.1"))
	enc, err := req.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	got := &RegisterPortForwardRequest{}
	if _, err := got.Decode(enc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ClientEndpoint != ClientEndpointKind_InProcess {
		t.Fatalf("ClientEndpoint = %v, want InProcess", got.ClientEndpoint)
	}
	if got.BindPort != 0 || len(got.BindAddr) != 0 {
		t.Fatalf("bind pair must stay empty for an in-process endpoint: addr=%q port=%d", got.BindAddr, got.BindPort)
	}
	if string(got.TargetHost) != "127.0.0.1" || got.TargetPort != 3000 {
		t.Fatalf("target round-trip mismatch: %q:%d", got.TargetHost, got.TargetPort)
	}
}

// TestRegisterPortForwardRequest_OsSocketIsZeroValue pins the enum order: a
// struct built without naming the field must mean "the client end is a real
// socket", which is what every existing -L / -R call site means.
func TestRegisterPortForwardRequest_OsSocketIsZeroValue(t *testing.T) {
	req := RegisterPortForwardRequest{Direction: PortForwardDirection_Local, BindPort: 18080}
	req.SetBindAddr([]byte("127.0.0.1"))
	if req.ClientEndpoint != ClientEndpointKind_OsSocket {
		t.Fatalf("zero value = %v, want OsSocket", req.ClientEndpoint)
	}
}

// TestPortForwardInfo_InProcessRoundTrip covers the list-result side, which is
// what makes an in-process forward distinguishable in `forward ls`.
func TestPortForwardInfo_InProcessRoundTrip(t *testing.T) {
	info := PortForwardInfo{
		ForwardId:      7,
		Direction:      PortForwardDirection_Local,
		TaskId:         TaskID{Id: [16]byte{0x22}},
		TargetPort:     6379,
		OriginKind:     ClientKind_Webui,
		ClientEndpoint: ClientEndpointKind_InProcess,
	}
	info.SetTargetHost([]byte("127.0.0.1"))
	info.SetOriginCid([]byte("ws:127.0.0.1:1-2"))
	enc, err := info.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	got := &PortForwardInfo{}
	if _, err := got.Decode(enc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ClientEndpoint != ClientEndpointKind_InProcess {
		t.Fatalf("ClientEndpoint = %v, want InProcess", got.ClientEndpoint)
	}
	if got.ForwardId != 7 || got.TargetPort != 6379 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}
