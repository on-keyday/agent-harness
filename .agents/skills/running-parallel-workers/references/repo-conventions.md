# Skeleton: the repository's conventions for parallel work

Copy this into the repository (the owner picks the path, e.g.
`parallel-work.md` in its docs directory), fill it from the repository's
existing docs, and ask the owner the points still open directly — no worker runs
yet. Commit it after the owner approves. Write in the repository's terms; no
harness commands, session handling, or account details. Leave out a section
that does not apply, and say so in one line.

Every `<...>` is a decision. Where a line offers choices (`<a | b>`), none is the
default. Lines marked "e.g." show the level of detail, not an answer.

---

# Parallel work in this repository

Decided: <date>, by <owner>. Changes to this document are owner decisions.

## Unit of work

- Where the backlog lives: <a backlog file | the issue tracker | both>.
- One worker takes one item (one issue) to a branch ready to land.
- Who picks a worker's next item: <the owner, each time | the coordinator, in
  backlog order | ...>.
- How an item is referenced in commits and PRs: <e.g. "Closes #123", the backlog
  item's heading>.
- Upper bound on parallel work, if the repository itself has one: <none | e.g.
  how many open PRs reviewers can take>. Machine-dependent limits do not go here.

## Workflow of one item

- Stages: <e.g. design, plan, implementation, final review>.
- Where the design is written down: <a spec file | the issue | the PR description>.
  It lists the owner decisions that apply, or "none".
- Gate before implementation: <owner approval when owner decisions apply;
  otherwise the review named here | ...>.
- Review before reporting ready: <what kind, by whom>.
- Records the work leaves: <decisions, deferred review findings, experiments,
  ... — and where each goes>.
- Estimates: <none | the worker estimates each task of its plan and the total
  before implementing, and records the actual (from `date` stamps) when it
  reports ready, in <where>>.
- Deferred backlog items with a trigger ("do this when X happens"): <none | where
  they live, and who reviews them at each landing>.

## Landing

- Mode: <local fast-forward of the trunk and push | pull request>.
- Pull request mode only:
  - Who opens the PR: <the coordinator | the worker>.
  - How a branch already pushed catches up with the trunk:
    <rebase and --force-with-lease on the work branch | merge the trunk in, no force>.
  - Who merges, and when: <required reviews and checks as configured; who presses merge>.
  - Merge method: <merge commit | squash | rebase>.
- Staging: explicit paths or `git add -u`; never a pathless `git add -A`.

## Branch sync

The command or steps that bring a work branch up to the trunk, including how
conflicts in generated files are resolved and which tests run afterwards.

  e.g. rebase onto the trunk; in generated files take the trunk's side and
  regenerate; stop on any other conflict for the worker to resolve; run the
  tests (a subset when only docs changed); commit regenerated files separately.

## Checks

- Checks a branch must pass before landing: <commands or CI jobs, and roughly
  how long each takes>.
- Checks a worker runs while working: <the same | a cheaper set — and how it is
  chosen, e.g. from the changed files, or the fast stages only>.
- Who runs the landing checks: <the worker, on its final tip | the coordinator,
  once per landing | CI>.
- Contention: <none — checks are light or run in CI | heavy checks run one at a
  time per machine, under <lock path convention>>.
- Sharing or moving runs: <none | several ready branches share one run of the
  landing checks | runs may go to another machine or to CI>.

## Owner decisions

What a worker must take to the owner, numbered so designs and questions can cite
them. Anything not listed, the worker decides and records as "Workflow of one
item" says.

| No. | Kind | In this repository |
|---|---|---|
| O1 | scope | <which items, split or close, order> |
| O2 | outward action | <pushes not covered above; issues, comments, PRs as experiments; settings> |
| O3 | rules | <rule documents, this one included; meaning of existing names> |
| O4… | <kinds this repository adds> | <e.g. external contracts — output formats, flags, exit codes; domain claims no source backs> |

## While the owner is away

<what continues and what waits — e.g. fewer workers; owner decisions batched for
their return; a failing trunk check isolated and recorded, not reverted, and
later landings held>.

## Reader on a different model

- Is there a reader: <yes | no>. With no reader, the work gets the reviews in
  "Workflow of one item" only.
- What it reads: <user-facing text (help, error messages, docs) | code |
  designs | ...> — one line per kind if there are several.
- What it may look at: <only the input it is handed | the repository as the
  review needs>.
- What it is asked: <the brief per kind — e.g. "where a newcomer could misread
  this, no judgement of correctness, no rewrites" for user-facing text; an
  adversarial review for code>.
- Which model: <a model other than the workers'>.
- Round limit: <n per kind>; after it, the coordinator checks what changed and
  records what was checked.

## Repository shape for parallel work (optional)

Measures one repository took to cut merge conflicts between parallel branches;
adopt what fits, drop the rest.

- Per-piece records go in one file per piece of work; any index of them is
  generated, so parallel branches do not edit one shared list.
- Merge drivers: <none that the PR host ignores — e.g. a GitHub PR merge ignores
  `merge=union`, which local `git merge` honours, and the PR shows CONFLICTING>.
- Registration tables: <one entry per line, in key order, order checked by a
  test — git merges inserts at distant lines on its own>.

## Coordinator's records

Where the coordinator keeps its private records of the parallel work (state,
instructions sent, message log, questions to the owner), outside the repository:
<a path relative to `~` — never a user name or an absolute path>.

  e.g. `~/.local/state/running-parallel-workers/<repo>` on Linux and macOS,
  `%LOCALAPPDATA%\running-parallel-workers\<repo>` on Windows.

## Experiments

Where real-world experiments may run: <repository, branch>; anything else is an
outward action (O2).

## Glossary

Names workers and coordinator use for things in this repository, with what
each means. Add a name here before building a discussion on it.

| Name | Means |
|---|---|
| | |
