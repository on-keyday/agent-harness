# Delegation bound — what a task may hand to the tasks it creates — design

## Problem

The server separates nothing between what a task may DO and what it may GIVE.
A child's caps are `creator & requested` (`intersectCaps`,
`server/capabilities.go:192-196`, applied at the four grant sites
`server/task_handler.go:1130` fresh submit, `:1276` submit resume, `:1638`
interactive resume, `:1659` interactive fresh). Its scope is the requested one
clamped to the creator's, per bit the child will hold (`attenuateScope`,
`server/capabilities.go:445-600`, as fixed in `3e5ae4ab`). The only bound on a
grant is therefore the granter's own authority.

- **P1.** The operator wants a parent's children to hold nothing, but the
  parent holds bits. An omitted `--caps` is `none` (`cli/session_opts.go:18`),
  yet the parent is an LLM that writes its own `--caps` on each spawn, and
  whatever it names out of its own set is granted. The operator's intent "use
  `exec_control` yourself, hand it to nobody" is not expressible; the one lever
  is withholding `spawn`, which forbids children altogether.
- **P2.** The same holds for scope. A parent whose visibility rank is global
  can give a child global visibility, so a child holding no caps still lists
  every task on the server; and any id inside the parent's set can be written
  into a child's `ids` / `vis-ids` (`attenuateScope`'s id checks).
- **P3.** Once a restriction exists, the operator needs to relax it on a live
  task — "now let it give the child X" — without restarting anything. Every
  other part of a task's authority is already editable live through `caps set`
  (`server/set_caps_handler.go`).

## Terms

**Delegation bound** — two values stored on every task, beside its caps and
scope, written by whoever creates or re-grants the task:

- `delegate_caps` — a capability mask: the bits this task may put into the
  caps of a task it grants to.
- `delegate_scope` — a `TaskScope` read as a CEILING on the scope this task may
  put on a task it grants to. Its two ranks cap the child's action base and
  visibility rank. Read as an ordinary scope around THIS task — base, ids,
  `exclude_self` — it also names the task ids this task may write into a
  child's scope: action ids from its action set, view-only ids from its
  visible set (§ The rule).

IN: a task's delegation bound limits every authority that task WRITES onto
another task: a fresh spawn (submit, open interactive), and a resume that
re-grants caps, scope or the delegation bound itself. It also limits the
delegation bound it gives that task, so the limit carries down the chain.

OUT: it does not limit what the task itself may do — `caps` and `scope` still
decide that, unchanged. It does not limit the operator. It does not limit an
operator's `caps set`, `caps set-parent`, or a resume without re-grant (which
keeps the recorded authority, `resume` bit doc in `runner/protocol/message.bgn`).

## Decisions taken

**operator** = the human chose it in conversation (2026-10-22). **this spec** =
the author chose it while writing; those rows are the ones to check.

| # | Decision | Decided by |
| --- | --- | --- |
| D1 | A task carries a delegation bound separate from its own caps and scope; the operator writes it per task | operator |
| D2 | The default bound delegates no bits: `delegate_caps = none`, `delegate_scope = subtree` with no ids. A parent spawned without one can still spawn (if it holds `spawn`), and every child it makes holds no caps. The ids it may write into a child's scope are its own and its descendants' (D21, D23) | operator |
| D3 | The bound is editable on a live task, through `caps set` — two more flags on it, not a new verb — and `--cascade` clamps descendants to it | operator (accepted the proposal) |
| D4 | The bound covers scope as well as caps: visibility rank and ids too (P2) | operator |
| D5 | Ids beyond what the ceiling's base covers are delegated per id, from a list the operator writes — not one switch for "the parent's ids" | operator |
| D6 | Two id lists, like a scope's own: ids that may be handed down as action ids, and ids that may be handed down as view-only ids | operator |
| D7 | Delivered as one change, caps and scope halves together | operator (accepted the playback) |
| D8 | `caps set-defaults` / `scope` (TUI, WebUI) carry a default bound for the session's fresh spawns, as they carry caps and scope | operator (accepted the playback) |
| D9 | Tasks recorded before this change replay with the default bound (D2). No migration; the operator re-grants with `caps set` where a supervisor needs to hand bits down | operator (accepted the playback) |
| D10 | No per-capability bound: `--scope-for` overrides are bounded by the same ceiling as the base scope (their base by the action-base ceiling, their ids by the action-id list) | operator (accepted the playback) |
| D11 | `delegate_scope` reuses the `TaskScope` format and the whole `--scope` grammar, so the ceiling has separate action and visibility ranks. The playback described one rank; `subtree` written alone still means one rank for both, because visibility follows the action base when its half is omitted | this spec |
| D12 | A child's caps = `creator.caps ∩ creator.delegate_caps ∩ requested`. Bits and ranks above the bound are CLAMPED silently; ids outside it are REFUSED (`scope_not_permitted`). This is the existing split — `intersectCaps` and `minScopeBase` clamp, `attenuateScope` refuses ids (its doc comment, `server/capabilities.go:465-468`) | this spec |
| D13 | No stored invariant `delegate_caps ⊆ caps`. The bound is kept as written; every use intersects with the holder's caps. Narrowing a task's caps therefore never needs to rewrite its bound, and a bit granted back later re-enables delegating it if the bound still names it | this spec |
| D14 | Changing only a task's bound is not a narrowing for connection teardown (`isNarrowing`): the bound is not exercised over any connection. A cascade that clamps a descendant's caps or scope still tears down that descendant's connections, as today | this spec |
| D15 | Flags `--delegate-caps` (caps grammar) and `--delegate-scope` (the scope grammar, `descendants` and `-self` forms included, D22). Display keys `delegate=` and `delegate-scope=` | this spec |
| D16 | Two presence bits, one per half, on every request that can re-grant (resume, `caps set`), mirroring `resume_caps_override` / `scope_present` | this spec |
| D17 | An operator connection's own bound is unbounded (`all`, global, any id), as its caps are `all` (`callerCaps`, `server/capabilities.go:201-205`) | this spec |
| D18 | No skew guard; rollout is server first, then every client, as for the resume-caps change | this spec |
| D19 | `--cascade` clamps descendants to the target's bound only when that `caps set` WRITES a bound half (either new presence bit). A caps- or scope-only cascade clamps exactly as today. Reason: D9 gives every pre-change supervisor the empty bound, so an unconditional clamp would turn the first ordinary `caps set <supervisor> --caps … --cascade` after landing into `caps=none` for all its workers, with their connections torn down | this spec (builder check) |
| D20 | `OpenInteractiveResponse` gains an `error_msg`, as `SubmitResponse` has, so the ceiling refusal text reaches `session new` — the path agents spawn workers through. Today that path prints one fixed text from the status (`cli/open_interactive_native.go:148-149`) | this spec (builder check) |
| D21 | The ceiling's ids are read around the GRANTING task, not the child: a `subtree` ceiling lets a parent write its own id and its descendants' ids into a child's scope with no list. The builder check showed that reading it around the child (a new task has no descendants, so no id passes) refuses the two supervisor recipes the agent skill documents (`runner/agentskills/supervising-workers/SKILL.md:427` own id, `:454` sibling ids) unless the operator lists each id, a sibling's only after it exists | operator |
| D22 | `exclude_self` on the ceiling keeps the granting task's OWN id out of the action ids it may hand down (`--delegate-scope descendants`); it may still hand it down as a view-only id. Same meaning `exclude_self` has on a task's scope — out of the action set, never out of sight. It closes the upward path: a child holding `exec_cowrite` on its parent can type into a session that holds more authority than the child | operator |
| D23 | The default ceiling is `subtree` with the parent's own id included, the zero value. The upward path needs a bit in `delegate_caps`, which only the operator adds; whoever adds it chooses `descendants` there. A `descendants` default would not be the zero value, so the format reuse (D11) and the legacy replay (D9) would both need a presence bit | operator (accepted the recommendation) |

## The rule

Notation: `P` is the granting task (the creator of a fresh task, or the resumer
on a re-granting resume). `C = P.delegate_scope`. `rank` orders bases
`none < subtree < global` (`scopeBaseRank`, `server/scope.go`). `min` is
`minScopeBase`.

`C` is used two ways:

- **As ranks**, it caps the bases of the child's scope. A child's base is
  read around the child, as every scope is.
- **As a scope around `P`** (D21), it names the ids `P` may write into a
  child's scope. Both sets come from `newGrantReach(P, C, children)` — the
  helper spawn and cascade already measure a granter through
  (`server/capabilities.go`, since `a839cacf`) — with `C` in place of `P`'s
  own scope, so the scope semantics live in one place:
  - `actD` = the ids its `reaches(id, bit)` admits: inside `C`'s action set
    `(C.exclude_self ? ∅ : {P}) ∪ baseSet(C.base) ∪ C.ids` AND its visible
    set, because every id a child is given joins the child's visible set.
    The ids `P` may hand down as action ids.
  - `visD` = the ids `reaches(id, none)` admits: `C`'s visible set `{P} ∪
    baseSet(C.VisRank()) ∪ C.vis_ids ∪ C.ids`. The ids `P` may hand down as
    view-only ids. `P` is always in it (D22).

  Either is "all" when its rank is global. `C` carries no overrides, so
  `ForCap` returns its base for every bit and the bit passed is immaterial.

With the default `C = subtree`: `actD = visD = {P} ∪ P's descendants`.
With `descendants`: `actD` drops `P`, `visD` keeps it. With `none`: `{P} ∪
C.ids`.

When `P` is an operator connection, none of the bound checks run (D17); the
requested values are written after `validateScope` alone, as today.

### Caps

`child.caps = P.caps ∩ P.delegate_caps ∩ requested_caps`. This replaces
`intersectCaps(creatorCaps, req.RequestedCaps)` at all four grant sites AND in
the mask `attenuateScope` is handed (`server/task_handler.go:1095`, `:1493`,
and `resumedCaps` in `server/capabilities.go`), through one helper, so the
scope is clamped for exactly the bits the task ends up holding.

### Scope

The existing `attenuateScope` runs unchanged, then the ceiling applies to its
output:

| Field of the child's scope | Rule |
| --- | --- |
| `base` | `min(base, C.base)` |
| visibility rank | `min(child.VisRank(), C.VisRank())`; written with an explicit `vis_base_present` only when the result differs from the clamped base, as `attenuateScope` and `clampScopeUnder` do |
| each override's `base` | `min(override.base, C.base)` |
| `ids`, each override's `ids` | each id must be in `actD`; else refused |
| `vis_ids` | each id must be in `visD`; else refused |
| `exclude_self` | untouched; it only ever narrows. The child acting on itself is not reach toward anyone else, and is what every task has today |

`attenuateScope` returns early only for an operator creator (since
`3e5ae4ab`), which D17 exempts from the bound as well, so the ceiling always
sees `attenuateScope`'s clamped output.

An id is admitted only when BOTH checks pass: the existing one (inside `P`'s
own set) and the ceiling's (`actD` / `visD`).

### The child's own bound

| Field | Rule |
| --- | --- |
| `delegate_caps` | `requested_delegate_caps ∩ P.delegate_caps` |
| `delegate_scope.base` | `min(requested.base, C.base)` |
| `delegate_scope` visibility rank | `min(requested.VisRank(), C.VisRank())`, written as above |
| `delegate_scope.ids` | each must be in `actD`; else refused |
| `delegate_scope.vis_ids` | each must be in `visD`; else refused |
| `delegate_scope.exclude_self` | set when requested, and FORCED on when the child is not in `actD`. A new child is in `actD` exactly when `scopeCoversNewChild(C, …)` holds (base subtree or global, `server/capabilities.go:149-152`). Without this, under `C = none` the child could hand its own id to a grandchild although `P` may not hand that id to anyone |

This makes the chain monotonic: the ids a grandchild can receive are inside
the child's `actD` / `visD`, which are inside `P`'s, because the child's
subtree is inside `P`'s and every list entry was admitted from `P`'s sets.

### Validation (all callers, operator included)

`validateScope` runs over the bound as over any scope; with no overrides
(D10) its only check is the canonical `vis_base` encoding. A refusal is
`scope_not_permitted` on spawn and resume, `invalid_scope` on `caps set`.

### Refusal text

`attenuateScope`'s `offender` already distinguishes "invalid scope: …" from an
out-of-reach id (`scopeRejectionMsg`, `server/task_handler.go`). A ceiling
refusal gets its own prefix, rendered as `scope id <id> is outside what you may
delegate (see whoami: delegate-scope)` — an agent reading "outside your own
scope" for an id it can act on itself would go looking at the wrong field.

### Resume

- `resume_caps_override = 1` → caps by the Caps rule with `P` = the resumer.
- `scope_present = 1` → scope by the Scope rule with `P` = the resumer.
- `delegate_caps_present = 1` / `delegate_scope_present = 1` → that half of the
  bound by the child's-own-bound rule with `P` = the resumer.
- Any bit 0 → that value is kept as recorded (D8 of the resume-caps spec,
  unchanged).
- `resume_exact = 1` requires both new presence bits 0; `exactFields` gains
  them and `exactMismatch` names `--delegate-caps` / `--delegate-scope`.

### `caps set` and `--cascade`

`caps set` stays operator-only. With `delegate_caps_present` /
`delegate_scope_present` it writes the bound (after `validateScope`, no
attenuation — the operator may grant anything).

With `--cascade` AND at least one bound presence bit set (D19), every
descendant `d` of the target `T` (target excluded) is clamped against `T`
after the change, in addition to today's clamp. Without a bound presence bit
the cascade is today's, unchanged:

- `d.caps &= T.caps ∩ T.delegate_caps` (today: `& T.caps`, line 98)
- `d.scope`: today's `clampScopeUnder`, then the ceiling table above with
  `P = T`, `C = T.delegate_scope`, where a refused id is DROPPED instead (a
  cascade has no one to refuse; `clampScopeUnder` already drops)
- `d.delegate_caps &= T.delegate_caps`; `d.delegate_scope` clamped by the
  child's-own-bound table with `P = T`, ids dropped instead of refused, and
  `exclude_self` forced on when `d` is not in `T`'s `actD`

Every descendant is clamped against `T` alone, as the caps cascade does
today. That stays inside each intermediate parent's bound for the ranks and
masks (`min` and `∩` distribute). For ids it is the weaker statement — inside
`T`'s sets, not necessarily inside an intermediate parent's narrower list —
which is the same guarantee `clampScopeUnder` gives a descendant's scope ids.

## Wire (`runner/protocol/message.bgn`)

| Format | Change |
| --- | --- |
| `SubmitRequest` | append `delegate_caps :Capability`, `delegate_scope :TaskScope`; `delegate_caps_present :u1`, `delegate_scope_present :u1` from `reserved :u4` (→ `u2`). The presence bits are resume-only; on create both values always apply, and their zero is the default bound |
| `OpenInteractiveRequest` | the same two values after `overrides`, before the conditional `x11` block. Its flag byte has one reserved bit left, so: `delegate_caps_present :u1` takes it, and a new byte holds `delegate_scope_present :u1`, `reserved2 :u7` |
| `OpenInteractiveResponse` | append `error_msg_len :u16`, `error_msg :[error_msg_len]u8` after the conditional `stream_id` (D20). Set on `scope_not_permitted`; the native and wasm clients print it when non-empty and fall back to today's fixed text otherwise |
| `SetCapsRequest` | append the two values; the two presence bits from `reserved :u4` (→ `u2`) |
| `TaskInfo` | append `delegate_caps :Capability`, `delegate_scope :TaskScope` |
| `WhoAmIResponse` | append the same two, after `server_dirty` / `reserved3` |

The zero values are the default bound: `Capability.none` is 0 and
`ScopeBase.subtree` is 0, so a request that omits them, a zero Go struct and a
legacy WAL record all mean D2.

`SubmitRequest.requested_caps`'s comment changes: `Capability.all` now means
"everything the creator may delegate", not "the creator's full set".

Rollout (D18): server first, then every client. Run
`scripts/wire-skew-check.sh` before landing.

## Persistence

- `TaskEntry` gains `DelegateCaps protocol.Capability` and `DelegateScope Scope`.
- `WALEvent` and `walEventJSON` gain `delegate_caps`, `delegate_scope_base`,
  `delegate_scope_vis_base`, `delegate_scope_vis_base_present`,
  `delegate_scope_ids`, `delegate_scope_vis_ids`, all `omitempty`, so a legacy
  record replays as D2 (D9). `TestWALEventJSONRoundTripCopiesEveryField`
  enforces the shadow struct.
- Written on `task_created` AND on every `task_caps_changed`.
  `writeCapsChangedLocked` (`server/taskstore.go:264`) writes a SNAPSHOT that
  replay assigns wholesale (`server/taskstore.go:1240-1244`); if it does not
  carry the bound, every `caps set` and re-granting resume would reset the bound
  to D2 at the next restart. Its signature takes the bound, and its two callers
  (`:361`, `:551`) pass the entry's current one.
- The projection lives beside `Scope.applyToWAL` / `scopeFromWAL` in
  `server/scope.go`, under the `delegate_` key prefix. `scopeFromWAL`'s legacy
  `board_observe` → global-visibility migration does NOT apply to the bound: it
  exists for records that predate the visibility axis, and every bound is newer
  than that axis.

## Surfaces

The `surface-parity-checklist` is walked item by item while writing the plan,
and this table is checked back against the code at the end (item 39).

| Surface | What |
| --- | --- |
| CLI verb | `--delegate-caps`, `--delegate-scope` in `spawnFlags` (`cli/verb/spawn.go`) on `submit` and `session new` (the `interactive` verb was removed in `17c8867b`), each with a presence field. Both conflict with `--exact`. Also on `caps set` (the `AtLeastOne` rule widens to the four flags) and `caps set-defaults` / `scope` |
| Converters | `--delegate-caps` uses `parseCapsFlag`, `--delegate-scope` uses `parseScopeFlag` (`cli/verb/authority.go`); no new grammar |
| Shared funnel | `cli.SessionOpts` gains `DelegateCaps`, `DelegateScope` and the two presence bits; `buildSubmitRequest` / `buildOpenInteractiveRequest` set them; `SetCapsOpts` likewise. TUI request builds go through `Authority.opts` (checklist item 28a) |
| Session defaults | TUI `App` gains the session's default bound beside `sessionCaps` / `sessionScope`; WebUI beside `spawnCaps` / `spawnScope`. Fresh spawns only (checklist item 26) |
| CLI errors | the ceiling refusal text above, on submit and — through D20's `error_msg` — on `session new` (`cli/open_interactive_native.go` and its wasm twin); `resume_exact_mismatch` naming the new flags |
| `ls` rows | `delegate=<caps> delegate-scope=<scope>` on every row, the default included (item 31) |
| `ls --json`, `session ls` | `delegate_caps`, `delegate_scope` in the same forms as `caps`, `scope` |
| `whoami` | both values on both branches; the operator branch prints `delegate=all delegate-scope=global` |
| `caps` catalog | a DELEGATION section in `WriteCaps` (`cli/verb/caps.go`) stating the rule, the default and that `--delegate-scope` takes the scope grammar |
| `caps audit` | a holder line is marked `deleg` when the task holds the bit AND its `delegate_caps` contains it — "who can spread X" |
| TUI task table | **omitted**: the table is width-bound (checklist item 18a's note); the detail popup carries the values |
| TUI detail popup | `formatTaskDetail` (`tui/detail.go`): a `delegate` line and a `delegate-scope` line |
| TUI authority picker | spawn, re-grant and set-defaults modes gain a delegation section built from the existing controls: caps chips, base and visibility radio rows, the task checklist for action ids and for view-only ids. Re-grant prefills from the task's current bound |
| TUI keys `r`/`R`/`u`/`U` | **omitted**: they keep the recorded authority and never re-grant |
| WebUI task row meta / task sheet | both values |
| WebUI compose panel and re-grant dialog | a delegation section of the same controls (`buildCapChips`, base radios, `buildTaskChecklist`), serialised through `cli.ScopeSpec` over the bridge (item 32), prefilled from raw values |
| wasm snapshot | `delegateCaps` label + `delegateCapsBits`, `delegateScope` label + `delegateScopeBase` / `delegateScopeVisBase` / `delegateScopeIds` / `delegateScopeVisIds` raw (item 22) |
| WebUI command input | the new flags through the wasm bridge's parse |
| README | the "Capabilities and scope" section: the bound, its default, `--delegate-*`, and `caps set` editing it |
| Agent skill | `runner/agentskills/supervising-workers/SKILL.md` (and both mirrors), section "Capabilities and scope": a child receives `creator_caps ∩ delegate_caps ∩ requested`; by default you may delegate no bits, so check `whoami` before promising a worker a bit; an id refused as "outside what you may delegate" is the operator's to widen, not a bug. The `--caps all` sentence (`:408`) changes to "everything you may delegate". The own-id (`:427`) and sibling (`:454`) recipes stay valid for their ids under the default ceiling (D21), and gain "the bit they grant — e.g. `board_peer` — must be in your `delegate`, or it is dropped"; under `descendants` your own id can be handed down only as a view-only id |
| Spec | this file; the resume-caps spec's D8 is unaffected |

## Testing

Unit, server:

- caps: {operator, agent with bound none, agent with bound `file_read`} ×
  requested {none, `file_read`, `file_read,cancel`, `all`} at each of the four
  grant sites → expected mask
- D13: bound names `cancel`, holder lacks it → child gets no `cancel`; `caps
  set` grants `cancel` to the holder → the next child can
- scope: bound `subtree` with the parent's vis global → child requesting
  `global/subtree` gets `subtree`
- ids under the default `C = subtree`: child `ids:<P>` and `ids:<sibling>` ok;
  `ids:X` refused for an X in `P`'s own scope ids but outside `P`'s subtree
- `C = descendants`: `ids:<P>` refused, `vis-ids:<P>` ok, `ids:<sibling>` ok
- `C = none`: only `<P>` and `C.ids`; the child's own bound gets
  `exclude_self` forced
- `C = subtree+ids:X` → `ids:X` ok; `C = subtree+vis-ids:Y` → `vis-ids:Y` ok,
  `ids:Y` refused; `C = global` → any id the parent can see; overrides' ids
  follow `actD`
- `actD` / `visD` are `grantReach` over `C`, not a copy of it (pinned over a
  real tree against `actionSet` / `visibleSetOf`)
- the child's own bound clamps to `P`'s; a grandchild of a default-bound child
  holds none
- resume: each presence bit alone writes only its half; exact resume with either
  new bit → `resume_exact_mismatch`
- `caps set`: each half alone; `invalid_scope` for a non-canonical
  `vis_base`; `--cascade` with a bound half clamps caps, scope and bound
  of a two-level subtree; `--cascade` with caps only leaves descendants above
  the bound untouched (D19); a bound-only change closes no connections (D14), a
  cascade that clamps a child's caps does
- WAL: a record without the keys replays as D2; `task_caps_changed` after a
  `caps set` of caps only keeps the bound across a restart
- the mapper completeness test covers `TaskInfo` / `WhoAmIResponse`

Unit, cli: both flags on all spawn verbs, `caps set`, `caps set-defaults`; the
`--exact` conflicts; `ScopeSpec` round trip of a bound, `descendants` included.

Live, against `scripts/dummy-harness.sh` (Pitfall 13), in the spelling the help
prints:

1. The operator submits agent A with `--caps spawn,file_read` and no bound.
   From A: `submit --caps file_read …` → the child's `ls` row reads
   `caps=none`.
2. The operator runs `caps set <A> --delegate-caps file_read`. From A: the same
   submit → `caps=file_read`.
3. The operator gives A `--scope subtree+ids:X` for a task X outside A's
   subtree. From A: a child with `--scope ids:X` → refused, "outside what you
   may delegate".
4. The operator runs `caps set <A> --delegate-scope subtree+vis-ids:X`. From A:
   a child with `--scope subtree+vis-ids:X` → ok, the child's `ls` lists X; a
   child with `--scope ids:X` → refused.
5. From A: child C1, then child C2 with `--scope subtree+vis-ids:<C1>` → ok,
   C2's `ls` lists C1. The operator runs `caps set <A> --delegate-caps
   exec_view --delegate-scope descendants`; from A, a child with `--caps
   exec_view --scope ids:<A>` → refused, `--scope ids:<C1>` → ok.
6. TUI and WebUI: spawn with the delegation section set, re-grant prefill, and
   `caps set-defaults --delegate-caps …` reaching a fresh spawn.

Before landing: `make` targets, `scripts/wire-skew-check.sh`.

## Non-goals and limits

- **Proxying is not prevented.** A child can ask its parent over the agentboard
  to act — talking to the parent needs no capability (`board_peer` doc,
  `message.bgn`) — and the parent acts under its own authority. The bound
  controls which tasks HOLD a bit, not what effects happen. What it does
  remove: depending on the parent's LLM choosing `--caps` correctly, and a
  child keeping a bit after its parent has ended.
- **Depth is not expressible beyond one level.** A bound without `spawn` makes
  every child a leaf. Whether a child's own bound contains `spawn` is its
  parent's choice inside the parent's bound, so "at most two levels" cannot be
  written with one bound.
- **The bound is checked when authority is written, not afterwards.**
  `caps set-parent` moves a task without touching its caps
  (`server/set_parent_handler.go:21-22`), so a reparented task may exceed its new
  parent's bound — the same property `child.caps ⊆ parent.caps` already has.
- **No bound on the operator** (D17), and none on a resume that keeps the
  recorded authority.
- **No per-capability bound** (D10).

## Found while writing

### Spawn-time attenuation, fixed before this change (`3e5ae4ab`)

`attenuateScope` let a child reach further than its creator in four ways:

1. It returned before any clamp when the creator's VISIBILITY rank was global,
   a leftover from when that rank was the action base. A `global/subtree`
   creator handed out `global`, and action ids it could only see.
2. Action ids were checked against the creator's VISIBLE set, so a view-only
   `vis-ids:<u>` became an action `ids:<u>` one spawn down.
3. The child's base and overrides were clamped to the creator's base only,
   ignoring a narrower `--scope-for` the creator holds for a bit the child
   receives.
4. The visibility rank was clamped to the creator's action base, not its
   visibility rank.

It now takes the mask the task will hold and bounds each held bit by the
creator's scope for that bit. `b78264bb` made that bound exact on spawn and
cascade alike (`grantReach.boundRanks`): an entry keeps its rank, and only the
held bits the creator narrows move into an override on the child, so a parent
narrowed with `--scope-for cancel=none` gives a `cancel,file_read` child
`global +cancel:none` rather than `none` for both. `0313d08c` widened "held"
to the bits a task EXERCISES (`exercisedBits`): an `exec_control` holder
attaches in view and cowrite mode under `exec_view`'s and `exec_cowrite`'s
own scopes, so those are bounded too. Cases 1 and 2 were first reproduced with a
throwaway unit test (deleted afterwards); the four regression tests in
`server/scope_attenuation_test.go` went red before the fix, and both cases
were re-run against a `scripts/dummy-harness.sh` instance built from the fix:
a `global/subtree` agent's `--scope global` child came out `global/subtree`,
and a `subtree+vis-ids:<u>` agent's `--scope ids:<u>` child was refused while
`--scope subtree+vis-ids:<u>` was accepted.

### The cascade, fixed before this change too (`a839cacf`)

`clampScopeUnder`, which `caps set --cascade` runs, had the same shape:

- it clamped a descendant's base to the parent's BASE only, so a parent
  narrowed with `--scope-for cancel=none` left a descendant's base-scope
  `cancel` as wide as before — case 3 above, on the operator's path;
- `filterIDs` kept only the parent's own id and its explicit `IDs`, so a
  descendant's ids inside the parent's subtree, or matching the parent's
  `vis_ids`, were dropped — narrower than the invariant needs;
- `isNarrowing` compared the base scope alone, so an override ranked down by a
  cascade left attaches opened under the wider grant running.

`attenuateScope`'s per-bit logic became `grantReach`, which `clampScopeUnder`
now measures through as well, and `isNarrowing` compares each still-held bit's
scope and the visible set. Three regression tests in `server/set_caps_test.go`
went red before the fix. Live, against `scripts/dummy-harness.sh`:
`caps set <A> --scope global --scope-for cancel=none --cascade` took A's
cancel-holding child from `subtree` to `subtree/none` and dropped a sibling's
`ids:<G1>` (A may no longer cancel G1); `caps set <B> --scope subtree
--cascade` kept a child's `ids:<sibling>`.

## Amendment — as implemented (2026-10-22)

Where this spec was silent or named code that has since moved, the
implementation (`docs/superpowers/plans/2026-10-22-delegation-bound.md`)
decided as follows.

- **The rule lives in `authority.Granter`** (`authority/delegate.go`): one task
  measured by its own scope (`Reach`) and by its bound read as a scope around it.
  `ChildCaps`, `ChildScope`, `ChildBound` and `ClampDescendant` are the four
  uses. Every server grant site goes through `TaskHandler.grantFor`;
  `intersectCaps` is gone and guarded against (`authority/no_copies_test.go`).
- **WAL keys gain `delegate_scope_exclude_self`.** § Persistence listed six keys
  and omitted it, but the bound has `exclude_self` (D22) and it is forced on
  under a `none` ceiling, so without the key that would be lost at a restart.
- **A refused id in the child's own bound** reads `delegate-scope id <id> is
  outside what you may delegate (see whoami: delegate-scope)`; § Refusal text
  fixed only the scope form.
- **`whoami` for an operator** carries `delegate_caps = all`,
  `delegate_scope = global` on the wire (D17), so both branches print what the
  wire says. A principal whose record is gone reports `none` / `none`, which is
  what enforcement treats it as.
- **`OpenInteractiveResponse.error_msg`** is also set on
  `resume_exact_mismatch`, naming the flag, as `SubmitResponse` does.
- **The WebUI caps-on-resume checkbox does not re-grant the bound.** It names
  caps and scope; only a typed `--delegate-caps` / `--delegate-scope` re-grants
  the bound on a resume.
- **Correction to § The child's own bound, `exclude_self` row.** "A new child
  is in `actD` exactly when `scopeCoversNewChild(C, …)` holds" is false when
  `C`'s visibility rank is below `subtree` (`subtree/none`, `global/none`):
  `actD` also requires the id to be in `C`'s visible set, and such a ceiling
  does not see a new child. The implementation forces `exclude_self` unless
  `C`'s base covers a new child AND its visibility rank is at least `subtree`,
  which agrees with what resume and cascade measure for an existing child.
  Found in the final review.
- **The two halves of the bound are independent on a resume.** A refusal of
  the `delegate_scope` half the request does not write leaves the
  `delegate_caps` half it does write unchanged (`ChildBound` returns the caps
  beside a scope error). Found in the final review.

