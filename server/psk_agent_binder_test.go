package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func buildAgentBinderReq(ticket [16]byte, transcript []byte, rid protocol.RunnerID, tid protocol.TaskID) *protocol.PskAuthRequest {
	req := &protocol.PskAuthRequest{Role: protocol.AuthRole_Client}
	binder, _ := cli.ComputePSKBinder(ticket[:], transcript)
	req.SetBinder(binder)
	hello := protocol.ClientHello{Kind: protocol.ClientKind_Agent}
	hello.SetAgentInfo(protocol.AgentInfo{RunnerId: rid, TaskId: tid})
	req.SetClientHello(hello)
	return req
}

func agentGate(rid protocol.RunnerID, tid protocol.TaskID, ticket [16]byte) *pskGate {
	g := newPSKGate()
	g.TicketFor = func(r protocol.RunnerID, t protocol.TaskID) ([16]byte, bool) {
		if r == rid && t == tid {
			return ticket, true
		}
		return [16]byte{}, false
	}
	return g
}

func TestGateAgentBinderAccepted(t *testing.T) {
	rid, tid, ticket := testRunnerID("r"), protocol.TaskID{Id: [16]byte{1}}, [16]byte{9, 9, 9}
	g := agentGate(rid, tid, ticket)
	var sent []byte
	_, _, accepted := g.Check(buildPskAuthData(t, buildAgentBinderReq(ticket, testTranscript, rid, tid)), testTranscript, func(b []byte) { sent = append(sent, b...) })
	if accepted == nil {
		t.Fatalf("refused: %v", parsePskResponse(t, sent))
	}
	if g.Role() != protocol.AuthRole_Client || g.Legacy() {
		t.Fatalf("role=%v legacy=%v", g.Role(), g.Legacy())
	}
}

// A binder captured on one connection is worthless on another: that is what
// sending the ticket itself could not give.
func TestAgentBinderFromAnotherConnectionIsRefused(t *testing.T) {
	rid, tid, ticket := testRunnerID("r"), protocol.TaskID{Id: [16]byte{1}}, [16]byte{9, 9, 9}
	g := agentGate(rid, tid, ticket)
	var sent []byte
	req := buildAgentBinderReq(ticket, []byte("connection A"), rid, tid)
	_, shouldClose, accepted := g.Check(buildPskAuthData(t, req), []byte("connection B"), func(b []byte) { sent = append(sent, b...) })
	if accepted != nil || !shouldClose {
		t.Fatal("a binder over another transcript was accepted")
	}
	if st := parsePskResponse(t, sent); st != protocol.PskAuthStatus_BadTicket {
		t.Fatalf("status %v, want bad_ticket", st)
	}
}

func TestGateAgentWithNoTicketForTaskIsBadTicket(t *testing.T) {
	rid, tid := testRunnerID("r"), protocol.TaskID{Id: [16]byte{1}}
	g := agentGate(rid, tid, [16]byte{1})
	var sent []byte
	req := buildAgentBinderReq([16]byte{1}, testTranscript, rid, protocol.TaskID{Id: [16]byte{2}})
	g.Check(buildPskAuthData(t, req), testTranscript, func(b []byte) { sent = append(sent, b...) })
	if st := parsePskResponse(t, sent); st != protocol.PskAuthStatus_BadTicket {
		t.Fatalf("status %v, want bad_ticket", st)
	}
}

func TestOperatorKindsUnderRoleClientProveOperatorPSK(t *testing.T) {
	g := newPSKGate()
	g.operatorPSK = []byte("op")
	for _, c := range []struct {
		key  string
		want protocol.PskAuthStatus
	}{{"op", protocol.PskAuthStatus_Ok}, {"wrong", protocol.PskAuthStatus_BadPsk}} {
		g := newPSKGate()
		g.operatorPSK = []byte("op")
		req := &protocol.PskAuthRequest{Role: protocol.AuthRole_Client}
		b, _ := cli.ComputePSKBinder([]byte(c.key), testTranscript)
		req.SetBinder(b)
		req.SetClientHello(protocol.ClientHello{Kind: protocol.ClientKind_Cli})
		var sent []byte
		g.Check(buildPskAuthData(t, req), testTranscript, func(b []byte) { sent = append(sent, b...) })
		if st := parsePskResponse(t, sent); st != c.want {
			t.Errorf("binder keyed by %q: %v, want %v", c.key, st, c.want)
		}
	}
	_ = g
}

// A task created over a legacy agent connection and one created over a new
// one must record the same principal kind: the WAL stores it as a number.
func TestLegacyAndNewAgentRecordSameOriginKind(t *testing.T) {
	rid, tid, ticket := testRunnerID("r"), protocol.TaskID{Id: [16]byte{1}}, [16]byte{9}
	legacy := buildAgentClientHello(nil, testTranscript, protocol.AgentInfoLegacy{RunnerId: rid, TaskId: tid, AuthTicket: ticket})
	fresh := buildAgentBinderReq(ticket, testTranscript, rid, tid)
	kinds := map[string]protocol.ClientKind{}
	for name, req := range map[string]*protocol.PskAuthRequest{"legacy": legacy, "new": fresh} {
		th := newTestTaskHandler(NewTaskStore())
		d := &Dispatcher{RecordClientIdentity: th.RecordClientIdentity}
		conn := &fakeConn{id: tcid("ws:127.0.0.1:8539-9")}
		if !pskDispatchIdentity(d, conn, req) {
			t.Fatalf("%s: dispatch failed", name)
		}
		kinds[name] = th.lookupClientKind(conn.id.String())
	}
	if kinds["legacy"] != protocol.ClientKind_Agent || kinds["new"] != protocol.ClientKind_Agent {
		t.Fatalf("recorded kinds %v, want agent for both", kinds)
	}
}
