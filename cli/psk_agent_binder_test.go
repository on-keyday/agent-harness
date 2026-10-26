package cli

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func setAgentTicketEnv(t *testing.T, ticket [16]byte) {
	t.Helper()
	t.Setenv("HARNESS_RUNNER_ID", "00112233445566778899aabbccddeeff")
	t.Setenv("HARNESS_TASK_ID", "ffeeddccbbaa99887766554433221100")
	t.Setenv("HARNESS_AUTH_TICKET", hex.EncodeToString(ticket[:]))
}

func captureHandshake(t *testing.T, transcript []byte, operatorKind protocol.ClientKind) protocol.PskAuthRequest {
	t.Helper()
	var sent []byte
	respCh := make(chan PskAuthReply, 1)
	respCh <- PskAuthReply{Resp: protocol.PskAuthResponse{Status: protocol.PskAuthStatus_Ok}}
	if err := SendMergedHandshake(context.Background(), func(b []byte) error { sent = b; return nil },
		resolveBinderPSK(), transcript, operatorKind, nil, respCh); err != nil {
		t.Fatal(err)
	}
	var req protocol.PskAuthRequest
	if err := req.DecodeExact(sent[1:]); err != nil {
		t.Fatal(err)
	}
	return req
}

// The ticket proves itself through the binder and never goes on the wire, so
// whoever terminates this handshake learns nothing it can replay elsewhere.
func TestAgentRoleClientCarriesNoTicket(t *testing.T) {
	ticket := [16]byte{0xde, 0xad, 0xbe, 0xef, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	setAgentTicketEnv(t, ticket)
	transcript := []byte("this connection")
	var sent []byte
	respCh := make(chan PskAuthReply, 1)
	respCh <- PskAuthReply{Resp: protocol.PskAuthResponse{Status: protocol.PskAuthStatus_Ok}}
	if err := SendMergedHandshake(context.Background(), func(b []byte) error { sent = b; return nil },
		resolveBinderPSK(), transcript, protocol.ClientKind_Cli, nil, respCh); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sent, ticket[:]) {
		t.Fatal("the ticket bytes are on the wire")
	}
	var req protocol.PskAuthRequest
	if err := req.DecodeExact(sent[1:]); err != nil {
		t.Fatal(err)
	}
	if req.Role != protocol.AuthRole_Client || req.ClientHello() == nil || req.ClientHello().Kind != protocol.ClientKind_Agent {
		t.Fatalf("role=%v hello=%+v, want role=client kind=agent", req.Role, req.ClientHello())
	}
	want, _ := ComputePSKBinder(ticket[:], transcript)
	if !bytes.Equal(req.Binder, want) {
		t.Fatal("the binder is not keyed by the ticket over this transcript")
	}
}

func TestOperatorHandshakeUsesRoleClient(t *testing.T) {
	t.Setenv("HARNESS_RUNNER_ID", "")
	t.Setenv("HARNESS_TASK_ID", "")
	t.Setenv("HARNESS_AUTH_TICKET", "")
	t.Setenv(OperatorPSKEnv, "operator-secret")
	transcript := []byte("op connection")
	req := captureHandshake(t, transcript, protocol.ClientKind_Tui)
	if req.Role != protocol.AuthRole_Client || req.ClientHello() == nil || req.ClientHello().Kind != protocol.ClientKind_Tui {
		t.Fatalf("role=%v hello=%+v", req.Role, req.ClientHello())
	}
	want, _ := ComputePSKBinder([]byte("operator-secret"), transcript)
	if !bytes.Equal(req.Binder, want) {
		t.Fatal("an operator's binder must be keyed by the operator PSK")
	}
}
