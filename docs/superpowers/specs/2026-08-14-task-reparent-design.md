# Task reparenting: an operator can re-point a task's parent link, or swap it with its parent

Date: 2026-08-14

## Problem

### 1. The parent link is decided at Create and can never be changed

`TaskInfo.creator_task_id` (`runner/protocol/message.bgn:491`) is the only
parent edge the harness has. It is written once, by `handleSubmit` /
`handleOpenInteractive`, to the task id of the agent principal on the creating
connection — all-zero when an operator created the task. Nothing rewrites it:

- `TaskStore.Resume` explicitly preserves it (`server/taskstore.go:315`:
  *"CreatorTaskID is intentionally NOT reset — it records the original creator
  and must not change on resume"*).
- The WAL replay path preserves it the same way (`server/taskstore.go:746`).
- There is no request kind that targets it.

### 2. The relation that turns out to be wrong mid-flight cannot be corrected

Work does not always keep the shape it was spawned with. A task A spawns B to
do a subtask; partway through it becomes clear that B holds the context that
should be driving and A is the subordinate piece. The parent link — and with
it the whole subtree target set — still says the opposite.

The workaround available today is to destroy and recreate the tasks in the
right order. That is unacceptable because **a task id is the identity its
conversation history is keyed under**: the runner re-attaches the worktree to
the retained `harness/<taskID>` branch on resume, and the agent's session
storage is keyed by the repo path the task was created under. Recreating the
pair loses exactly the accumulated context that made the role reversal
worthwhile.

### 3. What the parent link actually controls

It is not merely attribution. Five things read it, and the first two are
authority:

| consumer | file:line | effect |
|---|---|---|
| subtree target resolution | `server/capabilities.go:68` `childIndex`, `:87` `descendantsOf` | which tasks a `ScopeBase_subtree` task may cancel / attach / prune / transfer against |
| `set_caps` cascade | `server/set_caps_handler.go:70-97` | which tasks get clamped when the operator narrows an ancestor |
| list parent hop | `server/capabilities.go:254` `listVisibleToCaller` | the one redacted parent row a scope-restricted task may see |
| display | `cli/list.go:189,333`, `cli/whoami.go:71,90`, `tui/detail.go:94`, `webui/static/main.js:3613`, `cmd/harness-webui-wasm/main.go:626` | `by=` / `created by:` |
| persistence | `server/wal.go:46` | replayed on server restart |

The agentboard's "parent" (`agentboard/agentboard.bgn:55`) is a *message*
reply chain (`in_reply_to`), unrelated to the task edge, and is not a consumer.

## Decisions taken

Every point below is decided. None is left to the implementer.

| Question | Decision | Why |
|---|---|---|
| Mutate `creator_task_id`, or add a second mutable "authority parent" field? | **Mutate `creator_task_id`.** | A second field doubles every display surface, redaction path, WAL field and UI (items 11–23 below all become two-valued) to preserve a provenance record whose only stated consumer is a human reading `by=`. The original value survives in the WAL's `task_created` event regardless — the log is append-only with no compaction path in `server/wal.go` — so nothing is destroyed, only the live view changes meaning to "current parent". |
| New `TaskControlKind`, or a field on `SetCapsRequest`? | **New kind `set_parent`.** | Different preconditions (cycle check), different statuses (`would_cycle`, `no_parent`, `parent_not_found`), and `set_caps`'s `cascade` / `keep_conns` bits have no defined meaning for a topology change. |
| How is "detach to root" expressed? | **`parent_id` all-zero.** No presence bit. | Unlike `caps` (where 0 is `Capability_none`, a real value, hence `caps_present`), all-zero is *already* the field's encoding for "operator-rooted". There is no "not given" case: the request **is** the assignment. |
| Does it change caps or scope? | **No.** Orthogonal to `caps set`. | Inverting an edge moves the target set, not the granted verbs. An operator who wants the new parent to outrank its new child runs `caps set` after — that verb already exists, with the cascade and connection-teardown machinery this one deliberately lacks. |
| Does it close the old parent's live connections? | **No.** No `keep_conns` bit, no `conns_closed` field. | `set_caps` closes connections on a *narrowing* because a revoked capability that does not reach in-flight work is advisory. A reparent removes no capability bit from anyone. Worse, the primary use case is role inversion, where the former parent's live attach into the child is exactly what the operator wants to keep. Follow with `caps set` if teardown is wanted. |
| Are the former parent's ancestors touched? | **No.** | Descendants follow the moved node implicitly (their own links are untouched and still point at it). Ancestors are not consulted, walked, or notified. |
| Is the inversion atomic? | **Yes — a `swap` bit on the request, applied under one `TaskStore` lock.** | The alternative (client-side composition of two `set_parent` calls) costs two round trips, forces every UI to duplicate the ordering logic, needs a snapshot read to resolve the grandparent, and leaves an observable half-applied state on failure. |
| Does `swap` need a cycle check? | **No** — by construction. | Both links are rewritten before the lock is released, so the two-cycle the two-call sequence would transiently form is never observable. The non-swap path still checks. |
| Is the caller gated on a capability bit? | **No — operator-only**, same predicate as `set_caps`. | A bit authorising "re-point parents" is self-amplifying: whoever holds it adopts any victim task under itself and gains that victim's entire subtree, and `intersectCaps` can never claw it back. Operator identity (no principal task, established at the PSK gate in `server/psk.go`) is the only ungrantable predicate the server has. |
| Does the response report the moved descendants? | **No.** Three task ids only. | No descendant record is written — they follow implicitly. A `+N descendant(s) moved` suffix would name a fan-out that did not happen, which is the failure mode checklist item 29 exists to prevent. |
| CLI placement | **`harness-cli caps set-parent`**, inside the existing `caps` verb family. | `caps` is already the operator-only authority family (`cmd/harness-cli/main.go:191`, dispatching on `args[0] == "set"`). The parent link is an authority-topology edge; grouping it there keeps every operator-only mutator under one verb. |

## Wire schema

The complete schema change, in one place. All additions to
`runner/protocol/message.bgn`.

### `TaskControlKind` — one new member

```
    set_parent      # operator-only: re-point a live task's parent link, or
                    # swap it with its current parent. Gated on the caller
                    # having NO principal task, not on a capability bit — same
                    # self-amplification argument as set_caps: a task able to
                    # re-point parents would adopt any victim under itself and
                    # inherit that victim's whole subtree as a target set, and
                    # spawn-time intersection can never claw that back.
```

### Statuses

```
enum SetParentStatus:
    :u8
    ok                   = "ok"
    not_found            = "not_found"
    # parent_not_found covers both a named parent_id that is not in the store
    # and, on the swap path, a current parent whose record is gone (a pruned
    # creator — listVisibleToCaller already treats that as a reachable state).
    parent_not_found     = "parent_not_found"
    would_cycle          = "would_cycle"
    # swap on a task that is already operator-rooted: there is nothing to
    # invert. Named rather than silently succeeding as a no-op.
    no_parent            = "no_parent"
    # swap == 1 with a non-zero parent_id. Rejected rather than ignored: a
    # typed option either takes effect or errors.
    swap_takes_no_parent = "swap_takes_no_parent"
    not_operator         = "not_operator"
    internal_error       = "internal_error"
```

### Request

```
# SetParentRequest re-points task_id's parent link (its creator_task_id).
#
# Unlike SetCapsRequest this format has no presence bits. caps = 0 is
# Capability.none and scope{subtree, []} is a real scope, so neither has a
# spare value meaning "leave this alone" — a parent link does: all-zero is
# already the encoding for "operator-rooted". And there is no "not given"
# case to distinguish, because the request IS the assignment.
format SetParentRequest:
    task_id   :TaskID
    # New parent. All-zero detaches the task to the operator root. Must be
    # all-zero when swap == 1 (else swap_takes_no_parent).
    parent_id :TaskID
    # swap: invert task_id with its CURRENT parent. The target takes the
    # parent's place — inheriting the parent's own parent, which may be the
    # root — and the former parent becomes the target's child. Both links are
    # rewritten under one TaskStore lock, so the two-cycle that the equivalent
    # two-call sequence forms transiently is never observable, and this path
    # needs no cycle check.
    swap      :u1
    reserved  :u7
```

### Response

```
format SetParentResponse:
    status     :SetParentStatus
    # The target's parent before and after the change. All-zero means
    # operator-rooted, in both fields.
    #
    # new_parent is on the wire rather than left implicit because on the swap
    # path the client does not know it: the server resolves it from the former
    # parent's own link. Echoing it on the plain path too keeps one shape.
    old_parent :TaskID
    new_parent :TaskID
    # swap only: the former parent, which is now the target's child. All-zero
    # when swap was not requested.
    swapped_id :TaskID
```

There is deliberately no `moved`/`moved_len` list: no descendant record is
written, so reporting them would name a fan-out that did not occur.

## Server semantics

New file `server/set_parent_handler.go`, mirroring `server/set_caps_handler.go`.

**Gate.** `h.lookupPrincipal(cid).Id != ([16]byte{})` → `not_operator`. As in
`set_caps`, `not_operator` is a real answer rather than a not-found: it says
something about the caller and nothing about the target, so it is not an
existence oracle.

**Validation order** (before any write):

1. `swap == 1 && parent_id != 0` → `swap_takes_no_parent`.
2. target not in store → `not_found`.
3. Non-swap, `parent_id != 0`, parent not in store → `parent_not_found`.
4. Non-swap, `parent_id ∈ descendantsOf(childIndex(), targetHex)` →
   `would_cycle`. Reuses the existing helper unchanged; this is what keeps
   `listVisibleToCaller`'s *"only reachable through a creator cycle, which task
   creation cannot produce"* (`server/capabilities.go:266`) true. No separate
   `parent_id == task_id` check: `descendantsOf` seeds its result with the
   root it is asked about (`server/capabilities.go:97`), so self-parenting is
   already inside this predicate. Writing both would be two spellings of one
   rule.
5. Swap, target's creator is all-zero → `no_parent`.
6. Swap, target's creator is not in the store → `parent_not_found`.

The grandparent `P` on the swap path is deliberately **not** validated. If
`A`'s own creator names a task that has since been pruned, the swap writes that
dangling id onto the target — which is exactly the state a pruned parent
already produces today and which `listVisibleToCaller` already handles
(*"a creator no longer in the store"*, `server/capabilities.go:252`). Rejecting
it would make an unrelated prune block a reparent.

**Application.**

- Non-swap: `TaskStore.SetParent(targetHex, parentID)`.
- Swap, with `A` = the target's current parent and `P` = `A`'s current parent
  (possibly all-zero): `TaskStore.SwapWithParent(targetHex)` sets
  `target.CreatorTaskID = P` and `A.CreatorTaskID = target` under one hold of
  `s.mu`, appending one WAL event per link.

Capabilities and scopes of every task are untouched on both paths. No
connection is closed. `h.OnChange()` fires on success so the TUI and WebUI
snapshots refresh, matching `handleSetCaps`.

**Store API** (`server/taskstore.go`):

```go
func (s *TaskStore) SetParent(idHex string, newParent protocol.TaskID) (TaskEntry, bool)
func (s *TaskStore) SwapWithParent(idHex string) (target, former TaskEntry, err error)
```

`SwapWithParent` returns `ErrSwapNoParent` / `ErrSwapParentMissing` so the
handler maps them to `no_parent` / `parent_not_found` without re-reading the
store between check and write.

## Persistence

One new `WALEvent.Type` value, `task_parent_changed`. No struct change: it
reuses the existing `task_id` and `creator_task_id` fields.

```json
{"type":"task_parent_changed","task_id":"<hex>","creator_task_id":"<hex>","ts":0}
```

`creator_task_id` is `json:",omitempty"`, so **a detach writes the key
absent**. For this event type only, an absent `creator_task_id` means "set to
all-zero". That is unambiguous because the event type itself carries the
intent — unlike `task_created`, where absence means "was never set".

Replay applies the event in log order after the `task_created` that introduced
the task, so the last write wins. A WAL written before this change contains no
such event and replays byte-identically to today — no legacy default to
decide. A swap contributes two consecutive events, one per link; replaying them
in order reproduces the swapped state.

## Comment and test corrections required

The change makes three existing in-code statements false. Each must be
corrected in the same commit, not left to contradict the shipped behaviour:

1. `server/taskstore.go:315` — *"CreatorTaskID is intentionally NOT reset — it
   records the original creator and must not change on resume"*. Becomes: resume
   still does not change it; `set_parent` is the only writer after Create.
2. `server/taskstore.go:746` — the same statement on the WAL replay path.
3. `server/task_handler.go:1787-1791` — *"Spawn-time attenuation already
   guarantees caps_parent ⊇ caps_self, so the bitmask discloses nothing the
   caller cannot derive"*. The guarantee now holds only for links that Create
   made. Note that the disclosure argument is unaffected: knowing the parent's
   caps are a superset never told the child *which* superset, so seeing the
   exact bitmask was already strictly more information, and a subset bitmask is
   not a larger disclosure than a superset one.

`server/taskstore_test.go:967` `TestCreateRecordsCreatorTaskID` and
`TestWALReplayRestoresAttribution` (`:895`) assert the field never changes.
They keep asserting that for Create and Resume, and gain a sibling case for the
`set_parent` writer.

## Operator surfaces

Walk of the `operator-surface-checklist`, items 1–36. Verdicts are the
implementation plan; re-verified against the diff at review.

### Input surfaces

1. **CLI flags** — done. `cmd/harness-cli/main.go:191` `case "caps"` already
   dispatches on `args[0] == "set"`; add `"set-parent"`. Flags: `--parent <id>`,
   `--none`, `--swap`, mutually exclusive, with help text naming the swap
   semantics ("target takes its parent's place; the parent becomes its child").
2. **CLI parse helpers** — n/a. No new grammar: the arguments are task ids,
   parsed by the same helper `caps set` uses. `ParseCaps` / `ParseScope` are
   untouched.
3. **TUI cmdline verbs** — done. `set-parent` parser in `tui/cmdline.go`
   alongside `parseSetCaps`, same three flags.
4. **TUI keybindings** — done. `A` in `mainKeyMap` + a matching
   `mainKeyBindings` row (`tui/keys.go`; `keys_test.go` fails on an unpaired
   field). `A` is unused today and pairs with `a` (ReGrant), matching the
   existing lowercase/uppercase variant convention (`r`/`R`, `p`/`P`, `w`/`W`).
5. **TUI pickers/popups** — done. Reuse `tui/authoritypicker.go` in a parent
   mode: two pseudo-rows first — `(root — detach)` and, when the task has a
   parent, `(swap with <P8>)` — then the candidate tasks from `buildRows`. Width
   still derives from the terminal via `fit()` (item 32), and the batched
   `KeyRunes` caveat applies unchanged.
6. **WebUI controls** — done. A "Set parent…" action in the task detail sheet
   opening a `<dialog class="picker-modal">` with the same two pseudo-rows plus
   the task list.
7. **WebUI command input** — done. `set-parent` case in `runCmd`, accepting
   `--parent` / `--none` / `--swap`, plus its help text entry.
8. **wasm bridge** — done. `"setParent": js.FuncOf(harnessSetParent)` in the
   registration map at `cmd/harness-webui-wasm/main.go:112`, taking
   `{taskId, parentId?, swap?}` — key names identical to the JS request object.
9. **Session-default state** — n/a. This mutates an existing task; it is not a
   spawn, so `sessionCaps`/`sessionScope`/`spawnCaps`/`spawnScope` have no
   analogue to carry.
10. **Every other verb family** — n/a. No option is added to `file`, `git`,
    `forward`, `prune`, `notify`, `board`, or `session`. The new option set
    belongs to one new member of the `caps` family, which receives the CLI, TUI
    cmdline, WebUI control and `runCmd` coverage listed in 1, 3, 6 and 7.

### Display surfaces

The field is already rendered everywhere it needs to be, which is the practical
payoff of mutating `creator_task_id` rather than adding a field. Items 11, 12,
13, 14, 17, 20 need **no code change**; they are marked done because the walk
verified the field reaches each one, not because a line was added.

11. **`ls` human rows** — done, no change. `by=` at `cli/list.go:189`.
12. **`ls --json`** — done, no change. `createdBy` at `cli/list.go:333`.
13. **`whoami`** — done, no change. Both branches at `cli/whoami.go:71,90`.
14. **`session ls`** — done, no change. Session rows already carry
    `CreatorTaskId` (asserted by `cli/list_json_test.go:97`).
15. **`caps` catalog** — done. One line in the SCOPE section of `cli/caps.go`
    `WriteCaps`: subtree membership is defined by the parent link, and
    `caps set-parent` re-points it. Without it the grammar reference never
    mentions the thing `subtree` is computed from.
16. **TUI task table** — omitted. `tui/tasks.go` `SetRows` has no creator
    column today and gains none: the parent is an 8-hex id that costs a column
    to serve a value the detail popup already shows. This is the item 31 row-width
    exception, applied consistently with the existing scope elision.
17. **TUI task detail popup** — done, no change. `created by:` at
    `tui/detail.go:94`. It is the surface item 16 defers to.
18. **TUI runner detail** — n/a. `formatRunnerDetail` renders runners; the
    parent link is a task field.
19. **TUI picker rows** — done. `buildRows` gains the two pseudo-rows described
    in item 5; the per-task columns are unchanged.
20. **WebUI task row meta** — done, no change. `by=` at
    `webui/static/main.js:3613`.
21. **WebUI task detail sheet** — done. The `addItem` action list gains the
    "Set parent…" entry from item 6. The current parent is already visible in the
    row meta above it.
22. **WebUI dialogs' prefill** — done. The dialog highlights the task's current
    parent, which needs an exact id, not a label. `createdBy` is truncated to 8
    chars by `creatorShort`, so a prefix match could highlight the wrong row —
    the raw value is added in item 23. `--swap` and `--none` need no id at all,
    and `--parent` sends the picked row's own full id.
23. **wasm snapshot JSON** — done. Add `"createdById"` (full 32-hex, `""` when
    all-zero) beside the existing `"createdBy"` short label in the `ls`
    conversion map at `cmd/harness-webui-wasm/main.go:626` — the
    label-plus-raw-value pattern item 22 requires.

### Semantics axes

24. **Same option, other path** — done. `--parent`, `--none` and `--swap` are
    reachable only from `caps set-parent`; there is no second path (no resume
    variant, no spawn-time form). A spawn's parent is still fixed by the creating
    connection's principal and is not settable on `submit` / `interactive` /
    `session new` — deliberately, since letting a spawner *name* a parent is the
    self-amplification hole the operator-only gate closes.
25. **Presence** — done, and the answer is that no presence bit is needed;
    the reasoning is recorded in the Decisions table and in the schema comment
    rather than left implicit.
26. **Session defaults** — n/a, per item 9.
27. **Shared funnel** — done. One `cli/set_parent.go` exporting
    `SetParentWith(ctx, c, opts)` and `SetParent(ctx, serverCID, opts)`,
    mirroring `cli/set_caps.go:36,76`. TUI and WebUI/wasm call the `*With` form
    against their long-lived client (`a.client` / `currentClient()`); only the
    short-lived `harness-cli` binary dials. This is Pitfall 3's exact shape.
28. **Persistence** — done. See the Persistence section: new event type, reused
    fields, and a decided meaning for records written before it existed (absent
    event = no change, so old logs replay identically).
29. **Result messages name the target and the change** — done.
    - `set-parent <B8>: parent=<A8> → <P8>`
    - `set-parent <B8>: parent=<A8> → (root)`
    - `set-parent <B8> --swap: <B8> now under (root|<P8>), <A8> now under <B8>`

    No count suffix: nothing cascaded (see the Decisions table).
30. **Results go to the result surface** — done. WebUI `appendCmdOutput`, not
    `setStatus` (which is the connection badge); TUI `a.cmdresult`. Matches the
    `caps set` handler at `webui/static/main.js:2782,2790`.
31. **No hidden defaults in detail views** — done. `(root)` is rendered
    explicitly for an all-zero parent in the result message and in the picker's
    detach row; it is never blank. The one elision is item 16's task-table
    column, which the detail popup covers.
32. **One serializer per grammar per runtime** — n/a. No new grammar is
    introduced; task ids are already hex on every surface.
33. **A typed option either takes effect or errors** — done. `--swap` with
    `--parent` is rejected on the wire (`swap_takes_no_parent`), not silently
    ignored; `--swap` on a rootless task returns `no_parent` rather than
    succeeding as a no-op; a cycle returns `would_cycle` rather than being
    silently clamped.

### Documentation surfaces

34. **`README.md`** — done. The "Capabilities and scope" section gains the
    parent-link paragraph (what subtree is computed from, and that the operator
    can re-point it), and the TUI cmdline verb list gains `set-parent`.
35. **Agent-facing skill texts** — n/a. `set_parent` is operator-only; an agent
    can never call it, so `runner/agentskills/*/SKILL.md` gains nothing and the
    `.claude/` + `.agents/` mirrors stay as they are. The repo-dev skills are
    untouched.
36. **The feature's spec** — done. This document. The task-scoped caps spec
    (`docs/superpowers/specs/2026-08-13-task-scoped-caps-design.md`) describes
    the subtree walk over an immutable creator link; it gains an Amendment
    section pointing here, so a later reader verifying against it is not misled.

## Tests

Server (`server/set_parent_handler_test.go`, `server/taskstore_test.go`):

- Non-operator caller (agent principal on the connection) → `not_operator`,
  link unchanged.
- `parent_id == task_id` → `would_cycle`.
- A → B, then `set_parent(A, parent=B)` → `would_cycle`, both links unchanged.
- `set_parent(B, parent=0)` on a task with a parent → `ok`,
  `old_parent == A`, `new_parent == 0`.
- `swap` on an operator-rooted task → `no_parent`.
- `swap` with a non-zero `parent_id` → `swap_takes_no_parent`.
- `swap` where the current parent's record was pruned → `parent_not_found`.
- P → A → B, `swap(B)` → `ok`; `B.creator == P`, `A.creator == B`; response
  carries `old_parent == A`, `new_parent == P`, `swapped_id == A`.
- The same case with P all-zero (A operator-rooted) → `B.creator == 0`.
- A's other children stay under A after `swap(B)` — the surrounding tree is
  preserved.
- Caps and scopes of P, A and B are byte-identical before and after, on both
  paths.

Authority (`server/scope_attenuation_test.go` sibling):

- After `swap(B)` with both tasks at `ScopeBase_subtree`: `scopeSet` for B
  includes A, and `scopeSet` for A excludes B.

WAL (`server/wal_test.go`, `server/taskstore_test.go`):

- create → `task_parent_changed` → replay restores the new parent.
- A detach round-trips: the event marshals without the `creator_task_id` key
  and replays to all-zero.
- A swap's two events replay to the swapped state in order.

Client (`cli/set_parent_test.go`): request construction for all three forms,
and the three result-message shapes from item 29.

TUI: `keys_test.go` pairing for `A`; cmdline parser tests for the three flags
and the mutually-exclusive rejection.

## Out of scope

- **Multiple parents.** The edge stays single, as the schema comment at
  `runner/protocol/message.bgn:494` says.
- **Setting a parent at spawn time.** See item 24.
- **Moving conversation history, worktrees or branches.** Nothing about the
  task's identity, worktree or `harness/<id>` branch changes — which is the
  entire reason this feature exists instead of "delete and recreate".
- **Connection teardown on reparent.** Decided against; `caps set` remains the
  verb that reaches in-flight work.
