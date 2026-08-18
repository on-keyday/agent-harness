# harness-cli capabilities — server-enforced object-capability per task

Date: 2026-06-20
Status: design — awaiting review

## Problem

`harness-cli` is the agent's control surface for the whole harness: agentboard
messaging, worker-session lifecycle (`session new`/`kill`/`attach`), one-shot
`submit`, worktree file transfer (`file push`/`pull`/`delete`/`ls`), port
forwarding (`forward -L`/`-R`), operator `notify`, server-driven runner dialing
(`server dial-runner`), and discovery (`ls`/`logs`/`watch`/`agent topics`).

Today every connected agent can invoke **all** of it. The trust model
(`.claude/skills/harness-cli/SKILL.md` "Trust model") is "broker membership =
ambient auth; default-trust within the broker" — correct for a single operator,
but it leaves one concrete hole:

**The sandbox is bypassable through harness-cli.** `scripts/sandbox` confines an
agent's local fs/proc, but the sandboxed agent still has `harness-cli` on `PATH`
and `HARNESS_*` in its env. Through the server it can spawn an *unconfined*
session, `file pull`/`push` another task's worktree, open a port-forward pivot,
attach to another session's PTY, or make the server dial an arbitrary network
endpoint — all of which escape the sandbox's intended containment. The sandbox
governs the **local** plane; nothing governs the **control** plane.

The prerequisite to fix this already landed: per-connection **verified principal
identity** (`docs/superpowers/specs/2026-06-18-task-control-principal-identity-design.md`).
The server authenticates `{RunnerID, TaskID, AuthTicket}` via
`agentboard.Registry.Validate` and resolves any connection to its principal
(`server/task_handler.go:156` `lookupPrincipal`). This design attaches an
**authority** to that identity.

## Decision

Introduce a **capability set** (a `uint32` bitmask) bound to each task's auth
ticket, **enforced exclusively at the server**. The ticket *is* the capability
bundle: an agent may read `HARNESS_AUTH_TICKET` from its env, but that only lets
it authenticate as the principal the ticket already names — it cannot grant
itself authority it was not given. Capability checks live in the server-side RPC
handlers; the agent/CLI side performs **no** checks (a check on the LLM side is
self-declared text and meaningless — same rationale the trust model already
states).

Authority flows by **spawn-time attenuation**: when task A spawns task B, the
server computes `caps_B = caps_A ∩ requested`, where `caps_A` is A's
*authenticated* capability set (from `lookupPrincipal`) and `requested` is what
A asked to grant B. Authority is therefore **monotonically non-increasing** down
the spawn tree, and a task can never raise its own.

### Two-layer containment

The capability model governs **exactly** the set of server-mediated operations.
It is the control-plane half of a two-layer model:

| Layer | Governs | Contains |
|---|---|---|
| **OS sandbox** (`scripts/sandbox`) | the agent process's local fs / proc / network | commands that **never contact the server** (`skill`, the fs-deletion part of `prune-local`) |
| **Capabilities** (server-enforced) | every `harness-cli` path that opens a server connection (Dial + RPC) | everything else |

**Invariant: there is no third bucket.** Every `harness-cli` path that reaches
the server resolves to a capability decision — gated by a cap, or explicitly
"always-allowed data-plane" with a stated rationale. "Out of model" means
*literally* "never contacts the server" and is the sandbox's domain. A
server-touching command that is left ungated by omission is a bug against this
invariant. Together the two layers close the bypass: local actions are bounded
by the sandbox, remote actions by capabilities; no command escapes both.

## Scope

- Define the capability set, its server-side enforcement points, the
  attenuation/inheritance rules, persistence across resume, and the default
  policy for omitted grants.
- Wire the sandbox launch path to request a confined capability set.

### Non-goals

- **Parameterized / fine-grained capabilities** (send only to topic-prefix X,
  spawn quota of N, worktree-scoped file paths). Granularity is coarse: one bool
  per operation class. Exception: `INFO_GLOBAL` toggles a lineage *scope*, not a
  parameter (see below).
- **Lineage-scoped `agent send`** ("may only message parent/children"). Not
  enforceable today because topic↔task binding is convention, not protocol, and
  free-form broadcast topics (`harness.hello`) would leak it anyway. Designed as
  a future extension (see "Deferred").
- **Cryptographic / macaroon-style tokens** with offline attenuation. The
  registry-intersection model needs no crypto and matches the existing
  ticket-validation design. Out of scope and out of keeping with the toy-scope
  protocol posture.
- **Operator policy / central config** for caps. Authority is set by spawn-time
  attenuation only; the operator is the trusted root (full caps), not a policy
  author.
- **Multi-tenant authority.** Rule 3 territory; revisit if the broker stops
  being single-user.

## The capability set (11)

Coarse bool per operation class. The governing split rule:

> **Split one operation class into multiple caps only when (a) the risk class
> (confidentiality / integrity / availability / topology) genuinely differs AND
> (b) the wire already carries the distinction (zero schema invention).**
> Otherwise keep it a single cap.

`FILE_*` and `FORWARD_*` meet both bars (`FileTransferDirection` and
`PortForwardDirection` are already on the wire); the rule also resists arbitrary
future expansion.

| cap | gated `harness-cli` | server op | risk class |
|---|---|---|---|
| `SPAWN` | `session new` / `interactive` / `submit` | OpenInteractive / Submit | unconfined task proliferation |
| `CANCEL` | `session kill` / `cancel` | CancelTask | stop other tasks |
| `EXEC_ATTACH` | `session attach` | AttachSession (TaskControl) | drive another session's PTY |
| `FILE_READ` | `file pull` | OpenFileTransfer (Pull/DirPull) | confidentiality (exfil) |
| `FILE_WRITE` | `file push` / `file delete` | OpenFileTransfer (Push/Delete/Dir*) | integrity + availability (tamper/plant/destroy) |
| `FORWARD_LOCAL` | `forward -L` | OpenPortForward (Local) | egress / pivot into runner-reachable hosts |
| `FORWARD_REMOTE` | `forward -R` | OpenPortForward (Remote) | ingress listener on the runner host |
| `NOTIFY` | `notify` | operator notify | operator spam / misinformation |
| `PRUNE` | `prune` | server-side forget of terminal task records | permanent deletion of task records |
| `RUNNER_ADMIN` | `server dial-runner` | TaskControl DialRunner | topology/infra: make the server ECDH-dial an arbitrary `target` ConnectionID (server-side egress / relay-chain manipulation) |
| `INFO_GLOBAL` | see "Visibility scope" | — | metadata confidentiality; full-board view |

### `file ls` is the file-access floor

`file ls` (ListFiles) is permitted by holding **either** `FILE_READ` **or**
`FILE_WRITE`. Listing a target worktree's filenames is the minimal introspection
both a reader (find the file to pull) and a writer (see the destination before
push, avoid clobbering) legitimately need. It leaks filenames only — contents
remain behind `FILE_READ`. A task with no file cap cannot `ls`.

### Visibility scope — `INFO_GLOBAL`

Read/discovery is a *scope*, not all-or-nothing. Lineage data already exists
(`CreatorTaskID`, `server/taskstore.go:35`), so:

- **With `INFO_GLOBAL`** (operator default): full view — `ls` / `session ls`
  list all tasks; `logs` / `watch` / `notify-watch` work against any task;
  `agent topics` enumerates the whole board.
- **Without it** (confined default): the server filters to the caller's own task
  **plus its descendant subtree** (computed from the `CreatorTaskID` chain).
  `ls`/`session ls` show only that subtree; `logs`/`watch` are allowed iff the
  target task-id is within it; `agent topics` returns an empty list (no topics
  visible to you — consistent with the scope model).

This gives the "filter `ls` to what's mine" behavior as the *secure default*
rather than a separate feature.

**The list surfaces add one upward hop.** `ls` / `session ls` also carry the
caller's **direct creator**, as a redacted row: id, status, kind, timestamps,
attach state and busy/idle survive; `repo_path`, `worktree_dir`, `prompt`,
`error_message`, `agent_profile` and `assigned_to` are stripped. A child
legitimately needs to know whether the parent driving it is still alive, and it
can already read that parent's id from the ungated `whoami`
(`WhoAmIResponse.CreatorTaskId`) — what it must not gain is where the parent
runs or what it was told to do. `capabilities` and `creator_task_id` are NOT
stripped: attenuation already guarantees `caps_parent ⊇ caps_self`, and zeroing
them would render as "caps: none" / "operator-created", which are false claims
rather than absent fields.

The hop is **list-only and exactly one level**. Grandparents and siblings stay
invisible, and every access-scoped operation — `logs`, `conns`, port-forward
list/kill — keeps the strict self+descendants set (`visibleToCaller`); only
`handleList` consults the widened `listVisibleToCaller`. Opening the access
scope upward would hand a confined task the full log stream of a task that, by
construction, holds a superset of its own capabilities.

**`agent subscribe` eavesdrop gating is DEFERRED** (not in scope). Restricting a
confined task to only its "own" topic would require the server to know topic
ownership, which today is purely a client convention
(`chat.<first-8-hex-of-tid>`, `cli/agent/util.go`) — baking it into the server
violates protocol-explicit and stays leaky via free-form broadcast topics
(`harness.hello`, `build.events`). Honest eavesdrop closure needs server-owned
identity-bound topics; it is deferred to the same future work as lineage-scoped
send (see "Deferred"). In the single-user model the eavesdrop risk is low (all
peers are the user's own agents; the real exfil boundaries — network egress,
cross-worktree fs — are already gated by `FORWARD_*`/`FILE_*`).

### Always-allowed data-plane (no cap)

`agent send`, `agent inbox`, `agent subscribe --self` / `unsubscribe`,
`agent subscriptions`, and the scripting escape-hatches `agent wait` /
`agent dispatch`.

Rationale: the agentboard is the *only* sanctioned agent-to-agent channel and
`send` has **no direct side effect** — it hands text to a peer LLM, and the
trust model already terminates destructive/permanent/secret-exposing actions at
the *user*, not at peer-to-peer messages. `wait`/`dispatch` are blocking
variants of `send`+`inbox` and grant no authority beyond them (the "do not call
from an agent turn" rule is operational, orthogonal to caps). The board read
concern that IS gated now is full-board `topics` enumeration (under
`INFO_GLOBAL`); the other — eavesdropping by subscribing to a non-self topic —
is deferred (see "Visibility scope") because it needs server-owned topic
ownership, not a baked-in client convention.

### Local-only (sandbox domain, outside the cap model)

`skill` (prints embedded markdown; no server connection) and the fs-deletion
part of `prune-local` (deletes local worktrees; its server call is only a task-
status *read* that follows the `INFO_GLOBAL` scope). These are governed by the
OS sandbox, per the two-layer invariant.

## Authority flow (grant, attenuation, inheritance)

### Grant model — spawn-time attenuation

The server computes, at task creation, `caps_child = caps_creator ∩ requested`:

- `caps_creator` is the authenticated cap set of the principal on the spawning
  connection, via `lookupPrincipal` (already resolved for `CreatorTaskID`
  attribution at `server/task_handler.go:156`).
- `requested` is a new `RequestedCaps uint32` field carried on the spawn request
  bodies.
- The intersection is enforced server-side, so a confined task cannot request
  *more* than it holds, and cannot self-escalate.

### Top-level / operator-initiated tasks

A task whose creator is zero (created directly by the operator via CLI / TUI /
WebUI / runner, not by an agent) is granted the **full** cap set. The operator
is the trusted root; attenuation applies only to agent-spawned children.

### Omitted-request default — policy C (inherit-all + sandbox confines)

When the operator/agent does not specify caps, the CLI/spawn helper sets
`requested_caps = Capability.all` before encoding (see "Omitted-grant on the
wire"), so `caps_child = caps_creator ∩ all = caps_creator` — the child
**inherits the parent's full set**. Existing spawn flows are unchanged (zero
friction). (A literal `Capability.none`/zero on the wire is *not* this default;
it is an explicit "grant nothing" request.)

Confinement is **explicit at the point where confinement is decided**: the
sandbox launch path (`scripts/sandbox`, and any caller that spawns a sandboxed
worker) passes a restricted `RequestedCaps`. The motivating hole (a sandboxed
agent reaching the control plane) is thus closed exactly where the "sandbox
this" decision is made, while ordinary collaborative spawns keep inheriting.

This is deliberately **not** secure-by-default (a `deny-all` omitted default,
policy B, was rejected): in a single-user dogfood tool, breaking every existing
worker spawn to force explicit grants is high-friction with low marginal payoff.
The confinement travels with the sandbox decision, not a global default flip.

**Confined preset** (suggested for the sandbox path): drop
`SPAWN`, `FILE_READ`, `FILE_WRITE`, `FORWARD_LOCAL`, `FORWARD_REMOTE`,
`EXEC_ATTACH`, `RUNNER_ADMIN`, `PRUNE`, `NOTIFY`, `INFO_GLOBAL` — leaving the
always-allowed data-plane (self-topic send/recv) so the worker can still receive
its assignment and reply to its parent, and nothing else. Tune per use case.

### Resume

Capabilities are **persisted with the task record** (the task store / WAL,
alongside `OriginKind` / `CreatorTaskID`) and **restored on resume**, not
re-inherited from the parent.

- The parent may be dead at resume time; re-deriving from it would be
  non-deterministic or impossible.
- A new auth ticket is generated on resume (`server/server.go:826`); the
  restored cap set is registered against the new ticket.
- Monotonic: resume neither raises nor lowers caps — the persisted value is
  reinstated verbatim. No escalation path is introduced.

## Enforcement

1. **Storage.** The ticket registry entry (the auth-ticket side validated by
   `agentboard.Registry.Validate`, `server/agent_handler.go:108`) gains a
   `Capabilities uint32`. The task store gains a persisted `Capabilities` field
   for resume.
2. **Registration.** The ticket-generation/registration path
   (`server/server.go:826` and the dispatch path in `server/dispatch.go`)
   accepts and records the computed cap set.
3. **Check.** Each gated handler, after resolving the principal, checks
   `caps.Has(REQUIRED)` before acting and returns the existing per-RPC
   `*_Denied`/permission status otherwise. Handlers to gate:
   `handleOpenInteractive` / `handleSubmit` (SPAWN), `CancelTask` (CANCEL),
   `AttachSession` (EXEC_ATTACH), `handleOpenFileTransfer` keyed on
   `req.Direction` (FILE_READ vs FILE_WRITE), `handleListFiles`
   (FILE_READ∨FILE_WRITE), `handleOpenPortForward` keyed on `req.Direction`
   (FORWARD_LOCAL vs FORWARD_REMOTE), notify (NOTIFY), prune (PRUNE),
   `DialRunnerHandler` (RUNNER_ADMIN), and the discovery/visibility paths
   (INFO_GLOBAL + subtree filter).
4. **No client-side check.** `harness-cli` and `cli/*` issue requests as before;
   denial is authoritative only from the server.

## Schema changes (single source of truth)

Defined in **one** place in `runner/protocol/message.bgn` — no "also add this in
a later task".

### Capability bits — an explicit-value `enum Capability : u32`

brgen supports explicit-value enums with a chosen backing type (`AppKind` uses
`name = 0x40` over `:u8`, `appwire/app.bgn:10`). Capabilities are an
`enum Capability : u32` with power-of-two values:

```
enum Capability:
    :u32
    none           = 0x000   # empty set — "grant nothing" (also the zero value)
    spawn          = 0x001
    cancel         = 0x002
    exec_attach    = 0x004
    file_read      = 0x008
    file_write     = 0x010
    forward_local  = 0x020
    forward_remote = 0x040
    notify         = 0x080
    prune          = 0x100
    runner_admin   = 0x200
    info_global    = 0x400
    all            = 0x7ff   # OR of all defined bits (spawn..info_global);
                             # keep in sync when adding a cap
```

`none` (0x0) is the zero value and the literal "grant nothing" request; `all`
(0x7ff) is the full set used as the inherit-all default. (If brgen accepts
member-OR expressions in enum values, `all = spawn | cancel | … | info_global`
auto-tracks new bits and is preferable to the literal — author's call.)

**The bitmask fields are typed `:Capability`.** brgen does **not** validate enum
membership on encode/decode (validation is opt-in via an explicit
`field.is_defined == true` assertion), so a field typed `:Capability` round-trips
an arbitrary OR-combination of bits unchanged. The enum gives a named Go type and
named bit constants while the wire stays a plain `u32`.

> **Do NOT add an `is_defined` / membership assertion to any `Capability`
> field.** These fields are flag-sets (OR-combinations), not single members; a
> membership assertion would reject every multi-bit value. This is a bitmask
> deliberately reusing the enum mechanism for named constants only.

### Fields that carry it

- Spawn request bodies — `SubmitRequest`, `OpenInteractiveRequest`,
  `AssignTaskBody` — gain `requested_caps :Capability`.
- The auth-ticket registry entry and the persisted task record gain
  `capabilities :Capability`.

### Omitted-grant on the wire (no magic absence)

The zero value of `requested_caps` is `Capability.none` and means **grant
nothing** — a legitimate maximally-confined request (the data-plane needs no
cap). So zero **cannot** double as the inherit-all default. Instead the
**CLI/spawn helper applies the inherit-all default before encoding**: when the
operator/agent did not specify caps, it sets `requested_caps = Capability.all`
(0x7ff) explicitly, and the server computes `caps_creator ∩ all = caps_creator`.
The wire therefore always carries an explicit mask (consistent with "schema
describes every byte"), and the server logic is pure intersection with no
presence/sentinel special-casing. Because `all` is exactly the defined bits,
there are no undefined high bits to reason about.

## Deferred (designed, not built): lineage-scoped `agent send`

To enforce "a confined task may `send` only to its parent/children", the server
must resolve a topic to an owning task and check spawn-tree adjacency
(`X.CreatorTaskID == me || me.CreatorTaskID == X`). That requires
**protocolizing per-task inbox topics** — a server-owned, identity-bound topic
namespace (e.g. `task.<full-id>`) that only the owning task may subscribe to and
whose sends the server can authorize by lineage. Free-form broadcast topics
(`harness.hello`, `build.events`) would remain ungated, so this is partial
containment, not total. Marginal security payoff under the single-user model;
build it when topics gain protocol-level owner binding (Rule 3 territory), not
now.

## Testing

- **Attenuation is monotonic**: `caps_child = caps_parent ∩ requested`; a
  request exceeding the parent never widens the child; A→B→C chain yields
  `caps_C ⊆ caps_B ⊆ caps_A`.
- **Each gated handler denies on missing cap** and permits on present cap, for
  all 11 caps including the two `FILE_*` and two `FORWARD_*` direction splits.
- **`file ls` floor**: permitted with `FILE_READ` only, with `FILE_WRITE` only,
  denied with neither.
- **Confined preset**: a task spawned with the sandbox preset is denied
  SPAWN/FILE/FORWARD/EXEC/RUNNER_ADMIN but can still self-topic `send`/`inbox`.
- **`INFO_GLOBAL` scope**: without it, `ls` returns only self+subtree and
  `logs`/`watch` against an out-of-subtree task is denied; with it, full view.
- **Parent hop is list-only and one level**: without `INFO_GLOBAL`, `ls`
  carries the direct creator with its location/content fields stripped and its
  caps intact; the grandparent and unrelated roots stay absent; `logs` against
  that same parent still answers not-found.
- **Top-level full caps**: an operator-created (creator=zero) task holds all
  caps; existing flows unchanged (regression).
- **Resume**: a resumed task's caps equal the persisted value, independent of
  whether the parent is alive; no raise/lower.
- **Two-layer invariant**: a static check/test that every server-touching
  `harness-cli` subcommand maps to a cap decision (no ungated server path).

## Amendment 2026-08-18 — the omitted-request default is now `none` (policy B)

"Omitted-request default — policy C (inherit-all + sandbox confines)" above is
**superseded**. An omitted `--caps` now parses to `Capability.none`, so
`caps_child = caps_creator ∩ none = none`: a spawn grants nothing unless it
names what to grant. Everything else in this document — attenuation,
operator-as-root, persistence, resume semantics — is unchanged.

**Why the rejection of policy B did not survive.** It rested on "breaking every
existing worker spawn to force explicit grants is high-friction with low
marginal payoff". Live re-grant did not exist when that was written: `caps set`
(server `09e1094`, CLI/TUI/WebUI `908f391`) landed 2026-08-13, ~2 months after
this spec. The friction it priced in was "re-spawn the task with the right
flags"; it is now "`caps set <id> --caps …` on the running task, no restart, on
any of the three surfaces". With the recovery cost that low, the asymmetry
flips: the cost of a forgotten `--caps` under policy C is a worker holding the
full control plane for its whole life, silently.

**What this does NOT change.** Policy C's other half still holds — confinement
is still decided at the spawn point, and the sandbox path still has to think
about caps. The difference is only which way an unstated `--caps` falls.

**Implementation is client-side, deliberately.** The default lives in
`cli.ParseCaps("")` (CLI), `cli.SessionOpts.Caps`'s zero value (programmatic
callers), `App.sessionCaps` (TUI) and `spawnCaps` (WebUI Compose) — not in the
server. A `requested_caps_present` bit was considered and rejected: presence
only earns its keep when the default differs from the zero value, which was
true of `Capability.all` (4095) and is not true of `none` (0). Unset and
"explicitly none" now denote the same request, so `SessionOpts.Caps` also
stopped being a pointer.

The residual this leaves is version skew: a client that predates the flip still
sends `Capability.all` for an unadorned spawn, and the sandbox bind-mounts
`harness-cli` as a frozen inode for the container's life
(`scripts/sandbox/README.md`). Bounded by `caps_child = caps_creator ∩
requested` — it can only matter under a creator that itself holds broad caps —
and correctable with `caps set --cascade`.
