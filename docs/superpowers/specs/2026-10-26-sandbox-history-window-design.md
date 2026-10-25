# Sandbox history window — a confined agent sees only its own conversation — Design

Date: 2026-10-26

A new opt-in mode for `scripts/sandbox/agent-in-podman.sh`. Read
`2026-08-18-generic-agent-sandbox-design.md` (and its Amendment 1) first: this
document changes only what the wrapper mounts for an agent's own state, and
assumes the agent table, the auth modes and the reaper described there.

---

## 1. Intent

Under the default **mount auth**, the wrapper bind-mounts each agent's whole
config directory read-write (`~/.claude` + `~/.claude.json`, `~/.codex`,
`~/.gemini`, `~/.pi`, opencode's four XDG directories). A confined agent can
therefore read every conversation the operator has ever had with that agent,
and write the settings and hooks the host's own agent later loads and runs.

The operator wants a mode in which (operator, 2026-10-26):

- inside the container, **only the designated conversation history is
  physically visible** — no other conversation, no host settings, no hooks;
- that history is **not a sandbox-private copy**: it lives in the host's
  normal store, so the host's own agent (and the harness) see and resume it
  as if the sandbox were not there;
- it covers the other agents too, not only claude.

The motivation is hardening ahead of need, not a present incident (operator,
2026-10-26). The precedent is `--firewall-proxy`: opt-in until it was stable
and kept being forgotten per task, then made the default (Amendment 1 of the
sandbox design). The same path is open here later; this design does not take
it.

The existing `--token-auth` already hides `~/.claude`, but only by running
from an ephemeral home: the conversation is thrown away with the container,
so it fails the second point (and resume does not work at all).

## 2. Non-goals

- **Hiding credentials.** The requirement is about conversations, settings and
  hooks. Under this mode a credential file is still bind-mounted where the
  agent needs one (§6); `--token-auth` remains the way to keep claude's
  permanent refresh token out of the container. (Author decision: the operator
  was asked whether credential exposure stays acceptable and did not object;
  see §4 row 6.)
- **Changing the default, in this stage.** Without the new flag the wrapper
  behaves exactly as today.
- **A way to enable it other than the per-task flag** (§5): no
  `sandbox-*-window` presets, and the wrapper does not remember a task's mode.
- **opencode.** Its sessions live in one SQLite database (`opencode.db`:
  `session` / `message` / `part` tables), so no filesystem view can show one
  conversation and hide the rest. The flag is refused for it (§8).
- **agy, in this stage.** Its layout (`antigravity-cli/conversations/<uuid>.db`
  plus an index `conversation_summaries.db` keyed by `workspace_uris`) has the
  same shape as codex's, but nothing about how it rebuilds that index has been
  measured. Refused until it is (§8, §11).
- **Projecting anything else the agent keeps**: claude's auto memory
  (`projects/<encoded repo root>/memory`), prompt history (`history.jsonl`),
  `/rewind` checkpoints (`file-history/`), user-level `CLAUDE.md`, skills,
  plugins, MCP configuration. Inside the window mode these start empty every
  run. Adding any of them later is a separate decision, because each one is
  something the operator chose to hide.

## 3. The model: a window, not a second store

For each agent the container gets:

1. an **ephemeral config root** — a fresh, empty directory at the path where
   the agent keeps its state, discarded with the container;
2. a **window**: the host directory holding *this task's* conversations,
   bind-mounted read-write at the same path inside that root;
3. the **credential file(s)** the agent authenticates with, bind-mounted
   singly (§6);
4. the **read-only config file(s)** the agent cannot start without (§6).

Because the window is the host's real directory (claude, pi) or hardlinks into
it (codex, §7), everything the agent writes there is in the host's normal
store the moment it is written. Two consequences follow, and both are
requirements, not side effects:

- **Switching modes does not break resume.** A task can be resumed with the
  flag and then without it, or the other way round, and `--continue` /
  `resume --last` finds the same conversation, because both modes read the
  same host directory. Without the flag the run simply sees the whole store
  again. (This is what rules out a sandbox-private persistent home: there the
  conversation would split by mode.)
- **The stream adapter's transcript replay needs no change.** It reads
  `~/.claude/projects/<encoded cwd>/` on the host
  (`runner/streamagent/claude_transcript.go`), which is exactly where the
  window writes.

"Designated" means **the conversations whose working directory is this task's
cwd** (author, §4 row 7). Every agent here already keys resume on the cwd, and
the runner gives each task its own worktree path, so this is one task's
conversations.

That holds only for a linked worktree, so the flag is refused when the cwd is
not one (the runner's `--no-worktree` mode; §8). There the cwd is the shared
checkout: the window would hold every task's conversations in it, and for
claude it would also be the directory holding the repo's auto memory
(`projects/<enc of repo root>/memory/`), which §2 keeps hidden.

## 4. Decisions taken

`Decided-by` is provenance. Rows marked `author` were proposed in a playback
the operator accepted without objection on 2026-10-26; that is weaker than a
choice the operator made, and a later reader may overturn them on evidence.

| # | Decision | Decided-by |
|---|---|---|
| 1 | The goal is that a sandboxed agent cannot read or write host conversations, settings or hooks | operator |
| 2 | It covers agents other than claude | operator |
| 3 | Inside: only the designated history is visible. Outside: that history is in the host's normal store, transparently | operator |
| 4 | Opt-in; the default stays mount auth over the whole config directory | operator |
| 5 | A window onto the host store (§3), not a sandbox-private persistent home | author |
| 6 | Credential files stay visible in the container; `--token-auth` is the way to hide claude's | author (asked, not answered) |
| 7 | "Designated" = the conversations of this task's cwd | author |
| 8 | claude and pi: the window is a bind mount of the host's per-cwd directory | author |
| 9 | codex: the window is a per-task directory kept in hardlink sync with the host store (§7), and ships only if the gates in §7d pass | author |
| 10 | opencode and agy: the flag exits 2 | author |
| 11 | The upstream compatibility probe gains storage checks for codex (§10) | author, prompted by the operator pointing at the probe |
| 12 | Flag name `--history-window` | author |
| 13 | Refused outside a linked worktree (`--no-worktree`) | author |
| 14 | codex: the host's `thread-writer-locks/` is shared with the container, so other threads' ids (not their content) are visible | author |
| 15 | A rollout the container creates is linked to the host only if its first line names this cwd; rewriting it after the link is an accepted residual (§9) | author |
| 16 | Enabled per task with `--agent-arg --history-window` only; no window presets and no remembered per-task mode, so a resume without the flag runs shared (§5) | operator |
| 17 | claude's window works under mount auth with `.credentials.json` mounted on its own. **Reversal:** this row first required `--token-auth`, on the measurement that claude replaces the file. Two findings moved it: the writer's in-place fallback on `EBUSY`, and a live single-file mount that took the write (§6). The empty-file-on-crash risk is accepted | operator chose to measure; author on the result |
| 18 | Every container runs with `--tz=local` (§7b), in every mode, since sandboxed codex in the shared mode writes UTC-filed rollouts into the host store too | author, on gate V4 |
| 19 | The reaper re-indexes the host's codex state DB for this cwd after the final export (§7b). **Departs from row 9 / §7d**, which said a failed V4 refuses codex. V4 failed without the re-index and passed with it, using codex's own scan-and-repair | author, on gate V4 |
| 20 | pi's `~/.pi/agent/bin` is mounted read-only. It holds a helper binary, not history or config | author, on gate V7 |

## 5. Interface

```
--history-window   opt in. Exits 2 for an agent whose table entry has no
                   window (opencode, agy, bash), and when the window cannot
                   be computed (§6, §7).
```

It is a wrapper control flag like `--token-auth`: consumed from the argv
stream, passed per task with `--agent-arg`; all three UIs have a per-task
args field (CLI `--agent-arg`, the TUI submit popup, the WebUI). A slot's
`--agent-args` reaches only the default profile, which on an `--agents all`
slot is the unsandboxed claude, so it is not a way to enable this.

A per-task arg is not carried by a plain resume: only `--exact` reuses the
recorded args (`server/task_handler.go`, `exactMismatch` and its callers),
while the agent profile is carried either way. Resuming a window task without
the flag therefore runs it shared, with nothing failing; the summary line's
`history=shared` is the only trace. Accepted (§4 row 16).

The flag composes with every
other flag. With `--token-auth` it gives claude's token auth a working resume
for the first time, since the conversation no longer lives in the ephemeral
home.

The `[agent-in-podman]` summary line gains `history=window` or
`history=shared`. `probe.sh` gains a `config_history` line right after
`config_agent`, and every later line is read against it as with `config_auth`.

## 6. Per-agent table entries

New agent-table fields, so no `case "$AGENT"` appears outside the table:

- `A_HISTORY` — `dir` (bind the host's per-cwd directory), `sync` (§7), or
  empty (no window: the flag exits 2);
- the function or pattern that maps the cwd to the host's per-cwd directory;
- `A_CRED_FILES` — host files bind-mounted read-write, singly, at the same
  path;
- `A_RO_CONFIG` — host files bind-mounted read-only, singly, at the same path.

A credential file missing on the host exits 2 (the agent is not logged in;
starting it would fail later and less clearly).

| agent | config root (ephemeral) | window | credential file | read-only config |
|---|---|---|---|---|
| claude | `~/.claude`, `~/.claude.json` | `~/.claude/projects/<enc>/`, where `<enc>` replaces every byte that is not an ASCII letter or digit with `-` | `~/.claude/.credentials.json` under mount auth; none under `--token-auth` | none |
| pi | `~/.pi` | `~/.pi/agent/sessions/--<cwd without its leading />--` with `/`, `\` and `:` replaced by `-` (pi 1.0.3 `dist/core/session-manager.js:290-294`) | `~/.pi/agent/auth.json` | `~/.pi/agent/settings.json` (it names the provider); the directory `~/.pi/agent/bin` (the `fd` binary pi fetched; without it pi warns `fd not found`, measured 2026-10-26) |
| codex | `~/.codex` | the per-task sync directory mounted at `~/.codex/sessions` (§7), plus the host's `~/.codex/thread-writer-locks/` read-write (§7b) | `~/.codex/auth.json` | `~/.codex/config.toml` |

**Why a single-file credential mount is safe for pi and codex.** A file bind
mount breaks when the agent replaces the file by rename (EBUSY), and the
breakage would surface only at the next token refresh, hours later. Both write
in place:

- codex 0.164.0 `login/src/auth/storage.rs:227-244`: `OpenOptions`
  `truncate(true).write(true).create(true)`, then `write_all`. Logout deletes
  the file, which fails on a bind mount; acceptable.
- pi 1.0.3 `dist/core/auth-storage.js`: `writeFileSync` on the path, under a
  `proper-lockfile` lock created beside it. That lock lands in the container's
  ephemeral `~/.pi/agent/`, so a host pi and a sandboxed pi refreshing at the
  same moment are not mutually excluded. Accepted and stated.

The host files agree with both readings: each `auth.json` was last written
long after it was created (codex: created 2026-09-03, written 2026-10-23; pi:
created 2026-09-17, written 17:47 the same day).

**claude replaces its credential file, but falls back to writing in place when
the replace fails, so a single-file mount works.**

- On the host, claude replaces the file. On 2026-10-26 `~/.claude/.credentials.json`
  and `~/.claude.json` each had a birth time within 1 ms of their modify time
  (`stat -c '%n inode=%i birth=%w modify=%y' ~/.claude/.credentials.json ~/.claude.json ~/.codex/auth.json ~/.pi/agent/auth.json`).
- In claude 2.1.310 the plaintext credential store writes through an atomic
  writer that renames a staged temp file over the target. When the rename fails
  with `EXDEV`, `EPERM`, `EEXIST` or `EBUSY`, it truncates the target and writes
  into it instead (its "in-place arm"). Read from the minified bundle inside
  `~/.local/bin/claude`: the store's `write` calls `Dn(path, text, 0o600)`,
  which delegates to that writer, and the error set is
  `new Set(["EXDEV","EPERM","EEXIST","EBUSY"])`.
- Measured on a live single-file mount the same day. A sandboxed oneshot under
  today's mount auth, where `~/.claude.json` is mounted on its own, left the
  host file's inode and birth time unchanged and advanced its modify time
  (02:55:25 → 03:09:43) while the JSON stayed valid. The container's write
  reached the host in place.

The measurement covers `~/.claude.json`. The reading of the bundle is what
covers `.credentials.json`. Two risks remain and are accepted:
- a crash between the truncate and the write leaves an empty credential file
  on the host, and the host's claude would have to log in again;
- a later claude release can change the writer. Gate V2 re-runs on a claude
  update (§10).

`--token-auth` composes with the window and needs no credential file. It was
briefly made mandatory for claude's window, because the replace was measured
before the fallback was found (§4 row 17). It was also found expired on
2026-10-26 (`401 OAuth access token is invalid`).

Under `--token-auth` claude's HOME is the image's `/home/node`. The window's
source stays the host's `~/.claude/projects/<enc>/`, mounted at
`/home/node/.claude/projects/<enc>/`. The image has no `/home/node/.claude`,
so the ephemeral config root is still needed there (see the parent-chain
constraint below).

Whether claude needs anything outside `projects/<enc>/` and
`.credentials.json` to authenticate and `--continue` (for example
`oauthAccount` in `~/.claude.json`, which is ephemeral here) is gate V2 (§11).

**claude's long-path rule.** Past 200 characters claude truncates `<enc>` and
appends an undocumented hash (`runner/streamagent/claude_transcript.go`
handles it by prefix match). The wrapper does not guess that hash: an `<enc>`
over 200 characters exits 2.

**The window directory is created on the host if missing**, as the host user,
before the container starts, so the mount source exists and is owned
correctly.

**Constraint on the ephemeral root.** Every mount target's parent chain inside
the container must exist and be writable by the agent's uid. podman creates
missing mount parents root-owned, and an agent that then `mkdir`s beside its
window aborts (measured for opencode, 2026-08-19: `EACCES: mkdir
'$HOME/.cache'`). How to satisfy this is the builder's call; one way is a
host-side temporary directory per config root, created by the wrapper with the
mount targets pre-created inside it (an empty directory for the window, an
empty file for each single-file mount), mounted over the config root, and
removed by the reaper after the container is gone.

The in-container launcher's first-run seeding (`SANDBOX_SEED_CONFIG`: the
onboarding flags, trust for the mounted roots, `skipDangerousModePermissionPrompt`)
applies to claude under the window, since `~/.claude.json` and
`~/.claude/settings.json` are ephemeral there as they are under token auth.

## 7. codex: a synced window

### 7a. Why codex cannot use a plain bind

codex writes rollouts to `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-<time>-<uuid>.jsonl`:
one file per conversation, partitioned by **date**, with the cwd only inside
the first line (`session_meta.payload.cwd`). There is no per-cwd directory to
bind. The session list comes from `state_5.sqlite` (`threads` table:
`rollout_path`, `cwd`, recency, archive state).

Placing the per-task directory under the host store at a non-date depth
(`sessions/harness/<task>/YYYY/MM/DD/`) does not work: such a rollout is
readable and resumable by thread id but never appears in a listing, with or
without a cwd filter (measured by a peer agent, codex 0.164.0, app-server RPC
against a disposable `CODEX_HOME`, 2026-10-26; its scripts were not kept, so
gate V5 reproduces it as a committed probe). `resume --last` works from a
listing, so the host would never find such a conversation.

### 7b. The sync

- **Per-task directory**: `${XDG_STATE_HOME:-$HOME/.local/state}/harness-sandbox/codex/<task-id>/`,
  outside `~/.codex` so the host never lists anything twice. Mounted at the
  container's `~/.codex/sessions`.
- **At container start, host → task.** Empty the per-task directory of its
  previous links, then for every host rollout whose first line's
  `session_meta.payload.cwd` equals this cwd, create a hardlink at the same
  `YYYY/MM/DD/<name>` path inside it. The host is the source of truth; the
  per-task directory is rebuilt from it every time, so any divergence from a
  previous run is discarded rather than reconciled.
- **During the run and after exit, task → host.** The reaper, which already
  polls for the wrapper's death, also links every file in the per-task
  directory that has no counterpart on the host to the same relative path
  under `~/.codex/sessions/`, and does it once more after the container is
  removed. It links a file only when its first line's
  `session_meta.payload.cwd` is this cwd, so a container cannot plant a
  conversation in another task's history by naming that task's cwd (§9 for
  what this does not close). It **never overwrites or deletes** a host file:
  an existing host path with a different inode is left alone and logged.
  It also skips every path the last import took from the host, recorded in a
  manifest beside the per-task directory (`<task-id>.imported`, outside the
  container's reach). A rollout the host archived or deleted since is
  therefore not put back (found in review: "no counterpart on the host" alone
  cannot tell "not yet linked" from "removed by the host").
- **Neither direction follows a symlink.** The container can write the
  per-task directory, so the walk takes regular files only (`os.walk` without
  following links, `lstat`), and the start-of-run clearing removes the
  directory with `shutil.rmtree`, which removes a link and not its target. A
  directory symlink planted there had led the clearing into the host store and
  deleted rollouts of every cwd (found in review, reproduced, fixed before
  landing).
- **The only copy of a rollout is never deleted.** The clearing keeps any
  rollout the host neither holds (same inode) nor gave to the last import. If a
  codex release moved the cwd out of the first line, export would link nothing,
  and clearing would otherwise delete every conversation made in the window,
  with nothing to notice it (the storage probe writes its own rollouts). The
  kept file is logged on every clearing.
- **The per-task directory is removed after the run** (and its manifest), by
  the reaper after the re-index, unless it holds such an only copy. Kept, its
  hardlinks would hold on to conversations the host has since deleted, and one
  directory would accumulate per task. So the import's "export first" covers
  only a run whose reaper never finished.
- **Then the host's codex re-indexes this cwd** (added after gate V4 failed
  without it). `codex exec resume --last` asks the state DB first and treats
  any row for the cwd as authoritative; it scans rollout files only when the
  DB has none (codex 0.164.0 `exec/src/lib.rs`, `resolve_resume_thread_id`).
  A thread the container created reaches the host only as a linked file, so
  the host kept resuming an older thread. After its final export the reaper
  starts the host's `codex app-server` (with cwd `/`, never the
  container-writable worktree) and issues one `thread/list` for this cwd with
  `useStateDbOnly: false` and every source kind. That is codex's own
  scan-and-repair, and it adds the linked rollouts to the host DB. The listing
  is not repeated during the run, so a host `resume --last` issued while a
  window task is still running does not yet see that task's new thread.
- **The container runs in the host's timezone** (`podman run --tz=local`, every
  mode, when the host has `/etc/localtime`; without one the container stays
  UTC rather than failing to start). codex names rollouts and files them under `YYYY/MM/DD` by local time.
  In the image's UTC a rollout written at 03:30 JST was filed as
  `2026/10/25/…T18-30-08` and sorted behind the host's.
- **Writer locks are the host's.** codex serialises writers to one thread
  with `flock` on `$CODEX_HOME/thread-writer-locks/<thread-id>.lock`
  (`rollout/src/writer_lock.rs`, `File::try_lock`). In an ephemeral root a host
  `codex resume` and a sandboxed task could append to the same hardlinked
  rollout at once and interleave its JSONL; shared mode is safe only because
  both sides see one lock directory. So the host's `thread-writer-locks/` is
  bind-mounted read-write at the same path. Its file names are the ids of
  every thread on the host (§9).
- **The container's databases** — `state_5.sqlite` and the history projection
  `thread_history_1.sqlite` (which records how far into each rollout it has
  projected) — live in the ephemeral root and are rebuilt from the linked
  rollouts on each start. That a fresh database lists and resumes rollouts it
  has never seen, and that a listing on an existing database picks up a newly
  appeared rollout, was measured by the same peer for **legacy-format**
  rollouts only (§7d). The host's own projection of a thread the container
  appended to is behind by those bytes until the host next opens it; whether
  it catches up correctly is gate V4b.

Writes reach both sides immediately because both names are one inode, and the
rollout recorder opens with `append(true)` (codex 0.164.0
`rollout/src/recorder.rs:1789`, `:2079`).

### 7c. What can replace a rollout file, and why it is tolerable today

Read at tag `rust-v0.164.0` (`6aaac694`):

| path | effect | default |
|---|---|---|
| cold compression (`rollout/src/compression.rs`: files unmodified for 7 days become `.jsonl.zst`, original removed) | the two names stop sharing an inode | feature `local_thread_store_compression`, `Stage::UnderDevelopment`, `default_enabled: false` (`features/src/lib.rs:1330-1334`) |
| legacy → paginated migration (`thread-store/src/local/rollout_migration.rs`, staged file renamed over the original) | same | feature `background_paginated_rollout_migration`, under development, default off (`:1342-1346`) |
| archive / unarchive (`archive_thread.rs`, `unarchive_thread.rs`) | moves the file out of `sessions/` | explicit user action |
| revert of an unloaded paginated thread (`revert_thread.rs:15-18`) | writes a **new** rollout and moves the thread's current-file pointer **only in SQLite** | reachable from the interactive TUI's backtrack (`tui/src/app/event_dispatch.rs:797` → `thread/revert`), not from `exec` (V6) |

The first two are off by default, and the host's `config.toml` enables
neither (`grep -c 'local_thread_store_compression\|background_paginated_rollout_migration' ~/.codex/config.toml`
printed 0 on 2026-10-26). Because the
per-task directory is rebuilt from the host at every start (§7b), a broken
link costs at most the remainder of one run.

Revert is the one path a sync cannot follow, because the fact it changes is in
a database the other side does not share. V6 found it reachable, so it is a
recorded limitation: after a backtrack in an interactive codex session inside
the window, the host may continue the pre-revert file.

### 7d. Gates before codex ships

**Paginated is the current format.** On this host 77 of 155 codex threads are
`paginated`, all created by 0.147.0 or later; every legacy thread predates
2026-09-18
(`sqlite3 "file:$HOME/.codex/state_5.sqlite?immutable=1" "select history_mode, count(*), min(cli_version), max(cli_version), max(date(created_at,'unixepoch')) from threads group by 1"`).
The peer's measurements used hand-written legacy rollouts. So V3–V5 must be
repeated with paginated rollouts. If V3, V4 or V4b fails, codex's table entry
gets no window and the flag exits 2 for it, as for opencode. (V4 failed as
first built, and passes with the re-index and `--tz=local` of §7b: §4 rows
18–19, §11 results.)

## 8. Refusals

`--history-window` exits 2, naming the reason, for: opencode, agy, bash (no
window in the table); claude if V2 fails; a cwd that is
not a linked worktree (the wrapper's existing `gitdir != common` test, §3); a
claude `<enc>` over 200 characters; a missing credential file; for codex, a
per-task directory on a different filesystem from `~/.codex` (a hardlink
cannot cross one); and a cwd the
wrapper cannot resolve to an absolute path. It never falls back to the shared mode,
for the reason every other wrapper refusal gives: a task that silently ran in
the other mode looks identical in every listing.

## 9. Security notes

- The read-only config files are readable in the container. codex's
  `config.toml` may name MCP servers and their environment; pi's
  `settings.json` names a provider. Neither is a conversation, setting the
  host executes from the container's writes, or hook, so they are within the
  requirement; listing them per agent keeps the exposure explicit.
- The window is read-write by design. A confined agent can rewrite or delete
  its own task's past conversations on the host. For claude and pi it cannot
  reach another task's: the window is one directory.
- codex is weaker in two ways. The shared lock directory shows the ids of
  every host thread (names only; no rollout outside the window is mounted).
  And a rollout is one inode on both sides, so after the reaper has linked a
  file the container can still rewrite its first line to name another cwd;
  the link-time check (§7b) closes the plain case, not that one. Both are
  accepted under the kit's existing threat model: the container limits
  accidental blast radius and is not a boundary against an agent that sets
  out to cross it (`scripts/sandbox/README.md`, security model).
- claude reads `.claude/settings.json` inside the worktree (the runner injects
  the inbox hook there), as in every other mode. That file is the task's, not
  the host's.

## 10. Upstream probe

`check_agent_releases.py --smoke` today runs `--version` (and codex's helper)
in an empty home with `--network=none`. For codex it additionally runs the
storage checks below against the candidate release, using app-server RPC in a
disposable `CODEX_HOME`, synthetic rollouts in both formats, no credentials and
no model turn:

1. a fresh database lists and resumes a rollout at `sessions/YYYY/MM/DD/`, with
   and without a cwd filter;
2. a rollout added after startup appears in the next listing;
3. a hardlinked rollout keeps its inode through list and resume;
4. `local_thread_store_compression` and `background_paginated_rollout_migration`
   are still off by default;
5. the reaper's re-index request (`history_window.reindex_params`, the same
   object) adds a rollout that appeared while no server ran to a DB that
   already has rows for its cwd, so a DB-only listing then shows it.

A failure fails the probe, so a codex release that changes the storage this
design depends on is caught where the other release incompatibilities are,
instead of in a running task. `check_codex_release.py --smoke` runs the same
checks against the installed binary; the probe has no schedule, so running it
after updating codex on a host is the operator's step, and the README says so.

claude and pi have no offline check of their directory naming here (both need
a credential to create a session); the builder adds one if a credential-free
way exists.

## 11. Verification gates

Each is measured before the dependent part ships, through the wrapper on a real
runner slot unless stated, and recorded with the command that produced it.

| # | question | blocks |
|---|---|---|
| V1 | Does claude's credential write survive a single-file mount? **Answered 2026-10-26: yes**: it replaces the file, falls back to writing in place on `EBUSY`, and a live single-file mount took the write (§6) | — (answered) |
| V2 | Under mount auth with only `.credentials.json` mounted and an ephemeral `~/.claude.json`, does claude authenticate and `--continue` from the window alone? Re-run after a claude update | claude |
| V3 | codex: a fresh database lists and resumes a **paginated** rollout; CLI `resume --last` and `exec resume --last` pick the newest one for this cwd | codex |
| V4 | codex: the host lists and resumes a rollout the container created, after the reaper linked it | codex |
| V4b | codex: a host-originated paginated thread, resumed shared → window → shared, ends with the host resuming it with the window run's turns present (the host's history projection catches up from its stored offset) | codex |
| V5 | codex: the §7a non-date-depth result, as a committed probe | — (evidence) |
| V6 | codex: is thread revert reachable from an interactive or exec codex task? | the §7c limitation text |
| V7 | pi: `--continue` works with only the session directory, `auth.json` and `settings.json` from the host | pi |

### Results (2026-10-26)

Measured on a host running claude 2.1.310, pi 1.0.3 and codex 0.164.0. The
branch's wrapper was invoked directly, with the cwd set to a linked worktree
of a scratch repo and the `HARNESS_*` env removed. The per-agent symlink was
run as `<agent>-in-podman.sh --omit-harness-cli --no-proxy-log
--history-window <the preset's agentArgs and oneshot/resume argv>`. Every
codeword step is one model turn.

| gate | result |
|---|---|
| V1 | answered by measurement and reading (§6) |
| V2 | **pass.** Window oneshot, then window `--continue`, then host `claude --continue`: all recalled the codeword. `.credentials.json` kept its inode and size. Shared → window resume also recalled |
| V3 | **pass**, after rows 18 and 19. A window `exec resume --last` continued a host-originated **paginated** thread, and also the newest container-created one |
| V4 | **failed, then passed.** Without the re-index, the host's `exec resume --last` continued an older thread: the host DB had no row for the container's thread. A second cause was the UTC filing (row 18). With `--tz=local` and the reaper's re-index, the host resumed the container's thread |
| V4b | **pass.** Host `exec` (A), then window resume (recalled A, added B), then host resume recalled A and B. Both names share one inode |
| V5 | **reproduced.** `check_codex_release.py --smoke` reports `non_date_depth: {"listed": false, "readable": true}`; all five storage checks are true, with synthetic paginated rollouts too |
| V6 | **reachable** from the TUI's backtrack, not from `exec` (§7c) |
| V7 | **pass.** Window, then host `pi --continue`, then shared, then window, each recalled the previous codeword. Interactive start showed no trust prompt in either mode. The window first showed `fd not found` (row 20) |
| §12.1 | `probe.sh` inside each window. claude: `-wt(host)` plus an unmarked repo-root dir that claude creates in the ephemeral root for its auto memory. pi: its one session dir `(host)` of the host's 11. codex: 6 rollouts, all of this cwd, of the host's 259 |
| §12.4 | the refusal cases against real podman exited 2 with their reasons; no container started and no temp root was left |

## 12. Testing

1. Per enabled agent, a oneshot on a sandbox slot with `--history-window`,
   then from inside a second task: the agent's config root lists exactly the
   window, the credential file and the read-only config; the host's other
   conversations, settings and hooks are absent.
2. Transparency: after that oneshot, the host's own agent resumes the
   conversation in the worktree (claude `--continue`, pi `--continue`, codex
   `resume --last`), and for claude a stream task resumed with
   `--resume-conversation` replays it.
3. Mode switch both ways on one task: shared → window → shared, each step a
   `--resume-conversation` that recalls a codeword from the previous step.
4. Every refusal in §8 exits 2 with its reason, and no container starts.
5. `make scripts-test` covers the cwd → window path mapping for claude and pi
   and the codex sync's link rules (start rebuild, task → host linking, the
   never-overwrite rule) on fixtures, offline.
6. `probe.sh` reports `config_history=window` and the measured visible
   conversation directories.
