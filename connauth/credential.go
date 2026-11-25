package connauth

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// A credential (docs/superpowers/specs/2026-11-21-credential-tasks-design.md)
// as the handshake sees it: the value a script holds and the request that
// proves it. The verbs that issue, list and revoke one are package cli's.

// Environment names a script carries its credential in. Read in this order:
// --credential-file, CredentialFileEnv, CredentialEnv (first match wins).
const (
	CredentialFileEnv = "HARNESS_CREDENTIAL_FILE"
	CredentialEnv     = "HARNESS_CREDENTIAL"
)

// Credential is what a script holds: the task it is, and the key that proves
// it. Its string form `<32 hex task id>:<64 hex Ed25519 seed>` is the one
// secret; the server stores the public half only.
type Credential struct {
	TaskID protocol.TaskID
	Key    ed25519.PrivateKey
}

// ParseCredential reads the string form. An error never repeats the input:
// its second half is a secret.
func ParseCredential(s string) (Credential, error) {
	const form = "credential: want <32 hex task id>:<64 hex seed>"
	s = strings.TrimSpace(s)
	id, seed, ok := strings.Cut(s, ":")
	if !ok || len(id) != 32 || len(seed) != 2*ed25519.SeedSize {
		return Credential{}, errors.New(form)
	}
	rawID, err := hex.DecodeString(id)
	if err != nil {
		return Credential{}, errors.New(form)
	}
	rawSeed, err := hex.DecodeString(seed)
	if err != nil {
		return Credential{}, errors.New(form)
	}
	var tid protocol.TaskID
	copy(tid.Id[:], rawID)
	return Credential{TaskID: tid, Key: ed25519.NewKeyFromSeed(rawSeed)}, nil
}

// Secret is the credential string, seed included: the one place a secret is
// handed over (a printed `secret:` line, a credential file). It is not String:
// anything that formats a Credential with %v, %s or %#v (a log line, an error)
// gets the redacted form below, never the seed.
func (c Credential) Secret() string {
	return hex.EncodeToString(c.TaskID.Id[:]) + ":" + hex.EncodeToString(c.Key.Seed())
}

// String is the redacted form: the task, no seed. GoString too, so %#v cannot
// print the key either.
func (c Credential) String() string {
	return "credential " + hex.EncodeToString(c.TaskID.Id[:])[:8]
}

func (c Credential) GoString() string { return c.String() }

// LogValue keeps the seed out of a structured log line.
func (c Credential) LogValue() slog.Value {
	return slog.StringValue(c.String())
}

// SendCredentialHandshake is the credential's counterpart of
// SendMergedHandshake: PskAuthRequest{role=credential, credential_auth=
// {task id, signature over this connection's transcript}}, then the server's
// answer. There is no binder and no ClientHello: the credential names the
// principal itself, and nothing else the process holds (the operator PSK, an
// agent's ticket) is sent.
func SendCredentialHandshake(ctx context.Context, sendFn func([]byte) error, transcript []byte, cred Credential, serverKey ed25519.PublicKey, respCh <-chan PskAuthReply) error {
	if len(cred.Key) != ed25519.PrivateKeySize {
		return fmt.Errorf("credential: no key")
	}
	req := protocol.PskAuthRequest{Role: protocol.AuthRole_Credential}
	req.SetBinder(nil)
	auth := protocol.CredentialAuth{TaskId: cred.TaskID}
	copy(auth.Sig[:], SignCredentialAuth(cred.Key, transcript))
	if !req.SetCredentialAuth(auth) {
		return fmt.Errorf("psk: SetCredentialAuth failed")
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
		// Signature first, then status (CheckPskAuthReply): a forged
		// rejection must not turn a retry into a fatal one.
		return CheckPskAuthReply(reply, serverKey, transcript)
	case <-ctx.Done():
		return ctx.Err()
	}
}
