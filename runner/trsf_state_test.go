package runner

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// fakeTransport answers the one question trsfStates asks. The embedded
// interface is nil on purpose: anything else the code reaches for is a nil
// panic naming the new call, rather than a zero value that quietly reports
// a connection as idle.
type fakeTransport struct {
	trsf.Transport
	state *trsf.InternalState
}

func (f fakeTransport) GetInternalState() *trsf.InternalState {
	if f.state != nil {
		return f.state
	}
	return &trsf.InternalState{}
}

// The runner reports every connection it holds and applies no policy of its
// own: the server decides who may see what, because it is the only party that
// evaluates scope (D7). A second implementation of that rule here is the drift
// this design has already paid for once.
func TestRunnerReportsEveryConnectionItHolds(t *testing.T) {
	s := &Session{}
	s.registerTrsfConn("udp:1.2.3.4:5-6", protocol.ConnRole_Server, fakeTransport{}, protocol.TaskID{})
	s.registerTrsfConn("udp:1.2.3.4:7-8", protocol.ConnRole_Cli, fakeTransport{}, protocol.TaskID{Id: [16]uint8{9}})

	rows := s.trsfStates()
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want both connections", len(rows))
	}
	byCID := map[string]protocol.TrsfConnState{}
	for _, r := range rows {
		byCID[string(r.Cid)] = r
	}
	up, ok := byCID["udp:1.2.3.4:5-6"]
	if !ok {
		t.Fatal("the uplink is missing")
	}
	// The uplink multiplexes every task, so it can name none -- that is the
	// fact the whole visibility rule turns on.
	if up.PrincipalTask.Id != ([16]uint8{}) {
		t.Errorf("the uplink named a task: %v", up.PrincipalTask.Id)
	}
	if up.Role != protocol.ConnRole_Server {
		t.Errorf("uplink role = %v, want server (the role names the PEER)", up.Role)
	}
	dp := byCID["udp:1.2.3.4:7-8"]
	if dp.PrincipalTask.Id != ([16]uint8{9}) {
		t.Errorf("a data-plane conn lost its task: %v", dp.PrincipalTask.Id)
	}
}

func TestRunnerForgetsAClosedConnection(t *testing.T) {
	s := &Session{}
	s.registerTrsfConn("udp:1.2.3.4:5-6", protocol.ConnRole_Server, fakeTransport{}, protocol.TaskID{})
	s.unregisterTrsfConn("udp:1.2.3.4:5-6")
	if rows := s.trsfStates(); len(rows) != 0 {
		t.Fatalf("a closed connection is still reported: %+v", rows)
	}
}

// A nil Session is what a runner has before it is wired, and the call sites
// register without guarding, so this must not panic.
func TestRunnerTrsfStateOnANilSession(t *testing.T) {
	var s *Session
	s.registerTrsfConn("x", protocol.ConnRole_Server, fakeTransport{}, protocol.TaskID{})
	s.unregisterTrsfConn("x")
	if rows := s.trsfStates(); rows != nil {
		t.Fatalf("got %+v, want nil", rows)
	}
}
