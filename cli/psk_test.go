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
