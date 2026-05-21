---
description: Spawn an agent-runner slot via scripts/runner.sh up (auto-resolves server-cid from env)
argument-hint: "<tag> [roots=PATH,PATH] [no-worktree] [claude-bin=PATH] [max-tasks=N] [hostname=LABEL] [psk-file=PATH] [claude-args=\"...\"] [server-cid=CID]"
allowed-tools: Bash
---

Spawn an agent-runner background slot using `scripts/runner.sh up --as $1 [...]`.
This is the canonical wrapper — do not hand-roll `nohup setsid bin/agent-runner ...`. The script (via `scripts/_daemon.sh`) handles slot identity, detach, `bin/.run/<slot>.{pid,log}` state, pid-reuse safety, and shutdown escalation.

Arguments: $ARGUMENTS

## Procedure

1. **Resolve tag** from `$1`.
   - If empty, ask the user for a short distinct tag (e.g. `pdf2md`, `lint`, `bash`). Show existing slots from `ls $HARNESS_REPO_PATH/bin/.run/*.pid 2>/dev/null` so the user picks a non-colliding name.
   - Slot becomes `agent-runner-<tag>`; passed to `--as <tag>`.

2. **Resolve server-cid**:
   - If `server-cid=<cid>` is present in $ARGUMENTS, use it verbatim.
   - Else: read `$HARNESS_SERVER_CID` and rewrite the trailing `-<digits>` to `-*` so the runner reconnects across server restarts. Example: `ws:192.168.3.234:8549-12982` → `ws:192.168.3.234:8549-*`.
   - If env unset AND no flag given, abort with a clear error.

3. **Preset for tag `bash`**: if the user didn't supply `roots=` or `no-worktree`, default to `--no-worktree --claude-bin bash --roots $HOME/workspace` (matches the existing bash sandbox slot). Any user-supplied flag overrides the corresponding default.

4. **Pre-flight: detect dispatch-ambiguity collision**. Before spawning, run `harness-cli ls` and scan the RUNNERS section. The server uses longest-prefix-match across `AllowedRoots` plus selector to pick a runner; when two runners on the *same reported hostname* serve the *same exact roots string*, dispatch (`submit` / `session new` / `interactive`) with the default `Any` selector returns `AmbiguousRunner`, and a `--host <name>` pin cannot disambiguate them either.

   - Compute `intended_roots_csv` (the resolved `--roots` value, or `.` if omitted).
   - Compare against each existing row's `host=<H>` and `roots=<csv>` (exact string match on both).
   - **If a collision exists with `host == $HARNESS_HOSTNAME` AND `roots == intended_roots_csv`**, stop and present the situation to the user. Offer three paths:
     1. **Abort** — do nothing (often the right call: fold the new workload into the existing slot's `--max-tasks` capacity).
     2. **Differentiate via `--hostname`** — append `--hostname $HARNESS_HOSTNAME-<tag>` so this slot is independently pinnable via `--host $HARNESS_HOSTNAME-<tag>` from CLI / WebUI. The default `Any` selector remains ambiguous; users must pin from now on.
     3. **Change `roots`** — if the user actually meant a narrower / different repo set.
   - Proceed only after the user picks (2) or (3). Never silently override `--hostname` without confirmation.

5. **Build the command** (must run from `$HARNESS_REPO_PATH` — `scripts/runner.sh` resolves `bin/.run` relative to itself):

   ```
   cd "$HARNESS_REPO_PATH" && scripts/runner.sh up --as <tag> \
       --server-cid <resolved-cid> \
       [--roots <csv>] \
       --max-tasks <N> \
       [--hostname <label>] \
       [--no-worktree] \
       [--claude-bin <path>] \
       [--psk-file <path>] \
       [--claude-args "<...>"]
   ```

   Defaults applied when not given:
   - `--max-tasks 8` (observed convention across all live slots)
   - `--roots` omitted ⇒ agent-runner uses `.` (the repo path itself); usually you want to specify
   - `--hostname` only injected when the user picked path (2) in step 4

6. **Verify** after spawn:
   - `harness-cli ls` — confirm a new RUNNERS row with matching roots (and matching `host=` if `--hostname` was set) appears. If not visible immediately, retry once after ~2s.
   - `tail -n 30 "$HARNESS_REPO_PATH/bin/.run/agent-runner-<tag>.log"` — check there are no `FATAL` / `dial tcp` / `connection refused` errors.

7. **Report** to the user: slot tag, reported hostname, RunnerID from ls (e.g. `ws:192.168.3.14:NNNNN-NNNNN`), `tasks=0/N`, roots. If `--hostname` was injected, remind the user that this slot now requires `--host <label>` to pin from CLI/WebUI.

## Notes

- Before spawning a *new* slot, consider whether the workload can be folded into an existing slot's `--roots` (comma-separated). One runner per host/config is usually preferable to many narrow slots, unless `--max-tasks` parallelism is the bottleneck.
- `--server-cid` accepts a wildcard suffix (`-*`) so the runner reconnects across server restarts. Locking to a specific instance id is only useful for short-lived debugging.
