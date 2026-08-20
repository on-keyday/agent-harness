# Per-capability scope: visibility as an axis, one target set per verb, and a detachable `self`

Date: 2026-08-20

Supersedes the scope model of
[`2026-08-13-task-scoped-caps-design.md`](2026-08-13-task-scoped-caps-design.md)
(**the base spec** below). Its *enforcement* design — the `authorize` choke
point, "no such task" as the out-of-scope answer, `caps set` gated on operator
identity rather than a bit, the §8 wire-skew procedure — is unchanged and still
authoritative. What changes is the shape of the value being enforced.

## Problem

### 1. One scope per task cannot separate reading from writing

The base spec gives a task exactly one `TaskScope`, shared by every bit in its
mask. The requirement it cannot express:

> This worker may read its own session, but may not write to it.

`exec_view` against self and `exec_cowrite` not against self are two target
sets for one task, and there is one field to hold them. The same coarseness
covers `file_read` versus `file_write`, and `git_query` versus `cancel`.

### 2. `self` is unconditional, so no scope value can exclude it

The base spec defines the effective set as `{self} ∪ baseSet(base) ∪ ids` and
states that *"`self` is unconditional"*. `self` is joined **outside**
`baseSet`, so a new `ScopeBase` value meaning "descendants without me" does not
work — the union re-adds it after the base resolves. Excluding self requires
touching the union, not the enum. This is worth recording because the obvious
patch is the one the code silently defeats.

### 3. `info_global` is a scope-shaped power wearing a capability bit

`Capability_InfoGlobal` widens the set of tasks a caller may see. That is a
target-set question, which is what scope is for. Because `TaskScope` has no
visibility axis, the visibility widening was exiled into the capability mask,
and `visibleToCaller` (`server/capabilities.go`) became a bit short-circuiting
a scope resolution:

```go
func (h *TaskHandler) visibleToCaller(connID string) (all bool, allowed map[string]bool) {
    if h.lookupPrincipal(connID).Id == ([16]byte{}) { return true, nil }
    if hasCap(h.callerCaps(connID), protocol.Capability_InfoGlobal) { return true, nil }
    return h.scopeSet(connID)
}
```

The base spec then had to fence the two apart — *"It does NOT consult
`Capability_InfoGlobal` — that bit widens what may be SEEN, and folding it in
here would make it widen what may be DONE."* A fence is needed because the axes
overlap.

The two are **not** equivalent, and the difference is the shape of the problem:
`--scope global` sets `all = true` inside `scopeSet`, widening action *and*
visibility; `info_global` widens visibility alone. One axis lives in the scope
type, the other in the bitmask.

The bit is also overloaded: besides task visibility it gates connection
visibility and the agentboard's enumeration surfaces. Those are not one power,
and §8 separates them — the first two follow the axis, the third stays a verb
permission because the board is keyed by topic rather than by task.

### 4. Why the invariant stops holding by itself

Today "visible ⊇ actionable" is structural: `visibleToCaller` *is* the action
set, optionally replaced by everything. Split the action set per capability and
that accident disappears — which is why this design carries an explicit check
where the base spec needed none.

### 5. The motivating case

An event-stream agent
([`2026-08-20-event-stream-agent-design.md`](2026-08-20-event-stream-agent-design.md))
answers its own tool-approval requests unless something stops it, because the
default scope includes self. The alternative to this design is a hardcoded
"responder id ≠ requester id" rejection inside one handler — a rule the
permission model cannot see, state, or display.

## Design

### 1. Three axes in one value

```
# runner/protocol/message.bgn

format TaskScope:
    base             :ScopeBase   # unchanged: subtree = 0, none = 1, global = 2
    vis_base         :ScopeBase   # visibility rank
    exclude_self     :u1          # 0 = {self} is in the action set
    vis_base_present :u1          # 0 = visibility rank is `base`
    reserved         :u6
    vis_ids_len      :u16
    vis_ids          :[vis_ids_len]TaskID   # view-only extras; usually empty
    ids_len          :u16
    ids              :[ids_len]TaskID

format ScopeOverride:
    caps         :Capability      # a MASK: one or more bits, disjoint from every
                                  # other override's mask in the same list
    base         :ScopeBase
    exclude_self :u1
    reserved     :u7
    ids_len      :u16
    ids          :[ids_len]TaskID

# Appended after the existing `scope` field in each format that carries one:
    overrides_len :u8
    overrides     :[overrides_len]ScopeOverride
```

**Every field's zero value is the pre-change behaviour.** `base = subtree`,
`vis_base_present = 0` (visibility follows the action base, which is what
`visibleToCaller` does today without `info_global`), `exclude_self = 0` (self
included, as the base spec makes unconditional), empty lists. An all-zeros
`TaskScope` is today's default exactly, which is the same discipline the base
spec applied when it made `subtree` the zero value.

`exclude_self` is phrased negatively for that reason alone: `include_self`
would have had a zero value meaning "cannot touch my own worktree", and every
legacy WAL record would replay into it.

`ScopeOverride` is a separate, smaller format rather than a nested `TaskScope`:
an override has no visibility of its own, so carrying `vis_*` fields there
would create a state that must be validated as "always zero".

**An override carries a mask, and the masks are pairwise disjoint.** The
grouped case is the common one — "every write-ish bit gets `descendants`" — and
one bit per entry would spend six entries on it. A mask spends one.

Disjointness is what keeps that free. It is validated where the value is
written, by accumulating a union and rejecting the first intersection, and it
means resolution never needs a precedence rule: a capability is covered by at
most one override, so `override(cap)` stays a lookup. An empty mask is rejected
as well — an override matching nothing is dead weight and more likely a typo
than an intent.

A mask may name bits the task does not hold. Those stay **inert but retained**,
so a grant template can be reused across tasks holding different sets, and a
bit granted later by `caps set` picks up the standing override from that
moment. The direction is safe by construction: overrides only ever narrow, so a
retained one can never widen a newly granted bit.

**Overrides remain sparse.** Any bit may be covered; none is required to be. A
per-capability *default* table is deliberately not introduced — a second place
where authority is decided is a second place to get it wrong. `Capability_All`
is 15 bits, so disjointness bounds the list at 15 entries and `u8` covers it.

### 2. Resolution

```
visRank        = vis_base_present ? vis_base : base

visible        = {self} ∪ baseSet(visRank) ∪ vis_ids
                        ∪ ids ∪ ⋃ override.ids          ← automatic

effective(cap) = (s.exclude_self ? ∅ : {self}) ∪ baseSet(s.base) ∪ s.ids
                 where s = override(cap) if present, else the base scope
                 and override(cap) = the unique entry whose caps mask
                                     contains cap — unique because the
                                     masks are validated disjoint
```

Three consequences worth stating outright:

- **Action ids are visible without being repeated.** An id written into a grant
  was disclosed by the granter; hiding it from `ls` protects nothing. So
  `vis_ids` holds only view-only *extras* and is empty in the common case.
- **`self` is always visible**, even where `exclude_self` removes it from an
  action set. Seeing your own row is orientation, not authority; a task denied
  write access to itself still knows it exists.
- **`vis_ids` exists so that "watch X, touch nothing" is one grant.** Without
  it the only way to see X is to put X in an action `ids` and then override
  every held bit to exclude it — where one forgotten bit reaches X. That form
  fails open; this one fails closed.

### 3. The invariant, and the two checks that hold it

```
∀ cap:  effective(cap) ⊆ visible
```

Enforced at every write (spawn, `caps set`) by exactly two comparisons:

- `base` ranks at or below `visRank` under `none < subtree < global`
- every `override.base` ranks at or below `visRank`

Nothing about ids is checked, because every id in any action set is in
`visible` by construction (§2). Nothing about `exclude_self` is checked,
because it only ever removes. A violating value is rejected with
`scope_not_permitted` naming the offending bit — never silently clamped,
matching how the base spec treats an out-of-reach id.

The base spec's asymmetry survives intact: widening what may be **seen** never
widens what may be **done**. It is now a property of two fields in one value
rather than a fence between a bitmask and a scope.

### 4. Action never exceeds visibility, because `ls` must bound the reach

The check above refuses "visibility local, action global".

**An earlier draft of this section argued that from brute-force enumeration,
and that argument was wrong.** It said a task with a wide action rank would
"enumerate the server by attempting actions". Task ids are 128 random bits;
nothing guesses `60542da97c1115173918e86fd7ade954`. Corrected on the
observation that aiming at an unseen id is not something an attacker can
actually do.

The real reason is that **ids circulate outside the visibility set**, so a wide
action rank is exploitable without any guessing at all:

- Every agentboard message carries `from_task_id`
  (`agentboard/agentboard.bgn:149`) — a complete id, delivered to any task on
  the topic. Board delivery is data plane: it appears in no `requiredCap` entry
  and needs no capability.
- `whoami` returns the caller's `creator_task_id` ungated, and `ls` shows one
  redacted parent row under every base.
- Peers name ids to each other in message payloads as a matter of course; that
  is what the reply-topic convention is built on.

So a confined task accumulates real ids it was never granted. If its action
rank exceeded its visibility rank, it could act on every one of them, and the
operator reading `ls` would see none of it. **What the rule protects is not
secrecy of ids — it is that `ls` remains a complete statement of what a task
can reach.** Once those two can disagree, the scope column stops describing
the authority and starts describing a subset of it.

The out-of-scope answer stays `no_such_task` rather than `permission_denied`
for the base spec's own reason — a missing-capability answer about an unseen
target still distinguishes "exists" from "does not" for an id the caller
already holds — but that is a smaller property than this rule, not its
justification.

The neighbouring shape that **is** expressible, and is what such requests
usually want: `vis_base = none` with `override{cancel, ids:X}`. The task can
reach exactly `X` and itself, and `X` appears in its `ls` — so the reach is
still fully described by what it can see. **Un-enumerable and invisible are
different properties**, and it is almost always the first one that is wanted.

The reverse direction, visibility global with narrow action, is the common case
and is written `vis_base = global` with whatever `base` the task should act in.

### 5. `exclude_self` is a bit, not a fourth base

A bit composes with every base and with `ids:` — `ids:X` without self is
expressible, and so is `global` without self — whereas a fourth `ScopeBase`
value needs a partner for each existing one (6 values to encode 3 ranks × 2
self states). It also keeps the rank ordering one-dimensional, so the base
spec's permissiveness comparison and its fail-closed treatment of an
unrecognised byte are untouched.

`exclude_self` with `base = none` and no ids is the empty set: holds the bit,
can point it at nothing. That is a real state during a staged re-grant and is
left expressible.

### 6. The choke point gains an argument

```go
// scopeSet resolves the caller's effective TARGET set FOR ONE CAPABILITY.
// want == Capability_None resolves the VISIBILITY set: visRank, vis_ids, and
// every action id, per §2.
func (h *TaskHandler) scopeSet(connID string, want protocol.Capability) (all bool, allowed map[string]bool)

func (h *TaskHandler) authorize(connID string, want protocol.Capability, targetHex string) bool {
    if !hasCap(h.callerCaps(connID), want) {
        return false
    }
    all, allowed := h.scopeSet(connID, want)
    return all || allowed[targetHex]
}

func (h *TaskHandler) visibleToCaller(connID string) (all bool, allowed map[string]bool) {
    if h.lookupPrincipal(connID).Id == ([16]byte{}) {
        return true, nil
    }
    return h.scopeSet(connID, protocol.Capability_None)   // no capability bit consulted
}
```

The signature change is the enforcement mechanism: every existing call site
already holds the capability it is checking, so a kind that forgets to thread
it does not compile. `visibleToCaller` loses its `info_global` branch entirely
— visibility is now read from the value, like everything else.

The four INFO callers the base spec names — `handleList`, `handleGetTaskLog`,
`handleListPortForwards`, and the conns filter — keep their code verbatim and
resolve through the visibility axis. `KillPortForward` keeps its two-step
shape: `visibleToCaller` decides whether the server answers about the forward
at all, then the override-aware `authorize` decides whether the caller may kill
it, so a *visible* forward it may not act on still answers
`permission_denied`.

### 7. Worked examples

**Read self, do not write self** — the motivating case. `scope: base=subtree`,
override `{exec_cowrite, base=subtree, exclude_self=1}`:

| request | resolves through | answer |
|---|---|---|
| `ls` | visibility (`visRank = subtree`) | self + descendants |
| `session snapshot <self>` | `exec_view` → base | allowed |
| `session send <self>` | `exec_cowrite` → override, self removed | `no_such_task` |
| `session send <child>` | `exec_cowrite` → override | allowed |

**Named reach out of a blind base.** `scope: base=none, vis_base=none`,
override `{cancel, base=none, ids:X}`:

| request | answer |
|---|---|
| `ls` | self, `X`, plus the redacted parent hop |
| `cancel X` | cancelled |
| `cancel Y` | `no_such_task` |
| `session send X` | `no_such_task` — `exec_cowrite` resolves through `base=none` |

**See everything, act on the subtree** — today's `info_global`.
`scope: base=subtree, vis_base=global`. `ls` lists the server; `cancel` reaches
descendants only. Under the base spec this required a capability bit; here it
is two fields of one value.

**Watch a sibling without touching it.** `scope: base=subtree, vis_ids:[X]`.
`X` appears in `ls` and in nothing else — no override, no bit to forget.

### 8. `info_global` is retired as a *visibility* power, and keeps its bit

`visibleToCaller` no longer reads the bit. Everything else about the bit
survives under a new name (below), so both translations are **additive — they
grant the visibility rank and never clear a capability**:

- **Parsing.** `--caps info_global` grants the renamed bit *and* sets
  `vis_base = global` (unless `--scope` states a visibility rank explicitly),
  with a deprecation notice. `--caps all` continues to yield global visibility,
  because it contains the bit and the translation applies — preserving today's
  behaviour rather than silently narrowing every `all` grant.
- **WAL replay.** A legacy record whose caps contain the bit replays with
  `vis_base_present = 1, vis_base = global`, **keeping the bit**.

**Migration never clears a bit, and that rule is load-bearing.** An earlier
draft of this section had replay drop the bit once its visibility duty moved.
That would have silently revoked `board topics` / `board read` /
`board subscribers` from every task that had them, at the moment of a server
restart, with no operator action requesting it and no way to tell the result
apart from a deliberate narrowing afterwards. A replay is a reconstruction of
recorded state; the only authority it may add is the one the record already
implied, and the only thing it may remove is nothing.

The reverse direction is safe and is why the translation can be additive at
all: granting `vis_base = global` to a task that held the bit gives it exactly
the visibility it already had.

**The bit has three duties today, and they split two ways.** Enumerated from
every non-test use:

| duty | sites | goes to |
|---|---|---|
| task visibility | `visibleToCaller` (`server/capabilities.go:265`) | `vis_base` |
| connection visibility | `ListConns` (`server/task_handler.go:1610`) and the per-subscriber `conns_status` fanout (`server/server.go:363`) | `vis_base` |
| agentboard enumeration and read | `TaskControlKind_BoardTopics` / `BoardRead` / `BoardSubscribers` (`server/capabilities.go:38-41`) and the agent-side `list_topics` (`server/agent_handler.go:584`) | **stays a bit, renamed** |

The first two are task-space visibility and follow the axis. The third is not:
the agentboard is keyed by topic, not by task, and a topic is not something the
task hierarchy contains. Gating it on `visRank == global` would mix the two
namespaces the same way the row-filter idea in Deferred does — which is why
the board keeps a verb permission rather than inheriting a target-set rank.

**Rename to `board_observe`, same ordinal (1024).** Decided.

`board_read` was the runner-up: it matches the kind names an operator types
(`board read`, `board topics`, `board subscribers`). It loses on the failure
mode that matters. The bit is not needed to send, to subscribe, or to read your
own inbox — only to observe *other* agents' topics, messages and subscribers.
An agent denied `board_read` reads that as "I cannot read the board" and starts
debugging a messaging path that is working fine; denied `board_observe`, it
does not. The primary audience for a capability name is the agent reading its
own denial, and `observe` is the word that does not lie to it.

The catalogue text carries the boundary explicitly all the same:

> `board_observe` — list board topics, read a topic's retained messages, and
> list its subscribers. Not required to send, subscribe, or read your own
> inbox.

Nothing about the rename touches the wire: capabilities persist to the WAL as a
bitmask, not by name, so no record migrates. What changes is the generated
`String()`, the `--caps` catalogue and parser (which accepts `info_global` as a
deprecated alias with a notice), `caps --json`, and the cap pickers on all
three surfaces. `runner/agentskills/` is the `go:embed` source of truth for the
skill text and must be edited there, then mirrored to `.claude/skills/` and
`.agents/skills/`.

**Migration.** A legacy task holding the bit gets `vis_base = global` *and*
keeps the bit under its new name, so both duties survive intact. A task without
it is unchanged on both axes. `--caps all` continues to mean what it means
today.

### 8a. Scope applies to task-space only; other surfaces take a bit

Three operator-visible surfaces exist — tasks, connections, the agentboard —
and it is tempting to read that as three visibility axes. It is one:

- **Tasks** are the only thing a scope names. `vis_base` / `vis_ids` are about
  task ids, and every id in a scope is a `TaskID`.
- **Connections are a projection of the task set, not an axis.**
  `connInfoFor` (`server/server.go:1117`) shows a row to a confined caller only
  when `allowed[principalHex]` — the conn's principal task is in the
  task-visible set — and unidentified, runner and non-agent client conns are
  invisible to confined callers outright. A conn row carries its
  `PrincipalTask`, so "visible conn, invisible task" is not a state the data
  can be in. There is nothing left to scope separately.
- **The agentboard is keyed by topic**, which the task hierarchy does not
  contain, so no task scope can bound it. Hence §8's bit.

That yields the rule this design commits to:

> Scope is a target set over tasks. A surface whose objects are not tasks, or
> whose rows are a projection of the task set, is controlled by a capability
> bit — never by a scope of its own.

`board_observe` holding no scope is therefore not an anomaly; it is the first
instance of the rule. The extension path for "hide this surface entirely" is a
new bit beside it, and the axis count stays at one however many surfaces
appear.

Note what this does *not* claim: today the conn and task list surfaces are not
capability-gated at all. `requiredCap` has eleven entries and `List`,
`ListConns`, `GetTaskLog` and `ListPortForwards` are in none of them — they are
scoped and otherwise open, which the base spec records as the always-allowed
data plane. Withholding one is a new gate, not a translation of an existing
one; see Deferred.

### 9. Attenuation at spawn, per capability

The base spec's rules apply per bit:

- **base**: for each capability the child requests, `min` over
  `none < subtree < global` against the parent's effective base *for that same
  capability*.
- **ids**: every requested id — action or `vis_ids` — must be in the parent's
  effective set for that capability, `vis_ids` against the parent's visibility
  set. An id permitted for `file_read` and not for `cancel` is rejected on the
  `cancel` override alone, and `scope_not_permitted` names the bit.
- **visibility**: `visRank` clamps against the parent's `visRank` the same way.

All are checks, never silent adjustments. §3's two comparisons run beside them
against the value being written.

**`exclude_self` is granted as requested and is not clamped.** A task's access
to itself is not reach outward: the child and its worktree exist because the
parent caused them to. The base spec already exempts `self` from monotonicity
by making it unconditional; this design only makes the exemption switchable.
Clamping a child's self-reach against whether the parent can reach the child is
unresolvable at submit time — the child has no id yet — and is not proposed.

### 10. `caps set`, cascade, resume

`SetCapsRequest` carries the overrides list beside `caps` and `scope`, under
the **existing** `scope_present` bit: overrides and the visibility axis are
part of the scope half, written and cleared with it. A `scope_present = 1`
request with an empty override list clears every override — the only way to
remove one, and the same shape as writing an empty `ids`.

`--cascade` gains a third clamp beside its two: each descendant's override for
a bit is `min`'d against the target's new effective scope for that bit, and an
override naming a bit the descendant no longer holds is dropped. Visibility
clamps alongside.

Resume is unchanged in shape. The base spec's Amendment (2026-08-13) rule —
scope re-grants iff `scope_present`, independently of `resume_caps_override` —
covers the new fields for the same reason it covers `ids`.

### 11. The legal-value matrix

Enumerated so the model can be reviewed by inspection rather than by argument,
and so the test matrix is the same table.

**Rank pair — `base` against `visRank`.** Legal iff `rank(base) ≤ rank(visRank)`
over `none < subtree < global` (§3).

| `base` | `visRank` | | meaning |
|---|---|---|---|
| none | none | ✅ | sees and acts on self and named ids only — the tightest grant |
| none | subtree | ✅ | sees its subtree, acts only on self and named ids |
| none | global | ✅ | sees the server, acts only on self and named ids — today's `info_global` with `--scope none` |
| subtree | subtree | ✅ | **the default**, and today's omitted scope |
| subtree | global | ✅ | sees the server, acts on its subtree — today's `info_global` with `--scope subtree` |
| global | global | ✅ | today's `--scope global` |
| subtree | none | ❌ | action rank exceeds visibility (§4) |
| global | none | ❌ | same |
| global | subtree | ❌ | same |

**`vis_base_present = 0` pins the pair to the diagonal**, so the default value
and every legacy record are legal by construction — an illegal pair can only be
produced by explicitly asking for one.

**Override rank — `override.base` against `visRank`.** The same three-by-three,
and note what it is *not* compared against:

| `override.base` vs `base` | legal? |
|---|---|
| narrower | ✅ the ordinary case: `exec_cowrite` confined below the task's own base |
| equal | ✅ (usually only to carry `exclude_self` or different ids) |
| **wider** | ✅ **so long as it is ≤ `visRank`** — ❌ otherwise |

Both sides of that last row, concretely:

| written | verdict |
|---|---|
| `base=none, visRank=none` + `override{exec_control: subtree}` | ❌ the override outranks visibility: it could attach to descendants `ls` denies exist, and a successful attach discloses them |
| `base=none, visRank=subtree` + `override{exec_control: subtree}` | ✅ same reach, now stated as visible — sees its subtree and may attach within it, every other bit confined to self and named ids |

The difference is one explicit `vis_base`. Because `vis_base_present = 0` pins
the pair to the diagonal, writing `base=none` alone also makes `visRank` none,
so widening visibility is always something the grant says out loud.

The last row is the one that changed when visibility became an axis. Under a
single scope, `base=none` with `override{exec_view, base=global}` had to be
refused — the action would have reached past `ls`. With `vis_base=global`
stated explicitly, it is a legal and useful grant: read any session, act on
nothing but self. Overrides are bounded by *visibility*, never by the action
base.

**The modifiers do not interact with the rank pair.** Each does one thing, and
none can turn a legal pair illegal or the reverse:

- `exclude_self = 1` removes `self` from **one action set** — the one it is
  written on. It never removes `self` from `visible`, and never affects another
  capability.
- `ids` adds action targets for that scope. They join `visible` automatically
  (§2) and are clamped against the parent at grant time.
- `vis_ids` adds view-only targets. It is the only way to see without acting.

**Accepted, and deliberately not errors.** These read like mistakes. Each is a
legal value that resolves to something already true, and rejecting them would
turn a redundant grant into a failed spawn:

| written | resolves to | why it is not rejected |
|---|---|---|
| `ids:X` with `base = global` | no change; `X` was already reachable | the writer may not know the base is global — a template that always names its target stays correct |
| `vis_ids:X` with `visRank = global` | no change; `X` was already visible | same |
| `vis_ids:X` where `X` is also an action id | no change; action ids are visible anyway | the two lists are written for different reasons and may legitimately overlap |
| override mask naming a bit the task does not hold | inert, retained | a grant template outlives the mask it was written for; the bit may arrive later via `caps set` |
| `base = none`, `exclude_self = 1`, no ids | the empty action set | "holds the bit, can point it at nothing" is a real state during a staged re-grant |

**Every rejection, in one list.** A write (spawn or `caps set`) is refused with
`scope_not_permitted`, naming the offending bit where one applies, iff:

1. `rank(base) > rank(visRank)`
2. `rank(override.base) > rank(visRank)` for any override
3. an override's mask is empty
4. two overrides' masks intersect
5. `vis_base ≠ 0` while `vis_base_present = 0` — see below
6. an `ids` entry is outside the parent's effective set for that capability
7. a `vis_ids` entry is outside the parent's visibility set

**Rule 5 is new, and it is wire hygiene rather than authority.** With
`vis_base_present = 0` the `vis_base` byte is ignored, so a non-zero value
there gives two encodings of one authority — they would compare unequal,
render differently in `--json`, and disagree across a round-trip through a
client that normalises. Requiring the canonical form makes the value's
identity its bytes. It is the only rejection in the list that is not about what
a task may do.

## Wire, persistence, upgrade

`TaskScope` grows two enum bytes, a flags byte and a list; every format
carrying a scope gains an override list. This is a hard field-addition skew in
both directions, so **the base spec's §8 applies verbatim** — including the
silent-hang failure mode (a skewed request gets no response and the client
blocks on a deadline-less context) and the five-step upgrade order, whose gate
is resuming an interactive session on the new build before restarting
anything.

Formats affected are exactly the ones the base spec lists as carrying scope:
`SubmitRequest`, `OpenInteractiveRequest`, `SetCapsRequest`, `TaskInfo`,
`WhoAmIResponse`. `SetCapsResponse` is unchanged. No `Runner*` format changes,
so a runner built before this change still speaks correctly to a server built
after it.

WAL `task_created` / `task_caps_changed` gain `ScopeVisBase`,
`ScopeVisBasePresent`, `ScopeExcludeSelf`, `ScopeVisIDs` and `ScopeOverrides`.
Every one of them replays correctly from its JSON zero value, which is the
payoff for phrasing the two flags negatively and by-presence: a legacy record
yields `base` for visibility and `self` included, which is what it meant. The
one non-zero migration is §8's: caps containing `info_global` set
`vis_base_present = 1, vis_base = global`.

Rollback is reverting the server binary. The WAL reader sets no
`DisallowUnknownFields`, so records carrying the new keys replay cleanly on the
old server, which ignores them.

**Rollback is clean precisely because the migration is additive.** A task that
held `info_global` still holds the bit in every record, so an old server reads
it and restores global visibility exactly as before; the `vis_*` keys it does
not understand are dropped, which is the correct result on a binary whose
visibility model is the bit. Had replay cleared the bit, rolling back would
have left those tasks visible only in their own subtree with nothing in the WAL
to say why.

The one thing rollback does lose is authority that was *only* expressible in
the new model — a per-capability override, or a narrower visibility rank than
the action base. Those collapse to the old single-scope reading, which is a
widening. An operator rolling back with confined tasks in flight should expect
that and re-narrow with the old `caps set`.

## Surfaces

All three clients, per the repo rule that a feature spans CLI, TUI and WebUI.

**Human output stays one line.** Visibility is printed only when it differs
from the action base, overrides only when present:

```
caps=exec_view,exec_cowrite            scope=subtree +exec_cowrite:descendants
caps=exec_view,exec_cowrite,file_write scope=subtree +exec_cowrite,file_write:descendants
caps=exec_view,cancel                  scope=global/subtree +cancel:none
```

`global/subtree` reads *visibility / action*. `descendants` is the rendered
name for `base=subtree, exclude_self=1` — a UI vocabulary item; **the wire has
no such base value** and the parser expands it to the flag.

**`--json` emits the fully resolved capability→scope map, always** — every held
bit with the scope that actually applies after the merge, plus the visibility
set as its own entry, so a machine reader never re-derives it. Empty lists and
zero counts are emitted rather than elided.

- **CLI**: `cli.ParseScope` accepts `descendants` wherever `subtree` is
  accepted, a `vis/act` pair wherever a bare rank is accepted, and a
  per-capability form whose left side is a capability *list*, mirroring
  `--caps` — `--scope global/none --scope-for exec_cowrite,file_write=descendants`
  (repeatable). It also accepts its own rendered output as a single argument,
  so a scope copied from `ls` round-trips into `caps set` without being
  retyped. `harness-cli caps` documents the grammar and marks `info_global`
  deprecated with its replacement.
- **TUI**: the `caps`/`scope` cmdline commands and the `a` re-grant picker gain
  a visibility row and a per-capability section, collapsed by default so the
  single-scope case looks as it does today.
- **WebUI**: the same, in the spawn dialog and the task sheet's re-grant
  action.

## Testing

Extends, rather than replaces, the base spec's set.

**Unit (`server/`)**
- `scopeSet(cap)` with an override present, absent, and naming a bit the caller
  does not hold — the last must be inert, never an escalation.
- `scopeSet(Capability_None)` returning the union of §2, including action ids
  the caller can no longer act on.
- `authorize` for a caller whose base reaches a target while its override for
  the requested bit does not, and the reverse.
- `exclude_self` denying the caller's own id for the overridden bit while every
  other bit still reaches it, and while `ls` still lists it.
- The §3 checks in both directions: `base` outranking `visRank` rejected, an
  override outranking `visRank` rejected, ids outside the base accepted.
- Spawn attenuation rejecting an id per-bit, `scope_not_permitted` naming the
  bit.
- `info_global` migration: a legacy task replays with `vis_base = global` and
  keeps the renamed bit, so `ls`, `conns` and the three board kinds all answer
  exactly as before; a task without the bit is unchanged on both axes.
- **Replay clears no capability bit, asserted directly**: for a corpus of
  legacy records, the post-replay mask equals the recorded mask for every task.
  This is the check that would have caught the draft where the bit was dropped
  once its visibility duty moved — a silent board-access revocation triggered
  by nothing but a restart.
- Rollback: a WAL written by the new server replays on the old one with the
  bit still granting global visibility.
- The conn surfaces specifically — `ListConns` and the `conns_status` fanout —
  resolve through `vis_base` and no longer read a capability bit.
- `--caps info_global` still parses, sets `vis_base = global`, grants the
  renamed bit, and emits the deprecation notice exactly once.
- The invariant as a property test over randomised grants:
  `effective(cap) ⊆ visible` for every held bit. This is the assertion that
  catches the axes being collapsed in either direction.

**Completeness (`server/scope_completeness_test.go`)**
- Extended so a `Capability` bit that no `authorize` call site passes fails the
  build. Today's version catches an unwired *kind*; the gap this closes is a
  new *bit* that silently resolves through the base scope.

**Wire (`runner/protocol/scope_wire_test.go`)**
- A pre-change `SubmitRequest` payload is rejected by the new decoder, and a
  new-layout payload by a decoder truncated to the old field list — the skew is
  asserted, not assumed.
- An all-zeros `TaskScope` decodes to today's default in every field.
- A `ScopeOverride` with an empty mask is rejected, and two overrides whose
  masks intersect are rejected — at spawn and at `caps set`, not only at
  decode, since the same list arrives on both paths.
- `vis_base ≠ 0` with `vis_base_present = 0` is rejected as non-canonical
  (§11 rule 5), so one authority has one encoding.
- **§11's matrix is the test matrix**: every ✅ row round-trips and resolves to
  the stated set; every ❌ row and every numbered rejection is refused with
  `scope_not_permitted`; every "accepted, no effect" row is accepted and
  changes nothing.
- A mask naming a bit the task does not hold round-trips unchanged and applies
  the moment `caps set` grants that bit.

**Integration (dummy harness)**
- A child granted `exec_view` at `subtree` and `exec_cowrite` with
  `exclude_self` can `session snapshot` itself, is refused `session send`
  against itself, and both work against a child of its own.
- `base=none, vis_base=none` + `override{cancel, ids:X}`: `ls` shows `X`,
  `cancel X` works, `cancel Y` answers `no_such_task`.
- `vis_ids:[X]`: `X` is listed and every action against it is refused.
- `caps set` narrowing one bit's override takes effect on the next RPC with no
  restart, and drops only the connections that narrowing affects.

## Deferred

- **Whether the agentboard surfaces should be scoped at all, and along what
  axis.** `board topics` / `board read` / `board subscribers` are all-or-nothing
  today and stay that way here, with the gate translated to
  `visRank == global`. This is the one place the design leaves such a gate
  standing, and no replacement is proposed on purpose.

  Row-filtering by the task visibility set is **not** the obvious answer and is
  not recommended: a subscriber list answers "who is listening on this topic",
  which is a different namespace from "who is in my subtree". Agents message
  peers precisely because they are *outside* each other's subtrees, so that
  filter would empty the surface for every scoped task and break what it is
  for. Filtering by topic membership would fit the data better, but the board
  has no topic-ownership model and inventing one from the task hierarchy would
  import a structure it deliberately does not have. Either direction needs its
  own design, starting from what the board is for rather than from what `ls`
  does.
- **`conns_read`, and any other per-surface withholding bit.** §8a fixes the
  shape should it be wanted: a scope-less bit beside `board_observe`, never a
  fourth visibility axis. It is left out because no requirement asks for it,
  and because `ListConns` is currently ungated — adding the bit narrows a
  surface that is open today, which is a behaviour change needing its own
  three-surface sweep rather than a line in a migration.
- **Per-capability scope defaults at grant time.** A table mapping bits to
  default scopes adds a second site where authority is decided.
- **A `descendants` base value.** Rejected in §5 and recorded so it is not
  re-proposed: it cannot express the requirement, because `self` joins the
  union outside the base.
