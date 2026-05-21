---
description: Stop an agent-runner slot via scripts/runner.sh down — refuses if tasks are assigned or if it would self-terminate
argument-hint: "<tag>"
allowed-tools: Bash
---

Gracefully stop an agent-runner slot. **Refuses if the runner has any task currently assigned, or if downing it would terminate this very session.**

Use the canonical script — do not invoke `kill` directly. `scripts/runner.sh down` handles SIGTERM → SIGKILL escalation and state cleanup.

Arguments: $ARGUMENTS

## Procedure

1. **Resolve tag** from `$1`.
   - If empty, list candidates from `ls $HARNESS_REPO_PATH/bin/.run/*.pid 2>/dev/null` and ask the user which slot to take down.
   - Slot becomes `agent-runner${tag:+-$tag}`; primary slot has empty tag.

2. **Locate the process**:
   - Read pid from `$HARNESS_REPO_PATH/bin/.run/<slot>.pid`.
   - If the pid file is missing → "not running, nothing to do"; stop.
   - `ps -p <pid> -o args=` to confirm it's alive and extract its `--roots` argument. If the process is dead but the pid file exists → flag as stale, proceed with `down` for cleanup only.

3. **Self-kill protection** (HARD ABORT if matched):
   - This task is itself being served by the runner identified by `$HARNESS_RUNNER_ID` (e.g. `ws:192.168.3.14:NNNNN-NNNNN`).
   - Cross-check the target slot's pid / roots against `$HARNESS_RUNNER_ID` and `$HARNESS_REPO_PATH`. If the target runner serves the current task, **abort** and explain that downing it would SIGTERM this very claude process.
   - When in doubt, prompt the user before proceeding.

4. **Check assigned tasks** via `harness-cli ls`:
   - Locate the RUNNERS row whose `roots=` matches the process's `--roots` extracted in step 2.
   - Parse `tasks=X/Y`.
   - **If X > 0**: cross-reference the TASKS section, listing each task whose `repo=` lies under the runner's roots, then **abort**. Tell the user to wait for the tasks to finish, cancel them (`harness-cli cancel <id>`), or migrate them; do not force.
   - Note: the runner state label (`Idle` / `Busy`) is informational — the authoritative signal is the `tasks=X/Y` count. `Idle` only means "available to accept more"; it does NOT mean "no work in flight".

5. **Execute `down`** only after steps 3 and 4 both pass clean:

   ```
   cd "$HARNESS_REPO_PATH" && scripts/runner.sh down${tag:+ --as $tag}
   ```

   (`scripts/runner.sh` resolves `bin/.run` relative to itself, so it must run from `$HARNESS_REPO_PATH`, not a worktree.)

6. **Verify shutdown**:
   - The pid file should be gone (or no longer reference a live process).
   - `harness-cli ls` no longer lists a RUNNERS row with that roots / RunnerID.
   - If either still shows the slot after a few seconds, surface `tail -n 50 $HARNESS_REPO_PATH/bin/.run/<slot>.log` and stop; do not escalate to `kill` manually.
