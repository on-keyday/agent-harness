---
name: landing-to-main
description: Use when landing / pushing / merging a harness task-branch's work to a repo's trunk, AND at the start of a RESUMED session before new work. Covers both local-trunk-authoritative (FF-mirror) repos and PR-based repos. Universal rule — NEVER cherry-pick to the remote (it manufactures dup-SHA divergence) and rebase the task branch onto the current trunk first. Determine each repo's landing policy once and record it in memory.
---

# Landing harness task-branch work

Harness tasks run on a `harness/<taskID>` branch created **once** from the repo's
HEAD at task-creation (`runner/worktree.go` `Create`, no start-point) and **never
re-synced** — resume just re-attaches the same branch. So the branch drifts from
the trunk the longer it lives. **Landing is where that drift becomes divergence
if you do it wrong.** This skill is about landing safely on any repo the harness
manages.

"Trunk" below = the repo's default branch (`main` or `master`). Detect once:

```bash
TRUNK=$(git symbolic-ref --short refs/remotes/origin/HEAD 2>/dev/null | sed 's#^origin/##')
TRUNK=${TRUNK:-main}
```

## Universal rules (every repo, every mode)

1. **Rebase the task branch onto the current trunk before landing** (Procedure A). A stale branch lands stale.
2. **NEVER cherry-pick task commits onto the remote trunk** (`git cherry-pick … && git push`, or `git push origin <branch>:<trunk>` of cherry-picked SHAs). Cherry-picking mints NEW SHAs on the remote for work that still exists under OLD SHAs on the task branch; nobody reconciles them, so later sessions see "same content, two SHAs" = **dup-SHA divergence** (`git rev-list` over-counts — "72 ahead" — the recurring "something's weird"). The fix is to never create them.
3. **`git push --force` is banned.**
4. **Verify with content, not commit messages.** Two same-message commits across branches are NOT necessarily the same code — use `git diff <a> <b>` / `git cherry`.

## Per-repo landing policy — determine once, remember

Repos differ in who owns the trunk. **The first time you land in a repo, determine
its policy and record it in memory** as a `project`-type memory, one per repo. Two
things matter for it to work across sessions (follow your memory system's
conventions for everything else):

- **Name it predictably** — e.g. `landing-policy-<repo>` — so a later land
  **updates that one memory** instead of spawning a duplicate.
- **Put the repo name and the chosen mode in the description** — e.g. "Landing
  policy for <repo>: Mode B, PR against main" — so it surfaces (recall is
  description-driven) the next time you land there.

On later sessions, **recall it from memory** instead of re-deriving.

**If you don't know the repo's policy and can't confirm it: do NOT push anything
— not to the trunk, not even the task branch. Ask the user** which mode applies
(local-trunk-authoritative FF, or PR-based), then record the answer in memory.
Never guess and never push speculatively: an unwanted push — to the trunk *or* a
branch/PR — on the wrong repo is exactly what this rule exists to prevent. Push
only once you know (or have been told) the mode.

### Mode A — local-trunk-authoritative (single-user dogfood; e.g. remote-agent-harness)

The trunk checked out in the repo root is authoritative; `origin/<trunk>` is a
**downstream FF mirror** you push to, never sync from.

```bash
# 0. Branch sits on top of current local trunk (Procedure A if not).
# 1. Build/test on the tip (e.g. make test / go build ./...).
# 2. FF-push the tip to the remote trunk (no force):
git merge-base --is-ancestor origin/$TRUNK HEAD && git push origin HEAD:$TRUNK
# 3. Advance local trunk to the same SHA:
MAIN_WT=$(git worktree list --porcelain | awk '/^worktree /{print $2; exit}')
git -C "$MAIN_WT" merge --ff-only origin/$TRUNK
```

Result: task branch, local trunk, and `origin/<trunk>` all carry the work under
the **same SHAs**. No divergence.

### Mode B — PR-based (origin / review / CI owns the trunk)

You must **NOT push to the trunk directly**. Land via a PR.

```bash
MAIN_WT=$(git worktree list --porcelain | awk '/^worktree /{print $2; exit}')
# 0. Rebase onto the CURRENT trunk — here that means origin's latest:
git -C "$MAIN_WT" pull --ff-only        # local trunk follows origin in this mode
git rebase $TRUNK                       # (or the --onto variant; see Procedure A)
# 1. Push the task branch and open a PR:
git push -u origin HEAD                 # pushes harness/<id> (rename the branch if you prefer)
gh pr create --base $TRUNK --fill
# 2. Let it merge through the repo's process (review/CI). Do NOT FF local trunk yourself.
```

Still rebase-first, still never cherry-pick. After the PR merges, local trunk
follows origin via `git pull --ff-only`.

## Procedure A — Resume-sync (run at the START of a resumed session)

Make the task branch a clean descendant of the current trunk before building on it.

```bash
git stash -u 2>/dev/null
git rebase $TRUNK                                   # normal clean fork
# If LEGACY-DIVERGED (dup-SHA base; `git cherry $TRUNK HEAD` shows many '-'):
#   git cherry $TRUNK HEAD     # '+' = new (keep), '-' = already-on-trunk replay (drop)
#   BASE=<commit just below your first '+'>; git rebase --onto $TRUNK <BASE> HEAD
git stash pop 2>/dev/null || true
```

(In Mode B, `git -C "$MAIN_WT" pull --ff-only` the local trunk first so you rebase
onto origin's latest.) Rewriting the branch's SHAs is safe — `claude --resume`
keys on the worktree path, not the commit SHA.

## Healing an already-diverged state (dup-SHA mess)

Symptoms: `git rev-list --count origin/$TRUNK..HEAD` huge, `git cherry` shows many
'-', or `origin/$TRUNK` is ahead of local trunk (cherry-pick-landing residue). Fix:
FF local trunk up to origin if it's a true FF (`git -C "$MAIN_WT" merge --ff-only
origin/$TRUNK`), then land via Procedure A's `--onto` variant (drops the dup
replays) + the repo's mode.

## Harness caveats (any repo)

- **Worktree path routing**: inside a harness worktree, bare `/…/<repo>/<rel>`
  paths and `git -C /…/<repo>` hit the **parent** checkout, not this worktree. Use
  the worktree's own path for edits; use `$MAIN_WT` (from `git worktree list`)
  when you mean the main checkout.
- The trunk is checked out in the repo root, so you can't `git checkout <trunk>`
  inside the task worktree; move it only via `git -C "$MAIN_WT" merge --ff-only …`.
- Verify FF-safe before any push: `git merge-base --is-ancestor origin/$TRUNK HEAD`. Never `--force`.

## One-line summary

Rebase the task branch onto the current trunk, never cherry-pick to the remote,
and land by the repo's policy — FF-push for local-authoritative repos, PR for
review/CI-owned repos. Determine each repo's policy once and remember it.
