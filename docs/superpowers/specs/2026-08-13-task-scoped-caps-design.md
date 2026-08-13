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
    none    = "none"       # no set beyond self
    subtree = "subtree"    # self + descendants (today's visibility rule)
    global  = "global"     # every task

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

`ScopeBase.none` combined with `Capability_Spawn` yields a task that can create
children it cannot then supervise. That is expressible and left expressible —
if a spawner must be able to reach what it spawns, do not narrow its base below
`subtree`.

### 2. One resolution function, one choke point

```go
// scopeSet resolves the caller's effective target set.
//   all=true  → operator, Capability_InfoGlobal, or ScopeBase_Global.
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
```

`visibleToCaller` keeps its name and signature and becomes a call to
`scopeSet`, so `ls`, `task log`, the port-forward list and the conns filter
inherit the scope without further change. `listVisibleToCaller` (the extra
one-hop-to-parent LIST rule) stacks on top unchanged.

Visibility and action share the set deliberately. Splitting them would produce
a task that can `cancel X` while `ls` denies that X exists.

`Capability_InfoGlobal` keeps its present meaning — see everything — and is now
strictly a *visibility* override: it widens `scopeSet` for the INFO-scoped
kinds but `authorize` still requires the verb bit, and a caller with
`info_global` and `--scope subtree` sees a foreign task without being able to
act on it.

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
    cascade    :u8          # 1 = also clamp descendants
    keep_conns :u8          # 1 = do not drop the affected tasks' connections

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

**Cascade** (`--cascade`, default off). Breadth-first over descendants using the
same child index `scopeSet` builds:

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
  Omitting `--caps` keeps the stored bitmask; omitting `--scope` keeps the
  stored scope; at least one must be given. Prints the affected ids and the
  number of connections closed.
- `harness-cli caps` gains a `SCOPE` section documenting the grammar; `--json`
  gains a sibling `scopes` array.
- `whoami`, `ls --json`, `session ls` gain a `scope` field rendered by a new
  `cli.ScopeLabel`, and the human `ls` renders it beside `caps` only when it is
  not the default `subtree`.

**TUI** (`tui/`)

- The `caps` cmdline command accepts a scope argument, stored alongside
  `App.sessionCaps` and shown in the same places.
- The task list gains an operator-only re-grant action opening a prompt for
  caps + scope, calling `set_caps` and reporting the affected count.

**WebUI** (`webui/static/main.js`, `cmd/harness-webui-wasm`)

- Scope input in the spawn dialog next to the existing caps input.
- Scope column in the task table.
- Re-grant action on the task detail view, hidden when the session is not an
  operator connection.

**Agent skills.** `runner/agentskills/supervising-workers/SKILL.md` is the
`go:embed` source of truth and must be edited there, then mirrored to
`.claude/skills/` and `.agents/skills/`. The `--caps` section gains scope, and
the prune paragraph's convention text is rewritten to describe the enforced
rule rather than asking the agent to self-restrict.

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

**Integration (dummy harness, `scripts/dummy-harness.sh`)**

- A `--scope subtree` child cannot `cancel`, `attach` or `file pull` a sibling,
  and its `ls` does not list it.
- A `--scope ids:<sibling>` child can do exactly those three against that
  sibling and nothing else.
- `caps set <child> --caps none` from the operator: the child's next
  `harness-cli ls` reflects the new set with no restart.
- The same call while the child holds an attach to a grandchild: the attach
  drops, and the child's own session stays alive.
