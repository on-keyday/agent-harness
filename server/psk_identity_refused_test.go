package server

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// A handshake the gate accepted but whose identity was not recorded
// (pskDispatchIdentity answered false) must leave the connection admitting
// nothing. The gate was already authed by then, and a connection with no
// principal recorded is the operator to callerCaps: a message pipelined behind
// the PskAuthRequest, still queued when the close starts, would otherwise be
// dispatched with the operator's authority.

func TestGateRefusedIdentityAdmitsNothing(t *testing.T) {
	g := newPSKGate()
	transcript := []byte("transcript")
	hello := buildPskAuthData(t, buildOperatorClientHello(nil, transcript))
	if _, _, accepted := g.Check(hello, transcript, func([]byte) {}); accepted == nil {
		t.Fatal("the gate refused an operator hello with no PSK configured")
	}
	g.RefuseIdentity()

	if g.Authed() {
		t.Error("the gate is still authed after its identity was refused")
	}
	tc := []byte{byte(appwire.AppKind_TaskControl), byte(protocol.TaskControlKind_CredentialIssue)}
	if g.AllowsMessage(tc) {
		t.Error("task control admitted after the identity was refused")
	}
	if g.AllowsMessage([]byte{byte(appwire.AppKind_Pubsub), 0}) {
		t.Error("pubsub admitted after the identity was refused")
	}
	isPSK, _, accepted := g.Check(hello, transcript, func([]byte) {})
	if isPSK || accepted != nil {
		t.Error("a second PskAuthRequest was taken as a second try")
	}
	if g.Authed() {
		t.Error("a second PskAuthRequest re-opened the gate")
	}
}

// The end-to-end falsifier: a hello the recorder refuses (a client hello
// claiming kind=credential, which RecordClientIdentity answers not_permitted)
// with a credential issue pipelined right behind it. Whether the receive loop
// hands the queued message over after the close is a random select, so the
// attempt is repeated; any one dispatch creates a credential task.
func TestPipelinedRequestBehindRefusedIdentityIsNotDispatched(t *testing.T) {
	ctx, e := startPubsubAuthzServer(t)
	psk := []byte("pubsub-authz-operator")
	_, pub := newCredentialKey(t)
	issue := protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_CredentialIssue}}
	issue.SetCredentialIssue(protocol.CredentialIssueRequest{Pubkey: pub, TtlMs: uint64(time.Hour / time.Millisecond)})
	issueBytes := issue.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})

	const attempts = 40
	for i := 0; i < attempts; i++ {
		pc, err := cli.DialPeerConn(ctx, e.cid)
		if err != nil {
			t.Fatal(err)
		}
		pc.Start(ctx)
		transcript := pc.Connection().GetTranscript()
		req := &protocol.PskAuthRequest{Role: protocol.AuthRole_Client}
		binder, _ := cli.ComputePSKBinder(psk, transcript)
		req.SetBinder(binder)
		req.SetClientHello(protocol.ClientHello{Kind: protocol.ClientKind_Credential})
		if _, _, err := pc.Connection().SendMessage(buildPskAuthData(t, req)); err != nil {
			t.Fatal(err)
		}
		for j := 0; j < 4; j++ {
			if _, _, err := pc.Connection().SendMessage(issueBytes); err != nil {
				break
			}
		}
		select {
		case <-pc.Done():
		case <-time.After(3 * time.Second):
			t.Fatal("the server did not close a connection whose identity it refused")
		}
		pc.Close()
	}
	time.Sleep(300 * time.Millisecond) // past any dispatch still running
	for _, task := range e.s.Tasks().List(0) {
		if task.Kind == protocol.TaskKind_Credential {
			t.Fatalf("a request pipelined behind a refused identity was dispatched: credential %s was issued", task.ID)
		}
	}
}
