package runner

import (
	"net/netip"
	"testing"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestProxyHandlerCollisionDetection(t *testing.T) {
	const sharedID = 0x1234
	serverCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort("127.0.0.1:8539"), sharedID)
	agentCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort("127.0.0.1:55001"), sharedID)

	st := &proxyHandlerState{
		serverCID:     serverCID,
		hasServerConn: true,
		taskExists:    func(taskID protocol.TaskID) bool { return true },
	}

	var taskID protocol.TaskID
	resp := st.validateProxyRequest(agentCID, taskID)
	if resp.Status != protocol.ProxyEstablishStatus_IdCollision {
		t.Errorf("status: got %v want IdCollision", resp.Status)
	}
}

func TestProxyHandlerUnknownTask(t *testing.T) {
	serverCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort("127.0.0.1:8539"), 0x0001)
	agentCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort("127.0.0.1:55001"), 0x0002)

	st := &proxyHandlerState{
		serverCID:     serverCID,
		hasServerConn: true,
		taskExists:    func(taskID protocol.TaskID) bool { return false },
	}

	var taskID protocol.TaskID
	taskID.Id[0] = 0xAA
	resp := st.validateProxyRequest(agentCID, taskID)
	if resp.Status != protocol.ProxyEstablishStatus_UnknownTask {
		t.Errorf("status: got %v want UnknownTask", resp.Status)
	}
}

func TestProxyHandlerServerNotConnected(t *testing.T) {
	serverCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort("127.0.0.1:8539"), 0x0001)
	agentCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort("127.0.0.1:55001"), 0x0002)

	st := &proxyHandlerState{
		serverCID:     serverCID,
		hasServerConn: false,
		taskExists:    func(taskID protocol.TaskID) bool { return true },
	}

	var taskID protocol.TaskID
	resp := st.validateProxyRequest(agentCID, taskID)
	if resp.Status != protocol.ProxyEstablishStatus_ServerNotConnected {
		t.Errorf("status: got %v want ServerNotConnected", resp.Status)
	}
}

func TestProxyHandlerHappyPath(t *testing.T) {
	serverCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort("127.0.0.1:8539"), 0x0001)
	agentCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort("127.0.0.1:55001"), 0x0002)

	st := &proxyHandlerState{
		serverCID:     serverCID,
		hasServerConn: true,
		taskExists:    func(taskID protocol.TaskID) bool { return true },
	}

	var taskID protocol.TaskID
	resp := st.validateProxyRequest(agentCID, taskID)
	if resp.Status != protocol.ProxyEstablishStatus_Ok {
		t.Errorf("status: got %v want Ok", resp.Status)
	}

	alloc := st.allocateCID(agentCID)
	if alloc.Transport != serverCID.Transport {
		t.Errorf("alloc.Transport: got %q want %q", alloc.Transport, serverCID.Transport)
	}
	if alloc.Addr != serverCID.Addr {
		t.Errorf("alloc.Addr: got %v want %v", alloc.Addr, serverCID.Addr)
	}
	if alloc.ID != agentCID.ID {
		t.Errorf("alloc.ID: got %v want %v", alloc.ID, agentCID.ID)
	}
}
