# Task-scoped capabilities: a scope on every grant, and operator re-grant without a restart

Date: 2026-08-13

## Problem

### 1. A capability names a verb, never a target

`Task.Capabilities` (`server/taskstore.go:39`) is a bitmask of verbs. Nothing
in it says *which tasks* those verbs may be pointed at, and for most kinds the
server never asks. `visibleToCaller` (`server/capabilities.go:73`) — the
self+descendants BFS that is the only target-narrowing predicate the server
has — is consulted by exactly four things: `handleList`, `handleGetTaskLog`
(`server/task_handler.go:1517`), `handleListPortForwards`
(`server/port_forward_list.go:66`), and `KillPortForward`
(`server/task_handler.go:434`).

Everything else takes the target id straight off the wire:

| kind | gate today | consequence |
|---|---|---|
| `cancel` | `Capability_Cancel`, then `h.Tasks.Cancel(taskID)` (`server/task_handler.go:264`) | kill any task on the server by id |
| `attach_session` | `Capability_ExecAttach`; `handleAttachSession` (`server/task_handler.go:1178`) never receives `cid` | drive any interactive session's PTY |
| `open_file_transfer` | `FileRead`/`FileWrite`; `handleOpenFileTransfer` (`server/file_transfer.go:19`) never receives `cid` | read or overwrite any task's worktree |
| `list_files` | `FileRead`\|`FileWrite` | same |
| `git_query` | `FileRead` | read any task's diff, including terminal ones |
| `await_idle` | `ExecAttach` | probe any session's liveness |
| `open_port_forward` / `register_port_forward` | `ForwardLocal`/`ForwardRemote` | bind a forward against any task's runner |

A `--caps cancel` worker spawned to supervise one sibling can kill the
operator's own sessions. The blast radius of every granted bit is the whole
server, which makes attenuation much weaker than the `--caps` documentation
implies.

### 2. Resume is an unguarded takeover primitive

`handleSubmitResume` (`server/task_handler.go:656`) takes `origin` and
`callerCaps` but no `cid`, so it never asks whose task `resume_task_id` is. The
interactive path has the same shape (`server/task_handler.go:971`). With only
`Capability_Spawn`, a task can resume *any* terminal task in the store and
supply the prompt it runs with.

`ResumeCapsOverride` makes it worse than a takeover. Line 714 computes
`newCaps := intersectCaps(callerCaps, req.RequestedCaps)` and writes it onto
the target, so the target's capability set is *raised* to the caller's. Live
caps re-granting therefore already exists on the wire — just non-operator, and
only for tasks that happen to be terminal.

### 3. `prune`'s self-restriction is a written convention, not a gate

`PruneFn` (`server/server.go:201`) dispatches to `PruneTerminal(cutoff)` for
the bare form and `PruneByIDs(ids, force)` otherwise. Neither takes the caller.
The `supervising-workers` skill compensates in prose:

> Prune **only terminal** tasks … that **you** spawned … With **no** ids it
> forgets every terminal task older than `--before` … do **not** run that
> bare/age form on a shared server; it sweeps everyone's tasks.

An agent that ignores that paragraph erases the operator's task history, and
`--force` extends the same reach to Queued / Running / Detached tasks.

### 4. Changing a live task's caps means killing it

The only writer of `task_caps_changed` (`server/taskstore.go:296`) is
`TaskStore.Resume`, reached solely through `--resume --caps`. Resume requires a
terminal target (`ResumeErrNotTerminal`), so a running agent's caps cannot be
narrowed or widened at all without ending it first. This is not a limitation of
the enforcement model — `callerCaps` (`server/capabilities.go:53`) re-reads the
store on every RPC, so a store write already takes effect on the next request.
The missing piece is only an operator-reachable way to perform that write.

## Design

### 1. A scope value, orthogonal to the capability bitmask

Two independent axes rather than one enum, so "narrow below the subtree" and
"reach outside the subtree" are expressible at once:

```
# runner/protocol/message.bgn

enum ScopeBase:
    :u8
    # subtree is 0 deliberately. It is the default in three places that all
    # read a zero: a client that sends no scope, a WAL record written before
    # this change (the JSON fields are omitempty), and a zero-valued Go
    # struct. Making the default the zero value removes the "was it unset or
    # was it none?" question from every one of them.
    subtree = 0, "subtree"    # self + descendants (today's visibility rule)
    none    = 1, "none"       # no set beyond self
    global  = 2, "global"     # every task

# Ordering for clamping is by permissiveness (none < subtree < global), NOT by
# numeric value. minScopeBase ranks explicitly, and an unrecognised byte from a
# newer peer ranks as none — fail closed.

format TaskScope:
    base    :ScopeBase
    ids_len :u16
    ids     :[ids_len]TaskID
```

The effective target set of a task is

```
{self} ∪ baseSet(base) ∪ ids
```

`self` is unconditional: a task must be able to reach its own log, worktree and
session regardless of how narrowly it was scoped.

| written as | effective set | use |
|---|---|---|
| omitted | `{self} ∪ descendants` | default; identical to today's visibility rule |
| `--scope none` | `{self}` | strictest worker |
| `--scope ids:X,Y` | `{self, X, Y}` | agent that services two named tasks and nothing else |
| `--scope subtree+ids:X` | `{self} ∪ descendants ∪ {X}` | supervises its own children, plus one foreign task |
| `--scope global` | every task | today's *de facto* behaviour, now explicit |

No new `Capability` bit is added; `all` stays `0xfff`. "May act on anything" is
`--scope global`, not a twelfth verb.

A child never *inherits* a wider base. `--scope` omitted means `subtree`, not
"whatever the creator has" — an operator is `global`, so an inheriting default
would hand `global` to every task the operator spawns and reproduce exactly the
unscoped behaviour §1 is about. Widening past `subtree` is always explicit.

`ScopeBase.none` combined with `Capability_Spawn` yields a task that can create
children it cannot then supervise. That is expressible and left expressible —
if a spawner must be able to reach what it spawns, do not narrow its base below
`subtree`.

### 2. One resolution function, one choke point

```go
// scopeSet resolves the caller's effective TARGET set from its stored scope.
// It does NOT consult Capability_InfoGlobal — that bit widens what may be
// SEEN, and folding it in here would make it widen what may be DONE.
//   all=true  → operator (zero principal), or ScopeBase_Global.
//   otherwise → {self} ∪ baseSet ∪ scope.ids, keyed by task-id hex.
func (h *TaskHandler) scopeSet(connID string) (all bool, allowed map[string]bool)

// authorize is the single gate for every request that names a target task.
func (h *TaskHandler) authorize(connID string, want protocol.Capability, targetHex string) bool {
    if !hasCap(h.callerCaps(connID), want) {
        return false
    }
    all, allowed := h.scopeSet(connID)
    return all || allowed[targetHex]
}

// visibleToCaller keeps its name and signature: the action set, widened by
// info_global. ls, task log, the port-forward list and the conns filter call
// it as they do today and inherit the scope with no further change.
func (h *TaskHandler) visibleToCaller(connID string) (all bool, allowed map[string]bool) {
    if hasCap(h.callerCaps(connID), protocol.Capability_InfoGlobal) {
        return true, nil
    }
    return h.scopeSet(connID)
}
```

`listVisibleToCaller` stacks on top of `visibleToCaller` unchanged — see §2a.

The two sets are the same one except for that single bit, which is the point:
without a deliberate `info_global` grant, a task can never `cancel X` while
`ls` denies that X exists.

`Capability_InfoGlobal` therefore keeps its present meaning — see everything —
and becomes strictly a *visibility* override. A caller holding `info_global`,
`cancel` and `--scope subtree` lists a foreign task in full and is still
refused when it tries to kill it. That asymmetry is deliberate: reading the
task table is how an agent orients itself, and it is not the operation whose
blast radius §1 is about.

### 2a. The parent hop stays where it is

`listVisibleToCaller` (`server/capabilities.go:129`) returns the ACCESS set
plus one extra entry — the caller's direct creator — which `handleList`
renders through `redactParentTaskInfo` (`server/task_handler.go:1684`, blanking
repo path, worktree, prompt, error message, agent profile and assigned runner).

That hop is justified by the creator relationship, not by the target set: a
child needs to know whether the parent driving it is still alive, and it can
already read the creator id from the ungated `whoami` response. Scope does not
change that reasoning, so the hop is unconditional and survives every base —
a `--scope none` task still sees exactly one redacted parent row in `ls`.

What the hop must never do is feed `authorize`. The separation the existing
comment argues for gets stronger here, because the kinds being wired in §3 are
destructive: opening upward would let a child `cancel` or `file_write` a task
that, by attenuation, holds a superset of its own capabilities. So:

- `authorize` resolves through `scopeSet` only. The parent is not a target.
- `visibleToCaller` = `scopeSet`. LIST-only widening stays in
  `listVisibleToCaller`, one hop, redacted, as today.
- `base = global` sets `all = true`, so `parentHex` is empty and the hop is
  moot — unchanged from how `info_global` behaves today.

**`ids` may point upward, and that is the one way the parent becomes
actionable.** The spawn-time rule is "every requested id is in the granter's
effective set", and `self` is unconditionally in that set, so a parent can
spawn a child with `--scope ids:<parent-id>`. Reach stays monotone — the
target is inside the granter's own reach — and the child's row for the parent
is a full one rather than a redacted hop, because the id is in `allowed` and
the existing `if allowed[creatorHex]` early-return skips the redaction. The
inversion is therefore explicit, opt-in, and visible in `ls`; it is not
something the default `subtree` scope can produce.

### 3. The enforcement invariant

> Every `TaskControlRequest` variant carrying a target task id passes through
> `authorize` (action kinds) or `scopeSet` (INFO kinds) before the target is
> touched.

| kind | cap required | out-of-scope answer |
|---|---|---|
| `cancel` | `cancel` | `CancelStatus.no_such_task` |
| `submit` with `resume_task_id` | `spawn` | `SubmitStatus.resume_not_found` |
| `open_interactive` with `resume_task_id` | `spawn` | `OpenInteractiveStatus.resume_not_found` |
| `attach_session` | `exec_attach` | `AttachSessionStatus.not_found` |
| `await_idle` | `exec_attach` | `AwaitIdleStatus.not_found` |
| `open_file_transfer` | `file_read` \| `file_write` by direction | `OpenFileTransferStatus.no_such_task` |
| `list_files` | `file_read` \| `file_write` | `ListFilesStatus.no_such_task` |
| `git_query` | `file_read` | `GitQueryStatus.no_such_task` |
| `open_port_forward` | `forward_local` | `OpenPortForwardStatus.no_such_task` |
| `register_port_forward` | `forward_local` / `forward_remote` | `OpenPortForwardStatus.no_such_task` (the status type `RegisterPortForwardResponse` shares) |
| `kill_port_forward` | `forward_local` / `forward_remote` | unchanged (`no_such_forward`) |
| `get_task_log` | INFO | unchanged (`found=0`) |
| `list_port_forwards` | INFO | unchanged (filtered) |
| `prune_tasks` | `prune` | out-of-scope ids counted in `skipped_missing`; the bare `before_ts` sweep is filtered to the caller's set |

Out-of-scope targets answer "no such task", never `permission_denied` — the
rule `server/task_handler.go:434` already states for forwards: a
missing-capability answer for something the caller cannot see is an existence
oracle.

Two rows need reading carefully. `kill_port_forward` keeps its present
two-step shape — `visibleToCaller` decides whether the server answers about the
forward at all, and inside that branch `authorize` replaces the bare `hasCap`,
so a *visible* forward the caller may not act on still answers
`permission_denied` and an invisible one still answers `no_such_forward`. The
INFO rows (`get_task_log`, `list_port_forwards`) keep their code verbatim; what
changes underneath them is that `visibleToCaller` now resolves through
`scopeSet` rather than a hardcoded subtree BFS.

`CancelStatus` is currently `status :u8` with no vocabulary
(`runner/protocol/message.bgn:934`) and always replies 0. It gains an enum:

```
enum CancelResult:
    :u8
    ok           = "ok"
    no_such_task = "no_such_task"

format CancelStatus:          # name kept: it is the response variant's type
    status :CancelResult
```

The field is already `u8` and every existing reply writes 0, so `ok = 0` keeps
the wire byte identical for the success path.

**Completeness test.** `server/scope_completeness_test.go`, in the style of
`mapper_completeness_test.go`, enumerates every `TaskControlKind` and asserts
that each kind listed as target-carrying is exercised by a
denied-when-out-of-scope case. A new kind with a `task_id` field that is not
wired fails the build rather than shipping unscoped.

### 4. Attenuation at spawn

Capability bits keep `intersectCaps` unchanged. Scope attenuates on both axes:

- **base**: `min(parent_base, requested_base)` over the order
  `none < subtree < global`. A child asking for `global` under a `subtree`
  parent is clamped to `subtree`; its own subtree is a subset of the parent's,
  so no reach is gained.
- **ids**: every requested id must be inside the parent's effective set at
  spawn time. This is a static check against `scopeSet`, evaluated once.

A rejected id is an error, not a silent drop — silent clamping produces a task
whose scope differs from the one the caller wrote and nothing says so.
`SubmitStatus` and `OpenInteractiveStatus` each gain
`scope_not_permitted = "scope_not_permitted"`.

`SubmitResponse` has an `error_msg` field and uses it to name the offending id.
`OpenInteractiveResponse` has none — it carries `status`, `task_id`,
`stream_id` and an `ambiguous_runner`-conditional candidate list, and adding a
field to it is outside this change. On that path the status is the whole
answer, and the CLI supplies the detail locally by echoing the scope ids it
sent.

Operator connections (zero principal) are the trusted root: `callerCaps`
returns `Capability_All` and their effective base is `global`, so any scope
they request is granted as written.

### 5. `caps set` — operator-only re-grant against a live task

```
enum SetCapsStatus:
    :u8
    ok             = "ok"
    not_found      = "not_found"
    not_operator   = "not_operator"
    internal_error = "internal_error"

format SetCapsRequest:
    task_id    :TaskID
    caps       :Capability
    scope      :TaskScope
    # Presence bits, not conveniences: caps = 0 is Capability.none and
    # scope{subtree, []} is a real scope, so neither field has a spare value
    # meaning "leave it alone". Same shape as
    # SubmitRequest.resume_caps_override. Named *_present rather than set_*
    # because brgen's getter for a u1 named set_scope is `SetScope() bool`,
    # which reads as a setter for the neighbouring scope field.
    caps_present  :u1       # 1 = write caps;  0 = keep the persisted bitmask
    scope_present :u1       # 1 = write scope; 0 = keep the persisted scope
    cascade       :u1       # 1 = also clamp descendants
    keep_conns    :u1       # 1 = do not drop the affected tasks' connections
    reserved      :u4

format SetCapsResponse:
    status       :SetCapsStatus
    affected_len :u16
    affected     :[affected_len]TaskID   # target + every descendant actually changed
    conns_closed :u32
```

`TaskControlKind.set_caps` is appended to the enum (existing ordinals stay
stable).

**The gate is `lookupPrincipal(cid) == zero`, not a capability bit.** A bit
authorising "rewrite capabilities" is self-amplifying: whoever holds it grants
itself `all` with one call, and `intersectCaps` can never claw it back. Operator
identity is the only ungrantable predicate the server has, so it is the gate.
Non-operator callers get `not_operator`, which is a real answer and not an
existence oracle — the caller learns nothing about the target.

**Effect.** `TaskStore.SetCaps(id, caps, scope)` writes the fields and emits the
existing `task_caps_changed` WAL event (extended with the scope fields). Because
`callerCaps` and `scopeSet` read the store per request, the change is live on
the target's next RPC. Nothing restarts.

**Cascade** (`--cascade`, default off). Breadth-first over descendants using
the same `creatorHex → []childHex` index the `subtree` base walk builds — the
walk is needed here regardless of the target's own base, so it is factored out
of `scopeSet` rather than called through it:

- `child.caps &= newCaps`
- `child.scope.base = min(child.scope.base, newScope.base)`
- `child.scope.ids` is filtered to the target's new effective set

Without it, a revoked parent still reaches everything through a child it
already spawned with wider caps. With it, one operator call re-establishes the
monotonic invariant across the subtree. It defaults off because the widening
direction (granting a parent more) has no reason to touch children.

**In-flight streams.** A change that removes any capability bit or narrows the
scope closes every live connection whose principal is an affected task, unless
`--keep-conns`. The seam is `Server.activeConns`
(`server/server.go:116`, `objproto.ConnectionID → streamingConn`) joined with
`lookupPrincipal`; the TaskHandler reaches it through a new
`DropConnsForPrincipal func(taskIDHex string) int` hook, wired the same way as
`OnConnIdentified`. Closing the connection tears down every stream on it at
once — attach, file transfer and port forwards alike — which is why this is
done at the connection layer rather than per stream type (file transfers keep
no registry at all).

The blast radius is correct by construction: the dropped connections are the
ones the *revoked task opened outward*. That task's own PTY rides the runner
connection and survives, so a narrowed agent keeps running and simply finds its
next `harness-cli` call reconnecting under the new set. Pure widenings drop
nothing.

### 6. Persistence

`WALEvent` gains `ScopeBase uint8` and `ScopeIDs []string` on both
`task_created` and `task_caps_changed`. Events written before this change
replay with `ScopeBase == 0`; that zero is read as `subtree`, not `none`, so a
restart on an existing WAL reproduces today's visibility rule.

Existing tasks therefore become `subtree`-scoped on upgrade. For action kinds
this is a narrowing from "any task on the server", which is the point. An
operator-spawned agent that genuinely needs the old reach is re-granted with
`caps set <id> --scope global`, live, which is the feature this spec adds.

### 7. Surfaces

All three clients, per the repo rule that a feature spans CLI, TUI and WebUI.

**CLI** (`cmd/harness-cli`, `cli/`)

- `--scope` on `submit`, `interactive`, `session new`, parsed by a new
  `cli.ParseScope` beside `ParseCaps`. Grammar: `none` | `global` |
  `subtree` | `ids:<id>[,<id>…]` | `subtree+ids:<id>[,<id>…]`.
  A bare `ids:…` sets base `none`; the base is written explicitly only when it
  is not `none`, so `ids:X` and `none+ids:X` parse identically and
  `ParseScope` accepts both.
- `harness-cli caps set <task-id> [--caps NAMES] [--scope SPEC] [--cascade] [--keep-conns]`.
  Omitting `--caps` sends `caps_present = 0` and keeps the stored bitmask;
  omitting `--scope` sends `scope_present = 0` and keeps the stored scope. A call
  with neither is rejected client-side. Prints the affected ids and the
  number of connections closed.
- `harness-cli caps` gains a `SCOPE` section documenting the grammar; `--json`
  gains a sibling `scopes` array.
- `whoami`, `ls --json`, `session ls` gain a `scope` field rendered by a new
  `cli.ScopeLabel`, and the human `ls` renders it beside `caps` only when it is
  not the default `subtree`.

**TUI** (`tui/`)

- The `caps` cmdline command accepts a scope argument, stored alongside
  `App.sessionCaps` and shown in the same places.
- The task list gains a re-grant action opening a prompt for caps + scope,
  calling `set_caps` and reporting the affected count. Unconditionally
  visible — see below.

**WebUI** (`webui/static/main.js`, `cmd/harness-webui-wasm`)

- Scope input in the spawn dialog next to the existing caps input.
- Scope column in the task table.
- Re-grant action on the task detail view. Unconditionally visible — see
  below.

**Why the re-grant action is not conditionally hidden.** The operator/agent
split is made at the PSK gate, before the application layer sees the
connection. `pskGate.binderKey` (`server/psk.go:110`) requires any
`role=Client` connection whose `ClientHello.Kind` is not `agent` — that is
cli, tui and webui — to prove `operatorPSK`, a secret deliberately never
injected into an agent task's environment. `RecordClientIdentity`
(`server/task_handler.go:186`) then populates `principals[cid]` only for
`kind=agent`, so an accepted TUI or WebUI connection has a zero principal and
`callerCaps` returns `Capability_All`.

A non-operator TUI or WebUI session therefore does not exist: a client that
cannot prove `operatorPSK` never reaches the app layer as `kind=webui` at all.
Hiding the action on those surfaces would be gating on a state that cannot
occur. `SetCapsStatus.not_operator` is reachable from exactly one place —
`harness-cli` invoked inside a task, which connects as `kind=agent` — and that
is where the gate does its work.

One deployment caveat, pre-existing and named rather than addressed here: when
`operatorPSK` is unset, `binderKey` falls back to the shared connect psk, which
agents *do* hold, so an in-task agent could connect as `kind=webui` and be
taken for an operator. That is the hole the separate operator secret exists to
close, and no client-side check can substitute for configuring it.

**Agent skills.** `runner/agentskills/supervising-workers/SKILL.md` is the
`go:embed` source of truth and must be edited there, then mirrored to
`.claude/skills/` and `.agents/skills/`. The `--caps` section gains scope, and
the prune paragraph's convention text is rewritten to describe the enforced
rule rather than asking the agent to self-restrict.

### 8. Wire skew and upgrade order

Verified against the generated codec, not assumed.

**Field additions break hard, in both directions.** `DecodeExact` rejects
trailing bytes — `expect no remaining bytes but got %d bytes`
(`runner/protocol/message.go:273`) — and a short buffer errors
`not enough data to read for field "…"`. There is no optional-field mechanism
in the schema, so a new sender talking to an old receiver and an old sender
talking to a new receiver both fail.

On the server side the failure is silent. `TaskHandler.Handle`
(`server/task_handler.go:216`) logs the decode error and returns **without
sending a response**. The client blocks in its round-trip on a context that
`cmd/harness-cli/main.go:45` builds as `context.Background()` — no deadline —
so a skewed request presents as a hang until Ctrl-C, not as an error.

**Enum value additions are safe.** Decoding assigns the raw byte with no
`is_defined` check (`s.Status = SubmitStatus(tmp10332)`,
`runner/protocol/message.go:13928`) and `String()` falls back to
`SubmitStatus(%d)` (`message.go:13722`). So `scope_not_permitted` on
`SubmitStatus` / `OpenInteractiveStatus`, and `CancelResult` with `ok = 0`,
cannot break an old peer's decode — an unfamiliar status renders as a number.

**Blast radius is client↔server only.** `protocol.TaskControlRequest` appears
in `cli/` and `server/` and in no other non-test package; the runner never
decodes it. Changed formats:

| format | direction | change |
|---|---|---|
| `SubmitRequest` | client→server | `+= scope :TaskScope` |
| `OpenInteractiveRequest` | client→server | `+= scope :TaskScope` |
| `SetCapsRequest` / `SetCapsResponse` | client↔server | new |
| `TaskInfo` | server→client | `+= scope` |
| `WhoAmIResponse` | server→client | `+= scope` |
| `CancelStatus` | server→client | field retyped `u8` → `CancelResult`; bytes unchanged |
| `SubmitStatus` / `OpenInteractiveStatus` | server→client | new enum value |
| `TaskControlKind` | client↔server | `set_caps` appended, existing ordinals stable |

No `Runner*` format changes. `AssignTask`, `OpenExecRunnerRequest`,
`RunnerHello`, the PSK handshake and every file-transfer / port-forward relay
format are untouched, so **a runner built before this change speaks correctly
to a server built after it.**

**That is not sufficient, because recovery from the restart runs over the
formats that changed.** The server must restart, and after WAL replay every
task still Running is forced to Failed with reason `server_restart`
(`server/taskstore.go:731`); Detached is deliberately not persisted
(`taskstore.go:528`), so detached sessions replay as Running and land there
too. Recovering them *is* resume — and resume travels on `SubmitRequest` and
`OpenInteractiveRequest`, the two formats this change modifies. An
un-rebuilt TUI or `harness-cli` therefore fails precisely when it is needed,
and fails by hanging.

**Order.**

1. `make build` everywhere a client binary lives, and rebuild the wasm.
   Restart nothing yet.
2. Run the dummy-harness checks below against a *new* server with a *new*
   client, including resume of an interactive session. This is the gate.
3. Restart the server, then the runners
   (`scripts/build_and_restart_all.py`, self-last).
4. Resume the sessions the restart failed, using the rebuilt client.
5. Hard-reload every WebUI tab. A tab holding a cached pre-change wasm is an
   old client and will hang on submit and resume.

**Rollback** is reverting the server binary. The WAL is JSON and the reader
sets no `DisallowUnknownFields`, so `task_created` / `task_caps_changed`
records carrying `scope_base` / `scope_ids` replay cleanly on the old server,
which ignores them and reproduces the pre-change behaviour.

**One diagnostic fix belongs in this change.** `Handle`'s decode-failure log
gains the kind, request id and payload length. Both are readable from fixed
offsets before any union arm — `kind :u8` at byte 0 and `request_id :u32` at
bytes 1..5 (`runner/protocol/message.go:22450`) — so a skewed client is
identifiable from one server log line instead of being diagnosed from a hang.
Replying with a typed decode-error response is deliberately *not* proposed: an
old client cannot decode a response kind it does not know, so the reply would
not reach the peer that needs it.

A client-side request deadline would turn the remaining hang into an error,
but it changes the behaviour of every CLI command and is left out of this
change.

## Testing

**Unit (`server/`)**

- `scopeSet` for each base, with and without ids, with and without
  `info_global`, for operator and confined callers.
- `authorize` denial for every row of the §3 table, asserting the *specific*
  not-found status rather than `permission_denied`.
- Spawn attenuation: base clamping in both directions, out-of-scope id
  rejection with `scope_not_permitted`.
- `caps set`: `not_operator` for a confined caller; live effect on the next
  RPC; cascade clamping of a two-level subtree; `affected` contents.
- WAL round-trip of the extended `task_caps_changed`, and legacy events
  replaying as `subtree`.
- `scope_completeness_test.go` as described in §3.

**Wire (`runner/protocol/scope_wire_test.go`, beside the existing
`file_transfer_wire_test.go` / `agent_profile_wire_test.go`)**

- A hand-built pre-change `SubmitRequest` / `OpenInteractiveRequest` payload is
  rejected by the new decoder with `not enough data`, and a new-layout payload
  is rejected by a decoder truncated to the old field list with
  `expect no remaining bytes` — the skew is asserted, not assumed.
- A `SubmitResponse` carrying `scope_not_permitted` decodes without error under
  an enum switch that does not know the value, and renders as `SubmitStatus(8)`.
- `CancelStatus{ok}` encodes to the same single zero byte as before.

**Integration (dummy harness, `scripts/dummy-harness.sh`)**

- A `--scope subtree` child cannot `cancel`, `attach` or `file pull` a sibling,
  and its `ls` does not list it.
- A `--scope ids:<sibling>` child can do exactly those three against that
  sibling and nothing else.
- `caps set <child> --caps none` from the operator: the child's next
  `harness-cli ls` reflects the new set with no restart.
- The same call while the child holds an attach to a grandchild: the attach
  drops, and the child's own session stays alive.
- **Resume across the restart, on the new build**: spawn an interactive
  session, restart the server so the task lands in Failed/`server_restart`,
  and resume it with the rebuilt client. This is step 2 of the upgrade order
  and the check that the recovery path itself is not what the change broke.

## Amendment (2026-08-13): scope re-grants independently on resume

Shipped as designed, the resume paths gated BOTH halves of authority behind
the single `resume_caps_override` bit, which produced two silent behaviours:
a lone `--scope` on a resume was ignored without a word, and `--caps` without
`--scope` silently reset the task's scope to the request's zero value
(subtree) — `TaskStore.Resume` already took independent booleans, but both
handlers passed the caps override twice.

Fix: `SubmitRequest` and `OpenInteractiveRequest` gain `scope_present :u1`
out of their existing reserved bits (wire layout unchanged — no size skew;
an old client's zero bit reads as "keep", which is the safe default). On a
resume, caps re-grant iff `resume_caps_override`, scope re-grants
(attenuated, validated) iff `scope_present`; the two are independent, the
same shape as `SetCapsRequest.caps_present`/`scope_present`. On create the
bit is ignored — a fresh task's scope always applies and its zero value IS
the subtree default.

Clients: the CLI sets the bit when `--scope` was literally typed
(`flagExplicitlySet`), the TUI cmdline when its `--scope` flag was typed
(session defaults never rewrite a resumed task), and the WebUI gates both
halves behind the one "apply caps/scope on resume" checkbox — the Compose
scope picker's leftover state must not silently rewrite a resumed task.

## Amendment (2026-08-14): the creator link is operator-mutable

This spec describes the subtree walk over an immutable `creator_task_id`.
Since the task-reparent feature
(`docs/superpowers/specs/2026-08-14-task-reparent-design.md`) an operator can
re-point that link on a live task (`caps set-parent`, TaskControlKind
`set_parent`), including an atomic swap with the current parent. Resume still
never touches it. Consequently "caps_parent ⊇ caps_self" holds for links Create
made but is not an invariant of the CURRENT parent, and subtree membership is
whatever the (possibly re-pointed) links say at request time.

## Amendment (2026-08-20): the scope model is superseded — pointer only

§1's headline claim, that a scope value is **orthogonal to the capability
bitmask**, no longer holds. Scope is now resolved *per capability*, and the
unconditional `self` of §1 became a `TaskScope.include_self` bit — a task can
be granted read access to itself while being denied write access to itself,
which one shared scope cannot express and which no additional `ScopeBase` value
can produce (`self` joins the union outside `baseSet`).

The current definition lives in
[`2026-08-20-per-capability-scope-design.md`](2026-08-20-per-capability-scope-design.md).
Read §§1–2 of this spec for the axes and the vocabulary, then that file for the
shape actually enforced. Everything else here — the `authorize` choke point,
"no such task" as the out-of-scope answer, `caps set` gated on operator
identity rather than a bit, the §8 wire-skew procedure and upgrade order — is
unchanged and still authoritative.
