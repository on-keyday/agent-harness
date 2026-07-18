# file push -p/--parents + file mkdir

Date: 2026-07-18
Status: approved (design discussed interactively; both features chosen over
auto-mkdir-always and over mkdir-only)

## Problem statement

1. `file push` (single-file and `-r` dir push) fails when the destination's
   parent directory does not exist inside the task worktree — and it fails
   with the generic `io_error` ack ("runner I/O error"), which gives the
   caller no clue that the missing parent is the cause. Agents pushing into
   worktrees hit this repeatedly and cannot self-diagnose.
   - Single-file: `runner/file_transfer.go runPush` — `os.OpenFile` ENOENT
     → `io_error`.
   - Dir push: `runDirPush` — staging extract succeeds, final
     `os.Rename(staging, dest)` ENOENT on missing parent → `io_error`.
2. There is no way to create a directory in a worktree over the file-transfer
   protocol at all. The only workaround is pushing an empty local dir with
   `-r`, which itself requires the parent to exist.

Both are fixed in ONE wire change (single `.bgn` touch, single skew window):
an explicit opt-in `-p/--parents` flag on push, and a `file mkdir` command.

Explicit-flag design was chosen over always-on auto-mkdir (user decision:
opt-in, scp-like strictness by default).

## Wire schema (`runner/protocol/message.bgn`)

One authoritative change, both halves here (no split across tasks):

```
enum FileTransferDirection:
    :u8
    push
    pull
    delete
    dir_push
    dir_pull
    dir_delete
    mkdir        # create a directory. mkdir_parents=0 → strict os.Mkdir
                 # (parent must exist, existing dir → already_exists);
                 # mkdir_parents=1 → os.MkdirAll (parents created, existing
                 # dir → ok). Mirrors Unix mkdir / mkdir -p. No body bytes;
                 # ack only, like delete. force / expected_size are ignored.
```

Both `OpenFileTransferRequest` and `RunnerOpenFileTransferRequest` change

```
    force         :u1
    reserved      :u7
```

to

```
    force         :u1
    mkdir_parents :u1     # push / dir_push: create missing parent
                          # directories of the destination before writing.
                          # mkdir: MkdirAll instead of strict Mkdir.
                          # Ignored by pull / delete / dir_pull / dir_delete.
    reserved      :u6
```

Byte layout size is unchanged (bit taken from reserved). Regenerate with
`make protoregen ARGS='runner/protocol/message.bgn'`.

`FileTransferStatus` gains no new values. Existing `not_found` is reused for
"push destination's parent directory does not exist" — unambiguous because a
push's leaf is created with O_CREATE, so ENOENT can only mean the parent.

## Runner behavior (`runner/file_transfer.go`)

All paths run AFTER the existing `ValidateRelPath` + `rejectIfSymlinkInPath`
checks, so every directory created lives inside the worktree and no traversal
escapes it. Created directories use mode 0o755.

- `runPush`: when `mkdir_parents` is set, `os.MkdirAll(filepath.Dir(full),
  0o755)` before `OpenFile`. Independently of the flag, the ENOENT branch of
  `OpenFile` now acks `not_found` (was: `io_error`).
- `runDirPush`: when `mkdir_parents` is set, `os.MkdirAll(filepath.Dir(dest),
  0o755)` before the final rename. Independently of the flag, a rename ENOENT
  acks `not_found` (was: `io_error`). Staging + AlreadyExists/force handling
  is unchanged.
- New `runMkdir(stream, full, parents bool)`:
  1. `os.Lstat(full)`: exists and is not a dir → ack `not_a_directory`
     (both modes); exists and is a dir → ack `already_exists` when
     `parents=false`, ack `ok` (idempotent) when `parents=true`.
  2. Otherwise `parents=false` → `os.Mkdir(full, 0o755)`: ENOENT (parent
     missing) → `not_found`, other error → `io_error`. `parents=true` →
     `os.MkdirAll(full, 0o755)`: error → `io_error`.
  3. Success → ack `ok`, actual_size 0.
  No payload bytes are read; the stream carries only the ack (same shape as
  `delete`).
- `handleOpenFileTransfer` dispatch gains `case FileTransferDirection_Mkdir`.

## Client (`cli/`)

- `OpenFileTransfer(ctx, taskIDHex, direction, relPath, expectedSize, force,
  mkdirParents bool)` — parameter added; pull/delete callers pass false.
- The three push helpers replace their `force bool` parameter with a
  `FilePushOpts{Force, MkdirParents bool}` struct (same direction as the
  recent SessionOpts refactor): `FilePush(ctx, task, local, remote, opts)`,
  `FilePushBytes(..., opts, onProgress)`, `FilePushDir(ctx, task, local,
  remote, opts)`.
- New `FileMkdir(ctx, taskIDHex, remoteRel string, parents bool) error` —
  opens a mkdir transfer (mkdir_parents ← parents), reads the ack, maps via
  `ackError("mkdir", ack)`.
- `ackError`: for ops `push` / `push --recursive`, the `not_found` message
  becomes "destination parent directory does not exist (use -p/--parents or
  file mkdir)". For op `mkdir`: `not_found` reads "parent directory does not
  exist (use -p/--parents)", `already_exists` reads "directory already
  exists", `not_a_directory` reads "exists and is not a directory".
- New `IsNotFound(err error) bool` helper next to `IsAlreadyExists`, for the
  WebUI confirm-retry branch.
- `server/file_transfer.go handleOpenFileTransfer` copies the new bit:
  `body.SetMkdirParents(req.MkdirParents())`. No other server change (mkdir
  rides the existing splice path; capability gating is unchanged because the
  TaskControlKind is unchanged).

## Operator surfaces (Pitfall 9 matrix)

| Surface | -p/--parents | file mkdir |
| --- | --- | --- |
| CLI binary | `file push [-p\|--parents]` flag; usage string + help text | `file mkdir [-p\|--parents] TASK_ID WORKTREE_REL_DIR` subcommand; usage + help |
| TUI cmdline | `-p` in the `file push` FlagSet; `FilePushAction` gains `Parents bool`, threaded to `DoFilePush` | `file mkdir [-p] <task-id> <rel>` parsed to new `FileMkdirAction` (with `Parents bool`), executed via new `DoFileMkdir(c *cli.Client, ...)` (long-lived client, `*With`-style pattern like sibling Do* helpers) |
| TUI filepicker | Push path intentionally omitted: the picker's push destination is always the currently-browsed (existing) directory + basename, so a missing parent cannot occur on that path | New-folder key (`+`) opens a name-input mode (existing input/confirm state-machine pattern); creates via `FileMkdir` with parents=true (the typed name may be nested, e.g. `a/b/c`, and the result is immediately visible in the listing) |
| WebUI file browser | On push failure with `IsNotFound`, `confirm("...親ディレクトリが存在しません。作成して再試行しますか?")` → retry with parents=true (mirrors the existing AlreadyExists→confirm→force retry) | "New folder" button next to the existing browser actions: `prompt()` for the name, `fileMkdir` with parents=true, refresh listing (parity with the TUI picker key) |
| WebUI cmdline | Same confirm-retry in `filePushCmd` | `file mkdir [-p] <task-id> <rel>` command + help text |
| WASM bridge | `harness.filePushBytes(taskID, remoteRel, data, force, parents[, onProgress])` — parents inserted before the optional callback; both form and cmdline JS callers updated | `harness.fileMkdir(taskID, rel, parents) -> Promise<void>` |
| Shared cli/server/runner | as described above | as described above |

The wasm bridge signature change is breaking for main.js callers — all call
sites are updated in the same change (they live in this repo; no external
users).

## Testing

- Protocol round-trip test for the new bit + new direction (follow the
  existing message_test patterns).
- Runner-level tests: push into a missing dir without the flag acks
  `not_found`; with the flag succeeds and creates parents; dir_push likewise;
  strict mkdir acks `not_found` on a missing parent and `already_exists` on
  an existing dir; `-p` mkdir creates nested dirs and is idempotent; both
  modes reject an existing regular file with `not_a_directory`. Symlinked
  intermediate still rejected (existing `rejectIfSymlinkInPath` coverage
  extended to mkdir dispatch).
- Integration (`integration/file_transfer_e2e_test.go`): end-to-end -p push
  and mkdir cases.
- `scripts/wire-skew-check.sh` before landing (`.bgn` changed).

## Landing / deployment

- Work in the parent repo `/home/kforfk/workspace/remote-agent-harness/`
  (NOT the harness worktree), Mode A local-trunk FF push.
- `make build` after landing.
- Deployment order (Pitfall 10): server restart FIRST, then `/restart-all`
  for runners. Old-runner/new-server skew during the window degrades to the
  pre-change behavior (bit ignored / mkdir rejected), which is acceptable.

## Decisions taken

- Explicit opt-in flag, not always-on auto-mkdir (user choice).
- `mkdir` is strict by default (`os.Mkdir`) and honors the same
  `mkdir_parents` bit for MkdirAll semantics — Unix mkdir / mkdir -p
  isomorphism, zero extra wire cost (user choice, revised from an earlier
  always-MkdirAll draft).
- `not_found` reused for missing-parent on push and strict mkdir; no new
  status value.
- Push helpers move to `FilePushOpts`; low-level `OpenFileTransfer` stays
  positional.
- TUI filepicker gets a new-folder key and the WebUI file browser a
  new-folder button (user choice); both create with parents=true because the
  typed name may be nested and the result is immediately visible. The
  strict/-p distinction is exercised via CLI and the two cmdlines.
- The picker's push path keeps no `-p` handling; the omission rationale is
  recorded in the surface matrix per Pitfall 9.
