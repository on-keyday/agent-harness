package server

import (
	"bytes"
	"encoding/hex"
	"net/netip"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestValidForwardShape(t *testing.T) {
	legal := map[[2]int]bool{
		{int(protocol.ForwardProtocol_Tcp), int(protocol.PortForwardDirection_Local)}:  true,
		{int(protocol.ForwardProtocol_Tcp), int(protocol.PortForwardDirection_Remote)}: true,
		{int(protocol.ForwardProtocol_Udp), int(protocol.PortForwardDirection_Local)}:  true,
		{int(protocol.ForwardProtocol_Udp), int(protocol.PortForwardDirection_Remote)}: true,
		{int(protocol.ForwardProtocol_Ip), int(protocol.PortForwardDirection_Link)}:    true,
	}
	for _, p := range []protocol.ForwardProtocol{protocol.ForwardProtocol_Tcp, protocol.ForwardProtocol_Udp, protocol.ForwardProtocol_Ip} {
		for _, d := range []protocol.PortForwardDirection{protocol.PortForwardDirection_Local, protocol.PortForwardDirection_Remote, protocol.PortForwardDirection_Link} {
			want := legal[[2]int{int(p), int(d)}]
			if got := validForwardShape(p, d); got != want {
				t.Errorf("validForwardShape(%v, %v) = %v, want %v", p, d, got, want)
			}
		}
	}
}

func testLinkConfig(t *testing.T, client, runner string) protocol.IpLinkConfig {
	t.Helper()
	cfg, err := protocol.NewIpLinkConfig(netip.MustParsePrefix(client), netip.MustParseAddr(runner))
	if err != nil {
		t.Fatalf("link config: %v", err)
	}
	return cfg
}

// linkFixture is a running task on runner-1, and an ip × link request for it.
func linkFixture(t *testing.T) (*TaskHandler, *fakeConn, *fakeConn, *recordingBidiStream, *protocol.RegisterPortForwardRequest) {
	t.Helper()
	h := &TaskHandler{Tasks: NewTaskStore(), Registry: NewRegistry()}
	var rawID [16]byte
	rawID[0] = 0x6C
	idHex := hex.EncodeToString(rawID[:])
	h.Tasks.mu.Lock()
	h.Tasks.tasks[idHex] = &TaskEntry{ID: idHex, Status: protocol.TaskStatus_Running, AssignedTo: testRunnerID("runner-1")}
	h.Tasks.order = append(h.Tasks.order, idHex)
	h.Tasks.mu.Unlock()
	runnerConn := &fakeConn{id: tcid("runner-1")}
	h.Registry.Add(&RunnerEntry{ID: tcid("runner-1"), Identity: testRunnerID("runner-1"), Conn: runnerConn})
	ctrl := newRecordingBidiStream(556)
	t.Cleanup(func() { _ = ctrl.CloseBoth() })
	clientConn := &fakeConn{id: tcid("client-1"), nextBidi: ctrl}
	req := &protocol.RegisterPortForwardRequest{
		TaskId:    protocol.TaskID{Id: rawID},
		Direction: protocol.PortForwardDirection_Link,
		Protocol:  protocol.ForwardProtocol_Ip,
	}
	req.SetLink(testLinkConfig(t, "10.200.0.1/30", "10.200.0.2"))
	return h, clientConn, runnerConn, ctrl, req
}

// runnerRequests decodes every RunnerRequest a fake runner connection received.
func runnerRequests(t *testing.T, c *fakeConn) []protocol.RunnerRequest {
	t.Helper()
	var out []protocol.RunnerRequest
	for _, b := range c.Sent() {
		var rr protocol.RunnerRequest
		if _, err := rr.Decode(b[1:]); err != nil {
			t.Fatalf("decode runner req: %v", err)
		}
		out = append(out, rr)
	}
	return out
}

func TestRegisterIPLink_SetupOk(t *testing.T) {
	h, clientConn, runnerConn, _, req := linkFixture(t)
	resp := runSetupRegister(t, h, clientConn, req, runnerConn, true, "")
	if resp.Status != protocol.OpenPortForwardStatus_Ok {
		t.Fatalf("status = %v (%s), want Ok", resp.Status, resp.Detail)
	}
	if _, ok := h.pforwards().get(resp.ForwardId); !ok {
		t.Fatal("link not registered")
	}
	rrs := runnerRequests(t, runnerConn)
	if len(rrs) == 0 || rrs[0].Kind != protocol.RunnerRequestType_OpenPortForward {
		t.Fatalf("runner requests = %+v", rrs)
	}
	body := rrs[0].OpenPortForward()
	if body.Direction != protocol.PortForwardDirection_Link || body.Protocol != protocol.ForwardProtocol_Ip {
		t.Fatalf("runner request shape = %v × %v", body.Protocol, body.Direction)
	}
	got, want := body.Link(), req.Link()
	if got == nil || !bytes.Equal(got.ClientAddr, want.ClientAddr) || !bytes.Equal(got.RunnerAddr, want.RunnerAddr) ||
		got.PrefixLen != want.PrefixLen || got.Mtu != want.Mtu {
		t.Fatalf("runner got link %+v, want %+v", got, want)
	}
}

func TestRegisterIPLink_SetupFailedCarriesDetail(t *testing.T) {
	h, clientConn, runnerConn, ctrl, req := linkFixture(t)
	resp := runSetupRegister(t, h, clientConn, req, runnerConn, false, "TUNSETIFF: operation not permitted")
	if resp.Status != protocol.OpenPortForwardStatus_LinkFailed {
		t.Fatalf("status = %v, want LinkFailed", resp.Status)
	}
	if got := string(resp.Detail); got != "runner: TUNSETIFF: operation not permitted" {
		t.Fatalf("detail = %q", got)
	}
	if n := len(h.pforwards().list()); n != 0 {
		t.Fatalf("%d registrations left behind", n)
	}
	if !ctrl.Ended() {
		t.Fatal("control stream not closed")
	}
	var closed bool
	for _, rr := range runnerRequests(t, runnerConn) {
		if rr.Kind == protocol.RunnerRequestType_ClosePortForward && rr.ClosePortForward().ForwardId == 1 {
			closed = true
		}
	}
	if !closed {
		t.Fatal("no ClosePortForward sent after a failed setup")
	}
}

func TestRegisterRemoteForward_BindFailedCarriesDetail(t *testing.T) {
	h, clientConn, runnerConn, _, _ := linkFixture(t)
	req := &protocol.RegisterPortForwardRequest{
		TaskId:     protocol.TaskID{Id: [16]byte{0x6C}},
		Direction:  protocol.PortForwardDirection_Remote,
		TargetPort: 5432,
		BindPort:   15432,
	}
	req.SetBindAddr([]byte("127.0.0.1"))
	req.SetTargetHost([]byte("127.0.0.1"))
	resp := runSetupRegister(t, h, clientConn, req, runnerConn, false,
		"listen tcp 127.0.0.1:15432: bind: address already in use")
	if resp.Status != protocol.OpenPortForwardStatus_BindFailed {
		t.Fatalf("status = %v, want BindFailed", resp.Status)
	}
	if got := string(resp.Detail); got != "runner: listen tcp 127.0.0.1:15432: bind: address already in use" {
		t.Fatalf("detail = %q", got)
	}
}

func TestRegisterIPLink_InvalidConfigRefused(t *testing.T) {
	h, clientConn, runnerConn, _, req := linkFixture(t)
	bad := *req.Link()
	bad.PrefixLen = 32
	req.SetLink(bad)
	resp := h.handleRegisterPortForward(clientConn, req, clientConn.ConnectionID().String())
	if resp.Status != protocol.OpenPortForwardStatus_InternalError {
		t.Fatalf("status = %v, want InternalError", resp.Status)
	}
	if !strings.HasPrefix(string(resp.Detail), "invalid ip link: ") {
		t.Fatalf("detail = %q", resp.Detail)
	}
	if len(runnerConn.Sent()) != 0 {
		t.Fatal("the runner was asked about an invalid link")
	}
}

func TestRegisterRefusesIllegalShape(t *testing.T) {
	for _, shape := range []struct {
		p protocol.ForwardProtocol
		d protocol.PortForwardDirection
	}{
		{protocol.ForwardProtocol_Tcp, protocol.PortForwardDirection_Link},
		{protocol.ForwardProtocol_Ip, protocol.PortForwardDirection_Local},
	} {
		h, clientConn, runnerConn, _, _ := linkFixture(t)
		req := &protocol.RegisterPortForwardRequest{
			TaskId: protocol.TaskID{Id: [16]byte{0x6C}}, Direction: shape.d, Protocol: shape.p,
		}
		if shape.p == protocol.ForwardProtocol_Ip {
			req.SetLink(testLinkConfig(t, "10.200.0.1/30", "10.200.0.2"))
		}
		resp := h.handleRegisterPortForward(clientConn, req, clientConn.ConnectionID().String())
		if resp.Status != protocol.OpenPortForwardStatus_InternalError {
			t.Errorf("%v × %v: status = %v, want InternalError", shape.p, shape.d, resp.Status)
		}
		if len(runnerConn.Sent()) != 0 {
			t.Errorf("%v × %v: the runner was contacted", shape.p, shape.d)
		}
	}
}
