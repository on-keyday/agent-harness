//go:build js

package cli

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"syscall/js"

	"github.com/on-keyday/agent-harness/appwire"
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

// SendAndWaitPSK is the WASM variant — identical logic to the native build:
// the PSK is bound to the handshake transcript and only the binder crosses the
// wire (see ComputePSKBinder). crypto/hmac + crypto/sha512 compile
// for GOOS=js, so the browser client computes the same binder as the server.
func SendAndWaitPSK(ctx context.Context, sendFn func([]byte) error, psk, transcript []byte, respCh <-chan wire.PskAuthStatus) error {
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
		if status != wire.PskAuthStatus_Ok {
			return fmt.Errorf("psk: server rejected: %v", status)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
