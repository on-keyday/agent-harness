---
name: running-parallel-workers
description: Use when one session (the parent) runs several long-lived child sessions in parallel, each carrying one backlog item or issue from design to a landing-ready branch, while a human owner decides scope — settling the repository's conventions for this once, assigning work, relaying questions to the owner, resetting a child between items, recording board traffic before it expires, landing by local fast-forward or by pull request, and keeping the parent's own state across /clear and compaction. The harness commands are in supervising-workers and harness-cli; this is the operating discipline built on them, each rule with the incident that produced it.
---

# running-parallel-workers (a parent, its children, and an owner)

One parent session keeps several child sessions busy; each child takes one
backlog item (or one issue) to a branch ready to land; a human — the owner —
decides what gets built. The commands are in `supervising-workers` (spawn,
resume, caps, snapshot, git) and `harness-cli` (send, reply, inbox, progress).
This skill is the order of operations and the discipline.

Every rule carries the incident that produced it. When you adapt a rule, keep
its reason: a step whose reason is gone looks like ritual, gets skipped, and the
incident comes back.

What differs per repository is settled once in a conventions document (below);
this skill names those points as "the conventions". The details live in
`references/`, each named where it applies. The scripts in `scripts/` are
Python 3, standard library only, for Linux, macOS and Windows — `python` below
means `python3`, or `py -3` on Windows. Script and reference paths are relative to
this skill's directory, which in a harness worktree is
`.claude/skills/running-parallel-workers/` (also under `.agents/skills/`); from
the worktree root, call them by that path. With no such directory (a
`--no-worktree` task, a runtime that does not read `.claude/skills`), get them
from the binary: `harness-cli skill running-parallel-workers <file>` prints one,
`--extract <dir>` writes the whole skill as `<dir>/running-parallel-workers/`.
The session procedures assume Claude
Code children (`/clear`, `/effort`, AskUserQuestion); another runtime has its
own equivalents.

## Roles

| Role | Does | Does not |
|---|---|---|
| owner (human) | decides scope, outward actions, the conventions, and the other kinds of decision the conventions reserve | talk to children directly as the normal path — everything goes through the parent |
| parent (one session) | starts and assigns children, relays questions, verifies results, lands, keeps the records | do a child's work; decide in the owner's place |
| child (one per item) | takes one item through the stages the conventions name, then reports it ready to land | land (unless the conventions hand it PR creation), create sessions, ask the owner directly, write auto-memory |
| reader (optional, different model) | reads what the conventions give it, under their brief, and lists findings | change the work, or decide what gets fixed |

- **One child carries one item.** Do not split one plan's tasks across children
  — the shape of one person owning one pull request.
- **Landing is the parent's**, except what the conventions hand to the child.
  Why: when children move the trunk, the parent loses track of the whole. Say so
  in the child's first instruction.
- **Give children no `spawn` capability.** A grandchild is something the parent
  cannot track. Research subagents inside a child are fine.
- **When the owner does talk to a child directly, the child reports it to the
  parent at once** — what was said, verbatim, and when — and the parent records
  it as it would an owner answer. Why: the message log, the question register
  and the parent-state file see only what passes through the parent; an
  instruction typed into a child's session reaches none of them, and the parent
  would check the child's work against a scope the owner had already changed.
  Say so in the child's first instruction.

## How many children

No number is fixed. Choose it per run from the work and the resources, tell the
owner the number and why, and change it when the signals say so. The owner may
set it — a temporary instruction, kept in your parent-state file.

- **The work**: items ready and independent of each other (no shared files, no
  order between them); how many owner questions you can relay before the owner
  falls behind; your own turns — every child costs verification, relaying and a
  landing.
- **The resources**: runner slots (`ls` shows `tasks=N/M`; an interactive child
  holds its slot for life), memory and CPU for heavy checks, the heavy-check
  lock if there is one, usage limits.
- **Run fewer** on a long lock queue, low memory, children idle on owner answers,
  or ready reports waiting on you. **Run more** when every child is busy and
  resources and independent items are free.
- The run this skill comes from used two or three, one at night. Machine-bound
  numbers stay out of the repository; a repository's own bound (e.g. open PRs its
  reviewers can take) goes in the conventions.

## Before you start

- **Read `harness-cli whoami`.** Your caps decide which procedures are open:
  `spawn` / `resume`, `cancel`, `board_member` + `board_persist`.
- **Note the versions of the plugins and skills the children's workflow
  depends on**, and tell the owner when one changes. Why: a plugin auto-updated
  and its planning skill switched from "write the code in the plan" to
  "signatures only"; every child's plan changed shape at once, the estimates
  built on the old shape broke, and nobody noticed for six days.
- **Settle the conventions** (next section) — where your records go is one of
  them.
- **Then set up the records directory** at that place:
  `python scripts/parallel_common.py init <path>`. It creates the layout, marks
  the directory with its repository (`.repo`) and `git init`s it; it refuses a
  path in use for something else or holding another repository's records — ask
  the owner for another place then. Commit in it at each landing, owner answer
  and reader round.

  ```
  <records>/
    parent-state.md   your state; one file, history in git
    instructions/     every instruction sent, <date>-<child>-<n>.md
    log/              the message log, <start date>-<run>.md
    questions/        owner questions, q<n>.md; answered/ beside them
    readback/         reader inputs (prepared/<name>/) and results
    tours/            codebase-tour pages, their settings.md and reactions
  ```

## The conventions document

What differs per repository — the stages of a piece of work and its records,
estimates, landing mode, PR handling, branch sync, checks, the kinds of owner
decision, what happens while the owner is away, the reader, where your records
go — is decided **once, at the start**, in **one document committed to the
repository**. Skeleton: `references/repo-conventions.md`.

- **Look for it first**, read it in full, and cite it in instructions instead of
  restating it. After a /clear it is how you find your records again.
- **If there is none, draft it** from what the repository's docs already decide,
  and ask the owner the rest **directly** — no child runs yet, so nothing
  collides with a question tool, and the document's commit records the answers.
  For the records location offer `parallel_common.py suggest`; the owner confirms
  the path is free when choosing it. Commit after the owner approves, at the path
  the owner chooses.
- **A change mid-run** goes through a registered question, then the document
  (with the owner's approval), then the children's next instructions. Never let a
  convention live only in a message.
- **Repository terms only**: "the parent opens the PR", not harness commands or
  session handling (those go in your parent-state file). The records location is
  written relative to `~` — a user name or absolute home path would publish the
  owner's machine with the repository.
- Why one document: conventions decided ad hoc lived in a user's private memory,
  a CLAUDE.md, the parent's state file and local scripts at once, and could not
  be carried to the next repository or parent. Names nobody had fixed piled up
  until the owner could not tell what was being discussed — hence its glossary.

## The parent-state file

`parent-state.md`: hard constraints first, then the children (name, task id,
item, state), the owner's temporary instructions, open questions and waits, and
timestamped events.

- **Read it in full after every /clear, compaction and wake, before acting**;
  rewrite it the moment state changes. Why: compaction and a fresh context drop
  constraints and roles. After one compaction the parent used AskUserQuestion
  twice against the rule. After a /clear it answered "what was the last piece of
  work" from git alone, then decided it might be a child and asked the owner
  whether to land. The file survives both.
- The owner's temporary instructions ("do not push for a while") go here only —
  not in the repository, not to children.
- The last codebase tour's date and commit, and the landings counted since
  then (see Checkpoints).

## A child's lifetime

- **Create a child once, drive it by its task id ever after.** Bring it back with
  `session new -d --resume <id> --exact` (caps and permission mode as recorded,
  conversation new) — also the path without `spawn`. Pass `--agent <name>` on a
  runner with several profiles (else `ambiguous_runner`, exit 3). Snapshot once
  after start: effort and permission mode as meant?
- **The assignment** is a file, `instructions/<date>-<child>-<n>.md`, whose
  content you send — the record and the message cannot differ. Skeleton and its
  "do not" list: `references/assignment-template.md`. Send it with
  `--no-retire-on-reply`. Why: the child's hello_ack is a reply, a reply retires
  what it answers, and a child resumed later could not read its instruction.
- **Confirm the hello_ack within minutes**, well inside the topic's 30-minute
  TTL; if missing, send again. Why: a hello sent right after a /clear did not
  start the child's turn; it sat idle 55 minutes while the records read "sent",
  the TTL swept the message, and the child, waking, read through the parent's
  private records looking for it.
- **Read `delivered_to` on every send**; 0 means the child is not there. Why: a
  dead child went unnoticed for 17 minutes because "sent" was recorded without
  reading the output.
- **A child told to wait for a condition does not start by itself.** You watch
  the condition and send the go.
- **Reset between items** (after landing, before the next assignment):
  1. Retract the messages that are done with.
  2. Check the input line is empty — `act=idle` in `ls` and `snapshot --style`.
     Without `--style` a faint suggested prompt reads like typed text; the parent
     once reported to the owner text nobody had typed.
  3. Send `/clear` without Enter; check the line holds only `/clear`, not faint;
     then Enter on its own.
  4. Snapshot: the screen must show a new conversation. Why: a connection warning
     once swallowed the /clear, and the child took the next instruction in the
     old conversation.
  5. Check the effort is the one you set; set it again if it changed. Why: it
     reset after some /clears and not others.
  6. Send the instruction; confirm the hello_ack.

  The keyboard is for client operations like these; requests go over the
  agentboard.
- **A child that died** (`failed` after a runner disconnect or server restart):
  `--resume <id> --exact --resume-conversation` opens its latest conversation; if
  you already opened a new one, pick the pre-crash conversation from `/resume` by
  time and size. Without `cancel`, stop a child with `/exit`. A question it
  believed it sent just before dying may never have arrived — compare its last
  records with yours.

## Recording board traffic

**Record every arriving message verbatim, in the first tool call of the turn**;
read, think and answer after. Why: messages vanish after the topic's 30-minute
TTL or 64 newer messages, and `agent read` then answers `not readable`. A child's
spec report was gone 1 hour 16 minutes later; it had to be copied back out of the
session transcript.

`log_messages.py` from a `UserPromptSubmit` hook logs each message on
delivery; by hand it takes seqs. Setup, quirks (half-sent messages, expired
reply targets, seq prefixes across restarts), and the option of a retained topic
per child: `references/message-log.md`.

## Questions for the owner

Once children run. (Before that, while settling the conventions, ask directly.)

- **No AskUserQuestion while children run** — write the question and end the
  turn. Why: a child's message makes the harness type a wake prompt into the PTY,
  which collides with the question tool's screen. After a compaction the parent
  forgot and used it twice.
- **Register every question first**: one file, a number never reused (`Q<n>`),
  with `q.py`. Why: relayed questions were called "option A", "(a)",
  "Q1" — labels that meant nothing outside one message — until the owner could
  not follow and asked for one unique number for everything; registration was
  later forgotten for five days and an ad-hoc number collided with a registered
  one.
- **Make waiting short**: say on each question who it blocks, so the owner
  answers those first; mark one option as your recommendation, so the owner can
  accept the recommendations in one go (`q.py answer-recommended`). Tool and file
  format: `references/questions.md`.
- **`Q<n>-<m>` names the options of one question only**; independent decisions
  get their own numbers.
- **Restate, do not reference**: write an earlier decision out — when, which
  work, which question, what — never "the previous decision". Name things by
  content, not by a child's labels; send answers to children as content, not your
  numbers.
- **Re-compare your recommendation when evidence is added**; if you keep it, say
  per new fact which way it leans and what would change your mind.
- **Stop asking a question whose answer never changes** — it becomes a rule, and
  a line in the conventions if it is about the repository.

## Owner decisions

The conventions number the kinds of decision a child takes to the owner;
anything unlisted the child decides and records. Three kinds hold everywhere:

| Kind | Why the owner |
|---|---|
| scope — which items, split or close, in what order | parallel children must not eat each other's scope |
| outward action — pushes the conventions do not cover; issues, comments or PRs as experiments; settings | irreversible, or visible to others |
| the conventions and other rule documents, the meaning of existing names | other agents' behaviour changes |

Others depend on what the repository makes — e.g. external contracts (users'
behaviour changes) or domain claims no source backs (an unbacked claim is an
interpretation). Opening the work's own PR in a PR-mode repository is landing,
not an outward action.

- Wherever the child writes its design down (as the conventions say), it lists
  the owner decisions that apply, or "none". With some, implementation waits for
  approval; with none, for the review the conventions require.
- A child waiting on an answer goes ahead with whatever does not depend on it.
- **While the owner is away** the conventions say what continues. The source run
  ran fewer children, batched owner decisions for the owner's return, and on a
  failing trunk check isolated and recorded instead of reverting, holding later
  landings — the owner preferred work stopping to unattended autonomy.
  No codebase tour runs while the owner is away; a tour count reached then
  waits for their return, because the page exists to be questioned.

## Landing

1. The child reports ready: branch tip, check results, findings it set aside,
   decisions for the owner.
2. **Settle its next item now**, not after landing — ask the owner, or take the
   next in backlog order if the conventions allow. Why: asked after landing, the
   child sat idle waiting.
3. Build the branch and check the report against the diff — what the child says
   it wrote is in the output.
4. If the trunk moved, have the child run the conventions' branch sync.
5. Make sure the checks the conventions require before landing passed on this
   tip — run them yourself, or confirm the child ran them, as the conventions
   assign (`references/resources.md`).
6. Land by the conventions' mode (`landing-to-main`): local fast-forward from the
   main checkout without force (a harness worktree cannot `push . HEAD:<trunk>`
   while the trunk is checked out there), or the PR opened and merged as the
   conventions say.
7. Have the child land its work records — the actual time against its estimate,
   if the conventions keep estimates.
8. Retract the instruction messages that are done with; review deferred backlog
   items, and count the landing toward the next codebase tour ("Checkpoints").

- **Stage with `git add -u` and explicit paths only.** Why: the runner leaves
  `.claude/`, `.agents/` and `GEMINI.md` untracked in every worktree; a pathless
  `git add -A` committed 5,800 lines, caught only in final review. A committed
  `settings.json` installs hooks and permissions for everyone who clones. Look at
  `git diff --cached --stat` first.
- Before a child's working notes outside git are deleted, copy every decision in
  them to the conventions' place for decisions. Why: deleting the notes as a
  procedure said left most of the decisions nowhere.

## Estimates (when the conventions keep them)

The child estimates each task of its plan and the total before implementing, and
records the actual from its `date` notes when it reports ready — never from
memory. They land with the work. Use them to choose assignments and the number
of children, to price "fix it or record it", and to notice the ground shifting:
when a plugin changed how plans were written, estimates went wrong all at once,
and only the comparison showed it. An estimate is the child's, not a commitment;
a large miss asks what was unknown at planning time.

## Checkpoints

- **Deferred backlog items with a trigger** ("do when X happens"), if the backlog
  has them: at every landing, hold each against the trunk and propose raise /
  close / keep to the owner. Why: of 61 such items, zero closed because their
  trigger arrived; no child standing where a trigger fires checks the list. A
  "when a user asks" trigger counts users' issues.
- **A codebase tour after the conventions' number of landings since the last
  one**, with the owner present. First settle the children's next items, as at
  any landing, so no child waits on the tour. Then run `codebase-tour` on
  `<records>/tours/`. If the conventions have no "Codebase tour" section (one
  written before it existed), add it the way any mid-run change goes: a
  registered question, then the document. Don't invent a count, and don't ask
  the tour's settings directly while children run. When the owner's pushback asks for a fix, the fix becomes
  a backlog item or a child's assignment, or, when the owner agrees, joins the
  current item of a child already working in that area, because the parent
  does no child's work; a choice becomes a registered `Q<n>`. Why: the owner reads little of
  the code the children write, and passing tests hide how far the code has
  moved from what the owner decided. Only the owner looking at the real thing
  catches that, and by then the drift has spread.
- **No news to a child you will /clear** — put it in its next assignment; a child
  mid-work does get it.
- **Batch notices that need no reply** into the next exchange.

## Resources

When the machine limits the work, cut how often the expensive checks run before
anything else: the landing checks once, on the branch about to land, with
children running cheaper ones while they work; several ready branches sharing
one run; another runner or CI taking the run. Where checks contend, run them one
at a time per machine under `heavy_lock.py`. Run
`watch.py` in the background to be woken on idle children, a long lock
queue, low memory or a failed child. Details, the multi-machine case and usage
limits: `references/resources.md`.

## A reader on a different model (when the conventions have one)

The same model shares the same biases and misses the same gaps, so a session on
a different model reads part of the work — user-facing text, code or a design, as
the conventions say, under their brief. Briefs, delivery and the round limit:
`references/reader.md`. With no reader, say so in the instruction so nobody
waits for one. Whatever it reads:

- Take the input from the **actual artifact** — the built output, a commit — not
  from the child's description; sort the reply (fix / candidate / out of scope /
  no) outside the repository; pass what you cannot decide to the owner.
- Nothing from one round may reach the next: move the previous input out and
  `/clear` the reader each time, and check the input arrived before sending. Why:
  with an old directory left and /clear skipped, the reader saw the previous
  target; once the copy step was skipped, the reader answered "nothing found"
  about an empty input.

## How the parent writes

- **Times come from `date`.** Hand-written times drifted 2–6 minutes from the
  send time. Put `$(date …)` in the command; a quoted heredoc (`<<'EOF'`) does
  not expand it.
- **Keep the owner's decision, the letter of a convention and your own judgment
  apart**, costly judgments of your own first. Why: applying a rule to the
  letter, the parent had a child run a 52-minute check that verified nothing
  new, holding the lock and stalling the others — and the report never said it was
  the parent's call.
- **A fact that holds under a premise carries the premise in the same
  sentence.** Instructions get no review; a child copied a premise-less sentence
  straight into the backlog.
- **When a child would "declare" or "record" a problem, ask first whether it can
  be fixed and at what cost.** Why: one such item would have taken 4 tasks and 40
  minutes to fix and took unrecorded bugs of the same shape with it; another time
  the record was enough. Ask; do not default to the cheap record.
- **An approved reading is not an observation**: a child that skips checking on
  the real thing because a reading was approved checks anyway.
- **A new formatting convention in a child's diff goes to the owner before it
  spreads.** One ad-hoc format, unchecked by any test, reached 214 files.
- **Proceed with the decided next step without asking**; ask only where the
  conventions require the owner's choice.

## Never in the repository

The conventions document is the one place this work writes about itself there,
in repository terms only.

- **Harness operations and observations**, including how children are driven. A
  harness bug goes to the owner.
- **Account details** (organisations, plans) — not in children's instructions
  either; give the conclusion ("the experiment repository is on the free plan").
  Why: five organisations listed in an instruction were copied by a child into the
  repository's records and nearly pushed.
- **Auto-memory written by children** (where the runtime has it): in the source
  run parent and children shared one memory directory and children duplicated the
  parent's memories. A child reports what it learned; the parent decides.
- **The owner's private environment** — other machines, and the tools and
  arrangements around the repository.
