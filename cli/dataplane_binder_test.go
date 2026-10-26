package cli

import (
	"bytes"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The grant proves itself through the binder and never goes on the wire.
func TestDataPlaneHelloCarriesNoGrantID(t *testing.T) {
	grant := [16]byte{0xca, 0xfe, 0xba, 0xbe, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	tr := []byte("this connection")
	data, err := buildDataPlaneHello(tr, dataPlaneTarget{GrantID: grant, TaskID: protocol.TaskID{Id: [16]byte{7}}})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, grant[:]) {
		t.Fatal("the grant id is on the wire")
	}
	var req protocol.PskAuthRequest
	if err := req.DecodeExact(data[1:]); err != nil {
		t.Fatal(err)
	}
	if req.Role != protocol.AuthRole_Client || req.ClientHello() == nil || req.ClientHello().Kind != protocol.ClientKind_DataPlane {
		t.Fatalf("role=%v hello=%+v", req.Role, req.ClientHello())
	}
	want, _ := ComputePSKBinder(grant[:], tr)
	if !bytes.Equal(req.Binder, want) {
		t.Fatal("the binder is not keyed by the grant over this transcript")
	}
}
