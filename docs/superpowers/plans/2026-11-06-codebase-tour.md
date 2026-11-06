# Codebase Tour Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an embedded agent skill, `codebase-tour`. It writes one page
that relates the code to the owner's decisions, then handles the owner's
pushback on it. Also make running-parallel-workers run it between landings.

**Architecture:** The skill is a single procedural `SKILL.md` under
`runner/agentskills/codebase-tour/`. It ships no scripts (spec D14). It is
embedded through the existing `//go:embed` list and mirrored byte-for-byte
under `.claude/skills/` and `.agents/skills/`. running-parallel-workers gains
a Checkpoints entry, a conventions section, a `tours/` records directory and
a parent-state line.

**Tech Stack:** Markdown skill text; Go `embed` (`runner/agentskills`); Python 3
stdlib (`parallel_common.py`).

**Spec:** `docs/superpowers/specs/2026-11-06-codebase-tour-design.md` — read it
in full, Decisions table included, before Task 1.

## Global Constraints

- The skill text names nothing specific to one repository (D10). Lessons
  from the trial are stated as mechanisms with their reason, never as this
  repository's files.
- Nothing committed contains a local absolute path, host name or private
  location. The repository is public. Absolute paths appear only in pages,
  which are never committed (D8).
- About three items per page (D7). With no earlier page, the default
  baseline is the commit 30 days back (spec §1 step 1).
- Questions ask intent or choice and never quiz (D5). The page uses the
  owner's language in a polite register (D9).
- Only the owner's decisions explain a difference away. An agent's decision
  may anchor an item only when labelled as an agent's (D6, D15).
- Every mirror is byte-identical to its `runner/agentskills/` source
  (`TestMirrorsMatchEmbeddedSkills`).
- Stage with explicit paths only, never `git add -A`. The worktree has
  unrelated untracked files.
- Before writing skill text, load `superpowers:writing-skills` for skill
  authoring. Read `.claude/skills/implementation-pitfalls/SKILL.md` per this
  repository's CLAUDE.md.

## Review Focus

These are the inputs most likely to break the tour that no automated test
reaches. Each must be handled in the skill text, and each owning task has a
review step for it.

1. **A repository whose records mark no decision maker** (no `Decided by`
   column, no `DECIDED (operator, …)`). The first run must settle D16 with
   the owner. It must not treat every record as the owner's. → Task 1.
2. **A first run with no earlier page and no tour directory.** It must ask
   for the directory (D17), take the 30-day baseline or the owner's point,
   and write `settings.md`. It must never write into the repository. → Task 1.
3. **A difference explained only by an agent's decision** (a "this spec"
   row, a plan line, a comment). It must stay on the page labelled as an
   agent's decision with the stand-by question. It must not be dropped.
   → Task 1.
4. **Pushback inside running-parallel-workers that asks for a fix.** The
   parent must route it to a backlog item, an assignment or a `Q<n>`, and
   must not edit code itself (D13). → Tasks 1 and 2.
5. **The landing count is reached while the owner is away.** The tour waits
   for the owner's return; no page is written to an empty room (D11).
   → Task 2.

---

### Task 1: The `codebase-tour` skill

**Files:**
- Create: `runner/agentskills/codebase-tour/SKILL.md`
- Modify: `runner/agentskills/embed.go:17` (the `//go:embed` line)
- Modify: `runner/agentskills/agentskills_test.go:58` (the `want` map)
- Create (mirrors, byte-identical): `.claude/skills/codebase-tour/SKILL.md`,
  `.agents/skills/codebase-tour/SKILL.md`

**Interfaces:**
- Produces: the skill name `codebase-tour`, which Task 2 refers to by name;
  the tour directory layout `<tour dir>/<date>.md` + `<tour dir>/settings.md`
  (D17); the page format of spec §1.

- [ ] **Step 1: Write the failing test.** Add `"codebase-tour": false` to the
  `want` map in `TestList` (agentskills_test.go:58).

- [ ] **Step 2: Run it to see it fail.**
  Run: `go test ./runner/agentskills/ -run TestList`
  Expected: FAIL with `List() missing expected skill "codebase-tour"`

- [ ] **Step 3: Write `runner/agentskills/codebase-tour/SKILL.md`.**

  Frontmatter: `name: codebase-tour`. The `description` says when to use
  it: the owner wants to see where the code stands against their decisions,
  or running-parallel-workers' tour checkpoint fires.

  The body must carry each of the following, each with its reason, in the
  style of running-parallel-workers (a rule plus a `Why:`):
  - the purpose and the form: one page, then pushback (D2)
  - the yardstick: the owner's decisions, how far they got (D3)
  - the item sources and the search-aid-only role of signals (D4)
  - the claim check (D6)
  - agent decisions: anchoring and labelling, with no power to cancel a
    finding (D15)
  - the D16 marking and the D8 location form, settled once and kept in
    `settings.md`
  - the tour directory (D17)
  - the never-quiz rule (D5), the language and register (D9), about three
    items (D7)
  - the pages are never committed (D8)
  - the 10-step procedure (spec §1)
  - the page format, copied verbatim from spec §1
  - pushback routing in both contexts (D13)
  - the reaction lines (D12)
  - no unattended runs (D11)

  Review Focus items 1–4 must each be answerable from the text.

- [ ] **Step 4: Add `all:codebase-tour` to the `//go:embed` line in
  `embed.go`, keeping the names in alphabetical order.**

- [ ] **Step 5: Run the package tests.**
  Run: `go test ./runner/agentskills/`
  Expected: `TestList` passes. `TestMirrorsMatchEmbeddedSkills` FAILS for
  the two missing mirrors.

- [ ] **Step 6: Copy the skill into both mirrors and rerun.**
  Run: `for d in .claude/skills .agents/skills; do mkdir -p $d/codebase-tour && cp runner/agentskills/codebase-tour/SKILL.md $d/codebase-tour/; done && go test ./runner/agentskills/`
  Expected: `ok`

- [ ] **Step 7: Check that the CLI serves it.**
  Run: `go run ./cmd/harness-cli skill codebase-tour | head -5`
  Expected: the frontmatter, starting `name: codebase-tour`.

- [ ] **Step 8: Review against Review Focus 1–4 and the Global
  Constraints.** For each item, quote the sentence in `SKILL.md` that
  handles it. Then grep the file for local paths and host names; there
  must be none. `grep -nE '/home/|/tmp/|gmkhost|\.harness-worktrees' runner/agentskills/codebase-tour/SKILL.md`
  must print nothing.

- [ ] **Step 9: Commit.**
  ```bash
  git add runner/agentskills/codebase-tour/SKILL.md runner/agentskills/embed.go \
    runner/agentskills/agentskills_test.go \
    .claude/skills/codebase-tour/SKILL.md .agents/skills/codebase-tour/SKILL.md \
    docs/superpowers/specs/2026-11-06-codebase-tour-design.md \
    docs/superpowers/plans/2026-11-06-codebase-tour.md
  git commit -m "agentskills: codebase-tour, one page for the owner to push back on"
  ```

### Task 2: running-parallel-workers runs the tour

**Files:**
- Modify: `runner/agentskills/running-parallel-workers/SKILL.md`. Change the
  records tree in "Before you start", "The parent-state file", the owner-away
  bullet in "Owner decisions", and "Checkpoints".
- Modify: `runner/agentskills/running-parallel-workers/references/repo-conventions.md`
  to add a section "Codebase tour" after "While the owner is away".
- Modify: `runner/agentskills/running-parallel-workers/scripts/parallel_common.py:184`
  (`SUBDIRS`)
- Mirrors (byte-identical): the same three paths under `.claude/skills/` and
  `.agents/skills/`

**Interfaces:**
- Consumes: the skill name `codebase-tour` and the tour directory layout
  (Task 1).
- Produces: `<records>/tours/` from `parallel_common.py init`; three
  conventions lines that the tour copies into `settings.md`.

- [ ] **Step 1: Write the failing check.**
  Run: `d=$(mktemp -d)/rec && python3 runner/agentskills/running-parallel-workers/scripts/parallel_common.py init "$d" && test -d "$d/tours" && echo TOURS_OK`
  Expected: no `TOURS_OK`, because `tours/` is not created.

  If `init` needs a git repository as its working directory, run it from
  the worktree root; its usage text says so.

- [ ] **Step 2: Add `"tours"` to `SUBDIRS`.** Rerun the step 1 command.
  Expected: `TOURS_OK`.

- [ ] **Step 3: Edit `SKILL.md`.**
  - In the records tree, add a `tours/` line: "codebase-tour pages,
    `settings.md`, reactions".
  - In "The parent-state file", record the last tour's date and commit and
    the landings counted since then.
  - Under "While the owner is away", add that no tour runs, and that a
    count reached then waits for the owner's return (D11).
  - In "Checkpoints", add an entry. After the conventions' number of
    landings since the last tour, with the owner present, settle the
    children's next items first, then run `codebase-tour` on
    `<records>/tours/`. Pushback that asks for a fix becomes a backlog item,
    an assignment or a `Q<n>`, because the parent does no child's work
    (D13). Give it a `Why:` in the skill's style, citing the mechanism
    (owners stop reading agent-written code, and passing tests hide drift
    from their decisions), not this repository.

- [ ] **Step 4: Add the "Codebase tour" section to `repo-conventions.md`.**
  It has three `<...>` decision lines in the file's style:
  - the number of landings between tours
  - how code locations are written so the owner can open them (D8)
  - where the owner's decisions are recorded and how their maker is marked
    (D16), e.g. a `Decided by` column

- [ ] **Step 5: Copy the three changed files into both mirrors and run the
  tests.**
  Run: `for d in .claude/skills .agents/skills; do for f in SKILL.md references/repo-conventions.md scripts/parallel_common.py; do cp runner/agentskills/running-parallel-workers/$f $d/running-parallel-workers/$f; done; done && go test ./runner/agentskills/ && git diff --stat`
  Expected: `ok`. The stat lists the three files under all three trees.

- [ ] **Step 6: Review against Review Focus 4 and 5.** Quote the sentences
  that handle them.

- [ ] **Step 7: Commit** the nine paths explicitly, with the message
  `running-parallel-workers: run codebase-tour between landings`.

### Task 3: Acceptance run on this repository (with the owner)

This task writes nothing to the repository.

- [ ] **Step 1:** Run `codebase-tour` standalone on this repository with the
  owner present. On the first run, settle D17's directory, D8's location
  form and D16's marking with the owner.
- [ ] **Step 2:** Confirm the page against spec §3:
  - it follows the format
  - every item is anchored on a recorded decision, labelled as the owner's
    or an agent's
  - no claim is shown wrong by a decision record during the pushback
  - the reaction lines are filled in
- [ ] **Step 3:** If the run exposes a gap in the skill text, fix the skill
  and its mirrors, rerun `go test ./runner/agentskills/`, and commit. Then
  land Tasks 1–3 together by fast-forward, followed by `make build` in the
  main checkout.
