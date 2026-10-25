# Sandbox history window Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `--history-window` to `scripts/sandbox/agent-in-podman.sh`, so that a sandboxed claude, pi or codex sees only its own task's conversations, and those conversations stay in the host's normal store.

**Architecture:** Under the flag, the wrapper mounts a per-run host temp directory as the agent's config root. Inside it, it bind-mounts the host's per-cwd history directory (claude, pi). For codex it instead mounts a per-task directory that is kept in hardlink sync with `~/.codex/sessions` by a stdlib Python helper. The helper runs once before `podman run` and from the existing reaper. Credential and read-only config files are mounted one by one. Everything per-agent lives in the agent table.

**Tech Stack:** bash (wrapper), Python 3 stdlib (sync helper, tests, upstream probe), rootless podman, `unittest` via `make scripts-test`.

**Spec:** `docs/superpowers/specs/2026-10-26-sandbox-history-window-design.md`. Read it in full first, including §1 Intent. Read the parent design `docs/superpowers/specs/2026-08-18-generic-agent-sandbox-design.md` for the wrapper's existing modes.

## Global Constraints

- The repo is public. Committed files contain no LAN IPs, hostnames or private paths. Tests build every path under a `tempfile` directory.
- Per-agent differences live only in the wrapper's agent table (`case "$AGENT" in`). No `case "$AGENT"` appears elsewhere.
- Every refusal exits 2, prints `[agent-in-podman] --history-window: <reason>` to stderr, and starts no container. Nothing falls back to the shared mode.
- The flag is opt-in. Without it, the `podman run` argv is identical to today's, except for the new `--env SANDBOX_HISTORY=shared` and the summary line's `history=shared`.
- claude's window works under mount auth, with `.credentials.json` mounted on its own, and under `--token-auth`, with no credential file (spec §6, row 17). pi and codex run under mount auth.
- The sync helper and the storage probe use the Python stdlib only. Do not install packages.
- `make scripts-test` stays offline: no podman, no network, no installed agents, no credentials.
- Never read credential contents. Tests use fake files containing `x`.
- Wrapper comments follow the file's existing style: state the mechanism, plus the measurement and its date where one exists.
- Prose files (README, spec, `firing-log.md`) are edited by reading the whole file, then using Edit.

## Review Focus

1. **Nested mount order.** podman has to mount the config root before the window that sits inside it. If not, the window is shadowed, the agent writes into the temp root, and the conversation disappears without any error. The wrapper test in Task 2 pins the argv order. The probe run in Task 6 confirms that the window content is visible from inside the container.
2. **A rollout the reaper never linked.** If the host reboots, or the reaper dies, during a codex run, a rollout can exist only in the per-task directory. The next start must link it to the host before it empties that directory. Pinned in Task 1 (`test_import_links_unexported_rollout_before_clearing`).
3. **A rollout whose first line is not yet complete.** The periodic export can see a file whose first line has not been flushed. That file is skipped silently and linked on a later round. It is not refused. Pinned in Task 1.
4. **A cwd reached through a symlink.** The cwd codex records and the wrapper's `$PWD` can differ only by symlinks. Both sides are compared through `os.path.realpath`. Pinned in Task 1.
5. **A refusal must not leave a temp root behind.** The wrapper creates the per-run temp root only after every refusal check has passed. Pinned in Task 2 (`test_refusal_leaves_no_temp_root`).

---

### Task 1: codex sync helper

**Files:**
- Create: `scripts/sandbox/history_window.py`
- Test: `scripts/sandbox/test_history_window.py` (Tasks 2 and 3 add to the same file)

**Interfaces:**
- Produces (`history_window.py`):
  - `rollout_cwd(path: Path) -> str | None` — `payload.cwd` from the first line when `type == "session_meta"`. Returns `None` if the first line is incomplete (no `\n` within 8 MiB) or unparseable.
  - `same_dir(a: str, b: str) -> bool` — compares `os.path.realpath` of both.
  - `codex_export(task_dir: Path, host_sessions: Path, cwd: str, log: TextIO) -> list[Path]` — returns the host paths it linked.
  - `codex_import(task_dir: Path, host_sessions: Path, cwd: str, log: TextIO) -> list[Path]` — returns the task-dir paths it linked.
  - `class CrossDevice(Exception)`. Both functions raise it when `_device(task_dir) != _device(host_sessions)`. `_device(p: Path) -> int` returns `os.stat(p).st_dev`, so that tests can patch it.
  - CLI: `python3 history_window.py {codex-import|codex-export} --task-dir T --host-sessions H --cwd C`. Exits 0. Exits 2 on `CrossDevice`, with `[agent-in-podman] --history-window: <task-dir> and <host-sessions> are on different filesystems` on stderr. Log lines go to stderr.

Rules, from spec §7b:
- Only `YYYY/MM/DD/rollout-*.jsonl` is considered, on both sides. `.jsonl.zst` is ignored.
- **Export:** for each task file, take `rel` = its path relative to the task dir.
  - If `host/rel` exists with the same inode, skip it.
  - If it exists with a different inode, skip it and log `diverged <rel>`.
  - If it is missing and `rollout_cwd` is `None`, skip it silently.
  - If it is missing and the cwd is a different directory, skip it and log `refused <rel> cwd=<x>`.
  - Otherwise, create the parent directories and `os.link` the file. A `FileExistsError` there counts as "exists".
  - Never write, overwrite or unlink anything under `host_sessions`.
- **Import:** first call `codex_export`. Then unlink every `rollout-*.jsonl` under the task dir and remove the directories left empty. Then hardlink every host rollout whose `rollout_cwd` is the same directory to the same `rel` in the task dir.

- [ ] **Step 1: Write the failing tests** in `test_history_window.py`. Use a `HelperTests(unittest.TestCase)` class. Its fixture `rollout(root, rel, cwd)` writes `{"type":"session_meta","payload":{"cwd":cwd,"id":"t"}}\n{"type":"x"}\n`.

```python
def test_rollout_cwd_reads_first_line(self):            # == str(work)
def test_rollout_cwd_incomplete_first_line_is_none(self):  # file '{"type":"session_meta"' with no newline -> None
def test_same_dir_through_symlink(self):                 # symlink -> work: same_dir(str(link), str(work)) is True
def test_import_links_only_this_cwd(self):
    # host has 2026/10/25/rollout-a.jsonl (cwd=work) and rollout-b.jsonl (cwd=other)
    # -> task has rollout-a with st_ino equal to the host's; rollout-b absent
def test_import_clears_previous_links(self):
    # task already holds 2026/10/24/rollout-old.jsonl that also exists on the host (same inode), host copy now cwd=other
    # -> after import it is gone from task, still on host
def test_import_links_unexported_rollout_before_clearing(self):
    # task holds 2026/10/26/rollout-new.jsonl (cwd=work) that the host lacks
    # -> after import the host has it, with the same inode
def test_export_never_overwrites_host(self):
    # host and task both have rel, different inodes -> host content unchanged, log contains "diverged"
def test_export_refuses_other_cwd(self):                 # task rollout cwd=other -> not on host, log contains "refused"
def test_export_skips_incomplete_silently(self):         # -> not on host, log empty
def test_cross_device_raises(self):                      # patch history_window._device to return 1 then 2 -> CrossDevice
def test_cli_cross_device_exits_2(self):                 # main([...]) with _device patched -> returns 2
```

- [ ] **Step 2: Run them and see them fail.** `python3 -m unittest discover -s scripts/sandbox -p 'test_history_window.py' -v` fails with `ModuleNotFoundError: history_window`.
- [ ] **Step 3: Implement `history_window.py`** with the interfaces above. `main(argv: list[str] | None = None) -> int`, and `if __name__ == "__main__": raise SystemExit(main())`, as in `check_codex_release.py`.
- [ ] **Step 4: Run them and see them pass.** Same command; all pass.
- [ ] **Step 5: Commit.** `git add scripts/sandbox/history_window.py scripts/sandbox/test_history_window.py && git commit -m "sandbox: codex rollout hardlink sync for the history window"`

---

### Task 2: wrapper — the flag, the table fields, the claude and pi windows

**Files:**
- Modify: `scripts/sandbox/agent-in-podman.sh`. The touched blocks are flag parsing (`:98-129`), the agent table (`:152-383`), the git mounts (`:419-446`, which record whether the cwd is a linked worktree), auth (`:470-511`), the summary line (`:662`), the reaper (`:689-706`) and `podman run` (`:725-741`).
- Modify: the header comment's flag list (`:34-79`), to add `--history-window`.
- Test: `scripts/sandbox/test_history_window.py` (class `WrapperTests`).

**Interfaces:**
- New agent-table fields. Every path is relative to the home directory:
  - `A_HISTORY` — `dir` for claude and pi, `sync` for codex, empty for the others.
  - `A_HISTORY_STORE` — `.claude/projects` / `.pi/agent/sessions` / `.codex/sessions`.
  - `history_name()` — prints the window's directory name for the cwd in `$1`. It is defined inside the claude and pi arms only.
    - claude: `printf %s "$1" | LC_ALL=C sed 's/[^A-Za-z0-9]/-/g'`
    - pi: `--` + the cwd without its leading `/`, with `/`, `\` and `:` → `-`, + `--`
  - `A_HISTORY_ROOTS=( .claude )`, `( .pi )`, `( .codex )` — the ephemeral config roots (directories).
  - `A_HISTORY_ROOT_FILES` — claude `( .claude.json )`, otherwise `()`. These are ephemeral files, created in the temp root containing `{}`.
  - `A_CRED_FILES` — claude `( .claude/.credentials.json )`, pi `( .pi/agent/auth.json )`, codex `( .codex/auth.json )`. They are not mounted under `--token-auth`, which replaces them.
  - `A_RO_CONFIG` — claude `()`, pi `( .pi/agent/settings.json )`, codex `( .codex/config.toml )`. A file missing on the host is skipped.
  - `A_SHARED_DIRS` — codex `( .codex/thread-writer-locks )`, otherwise `()`. Created on the host if missing.
  - `A_HISTORY_SEED=1` for claude. Under the window, it adds the same `SANDBOX_SEED_CONFIG=1` / `SANDBOX_SEED_PROJECTS` env that token auth sets at `:495-499`, because the ephemeral `~/.claude.json` needs onboarding and trust seeded.
- Variables later tasks use:
  - `history_window=0|1`
  - `history_root` — the per-run temp root, or empty
  - `history_mode=window|shared`
- `podman run` gains `--env SANDBOX_HISTORY="$history_mode"`. The summary line gains ` history=$history_mode`.

The window block runs after the auth block. Order:
1. Refusals (below).
2. `history_root=$(mktemp -d "${TMPDIR:-/tmp}/harness-sandbox-root.XXXXXX")`.
3. Inside it, pre-create:
   - each root in `A_HISTORY_ROOTS` (directory) and each file in `A_HISTORY_ROOT_FILES` (containing `{}`);
   - `<store>/<name>` (directory);
   - each credential file and read-only config file (empty file);
   - each shared directory.
4. Create the host window directory (`mkdir -p "$HOME_DIR/<store>/<name>"`).
5. Append the mounts in this order:
   - roots and root files, `-v $history_root/<x>:$AGENT_HOME/<x>`;
   - window `-v $HOME_DIR/<store>/<name>:$AGENT_HOME/<store>/<name>`;
   - credential files (rw), read-only config (`:ro`), shared directories (rw), all mapping `$HOME_DIR/<x>` to `$AGENT_HOME/<x>`.

Under the window, the mount-auth loop over `A_CONFIG_MOUNTS` is skipped. Token auth still sets its env and seeding. With `A_HISTORY=sync`, Task 3 replaces step 4 and the window mount; until then `sync` exits 2 with `codex window not wired yet`.

Refusals, in this order (spec §8):
1. agent has no window (`A_HISTORY` empty)
2. cwd is not a linked worktree
3. the claude name is longer than 200 bytes
4. a credential file is missing on the host (checked only when the credential files are mounted, i.e. not under `--token-auth`)

Reaper: it gains a third positional argument, `history_root`. After a successful `podman rm` (set `removed=1` at the existing `&& { ...; break; }`), it runs `rm -rf -- "$history_root"`, but only when the path matches `"${TMPDIR:-/tmp}"/harness-sandbox-root.*`. If rm never succeeded, it leaves the root in place and logs `reaper: kept <root>`.

- [ ] **Step 1: Write the failing tests.** Fixture: a temp dir holding:
  - `home/`;
  - `repo/` (`git init`, one commit) and `wt/` (`git -C repo worktree add ../wt`);
  - `tools/`, containing a `setsid` shim (`exit 0`) and a `podman` shim that writes `json.dumps(sys.argv[1:])` to `capture.json`, copied from `test_codex_release.py:65-68`.

  Env: `HOME`, `PATH=tools:…`, `TMPDIR`. For claude create `home/.claude/.credentials.json` (containing `x`) and add `HARNESS_SANDBOX_CLAUDE_TOKEN_FILE` (a file containing `x`); for pi add `HARNESS_SANDBOX_PI_PKG` (a fake package containing `dist/bundle/cli.js`). Further fixtures:
  - `plain_dir`: a directory that is not a git repo;
  - `long_wt`: a linked worktree whose absolute path is longer than 200 bytes.

  The helper `wrap(agent, *args, cwd=wt)` returns `(returncode, stderr, argv or None)`. Do not name it `run`: that would override `unittest.TestCase.run`.

```python
def test_claude_window_mounts(self):
    # wrap("claude","--history-window")  -- mount auth
    enc = re.sub(r"[^A-Za-z0-9]", "-", str(wt))
    root_pair = next(a for a in argv if a.endswith(f":{home}/.claude"))
    self.assertRegex(root_pair, re.escape(str(tmp)) + r"/harness-sandbox-root\.[^/]+/\.claude:")
    self.assertTrue(any(re.search(r"/harness-sandbox-root\.[^/]+/\.claude\.json:" + re.escape(f"{home}/.claude.json") + "$", a) for a in argv))
    win = f"{home}/.claude/projects/{enc}:{home}/.claude/projects/{enc}"
    self.assertIn(win, argv)
    self.assertLess(argv.index(root_pair), argv.index(win))          # Review Focus 1
    self.assertIn(f"{home}/.claude/.credentials.json:{home}/.claude/.credentials.json", argv)
    self.assertNotIn(f"{home}/.claude:{home}/.claude", argv)
    self.assertNotIn(f"{home}/.claude.json:{home}/.claude.json", argv)
    self.assertIn("SANDBOX_SEED_CONFIG=1", argv)
    self.assertIn("SANDBOX_HISTORY=window", argv)
    self.assertTrue((home / ".claude/projects" / enc).is_dir())
def test_claude_window_token_auth(self):
    # wrap("claude","--history-window","--token-auth")
    # -> root at ":/home/node/.claude", window f"{home}/.claude/projects/{enc}:/home/node/.claude/projects/{enc}",
    #    and no argument containing ".credentials.json"
def test_pi_window_mounts(self):
    # auth.json and settings.json exist under home/.pi/agent
    name = "--" + str(wt).lstrip("/").replace("/", "-") + "--"
    # expect, in this order: root ":{home}/.pi", window, f"{home}/.pi/agent/auth.json:{home}/.pi/agent/auth.json",
    # f"{home}/.pi/agent/settings.json:{home}/.pi/agent/settings.json:ro"; and no f"{home}/.pi:{home}/.pi"
def test_shared_mode_unchanged_but_labelled(self):
    # wrap("pi") -> argv contains f"{home}/.pi:{home}/.pi" and "SANDBOX_HISTORY=shared"; stderr contains "history=shared"
def test_refusals(self):
    for agent, args, cwd, why in [
        ("opencode", ["--history-window"], wt, "no window"),
        ("agy", ["--history-window"], wt, "no window"),
        ("bash", ["--history-window"], wt, "no window"),
        ("claude", ["--history-window"], repo, "linked worktree"),
        ("claude", ["--history-window"], plain_dir, "linked worktree"),
        ("claude", ["--history-window"], long_wt, "200"),                    # long_wt: a worktree whose path is > 200 bytes
        ("claude", ["--history-window"], wt, ".credentials.json"),           # with the fixture's file removed
        ("pi", ["--history-window"], wt, "auth.json"),                       # auth.json absent
    ]:
        rc, err, argv = self.wrap(agent, *args, cwd=cwd)
        self.assertEqual(rc, 2); self.assertIn(why, err); self.assertIsNone(argv)
def test_refusal_leaves_no_temp_root(self):              # Review Focus 5
    # after every refusal: list(tmp.glob("harness-sandbox-root.*")) == []
```

- [ ] **Step 2: Run them and see them fail.** `python3 -m unittest discover -s scripts/sandbox -p 'test_history_window.py' -v`. The `WrapperTests` fail: there is no `history=` in stderr, and the mounts are absent.
- [ ] **Step 3: Implement** the fields, flag, block, env, summary and reaper change described above. Add a comment block above the window block that states:
  - why the root is a host temp directory (podman creates missing mount parents root-owned; opencode EACCES, 2026-08-19);
  - why claude's `.credentials.json` may be mounted on its own (spec §6: claude replaces it, falls back to writing in place on `EBUSY`; read from claude 2.1.310 and measured on `~/.claude.json`, 2026-10-26), and the accepted empty-file-on-crash risk.
- [ ] **Step 4: Run them and see them pass.** Also run `make scripts-test`; the whole target passes, including `bash -n` and the existing `test_codex_release.py` wrapper test.
- [ ] **Step 5: Commit.** `git add scripts/sandbox/agent-in-podman.sh scripts/sandbox/test_history_window.py && git commit -m "sandbox: --history-window for claude and pi"`

---

### Task 3: wrapper — codex sync wiring

**Files:**
- Modify: `scripts/sandbox/agent-in-podman.sh` (the codex table arm, the window block, the reaper)
- Test: `scripts/sandbox/test_history_window.py` (`WrapperTests`)

**Interfaces:**
- Consumes: the Task 1 CLI, and the Task 2 window block and reaper arguments.
- Helper path: `history_helper="$(dirname "$(readlink -f "$0")")/history_window.py"`.
- Per-task directory: `${XDG_STATE_HOME:-$HOME_DIR/.local/state}/harness-sandbox/codex/<key>`, where `<key>` is `$HARNESS_TASK_ID`. If that is unset, it is `cwd-` followed by the first 32 hex characters of `printf %s "$WT" | sha256sum`.

For `A_HISTORY=sync`, the window block:
1. Creates the per-task directory and `$HOME_DIR/.codex/sessions` if they are missing.
2. Runs `python3 "$history_helper" codex-import --task-dir D --host-sessions "$HOME_DIR/.codex/sessions" --cwd "$WT"`. If that exits non-zero, the wrapper exits with the same code before `mktemp`, so Review Focus 5 still holds.
3. Mounts `-v D:$AGENT_HOME/.codex/sessions` in place of the dir window.

The reaper gains four more positional arguments: the helper, D, the host sessions directory and the cwd. All four are empty unless the agent uses sync.
- While it waits, every 10th 0.5 s poll (every 5 s) it runs `python3 helper codex-export … >>"$log" 2>&1`.
- After the `podman rm` loop, it runs one more export, and then the temp root removal from Task 2.

- [ ] **Step 1: Write the failing tests.** The fixture adds:
  - a fake codex bin, per `test_codex_release.py:60-75`;
  - `home/.codex/auth.json`, `home/.codex/config.toml`;
  - one host rollout for `wt` and one for another directory;
  - `XDG_STATE_HOME` and `HARNESS_TASK_ID=00…01`.

```python
def test_codex_window_mounts_and_imports(self):
    d = state / "harness-sandbox/codex" / task_id
    self.assertIn(f"{d}:{home}/.codex/sessions", argv)
    self.assertIn(f"{home}/.codex/thread-writer-locks:{home}/.codex/thread-writer-locks", argv)
    self.assertIn(f"{home}/.codex/auth.json:{home}/.codex/auth.json", argv)
    self.assertIn(f"{home}/.codex/config.toml:{home}/.codex/config.toml:ro", argv)
    self.assertEqual(sorted(p.name for p in d.rglob("rollout-*.jsonl")), ["rollout-a.jsonl"])  # only wt's
def test_codex_reaper_exports_after_exit(self):
    # real setsid (no shim); podman shim: on "run", write D/2026/10/26/rollout-new.jsonl (cwd=wt) then exit 0;
    # on "rm", exit 0. Poll up to 15 s for home/.codex/sessions/2026/10/26/rollout-new.jsonl,
    # then assert its st_ino equals D's copy, and that no harness-sandbox-root.* remains in TMPDIR.
```

- [ ] **Step 2: Run them and see them fail.** With Task 2's placeholder in place, codex exits 2 with `codex window not wired yet`.
- [ ] **Step 3: Implement** as specified. Remove the placeholder. Comment the export interval and why the final export follows `podman rm` (spec §7b).
- [ ] **Step 4: Run them and see them pass.** Then run `make scripts-test`; it passes.
- [ ] **Step 5: Commit.** `git add scripts/sandbox/agent-in-podman.sh scripts/sandbox/test_history_window.py && git commit -m "sandbox: --history-window for codex via a hardlink-synced sessions dir"`

---

### Task 4: probe.sh reports the history mode

**Files:**
- Modify: `scripts/sandbox/probe.sh:45-69`

**Interfaces:**
- Consumes: `SANDBOX_HISTORY` from Task 2.
- Produces:
  - **`config_history`**, added right after `config_agent`. Its value is `<declared SANDBOX_HISTORY or unset> | visible: <store>=<entries,> …`. The visible part is, for each existing store under `$HOME`:
    - claude and pi stores: their directory names;
    - codex store: the count of `rollout-*.jsonl` files, then the distinct first-line cwds, read with `python3 -c` (stdlib).
  - **`config_auth`**: when a matched config mount's mountinfo field 4 (the mount's root within its source filesystem) contains `/harness-sandbox-root.`, it reads `window-root (per-run host temp dir as config root; see config_history)` instead of `mount (…)`.

- [ ] **Step 1: Implement** both lines. Keep the existing comment style: say why the mode line comes first.
- [ ] **Step 2: Verify the syntax.** Run `bash -n scripts/sandbox/probe.sh`; it exits 0. The live check is in Task 6, item P.
- [ ] **Step 3: Commit.** `git commit -am "sandbox probe: config_history line; window roots are not mount auth"`

---

### Task 5: upstream probe — codex storage checks (spec §10, gate V5)

**Files:**
- Create: `scripts/sandbox/codex_storage_probe.py`. It runs inside the container, from the disposable HOME, and is stdlib only.
- Modify: `scripts/sandbox/check_codex_release.py:105-160` (`smoke_bundle`)
- Test: `scripts/sandbox/test_codex_release.py`

**Interfaces:**
- `codex_storage_probe.py --codex BIN` prints one JSON object, `{"storage": "ok"|"fail", "checks": {name: bool}, "observations": {name: value}}`. It exits 1 if any check is false.
- The app-server RPC goes over `BIN app-server --listen stdio://` with `CODEX_HOME` and `HOME` set to a fresh directory. The sequence, which is the one the peer measured on 2026-10-26:
  1. request `initialize` `{"clientInfo":{"name":"storage_probe","version":"0"}}`
  2. notification `{"method":"initialized"}`
  3. `thread/list` `{"limit":50}` and `{"limit":50,"cwd":WORK}`
  4. `thread/read` `{"threadId":ID,"includeTurns":true}`
  5. `thread/resume` `{"threadId":ID}`
- Synthetic rollouts use `"source":"cli"`, because `thread/list` defaults to interactive sources (`app-server-protocol/src/protocol/v2/thread.rs`, `ThreadListParams.source_kinds`).
  - **Legacy**: the three lines the peer used. The first is `session_meta` with `id`, `timestamp`, `cwd`, `originator`, `cli_version`, `source`, `model_provider`.
  - **Paginated**: take the first-line shape from codex-rs `thread-store/src/local/list_threads.rs`, test `list_paginated_threads_uses_sqlite_name_over_legacy_compatibility`, and its fixture helpers. A real host paginated first line has the top-level keys `ordinal, payload, timestamp, type` and `payload.history_mode`.
- Checks:
  - `list_fresh_db` — both formats, with and without the cwd filter.
  - `resume_fresh_db` — both formats.
  - `list_after_add` — a rollout written after `initialized` appears in the next `thread/list`.
  - `hardlink_inode_kept` — a hardlinked rollout keeps its inode through list and resume.
  - `features_default_off` — `BIN features list` shows `local_thread_store_compression` and `background_paginated_rollout_migration` disabled.
- Observation `non_date_depth`: a rollout at `sessions/harness/x/YYYY/MM/DD/` — `{"listed": bool, "readable": bool}`. This is spec §7a / V5 kept as a committed probe. It is not a check.
- `smoke_bundle`, for agent codex: copy the probe into `home`. Append `python3 "$HOME/codex_storage_probe.py" --codex "$SANDBOX_AGENT_BIN"` to the shim's command list. Raise `ValueError("codex storage probe failed: …")` unless the output contains `"storage": "ok"`.

- [ ] **Step 1: Write the failing test** in `test_codex_release.py`:

```python
def test_smoke_runs_storage_probe_for_codex(self):
    # patch check.subprocess.run to capture the wrapper invocation's env/cwd and return a CompletedProcess whose
    # stdout = 'codex-cli 0.164.0\ncodex-code-mode-host\n{"storage": "ok"}'; assert (directory/"home"/"codex_storage_probe.py").exists()
def test_smoke_fails_when_storage_probe_fails(self):
    # same, stdout without '"storage": "ok"' -> assertRaisesRegex(ValueError, "storage probe")
```

- [ ] **Step 2: Run it and see it fail.** `python3 -m unittest discover -s scripts/sandbox -p 'test_codex_release.py' -v`
- [ ] **Step 3: Implement** the probe and the `smoke_bundle` change.
- [ ] **Step 4: Run the unit tests and see them pass**, then run the real probe against the installed codex: `scripts/sandbox/build.sh` if the image is stale, then `python3 scripts/sandbox/check_codex_release.py --smoke`.
  - Expected: `"status": "ok"`, and a runtime probe containing `"storage": "ok"`.
  - If a check is false, codex fails gate V3's precondition. Stop and report the JSON. Do not adjust the check to make it pass.
- [ ] **Step 5: Commit.** `git add scripts/sandbox/codex_storage_probe.py scripts/sandbox/check_codex_release.py scripts/sandbox/test_codex_release.py && git commit -m "sandbox upstream: codex storage checks the history window depends on"`

---

### Task 6: live gates and end-to-end checks (spec §11, §12)

**Files:**
- Modify: `docs/superpowers/specs/2026-10-26-sandbox-history-window-design.md` §11. Record each result with the command that produced it and the date.
- Modify: `scripts/sandbox/agent-in-podman.sh`, only if a gate fails. The failing agent's `A_HISTORY` becomes empty, and its arm gets a comment naming the gate.

Setup:
- A scratch repo with a linked worktree under the session scratchpad: `git init`, one commit, `git worktree add`. Copy `scripts/sandbox/probe.sh` into the worktree.
- Invoke the wrapper symlinks **from this branch's checkout** with cwd = the scratch worktree and `HARNESS_TASK_ID` exported. The fleet's slots run the main checkout's wrapper, so they cannot test this branch.
- Take the agent flags from `scripts/agent_presets.py`: the `sandbox-<agent>` `agentArgs`, and the oneshot / resume templates.
- Each step costs one model turn per agent run. Codewords are fixed strings.

- [ ] **P (§12.1, Review Focus 1):** For each enabled agent, a window oneshot asks the agent to run `bash probe.sh` and print the output verbatim. Expect:
  - `config_history` starts with `window`, and its visible list holds only this cwd's directory (codex: only this cwd);
  - `config_auth` reads `window-root`;
  - `agent_config_mount_lines` shows no host config root.
- [ ] **V2 + §12.2 (claude, mount auth):** `claude-in-podman.sh --history-window … -p "Remember codeword PLUM. Reply OK."`, then `… --continue -p "Codeword?"` → PLUM. Then from the host, unsandboxed, in the worktree: `claude --continue -p "Codeword?"` → PLUM.
  - Pass = all three, plus host `~/.claude/.credentials.json` still present and non-empty (`test -s`; do not read it).
  - The `--token-auth` variant runs only if `~/.config/harness/sandbox-claude-token` holds a valid token. It was expired on 2026-10-26; if it still is, record it as unmeasured.
- [ ] **V7 + §12.2 (pi):** The same three steps with `pi-in-podman.sh --history-window`, and pi's resume flag from the presets. Also start pi interactively in the window once. If it shows a trust prompt that it does not show in shared mode, record that, and add `.pi/agent/trust.json` to `A_RO_CONFIG` (with a re-run of this item).
- [ ] **V3 / V4 (codex):** A window `exec` oneshot with a codeword, then a window `exec resume --last` → codeword (V3). After the wrapper exits, the host runs `codex exec resume --last` in the worktree → codeword, and `ls -i` shows one inode for the host and per-task names (V4).
  - Record whether the thread is `paginated`, with `sqlite3 "file:$HOME/.codex/state_5.sqlite?immutable=1" "select history_mode from threads where cwd='<wt>'"`.
  - Also check whether `~/.codex/session_index.jsonl` gained a row for the thread. If `resume --last` depends on it, that is a V3 failure.
- [ ] **V4b + §12.3 (codex):**
  1. Host `codex exec` in the worktree (codeword A).
  2. Window resume (codeword B, which must recall A).
  3. Host resume, which must recall A and B.
- [ ] **§12.3 (claude, pi):** shared → window → shared on one worktree, each step recalling the previous codeword.- [ ] **V6 (codex, read-only):** Grep codex-rs for callers of `revert_thread`, and record whether the TUI or `exec` can reach it. If they can, add the spec §7c limitation sentence to the README in Task 7.
- [ ] **§12.4:** Repeat Task 2's refusal list against the real wrapper, with real podman on PATH. Each one exits 2, and `podman ps -a` shows no new container.
- [ ] **Record and commit:** fill in §11 (a result column with date and command), then `git commit -am "spec: history window gate results"`. If a gate failed, the table change goes in the same commit, and `make scripts-test` must still pass.

---

### Task 7: README, parity walk, firing log

**Files:**
- Modify: `scripts/sandbox/README.md`:
  - the header flag list in "Use it from a runner", or wherever `--token-auth` is introduced;
  - "Security model → Authentication" (`:310-441`): a new bullet for the history window;
  - "Verifying the above" (`:442-463`): `config_history`;
  - "Regression and upstream release checks" (`:171-224`): the codex storage checks, plus the operator step "run `check_codex_release.py --smoke` after updating codex on a host".
- Modify: `.claude/skills/surface-parity-checklist/firing-log.md`

**Content the README must state** (each item from the spec section named):
- **What the window shows and hides, per agent, and the enabling.**
  - Per agent: the spec §6 table.
  - Per task with `--agent-arg --history-window`. No slot or preset path enables it (row 16).
  - A plain resume without the flag runs shared, and only `history=shared` shows it (§5).
- **claude's credential file is mounted on its own, why that works (the in-place fallback), the accepted empty-file-on-crash risk, and that V2 is re-run after a claude update** (§6).
- **Refusals** (§8).
- **The codex weaknesses** (§9): lock-directory thread ids are visible, and a first-line rewrite after linking is not caught. Add the revert limitation if V6 found revert reachable.
- **The per-task sync directory location, and that it is rebuilt every start** (§7b).

- [ ] **Step 1:** Edit the README as above. Read the whole file first.
- [ ] **Step 2: Walk S1–S6** in `.claude/skills/surface-parity-checklist/SKILL.md:367-425`, with a verdict for each number.
  - Expected: S1 n/a (no preset, row 16); S2 n/a; S3 done (Task 2/3 table fields); S4 n/a; S5 done (`SANDBOX_HISTORY` is not `HARNESS_*`, so it is set explicitly, not forwarded); S6 done (README + probe).
  - Append one entry to `firing-log.md` in its existing format, listing only the done/omitted items.
- [ ] **Step 3: Verify.** Run `make scripts-test`; it passes. Then `git grep -n -e "$HOME" -e '192\.168\.' -- scripts/sandbox docs/superpowers/plans/2026-10-26-sandbox-history-window.md docs/superpowers/specs/2026-10-26-sandbox-history-window-design.md` prints nothing.
- [ ] **Step 4: Commit.** `git commit -am "sandbox README: the history window"`
