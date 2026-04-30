//go:build !js

package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/on-keyday/agent-harness/trsf/wire"
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
func SendAndWaitPSK(ctx context.Context, sendFn func([]byte) error, psk []byte, respCh <-chan wire.PskAuthStatus) error {
	if len(psk) == 0 {
		return nil
	}
	data := make([]byte, 1+len(psk))
	data[0] = byte(wire.ApplicationPayloadKind_PskAuth)
	copy(data[1:], psk)
	if err := sendFn(data); err != nil {
		return fmt.Errorf("psk: send: %w", err)
	}
	select {
	case status := <-respCh:
		if status != wire.PskAuthStatus_Ok {
			return fmt.Errorf("psk: server rejected: %v", status)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
