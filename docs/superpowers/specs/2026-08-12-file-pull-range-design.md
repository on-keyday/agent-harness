# Ranged file pull

**Date:** 2026-08-12
**Status:** Approved (brainstorming)

## Problem

`filePullBytes` transfers whole files only, so every consumer that wants a
prefix pays for the whole thing. The WebUI preview is the concrete case: it
shows at most 4 KiB of a binary file as a hex dump (`HEX_PREVIEW_MAX_BYTES`,
`webui/static/main.js`) but pulls up to its full size cap to get there, and it
refuses anything over the cap outright rather than showing its head.

Verbatim: *"うーんまあ今は足りてるけどあれだからrange対応拡張を入れてほしい"* — the
current caps suffice, so this is not urgent; it is the capability that removes
the class of problem instead of moving the number again.

## What already exists

- `runPull` (`runner/file_transfer.go:314`) opens the file, stats it, acks
  `{ok, actual_size = st.Size()}` (`:331`), then `io.Copy`s the whole body.
- `filePullDo` (`cli/file_pull.go:78`) reads that ack and hands `ActualSize` to
  its body callback as "how many bytes to expect"; `FilePull` and
  `FilePullBytes` both treat a short read as an error.
- `FileTransferAck` is fixed-size and its width is exported as
  `FileTransferAckSize` (`runner/protocol/message.go:28954`, currently **9**) so
  readers pre-allocate exactly that many bytes (`cli/file_transfer.go:126-130`,
  `runner/file_transfer.go:124`).
- The server relays the open by rebuilding a `RunnerOpenFileTransferRequest`
  field by field (`server/file_transfer.go:51-59`), so any new request field
  must be copied there or it is silently dropped.

**The finding that shapes this design:** `actual_size` already means "bytes
moved by this transfer" everywhere except pull — push acks `written`
(`runner/file_transfer.go:381`), dir_pull acks `bytesIn` (`:564`),
delete/mkdir ack 0. Only pull uses it as "size of the file" (`:331`), and for a
whole-file pull the two are the same number, which is why the difference has
never mattered. Redefining it as "bytes in this transfer" therefore changes no
existing behaviour and makes pull consistent with its siblings.

## Decisions

1. **Range applies to `pull` only.** `dir_pull` streams a generated tar whose
   byte offsets mean nothing stable, and the write directions have no read to
   slice.
2. **A range on any other direction is rejected**, not ignored, with a new
   `range_invalid` status. Ignoring it would hand a caller who asked for a slice
   the whole object — a silent wrong answer rather than a loud refusal.
3. **`length = 0` means "to EOF".** The zero value is therefore "whole file",
   so every existing caller keeps its exact meaning without being touched.
4. **`actual_size` becomes "bytes in this transfer" and `total_size` is added**
   to `FileTransferAck`. A ranged read that cannot report the whole size forces
   the caller into a second `ListFiles` that races the pull; HTTP settles this
   the same way with `Content-Range: bytes 0-999/12345`. For every non-pull
   direction the two are equal by construction, which is what `writeAck` keeps
   encoding.
5. **`offset` past EOF is `ok` with an empty body**, not an error. A caller
   paging through a file that shrank must be able to tell "past the end" from
   "the read failed", and an error status collapses them.
6. **The client carries the range as one value type**, `FileTransferRange`,
   rather than two more positional `uint64`s. `OpenFileTransfer` already takes
   seven parameters including `expectedSize`; three adjacent bare `uint64`s at
   a call site is a defect waiting to happen.
7. **The preview decides how much to pull from the extension where it can.**
   HTML and images are known by name, so they pull in one request; everything
   else pulls 4 KiB first, sniffs, and only continues for text. This is the
   part of the design that exists purely to stop pulling megabytes to render a
   4 KiB hex dump, and it can be dropped without affecting anything else.

## Schema delta

All of it, in one place: `runner/protocol/message.bgn`, regenerated with
`make protoregen`.

```
format FileTransferAck:
    status      :FileTransferStatus
    actual_size :u64   # bytes carried by THIS transfer's body. A ranged pull
                       # sends only the requested slice, so this is the slice.
    total_size  :u64   # NEW. Size of the complete object: the file size for a
                       # pull (ranged or not). Every other direction transfers
                       # the whole object, so writeAck sets it equal to
                       # actual_size; delete / mkdir leave both 0.

enum FileTransferStatus:
    ...
    range_invalid = "range_invalid"   # offset/length set on a direction that
                                      # has no range (anything but pull).

# Appended to BOTH request formats, after expected_size and before the flag
# byte, so the two sizes read together:
format OpenFileTransferRequest:
    ...
    expected_size :u64
    offset        :u64   # pull only: first byte to send. 0 = from the start.
    length        :u64   # pull only: max bytes to send. 0 = to EOF.
    force         :u1
    ...

format RunnerOpenFileTransferRequest:
    ...same two fields, same meaning...
```

`FileTransferAckSize` goes 9 → 17. **This is a breaking wire change**: a new
client against an old runner misreads the ack. The fleet is rebuilt and
restarted together (`scripts/build_and_restart_all.py`), which is the normal
operation for this repo.

## Architecture

### Runner

`runPull(stream, full, offset, length)`:

- `total = st.Size()`.
- `offset >= total` → ack `{ok, actual: 0, total}` and an empty body (decision 5).
- `n = total - offset`; if `length > 0 && length < n` then `n = length`.
- ack `{ok, actual: n, total}`, `f.Seek(offset, io.SeekStart)`,
  `io.CopyN(stream, f, n)`.

`handleOpenFileTransfer` rejects a range on any other direction with
`range_invalid` before dispatching.

`writeAck(st, status, size)` keeps its signature and sets `TotalSize = size`,
so the nine existing call sites are untouched and decision 4's "equal by
construction" lives in one place. The ranged pull uses a new
`writeAckRange(st, status, actual, total)`.

### Server

`server/file_transfer.go` copies `Offset` and `Length` into the runner request
next to `ExpectedSize`. Nothing else server-side changes: the relay is a splice
and does not read the body.

### Client

```go
// FileTransferRange selects a byte range of a pull. The zero value is the
// whole file, so existing call sites keep their meaning.
type FileTransferRange struct {
    Offset uint64
    Length uint64 // 0 = to EOF
}
```

- `OpenFileTransfer(ctx, taskIDHex, direction, relPath, expectedSize, rng, force, mkdirParents)`
  — the seven existing call sites pass `FileTransferRange{}`.
- `filePullDo(ctx, taskIDHex, remoteRel, rng, body)` where
  `body(stream trsf.BidirectionalStream, n, total uint64) error`. `n` is what
  to read; `total` is what the caller reports.
- `FilePullBytes(ctx, task, rel, onProgress)` keeps its signature and its
  meaning (whole file).
- `FilePullBytesRange(ctx, task, rel, rng, onProgress) (data []byte, total uint64, err error)`
  is added.

### Surfaces

- **CLI:** `file pull --offset N --length N`.
- **TUI:** `file pull -o N -n N`.
- **WebUI:** `harness.filePullBytesRange(taskID, rel, offset, length[, onProgress])
  -> Promise<{bytes: Uint8Array, total: number}>`. A separate binding rather than
  more optional positional arguments on `filePullBytes`, matching the Go split.

### Preview

| Case | Behaviour |
|---|---|
| HTML / image (known by extension) | pull `[0, min(size, cap))` in one request; if truncated, render with a "showing the first X of Y" note instead of refusing |
| anything else | pull `[0, 4 KiB)`, sniff with `isLikelyBinary`; binary → done, that is all the hex dump shows; text → pull `[4 KiB, cap)` and concatenate |

Oversize no longer refuses; it truncates and says so.

## Testing

- **Runner unit:** a mid-file offset, `length` shorter than the remainder,
  `length` longer than the remainder (clamped), `offset == size` (ok, empty),
  `offset > size` (ok, empty), and a range on `dir_pull` / `push` →
  `range_invalid`. Each asserts `total_size` is the file size while
  `actual_size` is the slice.
- **Ack round trip:** encode/decode at the new 17-byte width, and
  `FileTransferAckSize == 17`.
- **Client unit:** `FilePullBytesRange` returns the slice and the total; a short
  read against the acked `actual_size` is still an error.
- **Dummy harness:** `file pull --offset --length` output byte-identical to
  `dd` over the same file; a 5 MiB `.log` previews as a head with the note; a
  binary file transfers ~4 KiB rather than its full size (checked from the
  runner side, not from the browser).

## Known limitations

- Only `pull` is rangeable. A ranged `dir_pull` would need a stable tar layout,
  which the runner does not promise.
- The two-step sniff costs one extra round trip for text files with no
  identifying extension. Files with a known extension pay nothing.
- Nothing resumes a partial transfer; range is a read primitive here, not a
  resume protocol.
