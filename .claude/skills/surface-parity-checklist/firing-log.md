# Firing log

Append-only record of what each checklist item actually DID, per walk. The
point is not bookkeeping — it is to answer two questions the list cannot answer
about itself:

- **Which items keep getting MISSED?** A defect that ships past item N twice is
  evidence that N's wording is wrong, not that the walker was careless. Item 31
  is the worked example below.
- **Which items never fire at all?** `n/a` on every walk is dead weight, and the
  skill says why that matters: "a list that trains you to type `n/a` is how the
  walk decays." Those are pruning candidates.

## How to record

One entry per walk. Record **`done`** and **`missed`** only — everything not
listed was `n/a`, so writing n/a out would triple the size and bury the signal.

```
### <date> <commit> — <one line>
done:    11, 12, 14, 17, 20, 23, 28, 35, 36, 37
omitted: 16 (reason)
missed:  31 (what shipped, and how it was caught)
```

`missed` is filled in **later**, when the defect surfaces — edit the original
entry rather than adding a new one, so an item's miss count is countable by
grepping `missed:`. A walk that was never done at all is worth an entry saying
so: skipping the walk is itself a datum about when the trigger fails to fire.

## Entries

### 2026-08-20 `d6be39c` — skills_injected on TaskInfo

done:    11, 12, 14, 16, 17, 19, 20, 23, 24, 28, 31, 32, 35, 36, 37
omitted: 13 (whoami reports the caller's own authority; skills_injected is not
         authority, and a task can observe its own worktree directly — a
         strictly stronger check than the declaration)
missed:  —

Full walk, done before landing. Items 1–10 all `n/a`: the field is
server-derived and adds no input surface anywhere.

### 2026-08-20 `74ff10d` — observer counts (viewers / cowriters)

done:    11, 12, 14, 17, 20, 23, 24, 28, 31, 32, 35, 36, 37
omitted: 16 (an 8th column overflows an 80-cell frame), 19 (picker selects
         targets; observers are not a selection criterion)
missed:  **31** — the row elided `cowrite=0 viewer=0` at zero, so "nobody is
         watching" and "this row does not report watchers" rendered the same:
         the exact ambiguity the field was added to remove, reintroduced in the
         change that added it. Caught by the operator, not by the walk.
missed:  **16** — recorded as `omitted` on a real constraint, but the constraint
         was the column COUNT, not the feature. A width-conditional column set
         (`8f4791f`) fits it at >= 60 cells. "Omitted" was accurate and still
         cost the TUI the surface for a day.

**The walk itself was skipped before landing** and reconstructed only when the
operator asked "checklist は確認した?". That is the first datum: the trigger
("adding a field to TaskInfo") fired in the text of the skill and not in
practice, on the second and third change of the same session.

### 2026-08-20 `7004296` — exec_attach split into view/cowrite/control

done:    1, 2, 15, 35, 36, 37 (+ 3, 4, 5, 6, 11, 12, 13, 14, 17, 19, 23
         automatically: every UI enumerates from `cli.GrantableCaps()` /
         `CapsCatalog()`, so the new bits appeared with no edit)
missed:  — but the walk surfaced a **missing guard**: nothing tied
         `GrantableCaps()` to `Capability_All`, so a future cap added to the
         enum but not the list would silently make the WebUI `[all]` button
         grant a SUBSET of the server's `all`. Fixed in `db75eeb`. No item
         covers "the catalog must stay complete"; item 15 asks for the catalog
         to be updated, not for a test that catches the next omission.

Walk also skipped before landing; reconstructed with the one above.

### 2026-08-20 `8f4791f` — width-conditional Obs column

done:    16, 17, 34 (new)
missed:  —

Item **34 was born here**: making the column set dynamic panics inside bubbles'
`renderRow` if the rows and columns are ever swapped separately. Found by a
probe run, not by review. Per the skill's closing rule the number was added and
the list renumbered to 1–37.

### 2026-08-20 `06c534e` + `session resize` — exec_resize and its verb

done:    1 (session.go verb + flags), 2 (parseResizeSpec — one grammar, one
         parser), 15 (caps catalog), 33 (--resize refused on a bad spec; an
         unapplied resize exits 3 instead of passing silently), 36, 37
n/a:     3, 4, 5, 6, 7, 9 — the TUI and WebUI already emit winsize frames from
         their own terminals, so they gained the ability with no edit; nothing
         to type there. 11–14, 16–23: nothing new is DISPLAYED.
omitted: 10 — `session resize` is a new member of the `session` family, and the
         TUI cmdline / WebUI runCmd do not parse it. Neither parses
         `session snapshot`/`send`/`exec` either: the non-TTY trio is an
         agent-facing surface. Consistent, but it IS an omission.
missed:  —

Item **33 earned its keep** here rather than merely being satisfied: the server
discards a disallowed resize SILENTLY, which is right for the implicit
per-SIGWINCH stream and wrong for a typed command. Noticing that is what
produced the echo-wait acknowledgement and the exit-3, instead of a command
that prints success and changes nothing.

### 2026-08-20 `?` popup overflowed the terminal — DetailPopup gains a viewport

done:    17 (the popup is the surface), 34 (a fixed frame whose content is
         unbounded is the same class: the box must derive from the terminal,
         never from what it holds)
missed:  **17** — and it had been broken for as long as the key list has been
         long. Nothing in the checklist asks "does this VIEW fit the screen",
         only "is the field visible IN it". Operator found it by pressing `?`.

The popup rendered at its content's full height: 45 rows on a 24-row terminal,
so `?` pushed its own top border, title and first two groups off the top with
no way to scroll back. The task detail (`d`) shares it, and this session added
two lines to that body — so the sibling case got worse today even though `?`
was already broken.

Worth noting for item 32's "fixed frames" clause, which already says
popup/dialog WIDTH must derive from the viewport, never content. Height was
not covered, and that is exactly the axis that broke.

### 2026-08-20 `a280738` — per-capability scope (`--scope-for`): the TUI never sent it

**No entry was made when the feature landed** (`32c2417`…`c8965b3`). That is the
largest option-adding change in this log — a new spawn flag, a new wire field, a
new display column on every surface — and precisely the shape the tallies said
items 8, 9, 21, 22, 25, 26 and 27 were waiting for. The walk was not recorded,
and this entry is reconstructed from what the operator hit afterwards.

done (reconstructed, verified after the fact): 1, 2, 3, 6, 7, 8, 15, 17, 20, 22,
         23, 25 (`scope_present`), 33, 34a, 35, 37
missed:  **27** — its first firing, and a miss. `Overrides` was added to
         `cli.SessionOpts` and copied by BOTH shared builders, so the item was
         genuinely satisfied as written. But `SessionOpts` is a struct its
         callers fill by hand: seven literals in `tui/`, and the six in
         `interactive.go` predated the field. `session new --resume … --caps
         spawn --scope-for spawn=global` therefore spawned with the bare scope.
         `--caps` DID arrive, which made it read as a scope-parsing bug rather
         than a dropped field. Item **28a was born here**: reaching the funnel is
         not reaching the wire, and one input-surface cell hides N request
         builds ("TUI cmdline" hides six). Fixed by collapsing all seven onto
         `Authority.opts` with a test that fails when a second literal appears.
missed:  **32** — first firing, also a miss. `OverridesLabel` prints
         `spawn:global`; `ParseScopeFor` accepted only `spawn=global`; and the
         label's own doc comment claimed ls output "can be pasted back as
         flags". A serializer/parser pair with no round-trip test is exactly
         what 32 exists to prevent, and the false claim sat in the comment that
         should have prompted the test. Now accepts both spellings, with the
         round trip asserted.

Both defects were found by the operator using the feature, not by review — the
same pattern as `c8965b3`, where the invariant this feature was built around
was falsified by a live `ls`.

### 2026-08-20 `f4bb369` — exclude self, on the task's own scope

Operator question, not a bug report: 「webui ってデフォルトについては自分を除く
はできないんだっけ」. It could not, and the follow-on was worse than the gap.

done:    2 (`cli.ScopeSpec`, one builder), 6, 8, 22 (`scopeExcludeSelf` raw
         beside `scopeBase`), 23, 28a, 34a, 37
missed:  **34a** — the exclude-self checkbox went onto the OVERRIDE rows
         (`b7b4d35`) and not onto the task's own base row directly above them,
         in the same dialog. So the WebUI could say `descendants` for one
         capability and not for the task: three of the six bases unreachable
         from every graphical control, TUI picker included. A control added to
         one row must be carried to its siblings in the same walk.
missed:  **32** — second miss, same session, and the wording was the licence.
         "One serializer per grammar per RUNTIME" made two hand-written copies
         of the scope grammar sound correct; both were lossy in the same two
         ways and neither had the round-trip test the item claimed. Reworded to
         one serializer, full stop: the browser now calls `cli.ScopeSpec`
         through the wasm bridge and `scopeSpecJS` is deleted.
missed:  **31** — third miss, in its "do not lose what you do not show" form.
         The re-grant dialog rebuilt the scope from `scopeBase` + `scopeIds`,
         so `exclude_self` and the whole visibility pair were dropped on every
         apply: opening the dialog on a `global/descendants` task and pressing
         適用 wrote `subtree`. The scope string was already in the snapshot the
         entire time. Now carried through `ScopeSpec(..., carry)`.

Verified in a browser at 1280 and 390: all six bases reachable from the two
controls, and applying the dialog unchanged on a `global/descendants` task
leaves it `global/descendants`.


### 2026-08-21 `87c10a2` — AttachSessionResponse.kind + `session stream attach`

done:    1 (stream namespace, --stream open-then-follow, conflicts), 3, 8 (the
         wasm attach gate), 10 (the kind decision carried to EVERY attach-based
         verb: exec and resize refuse — resize would otherwise time out into
         the misleading exec_resize hint — send and raw snapshot deliberately
         agnostic with the reason in a comment, grid panes defensively stop,
         TUI `session attach` refuses and redirects), 24 (--stream on resume:
         kind is a per-open latest-recorded attribute, TaskStore.Resume §4c —
         the existing oneshot↔interactive conversion already answers it; read,
         not changed), 25 (event_stream=0 legitimately means PTY under that
         semantic, so no presence bit is owed), 27, 28a, 29, 30, 32
         (RenderText is THE renderer for event/request lines, used by the
         runner tap and the follow view, pinned from both sides), 33, 35, 37
omitted: 5 (the S session popup gains no stream toggle — TUI spawn of a stream
         session is the cmdline route until the stream write verbs land),
         6, 7 (the WebUI has no `session` command family at all and no spawn
         control for the kind; its attach paths refuse with guidance to the
         logs view, which is this kind's follower), 36 (session-debugging
         SKILL.md does not describe the stream verbs yet; the refusals
         self-describe, and the write verbs are the moment that text earns
         updating)
missed:  —

Item **28a caught a real defect before landing** for the first time: counting
the request builds surfaced that the CLI's own non-detach `session new
--stream` route ran `c.Interactive`, which splices NDJSON into a raw-mode
terminal. It became open-then-follow. The TUI's six builds are unreachable for
the kind by parse-time refusal (--stream needs -d there), which is a smaller
surface than wiring six literals.

The dummy-harness E2E also exercised `ErrBadLine` live: two raw `session send`
lines rendered as ⚠ warnings in both the follow view and the task log, and the
session survived — plus one send-without-newline sitting invisibly in the
adapter's line buffer until the next newline flushed it, which is the PTY
"text sits on the prompt" semantic and worth remembering when `stream turn`
is built (it must append the newline itself).

### 2026-08-21 — the claude preset supplies the event-stream adapter

Operator question, not a bug report: 「アダプターバイナリはどう指定するんだ…?」.
Reading the config path turned up that the answer was "not through any
documented route": `--agents claude` — the way
`feedback_use_runner_scripts` says to spawn a runner — produced a slot that
refused every event-stream task, because `KNOWN_AGENT_PRESETS` had no
`streamAdapter` key. `87c10a2`'s E2E passed the flag by hand and so never met
it.

done:    **S1** (the key goes in the preset map; the `sandbox-*` twins inherit
         it through the existing derivation, asserted equal to the base), 24
         (the value must ride BOTH expansion paths — `--agent-stream-adapter`
         for the default profile, the `streamAdapter` JSON key for every other
         name; a second claude slot in `--agents codex,claude` would otherwise
         lose the kind silently), 31 (the flag is emitted even when the value
         is EMPTY, so `--dry-run` states "this agent has no adapter" instead of
         leaving it inferred from an absent flag — gate on existence, not on
         value), 33 (`--agent-stream-adapter` joins `_CONFLICTING_FLAGS`: the
         list's invariant is "every flag `--agents` sets", so an explicit one
         alongside is refused rather than silently last-wins overridden), 35
         (README: the 5b block says which profiles serve the kind; also fixed
         two stale facts the event-stream work left — "all four binaries" and a
         Layout tree missing `cmd/harness-stream-adapter/`)
omitted: **S6 sandbox half** — `scripts/sandbox/README.md` and `probe.sh` gain
         nothing. The wrapper, its agent table and the container's security
         model are untouched: the adapter is a HOST process and the container
         only ever sees the agent argv it already got. Adding a "sandbox-claude
         serves event-stream tasks" line there would be exactly the unmeasured
         claim S6 warns about, since nothing has driven an approval through
         `podman run -i` yet. `.claude/commands/runner-up.md` (the other S6
         half) IS updated.
         **37** — the spec is not amended. It never said where the adapter path
         comes from (§2 places the adapter; `--agent-stream-adapter` names it),
         so there is no shipped behaviour for it to contradict. Recorded as
         entry 22 of the wiring log instead, which is where entry 21's handoff
         lives and where a next session would look.
missed:  —

**S1–S6's first firing in this log**, and it fired on the axis the section was
split out for: a 1–37 walk cannot reach `scripts/agent_presets.py`, and nothing
in `cli/`, `tui/` or `webui/` mentions it. The defect was not a missing pixel
or a dropped field — every UI surface for the kind was correct and complete
(`87c10a2`'s walk was thorough) — it was that no documented way existed to
LAUNCH a runner that serves the kind. Worth naming as a class: a feature can
pass a full 1–37 walk and still be unreachable, when what is missing is the
agent-launch config rather than an operator surface.

One thing S1's own wording got right in advance: "the twin must stay DERIVED
(only `bin` changes)". That rule produces the correct answer here for a reason
it does not state — the adapter runs on the host, so a hand-copied twin that
"helpfully" rewrote the path into the container would have been wrong. Added
the reason to the derivation comment and pinned it with the equality assertion.

### 2026-08-21 `9fef2b1` — the event-stream write verbs and the TUI chat

done:    1 (four CLI verbs + their flags), 3 (TUI cmdline parses all four), 4
         (`r` gains a third meaning; keys_test forced the help row with it), 10
         (the `session stream` family: every member that exists is on BOTH the
         CLI and the TUI cmdline — the deciding argument was that a family with
         one member parsed and its companions dropped is this item's own
         failure shape), 24 (the same option on the two write paths: the
         short-lived per-call attach and the chat's held one, which is why both
         route through cli rather than each building a message), 27 + 28a (both
         writers go through `cli.EncodeStreamMsg`; the TUI assembles no
         `streamagent.Msg` of its own, so the "reaching the funnel is not
         reaching the wire" gap cannot open here), 29 (`stream <verb> <id8>:
         sent`, never a bare ok), 30 (results to `a.cmdresult`), 32 (ONE builder
         for the grammar, newline included, pinned by a round-trip through the
         ADAPTER's decoder rather than through encoding/json), 33 (approve
         refuses neither-or-both of --allow/--deny, and refuses --message on an
         allow, instead of picking), 35, 37
omitted: 6, 7, 8 (WebUI) — **deferred to the next increment at the operator's
         direction**, which is a different verdict from the `87c10a2` walk's
         omission. It has no `session` command family in `runCmd` at all, so
         this is a family to add rather than a flag to thread.
         36 — no agent-facing skill describes the stream verbs yet; the CLI's
         own usage text carries them, and session-debugging's text earns
         updating when an agent is expected to drive this kind.
missed:  —

Item **4 is worth a note for its own sake**: `r` now means three things by
kind (take over a PTY, resume a finished task, open the chat), and the binding
table forced the help to say so because keys_test pairs every dispatched key
with a row. That guard did the work here without anyone remembering it.

**What the walk did NOT catch, and no item covers.** The chat rendered CENTRED
— its View branch was copied from the grid's, which centres fixed-width panes
deliberately, and full-width prose centred puts every line at its own indent.
Items 34/34a are the nearest neighbours and both are about a control's KIND or
a column set's arity, not about a layout constant copied from a sibling whose
reason did not travel with it. Found by driving a real turn through the view.
Not proposing a new number for one instance: the general form ("a constant
copied from a sibling carries that sibling's reason, which may not hold") is
too broad to walk. Recorded so a second instance can be counted against it.

### 2026-08-22 (pre-landing) — `--json` on `session snapshot` / `session send --snapshot`

done:    1 (both usage strings + the `main.go` help block), 10 (the option went
         onto BOTH members that share the render path, and the walk closed the
         pre-existing asymmetry it found rather than matching it: `send
         --snapshot` accepted `--style` but not `--color`, so `--json` would
         have shipped a `spans[]` that can carry colour on one verb and never on
         the other), 24 (`--json` means the same thing on both paths, and the
         snapshot-only flags stay refused without `--snapshot`), 31, 32, 33
         (`--raw` + `--json` refused instead of one silently winning; `--json`
         / `--color` without `--snapshot` join the existing stray-flag guard),
         36
omitted: 3, 6, 7 — the TUI cmdline's `parseSession` and the WebUI `runCmd` do
         not parse `session snapshot`/`send`/`exec` at ALL, same standing
         omission as the `06c534e` entry: the non-TTY trio is agent-facing.
         **8 is stronger than an omission and worth separating**: `--json`
         cannot exist in wasm, because the whole VT render does not — 
         `cli/snapshot_native.go` is `//go:build !js` and the browser renders
         through xterm.js instead. Structurally impossible, not unbuilt.
         35 — the README has no `session snapshot` flag reference to update; its
         session block lists commands, and the only two mentions are a `--raw`
         line in the stream section and the `exec_view` caps row.
         37 — no spec under `docs/superpowers/specs/` states snapshot's output
         format, so there is no shipped behaviour for this to contradict.
missed:  —

**27/28a are `n/a` for an unusual reason worth recording**: the count of request
builds this option reaches is ZERO. `--json` never becomes a wire field — it
selects an encoding of a response the client already has. That is the first
entry where 28a's counting question has a legitimate answer of none, as opposed
to "I did not count".

Item **32 shaped the diff rather than being satisfied by it.** The obvious
implementation is a second walk of the VT grid that emits JSON, next to
`scanSpans` which emits the `--- styles ---` text — two descriptions of one
screen, which is the drift 32 exists to forbid. Instead `collectSpans` became
the only scan and the text report is now RENDERED FROM its result through
`ScreenSpan.label()`, so the human form is a projection of the structured one.
Pinned two ways: `TestFormatSpansIsAProjectionOfCollectSpans`, and a live
byte-for-byte diff of `--style --color` output between the pre- and post-change
binaries against the same session (2664 bytes, identical).

Item **31 decided a field that would otherwise not exist.** `spans: []` alone
cannot separate "no style dimension was requested" from "requested, and the
screen has none" — the same not-shown/not-measured collapse the item keeps
catching, one level up: here the ambiguity is about whether the MEASUREMENT was
taken. So the object carries `attrs` and `color` booleans reporting what was
COLLECTED, and `collectSpans` returns `[]` rather than nil so the field is never
JSON `null`. Gate on existence, not on value.

Item **36 caught a real drift inside this walk**: the embedded SKILL.md was
edited again after being mirrored (a correction that `start`/`end` are grid
COLUMNS, not offsets into `lines[row]` — they coincide only while every earlier
cell is single-width). `TestMirrorsMatchEmbeddedSkills` failed on the byte
difference. The item is a reminder; the test is what actually held.

### 2026-08-22 (pre-landing) — the visibility half becomes editable (WebUI + TUI picker)

Operator question, not a bug report: 「webui って visibility の scope いじれないん
か?」. It could not: the two graphical surfaces CARRIED that half and edited only
the action one, so three of its states were reachable from typed grammar alone.

done:    2 (`cli.ScopeSpec` stays the one serializer; its `carry` parameter is
         GONE — see 32), 4 (picker key `v`; picker-local rune switch, so
         `tui/keys.go`'s mainKeyMap is untouched — the `A`/`N` precedent),
         5 (picker gains a `see:` row and a second checkbox per task row),
         6 (spawn form + re-grant dialog: rank radios + a `+vis-ids:`
         checklist), 8 + 22 + 23 (`visBase`/`visIds` across the bridge;
         `scopeVisBase`/`scopeVisIds` raw on the snapshot — `""` for NOT
         STATED, which no label form can express), 9, 24 (identical meaning on
         the spawn and re-grant paths), 25, 27 + 28a (both JS request builds
         and the picker's single `Result()`), 31, 32, 33 (an exclude-self
         visibility rank is REFUSED, not silently dropped — self is always
         visible), 34a, 35
n/a:     1, 3 (the CLI and the TUI cmdline already took the whole grammar
         through `cli.ParseScope`; this walk adds no flag), 11–21 (the scope
         label already printed both halves everywhere), 28 (`VisBase`/`VisIds`
         were already persisted), 36, 37 (no skill or spec states the UI's
         control set; `server/scope.go` is the semantic authority and already
         does)
missed:  —

Item **31 fired twice, and the second one changed the design.** First as the
three-state radio: an unstated rank is its own value ("follows the action
rank"), so `base に従う` is a radio option rather than the absence of one —
without it, opening either surface would have promoted every default to an
explicit rank. Then, harder: I wrote a rule that vis-ids are dropped at a global
visibility rank, mirroring the action set. The picker test caught it
immediately — seeding a `global/subtree+vis-ids:X` task and applying it
UNCHANGED erased the clause. The mirror was false: `global+ids:` does not parse,
so dropping the action set is the grammar's rule, while `global/…+vis-ids:`
parses and is merely redundant. **The invented rule was deleted rather than
scaffolded** (Pitfall 11), in the Go picker and in the JS, along with the
`disabled` styling that would have hidden a value still being sent.

Item **32 got to delete a parameter instead of adding one.** `ScopeSpec(base,
excludeSelf, ids, carry)` took the target's whole scope string and read only the
visibility half back out of it, purely because no graphical control could edit
that half. Carrying was never the goal — not erasing was — so the honest fix
once controls exist is `ScopeSpec(base, excludeSelf, ids, visBase, visIds)` and
no second way to set the same field. Scaffolding a rule outlives the rule unless
someone goes back for it.

Item **34a is the reason the TUI picker is in this diff at all.** Its recorded
miss was a control added to one row and not carried to its sibling in the same
dialog; the sibling here is a whole surface. Both were built from controls each
already had — a radio row and a task checklist — so neither grew a text box, and
the parser stayed in Go.

Item **35 turned up a stale paragraph, on exactly this axis.** The README still
said a visibility rank narrower than the action rank "is refused, and not as
policy", describing an invariant deleted in `c8965b3` — `server/scope.go` says
in bold that the two ranks are deliberately NOT compared. The new controls can
build that combination and the server accepts it, so the doc would have taught
the opposite of what the UI does. Rewritten from the code comment, which is the
authority.

Verified live against a throwaway instance, both viewports: the spawn form
serialized `global/subtree+ids:…+vis-ids:…` end to end through the bridge; the
re-grant dialog seeded `global` + one vis-id from a task set that way and echoed
its stored scope byte-for-byte (an untouched apply is a no-op); editing the rank
to `none` and unticking the see-only task applied, and the server then reported
`none/subtree`. At 390px every radio stayed `inline-block` beside its own label
with no sideways scroll — the container-selector CSS from 34a's own fix covers
the new row without touching it.

## Standing tallies

Update when adding an entry.

| item | done | missed | note |
|---|---|---|---|
| 31 (don't hide a value for what it IS) | 6 | **3** | The first two were elisions the item's own text licensed, and the row-width exception was withdrawn for them. The third is a different shape and the most expensive: the re-grant dialog did not merely hide `exclude_self` and the visibility pair, it ERASED them on apply, because it rebuilt the scope from parts instead of carrying the whole. Not-shown and not-kept are one item's problem. The fourth extends the axis again: an empty `spans[]` could not say whether the measurement was TAKEN, so the object reports which style dimensions were collected. Not-shown, not-kept, not-measured. |
| 16 (TUI task table) | 2 | 1 | Missed once as a defensible `omitted`; the constraint was real, the conclusion was not. |
| 13 (whoami) | 0 | 1 | Also elided `scope=subtree` until `d437f6e`. Easy to forget because it is not a task listing.
| 34 (dynamic column sets) | 2 | 0 | New. Second firing was the popup: same class, different widget. |
| 17 (TUI detail popup) | 3 | **1** | Missed the popup's own HEIGHT. The item asks whether a field is visible in the view, never whether the view fits the screen. |
| 33 (take effect or error) | 6 | 0 | First real firing: it turned "the server drops it silently" from acceptable into a bug worth an acknowledgement path. Third firing applied it to a flag-expansion collision rather than a wire value — the same axis one layer out. Fifth was two mutually-exclusive OUTPUT selectors (`--raw` vs `--json`), refused rather than ranked. |
| S1 (preset derivation) | 1 | 0 | First firing of S1–S6 at all. Caught a feature that passed a full 1–37 walk and was still unlaunchable: the gap was agent-launch config, which no UI grep reaches. |
| 10 (other verb families) | 3 | 0 | First `omitted`: a new `session` verb that the TUI/WebUI command lines do not parse — consistent with the rest of the non-TTY trio, but recorded rather than assumed. Third firing was the useful one: walking the family surfaced an asymmetry that PREDATED the change (`send --snapshot` took `--style` but not `--color`), and the item's answer was to close it in the same walk rather than to match it. |
| 1–10 (input surfaces) | 1 walk | 0 | `n/a` for every field-only change. Do NOT prune: they fired fully for the caps split, which is exactly the change that needed them. |
| 27 (shared funnel) | 2 | **1** | Same walk. Satisfied as written and still shipped the defect: it names the BUILDERS, and the loss was in the builders' callers. 28a is the missing half; if 27 misses again, split it rather than reword it. |
| 32 (one serializer, round-trip tested) | 5 | **2** | Both misses in one session, both the same wording defect: the item claimed round-trip tests that never existed, and "per RUNTIME" licensed the JS mirror that made the loss possible. `OverridesLabel` could not be pasted back; `scopeSpecFor`/`scopeSpecJS` each knew half the grammar. Reworded to one serializer, full stop. A third miss means the problem is not the wording. Fourth firing was PREVENTIVE and is the shape to aim for: it rejected the obvious two-scans implementation of `--json` before it existed, making the text report a projection of the structured form. |
| 28a (follow the value to the request build) | 4 | 0 | Second firing caught the CLI's non-detach --stream splicing NDJSON into a raw terminal BEFORE landing — the first pre-landing catch in this log. |
| 34a (same KIND of control as its neighbours) | 2 | **1** | Missed by omission rather than by wrong shape: the control was right and was not carried to the sibling row in the same dialog. |

**Never fired yet:** 21, 26. Too few walks to call either dead — revisit after
another change that adds a spawn OPTION rather than a display field. (29, 30
and 24 came off this list with the `87c10a2` entry.)

(8, 9, 22 and 25 came off this list with the `--scope-for` entry above; 27
came off it by MISSING, which is still a firing.)

(33 was on this list until `aa4a1dd`. Keeping the two halves consistent is
manual, so check the tallies against the entries when adding one — a log that
contradicts itself is worse than no log.)
