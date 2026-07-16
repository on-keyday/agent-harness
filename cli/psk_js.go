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

// resolveBinderPSK in the browser is always the operator surface: WASM never
// runs as an in-task agent and a browser is not a runner host, so the
// HARNESS_PSK env-inheritance footgun the non-js variant guards against does
// not exist here. The #psk fragment IS the operator secret to prove.
func resolveBinderPSK() []byte {
	return GetPSK()
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
		if resp.Status == protocol.PskAuthStatus_Ok {
			return nil
		}
		// Explicit server rejection — FATAL, not retryable (see persist.go).
		return &PskRejectedError{Status: resp.Status.String(), Code: resp.Status}
	case <-ctx.Done():
		// Transport drop / cancellation mid-handshake — RETRYABLE.
		return ctx.Err()
	}
}

