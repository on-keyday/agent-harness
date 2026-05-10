# Directory Transfer + --force Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `harness-cli file push --recursive` / `pull --recursive` for whole-directory transfer (tar over the existing `OpenFileTransfer` stream, atomic-on-the-receiver via staging dir + rename), plus a uniform `--force` flag across single and recursive ops.

**Architecture:** Wire body of dir transfers is a tar stream produced/consumed via Go stdlib `archive/tar`. Receiver always stages the entire tree to `.harness-staging/<random>/` (or `<localDir>.staging-<random>/` on the client) and `rename(2)`s into the final dest only after the stream completes successfully. `--force` is mapped to: `O_TRUNC` for single-file push, `O_TRUNC` for single-file pull (default changes from `O_TRUNC` to `O_EXCL`), `RemoveAll(dest)` + rename for dir push and dir pull. Symlinks / hard links / device files are rejected at the receiver.

**Spec:** `docs/superpowers/specs/2026-05-10-file-dir-transfer-design.md`

**Tech Stack:**
- `runner/protocol/message.bgn` (brgen schema)
- Go stdlib: `archive/tar`, `io/fs`, `path/filepath`, `os`
- Reuses existing wiring: `cli.OpenFileTransfer`, `server.handleOpenFileTransfer`, `runner.handleOpenFileTransfer`, `spliceBidiHalfClose`

**Branch:** `feature/file-dir-transfer` (already cut from `feature/file-delete`)

---

## File Structure

**Modified files:**
- `runner/protocol/message.bgn` — extend `FileTransferDirection`, `FileTransferStatus`; add `force` flag to `OpenFileTransferRequest` and `RunnerOpenFileTransferRequest`.
- `runner/protocol/message.go` — auto-regenerated; never hand-edited.
- `runner/file_transfer.go` — add `runDirPush`, `runDirPull`, plus dispatch arms for `dir_push`/`dir_pull`. Update `runPush` to honor `force` flag (`O_EXCL` ↔ `O_TRUNC`).
- `runner/file_transfer_test.go` — add unit tests for the new dir paths and the new force-on-push behavior.
- `cli/file_transfer.go` — extend `OpenFileTransfer` signature with `force bool`; thread through to the request.
- `cli/file_push.go` — add `force` parameter to `FilePush`; add `FilePushDir(ctx, taskIDHex, localDir, remoteDir, force)`. Extend `ackError` arms for `not_a_directory`.
- `cli/file_pull.go` — add `force` parameter to `FilePull` (changes default from `O_TRUNC` to `O_EXCL`); add `FilePullDir(ctx, taskIDHex, remoteDir, localDir, force)`.
- `cli/file_delete.go` — pass `force=false` (no-op) on the existing call to `OpenFileTransfer`.
- `cmd/harness-cli/main.go` — extend `file push` and `file pull` subcommand parsers with `--recursive` / `-r` and `--force` / `-f` flags; dispatch to dir variants when `--recursive`. Update help text.
- `integration/file_transfer_e2e_test.go` — add dir round-trip and `--force` overwrite tests.

**No new files.**

---

## Task 1: Schema additions and regeneration

**Files:**
- Modify: `runner/protocol/message.bgn`
- Regenerate: `runner/protocol/message.go`

- [ ] **Step 1: Extend FileTransferDirection and FileTransferStatus**

Open `runner/protocol/message.bgn`. Find `enum FileTransferDirection:` (around line 487) and append `dir_push`, `dir_pull` arms:

```
enum FileTransferDirection:
    :u8
    push
    pull
    delete       # remove a file from the worktree (rejects directories in v1).
    dir_push     # client → runner; body is a tar stream of the source dir.
    dir_pull     # runner → client; body is a tar stream of the source dir.
```

Find `enum FileTransferStatus:` and append `not_a_directory`:

```
enum FileTransferStatus:
    :u8
    ok             = "ok"
    path_invalid   = "path_invalid"
    not_found      = "not_found"
    already_exists = "already_exists"
    io_error       = "io_error"
    canceled       = "canceled"
    is_directory   = "is_directory"   # delete only: rel_path resolved to a directory.
    not_a_directory = "not_a_directory"  # dir_pull only: rel_path resolved to a regular file.
```

- [ ] **Step 2: Add force flag to OpenFileTransferRequest and RunnerOpenFileTransferRequest**

In the same file, find `format OpenFileTransferRequest:` and append a `force :u1` + `reserved :u7` pair:

```
format OpenFileTransferRequest:
    task_id       :TaskID
    direction     :FileTransferDirection
    rel_path_len  :u16
    rel_path      :[rel_path_len]u8
    expected_size :u64
    force         :u1
    reserved      :u7
```

Same for `RunnerOpenFileTransferRequest`:

```
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

- [ ] **Step 3: Regenerate Go code**

Run: `make protoregen`
Expected: `runner/protocol/message.go` rewritten. No errors.

- [ ] **Step 4: Verify build**

Run: `go build ./...`
Expected: success. The new symbols `protocol.FileTransferDirection_DirPush`, `_DirPull`, `protocol.FileTransferStatus_NotADirectory`, and the generated `Force()` / `SetForce(bool)` accessors on both request types must exist.

Run: `git grep -n "FileTransferDirection_DirPush\|FileTransferDirection_DirPull\|FileTransferStatus_NotADirectory\|SetForce" runner/protocol/message.go | head -10`
Expected: each symbol shows at least once.

- [ ] **Step 5: Commit**

```bash
git add runner/protocol/message.bgn runner/protocol/message.go
git commit -m "$(cat <<'EOF'
proto: add dir_push/dir_pull, not_a_directory, and force flag

Append-only enum extensions for directory transfer (the body is a tar
stream over the existing OpenFileTransfer wiring) and a force-overwrite
flag on both client and runner request envelopes. Adds 1 byte to each
of the two request formats — wire-breaking, requires rebuild of all
peers (acceptable per individual-dogfood scope).

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: Single-file push --force support

**Files:**
- Modify: `runner/file_transfer.go` (add `force` parameter to runPush; switch on `O_EXCL` vs `O_TRUNC`)
- Modify: `runner/file_transfer_test.go` (add force-overwrite test for push)
- Modify: `cli/file_transfer.go` (extend OpenFileTransfer signature with `force bool`)
- Modify: `cli/file_push.go` (extend FilePush signature with `force bool`)
- Modify: `cli/file_pull.go` (call OpenFileTransfer with force=false initially; updated in Task 3)
- Modify: `cli/file_delete.go` (call OpenFileTransfer with force=false)

This task does NOT yet wire the CLI flag — that comes in Task 5. It just makes the API plumbing accept and propagate the boolean.

- [ ] **Step 1: Extend OpenFileTransfer to accept force**

Open `cli/file_transfer.go`. Find `func (c *Client) OpenFileTransfer(...)`. Add `force bool` as the last parameter and set it on the request body:

```go
func (c *Client) OpenFileTransfer(
	ctx context.Context,
	taskIDHex string,
	direction protocol.FileTransferDirection,
	relPath string,
	expectedSize uint64,
	force bool,
) (trsf.BidirectionalStream, error) {
	tid, err := parseTaskIDHex(taskIDHex)
	if err != nil {
		return nil, fmt.Errorf("file: parse task id: %w", err)
	}
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_OpenFileTransfer}
	body := protocol.OpenFileTransferRequest{
		TaskId:       tid,
		Direction:    direction,
		ExpectedSize: expectedSize,
	}
	body.SetRelPath([]byte(relPath))
	body.SetForce(force)
	req.SetOpenFileTransfer(body)
	// ... rest unchanged
```

- [ ] **Step 2: Update FilePush, FilePull, FileDelete callers**

In `cli/file_push.go`, change `FilePush` signature and pass `force` through:

```go
func (c *Client) FilePush(ctx context.Context, taskIDHex, localPath, remoteRel string, force bool) error {
	src, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("file push: open local: %w", err)
	}
	defer src.Close()
	st, err := src.Stat()
	if err != nil {
		return fmt.Errorf("file push: stat local: %w", err)
	}
	stream, err := c.OpenFileTransfer(ctx, taskIDHex, protocol.FileTransferDirection_Push, remoteRel, uint64(st.Size()), force)
	// ... rest unchanged
}
```

In `cli/file_pull.go`, change `FilePull` signature; add `force` for the LOCAL write side (this changes the local-file open from O_TRUNC to O_EXCL by default):

```go
func (c *Client) FilePull(ctx context.Context, taskIDHex, remoteRel, localPath string, force bool) error {
	stream, err := c.OpenFileTransfer(ctx, taskIDHex, protocol.FileTransferDirection_Pull, remoteRel, 0, false)
	if err != nil {
		return err
	}
	defer stream.CloseBoth()
	ack, err := ReadFileTransferAck(stream)
	if err != nil {
		return fmt.Errorf("file pull: read ack: %w", err)
	}
	if err := ackError("pull", ack); err != nil {
		return err
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	dst, err := os.OpenFile(localPath, flags, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("file pull: %s already exists (use --force to overwrite)", localPath)
		}
		return fmt.Errorf("file pull: open local: %w", err)
	}
	defer dst.Close()
	n, err := io.Copy(dst, streamReadAll{stream})
	if err != nil {
		return fmt.Errorf("file pull: stream read: %w", err)
	}
	if uint64(n) != ack.ActualSize {
		return fmt.Errorf("file pull: short read (got %d, expected %d)", n, ack.ActualSize)
	}
	return nil
}
```

In `cli/file_delete.go`, pass `false`:

```go
	stream, err := c.OpenFileTransfer(ctx, taskIDHex, protocol.FileTransferDirection_Delete, remoteRel, 0, false)
```

- [ ] **Step 3: Update CLI dispatcher to pass force=false (placeholder for Task 5)**

In `cmd/harness-cli/main.go`, find the existing `case "push":` and `case "pull":` arms in the file subcommand router. Update the calls to pass `false` so the build still works:

```go
		case "push":
			if len(rest) != 3 {
				fmt.Fprintln(os.Stderr, "usage: harness-cli file push <task-id> <local-src> <worktree-rel-dst>")
				os.Exit(2)
			}
			if err := c.FilePush(ctx, rest[0], rest[1], rest[2], false); err != nil {
				die(err)
			}
		case "pull":
			if len(rest) != 3 {
				fmt.Fprintln(os.Stderr, "usage: harness-cli file pull <task-id> <worktree-rel-src> <local-dst>")
				os.Exit(2)
			}
			if err := c.FilePull(ctx, rest[0], rest[1], rest[2], false); err != nil {
				die(err)
			}
```

- [ ] **Step 4: Update runner runPush to honor force**

Open `runner/file_transfer.go`. Find `func (s *Session) runPush(stream trsf.BidirectionalStream, full string)`. Change its signature to accept `force bool` and switch the open flags:

```go
func (s *Session) runPush(stream trsf.BidirectionalStream, full string, force bool) {
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	f, err := os.OpenFile(full, flags, 0o644)
	if err != nil {
		switch {
		case os.IsExist(err):
			_ = writeAck(stream, protocol.FileTransferStatus_AlreadyExists, 0)
		default:
			_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
		}
		return
	}
	// ... rest unchanged (io.Copy, fsync, close, ack)
}
```

Find the dispatch switch in `handleOpenFileTransfer` and pass `req.Force()`:

```go
	switch req.Direction {
	case protocol.FileTransferDirection_Pull:
		s.runPull(stream, full)
	case protocol.FileTransferDirection_Push:
		s.runPush(stream, full, req.Force())
	case protocol.FileTransferDirection_Delete:
		s.runDelete(stream, full)
	default:
		_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
	}
```

- [ ] **Step 5: Add unit test for push --force**

Append to `runner/file_transfer_test.go`:

```go
func TestHandleOpenFileTransfer_PushForceOverwrites(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "x.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	taskIDHex := "00000000000000000000000000000030"
	taskID := mustParseTaskID(t, taskIDHex)

	sess := &Session{NoWorktree: true}
	sess.initMaps()
	sess.tasks[taskIDHex] = &taskEntry{repoPath: tmp}

	clientEnd, runnerEnd := newMemoryBidiPair()
	sess.Streams = staticStreamLookup{1: runnerEnd}

	req := &protocol.RunnerOpenFileTransferRequest{
		TaskId:    taskID,
		StreamId:  1,
		Direction: protocol.FileTransferDirection_Push,
	}
	req.SetRelPath([]byte("x.txt"))
	req.SetForce(true)

	go sess.handleOpenFileTransfer(context.Background(), req)
	if err := clientEnd.AppendData(false, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := clientEnd.AppendData(true); err != nil {
		t.Fatal(err)
	}
	ack := readAck(t, clientEnd)
	if ack.Status != protocol.FileTransferStatus_Ok {
		t.Fatalf("ack status = %v want ok", ack.Status)
	}
	got, _ := os.ReadFile(filepath.Join(tmp, "x.txt"))
	if string(got) != "new" {
		t.Errorf("file = %q want %q", got, "new")
	}
}
```

- [ ] **Step 6: Verify build + tests**

Run: `go build ./... && go test ./runner ./cli ./server -count=1 -timeout 60s`
Expected: all PASS.

- [ ] **Step 7: Commit**

```bash
git add runner/file_transfer.go runner/file_transfer_test.go cli/file_transfer.go cli/file_push.go cli/file_pull.go cli/file_delete.go cmd/harness-cli/main.go
git commit -m "$(cat <<'EOF'
file-transfer: thread force flag through OpenFileTransfer

Adds a bool 'force' parameter to OpenFileTransfer and to FilePush /
FilePull / FileDelete. Runner-side runPush honors it via O_EXCL ↔
O_TRUNC. Client-side FilePull additionally switches its local open from
O_TRUNC to O_EXCL by default (with force restoring O_TRUNC) — a
behavior change that mirrors push's safe default. Delete ignores force.

CLI dispatch passes force=false for now; the user-facing --force flag
lands in the recursive-transfer commit alongside --recursive.

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: Runner-side dir push (runDirPush) + tests

**Files:**
- Modify: `runner/file_transfer.go` (add `runDirPush`, dispatch arm)
- Modify: `runner/file_transfer_test.go` (add 5 unit tests for dir push)

- [ ] **Step 1: Implement runDirPush**

Append to `runner/file_transfer.go`:

```go
import (
	// existing imports ...
	"archive/tar"
	"crypto/rand"
	"encoding/hex"
)

// stagingRoot is the on-disk dir under which dir_push stages incoming trees.
// Lives inside the worktree so rename(2) into the final dest stays on the
// same filesystem.
const stagingRoot = ".harness-staging"

// runDirPush extracts a tar stream into <worktree>/<rel_path>, staging via
// a sibling directory and renaming atomically on success. Refuses to clobber
// an existing dest unless force is set.
func (s *Session) runDirPush(stream trsf.BidirectionalStream, worktreeDir, dest string, force bool) {
	// Reject existing dest when !force; reject when dest is a regular file
	// (we won't replace a file with a directory regardless of force).
	if fi, err := os.Lstat(dest); err == nil {
		if !fi.IsDir() {
			_ = writeAck(stream, protocol.FileTransferStatus_IsDirectory, 0)
			return
		}
		if !force {
			_ = writeAck(stream, protocol.FileTransferStatus_AlreadyExists, 0)
			return
		}
	} else if !os.IsNotExist(err) {
		_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
		return
	}

	staging, err := mkStagingDir(worktreeDir)
	if err != nil {
		_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
		return
	}
	cleanupStaging := true
	defer func() {
		if cleanupStaging {
			_ = os.RemoveAll(staging)
		}
	}()

	tr := tar.NewReader(streamReader2{stream})
	bytesIn := uint64(0)
	for {
		hdr, terr := tr.Next()
		if terr == io.EOF {
			break
		}
		if terr != nil {
			_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
			return
		}
		// Reject anything that isn't a regular file or directory. Hard
		// links, symlinks, devices, FIFOs all fall here.
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeDir {
			_ = writeAck(stream, protocol.FileTransferStatus_PathInvalid, 0)
			return
		}
		entryFull, perr := ValidateRelPath(staging, hdr.Name)
		if perr != nil {
			_ = writeAck(stream, protocol.FileTransferStatus_PathInvalid, 0)
			return
		}
		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(entryFull, 0o755); err != nil {
				_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
				return
			}
			continue
		}
		// Regular file: parent dirs must exist (tar may emit files
		// without preceding dir entries).
		if err := os.MkdirAll(filepath.Dir(entryFull), 0o755); err != nil {
			_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
			return
		}
		mode := os.FileMode(hdr.Mode & 0o777)
		if mode == 0 {
			mode = 0o644
		}
		f, oerr := os.OpenFile(entryFull, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if oerr != nil {
			_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
			return
		}
		n, cerr := io.Copy(f, tr)
		if cerr != nil {
			_ = f.Close()
			_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
			return
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
			return
		}
		if err := f.Close(); err != nil {
			_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
			return
		}
		bytesIn += uint64(n)
	}

	// All entries extracted to staging. Swap in.
	if force {
		if err := os.RemoveAll(dest); err != nil {
			_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
			return
		}
	}
	if err := os.Rename(staging, dest); err != nil {
		_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
		return
	}
	cleanupStaging = false
	_ = writeAck(stream, protocol.FileTransferStatus_Ok, bytesIn)
}

// mkStagingDir creates <worktreeDir>/.harness-staging/<random>/ and returns
// its absolute path. The parent .harness-staging/ is created as needed and
// left in place after success/failure for future transfers (and for prune
// cleanup on runner crash).
func mkStagingDir(worktreeDir string) (string, error) {
	parent := filepath.Join(worktreeDir, stagingRoot)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	dir := filepath.Join(parent, hex.EncodeToString(id[:]))
	if err := os.Mkdir(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}
```

- [ ] **Step 2: Add dir_push dispatch arm**

In `runner/file_transfer.go`, find the `switch req.Direction` block in `handleOpenFileTransfer`. Add an arm for `dir_push`. The arm needs the *worktreeDir* (parent of the resolved `full`) so it can create the staging sibling — re-derive via `worktreeDirFor` (already called earlier in the function; pass it through) or re-resolve. Simpler: store the worktreeDir at the top of `handleOpenFileTransfer` and reuse it. Refactor:

Find the existing line (early in `handleOpenFileTransfer`):

```go
	worktreeDir := s.worktreeDirFor(taskIDHex)
```

Confirm it is captured into a local variable in scope of the switch. Then update the switch:

```go
	switch req.Direction {
	case protocol.FileTransferDirection_Pull:
		s.runPull(stream, full)
	case protocol.FileTransferDirection_Push:
		s.runPush(stream, full, req.Force())
	case protocol.FileTransferDirection_Delete:
		s.runDelete(stream, full)
	case protocol.FileTransferDirection_DirPush:
		s.runDirPush(stream, worktreeDir, full, req.Force())
	default:
		_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
	}
```

- [ ] **Step 3: Add unit tests for runDirPush**

Append to `runner/file_transfer_test.go`:

```go
import (
	// existing imports ...
	"archive/tar"
)

// pushTar packs the entries map (rel name → bytes; "/" suffix marks a dir)
// and feeds the bytes into the client end of the bidi pair as a tar stream.
// Used by all the dir_push tests.
func pushTar(t *testing.T, clientEnd trsf.BidirectionalStream, entries map[string][]byte) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range entries {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}
		if strings.HasSuffix(name, "/") {
			hdr.Typeflag = tar.TypeDir
			hdr.Size = 0
			hdr.Mode = 0o755
		} else {
			hdr.Typeflag = tar.TypeReg
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader(%q): %v", name, err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write(body); err != nil {
				t.Fatalf("Write(%q): %v", name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tw.Close: %v", err)
	}
	if err := clientEnd.AppendData(false, buf.Bytes()); err != nil {
		t.Fatalf("AppendData: %v", err)
	}
	if err := clientEnd.AppendData(true); err != nil {
		t.Fatalf("AppendData(eof): %v", err)
	}
}

// pushSymlinkTar packs a tar stream with a single symlink entry — used to
// verify the receiver rejects symlink entries with PathInvalid.
func pushSymlinkTar(t *testing.T, clientEnd trsf.BidirectionalStream, name, target string) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{Name: name, Linkname: target, Typeflag: tar.TypeSymlink, Mode: 0o644}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := clientEnd.AppendData(false, buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := clientEnd.AppendData(true); err != nil {
		t.Fatal(err)
	}
}

func TestHandleOpenFileTransfer_DirPushOK(t *testing.T) {
	tmp := t.TempDir()
	taskIDHex := "00000000000000000000000000000040"
	taskID := mustParseTaskID(t, taskIDHex)

	sess := &Session{NoWorktree: true}
	sess.initMaps()
	sess.tasks[taskIDHex] = &taskEntry{repoPath: tmp}

	clientEnd, runnerEnd := newMemoryBidiPair()
	sess.Streams = staticStreamLookup{1: runnerEnd}

	req := &protocol.RunnerOpenFileTransferRequest{
		TaskId:    taskID,
		StreamId:  1,
		Direction: protocol.FileTransferDirection_DirPush,
	}
	req.SetRelPath([]byte("incoming"))

	go sess.handleOpenFileTransfer(context.Background(), req)
	pushTar(t, clientEnd, map[string][]byte{
		"a.txt":     []byte("AA"),
		"sub/":      nil,
		"sub/b.txt": []byte("BBB"),
	})

	ack := readAck(t, clientEnd)
	if ack.Status != protocol.FileTransferStatus_Ok {
		t.Fatalf("ack status = %v want ok", ack.Status)
	}
	if got, _ := os.ReadFile(filepath.Join(tmp, "incoming", "a.txt")); string(got) != "AA" {
		t.Errorf("a.txt = %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(tmp, "incoming", "sub", "b.txt")); string(got) != "BBB" {
		t.Errorf("sub/b.txt = %q", got)
	}
	// Staging parent should still exist (we don't tear it down between
	// transfers), but no random subdirectories should remain.
	entries, _ := os.ReadDir(filepath.Join(tmp, ".harness-staging"))
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("staging not cleaned: %v", names)
	}
}

func TestHandleOpenFileTransfer_DirPushRejectExisting(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "incoming"), 0o755); err != nil {
		t.Fatal(err)
	}
	taskIDHex := "00000000000000000000000000000041"
	taskID := mustParseTaskID(t, taskIDHex)

	sess := &Session{NoWorktree: true}
	sess.initMaps()
	sess.tasks[taskIDHex] = &taskEntry{repoPath: tmp}

	clientEnd, runnerEnd := newMemoryBidiPair()
	sess.Streams = staticStreamLookup{1: runnerEnd}

	req := &protocol.RunnerOpenFileTransferRequest{
		TaskId:    taskID,
		StreamId:  1,
		Direction: protocol.FileTransferDirection_DirPush,
	}
	req.SetRelPath([]byte("incoming"))

	go sess.handleOpenFileTransfer(context.Background(), req)
	// Client closes its send side without sending tar bytes — runner
	// rejected at the existence check before consuming the stream.
	_ = clientEnd.AppendData(true)

	ack := readAck(t, clientEnd)
	if ack.Status != protocol.FileTransferStatus_AlreadyExists {
		t.Fatalf("ack status = %v want already_exists", ack.Status)
	}
}

func TestHandleOpenFileTransfer_DirPushForceOverwrites(t *testing.T) {
	tmp := t.TempDir()
	old := filepath.Join(tmp, "incoming")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "stale.txt"), []byte("OLD"), 0o644); err != nil {
		t.Fatal(err)
	}
	taskIDHex := "00000000000000000000000000000042"
	taskID := mustParseTaskID(t, taskIDHex)

	sess := &Session{NoWorktree: true}
	sess.initMaps()
	sess.tasks[taskIDHex] = &taskEntry{repoPath: tmp}

	clientEnd, runnerEnd := newMemoryBidiPair()
	sess.Streams = staticStreamLookup{1: runnerEnd}

	req := &protocol.RunnerOpenFileTransferRequest{
		TaskId:    taskID,
		StreamId:  1,
		Direction: protocol.FileTransferDirection_DirPush,
	}
	req.SetRelPath([]byte("incoming"))
	req.SetForce(true)

	go sess.handleOpenFileTransfer(context.Background(), req)
	pushTar(t, clientEnd, map[string][]byte{"fresh.txt": []byte("NEW")})

	ack := readAck(t, clientEnd)
	if ack.Status != protocol.FileTransferStatus_Ok {
		t.Fatalf("ack status = %v want ok", ack.Status)
	}
	if _, err := os.Stat(filepath.Join(old, "stale.txt")); !os.IsNotExist(err) {
		t.Errorf("stale.txt should have been replaced; err=%v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(old, "fresh.txt")); string(got) != "NEW" {
		t.Errorf("fresh.txt = %q", got)
	}
}

func TestHandleOpenFileTransfer_DirPushRejectSymlinkEntry(t *testing.T) {
	tmp := t.TempDir()
	taskIDHex := "00000000000000000000000000000043"
	taskID := mustParseTaskID(t, taskIDHex)

	sess := &Session{NoWorktree: true}
	sess.initMaps()
	sess.tasks[taskIDHex] = &taskEntry{repoPath: tmp}

	clientEnd, runnerEnd := newMemoryBidiPair()
	sess.Streams = staticStreamLookup{1: runnerEnd}

	req := &protocol.RunnerOpenFileTransferRequest{
		TaskId:    taskID,
		StreamId:  1,
		Direction: protocol.FileTransferDirection_DirPush,
	}
	req.SetRelPath([]byte("incoming"))

	go sess.handleOpenFileTransfer(context.Background(), req)
	pushSymlinkTar(t, clientEnd, "evil", "/etc/passwd")

	ack := readAck(t, clientEnd)
	if ack.Status != protocol.FileTransferStatus_PathInvalid {
		t.Fatalf("ack status = %v want path_invalid", ack.Status)
	}
	if _, err := os.Stat(filepath.Join(tmp, "incoming")); !os.IsNotExist(err) {
		t.Errorf("dest must not exist after rejected push; err=%v", err)
	}
}

func TestHandleOpenFileTransfer_DirPushRejectPathTraversal(t *testing.T) {
	tmp := t.TempDir()
	taskIDHex := "00000000000000000000000000000044"
	taskID := mustParseTaskID(t, taskIDHex)

	sess := &Session{NoWorktree: true}
	sess.initMaps()
	sess.tasks[taskIDHex] = &taskEntry{repoPath: tmp}

	clientEnd, runnerEnd := newMemoryBidiPair()
	sess.Streams = staticStreamLookup{1: runnerEnd}

	req := &protocol.RunnerOpenFileTransferRequest{
		TaskId:    taskID,
		StreamId:  1,
		Direction: protocol.FileTransferDirection_DirPush,
	}
	req.SetRelPath([]byte("incoming"))

	go sess.handleOpenFileTransfer(context.Background(), req)
	pushTar(t, clientEnd, map[string][]byte{"../escape": []byte("X")})

	ack := readAck(t, clientEnd)
	if ack.Status != protocol.FileTransferStatus_PathInvalid {
		t.Fatalf("ack status = %v want path_invalid", ack.Status)
	}
}
```

Add to imports at top of test file:

```go
import (
	"archive/tar"
	"bytes"
	// ... existing imports
	"strings"
)
```

- [ ] **Step 4: Run tests**

Run: `go test ./runner -run "DirPush" -v -count=1`
Expected: all 5 PASS.

- [ ] **Step 5: Commit**

```bash
git add runner/file_transfer.go runner/file_transfer_test.go
git commit -m "$(cat <<'EOF'
runner: add runDirPush — staged tar extract with --force

Receives a tar stream from the OpenFileTransfer bidi, extracts each
entry into <worktree>/.harness-staging/<random>/, then atomically
renames into the requested dest. Refuses to clobber an existing dest
unless force is set; refuses to replace a regular file with a directory
regardless of force; rejects symlink/hardlink/device/fifo entries and
any tar header naming a path that escapes the staging root.

Tests cover happy path, dest-exists rejection, force overwrite,
symlink-in-tar rejection, and ../escape rejection.

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: Runner-side dir pull (runDirPull) + tests

**Files:**
- Modify: `runner/file_transfer.go` (add `runDirPull`, dispatch arm)
- Modify: `runner/file_transfer_test.go` (add 2 unit tests)

- [ ] **Step 1: Implement runDirPull**

Append to `runner/file_transfer.go`:

```go
// runDirPull walks the directory at full and writes a tar stream of its
// contents to the client. Symlinks, hard links, and special files are
// silently skipped (only regular files and directories are emitted).
func (s *Session) runDirPull(stream trsf.BidirectionalStream, full string) {
	fi, err := os.Stat(full)
	if err != nil {
		switch {
		case os.IsNotExist(err):
			_ = writeAck(stream, protocol.FileTransferStatus_NotFound, 0)
		default:
			_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
		}
		return
	}
	if !fi.IsDir() {
		_ = writeAck(stream, protocol.FileTransferStatus_NotADirectory, 0)
		return
	}
	if err := writeAck(stream, protocol.FileTransferStatus_Ok, 0); err != nil {
		return
	}

	tw := tar.NewWriter(streamWriter{s: stream})
	walkErr := filepath.WalkDir(full, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		// Skip the root itself: tar stream entries are relative to it,
		// and an empty Name is meaningless.
		if path == full {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		// Skip symlinks, hard links, devices, FIFOs.
		if info.Mode()&os.ModeType != 0 && !info.IsDir() {
			return nil
		}
		hdr, herr := tar.FileInfoHeader(info, "")
		if herr != nil {
			return herr
		}
		rel, rerr := filepath.Rel(full, path)
		if rerr != nil {
			return rerr
		}
		hdr.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			hdr.Name += "/"
			hdr.Typeflag = tar.TypeDir
			hdr.Size = 0
			return tw.WriteHeader(hdr)
		}
		hdr.Typeflag = tar.TypeReg
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, f)
		_ = f.Close()
		return err
	})
	if walkErr != nil {
		// Mid-stream error after ack: best we can do is close prematurely.
		// Client surfaces this as io.ErrUnexpectedEOF on tar.Reader.Next.
		return
	}
	_ = tw.Close()
	_ = stream.AppendData(true)
}
```

Add `"io/fs"` to the import block at the top of `runner/file_transfer.go` if not already present.

- [ ] **Step 2: Add dir_pull dispatch arm**

In `handleOpenFileTransfer`, extend the switch:

```go
	switch req.Direction {
	case protocol.FileTransferDirection_Pull:
		s.runPull(stream, full)
	case protocol.FileTransferDirection_Push:
		s.runPush(stream, full, req.Force())
	case protocol.FileTransferDirection_Delete:
		s.runDelete(stream, full)
	case protocol.FileTransferDirection_DirPush:
		s.runDirPush(stream, worktreeDir, full, req.Force())
	case protocol.FileTransferDirection_DirPull:
		s.runDirPull(stream, full)
	default:
		_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
	}
```

- [ ] **Step 3: Unit tests**

Append to `runner/file_transfer_test.go`:

```go
func TestHandleOpenFileTransfer_DirPullOK(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "outgoing")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("AA"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "b.txt"), []byte("BBB"), 0o644); err != nil {
		t.Fatal(err)
	}
	taskIDHex := "00000000000000000000000000000050"
	taskID := mustParseTaskID(t, taskIDHex)

	sess := &Session{NoWorktree: true}
	sess.initMaps()
	sess.tasks[taskIDHex] = &taskEntry{repoPath: tmp}

	clientEnd, runnerEnd := newMemoryBidiPair()
	sess.Streams = staticStreamLookup{1: runnerEnd}

	req := &protocol.RunnerOpenFileTransferRequest{
		TaskId:    taskID,
		StreamId:  1,
		Direction: protocol.FileTransferDirection_DirPull,
	}
	req.SetRelPath([]byte("outgoing"))

	go sess.handleOpenFileTransfer(context.Background(), req)

	ack := readAck(t, clientEnd)
	if ack.Status != protocol.FileTransferStatus_Ok {
		t.Fatalf("ack status = %v want ok", ack.Status)
	}
	body, err := io.ReadAll(streamReader(clientEnd))
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	tr := tar.NewReader(bytes.NewReader(body))
	files := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			b, _ := io.ReadAll(tr)
			files[hdr.Name] = b
		}
	}
	if string(files["a.txt"]) != "AA" {
		t.Errorf("a.txt = %q want AA", files["a.txt"])
	}
	if string(files["sub/b.txt"]) != "BBB" {
		t.Errorf("sub/b.txt = %q want BBB", files["sub/b.txt"])
	}
}

func TestHandleOpenFileTransfer_DirPullNotADirectory(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "afile"), []byte("X"), 0o644); err != nil {
		t.Fatal(err)
	}
	taskIDHex := "00000000000000000000000000000051"
	taskID := mustParseTaskID(t, taskIDHex)

	sess := &Session{NoWorktree: true}
	sess.initMaps()
	sess.tasks[taskIDHex] = &taskEntry{repoPath: tmp}

	clientEnd, runnerEnd := newMemoryBidiPair()
	sess.Streams = staticStreamLookup{1: runnerEnd}

	req := &protocol.RunnerOpenFileTransferRequest{
		TaskId:    taskID,
		StreamId:  1,
		Direction: protocol.FileTransferDirection_DirPull,
	}
	req.SetRelPath([]byte("afile"))

	go sess.handleOpenFileTransfer(context.Background(), req)
	ack := readAck(t, clientEnd)
	if ack.Status != protocol.FileTransferStatus_NotADirectory {
		t.Fatalf("ack status = %v want not_a_directory", ack.Status)
	}
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./runner -run "DirPull" -v -count=1`
Expected: both PASS.

- [ ] **Step 5: Commit**

```bash
git add runner/file_transfer.go runner/file_transfer_test.go
git commit -m "$(cat <<'EOF'
runner: add runDirPull — walk worktree subdir, emit tar to client

Stat the source; ack NotFound or NotADirectory before opening the tar
stream so the client can distinguish those errors from a mid-transfer
EOF. On ok, walk the directory with filepath.WalkDir, emitting a tar
header (and body for regular files) per entry. Symlinks, hard links,
and special files are silently skipped.

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: Client API for dir push/pull + CLI wiring

**Files:**
- Create: nothing (extending existing files)
- Modify: `cli/file_push.go` (add `FilePushDir`)
- Modify: `cli/file_pull.go` (add `FilePullDir`)
- Modify: `cli/file_push.go` (extend `ackError` for `not_a_directory`)
- Modify: `cmd/harness-cli/main.go` (add `--recursive` / `-r` and `--force` / `-f` flags to push/pull subcommands; dispatch to dir variants; update help)

- [ ] **Step 1: Implement FilePushDir**

Append to `cli/file_push.go`:

```go
import (
	"archive/tar"
	"io/fs"
	// existing imports ...
)

// FilePushDir packs localDir into a tar stream and pushes it to the runner,
// which extracts it under worktreeRel using staging-dir + atomic rename.
// Refuses to overwrite an existing remote dest unless force is set.
func (c *Client) FilePushDir(ctx context.Context, taskIDHex, localDir, remoteRel string, force bool) error {
	info, err := os.Stat(localDir)
	if err != nil {
		return fmt.Errorf("file push --recursive: stat local: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("file push --recursive: %s is not a directory", localDir)
	}
	stream, err := c.OpenFileTransfer(ctx, taskIDHex, protocol.FileTransferDirection_DirPush, remoteRel, 0, force)
	if err != nil {
		return err
	}
	defer stream.CloseBoth()

	tw := tar.NewWriter(streamWriter{s: stream})
	walkErr := filepath.WalkDir(localDir, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if path == localDir {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		// Skip symlinks, hard links, devices, FIFOs on the client side
		// too — runner would reject them anyway.
		if info.Mode()&os.ModeType != 0 && !info.IsDir() {
			return nil
		}
		hdr, herr := tar.FileInfoHeader(info, "")
		if herr != nil {
			return herr
		}
		rel, rerr := filepath.Rel(localDir, path)
		if rerr != nil {
			return rerr
		}
		hdr.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			hdr.Name += "/"
			hdr.Typeflag = tar.TypeDir
			hdr.Size = 0
			return tw.WriteHeader(hdr)
		}
		hdr.Typeflag = tar.TypeReg
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, f)
		_ = f.Close()
		return err
	})
	if walkErr != nil {
		return fmt.Errorf("file push --recursive: walk %s: %w", localDir, walkErr)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("file push --recursive: tar close: %w", err)
	}
	if err := stream.AppendData(true); err != nil {
		return fmt.Errorf("file push --recursive: stream EOF: %w", err)
	}

	ack, err := ReadFileTransferAck(stream)
	if err != nil {
		return fmt.Errorf("file push --recursive: read ack: %w", err)
	}
	return ackError("push --recursive", ack)
}
```

Extend `ackError` for the new status:

```go
func ackError(op string, ack *protocol.FileTransferAck) error {
	switch ack.Status {
	case protocol.FileTransferStatus_Ok:
		return nil
	case protocol.FileTransferStatus_PathInvalid:
		return fmt.Errorf("file %s: path invalid (escapes worktree or empty)", op)
	case protocol.FileTransferStatus_NotFound:
		return fmt.Errorf("file %s: not found", op)
	case protocol.FileTransferStatus_AlreadyExists:
		return fmt.Errorf("file %s: destination already exists (use --force to overwrite)", op)
	case protocol.FileTransferStatus_IoError:
		return fmt.Errorf("file %s: runner I/O error", op)
	case protocol.FileTransferStatus_Canceled:
		return fmt.Errorf("file %s: canceled", op)
	case protocol.FileTransferStatus_IsDirectory:
		return fmt.Errorf("file %s: is a directory", op)
	case protocol.FileTransferStatus_NotADirectory:
		return fmt.Errorf("file %s: not a directory", op)
	default:
		return fmt.Errorf("file %s: unknown status %d", op, ack.Status)
	}
}
```

- [ ] **Step 2: Implement FilePullDir**

Append to `cli/file_pull.go`:

```go
import (
	"archive/tar"
	"crypto/rand"
	"encoding/hex"
	// existing imports ...
)

// FilePullDir pulls the worktree directory at remoteRel into localDir. Stages
// the extracted tree at <localDir>.staging-<random>/ and renames atomically
// on success. Refuses to overwrite an existing local dest unless force is set.
func (c *Client) FilePullDir(ctx context.Context, taskIDHex, remoteRel, localDir string, force bool) error {
	if fi, err := os.Lstat(localDir); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("file pull --recursive: %s exists and is not a directory", localDir)
		}
		if !force {
			return fmt.Errorf("file pull --recursive: %s already exists (use --force to overwrite)", localDir)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("file pull --recursive: stat local: %w", err)
	}

	stream, err := c.OpenFileTransfer(ctx, taskIDHex, protocol.FileTransferDirection_DirPull, remoteRel, 0, false)
	if err != nil {
		return err
	}
	defer stream.CloseBoth()
	if err := stream.AppendData(true); err != nil {
		return fmt.Errorf("file pull --recursive: half-close: %w", err)
	}
	ack, err := ReadFileTransferAck(stream)
	if err != nil {
		return fmt.Errorf("file pull --recursive: read ack: %w", err)
	}
	if err := ackError("pull --recursive", ack); err != nil {
		return err
	}

	staging, err := mkLocalStaging(localDir)
	if err != nil {
		return fmt.Errorf("file pull --recursive: create staging: %w", err)
	}
	cleanupStaging := true
	defer func() {
		if cleanupStaging {
			_ = os.RemoveAll(staging)
		}
	}()

	tr := tar.NewReader(streamReadAll{stream})
	for {
		hdr, terr := tr.Next()
		if terr == io.EOF {
			break
		}
		if terr != nil {
			return fmt.Errorf("file pull --recursive: tar read: %w", terr)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeDir {
			return fmt.Errorf("file pull --recursive: unexpected entry type %d in %s", hdr.Typeflag, hdr.Name)
		}
		full, perr := validateRelPathLocal(staging, hdr.Name)
		if perr != nil {
			return fmt.Errorf("file pull --recursive: invalid entry %s: %w", hdr.Name, perr)
		}
		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(full, 0o755); err != nil {
				return fmt.Errorf("file pull --recursive: mkdir %s: %w", full, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("file pull --recursive: mkdir parent of %s: %w", full, err)
		}
		mode := os.FileMode(hdr.Mode & 0o777)
		if mode == 0 {
			mode = 0o644
		}
		f, oerr := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if oerr != nil {
			return fmt.Errorf("file pull --recursive: create %s: %w", full, oerr)
		}
		if _, err := io.Copy(f, tr); err != nil {
			_ = f.Close()
			return fmt.Errorf("file pull --recursive: write %s: %w", full, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("file pull --recursive: close %s: %w", full, err)
		}
	}

	if force {
		if err := os.RemoveAll(localDir); err != nil {
			return fmt.Errorf("file pull --recursive: replace existing dest: %w", err)
		}
	}
	if err := os.Rename(staging, localDir); err != nil {
		return fmt.Errorf("file pull --recursive: rename staging: %w", err)
	}
	cleanupStaging = false
	return nil
}

// mkLocalStaging creates <localDir>.staging-<random>/ as a sibling of
// localDir and returns its path. Sibling placement guarantees the rename
// stays on the same filesystem.
func mkLocalStaging(localDir string) (string, error) {
	parent := filepath.Dir(localDir)
	if parent == "" {
		parent = "."
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	dir := filepath.Join(parent, filepath.Base(localDir)+".staging-"+hex.EncodeToString(id[:]))
	if err := os.Mkdir(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// validateRelPathLocal is the client-side mirror of runner.ValidateRelPath:
// rejects absolute paths, NUL bytes, and entries whose cleaned form escapes
// staging via "..". Kept private to the cli package.
func validateRelPathLocal(stagingRoot, rel string) (string, error) {
	if strings.ContainsRune(rel, 0) {
		return "", fmt.Errorf("rel path contains NUL")
	}
	if rel == "" {
		return "", fmt.Errorf("rel path empty")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("rel path is absolute")
	}
	cleaned := filepath.Clean(rel)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("rel path escapes root")
	}
	full := filepath.Join(stagingRoot, cleaned)
	rootClean := filepath.Clean(stagingRoot)
	if full != rootClean && !strings.HasPrefix(full, rootClean+string(filepath.Separator)) {
		return "", fmt.Errorf("rel path escapes root")
	}
	return full, nil
}
```

Add `"strings"` to the import block of `cli/file_pull.go`.

- [ ] **Step 3: Wire CLI flags --recursive / --force**

Open `cmd/harness-cli/main.go`. Find the `case "file":` block (around line 203). Replace the `case "push":` and `case "pull":` arms with flag-aware versions:

```go
		case "push":
			fs := flag.NewFlagSet("file push", flag.ExitOnError)
			recursive := fs.Bool("recursive", false, "transfer a directory tree")
			fs.BoolVar(recursive, "r", false, "alias for --recursive")
			force := fs.Bool("force", false, "overwrite existing destination")
			fs.BoolVar(force, "f", false, "alias for --force")
			fs.Parse(rest)
			args := fs.Args()
			if len(args) != 3 {
				fmt.Fprintln(os.Stderr, "usage: harness-cli file push [-r] [-f] <task-id> <local-src> <worktree-rel-dst>")
				os.Exit(2)
			}
			if *recursive {
				if err := c.FilePushDir(ctx, args[0], args[1], args[2], *force); err != nil {
					die(err)
				}
			} else {
				if err := c.FilePush(ctx, args[0], args[1], args[2], *force); err != nil {
					die(err)
				}
			}
		case "pull":
			fs := flag.NewFlagSet("file pull", flag.ExitOnError)
			recursive := fs.Bool("recursive", false, "transfer a directory tree")
			fs.BoolVar(recursive, "r", false, "alias for --recursive")
			force := fs.Bool("force", false, "overwrite existing destination")
			fs.BoolVar(force, "f", false, "alias for --force")
			fs.Parse(rest)
			args := fs.Args()
			if len(args) != 3 {
				fmt.Fprintln(os.Stderr, "usage: harness-cli file pull [-r] [-f] <task-id> <worktree-rel-src> <local-dst>")
				os.Exit(2)
			}
			if *recursive {
				if err := c.FilePullDir(ctx, args[0], args[1], args[2], *force); err != nil {
					die(err)
				}
			} else {
				if err := c.FilePull(ctx, args[0], args[1], args[2], *force); err != nil {
					die(err)
				}
			}
```

- [ ] **Step 4: Update help text**

Find the usage block and replace the file push/pull lines:

```go
	fmt.Fprintln(os.Stderr, "  file push [-r|--recursive] [-f|--force] TASK_ID LOCAL_SRC WORKTREE_REL_DST")
	fmt.Fprintln(os.Stderr, "                                      copy a local file (or directory tree with -r) into the worktree")
	fmt.Fprintln(os.Stderr, "                                      default: O_EXCL refuses to overwrite; -f permits replacement")
	fmt.Fprintln(os.Stderr, "  file pull [-r|--recursive] [-f|--force] TASK_ID WORKTREE_REL_SRC LOCAL_DST")
	fmt.Fprintln(os.Stderr, "                                      copy a worktree file (or directory tree with -r) to a local path")
	fmt.Fprintln(os.Stderr, "                                      default: O_EXCL refuses to overwrite local; -f permits replacement")
	fmt.Fprintln(os.Stderr, "  file ls   TASK_ID [WORKTREE_REL_DIR]")
	fmt.Fprintln(os.Stderr, "                                      list a single directory under the worktree (default: worktree root)")
	fmt.Fprintln(os.Stderr, "  file delete TASK_ID WORKTREE_REL_PATH")
	fmt.Fprintln(os.Stderr, "                                      remove a file from the task's worktree (refuses directories)")
```

- [ ] **Step 5: Build + smoke-test help output**

Run: `go build -o bin/harness-cli ./cmd/harness-cli && bin/harness-cli 2>&1 | grep "file push\|file pull" -A1`
Expected: shows the new `[-r] [-f]` syntax.

- [ ] **Step 6: Run all unit tests as a regression check**

Run: `go build ./... && go test ./runner ./cli ./server -count=1 -timeout 60s`
Expected: all PASS.

- [ ] **Step 7: Commit**

```bash
git add cli/file_push.go cli/file_pull.go cmd/harness-cli/main.go
git commit -m "$(cat <<'EOF'
cli: add FilePushDir / FilePullDir + --recursive / --force flags

FilePushDir walks the local source dir and writes a tar stream to the
runner over the existing OpenFileTransfer wiring. FilePullDir reads
the runner-emitted tar stream and stages extraction to a sibling
.staging-<random>/ before renaming into place. Both honor a --force
flag for explicit overwrite.

CLI side: 'file push' and 'file pull' now accept -r/--recursive and
-f/--force. Without -r the existing single-file behavior applies (with
-f permitting overwrite via O_TRUNC for push and O_TRUNC for pull's
local file). Help text updated.

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: End-to-end integration test

**Files:**
- Modify: `integration/file_transfer_e2e_test.go` (append `TestFileDirTransferE2E`)

- [ ] **Step 1: Append the e2e test**

Open `integration/file_transfer_e2e_test.go`. The existing `TestFileTransferE2E` shows the bring-up pattern — copy that scaffolding for the new test. Append:

```go
func TestFileDirTransferE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}

	repo := initRepo(t)
	fakeClaude, err := filepath.Abs("../testdata/fake-claude-slow.sh")
	if err != nil {
		t.Fatal(err)
	}

	addr := "127.0.0.1:18546"
	peerCID, err := objproto.ParseConnectionID("ws:"+addr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse server cid: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	s := server.New(server.Config{Addr: addr, DataDir: t.TempDir()})
	serverDone := make(chan error, 1)
	go func() { serverDone <- s.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)

	runnerDone := make(chan error, 1)
	go func() {
		runnerDone <- runner.Run(ctx, runner.Config{
			ServerCID:    peerCID,
			AllowedRoots: []string{repo},
			ClaudeBin:    fakeClaude,
		})
	}()
	time.Sleep(500 * time.Millisecond)

	taskID, err := cli.Submit(ctx, peerCID, repo, "long-running")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	worktree := filepath.Join(repo, ".harness-worktrees", taskID)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(worktree); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("worktree did not appear: %v", err)
	}

	c, err := cli.Dial(ctx, peerCID)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	// 1. Build a local source directory tree.
	localSrc := filepath.Join(t.TempDir(), "src-tree")
	if err := os.MkdirAll(filepath.Join(localSrc, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localSrc, "a.txt"), []byte("AA"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localSrc, "sub", "b.txt"), []byte("BBB"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 2. Push the directory.
	if err := c.FilePushDir(ctx, taskID, localSrc, "incoming", false); err != nil {
		t.Fatalf("dir push: %v", err)
	}

	// 3. Verify it landed.
	if got, err := os.ReadFile(filepath.Join(worktree, "incoming", "a.txt")); err != nil || string(got) != "AA" {
		t.Errorf("incoming/a.txt = %q err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(worktree, "incoming", "sub", "b.txt")); err != nil || string(got) != "BBB" {
		t.Errorf("incoming/sub/b.txt = %q err=%v", got, err)
	}

	// 4. Push again without --force: must fail.
	if err := c.FilePushDir(ctx, taskID, localSrc, "incoming", false); err == nil {
		t.Errorf("second push without --force should fail")
	}

	// 5. Push with --force: must replace.
	localSrc2 := filepath.Join(t.TempDir(), "src-tree-2")
	if err := os.MkdirAll(localSrc2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localSrc2, "fresh.txt"), []byte("NEW"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.FilePushDir(ctx, taskID, localSrc2, "incoming", true); err != nil {
		t.Fatalf("force push: %v", err)
	}
	if _, err := os.Stat(filepath.Join(worktree, "incoming", "a.txt")); !os.IsNotExist(err) {
		t.Errorf("old a.txt should be gone after force replace; err=%v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(worktree, "incoming", "fresh.txt")); string(got) != "NEW" {
		t.Errorf("fresh.txt = %q", got)
	}

	// 6. Pull the directory back.
	localDst := filepath.Join(t.TempDir(), "pulled-tree")
	if err := c.FilePullDir(ctx, taskID, "incoming", localDst, false); err != nil {
		t.Fatalf("dir pull: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(localDst, "fresh.txt")); string(got) != "NEW" {
		t.Errorf("pulled fresh.txt = %q", got)
	}

	// 7. Pull again without --force: must fail (dest exists).
	if err := c.FilePullDir(ctx, taskID, "incoming", localDst, false); err == nil {
		t.Errorf("second pull without --force should fail")
	}

	cancel()
	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
	}
	select {
	case <-runnerDone:
	case <-time.After(2 * time.Second):
	}
}
```

- [ ] **Step 2: Run the e2e test**

Run: `go test ./integration -run TestFileDirTransferE2E -v -count=1 -timeout 90s`
Expected: PASS.

Run it 3 times to confirm no flakiness.

- [ ] **Step 3: Run the full suite as final regression check**

Run: `go test ./... -count=1 -timeout 300s`
Expected: all PASS, modulo any pre-existing flake (`TestSubmitWakeE2E` has been flaky historically).

- [ ] **Step 4: Commit**

```bash
git add integration/file_transfer_e2e_test.go
git commit -m "$(cat <<'EOF'
integration: e2e test for file dir push/pull (incl. --force)

Pushes a local 2-file/1-dir tree into a running task's worktree, then:
- verifies extraction
- verifies second push without --force is rejected
- verifies --force push replaces the dest
- pulls the tree back to a new local path
- verifies second pull without --force is rejected

Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>
EOF
)"
```

---

## Self-Review

**Spec coverage:** every section of `2026-05-10-file-dir-transfer-design.md` is implemented:

- Schema (Direction, Status, Force) → Task 1
- Single-file push --force (O_TRUNC) → Task 2 step 4
- Single-file pull --force (O_EXCL default + --force) → Task 2 step 2
- runDirPush + staging + symlink/traversal rejection → Task 3
- runDirPull + walk + symlink skip → Task 4
- FilePushDir / FilePullDir → Task 5
- CLI --recursive / --force flags → Task 5 step 3
- E2E coverage → Task 6

**Placeholder scan:** no TBD, no "implement later", no "similar to Task N" — every step has actual code.

**Type consistency:**
- `OpenFileTransfer(force bool)` — used identically in FilePush, FilePull, FilePushDir, FilePullDir, FileDelete (Task 2 + Task 5).
- `runPush(stream, full, force bool)`, `runDirPush(stream, worktreeDir, dest, force bool)`, `runDirPull(stream, full)` — signatures match the dispatch arms.
- `FileTransferStatus_NotADirectory` consistent in spec, schema (Task 1), runner (Task 4), cli ackError (Task 5).

## Out-of-scope (deferred)

- `file ls --recursive` (separate UX concern).
- `file delete --recursive` (rmdir; needs its own safety story).
- Hard-link / symlink preservation in tar.
- Wire compression.
- Resume across disconnect.
- TUI integration.
