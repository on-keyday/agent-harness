package server

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// closedRecords decodes every PortForwardEvent written to a control stream and
// returns the closed ones.
func closedRecords(t *testing.T, ctrl *recordingBidiStream) []protocol.PortForwardClosed {
	t.Helper()
	var out []protocol.PortForwardClosed
	buf := ctrl.Written()
	for len(buf) > 0 {
		var ev protocol.PortForwardEvent
		rest, err := ev.Decode(buf)
		if err != nil {
			t.Fatalf("decode control record: %v", err)
		}
		if ev.Kind == protocol.PortForwardEventKind_Closed {
			out = append(out, *ev.Closed())
		}
		buf = rest
	}
	return out
}

func closePortForwardIDs(t *testing.T, c *fakeConn) []uint64 {
	t.Helper()
	var ids []uint64
	for _, rr := range runnerRequests(t, c) {
		if rr.Kind == protocol.RunnerRequestType_ClosePortForward {
			ids = append(ids, rr.ClosePortForward().ForwardId)
		}
	}
	return ids
}

// stateFixture is a running task on runner-1 whose connection is runnerConn.
func stateFixture(t *testing.T) (*TaskHandler, string, *fakeConn) {
	t.Helper()
	h := &TaskHandler{Tasks: NewTaskStore(), Registry: NewRegistry()}
	taskHex := addRunningTask(t, h, 0x72, "runner-1")
	runnerConn := &fakeConn{id: tcid("runner-1")}
	h.Registry.Add(&RunnerEntry{ID: tcid("runner-1"), Identity: testRunnerID("runner-1"), Conn: runnerConn})
	return h, taskHex, runnerConn
}

func TestTeardownTellsRunnerWhenItHoldsState(t *testing.T) {
	cases := []struct {
		name string
		p    protocol.ForwardProtocol
		d    protocol.PortForwardDirection
		want bool
	}{
		{"ip link", protocol.ForwardProtocol_Ip, protocol.PortForwardDirection_Link, true},
		{"udp -L", protocol.ForwardProtocol_Udp, protocol.PortForwardDirection_Local, true},
		{"udp -R", protocol.ForwardProtocol_Udp, protocol.PortForwardDirection_Remote, true},
		{"tcp -R", protocol.ForwardProtocol_Tcp, protocol.PortForwardDirection_Remote, true},
		{"tcp -L", protocol.ForwardProtocol_Tcp, protocol.PortForwardDirection_Local, false},
	}
	for _, c := range cases {
		h, taskHex, runnerConn := stateFixture(t)
		pf := &portForward{direction: c.d, protocolKind: c.p, taskIDHex: taskHex,
			runnerID: testRunnerID("runner-1"), control: newRecordingBidiStream(1)}
		id := h.pforwards().add(pf)
		h.closePortForward(pf, protocol.PortForwardCloseReason_Killed, "")
		ids := closePortForwardIDs(t, runnerConn)
		got := len(ids) == 1 && ids[0] == id
		if got != c.want {
			t.Errorf("%s: ClosePortForward sent = %v (%v), want %v", c.name, got, ids, c.want)
		}
	}
}

func TestDropRunnerStateForwards(t *testing.T) {
	h, taskHex, _ := stateFixture(t)
	mk := func(p protocol.ForwardProtocol, d protocol.PortForwardDirection, runnerCID string) (*portForward, *recordingBidiStream) {
		ctrl := newRecordingBidiStream(1)
		pf := &portForward{direction: d, protocolKind: p, taskIDHex: taskHex,
			runnerID: testRunnerID("runner-1"), runnerCID: runnerCID, control: ctrl}
		h.pforwards().add(pf)
		return pf, ctrl
	}
	link, linkCtrl := mk(protocol.ForwardProtocol_Ip, protocol.PortForwardDirection_Link, "A")
	udpL, udpLCtrl := mk(protocol.ForwardProtocol_Udp, protocol.PortForwardDirection_Local, "A")
	tcpR, tcpRCtrl := mk(protocol.ForwardProtocol_Tcp, protocol.PortForwardDirection_Remote, "A")
	tcpL, tcpLCtrl := mk(protocol.ForwardProtocol_Tcp, protocol.PortForwardDirection_Local, "")
	other, _ := mk(protocol.ForwardProtocol_Ip, protocol.PortForwardDirection_Link, "B")

	h.DropRunnerStateForwards("A")

	for _, gone := range []struct {
		pf   *portForward
		ctrl *recordingBidiStream
	}{{link, linkCtrl}, {udpL, udpLCtrl}, {tcpR, tcpRCtrl}} {
		if _, ok := h.pforwards().get(gone.pf.forwardID); ok {
			t.Errorf("forward %d (%v × %v) survived its runner connection", gone.pf.forwardID, gone.pf.protocolKind, gone.pf.direction)
		}
		recs := closedRecords(t, gone.ctrl)
		if len(recs) != 1 || recs[0].Reason != protocol.PortForwardCloseReason_RunnerGone || len(recs[0].Detail) != 0 {
			t.Errorf("forward %d: closed records = %+v, want one runner_gone", gone.pf.forwardID, recs)
		}
	}
	for _, kept := range []*portForward{tcpL, other} {
		if _, ok := h.pforwards().get(kept.forwardID); !ok {
			t.Errorf("forward %d should have survived", kept.forwardID)
		}
	}
	if len(tcpLCtrl.Written()) != 0 {
		t.Error("tcp -L was told something")
	}
}

func TestRunnerDisplacementCallsOnRunnerDisplaced(t *testing.T) {
	var displaced []objproto.ConnectionID
	h := &RunnerHandler{Registry: NewRegistry(), Tasks: NewTaskStore(), Now: time.Now,
		OnRunnerDisplaced: func(id objproto.ConnectionID) { displaced = append(displaced, id) }}
	hello := func(conn *fakeConn) {
		msg := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_Hello}
		hl := protocol.RunnerHello{Version: 1, MaxTasks: 1, RunnerId: testRunnerID("runner-1")}
		msg.SetHello(hl)
		h.Handle(conn, encodeRunnerMessage(t, msg))
	}
	a := &fakeConn{id: tcid("conn-a")}
	b := &fakeConn{id: tcid("conn-b")}
	hello(a)
	if len(displaced) != 0 {
		t.Fatalf("a first hello displaced %v", displaced)
	}
	hello(b)
	if len(displaced) != 1 || displaced[0] != a.ConnectionID() {
		t.Fatalf("displaced = %v, want [%v]", displaced, a.ConnectionID())
	}
}

func linkRowOn(t *testing.T, h *TaskHandler, taskHex string, runnerConn *fakeConn) (*portForward, *recordingBidiStream) {
	t.Helper()
	ctrl := newRecordingBidiStream(1)
	pf := &portForward{direction: protocol.PortForwardDirection_Link, protocolKind: protocol.ForwardProtocol_Ip,
		taskIDHex: taskHex, runnerID: testRunnerID("runner-1"),
		runnerCID: runnerConn.ConnectionID().String(), control: ctrl}
	h.pforwards().add(pf)
	return pf, ctrl
}

func TestForwardEndedTearsDownWithDetail(t *testing.T) {
	h, taskHex, runnerConn := stateFixture(t)
	pf, ctrl := linkRowOn(t, h, taskHex, runnerConn)
	msg := protocol.ForwardEnded{ForwardId: pf.forwardID}
	msg.SetDetail([]byte("read: file descriptor in bad state"))
	h.handleForwardEnded(runnerConn, &msg)
	if _, ok := h.pforwards().get(pf.forwardID); ok {
		t.Fatal("the row survived ForwardEnded")
	}
	recs := closedRecords(t, ctrl)
	if len(recs) != 1 || recs[0].Reason != protocol.PortForwardCloseReason_EndpointFailed ||
		string(recs[0].Detail) != "runner: read: file descriptor in bad state" {
		t.Fatalf("closed records = %+v", recs)
	}
}

// Review Focus 2: only the connection holding the forward may end it.
func TestForwardEndedFromOtherConnIgnored(t *testing.T) {
	h, taskHex, runnerConn := stateFixture(t)
	pf, ctrl := linkRowOn(t, h, taskHex, runnerConn)
	msg := protocol.ForwardEnded{ForwardId: pf.forwardID}
	msg.SetDetail([]byte("forged"))
	h.handleForwardEnded(&fakeConn{id: tcid("someone-else")}, &msg)
	if _, ok := h.pforwards().get(pf.forwardID); !ok {
		t.Fatal("a connection that does not hold the forward ended it")
	}
	if len(ctrl.Written()) != 0 {
		t.Fatal("the owner was told something")
	}
}

func TestTapForwardClosedCarriesDetail(t *testing.T) {
	pf := newCounterForward()
	tap, recs := newTestTap(t, pf, protocol.ForwardTapFilter_Both, 0)
	defer pf.removeTap(tap)
	pf.closeTaps(protocol.PortForwardCloseReason_EndpointFailed, "runner: boom")
	got := drain(t, recs, 1)
	fc := got[0].ForwardClosed()
	if fc == nil || fc.Reason != protocol.PortForwardCloseReason_EndpointFailed || string(fc.Detail) != "runner: boom" {
		t.Fatalf("forward_closed = %+v", fc)
	}
}
