//go:build !js

package cli_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// ---------------------------------------------------------------------------
// Tests for the new merged handshake (SendMergedHandshake)
// ---------------------------------------------------------------------------

// TestSendMergedHandshake_OperatorKind_WithPSK verifies that the merged builder
// produces a well-formed [0x45]+PskAuthRequest when an operator kind is passed
// and a PSK is configured. Decoding the built bytes with the protocol types
// asserts the full round-trip: binder present, role=client, ClientHello.Kind
// equals the passed operator kind.
func TestSendMergedHandshake_OperatorKind_WithPSK(t *testing.T) {
	// Ensure no agent env is set so the operator path is taken.
	t.Setenv("HARNESS_RUNNER_ID", "")
	t.Setenv("HARNESS_TASK_ID", "")
	t.Setenv("HARNESS_AUTH_TICKET", "")

	psk := []byte("test-psk")
	transcript := []byte("test-transcript")
	operatorKind := protocol.ClientKind_Tui

	var sent []byte
	sendFn := func(data []byte) error {
		sent = append(sent, data...)
		return nil
	}

	respCh := make(chan cli.PskAuthReply, 1)
	respCh <- cli.PskAuthReply{Resp: protocol.PskAuthResponse{Status: protocol.PskAuthStatus_Ok}}

	err := cli.SendMergedHandshake(context.Background(), sendFn, psk, transcript, operatorKind, nil, respCh)
	if err != nil {
		t.Fatalf("SendMergedHandshake returned error: %v", err)
	}

	// First byte must be the PskAuth AppKind.
	if len(sent) < 1 {
		t.Fatal("nothing was sent")
	}
	if appwire.AppKind(sent[0]) != appwire.AppKind_PskAuth {
		t.Errorf("sent[0] = %d, want AppKind_PskAuth (%d)", sent[0], appwire.AppKind_PskAuth)
	}

	// Decode the PskAuthRequest from the remaining bytes.
	var req protocol.PskAuthRequest
	remain, err := req.Decode(sent[1:])
	if err != nil {
		t.Fatalf("Decode PskAuthRequest: %v", err)
	}
	if len(remain) != 0 {
		t.Errorf("unexpected remaining bytes after decode: %d", len(remain))
	}

	// Binder must be present and match ComputePSKBinder(psk, transcript).
	wantBinder, err := cli.ComputePSKBinder(psk, transcript)
	if err != nil {
		t.Fatal(err)
	}
	if req.BinderLen != uint16(len(wantBinder)) {
		t.Errorf("BinderLen = %d, want %d", req.BinderLen, len(wantBinder))
	}
	if !bytes.Equal(req.Binder, wantBinder) {
		t.Errorf("Binder mismatch: got %x, want %x", req.Binder, wantBinder)
	}

	// Role must be Client.
	if req.Role != protocol.AuthRole_Client {
		t.Errorf("Role = %v, want AuthRole_Client", req.Role)
	}

	// ClientHello must be present with the passed operator kind.
	ch := req.ClientHello()
	if ch == nil {
		t.Fatal("ClientHello() returned nil")
	}
	if ch.Kind != operatorKind {
		t.Errorf("ClientHello.Kind = %v, want %v", ch.Kind, operatorKind)
	}
	// Operator path: no AgentInfo.
	if ch.AgentInfo() != nil {
		t.Errorf("operator path must not set AgentInfo, got non-nil")
	}

	// RunnerHello must be nil for client role.
	if req.RunnerHello() != nil {
		t.Errorf("RunnerHello() must be nil for client role")
	}
}

// TestSendMergedHandshake_NoPSK verifies that when psk==nil, binder_len=0 is
// sent and the ClientHello is still present (no PSK dev-mode case).
func TestSendMergedHandshake_NoPSK(t *testing.T) {
	t.Setenv("HARNESS_RUNNER_ID", "")
	t.Setenv("HARNESS_TASK_ID", "")
	t.Setenv("HARNESS_AUTH_TICKET", "")

	var sent []byte
	sendFn := func(data []byte) error { sent = append(sent, data...); return nil }
	respCh := make(chan cli.PskAuthReply, 1)
	respCh <- cli.PskAuthReply{Resp: protocol.PskAuthResponse{Status: protocol.PskAuthStatus_Ok}}

	err := cli.SendMergedHandshake(context.Background(), sendFn, nil, nil, protocol.ClientKind_Cli, nil, respCh)
	if err != nil {
		t.Fatalf("SendMergedHandshake (no PSK): %v", err)
	}

	if appwire.AppKind(sent[0]) != appwire.AppKind_PskAuth {
		t.Errorf("sent[0] = %d, want AppKind_PskAuth", sent[0])
	}

	var req protocol.PskAuthRequest
	remain, err := req.Decode(sent[1:])
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(remain) != 0 {
		t.Errorf("unexpected remaining bytes: %d", len(remain))
	}
	if req.BinderLen != 0 {
		t.Errorf("BinderLen = %d, want 0 (no PSK)", req.BinderLen)
	}
	if len(req.Binder) != 0 {
		t.Errorf("Binder must be empty for no-PSK, got %d bytes", len(req.Binder))
	}
	ch := req.ClientHello()
	if ch == nil {
		t.Fatal("ClientHello() must be present even without PSK")
	}
	if ch.Kind != protocol.ClientKind_Cli {
		t.Errorf("ClientHello.Kind = %v, want Cli", ch.Kind)
	}
}

// TestSendMergedHandshake_AgentEnv verifies that when the agent env vars are
// set, the merged builder overrides the operator kind to Agent and populates
// AgentInfo. Uses dummy values that satisfy the env-resolution format.
func TestSendMergedHandshake_AgentEnv(t *testing.T) {
	// ResolveRunnerID parses a CID string like "ws:127.0.0.1:8540-1".
	t.Setenv("HARNESS_RUNNER_ID", "a3a3a3a3a3a3a3a3a3a3a3a3a3a3a3a3")
	// HARNESS_TASK_ID is a 32-hex (16-byte) task ID.
	t.Setenv("HARNESS_TASK_ID", "00000000000000000000000000000001")
	// HARNESS_AUTH_TICKET is a 32-hex (16-byte) ticket.
	t.Setenv("HARNESS_AUTH_TICKET", "00000000000000000000000000000002")

	var sent []byte
	sendFn := func(data []byte) error { sent = append(sent, data...); return nil }
	respCh := make(chan cli.PskAuthReply, 1)
	respCh <- cli.PskAuthReply{Resp: protocol.PskAuthResponse{Status: protocol.PskAuthStatus_Ok}}

	// Pass Cli as operator kind — the builder must override to Agent.
	err := cli.SendMergedHandshake(context.Background(), sendFn, nil, nil, protocol.ClientKind_Cli, nil, respCh)
	if err != nil {
		t.Fatalf("SendMergedHandshake (agent env): %v", err)
	}

	var req protocol.PskAuthRequest
	if _, err := req.Decode(sent[1:]); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	ch := req.ClientHello()
	if ch == nil {
		t.Fatal("ClientHello() returned nil")
	}
	if ch.Kind != protocol.ClientKind_Agent {
		t.Errorf("ClientHello.Kind = %v, want Agent (agent env should override)", ch.Kind)
	}
	if ch.AgentInfo() == nil {
		t.Errorf("AgentInfo must be set when agent env is present")
	}
}

// TestSendMergedHandshake_BadPsk verifies that a PskAuthResponse{BadPsk} results
// in an error.
func TestSendMergedHandshake_BadPsk(t *testing.T) {
	t.Setenv("HARNESS_RUNNER_ID", "")
	t.Setenv("HARNESS_TASK_ID", "")
	t.Setenv("HARNESS_AUTH_TICKET", "")

	sendFn := func([]byte) error { return nil }
	respCh := make(chan cli.PskAuthReply, 1)
	respCh <- cli.PskAuthReply{Resp: protocol.PskAuthResponse{Status: protocol.PskAuthStatus_BadPsk}}

	err := cli.SendMergedHandshake(context.Background(), sendFn, []byte("psk"), []byte("transcript"), protocol.ClientKind_Cli, nil, respCh)
	if err == nil {
		t.Fatal("expected error on BadPsk response, got nil")
	}
}

// TestSendMergedHandshake_BadTicket verifies that a PskAuthResponse{BadTicket}
// results in an error.
func TestSendMergedHandshake_BadTicket(t *testing.T) {
	t.Setenv("HARNESS_RUNNER_ID", "")
	t.Setenv("HARNESS_TASK_ID", "")
	t.Setenv("HARNESS_AUTH_TICKET", "")

	sendFn := func([]byte) error { return nil }
	respCh := make(chan cli.PskAuthReply, 1)
	respCh <- cli.PskAuthReply{Resp: protocol.PskAuthResponse{Status: protocol.PskAuthStatus_BadTicket}}

	err := cli.SendMergedHandshake(context.Background(), sendFn, nil, nil, protocol.ClientKind_Cli, nil, respCh)
	if err == nil {
		t.Fatal("expected error on BadTicket response, got nil")
	}
}

// TestSendMergedHandshake_ContextCancelled verifies ctx cancellation surfaces.
func TestSendMergedHandshake_ContextCancelled(t *testing.T) {
	t.Setenv("HARNESS_RUNNER_ID", "")
	t.Setenv("HARNESS_TASK_ID", "")
	t.Setenv("HARNESS_AUTH_TICKET", "")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sendFn := func([]byte) error { return nil }
	respCh := make(chan cli.PskAuthReply) // never receives

	err := cli.SendMergedHandshake(ctx, sendFn, nil, nil, protocol.ClientKind_Cli, nil, respCh)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("got %v, want context.Canceled", err)
	}
}

// TestSendMergedHandshake_BinderUnchanged verifies that the binder embedded in
// the PskAuthRequest is byte-identical to ComputePSKBinder(psk, transcript),
// confirming the binder crypto is unchanged by the new wire format.
func TestSendMergedHandshake_BinderUnchanged(t *testing.T) {
	t.Setenv("HARNESS_RUNNER_ID", "")
	t.Setenv("HARNESS_TASK_ID", "")
	t.Setenv("HARNESS_AUTH_TICKET", "")

	psk := []byte("unchanged-psk")
	transcript := []byte("ecdh-handshake-transcript")

	var sent []byte
	sendFn := func(data []byte) error { sent = append(sent, data...); return nil }
	respCh := make(chan cli.PskAuthReply, 1)
	respCh <- cli.PskAuthReply{Resp: protocol.PskAuthResponse{Status: protocol.PskAuthStatus_Ok}}

	if err := cli.SendMergedHandshake(context.Background(), sendFn, psk, transcript, protocol.ClientKind_Cli, nil, respCh); err != nil {
		t.Fatal(err)
	}

	var req protocol.PskAuthRequest
	if _, err := req.Decode(sent[1:]); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	wantBinder, err := cli.ComputePSKBinder(psk, transcript)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(req.Binder, wantBinder) {
		t.Errorf("binder mismatch: merged handshake binder differs from ComputePSKBinder output")
	}
}

// TestSendMergedHandshake_RejectionFatal_CancelRetryable is the regression guard
// for the fleet-wipeout incident: an explicit server rejection must be a
// *PskRejectedError (callers wrap it fatal), but a transport drop / context
// cancellation mid-handshake must NOT be — it must stay retryable so a server
// restart that interrupts the handshake triggers reconnect, not runner exit.
func TestSendMergedHandshake_RejectionFatal_CancelRetryable(t *testing.T) {
	t.Setenv("HARNESS_RUNNER_ID", "")
	t.Setenv("HARNESS_TASK_ID", "")
	t.Setenv("HARNESS_AUTH_TICKET", "")

	// Explicit server rejection → *PskRejectedError (fatal).
	respCh := make(chan cli.PskAuthReply, 1)
	respCh <- cli.PskAuthReply{Resp: protocol.PskAuthResponse{Status: protocol.PskAuthStatus_BadPsk}}
	err := cli.SendMergedHandshake(context.Background(),
		func([]byte) error { return nil }, nil, nil, protocol.ClientKind_Cli, nil, respCh)
	var rej *cli.PskRejectedError
	if !errors.As(err, &rej) {
		t.Fatalf("rejection: want *cli.PskRejectedError, got %T: %v", err, err)
	}

	// Cancellation mid-handshake (no response ever arrives) → context.Canceled,
	// NOT a PskRejectedError → retryable.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = cli.SendMergedHandshake(ctx,
		func([]byte) error { return nil }, nil, nil, protocol.ClientKind_Cli, nil,
		make(chan cli.PskAuthReply))
	if errors.As(err, &rej) {
		t.Fatalf("cancel: must NOT be *cli.PskRejectedError (must be retryable), got %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: want context.Canceled, got %v", err)
	}
}

// A pinned client must not take an unsigned ok: that is the impostor case.
func TestSendMergedHandshake_PinnedRefusesUnsignedOk(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	respCh := make(chan cli.PskAuthReply, 1)
	respCh <- cli.PskAuthReply{Resp: protocol.PskAuthResponse{Status: protocol.PskAuthStatus_Ok}}
	err := cli.SendMergedHandshake(context.Background(), func([]byte) error { return nil },
		nil, []byte("tr"), protocol.ClientKind_Cli, pub, respCh)
	var sae *cli.ServerAuthError
	if !errors.As(err, &sae) {
		t.Fatalf("err = %v, want *cli.ServerAuthError", err)
	}
}

// A pinned client takes a correctly signed ok.
func TestSendMergedHandshake_PinnedAcceptsSignedOk(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	tr := []byte("tr")
	resp := protocol.PskAuthResponse{Status: protocol.PskAuthStatus_Ok}
	resp.SetServerSig(cli.SignServerAuth(priv, tr, protocol.PskAuthStatus_Ok))
	respCh := make(chan cli.PskAuthReply, 1)
	respCh <- cli.PskAuthReply{Resp: resp}
	if err := cli.SendMergedHandshake(context.Background(), func([]byte) error { return nil },
		nil, tr, protocol.ClientKind_Cli, pub, respCh); err != nil {
		t.Fatal(err)
	}
}

// An undecodable reply (old server) returns at once with the skew message.
func TestSendMergedHandshake_UndecodableReplyIsImmediate(t *testing.T) {
	respCh := make(chan cli.PskAuthReply, 1)
	respCh <- cli.DecodePskAuthReply([]byte{0})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := cli.SendMergedHandshake(ctx, func([]byte) error { return nil },
		nil, nil, protocol.ClientKind_Cli, nil, respCh)
	if err == nil || !strings.Contains(err.Error(), "server response did not decode") {
		t.Fatalf("err = %v", err)
	}
	var rej *cli.PskRejectedError
	if errors.As(err, &rej) {
		t.Fatal("a decode failure must not be a PskRejectedError")
	}
}

func TestGetServerPubKey_Env(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	t.Setenv(cli.ServerPubKeyEnv, hex.EncodeToString(pub))
	t.Setenv(cli.ServerPubKeyFileEnv, "")
	got, err := cli.GetServerPubKey()
	if err != nil || !got.Equal(pub) {
		t.Fatalf("got %x, %v", got, err)
	}
	t.Setenv(cli.ServerPubKeyEnv, "not-a-key")
	if _, err := cli.GetServerPubKey(); err == nil {
		t.Fatal("malformed HARNESS_SERVER_PUBKEY accepted")
	}
}

// A malformed key is a configuration error and must surface BEFORE the dial
// (spec D9): with the server down, a PersistLoop caller would otherwise only
// ever see connection errors and retry forever.
func TestDial_MalformedServerPubKeyFailsBeforeDialing(t *testing.T) {
	t.Setenv(cli.ServerPubKeyEnv, "abcd")
	t.Setenv(cli.ServerPubKeyFileEnv, "")
	t.Setenv("HARNESS_PROXY_VIA_RUNNER", "")
	cid, err := objproto.ParseConnectionID("ws:127.0.0.1:1-*", objproto.ParseOption_AllowRandomID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	var fatal *cli.PSKAuthError
	if !errors.As(err, &fatal) || !strings.Contains(err.Error(), cli.ServerPubKeyEnv) {
		t.Fatalf("err = %v, want a *cli.PSKAuthError naming %s", err, cli.ServerPubKeyEnv)
	}
}
