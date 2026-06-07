//go:build !js

package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/on-keyday/agent-harness/appwire"
)

// GetPSK resolves the PSK in priority order:
//  1. HARNESS_PSK env (value)
//  2. HARNESS_PSK_FILE env (path → read file, trim whitespace)
//  3. nil (no PSK)
func GetPSK() []byte {
	if v := os.Getenv("HARNESS_PSK"); v != "" {
		return []byte(v)
	}
	if path := os.Getenv("HARNESS_PSK_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err == nil {
			if v := strings.TrimSpace(string(data)); v != "" {
				return []byte(v)
			}
		}
	}
	return nil
}

// SendAndWaitPSK sends a PskAuthRequest via sendFn and waits for a PskAuthResponse on respCh.
// No-op when psk is nil. sendFn is called exactly once with the encoded request bytes.
//
// transcript is the objproto handshake transcript (Connection.GetTranscript());
// the PSK is never sent verbatim — what goes on the wire is a transcript-bound
// binder (see ComputePSKBinder), so the exchange authenticates the
// channel instead of leaking a replayable bearer secret.
func SendAndWaitPSK(ctx context.Context, sendFn func([]byte) error, psk, transcript []byte, respCh <-chan appwire.PskAuthStatus) error {
	if len(psk) == 0 {
		return nil
	}
	binder, err := ComputePSKBinder(psk, transcript)
	if err != nil {
		return fmt.Errorf("psk: binder: %w", err)
	}
	data := make([]byte, 1+len(binder))
	data[0] = byte(appwire.AppKind_PskAuth)
	copy(data[1:], binder)
	if err := sendFn(data); err != nil {
		return fmt.Errorf("psk: send: %w", err)
	}
	select {
	case status := <-respCh:
		if status != appwire.PskAuthStatus_Ok {
			return fmt.Errorf("psk: server rejected: %v", status)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
