# Server-side screen model and repaint on attach — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give `SessionMux` a `vtgrid.Terminal` per session and end an attach's
replay with a repaint synthesised from it, so a reattaching client gets the
screen instead of whatever the replayed bytes happen to reconstruct.

**Architecture:** One grid per session, created with the mux and fed from
`runnerPump` alongside the ring, resized through the single `applyWinSizeFrame`
funnel. On attach the replay becomes `winsize │ preamble │ ring │ repaint`, with
the two server-synthesised payloads carried on a new `FrameType_Synth` so
`--raw` can still tell invented bytes from PTY bytes. The ring keeps its size
and its job: it is the scrollback.

**Tech Stack:** Go; `github.com/on-keyday/objtrsf` (`exec/frame`, `exec`);
`vtgrid`; brgen (`scripts/protoregen.sh`) for `.bgn` codegen.

**Spec:** `docs/superpowers/specs/2026-08-22-server-side-screen-repaint-design.md`

## Global Constraints

- **Two repositories, in order.** `objtrsf` (`/home/kforfk/workspace/objtrsf`)
  lands first and publishes; the harness bumps to it. Nothing in the harness
  compiles against `FrameType_Synth` until Task 2 is done.
- **Landing is Mode A in both repos**: rebase onto the current trunk, FF-push to
  `origin/main`, never cherry-pick to the remote, never `--force`. Then
  `make build` in the harness main checkout. See the `landing-to-main` skill.
- **Verify with make targets, not ad-hoc go commands**: `make vet`, `make test`,
  `make wasm-check`. `go test ./...` alone hides pattern breaks.
- **The Windows console leg cannot run here.** `vtgrid/repaint_console_windows_test.go`
  and `server/mode_tracker_console_windows_test.go` are `//go:build windows` and
  need a real conhost. Type-check them with `GOOS=windows go vet ./vtgrid/ ./server/`
  after any change they touch, and get them RUN on the Windows host before the
  final land.
- **`buildRepaint`'s byte order is load-bearing and already gated.** Do not
  reorder it. Its three orderings and the leading `ESC[?1049l` were each fixed
  by a measurement on a real console; the tests that would catch a regression
  are the ones that cannot run on this machine.
- **Ring semantics do not change.** `replayLimit`'s numbers, the 1 MiB default,
  and `capReplayTail`'s ground-state scan all stay exactly as they are.

---

### Task 1: `FrameType_Synth` in objtrsf

**Files:**
- Modify: `/home/kforfk/workspace/objtrsf/exec/frame/frame.bgn`
- Regenerate: `/home/kforfk/workspace/objtrsf/exec/frame/frame.go`
- Modify: `/home/kforfk/workspace/objtrsf/exec/exec_stream.go:59-92` (the dispatch switch)
- Test: `/home/kforfk/workspace/objtrsf/exec/exec_stream_synth_test.go` (create)

**Interfaces:**
- Produces: `frame.FrameType_Synth` (value 4), and the guarantee that
  `CommandExecutionStream` delivers a Synth frame's payload on `Stdout()` in
  frame order, indistinguishably from a Stdout frame's payload.

- [ ] **Step 1: Write the failing test**

Create `/home/kforfk/workspace/objtrsf/exec/exec_stream_synth_test.go`. Match
the existing helpers in `exec_test.go` for building a fake stream — read it
first and reuse whatever it already has rather than inventing a second fake.

```go
package exec

import (
	"bytes"
	"io"
	"testing"

	"github.com/on-keyday/objtrsf/exec/frame"
)

// A Synth frame carries bytes the SERVER synthesised — a screen repaint, a
// mode preamble — which the terminal must apply exactly like PTY output and in
// the same position in the stream. So it lands on Stdout(), in frame order,
// and only its TYPE distinguishes it. A consumer that needs the distinction
// reads frames itself; see the harness's CollectRaw.
func TestCommandExecutionStreamDeliversSynthOnStdoutInOrder(t *testing.T) {
	var wire []byte
	for _, f := range []struct {
		typ  frame.FrameType
		data string
	}{
		{frame.FrameType_Stdout, "real-"},
		{frame.FrameType_Synth, "synth-"},
		{frame.FrameType_Stdout, "real2"},
	} {
		hdr := frame.FrameHeader{Type: f.typ, Len: uint32(len(f.data))}
		wire = append(wire, hdr.MustAppend(nil)...)
		wire = append(wire, f.data...)
	}

	st := newFakeBidiStream(t, wire) // reuse exec_test.go's fake
	w := NewCommandExecutionStream(st)
	defer w.Close()

	got, err := io.ReadAll(w.Stdout())
	if err != nil && err != io.EOF {
		t.Fatalf("read: %v", err)
	}
	if want := "real-synth-real2"; string(got) != want {
		t.Errorf("Stdout() = %q, want %q (synth must interleave in frame order)", got, want)
	}
	_ = bytes.MinRead
}
```

- [ ] **Step 2: Run it and watch it fail**

```bash
cd /home/kforfk/workspace/objtrsf && go test ./exec/ -run TestCommandExecutionStreamDeliversSynthOnStdout -v
```

Expected: a compile error, `undefined: frame.FrameType_Synth`. If instead the
fake-stream helper name is wrong, fix the helper name — that is a real finding
about `exec_test.go`, not a reason to write a second fake.

- [ ] **Step 3: Add the enum value to the schema**

In `frame.bgn`, the enum becomes:

```
enum FrameType:
    :u8
    stdin
    stdout
    stderr
    control
    synth
```

`synth` goes LAST so the existing four keep their wire values. Do not reorder.

- [ ] **Step 4: Regenerate**

```bash
cd /home/kforfk/workspace/remote-agent-harness
scripts/protoregen.sh /home/kforfk/workspace/objtrsf/exec/frame/frame.bgn
```

The brgen kit is already cached at `~/.cache/brgen-kit`, so this starts a local
api server and returns in a few seconds. It writes `frame.go` beside the `.bgn`
and gofmts it.

- [ ] **Step 5: Read the generated diff before trusting it**

```bash
cd /home/kforfk/workspace/objtrsf && git diff --stat exec/frame/frame.go && git diff exec/frame/frame.go | head -60
```

Expected: the const block gains `FrameType_Synth FrameType = 4`, and `String()`
gains its case. **If anything else changed, stop and read it.** The generator
version may have moved since the file was last generated, in which case the diff
carries unrelated churn that must be understood before it lands — it is not
automatically safe just because a generator produced it.

- [ ] **Step 6: Route Synth to stdout**

In `exec/exec_stream.go`, the dispatch at line ~59:

```go
			switch hdr.Header.Type {
			case frame.FrameType_Stdout, frame.FrameType_Synth:
				if hdr.Header.Len == 0 {
					stdoutPipeW.Close()
					continue
				}
```

Only the `case` line changes. Add a comment above it saying why the two share a
destination: the bytes are for the terminal either way, and the type exists so
a frame-level reader can tell them apart, not so they land in different places.

- [ ] **Step 7: Run the test**

```bash
cd /home/kforfk/workspace/objtrsf && go test ./exec/ -run TestCommandExecutionStreamDeliversSynthOnStdout -v
```

Expected: PASS.

- [ ] **Step 8: Full objtrsf suite**

```bash
cd /home/kforfk/workspace/objtrsf && go build ./... && go vet ./... && go test ./...
```

Expected: all green. `exec/exec.go:255`'s inbound `default:` still warns on an
unknown type — that path receives stdin-direction frames and never a Synth, so
it is correct as it stands.

- [ ] **Step 9: Commit and land**

```bash
cd /home/kforfk/workspace/objtrsf
git add exec/frame/frame.bgn exec/frame/frame.go exec/exec_stream.go exec/exec_stream_synth_test.go
git commit   # message: the four points below
git fetch origin main && git merge-base --is-ancestor origin/main HEAD
git push origin HEAD:main
```

The message must carry: that Synth is appended so existing wire values are
untouched; that it shares `Stdout()`'s destination on purpose; that an old
consumer's `default: // ignore unknown frame types` makes this degrade rather
than break; and that `ControlType` could not have been used because its match
ends in `.. => error(...)`, which would hard-fail old consumers instead.

---

### Task 2: Bump the harness to the new objtrsf

**Files:**
- Modify: `go.mod`, `go.sum`

**Interfaces:**
- Consumes: `frame.FrameType_Synth` from Task 1.
- Produces: a harness tree in which `frame.FrameType_Synth` compiles.

- [ ] **Step 1: Bump**

```bash
GOPROXY=direct go get github.com/on-keyday/objtrsf@main
```

- [ ] **Step 2: Confirm the version moved**

```bash
grep objtrsf go.mod
```

Expected: a pseudo-version newer than `v0.0.0-20260820153246-b8b4d6dcd21d`.

- [ ] **Step 3: Verify nothing else moved with it**

```bash
make vet && make test && make wasm-check
```

Expected: green. An objtrsf bump can carry unrelated upstream commits; if
anything fails, the failure belongs to this step and must be understood before
continuing.

- [ ] **Step 4: Commit**

```bash
git add go.mod go.sum
git commit -m "chore: bump objtrsf for FrameType_Synth"
```

---

### Task 3: `(*vtgrid.Terminal).Repaint()`

**Files:**
- Create: `vtgrid/repaint.go`
- Modify: `vtgrid/repaint_test.go` (drop the local `buildRepaint`, call the method)
- Modify: `vtgrid/repaint_console_windows_test.go` (same call sites)

**Interfaces:**
- Produces: `func (t *Terminal) Repaint() []byte` — the byte sequence that
  reconstructs `t`'s screen on a terminal in any prior state.

- [ ] **Step 1: Move the function, verbatim**

Create `vtgrid/repaint.go` in `package vtgrid`. Take the CURRENT body of
`buildRepaint` from `vtgrid/repaint_test.go` unchanged — every ordering in it
was fixed by a measurement, and the Windows tests that would catch a regression
cannot run here. Rename to `Repaint`, make it a method, and carry its entire
doc comment across; the comment is the record of why the order is what it is.

The body loses nothing but its parameter:

```go
// Repaint synthesises the bytes that make a terminal show this screen …
// [carry the whole existing comment, unchanged]
func (t *Terminal) Repaint() []byte {
	_, rows := t.Size()
	x, y, vis := t.Cursor()
	var b strings.Builder
	// … body unchanged from buildRepaint …
	return []byte(b.String())
}
```

Add one sentence to the top of the comment saying what it is the inverse of:
`ANSI()` emits a block of text and says so; `Repaint()` is the program that
paints it.

- [ ] **Step 2: Point the tests at the method**

In both test files, delete the local `buildRepaint` and replace every
`buildRepaint(server)` / `buildRepaint(term)` with `server.Repaint()` /
`term.Repaint()`. The external test package can reach it now that it is
exported.

- [ ] **Step 3: Run the gates**

```bash
gofmt -l vtgrid/ && go test ./vtgrid/ -run TestRepaint -v && GOOS=windows go vet ./vtgrid/
```

Expected: `TestRepaintReconstructsScreen` still reports ROWS 1416/1416-shaped
totals with every observer state passing, `TestRepaintCorpusListIsComplete`
passes, and the Windows file type-checks.

- [ ] **Step 4: Full suite**

```bash
make vet && make test
```

- [ ] **Step 5: Commit**

```bash
git add vtgrid/repaint.go vtgrid/repaint_test.go vtgrid/repaint_console_windows_test.go
git commit -m "refactor(vtgrid): export the repaint sequence as Terminal.Repaint"
```

---

### Task 4: The grid, fed and resized

**Files:**
- Modify: `server/session_mux.go` (struct, `NewSessionMux`, `runnerPump`, `applyWinSizeFrame`)
- Test: `server/session_mux_screen_test.go` (create)

**Interfaces:**
- Consumes: `(*vtgrid.Terminal).Repaint()` from Task 3 (not called yet — Task 5).
- Produces: `func (m *SessionMux) screenRepaint() []byte` returning the repaint
  for the current screen, and `func (m *SessionMux) screenSize() (cols, rows int)`
  for tests. Both take the screen's own mutex.

- [ ] **Step 1: Write the failing tests**

Create `server/session_mux_screen_test.go`. Reuse `newFakeStream`,
`makeWireFrame` and `waitFor` from the existing `mode_tracker_test.go` /
`session_mux_test.go` — do not write new fakes.

```go
package server

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The grid is fed from the same frames the ring gets, so a screen exists
// without anyone having attached — which is the point: it saw bytes the ring
// will have evicted by the time someone does.
func TestSessionMuxScreenIsFedFromRunnerPump(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(1<<20), SessionHooks{})

	runner.QueueRead(makeWireFrame(1, []byte("\x1b[2J\x1b[1;1Hhello")))
	waitFor(t, func() bool { return strings.Contains(string(mux.screenRepaint()), "hello") })
}

// Both resize entry points reach the grid, because both funnel through
// applyWinSizeFrame. The observer path is the one that is easy to miss: it
// only applies while the control seat is empty, which is exactly the
// unattended-worker case exec_resize exists for.
func TestSessionMuxScreenResizesFromBothEntryPoints(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(1<<20), SessionHooks{})

	// Control path: no seat holder needed, applyWinSizeFrame is reached directly.
	if err := mux.applyWinSizeFrame(winSizeFrame(t, 100, 30)); err != nil {
		t.Fatalf("control resize: %v", err)
	}
	if cols, rows := mux.screenSize(); cols != 100 || rows != 30 {
		t.Errorf("after control resize screen is %dx%d, want 100x30", cols, rows)
	}

	// Observer path, seat empty.
	if err := mux.applyObserverWinSize(winSizeFrame(t, 80, 24)); err != nil {
		t.Fatalf("observer resize: %v", err)
	}
	if cols, rows := mux.screenSize(); cols != 80 || rows != 24 {
		t.Errorf("after observer resize screen is %dx%d, want 80x24", cols, rows)
	}
}
```

`winSizeFrame(t, cols, rows)` is a helper this file adds: build a
`frame.Control` with `ControlType_TerminalWindowSize` and wrap it in a
`FrameType_Control` frame. `server/session_mux_winsize_test.go` almost
certainly already has one — read it and reuse rather than duplicate.

- [ ] **Step 2: Run them and watch them fail**

```bash
go test ./server/ -run 'TestSessionMuxScreen' -v
```

Expected: compile error on `screenRepaint` / `screenSize`.

- [ ] **Step 3: Add the screen to the struct**

In `SessionMux`:

```go
	// screen is the session's terminal state as a grid, fed from the same
	// frames the ring gets. It exists from the mux's first byte, not from the
	// first attach: its whole value is that it saw output the ring has since
	// evicted, and one seeded from the ring later would just be the ring.
	//
	// Its own mutex, not m.mu, for the same reason modeTracker has one — the
	// feed is on runnerPump's hot path and m.mu is held by fan-out and attach.
	screenMu sync.Mutex
	screen   *vtgrid.Terminal
```

- [ ] **Step 4: Create it in `NewSessionMux`**

Beside the ring, before `go m.runnerPump()`. Size: from the caller when it has
one, else 80x24 per the spec §4.2. `NewSessionMux`'s signature gains
`cols, rows int`; pass `0, 0` from callers that have no size and default
inside, so no caller has to know the default.

- [ ] **Step 5: Feed it in `runnerPump`**

In the existing `switch frame.FrameType(frameBytes[0])`, in the
`Stdout, Stderr` arm, beside `m.modes.feed(...)`:

```go
			m.screenMu.Lock()
			_, _ = m.screen.Write(frameBytes[frameHeaderSize:])
			m.screenMu.Unlock()
```

- [ ] **Step 6: Resize it in `applyWinSizeFrame`**

`applyWinSizeFrame` already parses nothing — it stores the raw frame. Parse the
size out of `fb` with the same helper `frameIsWinSize` uses, and resize:

```go
	if cols, rows, ok := winSizeOf(fb); ok {
		m.screenMu.Lock()
		m.screen.Resize(cols, rows)
		m.screenMu.Unlock()
	}
```

`winSizeOf` is a small helper beside `frameIsWinSize`, which already decodes
the frame to check its control type — factor the decode so both use it rather
than decoding twice.

- [ ] **Step 7: Add the two accessors**

```go
// screenRepaint returns the byte sequence that reconstructs the current screen.
func (m *SessionMux) screenRepaint() []byte {
	m.screenMu.Lock()
	defer m.screenMu.Unlock()
	return m.screen.Repaint()
}

// screenSize reports the grid's dimensions. Test-facing.
func (m *SessionMux) screenSize() (cols, rows int) {
	m.screenMu.Lock()
	defer m.screenMu.Unlock()
	return m.screen.Size()
}
```

- [ ] **Step 8: Run the tests, then everything**

```bash
go test ./server/ -run 'TestSessionMuxScreen' -v && make vet && make test
```

Expected: PASS, and the whole suite green — `NewSessionMux`'s new parameters
touch every construction site, and `make test` is what finds them.

- [ ] **Step 9: Commit**

```bash
git add server/session_mux.go server/session_mux_screen_test.go server/task_handler.go
git commit -m "feat(server): hold a screen model per session"
```

---

### Task 5: Repaint in the replay, as a Synth frame

**Files:**
- Modify: `server/session_mux.go` (`Attach`, `attachObserver`, and the frame encoder)
- Test: `server/session_mux_replay_test.go` (create)

**Interfaces:**
- Consumes: `screenRepaint()` from Task 4, `frame.FrameType_Synth` from Task 2.
- Produces: an attach replay ending in a Synth-framed repaint, on both paths.

- [ ] **Step 1: Write the failing test**

```go
// The replay's shape is load-bearing and each part has a different job, so it
// is asserted as a shape: size, then the modes the grid does not model, then
// the history, then the screen. The two synthesised payloads carry a frame
// type that says so — `session snapshot --raw` exists to show the bytes the
// PTY actually emitted, and it cannot do that if invented bytes wear the same
// label.
func TestAttachReplayEndsWithASynthFramedRepaint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(1<<20), SessionHooks{}, 80, 24)

	runner.QueueRead(makeWireFrame(1, []byte("\x1b[2J\x1b[1;1Hhello")))
	waitFor(t, func() bool { return mux.RingBufferLen() > 0 })

	tui := newFakeStream(t)
	if err := mux.Attach(ctx, tui); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	frames := decodeFrames(t, tui.WaitWrittenAny(t))
	last := frames[len(frames)-1]
	if last.Type != frame.FrameType_Synth {
		t.Errorf("last replay frame is %v, want Synth (the repaint)", last.Type)
	}
	if !strings.Contains(string(last.Data), "hello") {
		t.Errorf("repaint does not carry the screen: %q", last.Data)
	}
	for _, f := range frames {
		if f.Type == frame.FrameType_Stdout && strings.Contains(string(f.Data), "\x1b[?25") {
			t.Error("the mode preamble is still riding as Stdout; it is synthesised and must say so")
		}
	}
}
```

`decodeFrames` is a helper this file adds, reading frames with
`frame.Frame.Read` over a `bytes.Reader`. `WaitWrittenAny` may need adding to
the fake beside the existing `WaitWritten(t, n)` — read the fake first.

- [ ] **Step 2: Run it and watch it fail**

```bash
go test ./server/ -run TestAttachReplayEndsWithASynthFramedRepaint -v
```

Expected: the last frame is the ring's, not a Synth repaint.

- [ ] **Step 3: Add a Synth encoder beside `encodeStdoutFrame`**

```go
// encodeSynthFrame wraps bytes the SERVER invented — a mode preamble, a screen
// repaint — so a frame-level reader can tell them from bytes the PTY emitted.
// They go to the same place on the terminal; only the label differs.
func encodeSynthFrame(p []byte) []byte { … same shape as encodeStdoutFrame, type Synth … }
```

- [ ] **Step 4: Use it in both attach paths**

In `Attach` and in `attachObserver`, change the preamble's
`encodeStdoutFrame(pre)` to `encodeSynthFrame(pre)`, and after the ring
snapshot append:

```go
	if rp := m.screenRepaint(); len(rp) > 0 {
		replay = append(replay, encodeSynthFrame(rp)...)
	}
```

In `Attach` the repaint goes after `m.replaySnapshot()`; in `attachObserver`
after `capReplayTail(...)`. The repaint is NEVER capped — `replayLimit` bounds
history, and the screen is not history.

- [ ] **Step 5: Run the test, then everything**

```bash
go test ./server/ -run TestAttachReplay -v && make vet && make test
```

`TestSessionMux_AttachRestoresEvictedCursorMode` asserts an exact replay byte
sequence and WILL fail here: its expected bytes are a Stdout-framed preamble
and no repaint. Update it — the preamble's frame type changed and a repaint now
follows — and keep its original point intact, which is that an evicted mode is
still re-established.

- [ ] **Step 6: Commit**

```bash
git add server/session_mux.go server/session_mux_replay_test.go server/mode_tracker_test.go
git commit -m "feat(server): end an attach replay with the screen, framed as synthesised"
```

---

### Task 6: The alt-screen trim becomes conditional

**Files:**
- Modify: `server/ring_buffer.go` (oldest-index accessor)
- Modify: `server/session_mux.go` (`runnerPump` transition list, `replaySnapshot`)
- Test: `server/session_mux_alttrim_test.go` (create)

**Interfaces:**
- Produces: `func (r *RingBuffer) OldestIndex() int` — the append-index of the
  oldest surviving frame, the value `SnapshotFrom` already computes internally.

- [ ] **Step 1: Write the failing tests**

The straddle case is the one that matters, because the obvious predicate gets
it wrong:

```go
// Two episodes, and the ring's oldest surviving frame sits INSIDE the first.
// "Is the most recent alt entry still in the ring" answers yes and is wrong:
// E1 straddles the start, so its fragments would paint the primary screen and
// scroll into scrollback. The predicate has to be about the ring's oldest
// frame, not about the newest entry.
func TestReplayTrimsWhenTheRingStartsInsideAnEpisode(t *testing.T) { … }

// The ordinary case: the entry survives, so the client enters the alternate
// buffer, the episode paints THAT buffer, and the pre-episode history is
// preserved instead of being trimmed away.
func TestReplayKeepsHistoryWhenTheEntrySurvives(t *testing.T) { … }
```

Write both fully, driving the mux with `makeWireFrame` payloads containing
`\x1b[?1049h` / `\x1b[?1049l` and sizing the ring so eviction lands where the
test needs it. Assert on whether `replaySnapshot()` starts at the ring's oldest
frame or at `mainMark`.

- [ ] **Step 2: Run them and watch them fail**

```bash
go test ./server/ -run 'TestReplay(Trims|Keeps)' -v
```

- [ ] **Step 3: Add the ring accessor**

```go
// OldestIndex returns the append-index of the oldest surviving frame. Callers
// that need to know whether a recorded index is still in the window compare
// against it; SnapshotFrom computes the same value internally.
func (r *RingBuffer) OldestIndex() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.appendCount - len(r.frames)
}
```

- [ ] **Step 4: Track transitions in `runnerPump`**

Beside the existing `mainMark` store, keep an ordered slice of
`(index, enteredAlt bool)` and, on each append, drop entries below
`m.ring.OldestIndex()` while remembering the direction of the last one dropped.
That remembered direction IS the alt state at the ring's oldest surviving frame.

- [ ] **Step 5: Make `replaySnapshot` conditional**

```go
func (m *SessionMux) replaySnapshot() []byte {
	if m.modes.onAltScreen() {
		return m.ring.Snapshot()
	}
	if !m.altAtOldestSurviving() {
		// The ring starts on the primary screen, so a finished episode inside
		// it enters and leaves the alternate buffer on its own and touches
		// neither the primary screen nor scrollback. Keep the history.
		return m.ring.Snapshot()
	}
	return m.ring.SnapshotFrom(int(m.mainMark.Load()))
}
```

- [ ] **Step 6: Run everything**

```bash
go test ./server/ -run 'TestReplay' -v && make vet && make test
```

- [ ] **Step 7: Commit**

```bash
git add server/ring_buffer.go server/session_mux.go server/session_mux_alttrim_test.go
git commit -m "feat(server): trim a finished alt episode only when the ring starts inside one"
```

---

### Task 7: `--raw` reads frames and keeps provenance

**Files:**
- Modify: `cli/snapshot_raw.go` (`CollectRaw`)
- Modify: `cmd/harness-cli/session.go` (the `--raw` help text)
- Test: `cli/snapshot_raw_test.go` (create)

**Interfaces:**
- Consumes: `frame.FrameType_Synth`.
- Produces: `CollectRaw` returning PTY bytes only, with synthesised bytes
  reported separately rather than merged.

- [ ] **Step 1: Decide the presentation, then write the test for it**

The spec deliberately left this open. Take the smallest honest option:
`CollectRaw` returns the PTY bytes (Stdout/Stderr frames) as `captured`, and
reports the synthesised total separately so `--raw` can say what it withheld.
`--raw` prints the PTY bytes to stdout unchanged — which is what it has always
promised — and writes one line to STDERR naming how many synthesised bytes were
dropped, so the omission is visible without polluting the artifact.

Write `cli/snapshot_raw_test.go` asserting that a stream carrying
`[Stdout "a"][Synth "REPAINT"][Stdout "b"]` yields `captured == "ab"` and a
synthesised count of 7.

- [ ] **Step 2: Run it and watch it fail**

```bash
go test ./cli/ -run TestCollectRaw -v
```

- [ ] **Step 3: Replace the wrapper with a frame loop**

`CollectRaw` currently wraps the stream in `agentexec.NewCommandExecutionStream`
and reads `stream.Stdout()`, which merges Stdout and Synth into one pipe and
loses exactly the distinction this task exists for. Read frames instead:

```go
	f := &frame.Frame{}
	for {
		if err := f.Read(rd); err != nil { break }
		switch f.Header.Type {
		case frame.FrameType_Stdout, frame.FrameType_Stderr:
			data = append(data, *f.Data()...)
		case frame.FrameType_Synth:
			synthBytes += len(*f.Data())
		case frame.FrameType_Control:
			// the size the server replays ahead of the ring, as before
		}
	}
```

Keep the settle window, the 8 MiB cap and the `AttachMode_View` attach exactly
as they are. `LastWindowSize()`'s job moves into the `Control` arm.

- [ ] **Step 4: Update the `--raw` help text**

It currently promises "the verbatim PTY replay bytes". That is now TRUE in a way
it was not before — the preamble used to ride as Stdout — so the text should say
so, and say that synthesised bytes are reported on stderr rather than silently
absent.

- [ ] **Step 5: Run everything**

```bash
go test ./cli/ -run TestCollectRaw -v && make vet && make test && make wasm-check
```

`make wasm-check` matters here: `cli/snapshot_raw.go` is untagged and serves the
js build too.

- [ ] **Step 6: Commit**

```bash
git add cli/snapshot_raw.go cli/snapshot_raw_test.go cmd/harness-cli/session.go
git commit -m "feat(cli): --raw shows PTY bytes and says what it withheld"
```

---

### Task 8: Windows verification and land

**Files:** none — this task is verification.

- [ ] **Step 1: Type-check the Windows legs here**

```bash
GOOS=windows GOARCH=amd64 go vet ./vtgrid/ ./server/ ./cli/
GOOS=windows GOARCH=arm64 go vet ./vtgrid/ ./server/ ./cli/
```

- [ ] **Step 2: Run them on the Windows host**

Hand the branch to the Windows worker (task `38a2aaf5…`, repo
`C:/workspace/agent-harness`) over the agentboard and ask for
`go test ./vtgrid/ ./server/` in a real console. What must pass:
`TestConsoleRepaintReconstructsScreen`, `TestConsoleRepaintLandsOnTheAlternateBuffer`,
`TestPreambleLandsCursorVisibilityOnAConsole` and its control. Note the
pre-existing `TestRunNotifyHook_*` failure on Windows is unrelated and predates
this work.

- [ ] **Step 3: Live check — the two things no test covers**

Attach to a real session from a real terminal and look at both §12 items:
whether the forced `ESC[?1049l` reads as a flash, and how the scrollback seam
immediately above the repaint looks. Record what you saw in the spec, whichever
way it goes — "looked fine" is a finding and belongs in the tree.

- [ ] **Step 4: Land**

```bash
git fetch origin main && git rebase origin/main
make vet && make test && make wasm-check
git merge-base --is-ancestor origin/main HEAD && git push origin HEAD:main
MAIN_WT=$(git worktree list --porcelain | awk '/^worktree /{print $2; exit}')
git -C "$MAIN_WT" fetch origin main -q && git -C "$MAIN_WT" merge --ff-only origin/main
make -C "$MAIN_WT" build
```

- [ ] **Step 5: Restart the fleet**

The server changed, so runners and clients must come back on the new wire.
`scripts/build_and_restart_all.py`. Server first.

---

## Self-Review

**Spec coverage.** §4.1 lifecycle → Task 4 Step 4. §4.2 initial size → Task 4
Step 4. §4.3 feed → Task 4 Step 5. §4.4 resize funnel → Task 4 Step 6 and its
test. §4.5 locking → Task 4 Step 3. §5 replay composition → Task 5. §5.1
unconditional → no task, by construction: nothing in the plan makes it
conditional. §5.2 scrollback → Task 8 Step 3, which is the only way to check it.
§6 Synth → Tasks 1, 2, 5. §6.1 `--raw` → Task 7. §7 modeTracker → no task: its
behaviour is unchanged, and the doc update rides Task 5's commit. §8 alt trim →
Task 6. §9 component table → the tasks' file lists. §10 edge cases → covered by
the tests named in Tasks 4–6, except the reattach-resize case, which is
structural and has no code to write. §11 testing → each task's test step. §12
deferred → Task 8 Step 3 records, does not resolve.

**Gap found and closed:** §7 says `mode_tracker.go`'s doc needs updating for its
narrowed role. Fold that into Task 5 Step 6's commit rather than leaving it
unowned.

**Type consistency.** `screenRepaint()` / `screenSize()` (Task 4) are used in
Tasks 4 and 5 under those names. `OldestIndex()` (Task 6) is used only there.
`encodeSynthFrame` (Task 5) matches `encodeStdoutFrame`'s existing shape.
`Repaint()` (Task 3) is called by `screenRepaint()` in Task 4.

**Known incompleteness, stated rather than hidden:** Task 6 Step 1 names the two
tests and what they must prove but does not write their bodies, because the
eviction arithmetic that puts the ring boundary inside episode E1 depends on
frame sizes the implementer will be looking at. That is the one place this plan
describes a test instead of showing it, and it is called out here rather than
left to be discovered as a placeholder.
