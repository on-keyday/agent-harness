---
description: Rebuild and restart the whole local agent-runner fleet via scripts/build_and_restart_all.py (self-last, detached). Just runs the script — no topology investigation.
allowed-tools: Bash
---

Rebuild + restart every alive local agent-runner slot. This is the canonical
fleet cutover/refresh action.

**Do NOT investigate topology, env vars, or self-termination risk first.** The
script already handles all of it: self-last ordering, detached self-restart that
survives the cascade (`scripts/restart.py`), stale-slot skipping, and self
detection via the parent-process chain. Re-deriving any of that before running is
exactly the wasted "let me check the situation" sequence this command exists to
skip.

## Procedure

1. Run the canonical orchestrator from the **main checkout** (not the worktree —
   the worktree's `bin/.run/` is empty; live slots live under
   `$HARNESS_REPO_PATH/bin/.run/`):

   ```
   python "$HARNESS_REPO_PATH/scripts/build_and_restart_all.py"
   ```

   Pass through any `$ARGUMENTS` (e.g. `--skip-build`, `--dry-run`) verbatim.

   Expect this to eventually restart the runner serving this very session
   (`$HARNESS_RUNNER_ID`) **last**, detached — so the build + restart of all
   other slots completes first, then this claude process may be torn down and
   re-spawned with `--continue`. That is normal, not an error.

   **A severed / empty / "not executed"-looking Bash result is the SUCCESS
   signature here, not a failure.** When the self-restart tears this process
   down, the running `build_and_restart_all.py` call is SIGHUP'd mid-flight, so
   its stdout never makes it back into the transcript — the tool result may show
   as empty, truncated, or as though the command never ran. That only means the
   connection was cut at the moment of self-restart; it says **nothing** about
   whether the restart happened. It almost certainly did: self is torn down
   **last**, after every other slot is already rebuilt and back. So:
   **do NOT conclude the restart failed, and do NOT re-run the script** on the
   strength of a missing/empty result. Confirm with step 2 instead.

2. Quick confirm — the Bash output is NOT the evidence (per the note above);
   these two cheap checks are, and they are **separate commands**:
   - `harness-cli ls` → runners re-registered `Idle` with **fresh connection
     IDs** (a new ID per runner = it reconnected = it restarted). This is the
     primary proof the fleet came back. `harness-cli ls` does **not** show
     binary build times — don't expect them here.
   - `ls -la "$HARNESS_REPO_PATH/bin/"` → **recent rebuild timestamps** on the
     binaries (proves `make build` ran). Only needed if you want build proof in
     addition to the reconnect proof.

   Report in 2–3 lines: what was restarted + that the fleet is back. Don't turn
   this into a multi-step health audit — these two reads are the whole check.

Arguments (optional): $ARGUMENTS
