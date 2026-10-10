# Server Signature Authentication Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The server signs every `PskAuthResponse` with an Ed25519 key, and a runner / harness-cli / harness-tui / in-task agent that has the server's public key configured refuses a response whose signature is missing or wrong.

**Architecture:** One schema change (`PskAuthResponse` gains `server_sig`). Signing, verifying, key parsing and response decoding live in one new file, `cli/server_auth.go`, used by the server and by both client handshake sites (`cli/client.go`, `runner/connect.go`). The server resolves its key like the operator PSK; clients resolve the public key like the PSK; the runner forwards it to agents in their env.

**Tech Stack:** Go, `crypto/ed25519`, brgen (`.bgn` → Go via `make protoregen`).

**Spec:** `docs/superpowers/specs/2026-10-11-server-signature-auth-design.md` — read its **Problem** section before starting any task.

## Global Constraints

- Signed message: `"agent-harness server-auth v1" ‖ 0x00 ‖ transcript ‖ status-byte`.
- `server_sig_len` is 0 or 64. The server's control listener always sends 64; the runner's data-plane listener sends 0.
- Keys are hex text: private key file = 32-byte Ed25519 seed; public key = 32 bytes. Surrounding whitespace (a trailing newline) is trimmed.
- Server files: `<data-dir>/server-key` (0600), `<data-dir>/server-key.pub`; `--server-key-file` overrides the first.
- Client config: `HARNESS_SERVER_PUBKEY` (value) wins over `HARNESS_SERVER_PUBKEY_FILE` (path). Runner flags `--server-pubkey` / `--server-pubkey-file`; a flag wins over its env var.
- Key configured → verify, fail closed. Not configured → do not look at the signature, do not warn.
- A configured key that does not parse, or a configured key file that cannot be read, is a startup error — never a fall back to "not configured".
- A failed verification is `*cli.ServerAuthError`, retryable (never wrapped in `*cli.PSKAuthError`).
- A response that does not decode is an immediate retryable error whose message contains `server response did not decode`.
- WebUI (WASM) decodes the new format and ignores the signature.
- Public repo: no LAN addresses, hostnames or private paths in code, tests or docs.
- Compile checks must not drop binaries in the worktree: `go build ./...`, `go vet ./...`, never a bare `go build ./cmd/<x>/`.

## Review Focus

1. **A pinned client against an OLD server** — the one-byte response must produce `server response did not decode` at once, not a hang (the runner's handshake context has no timeout). Tests: Task 3 Step 1 (`TestSendMergedHandshake_UndecodableReplyIsImmediate`), Task 4 Step 1 (`TestRunnerHandshake_UndecodableReplyIsRetryable`).
2. **An impostor answering `bad_psk` signed with the wrong key** — the runner must reconnect, not exit. The signature check must run BEFORE the status is turned into a `PskRejectedError`. Test: Task 4 Step 1 (`TestRunnerHandshake_ForgedRejectionIsNotFatal`).
3. **A key copied from a file with a trailing newline, or pasted with the wrong length** — the first works, the second fails at startup with a message naming the expected length. Tests: Task 1 Step 1 (`TestParseServerPubKey`).
4. **An unpinned client against the NEW server** keeps working (signature ignored). Test: Task 5 Step 1 (`TestServerAuthE2E_UnpinnedClientConnects`).
5. **An agent whose runner is pinned** gets the key in its env from every spawn path (oneshot, interactive, exec) — three hand-built `AgentEnvSpec` literals. Test: Task 4 Step 1 (`TestBuildAgentEnv_ServerPubKey`) plus Step 6's grep.

---

### Task 1: Wire format and `cli/server_auth.go`

**Files:**
- Modify: `runner/protocol/message.bgn` (`format PskAuthResponse`)
- Regenerate: `runner/protocol/message.go` (`make protoregen`)
- Create: `cli/server_auth.go`
- Test: `cli/server_auth_test.go`

**Interfaces:**
- Produces (package `cli`):
  - `const ServerPubKeyEnv = "HARNESS_SERVER_PUBKEY"`, `const ServerPubKeyFileEnv = "HARNESS_SERVER_PUBKEY_FILE"`
  - `func SignServerAuth(key ed25519.PrivateKey, transcript []byte, status protocol.PskAuthStatus) []byte`
  - `func VerifyServerAuth(pub ed25519.PublicKey, transcript []byte, resp *protocol.PskAuthResponse) error` — nil `pub` returns nil
  - `type ServerAuthError struct{ Reason string }` with `Error() string`
  - `func ParseServerPubKey(s string) (ed25519.PublicKey, error)`
  - `func ParseServerKeySeed(s string) (ed25519.PrivateKey, error)`
  - `func ResolveServerPubKey(value, file string) (ed25519.PublicKey, error)` — both empty → `nil, nil`
  - `type PskAuthReply struct { Resp protocol.PskAuthResponse; Err error }`
  - `func DecodePskAuthReply(payload []byte) PskAuthReply`
  - `func CheckPskAuthReply(reply PskAuthReply, serverKey ed25519.PublicKey, transcript []byte) error` — nil only for a verified `ok`
- Produces (package `protocol`, generated): `PskAuthResponse.ServerSigLen uint8`, `PskAuthResponse.ServerSig []uint8`, `func (t *PskAuthResponse) SetServerSig(v []uint8) bool`

- [ ] **Step 1: Write the failing tests**

`cli/server_auth_test.go`:

```go
package cli_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func signedResp(t *testing.T, priv ed25519.PrivateKey, transcript []byte, st protocol.PskAuthStatus) protocol.PskAuthResponse {
	t.Helper()
	resp := protocol.PskAuthResponse{Status: st}
	if !resp.SetServerSig(cli.SignServerAuth(priv, transcript, st)) {
		t.Fatal("SetServerSig failed")
	}
	return resp
}

func TestVerifyServerAuth(t *testing.T) {
	pub, priv := newKey(t)
	otherPub, _ := newKey(t)
	tr := []byte("transcript-A")
	ok := signedResp(t, priv, tr, protocol.PskAuthStatus_Ok)

	if err := cli.VerifyServerAuth(pub, tr, &ok); err != nil {
		t.Fatalf("genuine signature: %v", err)
	}
	if err := cli.VerifyServerAuth(nil, tr, &protocol.PskAuthResponse{}); err != nil {
		t.Fatalf("no key configured must not verify: %v", err)
	}

	var sae *cli.ServerAuthError
	cases := map[string]error{
		"other key":        cli.VerifyServerAuth(otherPub, tr, &ok),
		"other transcript": cli.VerifyServerAuth(pub, []byte("transcript-B"), &ok),
		"missing sig":      cli.VerifyServerAuth(pub, tr, &protocol.PskAuthResponse{Status: protocol.PskAuthStatus_Ok}),
	}
	// Status is inside the signed bytes: an ok signature on a bad_psk status fails.
	swapped := ok
	swapped.Status = protocol.PskAuthStatus_BadPsk
	cases["status swapped"] = cli.VerifyServerAuth(pub, tr, &swapped)
	for name, err := range cases {
		if !errors.As(err, &sae) {
			t.Errorf("%s: err = %v, want *cli.ServerAuthError", name, err)
		}
	}
}

// A signature over the same transcript and status but without the context
// string must not verify: the context string is part of the signed message.
func TestVerifyServerAuth_ContextStringIsSigned(t *testing.T) {
	pub, priv := newKey(t)
	tr := []byte("transcript")
	raw := append(append([]byte{}, tr...), byte(protocol.PskAuthStatus_Ok))
	resp := protocol.PskAuthResponse{Status: protocol.PskAuthStatus_Ok}
	resp.SetServerSig(ed25519.Sign(priv, raw))
	if err := cli.VerifyServerAuth(pub, tr, &resp); err == nil {
		t.Fatal("a signature without the context string verified")
	}
}

func TestParseServerPubKey(t *testing.T) {
	pub, _ := newKey(t)
	h := hex.EncodeToString(pub)
	got, err := cli.ParseServerPubKey(h + "\n")
	if err != nil || !got.Equal(pub) {
		t.Fatalf("trailing newline: got %x, %v", got, err)
	}
	if _, err := cli.ParseServerPubKey(h[:62]); err == nil || !strings.Contains(err.Error(), "32") {
		t.Fatalf("short key: err = %v, want a message naming 32 bytes", err)
	}
	if _, err := cli.ParseServerPubKey("zz" + h[2:]); err == nil {
		t.Fatal("non-hex key parsed")
	}
}

func TestParseServerKeySeed(t *testing.T) {
	_, priv := newKey(t)
	got, err := cli.ParseServerKeySeed(hex.EncodeToString(priv.Seed()) + "\n")
	if err != nil || !got.Equal(priv) {
		t.Fatalf("seed round trip: %v", err)
	}
	if _, err := cli.ParseServerKeySeed("abcd"); err == nil {
		t.Fatal("short seed parsed")
	}
}

func TestResolveServerPubKey(t *testing.T) {
	pub, _ := newKey(t)
	h := hex.EncodeToString(pub)
	file := filepath.Join(t.TempDir(), "server-key.pub")
	if err := os.WriteFile(file, []byte(h+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if k, err := cli.ResolveServerPubKey("", ""); k != nil || err != nil {
		t.Fatalf("nothing configured: got %x, %v; want nil, nil", k, err)
	}
	if k, err := cli.ResolveServerPubKey("", file); err != nil || !k.Equal(pub) {
		t.Fatalf("file: %v", err)
	}
	// The value wins over the file, even a file that does not exist.
	if k, err := cli.ResolveServerPubKey(h, filepath.Join(t.TempDir(), "absent")); err != nil || !k.Equal(pub) {
		t.Fatalf("value over file: %v", err)
	}
	// A configured file that cannot be read is an error, not "unset".
	if _, err := cli.ResolveServerPubKey("", filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("unreadable configured file resolved to no key")
	}
}

func TestCheckPskAuthReply(t *testing.T) {
	pub, priv := newKey(t)
	tr := []byte("tr")

	ok := cli.PskAuthReply{Resp: signedResp(t, priv, tr, protocol.PskAuthStatus_Ok)}
	if err := cli.CheckPskAuthReply(ok, pub, tr); err != nil {
		t.Fatalf("verified ok: %v", err)
	}

	bad := cli.PskAuthReply{Resp: signedResp(t, priv, tr, protocol.PskAuthStatus_BadPsk)}
	var rej *cli.PskRejectedError
	if err := cli.CheckPskAuthReply(bad, pub, tr); !errors.As(err, &rej) || rej.Code != protocol.PskAuthStatus_BadPsk {
		t.Fatalf("verified bad_psk: err = %v, want PskRejectedError(bad_psk)", err)
	}

	// Forged rejection: must be ServerAuthError, NOT PskRejectedError, or an
	// impostor could make a runner exit for good.
	_, otherPriv := newKey(t)
	forged := cli.PskAuthReply{Resp: signedResp(t, otherPriv, tr, protocol.PskAuthStatus_BadPsk)}
	err := cli.CheckPskAuthReply(forged, pub, tr)
	var sae *cli.ServerAuthError
	if !errors.As(err, &sae) || errors.As(err, &rej) {
		t.Fatalf("forged bad_psk: err = %v, want ServerAuthError only", err)
	}
}

func TestDecodePskAuthReply(t *testing.T) {
	_, priv := newKey(t)
	resp := signedResp(t, priv, []byte("tr"), protocol.PskAuthStatus_Ok)
	payload, err := resp.Append(nil)
	if err != nil {
		t.Fatal(err)
	}
	if r := cli.DecodePskAuthReply(payload); r.Err != nil || len(r.Resp.ServerSig) != 64 {
		t.Fatalf("new-format payload: %+v", r)
	}
	// An old server sends the status byte alone.
	r := cli.DecodePskAuthReply([]byte{byte(protocol.PskAuthStatus_Ok)})
	if r.Err == nil || !strings.Contains(r.Err.Error(), "server response did not decode") {
		t.Fatalf("old-format payload: err = %v", r.Err)
	}
	if err := cli.CheckPskAuthReply(r, nil, nil); err != r.Err {
		t.Fatalf("CheckPskAuthReply must surface the decode error even with no key: %v", err)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./cli/ -run 'ServerAuth|ServerPubKey|ServerKeySeed|PskAuthReply' -count=1`
Expected: FAIL to compile (`cli.SignServerAuth` undefined, `SetServerSig` undefined).

- [ ] **Step 3: Change the schema and regenerate**

In `runner/protocol/message.bgn`, replace the `PskAuthResponse` block with:

```
# PskAuthResponse is the server's accept/reject for a PskAuthRequest.
# On-wire: [0x45] + PskAuthResponse(brgen-encoded).
#
# server_sig authenticates the SERVER to the client: Ed25519 over
# "agent-harness server-auth v1" || 0x00 || transcript || status, where
# transcript is the objproto handshake transcript the client's binder also
# covers (cli.SignServerAuth). The server's control listener always sends 64
# bytes; the runner's data-plane listener sends 0. A client that has the
# server's public key configured refuses a missing or wrong signature.
format PskAuthResponse:
    status :PskAuthStatus
    server_sig_len :u8
    server_sig :[server_sig_len]u8
```

Run: `make protoregen`
Then: `git diff --stat runner/protocol/` — expect `message.go` changed; `grep -n 'func (t \*PskAuthResponse) SetServerSig' runner/protocol/message.go` must print one line.

- [ ] **Step 4: Write `cli/server_auth.go`**

```go
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
```

- [ ] **Step 5: Run the tests**

Run: `go test ./cli/ -run 'ServerAuth|ServerPubKey|ServerKeySeed|PskAuthReply' -count=1`
Expected: PASS. (Other packages may not compile yet only if they construct `PskAuthResponse` positionally — run `go build ./...` and expect success; the new fields are zero-valued elsewhere.)

- [ ] **Step 6: Commit**

```bash
git add runner/protocol/message.bgn runner/protocol/message.go cli/server_auth.go cli/server_auth_test.go
git commit -m "feat(auth): PskAuthResponse carries a server signature; sign/verify/parse in cli"
```

---

### Task 2: The server signs every response

**Files:**
- Modify: `server/psk.go` (`pskGate`, `sendPskResponse`, every call in `Check`)
- Modify: `server/server.go` (`Config.ServerKey`, `New`, the gate setup near `newPSKGate(s.cfg.PSK)`)
- Modify: `cmd/harness-server/main.go` (flag, `resolveServerKey`, `server.Config`)
- Test: `server/psk_test.go`, `cmd/harness-server/main_test.go`

**Interfaces:**
- Consumes: `cli.SignServerAuth`, `cli.ParseServerKeySeed`, `cli.VerifyServerAuth` (Task 1)
- Produces: `server.Config.ServerKey ed25519.PrivateKey`; `pskGate.serverKey ed25519.PrivateKey`; `func resolveServerKey(file, dataDir string) (ed25519.PrivateKey, error)` in `cmd/harness-server`; `const serverKeyFileName = "server-key"`

- [ ] **Step 1: Write the failing tests**

Append to `server/psk_test.go`:

```go
// parsePskResponseFull decodes the whole response, signature included.
func parsePskResponseFull(t *testing.T, sent []byte) protocol.PskAuthResponse {
	t.Helper()
	if len(sent) < 2 || appwire.AppKind(sent[0]) != appwire.AppKind_PskAuth {
		t.Fatalf("not a PskAuth response: %v", sent)
	}
	var resp protocol.PskAuthResponse
	if err := resp.DecodeExact(sent[1:]); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

// Every status Check can send is signed, rejections included (spec D4).
func TestPSKGate_SignsEveryStatus(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	psk := []byte("s3cr3t")
	cases := map[protocol.PskAuthStatus][]byte{
		protocol.PskAuthStatus_Ok:         buildPskAuthData(t, buildOperatorClientHello(psk, testTranscript)),
		protocol.PskAuthStatus_BadPsk:     buildPskAuthData(t, buildOperatorClientHello([]byte("wrong"), testTranscript)),
		protocol.PskAuthStatus_NoIdentity: {byte(appwire.AppKind_PskAuth), 0xff},
		protocol.PskAuthStatus_BadTicket:  buildPskAuthData(t, func() *protocol.PskAuthRequest {
			req := &protocol.PskAuthRequest{Role: protocol.AuthRole_Client}
			b, _ := cli.ComputePSKBinder(psk, testTranscript)
			req.SetBinder(b)
			req.SetClientHello(protocol.ClientHello{Kind: protocol.ClientKind_Agent}) // no AgentInfo
			return req
		}()),
	}
	for want, data := range cases {
		g := newPSKGate(psk)
		g.serverKey = priv
		var sent []byte
		g.Check(data, testTranscript, func(b []byte) { sent = append(sent, b...) })
		resp := parsePskResponseFull(t, sent)
		if resp.Status != want {
			t.Errorf("status = %v, want %v", resp.Status, want)
			continue
		}
		if err := cli.VerifyServerAuth(pub, testTranscript, &resp); err != nil {
			t.Errorf("%v: %v", want, err)
		}
	}
}

// server.New gives every server a key, so the gate it builds always signs.
func TestNew_AlwaysHasServerKey(t *testing.T) {
	s := New(Config{})
	if len(s.cfg.ServerKey) != ed25519.PrivateKeySize {
		t.Fatalf("server.New left ServerKey unset (len %d)", len(s.cfg.ServerKey))
	}
}
```

Add `"crypto/ed25519"` and `"crypto/rand"` to the test imports.

Append to `cmd/harness-server/main_test.go`:

```go
func TestResolveServerKey(t *testing.T) {
	dir := t.TempDir()
	k1, err := resolveServerKey("", dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, serverKeyFileName))
	if err != nil {
		t.Fatalf("key not generated into data dir: %v", err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %v, want 0600", st.Mode().Perm())
	}
	pubHex, err := os.ReadFile(filepath.Join(dir, serverKeyFileName+".pub"))
	if err != nil {
		t.Fatalf("public key not written: %v", err)
	}
	pub, err := cli.ParseServerPubKey(string(pubHex))
	if err != nil || !pub.Equal(k1.Public()) {
		t.Fatalf("server-key.pub does not hold the key's public half: %v", err)
	}
	// Second start reuses the key.
	k2, err := resolveServerKey("", dir)
	if err != nil || !k2.Equal(k1) {
		t.Fatalf("restart produced a different key: %v", err)
	}
	// A corrupt key file is a startup error, never silently regenerated.
	if err := os.WriteFile(filepath.Join(dir, serverKeyFileName), []byte("nothex\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveServerKey("", dir); err == nil {
		t.Fatal("corrupt key file accepted")
	}
	// No flag and no data dir: nowhere to keep a key.
	if _, err := resolveServerKey("", ""); err == nil {
		t.Fatal("no data dir and no --server-key-file must fail")
	}
}
```

Add `"path/filepath"`, `"runtime"`, `"os"` and the `cli` import as needed.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./server/ -run 'SignsEveryStatus|AlwaysHasServerKey' -count=1 && go test ./cmd/harness-server/ -run ResolveServerKey -count=1`
Expected: FAIL to compile (`g.serverKey`, `s.cfg.ServerKey`, `resolveServerKey` undefined).

- [ ] **Step 3: Sign in the gate**

In `server/psk.go`:
- add `"crypto/ed25519"` to imports;
- add to `pskGate`, after `operatorPSK`:

```go
	// serverKey signs every PskAuthResponse so a client holding the public
	// half can tell this server from an impostor. server.New guarantees one.
	serverKey ed25519.PrivateKey
```

- replace `sendPskResponse` with a method:

```go
// respond encodes and sends [AppKind_PskAuth] + PskAuthResponse{status, sig}.
// Rejections are signed too: an unsigned bad_psk is exactly what an impostor
// would send to stop a runner for good.
func (g *pskGate) respond(sendFn func([]byte), transcript []byte, status protocol.PskAuthStatus) {
	resp := protocol.PskAuthResponse{Status: status}
	if g.serverKey != nil {
		resp.SetServerSig(cli.SignServerAuth(g.serverKey, transcript, status))
	}
	out, err := resp.Append([]byte{byte(appwire.AppKind_PskAuth)})
	if err != nil {
		slog.Error("pskGate: failed to encode PskAuthResponse", "err", err)
		return
	}
	sendFn(out)
}
```

- replace every `sendPskResponse(sendFn, X)` in `Check` with `g.respond(sendFn, transcript, X)`. Afterwards `grep -n sendPskResponse server/` must print nothing.

- [ ] **Step 4: Give every server a key**

In `server/server.go`, add to `Config` after `OperatorPSK`:

```go
	// ServerKey signs every PskAuthResponse (spec 2026-10-11 server signature
	// auth). nil → New generates an ephemeral one, so a server always signs;
	// harness-server passes the persistent key from <data-dir>/server-key.
	ServerKey ed25519.PrivateKey
```

In `New`, right after the `cfg.Logger` default:

```go
	if cfg.ServerKey == nil {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			panic("server: generate ephemeral server key: " + err.Error())
		}
		cfg.ServerKey = priv
	}
```

(`crypto/rand` may already be imported in `server.go`; add `crypto/ed25519` and, if missing, `crypto/rand`.) Where the gate is built (`gate := newPSKGate(s.cfg.PSK)` / `gate.operatorPSK = ...`) add `gate.serverKey = s.cfg.ServerKey`.

- [ ] **Step 5: Resolve the key in `harness-server`**

In `cmd/harness-server/main.go` add the flag beside `operatorPSKFile`:

```go
	serverKeyFile = flag.String("server-key-file", "", "path to the server's Ed25519 signing key (hex seed); auto-generated if absent. Default <data-dir>/server-key. Its public half is written to <data-dir>/server-key.pub and logged at startup: configure it on clients as HARNESS_SERVER_PUBKEY (runners: --server-pubkey) so they refuse an impostor server.")
```

Add below `resolveOperatorPSK`:

```go
// serverKeyFileName is where the server's signing key lives when
// --server-key-file is not given; its public half goes to <name>.pub beside it.
const serverKeyFileName = "server-key"

// resolveServerKey loads the server's Ed25519 key, generating it on first run
// the way resolveOperatorPSK does. A file that exists but does not parse is an
// error: regenerating would silently invalidate every client's pinned key.
func resolveServerKey(file, dataDir string) (ed25519.PrivateKey, error) {
	if file == "" {
		if dataDir == "" {
			return nil, errors.New("no --server-key-file and no --data-dir to keep a server key in")
		}
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			return nil, fmt.Errorf("data-dir for %s: %w", serverKeyFileName, err)
		}
		file = filepath.Join(dataDir, serverKeyFileName)
	}
	var key ed25519.PrivateKey
	data, err := os.ReadFile(file)
	switch {
	case err == nil && strings.TrimSpace(string(data)) != "":
		if key, err = cli.ParseServerKeySeed(string(data)); err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
	case err == nil || errors.Is(err, os.ErrNotExist):
		_, key, err = ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("server key generate: %w", err)
		}
		if err := os.WriteFile(file, []byte(hex.EncodeToString(key.Seed())+"\n"), 0o600); err != nil {
			return nil, fmt.Errorf("server key write: %w", err)
		}
		slog.Info("generated server key", "path", file)
	default:
		return nil, fmt.Errorf("server key read: %w", err)
	}
	pubHex := hex.EncodeToString(key.Public().(ed25519.PublicKey))
	if dataDir != "" {
		pubPath := filepath.Join(dataDir, serverKeyFileName+".pub")
		if err := os.WriteFile(pubPath, []byte(pubHex+"\n"), 0o644); err != nil {
			return nil, fmt.Errorf("server public key write: %w", err)
		}
	}
	slog.Info("server public key", "pubkey", pubHex)
	return key, nil
}
```

Call it after the operator-PSK block:

```go
	serverKey, err := resolveServerKey(strings.TrimSpace(*serverKeyFile), *dataDir)
	if err != nil {
		slog.Error("server key setup failed", "err", err)
		os.Exit(1)
	}
```

and pass `ServerKey: serverKey,` in `server.Config{...}`. Add imports `crypto/ed25519`, `crypto/rand` (check: `resolvePSK` already uses `rand.Read` — reuse that import), `encoding/hex` (already used by `resolvePSK`).

- [ ] **Step 6: Run the tests**

Run: `go test ./server/ ./cmd/harness-server/ -count=1`
Expected: PASS (existing gate tests still pass: they build gates with no `serverKey` and only read `Status`).

- [ ] **Step 7: Commit**

```bash
git add server/psk.go server/psk_test.go server/server.go cmd/harness-server/main.go cmd/harness-server/main_test.go
git commit -m "feat(auth): the server signs every PskAuthResponse with a persistent Ed25519 key"
```

---

### Task 3: harness-cli / harness-tui / agents verify the server

**Files:**
- Create: `cli/server_pubkey.go` (`//go:build !js`), `cli/server_pubkey_js.go` (`//go:build js`)
- Modify: `cli/client.go` (`dialOn`), `cli/psk.go` (`SendMergedHandshake`), `cli/psk_js.go` (`SendMergedHandshake`)
- Modify (tests, mechanical): `cli/psk_test.go`
- Test: `cli/psk_test.go`

**Interfaces:**
- Consumes: `PskAuthReply`, `DecodePskAuthReply`, `CheckPskAuthReply`, `ResolveServerPubKey`, `ServerPubKeyEnv`, `ServerPubKeyFileEnv` (Task 1)
- Produces: `func GetServerPubKey() (ed25519.PublicKey, error)` (both builds; js returns `nil, nil`);
  `func SendMergedHandshake(ctx context.Context, sendFn func([]byte) error, psk, transcript []byte, operatorKind protocol.ClientKind, serverKey ed25519.PublicKey, respCh <-chan PskAuthReply) error` (both builds)

- [ ] **Step 1: Update the existing tests to the new signature and add the new ones**

In `cli/psk_test.go`, mechanically:
- every `make(chan protocol.PskAuthResponse, 1)` / `make(chan protocol.PskAuthResponse)` → `make(chan cli.PskAuthReply, 1)` / `make(chan cli.PskAuthReply)`;
- every send `respCh <- protocol.PskAuthResponse{...}` → `respCh <- cli.PskAuthReply{Resp: protocol.PskAuthResponse{...}}`;
- every `cli.SendMergedHandshake(ctx, sendFn, psk, transcript, kind, respCh)` → `cli.SendMergedHandshake(ctx, sendFn, psk, transcript, kind, nil, respCh)`.

Then append:

```go
// A pinned client must not take an unsigned ok: that is the impostor case.
func TestSendMergedHandshake_PinnedRefusesUnsignedOk(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	respCh := make(chan cli.PskAuthReply, 1)
	respCh <- cli.PskAuthReply{Resp: protocol.PskAuthResponse{Status: protocol.PskAuthStatus_Ok}}
	err := cli.SendMergedHandshake(context.Background(), func([]byte) error { return nil },
		nil, []byte("tr"), protocol.ClientKind_Cli, pub, respCh)
	var sae *cli.ServerAuthError
	if !errors.As(err, &sae) {
		t.Fatalf("err = %v, want *cli.ServerAuthError", err)
	}
}

// A pinned client takes a correctly signed ok.
func TestSendMergedHandshake_PinnedAcceptsSignedOk(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	tr := []byte("tr")
	resp := protocol.PskAuthResponse{Status: protocol.PskAuthStatus_Ok}
	resp.SetServerSig(cli.SignServerAuth(priv, tr, protocol.PskAuthStatus_Ok))
	respCh := make(chan cli.PskAuthReply, 1)
	respCh <- cli.PskAuthReply{Resp: resp}
	if err := cli.SendMergedHandshake(context.Background(), func([]byte) error { return nil },
		nil, tr, protocol.ClientKind_Cli, pub, respCh); err != nil {
		t.Fatal(err)
	}
}

// An undecodable reply (old server) returns at once with the skew message.
func TestSendMergedHandshake_UndecodableReplyIsImmediate(t *testing.T) {
	respCh := make(chan cli.PskAuthReply, 1)
	respCh <- cli.DecodePskAuthReply([]byte{0})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := cli.SendMergedHandshake(ctx, func([]byte) error { return nil },
		nil, nil, protocol.ClientKind_Cli, nil, respCh)
	if err == nil || !strings.Contains(err.Error(), "server response did not decode") {
		t.Fatalf("err = %v", err)
	}
	var rej *cli.PskRejectedError
	if errors.As(err, &rej) {
		t.Fatal("a decode failure must not be a PskRejectedError")
	}
}

func TestGetServerPubKey_Env(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	t.Setenv(cli.ServerPubKeyEnv, hex.EncodeToString(pub))
	t.Setenv(cli.ServerPubKeyFileEnv, "")
	got, err := cli.GetServerPubKey()
	if err != nil || !got.Equal(pub) {
		t.Fatalf("got %x, %v", got, err)
	}
	t.Setenv(cli.ServerPubKeyEnv, "not-a-key")
	if _, err := cli.GetServerPubKey(); err == nil {
		t.Fatal("malformed HARNESS_SERVER_PUBKEY accepted")
	}
}
```

Add imports `crypto/ed25519`, `crypto/rand`, `encoding/hex`, `errors`, `strings`, `time` as needed.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./cli/ -run 'SendMergedHandshake|GetServerPubKey' -count=1`
Expected: FAIL to compile (signature mismatch, `GetServerPubKey` undefined).

- [ ] **Step 3: Env resolution, both builds**

`cli/server_pubkey.go`:

```go
//go:build !js

package cli

import (
	"crypto/ed25519"
	"os"
)

// GetServerPubKey reads the server public key this process verifies against
// (HARNESS_SERVER_PUBKEY, then HARNESS_SERVER_PUBKEY_FILE). Neither set →
// nil, nil. Set but unusable → error: dialing without the check the operator
// asked for would be a silent downgrade.
func GetServerPubKey() (ed25519.PublicKey, error) {
	return ResolveServerPubKey(os.Getenv(ServerPubKeyEnv), os.Getenv(ServerPubKeyFileEnv))
}
```

`cli/server_pubkey_js.go`:

```go
//go:build js

package cli

import "crypto/ed25519"

// GetServerPubKey is nil in the browser: the page and its wasm are served by
// the server being authenticated, so an impostor serves a page without the
// check (spec 2026-10-11, "Why the WebUI is out of scope").
func GetServerPubKey() (ed25519.PublicKey, error) { return nil, nil }
```

- [ ] **Step 4: `SendMergedHandshake` (native and js)**

In BOTH `cli/psk.go` and `cli/psk_js.go`, change the signature to

```go
func SendMergedHandshake(ctx context.Context, sendFn func([]byte) error, psk, transcript []byte, operatorKind protocol.ClientKind, serverKey ed25519.PublicKey, respCh <-chan PskAuthReply) error {
```

and replace the `select` at the end with:

```go
	select {
	case reply := <-respCh:
		// Signature first, then status: see CheckPskAuthReply.
		return CheckPskAuthReply(reply, serverKey, transcript)
	case <-ctx.Done():
		// Transport drop / cancellation mid-handshake — RETRYABLE.
		return ctx.Err()
	}
```

Add `"crypto/ed25519"` to both files' imports. In `cli/psk.go` extend the doc comment's error list with: `//   - signature missing/wrong with serverKey set → *ServerAuthError (retryable)` and `//   - undecodable response → retryable error ("server response did not decode")`.

- [ ] **Step 5: `dialOn` in `cli/client.go`**

Before `psk := resolveBinderPSK()`:

```go
	serverKey, err := GetServerPubKey()
	if err != nil {
		pc.Close()
		// A configuration error, not a server answer: retrying cannot fix it.
		return nil, &PSKAuthError{Err: err}
	}
```

Change `pskRespCh := make(chan protocol.PskAuthResponse, 1)` to `pskRespCh := make(chan PskAuthReply, 1)` and the handler body to:

```go
		if kind == appwire.AppKind_PskAuth && len(payload) > 0 {
			select {
			case pskRespCh <- DecodePskAuthReply(payload):
			default:
			}
			return
		}
```

and pass `serverKey` to `SendMergedHandshake(pskCtx, ..., kind, serverKey, pskRespCh)`.

`PSKAuthError` is what `PersistLoop` treats as fatal; a malformed configured key is fatal at dial time for the same reason a wrong PSK is — the spec's D9 "startup error". Note this in the comment as written above.

- [ ] **Step 6: Run the tests and the wasm build**

Run: `go test ./cli/ -count=1 && make wasm-check`
Expected: PASS; `wasm-check` compiles the js build.

- [ ] **Step 7: Commit**

```bash
git add cli/server_pubkey.go cli/server_pubkey_js.go cli/client.go cli/psk.go cli/psk_js.go cli/psk_test.go
git commit -m "feat(auth): harness-cli, harness-tui and agents verify the server's signature when a key is configured"
```

---

### Task 4: The runner verifies the server and forwards the key to agents

**Files:**
- Modify: `runner/connect.go` (`Config.ServerPubKey`, `driveAfterConn` handler + call, `sendRunnerMergedHandshake`, Session construction)
- Modify: `runner/session.go` (`Session.ServerPubKey`, the two `AgentEnvSpec{...}` literals)
- Modify: `runner/exec_run.go` (the `AgentEnvSpec{...}` literal)
- Modify: `runner/agentenv.go` (`AgentEnvSpec.ServerPubKey`, `BuildAgentEnv`)
- Modify: `cmd/agent-runner/main.go` (flags, resolution, `runner.Config`)
- Modify (test, mechanical): `runner/connect_test.go`
- Test: `runner/connect_test.go`, `runner/agentenv_test.go`, `cmd/agent-runner/` test file holding `mainConfig` flag tests

**Interfaces:**
- Consumes: `cli.PskAuthReply`, `cli.DecodePskAuthReply`, `cli.CheckPskAuthReply`, `cli.ResolveServerPubKey`, `cli.ServerPubKeyEnv`, `cli.ServerPubKeyFileEnv`, `cli.ServerAuthError` (Tasks 1, 3)
- Produces: `runner.Config.ServerPubKey ed25519.PublicKey`; `Session.ServerPubKey ed25519.PublicKey`; `AgentEnvSpec.ServerPubKey ed25519.PublicKey`;
  `func sendRunnerMergedHandshake(ctx context.Context, sendFn func([]byte) error, psk, transcript []byte, cfg Config, respCh <-chan cli.PskAuthReply) error` (key read from `cfg.ServerPubKey`)

- [ ] **Step 1: Write the failing tests**

In `runner/connect_test.go`, mechanically update `TestRunnerMergedHandshakeEncodesRoleRunner`: `make(chan protocol.PskAuthResponse, 1)` → `make(chan cli.PskAuthReply, 1)` and its send to `cli.PskAuthReply{Resp: ...}`. Append:

```go
// A forged bad_psk must not become a PskRejectedError: Connect wraps a
// non-retryable PskRejectedError as fatal, and the runner would exit for good.
func TestRunnerHandshake_ForgedRejectionIsNotFatal(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	_, impostor, _ := ed25519.GenerateKey(rand.Reader)
	tr := []byte("tr")
	resp := protocol.PskAuthResponse{Status: protocol.PskAuthStatus_BadPsk}
	resp.SetServerSig(cli.SignServerAuth(impostor, tr, protocol.PskAuthStatus_BadPsk))
	respCh := make(chan cli.PskAuthReply, 1)
	respCh <- cli.PskAuthReply{Resp: resp}

	cfg := Config{RunnerID: NewRunnerID(), ServerPubKey: pub}
	err := sendRunnerMergedHandshake(context.Background(), func([]byte) error { return nil }, nil, tr, cfg, respCh)
	var sae *cli.ServerAuthError
	var rej *cli.PskRejectedError
	if !errors.As(err, &sae) || errors.As(err, &rej) {
		t.Fatalf("err = %v, want ServerAuthError and not PskRejectedError", err)
	}
}

func TestRunnerHandshake_UndecodableReplyIsRetryable(t *testing.T) {
	respCh := make(chan cli.PskAuthReply, 1)
	respCh <- cli.DecodePskAuthReply([]byte{0})
	err := sendRunnerMergedHandshake(context.Background(), func([]byte) error { return nil },
		nil, nil, Config{RunnerID: NewRunnerID()}, respCh)
	if err == nil || !strings.Contains(err.Error(), "server response did not decode") {
		t.Fatalf("err = %v", err)
	}
	var rej *cli.PskRejectedError
	if errors.As(err, &rej) {
		t.Fatal("must not be a PskRejectedError")
	}
}
```

Append to `runner/agentenv_test.go`:

```go
func TestBuildAgentEnv_ServerPubKey(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	env := BuildAgentEnv(AgentEnvSpec{ServerPubKey: pub})
	want := "HARNESS_SERVER_PUBKEY=" + hex.EncodeToString(pub)
	if !slices.Contains(env, want) {
		t.Fatalf("env lacks %q: %v", want, env)
	}
	for _, kv := range BuildAgentEnv(AgentEnvSpec{}) {
		if strings.HasPrefix(kv, "HARNESS_SERVER_PUBKEY=") {
			t.Fatalf("no key configured, but env has %q", kv)
		}
	}
}
```

Add imports (`crypto/ed25519`, `crypto/rand`, `encoding/hex`, `errors`, `slices`, `strings`) as needed. `NewRunnerID` is `runner/identity.go`.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./runner/ -run 'RunnerHandshake|RunnerMergedHandshake|BuildAgentEnv_ServerPubKey' -count=1`
Expected: FAIL to compile.

- [ ] **Step 3: Verify in `runner/connect.go`**

- `Config`: after `PSK []byte` add

```go
	// ServerPubKey, when non-nil, is the server's Ed25519 public key: the
	// handshake refuses a PskAuthResponse whose signature does not verify
	// against it, and every spawned agent gets it as HARNESS_SERVER_PUBKEY.
	// nil = no verification. agent-runner resolves it at startup.
	ServerPubKey ed25519.PublicKey
```

- `RunHandle.pskRespCh` type → `chan cli.PskAuthReply`; its `make` → `make(chan cli.PskAuthReply, 1)`.
- In the `SetOnControl` handler, replace the decode block with:

```go
		if kind == appwire.AppKind_PskAuth && len(payload) > 0 {
			select {
			case h.pskRespCh <- cli.DecodePskAuthReply(payload):
			default:
			}
			return
		}
```

- `sendRunnerMergedHandshake`: change the `respCh` parameter type to `<-chan cli.PskAuthReply` and replace the `select` with

```go
	select {
	case reply := <-respCh:
		// Signature before status (cli.CheckPskAuthReply): a forged bad_psk
		// must come back as a retryable ServerAuthError, not as the
		// PskRejectedError that Connect turns fatal. A rejection that DOES
		// verify still gets its Code via cli.NewPskRejectedError — this is the
		// third PskRejectedError creation site the 2026-07-16 note warns about.
		return cli.CheckPskAuthReply(reply, cfg.ServerPubKey, transcript)
	case <-ctx.Done():
		// Transport drop / cancellation mid-handshake — RETRYABLE: a server
		// restart that interrupts the handshake must trigger reconnect, not exit.
		return ctx.Err()
	}
```

- Session construction (the block with `PSK: psk,`): add `ServerPubKey: cfg.ServerPubKey,`.
- Add `"crypto/ed25519"` to imports.

- [ ] **Step 4: Thread the key to agents**

- `runner/session.go`: after the `PSK []byte` field of `Session` add `ServerPubKey ed25519.PublicKey // forwarded to agents as HARNESS_SERVER_PUBKEY`; in BOTH `BuildAgentEnv(AgentEnvSpec{...})` literals add `ServerPubKey: s.ServerPubKey,` after `PSK: s.PSK,`.
- `runner/exec_run.go`: same addition in its `AgentEnvSpec{...}` literal.
- `runner/agentenv.go`: after the `PSK []byte` field add

```go
	// ServerPubKey, when non-nil, is forwarded as HARNESS_SERVER_PUBKEY so an
	// in-task harness-cli verifies the server like its runner does. Not a
	// secret, unlike PSK.
	ServerPubKey ed25519.PublicKey
```

  and in `BuildAgentEnv`, after the `HARNESS_PSK` block:

```go
	if len(s.ServerPubKey) > 0 {
		env = append(env, cli.ServerPubKeyEnv+"="+hex.EncodeToString(s.ServerPubKey))
	}
```

  Add `"crypto/ed25519"` and `"github.com/on-keyday/agent-harness/cli"` to `runner/agentenv.go`'s imports (`encoding/hex` is already there; package `runner` already imports `cli` in `connect.go`, so there is no cycle).

- [ ] **Step 5: Flags in `cmd/agent-runner/main.go`**

`mainConfig`: add `ServerPubKey string` and `ServerPubKeyFile string`. In `bindFlags`, after `--psk-file`:

```go
	fs.StringVar(&c.ServerPubKey, "server-pubkey", c.ServerPubKey, "the server's public key (hex, from <data-dir>/server-key.pub on the server host); when set, the runner refuses a server that cannot prove it holds the matching private key, and agents inherit it (env: HARNESS_SERVER_PUBKEY)")
	fs.StringVar(&c.ServerPubKeyFile, "server-pubkey-file", c.ServerPubKeyFile, "path to a file holding --server-pubkey (env: HARNESS_SERVER_PUBKEY_FILE)")
```

After `resolvedPSK := resolvePSK(...)`:

```go
	pubVal, pubFile := cfg.ServerPubKey, cfg.ServerPubKeyFile
	if pubVal == "" {
		pubVal = os.Getenv(cli.ServerPubKeyEnv)
	}
	if pubFile == "" {
		pubFile = os.Getenv(cli.ServerPubKeyFileEnv)
	}
	serverPubKey, err := cli.ResolveServerPubKey(pubVal, pubFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-runner: %v\n", err)
		os.Exit(1)
	}
	if serverPubKey != nil {
		slog.Info("server key configured: the handshake verifies the server", "pubkey", hex.EncodeToString(serverPubKey))
	}
```

Add `"encoding/hex"` to `cmd/agent-runner/main.go`'s imports (it is not there today). Pass `ServerPubKey: serverPubKey,` in the `runner.Config{...}` that sets `PSK: resolvedPSK,`.

- [ ] **Step 6: Run the tests and the sibling grep**

Run: `go test ./runner/ ./cmd/agent-runner/ -count=1`
Expected: PASS.

Run: `grep -n 'PSK: *s.PSK' runner/*.go | grep -v _test` and `grep -n 'ServerPubKey: *s.ServerPubKey' runner/*.go | grep -v _test`
Expected: the same three file:line sites in both outputs.

- [ ] **Step 7: Commit**

```bash
git add runner/connect.go runner/connect_test.go runner/session.go runner/exec_run.go runner/agentenv.go runner/agentenv_test.go cmd/agent-runner/main.go
git commit -m "feat(auth): the runner verifies the server's signature and forwards the key to agents"
```

---

### Task 5: End-to-end, skew check, live run

**Files:**
- Create: `integration/server_auth_e2e_test.go`
- Modify: `scripts/wire-skew-check.sh` (phase 1 outcome list)

**Interfaces:**
- Consumes: everything above; integration helpers `freePort`, `singleAgentProfile`, `waitForRegisteredRunner`, `tempRepo` (or `initRepo`).

- [ ] **Step 1: Write the E2E tests**

`integration/server_auth_e2e_test.go`:

```go
//go:build integration

package integration

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/server"
	"github.com/on-keyday/objtrsf/objproto"
)

// startServerWithKey starts a server that signs with key.
func startServerWithKey(t *testing.T, key ed25519.PrivateKey) objproto.ConnectionID {
	t.Helper()
	addr := freePort(t)
	cid, err := objproto.ParseConnectionID("ws:"+addr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := server.New(server.Config{Addr: addr, DataDir: t.TempDir(), ServerKey: key})
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	})
	time.Sleep(300 * time.Millisecond)
	return cid
}

func genKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func clearPSKEnv(t *testing.T) {
	t.Setenv("HARNESS_PSK", "")
	t.Setenv("HARNESS_PSK_FILE", "")
	t.Setenv(cli.ServerPubKeyFileEnv, "")
}

// The transcript the server signs is the one the client verifies: a real
// handshake with the right key configured must succeed.
func TestServerAuthE2E_PinnedClientAcceptsRealServer(t *testing.T) {
	pub, priv := genKey(t)
	cid := startServerWithKey(t, priv)
	clearPSKEnv(t)
	t.Setenv(cli.ServerPubKeyEnv, hex.EncodeToString(pub))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatalf("pinned client vs genuine server: %v", err)
	}
	c.Close()
}

func TestServerAuthE2E_PinnedClientRefusesImpostor(t *testing.T) {
	pub, _ := genKey(t)
	_, impostor := genKey(t)
	cid := startServerWithKey(t, impostor)
	clearPSKEnv(t)
	t.Setenv(cli.ServerPubKeyEnv, hex.EncodeToString(pub))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err == nil {
		c.Close()
		t.Fatal("pinned client connected to an impostor")
	}
	var sae *cli.ServerAuthError
	if !errors.As(err, &sae) {
		t.Fatalf("err = %v, want *cli.ServerAuthError", err)
	}
}

func TestServerAuthE2E_UnpinnedClientConnects(t *testing.T) {
	_, priv := genKey(t)
	cid := startServerWithKey(t, priv)
	clearPSKEnv(t)
	t.Setenv(cli.ServerPubKeyEnv, "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatalf("unpinned client: %v", err)
	}
	c.Close()
}

// A pinned runner that reaches an impostor never registers there (so it can
// never be assigned a task) and keeps running (retrying), instead of exiting.
func TestServerAuthE2E_PinnedRunnerRefusesImpostor(t *testing.T) {
	pub, _ := genKey(t)
	_, impostor := genKey(t)
	cid := startServerWithKey(t, impostor)
	clearPSKEnv(t)
	t.Setenv(cli.ServerPubKeyEnv, "")

	repo := initRepo(t)
	fake, err := filepath.Abs("../testdata/fake-claude.sh")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runner.Run(ctx, runner.Config{
			RunnerID:         runner.NewRunnerID(),
			ServerCandidates: runner.CandidatesOf(cid),
			AllowedRoots:     []string{repo},
			MaxTasks:         1,
			Profiles:         singleAgentProfile(fake),
			ServerPubKey:     pub,
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})

	dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dcancel()
	op, err := cli.Dial(dctx, cid, protocol.ClientKind_Cli) // unpinned operator
	if err != nil {
		t.Fatal(err)
	}
	defer op.Close()

	if waitForRegisteredRunner(t, op, repo, 4*time.Second) {
		t.Fatal("pinned runner registered with an impostor")
	}
	select {
	case err := <-done:
		t.Fatalf("runner exited instead of retrying: %v", err)
	default:
	}
}
```

The `runner.Config` fields are spelled as in `integration/multi_task_test.go:125-132`; `initRepo(t)` is `integration/e2e_test.go:22`.

- [ ] **Step 2: Run them**

Run: `go test -tags integration ./integration/ -run ServerAuthE2E -count=1 -v`
Expected: PASS, four tests.

- [ ] **Step 3: Negative control**

Temporarily change `return CheckPskAuthReply(reply, serverKey, transcript)` in `cli/psk.go` to `return CheckPskAuthReply(reply, nil, transcript)`; rerun Step 2; expect `TestServerAuthE2E_PinnedClientRefusesImpostor` to FAIL. Revert (`git diff cli/psk.go` must be empty).

- [ ] **Step 4: Teach `wire-skew-check.sh` the new outcome**

In phase 1 of `scripts/wire-skew-check.sh`:
- add `server response did not decode` to the polling `grep -qiE "..."` list;
- add a branch before the `connection refused` branch:

```bash
elif grep -q "server response did not decode" "$TMP/runner.log"; then
  # Outcome 5: the server's RESPONSE format moved (2026-10-11: PskAuthResponse
  # gained server_sig). The NEW runner cannot decode the OLD server's one-byte
  # answer and reports it at once; the skew is exercised, so the same two
  # assertions apply.
  echo "        skew exercised: the old server's PskAuthResponse did not decode"
  runner_alive || fail "runner EXITED against the old server — an undecodable PskAuthResponse is being classified FATAL. A landing would wipe the fleet."
  grep -q "runner exit" "$TMP/runner.log" && fail "runner logged 'runner exit' (fatal path taken on skew)"
  echo "  [1/3] PASS — response undecodable, stayed alive, kept retrying"
```

- [ ] **Step 5: Run the skew check**

Run: `scripts/wire-skew-check.sh`
Expected: `[1/3] PASS — response undecodable…`, `[2/3]` self-heal PASS, `[3/3]` (old runner × new server) PASS. Phase 3 passing is the observation the spec marks "not yet observed": record the result in the commit message.

- [ ] **Step 6: Full verification**

Run: `make check` (or `make test vet` if `check` does not cover both) and `go test -tags integration ./integration/ -count=1 -timeout 600s`.
Expected: PASS. A failure in a test listed under "A family of server flakes" must be rerun at the package level before it is attributed to this change.

- [ ] **Step 7: Live run against binaries**

Use the `dummy-harness` skill (`scripts/dummy-harness.sh`) to start a server and runner from this tree. Then:
1. Read `<data-dir>/server-key.pub` of the dummy server.
2. Restart the dummy runner with `--server-pubkey <that hex>`, exactly as the help text spells it; confirm it registers (`harness-cli ls` shows it).
3. Restart it with `--server-pubkey` set to a different valid key; confirm its log shows `server auth:` and it keeps retrying (does not exit).
4. With `HARNESS_SERVER_PUBKEY=<real hex>` exported, run `harness-cli ls`; with a wrong key, confirm it fails with `server auth:`.
5. Submit a task through the correctly pinned runner whose prompt runs `env | grep HARNESS_SERVER_PUBKEY` (fake agent) or check the spawned agent's env, and confirm the key is present.
Bring the dummy instance down afterwards.

- [ ] **Step 8: Commit**

```bash
git add integration/server_auth_e2e_test.go scripts/wire-skew-check.sh
git commit -m "test(auth): impostor E2E for client and runner; wire-skew-check knows an undecodable PskAuthResponse"
```
