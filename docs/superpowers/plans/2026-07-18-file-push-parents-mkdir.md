# file push -p/--parents + file mkdir Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an opt-in `-p/--parents` flag to `file push` (create missing parent dirs on the runner) and a `file mkdir` command (strict by default, `-p` for MkdirAll), across CLI / TUI / WebUI / wasm / server / runner, with the missing-parent failure mapped to a diagnosable `not_found` instead of generic `io_error`.

**Spec:** `docs/superpowers/specs/2026-07-18-file-push-parents-mkdir-design.md` — read its Problem statement before starting any task.

**Architecture:** One wire change in `runner/protocol/message.bgn`: a `mkdir_parents :u1` bit (taken from `reserved`) in both `OpenFileTransferRequest` and `RunnerOpenFileTransferRequest`, plus a `mkdir` value in `FileTransferDirection`. The server relay copies the bit; the runner honors it in `runPush` / `runDirPush` and a new `runMkdir`. Client-side, the three push helpers move from a `force bool` parameter to a `FilePushOpts` struct, and `FileMkdir` mirrors the delete helper (ack-only stream).

**Tech Stack:** Go, brgen-generated wire code (`make protoregen`), bubbletea TUI, wasm + vanilla JS WebUI.

## Global Constraints

- **Work in the parent repo `/home/kforfk/workspace/remote-agent-harness/`, NOT in any `.harness-worktrees/<hash>/` directory.** Verify with `git rev-parse --abbrev-ref HEAD` (must print `main`) before writing code. Use absolute paths under the parent repo for all tool calls. (Pitfall 8: absolute paths inside a harness worktree silently route to the parent.)
- **Read `.claude/skills/implementation-pitfalls/SKILL.md` in full before writing code.**
- **Build hygiene:** compile-check with `go build ./...` (writes NO binary). NEVER bare `go build ./cmd/<x>/` (drops a stray executable into the repo root). `git status` must be as clean after your checks as before.
- **Never hand-edit `runner/protocol/message.go`** — it is generated. Schema changes go in `message.bgn` + `make protoregen ARGS='runner/protocol/message.bgn'`.
- Bit accessors follow the existing generated naming: `mkdir_parents` → `MkdirParents() bool` / `SetMkdirParents(bool) bool` (same shape as `Force()` / `SetForce`).
- All new runner-side directory creation uses mode `0o755` and happens only AFTER `ValidateRelPath` + `rejectIfSymlinkInPath` have passed.
- Commit after each task with a conventional-commit message ending in the `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>` trailer.

---

### Task 1: Wire schema — mkdir direction + mkdir_parents bit

**Files:**
- Modify: `runner/protocol/message.bgn` (FileTransferDirection ~line 1062, OpenFileTransferRequest ~line 1106, RunnerOpenFileTransferRequest ~line 1146)
- Regenerate: `runner/protocol/message.go` (via make target, not by hand)
- Test: `runner/protocol/file_transfer_wire_test.go` (create)

**Interfaces:**
- Produces: `protocol.FileTransferDirection_Mkdir`; `(*OpenFileTransferRequest).MkdirParents() bool` / `SetMkdirParents(bool) bool`; same pair on `RunnerOpenFileTransferRequest`. All later tasks consume these.

- [ ] **Step 1: Write the failing round-trip test**

Create `runner/protocol/file_transfer_wire_test.go`:

```go
package protocol

import "testing"

func TestOpenFileTransferRequest_MkdirParentsRoundTrip(t *testing.T) {
	req := OpenFileTransferRequest{
		TaskId:    TaskID{Id: [16]byte{1, 2, 3}},
		Direction: FileTransferDirection_Mkdir,
	}
	req.SetRelPath([]byte("a/b/c"))
	req.SetForce(false)
	req.SetMkdirParents(true)
	enc, err := req.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	got := &OpenFileTransferRequest{}
	if _, err := got.Decode(enc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Direction != FileTransferDirection_Mkdir {
		t.Errorf("direction = %v want mkdir", got.Direction)
	}
	if !got.MkdirParents() || got.Force() {
		t.Errorf("bits: mkdir_parents=%v force=%v want true/false",
			got.MkdirParents(), got.Force())
	}
	if string(got.RelPath) != "a/b/c" {
		t.Errorf("rel = %q want a/b/c", got.RelPath)
	}
}

func TestRunnerOpenFileTransferRequest_MkdirParentsRoundTrip(t *testing.T) {
	req := RunnerOpenFileTransferRequest{
		TaskId:    TaskID{Id: [16]byte{9}},
		StreamId:  7,
		Direction: FileTransferDirection_DirPush,
	}
	req.SetRelPath([]byte("x/y"))
	req.SetForce(true)
	req.SetMkdirParents(true)
	enc, err := req.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	got := &RunnerOpenFileTransferRequest{}
	if _, err := got.Decode(enc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.MkdirParents() || !got.Force() || got.StreamId != 7 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./runner/protocol/ -run MkdirParents 2>&1 | head -20`
Expected: compile error — `req.SetMkdirParents undefined` and `FileTransferDirection_Mkdir undefined`.

- [ ] **Step 3: Edit the schema**

In `runner/protocol/message.bgn`, append `mkdir` to the direction enum (after `dir_delete` — appending keeps existing numeric values stable):

```
    dir_delete   # remove a directory from the worktree. force=0 → empty
                 # dir only (fails on non-empty); force=1 → recursive
                 # (os.RemoveAll). Rejects regular files (use `delete`).
    mkdir        # create a directory. mkdir_parents=0 → strict os.Mkdir
                 # (parent must exist, existing dir → already_exists);
                 # mkdir_parents=1 → os.MkdirAll (parents created, existing
                 # dir → ok). Mirrors Unix mkdir / mkdir -p. No body bytes;
                 # ack only, like delete. force / expected_size are ignored.
```

In BOTH `OpenFileTransferRequest` and `RunnerOpenFileTransferRequest`, replace

```
    force         :u1
    reserved      :u7
```

with

```
    force         :u1
    mkdir_parents :u1     # push / dir_push: create missing parent
                          # directories of the destination before writing.
                          # mkdir: MkdirAll instead of strict Mkdir.
                          # Ignored by pull / delete / dir_pull / dir_delete.
    reserved      :u6
```

- [ ] **Step 4: Regenerate**

Run: `make protoregen ARGS='runner/protocol/message.bgn'`
Expected: exits 0, `git diff --stat runner/protocol/message.go` shows changes. (First invocation may download brgen-kit — that is normal.)

- [ ] **Step 5: Run tests**

Run: `go test ./runner/protocol/`
Expected: PASS (all, including the two new tests).

- [ ] **Step 6: Repo-wide compile check**

Run: `go build ./... && make wasm-check`
Expected: both succeed (nothing consumes the new symbols yet).

- [ ] **Step 7: Commit**

```bash
git add runner/protocol/message.bgn runner/protocol/message.go runner/protocol/file_transfer_wire_test.go
git commit -m "feat(protocol): mkdir direction + mkdir_parents bit on file transfer"
```

---

### Task 2: Runner — honor mkdir_parents, add runMkdir, map missing-parent to not_found

**Files:**
- Modify: `runner/file_transfer.go` (`handleOpenFileTransfer` dispatch ~line 179, `runPush` ~line 299, `runDirPush` ~line 405)
- Test: `runner/file_transfer_test.go` (append)

**Interfaces:**
- Consumes: `req.MkdirParents()`, `protocol.FileTransferDirection_Mkdir` (Task 1).
- Produces: runner acks `not_found` for missing-parent push/dir_push/strict-mkdir; `already_exists` for strict mkdir on an existing dir; `ok` (idempotent) for `-p` mkdir on an existing dir; `not_a_directory` when the mkdir leaf is a regular file.

- [ ] **Step 1: Write the failing tests**

Append to `runner/file_transfer_test.go` (fixtures `mustParseTaskID`, `newMemoryBidiPair`, `staticStreamLookup`, `readAck` already exist in this file):

```go
// pushReq builds a RunnerOpenFileTransferRequest for tests below.
func pushReq(t *testing.T, taskIDHex, rel string, dir protocol.FileTransferDirection, parents bool) *protocol.RunnerOpenFileTransferRequest {
	t.Helper()
	req := &protocol.RunnerOpenFileTransferRequest{
		TaskId:    mustParseTaskID(t, taskIDHex),
		StreamId:  1,
		Direction: dir,
	}
	req.SetRelPath([]byte(rel))
	req.SetMkdirParents(parents)
	return req
}

func newFileSession(t *testing.T, tmp, taskIDHex string) (*Session, trsf.BidirectionalStream) {
	t.Helper()
	sess := &Session{NoWorktree: true}
	sess.initMaps()
	sess.tasks[taskIDHex] = &taskEntry{repoPath: tmp}
	clientEnd, runnerEnd := newMemoryBidiPair()
	sess.Streams = staticStreamLookup{1: runnerEnd}
	return sess, clientEnd
}

func TestHandleOpenFileTransfer_PushMissingParentNotFound(t *testing.T) {
	tmp := t.TempDir()
	taskIDHex := "00000000000000000000000000000010"
	sess, clientEnd := newFileSession(t, tmp, taskIDHex)
	req := pushReq(t, taskIDHex, "no/such/dir/f.txt", protocol.FileTransferDirection_Push, false)

	go sess.handleOpenFileTransfer(context.Background(), req)
	// Runner fails at OpenFile before reading any payload; just close
	// our write side (same shape as the AlreadyExists test).
	if err := clientEnd.AppendData(true); err != nil {
		t.Fatalf("client EOF: %v", err)
	}
	ack := readAck(t, clientEnd)
	if ack.Status != protocol.FileTransferStatus_NotFound {
		t.Fatalf("ack status = %v want not_found", ack.Status)
	}
}

func TestHandleOpenFileTransfer_PushMkdirParentsOK(t *testing.T) {
	tmp := t.TempDir()
	taskIDHex := "00000000000000000000000000000011"
	sess, clientEnd := newFileSession(t, tmp, taskIDHex)
	req := pushReq(t, taskIDHex, "a/b/f.txt", protocol.FileTransferDirection_Push, true)
	req.ExpectedSize = 5

	go sess.handleOpenFileTransfer(context.Background(), req)
	if err := clientEnd.AppendData(false, []byte("hello")); err != nil {
		t.Fatalf("client write: %v", err)
	}
	if err := clientEnd.AppendData(true); err != nil {
		t.Fatalf("client EOF: %v", err)
	}
	ack := readAck(t, clientEnd)
	if ack.Status != protocol.FileTransferStatus_Ok {
		t.Fatalf("ack status = %v want ok", ack.Status)
	}
	got, err := os.ReadFile(filepath.Join(tmp, "a", "b", "f.txt"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("content = %q want hello", got)
	}
}

func TestHandleOpenFileTransfer_DirPushMissingParent(t *testing.T) {
	tmp := t.TempDir()
	taskIDHex := "00000000000000000000000000000012"

	// Without parents: not_found.
	sess, clientEnd := newFileSession(t, tmp, taskIDHex)
	req := pushReq(t, taskIDHex, "no/such/dest", protocol.FileTransferDirection_DirPush, false)
	go sess.handleOpenFileTransfer(context.Background(), req)
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	if err := tw.WriteHeader(&tar.Header{Name: "inner.txt", Typeflag: tar.TypeReg, Size: 2, Mode: 0o644}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := clientEnd.AppendData(false, tarBuf.Bytes()); err != nil {
		t.Fatalf("client write: %v", err)
	}
	if err := clientEnd.AppendData(true); err != nil {
		t.Fatalf("client EOF: %v", err)
	}
	if ack := readAck(t, clientEnd); ack.Status != protocol.FileTransferStatus_NotFound {
		t.Fatalf("no-parents ack = %v want not_found", ack.Status)
	}

	// With parents: ok, tree lands.
	sess2, clientEnd2 := newFileSession(t, tmp, taskIDHex)
	req2 := pushReq(t, taskIDHex, "no/such/dest", protocol.FileTransferDirection_DirPush, true)
	go sess2.handleOpenFileTransfer(context.Background(), req2)
	if err := clientEnd2.AppendData(false, tarBuf.Bytes()); err != nil {
		t.Fatalf("client write: %v", err)
	}
	if err := clientEnd2.AppendData(true); err != nil {
		t.Fatalf("client EOF: %v", err)
	}
	if ack := readAck(t, clientEnd2); ack.Status != protocol.FileTransferStatus_Ok {
		t.Fatalf("parents ack = %v want ok", ack.Status)
	}
	if _, err := os.Stat(filepath.Join(tmp, "no", "such", "dest", "inner.txt")); err != nil {
		t.Errorf("pushed tree missing: %v", err)
	}
}

func TestHandleOpenFileTransfer_Mkdir(t *testing.T) {
	tmp := t.TempDir()
	taskIDHex := "00000000000000000000000000000013"

	run := func(rel string, parents bool) protocol.FileTransferStatus {
		sess, clientEnd := newFileSession(t, tmp, taskIDHex)
		req := pushReq(t, taskIDHex, rel, protocol.FileTransferDirection_Mkdir, parents)
		go sess.handleOpenFileTransfer(context.Background(), req)
		if err := clientEnd.AppendData(true); err != nil {
			t.Fatalf("client EOF: %v", err)
		}
		return readAck(t, clientEnd).Status
	}

	// Strict mkdir with missing parent → not_found.
	if got := run("deep/nested/dir", false); got != protocol.FileTransferStatus_NotFound {
		t.Errorf("strict missing parent = %v want not_found", got)
	}
	// -p creates the whole chain.
	if got := run("deep/nested/dir", true); got != protocol.FileTransferStatus_Ok {
		t.Errorf("-p nested = %v want ok", got)
	}
	if fi, err := os.Stat(filepath.Join(tmp, "deep", "nested", "dir")); err != nil || !fi.IsDir() {
		t.Fatalf("dir not created: fi=%v err=%v", fi, err)
	}
	// Strict on an existing dir → already_exists; -p is idempotent ok.
	if got := run("deep/nested/dir", false); got != protocol.FileTransferStatus_AlreadyExists {
		t.Errorf("strict existing = %v want already_exists", got)
	}
	if got := run("deep/nested/dir", true); got != protocol.FileTransferStatus_Ok {
		t.Errorf("-p existing = %v want ok", got)
	}
	// Strict with existing parent → ok.
	if got := run("deep/sibling", false); got != protocol.FileTransferStatus_Ok {
		t.Errorf("strict existing parent = %v want ok", got)
	}
	// Leaf is a regular file → not_a_directory in both modes.
	if err := os.WriteFile(filepath.Join(tmp, "plainfile"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := run("plainfile", false); got != protocol.FileTransferStatus_NotADirectory {
		t.Errorf("strict on file = %v want not_a_directory", got)
	}
	if got := run("plainfile", true); got != protocol.FileTransferStatus_NotADirectory {
		t.Errorf("-p on file = %v want not_a_directory", got)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./runner/ -run 'MissingParent|Mkdir|DirPushMissing' -v 2>&1 | tail -20`
Expected: `PushMissingParentNotFound` FAILS (ack is `io_error`, want `not_found`); `Mkdir` FAILS (unknown direction → `io_error`); `PushMkdirParentsOK` FAILS.

- [ ] **Step 3: Implement**

In `runner/file_transfer.go`:

Dispatch (`handleOpenFileTransfer`) — change the Push/DirPush cases and add Mkdir:

```go
	case protocol.FileTransferDirection_Push:
		s.runPush(stream, full, req.Force(), req.MkdirParents())
	...
	case protocol.FileTransferDirection_DirPush:
		s.runDirPush(stream, worktreeDir, full, req.Force(), req.MkdirParents())
	...
	case protocol.FileTransferDirection_Mkdir:
		s.runMkdir(stream, full, req.MkdirParents())
```

`runPush` — new parameter + parent creation + ENOENT mapping:

```go
func (s *Session) runPush(stream trsf.BidirectionalStream, full string, force, mkdirParents bool) {
	if mkdirParents {
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
			return
		}
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	f, err := os.OpenFile(full, flags, 0o644)
	if err != nil {
		switch {
		case os.IsExist(err):
			_ = writeAck(stream, protocol.FileTransferStatus_AlreadyExists, 0)
		case os.IsNotExist(err):
			// The leaf is created by O_CREATE, so ENOENT can only mean
			// a missing parent directory — diagnosable, unlike io_error.
			_ = writeAck(stream, protocol.FileTransferStatus_NotFound, 0)
		default:
			_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
		}
		return
	}
	... // rest unchanged
```

`runDirPush` — new parameter; before the final rename:

```go
func (s *Session) runDirPush(stream trsf.BidirectionalStream, worktreeDir, dest string, force, mkdirParents bool) {
	...
	if mkdirParents {
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
			return
		}
	}
	if err := os.Rename(staging, dest); err != nil {
		if os.IsNotExist(err) {
			_ = writeAck(stream, protocol.FileTransferStatus_NotFound, 0)
		} else {
			_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
		}
		return
	}
	...
```

New `runMkdir` (place after `runDelete`):

```go
// runMkdir creates the directory at full. parents=false is strict
// os.Mkdir: a missing parent acks not_found, an existing directory acks
// already_exists. parents=true is os.MkdirAll: parents are created and
// an existing directory is ok (idempotent). A regular file at the leaf
// acks not_a_directory in both modes. Mirrors Unix mkdir / mkdir -p.
// Path validation + symlink rejection already ran in the dispatcher.
func (s *Session) runMkdir(stream trsf.BidirectionalStream, full string, parents bool) {
	if fi, err := os.Lstat(full); err == nil {
		if !fi.IsDir() {
			_ = writeAck(stream, protocol.FileTransferStatus_NotADirectory, 0)
			return
		}
		if parents {
			_ = writeAck(stream, protocol.FileTransferStatus_Ok, 0)
		} else {
			_ = writeAck(stream, protocol.FileTransferStatus_AlreadyExists, 0)
		}
		return
	} else if !os.IsNotExist(err) {
		_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
		return
	}
	var err error
	if parents {
		err = os.MkdirAll(full, 0o755)
	} else {
		err = os.Mkdir(full, 0o755)
	}
	if err != nil {
		if os.IsNotExist(err) {
			_ = writeAck(stream, protocol.FileTransferStatus_NotFound, 0)
		} else {
			_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
		}
		return
	}
	_ = writeAck(stream, protocol.FileTransferStatus_Ok, 0)
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./runner/`
Expected: PASS (new tests plus all pre-existing file-transfer tests — the existing `PushOK`/`PushAlreadyExists` tests exercise the changed signatures with `false, false`... note they call `handleOpenFileTransfer`, not `runPush` directly, so they need no edits; a fresh `RunnerOpenFileTransferRequest` has `mkdir_parents=0`).

- [ ] **Step 5: Commit**

```bash
git add runner/file_transfer.go runner/file_transfer_test.go
git commit -m "feat(runner): mkdir direction + mkdir_parents on push, not_found for missing parent"
```

---

### Task 3: Client library + server relay — FilePushOpts, FileMkdir, error mapping

**Files:**
- Modify: `cli/file_transfer.go` (`OpenFileTransfer` ~line 27)
- Modify: `cli/file_push.go` (all three push helpers, `ackError`, add `IsNotFound`)
- Create: `cli/file_mkdir.go`
- Modify: `cli/file_pull.go` (3 `OpenFileTransfer` call sites), `cli/file_delete.go` (1 call site)
- Modify: `server/file_transfer.go` (`handleOpenFileTransfer` ~line 58: copy the bit)
- Modify (mechanical, keep compiling): `tui/file.go` `DoFilePush` internals, `cmd/harness-webui-wasm/main.go` `harnessFilePushBytes` call site, any `integration/` callers of the push helpers
- Test: `cli/file_push_test.go` (create)

**Interfaces:**
- Consumes: Task 1 generated accessors.
- Produces (used by Tasks 4–8):
  - `type FilePushOpts struct { Force, MkdirParents bool }` in package `cli`
  - `func (c *Client) FilePush(ctx context.Context, taskIDHex, localPath, remoteRel string, opts FilePushOpts) error`
  - `func (c *Client) FilePushBytes(ctx context.Context, taskIDHex string, data []byte, remoteRel string, opts FilePushOpts, onProgress ProgressFunc) error`
  - `func (c *Client) FilePushDir(ctx context.Context, taskIDHex, localDir, remoteRel string, opts FilePushOpts) error`
  - `func (c *Client) FileMkdir(ctx context.Context, taskIDHex, remoteRel string, parents bool) error`
  - `func (c *Client) OpenFileTransfer(ctx, taskIDHex, direction, relPath, expectedSize, force, mkdirParents) (trsf.BidirectionalStream, error)` (mkdirParents appended)
  - `func IsNotFound(err error) bool`

- [ ] **Step 1: Write the failing unit test**

Create `cli/file_push_test.go`:

```go
package cli

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestAckErrorMessages(t *testing.T) {
	cases := []struct {
		op     string
		status protocol.FileTransferStatus
		want   string // substring
	}{
		{"push", protocol.FileTransferStatus_NotFound, "parent directory does not exist (use -p/--parents"},
		{"push --recursive", protocol.FileTransferStatus_NotFound, "parent directory does not exist"},
		{"pull", protocol.FileTransferStatus_NotFound, "not found"},
		{"mkdir", protocol.FileTransferStatus_NotFound, "parent directory does not exist (use -p/--parents)"},
		{"mkdir", protocol.FileTransferStatus_AlreadyExists, "directory already exists"},
		{"mkdir", protocol.FileTransferStatus_NotADirectory, "exists and is not a directory"},
	}
	for _, tc := range cases {
		err := ackError(tc.op, &protocol.FileTransferAck{Status: tc.status})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ackError(%q,%v) = %v, want substring %q", tc.op, tc.status, err, tc.want)
		}
	}
}

func TestIsNotFound(t *testing.T) {
	err := ackError("push", &protocol.FileTransferAck{Status: protocol.FileTransferStatus_NotFound})
	if !IsNotFound(err) {
		t.Error("IsNotFound(not_found ack) = false")
	}
	if IsNotFound(ackError("push", &protocol.FileTransferAck{Status: protocol.FileTransferStatus_IoError})) {
		t.Error("IsNotFound(io_error ack) = true")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./cli/ -run 'AckError|IsNotFound' 2>&1 | head`
Expected: FAIL — message substrings absent, `IsNotFound` undefined.

- [ ] **Step 3: Implement the cli package changes**

`cli/file_transfer.go` — append the parameter and set the bit:

```go
func (c *Client) OpenFileTransfer(
	ctx context.Context,
	taskIDHex string,
	direction protocol.FileTransferDirection,
	relPath string,
	expectedSize uint64,
	force bool,
	mkdirParents bool,
) (trsf.BidirectionalStream, error) {
	...
	body.SetRelPath([]byte(relPath))
	body.SetForce(force)
	body.SetMkdirParents(mkdirParents)
	...
```

`cli/file_push.go`:

```go
// FilePushOpts controls push behavior. Force overwrites an existing
// destination. MkdirParents creates missing parent directories of the
// destination on the runner (mkdir -p semantics) before writing.
type FilePushOpts struct {
	Force        bool
	MkdirParents bool
}
```

Change the three helpers to take `opts FilePushOpts` in place of `force bool` (doc comments updated to mention MkdirParents), threading both fields:

- `FilePush(ctx, taskIDHex, localPath, remoteRel string, opts FilePushOpts)` → `filePushFromReader(..., opts, nil)`
- `FilePushBytes(ctx, taskIDHex string, data []byte, remoteRel string, opts FilePushOpts, onProgress ProgressFunc)`
- `filePushFromReader(ctx, taskIDHex, src, size, remoteRel string→, opts FilePushOpts, onProgress)` → `OpenFileTransfer(ctx, taskIDHex, protocol.FileTransferDirection_Push, remoteRel, size, opts.Force, opts.MkdirParents)`
- `FilePushDir(ctx, taskIDHex, localDir, remoteRel string, opts FilePushOpts)` → `OpenFileTransfer(..., FileTransferDirection_DirPush, remoteRel, 0, opts.Force, opts.MkdirParents)`

`ackError` — replace the `NotFound`, `AlreadyExists`, and `NotADirectory` cases:

```go
	case protocol.FileTransferStatus_NotFound:
		switch op {
		case "push", "push --recursive":
			msg = fmt.Sprintf("file %s: destination parent directory does not exist (use -p/--parents or file mkdir)", op)
		case "mkdir":
			msg = "file mkdir: parent directory does not exist (use -p/--parents)"
		default:
			msg = fmt.Sprintf("file %s: not found", op)
		}
	case protocol.FileTransferStatus_AlreadyExists:
		if op == "mkdir" {
			msg = "file mkdir: directory already exists"
		} else {
			msg = fmt.Sprintf("file %s: destination already exists (use --force to overwrite)", op)
		}
	...
	case protocol.FileTransferStatus_NotADirectory:
		if op == "mkdir" {
			msg = "file mkdir: exists and is not a directory"
		} else {
			msg = fmt.Sprintf("file %s: not a directory", op)
		}
```

Add next to `IsAlreadyExists`:

```go
// IsNotFound reports whether err originated as a
// FileTransferStatus_NotFound ack from the runner — for push ops this
// means the destination's parent directory is missing. Used by
// interactive callers (WebUI) to offer a create-parents retry.
func IsNotFound(err error) bool {
	var fe *FileAckError
	return errors.As(err, &fe) && fe.Status == protocol.FileTransferStatus_NotFound
}
```

Create `cli/file_mkdir.go` (mirrors `fileDeleteCommon`'s ack-only stream shape):

```go
package cli

import (
	"context"
	"fmt"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// FileMkdir creates a directory at remoteRel inside the worktree of
// taskIDHex. parents=false is strict os.Mkdir on the runner (missing
// parent → not_found, existing dir → already_exists); parents=true is
// os.MkdirAll (parents created, existing dir is ok). Mirrors Unix
// mkdir / mkdir -p. Reuses the OpenFileTransfer stream the way delete
// does: no payload bytes flow either direction, the runner acks and
// closes.
func (c *Client) FileMkdir(ctx context.Context, taskIDHex, remoteRel string, parents bool) error {
	stream, err := c.OpenFileTransfer(ctx, taskIDHex, protocol.FileTransferDirection_Mkdir, remoteRel, 0, false, parents)
	if err != nil {
		return err
	}
	defer stream.CloseBoth()
	// Half-close our send side so the server-side splice's
	// client→runner relay EOFs and the runner can ack (see
	// fileDeleteCommon for the same dance).
	if err := stream.AppendData(true); err != nil {
		return fmt.Errorf("file mkdir: half-close: %w", err)
	}
	ack, err := ReadFileTransferAck(stream)
	if err != nil {
		return fmt.Errorf("file mkdir: read ack: %w", err)
	}
	return ackError("mkdir", ack)
}
```

- [ ] **Step 4: Server relay copies the bit**

`server/file_transfer.go`, after `body.SetForce(req.Force())`:

```go
	body.SetMkdirParents(req.MkdirParents())
```

- [ ] **Step 5: Fix every OpenFileTransfer / push-helper call site (mechanical)**

Run: `grep -rn 'OpenFileTransfer(\|FilePush\|FilePushDir\|FilePushBytes' --include='*.go' . | grep -v _test.go | grep -v '.harness-worktrees'`

Fix each (expected complete list — flag anything extra you find):
- `cli/file_pull.go:79,112,200` — append `, false` (pull never creates parents remotely).
- `cli/file_delete.go:29` — append `, false`.
- `tui/file.go` `DoFilePush` — keep its signature for now (Task 5 extends it); internals become `c.FilePushDir(ctx, taskID, localSrc, remoteDst, cli.FilePushOpts{Force: force})` / `c.FilePush(..., cli.FilePushOpts{Force: force})`.
- `cmd/harness-webui-wasm/main.go:1448` — `c.FilePushBytes(rootCtx, taskID, data, remoteRel, cli.FilePushOpts{Force: force}, onProgress)`.
- Update any `integration/*_test.go` callers the grep reveals the same way (`force` → `cli.FilePushOpts{Force: force}`).

- [ ] **Step 6: Build + test**

Run: `go build ./... && make wasm-check && go test ./cli/ ./server/ ./runner/ ./tui/`
Expected: all PASS; `AckErrorMessages` and `IsNotFound` now green.

- [ ] **Step 7: Commit**

```bash
git add cli/ server/file_transfer.go tui/file.go cmd/harness-webui-wasm/main.go integration/
git commit -m "feat(cli,server): FilePushOpts with MkdirParents, FileMkdir helper, parent-not-found messages"
```

---

### Task 4: CLI binary — `file push -p` and `file mkdir`

**Files:**
- Modify: `cmd/harness-cli/main.go` (`case "file"` block ~line 335, help text ~line 643)

**Interfaces:**
- Consumes: `cli.FilePushOpts`, `c.FileMkdir` (Task 3).

- [ ] **Step 1: Add the flag and subcommand**

In the `case "push":` FlagSet (after the force flags):

```go
			parents := fs.Bool("parents", false, "create missing parent directories of the destination (mkdir -p)")
			fs.BoolVar(parents, "p", false, "alias for --parents")
```

Usage line becomes:

```go
				fmt.Fprintln(os.Stderr, "usage: harness-cli file push [-r] [-f] [-p] <task-id> <local-src> <worktree-rel-dst>")
```

Both calls become:

```go
			opts := cli.FilePushOpts{Force: *force, MkdirParents: *parents}
			if *recursive {
				if err := c.FilePushDir(ctx, pargs[0], pargs[1], pargs[2], opts); err != nil {
					die(err)
				}
			} else {
				if err := c.FilePush(ctx, pargs[0], pargs[1], pargs[2], opts); err != nil {
					die(err)
				}
			}
```

New subcommand after `case "ls":`:

```go
		case "mkdir":
			fs := flag.NewFlagSet("file mkdir", flag.ExitOnError)
			parents := fs.Bool("parents", false, "create missing parent directories (mkdir -p); also makes an existing directory a success")
			fs.BoolVar(parents, "p", false, "alias for --parents")
			fs.Parse(rest)
			pargs := fs.Args()
			if len(pargs) != 2 {
				fmt.Fprintln(os.Stderr, "usage: harness-cli file mkdir [-p] <task-id> <worktree-rel-dir>")
				os.Exit(2)
			}
			if err := c.FileMkdir(ctx, pargs[0], pargs[1], *parents); err != nil {
				die(err)
			}
```

Update the two enumerations:
- ~line 337: `"usage: harness-cli file {push|pull|ls|mkdir|delete} ..."`
- Help block ~line 643: change the push line to `file push [-r|--recursive] [-f|--force] [-p|--parents] TASK_ID LOCAL_SRC WORKTREE_REL_DST` and add directly under it: `  file mkdir [-p|--parents] TASK_ID WORKTREE_REL_DIR`

- [ ] **Step 2: Build check (no stray binary)**

Run: `go build -o /dev/null ./cmd/harness-cli && go vet ./cmd/harness-cli`
Expected: clean.

- [ ] **Step 3: Commit**

```bash
git add cmd/harness-cli/main.go
git commit -m "feat(cli-bin): file push -p/--parents flag and file mkdir subcommand"
```

---

### Task 5: TUI cmdline + app wiring

**Files:**
- Modify: `tui/cmdline.go` (`FilePushAction` ~line 113, `isAction` block ~line 200, `parseFile` ~line 568)
- Modify: `tui/file.go` (`DoFilePush` signature, add `DoFileMkdir`)
- Modify: `tui/app.go` (`FilePushAction` case ~line 1563, new `FileMkdirAction` case)
- Modify: `tui/filepicker.go:461,680` (two `DoFilePush` call sites gain a `false` parents arg)
- Test: `tui/cmdline_test.go` (append)

**Interfaces:**
- Consumes: `cli.FilePushOpts`, `(*cli.Client).FileMkdir` (Task 3).
- Produces: `FilePushAction.Parents bool`; `type FileMkdirAction struct { TaskID, RelPath string; Parents bool }`; `DoFilePush(c *cli.Client, taskID, localSrc, remoteDst string, recursive, force, parents bool) tea.Cmd`; `DoFileMkdir(c *cli.Client, taskID, relPath string, parents bool) tea.Cmd` (Task 6 consumes `DoFileMkdir`).

- [ ] **Step 1: Write the failing parser tests**

Append to `tui/cmdline_test.go`:

```go
func TestParseFilePushParents(t *testing.T) {
	got, err := ParseCommand(`file push -p deadbeef ./f rel/dir/f`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(FilePushAction)
	if !a.Parents || a.Force || a.Recursive {
		t.Errorf("flags = %+v want Parents only", a)
	}
}

func TestParseFileMkdir(t *testing.T) {
	got, err := ParseCommand(`file mkdir -p deadbeef rel/new/dir`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(FileMkdirAction)
	if a.TaskID != "deadbeef" || a.RelPath != "rel/new/dir" || !a.Parents {
		t.Errorf("parsed = %+v", a)
	}
	if _, err := ParseCommand(`file mkdir deadbeef`, "/cwd"); err == nil {
		t.Error("missing rel arg accepted")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./tui/ -run 'ParseFilePushParents|ParseFileMkdir' 2>&1 | head`
Expected: compile error (`FileMkdirAction` undefined / no `Parents` field).

- [ ] **Step 3: Implement**

`tui/cmdline.go` — extend `FilePushAction` (comment updated to mention Parents) and add:

```go
type FilePushAction struct {
	TaskID    string
	LocalSrc  string
	RemoteDst string
	Recursive bool
	Force     bool
	Parents   bool // create missing parent dirs of RemoteDst (mkdir -p)
}

// FileMkdirAction creates a directory under a task's worktree.
// Parents=false is strict mkdir (missing parent → error, existing dir
// → error); Parents=true is mkdir -p (parents created, idempotent).
type FileMkdirAction struct {
	TaskID  string
	RelPath string
	Parents bool
}
```

Add `func (FileMkdirAction) isAction() {}` to the isAction block.

`parseFile` — add to the push FlagSet:

```go
			parents := fs.Bool("parents", false, "")
			fs.BoolVar(parents, "p", false, "")
```

push usage string → `file push [-r] [-f] [-p] <task-id> <local-src> <worktree-rel-dst>`, and the return adds `Parents: *parents`.

New verb before `default:`:

```go
	case "mkdir":
		fs := flag.NewFlagSet("file mkdir", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		parents := fs.Bool("parents", false, "")
		fs.BoolVar(parents, "p", false, "")
		if err := fs.Parse(rest); err != nil {
			return nil, fmt.Errorf("file mkdir: %w", err)
		}
		pargs := fs.Args()
		if len(pargs) != 2 {
			return nil, fmt.Errorf("file mkdir: usage: file mkdir [-p] <task-id> <worktree-rel-dir>")
		}
		return FileMkdirAction{TaskID: pargs[0], RelPath: pargs[1], Parents: *parents}, nil
```

Update both sub-verb enumerations in `parseFile` to `(ls | push | pull | mkdir | delete)`.

`tui/file.go` — extend `DoFilePush` and add `DoFileMkdir`:

```go
func DoFilePush(c *cli.Client, taskID, localSrc, remoteDst string, recursive, force, parents bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		opts := cli.FilePushOpts{Force: force, MkdirParents: parents}
		var err error
		if recursive {
			err = c.FilePushDir(ctx, taskID, localSrc, remoteDst, opts)
		} else {
			err = c.FilePush(ctx, taskID, localSrc, remoteDst, opts)
		}
		return FileResultMsg{
			Op:     "push",
			TaskID: taskID,
			Detail: fmt.Sprintf("%s -> %s", localSrc, remoteDst),
			Err:    err,
		}
	}
}

// DoFileMkdir creates a directory under the task's worktree. parents
// mirrors mkdir -p (create missing parents, existing dir is ok);
// without it the runner is strict.
func DoFileMkdir(c *cli.Client, taskID, relPath string, parents bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := c.FileMkdir(ctx, taskID, relPath, parents)
		label := relPath
		if parents {
			label += " (-p)"
		}
		return FileResultMsg{
			Op:     "mkdir",
			TaskID: taskID,
			Detail: label,
			Err:    err,
		}
	}
}
```

Also update `FileResultMsg`'s doc comment verb list to include "mkdir".

`tui/app.go` — `FilePushAction` case passes the new field, and add the mkdir case right after it (same resolve-prefix shape as `FileLsAction`):

```go
	case FilePushAction:
		...
		return a, DoFilePush(a.client, full, v.LocalSrc, v.RemoteDst, v.Recursive, v.Force, v.Parents)
	case FileMkdirAction:
		full, errStr := a.resolveTaskIDPrefix(v.TaskID)
		if errStr != "" {
			a.cmdresult.Append(ErrorStyle.Render(errStr))
			return a, nil
		}
		return a, DoFileMkdir(a.client, full, v.RelPath, v.Parents)
```

`tui/filepicker.go:461,680` — the two `DoFilePush(...)` calls gain a trailing `false` (picker push never needs parents; its dest parent always exists).

If the TUI cmdline has a help text listing file verbs (`grep -n 'file push' tui/*.go` beyond cmdline.go), update it the same way as the CLI help.

- [ ] **Step 4: Run tests**

Run: `go test ./tui/`
Expected: PASS including the two new parser tests.

- [ ] **Step 5: Commit**

```bash
git add tui/
git commit -m "feat(tui): file push -p and file mkdir via cmdline, DoFileMkdir helper"
```

---

### Task 6: TUI filepicker — `+` new-folder key

**Files:**
- Modify: `tui/filepicker.go` (mode enum ~line 30, `handleKey` ~line 246, `handleBrowseKey` ~line 261, browse help line ~line 748, footer switch ~line 767)

**Interfaces:**
- Consumes: `DoFileMkdir` (Task 5). Result rendering + auto-refresh come free: the existing generic `FileResultMsg` branch (~line 206) renders `"mkdir ok: <detail>"` / `"mkdir error: ..."` and re-lists `curDir` for any Op.

- [ ] **Step 1: Add the input mode**

```go
const (
	pickerNone pickerInputMode = iota
	pickerAskPushSrc
	pickerAskPullDst
	pickerAskNewDirName
	pickerConfirmDelete
	pickerConfirmPullOverwrite
	pickerConfirmPushOverwrite
)
```

- [ ] **Step 2: Route it in handleKey**

```go
	case pickerAskPushSrc, pickerAskPullDst:
		return m.handleInputKey(k)
	case pickerAskNewDirName:
		return m.handleNewDirKey(k)
```

- [ ] **Step 3: Bind `+` in handleBrowseKey** (after the `"D"` case)

```go
	case "+":
		// New folder: prompt for a directory name, created under the
		// current directory with mkdir -p semantics — the typed name
		// may be nested (a/b/c) and the result shows up in the
		// listing right after the auto-refresh.
		m.inputMode = pickerAskNewDirName
		m.input.Reset()
		m.input.Placeholder = "new directory name (nested ok, e.g. a/b/c)"
		m.input.Focus()
		return m, nil
```

- [ ] **Step 4: The mode's key handler** (place next to `handleInputKey`; no Tab/local-browse — a remote dir name has nothing to pick locally)

```go
// handleNewDirKey drives the pickerAskNewDirName prompt: Enter commits
// a FileMkdir (parents=true) under curDir, Esc cancels, anything else
// edits the input. Unlike handleInputKey there is no Tab/local-browser
// integration — the input is a remote-relative name, not a local path.
func (m FilePickerModel) handleNewDirKey(k tea.KeyMsg) (FilePickerModel, tea.Cmd) {
	switch k.Type {
	case tea.KeyEsc:
		m.inputMode = pickerNone
		m.input.Blur()
		return m, nil
	case tea.KeyEnter:
		name := strings.TrimSpace(m.input.Value())
		if name == "" {
			return m, nil
		}
		rel := joinRel(m.curDir, name)
		m.inputMode = pickerNone
		m.input.Blur()
		m.msg = "creating " + rel + "..."
		return m, DoFileMkdir(m.client, m.taskID, rel, true)
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	return m, cmd
}
```

- [ ] **Step 5: Render the prompt + advertise the key**

Browse help line (~748) gains `+ new dir`:

```go
		header.WriteString("↑↓ nav · → / Enter descend · ← / Backspace back · u push · g pull · d delete · D rm -rf · + new dir · r reload · Esc close\n")
```

Footer switch gains:

```go
	case pickerAskNewDirName:
		fmt.Fprintf(&footer, "\nnew directory under %s: %s\n(Enter: create (mkdir -p) · Esc: cancel)\n",
			displayDir(m.curDir), m.input.View())
```

If no `displayDir` helper exists, render `m.curDir` with the same fallback the header uses for the root (grep how the header prints `curDir`; root is the empty string — print `"."` for it).

- [ ] **Step 6: Build + full TUI tests**

Run: `go build ./... && go test ./tui/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add tui/filepicker.go
git commit -m "feat(tui): filepicker + key creates a new remote directory"
```

---

### Task 7: WASM bridge + WebUI JS — parents arg, fileMkdir, confirm-retry, New-folder button

**Files:**
- Modify: `cmd/harness-webui-wasm/main.go` (registration map ~line 93, `harnessFilePushBytes` ~line 1423, add `harnessFileMkdir`)
- Modify: `webui/static/main.js` (`filePushBtn` handler ~line 550, help text ~line 1318, `runFileCmd` ~line 2561, `filePushCmd` ~line 2618, file-browser button wiring ~line 348/447)
- Modify: `webui/index.html` (file browser buttons ~line 109–116)

**Interfaces:**
- Consumes: `cli.FilePushOpts`, `(*cli.Client).FileMkdir`, and the `not_found` rejection code that `rejectFileErr` already emits (no change needed there).
- Produces (JS API): `harness.filePushBytes(taskID, remoteRel, data, force, parents[, onProgress])` (parents inserted at index 4, progress moves to index 5); `harness.fileMkdir(taskID, rel, parents) -> Promise<void>`.

- [ ] **Step 1: wasm — extend filePushBytes**

In `harnessFilePushBytes`: doc comment updated to the new signature; require 5 args; read `parents := args[4].Truthy()`; `onProgress := jsProgress(args, 5)`; call becomes

```go
			if err := c.FilePushBytes(rootCtx, taskID, data, remoteRel, cli.FilePushOpts{Force: force, MkdirParents: parents}, onProgress); err != nil {
```

(The error message for missing args becomes "filePushBytes: missing taskID / remoteRel / data / force / parents args".)

- [ ] **Step 2: wasm — add harnessFileMkdir** (model on `harnessFileDelete`; place next to it)

```go
// harnessFileMkdir creates a directory at rel inside taskID's worktree.
// parents=false is strict mkdir (missing parent rejects with
// code="not_found", existing dir with code="already_exists");
// parents=true is mkdir -p (parents created, existing dir resolves).
//
//	harness.fileMkdir(taskID, rel, parents) -> Promise<void>
func harnessFileMkdir(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		if len(args) < 3 {
			rejectErr(reject, errors.New("fileMkdir: missing taskID / rel / parents args"))
			return nil
		}
		taskID := args[0].String()
		rel := args[1].String()
		parents := args[2].Truthy()
		go func() {
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			if err := c.FileMkdir(rootCtx, taskID, rel, parents); err != nil {
				rejectFileErr(reject, err)
				return
			}
			resolve.Invoke(js.Undefined())
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}
```

Register it in the map at ~line 93: `"fileMkdir": js.FuncOf(harnessFileMkdir),`.

- [ ] **Step 3: main.js — thread parents through both push paths with a not_found confirm-retry**

`filePushBtn` handler: the retry loop gains a `parents` flag mirroring `force`:

```js
      let force = false;
      let parents = false;
      for (;;) {
        try {
          await window.harness.filePushBytes(taskID, remoteRel, buf, force, parents, fp.onProgress);
          fileResultPre.textContent = `${force ? "push ok (overwritten)" : "push ok"}: ${file.name} -> ${remoteRel} (${buf.byteLength} bytes)`;
          break;
        } catch (e) {
          if (!force && e && e.code === "already_exists") {
            if (!window.confirm(`${remoteRel} already exists on the runner. Overwrite?`)) {
              fileResultPre.textContent = "push cancelled (overwrite declined)";
              return;
            }
            force = true;
            continue; // retry with overwrite
          }
          if (!parents && e && e.code === "not_found") {
            if (!window.confirm(`${remoteRel} の親ディレクトリが存在しません。作成して再試行しますか?`)) {
              fileResultPre.textContent = "push cancelled (missing parent dir)";
              return;
            }
            parents = true;
            continue; // retry creating parent dirs
          }
          fileResultPre.textContent = `push error: ${e.message}`;
          return;
        }
      }
```

`filePushCmd` (cmdline): same two-flag retry loop replaces the current single already_exists retry — reuse the loop above, returning strings instead of setting `fileResultPre` (`return \`push ok...\`` / `return "push cancelled (...)"`).

- [ ] **Step 4: main.js — mkdir cmdline verb + help**

`runFileCmd`: sub-verb error string → `(ls | delete | push | pull | mkdir)`; add:

```js
    case "mkdir":
      return fileMkdirCmd(args);
```

New function next to `fileDeleteCmd`:

```js
async function fileMkdirCmd(args) {
  let parents = false;
  const pos = [];
  for (const a of args) {
    if (a === "-p" || a === "--parents") { parents = true; continue; }
    pos.push(a);
  }
  if (pos.length !== 2) {
    throw new Error("usage: file mkdir [-p] <task-id> <worktree-rel-dir>");
  }
  const [taskID, rel] = pos;
  await window.harness.fileMkdir(taskID, rel, parents);
  return `mkdir ok: ${rel}`;
}
```

Help text (~line 1318): add under the `file push` line:

```
            "  file mkdir [-p] <task> <rel>",
            "                            create a worktree directory (-p: parents, idempotent)",
```

- [ ] **Step 5: index.html + main.js — New folder button in the file browser**

`webui/index.html` (next to `file-refresh-btn`, ~line 110):

```html
        <button id="file-mkdir-btn" disabled>New folder</button>
```

`main.js`: alongside the other lookups (~line 348) `const fileMkdirBtn = document.getElementById("file-mkdir-btn");`; in the enable/disable block (~line 447) `fileMkdirBtn.disabled = !hasTask;`; handler next to `filePushBtn`'s:

```js
  fileMkdirBtn.addEventListener("click", async () => {
    const taskID = fileTaskSelect.value;
    if (!taskID) return;
    const name = window.prompt("新しいディレクトリ名 (ネスト可, 例: a/b/c):");
    if (!name) return;
    const rel = joinFsPath(filePickerCurDir, name);
    try {
      // parents=true: 入力名がネストしていても作成でき、結果は直後の
      // 一覧更新で見える(TUI picker の + キーと同じ semantics)。
      await window.harness.fileMkdir(taskID, rel, true);
      fileResultPre.textContent = `mkdir ok: ${rel}`;
      refreshFilePicker();
    } catch (e) {
      fileResultPre.textContent = `mkdir error: ${e.message}`;
    }
  });
```

(Match the surrounding comment language — this file's comments are mixed JP/EN; keep whichever the sibling handlers use.)

- [ ] **Step 6: Build wasm + embed check**

Run: `make wasm-check && make webui-build && go build ./...`
Expected: all succeed; `webui/static/main.wasm` refreshed (do NOT commit main.wasm if it is gitignored — check `git status`).

- [ ] **Step 7: Commit**

```bash
git add cmd/harness-webui-wasm/main.go webui/static/main.js webui/index.html
git commit -m "feat(webui): push parents retry, fileMkdir bridge + cmdline verb + New folder button"
```

---

### Task 8: Integration E2E + wire-skew + full verification

**Files:**
- Modify: `integration/file_transfer_e2e_test.go` (append cases inside or alongside `TestFileTransferE2E`)

**Interfaces:**
- Consumes: everything above end-to-end (client → server splice → runner).

- [ ] **Step 1: Add E2E coverage**

Inside `TestFileTransferE2E` (after the existing negative-path checks, where a connected `*cli.Client` `c` and running task `taskID` are in scope — adapt local variable names to the file):

```go
	// Missing-parent push without -p: diagnosable not_found.
	err = c.FilePush(ctx, taskID, localFile, "newdir/sub/pushed.txt", cli.FilePushOpts{})
	if err == nil || !cli.IsNotFound(err) {
		t.Fatalf("push into missing dir: got %v, want not_found", err)
	}
	// Same push with MkdirParents: succeeds.
	if err := c.FilePush(ctx, taskID, localFile, "newdir/sub/pushed.txt", cli.FilePushOpts{MkdirParents: true}); err != nil {
		t.Fatalf("push -p: %v", err)
	}
	// Strict mkdir: parent now exists → ok; repeat → already_exists.
	if err := c.FileMkdir(ctx, taskID, "newdir/made", false); err != nil {
		t.Fatalf("strict mkdir: %v", err)
	}
	if err := c.FileMkdir(ctx, taskID, "newdir/made", false); err == nil || !cli.IsAlreadyExists(err) {
		t.Fatalf("strict mkdir repeat: got %v, want already_exists", err)
	}
	// -p mkdir: deep chain + idempotent.
	if err := c.FileMkdir(ctx, taskID, "p1/p2/p3", true); err != nil {
		t.Fatalf("mkdir -p: %v", err)
	}
	if err := c.FileMkdir(ctx, taskID, "p1/p2/p3", true); err != nil {
		t.Fatalf("mkdir -p repeat: %v", err)
	}
	// Strict mkdir with missing parent → not_found.
	if err := c.FileMkdir(ctx, taskID, "q1/q2/q3", false); err == nil || !cli.IsNotFound(err) {
		t.Fatalf("strict mkdir missing parent: got %v, want not_found", err)
	}
```

(`localFile` = whatever local fixture path the test already pushes; reuse it. If the existing test's structure makes appending awkward, add a sibling `TestFileMkdirAndParentsE2E` that copies the existing server/runner/task bootstrap verbatim.)

- [ ] **Step 2: Run the E2E**

Run: `go test ./integration/ -run FileTransferE2E -v -timeout 300s` (plus `-run FileMkdir` if a new func was added)
Expected: PASS.

- [ ] **Step 3: Wire-skew check** (`.bgn` changed → mandatory, Pitfall 10)

Run: `scripts/wire-skew-check.sh`
Expected: PASS — new runner × old server must degrade to a RECOVERABLE rejection, not a fatal wipe. Exit 2 means setup error (fix the environment, not the code); a FAIL here blocks landing.

- [ ] **Step 4: Full repo verification** (memory: verify with make targets, not ad-hoc builds)

Run: `make check && make test`
Expected: both PASS; `git status` clean apart from intended changes.

- [ ] **Step 5: Commit**

```bash
git add integration/file_transfer_e2e_test.go
git commit -m "test(integration): e2e for push -p and file mkdir strict/-p"
```

---

## Landing & deployment (after all tasks pass review)

Per `landing-policy-remote-agent-harness` (Mode A, local-trunk-authoritative) and memory `feedback_build_after_landing`:

1. All commits are already on local `main` (parent repo). `git push origin main` (FF only — never force, never cherry-pick).
2. `make build` in the parent checkout, unprompted (bin/ is stale after any land).
3. **Deployment needs the user's go-ahead and MUST be server-first** (Pitfall 10): restart the server (different host) BEFORE `/restart-all` for the runner fleet. Old-runner/new-server skew during the window degrades to pre-change behavior (bit ignored / mkdir rejected as unknown direction) and self-heals on runner restart.
