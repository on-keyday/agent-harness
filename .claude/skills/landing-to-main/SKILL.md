---
name: landing-to-main
description: Use when pushing / landing / merging harness task-branch work to main or origin, AND at the start of a RESUMED session before doing new work. This is a single-user repo where local main is the authoritative trunk and origin is just a fast-forward mirror. Land THROUGH local main + FF push — NEVER cherry-pick to origin (it manufactures dup-SHA divergence that makes every later session look "weird"). At resume, rebase the task branch onto local main first.
---

# Landing harness work to main (single-user, local-authoritative)

## The model (read this first)

- **One developer.** `main` checked out in the repo root is the authoritative trunk. `origin/main` is downstream — a **fast-forward mirror** you push to, never a source you sync *from*.
- Harness task branches (`harness/<taskID>`) are created **once** from the repo's HEAD at task-creation time (`runner/worktree.go` `Create`: `git worktree add -b harness/<id> <dir>`, no start-point) and are **never re-synced**. Resume just re-attaches the *same* branch (so `claude --resume <uuid>` finds its session by cwd-hash) — it does **not** rebase.
- So a task branch drifts from `main` the longer it lives / the more it's resumed.

## The invariant to protect

```
origin/main  ==  fast-forward of local main   (always)
local main   ==  every landed task's work, same SHAs
```

If you keep this invariant, divergence never happens.

## THE ANTI-PATTERN THAT CAUSED ALL THE PAIN — do not do it

**Never** `git cherry-pick <task commits>` onto `origin/main` and push that (and never `git push origin <branch>:main` of cherry-picked SHAs). Cherry-picking **mints new SHAs on origin** for work that still exists under **old SHAs** on local main / the task branch. Nobody reconciles them, so the next session sees "same content, two different SHAs" = **dup-SHA divergence** = `git rev-list` over-counts ("72 ahead") = the recurring "something's weird." Each cherry-pick landing manufactures one more dup-SHA pair. The fix is to stop creating them, not to keep cleaning them up.

`git push --force` is likewise banned here.

## Procedure A — Resume-sync (run at the START of a resumed session, before new work)

Goal: make the task branch a clean descendant of current local `main` before you build on it.

```bash
# from inside the task worktree (cwd = the harness/<id> worktree)
git stash -u 2>/dev/null            # park any uncommitted work (pop it after)
# If the branch already forks cleanly from main (normal, post-fix tasks):
git rebase main
# If the branch is LEGACY-DIVERGED (dup-SHA base, e.g. rev-list shows a huge "ahead"):
#   find your genuinely-new commits — git cherry marks them '+'; replays older ones are '-':
git cherry main HEAD                # '+' = new (keep), '-' = already-on-main replay (drop)
#   BASE = the commit just below your first '+' commit, then:
git rebase --onto main <BASE> HEAD  # drops the dup replays, lands only your new work on main
git stash pop 2>/dev/null || true
```
Resolve conflicts with judgment (you understand the change; main's version usually wins for files you didn't touch). Rewriting the branch's SHAs is safe — `claude --resume` keys on the worktree path, not the commit SHA.

## Procedure B — Land to main (when asked to push / merge / land the work)

```bash
# 0. Pre-req: branch sits on top of current local main (run Procedure A if not).
# 1. Verify it builds + tests pass on this tip:
make test            # (and make check / make wasm-check as appropriate)
# 2. Fast-forward push the branch tip to origin main (origin is the FF mirror):
git push origin HEAD:main           # FF — no force. Rejected push ⇒ invariant was already broken; see Healing.
# 3. Advance local main to the same tip so local == origin == the work:
MAIN_WT=$(git worktree list --porcelain | awk '/^worktree /{print $2; exit}')   # repo root (where main is checked out)
git -C "$MAIN_WT" merge --ff-only origin/main
```
After this, local `main`, `origin/main`, and the (now-redundant) task branch all carry the work under the **same SHAs**. No divergence introduced. The next task branches off this clean `main`.

If step 3's FF is blocked by uncommitted changes in the main checkout, report it — don't force; let the user clean/commit those first.

## Healing an already-diverged state (one-time, e.g. legacy dup-SHA mess)

Symptoms: `git rev-list --count origin/main..HEAD` is huge, or `git cherry main HEAD` shows many `-` lines, or `origin/main` is *ahead* of local main (residue of past cherry-pick landings).

1. **Re-establish the invariant**: if `origin/main` is ahead of local `main` (legacy), FF local main up to it once:
   `git -C "$MAIN_WT" merge --ff-only origin/main` (only if it's a true FF; it should be, since local main is a clean ancestor).
2. Then land the task branch via Procedure A (the `--onto` variant) + Procedure B. The 60-odd dup replays get dropped; only your real commits land.

## Caveats specific to this repo

- **Worktree path routing** (`feedback_worktree_path_routing`, Pitfall 8): inside a harness worktree, bare `/…/remote-agent-harness/<rel>` paths and `git -C /…/remote-agent-harness` hit the **parent** checkout, not this worktree. Use the worktree's own path for edits; use `$MAIN_WT` (from `git worktree list`) explicitly when you mean the main checkout.
- `main` is checked out in the repo root, so you cannot `git checkout main` inside the task worktree and you cannot `git branch -f main` it. Move it only via `git -C "$MAIN_WT" merge --ff-only …`.
- Verify before every push: `git merge-base --is-ancestor origin/main HEAD` must be true (FF-safe). Never `--force`.

## One-line summary

origin is a FF mirror of local main; land **through** local main (`rebase onto main → push origin HEAD:main → FF local main`), resync the branch onto main at resume, and **never cherry-pick to origin**.
