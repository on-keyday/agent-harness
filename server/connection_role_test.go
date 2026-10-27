package server

import (
	"crypto/ed25519"
	"testing"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestPSKDispatchIdentityRejectsFailedPrincipalRegistration(t *testing.T) {
	d := &Dispatcher{RecordClientIdentity: func(string, ConnHandle, *protocol.ClientHello) protocol.ClientHelloStatus {
		return protocol.ClientHelloStatus_BadTicket
	}}
	conn := &fakeConn{id: tcid("rejected-principal")}
	if pskDispatchIdentity(d, conn, buildAgentBinderReq([16]byte{}, testTranscript, protocol.RunnerID{}, protocol.TaskID{})) {
		t.Fatal("a revoked ticket was accepted as an unidentified operator")
	}
}

func TestAuthenticatedConnectionRoles(t *testing.T) {
	var taskID protocol.TaskID
	taskID.Id[0] = 1
	rid, ticket := testRunnerID("role-test"), [16]byte{7}
	pub, priv, _ := ed25519.GenerateKey(nil)
	roles := []struct {
		name string
		req  *protocol.PskAuthRequest
	}{
		{"operator", buildOperatorClientHello(nil, testTranscript)},
		{"agent", buildAgentBinderReq(ticket, testTranscript, rid, taskID)},
		{"runner", buildRunnerAuthReq(priv, testTranscript, protocol.RunnerHello{Version: 1, RunnerId: rid})},
	}
	for _, role := range roles {
		t.Run(role.name, func(t *testing.T) {
			g := gateWithKeys(t, pub)
			g.TicketFor = agentGate(rid, taskID, ticket).TicketFor
			_, closeConn, accepted := g.Check(buildPskAuthData(t, role.req), testTranscript, func([]byte) {})
			if closeConn || accepted == nil {
				t.Fatal("initial handshake rejected")
			}
			client := role.req.Role == protocol.AuthRole_Client
			for _, tc := range []struct {
				name string
				data []byte
				want bool
			}{
				{"task control", []byte{byte(appwire.AppKind_TaskControl), byte(protocol.TaskControlKind_Cancel)}, client},
				{"runner report", []byte{byte(appwire.AppKind_RunnerControl), byte(protocol.RunnerMessageType_TaskFinished)}, !client},
				{"task log pubsub", []byte{byte(appwire.AppKind_Pubsub)}, true},
				{"telemetry response", []byte{byte(appwire.AppKind_Telemetry)}, true},
				{"replace client identity", []byte{byte(appwire.AppKind_TaskControl), byte(protocol.TaskControlKind_ClientHello)}, false},
				{"replace runner identity", []byte{byte(appwire.AppKind_RunnerControl), byte(protocol.RunnerMessageType_Hello)}, false},
				{"truncated task control", []byte{byte(appwire.AppKind_TaskControl)}, false},
				{"truncated runner control", []byte{byte(appwire.AppKind_RunnerControl)}, false},
				{"unknown family", []byte{0xff}, false},
				{"empty", nil, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if got := g.AllowsMessage(tc.data); got != tc.want {
						t.Errorf("AllowsMessage = %v, want %v", got, tc.want)
					}
				})
			}
		})
	}
}

func TestUnauthenticatedConnectionHasNoRoleAuthority(t *testing.T) {
	g := newPSKGate()
	for _, kind := range []appwire.AppKind{appwire.AppKind_TaskControl, appwire.AppKind_RunnerControl, appwire.AppKind_Pubsub, appwire.AppKind_Telemetry} {
		if g.AllowsMessage([]byte{byte(kind), 1}) {
			t.Errorf("unauthenticated connection allowed %v", kind)
		}
	}
	// No TicketFor wired: every role=client agent is refused.
	req := buildAgentBinderReq([16]byte{}, testTranscript, protocol.RunnerID{}, protocol.TaskID{})
	_, closeConn, accepted := g.Check(buildPskAuthData(t, req), testTranscript, func([]byte) {})
	if !closeConn || accepted != nil || g.AllowsMessage([]byte{byte(appwire.AppKind_TaskControl), byte(protocol.TaskControlKind_List)}) {
		t.Fatal("rejected handshake granted application authority")
	}
}
