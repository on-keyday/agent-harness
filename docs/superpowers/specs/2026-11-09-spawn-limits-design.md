# Spawn limits — bounding how deep and how wide a task's subtree grows — design

## Problem

- **P1.** The operator does not grant `spawn` to agents. An agent holding it
  reads the bit as licence to create as many tasks as it likes, and nothing on
  the server says otherwise. The operator wants to be able to hand `spawn` out
  with a bound the server enforces.
- **P2.** The delegation bound (`2026-10-22-delegation-bound-design.md`) can
  express only three depths. A child's caps are `P.caps ∩ P.delegate_caps ∩
  requested` (`authority/delegate.go:87`) and its own bound is
  `requested ∩ P.delegate_caps` (`authority/delegate.go:136`), an
  intersection that never decreases with depth. So from the root the operator
  can write "no spawn", "children but no grandchildren" (default
  `delegate_caps = none`), or "unbounded" (`spawn` in `delegate_caps`, after
  which every generation may pass it on). "At most three generations" is not
  expressible; that spec says so under Non-goals.
- **P3.** Nothing bounds how many tasks exist. One task holding `spawn` can
  create any number of children. A per-task limit on DIRECT children does not
  fix this: k children per task over d generations is k + k² + … + k^d tasks,
  and with no generation limit it has no bound at all (operator, 2026-11-09).

## Design constraint

Add as few rules as possible, and **no rule that treats the operator
differently from an agent** (operator, 2026-11-09). Today the only things
reserved to the operator are `caps set` and `caps set-parent`
(`server/set_caps_handler.go:42`, `server/set_parent_handler.go:37`); every
other operator branch in `server/` implements one rule, "the operator is the
root and holds everything" (`callerCaps` returns `Capability_All`,
`server/capabilities.go:189`; `scopeSetWith`, `granter`, `visibleToCaller`
likewise). This design adds no branch on operator identity. Where the operator
needs a value, it is the root's value under that existing rule.

## The intended use

The operator grants an agent `spawn`, `resume` and `cancel` with the two
limits below, and does NOT grant `prune` (operator, 2026-11-09). The agent can
then create at most the limit's number of tasks; it can reuse one it created
by resuming it, which creates no record; it cannot free a place itself.
Places are freed by the operator, by pruning. The count is therefore a bound
on how many tasks the agent's subtree has ever produced and not yet been
cleaned up after — not only on how many run at once.

## Terms

**Record** — a task present in the server's task store, whatever its status.
A task stops being a record only when it is pruned (`PruneByIDs`,
`PruneTerminal`, `server/taskstore.go`), which since `380aba71` happens only to
terminal tasks.

**Spawn depth** (`spawn_depth`) — a value stored on a task: how many more
generations may be created below it. Either a number 0–255 or unbounded.
0 = the task may create no task. 1 = it may create children, which get 0.

**Descendant limit** (`max_descendants`) — a value stored on a task: how many
records its subtree may hold, the task itself NOT counted. Either a number
0–65535 or unbounded. Counted over the CURRENT parent links
(`creator_task_id`), so a `caps set-parent` moves tasks in and out of a
subtree's count at once.

IN: both limits are checked when a task is CREATED (submit or interactive open
with a zero `resume_task_id`), whoever sends the request.

OUT: a resume creates no record and is not checked against either limit.
Neither limit counts anything that is not a harness task: an agent's
in-process subagents, `exec run` processes and port forwards are not tasks.

## Decisions taken

**operator** = the human chose it in conversation (2026-11-09). **this spec** =
the author chose it while writing; those rows are the ones to check.

| # | Decision | Decided by |
| --- | --- | --- |
| D1 | Two limits, both server-enforced: a generation limit and a limit on the number of tasks | operator |
| D2 | The count limit counts RECORDS — every task in the store, whatever its status — not live tasks. A task frees its place when it is pruned, not when it ends. Replaces an earlier draft that counted live tasks (`Queued` / `Running` / `Detached` / `Held`); the operator's reason: a record persists after it ends, and the aim is that tasks are not created without bound, which the record count bounds directly given the intended use (agents hold no `prune`) | operator |
| D3 | The count limit counts the whole subtree below a task, not its direct children (P3) | operator |
| D4 | Counted over the current parent links. A `caps set-parent` takes effect on the count at once. Moving tasks INTO a subtree may leave it over its limit: nothing is removed, and creations under it are refused until it is back under. `caps set-parent` itself runs no check: it creates no record | operator (accepted the playback) |
| D5 | The generation limit is stored on the task and decremented per generation (`child = min(requested, P − 1)`), not computed from the task's position in the tree. A `caps set-parent`, plain or `--swap`, does not change any task's `spawn_depth` | operator (accepted the playback) |
| D6 | The descendant limit is kept as written and is NOT clamped to the parent's. Every ancestor's limit is checked on each creation, so a child given a larger number than its parent is still bounded by the parent's. What an agent sees as its room is the minimum over its ancestors (D16) | this spec |
| D7 | No exemption by who sends the request. The checks run for an operator connection as for an agent. An operator-created task has no parent (`creator_task_id` all-zero), so there is no ancestor limit to check and the operator's own `spawn_depth` is unbounded as the root; neither needs a branch | operator |
| D8 | Defaults: an omitted value is unbounded, for both limits, on every request. A task created without a requested `spawn_depth` gets `P.spawn_depth − 1` (unbounded stays unbounded). Reason: the delegation bound's default (`delegate_caps = none`) already stops grandchildren, so the limits only matter once the operator chooses to delegate `spawn`, and that is when the operator writes them. Tasks recorded before this change replay as unbounded, which is their behaviour today | this spec |
| D9 | A resume is not checked. It re-queues an existing record and adds none, so the count is unchanged; and it creates no generation. A resume that re-grants `spawn_depth` still writes `min(requested, P.spawn_depth − 1)` with `P` = the resumer | this spec |
| D10 | Prune is unchanged by this design, and it is how a place is freed. Pruning an ENDED task in the middle of a tree also removes the link its descendants are counted through, so the records below it leave every ancestor's count. Example: A (`max_descendants = 5`) → B (ended) → two workers; pruning B frees three of A's places. In the intended use no agent holds `prune`, so only the operator can do this. A holder of `prune` over an ended task inside a limited subtree can, and the `prune` entry in the `caps` catalog says so. The server's retention sweep (`--task-retain`, off by default, `cmd/harness-server/main.go:39`) prunes ended tasks the same way. No new rule is added to prune (Design constraint) | operator (accepted the playback) |
| D11 | The check and the creation happen in one `TaskStore.mu` critical section, so two concurrent creations cannot both take the last place | this spec |
| D12 | One new status, `spawn_limit`, on `SubmitStatus` and `OpenInteractiveStatus`, with `error_msg` naming which limit refused and the numbers. The ancestor that refused is named by its distance ("your own", "your parent's", "an ancestor's, 3 levels up"), not by id: the caller may not be able to see ancestors above its parent | this spec |
| D13 | Both values are editable on a live task through `caps set` (two more flags, as the delegation bound did), and the write touches that task only. `--cascade` does not touch either limit: it re-runs today's caps/scope clamp exactly as it does without these flags. A tighter `max_descendants` needs no cascade, since every ancestor's limit is checked on each creation; a tighter `spawn_depth` applies to tasks created after it, and existing descendants keep their values | operator (accepted the playback) |
| D14 | `caps set-defaults` (and the TUI / WebUI session defaults) carry both values for the session's fresh spawns, as they carry caps, scope and the delegation bound | this spec |
| D15 | Flags `--spawn-depth N|unbounded` and `--max-descendants N|unbounded`. Not `--max-tasks`: `agent-runner --max-tasks` already names a runner's concurrent-task cap | this spec |
| D16 | `whoami` prints this task's two values and its room — the minimum, over itself and its ancestors that have a numeric `max_descendants`, of `max_descendants − descendants` — so an agent can see the bound before it hits it (P1) | this spec |
| D17 | The counts are shown unfiltered: `TaskInfo.descendants`, `whoami`'s room and the refusal text count every record in the subtree, including ones the reader cannot see. They reveal a number, never an id, and an agent cannot use its limit without knowing how much of it is taken (P1, D16) | this spec (builder check) |
| D18 | What an agent does on a `spawn_limit` refusal is guidance in the agent skill, not a server rule: resume an existing worker first; otherwise ask the operator to prune or raise the limit. An agent that holds `prune` asks explicitly and prunes only with the operator's approval; an unapproved prune is restorable but discouraged. The server does not distinguish an approved prune from an unapproved one | operator |
| D19 | The operator-facing text (the `caps` catalog's SPAWN LIMITS section and README) cautions against granting more than the work needs: an agent given `spawn` with limits is bounded only while it lacks `prune` over its subtree (D10). A caution in text, nothing more — no runtime warning and no refusal of the `spawn` + `prune` combination | operator |

## The rule

Notation: `P` is whoever sends the request: an agent's principal task, or the
operator root, whose `spawn_depth` is unbounded and which has no ancestors. `X`
is the task being created. `anc(X)` is `P` and `P`'s parent, that task's
parent, and so on, following `creator_task_id` through tasks present in the
store, stopping at an all-zero link or a parent not in the store (D10); it is
empty when `P` is the operator. `desc(A)` is the number of records in `A`'s
subtree, `A` excluded, under the current parent links.

### Creating a task

1. If `P.spawn_depth` is a number and is 0 → refused, `spawn_limit`, "your
   spawn depth is 0".
2. For each `A` in `anc(X)` with a numeric `max_descendants`: if `desc(A) + 1
   > A.max_descendants` → refused, `spawn_limit`, naming `A` by distance and
   printing `desc(A)/A.max_descendants`.
3. Otherwise the task is created with
   - `spawn_depth = min(requested, P.spawn_depth − 1)`, where an omitted request
     and an unbounded `P` are both unbounded, and `min` with unbounded is the
     other value;
   - `max_descendants = requested` (D6), unbounded when omitted.

Steps 1–3 run in the critical section of `TaskStore.Create` (D11). Step 1 runs
before step 2, so a task with depth 0 is told about depth, not about a count it
could never use.

### Resuming a task

No check (D9). A resume that re-grants the limits (`spawn_depth_present` /
`max_descendants_present`) writes `spawn_depth = min(requested,
P.spawn_depth − 1)` (floored at 0) and `max_descendants = requested`, with `P`
= the resumer. A resume without the presence bits keeps the recorded values.

### `caps set`

Operator-only, as today. With `spawn_depth_present` / `max_descendants_present`
it writes that value on the target only (D13). A change to either value alone
is not a narrowing for connection teardown (`isNarrowing`): neither value is
exercised over a connection.

## Wire (`runner/protocol/message.bgn`)

Each limit is a value plus a `limited` bit (0 = unbounded), so the zero value
of every new field is "unbounded", which is D8's default and the legacy replay.

| Format | Change |
| --- | --- |
| `SubmitRequest` | append `spawn_depth :u8`, `max_descendants :u16`, and a byte `spawn_depth_limited :u1`, `max_descendants_limited :u1`, `spawn_depth_present :u1`, `max_descendants_present :u1`, `reserved :u4`. The `_present` bits are resume-only, as the delegation bound's are |
| `OpenInteractiveRequest` | the same fields, before the conditional `x11` block |
| `SetCapsRequest` | the same fields; here the `_present` bits say which value `caps set` writes |
| `SubmitStatus`, `OpenInteractiveStatus` | append `spawn_limit`. Both responses already carry `error_msg` |
| `TaskInfo` | append `spawn_depth`, `max_descendants`, both `limited` bits, and `descendants :u32` (the current `desc(A)`, so `ls` can show "3/5" without a second walk on the client) |
| `WhoAmIResponse` | append the same values as `TaskInfo`, and `spawn_room :u32` with `spawn_room_limited :u1` (D16). For the operator every `limited` bit is 0, the root's values |

Rollout: server first, then every client (no skew guard, as for the
delegation bound). Run `scripts/wire-skew-check.sh` before landing.

## Persistence

- `TaskEntry` gains the two values with their `limited` flags.
- `WALEvent` / `walEventJSON` gain `spawn_depth`, `spawn_depth_limited`,
  `max_descendants`, `max_descendants_limited`, all `omitempty`, so a legacy
  record replays unbounded (D8). `TestWALEventJSONRoundTripCopiesEveryField`
  enforces the shadow struct.
- Written on `task_created` AND in the `task_caps_changed` snapshot
  (`writeCapsChangedLocked`, `server/taskstore.go:364`), which replay assigns
  wholesale. The delegation bound spec records why: without it, the first
  `caps set` of caps only would reset the limits to unbounded at the next
  restart.
- `descendants` and `spawn_room` are computed, never persisted.

## Surfaces

The `surface-parity-checklist` is walked item by item while writing the plan.

| Surface | What |
| --- | --- |
| CLI verb | `--spawn-depth`, `--max-descendants` in `spawnFlags` (`cli/verb/spawn.go`) on `submit` and `session new`, each with a presence field; both conflict with `--exact`. Also on `caps set` (its at-least-one rule widens) and `caps set-defaults` |
| Converters | one parser for `N|unbounded` per range (u8, u16) in `cli/verb/authority.go` |
| Shared funnel | `cli.SessionOpts` and `SetCapsOpts` gain the values and presence bits; `buildSubmitRequest` / `buildOpenInteractiveRequest` set them |
| Session defaults | TUI `App` and WebUI spawn defaults, fresh spawns only (D14) |
| CLI errors | `spawn_limit` prints `error_msg` on `submit` and `session new` (native and wasm) |
| `ls` rows | `spawn-depth=<n|unbounded> max-descendants=<n|unbounded> descendants=<k>` on every row, the unbounded case included |
| `ls --json`, `session ls` | `spawn_depth`, `max_descendants` (`null` = unbounded), `descendants` |
| `whoami` | both values and `room=<k|unbounded>`; the operator prints `unbounded` for all three |
| `caps` catalog | a SPAWN LIMITS section in `WriteCaps` (`cli/verb/caps.go`): the two rules, the defaults, that a place is freed only by prune, and D19's caution. The `prune` entry gains D10's sentence: holding `prune` over a limited subtree includes freeing its places and, through an ended intermediate task, lifting its limits |
| `whatif` (`cli/whatif.go`) | the predicted child's `spawn_depth` |
| `caps audit`, `reach` | **omitted**: they report bits and target sets; neither limit is either |
| TUI detail popup | `formatTaskDetail` (`tui/detail.go`): a `spawn limits` line |
| TUI task table | **omitted**: width-bound, as the delegation bound decided |
| TUI authority picker | spawn, re-grant and set-defaults modes gain two numeric inputs with an unbounded toggle; re-grant prefills from the task |
| WebUI task row meta | the `ls` row's three values |
| WebUI compose panel, re-grant dialog | the same two inputs, prefilled from raw values |
| wasm snapshot | `spawnDepth` / `spawnDepthLimited`, `maxDescendants` / `maxDescendantsLimited`, `descendants` raw |
| WebUI command input | the new flags through the wasm bridge's parse |
| README | "Capabilities and scope": the two limits, defaults, flags, D10's note on `prune`, and D19's caution |
| Agent skill | `runner/agentskills/supervising-workers/SKILL.md` and both mirrors: check `whoami`'s `room` before promising to start workers; ending or cancelling a worker does NOT free its place. On a `spawn_limit` refusal, in this order (D18): resume a worker you already have; if that cannot serve, ask the operator — to prune or to raise the limit. A holder of `prune` does not prune as a first resort: it asks the operator explicitly and prunes only once approved. A prune without approval can be undone (`restore`), but is not the expected path. Never retry the spawn as is |

## Testing

Unit, server:

- depth: `P.spawn_depth` ∈ {unbounded, 0, 1, 3} × requested ∈ {omitted, 0, 5}
  → refusal or the child's value; a chain operator→A(3)→B→C→D, D refused
- descendants: A with `max_descendants = 2`; two children ok, a third refused;
  one ends → a third still refused; that one pruned → a third ok; a grandchild
  counts against A; a child given `max_descendants = 10` still stops at A's 2
  (D6)
- the refusal text names the distance (self, parent, 3 levels up)
- resume of an ended task under a full A → ok, and the count is unchanged (D9)
- an operator creating a task while some subtree is full → ok (no ancestors)
- `caps set-parent` moving a task into a full A → ok, A over limit; the next
  creation under A → refused (D4); `--swap` leaves both `spawn_depth`s
  unchanged (D5)
- prune (D10, pinning the documented behaviour): A full, an ended B under A
  with two children; prune B → A has three places again
- concurrency: two creations racing for A's last place → exactly one ok (D11),
  run under `-race`
- `caps set`: each value alone writes the target only; with `--cascade`,
  descendants' limits are unchanged and caps/scope are clamped as today; a
  limit-only change closes no connection (D13)
- WAL: a record without the keys replays unbounded; `task_caps_changed` after
  a caps-only `caps set` keeps both limits across a restart
- the mapper completeness test covers `TaskInfo` / `WhoAmIResponse`

Unit, cli: both flags and `unbounded` on all spawn verbs, `caps set`,
`caps set-defaults`; the `--exact` conflicts.

Live, against `scripts/dummy-harness.sh`, in the spelling the help prints:

1. Operator: `submit --caps spawn,resume,cancel --delegate-caps spawn
   --spawn-depth 2 --max-descendants 3 …` → agent A. From A: `whoami` shows
   `spawn-depth=2 max-descendants=3 room=3`.
2. From A: three `session new -d` → ok; a fourth → refused, `spawn_limit`,
   text naming "your own" limit and `3/3`.
3. From A: cancel one child, then a new spawn → still refused; `session new -d
   --resume <that child>` → ok.
4. Operator: prune the cancelled child (cancel it again first if the resume
   left it live); from A: a new spawn → ok.
5. From child B (holding `spawn`, `spawn_depth = 1`): a spawn → refused by A's
   limit, text "your parent's". After the operator frees a place: ok, and the
   grandchild's `whoami` shows `spawn-depth=0`; its own spawn → refused on
   depth.
6. TUI and WebUI: spawn with the two inputs set, re-grant prefill,
   `caps set-defaults --max-descendants …` reaching a fresh spawn, `ls` row
   values.

Before landing: `make` targets, `scripts/wire-skew-check.sh`.

## Non-goals and limits

- **Not a concurrency limit.** Every record counts, running or not; nothing
  here bounds how many run at once separately from how many exist.
- **No rate limit**, and no per-runner or per-host capacity: the limits count
  tasks in a subtree, wherever they run.
- **Not processes.** An agent's in-process subagents (e.g. Claude Code's own
  Task tool) and `exec run` processes are not harness tasks and are not
  counted.
- **Proxying.** A child without `spawn` can ask its parent over the agentboard
  to spawn for it. That spawn is the parent's and is counted in the parent's
  subtree, so it is inside the same limits.
- **`caps set-parent` can leave a subtree over its limit** (D4); it is reserved
  to the operator today and this design does not change that.
- **`prune` frees places, and pruning an ended intermediate task lifts limits**
  for the records below it (D10). The intended use grants agents no `prune`.
- **Tightening `spawn_depth` does not reach existing descendants** (D13).

## Amendment — as implemented (2026-11-09)

Where this spec was silent, or named a shape the code could not take, the
implementation (`docs/superpowers/plans/2026-11-09-spawn-limits.md`) decided as
follows.

- **One wire format, `SpawnLimit { limited :u1, reserved :u7, value :u24 }`**,
  replaces § Wire's separate value fields and `*_limited` bits, for both limits
  everywhere. `value` is u24 so the format is 4 bytes: embedded as a 3-byte
  format right after a bit-field group, it made brgen stop merging that group
  into one byte (`OpenInteractiveRequest`'s flag byte came out as one `uint8`
  per bit and `SetDelegateScopePresent` disappeared). Both limits therefore
  range 0–16777215, not § Terms' 0–255 / 0–65535.
- **Presence bits** take `SubmitRequest`'s and `SetCapsRequest`'s two reserved
  bits. In `OpenInteractiveRequest` the two limits sit before the flag byte,
  and the two presence bits share it with `delegate_scope_present`
  (`reserved2 :u5`).
- **`TaskStore.CreateWithinLimits`** is the checked creation, and the only one
  production code calls (`TestOnlyCreateWithinLimitsInProduction`);
  `TaskStore.Create` stays unchecked for tests that build trees directly.
- **`TaskInfo.descendants`** is filled by the list handler from one
  `DescendantCounts()` per request; `toTaskInfo` does not take it.
- **`caps what-if --spawn`** takes `--spawn-depth` / `--max-descendants` and
  predicts the child's written values only. It does not simulate the
  descendant-limit refusal, which depends on tasks a confined listing may not
  show.
- **The WebUI bridge** takes each limit as the flags' grammar string
  (`"unbounded"` or a number; `""` is unbounded), parsed by
  `verb.ParseSpawnLimit` like every other surface.
- **The wasm snapshot** carries each limit as one object `{label, limited,
  value}` (`spawnDepth`, `maxDescendants`) plus `descendants`, not the flat
  `spawnDepthLimited` / `maxDescendantsLimited` names § Surfaces listed.
