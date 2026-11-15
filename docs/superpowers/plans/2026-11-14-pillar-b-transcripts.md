# Pillar B — fetching agent transcripts — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `transcript ls` / `transcript get` fetch, by task id, the
transcripts every agent stored for the task's working directory — a whole
session as a tar or one file of it — on the CLI, TUI and WebUI, from Linux and
Windows runners, gated by a new `transcript_read` bit.

**Architecture:**
- **Runner:** a `runner/transcript` package with one `Store` per agent kind
  (claude, codex, pi, agy, opencode). Each lists the sessions recorded in a
  directory, lists a session's files, and opens one. A profile names its
  store kind (`transcriptStore`, as `logFormat` names a decoder). A new runner
  request resolves the task's directory, asks every distinct store, and writes
  the answer on a stream.
- **Server:** two task-control verbs on the git-query path
  (`server/git_query.go`). They gate, pick the task's assigned runner, relay,
  and splice. The server never parses the answer. Rows recorded in a shared
  directory are filtered by the runner, on a flag the server sets from the
  caller's `exec_run`.
- **Clients:** CLI verbs; the TUI `d` popup and command line; the WebUI task
  sheet, through wasm bridge calls that reuse the CLI client.

**Tech Stack:** Go (runner, server, runner/protocol, cli, tui, wasm), `.bgn` →
`make protoregen`, JS (webui), stdlib `archive/tar`.

**Spec:** `docs/superpowers/specs/2026-11-14-pillar-b-transcripts-design.md`
(facts B-F1–B-F7, decisions B-D1–B-D10, Wire, Testing), inside
`docs/superpowers/specs/2026-11-12-run-modes-and-records-grand-design.md`.

## Global Constraints

- **Before starting:** read `.claude/skills/implementation-pitfalls/SKILL.md`.
- **Where to work:** this worktree, its `harness/<dir>` branch.
- **Wire:** every `.bgn` change is in Task 1 and only there.
  - Enum values are appended.
  - Regenerate with `make protoregen`.
  - **Runner-decoded** (`RunnerRequest` gains a kind): run
    `scripts/wire-skew-check.sh <deployed sha>`. Restart the server first,
    then the runners. Restart host1 slots with
    `systemctl --user restart harness-agent-runner-<tag>`, never `restart.py`:
    Task 4 changes `scripts/agent_presets.py`, and `restart.py` would run the
    old argv (memory: restart.py freezes preset changes). The operator's own
    slot goes last, on a `systemd-run --user --on-active=90` timer.
- **Datagram size:** requests and responses are fixed-size apart from
  short ids and names (bounded below). Lists and file bytes go on a stream.
- **Capabilities:** `transcript_read = 0x10000000`, `all = 0x1fffffff`. Every
  place A1's `record_persist` touched is updated the same way:
  `runner/protocol/message.bgn` (bit, `all`, the `all` comment's list),
  `runner/protocol/capability_test.go`, `cli/verb/caps.go` (ordered list,
  `CapDescription`, tier table: **TierStrong**, since it reads file contents),
  `cli/verb/caps_audit_table_test.go`, `server/capabilities.go`,
  `server/cap_completeness_test.go`, `server/scope_percap_completeness_test.go`,
  and the skills listing caps (`runner/agentskills/supervising-workers/SKILL.md`
  and its mirrors).
- **Bounds:** a session id is ≤ 256 bytes, a file name ≤ 1024 bytes; a longer
  one is refused at the CLI and by the runner (`no_such_session` /
  `no_such_file`).
- **Never install a package silently.** If Task 0 finds agy needs a SQLite
  reader, adding a Go module is a decision point: stop and ask the operator.
- **Live checks:** `dummy-harness` skill, every surface (CLI, TUI in a PTY,
  WebUI in Playwright), screenshots kept under `a1-shots/` and reported.

## Review Focus

1. **A session id that belongs to another directory** (another task's,
   guessed or copied): `get`/`ls <session>` answer `no_such_session`, never
   the bytes. → Task 4 test.
2. **A file name that is not one the listing produced** (`../x`, an absolute
   path, a sibling session's file): `no_such_file`, nothing opened. → Task 2
   test.
3. **A transcript being written while it is read** (a live claude session; an
   agy `.db` with its `-wal`): `get` returns the bytes present when the file was
   opened and ends cleanly, never an error or a hang. → Task 2 test.
4. **A large transcript** (≥ 100 MB): streamed end to end. Neither the runner
   nor the server holds it in memory. → Task 4 test (a reader that fails if
   read into one buffer: assert peak allocation stays under 8 MiB for a 100 MB
   fixture).
5. **The runner disconnecting mid-stream:** the client returns an error
   naming the cut, never a short file reported as complete. → Task 6 test.

---

### Task 0: Measure what the spec left to measure; settle agy's index

**Files:**
- Modify: `docs/superpowers/specs/2026-11-14-pillar-b-transcripts-design.md`
  (append facts B-F8… and resolve B-D5's per-agent variables)

No code. Each measurement records the command that produced it (memory:
numbers carry their command). Read names, schemas and keys, never
conversation content.

- [ ] **Step 1: State directories, per agent, Linux.** From each agent's
  documentation and `--help`, name the variable or flag that moves its store
  (claude `CLAUDE_CONFIG_DIR`; codex `CODEX_HOME`; pi `--session-dir` and any
  env; agy; opencode `XDG_DATA_HOME`). Confirm each by starting the agent once
  in a scratch directory with the variable set, and finding the new file.
- [ ] **Step 2: Sandboxed variants.** For `sandbox-claude`, `sandbox-codex`,
  `sandbox-pi`, `sandbox-agy`, `sandbox-opencode`: open one session in a dummy
  harness worktree under the fleet's default auth mode. Record where its files
  appear on the host (B-F4). Token auth: confirm nothing persists.
- [ ] **Step 3: Windows.** Through `harness-cli exec` on a Windows runner's
  task: each of claude, codex and opencode's store path; claude's encoded
  directory name for a `C:/…/.harness-worktrees/<id>` path; the separator
  and case that each format records in its `cwd`; `opencode session list
  --format json` and `opencode export` output there.
- [ ] **Step 4: agy's index.** Find a source that maps a conversation id to
  its workspace without SQLite: a JSON or text file under
  `~/.gemini/antigravity-cli/` (`history.jsonl`, `brain/<id>/`,
  `jetski_state.pbtxt`), or an `agy` subcommand. If none exists,
  `conversation_summaries.db` (`workspace_uris`) is the source. **Stop and ask
  the operator** which reader to use: a pure-Go SQLite module (name it and its
  size) or the `sqlite3` binary (and what happens on a host without one).
- [ ] **Step 5: Write the facts into the spec** as B-F8… (store variables,
  sandbox placement, Windows paths, agy's source). Commit:

```bash
git add docs/superpowers/specs/2026-11-14-pillar-b-transcripts-design.md
git commit -m "spec: pillar B facts measured for the plan (stores, sandbox, Windows, agy index)"
```

---

### Task 1: Wire and the `transcript_read` bit

**Files:**
- Modify: `runner/protocol/message.bgn`, regenerate `runner/protocol/message.go`
- Modify: the capability touch-list (Global Constraints)
- Test: `runner/protocol/transcript_wire_test.go`; the existing capability tests

**Interfaces:**
- Produces (`.bgn`, names exact):
  - `TaskControlKind`: `transcript_list`, `transcript_get` (appended), with
    request/response arms.
  - `TranscriptListRequest { task_id :TaskID; session_len :u16; session
    :[session_len]u8 }`. An empty session asks for sessions; a session asks
    for its files.
  - `TranscriptGetRequest { task_id; session_len; session; file_len :u16;
    file :[file_len]u8 }`. An empty file asks for the whole session as a
    tar.
  - `enum TranscriptStatus :u8`: `ok`, `no_such_task`, `runner_offline`,
    `internal_error`, `no_such_session`, `no_such_file`, `store_error`.
  - `TranscriptResponse { status :TranscriptStatus; stream_id :u64 }`, for
    both verbs.
  - `RunnerRequestType.transcript => RunnerTranscriptRequest { task_id;
    stream_id :u64; op :TranscriptOp; include_shared :u1; reserved :u7;
    repo_path_len :u16; repo_path; session_len; session; file_len; file }`.
  - `enum TranscriptOp :u8`: `sessions`, `files`, `get`.
  - On the stream: `TranscriptResult { status; message_len :u16; message;
    op :TranscriptOp; match op: sessions => sessions :TranscriptSessionsBody,
    files => files :TranscriptFilesBody, get => (nothing) }`. For `get`, the
    raw bytes follow the header until EOF.
  - `TranscriptSession { agent_len :u8; agent; id_len :u16; id; created_ns
    :u64; updated_ns :u64; size :u64; shared :u1; reserved :u7 }`.
    `TranscriptSessionsBody { count :u32; sessions :[count]TranscriptSession }`.
  - `TranscriptFile { name_len :u16; name; size :u64; size_known :u1;
    reserved :u7 }`. `TranscriptFilesBody { count :u32; files
    :[count]TranscriptFile }`.
  - `Capability.transcript_read = 0x10000000`, `all = 0x1fffffff`.

- [ ] **Step 1: Write the failing wire test.** `TestTranscriptResultRoundTrip`:
  a sessions result with two rows (one `shared`), a files result with one
  `size_known=0` file, and a `get` header followed by raw bytes, each encoded
  and decoded back equal; the `get` header's encoded length is what a reader
  consumes before the raw bytes.
- [ ] **Step 2: Run it.** `go test ./runner/protocol -run TestTranscriptResult` →
  FAIL (undefined types).
- [ ] **Step 3: Add the formats and the bit, `make protoregen`, then walk the
  capability touch-list.**
- [ ] **Step 4: Run.** `go test ./runner/protocol ./cli/verb ./server -run
  'Transcript|Cap|Scope'` → PASS, including the completeness tests, which
  fail until every touch-list file names the bit.
- [ ] **Step 5: Commit.** `git commit -m "protocol: transcript list/get wire and the transcript_read bit"`

---

### Task 2: Line-based stores — claude, codex, pi

**Files:**
- Create: `runner/transcript/store.go`, `runner/transcript/jsonl.go`,
  `runner/transcript/path_unix.go`, `runner/transcript/path_windows.go`
- Test: `runner/transcript/jsonl_test.go`, `runner/transcript/testdata/`

**Interfaces:**
- Produces:

```go
package transcript

type Session struct {
	Agent, ID        string
	Created, Updated time.Time
	Size             int64 // all of the session's files
}
type File struct {
	Name      string // relative to the session, slash-separated
	Size      int64
	SizeKnown bool
}
var ErrNoSession, ErrNoFile error

type Store interface {
	Agent() string
	Sessions(dir string) ([]Session, error) // recorded with cwd == dir (SamePath)
	Files(id string) ([]File, error)        // ErrNoSession
	Open(id, name string) (io.ReadCloser, error) // name must be one Files returned, else ErrNoFile
}

// New returns the store for kind ("claude", "codex", "pi", "agy", "opencode")
// rooted where the agent keeps it, resolved through getenv (B-D5, Task 0).
func New(kind string, getenv func(string) string) (Store, error)

// SamePath compares two paths as the runner's OS does (B-D9).
func SamePath(a, b string) bool
```

- [ ] **Step 1: Write the failing tests**, with fixtures shaped as B-F1/B-F1a
  (and Task 0's facts):
  - `TestClaudeSessionsMatchRecordedCwd`: three sessions under one encoded
    dir. Two record `cwd` = the task dir. One records a different directory
    whose truncated name collides (B-F2). Expect exactly the two.
  - `TestClaudeFilesIncludeSubagentsAndToolResults`: Files lists
    `<id>.jsonl`, `<id>/subagents/a.jsonl`, `<id>/tool-results/t.txt`, with
    sizes.
  - `TestCodexSessionsFilterByMetaCwd`: by-date tree, the `session_meta`
    `payload.cwd` decides.
  - `TestPiSessionsFilterByHeaderCwd`.
  - `TestOpenRefusesNamesNotListed` (Review Focus 2): `../x`, an absolute
    path, another session's file → `ErrNoFile`.
  - `TestOpenWhileWritten` (Review Focus 3): a goroutine appends to the file
    while `Open` reads it to EOF. The read ends without error and returns at
    least the bytes present at open.
  - `TestSamePath` (table: separators and case behave as the OS does; on
    Windows `C:\A\b` equals `c:/a/b`).
- [ ] **Step 2: Run** `go test ./runner/transcript` → FAIL.
- [ ] **Step 3: Implement** `New` for claude, codex and pi, plus `SamePath`.
  Read only each file's first line (codex and pi) or the first line that
  carries `cwd` (claude) to classify it. Times come from file mtime and the
  first record's timestamp.
- [ ] **Step 4: Run** → PASS; also `GOOS=windows go vet ./runner/transcript`.
- [ ] **Step 5: Commit.** `git commit -m "runner/transcript: claude, codex and pi stores"`

---

### Task 3: Index-backed stores — opencode, agy

**Files:**
- Create: `runner/transcript/opencode.go`, `runner/transcript/agy.go`
- Test: `runner/transcript/opencode_test.go`, `runner/transcript/agy_test.go`

**Interfaces:**
- Consumes: Task 2's `Store`, `File`, `Session`, `ErrNoSession`, `ErrNoFile`.
- Produces: `New("opencode", …)` and `New("agy", …)`.
  - opencode runs `opencode session list --format json` (cwd: the task dir, or
    the repo when the worktree is gone) and keeps rows whose `directory`
    matches. It has one file per session, `export.json`, `SizeKnown=false`,
    and `Open` runs `opencode export <id>`. The command runner is injectable
    for tests (`execCommand` variable).
  - agy uses the index Task 0 settled. Its files are `<id>.db`, any
    `<id>.db-wal` / `<id>.db-shm`, and `brain/<id>/…`.

- [ ] **Step 1: Failing tests:**
  - `TestOpencodeListsByDirectory`: a fake `session list` JSON with two
    directories.
  - `TestOpencodeGetRunsExport`: asserts the argv `export <id>`, and returns
    its stdout.
  - `TestOpencodeExportFailureIsStoreError`.
  - `TestAgyFilesIncludeWalAndBrain`.
  - `TestAgySessionsByWorkspace`.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** → PASS.
- [ ] **Step 5: Commit.** `git commit -m "runner/transcript: opencode (session list/export) and agy stores"`

---

### Task 4: Runner request handler, profiles' `transcriptStore`

**Files:**
- Create: `runner/transcript_query.go`
- Modify: `runner/agent_profile.go` (`TranscriptStore string`, JSON
  `transcriptStore`, `UnrecognisedTranscriptStores()`),
  `cmd/agent-runner/main.go` (`--agent-transcript-store`; warn like
  `UnrecognisedLogFormats`), the runner's `RunnerRequest` dispatch,
  `scripts/agent_presets.py` (`transcriptStore` per preset: claude, codex,
  agy, opencode, pi; `""` for bash and cmd; sandbox variants inherit through
  the derivation), the preset regression tests (`make scripts-test`)
- Test: `runner/transcript_query_test.go`

**Interfaces:**
- Consumes: Task 1's wire, and Task 2/3's `transcript.New`.
- Produces: `func (s *Session) handleTranscript(ctx context.Context, req
  *protocol.RunnerTranscriptRequest)`.
  - The directory is `filepath.Join(repo, ".harness-worktrees", taskID)`
    (computed, not required to exist), or `repo` when `s.NoWorktree`. The
    latter's sessions are `shared` (B-D2).
  - It asks each distinct `TranscriptStore` of `s.Profiles`.
  - When `!include_shared` it drops shared rows and answers
    `no_such_session` for a shared id.
  - A whole-session `get` writes a tar of `Files` in order.
  - A repo path outside `--roots` → `store_error` with a message, as the git
    query re-validates.

- [ ] **Step 1: Failing tests:**
  - `TestTranscriptSessionsAcrossAgents`: claude and codex fixtures in the
    same dir. Both appear, with `agent` set.
  - `TestTranscriptSharedHiddenWithoutIncludeShared` (no-worktree session).
  - `TestTranscriptForeignSessionIsNoSuchSession` (Review Focus 1): the id
    exists in another task's dir.
  - `TestTranscriptWholeSessionIsTar`: the tar lists the same names as Files.
  - `TestTranscriptStreamsLargeFile` (Review Focus 4): 100 MB, with peak
    allocation under 8 MiB.
  - `TestProfileTranscriptStoreFromJSON`.
  - a preset test that every conversational preset and its sandbox variant
    names its store.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run**
  `go test ./runner/... && make scripts-test` → PASS.
- [ ] **Step 5: Commit.** `git commit -m "runner: transcript requests; profiles name their transcript store"`

---

### Task 5: Server verbs

**Files:**
- Create: `server/transcript.go`
- Modify: `server/capabilities.go` (`requiredCap`: both kinds →
  `Capability_TranscriptRead`), `server/task_handler.go` (dispatch)
- Test: `server/transcript_test.go`

**Interfaces:**
- Consumes: Task 1's wire.
- Produces:
  - `func (h *TaskHandler) handleTranscript(cid string, conn ConnHandle, req
    *protocol.TaskControlRequest)`.
  - `func runnerTranscriptRequest(req …, op protocol.TranscriptOp, repoPath
    string, includeShared bool, streamID uint64) protocol.RunnerTranscriptRequest`,
    a separate relay function like `runnerGitRequest`.
  - The handler gates on `transcript_read` within scope: out of scope →
    `no_such_task`, a missing bit → `PermissionDenied`.
  - `include_shared` = the caller holds `exec_run` within scope on the task.
  - The runner is `task.AssignedTo`; a disconnected one →
    `runner_offline` (B-D6). Then it splices like `handleGitQuery`.

- [ ] **Step 1: Failing tests:**
  - `TestTranscriptNeedsTranscriptRead`: `exec_view` alone →
    PermissionDenied.
  - `TestTranscriptOutOfScopeIsNoSuchTask`.
  - `TestTranscriptIncludeSharedNeedsExecRun`.
  - `TestTranscriptRunnerOfflineWhenUnassigned`, which also covers a resumed,
    queued task.
  - `TestRunnerTranscriptRequestCopiesEveryField`.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run**
  `env -u HARNESS_SERVER_PUBKEY go test ./server -run Transcript` → PASS.
- [ ] **Step 5: Commit.** `git commit -m "server: transcript list/get, gated by transcript_read"`

---

### Task 6: CLI

**Files:**
- Create: `cli/transcript.go`, `cli/transcript_test.go`
- Modify: `cli/verb/table.go` (rows `transcript ls`, `transcript get`;
  `CmdlineSurfaces: CLI | TUI`; `ModalSurfaces` for the TUI popup and the
  WebUI sheet), `cmd/harness-cli/dispatch.go`

**Interfaces:**
- Produces:

```go
func (c *Client) TranscriptSessions(ctx context.Context, taskIDHex string) ([]protocol.TranscriptSession, error)
func (c *Client) TranscriptFiles(ctx context.Context, taskIDHex, session string) ([]protocol.TranscriptFile, error)
func (c *Client) TranscriptGet(ctx context.Context, taskIDHex, session, file string, w io.Writer) error
func TranscriptLsWith(ctx context.Context, c *Client, taskIDHex, session string, out io.Writer) error
func TranscriptGetWith(ctx context.Context, c *Client, taskIDHex, session, file string, out io.Writer) error
func TranscriptSessionRow(s protocol.TranscriptSession) string // "<agent> <id>  created=… updated=…  size=…[  shared]"
```

- `transcript get <task> <session> [<file>] [-o FILE]`. With no `-o`, the
  bytes go to stdout. `-o` is created only once the first byte arrives, so a
  failure leaves no empty file (A1's deferred minor, not repeated here).

- [ ] **Step 1: Failing tests:**
  - `TestTranscriptSessionRowFields`: an absent field is never elided at
    zero (size=0B prints).
  - `TestTranscriptGetCutStreamIsError` (Review Focus 5): a stream that EOFs
    before the header, or before a known size is reached, returns an error.
  - `TestTranscriptGetDoesNotCreateFileOnRefusal`.
  - `TestTranscriptVerbBounds` (an over-long session id is refused).
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run**
  `go test ./cli/... ` → PASS.
- [ ] **Step 5: Commit.** `git commit -m "cli: transcript ls / get"`

---

### Task 7: TUI

**Files:**
- Modify: `tui/detail.go` (a transcripts block under the records block),
  `tui/actions.go` (fetch on `d`), `tui/client.go` (`DoTranscriptList`,
  `TranscriptListMsg`), `tui/app.go`, `tui/dispatch.go` (verbs; `get` needs
  `-o`, as `RecordGet`)
- Test: `tui/transcript_test.go`

- [ ] **Step 1: Failing tests:**
  - `TestDetailShowsTranscriptsWithAgentTimeAndSize`.
  - `TestDetailTranscriptListError`.
  - `TestTuiTranscriptGetNeedsOutputFile`.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run**
  `go test ./tui` → PASS.
- [ ] **Step 5: Commit.** `git commit -m "tui: transcripts in the d popup and on the command line"`

---

### Task 8: WebUI

**Files:**
- Create: `cmd/harness-webui-wasm/transcript.go` (`transcriptSessions`,
  `transcriptFiles`, `transcriptGet`, which reuse `recordPromise`-style
  error naming), `webui/static/transcript_test.mjs`
- Modify: `cmd/harness-webui-wasm/main.go` (register),
  `webui/static/task_models.js` (`transcriptSheetModel`,
  `transcriptDownloadName`), `webui/static/main.js` (a 会話記録 section on the
  task sheet: list, open one to list its files, a download per file and one
  for the whole session), `Makefile` (`js-test` list)

- [ ] **Step 1: Failing tests:** the bridge exports the three calls (checked
  on `page.bridge`, not the recorder); `transcriptSheetModel` rows and actions;
  `transcriptDownloadName(task, agent, session, file)` (`<id8>-<agent>-<session8>.tar`
  for the whole).
- [ ] **Step 2: Run** `make js-test` → FAIL. **Step 3: Implement.**
  **Step 4: Run** → PASS.
- [ ] **Step 5: Commit.** `git commit -m "webui: transcripts on the task sheet"`

---

### Task 9: Docs, parity walk, live checks, landing

**Files:**
- Modify: `README.md` (a "transcripts" quick-start entry, and the capability
  list), `runner/agentskills/supervising-workers/SKILL.md` and its mirrors
  (`transcript_read`, the verbs),
  `.claude/skills/surface-parity-checklist/firing-log.md`

- [ ] **Step 1:** Walk `surface-parity-checklist` items 1–39 for the two verbs
  and the new bit, one verdict per number, and append the entry to its
  firing log.
- [ ] **Step 2: Live, Linux** (`dummy-harness` with `-- --force-inject-harness-settings`;
  a real agent each):
  - one worktree session each for claude, codex, pi, agy and opencode, then
    `transcript ls`/`get` (one file, and the whole tar, `tar tf` matching
    `ls <session>`) from the CLI, the TUI `d` popup in a tmux PTY, and the
    WebUI sheet in Playwright;
  - one sandboxed session per agent;
  - a `--no-worktree` runner's task, read with and without `exec_run`;
  - a task holding only `exec_view` is refused.
  - Screenshots go to `a1-shots/` and are reported.
- [ ] **Step 3: Live, Windows:** one claude or codex session on a Windows
  runner, `transcript ls`/`get` from the host1 CLI.
- [ ] **Step 4:** `env -u HARNESS_SERVER_PUBKEY -u HARNESS_SERVER_CID -u
  HARNESS_AUTH_TICKET -u HARNESS_TASK_ID make test`, `make wasm-check`,
  `scripts/wire-skew-check.sh <deployed sha>` → all green.
- [ ] **Step 5:** Land per the landing policy: the spec, this plan and the
  code together, FF-push, then `make build` in the main checkout. Report the
  restart order: server, then runners via systemd, the operator's slot last.
