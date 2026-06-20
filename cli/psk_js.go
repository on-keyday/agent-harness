//go:build js

package cli

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"syscall/js"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
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

// buildMergedClientHello constructs the ClientHello for the WASM context.
// WASM runs in the browser (operator context) so agent-env detection is not
// applicable; the supplied operatorKind is always used.
func buildMergedClientHello(operatorKind protocol.ClientKind) protocol.ClientHello {
	return protocol.ClientHello{Kind: operatorKind}
}

// SendMergedHandshake is the WASM variant of the merged PSK+identity handshake.
// Builds a PskAuthRequest{binder (or empty when psk==nil), role=client,
// client_hello=<operatorKind>}, sends [0x45]+PskAuthRequest, and awaits a
// PskAuthResponse on respCh. Identical semantics to the native build.
func SendMergedHandshake(ctx context.Context, sendFn func([]byte) error, psk, transcript []byte, operatorKind protocol.ClientKind, respCh <-chan protocol.PskAuthResponse) error {
	req := protocol.PskAuthRequest{Role: protocol.AuthRole_Client}

	if len(psk) > 0 {
		binder, err := ComputePSKBinder(psk, transcript)
		if err != nil {
			return fmt.Errorf("psk: binder: %w", err)
		}
		if !req.SetBinder(binder) {
			return fmt.Errorf("psk: SetBinder failed (len=%d)", len(binder))
		}
	} else {
		req.SetBinder(nil) // binder_len = 0
	}

	hello := buildMergedClientHello(operatorKind)
	if !req.SetClientHello(hello) {
		return fmt.Errorf("psk: SetClientHello failed")
	}

	data, err := req.Append([]byte{byte(appwire.AppKind_PskAuth)})
	if err != nil {
		return fmt.Errorf("psk: encode: %w", err)
	}
	if err := sendFn(data); err != nil {
		return fmt.Errorf("psk: send: %w", err)
	}

	select {
	case resp := <-respCh:
		switch resp.Status {
		case protocol.PskAuthStatus_Ok:
			return nil
		case protocol.PskAuthStatus_BadPsk:
			return fmt.Errorf("psk: server rejected: %v", resp.Status)
		case protocol.PskAuthStatus_BadTicket:
			return fmt.Errorf("psk: server rejected agent ticket: %v", resp.Status)
		case protocol.PskAuthStatus_NoIdentity:
			return fmt.Errorf("psk: server rejected (no identity): %v", resp.Status)
		default:
			return fmt.Errorf("psk: server rejected: %v", resp.Status)
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

