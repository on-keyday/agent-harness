//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/agent-harness/server"
)

// startServerWithPSK is startServerAt + a configured PSK. Kept local because
// the shared helper does not take a PSK.
func startServerWithPSK(t *testing.T, psk []byte) objproto.ConnectionID {
	t.Helper()
	addr := freePort(t)
	peerCID, err := objproto.ParseConnectionID("ws:"+addr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse server cid: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := server.New(server.Config{Addr: addr, DataDir: t.TempDir(), PSK: psk})
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	})
	time.Sleep(300 * time.Millisecond)
	return peerCID
}

// TestPSKBinderE2E_CorrectPSK is the make-or-break check for the transcript-
// bound PSK binder: it only passes if the client and server derive a
// byte-identical objproto handshake transcript, so their binders match. A
// client carrying the correct PSK must authenticate over a real handshake.
func TestPSKBinderE2E_CorrectPSK(t *testing.T) {
	const psk = "correct-horse-battery-staple"
	cid := startServerWithPSK(t, []byte(psk))
	t.Setenv("HARNESS_PSK", psk)
	t.Setenv("HARNESS_PSK_FILE", "")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := cli.Dial(ctx, cid)
	if err != nil {
		t.Fatalf("Dial with correct PSK must succeed (client/server transcripts must match): %v", err)
	}
	c.Close()
}

// TestPSKBinderE2E_WrongPSK confirms a mismatched PSK is rejected by the gate.
func TestPSKBinderE2E_WrongPSK(t *testing.T) {
	cid := startServerWithPSK(t, []byte("server-side-secret"))
	t.Setenv("HARNESS_PSK", "client-has-the-wrong-one")
	t.Setenv("HARNESS_PSK_FILE", "")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := cli.Dial(ctx, cid)
	if err == nil {
		c.Close()
		t.Fatal("Dial with wrong PSK must fail")
	}
	var pskErr *cli.PSKAuthError
	if !errors.As(err, &pskErr) {
		t.Errorf("err = %v, want *cli.PSKAuthError", err)
	}
}
