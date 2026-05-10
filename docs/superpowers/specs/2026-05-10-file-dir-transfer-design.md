# Directory Transfer + --force Mode — Design

**Date:** 2026-05-10
**Status:** approved (extends the file transfer feature with directory variants)

## Motivation

Single-file `harness-cli file {push|pull}` is in. Users want to move whole
directories (datasets, generated artifacts, sub-projects) without scripting
around it. Also, the `O_EXCL` default for push is the safe choice but there
is currently no escape hatch for "yes I really do want to overwrite".

This spec covers:

1. `harness-cli file push --recursive` / `pull --recursive` for whole-tree
   transfer.
2. `--force` flag for explicit overwrite, applied uniformly across single
   files and directories.

## Scope

In:

- Recursive push: client local dir → worktree subdir.
- Recursive pull: worktree subdir → client local dir.
- `--force` for push (single + recursive) and pull (single + recursive).
- Atomic-on-the-receiver: dir transfers stage to `.harness-staging/<random>/`
  and `rename` into place once the entire stream completes successfully.

Out:

- Recursive `file ls --recursive` (separate concern; can be a follow-up).
- Recursive `file delete` (rmdir; deferred — risky enough to want its own
  spec/UX).
- Resume / partial transfer across disconnect (full retry).
- Hard links, sparse files, device files, FIFOs (skip with no-op; warn in
  client output).
- Cross-filesystem rename (would `EXDEV`; staging is constrained to the
  same fs as dest).
- Compression on the wire.

## Wire format

The body of a directory transfer is a **tar stream** carried on the same
trsf bidi stream that single-file transfer uses. Both client and runner use
`archive/tar` from the Go stdlib — no shelling out to a `tar` binary, no
new external dependencies. The wire is "tar" (well-known, debuggable);
implementation is "pure Go" (no subprocess, no resource bounds to manage).

The tar header carries POSIX file mode, name, and size. Symlinks
(`tar.TypeSymlink`), hard links, devices, and FIFOs are **rejected at the
receiver** for v1 — the worktree only contains regular files and
directories. This collapses with the existing
`runner/file_transfer.go::rejectIfSymlinkInPath` defense: tar entries
naming `..` or absolute paths are rejected the same way `ValidateRelPath`
already rejects them for single-file ops.

### Schema additions

In `runner/protocol/message.bgn`, append-only changes:

```
enum FileTransferDirection:
    :u8
    push
    pull
    delete
    dir_push        # client → runner; body is a tar stream of the source dir.
    dir_pull        # runner → client; body is a tar stream of the source dir.

enum FileTransferStatus:
    :u8
    ok             = "ok"
    path_invalid   = "path_invalid"
    not_found      = "not_found"
    already_exists = "already_exists"
    io_error       = "io_error"
    canceled       = "canceled"
    is_directory   = "is_directory"
    not_a_directory = "not_a_directory"   # dir_pull only: rel_path resolved to a regular file.
```

`OpenFileTransferRequest` and `RunnerOpenFileTransferRequest` each gain a
trailing flag byte:

```
format OpenFileTransferRequest:
    task_id       :TaskID
    direction     :FileTransferDirection
    rel_path_len  :u16
    rel_path      :[rel_path_len]u8
    expected_size :u64
    force         :u1                      # push / dir_push: permit overwrite.
    reserved      :u7

format RunnerOpenFileTransferRequest:
    task_id       :TaskID
    stream_id     :u64
    direction     :FileTransferDirection
    rel_path_len  :u16
    rel_path      :[rel_path_len]u8
    expected_size :u64
    force         :u1
    reserved      :u7
```

This is a wire-breaking change (1 trailing byte added to two formats). Per
the project's individual-dogfood scope, that is acceptable; existing
client/server/runner binaries must be rebuilt together.

## --force semantics

`--force` is a single CLI flag with consistent meaning ("permit
overwriting destination") whose mechanics differ by op:

| Op | Default | --force |
|----|---------|---------|
| push (file) | runner: `O_EXCL` → `AlreadyExists` if dest exists | runner: `O_TRUNC` overwrites |
| pull (file) | client: `O_EXCL` → fail if local exists | client: `O_TRUNC` overwrites local |
| push --recursive (dir) | runner: reject if remote dest exists | runner: `RemoveAll(dest)` then rename staging |
| pull --recursive (dir) | client: reject if local dest exists | client: `RemoveAll(local)` then rename staging |
| delete (file) | unlink | (--force ignored — no overwrite semantics here) |

The wire `force` flag is interpreted **runner-side for push / dir_push only**.
For pull / dir_pull, the runner ignores it; the client applies its own
`O_EXCL` / `O_TRUNC` / `RemoveAll` decision based on the user's flag.

Note: single-file pull's existing default was `O_TRUNC`. This spec **changes
that default to `O_EXCL`** for symmetry with push. Without `--force`, pull
now refuses to clobber a local file — a safety improvement that
matches the push side. Per individual-dogfood scope, this is a quality fix,
not a migration concern.

## Staging dir + atomic rename

Directory transfers stage to a sibling directory and `rename(2)` into
place. This gives the agent (claude inside the worktree) an all-or-nothing
view: at no point does it observe a half-extracted tree.

**dir_push (runner)**:

1. Validate `rel_path`. Compute `dest := <worktree>/<rel_path>`.
2. Stat `dest`:
   - If exists and `!force` → ack `AlreadyExists`, close stream, no staging
     created.
   - If exists, is a regular file (not dir), and `force` → ack `IsDirectory`
     (won't replace a file with a dir; user should `delete` first).
3. `staging := <worktree>/.harness-staging/<random>/`.
   `os.MkdirAll(staging, 0o755)`.
4. `tar.NewReader(stream)`. For each header:
   - Validate `header.Name` via `ValidateRelPath(staging, header.Name)`.
     Reject on error (ack `PathInvalid`, `RemoveAll(staging)`).
   - Reject `header.Typeflag` other than `Reg` / `Dir`. Symlinks and
     specials → ack `PathInvalid`, `RemoveAll(staging)`.
   - For dir: `MkdirAll(target, header.Mode & 0o777)`.
   - For regular file: `OpenFile(target, O_WRONLY|O_CREATE|O_EXCL, header.Mode & 0o777)`,
     `io.Copy(f, tr)`, `f.Sync()`, `f.Close()`. (`O_EXCL` against entries
     of the *same name* in the tar; staging dir is fresh so `O_EXCL` cannot
     conflict with pre-existing content.)
5. After the tar EOF:
   - If `force && dest exists`: `RemoveAll(dest)`.
   - `os.Rename(staging, dest)`. (Atomic since dest does not exist after
     RemoveAll.)
6. Ack `Ok`. Close stream.

On any error in steps 4–5: `RemoveAll(staging)`, ack appropriate status,
close. The `.harness-staging/` parent directory is not removed; future
transfers reuse it. Crashed-runner cleanup is left to the existing
`harness-cli prune-local` family (out of scope for this spec).

**dir_pull (runner)**:

1. Validate `rel_path`. Compute `src := <worktree>/<rel_path>`.
2. Stat `src`:
   - Missing → ack `NotFound`, close.
   - Regular file (not dir) → ack `NotADirectory`, close.
3. Ack `Ok` first (consistent with single-file pull's "ack-then-body").
4. `tar.NewWriter(stream)`. `filepath.WalkDir(src, ...)`:
   - For each dir/file: emit a `tar.Header` (use `tar.FileInfoHeader`).
     `header.Name = filepath.Rel(src, path)` (worktree-relative).
   - Skip symlinks, hard links, devices, FIFOs (don't `WriteHeader` for
     them; the client never sees them).
   - For regular files: `tw.WriteHeader(header)` then `io.Copy(tw, srcFile)`.
5. `tw.Close()`, then close stream.

**dir_pull (client)**: mirrors push staging on the local side:

1. `staging := <localDir>.staging-<random>/`. `MkdirAll(staging, 0o755)`.
2. Read ack first.
3. If ack != ok → return error.
4. `tar.NewReader(stream)`. Same per-entry validation as runner-side
   `dir_push`: reject `..`, reject non-Reg/Dir.
5. After EOF:
   - If `force && localDir exists`: `RemoveAll(localDir)`.
   - `os.Rename(staging, localDir)`.

Same staging filesystem rule: `<localDir>.staging-<random>/` is a sibling
of `<localDir>` so `rename` works without `EXDEV`.

## CLI

```
harness-cli file push [-r|--recursive] [-f|--force] TASK_ID LOCAL_SRC WORKTREE_REL_DST
harness-cli file pull [-r|--recursive] [-f|--force] TASK_ID WORKTREE_REL_SRC LOCAL_DST
```

`--recursive` requires both `LOCAL_*` and `WORKTREE_REL_*` to refer to
directories; otherwise the operation is rejected. Without `--recursive`,
the args refer to single files and the existing single-file path applies.

`--force` is accepted on both push and pull regardless of `--recursive`;
its meaning is the table above.

## Test scenarios

Unit (runner):

- `dir_push` happy path: 2 files + 1 nested dir; verify all extracted under
  worktree; verify staging dir gone after success.
- `dir_push` reject existing dest: ack `AlreadyExists`; verify no staging
  left behind.
- `dir_push` --force replaces existing dir: verify old dest contents
  vanished, new contents present.
- `dir_push` rejects symlink entry in tar: ack `PathInvalid`; staging gone.
- `dir_push` rejects `../escape` entry in tar: ack `PathInvalid`; staging
  gone.
- `dir_pull` happy path: 2 files + 1 nested dir; verify tar stream decodes
  to the right entries.
- `dir_pull` reject not-a-directory: ack `NotADirectory`.

Unit (client):

- `FilePushDir` happy path: walk a tempdir, write tar to a fake stream,
  decode, verify entries.

Integration (e2e, real server + runner):

- Round-trip: push a 3-file dir, pull it back, byte-compare.
- `--force` overwrite: push twice with --force; second push wins.
- Without --force, second push fails with `already exists`.

## Out-of-scope follow-ups

- `file ls --recursive` (deeper listing).
- `file delete --recursive` (rmdir; needs its own UX/safety story).
- Hard link / symlink preservation (would need an opt-in flag and trust
  story).
- Compression at the wire layer.
- Resume across disconnect.
- Concurrent multi-file dir transfer (1-stream-per-file architecture).
