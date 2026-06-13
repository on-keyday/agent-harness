# Interactive view-only (read-only) attach — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.
>
> **This repo's traps (read first):** `.claude/skills/implementation-pitfalls/SKILL.md`. In particular:
> - **Worktree path routing.** This task runs in a harness worktree. Commits must land on the worktree's own `harness/<id>` branch. Run all `git`/build/test commands with the working directory set to the worktree root (the directory this plan lives under), and use worktree-relative paths. Do NOT use absolute `/home/.../remote-agent-harness/...` paths — they route to the PARENT checkout (Pitfall 8).
> - **Build hygiene.** Compile-check with `go build ./...` or `go vet ./pkg/...`; never bare `go build ./cmd/x/` (drops a binary into the tree). `go test` cleans up after itself.
> - **Sibling-code consistency.** Each client (CLI/TUI/WebUI) must thread the new `mode` argument the same way adjacent code threads its args.

**Goal:** Add a read-only "view" attach mode so multiple clients can passively watch a live interactive session without taking it over.

**Architecture:** Server-side fan-out in `SessionMux`: the existing single writer (control) slot is unchanged; viewers are an additive set, each with a bounded queue (drop-on-overflow) plus a read-and-discard input drain, so a viewer can never wedge or backpressure the real session. An `AttachMode` field on `AttachSessionRequest` selects the mode; only the reattach handler branches. Viewers stay orthogonal to the Running/Detached state machine. The runner protocol is untouched. Surfaced on WebUI/CLI/TUI.

**Tech Stack:** Go (server, CLI native, TUI), Go/WASM + JS (WebUI), brgen `.bgn` schema codegen.

**Spec:** `docs/superpowers/specs/2026-06-14-interactive-viewonly-attach-design.md`

---

## File structure

| File | Change |
|------|--------|
| `runner/protocol/message.bgn` | Add `AttachMode` enum + `mode` field on `AttachSessionRequest` |
| `runner/protocol/message.go` | **Generated** — regenerate via `make protoregen`, do not hand-edit |
| `server/session_mux.go` | Viewer state + `AttachViewer` + fan-out + drop + input drain + `Stop`/`ViewerCount` |
| `server/session_mux_test.go` | Fake block-writes gate + `Written()` helper + viewer tests |
| `server/task_handler.go` | `handleAttachSession`: branch on `req.Mode` |
| `server/task_handler_test.go` | View-mode routing test |
| `cli/attach.go` | `attachSessionRPC` gains a `mode` param |
| `cli/attach_native.go` | `AttachSession`/`SessionAttach` gain `mode` |
| `cli/attach_js.go` | `AttachSession` (WASM) gains `mode` |
| `cmd/harness-cli/session.go` | `session attach --view` flag |
| `tui/interactive.go` | `DoAttachSession` gains `mode` |
| `tui/app.go` | `v` key → view-attach the selected session |
| `cmd/harness-webui-wasm/main.go` | `harnessAttachSession` reads optional `mode` arg |
| `webui/static/main.js` | "👁 View" button beside Reattach |

---

## Task 1: Schema — `AttachMode` enum + `mode` field (single source of truth)

Per `feedback_no_split_schemas`, the entire wire change lives in this one task.

**Files:**
- Modify: `runner/protocol/message.bgn` (around `:539`, the `AttachSessionRequest` / `AttachMode` area)
- Regenerate: `runner/protocol/message.go`

- [ ] **Step 1: Add the `AttachMode` enum and the `mode` field**

In `runner/protocol/message.bgn`, replace the `AttachSessionRequest` block:

```
format AttachSessionRequest:
    task_id :TaskID
```

with (insert the enum immediately before it, add the field):

```
enum AttachMode:
    :u8
    control = "control"   # default (ordinal 0): takeover-writer behavior
    view    = "view"      # read-only observer; no input, no takeover

format AttachSessionRequest:
    task_id :TaskID
    mode :AttachMode
```

- [ ] **Step 2: Regenerate Go from the schema**

Run (from the worktree root): `make protoregen`
Expected: regenerates `runner/protocol/message.go`; first run downloads `~/.cache/brgen-kit` (~20 MB, one-time). Exit 0.

- [ ] **Step 3: Verify the generated symbols exist**

Run: `grep -nE 'AttachMode_Control|AttachMode_View|Mode .*AttachMode' runner/protocol/message.go`
Expected: matches showing `AttachMode_Control`, `AttachMode_View`, and a `Mode AttachMode` field on the request struct.

- [ ] **Step 4: Confirm the tree still builds**

Run: `go build ./...`
Expected: builds clean (no callers set `Mode` yet; it defaults to `AttachMode_Control` == 0, which is the existing behavior).

- [ ] **Step 5: Commit**

```bash
git add runner/protocol/message.bgn runner/protocol/message.go
git commit -m "feat(protocol): AttachMode (control|view) on AttachSessionRequest"
```

---

## Task 2: Test infra — block-writes gate + `Written()` on the fake stream

The viewer tests need a stream whose writes can be made to block (to prove a
slow viewer is dropped without wedging the pump) and a non-blocking getter for
captured writes.

**Files:**
- Modify: `server/session_mux_test.go` (the `fakeBidiStream` type, ~`:21-120`)

- [ ] **Step 1: Add the gate field and helpers**

Add to the `fakeBidiStream` struct (after the `closed atomic.Bool` field, ~`:33`):

```go
	blockWrites atomic.Bool // when true, Write spins until cleared or closed
```

Add these methods near `IsClosed` (~`:64`):

```go
// SetBlockWrites makes Write block (spin) until cleared or the stream is closed.
// Used to simulate a viewer whose client cannot keep up.
func (f *fakeBidiStream) SetBlockWrites(b bool) { f.blockWrites.Store(b) }

// Written returns a snapshot copy of all bytes written so far (non-blocking).
func (f *fakeBidiStream) Written() []byte {
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	return append([]byte{}, f.written...)
}
```

- [ ] **Step 2: Make `Write` honor the gate**

Replace the body of `Write` (~`:94-102`) with:

```go
func (f *fakeBidiStream) Write(p []byte) (int, error) {
	for f.blockWrites.Load() && !f.closed.Load() {
		time.Sleep(time.Millisecond)
	}
	if f.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	f.writeMu.Lock()
	f.written = append(f.written, p...)
	f.writeMu.Unlock()
	return len(p), nil
}
```

- [ ] **Step 3: Verify the test file still compiles**

Run: `go vet ./server/`
Expected: no errors (helpers unused yet is fine for methods; `go vet` does not flag unused methods).

- [ ] **Step 4: Commit**

```bash
git add server/session_mux_test.go
git commit -m "test(server): block-writes gate + Written() on fakeBidiStream"
```

---

## Task 3: `SessionMux` viewer support (core)

**Files:**
- Modify: `server/session_mux.go`
- Test: `server/session_mux_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `server/session_mux_test.go`:

```go
func TestSessionMux_AttachViewer_ReplaysThenStreams(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})

	pre := makeWireFrame(1, []byte("scrollback"))
	runner.QueueRead(pre)
	waitFor(t, func() bool { return mux.RingBufferLen() == len(pre) })

	viewer := newFakeStream(t)
	if err := mux.AttachViewer(ctx, viewer); err != nil {
		t.Fatalf("AttachViewer: %v", err)
	}
	if got := viewer.WaitWritten(t, len(pre)); !bytes.Equal(got, pre) {
		t.Fatalf("viewer replay got %q want %q", got, pre)
	}
	if mux.IsAttached() {
		t.Fatal("AttachViewer must NOT occupy the writer slot")
	}

	live := makeWireFrame(1, []byte("live"))
	runner.QueueRead(live)
	want := append(append([]byte{}, pre...), live...)
	if got := viewer.WaitWritten(t, len(want)); !bytes.Equal(got, want) {
		t.Fatalf("viewer live got %q want %q", got, want)
	}
}

func TestSessionMux_FanOutWriterAndViewers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})

	writer := newFakeStream(t)
	if err := mux.Attach(ctx, writer); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	v1 := newFakeStream(t)
	if err := mux.AttachViewer(ctx, v1); err != nil {
		t.Fatalf("v1: %v", err)
	}
	v2 := newFakeStream(t)
	if err := mux.AttachViewer(ctx, v2); err != nil {
		t.Fatalf("v2: %v", err)
	}

	fr := makeWireFrame(1, []byte("broadcast"))
	runner.QueueRead(fr)
	for name, s := range map[string]*fakeBidiStream{"writer": writer, "v1": v1, "v2": v2} {
		if got := s.WaitWritten(t, len(fr)); !bytes.Equal(got, fr) {
			t.Fatalf("%s got %q want %q", name, got, fr)
		}
	}
}

func TestSessionMux_SlowViewerDroppedWithoutWedge(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(1<<20), SessionHooks{})

	writer := newFakeStream(t)
	if err := mux.Attach(ctx, writer); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	slow := newFakeStream(t)
	slow.SetBlockWrites(true) // its output pump blocks on the first frame
	if err := mux.AttachViewer(ctx, slow); err != nil {
		t.Fatalf("AttachViewer: %v", err)
	}

	const n = viewerQueueDepth + 50
	var want []byte
	for i := 0; i < n; i++ {
		fr := makeWireFrame(1, []byte{byte(i)})
		want = append(want, fr...)
		runner.QueueRead(fr)
	}
	// Writer receives EVERY frame despite the stuck viewer (pump not wedged).
	if got := writer.WaitWritten(t, len(want)); !bytes.Equal(got, want) {
		t.Fatalf("writer missing frames — pump wedged on slow viewer?")
	}
	// The slow viewer is dropped and closed.
	waitFor(t, func() bool { return mux.ViewerCount() == 0 })
	if !slow.IsClosed() {
		t.Fatal("dropped viewer stream must be CloseBoth'd")
	}
}

func TestSessionMux_ViewerInputDiscarded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})

	viewer := newFakeStream(t)
	if err := mux.AttachViewer(ctx, viewer); err != nil {
		t.Fatalf("AttachViewer: %v", err)
	}
	viewer.QueueRead([]byte("rm -rf / # should never reach runner\n"))
	waitFor(t, func() bool { return !viewer.HasRecvData() }) // drain consumed it
	time.Sleep(50 * time.Millisecond)                        // settle
	if w := runner.Written(); len(w) != 0 {
		t.Fatalf("viewer input was forwarded to runner: %q", w)
	}
}

func TestSessionMux_ViewerDoesNotFireOnAttach(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var attaches int32
	hooks := SessionHooks{OnAttach: func(string) { atomic.AddInt32(&attaches, 1) }}
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), hooks)

	v := newFakeStream(t)
	if err := mux.AttachViewer(ctx, v); err != nil {
		t.Fatalf("AttachViewer: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := atomic.LoadInt32(&attaches); n != 0 {
		t.Fatalf("onAttach fired %d times for a viewer; want 0", n)
	}
	w := newFakeStream(t)
	if err := mux.Attach(ctx, w); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	waitFor(t, func() bool { return atomic.LoadInt32(&attaches) == 1 })
}

func TestSessionMux_StopClosesViewers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})

	v := newFakeStream(t)
	if err := mux.AttachViewer(ctx, v); err != nil {
		t.Fatalf("AttachViewer: %v", err)
	}
	mux.Stop()
	waitFor(t, func() bool { return v.IsClosed() })
	waitFor(t, func() bool { return mux.ViewerCount() == 0 })
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./server/ -run 'TestSessionMux_(AttachViewer_ReplaysThenStreams|FanOutWriterAndViewers|SlowViewerDroppedWithoutWedge|ViewerInputDiscarded|ViewerDoesNotFireOnAttach|StopClosesViewers)' -v`
Expected: FAIL to compile — `AttachViewer`, `ViewerCount`, `viewerQueueDepth` undefined.

- [ ] **Step 3: Add the viewer state**

In `server/session_mux.go`, add the constant and type near the top (after `frameHeaderSize`, ~`:18`):

```go
// viewerQueueDepth bounds per-viewer buffering. A viewer that cannot drain its
// queue this fast is dropped (it can never block the runner pump or the writer).
const viewerQueueDepth = 256

// viewerConn is one read-only observer of the session. Its output is delivered
// through a bounded channel by a dedicated pump; its input is read-and-discarded.
type viewerConn struct {
	stream trsf.BidirectionalStream
	ch     chan []byte
	cancel context.CancelFunc
}
```

Add a field to the `SessionMux` struct (after `tuiCancel context.CancelFunc`, ~`:80`):

```go
	viewers map[*viewerConn]struct{}
```

In `NewSessionMux`, initialize it in the struct literal (after `stopped: make(chan struct{}),`, ~`:102`):

```go
		viewers:  make(map[*viewerConn]struct{}),
```

- [ ] **Step 4: Add the fan-out to `runnerPump`**

In `runnerPump`, after the existing writer-forward block (the `if tui != nil { ... }` ending ~`:147`), add:

```go
		// Fan out to viewers (non-blocking). A viewer whose queue is full
		// cannot keep up and is dropped here — never blocking this pump.
		m.mu.Lock()
		for v := range m.viewers {
			select {
			case v.ch <- frameBytes:
			default:
				m.dropViewerLocked(v)
			}
		}
		m.mu.Unlock()
```

- [ ] **Step 5: Add `AttachViewer`, the pumps, drop, and `ViewerCount`**

Add these methods to `server/session_mux.go` (e.g. after `Attach`, ~`:202`):

```go
// AttachViewer adds a read-only observer. Unlike Attach it does NOT take over
// the writer slot, fire onAttach, or forward input to the runner. It replays
// the ring (and mode preamble) to the viewer, then streams live frames.
func (m *SessionMux) AttachViewer(ctx context.Context, stream trsf.BidirectionalStream) error {
	m.mu.Lock()
	if m.ctx.Err() != nil {
		m.mu.Unlock()
		return errors.New("session_mux: stopped")
	}
	vctx, vcancel := context.WithCancel(m.ctx)
	v := &viewerConn{stream: stream, ch: make(chan []byte, viewerQueueDepth), cancel: vcancel}
	m.viewers[v] = struct{}{}
	// Snapshot replay state under the SAME lock as the insert so runnerPump's
	// fan-out cannot interleave between "added" and "snapshotted".
	var replay []byte
	if pre := m.modes.preamble(); len(pre) > 0 {
		replay = append(replay, encodeStdoutFrame(pre)...)
	}
	replay = append(replay, m.ring.Snapshot()...)
	m.mu.Unlock()

	// Replay BEFORE starting the output pump, so replayed bytes always precede
	// live frames (live frames buffer in v.ch meanwhile).
	if len(replay) > 0 {
		if err := stream.AppendData(false, replay); err != nil {
			m.dropViewer(v)
			return err
		}
	}
	go m.viewerOutputPump(vctx, v)
	go m.viewerInputDrain(vctx, v)
	return nil
}

// viewerOutputPump drains v.ch to the viewer stream. Drops the viewer on write error.
func (m *SessionMux) viewerOutputPump(ctx context.Context, v *viewerConn) {
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-v.ch:
			if err := v.stream.AppendData(false, b); err != nil {
				m.dropViewer(v)
				return
			}
		}
	}
}

// viewerInputDrain reads and DISCARDS the viewer's incoming direction. This is
// the read-only enforcement point: unlike tuiPump it never forwards to the
// runner. Draining prevents the bidi recv side from backpressuring/wedging and
// gives prompt EOF when the client closes. ReadDirectContext (not ReadDirect)
// so cancel()/Stop() unblock the read immediately.
func (m *SessionMux) viewerInputDrain(ctx context.Context, v *viewerConn) {
	const maxRead = 32 * 1024
	for {
		_, eof, err := v.stream.ReadDirectContext(ctx, maxRead)
		if eof || err != nil {
			m.dropViewer(v)
			return
		}
	}
}

func (m *SessionMux) dropViewer(v *viewerConn) {
	m.mu.Lock()
	m.dropViewerLocked(v)
	m.mu.Unlock()
}

// dropViewerLocked removes and tears down a viewer. Idempotent: if v is no
// longer in the set, it is a no-op (both viewer goroutines may call it).
// Must be called with m.mu held.
func (m *SessionMux) dropViewerLocked(v *viewerConn) {
	if _, ok := m.viewers[v]; !ok {
		return
	}
	delete(m.viewers, v)
	v.cancel()
	_ = v.stream.CloseBoth()
}

// ViewerCount reports the number of attached viewers (test/inspection helper).
func (m *SessionMux) ViewerCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.viewers)
}
```

- [ ] **Step 6: Close viewers in `Stop`**

In `Stop` (~`:269`), inside the `stopOnce.Do` closure, extend the locked section that nils `m.tui`. Replace:

```go
		m.mu.Lock()
		tui := m.tui
		m.tui = nil
		if m.tuiCancel != nil {
			m.tuiCancel()
			m.tuiCancel = nil
		}
		m.mu.Unlock()
		if tui != nil {
			_ = tui.CloseBoth()
		}
```

with:

```go
		m.mu.Lock()
		tui := m.tui
		m.tui = nil
		if m.tuiCancel != nil {
			m.tuiCancel()
			m.tuiCancel = nil
		}
		vs := make([]*viewerConn, 0, len(m.viewers))
		for v := range m.viewers {
			vs = append(vs, v)
		}
		m.viewers = make(map[*viewerConn]struct{})
		m.mu.Unlock()
		if tui != nil {
			_ = tui.CloseBoth()
		}
		for _, v := range vs {
			v.cancel()
			_ = v.stream.CloseBoth()
		}
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test ./server/ -run 'TestSessionMux_' -v`
Expected: PASS (all `TestSessionMux_*`, including the six new ones and the three pre-existing ones).

- [ ] **Step 8: Race check**

Run: `go test ./server/ -run 'TestSessionMux_' -race`
Expected: PASS, no data races.

- [ ] **Step 9: Commit**

```bash
git add server/session_mux.go server/session_mux_test.go
git commit -m "feat(server): SessionMux read-only viewers with bounded-queue fan-out"
```

---

## Task 4: Route `mode=view` in `handleAttachSession`

**Files:**
- Modify: `server/task_handler.go` (`handleAttachSession`, ~`:741`)
- Test: `server/task_handler_test.go`

- [ ] **Step 1: Write the failing test**

Append to `server/task_handler_test.go` (mirrors `TestHandleAttachSession_Ok_FromDetached`, ~`:802`):

```go
// View mode must succeed without taking the writer slot and without flipping
// the task to Running (it must register a viewer instead).
func TestHandleAttachSession_ViewMode_NoWriterTakeover(t *testing.T) {
	h := newTestHandler(t)
	h.Sessions = NewSessionRegistry()

	id := makeDetachableTask(t, h.Tasks, protocol.TaskStatus_Running)
	if err := h.Tasks.SetDetached(id); err != nil {
		t.Fatalf("SetDetached: %v", err)
	}

	runnerStream := newFakeStream(t)
	ring := NewRingBuffer(4096)
	ring.Append([]byte("hello from runner"))
	mux := NewSessionMux(context.Background(), id, runnerStream, ring, SessionHooks{})
	h.Sessions.Add(id, mux)
	defer func() {
		runnerStream.CloseRead()
		mux.Stop()
	}()

	tuiConn := &fakeConn{
		id:           objproto.MustParseConnectionID("ws:127.0.0.1:9400-1"),
		nextStreamID: trsf.StreamID(33),
	}

	req := &protocol.AttachSessionRequest{TaskId: taskIDFromHexStr(t, id), Mode: protocol.AttachMode_View}
	resp := h.handleAttachSession(tuiConn, req)
	if resp.Status != protocol.AttachSessionStatus_Ok {
		t.Fatalf("status=%v want Ok", resp.Status)
	}
	if mux.IsAttached() {
		t.Fatal("view attach must NOT occupy the writer slot")
	}
	waitFor(t, func() bool { return mux.ViewerCount() == 1 })
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./server/ -run TestHandleAttachSession_ViewMode_NoWriterTakeover -v`
Expected: FAIL — view requests currently go through `mux.Attach` (writer takeover), so `mux.IsAttached()` is true / `ViewerCount()` is 0.

- [ ] **Step 3: Add the mode branch**

In `handleAttachSession` (`server/task_handler.go`), replace the attach call (~`:741`):

```go
	if err := mux.Attach(parentCtx, tuiStream); err != nil {
		slog.Error("AttachSession: mux.Attach", "task", idHex, "err", err)
		_ = tuiStream.CloseBoth()
		return errResp(protocol.AttachSessionStatus_InternalError)
	}
```

with:

```go
	attach := mux.Attach
	if req.Mode == protocol.AttachMode_View {
		attach = mux.AttachViewer
	}
	if err := attach(parentCtx, tuiStream); err != nil {
		slog.Error("AttachSession: attach", "task", idHex, "mode", req.Mode, "err", err)
		_ = tuiStream.CloseBoth()
		return errResp(protocol.AttachSessionStatus_InternalError)
	}
```

(`Attach` and `AttachViewer` share the signature `func(context.Context, trsf.BidirectionalStream) error`, so the `attach` variable typechecks.)

- [ ] **Step 4: Run it to verify it passes**

Run: `go test ./server/ -run 'TestHandleAttachSession' -v`
Expected: PASS (the new test plus all pre-existing `TestHandleAttachSession_*`).

- [ ] **Step 5: Commit**

```bash
git add server/task_handler.go server/task_handler_test.go
git commit -m "feat(server): route AttachSession mode=view to AttachViewer"
```

---

## Task 5: CLI — thread `mode` and add `session attach --view`

**Files:**
- Modify: `cli/attach.go`, `cli/attach_native.go`, `cli/attach_js.go`, `cmd/harness-cli/session.go`

> Note: a viewer's keystrokes are discarded server-side (Task 3), so the CLI
> reuses the existing splice; view mode just prints a different banner. A
> dedicated output-only renderer is a future enhancement, out of scope here.

- [ ] **Step 1: Add `mode` to the shared RPC builder**

In `cli/attach.go`, change `attachSessionRPC` (~`:17`):

```go
func (c *Client) attachSessionRPC(ctx context.Context, taskIDHex string, mode protocol.AttachMode) (trsf.BidirectionalStream, uint64, error) {
```

and the request build (~`:24`):

```go
	req.SetAttach(protocol.AttachSessionRequest{TaskId: tid, Mode: mode})
```

- [ ] **Step 2: Thread `mode` through native `AttachSession` / `SessionAttach`**

In `cli/attach_native.go`:

```go
func (c *Client) AttachSession(ctx context.Context, taskIDHex string, mode protocol.AttachMode) (*agentexec.CommandExecutionStream, uint64, error) {
	st, replayBytes, err := c.attachSessionRPC(ctx, taskIDHex, mode)
	if err != nil {
		return nil, 0, err
	}
	return agentexec.NewCommandExecutionStream(st), replayBytes, nil
}

func (c *Client) SessionAttach(ctx context.Context, taskIDHex string, mode protocol.AttachMode) (string, error) {
	stream, replayBytes, err := c.AttachSession(ctx, taskIDHex, mode)
	if err != nil {
		return taskIDHex, err
	}
	defer stream.Close()

	if mode == protocol.AttachMode_View {
		fmt.Fprintf(os.Stderr, "harness-cli: VIEW-ONLY attach to task %s (replay %d bytes; your input is ignored; Ctrl+] to detach)\n", taskIDHex, replayBytes)
	} else {
		fmt.Fprintf(os.Stderr, "harness-cli: attached to task %s (replay %d bytes; Ctrl+] to detach client; Ctrl+D / `exit` ends the session)\n", taskIDHex, replayBytes)
	}

	if err := stream.RemoteShell(); err != nil {
		return taskIDHex, err
	}
	return taskIDHex, nil
}
```

Add the `"github.com/on-keyday/agent-harness/runner/protocol"` import to `cli/attach_native.go` if not already present.

- [ ] **Step 3: Thread `mode` through WASM `AttachSession`**

In `cli/attach_js.go`, change the signature and the RPC call:

```go
func (c *Client) AttachSession(ctx context.Context, taskIDHex string, mode protocol.AttachMode) (string, error) {
	stream, _, err := c.attachSessionRPC(ctx, taskIDHex, mode)
	if err != nil {
		return "", err
	}
	...
```

Add the `"github.com/on-keyday/agent-harness/runner/protocol"` import to `cli/attach_js.go`.

- [ ] **Step 4: Add the `--view` flag to `session attach`**

In `cmd/harness-cli/session.go`, locate the `attach` subcommand handling (it calls `SessionAttach`). Add a `--view` bool flag to its flagset and pass the mode:

```go
	view := fs.Bool("view", false, "attach read-only (observe; your input is ignored)")
	// ... after parsing args, where SessionAttach is called:
	mode := protocol.AttachMode_Control
	if *view {
		mode = protocol.AttachMode_View
	}
	_, err := client.SessionAttach(ctx, taskIDHex, mode)
```

(Match the existing flag-registration and call style in that file; add the `protocol` import if missing.)

- [ ] **Step 5: Build all targets**

Run: `go build ./...`
Expected: clean. Then `make wasm-check` (compiles the WASM build of `cli` + webui-wasm).
Expected: clean — confirms `cli/attach_js.go` and any WASM callers typecheck.

- [ ] **Step 6: Commit**

```bash
git add cli/attach.go cli/attach_native.go cli/attach_js.go cmd/harness-cli/session.go
git commit -m "feat(cli): session attach --view (read-only) threads AttachMode"
```

---

## Task 6: TUI — view-attach key

**Files:**
- Modify: `tui/interactive.go` (`DoAttachSession`, ~`:84`), `tui/app.go` (key handling + reattach call, ~`:620-623`, hint ~`:814`)

- [ ] **Step 1: Add `mode` to `DoAttachSession`**

In `tui/interactive.go`:

```go
func DoAttachSession(c *cli.Client, taskIDHex string, mode protocol.AttachMode) tea.Cmd {
	return func() tea.Msg {
		stream, _, err := c.AttachSession(context.Background(), taskIDHex, mode)
		if err != nil {
			return InteractiveReadyMsg{Err: fmt.Errorf("attach session: %w", err)}
		}
		return InteractiveReadyMsg{Stream: stream, TaskID: taskIDHex}
	}
}
```

Add the `"github.com/on-keyday/agent-harness/runner/protocol"` import to `tui/interactive.go`.

- [ ] **Step 2: Update the existing reattach caller to pass control mode**

In `tui/app.go`, the existing reattach call (~`:623` and `:1000`):

```go
				return a, DoAttachSession(a.client, a.tasks.SelectedID(), protocol.AttachMode_Control)
```

and

```go
	case SessionAttachAction:
		return a, DoAttachSession(a.client, v.TaskID, protocol.AttachMode_Control)
```

Add the `protocol` import to `tui/app.go` if missing.

- [ ] **Step 3: Add the `v` key for view-attach**

In `tui/app.go`, alongside the `r`/`R` reattach key handling (~`:616-623`), add a `v` case that view-attaches the selected task when it is a live (Running/Detached) detachable session — reuse the same gate `resumeReattachAction` already applies:

```go
		case "v":
			act := resumeReattachAction(a.tasks.SelectedTask(), true)
			if act.Kind == actionReattach {
				return a, DoAttachSession(a.client, a.tasks.SelectedID(), protocol.AttachMode_View)
			}
```

Update the key hint string (~`:814`) to include `v view`:

```go
		hint = "tab focus · ←/→ scroll · / filter · s submit · S session · i interactive · r reattach/resume · R resume-fresh · v view-only · F file picker · d detail · c cancel · p/P L-forward · b/B R-forward · q quit"
```

- [ ] **Step 4: Build the TUI**

Run: `go build ./tui/... ./cmd/harness-tui/`
Expected: clean. (Per build-hygiene: this writes no binary because no `-o`; `go build ./pkg/...` only typechecks.)

If a stray `harness-tui` binary appears, remove it: `git status` must be clean of build artifacts before committing.

- [ ] **Step 5: Run TUI tests**

Run: `go test ./tui/ -v`
Expected: PASS (existing `TestResumeReattachAction` etc. still green).

- [ ] **Step 6: Commit**

```bash
git add tui/interactive.go tui/app.go
git commit -m "feat(tui): v key view-only attaches the selected session"
```

---

## Task 7: WebUI — "👁 View" button

**Files:**
- Modify: `cmd/harness-webui-wasm/main.go` (`harnessAttachSession`), `webui/static/main.js` (reattach button area, ~`:597-600`; `reattachTo` helper)

- [ ] **Step 1: Read the two functions you will edit**

Read `harnessAttachSession` in `cmd/harness-webui-wasm/main.go` and the `reattachTo` helper + the `mkBtn("↪ Reattach", ...)` call sites (~`:597-600`) in `webui/static/main.js`. Preserve their existing logic; you are only threading an optional `mode`.

- [ ] **Step 2: Read an optional `mode` arg in `harnessAttachSession`**

In `harnessAttachSession` (`cmd/harness-webui-wasm/main.go`), after resolving the task-id arg and before calling `c.AttachSession`, parse an optional second arg and map it:

```go
				mode := protocol.AttachMode_Control
				if len(args) > 1 && args[1].Type() == js.TypeString && args[1].String() == "view" {
					mode = protocol.AttachMode_View
				}
				// existing call, now passing mode:
				taskID, err := c.AttachSession(ctx, taskIDHex, mode)
```

Add the `"github.com/on-keyday/agent-harness/runner/protocol"` import to `cmd/harness-webui-wasm/main.go` if missing.

- [ ] **Step 3: Thread mode through `reattachTo` and add the View button**

In `webui/static/main.js`, give `reattachTo` an optional mode and forward it to the binding:

```js
  function reattachTo(id, view) {
    // ...existing body, but the attachSession call becomes:
    return harness.attachSession(id, view ? "view" : "control");
  }
```

At each place a Reattach button is offered for a live session (~`:597` and `:600`), add a View button next to it:

```js
          mkBtn("↪ Reattach", () => reattachTo(t.id, false));
          mkBtn("👁 View", () => reattachTo(t.id, true));
```

(Keep the existing Reattach call working — if its current form is `mkBtn("↪ Reattach", reattachTo)` passing the function directly, change it to the arrow form above so the `view` arg is explicit.)

- [ ] **Step 4: Build the WASM + webui**

Run: `make wasm-check`
Expected: clean (WASM build of webui-wasm typechecks with the new `mode` arg).

- [ ] **Step 5: Commit**

```bash
git add cmd/harness-webui-wasm/main.go webui/static/main.js
git commit -m "feat(webui): 👁 View button for read-only attach"
```

---

## Task 8: Full verification

**Files:** none (verification only)

- [ ] **Step 1: Vet + unit tests across the touched Go packages**

Run: `make check`
Expected: PASS.

- [ ] **Step 2: WASM build check**

Run: `make wasm-check`
Expected: PASS.

- [ ] **Step 3: Race-test the server core once more**

Run: `go test ./server/ -race -run 'TestSessionMux_|TestHandleAttachSession'`
Expected: PASS, no races.

- [ ] **Step 4: Confirm the worktree is clean of build artifacts**

Run: `git status --porcelain`
Expected: empty (no stray `harness-tui`, `*.test`, `*.wasm`, or `.playwright-mcp/` noise staged).

- [ ] **Step 5: Manual smoke (optional, via Playwright MCP)**

Resume a bash-runner session, open the WebUI, click "👁 View" on a second browser context, confirm read-only streaming while another client holds the writer. (Driving instructions: `project_playwright_webui_visual_check` memory.)

---

## Self-review

**Spec coverage:**
- View-only attach mode, all 3 surfaces → Tasks 1 (schema), 5 (CLI), 6 (TUI), 7 (WebUI). ✓
- Server fan-out, unchanged writer path, bounded-queue drop, read-and-discard input drain (`ReadDirectContext`) → Task 3. ✓
- Routing on reattach handler only; `open_interactive` untouched → Task 4. ✓
- Viewers orthogonal to Running/Detached state machine → Task 3 (`TestSessionMux_ViewerDoesNotFireOnAttach`) + Task 4 (`...NoWriterTakeover`). ✓
- Runner protocol untouched → no runner files in the file map. ✓
- Non-goals (watcher count / auto-reconnect / in-place promotion) → not implemented. ✓
- Verification with `make check` + `make wasm-check` → Task 8. ✓

**Placeholder scan:** No TBD/TODO; every code step shows the code. The `viewerQueueDepth = 256` is an explicit value. ✓

**Type consistency:** `AttachViewer(ctx, stream) error` matches `Attach`'s signature (used as the `attach` variable in Task 4). `AttachSession(ctx, taskIDHex, mode)` — the 3-arg form is used consistently in Tasks 5/6/7 and the callers (`tui/interactive.go`, `cmd/harness-webui-wasm/main.go`) are all updated. `dropViewer`/`dropViewerLocked`/`ViewerCount`/`viewerOutputPump`/`viewerInputDrain`/`viewerConn`/`viewerQueueDepth` names are consistent across Tasks 3 and 4. ✓
