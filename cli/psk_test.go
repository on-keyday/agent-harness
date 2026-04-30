//go:build !js

package cli_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/trsf/wire"
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
	err := cli.SendAndWaitPSK(context.Background(), func([]byte) error { return nil }, nil, nil)
	if err != nil {
		t.Fatalf("nil PSK must be a no-op, got %v", err)
	}
}

func TestSendAndWaitPSK_OK(t *testing.T) {
	var sent []byte
	sendFn := func(data []byte) error { sent = append(sent, data...); return nil }

	respCh := make(chan wire.PskAuthStatus, 1)
	respCh <- wire.PskAuthStatus_Ok

	err := cli.SendAndWaitPSK(context.Background(), sendFn, []byte("secret"), respCh)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sent) < 2 {
		t.Fatalf("sent too short: %v", sent)
	}
	if wire.ApplicationPayloadKind(sent[0]) != wire.ApplicationPayloadKind_PskAuth {
		t.Errorf("sent[0] = %v, want PskAuth", sent[0])
	}
	if string(sent[1:]) != "secret" {
		t.Errorf("sent PSK = %q, want %q", sent[1:], "secret")
	}
}

func TestSendAndWaitPSK_BadPSK(t *testing.T) {
	sendFn := func([]byte) error { return nil }
	respCh := make(chan wire.PskAuthStatus, 1)
	respCh <- wire.PskAuthStatus_BadPsk

	err := cli.SendAndWaitPSK(context.Background(), sendFn, []byte("secret"), respCh)
	if err == nil {
		t.Fatal("expected error on BadPsk, got nil")
	}
}

func TestSendAndWaitPSK_SendError(t *testing.T) {
	sendErr := errors.New("network gone")
	sendFn := func([]byte) error { return sendErr }
	respCh := make(chan wire.PskAuthStatus, 1)

	err := cli.SendAndWaitPSK(context.Background(), sendFn, []byte("secret"), respCh)
	if !errors.Is(err, sendErr) {
		t.Errorf("got %v, want wrapping %v", err, sendErr)
	}
}

func TestSendAndWaitPSK_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	sendFn := func([]byte) error { return nil }
	respCh := make(chan wire.PskAuthStatus) // never receives

	err := cli.SendAndWaitPSK(ctx, sendFn, []byte("secret"), respCh)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("got %v, want context.Canceled", err)
	}
}
