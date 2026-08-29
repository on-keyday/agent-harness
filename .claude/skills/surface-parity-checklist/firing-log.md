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

### 2026-08-22 `fe894b4` — cursor + alt_screen on the session snapshot object

done:    10 (`session send --snapshot` shares `printSessionScreen`, so the two
         paths cannot render different objects — verified it is the only path,
         and its `--json` help still says "same shape" truthfully), 24 (same:
         one renderer, so the meaning is identical by construction rather than
         by agreement), 31 (reported unconditionally — collecting is free, and
         a field present only sometimes is one a reader cannot rely on; also
         kept `visible` separate from the position, since a hidden cursor still
         has one), 32 (the struct tags are the only spelling of the grammar;
         the text forms deliberately do not restate it), 33 (a `--cursor` flag
         was considered and REJECTED: it would be inert under `--json`, which
         is this item's failure shape — no flag means nothing can be ignored),
         36, 38
omitted: 35 (README documents `session snapshot`'s existence and its capability,
         never its JSON shape — verified by grep; adding the first such
         description here would put a second source of truth next to the
         `--json` help text)
         37 (no snapshot spec exists — the specs that mention it do so in
         passing about other features)
         38's second half: neither live pane DRAWS a cursor (TUI grid pane
         reads `emu.Cursor()` for scroll anchoring only; the WebUI preview does
         not touch it). Rendering a caret is a different deliverable from
         reporting terminal state, with its own question — what style, and how
         it composes with reverse-video content. Named rather than silently
         skipped.
missed:  —

Note on 11–23: `n/a` for all of them, and for an honest reason rather than a
reflex — they walk TASK fields across listings and detail views, and this field
belongs to a SCREEN. That is what produced item 38.

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

### 2026-08-22 (pre-landing) — `session snapshot --detect`: screen → agent state

Follows the herdr read ([[reference_herdr_for_harness_design]]): a rendered
screen plus the OSC title, judged by priority-ordered rules held as DATA, with
the evidence for the verdict. The point is `blocked` — waiting on a HUMAN —
which PTY byte-quiescence cannot tell from thinking, because both are silent.

done:    1 (`--detect` / `--detect-agent` on both snapshot-rendering commands,
         both usage strings, the `main.go` help block), 2 (one rule format, one
         loader, one evaluator; the rules are validated AT LOAD so a rule with
         no condition or an unknown region is a named error rather than a
         detection bug months later), 10, 24, 29 (the report names the state,
         the rule, its region and priority, AND the text that rule read — a
         bare "blocked" would be the count-without-a-target this item exists
         for), 31, 32, 33 (three refusals: `--raw`+`--detect`, `--detect-agent`
         without `--detect`, and an unknown agent that NAMES what is
         available), 36
omitted: 3, 6, 7 — standing omission, unchanged: the TUI cmdline and the WebUI
         `runCmd` parse no `session snapshot` at all.
         8 — the evaluator itself builds for wasm (no build tag), but the
         browser has no screen to feed it: `cli/snapshot_native.go` is
         `//go:build !js` and the WebUI renders through xterm.js. Available,
         unreachable — worth separating from "not built".
         35 — the README has no `session snapshot` flag reference to extend.
missed:  —

Item **31 decided two fields.** `unknown` is a first-class state with a
`fallback_reason`, so "no rule claimed this screen" and "the agent is free"
cannot be the same answer — the collapse that makes a silent misdetection read
as a confident verdict. And `title` is reported even when empty, because an
empty title is a measurement (a long-quiet session's title can age out of the
replay ring) rather than a missing field.

Item **29 is the whole feature, not a formatting note.** A verdict that says
only `blocked` is unfalsifiable; one that says which rule fired, on which
region, at which priority, and prints the text that rule read, can be argued
with. That is the property worth copying from herdr — not their regexes, which
we did not take.

**A live run contradicted the fixtures and improved the rules.** Pointing
`--detect` at THIS session mid-turn returned `working`, and the `--json` explain
showed THREE rules matching at once: the title spinner (1100), the interrupt
hint (960) and — the surprise — `prompt_box_idle` (950). Claude keeps its input
box on screen for the entire turn, so a screen-only idle rule reads a working
agent as free, and only the priority ordering saves it. The same run surfaced
`· esc to interrupt` in the footer, which became a second working rule: it does
not depend on the title, which a replay ring can drop. Both are now fixtures.
Negative control: the same command against a plain bash pane returns `unknown`
with `no_rule_matched`, not `idle`.

### 2026-08-22 (pre-landing, follow-up) — the detection defects the operator found within the hour

Two, from one pasted `--detect --json`. Both were caught by the explain output
itself, which is the case FOR that output: the verdict said `unknown`, and the
per-rule evidence said which region each rule had read, so the diagnosis started
from data rather than from a re-run.

done:    2, 29, 31, 32, 33 — unchanged shapes, re-exercised.
missed:  — but recorded because the FEATURE shipped two defects a live operator
         hit immediately, on a change whose own walk was clean.

**1. The window title came back as one byte.** Chased the wrong way first: the
capture is cut on a settle timer, so a mid-sequence tail looked like the answer.
A probe killed it — x/vt only fires its Title callback on a TERMINATED sequence,
and the raw capture held the full 36-byte title, BEL-terminated, a kilobyte from
the end. Isolating the sequence found the real cause: **x/vt's Title callback
truncates at the first multi-byte character**, unconditionally (tried with no
prefix, ASCII text, UTF-8 text, an earlier ASCII title, an earlier UTF-8 title).
Now the title is scanned out of the captured bytes instead, with a test that
asserts the library path is STILL wrong — so if x/vt is fixed, the scan's reason
for existing is visible rather than folklore.

**2. `!` is a prompt marker.** Typing `!` replaces Claude's `❯` with it (shell
mode), so the idle rule missed a session plainly waiting for input. The
operator's one-line correction — "`!` does not appear unless I type it" —
converted this from "a hint row cycles" (my guess, wrong) into a marker-set gap.
`/` and `@` are deliberately NOT added: their rendering has not been observed,
and guessing them is how a rule set starts matching screens nobody has seen.

**The lesson is about the fixtures, not the rules.** Every fixture came from a
screen I had captured, which felt like the disciplined choice and still only
covered the states I happened to drive: I never typed `!`, so shell mode did not
exist for me. Screens an OPERATOR produces are a different distribution from
screens an agent produces, and only one of those was in the test set.

### 2026-08-23 (pre-landing) — the repaint carries the title; `--with-synth` inverts to `--without-synth`

Operator report, not a bug filing: 「snapshot したときに title がない判定になるのよね
しばらくしたときに」. One symptom, three causes stacked.

done:    1, 10 (the flag went onto BOTH members that share `printSessionScreen`
         — `session snapshot` and `session send --snapshot` — rather than onto
         the one where `--raw` lives), 24 (the option now has a written meaning
         on THREE paths: drop them from the emitted stream (`--raw`), from what
         the emulator is fed (the render), and the same again under `send
         --snapshot`; before this it had a meaning on one and was refused on the
         others), 27 (`screenOpts` is this family's funnel and says so in its own
         doc comment), 28a, 29, 31, 33, 35 (checked: the README's single mention
         does not describe the withholding, so nothing contradicts), 36, 37, 38
omitted: 3, 6, 7 — standing omission, unchanged: the TUI cmdline and the WebUI
         `runCmd` parse no `session snapshot`/`send` at all.
         8 — structurally impossible rather than unbuilt, same as the `--json`
         and `--detect` entries: `cli/snapshot_native.go` is `//go:build !js`,
         so there is no wasm render for the flag to steer.
missed:  —

**Item 38's first firing that changed the diff**, and on exactly the axis it was
split out for. Asking "do the live panes render this?" is what turned up that
`tui/pane_streamer.go` and `cli/preview_wasm.go` both read
`CommandExecutionStream.Stdout()` — which MERGES Stdout and Synth — while the
native snapshot renderer passed `includeSynth=false`. Three renderers, one
deprived, and the deprived one is the only one anybody had complained about.
That comparison is what converted "should the default change?" from taste into
evidence: two thirds of the surfaces already behaved the new way.

**Item 33 caught a live defect DURING the walk.** `--without-synth` was added to
`session send`'s flag set and not to the `fs.Visit` stray-flag switch beside it,
so without `--snapshot` it would have parsed and done nothing — this item's exact
failure shape, in the same edit that invoked the item. Verified after fixing:
`session send --without-synth <id> hello` now errors.

**Item 28a found the caller untagged tooling cannot see.** Counting the builds
turned up `c1.SessionSnapshot(...)` in `integration/session_snapshot_raw_test.go`,
invisible to `go build ./...`, `go test ./...` and the untagged vet — caught by
`make vet`'s second, `-tags integration` pass, which exists for this.

**Item 36 caught a doc sentence the change FALSIFIED**, which is a different job
from the mirror-drift it usually catches. `session-debugging`'s SKILL.md told
agents that an empty `title` is a measurement because "a long-quiet session's
title may have aged out of the replay ring". True when written; false the moment
the repaint carried the title. A doc that stays byte-identical across its mirrors
can still be wrong in all three.

**The walk was skipped and then requested.** I had already written and tested the
flag change before the operator asked 「そもそも checklist あるよな」. Same datum as
the `74ff10d` entry: the trigger ("adding an option to any verb family") fires in
the skill's text and not in practice. Two of the four items that did work here
(33 and 28a) were found only because the walk ran, so the cost of skipping it was
not hypothetical.

### 2026-08-23 (pre-landing) — `live` on the snapshot object: what arrived, not what is drawn

Operator question, not a bug report: 「frame update rate みたいなのって snapshot 時に
出せないんだっけ」. It could not. The material was already flowing and being
discarded — `CollectRaw` classifies Stdout/Stderr vs Synth per frame and already
returned a `synthesised` count that `collectScreen` threw away with `_`.

done:    1 (no new flag; the `--json` help text's KEY LIST is the surface, and it
         enumerates every field — leaving `live` out of it would make the help
         text lie about the object beside it), 10 (`session send --snapshot`
         shares `printSessionScreen`, so both members carry the field and the
         `--detect` line with no second rendering), 24 (all four render paths
         produce it identically because all four funnel through `collectScreen`),
         27 + 28a (counted: `ScreenSnapshot{` is constructed in exactly ONE place,
         `buildSnapshot`, whose doc comment already says that is the reason it
         exists — so the "reaching the funnel is not reaching the wire" gap cannot
         open), 29, 30, 31, 36, 37
omitted: 24's `--raw` half — `SessionSnapshotRaw` does NOT surface it. That path
         promises bytes and already spends its one stderr line on the synthesised
         count; a second note would compete with it for the same channel, and a
         measurement ABOUT a capture belongs with the structured form. Recorded
         rather than left to look like an oversight.
         35 — the README still has no `session snapshot` output reference to
         extend (re-verified by grep: two mentions, `--raw` in the stream section
         and the `exec_view` caps row).
         38 — `tui/pane_streamer.go` and the WebUI preview hold the SAME stream
         and could measure this CONTINUOUSLY rather than by a 1500 ms sample,
         which would be strictly better. Not built: they have no verdict to print
         it beside, and the measurement's intended consumer — a detection rule —
         does not exist yet. Building half of it there would ship an asymmetry
         nobody had looked at, which is the outcome this item exists to force.
missed:  **38** — the omission above was recorded with a FALSE reason, and the
         operator falsified it within the hour: 「tui に rate 足すみたいなのって既に
         デバッグ用表示のあれなかったっけ」. The TUI grid pane has had a per-pane
         diagnostic overlay the whole time — `HARNESS_GRID_DIAG` →
         `PaneStreamer.DiagLine()`, overlaid on each pane's first body row
         (`tui/grid.go`) — already counting `rxBytes` and `reads`. So "no verdict
         to print it beside" was simply wrong: there was a surface, it was
         printing the same two quantities CUMULATIVELY, and cumulative counters
         are the thing a rate exists to replace (`rx=41230` cannot say whether
         anything is arriving now; reading it needs two screenshots and a
         subtraction, on exactly the pane you are watching because it looks
         stuck). Fixed in the same session: `DiagLine` now carries `B/s` and
         `rd/s` over a rolling one-second window, and the overlay gained a `diag
         [on|off]` cmdline verb because an env var can only be set before launch
         — a grid that goes wrong after ten minutes cannot be relaunched without
         losing the state being diagnosed.

         **What made the reason false is worth naming.** I searched for a place
         to print a VERDICT, because that is what the CLI change had just built.
         The TUI surface is not a verdict, it is a diagnostic overlay, so the
         search that would have found it is "does this surface already report
         anything about the stream" — the question item 38 actually asks. An
         `omitted` verdict is only as good as the search behind it, and this one
         searched for the wrong noun.

**And the fix itself then shipped a rendering defect no item covers.** The rate
was derived from the OPEN window as count/elapsed, so an idle pane "decayed to
zero on its own" — visibly, one render per tick. Operator, within minutes:
「なんかすげー勢いで数値がカウントダウンするみたいになってますけど」. Nothing in 1–38
asks whether a displayed value is STILL when its subject is; item 34 is the
nearest neighbour and is about a column set's arity, 34a about a control's kind.
This is the second instance of the general form the `9fef2b1` entry declined to
number ("a property of the render that no item interrogates" — there, a centred
chat pane). Two instances, still different enough not to share a number: one was
a layout constant copied from a sibling, this one is a value that animates while
its subject is static. Recorded so a third can be counted against them.

Worth keeping for the test discipline rather than the checklist: the first
negative control **passed**, i.e. failed to falsify. A rolled window resets its
count, so count/elapsed yields a steady 0 and the buggy implementation looks
correct — the bug only shows for a burst shorter than one window. A negative
control that goes green is not a clean bill; it means the control did not reach
the defect.
n/a:     2–9, 11–23 — no grammar, no input surface, and 11–23 walk TASK fields
         across listings while this belongs to a CAPTURE. Same reasoning as the
         `fe894b4` entry that produced item 38.
missed:  —

Item **31 fired hardest, and it decided the field's shape rather than its
visibility.** A count alone cannot be read: `0 frames` is indistinguishable from
"not measured", so `window_ms` travels with it and both are printed and
marshalled unconditionally, zero included. The same axis produced `anchored` —
without an end-of-replay repaint the window still holds replayed history
delivered at wire speed, so the numbers look identical and mean nothing. That is
the not-shown / not-kept / not-measured progression this row keeps extending,
now with a fourth face: **not-VALID**. A measurement that cannot say whether it
is meaningful is worse than an absent one, because it reads as data.

**A negative control, per Pitfall 10.** The anchoring IS the feature, so it was
removed to confirm the test goes red: the same capture then reads 4 frames /
34 B / 1500 ms instead of 2 / 8 / 1300 — the replay burst counted as live output,
which is exactly the fictitious rate the field would otherwise report.

Item **29 fired on a measurement rather than a mutation**, the same one-layer-out
shape as the `--detect` entry: the line names all three quantities and appends
`(no rule reads this yet)`, because a number printed beside a verdict will be
read as part of it unless it says otherwise.

**What the change did NOT do, deliberately.** No detection rule consumes `live`.
`detect_rules.json`'s condition language matches text against screen regions;
giving it a numeric input is a schema change that should follow evidence about
which thresholds separate the states, not precede it.

**The trigger fired late again.** The implementation, its tests and the negative
control were done before the checklist was invoked — third entry in a row saying
so (`74ff10d`, the `9580515` entry, this one). Unlike those two, nothing was
found by the walk that the diff had missed; what the walk produced here was the
three `omitted` verdicts, which are the part that would otherwise have been
silent.

### 2026-08-23 (pre-landing) — `board subscribers` carries the delivery position

The agentboard's "how far has this task been shown its messages" mark moved from
a client-side cursor file into server state, and the whole point of moving it was
that a file on the runner host is on NO operator surface. So the field exists
because of this checklist's own axis, not merely subject to it.

done:    5 (`tui/board.go` subscribers view), 6 (`renderBoardSubscribers`), 8
         (the wasm bridge), 10 (`board topics` also consumes `BoardSubscribers`
         for its per-topic counts — the same type change reached it on all three
         surfaces), 24 (`board subscribers` with and without a topic: the mark is
         per (task, topic), so both paths report identically), 27 (one builder,
         `Board.ListSubscribers`; all three UIs read it through
         `cli.BoardSubscribers`), 28a, 29, 31, 37
omitted: 32 — the display string `name(shown=N pending=M)` is hand-written in
         THREE places (CLI, TUI, JS). It is not a grammar: nothing re-parses it,
         and each surface embeds it in a different row format. Recorded rather
         than left implicit, because three copies of one format is the drift this
         item exists for even when the format is display-only.
         36 — the harness-cli skill's rewrite is a separate task in this plan;
         it names `board subscribers` as the way to SEE the position.
n/a:     1, 2, 9 (no flag, no grammar, not a spawn field). 3 and 7 for a reason
         worth stating rather than assuming: `tui/cmdline.go` has no `board` verb
         and `runCmd` has no `board` case — both verified by grep. The board is a
         keybinding-reached modal and a tab panel respectively, so unlike the
         `session snapshot` standing omission there is no parser that COULD have
         taken it. 11–23 walk TASK fields across listings; this is a board row.
         28 (the mark is in-memory taskState and dies with the board it indexes —
         a spec Decision, not an oversight). 34, 34a, 38. 35 (grep: the README
         has no `board subscribers` output reference). S1–S6 (no agent, bin,
         argv, credential, egress or env change; also grepped `scripts/` for
         `agent-cursor` / `since-last` to confirm the sandbox never referenced
         the file being deleted).
missed:  —

Item **28a answered its counting question in two different ways in one walk**,
which is worth recording because only one half was compiler-checked. Changing
`Patterns` from `[]string` to `[]SubscriberPattern` made the Go compiler
enumerate every consumer for me — five sites across `cli/`, `tui/` and the wasm
bridge, each a build error until fixed. **JS has no such guard**, so the browser
half was counted by grep instead: exactly two `.patterns` consumers in
`main.js`, both updated. A type change is a free 28a walk on one side of the
bridge and no help at all on the other.

Item **8 nearly shipped a runtime panic that `make wasm-check` passed.** The
bridge builds `[]any` and appended the pattern value into it — a struct satisfies
`any`, so the compiler was content, and `js.ValueOf` would have panicked on a Go
struct at runtime. The same edit surfaced a second, quieter one: board seq is
UnixNano-seeded (~1.9e18), past `Number.MAX_SAFE_INTEGER`, so `shown` had to
cross as a decimal STRING like `lastSeq` already does. A `float64` would have
rounded to the nearest ~256 and nobody would have seen it.

Item **31 is why the zero case has its own test.** A topic nothing has been
published to reports `shown=0 pending=0`, not an omitted pattern — "subscribed,
nothing yet" and "subscribed, all read" are different states and the whole field
exists to separate them. `TestBoard_ListSubscribersShowsUnpublishedTopic` pins
it. Gate on existence (is this topic subscribed), never on value.

### 2026-08-24 (pre-landing) — `.harness/config`: a client workspace restored on start and reconnect

A named set of connection values, a task to bring back, its forwards and a grid
selection, applied by the TUI on start, on reconnect and on `workspace apply`.
Walked BEFORE the spec was written, which is the first entry in this log where
the trigger fired on time — and the walk changed the design twice before any
code existed.

done:    1 (`--config` / `--workspace` on both binaries, both usage blocks), 2
         (the config delegates every value grammar it does not own: `forward`
         to `cli.ParseForwardSpec` / `ParseRemoteForwardSpec`, `grid` to a
         `cli.ParseGridArgs` EXTRACTED from `tui.parseGrid` for this — the new
         package cannot import `tui`, and a copy would have been a mirror with
         no way to fail loudly), 3 (`workspace save|apply|ls|show` in the TUI
         cmdline), 9 (the workspace feeds `tui.Config`'s existing
         `Server`/`DefaultRepo`, adding no second state), 10 (a new verb family
         on the CLI and the TUI cmdline; `harness-cli forward` deliberately does
         NOT read the file's forward lines — that verb takes its specs as
         arguments and mixing an implicit source in makes "which one took
         effect" unanswerable), 24 (per-path meaning written down: `harness-cli`
         reads only server-cid / ws-path / repo, the TUI reads all of it, and
         there is no `harness-cli workspace apply` at all), 25 (`GridSet` is a
         presence bit — `grid =` with an empty value is the unnarrowed grid and
         is NOT the same as omitting the key), 27 + 28a (the detached resume
         goes through `auth.opts(sessionRequest{…})`, the funnel
         `TestSessionOptsIsBuiltInOnePlace` guards; counted the starter call
         sites before changing their signature — two, plus the ONE
         `PortForwardSession{` construction site), 29 (every apply line names
         its subject: `workspace default: -L 3000:… on 3f2a9c… failed: …`, never
         a bare count), 30 (results to `a.cmdresult`), 31 (the save prints
         `N in-process skipped` even at zero — the operator wondering where
         their `t` pane went needs the zero), 32 (`cli.GridArgsString` is the
         other half of `ParseGridArgs` and its round trip is pinned;
         `cli.PortForwardConfigSpec` is pinned by feeding its output back
         through the forward parsers), 33 (an unknown key, header or enum value
         in the file is a parse ERROR, not a skipped line — a typo'd `fowardd`
         would otherwise establish nothing while the file looks right), 35, 37,
         **S5**
omitted: 4 (no keybinding: re-applying is rare and the cmdline reaches it; a
         dedicated key would need a free letter for an action taken once a day)
         6, 7, 8 (WebUI): a browser has no file to read, which IS the reason
         this design is client-local; and independently it cannot listen, so
         `-L` has no equivalent there — its analogue is the preview pin, which
         binds nothing locally, and `-R` (dial back to the viewer's own host)
         means nothing on a phone. A workspace applied in the WebUI would mean
         something different from the same file applied in the TUI.
         36 — no agent-facing skill mentions the config, deliberately: an agent
         must never read an operator's workspace, and documenting the flag in a
         skill agents read would invite exactly that.
n/a:     5, 11–23, 26, 28, 34, 34a, 38 — nothing new is DISPLAYED on a task or
         runner row and nothing crosses the wire; `FromWorkspace` on
         `PortForwardSession` is client-local teardown bookkeeping, which is why
         it is NOT surfaced: the server-side registry has no notion of a
         workspace and giving it one would mean a wire field. S1–S4, S6 (no
         agent, bin, argv, credential or egress change).
missed:  —

Item **S5 fired, and it is the only item that could have caught this.** The
sandbox wrapper forwards environment into the container BY PREFIX
(`scripts/sandbox/agent-in-podman.sh:385`: `case "$name" in HARNESS_*) CLI+=(
--env "$name" ) ;;`), so a `HARNESS_CONFIG` left in a runner's environment would
ride into EVERY sandboxed agent — pointing at a path that inside the container
either does not exist or is a different operator's file. Nothing in `cli/`,
`tui/` or `cmd/` mentions that forwarding. The answer was to refuse to read any
config when `HARNESS_AUTH_TICKET` is set, which is the unambiguous "I am an
in-task agent" marker (`cli/cliopts/cliopts.go` requires it with no flag
fallback). **Verified live, by accident and then on purpose**: running the new
binary from inside this task silently ignored a malformed config, and the same
file with the variable scrubbed exited 2 naming the line.

Item **33 decided the parser's whole error posture**, not one flag. The
alternative — skip what you do not recognise, the way most ini readers do —
fails silently in the one direction that matters here: a workspace that
establishes nothing looks identical to one that was never applied.

Item **1 turned up a defect in code the change did not add.** `harness-tui`'s
`--server-cid` and `--ws-path` carried their defaults INSIDE `flag.String`, and
a flag default is indistinguishable from an operator typing that value — so the
built-in would have beaten the workspace on every run and `--workspace default`
could never have supplied a server-cid. Both defaults moved out of the flag and
are applied after resolution. The pre-existing tier order (flag > env) was
correct; adding a third tier below it is what exposed that the first tier was
being populated by nobody.

Item **2 is why a package boundary moved.** `cli/workspace` must not import
`tui` (the dependency runs the other way), so the `grid` argument grammar could
not stay in `tui/cmdline.go` if the config was to accept the same string an
operator types. Extracting it to `cli.ParseGridArgs` is what let the config
reuse the parser instead of mirroring it — the item's rule producing a
refactor rather than a check.

**Two design reversals came from the operator, not the walk**, and both are
worth recording because the walk would not have caught either. The first: the
config's `forward` lines had no task binding at all, because I wrote them as a
flat list under the workspace — 「forwardタスクに紐づいてるよね??」. A forward is
registered against a task id and a `-R` is bound to that task's RUNNER, so the
binding belongs in the section header. The second: an `attach` key that
collapsed the wire's three attach modes into one and, being `control`, would
have silently taken a session back from another client on every reconnect —
「そもそも欲しいのはresumeしてはほしいけどattachは別にしてほしくない」. The answer
was to delete the key: resume runs DETACHED and the grid restores the screen,
and grid panes attach as `cowrite`, which the schema defines as non-takeover.

### 2026-08-25 (pre-landing) — `ssh-gateway`: an ssh front door to a session

A listener inside an ordinary harness client. The ssh user name names the task
and the attach mode; the channel is spliced to the existing AttachSession
stream. No wire change, no server change.

done:    1 (`ssh-gateway` verb + usage block in `cmd/harness-cli/main.go`), 3
         (`parseSSHGateway` in `tui/cmdline.go`, beside `parseForward`), 10 (a
         new verb family on the CLI and the TUI cmdline; nothing added to an
         existing family), 24 (per-path meaning written down for the one option
         that has paths: the three user-name forms, what each does to the
         control seat and to the PTY size, and `--authorized-keys` being
         optional on loopback and mandatory off it), 27 (both surfaces call
         `sshgw.Listen`/`Run` with the same `Options`; the host-key default and
         the address-splitting hints live in `sshgw` — `DefaultHostKeyPath`,
         `HostOf`, `PortOf` — rather than being recomputed per surface), 28a
         (counted the builds: `grep -rn 'sshgw.Options{'` returns exactly two,
         one per surface, and the attach itself is built at ONE site inside the
         package), 29 (start names the bound address and the invocation to
         paste into `~/.ssh/config`; stop names the address it is stopping), 30
         (TUI → `a.cmdresult`; the CLI writes to stderr and never to stdout),
         31 (`ssh-gateway` with no argument prints "not running" rather than
         nothing, and an unusable `pty-req` size is reported rather than
         silently defaulted), 32 (the user-name grammar is parsed in ONE place,
         `sshgw.ParseUserName`; the hint line that produces an example is built
         from the same package's `HostOf`/`PortOf`. No JS mirror exists to
         drift, because 6–8 are structurally n/a), 33 (a non-loopback
         `--listen` with no keys is a startup REFUSAL, not a quietly open
         listener; an unparseable host key is an error, not a regeneration; an
         unknown user-name suffix is rejected naming the accepted forms), 35
         (README: the harness-cli summary list, a new **SSH gateway** section,
         and the TUI cmdline verb list), 37 (the spec was amended twice DURING
         implementation — see the miss below)
omitted: 4 (no keybinding: every task-pane key acts on the selected task, and a
         gateway is process-scoped, so there is no row for a key to act on)
         5 (no modal: `PortForwardModal` exists because a forward spec is four
         fields with no default; a gateway takes one optional address)
         36 (no agent-facing skill: this is an operator surface, and a listener
         that hands out operator-grade attaches is not something to document to
         agents)
n/a:     2 (no caps/scope grammar), 6, 7, 8 (a browser cannot accept an inbound
         TCP connection, so no wasm build can host a listener — `cli/sshgw` is
         `!js` and `make wasm-check` proves it never enters that build), 9, 26
         (no spawn), 11–23 (no new task or runner field; a gateway attach shows
         up in the observer counts that already exist — verified in the live
         run's `ls` row), 25, 28 (no wire field, no WAL), 34, 34a (no table
         column, no form), 38 (the gateway renders no screen of its own; it
         hands PTY bytes to an ssh client, and neither live pane is involved).
         S1–S6: no agent added, renamed, or changed in bin, argv, log format,
         credential mode, egress or launch env.
missed:  33 — caught before landing, by the end-to-end test rather than by the
         walk. `exec` was REFUSED with the reason written to the channel's
         stderr, which reads as "takes effect or errors" and is neither: a
         client whose request is refused tears the session down without ever
         draining stderr, so the sentence was written and never seen. The test
         asserted the reason ARRIVES, which is the assertion the item implies
         and the walk does not make. Now accepted-then-answered with exit 1.
         The item's wording is fine; what was missing is that "errors" has to
         mean an error the operator can READ.

Two things this walk is worth recording for beyond the numbers. First, item 6–8
came back `n/a` for a structural reason rather than a scope decision, and that
is the first time in this log that a whole UI is unreachable **by
construction** — worth stating in the spec rather than leaving as three empty
cells, because a later reader would otherwise read it as an omission someone
might close. Second, the walk was run AFTER the code and found nothing 1–10
had not already forced; the value came from 24, 32 and 33, which are the
meaning items. On a verb whose whole content is a grammar and a set of
refusals, the semantics axes are the checklist and the surface list is the
formality.

### 2026-08-25 (pre-landing) — `exec`: a command in a task's worktree, and `exec_count`

A verb family AND a displayed field in one change, so both halves of the list
fire — unlike the `ssh-gateway` walk immediately above, where 11–23 were all
`n/a`.

done:    1 (the verb, its per-verb usage, the `main.go` help block, AND a
         cross-reference added to `session exec`'s line so the two are told
         apart at the point of use rather than only in the README), 3
         (`parseExecRun` in `tui/cmdline.go`, beside `parseForward`), 6 (WebUI
         task-sheet action), 7 (`exec` case in `runCmd`), 8 (`harness.execRun` /
         `execRunList` / `execRunKill` / `execArgvText`), 10 (a new family on all
         three command surfaces plus ssh — and the walk CLOSED an asymmetry it
         found rather than copying it: `forward ls` takes a `-task` filter on the
         CLI and not in the TUI, so `exec ls` got the filter on both), 11
         (`execs=N`), 12, 14 (free: `sessionJSON` embeds `taskJSON`), 15
         (`exec_run` in the catalog, verified against a live server), 16, 17, 20,
         **21**, 23, 24, 28a, 29, 30, 31, 32, 33, 34, 34a, 35, 36, 37
omitted: 2 — there is no grammar to share: the argv is positional and each
         surface's three-line `--` peel differs in shape (the CLI's returns
         (taskID, argv), the TUI's also decides the sub-verb, the JS one is
         inside a switch). The one thing that IS a grammar — argv QUOTING for
         display — has a single implementation, `cli.ExecArgvString`, with the
         wire form as an adapter and the browser calling it over the bridge.
         4, 5 — no keybinding and no modal. The TUI cmdline resolves id
         PREFIXES, so `exec ab12 -- make test` is already short, and a key would
         have to open a text modal that duplicates the line directly below it.
         The WebUI DOES get a sheet action, and the asymmetry is deliberate:
         there the row you tapped supplies the 32-hex id, which is the friction
         on a phone.
         38 — a live pane renders a SESSION's screen, and an exec touches no
         session; that is the property it exists for. Asked the question the way
         the item's own recorded miss says to — "does this surface already
         report anything about this?" — and grepped `PaneStreamer.DiagLine`: it
         reports STREAM quantities for the pane's own attach (rx, reads, B/s),
         not task fields. The count belongs on the row above the pane, where it
         now is.
n/a:     9, 26 (no spawn option), 13 (whoami reports the CALLER's own authority,
         not per-task worktree state), 18 (runner detail describes a runner), 19
         (the picker selects capability/scope TARGETS; a running command is not
         a selection criterion), 22 (no dialog prefills from exec state), 25 (no
         set-style RPC — nothing has to tell absent from zero on the wire), 27
         (no shared request builder is involved; see 28a), 28 (`exec_count` is
         DERIVED at read time from the registry, so there is no WAL field and no
         replay meaning to decide for old records). S1–S6 (no agent added or
         changed in bin, argv, log format, credential mode, egress or env).
missed:  —

Item **21 fires for the first time in this log**, and it came off the
"never fired yet" list. Its shape is worth stating for the next walk: the WebUI
task sheet is an ACTION list, not a field view, so what a verb owes it is the
action — the field went to the row meta (item 20) directly above it.

Item **36 found a real gap and a pre-existing one beside it.** `exec_run` is a
GRANTABLE capability, so an agent can be given it — and no agent-facing text had
the verb at all. `supervising-workers` gained a section (with the table that
separates it from `session exec`, which is the confusion the whole feature
exists to resolve). The same list turned out to be missing `exec_resize` too,
from a change months earlier; both added, and the list now points at
`harness-cli caps` as the authority so the next omission is recoverable.

Item **31 fired twice with opposite answers, and both are right.** In `ls`, the
JSON, the WebUI row and the TUI `d` popup the count prints at ZERO, because
every task can be asked how many commands run in its tree and none is a real
answer — and it is deliberately NOT gated on a live session, unlike the observer
counts beside it, because a task that ended dirty keeps its tree and can still
be exec'd. In the TUI table row it appears only when non-zero, which is a column
ARITY constraint (the same one that already makes Obs itself width-conditional)
and not a value judgement — recorded here rather than left to look like the
elision this item keeps catching.

**What the walk did NOT catch, and what did.** Running the verb against the
dummy harness — whose runner is `--no-worktree` — refused EVERY exec with "no
worktree for task". `execRunDir` knew only the worktree shape while the runner
has two, and `gitSourceFor` in the same package has known both the whole time.
Nothing in 1–38 asks it: the items walk operator SURFACES, and this was a
resource-resolution shape on the runner. Item 24 is the nearest neighbour ("the
same operation, other path") one layer out from an option's paths. Not proposing
a number for one instance — but the general form is cheap to state and worth
counting a second instance against: **when a feature resolves a path the runner
already resolves elsewhere, read that sibling before writing a second
resolution.** The e2e was green throughout, because it runs against a runner
configured the canonical way.

### 2026-08-25 (pre-landing) — `exec --shell`, the runner's OS on the wire, and killing the tree

Three changes that turned out to be one, and none of them found by a walk: an
operator ran `sleep` through the ssh gateway against a WINDOWS runner, killed
it, and the process stayed.

done:    1 (`exec --shell` + both usage layers), 3 (TUI cmdline), 7 (WebUI
         `runCmd`), 8 (`shell` on the bridge), 10 (the option went onto the
         `exec` family on every command surface, not only the one that needed
         it — a pipe is what an operator reaches for and an argv cannot express
         one), 11 + 12 (`os=` on the `ls` runner row and in `--json`), 18 (TUI
         runner detail), 23 (the wasm snapshot's runner row), 24 (`--shell` has
         a written meaning on the run path and is REFUSED on `ls`/`kill`, where
         it would otherwise be a typed option doing nothing), 27 + 28a
         (`ExecRunOpts` is the funnel; counted the builds — `ExecRunRequest{` is
         constructed in exactly ONE place, `cli.ExecRun`), 31 (`unknown`, never
         blank, for a runner that predates the field: blank would read as "this
         row does not report the OS"), 32 (`cli.RunnerGOOSStr` is the one
         renderer, exported for the TUI and crossing the wasm bridge rather
         than mirrored in JS), 33 (`--shell` with a multi-element argv is
         refused at the CLIENT, before a round trip — a caller that built an
         argv AND asked for shell interpretation cannot have meant both), 35,
         36 (`n/a` — no agent-facing text describes the gateway or the shell
         choice; the `exec` section added earlier stands), 37
omitted: 4, 5 (no keybinding, no modal — `--shell` is a word on a line that
         already exists, and the WebUI's runner section has no detail sheet to
         extend)
         19 (the authority picker selects capability TARGETS; a runner's OS is
         not a selection criterion)
         38 (a live pane renders a SESSION's screen; a runner's platform is a
         row field and an exec touches no session)
n/a:     2 (no new grammar — `--shell` is a boolean and the argv quoting rule
         already has its one implementation),
         9, 26 (no spawn), 13 (whoami reports the caller's own authority), 14
         (`session ls` carries TASK rows; goos is a runner field), 15, 16, 17,
         20, 21, 22 (task surfaces — this is a runner field and a verb option),
         25, 28 (nothing persisted; goos is re-reported on every hello), 34,
         34a. S1–S6 (no agent added or changed).
missed:  **6 — twice, and the entry above originally recorded it as `n/a`.**
         The verdict was written from MEMORY of this list rather than from
         reading it, which is the decay the skill's own text warns about; the
         operator asked "checklist は見てますかね?" and re-reading it found both
         in minutes.
         (a) The WebUI task sheet's 「⌨ コマンド実行」 prompts for a COMMAND and
         then `tokenize`d the line into an argv sent verbatim — so `ls | wc -l`,
         the first thing anyone types into a one-line prompt, reached `ls` with
         a literal `|` as an argument. A one-line prompt labelled "command" IS
         the shell case by construction; nobody types an argv into one. It now
         sends the line with ShellLine and says so in its own label.
         (b) The Compose **host-pin dropdown** showed `host [status]` and not
         the OS. That dropdown is where an operator CHOOSES where to spawn, and
         this fleet is deliberately mixed (Windows and Linux runners on one
         server), so the platform a task lands on — its paths, its shell — was
         the one fact missing from the one control that decides it. Adding
         `os=` to the runner ROW and stopping there is what "6" is for.

**The walk found nothing. The operator did, and then the code did.** Worth
recording plainly, because three separate defects reached a live fleet through
walks that were clean:

1. The gateway hard-coded `sh -c` for every platform. No item asks "does this
   value depend on the far side's platform, and do we know it?" — items 1–10
   check that a surface ACCEPTS an option, 11–23 that a field is VISIBLE, and
   24 that an option means the same thing on each PATH. A hard-coded constant
   that is wrong on one OS is none of those.
2. Killing an exec killed one process. Also no item's question.
3. And the fix for (1) put a new bit in FRONT of an existing one in the same
   byte, which is a silent misreading on the old build rather than a decode
   failure. Caught by running the ssh path against the not-yet-restarted live
   server — not by the walk, and not by `wire-skew-check.sh` either, which was
   run only afterwards.

The general form of all three: **they are about the far side's platform and the
wire's layout, and this checklist is about surfaces.** `implementation-pitfalls`
Pitfall 10 owns the wire half and names the script; nothing owns "a constant
whose correctness depends on the peer's OS". Not proposing a number for one
instance — the second one can be counted against this note.

### 2026-08-28 (pre-landing) — `board retract`: the operator withdraws a message

Operator complaint, not a bug report: 「履歴はしばらく保持しときたいけど不都合だから
こっちで消したいって時に purge しかないの不便だな」. A new verb on an existing family
plus two new fields on an existing row, so both halves of the list fire.

done:    1 (`board retract <topic> --seq N` + `boardUsage()` + the top-level help
         line), 4 (`modalKeys.BoardRetractMsg` = `w`, dispatcher case, modal-key
         help line), 6 (⊘ per live message card + `.board-msg-retract`, incl. the
         ≤390px rule), 8 (`harness.boardRetract`, seq as a decimal STRING like
         `boardPurge` — board seq is past `Number.MAX_SAFE_INTEGER`), 10 (the
         `board` family: the verb exists on the CLI, the TUI modal and the WebUI
         panel — the three surfaces this family HAS), 15 (the `purge` catalog
         line now names both verbs it gates), 24 (`--seq` has a different meaning
         on each verb and the difference is written down: 0 means the whole topic
         on `purge` and is an ERROR on `retract`), 27 + 28a (counted:
         `BoardRetractRequest{` is constructed in exactly ONE place,
         `cli.BoardRetract`; every surface reaches the wire through it — the CLI
         wrapper, `DoBoardRetract`, the wasm bridge), 29 (`retracted #<seq>
         (still readable here)`; the CLI's JSON line names topic and seq), 30
         (TUI → `SetStatus` like its purge sibling; WebUI → `appendCmdOutput`),
         31, 32 (`cli.RetractedByLabel` is the one spelling; the bridge ships its
         RESULT rather than letting JS re-derive it from two fields), 33 (a
         missing or zero `--seq` errors and names `board read` for finding one;
         the bridge rejects seq 0 too), 35, 36, 37
omitted: 3, 7 — the TUI cmdline and the WebUI `runCmd` get no `board retract`,
         because neither parses a `board` verb at ALL (re-verified by grep, as
         the `board subscribers` entry did). Unlike the `session snapshot`
         standing omission there is no parser that COULD have taken it; retract
         is at parity with `topics`/`read`/`purge`/`subscribers` in being absent.
         Adding the family is a separate change.
n/a:     2 (no grammar — a topic string and a u64), 5, 9, 11–14, 16–23 (task
         surfaces; this is a board row, same reasoning as the `board
         subscribers` entry), 25 (nothing has to tell absent from zero: seq 0 is
         refused rather than defaulted, and the two new row fields are gated on
         the existing `retracted` bit), 26, 28 (the board is in-memory by design;
         no WAL), 34, 34a (a button beside its purge sibling, not a form field),
         38 (nothing renders a session screen). S1–S6 (no agent, bin, argv, log
         format, credential mode, egress or launch env change).
missed:  —

Item **31 decided the wire, not the rendering.** The base spec had refused a
`retracted_by` field on the grounds that it "would always repeat `from_task`" —
true while one path could withdraw a message. Adding a second path makes the
absent field an active lie: every withdrawn row would read as the author having
done it. So the reversal was recorded AS a reversal (spec §2 and Decision 4 now
point at the amendment) rather than the amendment quietly contradicting them.
The same item then decided the ZERO case twice: `by=` prints on every withdrawn
row including the author's (not elided as "the common case"), and a zero caller
id renders `purge_cap:operator` rather than 32 zeros or a blank — an operator
client holds capabilities directly and has no principal task, which is a real
state. Reading the id alone is wrong in both directions, so the enum and the id
are documented as one field.

Item **15 fired on a capability whose NAME did not change**, which is the shape
worth recording: no new bit, so nothing in the catalog was missing — but `purge`
now gates two verbs with different outcomes, and a description naming only the
destructive one understates what a grant permits. The `.bgn` comment on the bit
carries the same addition, plus the consequence the reuse creates: "may withdraw
but may not destroy" is not a grantable shape.

Item **10's answer differed from the `session snapshot` standing omission**, and
the distinction is worth keeping: there the TUI cmdline HAS a `session` parser
that skips three verbs; here neither command line has the family at all. One is
a gap inside a parser, the other is an absent parser. Both are `omitted`, but
only the first is a candidate for closing without designing a new surface.

**The trigger fired on time for once** — the walk was promised in the design and
run before landing — and it earned two of the numbers above: 15 (the catalog
line, which nothing else would have prompted since no capability was added) and
the 3/7 verdicts, which would otherwise have been silent.

### 2026-08-29 (pre-landing) — the exec LIST gets a view of its own on the TUI and the WebUI

Triggered by the operator asking whether `exec ls` is command-line-only. It was.
The count (`execs=N`) had reached every surface months ago; the LIST had not.

done:    4 (`Execs: "e"` in mainKeyMap AND its mainKeyBindings row — keys_test
         enforces the pair, so a key added to one alone fails the build),
         6 (the 「実行中の exec」 panel on the Connections tab, beside the
         port-forward list, each row with its own kill button — not a new
         control TYPE, deliberately: see 34a),
         8/23 (`execs` on the wasm snapshot, RAW `started_unix_ms` rather than a
         rendered age — the page redraws on every poll, so a formatted age
         would freeze while the row kept being drawn, a clock that looks live
         and is not; the TUI formats at snapshot time because its table holds
         strings and does NOT redraw),
         10 (this walk WAS the item: `exec` is the verb family that had the
         list-and-kill pair without the view its sibling `forward` has had all
         along),
         24 (`DoExecRunList` is now reached from two paths — the `exec ls`
         cmdline verb and the `e` modal, plus the refresh a kill triggers. The
         meaning is written down per path as `ToCmdresult`, copied from
         ForwardsSnapshotMsg, rather than inferred from whether the modal
         happens to be open),
         28a (two call sites, both audited: `grep -c 'DoExecRunList('` = 2),
         29 (the kill reports `killed exec N` and the refresh is SILENT — a
         listing after the report would be a second report nobody asked for),
         30 (TUI → cmdresult, WebUI → appendCmdOutput, neither to a status
         badge),
         31 (empty state prints `running execs (0)` AND `no running execs`
         rather than an absent panel; age prints `-` when started_unix_ms is 0
         rather than eliding the column),
         32 (added `cli.ExecRunOrigin` and routed the CLI table, the TUI modal
         and the wasm bridge through it — the origin was being built inline in
         `ExecRunInfoLines` and would have been re-built twice more by this
         change, which is exactly how the three surfaces come to disagree about
         what an origin is),
         34a (the WebUI row shares the forward row's DOM shape AND its CSS —
         the selectors are grouped (`.forward-row, .exec-row`) rather than
         copied, because a second copy is precisely how the 600px stacking rule
         ends up applying to only one of them; verified at 390px:
         flex-direction column, no horizontal overflow, kill button
         flex-start),
         35 (README: the exec section now says the count and the list answer
         different questions, and names `e` / the Connections panel),
         39 (NEW, and this walk is why it exists — see below)
omitted: 33 (`x` on an empty list is a silent no-op: BeginKillConfirm returns
         false and nothing is said. There is no target to name, and the footer
         one line below already reads `no running execs`. Recorded rather than
         defaulted),
         38 (neither live pane renders this: an exec is not a session — it has
         separate stdout/stderr streams and no PTY, so there is no screen for a
         pane to draw. Searched before recording, per that item's own lesson),
         36 (agent-facing texts describe `exec ls` as a COMMAND, which is still
         exactly true; the two new views are operator surfaces and no agent
         reaches them)
missed:  39 — and it is the reason the item now exists. The task-exec spec's own
         Surfaces table has said "TUI display | running execs listed **like
         forwards**" and "WebUI | a listing **beside the forwards one**" since
         `2026-08-25-task-exec-design.md` was written. Neither shipped. What
         shipped was the cmdline dump into a five-line viewport that
         `GotoBottom`s on every write, so a listing of more than four execs
         showed its tail and hid its own header — which is how the operator met
         it. Every 1–37 item was `n/a` for the change that omitted them,
         correctly: they all walk what a CHANGE must reach, and none of them
         asks whether the feature's own table was ever built. Four days.
         Note that 37 does NOT cover this. 37 keeps the spec from contradicting
         shipped behaviour by amending the SPEC; here the spec was right and the
         CODE was behind it, which 37 has no direction for.


### 2026-08-29 (pre-landing) — `ssh-gateway` becomes workspace state, and `workspace detach`

done:    3 (TUI cmdline: `detach` sub-verb and its `--stop`),
         5 (the save picker gains an ssh-gateway row on `s`, cycling
         keep / running now / none — the grid row's three states, because both
         are one workspace-level value and an enum-shaped choice belongs on a
         control rather than in a text field),
         10 (the `workspace` family: a verb added and a flag added to it, both
         parsed beside the family's existing ones),
         24 (`--stop` has ONE meaning and it is written down: detach clears the
         install either way, and --stop additionally stops the workspace's own
         forwards and its gateway — never a hand-started forward, never a
         session),
         29 (`workspace mine: detached — 0 forward(s) stopped; resumed sessions
         left running` names the target, the count and what was deliberately
         NOT touched),
         31 (the zero prints: "0 forward(s) stopped" is a measurement, and a
         detach that stopped nothing must not read as one that stopped
         something unspecified),
         32 (the config validates the address with the SAME shape check the
         file's other values follow — one grammar, one parser — but see the
         omission below for the half it cannot reach),
         33 (`--stop` on any verb but detach is refused, and `detach <name>` is
         refused: there is only ever one installed workspace, so a name would
         read as "detach that one instead of mine"),
         35 (README: the file example, the detach paragraph, and the reconcile
         sentence),
         37 (BOTH specs — the workspace design gets a fourth-round amendment,
         and the ssh-gateway design's `Workspace config | Intentionally
         omitted` row is struck through with the half of its reasoning that did
         not hold),
         39 (walked the workspace spec's own Surfaces table against the code
         before starting; that is what surfaced `harness-cli workspace` having
         no `apply` — and therefore no `detach` either — as a DECIDED row
         rather than an omission to repair)
omitted: 32, second half (the loopback / authorized-keys refusal is NOT
         validated in the config. `cli/workspace` compiles for js/wasm and
         `cli/sshgw` is //go:build !js, so importing the owning parser there
         would break the wasm build — caught by `make wasm-check`, which is the
         target that exists for it. What a config can check is that the address
         is well formed; the rest stays a runtime refusal in sshgw.Run, where
         the flags are also visible. Recorded because "one serializer" is
         satisfied only in part),
         6/20/21/23 (WebUI: a workspace is a client-local FILE and a browser has
         no file — the design's own Scope says so. Not a surface this can reach)


### 2026-08-29 — port-forward traffic counters + `forward tap`

done:    1, 2, 3, 4, 6, 7, 8, 10, 13, 15, 23, 24, 27, 28a, 29, 30, 31, 32, 33,
         34, 34a, 35, 36, 37, 39
omitted: 25 (`max_record_bytes = 0` is a real value — "the whole payload" — not
         an absent one, so presence needs no bit; stated in the schema comment)
         28 (counters are per REGISTRATION and die with it; a forward does not
         outlive its row, so there is nothing for the WAL to replay. Deliberate:
         persisting them would mean inventing a lifetime the object lacks)
         38 (the TUI grid pane and the WebUI session preview render a SESSION's
         screen; a forward has no screen. Asked properly rather than assumed —
         grepped both panes for what they draw before recording it)

Notes worth keeping:

- **36 fired as a real gap, for the second time in the same shape.** Its own row
  below records `exec_run` shipping grantable-to-an-agent with no agent-facing
  text naming the verb. `forward_tap` is grantable to an agent too, and the
  granular-names list in `supervising-workers/SKILL.md` would have shipped
  without it — the same list, the same omission, one feature later. The list now
  also says what the bit reaches and what it is NOT implied by, because "a name
  in a list" was what let the previous one hide.

- **31 fired on a whole row rather than one field.** Every counter prints at
  zero (`conns=0/0 to-target=0 from-target=0 taps=0`); `last` is the one that
  reads as a word, and it gates on EXISTENCE — no byte has ever crossed — not on
  the value being small. That is the item's own distinction, applied without
  having to be reminded of it this time.

- **34 answered by REMOVING the condition.** The traffic columns are
  unconditional and sit before the flex column, so the column set never varies
  and the swap invariant is untouched. A test now pins row length == column
  count, which is cheaper than the `applyColumns` dance the item describes and
  is available only because nothing here is width-conditional.

- **A guard OUTSIDE this checklist did the work three times.** The repo's own
  completeness tests — `TestPortForwardInfoMapsEveryField`,
  `TestEveryTaskControlKindIsClassified`,
  `TestEveryCapabilityDeclaresHowItsTargetIsResolved`,
  `TestGrantableCapsCoversEveryBitOfAll` — each went red on a half-done step and
  named exactly what was missing. Two of them found things this checklist does
  not ask about at all: that a new capability needs a target-resolution
  classification, and that its handler must pass the bit to `inScope` BY NAME.
  Worth noting because it is the counter-example to the log's premise: not every
  parity failure is a UI surface, and the cheapest guards here are executable.

## Standing tallies

Update when adding an entry.

| item | done | missed | note |
|---|---|---|---|
| 31 (don't hide a value for what it IS) | 17 | **3** | The first two were elisions the item's own text licensed, and the row-width exception was withdrawn for them. The third is a different shape and the most expensive: the re-grant dialog did not merely hide `exclude_self` and the visibility pair, it ERASED them on apply, because it rebuilt the scope from parts instead of carrying the whole. Not-shown and not-kept are one item's problem. The fourth extends the axis again: an empty `spans[]` could not say whether the measurement was TAKEN, so the object reports which style dimensions were collected. The fifth adds not-VALID: `live`'s counts are meaningless without the window they were taken over and without `anchored`, so all three ship together. Not-shown, not-kept, not-measured, not-valid. The thirteenth fired TWICE in one walk with opposite answers: `exec_count` prints at zero on every surface that has room, and appears only when non-zero in the TUI table row — because that one is a column ARITY constraint, not a judgement about the value. Both recorded, so the conditional one cannot later read as this item's failure shape. |
| 16 (TUI task table) | 3 | 1 | Missed once as a defensible `omitted`; the constraint was real, the conclusion was not. |
| 13 (whoami) | 0 | 1 | Also elided `scope=subtree` until `d437f6e`. Easy to forget because it is not a task listing.
| 34 (dynamic column sets) | 4 | 0 | New. Second firing was the popup: same class, different widget. Third was the cheapest kind: a cell's CONTENT grew (`Nx` beside the observer pair) while the column COUNT stayed put, so the swap invariant was untouched — the item's question answered by checking that `rebuild()` is still the only cell builder. |
| 17 (TUI detail popup) | 4 | **1** | Missed the popup's own HEIGHT. The item asks whether a field is visible in the view, never whether the view fits the screen. |
| 33 (take effect or error) | 17 | **1** | First real firing: it turned "the server drops it silently" from acceptable into a bug worth an acknowledgement path. Third firing applied it to a flag-expansion collision rather than a wire value — the same axis one layer out. Fifth was two mutually-exclusive OUTPUT selectors (`--raw` vs `--json`), refused rather than ranked. The tenth is the first where the item caught a defect in the very edit that invoked it: a new flag added to the flag set and not to the stray-flag guard beside it. The twelfth is the first MISS: an ssh `exec` request was refused with the reason written to a stderr no refused-request client ever drains, so "errors" was satisfied while the operator saw nothing. "Errors" has to mean an error someone can READ, and the end-to-end test caught that, not the walk. |
| S1 (preset derivation) | 1 | 0 | First firing of S1–S6 at all. Caught a feature that passed a full 1–37 walk and was still unlaunchable: the gap was agent-launch config, which no UI grep reaches. |
| S5 (env and addressing contract) | 1 | 0 | New, and the second S-item to fire. Same lesson as S1 one axis over: the defect was invisible to every 1–37 item because it lived in the sandbox wrapper's `HARNESS_*` PREFIX forwarding, which no `cli/` / `tui/` / `cmd/` grep reaches. A new client-side env var is automatically an agent-side one, and the item's own wording predicted it: "a new `HARNESS_…` var rides along automatically". |
| 10 (other verb families) | 13 | 0 | First `omitted`: a new `session` verb that the TUI/WebUI command lines do not parse — consistent with the rest of the non-TTY trio, but recorded rather than assumed. Third firing was the useful one: walking the family surfaced an asymmetry that PREDATED the change (`send --snapshot` took `--style` but not `--color`), and the item's answer was to close it in the same walk rather than to match it. |
| 1–10 (input surfaces) | 5 walks | 0 | `n/a` for every field-only change. Do NOT prune: they fired fully for the caps split, which is exactly the change that needed them. |
| 27 (shared funnel) | 7 | **1** | Same walk. Satisfied as written and still shipped the defect: it names the BUILDERS, and the loss was in the builders' callers. 28a is the missing half; if 27 misses again, split it rather than reword it. |
| 32 (one serializer, round-trip tested) | 14 | **2** | Both misses in one session, both the same wording defect: the item claimed round-trip tests that never existed, and "per RUNTIME" licensed the JS mirror that made the loss possible. `OverridesLabel` could not be pasted back; `scopeSpecFor`/`scopeSpecJS` each knew half the grammar. Reworded to one serializer, full stop. A third miss means the problem is not the wording. Fourth firing was PREVENTIVE and is the shape to aim for: it rejected the obvious two-scans implementation of `--json` before it existed, making the text report a projection of the structured form. |
| 28a (follow the value to the request build) | 12 | 0 | Second firing caught the CLI's non-detach --stream splicing NDJSON into a raw terminal BEFORE landing — the first pre-landing catch in this log. Sixth is the cheap-check form the item describes: `grep -rn 'ScreenSnapshot{'` returns exactly one site, so the count answered the question outright. Seventh split the walk in half by language: a Go type change enumerated five consumers as build errors, while the browser's two had to be grepped — the item is free on one side of the wasm bridge and unassisted on the other. |
| 34a (same KIND of control as its neighbours) | 4 | **1** | Missed by omission rather than by wrong shape: the control was right and was not carried to the sibling row in the same dialog. |
| 38 (live screen-rendering surfaces) | 6 | **1** | Born as an `omitted` (neither live pane draws a cursor). Second firing is the one that justifies the number: asking it revealed that both live panes ALREADY merged the Synth frames the native snapshot renderer was dropping, which turned a default-value argument into a three-surface asymmetry with two votes against one. Third was recorded as `omitted` and was a MISS: the reason given ("no verdict to print it beside") was false — the TUI grid pane already had a diagnostic overlay printing the same quantities cumulatively, and the operator named it within the hour. The lesson is about the search, not the item: it asks whether the live panes report this, and I searched for a place to print a VERDICT because that is what I had just built elsewhere. An `omitted` is only as good as the search behind it. Fourth firing applied that lesson deliberately: grepped `DiagLine` for what the pane ALREADY reports before recording the omission, and found stream quantities rather than task fields. |
| 29 (result messages name the target and the change) | 9 | 0 | First row. Fired on a VERDICT rather than a mutation: `--detect` printing only a state would have been unarguable, so the report names the rule, its region and priority, and the text it read. Same item, one layer out from a caps/scope result line. Third firing went further out still — a MEASUREMENT printed beside a verdict, which needed `(no rule reads this yet)` to stop being read as part of it. |
| 36 (agent-facing skill texts) | 4 | 0 | First row. Fired as a real gap rather than mirror drift: `exec_run` is grantable to an AGENT and no agent-facing text had the verb, so a task could hold a capability it could not find. The same list was also missing `exec_resize` from months earlier — one omission hides the next, which is why the list now points at `harness-cli caps` as the authority. |
| 6 (WebUI controls) | 5 | **2** | Both misses in one walk, and both because the verdict was written from memory instead of from the list. A one-line prompt labelled "command" is a shell line, not an argv — `ls \| wc -l` reached `ls` with a literal pipe. And the host-pin dropdown, the control that decides WHICH platform a task lands on, was the one place the new `os=` was not added. The shape to remember: item 6 is not "did the WebUI get a form field", it is "does every control that ALREADY decides this now say so". |
| 39 (feature's own surface matrix vs. what shipped) | 2 | **1** | Born as a MISS, which is the only way this one could have been born: it exists because two rows of the task-exec spec's Surfaces table shipped unimplemented and no other item asks about the spec. Watch whether it fires again as `done` on a feature's LAST walk — if it only ever fires retroactively, the item is a post-mortem rather than a check, and belongs at the end of the walk with teeth (strike the row in the spec, or build it). |
| 15 (caps catalog) | 2 | 0 | First row, and it fired on a change that added NO capability: `purge` now gates two verbs with different outcomes (`board purge` destroys, `board retract` withdraws), so the catalog line understated the grant while being literally accurate. The item's question is not "was a bit added" but "does the description still name everything the bit reaches". |

**Never fired yet:** 26. (21 came off this list with the `exec` entry: the WebUI task sheet is an ACTION list, so a verb owes it the action while the field goes to the row meta above it.) Too few walks to call either dead — revisit after
another change that adds a spawn OPTION rather than a display field. (29, 30
and 24 came off this list with the `87c10a2` entry.) 25 fired again on the
workspace entry, in its cleanest form yet: `grid =` with an empty value is a
real selection and an omitted `grid` key is not, so presence needed its own bit
in a plain Go struct — the same question the wire asks, one layer up from it.

(8, 9, 22 and 25 came off this list with the `--scope-for` entry above; 27
came off it by MISSING, which is still a firing.)

(33 was on this list until `aa4a1dd`. Keeping the two halves consistent is
manual, so check the tallies against the entries when adding one — a log that
contradicts itself is worse than no log.)
