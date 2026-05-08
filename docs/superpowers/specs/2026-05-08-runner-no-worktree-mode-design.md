# Runner `--no-worktree` mode

Status: Approved
Date: 2026-05-08

## Motivation

The harness creates a `git worktree` per task under `<repo>/.harness-worktrees/<task-id>/` for isolation between parallel claude sessions. That isolation matters for the original use case (concurrent claude-code runs on the same repo) but is dead weight for "generic process runner" workloads — e.g. `agent-runner --claude-bin bash`, where the spawned process does not edit files and does not need a per-task git checkout. The worktree creation, branch ref, settings injection, and conditional cleanup all become noise.

This spec adds a runner-level operating mode flag, `--no-worktree`, that turns off the worktree machinery entirely for that runner instance.

## Scope

- Runner-level (process-wide) flag. One runner = one mode. Mixed mode within a single runner is **not** supported.
- No protocol changes. `protocol.AssignTask` / `protocol.OpenExecRunnerRequest` / `protocol.RunnerHello` are unchanged.
- No server, CLI, TUI, or WebUI changes.

## Configuration surface

### CLI flags (`cmd/agent-runner/main.go`)

```
--no-worktree                     bool, default false
--force-inject-harness-settings   bool, default false
```

`--force-inject-harness-settings` is orthogonal to `--no-worktree`:
- Default mode (worktree on): always injects regardless of this flag — flag is effectively no-op.
- `--no-worktree` alone: skips injection (default for that mode).
- `--no-worktree --force-inject-harness-settings`: skips worktree but still writes `<repoPath>/.claude/settings.json` and `<repoPath>/.claude/skills/`. For users who want agentboard hooks active when running real `claude` in their main checkout.

### Config plumbing

```go
// runner/connect.go
type Config struct {
    // ... existing fields ...
    NoWorktree                  bool
    ForceInjectHarnessSettings  bool
}

// runner/session.go
type Session struct {
    // ... existing fields ...
    NoWorktree                  bool
    ForceInjectHarnessSettings  bool
}
```

`runner.Run` propagates both fields from `cfg` into the `Session` it constructs.

## Behavior matrix

For each step of `handleAssign` and `handleOpenExec`, the difference between current behavior (`NoWorktree=false`) and the new mode (`NoWorktree=true`):

| Step | `false` (current) | `true` (new) |
|------|-------------------|--------------|
| 1. `TaskAccepted` | sent | sent |
| 2a. worktree create | `wm.Create(taskID)` → `<repo>/.harness-worktrees/<id>` | **skipped**; `dir = repoPath` |
| 2b. branch `harness/<id>` | created (or reused on resume) | **skipped**; HEAD untouched |
| 3. `TaskStarted.WorktreeDir` | worktree dir | `repoPath` (sent as-is) |
| 4a. `WriteAgentSettings(dir)` | merge into `<dir>/.claude/settings.json` | **skipped by default**; runs if `ForceInjectHarnessSettings=true` (target = `<repoPath>/.claude/settings.json`) |
| 4b. `WriteAgentSkills(dir)` | inject `<dir>/.claude/skills/<harness>/` | **skipped by default**; runs if `ForceInjectHarnessSettings=true` (target = `<repoPath>/.claude/skills/`) |
| 5. claude exec | `cwd = <worktree dir>` | `cwd = repoPath` |
| 6. `TaskFinished` | sent | sent |
| 7. `wm.RemoveIfClean` | run; remove if clean | **skipped** (do not remove user's repo) |

Implementation pattern in both `handleAssign` and `handleOpenExec`:

```go
var dir string
if s.NoWorktree {
    dir = repoPath
    s.logger().Info("no-worktree mode: using repo path as cwd",
        "task_id", taskIDHex, "repo", repoPath)
} else {
    wm := s.getWorktreeManager(repoPath)
    d, err := wm.Create(taskIDHex)
    if err != nil {
        // existing error path
    }
    dir = d
}
```

The `wm.RemoveIfClean` block at Step 6 is wrapped:

```go
if !s.NoWorktree {
    switch r := wm.RemoveIfClean(taskIDHex, HarnessInjectedPaths); {
        // existing cases
    }
}
```

`RemoveIfClean` is **never** called in `NoWorktree=true`, even with `ForceInjectHarnessSettings=true` — calling `git worktree remove` on the user's main checkout would be destructive. Injected `.claude/settings.json` / `.claude/skills/` persist after task end; users clean up manually if desired.

The `WriteAgentSettings` / `WriteAgentSkills` calls are wrapped on the **inverse** condition:

```go
if !s.NoWorktree || s.ForceInjectHarnessSettings {
    if err := WriteAgentSettings(dir); err != nil { /* warn */ }
    if err := WriteAgentSkills(dir); err != nil { /* warn */ }
}
```

## Concurrency

No additional serialization. The runner's `s.tasks` map plus per-task `taskCtx` already supports concurrent tasks. With `NoWorktree=true`, multiple tasks share `cwd = repoPath`, but for the target use case (e.g. `--claude-bin bash`) this is benign — independent processes do not collide.

If a user opts into `NoWorktree` while running real `claude` on the same repo with `--max-tasks > 1`, parallel claude sessions could clobber each other's edits. That is the user's responsibility — the flag is opt-in. Documented in the README note.

## Allowed roots gate

Unchanged. `repoAllowed(repoPath)` still gates every assignment regardless of mode. This prevents a malicious / malformed `AssignTask.RepoPath` from setting `cwd` to an arbitrary path.

The empty-`RepoPath` fallback (`repoPath = AllowedRoots[0]`) also still applies.

## Non-git directories

Automatically supported. `wm.Create` is the only call that requires git; skipping it means a `--no-worktree` runner can serve directories without `.git/`. No explicit "is this git?" check is added or removed.

## Resume

`--resume` semantics rely on:
1. Server reusing the same `task_id`.
2. Runner reusing the same `cwd` so claude's project key (`~/.claude/projects/<cwd-hash>/`) matches.

In `NoWorktree=true`, condition (2) is satisfied trivially because `cwd = repoPath` is constant per runner. The branch-reuse logic in `wm.Create` is irrelevant (never invoked). Resume works without any additional code; a regression test confirms it.

Side effect: every task on a `--no-worktree` runner shares the same claude project key. If the user `--resume`s by `<session-uuid>`, they get the right session; if they let claude pick a default, all tasks see the same project history. Documented; not a bug.

## prune-local

`harness-cli prune-local` walks `<repo>/.harness-worktrees/`. In `NoWorktree=true` runs nothing is created there, so `prune-local` is a no-op for this runner's data. No code change needed.

## Mixed-mode deployments

A server may have one runner with `--no-worktree` and another without. Server-side dispatch is mode-agnostic; selection happens via the existing `--runner / --host / --ip` selectors at submit time. No coordination required.

## Detach / reattach

Independent flag. `NoWorktree=true` + `OpenInteractive(Detachable=1)` is supported: the SessionMux uses `cwd = repoPath` and reattach works the same. No additional logic.

## Agentboard hook loss (trade-off)

`WriteAgentSettings` is what installs the runner's hooks (`SessionStart` × 2 for `harness.hello` / `--self` subscriptions, `UserPromptSubmit` for inbox flush). Skipping it in `NoWorktree=true` means an agent in this mode is not auto-subscribed to agentboard topics and does not auto-flush its inbox.

For the target use case (`--claude-bin bash` etc.), this does not matter — bash does not read `.claude/settings.json`. For users who want both `NoWorktree` and agentboard integration with real `claude`, the escape hatch is `--force-inject-harness-settings`: the runner will then merge its hooks into `<repoPath>/.claude/settings.json` and write `<repoPath>/.claude/skills/<harness>/`. The merge logic is idempotent (deduplicated by command prefix) so re-running is safe; user-defined hooks under other keys are preserved. README notes this.

## Logging

- At runner startup (after `Config` is parsed): `slog.Info("runner config", ..., "no_worktree", cfg.NoWorktree, "force_inject_harness_settings", cfg.ForceInjectHarnessSettings)`.
- At each `handleAssign` / `handleOpenExec` Step 2 in `NoWorktree=true`: `s.logger().Info("no-worktree mode: using repo path as cwd", "task_id", taskIDHex, "repo", repoPath)`.

## Testing

### Unit tests (`runner/session_test.go`)

1. `TestHandleAssign_NoWorktree_NoGitDir` — `Session{NoWorktree:true}` over a non-git temp dir. Expectations:
   - `TaskAccepted`, `TaskStarted(WorktreeDir == repoPath)`, `TaskFinished` all delivered.
   - `<repo>/.harness-worktrees/` does not exist.
   - `<repo>/.claude/settings.json` does not exist (or, if pre-existing, is byte-identical after the run).
   - `<repo>/.claude/skills/` does not exist (or unchanged).
   - `<repo>` itself still exists.

2. `TestHandleAssign_NoWorktree_GitDir_HEADUntouched` — existing git repo with a known current branch. Expectations after task end:
   - `git symbolic-ref HEAD` returns the same ref as before.
   - No `harness/<id>` ref under `refs/heads/`.
   - No `<repo>/.harness-worktrees/<id>` directory.
   - `git worktree list --porcelain` shows only the original worktree.

3. `TestHandleOpenExec_NoWorktree_StreamPath` — interactive equivalent of (1), using a mock bidi stream.

4. `TestHandleAssign_NoWorktree_ConcurrentTasks` — two `handleAssign` invocations in parallel on the same `repoPath`. Both reach `TaskFinished`; neither blocks on the other.

5. `TestHandleAssign_NoWorktree_Resume` — same `task_id` assigned twice. Second run also completes successfully; cwd identical (verified via a probe — e.g. a marker file written by the test process and observed unchanged).

6. `TestHandleAssign_NoWorktree_ForceInject` — `Session{NoWorktree:true, ForceInjectHarnessSettings:true}` over an empty temp dir. Expectations:
   - `<repo>/.claude/settings.json` exists and contains the runner's `harness-cli` hook entries (verified by parsing JSON and asserting the expected commands are present).
   - `<repo>/.claude/skills/` exists (non-empty, at least one harness skill dir).
   - `<repo>` itself still exists; no `<repo>/.harness-worktrees/` (worktree skip is independent).

### Integration test (`integration/e2e_test.go`)

One additional scenario: launch `agent-runner --no-worktree`, submit a task, tail the log, observe completion. Mirrors the existing minimal e2e but with the flag set.

### Manual smoke

```
agent-runner --no-worktree --claude-bin bash --roots /tmp/scratch
harness-cli submit --repo /tmp/scratch --task 'pwd && ls -la'
```

Expected output: `cwd == /tmp/scratch`, no `.harness-worktrees/` created, no `.claude/` files written, dir intact after task end.

## README updates

Short addition under the existing operating-modes section (or equivalent location):

> `--no-worktree`: skip worktree creation and run each task directly in the bound repo path. Intended for generic-process workloads (`--claude-bin bash`, etc.). Disables `.claude/settings.json` and `.claude/skills/` injection — agentboard hooks are not auto-installed in this mode. The user's repo is left untouched on task end.
>
> `--force-inject-harness-settings`: only meaningful with `--no-worktree`. Re-enables `.claude/settings.json` / `.claude/skills/` injection at the bound repo path, so agentboard hooks fire even without a per-task worktree. Files persist after task end (no auto-cleanup); manage them manually if desired.

## Out of scope

- Per-task `NoWorktree` override (would require protocol changes; not justified for dogfood).
- Auto-detection from `--claude-bin` value (implicit behavior; rejected per project memory on protocol-explicit-over-convention).
- Changes to `harness-cli prune-local` (already a no-op when nothing is created).
- Server-side awareness of mode (intentionally none).

## File touch list

- `runner/connect.go` — `Config.NoWorktree`; pass into `Session`.
- `runner/session.go` — `Session.NoWorktree`; branching in `handleAssign` / `handleOpenExec` Steps 2, 4, 7.
- `cmd/agent-runner/main.go` — `--no-worktree` flag.
- `runner/session_test.go` — 5 new tests above.
- `integration/e2e_test.go` — 1 new scenario.
- `README.md` — 1 short note.
