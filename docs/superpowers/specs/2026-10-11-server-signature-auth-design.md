# Server signature authentication — design

## Problem

The control-connection handshake authenticates the client to the server and
never the server to the client.

objproto's handshake is unauthenticated by design (`cli/psk_binder.go`); the
app layer adds a PSK binder that the CLIENT computes and the server checks
(`server/psk.go`, `pskGate.Check`). The server's answer, `PskAuthResponse`, is
one status byte (`runner/protocol/message.bgn`, `format PskAuthResponse`), and
both receivers act on that byte alone (`runner/connect.go`, the `SetOnControl`
handler in the run setup; `cli/client.go`, the `pskRespCh` handler). The
`--tls-cert` listener does not close the gap either: its help text says clients
do not verify the certificate.

Anyone who can answer a client's connection — DNS, a route, a hostile network
on the path — can therefore pose as the server:

1. **A runner executes whatever the impostor dispatches.** The runner buffers
   runner-control messages that arrive during the handshake and replays them
   once the handshake succeeds (`runner/connect.go`, `bufferOrDispatch`). An
   impostor answers `ok` and sends `AssignTask`: code execution on the runner
   host.
2. **An impostor can stop the whole fleet.** `bad_psk` and `bad_ticket` are
   fatal to a runner (`cli.PskRejectedError.Retryable`); a forged status byte
   makes every runner that reaches the impostor exit for good.
3. **An operator surface talks to the impostor** as if it were the server.

The binder does not help against any of these: it stops an attacker from
relaying a captured proof to the REAL server, not from being the server.

Inside the tailnet this deployment uses today, the network layer authenticates
the server's node, so none of the three is reachable there. They become
reachable the moment a client connects over a path the network layer does not
authenticate.

## Decisions taken

**operator** = the human chose it in conversation (2026-10-11), including
everything in the design presented and approved there; **this spec** = the
author chose it while writing — those rows are the ones worth a second look.

| # | Decision | Decided by |
| --- | --- | --- |
| D1 | The server holds an Ed25519 key and signs every `PskAuthResponse`; a client verifies the signature against a public key configured on the client | operator |
| D2 | The public key is configured explicitly. A client with no key configured does not verify (and does not warn); a client with one configured refuses any response whose signature is missing or wrong | operator |
| D3 | Scope: control connections — runner → server, and harness-cli / harness-tui / in-task agent → server. The runner's data-plane listener is out of scope | operator |
| D4 | Rejections are signed too, and the status is inside the signed bytes | operator |
| D5 | The WebUI is out of scope (see below) | operator |
| D6 | A failed verification is RETRYABLE | operator |
| D7 | A `PskAuthResponse` that does not decode is reported at once as a retryable error, instead of being dropped | operator |
| D8 | The server always signs; there is no flag to turn it off | operator |
| D9 | A configured key that does not parse is a startup error, never a silent fall back to "not configured" | operator |
| D10 | Keys are hex text, like the generated PSK (`resolvePSK` writes hex): the private key file holds the 32-byte Ed25519 seed, the public key is 32 bytes | this spec |
| D11 | Files: `<data-dir>/server-key` (seed, 0600) and `<data-dir>/server-key.pub`; flag `--server-key-file` overrides the first | this spec |
| D12 | A verification failure is its own error type, `cli.ServerAuthError`, not a `PskRejectedError` | this spec |

## Why the WebUI is out of scope (D5)

The page, its JavaScript and `main.wasm` are served by the server the browser
connects to (`server/server.go`, the `/` and `/static/` handlers). An impostor
serves its own page, so any check written into the page is the impostor's to
remove. Protecting the browser needs the page to come from somewhere the
impostor does not control; that is a different change. The WASM client still
decodes the new response format (it shares `PskAuthResponse`), and ignores the
signature.

## Wire

```
format PskAuthResponse:
    status :PskAuthStatus
    server_sig_len :u8            # 0 or 64
    server_sig :[server_sig_len]u8
```

The server's control listener always sends 64. The runner's data-plane
listener (`runner/dataplane_accept.go`, `sendPskAuthStatus`) sends 0 (D3).

The signed message is

```
"agent-harness server-auth v1" ‖ 0x00 ‖ transcript ‖ status
```

where `transcript` is this connection's `Connection.GetTranscript()` — the
same bytes the client's binder covers — and `status` is the one status byte.
The context string keeps a signature from being valid in any other use of the
key; the transcript ties it to this connection — the transcript is the
client's hello and the server's ack, each carrying an ECDH key generated per
handshake (objtrsf `objproto/crypto.go`), so a signature taken from one
connection does not verify on another; the status byte makes a rejection as unforgeable as an acceptance
(D4).

Signing and verifying live in `cli`, beside `ComputePSKBinder`, for the same
reason that one does: the server and every client build the same bytes, and
one function is the only way to keep them from drifting.

## Server

- The key is resolved like the operator PSK (`resolveOperatorPSK`):
  `--server-key-file` if given, else `<data-dir>/server-key`; generated when
  absent or blank, written 0600. With no data dir and no flag, startup fails.
- On every start the server writes `<data-dir>/server-key.pub` (public key, hex,
  one line) and logs the public key, so the operator can read it from the
  server host.
- `sendPskResponse` (`server/psk.go`) is the only place the server builds a
  `PskAuthResponse`. It gains the signing key and the transcript; `pskGate`
  holds the key, and `Check` already receives the transcript.

## Clients

Configuration mirrors the PSK's:

| Process | Value | File |
| --- | --- | --- |
| harness-cli, harness-tui, in-task agent | `HARNESS_SERVER_PUBKEY` | `HARNESS_SERVER_PUBKEY_FILE` |
| agent-runner | `--server-pubkey` (env `HARNESS_SERVER_PUBKEY`) | `--server-pubkey-file` (env `HARNESS_SERVER_PUBKEY_FILE`) |

- The runner injects `HARNESS_SERVER_PUBKEY` into every spawned agent when it
  has a key, beside `HARNESS_PSK` in `runner/agentenv.go`. The key is not a
  secret.
- Both handshake call sites verify: `cli/client.go` (every operator surface and
  every in-task agent, including those that reach the server through a runner
  proxy — the binder already proves the transcript is end-to-end on that path)
  and `runner/connect.go`. Verification happens before the status is acted on,
  so a runner that fails it closes the connection without replaying the
  buffered `AssignTask`.
- Key configured, signature missing or wrong → `cli.ServerAuthError`. No key
  configured → the signature is not looked at.

## Errors and version skew

`cli.ServerAuthError` is retryable (D6). It is not a `PskRejectedError`: the
server did not reject anything, the client rejected the server. The runner's
`PersistLoop` already treats every error except a non-retryable
`PskRejectedError` as retryable, so the runner reconnects. Making it fatal
would hand an impostor the fleet-stopping power of problem 2.

Today both receivers DROP a response that does not decode (`if err == nil`),
and the runner's handshake context has no timeout, so the runner waits until
the connection dies. D7 replaces the drop with an immediate retryable error
that names the likely cause: a server older than this client.

Skew, given no compatibility shims in this project:

- **New client, old server**: the old one-byte response does not decode as
  the new format — for every new client, key configured or not. With D7 it is a
  retryable error, and it heals when the server is upgraded. **Restart the
  server first.** Phase 1 of `scripts/wire-skew-check.sh` (new runner × old
  server) proves a skew was exercised by matching the runner log against a
  list of known rejection strings; D7's error is in none of them, so the
  script would report "skew NOT exercised" and fail. Its message joins that
  list, as an outcome of its own.
- **Old client, new server**: the old receivers use `Decode`, not
  `DecodeExact`, so they are expected to ignore the trailing signature. Not
  yet observed; phase 3 of the same script (old runner × new server) checks
  it for the runner.

## Testing

- `cli`: a signature verifies; changing any of transcript, status, key or
  context string fails it; a missing signature fails when a key is configured
  and is ignored when not.
- Server: every status `pskGate.Check` can send carries a valid signature.
- Impostor E2E: a server signing with a different key. A runner with the real
  key configured must close without running an assigned task and keep
  retrying; a harness-cli invocation must fail with `ServerAuthError`.
- `scripts/wire-skew-check.sh` (it builds the old side and the new side as
  real processes).
- One dummy-harness run with the key passed exactly as the help text spells
  it, for the runner flag and the env var each.
