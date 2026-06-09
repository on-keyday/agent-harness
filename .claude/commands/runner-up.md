---
description: Spawn an agent-runner slot via scripts/runner.sh up (auto-resolves server-cid from env). `persist` keyword routes via runner-autostart.py register for boot/login persistence.
argument-hint: "<tag> [persist] [roots=PATH,PATH] [no-worktree] [claude-bin=PATH] [max-tasks=N] [hostname=LABEL] [psk-file=PATH] [claude-args=\"...\"] [server-cid=CID]"
allowed-tools: Bash
---

Spawn an agent-runner background slot using `scripts/runner.sh up --as $1 [...]`.
This is the canonical wrapper — do not hand-roll `nohup setsid bin/agent-runner ...`. The shell script is a thin shim over `scripts/runner.py` (which delegates to `scripts/daemon.py`); together they handle slot identity, detach, `bin/.run/<slot>.{pid,log}` state, pid-reuse safety, and shutdown escalation.

Arguments: $ARGUMENTS

## Procedure

1. **Resolve tag** from `$1`.
   - If empty, ask the user for a short distinct tag (e.g. `pdf2md`, `lint`, `bash`). Show existing slots from `ls $HARNESS_REPO_PATH/bin/.run/*.pid 2>/dev/null` so the user picks a non-colliding name.
   - Slot becomes `agent-runner-<tag>`; passed to `--as <tag>`.

2. **Resolve server-cid**:
   - If `server-cid=<cid>` is present in $ARGUMENTS, use it verbatim.
   - Else: read `$HARNESS_SERVER_CID` and rewrite the trailing `-<digits>` to `-*` so the runner reconnects across server restarts. Example: `ws:192.168.3.234:8549-12982` → `ws:192.168.3.234:8549-*`.
   - If env unset AND no flag given, abort with a clear error.

3. **Shell-sandbox presets** — when the tag matches one of the well-known shell names and the user didn't supply `roots=` or `no-worktree`, fill in defaults. Any user-supplied flag overrides the matching default. The presets are OS-specific in intent — invoke them from a runner / host where the named shell actually exists; mismatched invocations will fail at spawn time.

   | tag          | default flags                                                                                                              | target |
   |--------------|----------------------------------------------------------------------------------------------------------------------------|--------|
   | `bash`       | `--no-worktree --claude-bin bash --roots $HOME/workspace`                                                                  | Linux / macOS (existing sandbox slot) |
   | `cmd`        | `--no-worktree --claude-bin C:\Windows\System32\cmd.exe --roots C:/workspace`                                              | Windows command prompt |
   | `powershell` | `--no-worktree --claude-bin C:/Windows/System32/WindowsPowerShell/v1.0/powershell.exe --roots C:/workspace`                | Windows PowerShell 5.1 (built-in) |
   | `sandbox`    | `--claude-bin $HARNESS_REPO_PATH/scripts/sandbox/claude-in-podman.sh --claude-args "--dangerously-skip-permissions"`        | Linux rootless-podman confinement (see below) |

   **The `sandbox` preset is NOT a shell preset.** It runs the *full* claude
   inside a rootless-podman container (`scripts/sandbox/`), confining the agent's
   command execution while keeping worktree-based isolation — so it does **not**
   set `--no-worktree`, and the user must still supply `roots=` (no sensible
   default). Prerequisites before spawning: `podman` installed and the image
   built once via `scripts/sandbox/build.sh`. one-shot is verified; interactive
   is bridged via a conditional `podman -t` (worth confirming on real use; see
   `scripts/sandbox/README.md`). The preset
   defaults `--claude-args "--dangerously-skip-permissions"` — safe here because
   the container is the boundary and keep-id runs claude non-root (so the flag is
   accepted); the wrapper itself stays a pure pass-through. Optional wrapper
   controls, passed the same way (`--claude-arg` / `--claude-args`): `--firewall`
   (default-deny egress allowlist) and `--omit-harness-cli` (drop the control-plane
   bridge for full isolation) — see `scripts/sandbox/README.md`. Because the roots
   usually overlap a broad existing slot (e.g. the `bash` runner serving
   `$HOME/workspace`), inject `--hostname $HARNESS_HOSTNAME-sandbox` so the slot
   is unambiguously pinnable via `--host`.

   **Windows: always specify `--claude-bin` as an absolute path.** Task Scheduler / autostart sessions don't inherit the same PATH as an interactive shell, so a bare `cmd.exe` or `powershell.exe` can fail to resolve at spawn time. The presets bake the standard System32 paths in; if the user overrides `claude-bin=...` on Windows, the override should also be an absolute path. PowerShell 7+ (`pwsh.exe`) is a common override — its location varies by install method (typically `C:/Program Files/PowerShell/7/pwsh.exe`), so look it up before passing.

   **Use forward slashes (`/`) in Windows paths**, not backslashes. The agent (claude) sometimes executes shell commands via git-bash on Windows, where backslashes in path literals get interpreted as escape sequences and produce subtly broken commands. Forward slashes are accepted by every Windows shell (cmd, powershell, git-bash) and every Windows API that takes a path string, so they are the safe portable form. Do not "normalize" the slashes back to `\` when writing into this file or into command lines.

   The Windows roots default `C:/workspace` matches the user's observed convention; override with `roots=` if the actual workspace is elsewhere.

4. **Pre-flight: detect dispatch-ambiguity collision**. Before spawning, run `harness-cli ls` and scan the RUNNERS section. The server uses longest-prefix-match across `AllowedRoots` plus selector to pick a runner; when two runners on the *same reported hostname* serve the *same exact roots string*, dispatch (`submit` / `session new` / `interactive`) with the default `Any` selector returns `AmbiguousRunner`, and a `--host <name>` pin cannot disambiguate them either.

   - Compute `intended_roots_csv` (the resolved `--roots` value, or `.` if omitted).
   - Compare against each existing row's `host=<H>` and `roots=<csv>` (exact string match on both).
   - **If a collision exists with `host == $HARNESS_HOSTNAME` AND `roots == intended_roots_csv`**, stop and present the situation to the user. Offer three paths:
     1. **Abort** — do nothing (often the right call: fold the new workload into the existing slot's `--max-tasks` capacity).
     2. **Differentiate via `--hostname`** — append `--hostname $HARNESS_HOSTNAME-<tag>` so this slot is independently pinnable via `--host $HARNESS_HOSTNAME-<tag>` from CLI / WebUI. The default `Any` selector remains ambiguous; users must pin from now on.
     3. **Change `roots`** — if the user actually meant a narrower / different repo set.
   - Proceed only after the user picks (2) or (3). Never silently override `--hostname` without confirmation.

5. **Choose path: ephemeral vs persistent**.

   - **Default (ephemeral)** — runner survives only this login. Build:

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

   - **`persist` in `$ARGUMENTS`** — runner is also registered with the OS login-autostart (Linux systemd user unit / Windows Task Scheduler) so it comes back after reboot / sign-out. Build instead:

     ```
     cd "$HARNESS_REPO_PATH" && scripts/runner-autostart.py register --tag <tag> \
         --server-cid <resolved-cid> \
         [--roots <csv>] \
         --max-tasks <N> \
         [--hostname <label>] \
         [--no-worktree] \
         [--claude-bin <path>] \
         [--psk-file <path>] \
         [--claude-args "<...>"]
     ```

     `runner-autostart.py register` writes the autostart entry, then starts the slot immediately by default (same as `--now` semantics), so a single invocation registers + brings it up.

   Defaults applied when not given (apply to both paths):
   - `--max-tasks 8` (observed convention across all live slots)
   - `--roots` omitted ⇒ agent-runner uses `.` (the repo path itself); usually you want to specify
   - `--hostname` only injected when the user picked path (2) in step 4

   Strip the literal `persist` token from `$ARGUMENTS` before forwarding the remaining flags — it is a slash-command keyword, not a runner.py flag.

6. **Verify** after spawn:
   - `harness-cli ls` — confirm a new RUNNERS row with matching roots (and matching `host=` if `--hostname` was set) appears. If not visible immediately, retry once after ~2s.
   - `tail -n 30 "$HARNESS_REPO_PATH/bin/.run/agent-runner-<tag>.log"` — check there are no `FATAL` / `dial tcp` / `connection refused` errors.

7. **Report** to the user: slot tag, reported hostname, RunnerID from ls (e.g. `ws:192.168.3.14:NNNNN-NNNNN`), `tasks=0/N`, roots. If `--hostname` was injected, remind the user that this slot now requires `--host <label>` to pin from CLI/WebUI.

## Notes

- Before spawning a *new* slot, consider whether the workload can be folded into an existing slot's `--roots` (comma-separated). One runner per host/config is usually preferable to many narrow slots, unless `--max-tasks` parallelism is the bottleneck.
- `--server-cid` accepts a wildcard suffix (`-*`) so the runner reconnects across server restarts. Locking to a specific instance id is only useful for short-lived debugging.

### Corner cases for `persist`

- **Convert a running ephemeral slot to persist**: the pre-flight collision check (step 4) will block `/runner-up <same-tag> persist` because the slot is already running. Use the manual flow instead — run `scripts/runner-autostart.py register --tag <tag> --no-start <same flags>` directly. `--no-start` skips the immediate spawn (slot is already running), but the autostart entry is registered for the next reboot.
- **Stop the daemon but keep the autostart entry**: there is no built-in flag for this. Use the symmetric two-step — `scripts/runner-autostart.py unregister --tag <tag>` (kills daemon + removes entry), then `scripts/runner-autostart.py register --tag <tag> --no-start <same flags>` to re-add the entry without starting.
