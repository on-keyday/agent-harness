# Persistent reverse dial, and kicking one runner — design

## Problem

The motivating layout (operator, 2026-11-04): the server stays on a home
Raspberry Pi with nothing opened inbound, and a runner sits on a cloud host
(AWS). The runner listens (`agent-runner --listen` / `--udp-listen`) and the
server dials it. Today that works once and then stops working.

- **P1 — a reverse-dialed runner does not come back.** `server dial-runner` is
  one dial (`server/dial_runner_handler.go`, `Handle`); nothing re-dials when
  the connection ends. The 2026-05-22 reverse-dial spec made this a non-goal
  (`2026-05-22-server-mode-runner-reverse-dial-design.md:26`: "conn が切れたら
  admin が再 trigger する"). This spec reverses that non-goal.
- **P2 — a deliberate server restart loses a listen-mode runner's held tasks.**
  The hold spec names the gap (`2026-09-10-task-hold-across-server-restart-design.md`
  §6a.3): the reverse-dial set is not persisted, so the restarted server does
  not know to dial the runner back, and held tasks expire at the end of
  `--hold-window` unless an operator re-dials by hand.
- **P3 — a listen-mode runner without a server key takes any dialer as its
  server.** Listen mode runs the same handshake as dial mode
  (`runner/listen.go` `handleServerConn` → `driveAfterConn` →
  `cli.CheckPskAuthReply`, `runner/connect.go:614`), and `VerifyServerAuth`
  checks nothing when no key is configured (`cli/server_auth.go`, `pub == nil`
  → `nil`). Without `--server-pubkey` the only log line is the absence of
  `server key configured`. Anyone who reaches the listening port and answers
  `ok` can assign tasks.
- **P4 — one runner cannot be removed without removing its key.** The only
  removal is `runner unenroll <key> --force` (`server/runner_keys_handler.go`):
  it removes the key, and by default one key serves every slot on a machine
  (enrollment spec D9). The connection close it does is not removal either: a
  dial-mode runner reconnects (`cli.PersistLoop`), and a runner refused as
  `not_enrolled` keeps retrying (`cli/persist.go:124-128`, retryable). There
  is no verb that targets one runner.
- **P5 — a listen-mode runner can hold two server connections, and the older
  one's teardown kills the newer one's tasks.** `ListenAndServe` assumes at
  most one server connection ("at most one server conn is alive at a time",
  `runner/listen.go`), but nothing enforces that. When a connection ends,
  `OnConnect` calls `cancelTasksUnlessHeld`, which cancels **every** task in
  the process-wide registry (`runner/hold.go:708-723`), not only that
  connection's tasks. Dial mode does not get two connections, because
  `PersistLoop` (`cli/persist.go:195-`) is one loop that dials, runs
  `onConnect`, and dials again only after that returns. P1's fix creates
  the overlap: after a server crash and restart, the new server dials while
  the runner's old UDP connection is still waiting for its inactivity GC (~1
  min). When that GC fires, it cancels the tasks the new server has just
  assigned.

## Scope

This spec covers all of P1–P5. Persistence and kick are in one document
because both append to `message.bgn` and both decide what happens to a dial
registration. A kick of a reverse-dialed runner must remove its registration,
or the server dials the runner straight back.

## Decisions taken

**operator** = chosen by the human in conversation (2026-11-04). **this spec**
= chosen by the author while writing. Rows marked "this spec" are the ones
worth a second look.

| # | Decision | Decided by |
| --- | --- | --- |
| D1 | The server persists reverse-dial registrations and re-dials them, both after a disconnect and after its own restart | operator |
| D2 | An unplanned disconnect (home link drop, server crash) ends the tasks on that connection, as it does in dial mode today. Only a deliberate server restart preserves tasks, through the existing hold | operator |
| D3 | Registering a reverse dial is something an agent can do with a capability, with no operator step. The model is `runner enroll` (enrollment spec D2) | operator |
| D4 | A registration holds an **ordered list of candidate addresses as text**, and every attempt walks the list from the top, re-resolving each entry. These are the rules of `agent-runner --server-cid` (`runner/connect.go` `ConnectWith`, `runner/servercandidates.go` `ResolveServerCandidate`). Because the text is re-resolved, a host name follows a cloud host whose public IP changes on stop/start | this spec (proposed in conversation, not objected to) |
| D5 | Unenrolling a key removes every registration bound to it | operator |
| D6 | A registration is bound to an enrolled key when it is added: `--key <prefix>` is required, and adding with a key that is not enrolled is refused. A connection made for a registration that authenticates with a different key is closed (D12). The alternative was to learn the key at the first successful connection; that leaves a window in which a registration is bound to no key, so unenroll (D5) would not cascade to it | this spec |
| D7 | A runner can be kicked by itself, independently of its key | operator |
| D8 | A kicked runner leaves: it ends its tasks and the process exits with status 0. Keeping it out after a reboot or re-login is left to `runner unenroll` and the host-side `runner-autostart.py unregister`. The server keeps no ban list | operator (chose option ① of three) |
| D9 | Kicking a runner that a registration dialed also removes that registration | operator (part of option ①) |
| D10 | `agent-runner --listen` / `--udp-listen` with no server key (`--server-pubkey`, `--server-pubkey-file`, or their env vars) refuses to start: exit 1, with a message naming the flag. There is no override flag. The server always has a key (`--server-key-file`, generated when absent), so the only cost is copying `server-key.pub` once | operator ("fail closed でいい") |
| D11 | Capabilities. Adding, listing and removing dial registrations is gated on `runner_admin`. Kicking is gated on `runner_enroll`. Reason: enrollment spec D7 drew the line that `runner_admin` is "using the server's network reach (`dial_runner`)" and `runner_enroll` is "adding to the registry". A persistent dial is the first kind of power, made durable. Kicking a runner is registry hygiene, the reason `runner_enroll` exists (enrollment D4) | this spec |
| D12 | A registration's connection that authenticates with a key other than the bound one is refused **before `Registry.Add`**. The record travels from the dialer to the runner-hello handler keyed by ConnectionID, the way `pendingViaInfo` already does (`server/server.go:762-776`, `takePendingViaInfo`). On a mismatch the handler sends no `RunnerHelloResponse`, closes the connection with `KickConn`, and records `key mismatch: <prefix16>` as the record's last error. This is the shape of the existing identity-key refusal (`server/runner_handler.go:176-189`: "the gate already said ok, so the runner sees a plain drop and retries"). The check cannot sit at the PSK gate: the gate has already answered `ok` by the time the hello is dispatched (`server/server.go:1458-1470`). No new `PskAuthStatus` is added, because the handshake is the path the harness upgrades itself through (implementation-pitfalls Pitfall 10) | this spec |
| D13 | A listen-mode runner keeps at most one server connection. The new connection replaces the old one when its **`RunnerHelloResponse`** arrives, i.e. once the server has registered it. Passing the signature check alone is not enough: under D12 a verified server can still refuse the connection, and retiring the good connection for a refused one would cancel every task each time the record retried (builder check, 2026-11-04). The previous connection is closed, and the runner waits for its `OnConnect` to return, so `retireSender` and `cancelTasksUnlessHeld` have run. This happens synchronously inside the handling of that `RunnerHelloResponse`, before any later message on the new connection is dispatched. An `AssignTask` on the new connection therefore lands after the old teardown, given that the server registers a runner before it assigns to it (§6 tests that ordering) | this spec |
| D14 | Kick refuses a runner that has live tasks unless `--force` is given, and lists those tasks. This follows `runner unenroll`'s in-use guard and the `runner-down` skill's refusal | this spec |
| D15 | A runner too old to know the `leave` request decodes it as an error and ignores it (`runner/connect.go` `dispatchRunnerRequest`: `req.Decode` failure is logged and returns). The server therefore waits 10 s for the connection to end. If it has not ended, the server closes it with `kickConn` and reports `closed_without_leaving`. A dial-mode runner treated this way reconnects; the result says so | this spec |
| D16 | Registrations are stored in `<data-dir>/runner-dials.json`, beside `runner-keys.json` and in the same way (`server/runnerkeys.go`): JSON, written to a temporary file then renamed, mode 0600, and a file that does not parse fails startup. Nothing in a task record refers to a registration, so it does not go in the WAL, and using JSON keeps it off the `.bgn` disk axis | this spec |
| D17 | `runner dial rm` stops re-dialing and does **not** close a live connection. To disconnect now, use `runner kick`, which also removes the registration (D9) | this spec |
| D18 | `server dial-runner` (one dial) and `--via` stay as they are. A persistent registration through a relay runner is out of scope | this spec |
| D19 | Kick and dial-registration verbs are reached from the command line on all three surfaces (CLI, TUI cmdline, WebUI command input). No buttons or keybindings: like `runner enroll` and `server dial-runner`, these are administrative verbs run a handful of times (`NoModalSurface` with that reason) | this spec |
| D20 | One runner, one connection, whoever dialed it. A second registration of the same identity on the same key displaces the first (`server/registry.go:185-213`, `displaced`), so two dialers aimed at one runner would replace each other forever, killing tasks on every switch. Three rules prevent that. **Adopt:** before an attempt, if the registry already holds a runner on the bound key whose ConnectionID has the transport and address of one of the record's resolved candidates, the record takes that connection as its live one and does not dial. Only transport and address are compared; the ID part is random per dial, as with `ViaDialAddr`. This covers moving a runner from a one-shot `server dial-runner` to a record without touching its tasks. **Exact duplicate:** `runner dial add` refuses a candidate whose text equals a candidate of an existing record (`duplicate_candidate`). **Runtime duplicate:** if a record's connection displaces another record's live connection, the displaced record goes to state `duplicate` (naming the other record's id) and stops dialing until it is removed. The tasks on the displaced connection end once; there is no loop. A one-shot `server dial-runner` that displaces a record's connection ends it once as well; the record then adopts the new connection by the adopt rule | this spec (builder check, 2026-11-04) |

## Non-goals

- Keeping tasks alive across an unplanned disconnect (D2).
- Keeping a kicked runner out after its host restarts (D8).
- Persistent dials through a relay runner (D18).
- Keeping an in-task agent's proxied connection alive across a restart. That
  connection rides the runner's connection and is re-dialed by the agent (hold
  spec §6a.4). Unchanged.

## 1. Dial registrations (server)

### 1.1 Record

```
RunnerDialRecord {
  ID          uint32     // server-assigned, random, unique; shown as 8 hex
  Candidates  []string   // ordered, as typed: "ws:runner.example:8540-*", ...
  Key         ed25519.PublicKey  // the enrolled key it is bound to (D6)
  Label       string
  AddedAt     time.Time
  AddedBy     string     // "operator" or "task:<32 hex>", as enrolledBy
}
```

`RunnerDialStore` mirrors `RunnerKeyStore`: Load / Add / Remove / List /
RemoveByKey, persisted to `<data-dir>/runner-dials.json` (D16).

Limits: at most 8 candidates per record, each at most 255 bytes, 512 bytes in
total; a label of at most 64 bytes. Every candidate must parse with
`objproto.ParseConnectionID(spec, AllowRandomID)` **without** resolving, so a
host name that does not resolve yet is still accepted. The transport must be
one the server's endpoint has a leg for (ws / udp / vsock). Otherwise the add
is refused with `invalid_candidates`.

### 1.2 Dialer

One goroutine per record, owned by the server. The dialers start inside
`serve`, **after** the WAL has been replayed (`ReplayEvents`,
`server/server.go:868`), `SetWAL` (:889), and `watchHeldDeadlines` has
started (:972). The point where the endpoint exists (:750) is too early. A
dialed connection skips the accept loop (`OnDialed` → `handleConnection`), so
a runner registered before replay would be answered with no held tasks, and
its `killHeldExcept` would kill the very children P2 is about. Every stored
record starts dialing at once, in parallel, which is what makes the
re-adoption land inside `--hold-window`. A newly added record starts
immediately. Before each attempt, apply D20's adopt rule.

One attempt goes as follows:

1. Walk `Candidates` in order. For each entry, resolve it with the same
   function the runner uses: `runner.ResolveServerCandidate` moves to `cli`
   (`cli.ResolveCandidate`, which `runner` then calls), because `server` does
   not import `runner` and both already import `cli`. Then dial as
   `DialRunnerHandler.Handle` does: ECDH, `DialGreeting`, then
   hand the connection to `handleConnection` through `OnDialed`. Each
   candidate has a 10 s timeout (the existing `DialTimeout` default). The
   first candidate that completes ECDH ends the walk.
2. The attempt **succeeds** when that connection registers a runner (the
   registry inserts an entry for its ConnectionID) authenticated with
   `Key`. A different key → D12. A connection that ends without registering
   is a failed attempt, recorded with the reason the gate logged.
3. While the connection lives, the dialer does nothing. It never dials a
   second connection for a record that already has one.
4. When the connection ends, or an attempt fails: back off and try again.
   The backoff starts at 500 ms, doubles to a cap of 30 s, and resets after a
   successful attempt. These are `cli.PersistConfig`'s defaults, the
   runner-side twin of this loop. There is no retry limit.

Server shutdown cancels every dialer before the connections are drained. The
hold sequence is unchanged.

Per-record state shown by `runner dial ls`:
- `connected`: the runner identity and the candidate that answered
- `dialing`
- `waiting`: the time of the next attempt and the last error
- `duplicate`: the id of the record whose connection displaced this one's (D20)

### 1.3 Interaction with keys

- `runner dial add` resolves `--key` through `RunnerKeys.Resolve`: a unique
  prefix of at least 8 hex characters, the same rules as `unenroll`.
- `runner unenroll` removes every record bound to the key (D5), and its
  answer lists them. A key that has records but no live runner is **not**
  "in use" for the `--force` guard. That guard keeps its current meaning,
  which is live runners. Records are listed in the answer so that nothing
  disappears silently.

## 2. Kick

`runner kick <runner-identity-prefix> [--force]`. The prefix is at least 8
hex characters of the 32-hex identity, the `id=` column of `harness-cli ls`.

Server steps:
1. Resolve the prefix against the live registry: `not_found` /
   `ambiguous_prefix`.
2. If the runner has tasks that are not terminal and `--force` was not given:
   `in_use`, listing those tasks (D14).
3. Remove every dial record that points at this runner (D9), so no re-dial
   is started. A record points at the runner if any of these holds:
   - its live connection is the runner's connection;
   - the runner identity of its last registration equals this runner's;
   - one of its resolved candidates has the transport and address of the
     runner's ConnectionID.

   The last two catch a record that is in `waiting` or `duplicate` while the
   runner is connected some other way (D20). Without them, the record would
   keep dialing the port and bring the runner back after its host restarts.
4. Send `RunnerRequest{leave}` with `by` = the caller's principal (as
   `enrolledBy` formats it).
5. Wait up to 10 s for the connection to end. If it ends: `left`. If not:
   `kickConn` and `closed_without_leaving` (D15).

Tasks end the way a runner death ends them: the registry's `OnRemove` →
`failAndRevokeTasksOf`.

Runner steps on `leave`:
- Log `leaving on the server's request` with `by`.
- Cancel the process context. This is the path `--shutdown-file` takes, so
  the existing graceful-shutdown order applies, and no hold is armed, so
  children are cancelled.
- Exit 0. In dial mode, `PersistLoop` must not reconnect after a `leave`; it
  already waits for `onConnect` and then calls `h.Close()`
  (`cli/persist.go:261-264`), so the wire Close goes out. In listen mode,
  `ListenAndServe` must **drain** before it returns. Today it returns as soon
  as `ctx.Done()` fires (`runner/listen.go:108-111`), and `main` exits right
  after (`cmd/agent-runner/main.go:523-528`). That can happen before the
  server connection's `h.Close()` has sent its Close, and before the
  cancelled children are reaped. The server would then see nothing for about
  a minute, and kick would report `closed_without_leaving` for exactly the
  runners this spec is about. The fix: on cancel, `ListenAndServe` waits on
  D13's current-connection done channel (bounded at 5 s) before it returns.
  This also changes an ordinary listen-mode shutdown, which gains the same
  announced close that dial mode already has (`project_udp_peer_death`:
  "shutdown must SEND the close").

## 3. Runner listen mode

- D10: the check sits in `cmd/agent-runner/main.go` after
  `cli.ResolveServerPubKey`: `isListenMode() && serverPubKey == nil` → exit
  1. `--listen` / `--udp-listen` help text says so. Integration tests that
  call `runner.ListenAndServe` directly are not affected. If one of them runs
  without a key, it keeps doing so; the guarantee is at the binary.
- D13: the listen loop keeps the current server connection (handle + done
  channel). When the new connection's `RunnerHelloResponse` is handled, it
  closes the previous connection with `peer.Conn.Close`, waits for that
  connection's `OnConnect` to return, then lets dispatch continue. The hello
  the new connection sent already carries `Held = cfg.Tasks.heldReport()`,
  which is empty unless a hold is armed (`runner/connect.go`
  `buildRunnerHello`). So retiring at this point does not cancel any task the
  new hello reported. The comment "at most one server conn is alive at a
  time" becomes a statement of what the code enforces.
- The drain on cancel (§2) uses the same done channel.

## 4. Wire (`runner/protocol/message.bgn`)

All values are appended, so existing ordinals stay stable.

- `TaskControlKind`: `runner_dial_add`, `runner_dial_list`,
  `runner_dial_remove`, `runner_kick`.
- `RunnerDialAddRequest { key_prefix (u8 len, ≤64), label (u8 len, ≤64),
  candidate list (u8 count ≤8, each u8 len) }` → `RunnerDialAddResponse {
  status, id :u32 }`.
- `RunnerDialListRequest {}` → response carries a stream id; the rows travel
  on that stream (`respondWithBody`), as `runner keys` does. Rows must not be
  inline (`TestResponseRowsAreNotInline`).
- `RunnerDialRemoveRequest { id :u32 }` → `{ status }`.
- `RunnerKickRequest { prefix (u8 len), force :u1 + reserved }` →
  `RunnerKickResponse { status, stream_id }`; the body (runner identity,
  hostname, the task list for `in_use`, the removed dial record if any)
  travels on the stream.
- `RunnerUnenrollBody` gains the list of removed dial records.
- `RunnerRequestType.leave` → `LeaveRequest { by (u8 len, ≤64) }`.
- Status enums: `RunnerDialStatus { ok, invalid_candidates, key_not_found,
  ambiguous_key, not_found, duplicate_candidate }`; `RunnerKickStatus { left,
  closed_without_leaving, not_found, ambiguous_prefix, in_use }`.

Every request is one control datagram over UDP. A test encodes the
worst-case `RunnerDialAddRequest` (8 × 255 would exceed the budget, which is
why §1.1 caps the total at 512) and fails above 1170 bytes
(`project_control_message_datagram_size`).

`scripts/wire-skew-check.sh` runs before landing. `leave` is a runner-decoded
format: an old runner logs a decode error, and kick falls back to D15.

## 5. Surfaces

| Surface | What |
| --- | --- |
| `cli/verb/table.go` | Rows `runner dial add` / `runner dial ls` / `runner dial rm` / `runner kick`, each with `WebUIDispatch` and `NoModalSurface` (D19). These reach the CLI, TUI cmdline and WebUI command input |
| WASM bridge | One function per verb, beside `runnerEnroll` / `runnerKeys` |
| TUI / WebUI buttons, keys | omitted (D19) |
| `runner dial ls` | Human rows and `--json`. Every field is printed, a zero value is not hidden (checklist 31) |
| `ls` runner rows, TUI runner table and detail | omitted: `runner dial ls` names the runner identity of each live record, which is the join an operator needs. A dial-id field on `RunnerInfo` would be a snapshot wire change for the same information |
| `caps` catalog (`cli/verb/caps.go`) | `runner_admin`: "+ runner dial add / ls / rm". `runner_enroll`: "+ runner kick" |
| `runner unenroll` output | Lists removed dial records |
| README | Reverse-dial section: persistence, listen mode needs a server key, `runner kick` |
| Agent skills | None mention `runner enroll` or `server dial-runner` today; unchanged |

## 6. Verification

Unit:
- `RunnerDialStore`: round trip, an unparsable file fails startup.
- Dialer: backoff and its reset, candidate order, no second connection while
  one lives, the start point after replay (a held task is re-adopted by a
  record dialed at startup).
- Key mismatch (D12): no `RunnerHelloResponse` is sent, the runner keeps its
  existing connection and its tasks, and the record's last error says
  `key mismatch`.
- D20: adoption of a one-shot connection without disturbing its tasks;
  `duplicate_candidate`; a runtime duplicate goes to `duplicate` and stops
  dialing, with no second switch.
- Unenroll cascade.
- Every kick status, including record removal by last identity and by
  address while the record is in `waiting`.
- Runner `leave` → exit 0 in both modes, `PersistLoop` does not reconnect,
  and over UDP-only listen mode the server sees the connection end within the
  10 s window (`left`, not `closed_without_leaving`).
- D13: a second connection retires the first when its
  `RunnerHelloResponse` arrives, and a task assigned on the second survives
  the first's teardown. Write this test red first, against the current code.
  Also check the order the test relies on: the server sends
  `RunnerHelloResponse` before any `AssignTask` on that connection.
- D10 flag check.
- Datagram-size test.

Dummy-harness E2E, run as the operator would type the commands:
1. A listen-mode runner with a server key. `runner dial add` → the runner
   registers. Restart the server **deliberately** with an interactive task
   running → the task is re-adopted with no manual dial (P2).
2. Kill the server (crash) and restart it → the runner re-registers by
   itself, and the task is Failed (D2).
3. `runner kick` → the runner process exits 0, `runner dial ls` no longer
   lists the record, and the runner does not return.
4. A listen-mode runner started without a key → exit 1 with the message.
