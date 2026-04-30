//go:build js

package cli

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"syscall/js"

	"github.com/on-keyday/agent-harness/trsf/wire"
)

// GetPSK reads the PSK from the URL fragment (#psk=<value>).
// Returns nil when the fragment is absent or contains no psk= key.
func GetPSK() []byte {
	hash := js.Global().Get("location").Get("hash").String()
	hash = strings.TrimPrefix(hash, "#")
	vals, err := url.ParseQuery(hash)
	if err != nil {
		return nil
	}
	v := vals.Get("psk")
	if v == "" {
		return nil
	}
	return []byte(v)
}

// SendAndWaitPSK is the WASM variant — identical logic to the native build.
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
