# Codebase tour: one page for the owner to push back on — design

## Problem

When agents write the code, the owner stops reading it, and the work
still functions. Specs get written, but the owner does not always read them
either. Nothing then shows the owner where the code has moved away from what
they decided. Tests pass, behaviour is right, and the agent has no reason to
report that a decision was only half carried out or that a structure is
turning into debt. Noticing that takes a human looking at the real thing.

- **P1 — the owner's picture of the codebase goes stale.** The owner keeps
  the intent of past decisions; the agents have the layout. Whether the layout
  still matches the intent goes unchecked.
- **P2 — divergence is noticed late.** By the time unease turns into a
  finding, the pattern has spread. Examples are the verb grammar written three
  times over (`2026-09-03-cli-verb-ssot-design.md`, Problem) and, in
  running-parallel-workers' own history, a formatting convention that reached
  214 files.
- **P3 — a diff review cannot carry this.** A diff shows what changed,
  not what is missing, not what holds across branches, and not how a change
  relates to an earlier decision.

**Trial run (2026-11-06, this repository).** A first page was written by
hand. What it showed:

- Its three items were picked by signals the author chose: comments calling
  themselves "mirrors", file growth, and copies that had not grown. One item
  was wrong. A badge spelled differently on two surfaces was presented as
  drift, but `2026-10-27-task-lifecycle-design.md` D28 decides that
  difference. Another item told the owner what they already knew.
- The questions hid the answer and asked the owner to predict it. That
  framing was rejected.
- The useful outcome came from a follow-up question the owner asked about
  splitting `main.js`. That led to the split (9e101803), and the split
  surfaced a comment naming a function that had never existed (1fb36d34).
  Neither finding was on the page.
- The owner knew the intent behind the code's structure (helpers hoisted out
  of the page IIFE so node can test them). They did not know how far that
  intent had got: about 3,000 lines out, about 8,300 still inside.

## Scope

- A new embedded agent skill, `codebase-tour`, that produces the page and
  handles the conversation that follows.
- The changes to `running-parallel-workers` that call it between landings.

## Decisions taken

**operator** = chosen by the human in conversation (2026-11-06). **this spec**
= chosen by the author while writing. Rows marked "this spec" are the ones
worth a second look.

| # | Decision | Decided by |
| --- | --- | --- |
| D1 | The tour is its own skill. It can be run on its own, and running-parallel-workers runs it at intervals | operator |
| D2 | Its form is one page, then the owner pushes back on it. The page's job is to give the owner something to question, not to be a complete report. The conversation that follows is where the value is | operator |
| D3 | Each item is anchored on **a decision the owner made, and how far the code has carried it out**. The owner's decisions are spec rows marked as the owner's, the conventions, and answered owner questions, found as D16 settles. A decision an agent made may anchor an item only as D15 allows. The owner's decision is the yardstick, which keeps the author's own idea of "debt" out of item selection | operator |
| D4 | Two further sources feed items. (a) Symptoms the owner has named, such as "many bugs where A and B disagreed"; the fix history can be grouped by kind to show them. (b) The reaction log of earlier pages (D12). Mechanical signals (counts of self-declared copies, file growth, identifiers named in comments but defined nowhere) are only search aids for these sources and are never a reason to list an item. They carry the author's assumptions about what debt looks like, and in the trial they produced one wrong item and one already-known item | operator (2026-11-06; selection by signals was rejected as carrying the model's prior) |
| D5 | Questions ask what only the owner can answer: what they intended, or what they want done. A question whose answer the author already holds, posed so the owner has to guess, is never used. Facts are shown side by side so a difference is visible without a quiz | operator |
| D6 | Every claim on the page is checked against the decision records and against the artifact itself before the page is shown. A difference counts as a finding only after confirming that no **owner** decision covers it (D15). A claim that cannot be checked is labelled as unchecked or left off | this spec (the trial's wrong item) |
| D7 | A page holds about three items. When no more than one or two hold up, it holds those, never padded. When the owner names a number, that number | this spec; operator (2026-11-07: about three by default, one or two when no more are found, the owner's number when given) |
| D8 | Code locations are written so the owner can open them as they are, in their editor. In the trial that meant an absolute path with a line number in the owner's checkout. The form is settled once per repository: in running-parallel-workers' conventions, or by asking the owner on a standalone run. Because those paths are local, **the page is never committed** to the repository | operator (absolute `path:line` for the trial owner); this spec (settle per repository) |
| D9 | The page uses the owner's language in a polite register | operator |
| D10 | The skill is embedded (`runner/agentskills/codebase-tour/`) and mirrored under `.claude/skills/` and `.agents/skills/`, like running-parallel-workers. That skill names it, so every agent that gets running-parallel-workers must get this one too. Nothing specific to one repository goes in the skill text | this spec |
| D11 | Inside running-parallel-workers the tour runs after several landings since the last one. The count is a line in the conventions document. It runs only while the owner is present, never under "while the owner is away", because the page exists to be questioned | operator (after several landings); this spec (count in the conventions, owner present) |
| D12 | The page records the commit it describes, which becomes the next page's baseline. After the conversation, each item gets a reaction line: pushed back on, accepted as is, or passed over, plus what came of it. The next page reads those lines to choose its sources | this spec |
| D13 | When the owner pushes back, the author answers by digging into the artifact (history, decision records, code, a run) and not from memory or summary. Outcomes are routed as follows. Inside running-parallel-workers, the parent does no child's work: a fix becomes a backlog item or an assignment, or, when the owner agrees, joins the current item of a child already working in that area as a new instruction; a choice becomes a registered owner question (`Q<n>`). On a standalone run, the session may make a fix itself when the owner asks it to | this spec; operator (2026-11-07: a fix may join a running child's current item) |
| D14 | One script, the draw (D19); everything else is procedure. Further scripts come once a source has proved useful across several pages, following the repository's prototype-first practice. The draw is a script from the start because done by hand it would be the author's choice again, which is what it exists to remove | this spec; operator (2026-11-07: the draw is a script) |
| D15 | **Decisions made by agents.** These are spec rows the author decided, plan lines, commit messages and code comments. They may anchor an item, but the page says the decision was an agent's and not the owner's, and asks whether the owner stands by it. If the owner confirms it, it counts as the owner's decision from then on, and the confirmation is recorded in the reaction line. Only the owner's own decisions can explain a difference away under D6. An agent's written reason never cancels a finding, because the tour exists to show drift from the owner's intent, and an agent's justification is where that drift hides | operator |
| D16 | Where the owner's decisions are recorded, and how their maker is marked, is settled once per repository, alongside D8's location form. Examples are a `Decided by` column, `DECIDED (operator, <date>)` text, or an answered-questions directory. Not every record in a repository marks its maker: in this one, 36 of 133 specs have a `Decided by` column. A record without a mark counts as an agent's (D15) | this spec (builder check) |
| D17 | **Tour state.** One directory per repository, outside the repository, holds the pages, their reaction lines, and a `settings.md` with the D8 and D16 answers and the draw's exclusions (D19). Page files are numbered `<nnn>-<date>.md` from `001`; the highest number is the last page, and a redone page takes the next number instead of overwriting one. If the repository has running-parallel-workers records, the directory is `<records>/tours/`. A standalone run uses the same directory, so there is one reaction history. **Finding it assumes nothing about the repository:** one has running-parallel-workers' conventions, one records the directory some other way, one records it nowhere. A run that keeps a memory across sessions also looks there, and records the directory there once it is known. If nothing names it, the run asks the owner; if it is named but out of the run's reach (a sandbox that does not mount it), the run says so and waits. It never picks a directory itself | this spec (builder check); operator (2026-11-07: no assumption about the repository's conventions; record the location in memory; number the page files) |
| D18 | **How the chosen items are picked.** By drift, not by age. A decision carried out since the last page is an item only where the code has moved away from it; one its own commit just carried out and explained, with nothing moved, would only confirm what the owner just read, and takes no slot. Beyond that the run prefers what no page has shown, then what was shown longest ago. Over many pages the tour is meant to have walked the owner over the whole codebase | operator (2026-11-07: recent decisions are fine when they drift, a bare confirmation is not; the tour should come to cover the whole codebase, old and long-unseen decisions included) |
| D19 | **One item is drawn, not chosen.** A script reads every earlier page and walk and counts as shown the lines they quoted, matched by content so moved lines still count. It picks a directory, weighted by the fraction of its lines not yet shown, then an unshown line in it, both from sha256 of the described commit, so the draw is reproducible. That item goes on the page whatever the author finds there, with the command and its output; the author never redraws. The exclusions (generated code, mirrors, test data) are a `draw-exclude` block in `settings.md` that only the owner sets. Coverage is approximate: a quoted line counts as shown even if the page did not discuss it | operator (2026-11-07: LLM judgement of "no drift" would hide places, so selection takes a random element; directories as the unit; a page shows one drawn item; a file seen once must not remove its directory, so coverage is counted in quoted lines) |

## Non-goals

- Replacing correctness review or tests. The tour checks the code against
  decisions, not against bugs.
- A debt dashboard or a set of metrics tracked over time.
- Unattended runs. A page with nobody to question it has no purpose (D11).
- Choosing items by the model's own sense of code quality (D4).

## 1. The skill: `codebase-tour`

### When to use

The owner wants to see where the codebase stands against their decisions, or
running-parallel-workers' trigger fires (§2).

### Procedure

1. **State and baseline.** Find the tour directory (D17) and read its
   `settings.md`. On a first run, settle D8 and D16 with the owner and write
   them there. The baseline is the commit the last page recorded. With no
   earlier page, use a point the owner names, or the commit 30 days back.
   Run the draw (D19) on the commit the page will describe.
2. **Gather decisions** for the chosen items: those made or touched since the
   baseline, those still open from earlier pages, and older ones, with where
   each is recorded (file and row). Mark each one as the owner's or an agent's
   by the marking in `settings.md` (D16). Choose by drift, not by age (D18).
3. **Gather owner-named symptoms.** Use symptoms the owner has named in
   conversation, in questions, or in earlier reactions. To show a symptom's
   shape, group the fix commits since the baseline by the kind of fault they
   repair.
4. **Read earlier reactions.** Raise the weight of sources whose items were
   pushed back on, and lower it for sources whose items were passed over.
5. **Measure how far each decision got.** For each candidate, check the
   artifact: how much of what was decided is in place, and where the rest is.
   Mechanical searches are allowed here, as a way to find the evidence.
6. **Check every claim (D6).** Re-read the decision record and the code for
   each statement. Drop anything an owner decision explains. Label anything
   that could not be checked. A difference that only an agent's decision
   explains stays on the page as that agent's decision (D15).
7. **Write the page.** Use about three items, or as D7 allows, in the format below, and
   record the commit described.
8. **Present it and stop.** Hand the page over and end the turn. The owner
   may take a while.
9. **Answer the pushback (D13).** Dig into the artifact for each question.
   Route each outcome.
10. **Record reactions (D12).** Write one line per item on the page.

### Page format

```
# Codebase tour <nnn> — <date>  (describes <commit>; baseline <commit>)

## <n>. <decision, in a few words>: <how far it got, in a few words>

Decision: <what was decided>, recorded at <file and row> — <the owner's |
an agent's, not yours (do you stand by it?)>.
Now: <how far it got, with the figure>; <where the rest is>.
<Locations, in the form D8 settled, side by side where two things are compared>

What I'd like to ask: <intent or choice; never a quiz>

---
Reactions (filled in after the conversation):
- <n>: pushed back | accepted | passed over — <what came of it>
```

### Where the page goes

The page goes in `<tour directory>/<nnn>-<date>.md` (D17), never in the
repository (D8).

## 2. Changes to running-parallel-workers

- **Checkpoints:** add an entry. After the number of landings set in the
  conventions since the last tour, and with the owner present, run
  `codebase-tour`. Settle the next items of the children first, as at any
  landing, so no child waits on the tour.
- **Conventions skeleton** (`references/repo-conventions.md`): add a section
  "Codebase tour" with three lines. They give how many landings pass between
  tours, how code locations are written for the owner (D8), and where the
  owner's decisions are recorded and how their maker is marked (D16). The tour
  copies the last two into its `settings.md`.
- **Records layout:** add `tours/` to the directories `parallel_common.py
  init` creates (`SUBDIRS` in `scripts/parallel_common.py`).
- **A count reached while the owner is away** waits for their return.
- **Parent-state file:** record the last tour's date and commit, and the
  landings counted since then.
- **While the owner is away:** add a line saying no tour runs.

## 3. Verification

- `go test ./runner/agentskills/`: the new skill is embedded, and its
  mirrors under `.claude/skills/` and `.agents/skills/` are byte-identical.
- `harness-cli skill codebase-tour` prints it.
- One run on this repository with the owner present. The page must follow
  the format and anchor every item on a recorded owner decision. Every claim
  must survive the owner's pushback without being shown wrong by a decision
  record. The reactions must be recorded on the page.
