//go:build js

package cli

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"net/url"
	"strings"
	"syscall/js"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// GetOperatorPSK reads the operator secret from the URL fragment
// (#psk=<value>; the fragment keeps its name so existing links work).
// Returns nil when the fragment is absent or contains no psk= key.
func GetOperatorPSK() []byte {
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
// runs as an in-task agent. The #psk fragment IS the operator secret.
func resolveBinderPSK() []byte {
	return GetOperatorPSK()
}

// EffectiveClientKind is the WASM variant: a browser is always an operator
// context, so there is no in-task env to upgrade to Agent and the kind asked
// for is the kind announced.
//
// Declared here rather than left to the !js build so buildMergedClientHello
// and the telemetry row a client reports about ITSELF go through one function
// on both builds — the header cannot disagree with the handshake, and neither
// can the row.
func EffectiveClientKind(operatorKind protocol.ClientKind) (protocol.ClientKind, protocol.TaskID) {
	return operatorKind, protocol.TaskID{}
}

// buildMergedClientHello constructs the ClientHello for the WASM context.
// WASM runs in the browser (operator context) so agent-env detection is not
// applicable; the supplied operatorKind is always used.
func buildMergedClientHello(operatorKind protocol.ClientKind) protocol.ClientHello {
	kind, _ := EffectiveClientKind(operatorKind)
	return protocol.ClientHello{Kind: kind}
}

// SendMergedHandshake is the WASM variant of the merged PSK+identity handshake.
// Builds a PskAuthRequest{binder (or empty when psk==nil), role=client,
// client_hello=<operatorKind>}, sends [0x45]+PskAuthRequest, and awaits a
// PskAuthResponse on respCh. Identical semantics to the native build.
func SendMergedHandshake(ctx context.Context, sendFn func([]byte) error, psk, transcript []byte, operatorKind protocol.ClientKind, serverKey ed25519.PublicKey, respCh <-chan PskAuthReply) error {
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
	case reply := <-respCh:
		// Signature first, then status: see CheckPskAuthReply.
		return CheckPskAuthReply(reply, serverKey, transcript)
	case <-ctx.Done():
		// Transport drop / cancellation mid-handshake — RETRYABLE.
		return ctx.Err()
	}
}
