package cli

import (
	"crypto/hmac"
	"crypto/sha512"
	"fmt"

	"github.com/on-keyday/agent-harness/objproto"
)

// ComputePSKBinder derives a transcript-bound authenticator from the PSK, in
// the style of the TLS 1.3 PSK binder: HMAC over the objproto handshake
// transcript, keyed by a key derived from the PSK.
//
// objproto's handshake is deliberately unauthenticated (it provides the
// "encrypt first" half of a TLS-1.3-style exchange and exports the transcript
// via Connection.GetTranscript so an upper layer can authenticate). PSK auth is
// that upper layer, so the binder lives here in the app layer — objproto stays
// auth-free and only exposes the DeriveKey primitive the binder builds on.
// Binding the PSK proof to the transcript is what closes the gap: it turns the
// PSK from a replayable bearer secret (sent verbatim inside an unauthenticated
// channel) into a channel authenticator. An active MITM that relays two
// separate handshakes ends up with two different transcripts, so a binder
// captured on one leg does not validate on the other, and the PSK itself never
// crosses the wire. Both ends derive the same transcript (clientHandshake ‖
// serverAck), so the binders match for a genuine end-to-end handshake.
//
// No build tag: crypto/hmac + crypto/sha512 compile for GOOS=js too, so both
// the native (psk.go) and WASM (psk_js.go) clients compute the same binder.
func ComputePSKBinder(psk, transcript []byte) ([]byte, error) {
	binderKey, err := objproto.DeriveKey(psk, "ksdk-psk-binder", 32)
	if err != nil {
		return nil, fmt.Errorf("psk binder key derive: %w", err)
	}
	mac := hmac.New(sha512.New, binderKey)
	mac.Write(transcript)
	return mac.Sum(nil), nil
}
