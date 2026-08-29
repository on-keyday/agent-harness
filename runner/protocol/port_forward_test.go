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

// TestForwardTapRecordRoundTrip covers the union shape: only the arm named by
// kind is readable, and the others answer nil rather than a zero value that
// would read as real data.
func TestForwardTapRecordRoundTrip(t *testing.T) {
	rec := ForwardTapRecord{Kind: ForwardTapRecordKind_Data, UnixMs: 1756000000000}
	d := ForwardTapData{
		ConnSeq:        3,
		Direction:      ForwardTapDirection_ToTarget,
		StreamOffset:   4096,
		TruncatedBytes: 12,
	}
	d.SetData([]byte("GET /x HTTP/1.1\r\n"))
	rec.SetData(d)

	enc, err := rec.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	got := &ForwardTapRecord{}
	if _, err := got.Decode(enc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	arm := got.Data()
	if arm == nil {
		t.Fatalf("data arm missing, kind=%v", got.Kind)
	}
	if arm.ConnSeq != 3 || arm.StreamOffset != 4096 || arm.TruncatedBytes != 12 {
		t.Fatalf("arm fields: %+v", arm)
	}
	if string(arm.Data) != "GET /x HTTP/1.1\r\n" {
		t.Fatalf("payload: %q", arm.Data)
	}
	if got.Gap() != nil || got.ConnOpen() != nil {
		t.Fatal("a wrong arm is readable on a data record")
	}
}

func TestForwardTapForwardClosedCarriesReason(t *testing.T) {
	rec := ForwardTapRecord{Kind: ForwardTapRecordKind_ForwardClosed, UnixMs: 1}
	rec.SetForwardClosed(ForwardTapForwardClosed{Reason: PortForwardCloseReason_Killed})
	enc, err := rec.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	got := &ForwardTapRecord{}
	if _, err := got.Decode(enc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ForwardClosed() == nil || got.ForwardClosed().Reason != PortForwardCloseReason_Killed {
		t.Fatalf("reason lost: %+v", got.ForwardClosed())
	}
}

func TestForwardTapConnCloseCarriesItsOwnTotals(t *testing.T) {
	rec := ForwardTapRecord{Kind: ForwardTapRecordKind_ConnClose, UnixMs: 1}
	rec.SetConnClose(ForwardTapConnClose{ConnSeq: 3, BytesToTarget: 4198, BytesFromTarget: 1 << 20})
	enc, err := rec.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	got := &ForwardTapRecord{}
	if _, err := got.Decode(enc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	cc := got.ConnClose()
	if cc == nil || cc.BytesToTarget != 4198 || cc.BytesFromTarget != 1<<20 {
		t.Fatalf("per-connection totals lost: %+v", cc)
	}
}

// TestPortForwardInfoCarriesCounters pins the six appended fields. A row that
// silently drops them would report every forward as idle.
func TestPortForwardInfoCarriesCounters(t *testing.T) {
	in := PortForwardInfo{
		ForwardId:          7,
		BytesToTarget:      1 << 20,
		BytesFromTarget:    48,
		ConnsTotal:         41,
		ConnsOpen:          3,
		Taps:               1,
		LastActivityUnixMs: 1756000000000,
	}
	in.SetBindAddr([]byte("127.0.0.1"))
	in.SetTargetHost([]byte("localhost"))
	in.SetOriginCid([]byte("ws:abc"))

	enc, err := in.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	got := &PortForwardInfo{}
	if _, err := got.Decode(enc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.BytesToTarget != 1<<20 || got.BytesFromTarget != 48 ||
		got.ConnsTotal != 41 || got.ConnsOpen != 3 || got.Taps != 1 ||
		got.LastActivityUnixMs != 1756000000000 {
		t.Fatalf("counters lost: %+v", got)
	}
}

func TestOpenPortForwardRequestCarriesForwardID(t *testing.T) {
	in := OpenPortForwardRequest{RemotePort: 3000, ForwardId: 9}
	in.SetRemoteHost([]byte("localhost"))
	enc, err := in.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	got := &OpenPortForwardRequest{}
	if _, err := got.Decode(enc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ForwardId != 9 {
		t.Fatalf("forward id lost: %d", got.ForwardId)
	}
}
