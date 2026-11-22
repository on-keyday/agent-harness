# Delegated re-grant and task rename — design

Date: 2026-11-22. Line references are to `main` at `b758943a`.

Status: implemented (plan `docs/superpowers/plans/2026-11-22-delegated-regrant-and-rename.md`).
As shipped, the TUI reaches `session rename` through its command line only;
the detail-popup prefill key in the plan was not added.

## Why

`caps set` is operator-only (`server/set_caps_handler.go:42`), so every live
change to a worker's authority — and, since D21 of the credential spec, every
change to a task's display name — waits on the operator. A supervisor agent
already has a bounded way to rewrite a descendant's authority: a resume with
`--caps` / `--scope` / `--delegate-*` re-grants through `grantFor`
(`server/task_handler.go`, delegation-bound spec `2026-10-22-delegation-bound-design.md`
§ Resume), bounded by the resumer's caps and delegation bound. That costs a
stop and a restart of the worker. This change offers the same bounded
re-grant on a live task, and a way to rename a task.

## Goals

- A task may run `caps set` on a **descendant**, live, with the result
  computed exactly as a re-granting resume computes it (`grantFor` with the
  caller as granter).
- `session rename <task-id> <name>` sets a task's display name (`name`, D21 of
  `2026-11-21-credential-tasks-design.md`), for the operator and, under the same
  rule, for a task on its descendants (never itself, R3).
- The operator's `caps set` is unchanged.

## Non-goals

- A task re-granting itself, an ancestor, or a non-descendant.
- Widening anything past what the caller could grant at spawn — including its
  own delegation bound; that stays the operator's.
- `caps set-parent` / `set_parent` for tasks (stays operator-only).
- New TUI/WebUI controls for task-initiated `caps set`: those surfaces are
  operator surfaces and already have the re-grant controls.

## Anti-goals

- **Self-amplification.** The reason `caps set` is operator-only
  (`set_caps_handler.go:14-16`): a bit that rewrites capabilities lets its
  holder grant itself all. Every path here must leave the caller's own
  authority and every ancestor's untouched.
- **Borrowed reach.** As in credential D18a: re-granting a non-descendant would
  let the caller act through that task's position in the tree. Descendants only.

## Facts the design rests on

- `grantFor(g, r, idHex)` with a non-empty `idHex` is the resume re-grant: it
  writes only present halves; caps through `resumedCaps` (`g.ChildCaps` when
  present); scope through `g.ChildScope` for the caps the task ends up holding;
  bound through `g.ChildBound`; `SpawnDepth` through `g.ChildSpawnDepth`;
  `MaxDescendants` only tightens (`MinLimit` with the recorded value).
- `h.granter(cid, callerCaps)` builds the caller's `Granter`; nil for the
  operator.
- The re-granting resume gate is the `resume` bit over the target
  (`resumeTargetAllowed`), not limited to descendants.
- The descendant walk used for credential re-key (D18a, `rekeyReaches`) uses
  the live creator index (`h.childIndex()` + `authority.DescendantsOf`).
- `set_caps_handler.go` already: refuses masked bits on credentials (D14),
  validates scope, cascades with `CascadeClamp`, tears down connections and
  data-plane grants on narrowing.
- No `task` verb family exists; operators manage tasks through `session …`
  (`session kill` is the alias of `cancel`). Since A3 every task but a
  credential is a session.

## Decisions

| # | Decision | Decided by |
|---|---|---|
| R1 | **Gate for a task caller: the `resume` bit with the target in its scope (`resumeTargetAllowed(cid, false, T)`) AND the target a strict descendant of the caller.** Same power as a re-granting resume today, minus the stop/restart; no new bit | operator, 2026-11-22 |
| R2 | **Shape: `caps set` opens to task callers** (no new verb). Operator caller → today's path unchanged. Task caller → R1, then `grantFor(h.granter(cid, callerCaps), r, T)` builds the update | operator, 2026-11-22 |
| R3 | **Rename: `session rename <task-id> <name>`**, a new TaskControl kind. Operator always; a task only on a target passing R1 — **never itself**. Name rules = issue's (`credentialNameRefusal`: ≤ 64 bytes, UTF-8, no C0/DEL/C1); empty clears it. **Reversal** of an earlier "a task may rename itself (a name carries no authority)": the name is the board sender label (`from_name`, credential D21), so a self-rename lets a hostile agent or a leaked credential key pose as a trusted task to readers who go by the name; a task's own state is what `progress_set` is for | operator, 2026-11-22 |
| R4 | **Refusals for a task caller**: target not found or failing R1 → `not_found` (no existence oracle, as credential re-key); target == caller → `not_found` too (self is never a re-grant target); a half `grantFor` refuses → `invalid_scope` carrying the authority package's message (it already distinguishes "outside your reach" from "outside what you may delegate"). `not_operator` is no longer answered to a task caller | this spec |
| R5 | **`--cascade` from a task caller** clamps the target's descendants exactly as today (`CascadeClamp` against the target after the change). It only ever narrows them, and they are the caller's descendants too | this spec |
| R6 | **`--keep-conns`, narrowing teardown, data-plane withdrawal**: unchanged; they apply to a task caller's narrowing as to the operator's | this spec |
| R7 | **Provenance**: a new `AuthoritySource.task_caps_set` with the caller as `AuthorityActor`; cascaded descendants keep `cascade` with the target as actor. Rename writes no authority provenance (not authority) | this spec |
| R8 | **Credential targets**: a descendant credential may be re-granted under R1 (it is the issuer's to re-key anyway, D18a); D14's masked bits still refused (`masked_cap`) | this spec |
| R9 | **What-if**: `authority/whatif.go` (`apply`, `Regrant`) predicts the same rule, so `caps whatif`-style previews agree with the server | this spec |
| R10 | **Persistence**: rename writes a `task_renamed` WAL event (TaskID, name); replay sets `Name`. A renamed task's past board messages keep the name they were published with (`from_name` is a copy) | this spec |

## Design

### `caps set` (server)

`handleSetCaps`:

1. Principal zero → today's operator path, unchanged.
2. Otherwise: `T` must exist, `T != caller`, `T` a strict descendant of the
   caller (live creator index), and `resumeTargetAllowed(cid, false, T)`; any
   failure → `not_found`.
3. Masked-bit refusal on credentials as today.
4. Build `grantRequest` from the request's present halves (the same fields a
   resume carries) and call `grantFor(h.granter(cid, h.callerCaps(cid)), r,
   T)`; an error → `invalid_scope` + message.
5. `Tasks.SetCaps(T, u, AuthorityWrite{Source: task_caps_set, Actor: caller})`.
6. Cascade / narrowing teardown / data-plane withdrawal as today.

`SetCapsResponse` gains an appended `message` (length-prefixed text) for the
refusal text of step 4.

### `session rename` (server)

New `TaskControlKind.task_rename` with `TaskRenameRequest { task_id, name_len,
name }` → `TaskRenameResponse { status: ok | not_found | bad_name }`. Gate per
R3; name check per R3; `TaskStore.SetName(id, name)` writes `task_renamed`;
publish a task event so lists refresh. `requiredCap`: none centrally (gated in
the handler like set_caps; classified `capInHandler`).

### Surfaces

| Surface | `caps set` (task caller) | `session rename` |
|---|---|---|
| CLI | existing verb; an agent identity now gets results instead of `not_operator` | new verb row, CLI\|TUI\|WebUI |
| TUI | unchanged (operator surface) | cmdline verb; task detail popup action `n` "rename" prefilling the cmdline |
| WebUI | unchanged | command input; a "rename" action in the task sheet |
| Agent skill | `supervising-workers` "Capabilities and scope": live re-grant of a worker with `caps set`, bounded like a re-granting resume, descendants only | one line under task management |
| README | `caps set` section: who may call it | verb list |

## Wire

All appended: `SetCapsResponse.message`; `AuthoritySource.task_caps_set`;
`TaskControlKind.task_rename` + request/response formats; `TaskRenameStatus`.
Run `scripts/wire-skew-check.sh`.

## Approaches considered

- Gate: `resume` bit + descendant (**chosen**) / a new `regrant` bit / bound
  only. A new bit protects nothing a re-granting resume does not already
  expose; bound-only lets every spawning parent rewrite its children.
- Shape: open `caps set` (**chosen**) / a new `caps regrant` verb (two entry
  points, every UI twice).
- Rename verb: `session rename` (**chosen**; the session family is where
  operators manage tasks) / top-level `rename` / `credential name`.

## Testing

- Task caller: descendant re-grant applies exactly what a re-granting resume
  would (table test over caps/scope/bound/limits, comparing with `grantFor`
  on the resume path); self, ancestor, sibling, non-descendant in scope →
  `not_found`; out of `resume` scope → `not_found`; widening past the caller's
  delegate → clamped/refused as at resume; credential masked bits →
  `masked_cap`; provenance `task_caps_set` + actor.
- Operator path unchanged (existing set_caps tests stay green).
- `--cascade` from a task narrows the target's descendants; narrowing closes
  the target's connections.
- Rename: operator any task; task descendant under R1; task self →
  `not_found`; non-descendant → `not_found`; bad name → `bad_name`; WAL replay keeps the name; `ls` / TUI /
  WebUI show it.
- whatif agrees with the server on a sample of re-grants.
- Dummy-harness: an agent identity re-grants its worker live and renames it.
