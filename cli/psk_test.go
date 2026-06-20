//go:build !js

package cli_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"bytes"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestGetPSK_Unset(t *testing.T) {
	t.Setenv("HARNESS_PSK", "")
	t.Setenv("HARNESS_PSK_FILE", "")
	if got := cli.GetPSK(); got != nil {
		t.Errorf("GetPSK() = %q, want nil", got)
	}
}

func TestGetPSK_EnvValue(t *testing.T) {
	t.Setenv("HARNESS_PSK", "hunter2")
	t.Setenv("HARNESS_PSK_FILE", "")
	got := cli.GetPSK()
	if string(got) != "hunter2" {
		t.Errorf("GetPSK() = %q, want %q", got, "hunter2")
	}
}

func TestGetPSK_FileValue(t *testing.T) {
	t.Setenv("HARNESS_PSK", "")
	f := filepath.Join(t.TempDir(), "psk")
	if err := os.WriteFile(f, []byte("filekey\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_PSK_FILE", f)
	got := cli.GetPSK()
	if string(got) != "filekey" {
		t.Errorf("GetPSK() = %q, want %q", got, "filekey")
	}
}

func TestGetPSK_EnvWinsOverFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "psk")
	if err := os.WriteFile(f, []byte("fromfile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_PSK", "fromenv")
	t.Setenv("HARNESS_PSK_FILE", f)
	got := cli.GetPSK()
	if string(got) != "fromenv" {
		t.Errorf("GetPSK() = %q, want env value %q", got, "fromenv")
	}
}

func TestGetPSK_FileMissing(t *testing.T) {
	t.Setenv("HARNESS_PSK", "")
	t.Setenv("HARNESS_PSK_FILE", "/nonexistent/psk")
	if got := cli.GetPSK(); got != nil {
		t.Errorf("GetPSK() with missing file = %q, want nil", got)
	}
}

func TestSendAndWaitPSK_NilPSK(t *testing.T) {
	err := cli.SendAndWaitPSK(context.Background(), func([]byte) error { return nil }, nil, nil, nil)
	if err != nil {
		t.Fatalf("nil PSK must be a no-op, got %v", err)
	}
}

func TestSendAndWaitPSK_OK(t *testing.T) {
	var sent []byte
	sendFn := func(data []byte) error { sent = append(sent, data...); return nil }

	respCh := make(chan appwire.PskAuthStatus, 1)
	respCh <- appwire.PskAuthStatus_Ok

	psk := []byte("secret")
	transcript := []byte("handshake-transcript-bytes")
	err := cli.SendAndWaitPSK(context.Background(), sendFn, psk, transcript, respCh)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sent) < 2 {
		t.Fatalf("sent too short: %v", sent)
	}
	if appwire.AppKind(sent[0]) != appwire.AppKind_PskAuth {
		t.Errorf("sent[0] = %v, want PskAuth", sent[0])
	}
	// The raw PSK must NOT be on the wire; a transcript-bound binder is.
	if bytes.Contains(sent[1:], psk) {
		t.Errorf("raw PSK leaked onto the wire: %v", sent[1:])
	}
	wantBinder, err := cli.ComputePSKBinder(psk, transcript)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sent[1:], wantBinder) {
		t.Errorf("sent binder = %x, want %x", sent[1:], wantBinder)
	}
}

// TestSendAndWaitPSK_BinderIsTranscriptBound locks in the MITM-resistance
// property: the same PSK over a different transcript yields a different binder.
func TestSendAndWaitPSK_BinderIsTranscriptBound(t *testing.T) {
	psk := []byte("secret")
	a, err := cli.ComputePSKBinder(psk, []byte("transcript-A"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := cli.ComputePSKBinder(psk, []byte("transcript-B"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("binder must differ across transcripts (else a relayed binder validates on another leg)")
	}
}

func TestSendAndWaitPSK_BadPSK(t *testing.T) {
	sendFn := func([]byte) error { return nil }
	respCh := make(chan appwire.PskAuthStatus, 1)
	respCh <- appwire.PskAuthStatus_BadPsk

	err := cli.SendAndWaitPSK(context.Background(), sendFn, []byte("secret"), nil, respCh)
	if err == nil {
		t.Fatal("expected error on BadPsk, got nil")
	}
}

func TestSendAndWaitPSK_SendError(t *testing.T) {
	sendErr := errors.New("network gone")
	sendFn := func([]byte) error { return sendErr }
	respCh := make(chan appwire.PskAuthStatus, 1)

	err := cli.SendAndWaitPSK(context.Background(), sendFn, []byte("secret"), nil, respCh)
	if !errors.Is(err, sendErr) {
		t.Errorf("got %v, want wrapping %v", err, sendErr)
	}
}

func TestSendAndWaitPSK_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	sendFn := func([]byte) error { return nil }
	respCh := make(chan appwire.PskAuthStatus) // never receives

	err := cli.SendAndWaitPSK(ctx, sendFn, []byte("secret"), nil, respCh)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("got %v, want context.Canceled", err)
	}
}

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

	respCh := make(chan protocol.PskAuthResponse, 1)
	respCh <- protocol.PskAuthResponse{Status: protocol.PskAuthStatus_Ok}

	err := cli.SendMergedHandshake(context.Background(), sendFn, psk, transcript, operatorKind, respCh)
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
	respCh := make(chan protocol.PskAuthResponse, 1)
	respCh <- protocol.PskAuthResponse{Status: protocol.PskAuthStatus_Ok}

	err := cli.SendMergedHandshake(context.Background(), sendFn, nil, nil, protocol.ClientKind_Cli, respCh)
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
	t.Setenv("HARNESS_RUNNER_ID", "ws:127.0.0.1:8540-1")
	// HARNESS_TASK_ID is a 32-hex (16-byte) task ID.
	t.Setenv("HARNESS_TASK_ID", "00000000000000000000000000000001")
	// HARNESS_AUTH_TICKET is a 32-hex (16-byte) ticket.
	t.Setenv("HARNESS_AUTH_TICKET", "00000000000000000000000000000002")

	var sent []byte
	sendFn := func(data []byte) error { sent = append(sent, data...); return nil }
	respCh := make(chan protocol.PskAuthResponse, 1)
	respCh <- protocol.PskAuthResponse{Status: protocol.PskAuthStatus_Ok}

	// Pass Cli as operator kind — the builder must override to Agent.
	err := cli.SendMergedHandshake(context.Background(), sendFn, nil, nil, protocol.ClientKind_Cli, respCh)
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
	respCh := make(chan protocol.PskAuthResponse, 1)
	respCh <- protocol.PskAuthResponse{Status: protocol.PskAuthStatus_BadPsk}

	err := cli.SendMergedHandshake(context.Background(), sendFn, []byte("psk"), []byte("transcript"), protocol.ClientKind_Cli, respCh)
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
	respCh := make(chan protocol.PskAuthResponse, 1)
	respCh <- protocol.PskAuthResponse{Status: protocol.PskAuthStatus_BadTicket}

	err := cli.SendMergedHandshake(context.Background(), sendFn, nil, nil, protocol.ClientKind_Cli, respCh)
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
	respCh := make(chan protocol.PskAuthResponse) // never receives

	err := cli.SendMergedHandshake(ctx, sendFn, nil, nil, protocol.ClientKind_Cli, respCh)
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
	respCh := make(chan protocol.PskAuthResponse, 1)
	respCh <- protocol.PskAuthResponse{Status: protocol.PskAuthStatus_Ok}

	if err := cli.SendMergedHandshake(context.Background(), sendFn, psk, transcript, protocol.ClientKind_Cli, respCh); err != nil {
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
