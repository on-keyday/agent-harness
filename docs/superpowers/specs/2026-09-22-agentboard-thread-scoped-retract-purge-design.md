# Thread-scoped retract and purge, on the unit the view already groups by

`board thread` assembles conversations across topics and `board thread --json` /
the WebUI Export sheet take them somewhere. Nothing takes them *away*. The
destructive verbs are per-(topic, seq), and a conversation is split across at
least two topics by construction, so clearing one is N calls over M topics with
the operator holding the mapping in their head.

## Decisions taken

| Decision | Decided by |
|---|---|
| Both verbs — `retract-thread` AND `purge-thread`, not one | operator, 2026-09-22 |
| No enforced export before purge; placement only | operator, 2026-09-22 |
| Client-side fan-out over existing per-seq calls; no wire, server or `.bgn` change | operator, 2026-09-22 |
| WebUI: buttons on the existing per-conversation header; TUI: chains becomes list → detail | operator, 2026-09-22 |
| `ThreadFilter` gains `Conversation`, because `--seq` selects a chain and the unit is a conversation | claude, approved by operator 2026-09-22 |

The operator's stated workflow, which the shape follows: **retract when the
discussion ends so a resumed peer cannot re-read it, export later, purge after
that.** Two moments in time, not one — which is why this is two verbs and not a
combined `archive-thread`.

## Problem

Four distinct gaps, all reachable today:

1. **A finished discussion cannot be withdrawn as a unit.** `agent retract`
   takes one seq and checks authorship; `board retract` takes one
   (topic, seq). A 22-message exchange over 3 topics is 22 calls, and the
   operator must know which topic each seq sits on.
2. **A retracted thread still clutters every operator surface.** Withdrawal
   moves a message from `topic.ring` to `topic.retracted`
   (`agentboard/topic.go` `withdrawLocked`); `board read`, the TUI board modal
   (`tui/board.go:635`) and the WebUI chains rows all keep rendering it with a
   RETRACTED badge. That is deliberate — the operator audit window must not be
   shrinkable in seconds — but it means retract does not answer "get this off
   my screen". Only purge does.
3. **The destructive verbs cannot name the unit the read verbs group by.**
   `ThreadFilter` has `Seq` (the chain containing it) and `Tasks` (a union that
   deliberately also pulls in third-party exchanges). Neither is a
   conversation. `conversationKey`'s own comment records the measurement:
   *"a two-party exchange five minutes long produced four chains and one
   conversation."* So `--seq` under-selects and `--task` over-selects.
4. **The export half already shipped; the destruction half did not.**
   `672ac902` (2026-09-22) gave the WebUI chains view an Export modal rendering
   `cli.RenderThreads` with Text/JSON, Copy and Download. The operator's loop
   ends there and the messages stay.

### What is NOT the problem

- **Atomicity.** The fan-out is not atomic and does not need to be. The
  operation is idempotent, the per-seq outcome is reported, and the subject is
  a discussion that has already ended.
- **Retention.** Topics age out 30 minutes after their last publish and a
  server restart drops the board entirely. This feature is about clearing on
  purpose, not about retention policy.
- **Data safety by enforcement.** The operator declined coupling purge to a
  proven export. The safety mechanism here is the same one the repo already
  uses — make the wide form be typed — not a new confirmation ceremony no
  sibling verb has.
- **A new capability.** In the discovery / content-read / destruction
  triage, this is destruction on the operator face, which is exactly the
  `purge` bit `board retract` and `board purge` already carry.

## Scope

**In:** `ThreadFilter.Conversation` and a `--conversation` flag on the three
thread verbs; two new CLI verbs; a fan-out helper with per-category reporting;
a TUI chains list → detail restructure with `w`/`X` on the highlighted
conversation; two WebUI buttons on the existing conversation header; two wasm
bridge functions; the `purge` capability description; README; the agent-facing
skill.

**Out:** any server, wire or `.bgn` change. Any change to how threads are
assembled or rendered. Any new capability. A combined export+destroy verb.

## The selector

`ThreadFilter` gains one field:

```go
type ThreadFilter struct {
	Tasks        []string
	Seq          uint64
	Conversation string // the key groupConversations stamps on every row
}
```

`SelectThreads` gains one branch. The key is already computed
(`conversationKey`), already stamped on every row (`groupConversations`), and
already shipped to the browser as `conversations[].key`. It is printable and
typeable by construction: a named topic (`rr.dec-019`) when the chain touched
one, otherwise the sorted `+`-joined 8-hex party prefixes
(`60542da9+70fbad4a`), otherwise `(unattributed)`.

Adding it to `board thread` as well is not scope creep — it is the property the
whole design rests on: **the expression that names what you read is the
expression that names what you destroy.** `board thread --conversation X`
prints exactly what `board purge-thread --conversation X` removes.

## The two verbs

```
board retract-thread  [--seq N] [--task ID]... [--conversation KEY]
board purge-thread    [--seq N] [--task ID]... [--conversation KEY]
```

**No topic positional.** This is the visible difference from
`board retract <topic> --seq N` and `board purge <topic>`, and it is the point:
a conversation spans topics, so requiring one makes the unit inexpressible.
The `Notes` say so, because a reader who knows the sibling verbs will look for
the argument.

**`AtLeastOne: {seq, task, conversation}`.** `board thread` with no selector
prints every chain on the board; a destructive twin inheriting that default
means "destroy the board". This is the `--seq`-left-at-zero shape that
`cli/verb/table.go` records as having *"destroyed two messages on a live
board"*, and the same answer `prune` already gives: the widest form has to be
asked for. `TestWidestFormIsNeverTheBareOne` enforces it.

**No `--force` / `--yes`.** The repo has no `--yes` or `--confirm`; `--force`
appears five times and is never the safety on a destructive default — `prune`'s
safety is `AtLeastOne`. Inventing a confirmation no sibling has would make this
verb's ceremony a local dialect. The preview is `board thread` with the same
selector, and the Examples print the pair.

**Wiring:** `Action: "BoardAction"` with `Const: {"Sub": "retract-thread"}` /
`{"Sub": "purge-thread"}`, two cases in `RunBoardAction`'s switch.
`CmdlineSurfaces: CLI`, matching every other `board` sub-verb — neither the TUI
nor the WebUI has a `board` verb family to type into
(`cli/verb/table.go:1057`). `ModalSurfaces`: TUI `tui/board.go:BoardModal`,
WebUI `webui/index.html#board-chains-view`.

## The fan-out, and reporting it honestly

One helper, reached by all three call sites (CLI, TUI action, wasm bridge):

1. `CollectThreadsWith(ctx, c, filter)` → the rows.
2. Print `ConversationHeader(rows)` — the same string `board thread` prints, so
   the target is named in the vocabulary the operator just read it in.
3. Partition locally. **A row whose `Msg.Retracted` is already true is not
   called for.** It is counted `already-withdrawn`.
4. For the rest, `BoardRetract(ctx, topic, seq)` or
   `BoardPurge(ctx, topic, seq)` per row, using the row's own topic.
5. Summarise every category, zeros included.

```
$ harness-cli board thread --conversation 60542da9+70fbad4a        # look
$ harness-cli board retract-thread --conversation 60542da9+70fbad4a
thread: claude/70fbad4a ↔ agy/60542da9   10 messages across 2 topics
  retracted 5   already-withdrawn 5   not-found 0   failed 0   skipped 0

$ harness-cli board purge-thread --conversation 60542da9+70fbad4a
thread: claude/70fbad4a ↔ agy/60542da9   10 messages across 2 topics
  purged 10   not-found 0   failed 0   skipped 0
```

**Why step 3 is not cosmetic.** The server's `not_found` deliberately collapses
unknown-topic, unknown-seq, already-withdrawn and seq-0 into one answer so the
reply cannot be used to probe a topic. A naive fan-out therefore reports a
thread that is half-withdrawn as `retracted 5 / not_found 5` — success rendered
as failure. This is not hypothetical: in the exchange that prompted this
feature, 11 of 22 messages were already auto-retracted, because a reply
withdraws the message it answers on the sender's behalf. The client has just
read every row and holds `Retracted` per message, so the distinction is free
and needs nothing from the server.

**Purge has no `already-` category.** `removeSeq` scans `ring` and then
`retracted`, so purge reaches withdrawn messages; a `not-found` on a re-run
correctly means "already gone".

**Errors stop; `not_found` continues.** A capability denial on the first call
is a denial on every call, so the helper does not hammer the server with 21
more. Transport errors likewise. What completed before the stop is printed,
with the untried remainder counted as `skipped N` and the error itself on the
next line, and the operation is idempotent, so a re-run finishes the job. On a
clean run `skipped` is 0 and is printed like every other category.

`--json` emits one record per row — `seq`, `topic`, and `outcome`, whose values
are exactly the summary's categories: `retracted` / `purged`,
`already-withdrawn` (retract only), `not-found`, `failed`. A row the helper
never reached because an earlier error stopped it carries `skipped`, so a
reader can tell "we tried and it was gone" from "we never got there" — the same
distinction the summary's stop-and-report makes in prose.

## Surface matrix

| Surface | Retract a thread | Purge a thread | Notes |
|---|---|---|---|
| CLI | `board retract-thread` | `board purge-thread` | selector required |
| CLI read | `board thread --conversation` | — | same selector, same rows |
| TUI | `w` on the highlighted conversation in the chains **list** | `X` on it | chains becomes list → detail, matching topics → `Enter` → messages |
| WebUI | button on `.board-chain-conv` | button on `.board-chain-conv` | the header already carries `ConversationHeader`; no selection model added |
| WebUI Export modal | unchanged | unchanged | stays export-only; its content is "everything shown", not one thread |
| wasm bridge | `harness.boardRetractThread(key)` | `harness.boardPurgeThread(key)` | the browser passes back the key it was given |

**The invariant this table exists to hold: no surface offers a wider
destructive form than the CLI allows.** The CLI requires a selector; the UIs
require a selection. A button acting on "whatever is displayed" would be the
bare form the CLI refuses to have, which is why it is not in the Export modal —
`harness.boardThread()` takes no arguments and returns every conversation on
the board.

**TUI restructure.** The board modal is already list → detail everywhere else
(topics → `Enter` → messages → `w`/`X` on a message). `chains` is the one view
that is a scrolled blob with no cursor. Making it a list of conversations
(`ConversationHeader` + counts) with `Enter` into the existing chain rendering
removes that asymmetry and supplies the selection the action needs, in one
move. Key meanings stay what the modal already established: `w` = retract,
`x`/`X` = purge, scope = whatever the view is a view of.

## Testing

**Free, from the existing invariant suite**, once the table rows exist:
`TestWidestFormIsNeverTheBareOne` (the `AtLeastOne`),
`TestEveryVerbReachesSomeSurface`, `TestSurfaceNarrowingHasAReason`,
`TestEveryDeclaredFlagIsReadByItsBuild`, `TestNoDuplicateSpellings`,
`TestUsageNamesItsVerb`, `TestUsagePositionalsParse`, and `rules_test.go`'s
`TestDeclaredRulesRefuseWhatTheBuildsRefused` /
`TestDeclaredRulesAcceptTheOrdinaryForms` on the rule from both sides.

**Written here:**

1. **`--conversation` is not `--seq`.** Fixture shaped like the measurement in
   `conversationKey`'s comment — a two-party exchange producing four chains and
   one conversation. Assert `--seq` and `--conversation` return **different**
   row sets. A test where both pass on the same fixture does not exercise the
   reason the field was added.
2. **`already-withdrawn` classification.** A recording fake client. Assert no
   call is issued for a row whose `Retracted` is set, and that the summary is
   `retracted N / already-withdrawn M / not-found 0`. Pins the 11-of-22
   regression directly.
3. **Partial failure.** First error stops; `not_found` continues; what
   completed before the stop is reported.
4. **Two-stage lifecycle, e2e** (beside `cli/board_e2e_test.go`). Publish a
   small cross-topic exchange; `retract-thread`; assert the agent-facing paths
   no longer deliver it while `board read` still shows it withdrawn;
   `purge-thread`; assert `board read` no longer shows it. This pins by test
   what is currently only known by reading `removeSeq` — that stage 2 reaches
   stage 1's output. The whole feature rests on it, so it is checked, not
   trusted.
5. **TUI** (`tui/board_test.go`): `w`/`X` in the chains list act on the
   highlighted conversation only. This is the surface-parity invariant, not
   presentation.
6. **WebUI** (`make js-test`): the header button passes that row's
   `conversation` key and nothing else.
7. **One fan-out, not three.** A test that fails when a second
   `BoardRetract`/`BoardPurge` loop appears outside the helper — the
   `tui/session_opts_test.go` precedent for the same failure class.

Verification runs through `make check`, `make test`, `make vet` and
`make test-integration`. Not bare `go build` / `go test ./...`, which hide
pattern breaks.

## Risks

- **A conversation key is not stable across a task's lifetime.** The key is
  derived from participants or a named topic; the previous walk's firing-log
  entry already records that a supervisor sending each instruction from a fresh
  task splits into several conversations. A key typed from an older listing can
  therefore select nothing. Mitigated by the selector being echoed in the
  header before anything is destroyed, and by `not-found` being reported rather
  than swallowed. Not fixed here: a stable identity above the task id would be
  a wire concept.
- **Race between collect and destroy.** A message published into the thread
  between step 1 and step 4 is missed. Benign for the stated use (a finished
  discussion) and visible as a leftover row on the next `board thread`.
- **`--task` remains the over-selecting selector.** It unions in third-party
  exchanges by design. Its help text must say so on the destructive verbs,
  where over-selection is not merely a wider view.

## Completion

- [ ] `ThreadFilter.Conversation` + `SelectThreads` branch + `--conversation`
      on `board thread`
- [ ] Two verb table rows with `AtLeastOne`; two `RunBoardAction` cases
- [ ] Fan-out helper with local `Retracted` partitioning and zero-preserving
      summary
- [ ] TUI chains list → detail; `w`/`X` on the highlighted conversation
- [ ] WebUI conversation-header buttons; two wasm bridge functions
- [ ] `CapDescription(Capability_Purge)` names the thread-scoped forms
- [ ] README board section + TUI cmdline verb list
- [ ] `runner/agentskills/harness-cli/SKILL.md` + both mirrors
- [ ] Tests 1–7 above; `make check` / `test` / `vet` / `test-integration` green
- [ ] **Item 39 walked against the Surfaces table above, row by row**, and a
      `firing-log.md` entry written

## Surface-parity walk (1–39)

Walked 2026-09-22, before implementation. Item 39 is explicitly an
end-of-feature check and is the last box in Completion above.

**Input surfaces**

1. **done** — two `VerbSpec` rows in `cli/verb/table.go`, plus `--conversation`
   on the existing `board thread` row. One declaration, three command lines.
2. **done** — `AtLeastOne{seq, task, conversation}` is declared on the spec and
   enforced in `Build`, not in any surface.
3. **done** — pointer only; the button/key half is items 4, 6 and 8.
4. **done** — TUI chains list gains ↑/↓/`Enter`, `w`, `X`. Note: the board
   modal's keys are dispatched inside `tui/board.go`, not `mainKeyMap`, so
   `keys_test`'s map/binding pair rule does not reach them; confirm during
   implementation whether the modal footer strings carry their own assertion,
   and add one if not.
5. **n/a** — no picker or popup; the action is a key inside an existing modal
   view.
6. **done** — two buttons appended to the existing `.board-chain-conv` header
   element in `renderBoardChains`.
7. **n/a** — `CmdlineSurfaces: CLI`, so `pathsForSurface("webui")` does not
   return these paths and `WEBUI_DISPATCH` has nothing to name. Same shape as
   `board purge` today.
8. **done** — the WebUI does not reach these through the grammar, so each needs
   its own bridge function: `harness.boardRetractThread(key)` /
   `harness.boardPurgeThread(key)`.
9. **n/a** — not a spawn; no session-default state is read.
10. **done** — pointer; the output conventions are items 29–31.

**Display surfaces**

11–14, 16–18, 18a, 19–23. **n/a** — no `TaskInfo` / `RunnerInfo` / snapshot
field is added or changed. Nothing in `ls`, `whoami`, `session ls`, the TUI task
or runner tables, their detail popups, the picker rows, the WebUI task row meta
or detail sheet, or the wasm snapshot conversion changes.

15. **done** — `CapDescription(Capability_Purge)` in `cli/caps.go` enumerates
    the verbs the bit authorizes (*"agent purge / board purge … board
    retract"*). Two more forms exist after this change and the sentence must
    name them, or the catalog understates what granting `purge` hands over.

**Semantics axes**

24. **done** — `--conversation` is reachable from three verbs. On
    `board thread` it filters a read; on the two destructive verbs it bounds a
    destruction. The meaning is identical by construction (one
    `SelectThreads` call), and that identity is the design, so it is written
    here rather than implied. Separately, `--seq` means the same thing on all
    three (the chain containing it) but is **narrower than a conversation** —
    the destructive verbs' help text must say so, because on a read that is a
    smaller view and on a destroy it is a half-cleared thread. No
    `Flag.Resolve` ladder is involved.
25. **n/a** — no wire field is added; the selector is resolved client-side and
    never crosses the boundary. There is no zero-vs-absent question to answer.
26. **n/a** — not a spawn; no resume path.
27. **done** — CLI, the TUI action and the wasm bridge all reach one
    `CollectThreadsWith` + one fan-out helper. Letting each build its own loop
    is the Pitfall-3 shape verbatim.
28. **n/a** — the board is in-memory; no `WALEvent` and no replay meaning.
28a. **done** — the fan-out helper is the single construction point for
    `BoardRetract`/`BoardPurge` calls on this path. Grep every call site of
    both and confirm none of the three surfaces grew a second loop; pinned by
    test 7.

**Conventions**

29. **done** — the result names the target (`ConversationHeader`) and the
    change (per-category counts), never a bare count.
30. **done** — WebUI results go to `appendCmdOutput`, TUI to `a.cmdresult`;
    `setStatus` is the connection badge and is not touched.
31. **done** — `not-found 0` and `failed 0` are printed. Gated on the category
    existing, never on its value. This is the item the reporting design turns
    on: a zero here is a measurement, and eliding it would make "nothing
    failed" indistinguishable from "failures were not counted".
32. **done** — the conversation key is produced by Go (`conversationKey`) and
    handed to the browser, which passes back the string it was given and never
    constructs or re-derives one. If the browser ever needs to build a key, the
    Go function gets exported over the bridge rather than mirrored in JS.
33. **done** — a selector-less invocation errors via `AtLeastOne`; it never
    silently widens.
34. **n/a** — the TUI chains list renders as text lines, not a bubbles table
    with a conditional column set. Stated as a decision rather than assumed: if
    implementation reaches for `table.Model`, this item fires and the
    `applyColumns` shape applies.
34a. **done** — the WebUI control is an action button among the action buttons
    the view already has. The operator never types a conversation key in the
    browser; the key is carried by the row.

**Live surfaces and the spec's own table**

38. **n/a** — `tui/pane_streamer.go` and the WebUI session preview render a
    session's screen. This feature is about board messages and touches neither.
39. **pending** — explicitly an end-of-feature check. It is the last box in
    Completion, to be walked against the Surfaces table above once the code
    exists, with the result recorded in `firing-log.md`.

**Documentation surfaces**

35. **done** — README's board section and the TUI cmdline verb list.
36. **done** — `runner/agentskills/harness-cli/SKILL.md` already tells agents
    that somebody else can withdraw their messages
    (*"Somebody else can withdraw your message"*). Two more forms can now do so
    at conversation scope, which changes what an agent should conclude when its
    messages vanish. Edit the go:embed source, then mirror to `.claude/skills/`
    and `.agents/skills/` in the same commit
    (`TestMirrorsMatchEmbeddedSkills` enforces the bytes).
37. **done** — this document. Semantic changes after it lands go in as
    Amendment sections rather than edits, so it never contradicts shipped
    behaviour.

**S1–S6** — **n/a**, trigger did not fire: no agent is added, renamed or
removed; no bin path, argv template, log format, config directory, credential
mode, egress domain, launch env or server addressing changes.
