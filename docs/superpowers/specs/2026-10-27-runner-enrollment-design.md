# Runner enrollment, and the connect PSK's removal — design

## Problem

The connect PSK is the only thing a runner proves, and this fleet does not set
one. Read on 2026-10-27: none of the 11 `agent-runner` processes on the main
runner host has `--psk`, `--psk-file`, `HARNESS_PSK` or `HARNESS_PSK_FILE`, and
the task this spec was written from has no `HARNESS_PSK`. A server holding a PSK
answers a binder-less runner `bad_psk`, which is fatal to the runner
(`cli.PskRejectedError.Retryable`), so the server runs without one. The server
host's own command line was not read.

- **P1 — anything that can reach the server can register as a runner.**
  `pskGate.Check` skips the binder when no PSK is configured
  (`server/psk.go`, step 2), and `role=runner` proves nothing else. A
  registered runner is handed Queued tasks and a fresh ticket per task
  (backlog, "Any connect-PSK holder can register as a runner").
- **P2 — a runner cannot be shut out on its own.** The only runner credential
  is one fleet-wide secret; removing one machine means rotating it everywhere.
  The operator wants stale runners gone for hygiene, not for emergencies.
- **P3 — the connect PSK is unused because it is a chore.** It has to reach
  every runner's launch environment, every operator CLI that dials the data
  plane (`cli/dataplane_dial.go`), and every agent (`runner/agentenv.go`
  injects `HARNESS_PSK`). If it were set, every agent would hold one secret
  valid across the whole fleet.
- **P4 — the agent ticket and the data-plane grant travel as bearer values.**
  `cli/psk.go` puts the 16-byte ticket into `AgentInfo.auth_ticket`;
  `cli/dataplane_dial.go` puts the grant id into `DataPlaneInfo.grant_id`. The
  channel is encrypted (objproto ECDH + AES-128-GCM) but unauthenticated, so
  whoever terminates the handshake reads the value and can replay it on a
  connection of its own. The server's signature (2026-10-11) is checked only
  after the request carrying the ticket has been sent.
- **P5 — a runner identity can be taken over by any registering connection.**
  `Registry.Add` (`server/registry.go`) repoints `byIdentity` to the newest
  connection claiming a `runner_id` and returns the previous one for teardown.
  `runner_id` is self-declared, and an agent reads its runner's from
  `HARNESS_RUNNER_ID`.

## Threat model

Accidents, not a hostile agent: `docs/backlog.md`, "Multi-tenancy", question 1
(operator, 2026-10-27), resting on the harness-cli skill's "Trust model". An
agent outside a sandbox runs under its runner's uid and can read the runner's
key file, the server's data directory if it is on the same host, and other
tasks' `/proc/<pid>/environ`. Nothing here is a boundary against that. What it
buys is: a runner credential per key file (per machine by default, D9) that can
be withdrawn alone, nothing
fleet-wide in an agent's environment, credentials bound to the connection they
were presented on, and the end of P1 for hosts that are not part of the fleet.

## Decisions taken

**operator** = chosen by the human in conversation (2026-10-27); **this spec** =
chosen by the author while writing — those rows are the ones worth a second
look.

| # | Decision | Decided by |
| --- | --- | --- |
| D1 | Replace the connect PSK with runner enrollment rather than start setting the PSK | operator |
| D2 | Agents bring runners up today (`scripts/runner.sh up`, `runner-autostart.py register`, on Linux and Windows alike). Enrolling must be something an agent can do with a capability, with no operator step per runner | operator |
| D3 | Bootstrap: the operator enrolls the first runner; an agent on it is granted the enrollment capability and enrolls the rest | operator |
| D4 | Withdrawing a runner is for hygiene: list what is enrolled, remove what is stale. No emergency revocation machinery | operator |
| D5 | The migration is carried out through the harness itself, by agents, without falling back to out-of-band access. So the server keeps accepting the old handshakes until a follow-up change removes them. The server's own restart stays the operator's manual step, as it is today | operator |
| D6 | The agent and data-plane handshakes move to transcript-bound binders in the same change (operator: "楽にできるなら"; they touch the same gate code the PSK removal touches, and one wire change means one skew round) | this spec |
| D7 | A new capability bit `runner_enroll`, not `runner_admin`. `runner_admin` gates `dial_runner`, i.e. using the server's and registered runners' network reach (`server/dial_runner_handler.go`); a dialed runner still passes the handshake. Adding to the registry is a different power | this spec, after the operator pointed out the difference |
| D8 | Enrollment registers a runner's **public key**, entered by a `runner_enroll` holder. There is no bootstrap token: a token would be one more server-side secret with a lifetime, and both routes need the same one copy between the runner host and an authorised shell | this spec |
| D9 | The server knows keys only, never machines or slots: any number of runners may authenticate with one key. What a runner uses is its key file, and the default path is one file in the user's home, so by default every slot one user runs on one machine shares a key. `--runner-key-file` gives a slot its own key with no server change. Slots under one uid can read each other's files, so per-slot keys would separate nothing and would multiply enrolling, rotating and cleaning up by the slot count | operator accepted the default, 2026-10-27 |
| D10 | The old handshakes stay decodable through `AuthRole`, not `ClientKind`: today's two roles are renamed `client_legacy` / `runner_legacy` (numbers 0 and 1 unchanged) and the new shapes get `client = 2` / `runner = 3`. `ClientKind` is untouched, because it is also the principal kind the WAL stores as a number (`OriginKind`, `ResumedByKind`; `server/wal.go`) and ~15 sites compare against `ClientKind_Agent`; `AuthRole` is never persisted. The follow-up change deletes the two `_legacy` roles and the formats only they carry | this spec, to carry D5; revised after the builder check found the disk axis |
| D11 | A key the server does not know is answered a new `not_enrolled`, retryable. There is no flag that enrolls unknown keys automatically: under D5 the migration enrolls through an agent, so nothing needs one | this spec (Pitfall 10) |
| D12 | An identity held by a live connection authenticated with key K can be taken over only by a connection authenticated with K | this spec |
| D13 | The connect PSK is removed outright: flags, env, config fields, injection | this spec |

D5 and D10 are the first use of the one exception to this repo's
no-compatibility-shim rule (implementation-pitfalls, Pitfall 10): the path the
harness upgrades itself through. It is bounded as that exception requires: the
follow-up change that ends it is specified below, and its trigger is visible on
every operator surface.

## Mechanism

### The runner's key

- An Ed25519 key, the 32-byte seed stored as hex (the server-key format,
  2026-10-11 spec D10), mode 0600.
- Default path `<home>/.config/harness/runner-key` on every OS, `<home>` from
  `os.UserHomeDir()` (Windows: `%USERPROFILE%\.config\harness\runner-key`).
  `~/.config/harness/` is where this repo's per-user files already live
  (`server-key.pub`, `sandbox-claude-token`, the kvm guest's `agent-token`).
  Not `os.UserConfigDir()`: it follows `XDG_CONFIG_HOME` on Linux and
  `%AppData%` on Windows, so a runner started by a systemd user unit and an
  agent's shell running `--print-pubkey` could resolve different files.
  Overridden by `--runner-key-file` or `HARNESS_RUNNER_KEY_FILE`.
- Generated when the file is absent, as the server key is. A file that does
  not parse is a startup error, never a silent regeneration — regenerating
  would orphan the enrollment. Read once, at startup.
- `agent-runner --print-pubkey` prints the public key as hex and the path it
  read (generating the key if absent), and exits 0. That is how an agent or the
  operator gets the value to enroll. The runner logs the same two values at
  startup, so a runner reading a different file than the one enrolled shows in
  its log.
- The runner does not pass the key path to agents. Same-uid agents can read the
  file anyway (Threat model); sandboxed agents do not see it, because the
  sandbox kit does not mount `~/.config/harness`.

### Rotating a key

The store holds any number of keys, so a rotation overlaps the old and new key
and no runner is cut off:

1. `agent-runner --rotate-key` writes a new key to the key file, moves the old
   one to `<key file>.prev`, and prints the new public key and the path.
   Running runners keep the key they read at startup.
2. `runner enroll <new key>`. Both keys are now valid.
3. Restart the runners that use the file whenever convenient; each comes back
   with the new key. One restarted before step 2 retries on `not_enrolled`
   until step 2.
4. When `runner keys` shows no runner on the old key,
   `runner unenroll <old key>`.

Unenrolling first would close every runner on the key at once — every slot of
a machine under the default path.

### The handshake roles, and the runner handshake

`AuthRole` becomes:

| Value | Name | Arm |
| --- | --- | --- |
| 0 | `client_legacy` | `client_hello :ClientHelloLegacy` — today's `ClientHello`, renamed with the `AgentInfoLegacy` (ticket included) and `DataPlaneInfoLegacy` (grant id included) it contains |
| 1 | `runner_legacy` | `runner_hello :RunnerHello`, as today |
| 2 | `client` | `client_hello :ClientHello` — same `ClientKind`s, with `AgentInfo` and `DataPlaneInfo` lacking the bearer value (below) |
| 3 | `runner` | `runner_auth :RunnerAuth` |

The gate stores the role it accepted normalised to client / runner, plus a
`legacy` flag, so `AllowsMessage`, `authorizeJoin` and every other reader of
the role compare against two values, as today. `ClientKind`, and so every
stored and displayed principal kind, is unchanged.

```
format RunnerAuth:
    key :[32]u8          # Ed25519 public key
    sig :[64]u8          # Ed25519 over "agent-harness runner-auth v1" || 0x00 || transcript
    hello :RunnerHello
```

`transcript` is the objproto handshake transcript, the same bytes the binder
and the server's signature cover. A runner sends `binder_len = 0`.

Server, for `role=runner`, in `pskGate.Check` order:

1. Verify `sig` against `key`. Failure → `bad_psk` (a wrong proof, fatal, as
   today).
2. Look `key` up in the enrollment store. Unknown → `not_enrolled`, and a WARN
   naming the key, the hello's hostname and the connection.
3. The existing identity checks (zero `runner_id` → `no_identity`).

`runner_legacy` and `client_legacy` are accepted as today (no proof for a
runner, the ticket value for an agent) until the follow-up change. The server
logs each at INFO with hostname and connection; the runner's registry entry and
the connection's row are marked legacy (Operator surfaces).

`not_enrolled = 6` is appended to `PskAuthStatus`, signed like every response.
`PskRejectedError.Retryable` returns true for `no_identity` and `not_enrolled`.
On its first `not_enrolled` the runner logs one WARN naming its public key and
the command that enrolls it; later attempts log through `PersistLoop` as usual.

Both runner modes use this one path: dial mode and listen mode both reach
`sendRunnerMergedHandshake` through `driveAfterConn` (`runner/connect.go`,
`runner/listen.go`), so `server dial-runner` (direct or `--via`) needs no
separate handling. A new runner always sends `role=runner`, and every new
client sends `role=client`; neither has a legacy mode.

### Identity binding (D12)

`RunnerEntry` records the key its connection authenticated with (none for a
legacy runner). `Registry.Add` keeps today's takeover for a connection with the
same key (a runner's own reconnect) and refuses one with a different key: the
new connection is closed and the refusal is logged at WARN with both key
prefixes. The gate has already answered `ok` by then, so the refused runner
sees a plain drop and retries; no status is added for it. Two runner processes
only collide on a `runner_id` (128 random bits per process) if one copied the
other's, which is outside the accident model. A legacy entry can be taken over
as today. `RunnerHello.runner_id` stays per process (`runner/identity.go`), so a
restart is a new identity, as today.

### The enrollment store

- `<data-dir>/runner-keys.json`, an array of
  `{key (hex), label, enrolled_at (RFC 3339), enrolled_by}`. `enrolled_by` is
  `operator` or `task:<id>`.
- Read at startup; a file that does not parse is a startup error. Written by
  temp file + rename, mode 0600.
- With no `--data-dir` it is held in memory and the server WARNs at startup
  that enrollments will not survive a restart. `server.Config` carries the path
  and an initial key list, which tests use to pre-enroll.
- Not in the WAL: nothing in a task record refers to it, so no `.bgn` format
  is persisted and there is no disk axis.

### Enrolling and withdrawing

Three `TaskControlKind`s, each gated by `runner_enroll` in
`server/capabilities.go`, exposed as a new `runner` verb family (operator,
2026-10-27). Not under `server` beside `server dial-runner`: that verb makes the
server open a connection, while these three manage one set of keys, and a
family of their own keeps them together in `help` and in the TUI and WebUI
command inputs, with room for later key operations:

| Verb | Effect |
| --- | --- |
| `runner enroll <pubkey-hex> [--label L]` | Adds the key. An already enrolled key answers ok and keeps its record; `--label` replaces the label |
| `runner keys` | Lists key (first 16 hex), label, enrolled_at, enrolled_by, and the hostnames of runners currently registered with it; then the registered legacy runners, by hostname |
| `runner unenroll <key-prefix> [--force]` | Removes the key (a unique prefix of at least 8 hex). While any runner is registered with the key, it refuses and lists those runners unless `--force` is given; with `--force`, every live runner connection authenticated with the key is closed and its tasks end as on any runner death. The answer prints the removed key in full and the `runner enroll` command that restores it |

The unenroll guard and the printed undo exist because one key serves every
slot of a machine by default (D9): removing the only key in use cuts off the
whole machine, including any agent that would re-enroll it from there, and
`runner keys` shows only a prefix. Hygiene (D4) and rotation (step 4) only
remove keys no runner uses, so neither meets the guard (operator,
2026-10-27).

`runner_enroll = 0x800000`; `all` becomes `0xffffff`. Audit tier strong, scope
none, like `runner_admin` (`cli/verb/caps.go` `capAuditTable`). An operator
connection holds it (operators read as `all`). A task mask persisted as the old
`all` literal (`0x7fffff`) does not gain it; `caps set` grants it.

An agent's procedure for a new runner is `bin/agent-runner --print-pubkey`,
`harness-cli runner enroll <key> --label <host>`, then `scripts/runner.sh
up` (or `runner-autostart.py register`) — written into
`.claude/commands/runner-up.md` and `runner-autostart.py`'s docstring. A runner
started before its key is enrolled retries and connects once it is.

### The agent handshake: a ticket-keyed binder

- Under `role=client`, `kind=agent` carries `AgentInfo` without `auth_ticket`
  and sends `binder = ComputePSKBinder(ticket, transcript)`. Under
  `role=client_legacy` it carries `AgentInfoLegacy`, ticket included, as today.
- The server's `binderKey` for `kind=agent` is
  `Board.Registry().Ticket(runner_id, task_id)` (`agentboard/registry.go`).
  No ticket for the pair → `bad_ticket`, as an unknown pair is today. The
  constant-time binder compare replaces `Validate`'s ticket compare.
  `client_legacy` agents are validated as today until the follow-up change.
- Whether a task's agent is served by a legacy or an enrolled runner does not
  matter: the ticket comes from the env either way.
- Everything that hands a task's ticket to a process keeps doing so through
  the env (`HARNESS_AUTH_TICKET`, `exec`'s lookup); only what goes on the wire
  changes. `HeldTask.ticket` (runner → server) is unchanged.
- The negotiated-proxy path (`HARNESS_PROXY_VIA_RUNNER`) is unaffected: the
  binder covers the same transcript the PSK binder covered.

### The data plane: a grant-keyed binder

- Under `role=client`, `kind=data_plane` carries `DataPlaneInfo` without
  `grant_id`; the client sends `task_id` and `binder =
  ComputePSKBinder(grant_id, transcript)`.
- The runner tries each live grant it holds for that task (`grantStore`,
  `runner/dataplane_grants.go`) and accepts the one whose binder matches; it
  then applies that grant's existing kind / direction / TTL checks. No match →
  `bad_ticket`. A task has a handful of live grants at most, so the trial is
  bounded by what the store already holds.
- A new runner accepts a `client_legacy` data-plane hello as today until the
  follow-up change. A new client's file transfer to a runner that has not been
  restarted yet fails (an old runner decoding `AuthRole` 2 fails and refuses)
  until that runner is restarted. The migration does not need file transfer.
- A legacy data-plane hello comes only from an old client binary, and that
  binary's control connection to the server is a `client_legacy` one, so it is
  visible as a legacy connection row while it is in use.
- `DataPlaneGrant` itself is unchanged: it reaches the client in the server's
  response and the runner in `AuthorizeDataPlane`.

### Removing the connect PSK

- Server: `--psk`, `--psk-file`, `HARNESS_PSK` and `server.Config.PSK` go. The
  binder key is the operator PSK for operator kinds, the ticket for `agent`,
  none for runners. `--dangerously-permit-no-operator-psk` now means operator
  surfaces prove nothing (it used to fall back to the connect PSK).
- Runner: `--psk`, `--psk-file`, `runner.Config.PSK` and
  `AgentEnvSpec.PSK` / the `HARNESS_PSK` injection go.
- CLI: `cli.GetPSK` (`HARNESS_PSK`, `HARNESS_PSK_FILE`) goes. The WASM
  `GetPSK` reads `#psk=`, which is already the operator secret; it is renamed
  `GetOperatorPSK` so no symbol is left named for the removed secret. The URL
  fragment keeps its name.
- Unaffected: the ssh gateway's own authentication (`cli/sshgw/auth.go` only
  mentions `--psk-file` in a comment), the server's signature, the operator PSK.

## Operator surfaces

The three verbs above on CLI, TUI command line and WebUI command input through
the verb table. `runner keys` output is the hygiene view. Two display
additions, on CLI, TUI and WebUI alike:

- runner rows (`ls`, the TUI runner list, the WebUI runner list) show the
  key's label, or `legacy`;
- connection rows (`conns`, the TUI connections view, the WebUI Connections
  tab) mark a connection that authenticated with a `_legacy` role.

Together they are the follow-up change's trigger, readable from any surface.
The plan walks `surface-parity-checklist` for all of it.

## Version skew and the migration (D5)

| Pair | Result |
| --- | --- |
| old runner × new server | `runner_legacy`, accepted |
| old client (any kind) × new server | `client_legacy`, accepted |
| new runner, enrolled × new server | accepted |
| new runner, not enrolled × new server | `not_enrolled`, retried |
| new runner × old server | unknown `AuthRole` → decode fails → `no_identity`, retried |
| new client (any kind) × old server | unknown `AuthRole` → `no_identity`; the call errors |
| new client × old runner, data plane | refused; works once the runner restarts |

The fifth and sixth rows are why the order matters: an agent whose host has
the new `harness-cli` while the server is still old has no harness access, and
that includes the agent carrying out the migration. So the server host is
built and restarted before any other host is built. The server is restarted by
the operator by hand, as every server restart is today (operator,
2026-10-27); every later step goes through the harness:

1. The operator builds the server host and restarts the server on the new
   binary (a deliberate restart: held tasks are re-adopted by the old runners,
   which reconnect as `runner_legacy`; old clients reconnect as
   `client_legacy`). Remove `--psk` / `--psk-file` from the server's command
   line first if it carries them: the new binary no longer defines them and
   refuses to start, and `scripts/restart.py` replays the running argv.
2. Build every other host. From here every agent's `harness-cli` is new and
   speaks `client`, whichever runner it sits on.
3. Grant `runner_enroll` to an agent (`caps set`). For each runner host it runs
   `agent-runner --print-pubkey` there (in a task on that host, or `exec`) and
   `runner enroll`.
4. Restart the runners host by host (`build_and_restart_all.py` on Linux; on
   Windows from a task on that host, or its autostart entry). Each restarted
   runner connects with its enrolled key. Restarting a runner ends the tasks
   running on it, as any runner restart does, so the agent driving the
   migration should run on the host restarted last, or be respawned.
5. When no runner row and no connection row reads `legacy`, land the
   follow-up change.

## The follow-up change

Deletes the `client_legacy` and `runner_legacy` roles, the formats only they
carry (`ClientHelloLegacy`, `AgentInfoLegacy`, `DataPlaneInfoLegacy`), every
code path that accepts them, and the legacy marking on runner and connection
rows. After it, P1 is closed. Trigger: no runner row and no connection row
reads `legacy` on any surface. Nothing persisted names a role, so it needs no
disk handling, and nothing left speaks the legacy roles, so it needs no skew
handling.

## Testing

- **Negative controls first**, through a real server: (1) a runner with no key
  registers on today's code; (2) an agent connection presenting another task's
  ticket bytes is accepted; (3) a data-plane hello with a grant id from another
  connection is accepted. After the change: (1) a key that is not enrolled is
  refused `not_enrolled`; (2) and (3) cannot be expressed under `role=client` —
  no value is sent, and a binder over another transcript is refused.
- Legacy acceptance: a `runner_legacy` hello and a `client_legacy` agent hello
  are accepted, and their runner / connection rows read `legacy`; a
  `client_legacy` data-plane hello is accepted by a new runner. A task created
  by a `client_legacy` agent and one created by a `client` agent record the
  same `OriginKind`.
- Runner key: generated when absent, reused when present, a corrupt file is a
  startup error, `--print-pubkey` matches what the handshake presents.
  Rotation: after `--rotate-key` a running runner stays connected on the old
  key, a restarted one connects on the new key once it is enrolled, and
  `.prev` holds the old key. Tests point `--runner-key-file` (or the config
  field) at a temp dir, never at the real `~/.config/harness/runner-key`.
- Enrollment: enroll / list / unenroll through the verbs, each refused without
  `runner_enroll`; unenroll of a key in use is refused without `--force` and
  names the runners, and with `--force` closes them; the printed undo command,
  run as printed, brings those runners back on their next retry; the store
  survives a restart;
  a corrupt store is a startup error; a runner refused `not_enrolled` connects
  on a later attempt once enrolled.
- Identity binding: same-key reconnect takes over; a different key claiming a
  live identity is refused.
- `Retryable()`: true for `no_identity` and `not_enrolled`, through every
  `PskRejectedError` creation site (Pitfall 10's third-site lesson).
- `scripts/wire-skew-check.sh` (a `.bgn` changed): new runner × old server is
  recoverable. And the D5 direction, which the script does not cover: an old
  runner binary keeps working against the new server.
- Live, on a dummy harness: an old-binary runner registers as legacy; a new
  runner is refused, prints its key, is enrolled with `runner enroll`
  and connects; an agent's `harness-cli ls` and `file push`/`pull` work against
  the new runner.

## Docs to update

README (Quick start's `--psk` lines, the PSK paragraph, Capabilities),
`.claude/commands/runner-up.md`, the dummy-harness skill, `scripts/server.py`'s
usage, `runner-autostart.py`'s docstring, and the scripts that pass a PSK
(`dummy-harness.py`, `netem-lab`, `e2e-oneshot-progress.sh`, `restart.py`,
`test_daemon.py`, `wire-skew-check.sh`). `scripts/kvm-runner/`: a guest's key
lives in the guest's home, so reverting the guest to a snapshot taken before
the key existed regenerates it and the new key needs enrolling; its README
says to create and enroll the key before taking the snapshot.

## Out of scope

- A boundary against a hostile agent (Threat model).
- A pending-approval list of refused keys; the refusal is logged on both ends
  with the key, which is what enrolling needs.
- Per-slot keys by default (D9); `--runner-key-file` is the opt-in.

## When this lands

In `docs/backlog.md`: in "Any connect-PSK holder can register as a runner",
note that the follow-up change closes it, and delete the entry when that
change lands; in "Multi-tenancy", drop "The connect PSK, which every agent
holds as `HARNESS_PSK`" from the ways out of the tree.

## Amendment 2026-10-27 (implementation)

Where the implementation (plan `2026-10-27-runner-enrollment.md`) departs from
the text above, and why. Each is the implementer's call, recorded so a reader
verifying against this spec is not misled.

- **The enrollment store reaches the server as `server.Config.RunnerKeys`, a
  loaded `*RunnerKeyStore`**, not as a path plus an initial key list.
  `server.New` returns no error, so a corrupt `runner-keys.json` could not stop
  startup from inside it; `harness-server` loads the store and exits on a bad
  file. Tests build a store with `LoadRunnerKeyStore("", initial)`.
- **Malformed keys are refused by the client decode and the server
  (`invalid_key`), not by the verb declaration.** The verb table's own guards
  synthesise arbitrary positionals to check every flag is wired, so a hex
  check there would fail them.
- **A refused takeover (D12) ends the connection with a wire close, then the
  local close** (`Server.kickConn`, the shutdown path's order), and
  `runner unenroll --force` uses the same. A local close alone reached the
  runner only at its dead-peer GC, about 70 s, which is not the plain drop and
  retry D12 describes. Pinned by `TestRefusedTakeoverIsNoticedPromptly`.
- **Operator kinds also move to `AuthRole.client`** in the same change as
  agents: one hello builder serves both, and the gate keys an operator kind's
  binder by the operator PSK under either client role.
- **Testing, correction:** `scripts/wire-skew-check.sh` phase 3 already runs an
  OLD runner against the NEW server, so the D5 direction is covered by it; the
  script now seeds the new runner's key into the data dir both servers start
  from, which is what makes phase 2's self-heal a plain reconnect.
- **Migration step 1** gained its precondition: the server's own command line
  must not carry `--psk` / `--psk-file`.
- **`agent-runner` keeps `--psk` / `--psk-file`, ignored, with a startup WARN**
  (amends D13 for the runner only). A slot's command line is frozen where it
  was registered — a systemd unit, a Task Scheduler entry, the argv
  `restart.py` replays — so a runner that refused an unknown flag would not
  come back on a remote host, and D5 has no hands on that host. The server's
  flags stay removed: its restart is the operator's own step. The follow-up
  change removes the runner's two.
- **A listen-mode runner refused `not_enrolled` does not retry.** "Needs no
  separate handling" above holds for the proof, not for the retry: the server
  dialed, and nothing redials. `server dial-runner` answers `ok` once ECDH and
  the greeting are done, before the gate, so the refusal shows only as an
  absent `ls` row. After an enroll, or an undo, the operator dials it again.
  Documented in the README; no code change.
- **Skew row, added:** old client × new server authenticates as
  `client_legacy`, but cannot decode `ls`, because `RunnerInfo` grew. No
  compatibility layer for it: it is not the path the harness upgrades through
  — migration step 2 rebuilds every client, and nothing between steps 1 and 2
  needs `ls`.
- **The runner key is created by link, not by create-exclusive.** Every slot
  of a machine reads one file and a fleet restart starts them together; a
  slot that lost the create race could read the winner's empty file and exit
  "does not parse". The key is written whole to a temp file and linked into
  place; the loser reads the winner's file.
- **The undo line shell-quotes the label**, so it runs as printed whatever the
  label holds.
