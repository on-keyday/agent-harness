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

   **Why the launch may or may not be recorded — and why that is fine.** When
   the self-restart tears this process down, the running
   `build_and_restart_all.py` call is SIGHUP'd mid-flight. Whether its Bash
   tool-call/result record was flushed into the transcript before the kill is
   **timing-dependent and non-deterministic**: sometimes the record survives,
   sometimes the transcript has a **hole** exactly where the launch was. So an
   empty / truncated / "never executed"-looking result — or no record at all —
   proves **nothing** either way. Do **not** infer success from its presence,
   and do **not** infer failure from its absence. (Self is torn down **last**,
   after every other slot is already rebuilt and back, so the restart almost
   certainly happened regardless of what the transcript shows.)

   **Decide only from ground truth, never from the transcript.** A
   `--continue`-resumed session is the dangerous case: it reloads history that
   may contain that hole, sees no trace of its own triggering launch, and
   concludes "I never ran it" — then re-runs the script. Don't. The only
   reliable evidence is (a) the debounce-stamp age, (b) the `bin/` binary build
   mtimes, and (c) **fresh connection IDs** in `harness-cli ls` (step 2) —
   never whether the launch Bash appears in the transcript.

   **`--continue` is a silent history reload, NOT a `continue` input token.**
   On restart the runner re-spawns claude with `--continue`, which silently
   reloads the prior conversation and waits — it does **not** inject any
   user-side message. So a literal `continue` turn is the **human operator**
   nudging the resumed session, not an auto-generated token (the harness's own
   synthetic prompts are tagged, e.g. `<harness:...>`; a bare `continue` is not
   one). Either reading leads to the same action: it means "resume here" —
   confirm via step 2 and stop. Do **NOT** treat a missing record as failure,
   and do **NOT** re-run the script — least of all with `--force`.

   Belt-and-suspenders: the script is **debounced** — it stamps
   `bin/.run/last-restart-stamp` and no-ops (exit 0, "a restart completed Ns
   ago … skipping") if re-run within 300s. So if a resume re-fires it anyway,
   you get a safe skip, not a double restart. **A debounce skip is the safety
   mechanism working correctly — it is PROOF the prior (possibly unrecorded)
   run already happened, not an obstacle.** `--force` is a deliberate,
   explicitly-requested manual action for when you genuinely want another cycle
   inside the window. **Never reach for `--force` as a reflex to get past an
   unexpected skip** — doing so produces exactly the redundant double restart
   the debounce exists to prevent.

   **Timeline of a normal self-restart** — note where the record hole opens and
   how the decision routes around it:

   ```
   T+0s     /restart-all → you launch:  python …/build_and_restart_all.py
   T+1–8s   every OTHER slot rebuilds + reconnects
            → fresh connection IDs start appearing in `harness-cli ls`
   T+~9s    the SELF slot (this session's runner) is torn down LAST, detached
            → SIGHUP severs the launch Bash mid-flight
            → its tool-call/result record MAY or MAY NOT reach the transcript
   T+~12s   runner re-spawns `claude --continue` → history silently reloaded
            → if the record was lost, the transcript now has a HOLE exactly
              where the launch was. "Did I even run it?" is the trap — ignore it.
   later    a bare `continue` turn may arrive → that is the HUMAN nudging the
            resumed session (not an injected token); it proves nothing by itself
   T+Δ      you run step 2:  fresh IDs in `harness-cli ls` + recent `bin/`
            mtimes + a <300s debounce stamp  →  restart CONFIRMED, stop.
            (re-firing the script here returns a safe debounce skip — and that
             skip is itself proof the prior, possibly unrecorded, run happened)
   ```

   Whether the `T+~9s` launch record survives is the non-deterministic part;
   every line of the decision at `T+Δ` reads ground truth, never the transcript.

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
