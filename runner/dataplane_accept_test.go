package runner

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func storedListGrant(t *testing.T, sess *Session, id byte) {
	t.Helper()
	sess.ensureGrants().Insert(protocol.DataPlaneGrant{
		GrantId:       [16]uint8{id},
		TaskId:        protocol.TaskID{Id: [16]uint8{7}},
		ExpiresUnixMs: uint64(time.Now().Add(time.Minute).UnixMilli()),
		Kind:          protocol.TaskControlKind_ListFiles,
	}, 0x50, 0)
}

func TestValidateDataPlaneHelloUnknownGrant(t *testing.T) {
	sess := &Session{Grants: newGrantStore()}
	grant := protocol.DataPlaneGrant{GrantId: [16]uint8{3}, TaskId: protocol.TaskID{Id: [16]uint8{7}}}
	got := validateDataPlaneHello(sess, grant, protocol.TaskControlKind_ListFiles, 0, time.Now())
	if got != protocol.ClientHelloStatus_BadTicket {
		t.Fatalf("want bad_ticket, got %v", got)
	}
}

func TestValidateDataPlaneHelloAcceptsStoredGrant(t *testing.T) {
	sess := &Session{Grants: newGrantStore()}
	storedListGrant(t, sess, 3)
	grant := protocol.DataPlaneGrant{GrantId: [16]uint8{3}, TaskId: protocol.TaskID{Id: [16]uint8{7}}}
	got := validateDataPlaneHello(sess, grant, protocol.TaskControlKind_ListFiles, 0, time.Now())
	if got != protocol.ClientHelloStatus_Ok {
		t.Fatalf("want ok, got %v", got)
	}
}

// No server conn means no grant can ever have been pushed. Answering
// unknown_task keeps that indistinguishable from a task this runner does not
// have, rather than disclosing which of the two it is.
func TestValidateDataPlaneHelloWithNoSession(t *testing.T) {
	got := validateDataPlaneHello(nil, protocol.DataPlaneGrant{}, protocol.TaskControlKind_ListFiles, 0, time.Now())
	if got != protocol.ClientHelloStatus_UnknownTask {
		t.Fatalf("want unknown_task, got %v", got)
	}
}

// The hello an old client sent: role=0 (client_legacy), kind=data_plane, the
// grant id as a value. The role is retired, so it no longer decodes.
func TestCheckDataPlaneHelloRefusesTheRetiredRole(t *testing.T) {
	data := []byte{byte(appwire.AppKind_PskAuth), 0x00, 0x00, 0x00, byte(protocol.ClientKind_DataPlane)}
	data = append(data, make([]byte, 32)...) // grant_id, task_id
	if _, st := checkDataPlaneHello(data, nil, newGrantStore()); st != protocol.PskAuthStatus_NoIdentity {
		t.Fatalf("want no_identity, got %v", st)
	}
}

// Every other client kind is refused here: this gate accepts exactly one thing,
// and an operator surface must never reach a runner directly.
func TestCheckDataPlaneHelloRefusesOtherClientKinds(t *testing.T) {
	psk := []byte("secret")
	transcript := []byte("transcript-bytes")
	binder, _ := cli.ComputePSKBinder(psk, transcript)
	var req protocol.PskAuthRequest
	req.Binder = binder
	req.BinderLen = uint16(len(binder))
	req.Role = protocol.AuthRole_Client
	req.SetClientHello(protocol.ClientHello{Kind: protocol.ClientKind_Cli})
	data := req.MustAppend([]byte{byte(appwire.AppKind_PskAuth)})

	_, st := checkDataPlaneHello(data, transcript, nil)
	if st != protocol.PskAuthStatus_NoIdentity {
		t.Fatalf("want no_identity for kind=cli, got %v", st)
	}
}

func TestCheckDataPlaneHelloRefusesNonPskFirstMessage(t *testing.T) {
	_, st := checkDataPlaneHello([]byte{byte(appwire.AppKind_RunnerControl), 1, 2}, nil, nil)
	if st != protocol.PskAuthStatus_NoIdentity {
		t.Fatalf("want no_identity, got %v", st)
	}
}
