---
description: Rebuild and restart the whole local agent-runner fleet via scripts/build_and_restart_all.py (self-last, detached). Just runs the script — no topology investigation.
allowed-tools: Bash
---

Rebuild + restart every alive local agent-runner slot. This is the canonical
fleet cutover/refresh action.

## When you need this — and when you DON'T

`/restart-all` rebuilds every binary (`make build`) and restarts the local
**agent-runner** fleet. It does **NOT** restart the harness **server** — the
server is a separate process (typically a different host) that you bring up /
restart yourself, so server-only changes are **out of scope** for this command.

Pick by what actually changed:

- **CLI only** (`harness-cli` subcommand, `cli/` package, TUI rendering) → just
  `make build` in the MAIN checkout (`$HARNESS_REPO_PATH`, where `which
  harness-cli` lives). The CLI is invoked fresh each time, so the rebuilt binary
  is used immediately — **no restart**.
- **Runner** behavior (`runner/`, or what a live agent-runner executes) →
  `/restart-all`.
- **Server** behavior (`server/`) → restart the server on its own host yourself;
  `/restart-all` will not do it.

`make build` rebuilds all binaries + the webui wasm but **restarts nothing**.
Ask "what changed — CLI, runner, or server?" before reaching for `/restart-all`;
don't run it reflexively after every land.

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

   **A bare `continue` arriving as a USER/human-side input turn — right where
   the self-restart Bash should have been executing — is the resume-after-
   restart SIGNAL, not a "re-run me" prompt.** (This is specifically about an
   *input turn*, not anything inside a Bash result or tool output.) When this
   session's own runner is torn down last, the SIGHUP severs the transcript
   mid-call; the runner re-spawns claude with `--continue`, which surfaces on
   the next turn as a user-side `continue` picking up at exactly that Bash
   point. If the conversation sequence is *[the self-restart Bash point] → a
   user input turn that just says `continue`*, read it as **"the restart
   already completed, resume from here"** — confirm via step 2 and stop. Do
   **NOT** read it as the command having failed, and do **NOT** re-run the
   script in response — least of all with `--force`.

   Belt-and-suspenders: the script is **debounced** — it stamps
   `bin/.run/last-restart-stamp` and no-ops (exit 0, "a restart completed Ns
   ago … skipping") if re-run within 300s. So if a resume re-fires it anyway,
   you get a safe skip, not a double restart. **A debounce skip that lands right
   after such a `continue` is the safety mechanism working correctly — the
   restart already happened; the skip is the proof, not an obstacle.** `--force`
   is a deliberate, explicitly-requested manual action for when you genuinely
   want another cycle inside the window. **Never reach for `--force` as a reflex
   to get past an unexpected skip** — doing so produces exactly the redundant
   double restart the debounce exists to prevent.

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
