# Runner identity, decoupled from the connection — Design

Status: draft, pending operator review
Date: 2026-09-09

Prerequisite, already landed: `6b6aafe7` (every agentboard key goes through
`server/boardkey.go`) and `6dd96a6a`..`68089459` (`ConnID` exists and the five
dial-address wire fields use it, byte-identically).

## 1. Problem

A `protocol.RunnerID` is an address. Every field of it is filled straight from
the runner's `objproto.ConnectionID` — transport, ip, **port and
unique_number** included (`server/dispatch.go:136-151`) — so its value changes
on every reconnect of the same runner process.

Three consequences, in increasing order of how much they hurt:

1. **A runner's identity expires with its connection.** Nothing in the system
   can name "this runner process" across a re-dial. The launcher does have a
   durable name (`--as TAG` → `bin/.run/agent-runner-<TAG>.{pid,log}`) but it
   reaches neither the runner process nor the wire, which is why two slots on
   one host serving the same roots are ambiguous and had to be addressed by
   agent profile instead.

2. **An agent's credential dies with its runner's connection.** The agentboard
   keys a task's ticket and its `taskState` by `(runner identity, task id)`
   (`agentboard/registry.go:18-37`), the agent's env freezes the runner id at
   spawn (`runner/agentenv.go:55`), and `Validate` looks the pair up
   (`agentboard/registry.go:62-73`). So a surviving agent whose runner
   reconnected presents a stale key and is told `UnknownTask` — not even
   `RunnerMismatch`, so the diagnosis misleads.

   The original design knew this and justified it conditionally: *"`runner_id`
   は ephemeral (再接続で変わる) だが、agent ≈ task の寿命と一致するため
   identity 切れは発生しない。runner disconnect 時 task は Failed 化 (既存
   `OnRemove`) → 同じ identity で再接続することはない"*
   (`docs/superpowers/specs/2026-04-28-agent-comms-design.md:77`). The premise
   is still enforced, by `failAndRevokeTasksOf` (`server/server.go:1291-1297`).

3. **Therefore a task cannot be held across a reconnect.** Which is the actual
   goal behind this spec: a deliberate server restart currently kills every
   interactive session, the fleet has to be re-established by hand, and the
   observed adaptation is to stop keeping sessions up at all. Nothing in that
   chain can be fixed while identity expires with the connection.

This spec addresses (1) and (2). Holding a task across a reconnect is the next
change and is explicitly **not** attempted here (§2).

## 2. Non-goals

Marked so a later reader does not mistake a boundary for a refusal — these are
v1 scope lines, not decisions that the ideas are wrong.

- **Holding a task across a reconnect.** No PTY re-binding, no runner-side gap
  ring, no re-adoption message, no `task_held` WAL event. This spec only removes
  the reason those cannot work.
- **Capacity accounting across a reconnect.** `--max-tasks` binding stays
  per-connection (`Registry.BindTask`), so a reconnect still starts from zero
  active tasks. Correct today (the tasks died); it becomes the held-task
  change's problem.
- **A durable per-slot name on the wire.** The launcher's `--as TAG` stays
  local. Identity here is per **process**, so an operator pin still goes stale
  across a restart — strictly better than today (it goes stale across a
  reconnect) but not durable.
- **Retiring the runner-ambiguity workaround.** `--agent <profile>` and the
  interactive picker stay as they are.
- **Migrating old WAL entries.** See §8.

## 3. Decisions taken

`Decided-by` is provenance, not emphasis. Only rows marked `operator` were
settled by the operator; everything else is the author's call and can be argued
with on its merits.

| # | Decision | Decided-by |
|---|---|---|
| D1 | Do this at all: separate runner identity from the connection | operator |
| D2 | Identity is a 16-byte opaque id, the same shape as `TaskID` | operator |
| D3 | Identity is minted by the **runner**, once per **process**, and carried in `RunnerHello` | author — forced, see below |
| D4 | `DialRunnerRequest.target` becomes `ConnID` (address) | author — forced, see below |
| D5 | `DialRunnerRequest.via` stays `RunnerID` (identity) | author |
| D6 | `RunnerInfo` gains an address field; `ConnInfo` gains `principal_runner` | author |
| D7 | `--runner` keeps accepting a pasted address, via a new `by_conn_id` selector arm | author |
| D8 | `Registry` stays keyed by connection id; an identity→connection index is added beside it | author |
| D9 | `TaskEntry.AssignedTo` and `TaskInfo.assigned_to` become the identity | author |
| D10 | No compatibility shim; deploy is server-first with a fleet restart | author — dogfood scope |
| D11 | A zero `runner_id` in a hello is REJECTED at the identity gate as `NoIdentity` | author |

**D3 is forced, not preferred.** Server assignment cannot produce an id that is
stable across a reconnect: to re-issue the same id the server would have to
recognise the runner before assigning one, which requires the runner to identify
itself first. So the runner mints it. `RunnerHelloResponse.your_runner_id`
therefore stops being an assignment and becomes an echo — kept, because the
runner still needs to hear the value the server will use, and a mismatch is then
visible instead of silent.

Per **process**, not per slot, is also forced by what the id has to mean: a
reconnect must keep it (the children are alive) and a restart must change it (the
children are dead). The id changing IS the statement "do not re-adopt", so
nothing has to detect a restart separately. Consequence: it is held in memory and
never written to disk. Persisting it would make a restart look like a reconnect,
and that failure would only ever appear after a crash.

**D4 is forced too.** `DialRunnerHandler.Handle` validates the target and then
dials it with ECDH on the spot (`server/dial_runner_handler.go:78-104`); the
runner is **unregistered** at that moment and only afterwards goes through PSK →
`RunnerHello` → Registry insert. There is no identity to resolve, so the operator
must supply an address.

**D5 is the opposite answer in the same message.** `via` names a *registered*
proxy runner, looked up today by exact `ConnectionID` match (`ResolveVia`, wired
to `Registry.GetByConnectionID`). A live registered runner is exactly what an
identity names, and resolving identity→address makes `via` survive the proxy's
own reconnect, which exact-address matching does not.

**D7**: after this change `ls` prints the identity hex as `id=`, so that is what
an operator copies. But `fa7f5108` exists because operators paste the
`transport:ip:port-id` form, and that form no longer *is* a `RunnerID`. Keeping
it working needs its own selector arm rather than a parse fallback.

**D11**: a zero identity is an ABSENT one, and absorbing it is worse than
refusing it. The board keys every ticket by (runner identity, task id) and
`RegisterTask` overwrites, so two runners both claiming zero would share one key
namespace: dispatching to the second invalidates the credential the first one's
agent is holding. `NoIdentity` is the status because it is already classified
RETRYABLE (`cli/persist.go`, `PskRejectedError.Retryable`), so the runner that
sends a zero id — one built before the field existed — reconnects and self-heals
once the pair is consistent, rather than exiting. That is the behaviour
`d4f7a5a` established after a wire skew killed twelve slots, and it is the
reason this change does not need its own compatibility story beyond restart
order. `boardRegisterTask` refuses zero as well, so a path that ever bypasses
the gate fails loudly instead of handing out a credential another dispatch will
silently overwrite.

**D8**: `runners map[string]*RunnerEntry` keyed by connection id is not the bug —
it is the index of *live connections*, which is legitimately connection-scoped.
What is missing is a second index. Re-keying the map instead would touch ~40 call
sites across five API methods for no gain.

## 4. Wire changes — all of them, in one place

```diff
-format RunnerID:
-    transport_len :u8
-    transport :[transport_len]u8
-    ip_addr_len :u8
-    ip_addr_len == 0 || ip_addr_len == 4 || ip_addr_len == 16
-    ip_addr :[ip_addr_len]u8
-    port :u16
-    unique_number :u16
+# RunnerID names WHICH RUNNER PROCESS, and nothing else: 16 opaque bytes minted
+# by the runner at startup. Stable across that process's reconnects, different
+# after a restart — the change of value is what says "its children are gone".
+# Same shape as TaskID so the hex plumbing, selectors and display are shared.
+format RunnerID:
+    id :[16]u8

 format RunnerHello:
     version :u8
+    # Minted per runner PROCESS. The server records it and answers with it in
+    # RunnerHelloResponse.your_runner_id; a value that comes back different is a
+    # bug worth seeing rather than absorbing.
+    runner_id :RunnerID
     hostname_len :u8
     ...

 format RunnerInfo:
     id :RunnerID
+    # WHERE this runner is reached right now. Was implicit while id was an
+    # address; without it `ls` loses transport, host and port, and hostname
+    # alone cannot separate two slots on one machine.
+    addr :ConnID
     hostname_len :u8
     ...

 format ConnInfo:
     cid :[cid_len]u8
     role :ConnRole
     principal_task :TaskID
+    # runner conn: which runner process it belongs to. All-zero for every other
+    # role, mirroring principal_task. Until now `cid` answered this by accident.
+    principal_runner :RunnerID
     ...

 enum RunnerSelectorKind:
     :u8
     any
     by_runner_id
     by_hostname
     by_ip
+    by_conn_id   # a pasted transport:ip:port-id, which is no longer a RunnerID

 format RunnerSelector:
     kind :RunnerSelectorKind
     match kind:
         ...
+        RunnerSelectorKind.by_conn_id => conn_id :ConnID

 format DialRunnerRequest:
-    target :RunnerID
+    target :ConnID      # an UNREGISTERED runner: there is no identity yet (D4)
     via    :RunnerID    # a REGISTERED proxy runner, so identity (D5)
```

### `agentboard/agentboard.bgn` — a second schema, easy to miss

The board carries its OWN `RunnerID` format, address-shaped like the other one
was, used by `from_runner_id` (the provenance the server stamps on every
message) and `from_runner`:

```diff
-format RunnerID:
-    transport_len :u8
-    transport :[transport_len]u8
-    ip_addr_len :u8
-    ip_addr_len == 4 || ip_addr_len == 16
-    ip_addr :[ip_addr_len]u8
-    port :u16
-    unique_number :u16
+format RunnerID:
+    id :[16]u8
```

Note its constraint excluded `ip_addr_len == 0`, unlike the protocol one, which
is why `boardRunnerIDFromProto` needed a zero-length guard (`0fd8a79c`). Both
the constraint and the guard go away here.

`TrsfConnState` deliberately does NOT gain `principal_runner`: its rows already
carry `cid`, which joins to the `ConnInfo` listing that has it.

Unchanged and still `RunnerID`: `RunnerHelloResponse.your_runner_id`,
`AgentInfo.runner_id`, `TaskInfo.assigned_to`, `RunnerSelector.by_runner_id`,
`RunnerStatusEvent.runner_id` — all of them identity, all of them now 16 bytes
instead of an address.

`TaskInfo` gains nothing: `cli/list.go` already joins tasks to runners through
`runnerByID[...]`, so the task row reaches the address via `RunnerInfo.addr`.

## 5. Server

- `RunnerEntry` gains `Identity protocol.RunnerID`, taken from `RunnerHello`.
- `Registry` gains `byIdentity map[string]string` (identity hex → connection
  id), maintained by `Add`/`Remove`, plus `GetByIdentity(protocol.RunnerID)`.
  The existing map and its five id-taking methods are untouched (D8).
- **One live connection per identity.** A hello whose identity is already held
  by a live entry closes the OLD connection and takes over. New wins: the runner
  re-dialed, so by hypothesis the old path is dead, and the runner is the
  authority on its own liveness. Making the old side wait for ping timeout would
  reintroduce the very latency this exists to remove.
- **Late cleanup must be fenced.** With one entry per identity, the old
  connection's teardown can arrive after the new one registered — the normal
  case for anything but a clean close, since the runner notices a drop first and
  the server's detection is bounded by `--ping-interval`. So `RunnerEntry`
  records which connection owns it and every runner-scoped cleanup
  (`failAndRevokeTasksOf`, unbind, revoke) is a no-op when the caller is not the
  current owner. No wire field: the server already knows which connection a
  message arrived on. Note this race does not exist today and is bought by the
  decoupling — a connection-derived key cannot collide with itself.
- `server/boardkey.go`'s three helpers keep taking the runner **connection** id
  and resolve it to the identity internally; they gain a `*Registry` parameter.
  The twelve call sites change by one argument and nothing else. This is what
  the funnel was for.
- `TaskStore.Assign` receives the identity (D9); `Registry.BindTask/UnbindTask`
  keep taking the connection id (capacity is per connection, §2).

## 6. Runner

- Mint 16 random bytes at process start, hold them in memory, send them in
  `RunnerHello`, and compare `RunnerHelloResponse.your_runner_id` against them —
  log loudly on mismatch.
- `PersistLoop` rebuilds the Session per connection; the identity must live
  ABOVE that loop, or a reconnect mints a new one and the whole change is inert.
  This is the single easiest way to get this wrong.
- `HARNESS_RUNNER_ID` in the agent env becomes the identity hex. It stops going
  stale on reconnect, which is the user-visible half of problem (2).

## 7. Surface matrix

| Surface | Change |
|---|---|
| `ls` (text) | `id=` becomes 32-hex; new `addr=` column from `RunnerInfo.addr` |
| `ls --json` | `id` becomes hex; `addr` added |
| `ls` task rows | `assigned_to` renders the runner's identity; the address comes from the join |
| `session ls`, `conns` | `conns` gains a runner-identity column from `principal_runner` |
| `--runner` | 32-hex is the primary form; a pasted `transport:ip:port-id` routes to `by_conn_id` (D7); the `IpAddrLen == 0` guard in `buildRunnerIDSelector` is deleted — it is residue of an encoder invariant that no longer exists |
| TUI | runner pane + detail (`tui/detail.go:118,202`), task rows (`tui/tasks.go:240,262`), `tui/interactive.go:106` |
| WebUI / wasm | `cmd/harness-webui-wasm/main.go:989`, the runner listing and any id display |
| Agent env | `HARNESS_RUNNER_ID` |
| `cli/list.go` | `runnerByID` keys become identity hex (9 sites) |

## 8. Rollout

- **Server first, then the fleet.** `RunnerHello` and `ClientHello` both carry a
  `RunnerID`, so this is the same class of change as the one that killed twelve
  runner slots (`d4f7a5a`, and Pitfall 10). `no_identity` is retryable now, so a
  skew costs a reconnect rather than a wipe — but only once both ends carry that,
  so the order still matters.
- `scripts/wire-skew-check.sh` must be run and is expected to REJECT-then-heal
  here, unlike the ConnID split where it reported no rejection at all.
- **Old WAL entries keep connection-id strings in `runner_id` /
  `bound_runner_id`** (`server/wal.go:38,89,110,133`). They will not resolve to
  any live runner. Accepted: by the time a replay sees them the tasks are
  terminal, and a stale `assigned_to` on a finished task is cosmetic. No shim
  (D10, dogfood scope).
- Every agent alive at the restart dies with its task, as today.

## 9. What could go wrong

1. **Identity minted inside `PersistLoop`** → new id per reconnect → the change
   does nothing and looks like it works. §6.
2. **A missed cleanup fence** → a late teardown fails tasks the new connection
   owns. Presents as tasks that die intermittently after a reconnect with
   `err="runner_disconnected"` and no reproducible trigger.
3. **Identity claimed by another runner.** Self-minted means claimed, so two
   runners can name the same id and the second takes the first's connection slot
   — the same mechanism as the reconnect takeover, which is why it cannot be
   rejected outright. Authentication does not degrade (the board still requires
   the per-task 128-bit ticket); *task routing* does. Bounded by: runner
   registration is PSK-gated, and a runner is already trusted with `--roots` and
   arbitrary dial per the README's trusted-hub statement. Accepted at this
   scope, and written here rather than discovered.
4. **`by_conn_id` forgotten in one surface** → `--runner ws:...` silently
   selects nothing somewhere. The matrix in §7 is the checklist.
5. **Board key resolution failing during the hello window** → a task dispatched
   to a runner whose identity is not yet indexed registers under a zero key.
   Order the index insert before the first dispatch can reach that entry.

## 10. Testing

- Unit: identity round-trip; `Registry` identity index add/remove/takeover; the
  cleanup fence (old owner's teardown after a takeover is a no-op); selector
  parsing for both `--runner` forms; `boardkey` helpers resolving through the
  registry.
- `scripts/wire-skew-check.sh` — must show reject-then-heal.
- Live, on `scripts/dummy-harness.sh`, because none of the above crosses a
  process boundary:
  1. `agent send` / `agent inbox` from a task, then **kill the runner's
     connection without killing the runner process** and confirm the agent's
     credential still validates after the reconnect. This is the whole point of
     the change and the only check that actually proves it.
  2. `ls` / `conns` / `--runner <hex>` / `--runner <addr>` all resolve the same
     runner.
  3. A restart of the runner **process** must produce a *different* identity and
     must NOT re-adopt anything.
- Windows is a separate pass: the identity lives across `PersistLoop`, and the
  runner's own handshake is a distinct code path (`sendRunnerMergedHandshake`).
