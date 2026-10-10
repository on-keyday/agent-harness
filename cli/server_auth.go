package cli

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// ServerPubKeyEnv / ServerPubKeyFileEnv name the server public key a client
// verifies PskAuthResponse.server_sig against. Unlike the operator PSK this is
// not a secret: the runner forwards it into every agent's env.
const (
	ServerPubKeyEnv     = "HARNESS_SERVER_PUBKEY"
	ServerPubKeyFileEnv = "HARNESS_SERVER_PUBKEY_FILE"
)

// serverAuthContext keeps a server signature from being valid for any other
// use of the key. Its trailing NUL separates it from the transcript.
const serverAuthContext = "agent-harness server-auth v1\x00"

func serverAuthMessage(transcript []byte, status protocol.PskAuthStatus) []byte {
	msg := make([]byte, 0, len(serverAuthContext)+len(transcript)+1)
	msg = append(msg, serverAuthContext...)
	msg = append(msg, transcript...)
	return append(msg, byte(status))
}

// SignServerAuth is the server half of PskAuthResponse.server_sig. It lives
// beside ComputePSKBinder for the same reason that one does: both ends must
// build the same bytes, and one function is how they stay the same.
func SignServerAuth(key ed25519.PrivateKey, transcript []byte, status protocol.PskAuthStatus) []byte {
	return ed25519.Sign(key, serverAuthMessage(transcript, status))
}

// ServerAuthError is the CLIENT refusing the server: the response's signature
// is missing or does not verify against the configured key. It is retryable on
// purpose — it is not a PskRejectedError, so PersistLoop reconnects. Were it
// fatal, an impostor could stop every runner that reached it.
type ServerAuthError struct{ Reason string }

func (e *ServerAuthError) Error() string { return "server auth: " + e.Reason }

// VerifyServerAuth checks resp.ServerSig. A nil pub means no key is configured
// and nothing is checked.
func VerifyServerAuth(pub ed25519.PublicKey, transcript []byte, resp *protocol.PskAuthResponse) error {
	if pub == nil {
		return nil
	}
	if len(resp.ServerSig) == 0 {
		return &ServerAuthError{Reason: "the response carries no signature, and a server key is configured"}
	}
	if !ed25519.Verify(pub, serverAuthMessage(transcript, resp.Status), resp.ServerSig) {
		return &ServerAuthError{Reason: "the response's signature does not verify against the configured server key"}
	}
	return nil
}

func decodeHexKey(s, what string, size int) ([]byte, error) {
	b, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("%s: not hex: %w", what, err)
	}
	if len(b) != size {
		return nil, fmt.Errorf("%s: %d bytes, want %d", what, len(b), size)
	}
	return b, nil
}

// ParseServerPubKey parses a hex Ed25519 public key; surrounding whitespace
// is ignored, so a key read from a file with a trailing newline parses.
func ParseServerPubKey(s string) (ed25519.PublicKey, error) {
	b, err := decodeHexKey(s, "server public key", ed25519.PublicKeySize)
	if err != nil {
		return nil, err
	}
	return ed25519.PublicKey(b), nil
}

// ParseServerKeySeed parses the server's private key file: a hex 32-byte seed.
func ParseServerKeySeed(s string) (ed25519.PrivateKey, error) {
	b, err := decodeHexKey(s, "server key seed", ed25519.SeedSize)
	if err != nil {
		return nil, err
	}
	return ed25519.NewKeyFromSeed(b), nil
}

// ResolveServerPubKey is the one place a configured public key is read: value
// first, then file. Neither set → nil, nil (verification off). A configured
// file that cannot be read is an error, never "not configured".
func ResolveServerPubKey(value, file string) (ed25519.PublicKey, error) {
	if value != "" {
		return ParseServerPubKey(value)
	}
	if file == "" {
		return nil, nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("server public key file: %w", err)
	}
	return ParseServerPubKey(string(data))
}

// PskAuthReply is what a handshake receiver hands the waiting handshake: the
// decoded response, or why it did not decode. Dropping an undecodable response
// instead left the waiter blocked until the connection died — the runner's
// handshake context has no timeout.
type PskAuthReply struct {
	Resp protocol.PskAuthResponse
	Err  error
}

// DecodePskAuthReply decodes a PskAuthResponse payload (after the kind byte).
// The error text "server response did not decode" is matched by
// scripts/wire-skew-check.sh as the new-client × old-server outcome.
func DecodePskAuthReply(payload []byte) PskAuthReply {
	var r PskAuthReply
	if _, err := r.Resp.Decode(payload); err != nil {
		r.Err = fmt.Errorf("psk: server response did not decode — is the server older than this client? (%w)", err)
	}
	return r
}

// CheckPskAuthReply turns a reply into the handshake's result: nil only for a
// verified ok. The signature is checked BEFORE the status is believed, so a
// forged rejection surfaces as a retryable ServerAuthError rather than a
// PskRejectedError that could be fatal.
func CheckPskAuthReply(reply PskAuthReply, serverKey ed25519.PublicKey, transcript []byte) error {
	if reply.Err != nil {
		return reply.Err
	}
	if err := VerifyServerAuth(serverKey, transcript, &reply.Resp); err != nil {
		return err
	}
	if reply.Resp.Status == protocol.PskAuthStatus_Ok {
		return nil
	}
	return NewPskRejectedError(reply.Resp.Status)
}
