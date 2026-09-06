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
| D10 | The punch field AND its runner-side handler ship in v1, unset and unreached, so that a later direct path is a server-and-client change with no runner in it | operator |

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
# punch_target is where the runner should send probes so the client can reach
# it directly. transport_len == 0 means "do not punch, the server is
# forwarding" — the same encoding DialRunnerRequest.via uses for "not
# specified", and the only value v1's server ever writes. It is defined and
# handled now, unset and unreached, so that a direct path later is a change to
# the server and the client with no runner in it (D10). A zero RunnerID
# encodes: measured at 6 bytes, round-trips.
#
# slot_id precedes grant for the same reason: DataPlaneGrant now ends in a
# variant, so anything embedding it must place it last.
format AuthorizeDataPlaneRequest:
    slot_id :u16            # the connection id the forwarded packets will carry
    punch_target :RunnerID  # transport_len == 0 = do not punch
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

**The punch handler (D10).** When `punch_target.transport_len != 0` the runner
calls `objproto.Endpoint.SendProbe` toward that address every 500 ms until the
grant is redeemed or expires. v1's server never sets the field, so this never
runs in v1; it is here so that the direct path does not have to be deployed to
every runner host later. The mechanism is not a guess — probe 3 in the
companion doc ran exactly this loop against a live Windows runner host, 240
probes with none failing, and the peer's ECDH completed in 33 ms where the
un-punched control timed out. It gets a unit test so it cannot rot unnoticed
while unreached.

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
- The punch handler: a `punch_target` with `transport_len == 0` sends nothing,
  a set one sends probes at the interval and stops when the grant is redeemed
  or expires. Unit, against a fake endpoint. This is the only cover the handler
  gets until the direct path exists, which is why D10 requires it.
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
  would need. This design builds four of them: the runner-side authorization,
  the dial-mode accept loop with its third first-payload arm, the coexistence
  of two routes, and — by D10 — the punch field and its handler. The first
  three are not foresight but necessity: a forwarded connection arrives at the
  runner's socket as an inbound connection like any other, and something has to
  accept and authorize it.

  What is NOT built is the server ever setting `punch_target`, the client
  dialing the runner's address instead of the server's, and the ordering window
  between a punch and that dial. Those are the direct path; they live in the
  server and the client, which is the point of D10 — **a later direct path
  needs no runner change and so no coordinated restart of every runner host.**
  A `.bgn` change costs one of those (Pitfall 10), and `feedback_no_split_schemas`
  is the rule that says the whole schema belongs in one change rather than a
  follow-up.

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

## Amendment — how the request itself reaches the runner (2026-09-06, during Task 5)

The Shape section above says the runner "matches G, binds the connection to the
task, opens the file stream in that worktree" and never says how the runner
learns the path, the direction's operand, `force`, or `mkdir_parents`. The grant
carries the request KIND, deliberately, and nothing more.

**The client sends the request on the data-plane connection, right after the
handshake**, in the existing `RunnerRequest` envelope. The server therefore
stops sending `OpenFileTransfer` / `ListFiles` to the runner on that path
entirely; it mints, authorizes and proxies, and that is all.

Three reasons this is the right half to put it in, rather than extending
`AuthorizeDataPlaneRequest`:

- The client is the party that knows the path and the runner is the party that
  validates it (`ValidateRelPath`, plus the symlink check after it). Routing
  those bytes through the server would add a hop for a value the server does not
  inspect.
- It keeps the grant generic. Folding a file-transfer request into the authorize
  message would couple the grant to one family, which is what carrying only
  `TaskControlKind` was for.
- It generalizes: `git_query` and `exec` send their own request on their own
  connection, and the grant still says only which of them was authorized.

The runner checks the request against the grant before serving it — kind,
direction and task id all have to agree — so the client naming a different file
operation than the one the server authorized is refused, not served.

`handleOpenFileTransfer` and `handleListFiles` each gained an `...On(lookup)`
form for this: the stream carrying the bytes lives on the data-plane connection,
not on the one the request arrived over. Same split as the `X` / `XWith` pairs
in `cli`.

## Amendment — what the implementation changed (2026-09-06)

Four corrections, all found by building the thing.

**D8's rule is transport equality, not "`ws:` keeps the splice".** The route is
taken when the client and the runner reach the server over the SAME transport,
and the splice is taken otherwise. `ws↔ws` therefore goes end to end — the
dummy harness is all WebSocket and carries 1.2 MB byte-identical over it. What
falls back is a MIXED pair, because forwarding rewrites a packet's connection
id and re-emits it, and doing that from a WebSocket arrival out over UDP has
never been exercised. Applied to the current fleet: a `ws:` client with the
Linux runners goes end to end; with the Windows runners, which are `udp:`, it
splices.

**There is an escape hatch, and it is a request bit.** `--no-data-plane` on all
seven `file` verbs sets `no_data_plane` on the request, and the server splices
that one call. The route stays on by default and is not opt-in — it changes
which socket the bytes cross and nothing an operator can see, and a path that
is off by default is a path that rots. The bit is the other half: this is now
the only file-transfer path, so a fault in it takes push, pull and ls with it,
and one invocation has to be able to get them back with no restart and no
rebuild. It also makes the two routes comparable on one file.

It is threaded as a parameter through every `cli` file entry point rather than
carried on the `Client`, because it is a property of the request and not of the
connection: on the `Client` it would be state a caller can forget to set. The
TUI's and WebUI's own widgets pass the default; only their command lines, which
run the verb, carry the flag.

**The runner may not close as soon as the handler returns.** The file handlers
return when the last bytes are WRITTEN, not when they have left. Closing there
drops whatever the send path still holds — `file ls` hung forever while the
runner logged a clean, complete serve. It waits for the client to hang up now,
which is the only party that knows the transfer is over.

**One end of the data plane has to take the server half of the stream-id
space.** `peer.Conn` defaults to the client half and only `server/server.go`
builds a server-half end, so client-to-runner had two client halves and neither
could create a stream the other would accept. The client takes it: the runner's
side is an accepted connection whose kind is unknown until its first payload is
read, by which time its trsf exists.

None of these four could have been found by a unit test — they live between two
processes, which is what `scripts/dummy-harness.sh` is for.

## Amendment — why the transports must match (2026-09-06)

The amendment above says a mixed transport pair falls back "because forwarding
rewrites a packet's connection id and re-emits it, and doing that from a
WebSocket arrival out over UDP has never been exercised". That was caution, and
it understated the case: there is a hard reason, and stating it weakly invites
someone to lift the restriction by simply trying it.

Each end sizes its packets from its OWN connection's transport —
`peer.MTUForTransport`, called with `conn.ConnectionID().Transport` — and
`ws`/`wss` get `StreamMTU` (16384) where `udp` gets the path-MTU-safe trsf
defaults. `MTUForTransport`'s own comment names the assumption underneath:
*"Both ends of a connection derive it from the same scheme, so the choice is
always symmetric."* A mixed pair is precisely where that stops holding.

Forwarding re-emits a packet byte for byte and never re-fragments. So the end
that is on WebSocket produces 16 KB packets, and on the UDP leg those are past
the datagram MTU: dropped, or `EMSGSIZE` / `WSAEMSGSIZE` on send. It fails in
**both** directions, since either end can be the WebSocket one.

This is fixable rather than fundamental: the two ends would have to agree on
the smaller MTU, and `AuthorizeDataPlaneRequest` is the natural place to carry
it. Until they do, a mixed pair takes the splice — which is what today's fleet
does for a `ws:` client against the `udp:` Windows runners.

## Amendment — the transports no longer have to match (2026-09-06)

**This reverses the amendment immediately above it.** That one is right about
the mechanism and wrong about the conclusion: the MTU asymmetry is real, and it
is negotiable, so it bounds nothing.

The server is the only party that sees both transports. It now computes the
smaller of the two packet sizes at setup and sends it to both ends — to the
runner on `AuthorizeDataPlaneRequest.mtu`, to the client on the response's
`mtu` — and each end builds its connection with it (`peer.DialConfig.MTU`,
applied as the maximum too so PLPMTUD cannot probe back above what the other
end can carry). Neither end restates the rule; a value of 0 means "keep your
own default", which is what a same-transport pair gets.

`dataPlaneRoute` therefore asks only that both ends have a transport at all.

Measured on one `scripts/dummy-harness.sh --udp` instance, which is what that
flag was added for:

| pair | evidence |
| --- | --- |
| `ws` client × `udp` runner | `set proxy setting owned=ws:… allocate=udp:…`; 1.5 MB pushed and pulled back byte-identical |
| `udp` client × `udp` runner | `set proxy setting owned=udp:… allocate=udp:…`; push and `ls` both exit 0 |

This matters more than it looks. `harness-cli conns` on the live fleet shows
the operator's TUI on `udp:` and twelve of the fifteen runners on `ws:`, so
under the equality rule the TUI took the splice for almost every runner it
touches. The WebUI, being a browser, is `ws:` and had the mirror problem
against the Windows runners. Both halves are now the fast path.

## Amendment — could is not should (2026-09-06)

Everything above decides whether a data plane **can** move end to end: the rule
in "What this covers" is that no server feature reads the bytes, and
`dataPlaneRoute` asks only that both ends have a transport. Nothing anywhere
asks whether it **should**. That is a hole in the design, not a slip in the
implementation, and it was reported from the TUI: `file ls` came out slower
than the splice it replaced.

The arithmetic is in P1's own numbers. The route buys 65.6 MB/s against 36.4,
about 1.8x. It pays a fixed setup the splice does not: a server-to-runner
authorize round trip that blocks the client's request, a fresh P521 ECDH and a
PSK hello between client and runner, then a teardown. A listing is a few
hundred bytes; 1.8x of that is nothing and the setup is everything. The route
loses on every request carrying almost no payload — which is most of the file
family by call count, because `ls` is what a file browser does between all the
other operations.

So the server asks the second question too, in `dataPlaneWorthIt`, and asks it
**before** minting a grant or sending the authorize. A spliced request pays
nothing at all, not a cheaper setup: it takes exactly the path it took before
this design existed.

| request | route | why |
| --- | --- | --- |
| `list_files` | splice | a few hundred bytes |
| `delete`, `dir_delete`, `mkdir` | splice | an ack and no body |
| `push` | ≥ 1 MiB | the one direction whose size is known before the transfer |
| `pull`, `dir_pull`, `dir_push` | always | size unknown at decision time — see below |

`OpenFileTransferRequest.expected_size` already existed and every push path
fills it from a real `Stat`: the file-backed `FilePush` and the WebUI's
`FilePushBytes` both funnel through `filePushFromReader`. No schema change.

**The threshold is reasoned, not measured.** 1 MiB is where 1.8x is about 12 ms
at those two rates, comfortably more than a setup even with a WAN round trip in
it. The honest way to tune it is `scripts/netem-lab` across a size ladder at two
RTTs, finding where the curves cross. Until that is run the constant is an
estimate, and its comment says so rather than implying a measurement.

**Pull is not gated, and that is a known remaining inversion.** Nobody knows a
pull's size at decision time: the client has not seen the file and the server
never stats it — only the runner does, and that happens after the authorize the
gate exists to avoid. Asking the runner first would cost exactly the round trip
being saved. So `file pull` of a small file still pays the setup. Closing it
means a client-supplied size hint — the TUI does know the size, from the
listing it just rendered — which is a schema change, deliberately not taken
here.

A kind the switch has not considered returns false. D2 names `git_query` and
`exec` as the next two applications; each must answer this question explicitly
rather than inherit a yes from a default arm, which is the mistake this
amendment corrects.

## Amendment — the route is opt-in, because forwarding is slower than splicing (2026-09-06)

**This reverses D8's default and deletes the size gate the amendment above
added.** P1 priced the splice against a relay whose CPU was the bottleneck.
That is not this deployment, and it is not most deployments, and where it is
not true the whole argument inverts.

Measured on `scripts/netem-lab`, 4 MB pushes, interleaved, n=7 (n=5 for the
last two rows), loss held at 0 except where stated:

| one-way delay | end-to-end RTT | data plane | splice | ratio |
| --- | --- | --- | --- | --- |
| 1 ms | 4 ms | 652 ms | 528 ms | 1.23x |
| 5 ms | 20 ms | 1764 ms | 682 ms | 2.59x |
| 25 ms | 100 ms | 5668 ms | 1960 ms | 2.89x |
| 50 ms | 200 ms | 10811 ms | 3532 ms | 3.06x |
| 25 ms + **1% loss** | 100 ms | 23305 ms (one run unfinished at 45 s) | 2895 ms | **8.05x** |

**The cause is split-connection gain, and it is structural.** The splice
terminates both legs, so it runs two congestion loops of half the path each; a
loss is recovered in one leg's RTT and one leg's window backs off. Forwarding
packets leaves ONE loop spanning client → server → runner: double the RTT,
double the recovery time, and a single window that any loss on either leg
collapses. Window-limited throughput is `W/RTT`, so doubling the RTT halves it
— which is the 2.6–3.1x, and the ratio does not shrink with transfer size, so
no threshold repays it. `dataPlaneWorthIt` is therefore removed rather than
raised: with the route off by default, the default is what answers "should",
and a size heuristic that second-guesses an explicit request only makes
`--data-plane` untestable on small operations.

Two hypotheses were killed on the way, and both had been asserted before they
were tested:

- **MTU negotiation.** A 2x2 of client × runner transport on the live fleet put
  the negotiated pairs at both the smallest penalty (+813 ms) and the largest
  (+3768 ms). Negotiation does not sort the cells.
- **Loss recovery alone.** The lab shows 2.89x at 25 ms with **zero** loss, so
  recovery cannot be what produces the gap. Loss multiplies an effect that is
  already there; it does not create it.

A third reading was wrong in the other direction: on the live fleet, raising
parallelism did not raise aggregate throughput, and that was taken as ruling
window-limitation out. It ruled out nothing — that path was bandwidth-limited
at ~5 MB/s, where no flow count helps. The lab has no rate limit, and there
window-limitation is exactly what shows up.

**So the bit is now `data_plane`, opt-in, and spelled that way round on
purpose.** The default has to be a property of the wire: a caller that says
nothing splices. Under the old `no_data_plane` spelling the default lived in
every call site remembering to pass a flag, and one forgotten widget routed
silently — which is how the route became the default the first time.
`RunnerOpenFileTransferRequest` loses its copy of the bit entirely: by the time
a request reaches the runner the route is already chosen, the message arrives
either on the spliced stream or on the data-plane connection, and no runner
ever read it.

What the route still buys is P2, unchanged: the server holds no plaintext. That
is a property rather than a speed, so it is worth asking for and not worth
defaulting to.

**None of this is an argument against the direct path.** Every figure here is
about a *relay*, and the defect is that the relay doubles the control loop. A
direct client↔runner connection is ONE hop — its loop is the path's own RTT,
not twice it, and it has no second leg to inherit a stall from. D10 was
written so that path costs no runner change, and this measurement is the
reason to finish it rather than to abandon the idea: the original question was
whether file transfer could go peer to peer, and what was measured slow is the
substitute, not the answer.

## Amendment — three routes, named by the caller (2026-09-06)

**This replaces the `data_plane` bit, and with it D8 and the server-side
choice.** There are three paths, they differ in where the plaintext is, and
the request now names one:

| route | the server | plaintext | measured |
| --- | --- | --- | --- |
| `splice` (default) | terminates both legs, copies between them | **reads it** | fastest everywhere |
| `forwarded` | forwards packets (`SetProxy`) | cannot read it | 2.6x slower at 20ms RTT, 8x with 1% loss |
| `direct` | not in the path at all | cannot read it | one hop; unmeasured across hosts |

Two things were wrong with the bit it replaces, and they are the same mistake
seen from two sides.

**A bit cannot name three things.** Which of `forwarded` and `direct` a caller
got was decided by a server flag it could not see or choose. Three modes, one
bit, and the discriminator in the wrong process.

**"Not wanted" and "not possible" were one `false`.** `tryDataPlane` returned
the same answer for "the caller asked for the splice" and for "the hook is
absent / the transports differ / the runner refused / setup timed out", so
every one of those came out as a splice nobody asked for. That is not a
fallback, it is a silent substitution — and for these two routes it substitutes
the one path that hands the server exactly what the caller withheld. So a route
that cannot be taken is now answered `route_unavailable` and nothing is
attempted in its place. Retrying on another route is the caller's decision, and
it cannot make one it is not told about.

**There is no automatic fallback, deliberately.** A failed direct dial does not
quietly become forwarded or spliced. Falling back to the splice would break the
promise the caller made the request for; falling back to `forwarded` would keep
the promise but silently take the slowest path; and either one hides which
route ran, which makes the three impossible to compare. The failure is
reported and the operator decides.

The word is validated at bind time, before anything dials — the first version
parsed it inside the action, so `--route bogus` opened a connection and
complained afterwards. `ParseFileTransferRoute` lives in `cli/verb` because
`cli` imports `verb` and not the reverse, and because the alternative is the
list of spellings written down twice, which is how a CLI and a TUI drift into
accepting different words for the same path.

Verified on one all-udp `scripts/dummy-harness.sh --udp` instance, read off
which address each connection goes to (server `:42229`, runner `:33861`):

| invocation | connections | route taken |
| --- | --- | --- |
| default | server | splice |
| `--route splice` | server | splice |
| `--route forwarded` | server, server | forwarded |
| `--route direct` | server, **runner** | direct |
| `--route direct` from a **ws** client | server only, `route_unavailable` | refused, NOT spliced |
| `--route bogus` | none — refused before dialing | — |

Verified on one `scripts/dummy-harness.sh` instance by counting the connections
each invocation opens — the data plane is a second connection, so the count is
the route:

| invocation | connections | route |
| --- | --- | --- |
| `file ls` | 1 | splice |
| `file push` 1 KiB | 1 | splice |
| `file push` 2 MiB | 2 | data plane |
| `file pull` | 2 | data plane |
| `file mkdir`, `file delete` | 1 | splice |
| `file push` 2 MiB `--no-data-plane` | 1 | escape hatch intact |

md5 identical across the spliced and the routed pull of the same file.

## Amendment — the direct path measured, and it loses too (2026-09-06)

The previous amendment ended by saying the relay's defect was that it doubles
the control loop, and that a direct client↔runner connection is one hop and so
does not have it. The direct path now exists and has been measured on the live
fleet. **It loses to the splice as well**, and the reason retires the hop-count
argument entirely.

udp client (gmkhost) → udp runner on a Windows host across the LAN, interleaved,
no failures in any run:

| size | n | splice | forwarded | direct | verdict |
| --- | --- | --- | --- | --- | --- |
| 4 MiB | 13 | 1271 ms | 1714 ms (+35%) | 922 ms (−28%) | forwarded REAL; direct inside the ~30% noise |
| 4 MiB | 27 | 1580 ms | — | 1795 ms (+14%) | inside the ~20% noise |
| 32 MiB | 9 | 10085 ms | — | **16143 ms (+60%)** | **REAL** (resolution ~36%) |

The 32 MiB row is the one that decides it. If the direct path were merely
paying a variable setup, the gap would SHRINK as the transfer grows. It widens.

The distributions say the rest. At 32 MiB the splice runs 8.1–14.1 s with 19%
stdev; direct runs 6.2–42.9 s with 53%. Its best case is the fastest thing
measured all day, and its worst is four times the splice's worst.

**What actually decides these three is not hop count but whether the bad
segment is isolated behind its own congestion controller.** The splice
terminates each leg, so the lossy hop — a Windows laptop over Wi-Fi — is
absorbed by a controller that spans only it, and the client's leg never sees
that loss. Both other routes put ONE controller across the bad segment:
`forwarded` spans client→server→runner, `direct` spans client→runner. Fewer
hops does not help when the single loop still contains the hop that misbehaves.

That is the same mechanism as the earlier `forwarded` result, and it now covers
every measurement in this document. It also predicts where the direct path
WOULD win: a deployment whose client↔runner path is better than its
client↔server↔runner path — a distant server with two well-behaved local ends —
which is not this fleet.

So all three routes stay, splice stays the default, and the two others remain
what they became one amendment ago: ways to ask for the server not to read the
bytes, at a measured cost. Nothing here argues for removing the direct path —
it works, it traverses the Windows host firewall the punch was built for, and
it is the only route whose best case beat everything else. It argues against
claiming it is faster.

One defect was found by the measurement and fixed. `dialDataPlane` bounded only
the runner's answer to the hello; `peer.Dial` ran on the caller's context, which
for a CLI push has no deadline. A direct dial the punch had not opened parked
forever — observed at six minutes with no CPU, on a pair that had completed in
520 ms minutes earlier. Dial and hello now share one deadline. With no fallback
by design, a prompt failure is the whole of what the caller gets back.

## Amendment — the control that was missing, and what actually decides (2026-09-06)

The amendment above concluded from the live fleet that hop count is the wrong
model. It was measured without a control, and the control reverses half of it.

Same three routes, 32 MiB, n=9, one host, no radio hop
(`scripts/dummy-harness.sh --udp`), resolution ~11%:

| route | median | MB/s | vs splice |
| --- | --- | --- | --- |
| splice | 909 ms | 36.9 | — |
| forwarded | 813 ms | 41.3 | −10.6% (inside the noise) |
| **direct** | **523 ms** | **64.1** | **−42.5% (REAL)** |

Those three figures land almost exactly on the throughput ladder P1 was argued
from — 36.4 splice, 65.6 forwarded-with-no-middle-endpoint. **The code does what
the design said it would.** On a clean, latency-free path the direct route is
the fastest thing here by a wide margin.

So neither model is right on its own; each owns a regime:

| path | winner | what decides |
| --- | --- | --- |
| clean, ~0 RTT, CPU-bound | **direct**, by 42% | how many times the bytes are crypto'd and copied |
| lossy / high RTT | **splice** | whether the bad segment sits behind its own congestion controller |

The live fleet is the second regime, and the reason is not the runner's
operating system. The Windows runner served a splice at the same speed as a
Linux one (16 MiB: 4066 ms against 4164 ms). What differs is the path: the
client here is on `wlan1`, its wired interface is down, and the Windows host is
a laptop. A direct connection between two wireless stations crosses the radio
TWICE — station → AP → station — under one congestion controller, while the
splice puts one radio hop under each of two. Fewer IP hops, more airtime, one
loop spanning all of it.

That is a property of this deployment, not of the route. `direct` is the right
answer wherever the client↔runner path is genuinely better than
client↔server↔runner: two wired ends, or a distant server. `splice` stays the
default because this fleet is not that.

**Correction, once the server's link was checked: it is wireless too, and that
makes the airtime argument point the other way.** In infrastructure mode every
station-to-station frame goes to the AP and is relayed, so a byte crosses the
air twice per leg:

| route | air crossings | app throughput at 32 MiB | airtime consumed |
| --- | --- | --- | --- |
| direct | 2 (client → AP → runner) | 2.08 MB/s | ~33 Mbit/s |
| splice | **4** (client → AP → server, server → AP → runner) | **3.33 MB/s** | ~107 Mbit/s |

The splice spends about three times the airtime and still delivers more. If the
channel were the limit that could not happen, so **the channel has headroom and
the direct route is failing to use it.** What bounds `direct` here is therefore
not the medium's capacity but the congestion controller's response to the loss
and jitter of a path it spans end to end — and that is a property of trsf under
these conditions, not an immutable fact about the deployment.

Stated as the falsifiable claim it is: the airtime accounting assumes both hops
of each leg run at comparable PHY rates and that the AP relays at line rate.
What it does not assume is anything about which link is worst — both routes
cross the runner's own hop, so a bad link there cannot explain the gap on its
own. The thing that differs is still which controller owns that hop: its own,
or one that also owns everything else.

**A regression of this document's own making, found by the control.** The
previous amendment bounded the data-plane dial with `context.WithTimeout` on the
caller's context. `peer.Dial` hands that context to `WrapAcceptedConn`, which
derives the STREAM lifetime from it, and `Start` runs `AutoReceive` on it — so
the deadline was not on the handshake, it was on the connection. Every
`forwarded` and `direct` transfer died at "stream write: context canceled". The
dial is now raced against a timer on a cancellable child whose cancel is handed
to the connection's closer, so the bound applies to waiting and never to the
transfer.

It reached main because the live measurements ran against the main checkout's
binary, built before that commit, while the fix was verified only by `go build`
— which does not refresh `bin/`. The rule already written down for runners
holds for the CLI too: rebuild `bin/` before believing a client-side check.

## Amendment — the radios measured, and they are not the limit (2026-09-06)

Every amendment above reasons about "a Wi-Fi path" without having measured one.
All three stations have now been read, and they retire the explanation this
document kept reaching for.

| station | signal | PHY rate | note |
| --- | --- | --- | --- |
| client (Linux, gmkhost) | −58 dBm | tx 720.6 / rx 612.5 Mbit/s | Wi-Fi 6, 80 MHz, 2 streams; tx failed 12 |
| **server (Raspberry Pi)** | −60 dBm | **433.3 Mbit/s both ways** | one spatial stream (80 MHz / MCS9 exactly); tx failed 18413 |
| runner (Windows laptop) | −61 dBm | 907 / 961 Mbit/s | |

All three sit between −58 and −61 dBm, and the WEAKEST link is 433 Mbit/s ≈ 54
MB/s. The transfers measured in this document ran at 3–5 MB/s, an order of
magnitude below that. The Pi's 18413 failed transmissions look alarming and are
not: against 147 million packets over 125 days of uptime they are 0.0125%.

**"The Pi is the bottleneck" does not survive either.** On a splice the Pi
carries every byte twice on one stream, so call its ceiling ~27 MB/s — still
five times what was measured. And `direct` does not involve the Pi at all: two
strong stations, 720 and 961 Mbit/s, two air crossings, no middle endpoint. If
the Pi were the limit, direct would have been the fastest route. It was the
slowest.

**What is left is the congestion control, and the runner-side counters now show
it directly.** During a 32 MB pull, sampled from the sending runner:

- `bytes_in_flight` tracks `cwnd` throughout — window-limited, not
  bandwidth-limited and not CPU-limited
- `cwnd` reaches roughly 1 MB; at the observed ~50 ms srtt that is ~20 MB/s
  worth of window, the same order as the throughput actually seen
- loss accumulates steadily and `loss_spurious` stays 0, so the window is being
  cut by real losses rather than by mistimed retransmits

Wi-Fi's own retries make the delay jitter, the controller cannot keep the window
open, and on top of that sits the difference this document measured three ways:
ONE loop over the whole path (`forwarded`, `direct`) against two half-length
loops (`splice`).

So physical placement matters — per-station rates, shared airtime, and
station-to-station traffic crossing the air twice through the AP — but on THIS
fleet it does not set the limit. Wiring the server would remove two of the four
air crossings a splice makes and take the one-stream radio out of the path, and
it is worth doing; it is not what decides the ordering of the three routes.

The open question this leaves is the sharp one: a 433 Mbit/s medium is carrying
30 Mbit/s of application traffic. That gap belongs to the transport, and
`harness-cli conns --trsf --watch` is the first tool this project has had for
looking at it from either end.

## Amendment — the gap is not the congestion control, and it reproduces at 2 ms with no loss (2026-09-06)

The amendment above hands that open question to the congestion controller. It
was reasoned from the fleet, where the Wi-Fi, the Pi, the loss and the
controller are all present at once and none of them can be turned off. Put the
same transfer on `scripts/netem-lab` — one host, veth pairs, no radio, no rate
limit, no configured loss — and **the same 3–5 MB/s appears.** Nothing in the
fleet's physical layer is needed to produce it, so nothing in the fleet's
physical layer explains it.

### The ladder that decides it

`file push` of one 32 MB file over the default `splice` route,
`netem-lab bench --runs 5`, one knob changed between rows:

| one-way delay | end-to-end RTT | median | stdev | spread |
| --- | --- | --- | --- | --- |
| 0 | ~0.1 ms | **43.54 MB/s** | 7% | 1.18x |
| 0.25 ms | 0.5 ms | 21.47 MB/s | 28% | 2.18x |
| 1 ms | 2 ms | 9.60 MB/s | 70% | 4.44x |
| 25 ms | 50 ms | 7.49 MB/s | 16% | 1.51x |

**Above about 2 ms the round trip stops mattering.** 2 ms → 50 ms is a 25x
increase in RTT for a 22% loss of throughput, inside the 2 ms row's own
resolution. Window-limited throughput is `W/RTT`; a window-limited transfer
would have lost a factor of 25. This one lost nothing measurable.

**Below it, a quarter of a millisecond each way costs half the throughput.**
That is the opposite sensitivity: the transport is hurt by the *presence* of
latency, not by its size. At zero delay it reaches 43.54 MB/s, which is the
in-process relay rung on this same host — so with the path's latency removed,
the deployed harness performs exactly as the ladder in P1 says it should.

### Four controls, and what each one removes

All on the same lab, same host (Intel N100), during or beside the same
transfers:

| control | measured | what it removes |
| --- | --- | --- |
| `iperf3` TCP through the lab | **1579 MB/s**, 0 retransmits | the path is not the limit |
| `iperf3` UDP, 1200 B datagrams | **47.7 MB/s = 41,672 pkt/s**, 0.015% loss | the path carries our packet rate, losslessly |
| objtrsf in-process, `udp` / `relay` rungs | **119–141** / **34–57 MB/s** | the transport code is not the limit |
| server CPU, 25 s profile during a push | **21.3% of one core**; AES-GCM 1.5% | nothing is CPU-bound, and crypto is noise |
| wire bytes for a 32 MB push (tc delta) | 36.9 MB toward the server, 36.3 MB toward the runner, **dropped 0** | 1.10x — there is no retransmission amplification |
| sender's own counters during a 256 MB pull | cwnd **1.4–8.6 MB**, bytes in flight **1463 = one packet**, srtt 2.2–2.4 ms | the window is wide open and unused |

The last row is the direct contradiction. `conns --trsf --runner … --watch 1s`,
read from the sending runner, showed a congestion window of megabytes with one
packet outstanding in nearly every sample. The sender never reaches its window,
so the window cannot be what is holding it back.

### What is left: the loop is waiting, not working

`trsf/conn.go`'s run loop emits at most one packet per pass — it pops one send
stream, builds one `SendAction`, and goes back to the top. So the loop's
iteration rate *is* the packet rate, and that is what the `LOOP+` column
measures. Two runs of the identical 32 MB push in the identical lab:

| run | throughput | packet rate |
| --- | --- | --- |
| slow | 2.44 MB/s | ~1,850 pkt/s (`LOOP+` steady state) |
| fast | 11.42 MB/s | 8,623 pkt/s (25,352 packets in 2.94 s, from tc) |
| the path itself | 47.7 MB/s | 41,672 pkt/s at 0.015% loss |

1,850 packets per second is 540 µs per packet on a machine that is 79% idle.
The loop is not computing for 540 µs; it is asleep. Finding *what* it sleeps on
— the pacer's `max(1*time.Millisecond, …)` floor, the loss-detection timer, or
cross-process wakeup latency — is the next step, and it needs a wake-reason
counter inside `trsf`, which no counter in `InternalState` currently provides.

The same fact explains the variance the netem-lab README records as unexplained
("where it comes from is not yet known"): the 4.4x spread at 2 ms is the same
loop running at 1,850 pkt/s on some runs and 8,600 on others.

### Corrections to the amendment above

- "`bytes_in_flight` tracks `cwnd` throughout — window-limited" does not
  reproduce. Whatever the fleet showed, the inference does not survive the RTT
  ladder: a window-limited transfer scales with `1/RTT` and this one does not.
- "`cwnd` reaches roughly 1 MB; at ~50 ms srtt that is ~20 MB/s worth of
  window, the same order as the throughput actually seen" — 20 MB/s is not the
  same order as the 3.33 MB/s in the table two amendments up. The arithmetic
  was already saying the window was not the constraint.
- "`loss_spurious` stays 0, so the window is being cut by real losses" is
  weaker than it reads. `PacketNumTracker.GenerateACK` takes and CLEARS its
  ranges, so a packet number is reported in exactly one ACK. A spurious loss is
  counted only when the vindicating ACK arrives; if that ACK is itself lost the
  loss is spurious in fact and invisible in the counter. (Code reading, not a
  measurement — the lab's losses were not investigated.)

### What this does not say

The lab is one host with an N100 and netem on veth; the fleet is three hosts,
one of them a Pi, over Wi-Fi. The absolute MB/s are not comparable and are not
being compared. What transfers is the shape: the same order of throughput
appears with the radios, the loss and the Pi all removed, and it varies with
RTT in a way a congestion window cannot.

The route ordering established by the amendments above is untouched. Splice
stays the default; nothing here is an argument about `forwarded` versus
`direct`. It is an argument that all three are being measured against a ceiling
none of them set.

### A tooling defect found on the way

The netem-lab README says `shape` "**replaces** the qdisc, which resets every
counter". It does not, at least when the new shaping has the same qdisc kind:
`tc qdisc replace` on a matching handle updates in place and keeps the
statistics. Reading `show` after a `shape` as if it were a fresh count turned
one 32 MB push into an apparent 11x wire amplification that is not there — the
real figure, taken as a delta across the push, is 1.10x.

## ~~Amendment — the loop is starved, not throttled~~ (2026-09-06) — RETRACTED

**The heading is wrong and so is the conclusion below it.** `wake_send` names
the CHANNEL that ended a park, not what pushed it, and the section reads the
channel as an answer. The amendment after this one carries the measurement that
kills it: on both a lossy fleet path and a clean lab one, the pushes that
actually fired that channel were the loop's own continuation and inbound ACKs,
with the application at 1–3%.

What survives is the timer half — thousands of parks, single-digit timer wakes —
and it survives on both paths. Everything below about "starved" does not. The
section is kept rather than rewritten so the correction has something to point
at.

The amendment above ends by naming what it could not answer: the loop sleeps
540 µs per packet on an idle host, and no counter said on what. It now does.
objtrsf `6389fc9` + `a69393f` add five counters to the run loop and
`conns --trsf` reports them, as `BLOCK%` (the share of the interval the loop
spent parked) and `WAIT` (what ended those parks), with the raw five in
`--json`.

Sampled from the SENDING runner during a 256 MB pull over the 2 ms lab path,
which ran at 3.84 MB/s:

| BLOCK% | parks | timer | send | peer | armed_pacer | mean park |
| --- | --- | --- | --- | --- | --- | --- |
| 95% | 9,078 | **4** | 5,014 | 4,060 | 0 | 209 µs |
| 97% | 4,769 | **11** | 2,406 | 2,352 | 0 | 407 µs |
| 93% | 14,678 | **2** | 8,664 | 6,012 | 1,776 | 127 µs |
| 94% | 18,439 | **2** | 11,618 | 6,819 | 0 | 102 µs |
| 91% | 21,571 | **1** | 13,317 | 8,253 | 0 | 85 µs |
| 99% | 972 | **0** | 278 | 694 | 0 | 2047 µs |

**The loop is parked 91–100% of the time, and essentially never on a timer** —
single digits out of thousands of parks. That rules out, directly rather than by
argument, the two candidates the previous amendment named: the pacer's
`max(1*time.Millisecond, …)` floor and the loss-detection timer. `armed_pacer`
is 0 in most intervals, so the pacer rarely even supplies the deadline.

What the loop waits for is the **send trigger** (55–60% of parks) and the
**peer** (40–45%). Neither is the transport throttling itself:

- `send` means the run loop had nothing queued and was waiting for the
  application to hand it more. On a pull the runner's send stream is fed by a
  file read into a 1 MB buffer, and it drains no faster than the peer's window
  and the splice's own copy allow.
- `peer` means it was waiting for inbound — an ACK, or the next packet.

**So the transport is not slow; it is starved.** Every layer measured so far
has been exonerated in turn — the medium, the Pi, the path, the CPU, the
congestion window, and now the transport's own timers. What has never been
measured is the thing between them: the file-transfer read/write path and
`spliceBidiHalfClose`'s 64 KB copy between two trsf streams, whose alternation
P1 already named as one of the four costs of splicing. That is where the next
measurement goes.

Two defects in the instrument itself, both found by pointing it at a live
transfer and neither reachable by a unit test:

- **`BLOCK%` printed 135%.** The interval was timed at the client while the
  counters advanced on the answerer, and the two differ by the change in
  round-trip time between readings. `TrsfStateResultBody` and
  `RunnerTrsfStateResponse` now carry `sampled_unix_ns`, stamped by whoever
  walked its connections; the runner's value passes through the server rather
  than being restamped a round trip away.
- **A loop parked for a whole interval printed 0%**, i.e. "busy" — the opposite
  of the truth, and the first thing it printed on a finished transfer.
  `blocked_ns` accrued only when a park ENDED; the reader now adds the park in
  progress.

The instrumentation costs one clock read per park, not per packet. Interleaved
A/B against the throughput ladder, 6 alternations: `udp` +4.7% (resolution
±17%), `mock` control −3.8% (±10%) — neither outside the noise, and the control
did not move.

## Amendment — the fleet, and what the counter above could not say (2026-09-06)

The server and the runners were restarted, so the counters could finally be read
where the problem was reported. Windows runner sending over Wi-Fi, three 32 MiB
pulls, 4.16 MB/s, sampled from the runner:

| BLOCK% | parks | timer | send | peer | armed_pacer | cwnd | in flight | srtt |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 97% | 5,444 | **2** | 3,499 | 1,943 | 3,795 | 588,528 | **589,589** | 41 ms |
| 99% | 4,063 | **6** | 2,533 | 1,524 | 2,736 | 437,947 | **438,900** | 66 ms |
| 92% | 5,584 | **12** | 3,473 | 2,099 | 3,436 | 446,436 | **447,678** | 80 ms |

**The timer result holds.** Single-digit timer wakes out of thousands of parks,
on the real path as in the lab: the pacer's 1 ms floor and loss detection are
both ruled out. `armed_pacer` is now 60–80% of parks rather than ~0, which
sharpens it — the pacer supplies the deadline almost every time and almost never
gets to fire, because a notification arrives first.

**Two things the previous amendment got wrong, and one it could not have known.**

**1. The fleet IS window-limited.** `bytes_in_flight` sits on `cwnd` in nearly
every sample (588,528/589,589; 437,947/438,900; 446,436/447,678), and cwnd/srtt
— 450 KB over 40–140 ms — lands on the 4.16 MB/s measured. The amendment
"the radios measured" was right about this and the retracted section was wrong to
call it unreproducible: it does not reproduce in the LAB, which is lossless and
where the window stays open. Two paths, two regimes, and the earlier reading
generalised one of them.

**2. `wake_send` cannot mean "the application is not feeding it".** `sendTrigger`
has ten push sites meaning at least five different things, so the channel is
many-to-one; the retracted section read it as one of the five. objtrsf `5368094`
counts the reasons where each push is MADE, and `WAIT` now says which:

| netem-lab, 2 ms, 128 MB pull, per 2 s | app | ack | self | cwnd | loss | other |
| --- | --- | --- | --- | --- | --- | --- |
| 8,655 parks | **196** | 571 | **4,505** | 2 | 4,491 | 10 |
| 21,262 parks | **542** | 10,861 | **12,407** | 10 | 154 | 2 |
| 25,783 parks | **673** | 14,511 | **15,432** | 9 | 3,806 | 14 |

`app` is 1–3%. `self` — `triggerPacket` re-pushing because data was STILL
buffered after the packet it had just built — dominates. Both say the same
thing from opposite directions: **the send buffer was rarely empty, so the
application was ahead of the transport, not behind it.** "Starved" is dead on
the clean path too, not only on the fleet.

**3. `armed_pacer` was right on both paths and `wake_send` was right on
neither, and the difference is where each is counted.** `armed_pacer` is
incremented inside `nextWakeDeadline`, where the choice between the pacer's
deadline and loss detection's is made. `wake_send` is incremented at the select,
where every cause has already collapsed into one channel. The rule, stated so it
outlives this document: **attribute a counter where the decision is made, never
at the channel the event passes through.**

Two observations recorded rather than explained, both from the same runs:

- **`pushOther` was 4,966 against 4,953 lost packets in one interval** — the
  catch-all was retransmission pressure wearing a name that hid it, which is the
  uninterpretable bucket the reasons exist to remove. `loss` has its own count
  now, one per lost PACKET rather than per congestion event.
- **Thousands of packets are declared lost per 2 s on a netem path configured
  with zero loss** (4,491 and 3,806 above, against `loss_events` of 1). The tc
  counters say the link dropped nothing, and the wire carries only 1.10x the
  file, so these cannot all be retransmitted. What the loss detector is giving up
  on, and what happens to it afterwards, is the next thing to measure — and note
  that `loss_spurious` cannot answer it, for the reason two amendments up:
  `GenerateACK` clears its ranges, so a packet number is reported in exactly one
  ACK and a vindicating ACK for a lost one never comes.

An earlier reading also said in-flight sits at one packet on the lab path. One
interval here shows 3,143,987 against a cwnd of 3,143,354, so that was an
artifact of instantaneous sampling: the window does close there, just not
always.

## Amendment — the window is the delay, so cwnd/srtt proves nothing (2026-09-06)

The fleet was restarted again, so the push reasons could be read on the real
path. Windows runner sending over Wi-Fi, three 32 MiB pulls, 3.63 MB/s on the
push and ~4 MB/s on the pulls, sampled from the runner every 2 s:

| parks | timer | app | ack | self | cwnd | loss | in flight | cwnd | srtt |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 17,381 | **13** | 508 | 10,347 | 10,874 | 248 | 0 | 771,001 | 770,028 | 215 ms |
| 10,373 | **6** | 270 | 5,802 | 6,197 | 229 | 0 | 605,682 | 604,402 | 113 ms |
| 12,078 | **6** | 308 | 7,306 | 7,358 | 181 | 58 | 487,179 | 486,924 | 125 ms |
| 11,522 | **8** | 302 | 6,924 | 6,927 | 207 | 26 | 415,492 | 414,042 | 68 ms |
| 8,777 | **5** | 224 | 5,231 | 5,143 | 202 | 41 | 367,213 | 365,857 | 60 ms |

Three things are now measured rather than argued on the path that raised the
question:

- **The timers are not it.** 5–18 timer wakes out of 7,900–17,400 parks.
- **The application is not it.** `app` is 2–3% of pushes, the same as the lab.
  The send buffer is not running dry on either path.
- **`ack` and `self` are equal to within a percent, in every interval.** One
  range retired, one packet emitted. That is an ACK-clocked sender, and with
  `in flight` sitting on `cwnd` in every busy sample and a non-zero `cwnd` push
  count (streams really are parking in `congestionBlocked` and being revived),
  the sender is genuinely window-blocked.

**And here is what the previous two amendments both got wrong, in opposite
directions, using the same vacuous arithmetic.** "cwnd/srtt lands on the
throughput measured" was offered as evidence of window-limitation — by the
radios amendment, and again by the correction that retracted it. It is not
evidence of anything:

| interval | in flight | ÷ 4 MB/s | srtt |
| --- | --- | --- | --- |
| 1 | 771,001 | 193 ms | 215 ms |
| 3 | 487,179 | 122 ms | 125 ms |
| 4 | 415,492 | 104 ms | 68 ms |

**The window's own drain time IS the round-trip time.** The bytes in flight are
standing in a queue, and srtt is measuring that queue. So `cwnd/srtt` equals the
delivered rate identically, for ANY cwnd, and can never distinguish "the window
limits the rate" from "the window sets the queue depth". Both amendments read a
tautology as a measurement.

What survives is the direct observation, not the arithmetic: the sender is
blocked by its window (`in flight == cwnd`). What does NOT follow is that a
larger window would deliver more — at 25–40× a plausible bare-path BDP it would
deliver more queue. An srtt of 60–215 ms on a two-station LAN Wi-Fi hop is the
anomaly, and it is self-inflicted.

**The cheapest thing that separates them is one field.** `congestion.RTTStats`
already tracks `MinRTT` and `InternalState` does not carry it. min_rtt against
srtt is queueing delay, directly: if min_rtt is a few milliseconds while srtt is
150, the window is standing in a buffer and the controller is filling it. That
is the next increment, and it is one field rather than another investigation.
