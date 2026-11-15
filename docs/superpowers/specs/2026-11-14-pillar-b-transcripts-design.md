# Pillar B — fetching agent transcripts

Date: 2026-11-14. Line references are to `main` at `8cce5c42`.

Implements pillar B of `2026-11-12-run-modes-and-records-grand-design.md`
(cited as **GD**, its rows as G1…G46). Where this spec reverses a GD row, its
Decisions table names the row and what moved it (GD, "Left to the pillar spec
authors").

## Why

Pillar A3 (retiring oneshot) removes `task.<id>.log`, the only log a oneshot
run leaves on the server. For an agent, the agent's own transcript replaces it:
the conversation, tool calls included, as the agent stored it. Pillar B lands
before A3 so that no run's answer becomes unreadable in between (operator,
2026-11-14; pillar A spec §A3). The broader motive is GD's third: records and
logs readable remotely, without logging in to the runner host (operator,
2026-11-12).

## Goals

- `transcript ls <task>` lists the transcripts the agents stored for the
  task's working directory; `transcript ls <task> <session>` lists the files
  one of them is made of. `transcript get <task> <session> [<file>]` returns
  one file as the agent stored it, or the whole session as a tar.
- Every agent the fleet runs that keeps a transcript: claude, codex, agy,
  opencode and pi, with their sandboxed variants (GD G32, operator).
- On all three surfaces: CLI, TUI and WebUI (operator feedback: features span
  all three UIs).
- On Linux and Windows runners alike (operator, 2026-11-14): the fleet runs
  claude, codex and opencode on Windows too.

## Non-goals

- **Rendering.** The bytes are returned as stored; reading them is the
  client's business (operator, 2026-11-14: "取り寄せだけ"). How A3's `logs`
  renders a transcript is A3's work, not B's. Note for A3: the existing
  decoders (`runner/agentlog/agentlog.go:306-461`) read the print-mode stdout
  formats, not the stored ones. codex's rollout file starts with
  `session_meta`, and claude's stored lines carry an envelope its stdout
  lines do not.
- **A server-side copy (settles GD O5: none).** Every fetch goes to the runner;
  a transcript is unreadable while its runner host is down (operator,
  2026-11-14). A copy made when a run ends can be added later without changing
  what B builds.
- **A per-runner on/off flag.** Reverses GD G10's "a runner flag turns the
  feature on or off" (operator, 2026-11-14). The gate below decides who reads
  and the directory decides what; the only thing left for a flag was a blanket
  "nothing leaves this host", which nothing in the fleet needs. It can be
  added later.
- Deleting transcripts. A transcript is the agent's resume state on the runner
  host, not a harness record (GD G20).

## Anti-goal

A principal receiving a conversation it was not granted: one without
`transcript_read` on the task, or, for a session recorded in a shared
directory, one that could not already read it through a command (B-D2).

## Facts

Measured 2026-11-14 on the host1 runner host by reading file names, table
schemas and JSON keys, not conversation content. Agent versions: claude
2.1.325, codex-cli 0.165.0, agy 1.5.8, opencode 1.18.18, pi 1.4.0.

- **B-F1 — where each agent stores a session, and how it names the working
  directory:**

  | agent | storage | one session = one file | working directory recorded as |
  |---|---|---|---|
  | claude | `$CLAUDE_CONFIG_DIR` or `~/.claude`, `projects/<encoded cwd>/<session-id>.jsonl` | yes | `cwd` on every line |
  | codex | `~/.codex/sessions/YYYY/MM/DD/rollout-<time>-<session-id>.jsonl` (by date) | yes | first line `session_meta`, `payload.cwd` |
  | pi | `~/.pi/agent/sessions/--<encoded cwd>--/<time>_<id>.jsonl` | yes | first line `type: session`, `cwd` |
  | agy | `~/.gemini/antigravity-cli/conversations/<conversation-id>.db` (SQLite, one per conversation) | yes | `conversation_summaries.db`, table `conversation_summaries`, column `workspace_uris`, keyed by `conversation_id` |
  | opencode | `~/.local/share/opencode/opencode.db` (SQLite, **every session of every project in one file**) | **no** | table `session`, column `directory` |

  opencode's per-session JSON files under `storage/` date from an older
  version (last written 2026-08-19); the current version writes the database.
  `opencode export [sessionID]` writes one session as JSON (its `--help`).
- **B-F1a — a session is more than its main file.** Beside claude's
  `<session-id>.jsonl` are `<session-id>/subagents/` (the subagents'
  conversations) and `<session-id>/tool-results/` (large tool outputs kept
  apart). agy keeps `brain/<conversation-id>/` beside the `.db`, and a live
  `.db` has `-wal` and `-shm` files, without which recent writes are missing.
  (Found by the builder check, by file names only.) Windows was not measured:
  the plan measures each agent's store on a Windows runner first.
- **B-F2 — claude truncates a long directory name.** Past 200 characters it
  cuts the encoded name and appends an undocumented hash
  (`runner/streamagent/claude_transcript.go:71-108`, which then matches on the
  prefix). For a worktree path that long, a name prefix can be shared by
  several tasks' directories. Every format in B-F1 records its working
  directory inside the file or database, so a match can be checked there.
- **B-F3 — a worktree task's directory is the task's alone.**
  `<repo>/.harness-worktrees/<task-id>`, kept across resumes. A task on a
  `--no-worktree` runner runs in the repo itself (`runner/session.go`, the
  no-worktree branch of `handleOpenExec`), where other tasks' and the
  operator's sessions also live.
- **B-F4 — sandboxed profiles store in the host's normal store** except under
  token auth. Mount auth bind-mounts the host config dir; inject auth gives the
  container a per-run config dir "holding the host's conversation store"
  (`scripts/sandbox/claude-in-podman.sh`, the `--inject-auth` help text);
  `--history-window` shows the container only this task's conversations and
  keeps them in the host store. Under `--token-auth` the home is ephemeral and
  nothing persists. The plan confirms each of these by running one sandboxed
  session per agent and finding its file.
- **B-F5 — the git queries are the shape to follow.** `git log` / `git file`
  are read-only queries the server forwards to the task's runner, which
  resolves them against the task and streams the result back
  (`runner/git_query.go`, `server/git_query.go`). A transcript fetch is the same kind of
  thing: resolved by the runner from the task, read-only, streamed.
- **B-F6 — what can already read these files.** A principal holding
  `exec_run`, `exec_cowrite` or `exec_control` on any task on a runner host can
  run commands there as the runner's user, and so read every transcript on
  that host. A principal holding only `exec_view` cannot: it sees the screens
  of the tasks in its scope.
- **B-F7 — a transcript holds what no screen showed.** That includes the full
  text of files the agent read (tool results), the large outputs kept apart
  (B-F1a), the subagents' conversations, every past run, and other agents run
  in the same directory. `exec_view` shows only what reached the terminal:
  the live screen, the replay ring and A1's PTY record.

Measured for the plan (Task 0), 2026-11-14, host1:

- **B-F8 — what moves each store.**
  - claude: `CLAUDE_CONFIG_DIR`, as in B-F1.
  - codex: `CODEX_HOME`, default `~/.codex` (the name is in its binary;
    `--help` names only `~/.codex/config.toml`).
  - pi: `PI_CODING_AGENT_SESSION_DIR`, else `$PI_CODING_AGENT_DIR/sessions`,
    else `~/.pi/agent/sessions`, from its `docs/environment-variables.md`.
    `--session-dir` overrides both per run.
  - opencode is read through its own CLI, which resolves its store itself.
  - agy: `~/.gemini/antigravity-cli`. Its binary names `ANTIGRAVITY_*_DIR`
    variables whose meaning is undocumented, so the runner uses the default.

  None of these was confirmed by starting the agent with the variable set.
- **B-F9 — opencode lists itself.** `opencode session list --format json`,
  run in a directory of the project, returns rows `id, directory, created,
  updated, projectId, title`. In a directory of no project it prints nothing.
- **B-F10 — agy's conversation→workspace index.** `history.jsonl` lines carry
  `conversationId` and `workspace`, but cover 230 of the 263 conversations on
  disk: it is a typed-prompt history, rotated at 2000 lines.
  `conversation_summaries.db` (`workspace_uris`) covers 254 of 263. Counted
  with Python over ids only.
- **B-F11 — the Windows tasks were unreachable for measuring.** Every listed
  Windows task's `assigned_to` names a runner identity no connected runner
  has: the Windows slots are up, under other identities. Reaching one would
  need a new session there, which this session's grant (no `spawn`) cannot
  open. host1 slots keep their identity across a systemd restart: a task
  placed before the restart still takes an `exec`. For B this means **a task
  whose runner re-registered under a new identity answers `runner_offline`**
  (B-D6), though its files are still on the host. The Windows and sandbox
  measurements move to the plan's live checks.

## Approaches

1. **A read-only query on the git-query path (chosen, operator 2026-11-14).**
   New verbs `transcript ls` and `transcript get`. The server checks the gate
   and forwards to the task's runner; the runner resolves the agent's storage
   and streams the answer back through the server. It keeps file transfer's
   "never outside the worktree" rule intact. The cost is that every byte
   crosses the server, which is fine at the measured scale (MB).
2. Extending file transfer with a "transcript" source. This was not chosen.
   It would give direct routes and ranges, but it would mix a read outside the
   worktree and a different gate into code built on `file_read` and worktree
   confinement.

## Decisions

**operator** = chosen by the human in conversation; **GD** = inherited;
**this spec** = the author's call, to check.

| # | Decision | Decided by |
|---|---|---|
| B-D1 | **Scope of a fetch: the task's working directory, for every agent.** `transcript ls <task>` lists every session that any agent the runner knows (its profiles) stored with a recorded working directory (B-F1's column) equal to the task's. That directory is the worktree, or the repo for a `--no-worktree` task. This covers every agent, not just the task's latest profile: a task's profile is set again on each open, so a task resumed under another agent still lists its earlier agent's sessions, and an agent started inside a shell task's worktree is listed too. Each row gives the agent, its session id, the times the session was created and last written, and its size. The times are what A3's `logs` uses to pick a run's session: the session created after that run's start. Runs from before B are included: their files carry the same directory. **Reverses GD G18**, which bound transcripts to runs by a session id recorded at run start and returned only bound ones. G18 protected against returning another task's conversation, and B-D2 protects against that by directory and gate instead. A row is not labelled with a run: the times can be read against the task's run times | operator (no strict per-run exclusion; whatever a shell on the task could read anyway), this spec (the directory match, the row fields) |
| B-D2 | **Gate, decided per session by the directory it was recorded in.** The runner sorts each session into one of two kinds. A session recorded in the task's worktree path, `<repo>/.harness-worktrees/<task-id>`, is the task's own. That path is unique to the task and computable whether or not the worktree still exists, so the server needs no persisted classification (the server's `WorktreeDir` is not in the WAL and is cleared on resume). A session recorded in the repo itself is shared: that is how a `--no-worktree` task's sessions are recorded, and other tasks' and the operator's land there too. A session of the task's own kind needs **`transcript_read`** on the task, within its scope (B-D10). **A shared one needs `exec_run` as well.** The server drops shared rows for a caller without both, and refuses `get` for them, because only a principal that can already read them through a command (B-F6) may receive them. A session someone else started inside the task's worktree, such as a parent's `exec` or a human in that directory, counts as the task's: it was run in the task's directory. This is accepted | operator (keep only "a view-only principal gets nothing it cannot see"), this spec (per-session classification, sessions others started in the worktree) |
| B-D3 | **The match is checked inside the file, not only by the name.** A name or path match only finds candidates. A session is listed when the directory it records (B-F1) equals the task's. This closes B-F2's truncation case and codex's by-date layout | this spec |
| B-D4 | **opencode is fetched by `opencode export <session-id>`, run by the runner**, because its store holds every session in one database (B-F1) and returning that file would break the anti-goal (operator, 2026-11-14). Listing runs `opencode session list --format json` (B-F9), so the runner needs no SQLite reader for it. agy's per-conversation `.db` is returned as the file it is; the client opens it with SQLite | operator (export for opencode), this spec (agy as a file) |
| B-D5 | **The agent's state directory is resolved in the agent's environment**, as GD's pillar B note says: `$CLAUDE_CONFIG_DIR` for claude, and for each agent the variable or flag it reads (pi's `--session-dir`, for one). The plan names each agent's from its documentation, never from where the runner's own home would put it. A sandboxed profile resolves to the host store B-F4 describes. A run under token auth leaves nothing, so its list is empty: which runs used token auth is set per run through the task's args, and the runner does not track it | GD, this spec |
| B-D6 | **The runner is the task's assigned runner, as for the git queries** (`server/git_query.go:38-40`). A task whose runner is not connected answers `runner_offline`, naming the runner; no server copy exists (Non-goals). A resumed task waiting in the queue has no assigned runner and answers `runner_offline` until it is placed again. This is accepted as git queries accept it | operator (no copy), this spec (the runner choice, the queued gap) |
| B-D8 | **A session is a set of files, fetched one by one or whole** (operator, 2026-11-14). `transcript ls <task> <session>` lists the files the runner counts as that session (B-F1a), by a name relative to the session, with sizes. `transcript get <task> <session> <file>` returns that one file as stored. Without `<file>` it returns the whole set as a tar. A `<file>` is accepted only if it is a name that listing produced. The runner resolves it, so no path from the client is ever opened. An opencode session is the single JSON `export` writes | operator (both forms), this spec (tar for the whole, names from the listing only) |
| B-D10 | **A new capability bit, `transcript_read`, gates reading transcripts**, bounded by the task's scope like every bit (operator, 2026-11-14). **Reverses GD G34's reuse of `exec_view` for transcripts.** G34 reasoned that a transcript shows what that bit already shows. B-F7 shows it does not: a transcript carries what never reached a screen, and an agent granted `exec_view` to watch screens would get all of it. A1's PTY record stays on `exec_view`: it holds only what reached the terminal. Held by the operator; an agent has it only when granted (`caps_child = caps_parent ∩ delegate_parent ∩ requested`). It is the 29th of 32 bits, leaving 3. Of pillar S's and pillar C's planned bits, the first is added with 3 left; the second is proposed with 2 left, which triggers the `u64` widening first (operator memory: widen at two left; never merge gates to save bits). The capability touch-list (descriptions, tiers, completeness tests, every surface's caps lists) applies | operator (own bit), this spec (scope, A1 unchanged, bit count) |
| B-D9 | **Windows runners are in scope** (operator, 2026-11-14). Before writing code, the plan measures on a Windows runner each agent's store, claude's directory-name encoding, and `opencode export`. B-D3's directory comparison treats paths as the runner's OS does: separators, and case on Windows. E2E runs on a Windows runner too | operator (in scope), this spec (measure first, OS-native comparison) |
| B-D7 | **Surfaces.** CLI: `transcript ls <task> [<session>]` and `transcript get <task> <session> [<file>] [-o FILE]`. TUI: the `d` popup lists the task's transcripts with time and size under the records, and the command line takes the CLI verbs (`get` needs `-o`, as `record get` does). WebUI: the task sheet lists them; opening one lists its files, each with a download button, plus one for the whole session. The plan walks `surface-parity-checklist` | this spec |

## Wire

- Task control: `transcript_list` (task id, optional session id: sessions or
  one session's files) and `transcript_get` (task id, session id, optional
  file name), each answered with a status and a stream id. The server writes
  the list body (rows as B-D1) or the file's bytes into that stream (GD G13:
  never inside one control message). Statuses cover ok, not found, not
  permitted, `runner_offline`, no such session, and an agent without
  transcripts (a shell profile).
- Server → runner: the same two as a runner request, and the runner's reply on
  a stream. **Runner-decoded**: the plan runs `scripts/wire-skew-check.sh`
  and states the restart order. Restart runners through their systemd units,
  not `restart.py`: if the change touches `scripts/agent_presets.py`,
  `restart.py` would run the runners with the old argv.
- The agent → storage facts (B-F1, B-D5) live on the runner, per profile, as
  `logFormat` does today. Whether a profile's agent keeps transcripts at all is
  the runner's to say.

## Persistence

Nothing new is persisted on the server: no copy (Non-goals), and no session
binding (B-D1).

## Testing

- Unit, runner, per agent: given a fixture store with sessions in the task's
  directory, in another directory, and (claude) under a truncated name shared
  with another directory, `ls` returns exactly this directory's sessions with
  their times and sizes; `get` returns the stored bytes unchanged; opencode's
  `get` runs `export` for the named session. Each fixture's shape is copied
  from B-F1, which the plan re-reads on the installed versions first.
- Unit, server: the gate. `transcript_read` within scope reads the task's own
  sessions, and `exec_view` alone is refused. A shared session needs
  `exec_run` as well: without it, its row is dropped and its `get` refused. A
  disconnected runner answers `runner_offline`.
- Dummy harness, real agents: one session per agent in a worktree task, then
  `transcript ls` and `get` from the CLI, the TUI `d` popup and the WebUI
  sheet. One sandboxed session per agent (B-F4). One `--no-worktree` task,
  read with and without `exec_run`.

## Left to the builder

Field names, the list body's encoding, how the runner reads SQLite (a pure-Go
driver or the `sqlite3` binary: weigh the dependency, and say which), and the
TUI/WebUI layout within B-D7.

## Amendments (as built, 2026-11-15)

Plan: `docs/superpowers/plans/2026-11-14-pillar-b-transcripts.md`.

### Where the shipped code differs from the text above

- **Statuses (Wire).** `TranscriptStatus` (`runner/protocol/message.bgn`) is
  `ok`, `no_such_task`, `runner_offline`, `internal_error`,
  `no_such_session`, `no_such_file`, `store_error`, `runner_unsupported`.
  There is no "not permitted" status. A task out of `transcript_read`'s scope
  answers `no_such_task`, so the answer is no existence oracle
  (`server/transcript.go`). A caller without the bit gets the shared
  `PermissionDenied` response, through `requiredCap`
  (`server/capabilities.go`). "An agent without transcripts" is
  `store_error` with a message naming `--agent-transcript-store` and the
  restart through the unit (see Deployment), given only when no profile on the
  runner names a store; a shell profile
  beside agent profiles is skipped (`runner/transcript_query.go`).
- **Shared sessions without `exec_run` (B-D2).** `ls <task>` answers `ok`
  with no rows and a message saying the sessions are listed only to a caller
  holding `exec_run`. `ls <task> <session>` and `get` answer
  `no_such_session` (`runner/transcript_query.go`).
- **The shared-row filter runs on the runner, not the server (B-D2).** The
  server sends `include_shared`, set when the caller holds `exec_run` and the
  task is inside `exec_run`'s scope. The runner applies it
  (`server/transcript.go`, `runner/transcript_query.go`). The runner does not
  classify each session. It reads the task's directory from its own
  `--no-worktree` mode: the worktree path, all sessions the task's own, or the
  repo, all sessions shared. On a worktree runner, sessions recorded in the
  repo itself are not listed for any task.
- **Runner capability bit.** `RunnerHello` carries `transcripts`. The server
  keeps it on `RunnerEntry.Transcripts` and never sends a transcript request to
  a runner without it, because an older runner drops the connection on an
  unknown request type. Such a task answers `runner_unsupported`
  (`server/registry.go`, `server/transcript.go`, `runner/connect.go`).
- **Bounds.** A session id is at most 128 bytes and a file name at most 512.
  Both are declared once in `runner/protocol/transcript_bounds.go`, so the
  control messages that carry them fit one datagram. The client checks them
  before sending (`cli/transcript.go`), the server before relaying
  (`no_such_session` / `no_such_file`, `server/transcript.go`), and the runner
  before looking anything up.
- **agy's index is read with the `sqlite3` CLI** (operator, 2026-11-15), as
  `sqlite3 -readonly -json`, run through `runner/hostcmd`
  (`runner/transcript/agy.go`). The whole index is read and matched in Go, so
  no caller-supplied path reaches the SQL.
  - Only the first URI in `workspace_uris` counts as the conversation's
    directory. Later ones are directories added with `--add-dir`.
  - agy writes a zero `last_modified_time` for some conversations. The updated
    time then falls back to the newest mtime among the conversation's files.
    The index keeps no creation time, so agy rows have none (B-D1's created
    time is empty for agy).
  - The store root comes from `HOME` (`USERPROFILE` on Windows).
  - If `~/.gemini/antigravity-cli` is absent, the list is empty. If the root
    exists but the index file, a column or `sqlite3` is missing, the result is
    an error that names it, shown as the `ls` note. So a format change by agy
    appears as an error, not as an empty list.
- **"The agent's environment" (B-D5) is the runner process's environment**,
  which the agents it starts inherit (`Session.transcriptEnv`, nil →
  `os.Getenv`, `runner/session.go`). Per-profile or per-run settings are not
  consulted. In particular, pi's per-run `--session-dir` is **not** read. pi
  resolves only `PI_CODING_AGENT_SESSION_DIR`, then
  `$PI_CODING_AGENT_DIR/sessions`, then `~/.pi/agent/sessions`
  (`newPi`, `runner/transcript/jsonl.go`).
- **Client time bounds.** The client bounds only the wait for the server's
  `TranscriptResponse`, at 15 s. A timeout is reported as a server older than
  transcripts, which drops the request. The stream body has no fixed bound. It
  is read under the caller's context (`transcriptAnswerWait`,
  `streamCtxReader`, `cli/transcript.go`): Ctrl-C ends a CLI fetch. The TUI's
  `transcript get` and the WebUI's download run under contexts that live as
  long as the program or page, and neither has a cancel control, so a large
  fetch there runs to its end.
- **WebUI download memory.** The wasm bridge returns a download as an array
  of `Uint8Array` parts, one per write (`jsChunks`,
  `cmd/harness-webui-wasm/transcript.go`). `triggerDownload`
  (`webui/static/files.js`) joins them into one `Blob`. So the wasm heap never
  holds a whole transcript, but the browser's `Blob` does: no download API
  that streams to disk exists in every browser.
- **A store an agent can write is not trusted for names.** A sandboxed
  agent can write its own store (agy mounts `~/.gemini`, claude its project
  directory). A session id that is not a single local name (`..`, `../x`,
  `a/b`) is not a session, and every companion walk and stat goes through
  `os.Root`, so a directory swapped for a symlink that leaves the store is not
  followed (`localID`, `walkRegular`, `lstatRegular`,
  `runner/transcript/open.go`). Found by the final review: before it, a
  listing could name files and sizes outside the store.
- **Windows is not verified (Goals, B-D9).** B-D9 asked for each agent's store
  to be measured on a Windows runner before the code, and for an E2E run
  there. Neither was done: no Windows task was reachable (B-F11). The
  Windows-specific paths (`samePath` with case folding, `USERPROFILE` for
  agy, claude's encoding of a `C:\…` directory, opencode through its `.cmd`
  shim, the spool that is not unlinked while open) have run only as unit
  tests on Linux. Closing it: one claude or codex session on a Windows runner,
  then `transcript ls` / `get` from another host.
- **Deliberately shown on no surface.** The `transcripts` bit stays on the
  server's `RunnerEntry`. It is not copied to `RunnerInfo`, so `ls` does not
  show it: it only reveals version skew, and `runner_unsupported` names the
  skew when it matters. A profile's `transcriptStore` is not displayed either.
- **Deployment.** A runner restarted by replaying its old argv
  (`scripts/restart.py`, `build_and_restart_all.py`) gets the new binary, which
  sets `transcripts`. Its argv carries no `--agent-transcript-store`, and its
  `--agent-profiles` JSON has no `transcriptStore`. Every fetch then answers
  `store_error`, whose message names the flag and this restart. Restart slots
  through their systemd units (`runner-autostart.py`'s `ExecStart` runs
  `runner.py up`, which re-expands `scripts/agent_presets.py`), so the preset
  change takes effect.
