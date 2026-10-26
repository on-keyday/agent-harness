package runner

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func grantFor(id byte, task byte) protocol.DataPlaneGrant {
	return protocol.DataPlaneGrant{
		GrantId:       [16]uint8{id},
		TaskId:        protocol.TaskID{Id: [16]uint8{task}},
		ExpiresUnixMs: uint64(time.Now().Add(time.Minute).UnixMilli()),
		Kind:          protocol.TaskControlKind_ListFiles,
	}
}

func boundDataPlaneHello(t *testing.T, grantID [16]byte, task byte, transcript []byte) []byte {
	t.Helper()
	req := protocol.PskAuthRequest{Role: protocol.AuthRole_Client}
	binder, err := cli.ComputePSKBinder(grantID[:], transcript)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBinder(binder)
	hello := protocol.ClientHello{Kind: protocol.ClientKind_DataPlane}
	hello.SetDataPlaneInfo(protocol.DataPlaneInfo{TaskId: protocol.TaskID{Id: [16]uint8{task}}})
	req.SetClientHello(hello)
	return req.MustAppend([]byte{byte(appwire.AppKind_PskAuth)})
}

// A binder keyed by a grant is good on the connection it was made for only.
func TestDataPlaneBinderFromAnotherConnectionIsRefused(t *testing.T) {
	g := newGrantStore()
	g.Insert(grantFor(3, 7), 0x50, 0)
	data := boundDataPlaneHello(t, [16]byte{3}, 7, []byte("connection A"))
	if _, st := checkDataPlaneHello(data, []byte("connection B"), nil, g); st != protocol.PskAuthStatus_BadTicket {
		t.Fatalf("want bad_ticket, got %v", st)
	}
}

func TestDataPlaneBinderPicksTheMatchingGrant(t *testing.T) {
	g := newGrantStore()
	g.Insert(grantFor(3, 7), 0x50, 0)
	g.Insert(grantFor(4, 7), 0x51, 0)
	tr := []byte("this connection")
	info, st := checkDataPlaneHello(boundDataPlaneHello(t, [16]byte{4}, 7, tr), tr, nil, g)
	if st != protocol.PskAuthStatus_Ok || info == nil || info.GrantId != ([16]uint8{4}) {
		t.Fatalf("status %v info %+v, want ok with grant 4", st, info)
	}
}

// The binder is only tried against the task the hello names: a grant of
// another task does not answer for it.
func TestDataPlaneBinderIgnoresOtherTasksGrants(t *testing.T) {
	g := newGrantStore()
	g.Insert(grantFor(3, 8), 0x50, 0)
	tr := []byte("this connection")
	if _, st := checkDataPlaneHello(boundDataPlaneHello(t, [16]byte{3}, 7, tr), tr, nil, g); st != protocol.PskAuthStatus_BadTicket {
		t.Fatalf("want bad_ticket, got %v", st)
	}
}

func TestLegacyDataPlaneHelloStillAccepted(t *testing.T) {
	tr := []byte("transcript-bytes")
	info, st := checkDataPlaneHello(dataPlaneHelloBytes(t, nil, tr, 3), tr, nil, newGrantStore())
	if st != protocol.PskAuthStatus_Ok || info == nil || info.GrantId != ([16]uint8{3}) {
		t.Fatalf("legacy hello: status %v info %+v", st, info)
	}
}
