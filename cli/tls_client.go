//go:build !js

package cli

import "crypto/tls"

// ClientTLSConfig is the TLS config for dialing a wss: peer. objtrsf consults
// it only for a wss: destination, so ws: dials are unaffected.
//
// It does not verify the server certificate, and that is safe here: TLS on
// this path exists for browser compatibility (an HTTPS WebUI), not security.
// Peer authentication is objproto's PSK handshake and confidentiality is its
// AEAD, so a man in the middle who terminates TLS still cannot complete the
// objproto handshake without the PSK. Verifying would also need a hostname,
// and a ConnectionID carries only an address.
func ClientTLSConfig() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}
}
