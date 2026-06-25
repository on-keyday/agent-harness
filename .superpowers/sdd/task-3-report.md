# Task 3 Report: board_read streams retained payloads

## Status

DONE_WITH_CONCERNS (pre-existing flaky test; see Concerns section).

## Files Changed

| File | Change |
|------|--------|
| `server/board_handler.go` | Added `handleBoardRead` (60 lines including comment); added `trsf` import |
| `server/task_handler.go` | Added `writeStreamAll` helper (9 lines); refactored `handleGetTaskLog` to call it; added `BoardRead` dispatch case (3 lines) |
| `server/board_handler_test.go` | Added `TestHandleBoardRead_StreamsPayloadsInOrder` (30 lines) |
| `server/fakes_test.go` | Extended `fakeConn` and `recordingSendStream` (additive — no existing behaviour removed) |

## writeStreamAll Extraction

`writeStreamAll` was NOT pre-existing. Added to `server/task_handler.go` (next to `handleGetTaskLog`):

```go
func writeStreamAll(s trsf.SendStream, p []byte) error {
    return s.AppendData(false, p)
}
```

`handleGetTaskLog`'s write loop was refactored: `stream.AppendData(false, buf[:n])` → `writeStreamAll(stream, buf[:n])`. `handleBoardRead`'s goroutine uses the same helper.

## Fake Stream Extension

`fakeConn` changes (all additive):

1. **`autoStreamCounter atomic.Uint64`** — when `nextSendStreamID == 0`, auto-allocates an ID via `Add(1)` so tests need not set `nextSendStreamID` explicitly.
2. **`done chan struct{}` + `closeOnce sync.Once` on `recordingSendStream`** — `Close()` closes the channel; `sendStreamBytes` waits on it before returning bytes.
3. **`sendStreamBytes(t, streamID uint64) []byte`** — method on `fakeConn`; finds stream by ID, `<-s.done`, returns bytes.
4. **`lastTaskControlResponse(t) *protocol.TaskControlResponse`** — method form on `fakeConn` returning pointer so pointer-receiver accessors like `BoardRead()` can be chained directly on the result.

Existing tests that set `nextSendStreamID` explicitly continue to work (explicit ID wins). Tests that don't set it now get an auto-allocated ID.

## go test ./server/ Command + Output

```
$ go test -count=1 ./server/
ok      github.com/on-keyday/agent-harness/server       0.703s
```

Full output (abbreviated, PASS only):
- `TestHandleBoardRead_StreamsPayloadsInOrder` — PASS
- `TestHandleBoardTopics_ListsTopics` — PASS (Task 2)
- `TestHandleBoardPurge_WholeAndSeq` — PASS (Task 2)
- All other existing tests — PASS

## Self-Review

- Handler mirrors `handleGetTaskLog` exactly: respond with stream_id first, stream payloads asynchronously via `go func() { defer stream.Close(); ... }()`.
- `writeStreamAll` is a single-line canonical wrapper — no logic duplication.
- `done` channel synchronisation is correct: goroutine writes via `AppendData(false, p)`, then `defer stream.Close()` closes `done`; `sendStreamBytes` reads bytes only after `<-s.done`. The Go memory model guarantees all prior writes happen-before the channel close.
- Cap gate (`InfoGlobal`) is confirmed present in `server/capabilities.go` line 23 — not re-added.
- Empty ring (`len(rows) == 0`) handled: responds Ok with `stream_id=0` (no stream created).
- `nil` stream (degraded path) handled: falls back to metadata-only response.
- `BoardMessageRow.FromTask` is assigned directly (`row.FromTask = m.FromTask`) rather than `copy()`; functionally equivalent, cleaner.

## Concerns

**Pre-existing flaky test**: `TestHandleOpenPortForward_RemoteRegisters` fails when run in isolation ~3/5 times both with and without these changes. This is a known race condition (noted in project memory as "flaky TestHandleOpenPortForward_RemoteRegisters noted") between `watchRemoteForwardControl` removing the registration via EOF on a `noopBidiStream` and the test asserting it is still present. The full `go test ./server/` run is reliable because goroutine scheduling under the larger test suite defers the competing goroutine long enough. My changes do not introduce this flakiness; confirmed by stash+run comparison.

## Commit SHA

`3c52740`

---

## Fix Pass: Review Findings (post-3c52740)

### Finding 1 — handleBoardRead EOF via AppendData(true)

**Root cause**: `handleBoardRead` used `defer stream.Close()` to signal stream EOF, but the proven sibling `handleGetTaskLog` signals EOF explicitly via `stream.AppendData(true)`. `stream.Close()` is NOT the verified EOF mechanism for this transport.

**Fix** (`server/board_handler.go`):
- Added `"log/slog"` import.
- Removed `defer stream.Close()` from the payload-writing goroutine.
- Added per-write error check: `slog.Warn("BoardRead: stream write failed", ...)` + `break` on error (mirrors `handleGetTaskLog`).
- Added `stream.AppendData(true)` after the loop with `slog.Warn("BoardRead: stream EOF failed", ...)` on error (mirrors `handleGetTaskLog`).

`handleBoardRead`'s stream finalization is now byte-for-byte the same mechanism as `handleGetTaskLog`.

### Finding 2 — Restore fakeConn nil-stream contract

**Root cause**: Task 3 introduced `autoStreamCounter` so `fakeConn.CreateSendStream()` never returned nil. The `ConnHandle` interface documents the nil-return as the degraded/test path; both `handleGetTaskLog` and `handleBoardRead` gate on `stream == nil`. With auto-allocation, those nil-stream branches were silently unreachable, and future tests would inadvertently get a stream instead of exercising the degraded path.

**Fix** (`server/fakes_test.go`):
- Removed `autoStreamCounter atomic.Uint64` field from `fakeConn`.
- Restored `CreateSendStream` to return nil when `nextSendStreamID == 0`; only allocates when a caller has set `nextSendStreamID` (cleared after use).
- Updated `recordingSendStream.AppendData`: when `eof=true`, now also closes the `done` channel via `closeOnce.Do`. This unblocks `sendStreamBytes` which waits on `<-s.done`; previously only `Close()` closed the channel, but after Finding 1's fix the goroutine signals EOF via `AppendData(true)` (not `Close()`).
- Updated comments on `CreateSendStream` and `sendStreamBytes` to reflect restored contract.

**Fix** (`server/board_handler_test.go`):
- `TestHandleBoardRead_StreamsPayloadsInOrder`: added `conn.nextSendStreamID = 5` before calling `handleBoardRead` so the handler gets a non-nil stream and the streaming path is exercised.

### go test ./server/ output

```
$ go test ./server/ -count=1
ok      github.com/on-keyday/agent-harness/server       0.711s
```

Targeted run:
```
$ go test ./server/ -count=1 -v -run "TestHandleBoardRead|TestHandleGetTaskLog"
=== RUN   TestHandleBoardRead_StreamsPayloadsInOrder
--- PASS: TestHandleBoardRead_StreamsPayloadsInOrder (0.00s)
PASS
ok      github.com/on-keyday/agent-harness/server       0.005s
```
