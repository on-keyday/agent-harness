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

2. **One** quick confirm — a single `harness-cli ls` to show the fleet came back
   (binaries rebuilt, runners re-registered `Idle`). Report in 2–3 lines:
   what the script restarted + that the fleet is back. Do not turn this into a
   multi-step health audit; the script's own output is authoritative.

Arguments (optional): $ARGUMENTS
