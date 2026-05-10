# File Transfer (client ↔ runner) — Design

**Date:** 2026-05-10
**Status:** approved (scope: client ↔ runner only; runner ↔ runner deferred)

## Motivation

Users running a task in a remote runner currently have no first-class way to
push input files into the worktree (datasets, configs, fixtures) or pull
generated artifacts back to their local machine. The only paths today are:

- Commit + push via git (heavy, leaks intermediate artifacts into history)
- Copy-paste through the interactive PTY (impractical for binary / large)
- agentboard `send` (envelope-bound, payload size cap)

A direct file-transfer primitive between client and runner closes the gap
without inflating the agentboard or overloading the PTY stream.

## Scope

In:

- **push** (`client → runner worktree`)
- **pull** (`runner worktree → client`)
- **ls** (`list a single directory under the worktree`) — needed so the user
  can find the path to pull without guessing.
- Triggered by an explicit `harness-cli file {push|pull|ls}` invocation
  against an existing, active task

Out:

- Runner ↔ runner / agent ↔ agent file transfer (separate spec, leverages
  agentboard's existing `PayloadStreamId` mechanism)
- TUI/WebUI affordances (CLI only in v1)
- Resumable / chunked transfers across reconnect
- Symlink, directory, or recursive copy
- Files outside the worktree (e.g. arbitrary paths on the runner host)

## Non-goals

- Replacing `git`, `scp`, or `rsync`. This is a low-friction primitive for
  the common case "drop one file into the agent's worktree" / "grab one
  artifact out".

## Architecture

The hub-and-spoke topology is unchanged. Client and runner each terminate at
the server; the server is the only process that bridges them. All bytes
travel over the existing `trsf` stream multiplexer on top of objproto / WS.

```
┌────────┐   TaskControl{open_file_transfer}   ┌────────┐   RunnerRequest{open_file_transfer}   ┌────────┐
│ client │ ────────────────────────────────▶  │ server │ ───────────────────────────────────▶  │ runner │
│        │ ◀───── TaskControlResp{stream_id}  │        │                                       │        │
└───┬────┘                                    └────┬───┘                                       └────┬───┘
    │  bidi trsf stream (server-allocated, splicedBidi)                                              │
    └────────────────────────────────────────────────────────────────────────────────────────────────┘
```

The server allocates **two** bidi streams (one per leg) and splices them with
the existing `spliceBidi` helper (`server/task_handler.go:650`) — exactly the
same pattern as `handleOpenInteractive`. The transferred file bytes are pure
payload; no per-byte server-side processing.

## Wire format

All additions live in `runner/protocol/message.bgn`. No new `.bgn` file.

### Shared

```
enum FileTransferDirection:
    :u8
    push        # client → runner (worktree に書く)
    pull        # runner → client (worktree から読む)

enum FileTransferStatus:
    :u8
    ok             = "ok"
    path_invalid   = "path_invalid"     # rel_path empty / absolute / contains "..".
    not_found      = "not_found"        # pull only: file does not exist.
    already_exists = "already_exists"   # push only: O_EXCL collision.
    io_error       = "io_error"         # generic open/read/write failure.
    canceled       = "canceled"         # peer closed stream early.

format FileTransferAck:
    status      :FileTransferStatus
    actual_size :u64                    # pull: file size; push: bytes runner persisted.

# A single directory entry returned by `ls`. `is_dir` is set for sub-directories;
# `size` is the file size for regular files (undefined for directories).
format FileEntry:
    name_len :u16
    name     :[name_len]u8
    size     :u64
    mode     :u32                       # POSIX mode bits, low 12 bits significant.
    is_dir   :u1
    reserved :u7

# Encoded onto the ls stream after a successful ListFilesResponse.
format FileListing:
    count   :u32
    entries :[count]FileEntry
```

### Client → server (TaskControl envelope)

```
enum TaskControlKind:
    :u8
    ...
    open_file_transfer
    list_files

format OpenFileTransferRequest:
    task_id       :TaskID
    direction     :FileTransferDirection
    rel_path_len  :u16
    rel_path      :[rel_path_len]u8     # worktree-relative POSIX path
    expected_size :u64                  # push only (0 = unknown).

enum OpenFileTransferStatus:
    :u8
    ok                = "ok"
    no_such_task      = "no_such_task"     # task_id unknown / finished.
    runner_offline    = "runner_offline"   # task exists but runner disconnected.
    internal_error    = "internal_error"

format OpenFileTransferResponse:
    status    :OpenFileTransferStatus
    stream_id :u64                      # populated when status == ok.

format ListFilesRequest:
    task_id      :TaskID
    rel_path_len :u16
    rel_path     :[rel_path_len]u8      # empty = worktree root; subdir name otherwise.
                                        # Single dir; not recursive in v1.

enum ListFilesStatus:
    :u8
    ok               = "ok"
    no_such_task     = "no_such_task"
    runner_offline   = "runner_offline"
    path_invalid     = "path_invalid"   # rel_path escapes worktree.
    not_found        = "not_found"      # rel_path does not exist.
    not_a_directory  = "not_a_directory"
    io_error         = "io_error"
    internal_error   = "internal_error"

format ListFilesResponse:
    status    :ListFilesStatus
    stream_id :u64                      # populated when status == ok; carries the
                                        # encoded FileListing payload (see framing).
```

`TaskControlRequest` / `TaskControlResponse` switch arms are added for
`TaskControlKind.open_file_transfer` and `TaskControlKind.list_files`.

### Server → runner (RunnerRequest envelope)

```
enum RunnerRequestType:
    :u8
    ...
    open_file_transfer
    list_files

format RunnerOpenFileTransferRequest:
    task_id       :TaskID
    stream_id     :u64                  # server-allocated bidi stream toward runner.
    direction     :FileTransferDirection
    rel_path_len  :u16
    rel_path      :[rel_path_len]u8
    expected_size :u64

format RunnerListFilesRequest:
    task_id      :TaskID
    stream_id    :u64                   # server-allocated bidi stream toward runner.
    rel_path_len :u16
    rel_path     :[rel_path_len]u8      # empty = worktree root.
```

`RunnerRequest` switch arms added for `RunnerRequestType.open_file_transfer`
and `RunnerRequestType.list_files`.

Note: no `auth_ticket` field. The server↔runner connection is already
PSK-authenticated and no child process is spawned, so there is nothing to
inject a ticket into. (Contrast with `OpenExecRunnerRequest` whose ticket
exists solely to be propagated as `HARNESS_AUTH_TICKET` to a spawned claude.)

## Stream framing

### push / pull

The bidi stream carries a `FileTransferAck` followed by file bytes. The order
differs by direction:

**push (client → runner):**

1. Client writes file bytes to its end of the stream until EOF.
2. Runner reads bytes, writes to `<worktree>/<rel_path>` with `O_EXCL`.
3. After EOF + `fsync`, runner writes a single `FileTransferAck` to its end
   then closes the stream.
4. Client reads the ack to learn the final status.

**pull (runner → client):**

1. Runner opens `<worktree>/<rel_path>`. Writes `FileTransferAck` (with
   `actual_size` set on success) to the stream first.
2. If status == ok, runner streams file bytes then EOFs.
3. If status != ok, runner closes immediately after the ack.
4. Client reads ack, then bytes (only if ok).

Rationale for ack-first on pull / ack-last on push:

- pull: open failure (not_found, path_invalid) is determinable up front.
  Sending ack first lets the client distinguish "file doesn't exist" from
  "transfer cut off mid-stream".
- push: success implies bytes have been durably written. Ack must follow
  fsync so the client knows the file is on disk.

`FileTransferAck` is encoded with its on-the-wire byte length as a `u32` BE
length prefix on the stream (the `.bgn` `format` does not self-delimit), so
the reader can buffer exactly the ack and then switch to raw byte reads.

### ls

The runner writes a single `FileListing` payload to the bidi stream and
closes the write side. The wire encoding self-delimits via `count` so no
length prefix is needed. The client reads the entire payload to EOF and
decodes once.

If the runner fails after the ok response but before writing the listing
(e.g. the directory was deleted between open and readdir), it closes the
stream without writing — the client surfaces this as `io.ErrUnexpectedEOF`.

## Server behavior

`server/task_handler.go` gets `handleOpenFileTransfer(conn, req)`:

1. Look up the task by `req.TaskId`. If not found or in terminal state →
   `OpenFileTransferStatus_NoSuchTask`.
2. Resolve the assigned runner from the task. If `runner.Conn == nil` →
   `OpenFileTransferStatus_RunnerOffline`.
3. Allocate `clientStream := conn.CreateBidirectionalStream()`. On nil →
   `InternalError`.
4. Allocate `runnerStream := runner.Conn.CreateBidirectionalStream()`. On nil →
   close `clientStream`, return `InternalError`.
5. Send `RunnerRequest{open_file_transfer, RunnerOpenFileTransferRequest{
   task_id, stream_id=runnerStream.ID(), direction, rel_path, expected_size}}`.
   On send error → close both streams, return `InternalError`.
6. Spawn `go spliceBidi(clientStream, runnerStream, taskIDHex)`.
7. Return `OpenFileTransferResponse{ok, stream_id=clientStream.ID()}`.

`server/task_handler.go` gets `handleListFiles(conn, req)`, structured the
same way as `handleOpenFileTransfer`:

1. Same task lookup / runner resolution / stream allocation as above.
2. Send `RunnerRequest{list_files, RunnerListFilesRequest{task_id,
   stream_id, rel_path}}`.
3. Spawn `go spliceBidi(clientStream, runnerStream, taskIDHex)`.
4. Return `ListFilesResponse{ok, stream_id=clientStream.ID()}`.

`spliceBidi` is reused as-is for both: the framing happens at the
endpoints, not the server.

## Runner behavior

`runner/session.go` gets `handleOpenFileTransfer(ctx, oer)`:

1. Acquire the server-allocated stream via
   `peer.WaitForBidirectionalStream(ctx, s.Streams, oer.StreamId)`. nil →
   log + return (no way to report; server's spliceBidi will see EOF).
2. Look up `s.tasks[hex(oer.TaskId)]` to find `repoPath`. Missing → write
   `FileTransferAck{status=io_error}`, close.
3. Compute worktree dir: `s.NoWorktree ? repoPath : <repoPath>/.harness-worktrees/<taskIDHex>`.
4. Validate `oer.RelPath`:
   - Reject empty.
   - Reject absolute (starts with `/`).
   - After `filepath.Clean`, reject if it starts with `..` or contains a
     `..` segment.
   - Reject if it contains a NUL byte.
   - On any rejection → write `FileTransferAck{status=path_invalid}`, close.
5. Compute `fullPath := filepath.Join(worktreeDir, cleanRelPath)`. As a
   defense-in-depth check, verify
   `filepath.HasPrefix(fullPath, worktreeDir+string(filepath.Separator))`.
6. Branch on direction:
   - **pull**: `os.Open(fullPath)`. On `os.IsNotExist` → ack `not_found`. On
     other error → `io_error`. On success → ack `ok` with `actual_size = stat.Size()`,
     then `io.Copy(stream, file)`, then close-write side. (No fsync: read-only.)
   - **push**: `os.OpenFile(fullPath, O_WRONLY|O_CREATE|O_EXCL, 0o644)`. On
     `os.IsExist` → ack `already_exists` (do not consume client's stream:
     the client expects the runner to close immediately on error; spliceBidi
     teardown will release the client's send side). On success: read from
     stream until EOF, write to file, fsync, close, then ack `ok` with
     `actual_size = bytes_received`.

Concurrency note: file I/O is performed on the dispatch goroutine assigned
to the request. Multiple file transfers against the same task run in
parallel; the OS handles concurrent reads. Concurrent push to the same path
is blocked by `O_EXCL` (one wins, the other gets `already_exists`).

`runner/session.go` gets `handleListFiles(ctx, req)`:

1. Acquire the server-allocated stream as in `handleOpenFileTransfer`.
2. Look up task → repoPath → worktreeDir, validate `req.RelPath` with the
   same shared helper.
3. `os.Stat(fullPath)` → if missing, write a small error sentinel listing
   (count=0) and close. (Status is reported via the `ListFilesResponse`
   envelope path for `path_invalid` / `not_found` only when detectable
   server-side; runner-side detection writes an empty listing as a
   degraded-but-safe response. v2 may add an in-stream status header.)
   For v1, the runner emits an empty `FileListing{count=0}` on any failure
   after the open-stream point. The status code is set in
   `ListFilesResponse` only for failures the **server** can determine
   without consulting the runner (no_such_task / runner_offline).
4. `os.ReadDir(fullPath)`, sort entries by name, encode each as `FileEntry`
   into a single `FileListing`, write to the stream, close write side.

Maximum entries per directory is uncapped in v1; clients that ask for
huge directories pay the proportional bytes. The 64 KiB read chunk in
`spliceBidi` keeps the relay efficient regardless of total payload size.

## Client behavior

`cli/open_file_transfer_native.go`:

```go
func (c *Client) OpenFileTransfer(
    ctx context.Context,
    taskIDHex string,
    direction protocol.FileTransferDirection,
    relPath string,
    expectedSize uint64,
) (trsf.BidirectionalStream, error)
```

- Build `OpenFileTransferRequest`, send via `c.RoundTripTaskControl`.
- Map non-Ok status to a Go error.
- Wait for the bidi stream via `peer.WaitForBidirectionalStream`.
- Return the stream; caller drives push / pull byte loops.

A second client method `(c *Client) ListFiles(ctx, taskIDHex, relPath)
([]FileEntryView, error)` wraps the `list_files` round-trip: send the
request, read the encoded `FileListing` to EOF, decode, return the entries.
`FileEntryView` is a Go-side struct (Name, Size, Mode, IsDir) so callers
do not have to import the brgen-generated `FileEntry` type.

CLI-level wrappers in `cli/file_push.go`, `cli/file_pull.go`, `cli/file_ls.go`:

```
harness-cli file push <task-id> <local-src> <worktree-rel-dst>
harness-cli file pull <task-id> <worktree-rel-src> <local-dst>
harness-cli file ls   <task-id> [<worktree-rel-dir>]
```

- `push`: `os.Open(local)`, `Stat()` for `expected_size`, call
  `OpenFileTransfer(push)`, `io.Copy(stream, file)`, half-close write side
  (`stream.AppendData(true)`), read ack, surface error if non-ok.
- `pull`: call `OpenFileTransfer(pull)`, read ack first, error out if
  non-ok, otherwise `os.Create(local)` and `io.Copy(file, stream)`.
- `ls`: call `ListFiles`, print one line per entry as
  `<mode> <size> <name>[/]` (trailing slash for directories). Empty
  listing prints nothing and exits 0.

The local destination for `pull` is opened **only after** the ack confirms
success, so a failed pull does not leave a zero-byte file on the client.

## Security & containment

- The path validation in step 4 above is the only security boundary. It
  prevents a malicious client from writing outside the worktree
  (`../../../etc/passwd`) or reading arbitrary host files.
- PSK pre-auth gates the client connection upstream; only authenticated
  clients can issue `OpenFileTransfer`.
- File mode is hard-coded `0o644` for push. No execute bit, no umask
  weirdness. v2 may add `--mode` if needed.

## Failure modes

| Symptom | Cause | Visible to client as |
|---------|-------|----------------------|
| `OpenFileTransferStatus_NoSuchTask` | task_id unknown / finished | error from CLI |
| `OpenFileTransferStatus_RunnerOffline` | runner crashed | error from CLI |
| `FileTransferStatus_PathInvalid` | escape attempt / empty | error from CLI |
| `FileTransferStatus_NotFound` | pull target missing | error from CLI |
| `FileTransferStatus_AlreadyExists` | push collision | error; suggest `--force` (not in v1) |
| `FileTransferStatus_IoError` | disk full / permission denied | error with no detail (kept opaque to avoid leaking host paths) |
| stream closes mid-transfer | server / network failure | client read returns short / `io.ErrUnexpectedEOF` |

## Out-of-scope follow-ups

- Resume / chunked transfer with byte-offset (would require a
  `chunk_offset` field in `OpenFileTransferRequest` and a content-defined
  chunking scheme).
- sha256 integrity check (add to `FileTransferAck`).
- `--force` overwrite for push.
- Recursive directory transfer (would need a per-entry header on the
  stream, more like a tar pipe).
- TUI affordance (file picker → push, artifact list → pull).
