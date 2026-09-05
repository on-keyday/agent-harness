# File transfer the server does not decrypt — design

- Date: 2026-09-06
- Status: design, nothing implemented
- Scope: `server/file_transfer.go`, `runner/file_transfer.go`, `cli/file_*.go`,
  `runner/protocol/message.bgn`, and the runner's accept path
  (`runner/listen.go`, `runner/connect.go`)
- Companion: `2026-09-06-direct-client-runner-dial-probe.md` measured the
  route this design does NOT take, and is cited below as F1–F8.

## Problem

**P1. The server does the work twice.** Every byte of a file transfer is
decrypted on one connection and re-encrypted on the other.
`server/file_transfer.go:73` and `:128` hand both streams to
`spliceBidiHalfClose` (`server/task_handler.go:1552`), which reads 64 KB from
one trsf stream and appends it to another. Priced on the throughput ladder
(objtrsf `ab305b2`, 10 interleaved rounds on a quieted box): a middle endpoint
that splices runs at **36.4 MB/s**, one that forwards packets at **65.6**, and
no middle endpoint at all at **110.7**. Splicing costs 1.8x, and the ladder
attributes that to AES-GCM twice, two of the four trsf stacks, the
`ReadDirect`/`AppendData` copy, and the alternation between the legs.

**P2. The server holds the plaintext, with nothing asking it to.** The README
says so: the server "handles task logs, file contents, PTY streams and
port-forward bytes in plaintext". For task logs, PTY streams and port-forward
bytes that is load-bearing — the log store, the session ring buffer and
`forward tap` all read those bytes. For file transfer nothing does. The
server's whole role there is routing, and `handleOpenFileTransfer`'s own
comment says it: "this function is a routing primitive".

**P3. The two are the same fact.** The cost in P1 is paid to produce the
exposure in P2. A relay that does not decrypt removes both, and the mechanism
is already in this repo — `objproto.SetProxy` forwards packets for the
agent-proxy and via-relay paths and has never been put on a bulk data plane.

## What this covers, and what it deliberately leaves

`spliceBidiHalfClose` has four call sites and `spliceBidiCounted` two more.
The rule that decides each: **a data plane can move end-to-end exactly when no
server feature reads its bytes.**

| Call site | Reads the bytes? | Verdict |
| --- | --- | --- |
| `server/file_transfer.go:73` (push/pull) | nothing | **converted** |
| `server/file_transfer.go:128` (`file ls`) | nothing | **converted** |
| `server/git_query.go:66` | nothing | not converted in v1 — see below |
| `server/exec_run.go:114` | nothing | not converted in v1 — see below |
| `server/forward_splice.go:23` (port forward, `server/port_forward.go:76` and `:270`) | **yes** — `forward tap` streams them and the counters count them | not converted: `forward tap` would return an empty stream and `forward ls` would report 0 bytes on a busy forward |
| session PTY (`server/session_mux.go`, the ring at `:49`) | **yes** — the ring replays them on attach, `session snapshot` renders them | not converted: a reattach would replay nothing and `session snapshot` would return a blank screen |

Git query and exec pass the rule and are still left out of v1: each has its own
request/response shape and its own status vocabulary, and converting three
families at once would put three schema changes in one review. They are the
next two applications of the same mechanism, in that order, and nothing in this
design is specific to file transfer — the grant, the accept arm and the proxy
setup are shared. P1 and P2 are stated about file transfer because that is what
v1 converts; the rows above are the whole population the problem could apply
to, with a verdict for every one.

## Decisions taken

The third column records who decided. **operator** means the human chose it in
conversation; **this spec** means the author chose it while writing — those are
the rows worth a second look.

| # | Decision | Decided by |
| --- | --- | --- |
| D1 | The server keeps the packets on its socket and forwards them with `SetProxy`; the client does not dial the runner directly | operator |
| D2 | Scope for v1 is the file-transfer family only, by the rule above, with git-query and exec named as the next two | this spec |
| D3 | The runner authorizes the connection; it does not infer authorization from the fact that a packet arrived | this spec |
| D4 | The credential is a NEW short-lived per-operation grant, not the existing per-task `auth_ticket` | operator |
| D5 | The grant rides the existing PSK handshake as a new `ClientKind` arm, not a new first-message kind | this spec |
| D6 | Revocation is a runner-side message AND `DeleteProxy` at the server AND a TTL on the grant — all three | this spec |
| D7 | caps and scope are evaluated only on the server. The runner stores a grant naming a request kind, and never evaluates a scope expression or sees a `Capability` value | operator |
| D8 | `ws:` runners keep the splice path; both routes coexist and the server chooses per request | this spec |
| D9 | The client presents the grant as bytes, and the binder stays keyed by the PSK | operator |

## Why the runner has to authenticate (D3)

The tempting argument is that only the server can cause packets to reach the
runner at the proxied connection id, so arrival is itself authorization. That
argument is false on this fleet, and the probe shows why: a dial-mode runner's
UDP endpoint is `EndpointModeMutual` and its socket is bound regardless of mode
(F1), and probe 1 completed an ECDH into exactly that shape with no server
involved. What stopped it against the live Windows runners was a host firewall
(F3), not anything the harness enforces. A Linux runner on the same LAN with no
such filter would accept the dial.

So reachability is a property of the deployment, not an invariant of the
design, and an authorization that rests on it would be an invariant with no
enforcement behind it. The runner checks a credential.

## Why a new grant rather than the existing ticket (D4)

`auth_ticket :[16]u8` (`message.bgn:155`) already exists and already travels
server → runner → agent: the server mints it, `AssignTaskBody` carries it to
the runner, the runner injects it as `HARNESS_AUTH_TICKET`
(`runner/agentenv.go:74`), the agent presents it in `AgentInfo`, and the server
validates it against `agentboard/registry.go`'s store. The verifier is the
server; here it needs to be the runner. That mirroring is the cheap part.

Reusing the value is the part that does not work. That ticket is the identity
of **the agent of task T** — `Ticket()`'s comment records that re-registering
it would invalidate the credential a running agent is holding, and that `exec`
must look up the existing one rather than mint a new one. A client opening a
file transfer is not that principal, and handing it that ticket would let it
speak as the task's agent on the agentboard. The new grant names an operation,
not a principal.

It is also short-lived where that one is not, which is what makes D6's TTL
meaningful.

## Why revocation needs all three parts (D6)

`caps set` promises that narrowing a task's authority reaches work already in
flight — `server/set_caps_handler.go:109` says a narrowing "has to reach
in-flight work or it is advisory", and `:117` drops the principal's connections
when `narrowed && !KeepConns()`. Today that works because the server is the
data plane: closing the client's connection to the server ends the transfer.

Once the server forwards packets instead, dropping the client's control
connection does not touch the data plane, so three things are needed and none
of them is redundant:

- **`DeleteProxy` at the server.** Immediate, needs no wire change
  (`objproto/session.go:57`), and stops every transfer that reaches the runner
  through the server — which is all of them wherever a firewall or NAT sits
  between client and runner.
- **A revoke request to the runner.** `DeleteProxy` does not stop a client that
  can reach the runner directly, and by the argument in D3 the server cannot
  know whether this one can. The runner closes the connection it holds.
- **A TTL on the grant.** The revoke is a message and messages are lost. The
  TTL bounds the exposure to the grant's remaining life without any delivery
  guarantee.

An earlier reading in conversation had `DeleteProxy` alone as sufficient, on
the ground that the server is still in the packet path. It is in the path for
the connections it set up; the D3 argument is that it cannot assume those are
the only ones the runner will accept.

## Shape

```
1. client → server   OpenFileTransfer(task, direction, path, …)   [unchanged]
2. server            evaluates caps + scope                        [unchanged]
3. server            mints grant G = (task, kind, direction, expiry)
4. server → runner   AuthorizeDataPlane(slot, G)                    [new]
5. server → client   OpenFileTransferResponse{ …, runner_slot, G } [extended]
6. server            SetProxy(owned = client's data-plane CID,
                              allocate = runner's CID at the slot) [new]
7. client → runner   ECDH through the proxy; then PskAuthRequest
                     with ClientHello{kind = data_plane, G}        [new arm]
8. runner            matches G, binds the connection to the task,
                     opens the file stream in that worktree        [existing I/O]
```

Steps 1, 2 and 8's file I/O are untouched: the server still decides who may do
what, and `runner/file_transfer.go` still does the reading and writing. What
changes is that the bytes between them are carried on one connection the server
cannot read, instead of two it terminates.

The runner's side of step 7 is a third arm in `handleAcceptedConn`
(`runner/listen.go`), which today dispatches `DialGreeting` → server conn and
`AgentProxyControl` → agent proxy and closes everything else. Dial-mode runners
additionally need the accept loop itself, which today exists only in listen
mode (F6).

## Wire

The whole schema change is here, in one place.

```
# A grant is one request on one task, for a bounded time. It is NOT the task's
# auth_ticket: that names the task's agent, this names a request.
#
# kind is TaskControlKind — the enum that already names every client request,
# and the one PermissionDeniedResponse already pairs with a Capability. Nothing
# new is introduced to say "which request": a grant for git_query is
# kind = git_query and needs no arm, so that family costs zero schema.
#
# The variant tail is LAST so grant_id, task_id and expires_unix_ms sit at
# fixed offsets whatever the kind, and a future arm moves none of them.
format DataPlaneGrant:
    grant_id :[16]u8
    task_id :TaskID
    expires_unix_ms :u64
    kind :TaskControlKind
    if kind == TaskControlKind.open_file_transfer:
        direction :FileTransferDirection

# server → runner, on the existing registered conn. The runner stores the
# grant and expects one connection to present grant_id.
#
# slot_id precedes grant for the same reason: DataPlaneGrant now ends in a
# variant, so anything embedding it must place it last.
format AuthorizeDataPlaneRequest:
    slot_id :u16            # the connection id the forwarded packets will carry
    grant :DataPlaneGrant

enum AuthorizeDataPlaneStatus:
    :u8
    ok = "ok"
    unknown_task
    slot_collision          # slot_id equals the runner's server-conn id
    duplicate_grant

format AuthorizeDataPlaneResponse:
    status :AuthorizeDataPlaneStatus

# server → runner. Idempotent: revoking an unknown grant is ok.
format RevokeDataPlaneRequest:
    grant_id :[16]u8

format RevokeDataPlaneResponse:
    closed :u32             # connections the runner tore down

# client → runner, inside the existing PskAuthRequest. ClientKind gains
# data_plane; ClientHello gains the arm.
format DataPlaneInfo:
    grant_id :[16]u8
    task_id :TaskID
```

`ClientKind` gains `data_plane`; `ClientHello` gains
`if kind == ClientKind.data_plane: data_plane_info :DataPlaneInfo`.
`ClientHelloStatus` already carries `ok`, `bad_ticket` and `unknown_task`
(`message.bgn:382`); it gains `expired` and `not_permitted` so a refusal says
which of the three it was.

`OpenFileTransferResponse` and `ListFilesResponse` each gain `grant_id`,
`slot_id` and `runner_cid :RunnerID`. `RunnerRequestType` gains
`authorize_data_plane` and `revoke_data_plane`.

The grant names a request, not a permission, because of D7: the runner must
not hold anything it could interpret as policy. `kind` and `direction` are both
enums the schema already has and the runner already parses, so its check is an
equality against the request it just received — no mask arithmetic, no
`Capability` value, no scope. A read/write bit pair was the first draft and was
dropped: two bits meaning "may read files" and "may write files" are
`Capability.file_read` and `file_write` under another name, which is the
restatement D7 exists to prevent.

Reusing `TaskControlKind` means the field's type admits kinds no grant will
ever carry — `submit`, `set_caps`. That is the enum's existing usage rather
than a new wart: `PermissionDeniedResponse.requested_kind` is the same type and
only a subset of it can ever be denied.

## Server behaviour

`handleOpenFileTransfer` keeps its status codes and its existing checks — task
exists, task is `Running` or `Detached`, runner is registered — and gains, on
the path where it currently calls `CreateBidirectionalStream` twice:

1. Mint `grant_id` from `crypto/rand`, `kind` and `direction` copied from the
   request the caps check just passed, `expires_unix_ms` = now + the grant TTL.
2. Send `AuthorizeDataPlane` to the runner and wait for the response, with the
   same correlation pattern `sendEstablishRelayRequest` already uses. A non-`ok`
   status becomes `OpenFileTransferStatus_InternalError`, except
   `slot_collision`, which retries once with a fresh slot.
3. `SetProxy(owned, allocate)`. `owned` is keyed by the address the client's
   data-plane packets arrive FROM and `allocate` by the runner's address, both
   at `slot_id` — the shape `runner/relay_handler.go` builds from
   `serverCID.Addr` and the target's address, and the shape the throughput
   rung's `proxyPair` uses.
4. Answer the client with the grant, the slot and the runner's `RunnerID`.

The grant TTL is 5 minutes, refreshed by the runner for as long as the
connection carrying it is open. A transfer longer than the TTL is normal and
must not be cut; the TTL bounds how long a grant that was never used, or whose
revoke was lost, stays redeemable.

When `handleSetCaps` narrows a task, the same path that calls
`DropConnsForPrincipal` also revokes every grant issued for that task and calls
`DeleteProxy` on each proxied client id.

## Runner behaviour

A grant store keyed by `grant_id`, holding the task, the request the grant
names and the expiry — the mirror of `agentboard/registry.go`, with `Validate`
comparing in constant time the same way. `AuthorizeDataPlane` inserts,
`RevokeDataPlane` deletes and closes, expiry sweeps on a ticker.

On an accepted data-plane connection the runner refuses unless: the binder is
valid (the existing PSK gate, unchanged), the grant exists, it has not expired,
its `task_id` names a task this runner is running, and its `kind` and
`direction` equal the request that arrived. The refusals map onto
`ClientHelloStatus` so the client is told which check failed.

The file I/O afterwards is `runner/file_transfer.go` unchanged: it already
takes a task id and a stream and confines paths to the worktree root.

## Capability and scope

Nothing changes. `file_read` and `file_write` are evaluated on the server
exactly as they are today, against the scope the task holds, before a grant is
minted. The runner never sees a capability name or a scope expression — D7 —
so there is no second place where the caps model is written down and no way for
the two to disagree.

## Surfaces

No operator-visible option is added, so the surface matrix is short: `file ls`,
`file push`, `file pull`, `file delete`, `file edit` and `file mkdir` keep
their flags, their output and their status vocabulary on CLI, TUI keybindings,
TUI cmdline, WebUI buttons, WebUI cmdline and the WASM bridge. The route is
chosen by the server per request and is not selectable.

One display change: a transfer that fails because its grant was revoked mid-way
must say so rather than surfacing a bare connection error, on every surface
that renders a transfer error today.

The WebUI is on WebSocket and takes the splice path (D8, F7). That is not a
degraded mode to be fixed later — browsers have no raw UDP, and the splice path
remains the only one for them.

## Testing

- The runner's grant store: expiry, constant-time mismatch, wrong-direction
  refusal, idempotent revoke. Unit.
- `handleOpenFileTransfer` mints, authorizes and proxies in the right order,
  and answers `InternalError` when the runner refuses. Unit against the fakes
  in `server/fakes_test.go`.
- An end-to-end push and pull over the proxied path, asserting the bytes arrive
  and that the server's own endpoint never held a decryptable copy — the second
  half is what distinguishes this from the existing test.
- A narrowing `caps set` during an in-flight transfer closes it, and the client
  reports the revocation rather than a connection error.
- `scripts/wire-skew-check.sh`, because `.bgn` changes (Pitfall 10).
- One run of `file push` and `file pull` against `scripts/dummy-harness.sh` in
  the exact spelling the help text prints, because the client's argv parser and
  the new response fields meet only there (Pitfall 13).

## Non-goals

- **A direct client→runner path.** The probe doc lists seven items such a path
  would need. This design builds three of them, because a forwarded connection
  arrives at the runner's socket as an inbound connection like any other and
  something has to accept and authorize it: the runner-side authorization, the
  dial-mode accept loop with its third first-payload arm, and the coexistence
  of two routes. What is not built here is the punch request and the ordering
  window it needs. A later direct path changes how packets are routed and
  reuses the grant, the `ClientKind` arm, the runner's store and the accept
  path unchanged.

  Two of the probe's seven look avoidable in both routes, and writing this
  design is what made that visible: **the client needs no second socket.**
  `SetProxy`'s `owned` is keyed by the address the client's data-plane packets
  arrive FROM, and that is the socket the client already holds open to the
  server — an address the server therefore already observes, differing from its
  control connection only in the 16-bit id. A punch would name that same
  address, which is exactly what F5 demands, so neither the client's port
  discovery (item 4) nor an objtrsf accessor for a bound port (item 7) is on
  the path. The probe bound explicit ports on both ends for the convenience of
  the experiment, not because the mechanism requires them.

  What makes that legal is that the two ends of a connection do not have to
  name it identically. In the probe's first run the client held
  `udp:127.0.0.1:37037-16962` while the runner held
  `udp:127.0.0.1:37149-16962` for the same connection and the ECDH completed;
  only the 16-bit id is shared.
- **Converting port forwards or PTY sessions.** `forward tap` and the session
  ring buffer read those bytes; end-to-end encryption would delete features
  that exist on purpose.
- **Moving the Linux runners to the UDP underlay.** The server is already
  dualstack and each runner picks its underlay with `--server-cid`, so this is
  a per-runner operational change with no code in it.
- **Hiding file contents from the runner.** The runner reads and writes the
  files; it is the endpoint, not a relay.
