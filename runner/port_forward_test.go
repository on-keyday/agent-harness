package runner

import (
	"context"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// fakeStreamCreator hands out a noopBidiStream with a fixed id, standing in for
// pc.Transport() in remote port-forward tests.
type fakeStreamCreator struct{ id trsf.StreamID }

func (f *fakeStreamCreator) CreateBidirectionalStream() trsf.BidirectionalStream {
	return &noopBidiStream{streamID: f.id}
}

// TestRemoteForwardListener_AcceptsAndNotifies verifies the runner's
// remote-forward listener: each accepted connection creates a stream and is
// announced to the server via RunnerMessage{RemoteForwardConn}.
func TestRemoteForwardListener_AcceptsAndNotifies(t *testing.T) {
	ms := &mockSender{}
	s := &Session{Sender: ms, creator: &fakeStreamCreator{id: 4242}, Now: time.Now}

	ln, err := s.startRemoteForwardListener(context.Background(), 7, "127.0.0.1", 0)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	deadline := time.Now().Add(2 * time.Second)
	var found *protocol.RemoteForwardConn
	for time.Now().Before(deadline) {
		ms.mu.Lock()
		msgs := append([][]byte{}, ms.sent...)
		ms.mu.Unlock()
		for _, raw := range msgs {
			m := decodeRunnerMsg(t, raw)
			if m.Kind == protocol.RunnerMessageType_RemoteForwardConn {
				found = m.RemoteForwardConn()
				break
			}
		}
		if found != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if found == nil {
		t.Fatal("no RemoteForwardConn sent after a connection arrived")
	}
	if found.ForwardId != 7 || found.StreamId != 4242 {
		t.Fatalf("RemoteForwardConn = %+v, want forwardId 7 streamId 4242", found)
	}
}

// TestCloseAllRemoteForwardListeners verifies closeAll releases every tracked
// listener — the connection-teardown path, so a server reconnect (which builds
// a fresh Session) does not leak the previous Session's bound ports.
func TestCloseAllRemoteForwardListeners(t *testing.T) {
	s := &Session{Now: time.Now}
	addrs := make([]string, 0, 3)
	for i, fid := range []uint64{1, 2, 3} {
		ln, err := s.startRemoteForwardListener(context.Background(), fid, "127.0.0.1", 0)
		if err != nil {
			t.Fatalf("listen %d: %v", i, err)
		}
		s.rforwardListeners().add(fid, ln)
		addrs = append(addrs, ln.Addr().String())
	}

	s.rforwardListeners().closeAll()

	for _, addr := range addrs {
		deadline := time.Now().Add(time.Second)
		closed := false
		for time.Now().Before(deadline) {
			c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
			if err != nil {
				closed = true
				break
			}
			_ = c.Close()
			time.Sleep(10 * time.Millisecond)
		}
		if !closed {
			t.Fatalf("listener %s still accepting after closeAll", addr)
		}
	}
}

// TestCloseRemoteForwardListener verifies the listener registry closes the
// tracked listener (so a subsequent accept fails).
func TestCloseRemoteForwardListener(t *testing.T) {
	s := &Session{Now: time.Now}
	ln, err := s.startRemoteForwardListener(context.Background(), 9, "127.0.0.1", 0)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s.rforwardListeners().add(9, ln)
	addr := ln.Addr().String()
	s.rforwardListeners().close(9)

	// The listener is closed: a dial should fail (no acceptor) — give the OS a
	// moment to release the port.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			return // closed as expected
		}
		_ = c.Close()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("listener still accepting after close")
}

// The ClosePortForward dispatch asks for the -R registry -- for every close,
// udp and link ones included -- while RunHandle.Close releases it on another
// goroutine. CI's -race run caught this in TestUDPRemoteForwardE2E.
func TestRemoteForwardCloseRacesRelease(t *testing.T) {
	s := &Session{}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.rforwardListeners().close(1) }()
	go func() { defer wg.Done(); s.releaseForwardState() }()
	wg.Wait()
}

// The registry is created once however many goroutines ask first.
func TestRemoteForwardRegistryInitIsRaceFree(t *testing.T) {
	s := &Session{}
	const n = 16
	got := make([]*remoteForwardListeners, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); got[i] = s.rforwardListeners() }(i)
	}
	wg.Wait()
	for i := 1; i < n; i++ {
		if got[i] != got[0] {
			t.Fatal("two registries were created")
		}
	}
}

// freePort returns a port nothing is bound to on 127.0.0.1 for network
// ("tcp" or "udp"), so a test can name the port a forward will bind and then
// check whether it was released.
func freePort(t *testing.T, network string) uint16 {
	t.Helper()
	switch network {
	case "tcp":
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		return uint16(ln.Addr().(*net.TCPAddr).Port)
	default:
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer pc.Close()
		return uint16(pc.LocalAddr().(*net.UDPAddr).Port)
	}
}

// The connection ended (RunHandle.Close -> releaseForwardState) while a -R
// was being set up: its listener must not be registered on the dead session,
// where nothing would ever close it and the port would stay bound.
func TestRemoteForwardSetupAfterReleaseIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		proto   protocol.ForwardProtocol
		network string
	}{
		{"tcp", protocol.ForwardProtocol_Tcp, "tcp"},
		{"udp", protocol.ForwardProtocol_Udp, "udp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ms, tid, _ := linkSession(t, newFakeTun(), nil)
			s.creator = &fakeStreamCreator{id: 1}
			s.udpForwards = &udpForwards{} // as driveAfterConn creates it
			s.releaseForwardState()

			port := freePort(t, tc.network)
			req := &protocol.RunnerOpenPortForwardRequest{TaskId: tid, Direction: protocol.PortForwardDirection_Remote,
				Protocol: tc.proto, ForwardId: 8, BindPort: port}
			req.SetBindAddr([]byte("127.0.0.1"))
			s.handleOpenPortForward(context.Background(), req)

			res := setupResults(t, ms)
			if len(res) != 1 || res[0].Ok() || string(res[0].Detail) != "connection ended during setup" {
				t.Fatalf("setup results = %+v", res)
			}
			addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))
			if tc.network == "tcp" {
				ln, err := net.Listen("tcp", addr)
				if err != nil {
					t.Fatalf("the -R listener opened for a dead session was left bound: %v", err)
				}
				ln.Close()
			} else {
				pc, err := net.ListenPacket("udp", addr)
				if err != nil {
					t.Fatalf("the udp -R socket opened for a dead session was left bound: %v", err)
				}
				pc.Close()
			}
		})
	}
}
