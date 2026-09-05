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
	info := protocol.DataPlaneInfo{GrantId: [16]uint8{3}, TaskId: protocol.TaskID{Id: [16]uint8{7}}}
	got := validateDataPlaneHello(sess, info, protocol.TaskControlKind_ListFiles, 0, time.Now())
	if got != protocol.ClientHelloStatus_BadTicket {
		t.Fatalf("want bad_ticket, got %v", got)
	}
}

func TestValidateDataPlaneHelloAcceptsStoredGrant(t *testing.T) {
	sess := &Session{Grants: newGrantStore()}
	storedListGrant(t, sess, 3)
	info := protocol.DataPlaneInfo{GrantId: [16]uint8{3}, TaskId: protocol.TaskID{Id: [16]uint8{7}}}
	got := validateDataPlaneHello(sess, info, protocol.TaskControlKind_ListFiles, 0, time.Now())
	if got != protocol.ClientHelloStatus_Ok {
		t.Fatalf("want ok, got %v", got)
	}
}

// No server conn means no grant can ever have been pushed. Answering
// unknown_task keeps that indistinguishable from a task this runner does not
// have, rather than disclosing which of the two it is.
func TestValidateDataPlaneHelloWithNoSession(t *testing.T) {
	got := validateDataPlaneHello(nil, protocol.DataPlaneInfo{}, protocol.TaskControlKind_ListFiles, 0, time.Now())
	if got != protocol.ClientHelloStatus_UnknownTask {
		t.Fatalf("want unknown_task, got %v", got)
	}
}

func dataPlaneHelloBytes(t *testing.T, psk, transcript []byte, grantID byte) []byte {
	t.Helper()
	var req protocol.PskAuthRequest
	if len(psk) > 0 {
		binder, err := cli.ComputePSKBinder(psk, transcript)
		if err != nil {
			t.Fatalf("binder: %v", err)
		}
		req.Binder = binder
		req.BinderLen = uint16(len(binder))
	}
	req.Role = protocol.AuthRole_Client
	var hello protocol.ClientHello
	hello.Kind = protocol.ClientKind_DataPlane
	hello.SetDataPlaneInfo(protocol.DataPlaneInfo{
		GrantId: [16]uint8{grantID},
		TaskId:  protocol.TaskID{Id: [16]uint8{7}},
	})
	req.SetClientHello(hello)
	return req.MustAppend([]byte{byte(appwire.AppKind_PskAuth)})
}

func TestCheckDataPlaneHelloAcceptsAGoodBinder(t *testing.T) {
	psk := []byte("secret")
	transcript := []byte("transcript-bytes")
	data := dataPlaneHelloBytes(t, psk, transcript, 3)

	info, st := checkDataPlaneHello(data, transcript, psk)
	if st != protocol.PskAuthStatus_Ok {
		t.Fatalf("want ok, got %v", st)
	}
	if info == nil || info.GrantId != ([16]uint8{3}) {
		t.Fatalf("info lost: %+v", info)
	}
}

func TestCheckDataPlaneHelloRefusesABadBinder(t *testing.T) {
	data := dataPlaneHelloBytes(t, []byte("wrong-psk"), []byte("transcript-bytes"), 3)
	_, st := checkDataPlaneHello(data, []byte("transcript-bytes"), []byte("secret"))
	if st != protocol.PskAuthStatus_BadPsk {
		t.Fatalf("want bad_psk, got %v", st)
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
	var hello protocol.ClientHello
	hello.Kind = protocol.ClientKind_Cli
	req.SetClientHello(hello)
	data := req.MustAppend([]byte{byte(appwire.AppKind_PskAuth)})

	_, st := checkDataPlaneHello(data, transcript, psk)
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
