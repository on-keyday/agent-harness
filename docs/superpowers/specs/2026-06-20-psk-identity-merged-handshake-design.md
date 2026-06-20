# Merged PSK + identity handshake (schematized, 1 round-trip, mandatory principal)

Date: 2026-06-20
Status: design — awaiting review

## Problem

Three independent issues, one root:

1. **Enforcement hole (found via live sandbox E2E).** Capability enforcement and
   INFO-scoping gate on the connection's *principal* (`lookupPrincipal(cid)`),
   established by a ClientHello. But the ClientHello is an **opt-in, per-command**
   step: only `submit`/`interactive`/`session`/`forward`/`dial-runner` call
   `SayHelloAuto`. `ls`/`cancel`/`prune`/`file*`/`logs`/`watch` just `Dial` and
   never announce → server `callerCaps` sees a zero principal → **fail-OPEN to
   `Capability_All` (operator)** → caps + INFO-scoping bypassed. Proven live: a
   `--caps none` sandbox task got `submit` denied (it announces) but `ls` saw all
   19 tasks (it does not announce). Task-control is fail-open to operator;
   agentboard is fail-closed (`ac.helloed`). The asymmetry is the bug.

2. **The PSK wire format is not in any schema.** `appwire/psk.go` is hand-written;
   the request (`[0x45] + binder`) and response (`[0x45, status]`) are hand-built
   raw bytes with no `.bgn`. Wire bytes not described by the schema make the
   schema lie (project rule: schema must describe every byte).

3. **Two round-trips.** PSK auth (1 RTT) then identity hello (1 RTT) are separate.

## Decision

Merge PSK auth and identity into a **single, brgen-schematized first message**,
processed in **one round-trip**, making principal establishment **structurally
mandatory** (the PSK gate already rejects any connection whose first message is
not the PSK message — so folding identity into that message means no connection
can reach the handlers without identity). This closes the enforcement hole at the
transport-auth boundary, schematizes the format, and saves a round-trip.

The **implementation may stay hand-written** (binder = HMAC-SHA512 over the
objproto transcript, unchanged), but the **message structure is defined in
`.bgn`** and encoded/decoded via the generated types.

### Authority/identity is shared by three connection roles
client/operator (ClientHello), in-task agent (ClientHello kind=agent + ticket),
and **runner (RunnerHello)** all share the same PSK gate. So the merged message's
identity is a **union** over the existing hello types — no role is special-cased
in the gate.

## Scope

- Define the PSK handshake in `.bgn` (request/response/status), carrying identity.
- Server PSK gate: decode, verify binder, validate identity, establish principal /
  register runner, single accept/reject. Fail-closed: identity required.
- Client (cli task-control via `Dial`, cli agentboard, runner) send the merged
  message; drop the separate `SayHelloAuto`/RunnerHello round-trip.
- Re-verify the live sandbox E2E (confined task: `ls` scoped, `cancel`/`file`/
  `submit` denied).

### Non-goals
- Changing the binder crypto (HMAC-SHA512 / transcript binding stays).
- Changing the capability model / TaskInfo / enforcement gates themselves (landed).
- Multi-tenant / adversarial-client hardening (single-user trust: a client that
  ships a custom non-announcing binary is out of scope; the standard CLI announces).

## Wire schema (in `runner/protocol/message.bgn`)

`ClientHello` and `RunnerHello` already live here, so the merged request — which
references both — must live here too (placing it in `appwire` would invert the
`appwire → runner/protocol` layer dependency). The `AppKind` byte `psk_auth =
0x45` stays in `app.bgn`. `PskAuthStatus` moves from the hand-written
`appwire/psk.go` into this schema.

```
enum AuthRole:
    :u8
    client = 0       # ClientHello identity (operator cli/tui/webui or in-task agent)
    runner = 1       # RunnerHello identity (runner registration)

enum PskAuthStatus:
    :u8
    ok          = 0
    bad_psk     = 1
    bad_ticket  = 2   # binder ok but kind=agent presented an invalid auth ticket
    no_identity = 3   # binder ok but no identity union present (fail-closed)

format PskAuthRequest:
    binder_len :u16              # 0 when no PSK is configured (dev mode); else 64
    binder :[binder_len]u8       # HMAC-SHA512 over the objproto transcript
    role :AuthRole
    role == AuthRole.client => client_hello :ClientHello
    role == AuthRole.runner => runner_hello :RunnerHello

format PskAuthResponse:
    status :PskAuthStatus
```

The on-wire first message is `[0x45] + PskAuthRequest(brgen-encoded)`; the
response is `[0x45] + PskAuthResponse(brgen-encoded)`. (`binder_len` makes the
binder explicit/measurable in the schema rather than "the rest of the bytes".)

## Server gate (`server/psk.go` `pskGate.Check`)

The gate becomes the single accept/reject authority for the handshake, but
**delegates identity recording to the existing handlers** (it stays thin on
business logic):

1. Decode `PskAuthRequest` from `data[1:]`.
2. **Binder**: if a PSK is configured, recompute the expected binder over this
   connection's transcript and constant-time-compare against `binder`; mismatch →
   `PskAuthResponse{bad_psk}` + close. If no PSK configured, skip (binder_len 0).
3. **Identity required**: a `role`+hello must be present → else `no_identity` +
   close. (This is the fail-closed property.)
4. **Ticket** (client role, kind=agent): `Registry.Validate(rid, tid, ticket)`;
   invalid → `PskAuthResponse{bad_ticket}` + close. (Closes the bad-ticket→operator
   fallback.)
5. On accept: `PskAuthResponse{ok}`, mark authed, then **hand the embedded hello
   to the existing dispatch path** so the normal handler records identity:
   client_hello → the task-control ClientHello handler (sets `clientKinds` +
   `principals`); runner_hello → the runner registration handler. The handlers'
   own follow-up messages (e.g. RunnerHelloResponse, assignments) flow as today.

This keeps the gate's *new* knowledge to: decode the request, verify binder,
require identity, validate the agent ticket (it already conceptually gates on the
PSK; ticket validation is the identity analogue). Recording/registration stays in
the handlers via re-dispatch — no runner-registration logic moves into the gate.

`newPSKGate` still starts `authed=false` when a PSK is configured; when no PSK is
configured it must STILL require the identity handshake (so identity is mandatory
in dev too) — i.e. the "no PSK" path no longer means "no first message", it means
"first message is a PskAuthRequest with binder_len 0".

## Client changes

The merged message is built once and sent as the first message on every
connection; the separate hello round-trip is removed.

- **cli task-control (`Dial`)**: after the objproto handshake, build a
  `PskAuthRequest{ binder (or empty), role=client, client_hello = <SayHelloAuto's
  ClientHello: kind=agent+ticket in-task, else the operator kind> }`, send, await
  `PskAuthResponse`. This replaces both `SendAndWaitPSK` and the per-command
  `SayHelloAuto`. **Every** task-control command (ls/cancel/prune/file/logs/...)
  now announces its principal by construction. `Dial` needs the operator kind →
  add a `kind protocol.ClientKind` parameter to `Dial` (callers: cli/* = `Cli`,
  TUI = `Tui`, WebUI = `Webui`); in-task it is overridden to agent as today.
- **cli agentboard (`cli/agent`)**: same merged request (role=client,
  client_hello kind=agent), replacing its PSK + AgentBridge/ClientHello steps.
- **runner (`runner/connect.go`)**: build `PskAuthRequest{ binder, role=runner,
  runner_hello = RunnerHello{...} }`, replacing `SendAndWaitPSK` + the separate
  RunnerHello send.

`SayHelloAuto` / `SayHello` / `SendAndWaitPSK` collapse into the merged-request
builder (kept as internal helpers where useful).

## Error handling / edge cases

- **bad_psk / bad_ticket / no_identity** → connection closed (fail-closed).
- **No PSK configured (dev)**: binder_len 0; identity still required + recorded.
- **Operator (cli/tui/webui, no ticket)**: role=client, ClientHello kind=cli/tui/
  webui, no AgentInfo → accepted, principal is zero → operator (`Capability_All`)
  — unchanged, correct (real operators are the trusted root).
- **Response correlation**: the client awaits exactly one `PskAuthResponse`
  (status). Identity acceptance is folded into that status; the embedded hello no
  longer needs its own separate response for the *handshake* (runner's later
  control traffic — assignments etc. — is unchanged).

## Security: transcript binding preserved

The binder's cryptographic input is **unchanged**: `binder =
HMAC-SHA512(objproto handshake transcript)` keyed by a PSK-derived key (the
TLS-1.3-style PSK binder). The transcript is the ECDH handshake — it does NOT
include the app-layer hello — so folding the hello into the same message does not
change what the binder covers.

- **MITM resistance (unchanged):** an active MITM's two legs have different ECDH
  transcripts, so a relayed binder fails the server's recompute → `bad_psk` +
  close. Identity cannot be accepted on a MITM'd channel because the binder gates
  acceptance.
- **Replay resistance (unchanged):** a captured `PskAuthRequest` replayed on a new
  connection fails — the new connection's ECDH ephemerals yield a different
  transcript (binder mismatch), and the AEAD keys differ (the encrypted hello
  cannot be re-decrypted/re-encrypted for the new channel).
- **Hello integrity/confidentiality:** the whole first message (binder + hello) is
  carried inside the post-ECDH AES-GCM channel, so the hello is integrity- and
  confidentiality-protected, and it only reaches the gate from a peer that
  completed ECDH. The binder additionally proves that peer knows the PSK, so the
  hello rides a PSK-authenticated, MITM-checked channel — the same protection the
  separate-message hello had today. The binder does NOT need to cover the hello:
  the AEAD channel + the per-task ticket (`Registry.Validate`) already authenticate
  it, and the ticket is its own credential independent of the transcript.
- **Processing order (load-bearing):** the gate may *decode* the `PskAuthRequest`
  (binder + hello) before binder verification — decode is parse-only and the brgen
  decoder is robust to malformed input — but it MUST verify the binder BEFORE
  taking any identity action (ticket validation, principal recording, runner
  registration). No identity is trusted until the binder confirms an end-to-end,
  PSK-authenticated channel.

Net: no change to the transcript-binding guarantee; the merge is security-neutral
on the binder and reuses the existing AEAD + ticket protections for the hello.

## Backward compatibility

Single-user dogfood, no external clients: this is a breaking wire change. ALL
participants must be rebuilt together — server, every CLI (including the
harness-cli bridged into runner-host sandboxes), TUI, WebUI (wasm), and the
runner. An old client against a new server (or vice versa) fails the handshake
loudly (decode/`bad_psk`), which is the desired fail-closed behavior. No
migration shim.

## Testing

- **Unit**: `PskAuthRequest`/`PskAuthResponse` round-trip (client + runner roles,
  with/without binder). `pskGate.Check`: ok; bad binder → bad_psk+close; missing
  identity → no_identity+close; agent role + bad ticket → bad_ticket+close;
  operator role → ok+zero-principal; runner role → ok + registered.
- **Server integration**: after the merged handshake, `callerCaps(cid)` returns
  the agent's caps (not All) for an in-task ClientHello; a confined caller's
  `handleList` is subtree-scoped; `cancel`/`file` are denied without the cap.
- **Live E2E (the original goal)**: spawn a `--caps none` sandbox task; confirm
  `ls` shows only its subtree (not all tasks), and `submit`/`cancel`/`file pull`
  return permission-denied — i.e. the hole that the probe surfaced is closed.
- Existing PSK tests (`cli/psk_test.go`, server) updated to the schematized
  message; runner connect tests updated to the merged handshake.

## Rollout note

The server enforcement was already correct; this change makes the *client/runner*
side reliably present identity and schematizes the wire. Re-running the sandbox
probe requires the runner-host harness-cli (the bridged binary) rebuilt too — not
just the server.
