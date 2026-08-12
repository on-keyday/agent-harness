# Ranged file pull — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a `pull` transfer a byte range instead of the whole file, and report the whole file's size alongside the slice.

**Architecture:** `offset` / `length` are added to both open-file-transfer request formats and `total_size` to the ack; `runPull` seeks and copies a bounded slice. The client carries the range as one `FileTransferRange` value so existing call sites keep their meaning, and CLI, TUI and WebUI each expose it. The WebUI preview becomes the first real consumer: it pulls heads instead of refusing oversize files.

**Tech Stack:** Go, brgen `.bgn` schema (`make protoregen`), vanilla JS (`webui/static/main.js`), `GOOS=js GOARCH=wasm`.

## Global Constraints

- Spec: `docs/superpowers/specs/2026-08-12-file-pull-range-design.md`. Its decisions are binding.
- **Range applies to `pull` only.** Any other direction with a non-zero offset or length is answered `range_invalid`, never silently ignored.
- **`length = 0` means "to EOF"**, so `FileTransferRange{}` is the whole file and existing callers are unchanged in meaning.
- **`actual_size` = bytes in this transfer; `total_size` = size of the complete object.** For every non-pull direction they are equal by construction, which `writeAck` keeps encoding.
- **`offset >= size` is `ok` with an empty body**, not an error.
- `FileTransferAckSize` goes 9 → 17: a breaking wire change. Rebuild and restart the fleet together with `scripts/build_and_restart_all.py`; do not mix binaries.
- Schema edits go in `runner/protocol/message.bgn` only, regenerated with `make protoregen`. Never hand-edit `message.go`.
- Verify with `make check`, `make wasm-check`, `make test`, `make vet` — not ad-hoc `go build`.
- WebUI: dark theme `#1e1e1e` / `#d4d4d4`; check desktop **and** 390px.

## File Structure

| File | Responsibility |
|---|---|
| `runner/protocol/message.bgn` (modify) | The whole schema delta: two request fields, one ack field, one status. |
| `runner/file_transfer.go` (modify) | Ranged `runPull`, `range_invalid` guard, `writeAckRange`. |
| `runner/file_transfer_test.go` (modify) | Range unit tests. |
| `server/file_transfer.go:51-59` (modify) | Copy `Offset` / `Length` into the runner request. |
| `cli/file_transfer.go` (modify) | `FileTransferRange`, `OpenFileTransfer` signature. |
| `cli/file_pull.go` (modify) | `filePullDo` range + total, `FilePullBytesRange`. |
| `cli/file_pull_range_test.go` (create) | Client-side range tests. |
| `cmd/harness-cli/main.go:401` (modify) | `--offset` / `--length`. |
| `tui/cmdline.go:711` (modify) | `-o` / `-n`. |
| `cmd/harness-webui-wasm/main.go` (modify) | `harness.filePullBytesRange`. |
| `webui/static/main.js` (modify) | Preview head-pull + sniff. |

---

### Task 1: Schema delta

**Files:**
- Modify: `runner/protocol/message.bgn` (`FileTransferAck` ~:1155, `FileTransferStatus` ~:1143, `OpenFileTransferRequest` ~:1176, `RunnerOpenFileTransferRequest` ~:1220)
- Modify: `runner/file_transfer_test.go`

**Interfaces:**
- Produces: `protocol.FileTransferAck.TotalSize uint64`; `protocol.FileTransferStatus_RangeInvalid`; `Offset` / `Length` on both open-file-transfer request formats; `protocol.FileTransferAckSize == 17`.

- [ ] **Step 1: Write the failing test**

Append to `runner/file_transfer_test.go`:

```go
// The ack carries both numbers now, so its fixed width changed. Readers
// pre-allocate exactly FileTransferAckSize bytes; if the constant and the
// encoder ever disagree, every transfer misparses.
func TestFileTransferAckWidthAndRoundTrip(t *testing.T) {
	if protocol.FileTransferAckSize != 17 {
		t.Fatalf("FileTransferAckSize = %d, want 17", protocol.FileTransferAckSize)
	}
	in := protocol.FileTransferAck{
		Status:     protocol.FileTransferStatus_Ok,
		ActualSize: 4096,
		TotalSize:  1 << 30,
	}
	body, err := in.Append(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != protocol.FileTransferAckSize {
		t.Fatalf("encoded %d bytes, want %d", len(body), protocol.FileTransferAckSize)
	}
	var out protocol.FileTransferAck
	if err := out.Decode(body); err != nil {
		t.Fatal(err)
	}
	if out.ActualSize != 4096 || out.TotalSize != 1<<30 {
		t.Fatalf("round trip gave actual=%d total=%d", out.ActualSize, out.TotalSize)
	}
}
```

If `Decode` is not the generated decoder's name, use whatever `message.go`
exposes for `FileTransferAck` (check with
`grep -n "func (t \*FileTransferAck)" runner/protocol/message.go`) — the
assertion, not the call, is what matters.

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./runner/ -run TestFileTransferAckWidthAndRoundTrip`
Expected: compile failure — `in.TotalSize undefined`.

- [ ] **Step 3: Edit the schema**

In `runner/protocol/message.bgn`:

```
format FileTransferAck:
    status      :FileTransferStatus
    actual_size :u64   # bytes carried by THIS transfer's body. A ranged pull
                       # sends only the requested slice, so this is the slice,
                       # which is what push / dir_pull already meant by it.
    total_size  :u64   # size of the complete object: the file size for a pull,
                       # ranged or not. Every other direction transfers the
                       # whole object, so writeAck sets it equal to actual_size.
```

Add to `FileTransferStatus`:

```
    range_invalid   = "range_invalid"  # offset/length set on a direction that has no range (anything but pull).
```

Add to **both** `OpenFileTransferRequest` and `RunnerOpenFileTransferRequest`,
immediately after `expected_size` and before `force`:

```
    offset        :u64   # pull only: first byte to send. 0 = from the start.
    length        :u64   # pull only: max bytes to send. 0 = to EOF.
```

- [ ] **Step 4: Regenerate and build**

```bash
make protoregen
go build ./... 2>&1 | head -20
```

Expected: `writeAck` still compiles (it sets only `ActualSize` so far — Step 5
fixes that), and nothing else references the new fields yet.

- [ ] **Step 5: Make `writeAck` carry the invariant**

In `runner/file_transfer.go:127`:

```go
// writeAck answers a whole-object transfer: actual and total are the same
// number by construction, because the transfer carried all of it. Only a
// ranged pull can separate them; that path uses writeAckRange.
func writeAck(st trsf.BidirectionalStream, status protocol.FileTransferStatus, size uint64) error {
	return writeAckRange(st, status, size, size)
}

func writeAckRange(st trsf.BidirectionalStream, status protocol.FileTransferStatus, actual, total uint64) error {
	ack := protocol.FileTransferAck{Status: status, ActualSize: actual, TotalSize: total}
	body, err := ack.Append(nil)
	if err != nil {
		return err
	}
	if _, err := st.Write(body); err != nil {
		return err
	}
	return nil
}
```

- [ ] **Step 6: Run the test and commit**

```bash
go test ./runner/ -run TestFileTransferAckWidthAndRoundTrip -v
make check && make vet
git add runner/protocol/message.bgn runner/protocol/message.go runner/file_transfer.go runner/file_transfer_test.go
git commit -m "feat(protocol): offset/length on pull, total_size on the transfer ack"
```

---

### Task 2: Ranged `runPull` and the `range_invalid` guard

**Files:**
- Modify: `runner/file_transfer.go` (`handleOpenFileTransfer` ~:179, `runPull` :314)
- Modify: `runner/file_transfer_test.go`

**Interfaces:**
- Consumes: `writeAckRange` and the schema fields from Task 1.
- Produces: `runPull(stream trsf.BidirectionalStream, full string, offset, length uint64)`.

- [ ] **Step 1: Write the failing tests**

The existing tests in this file build a `protocol.RunnerOpenFileTransferRequest`
and drive the handler; copy that setup exactly (see the ones at
`runner/file_transfer_test.go:65` and `:115`) and add:

```go
// A mid-file slice: the body is the slice, actual_size is the slice, and
// total_size is still the whole file — that last one is the point of the ack
// change, so assert it in every case below.
func TestRunPullRangeMidFile(t *testing.T) {
	// content is 0..99; ask for bytes [10,20)
	// expect: ack.Status ok, ack.ActualSize 10, ack.TotalSize 100, body == content[10:20]
}

func TestRunPullRangeLengthPastEOFIsClamped(t *testing.T) {
	// offset 90, length 999 over a 100-byte file
	// expect: ActualSize 10, TotalSize 100, body == content[90:]
}

func TestRunPullRangeOffsetAtEOFIsEmptyOk(t *testing.T) {
	// offset 100 over a 100-byte file
	// expect: Status ok, ActualSize 0, TotalSize 100, empty body
}

func TestRunPullRangeOffsetPastEOFIsEmptyOk(t *testing.T) {
	// offset 5000 over a 100-byte file — NOT an error: a caller paging a file
	// that shrank must tell "past the end" from "the read failed".
	// expect: Status ok, ActualSize 0, TotalSize 100, empty body
}

func TestRunPullWholeFileUnchanged(t *testing.T) {
	// offset 0, length 0 — the existing behaviour, asserted so the default
	// path cannot regress: ActualSize 100, TotalSize 100, body == content
}

func TestRangeOnNonPullDirectionIsRejected(t *testing.T) {
	// direction dir_pull with offset 1 -> Status range_invalid
	// direction push with length 1 -> Status range_invalid
}
```

Fill each body using the surrounding file's helpers rather than inventing new
ones. Every case asserts `TotalSize` explicitly.

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./runner/ -run 'RunPullRange|RangeOnNonPull|RunPullWholeFile' -v`
Expected: FAIL — the handler ignores the new fields, so ranged cases return the whole file and the non-pull cases return `ok`.

- [ ] **Step 3: Guard non-pull directions**

In `handleOpenFileTransfer`, immediately before the `switch req.Direction`:

```go
	// A range on a direction that has no range is refused rather than ignored:
	// ignoring it would hand a caller who asked for a slice the whole object,
	// which is a silent wrong answer instead of a loud refusal.
	if (req.Offset != 0 || req.Length != 0) && req.Direction != protocol.FileTransferDirection_Pull {
		_ = writeAck(stream, protocol.FileTransferStatus_RangeInvalid, 0)
		return
	}
```

and change the pull case to `s.runPull(stream, full, req.Offset, req.Length)`.

- [ ] **Step 4: Implement the ranged pull**

Replace `runPull`'s ack-and-copy tail (`runner/file_transfer.go:331-337`):

```go
	total := uint64(st.Size())
	// Past the end is an empty ok, not an error — see the spec's decision 5.
	if offset >= total {
		_ = writeAckRange(stream, protocol.FileTransferStatus_Ok, 0, total)
		_ = stream.AppendData(true)
		return
	}
	n := total - offset
	if length > 0 && length < n {
		n = length
	}
	if offset > 0 {
		if _, err := f.Seek(int64(offset), io.SeekStart); err != nil {
			_ = writeAckRange(stream, protocol.FileTransferStatus_IoError, 0, total)
			return
		}
	}
	if err := writeAckRange(stream, protocol.FileTransferStatus_Ok, n, total); err != nil {
		return
	}
	// Errors are silent; the client sees a short read against the acked size.
	_, _ = io.CopyN(stream, f, int64(n))
	_ = stream.AppendData(true)
```

- [ ] **Step 5: Run the tests and commit**

```bash
go test ./runner/ -run 'RunPull|Range' -v
go test ./runner/
git add runner/file_transfer.go runner/file_transfer_test.go
git commit -m "feat(runner): serve a byte range on pull, refuse one on every other direction"
```

---

### Task 3: Server relay and client API

**Files:**
- Modify: `server/file_transfer.go:51-59`
- Modify: `cli/file_transfer.go` (`OpenFileTransfer` :26)
- Modify: `cli/file_pull.go` (`filePullDo` :78, `FilePullBytes` :49)
- Modify: `cli/file_delete.go:29`, `cli/file_mkdir.go:18`, `cli/file_push.go:55,84`, `cli/file_pull.go:112,200`
- Create: `cli/file_pull_range_test.go`

**Interfaces:**
- Produces:
  - `type FileTransferRange struct { Offset, Length uint64 }`
  - `OpenFileTransfer(ctx, taskIDHex string, direction protocol.FileTransferDirection, relPath string, expectedSize uint64, rng FileTransferRange, force, mkdirParents bool) (trsf.BidirectionalStream, error)`
  - `FilePullBytesRange(ctx, taskIDHex, remoteRel string, rng FileTransferRange, onProgress ProgressFunc) ([]byte, uint64, error)`

- [ ] **Step 1: Write the failing test**

Create `cli/file_pull_range_test.go`:

```go
package cli

import "testing"

// The zero value must mean "whole file", because every pre-existing call site
// passes it and none of them intends a range.
func TestFileTransferRangeZeroValueIsWholeFile(t *testing.T) {
	var r FileTransferRange
	if r.Offset != 0 || r.Length != 0 {
		t.Fatalf("zero value is %+v", r)
	}
}
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./cli/ -run TestFileTransferRangeZeroValue`
Expected: compile failure — `undefined: FileTransferRange`.

- [ ] **Step 3: Add the type and thread it through**

In `cli/file_transfer.go`, above `OpenFileTransfer`:

```go
// FileTransferRange selects a byte range of a pull. The zero value is the whole
// file, so a caller that does not care passes FileTransferRange{} and keeps
// exactly the behaviour it had. Only pull honours it; the runner answers
// range_invalid for anything else.
type FileTransferRange struct {
	Offset uint64
	Length uint64 // 0 = to EOF
}
```

Add `rng FileTransferRange` to `OpenFileTransfer` after `expectedSize`, and set
`body.Offset = rng.Offset` / `body.Length = rng.Length` beside `ExpectedSize`.
Update the seven existing call sites to pass `FileTransferRange{}`:
`cli/file_pull.go:79,112,200`, `cli/file_delete.go:29`, `cli/file_mkdir.go:18`,
`cli/file_push.go:55,84`.

In `server/file_transfer.go`, add to the `RunnerOpenFileTransferRequest` literal:

```go
		Offset:       req.Offset,
		Length:       req.Length,
```

**This copy is easy to forget and fails silently** — the server rebuilds the
request field by field, so an uncopied field reaches the runner as 0 and the
range is ignored with no error anywhere.

- [ ] **Step 4: Give `filePullDo` the range and the total**

`filePullDo(ctx, taskIDHex, remoteRel string, rng FileTransferRange, body func(stream trsf.BidirectionalStream, n, total uint64) error) error`
— it passes `ack.ActualSize` as `n` and `ack.TotalSize` as `total`. `FilePull`
and `FilePullBytes` pass `FileTransferRange{}` and ignore `total`. Then add:

```go
// FilePullBytesRange is FilePullBytes over a byte range. It returns the slice
// and the size of the whole file, so a caller rendering a head knows whether
// there is more without a second round trip that could race the pull.
func (c *Client) FilePullBytesRange(ctx context.Context, taskIDHex, remoteRel string, rng FileTransferRange, onProgress ProgressFunc) ([]byte, uint64, error) {
	var buf bytes.Buffer
	var total uint64
	if err := c.filePullDo(ctx, taskIDHex, remoteRel, rng, func(stream trsf.BidirectionalStream, n, tot uint64) error {
		total = tot
		buf.Grow(int(n))
		got, err := copyWithProgress(&buf, stream, n, onProgress)
		if err != nil {
			return fmt.Errorf("file pull: stream read: %w", err)
		}
		if got != n {
			return fmt.Errorf("file pull: short read (got %d, expected %d)", got, n)
		}
		return nil
	}); err != nil {
		return nil, 0, err
	}
	return buf.Bytes(), total, nil
}
```

- [ ] **Step 5: Map the new status**

`ackError` in `cli/file_transfer.go` must give `range_invalid` a message rather
than falling through to a generic one. Find it with
`grep -n "func ackError" -A 25 cli/file_transfer.go` and add the case:
`"file %s: range not supported for this operation"`.

- [ ] **Step 6: Build, test and commit**

```bash
make check && make vet && go test ./cli/ ./server/ ./runner/
git add cli/ server/file_transfer.go
git commit -m "feat(cli): FileTransferRange, ranged pull bytes, relay offset/length"
```

---

### Task 4: CLI and TUI flags

**Files:**
- Modify: `cmd/harness-cli/main.go:401`
- Modify: `tui/cmdline.go:711`, and the TUI action struct + executor for `FilePullAction`
- Modify: `tui/cmdline_test.go`

**Interfaces:**
- Consumes: `cli.FileTransferRange`, `cli.Client.FilePullBytesRange`.

- [ ] **Step 1: Write the failing TUI parse test**

In `tui/cmdline_test.go`, beside the existing `file pull` test at `:416`:

```go
func TestParseFilePullRange(t *testing.T) {
	got, err := ParseCommand(`file pull -o 10 -n 20 deadbeef rel/file.txt ./local.txt`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	act, ok := got.(FilePullAction)
	if !ok {
		t.Fatalf("got %T", got)
	}
	if act.Offset != 10 || act.Length != 20 {
		t.Fatalf("offset=%d length=%d", act.Offset, act.Length)
	}
}
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./tui/ -run TestParseFilePullRange`
Expected: FAIL — `act.Offset undefined`.

- [ ] **Step 3: Add the flags**

TUI (`tui/cmdline.go`, in the `"pull"` case, matching the `-r` / `-f` style):

```go
		offset := fs.Uint64("offset", 0, "")
		fs.Uint64Var(offset, "o", 0, "")
		length := fs.Uint64("length", 0, "")
		fs.Uint64Var(length, "n", 0, "")
```

Add `Offset, Length uint64` to `FilePullAction`, set them, and reject
`-r` combined with either: a directory pull has no byte range.
Usage string becomes
`file pull [-r] [-f] [-o off] [-n len] <task-id> <worktree-rel-src> <local-dst>`.

CLI (`cmd/harness-cli/main.go`, the `file pull` flag set):

```go
			offset := fs.Uint64("offset", 0, "first byte to pull (pull only, not with -r)")
			length := fs.Uint64("length", 0, "max bytes to pull; 0 = to end of file")
```

and pass `cli.FileTransferRange{Offset: *offset, Length: *length}` through. When
either is non-zero, write the slice with the ranged path; refuse the
combination with `--recursive` before dialling.

- [ ] **Step 4: Run tests and commit**

```bash
go test ./tui/ && make check && make vet
git add cmd/harness-cli/main.go tui/
git commit -m "feat(cli,tui): file pull --offset/--length"
```

---

### Task 5: WebUI binding and head-pulling preview

**Files:**
- Modify: `cmd/harness-webui-wasm/main.go` (binding table ~:99, handler beside `harnessFilePullBytes` :1982)
- Modify: `webui/static/main.js` (preview button handler ~:1136)

**Interfaces:**
- Consumes: `cli.FilePullBytesRange`.
- Produces: `harness.filePullBytesRange(taskID, rel, offset, length[, onProgress]) -> Promise<{bytes: Uint8Array, total: number}>`.

- [ ] **Step 1: Add the wasm binding**

Table entry beside `"filePullBytes"`:

```go
		"filePullBytesRange": js.FuncOf(harnessFilePullBytesRange),
```

Handler, modelled on `harnessFilePullBytes` (same promise executor, same
`rejectFileErr`), resolving an object:

```go
//	harness.filePullBytesRange(taskID, remoteRel, offset, length[, onProgress])
//	  -> Promise<{bytes, total}>
//
// total is the whole file's size, so a caller rendering a head knows whether it
// truncated without a second round trip that could race the pull.
func harnessFilePullBytesRange(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			if len(args) < 4 {
				rejectErr(reject, errors.New("filePullBytesRange: want (taskID, remoteRel, offset, length[, onProgress])"))
				return
			}
			rng := cli.FileTransferRange{
				Offset: uint64(args[2].Int()),
				Length: uint64(args[3].Int()),
			}
			data, total, err := c.FilePullBytesRange(rootCtx, args[0].String(), args[1].String(), rng, jsProgress(args, 4))
			if err != nil {
				rejectFileErr(reject, err)
				return
			}
			out := js.Global().Get("Uint8Array").New(len(data))
			js.CopyBytesToJS(out, data)
			resolve.Invoke(js.ValueOf(map[string]any{"bytes": out, "total": total}))
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}
```

- [ ] **Step 2: Build the wasm**

Run: `make wasm-check && make webui-build`
Expected: builds clean.

- [ ] **Step 3: Make the preview pull heads**

Replace the oversize-refusal block in the preview button handler with:

```js
    const cap = previewMaxBytesFor(sel.name);
    const fp = beginFileProgress(sel.name);
    try {
      let bytes, total;
      if (isHtmlExt(sel.name) || isImageExt(sel.name)) {
        // The extension settles the renderer, so one request is enough.
        ({ bytes, total } = await pullPreviewSlice(taskID, rel, 0, Math.min(sel.size, cap), fp));
      } else {
        // Unknown type: fetch only what a hex dump would show, decide from
        // those bytes, and continue only if it is text. This is what stops a
        // multi-megabyte binary being pulled to render 4 KiB of hex.
        const head = await pullPreviewSlice(taskID, rel, 0, Math.min(sel.size, HEX_PREVIEW_MAX_BYTES), fp);
        total = head.total;
        if (isLikelyBinary(head.bytes) || head.total <= HEX_PREVIEW_MAX_BYTES) {
          bytes = head.bytes;
        } else {
          const rest = await pullPreviewSlice(taskID, rel, HEX_PREVIEW_MAX_BYTES,
            Math.min(head.total, cap) - HEX_PREVIEW_MAX_BYTES, fp);
          bytes = new Uint8Array(head.bytes.length + rest.bytes.length);
          bytes.set(head.bytes, 0);
          bytes.set(rest.bytes, head.bytes.length);
        }
      }
      renderFilePreview(rel, sel.size, sel.name, bytes);
      if (bytes.byteLength < total) {
        const p = document.createElement("p");
        p.className = "preview-note";
        p.textContent = `showing the first ${formatBytes(bytes.byteLength)} of ${formatBytes(total)}. Use Pull to download the whole file.`;
        filePreviewBody.appendChild(p);
      }
    } catch (e) {
      openFilePreview(rel, sel.size, null, `preview error: ${e.message}`);
    } finally {
      fp.end();
    }
```

with the helper at column 0, beside the other preview helpers:

```js
// pullPreviewSlice returns {bytes, total} for one range of a file.
async function pullPreviewSlice(taskID, rel, offset, length, fp) {
  const res = await window.harness.filePullBytesRange(taskID, rel, offset, length, fp && fp.onProgress);
  return { bytes: new Uint8Array(res.bytes), total: Number(res.total) };
}
```

The oversize *refusal* is deleted: a large file now opens as a head with the
note, which is the whole point of the change.

- [ ] **Step 4: Commit**

```bash
make check && make wasm-check
git add cmd/harness-webui-wasm/main.go webui/static/main.js
git commit -m "feat(webui): preview pulls heads instead of refusing oversize files"
```

---

### Task 6: End-to-end on a dummy harness, then land

**Files:** none — produces evidence, plus a fix commit if it finds one.

- [ ] **Step 1: Bring up a dummy harness**

Follow the `dummy-harness` skill: `scripts/dummy-harness.sh up --detach --agent fake --name pullrange`, then eval its env and submit a long-running `bash` task so a `Running` task exists.

- [ ] **Step 2: Prove the bytes are right**

Put a known file in the worktree, then compare a ranged pull against `dd`:

```bash
harness-cli --server-cid "$CID" file pull --offset 1000 --length 256 "$TASK" big.bin /tmp/slice.bin
dd if="$REPO/big.bin" bs=1 skip=1000 count=256 of=/tmp/expect.bin 2>/dev/null
cmp /tmp/slice.bin /tmp/expect.bin && echo "range bytes match"
```

Also check `--offset` past EOF yields an empty file and exit status 0.

- [ ] **Step 3: Prove the non-pull refusal**

A ranged `--recursive` pull must be refused by the client before dialling, and a
hand-built ranged `dir_pull` must come back `range_invalid`. The second is
covered by Task 2's unit test; assert the CLI refusal here.

- [ ] **Step 4: Drive the WebUI with Playwright MCP**

Open `<webui>/#psk=<psk>`, Files tab, and check, at desktop **and** 390px:
a 5 MiB `.log` opens as a head with the "showing the first …" note instead of
being refused; a large binary previews as hex; and the runner transferred only
about 4 KiB for it — measure that from the runner side (a wrapper that counts
bytes, or the file's size vs. the progress row's total), **not** from the
browser, which cannot see what it did not receive.

- [ ] **Step 5: Tear down and land**

`scripts/dummy-harness.sh down --name pullrange`, keep the screenshots (they are
deliverables — name them and report their paths), then follow `landing-to-main`
for Mode A: rebase if needed, `make test`, FF-push, advance local trunk,
`make build` in the main checkout.

- [ ] **Step 6: Restart the fleet**

The ack width changed, so mixed binaries misparse it. Run
`scripts/build_and_restart_all.py` (the `restart-all` skill) and confirm the
runners come back, then `harness-cli notify` the operator with the commit range
and the screenshot paths.

## Self-Review

**Spec coverage:** decisions 1–2 → Task 2's guard; 3 → Task 3's zero value plus Task 2's whole-file test; 4 → Task 1's ack and `writeAck` wrapper; 5 → Task 2's two past-EOF tests; 6 → Task 3's `FileTransferRange`; 7 → Task 5's two-step. Schema delta → Task 1. Server relay → Task 3 Step 3. Surfaces → Tasks 4 and 5. Testing section → Tasks 1, 2, 3 and 6.

**Known soft spot:** Task 2's test bodies are described rather than written out, because they must reuse the request-building helpers already in `runner/file_transfer_test.go` and copying a guess of that setup into the plan would be worse than pointing at the real thing. Each case states its inputs and its three assertions, so nothing is left to judgement.

**Wire-change reminder:** Task 6 Step 6 is not optional. A landed ack-width change with a stale runner still running is a misparse on the next transfer.
