# Read-only git view from the operator surfaces

Date: 2026-08-03

## Problem

The runner gives every task its own `git worktree` under
`<repo>/.harness-worktrees/<taskID>/` and the agent works there. The
operator has no way to read what the agent changed:

- **There is no route to a diff at all.** `file ls` / `file pull` expose
  file *contents*, not the change against a baseline. Committed work is
  invisible to them, and so is "which files did this task touch".
- **The one workaround needs a live shell and mangles the output.**
  `harness-cli session exec <id> "git diff"` requires an interactive
  session with a foreground shell (a `submit`-mode task has none), and
  the answer comes back through a PTY — width-wrapped, sentinel-framed,
  and subject to the bare-CR boundary handling in `cli/exec_native.go`.
  A diff is exactly the kind of payload that does not survive it.
- **Reading a diff must not disturb the agent.** The dominant case is a
  claude session that is still running: the operator wants to look at
  the work in progress from the side. Injecting a command into that
  session's shell is not "from the side".
- **The baseline is not fixed.** Agents commit inside the worktree, so
  plain `git diff` shows nothing while `git diff <branch-point>` shows
  everything. Which baseline is wanted changes with the situation, so
  the baseline has to be a runtime choice, not a design-time constant.
- **A repository inside the repository is invisible.** Verified against
  git, not assumed: a plain nested repo (a directory with its own `.git`,
  not a submodule) collapses to a single `?? nested/` entry — even under
  `--untracked-files=all`, which does not descend into one — and appears
  in no diff at all, because it is untracked. A submodule fares slightly
  better: `status` reports ` M sub` and `diff` reports the gitlink moving,
  with a `-dirty` marker, but the file-level change inside stays hidden.
  Either way "what did the agent change" is silently incomplete, and
  plain-nested is the shape a real project in use here has.
- **A finished task's work is unreachable.** `server/file_transfer.go`
  rejects anything that is not `Running` / `Detached`, and the worktree
  dir is removed on a clean task end. The branch `harness/<taskID>` is
  deliberately retained (`runner/worktree.go`) but nothing reads it.

## Non-goals

- Writing git operations (`commit`, `add`, `checkout`, `stash`). This
  view has no side effects on the repository.
- A general "run a command in the worktree" RPC. That would hand remote
  code execution to any principal holding a read capability and would
  make the capability model meaningless.
- Rendering a semantic/structural diff (per-hunk folding, word-level
  intra-line diff, three-way merge views). The unified diff text as git
  produces it is the payload.

## Solution overview

One new read-only `TaskControl` RPC, `git_query`, fanned out to the
task's runner exactly like `list_files` is. The runner shells out to
`git` in the task's worktree and streams the result back. Five query
kinds — `log`, `diff`, `show`, `status`, `subrepos` — cover the whole
surface, and a query may re-root itself into a nested repository.

Every operator surface gets it: a `harness-cli git` subcommand, a TUI
modal plus a TUI cmdline verb, a WebUI tab plus a WebUI cmdline verb,
and the WASM bridge underneath the WebUI.

## Where git runs

The client names only a task ID. The runner resolves the working
directory itself:

| Situation | cwd | tip ref | Visible |
| --- | --- | --- | --- |
| Worktree present (`Running` / `Detached`, the normal case) | `<repo>/.harness-worktrees/<taskID>/` | `HEAD` | committed + uncommitted + untracked |
| `--no-worktree` task | `<repo>` | `HEAD` | same, without branch isolation |
| Worktree already removed (terminal task) | `<repo>` | `refs/heads/harness/<taskID>` | committed only |

Row 1 is the design centre; rows 2 and 3 are a single fallback in the
cwd/tip resolution and cost nothing extra.

`repo_path` comes from the server's task record (`TaskInfo.repo_path`)
because the runner's own `s.tasks[taskIDHex]` entry is dropped when the
task ends — `worktreeDirFor` (`runner/file_transfer.go:109`) cannot
answer for a terminal task. The runner **re-validates** that path
against its own `AllowedRoots` via `repoAllowed`
(`runner/session.go:235`) before running anything, and reconstructs the
worktree directory itself as `filepath.Join(repoPath,
".harness-worktrees", taskIDHex)` rather than trusting a
server-supplied directory.

The tip ref matters only in row 3: with no worktree there is no `HEAD`
pointing at the task's work, so the runner passes
`refs/heads/harness/<taskID>` explicitly. If neither the worktree nor
that ref exists the answer is `no_source`.

Row 1's worktree is recognised with `git rev-parse --show-toplevel`, not
a directory stat. A directory left behind by a crashed cleanup still
exists, and git run inside it walks up to the parent repository's `.git`
and answers about the *parent* filtered to that subdirectory — a wrong
answer wearing a right answer's face.

**Row 3 does not let the shared repository stand in for the task.** With
no worktree the cwd is a checkout that belongs to whoever has it out, so
three things have to be redirected or the answers are quietly about the
wrong thing:

- A leading `HEAD` in a revision resolves to the tip ref instead
  (`HEAD~1` → `refs/heads/harness/<taskID>~1`). Left alone, `diff HEAD~1`
  dies with "bad revision" and `diff HEAD` silently diffs against main.
- `status` returns an empty listing. Running it in the shared repo
  reports *its* state — other tasks' worktree directories show up as
  untracked entries.
- The `worktree` and `index` diff targets fall back to the tip, so
  `diff <base>` means `<base>..<tip>`. An empty base then yields
  `<tip>..<tip>`, i.e. nothing uncommitted, which is the truth; inventing
  a parent revision instead would break on a root commit.

All three were found by driving a dummy harness, not by the unit tests.

## Authorization

`git_query` requires `Capability_FileRead`, added to `requiredCap` in
`server/capabilities.go`.

The `Running || Detached` guard that `handleOpenFileTransfer` /
`handleListFiles` apply (`server/file_transfer.go:29`) is **not**
applied to `git_query`: a task record that still exists is enough. The
runner named by `task.AssignedTo` must still be online, otherwise
`runner_offline`.

**Known limitation, stated rather than discovered.** `task.AssignedTo` is
a connection id, not a stable runner identity, so once the runner that
ran a task restarts, `git_query` on that task answers `runner_offline`
even though its branch is still on disk and the replacement runner serves
the same root. Falling back to any online runner covering the task's
repo path would paper over it, but a same-named path on a different host
is a different repository, and answering from it would be the same class
of wrong-answer-wearing-a-right-answer's-face this design spends effort
avoiding. The dominant case — reading a running agent's work from the
side — is unaffected, because that task's runner is by definition online.

Scope note, so the widening is on the record: `OpenFileTransfer` and
`ListFiles` are gated on the capability bit alone — there is no
per-task scope check on them (`server/task_handler.go:323-360`;
`visibleToCaller` is used by `List` / `GetTaskLog` / port-forward
listing, not by file ops). `FileRead` therefore already means "read any
task's worktree". Putting `git_query` on the same bit keeps it inside
that existing trust class rather than inventing a new gate. What is
newly reachable is repository *history* — including commits whose
worktree has been removed, and other `harness/<id>` branches in the same
repo. That is accepted: an agent already has a shell in a worktree that
shares `.git` with the whole repository, so plain `git log` there
reaches the same objects.

## Repositories inside the repository

Two independent mechanisms, because the two cases are not the same shape.

### Re-rooting: `subrepo`

A query may name a worktree-relative directory that is itself a git repo
root, and the whole query runs there instead. `log`, `diff`, `show` and
`status` all honour it, so a nested repo gets the same view — including
the same movable baseline — as the task's own worktree.

This covers the plain-nested case, which nothing else can: from outside,
git has no view into an untracked nested repo. It covers submodules too,
and more completely than `--submodule` below, since the submodule's own
history becomes browsable.

`subrepo` is its own field, not a reuse of `path`. `path` is a pathspec —
a filter applied to a repository — while `subrepo` chooses *which
repository*. Overloading one on the other would be exactly the kind of
by-convention encoding this project has ruled out.

Resolution, in order: `ValidateRelPath` against the task's worktree (so
`..` cannot escape), then `rejectIfSymlinkInPath` (so a symlink cannot),
then the same `git rev-parse --show-toplevel` equality that recognises a
worktree root. A path that resolves but is not a repo root answers
`subrepo_invalid` rather than silently running in the enclosing
repository — the failure mode this design keeps spending effort to avoid.

Inside a subrepo the tip is its own `HEAD`. The `harness/<taskID>`
fallback does **not** apply: that branch lives in the outer repository. A
task whose worktree is gone therefore answers `no_source` for any
`subrepo` query, because the nested repo lived inside that worktree and
went with it.

### Discovery: the `subrepos` query kind

Nested repos are listed by the runner, not guessed at by the UI. Deriving
the list from `status` `??` entries was rejected: it misses a nested repo
that is gitignored and reports plain untracked directories that are not
repos at all.

The walk starts at the current root (so it composes: with `subrepo` set,
it lists what is nested one level further in), descends at most 6
directory levels, skips `.git` and `.harness-worktrees`, and does not
descend into a repo it has already found — a repo inside a nested repo is
that repo's business. A directory is a candidate when it holds a `.git`
entry of either kind (a submodule's is a file, not a directory); each
candidate is then confirmed with the `--show-toplevel` check. Candidates
are capped at 64 and the body carries a `truncated` flag, because a cap
that is not reported reads as "there were none".

### Combined view: `--submodule`

Off by default, matching git. When set, `diff` and `show` pass
`--submodule=diff`, which inlines the submodule's own file-level changes —
uncommitted ones included — under the gitlink entry.

It stays opt-in because that output is no longer an applyable patch
(`git apply --check` rejects it: `Submodule sub <a>..<b>:` is not patch
syntax), and `harness-cli git <id> diff > x.patch` currently produces one.
It costs nothing in a repository with no submodules: verified byte-identical
output.

## Wire schema

All of it lands in `runner/protocol/message.bgn` in one place.

`git_query` is appended to `TaskControlKind` and `RunnerRequestType`
(existing ordinals unchanged) and gets one arm each in
`TaskControlRequest`, `TaskControlResponse` and `RunnerRequest`.

```
enum GitQueryKind:
    :u8
    log      # commit list for the picker pane
    diff     # unified diff between base_rev and the chosen target
    show     # one commit: header + its diff
    status   # porcelain state: uncommitted + untracked
    subrepos # list the git repo roots nested under the current root

# What the right-hand side of the diff is. base_rev is always the left.
enum GitDiffTarget:
    :u8
    worktree   # the working tree  -> git diff [base]
    index      # the staged tree   -> git diff --cached [base]
    rev        # target_rev names it -> git diff base target_rev

format GitQueryRequest:
    task_id        :TaskID
    kind           :GitQueryKind
    target         :GitDiffTarget   # diff only; ignored by log/show/status
    base_rev_len   :u16
    base_rev       :[base_rev_len]u8   # commit-ish. empty: diff -> the index,
                                       # show -> HEAD, log -> the task tip.
    target_rev_len :u16
    target_rev     :[target_rev_len]u8 # only when target == rev
    path_len       :u16
    path           :[path_len]u8       # worktree-relative pathspec; empty = all
    max_commits    :u32                # log only. 0 -> 100, clamped to 1000
    max_bytes      :u32                # diff/show only. 0 -> 2 MiB, clamped to 8 MiB
    subrepo_len    :u16
    subrepo        :[subrepo_len]u8    # worktree-relative directory that is
                                       # itself a repo root; the whole query
                                       # runs there. Empty = the task's own
                                       # worktree. NOT a pathspec — see `path`.
    submodule_diff :u1                 # diff / show: pass --submodule=diff so a
                                       # submodule's own file-level changes are
                                       # inlined. Off by default (git's own
                                       # default, and the output stops being an
                                       # applyable patch).
    reserved       :u7

enum GitQueryStatus:            # what the server can decide; inline in the response
    :u8
    ok
    no_such_task
    runner_offline
    internal_error

format GitQueryResponse:
    status    :GitQueryStatus
    stream_id :u64              # 0 unless status == ok

format RunnerGitQueryRequest:   # server -> runner
    task_id        :TaskID
    stream_id      :u64
    repo_path_len  :u16
    repo_path      :[repo_path_len]u8
    kind           :GitQueryKind
    target         :GitDiffTarget
    base_rev_len   :u16
    base_rev       :[base_rev_len]u8
    target_rev_len :u16
    target_rev     :[target_rev_len]u8
    path_len       :u16
    path           :[path_len]u8
    max_commits    :u32
    max_bytes      :u32
    subrepo_len    :u16
    subrepo        :[subrepo_len]u8
    submodule_diff :u1
    reserved       :u7

enum GitRunStatus:              # what the runner decides; carried on the stream
    :u8
    ok
    repo_not_allowed   # repo_path is not under this runner's --roots
    no_source          # neither a worktree dir nor refs/heads/harness/<task>
    not_a_git_repo
    bad_rev            # a rev did not resolve, or began with '-'
    git_failed         # git exited non-zero for another reason
    io_error

format GitCommit:
    sha_len     :u8             # 40 for sha1 repos, 64 for sha256 ones — a
    sha         :[sha_len]u8    # fixed [40]u8 would be a hard assertion the
                                # encoder panics on the day a sha256 repo shows up
    author_len  :u16
    author      :[author_len]u8
    when        :u64            # author date, unix seconds
    subject_len :u16
    subject     :[subject_len]u8

format GitLogBody:
    count     :u32
    commits   :[count]GitCommit
    truncated :u1               # max_commits cut the list short
    reserved  :u7

format GitStatusEntry:
    xy       :[2]u8             # the two porcelain status bytes, e.g. " M", "??"
    path_len :u16
    path     :[path_len]u8

format GitStatusBody:
    count   :u32
    entries :[count]GitStatusEntry

format GitSubrepoEntry:
    path_len :u16
    path     :[path_len]u8      # relative to the query's current root

format GitSubrepoBody:
    count     :u32
    entries   :[count]GitSubrepoEntry
    truncated :u1               # the 64-candidate cap cut the walk short
    reserved  :u7

format GitTextBody:
    len       :u64
    text      :[len]u8          # git's stdout verbatim (--no-color)
    truncated :u1               # max_bytes cut the text short
    reserved  :u7

format GitQueryResult:          # the stream payload
    status     :GitRunStatus
    stderr_len :u16
    stderr     :[stderr_len]u8  # git's stderr, trimmed; empty when ok
    kind       :GitQueryKind
    match kind:
        GitQueryKind.log    => log    :GitLogBody
        GitQueryKind.status => status :GitStatusBody
        GitQueryKind.diff   => diff   :GitTextBody
        GitQueryKind.show   => show   :GitTextBody
```

When `status != ok` the body arm is still present and empty (`count = 0`
/ `len = 0`). Wasting a few bytes on the error path is preferred over
conditional encoding, which is harder to read and to round-trip test.

## How git is invoked

`exec.Command("git", ...)` with an explicit argv — never through a
shell. On top of that:

- A rev or pathspec beginning with `-` is rejected with `bad_rev` before
  git is started, and a `--` separator always precedes the pathspec.
- `--no-ext-diff` and `--no-textconv` are always passed. Without them a
  `.gitattributes` in the worktree — which the agent can write —
  designates an external diff driver, and reading a diff becomes command
  execution on the runner host.
- `--no-color`. Colouring happens client-side; ANSI from the runner
  would have to be stripped again for the WebUI.
- The command runs under a 30s context timeout, and stdout is cut at
  `max_bytes` (the process is killed rather than drained).

Per-kind argv:

| Kind | argv after `git` |
| --- | --- |
| `log` | `log --no-color --max-count=<n+1> --format=%H%x00%an%x00%at%x00%s <tip> -- <path>` |
| `diff` (target `worktree`) | `diff --no-color --no-ext-diff --no-textconv [base] -- <path>` |
| `diff` (target `index`) | `diff --no-color --no-ext-diff --no-textconv --cached [base] -- <path>` |
| `diff` (target `rev`) | `diff --no-color --no-ext-diff --no-textconv <base> <target_rev> -- <path>` |
| `show` | `show --no-color --no-ext-diff --no-textconv <base_rev, or HEAD/tip when empty> -- <path>` |
| `status` | `status --porcelain --untracked-files=all -z -- <path>` |

`--max-count=<n+1>` is how `GitLogBody.truncated` is decided: ask for one
more than the client wanted, drop the extra commit before encoding, and
set `truncated` if it arrived.

`<tip>` is `HEAD` when a worktree was found and
`refs/heads/harness/<taskID>` when the runner fell back to the repo
directory. `log` uses `base_rev` as its starting point when the client
supplied one, and `<tip>` otherwise.

`diff` with `target == rev` requires a non-empty `base_rev`; an empty one
is `bad_rev` rather than an argv with a hole in it.

`status` parses with the same `-z` record walk as `filterDirtyPaths`
(`runner/worktree.go`), including consuming the extra record that a
rename entry carries.

## Diff semantics

git's own, unchanged:

| Intent | base_rev | target |
| --- | --- | --- |
| Unstaged only | empty | `worktree` |
| All uncommitted | `HEAD` | `worktree` |
| **From a chosen branch point through the current working tree** | `<sha>` / `HEAD~5` / `main` | `worktree` |
| Staged only | `HEAD` | `index` |
| Between two commits | `<sha1>` | `rev(<sha2>)` |

Row 3 is the flexible-baseline requirement. Selecting a commit in the
log pane replaces `base_rev`; nothing else changes.

Untracked files appear in no diff — that is git, not a gap in this
design — which is why `status` exists: they show up as `??` entries.
Their contents are already readable through the existing file browser,
so no `diff --no-index` trickery is added.

## Client API

Methods on the long-lived `*cli.Client`, matching `FileLs`. No
`Dial+Close` variant: the TUI and WebUI already hold a client, and a
fresh-dial helper is exactly what they would wrongly copy — pitfall 3 in
`.claude/skills/implementation-pitfalls/SKILL.md`.

```go
type GitQuery struct {
    Kind       protocol.GitQueryKind
    Target     protocol.GitDiffTarget
    BaseRev    string
    TargetRev  string
    Path       string
    MaxCommits uint32
    MaxBytes   uint32
}

type GitCommit struct {
    SHA     string
    Author  string
    When    time.Time
    Subject string
}

type GitStatusEntry struct {
    XY   string
    Path string
}

type GitResult struct {
    Status    protocol.GitRunStatus
    Stderr    string
    Kind      protocol.GitQueryKind
    Commits   []GitCommit     // log
    Entries   []GitStatusEntry // status
    Text      string          // diff / show
    Truncated bool
}

func (c *Client) GitQuery(ctx context.Context, taskIDHex string, q GitQuery) (*GitResult, error)
```

with `GitLog` / `GitDiff` / `GitShow` / `GitStatus` as thin wrappers.

## Operator surfaces

Every cell is filled; none is silently skipped.

| Surface | What lands |
| --- | --- |
| CLI binary | `harness-cli git <task-id> log\|diff\|show\|status …` |
| TUI keybinding | `G` opens the git modal for the selected task (`g` is already Grid) |
| TUI cmdline | `git <task-id> …`, same grammar as the CLI |
| TUI popup | `GitModal`, shaped after `BoardModal` (`tui/board.go`) |
| WebUI buttons/forms | a **Git** tab on the task sheet |
| WebUI cmdline | `git` in `runCmd`, same grammar, plus its help line |
| WASM bridge | git query + result decode exposed like the file-edit bridge |
| Shared cli/server/runner | the client API, `requiredCap`, the runner handler |

### CLI grammar

```
harness-cli git <task-id> log      [--max N] [--subrepo DIR] [-- <path>]
harness-cli git <task-id> diff     [--staged] [--submodule] [--subrepo DIR] [<base>] [<target>] [--max-bytes N] [-- <path>]
harness-cli git <task-id> show     [<rev>] [--submodule] [--subrepo DIR] [-- <path>]
harness-cli git <task-id> status   [--subrepo DIR] [-- <path>]
harness-cli git <task-id> subrepos [--subrepo DIR]
```

`diff` counts positionals the way git does: none means unstaged, one
means `<base>` against the working tree, two means commit against
commit. `--staged` puts the index on the right-hand side.
`GitDiffTarget` is not exposed as a flag — a hand that knows git should
not have to learn a new noun. Flags parse in any position relative to
the positionals, matching the fix in `4213cca`.

### TUI modal

```
┌ git — task 7f3a…  base: HEAD  repo: (root) ──────────────┐
│ > [WORKTREE]  3 changed, 1 untracked                     │
│   [INDEX]     staged                                     │
│   a1b2c3d  2h ago   claude   feat(tui): grid paging      │
│   9e8f7a6  3h ago   claude   wip                         │
│   [REPO]  pkg/inner                                      │
│   [REPO]  vendor/lib                                     │
├──────────────────────────────────────────────────────────┤
│ diff --git a/tui/grid.go b/tui/grid.go                   │
│ @@ -261,6 +261,9 @@                                      │
│ +      switch msg.String() {                             │
│ -      switch msg.Type {                                 │
└ enter:show b:base s:status m:submodule u:up r:refresh esc┘
```

- `[WORKTREE]` and `[INDEX]` are client-side pseudo rows, not protocol.
  Selecting them issues a `diff` with target `worktree` / `index`;
  selecting a commit issues a `show`.
- `b` sets the selected commit as `base_rev`. The header reflects it and
  re-selecting `[WORKTREE]` then shows everything since that point. This
  is the whole of the flexible-baseline feature.
- `[REPO]` rows sit at the bottom of the same picker, from the `subrepos`
  query. Enter on one re-roots the whole modal into that repository — its
  own commits, its own movable baseline — and the header names it. `u`
  goes back up one level. Discovery and selection live in one place, and
  no second pane is needed.
- `m` toggles `--submodule` for the diff and show queries.
- `n` / `N` jump between `diff --git ` header lines inside the viewport.
  Purely client-side; no protocol support.
- Colouring is by line prefix (`+` / `-` / `@@` / `diff --git`), done in
  each UI: lipgloss in the TUI, CSS in the WebUI, ANSI from the CLI only
  when stdout is a TTY.
- A non-`ok` `GitRunStatus` renders git's stderr inside the modal, in
  the TUI's own frame (the approach taken in `290d1fc`).
- Truncation renders as `truncated at 2.0 MiB` in the footer. It is
  never silent.

The WebUI Git tab carries the same rows, the same base-selection action
(a button on the commit row), and the same colouring rules.

## Decisions taken

- Colouring is client-side; the runner always passes `--no-color`.
- Untracked files surface through `status` as `??`; no `diff --no-index`.
- Defaults: 100 commits (max 1000), 2 MiB of diff text (max 8 MiB).
- No git binary, or not a repository: answer `not_a_git_repo` and stop.
  There is no fallback path.
- The error body arm is encoded empty rather than conditionally omitted.
- `GitCommit.sha` is length-prefixed, not a fixed 40 bytes, so a sha256
  repository does not trip an encoder assertion.
- `subrepo` is a separate field from `path`, and a path that is not a repo
  root is refused rather than silently running one directory up.
- Nested repos are discovered by the runner (`subrepos`), not inferred by
  the UI from `status` output.
- The subrepo walk is depth 6, capped at 64 candidates, and reports its
  own truncation.
- `--submodule` is off by default; the combined output is not an applyable
  patch, and git's own default is off.

## Testing

- `runner/protocol/git_query_wire_test.go` — round-trip of every new
  format and both enums, mirroring `file_transfer_wire_test.go`.
- Runner unit tests against a real temporary git repository: a commit, a
  modified tracked file, a staged file and an untracked file; assert
  `log`, `diff` for each target, `show`, `status`. Plus: a `-`-prefixed
  rev is rejected as `bad_rev` before git runs; a repo outside
  `AllowedRoots` gives `repo_not_allowed`; removing the worktree still
  answers through `refs/heads/harness/<taskID>`; a missing branch and
  missing worktree give `no_source`.
- Server tests: `file_read` absent gives `permission_denied`; a terminal
  task is accepted (the `Running`/`Detached` guard does not apply); an
  offline assigned runner gives `runner_offline`.
- CLI parse tests for the grammar above, including flags placed before
  and after the positionals.
- `tui/cmdline_test.go` gains the `git` verb; `GitModal` gets key tests
  covering `enter`, `b`, `s`, `n`/`N`, `m` and the `[REPO]` re-root plus
  `u`.
- Runner tests against a fixture with BOTH a plain nested repo and a
  submodule: `subrepos` lists both; a `subrepo` query answers about the
  inner repository and not the outer one; `..`, a symlink, a missing path
  and a non-repo directory each give `subrepo_invalid`; `--submodule`
  inlines the submodule's file-level change and its absence does not.
- `scripts/wire-skew-check.sh` — mandatory because `.bgn` changes. On
  deployment the **server restarts first**; a runner-first rollout is
  what wiped the fleet in the incident behind that script.
- `make check` and `make wasm-check`.
- End-to-end on a dummy harness (`scripts/dummy-harness.sh`): a local
  server and runner from this checkout's own `bin/`, a task that commits
  and then leaves both a dirty tracked file and an untracked one, driven
  through `harness-cli git` and through the TUI modal.
