# Session Viewer Grid Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A live, read-only, tmux-style grid of session panes in the TUI and WebUI, each an `AttachViewer` observer of one interactive session, without ever touching the session's PTY size.

**Architecture:** The server already multiplexes N concurrent `AttachMode_View` observers per session; only one server change is needed — fan out mid-stream `TerminalWindowSize` frames to observers (a latent bug fix, since today observers learn the size only once at attach). WebUI generalizes the existing single live-preview engine (`cli/preview_wasm.go` + JS modal) from a singleton to a pane-keyed map, rendering each pane in its own scaled xterm. TUI adds a NEW native continuous VT-emulator pump (does not exist today; native preview was deliberately never built) feeding per-pane `charmbracelet/x/vt` emulators, composited as a normal bubbletea overlay (the `connsModal` template) — NOT `tea.Exec`, which would freeze the Update loop.

**Tech Stack:** Go (server `session_mux`, wasm `cli`/bridge), `charmbracelet/x/vt` (native VT emulator), `charmbracelet/bubbletea` + `lipgloss` (TUI), `charmbracelet/objtrsf/exec` (`CommandExecutionStream`), xterm.js (WebUI panes), Playwright (WebUI verification).

## Global Constraints

- **Work in the harness worktree** `/home/kforfk/workspace/remote-agent-harness/.harness-worktrees/5b65231c141da46b2239912efd108196` on branch `harness/5b65231c141da46b2239912efd108196`. Do NOT write to `/home/kforfk/workspace/remote-agent-harness/<rel>` (that routes to the PARENT checkout). Verify `git rev-parse --abbrev-ref HEAD` before writing. The controller (not the implementer) runs all `git commit`s from the worktree cwd.
- **PTY size is never touched by the grid.** The control client is the sole size authority. No task may send a `TerminalWindowSize` frame from an observer, resize a session's PTY, or aggregate a min-size. Panes render the session's REAL grid locally (WebUI scale, TUI crop).
- **Read-only.** Observer streams use `protocol.AttachMode_View`; no stdin path from a pane.
- **Reuse the long-lived client.** WebUI panes share the one wasm `*cli.Client` via `currentClient()`. TUI grid uses `a.client` threaded through free `Do*`-style helpers, never a fresh Dial.
- **Build hygiene.** Compile-check with `go build ./...` (writes no binary) or `go vet ./cmd/<x>`. NEVER bare `go build ./cmd/<x>/` (drops a stray binary in the worktree). `go test ./...` cleans up after itself.
- **Dark theme, mobile.** WebUI grid matches the `#1e1e1e`/`#d4d4d4` palette; ≤600px collapses to a 1-column stack. Verify desktop + 390px in Playwright.
- **No-size fallback: 80×24**, mirroring `cli/snapshot_native.go`.
- **wasm build tag.** `cli/preview_wasm.go` is `//go:build js`. wasm compile-check: `GOOS=js GOARCH=wasm go build ./cmd/harness-webui-wasm`.

---

## File Structure

- `server/session_mux.go` — add `fanoutToViewersLocked(frame []byte)` helper; call it from `forwardControlFrames` after recording a winsize frame. (Task 1)
- `server/session_mux_winsize_test.go` — add mid-stream fan-out tests. (Task 1)
- `cli/preview_wasm.go` — refactor singleton (`previewStream`/`previewGen`) to a pane-keyed `map[string]*previewSlot`; thread a `paneKey` through `StartPreview`/`StopPreview`/`previewPump`/`previewCall`; pass `paneKey` as first arg to every `harness_preview*` JS hook. (Task 2)
- `cmd/harness-webui-wasm/main.go` — `previewStart`/`previewStop` gain a `paneKey` arg. (Task 2)
- `webui/static/main.js` — arrayify preview state to a per-pane record keyed by `paneKey`; route the four `harness_preview*` hooks by key; add the grid `<dialog>` engine, a `grid` runCmd case, and a task-list entry. (Task 4)
- `webui/index.html` — add the grid `<dialog>`. (Task 4)
- `webui/static/style.css` — grid pane layout (dark, responsive). (Task 4)
- `tui/pane_streamer.go` (new) — native `PaneStreamer`: owns one view-attach stream + one `vt.Emulator`, pumps `Stdout()` into it, tracks a dirty flag, applies mid-stream resize, renders a bottom-left crop. (Task 3)
- `tui/pane_streamer_test.go` (new) — TDD for the crop renderer + resize handling with a fake stream. (Task 3)
- `tui/grid.go` (new) — `GridModel`: `connsModal`-style overlay holding N `*PaneStreamer`, focus movement, Enter=attach, `x`=dismiss. (Task 5)
- `tui/grid_test.go` (new) — GridModel layout/focus tests. (Task 5)
- `tui/app.go` — wire the open key (`g`), `WindowSizeMsg` size propagation, `IsOpen()` intercept, `View()` render, footer hint, pane population from live sessions. (Task 5)

Task dependency order: **Task 1 (server, independent)** → then two independent tracks: WebUI **Task 2 → Task 4**, TUI **Task 3 → Task 5**. Task 1 should land first because Tasks 4 and 5 verify mid-stream resize, which needs the fan-out.

---

## Task 1: Server — mid-stream winsize fan-out to observers

**Files:**
- Modify: `server/session_mux.go` (`forwardControlFrames` ~L465-484; add a helper near the `runnerPump` fan-out ~L245-253)
- Test: `server/session_mux_winsize_test.go`

**Interfaces:**
- Consumes: existing `viewerConn{ch chan []byte}`, `m.viewers map[*viewerConn]struct{}`, `m.mu`, `m.dropViewerLocked`, `frameIsWinSize(fb []byte) bool`.
- Produces: `func (m *SessionMux) fanoutToViewersLocked(fb []byte)` — non-blocking fan-out of one complete frame to every observer, dropping any whose queue is full. Caller must hold `m.mu`.

- [ ] **Step 1: Write the failing test**

Add to `server/session_mux_winsize_test.go`:

```go
// After a viewer is attached, a mid-stream resize from the control client must
// reach the viewer too — not only be recorded in lastWinSize. Without fan-out a
// long-lived observer keeps rendering at the stale attach-time size.
func TestSessionMux_ControlResize_FansOutToViewer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})

	tui := newFakeStream(t)
	if err := mux.Attach(ctx, tui); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	// Establish an initial size and a viewer that consumes the preamble.
	first := makeWinSizeFrame(24, 80)
	tui.QueueRead(first)
	waitFor(t, func() bool {
		mux.mu.Lock()
		defer mux.mu.Unlock()
		return bytes.Equal(mux.lastWinSize, first)
	})
	viewer := newFakeStream(t)
	if err := mux.AttachViewer(ctx, viewer); err != nil {
		t.Fatalf("AttachViewer: %v", err)
	}
	viewer.WaitWritten(t, len(first)) // drain the attach preamble

	// Control client resizes mid-stream. The NEW frame must arrive at the viewer.
	second := makeWinSizeFrame(50, 200)
	tui.QueueRead(second)
	waitFor(t, func() bool { return bytes.Contains(viewer.Written(), second) })
}

// A cowriter is also an observer and must receive the fanned-out resize (so a
// cowriter's local renderer, if any, tracks size) even though its OWN resize is
// still dropped.
func TestSessionMux_ControlResize_FansOutToCoWriter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})

	tui := newFakeStream(t)
	if err := mux.Attach(ctx, tui); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	cw := newFakeStream(t)
	if err := mux.AttachCoWriter(ctx, cw); err != nil {
		t.Fatalf("AttachCoWriter: %v", err)
	}
	resize := makeWinSizeFrame(50, 200)
	tui.QueueRead(resize)
	waitFor(t, func() bool { return bytes.Contains(cw.Written(), resize) })
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./server/ -run 'TestSessionMux_ControlResize_FansOut' -v`
Expected: FAIL — the viewer/cowriter never receive `second`/`resize` (fan-out not wired); `waitFor` times out.

- [ ] **Step 3: Write minimal implementation**

In `server/session_mux.go`, add the helper (place it right after `runnerPump`, near the existing fan-out loop):

```go
// fanoutToViewersLocked delivers one complete frame to every observer
// (viewers AND cowriters, both live in m.viewers) non-blocking, dropping any
// whose bounded queue is full — identical policy to runnerPump's output
// fan-out. Caller MUST hold m.mu. Used to propagate a control-client resize to
// observers so a long-lived read-only renderer tracks the current PTY size
// instead of the stale attach-time size.
func (m *SessionMux) fanoutToViewersLocked(fb []byte) {
	for v := range m.viewers {
		select {
		case v.ch <- fb:
		default:
			m.dropViewerLocked(v)
		}
	}
}
```

Then in `forwardControlFrames`, inside the `if frameIsWinSize(fb) { ... }` block, after recording `m.lastWinSize`, fan out the same frame. The existing block (~L472-476) becomes:

```go
		if frameIsWinSize(fb) {
			cp := append([]byte(nil), fb...)
			m.mu.Lock()
			m.lastWinSize = cp
			m.fanoutToViewersLocked(cp)
			m.mu.Unlock()
		}
```

(Note: `cp` is the copied frame — safe to share across viewer channels since it is never mutated after copy.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./server/ -run 'TestSessionMux' -v`
Expected: PASS — both new tests plus all existing `TestSessionMux_*` (AttachViewer replay, cowriter drop-resize, no-size) stay green.

- [ ] **Step 5: Full server package + vet**

Run: `go test ./server/ && go vet ./server/`
Expected: PASS, no vet complaints.

- [ ] **Step 6: Commit** (controller runs this from the worktree)

```bash
git add server/session_mux.go server/session_mux_winsize_test.go
git commit -m "fix(server): fan out mid-stream winsize to observers"
```

---

## Task 2: WebUI wasm — pane-keyed preview engine

Refactor the single-preview wasm engine to be keyed by a `paneKey` string so N panes can each hold an independent view stream over the one shared client. The existing single preview becomes "one pane keyed `preview`". No new server RPC.

**Files:**
- Modify: `cli/preview_wasm.go` (full rewrite of the singleton state; `//go:build js`)
- Modify: `cmd/harness-webui-wasm/main.go` (`harnessPreviewStart`/`harnessPreviewStop` read a `paneKey` arg)

**Interfaces:**
- Consumes: `c.attachSessionRPC(ctx, taskIDHex, protocol.AttachMode_View)`, `agentexec.NewCommandExecutionStream(st)`, `stream.Stdout() io.Reader`, `stream.LastWindowSize() (rows, cols uint16, ok bool)`, `stream.Close()`.
- Produces:
  - `func (c *Client) StartPreview(ctx context.Context, paneKey, taskIDHex string) error`
  - `func StopPreview(paneKey string)`
  - JS hooks now called as `harness_previewOpen(paneKey, rows, cols, ok)`, `harness_previewWrite(paneKey, u8)`, `harness_previewResize(paneKey, rows, cols)`, `harness_previewClosed(paneKey)`.
  - Bridge: `harness.previewStart(paneKey, taskIDHex) -> Promise<taskIDHex>`, `harness.previewStop(paneKey)`.

- [ ] **Step 1: Rewrite `cli/preview_wasm.go` to a keyed map**

Replace the singleton state and thread `paneKey` everywhere. Full new file:

```go
//go:build js

package cli

import (
	"context"
	"sync"
	"sync/atomic"
	"syscall/js"

	agentexec "github.com/on-keyday/objtrsf/exec"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// previewSlot is one live pane's view stream plus a generation guard, so a
// superseded StartPreview (or a StopPreview) makes the old pump exit silently.
type previewSlot struct {
	stream *agentexec.CommandExecutionStream
	gen    uint64
}

var (
	previewMu    sync.Mutex
	previewSlots = map[string]*previewSlot{}
	previewGen   atomic.Uint64 // monotonic across all panes; each start reserves one
)

// StartPreview view-attaches taskIDHex read-only and pumps its output to the
// JS hooks tagged with paneKey. Reserves a generation BEFORE the attach RPC so
// a StopPreview/replacement landing mid-attach wins. Any existing stream for
// paneKey is closed first.
func (c *Client) StartPreview(ctx context.Context, paneKey, taskIDHex string) error {
	gen := previewGen.Add(1)
	previewMu.Lock()
	if old := previewSlots[paneKey]; old != nil && old.stream != nil {
		_ = old.stream.Close()
	}
	previewSlots[paneKey] = &previewSlot{gen: gen}
	previewMu.Unlock()

	st, _, err := c.attachSessionRPC(ctx, taskIDHex, protocol.AttachMode_View)
	if err != nil {
		return err
	}
	stream := agentexec.NewCommandExecutionStream(st)

	previewMu.Lock()
	slot := previewSlots[paneKey]
	if slot == nil || slot.gen != gen {
		// superseded while attaching — discard.
		previewMu.Unlock()
		_ = stream.Close()
		return nil
	}
	slot.stream = stream
	previewMu.Unlock()

	go previewPump(paneKey, stream, gen)
	return nil
}

// StopPreview closes paneKey's stream (if any) and bumps the guard so its pump
// exits silently. Idempotent.
func StopPreview(paneKey string) {
	gen := previewGen.Add(1)
	previewMu.Lock()
	slot := previewSlots[paneKey]
	delete(previewSlots, paneKey)
	previewMu.Unlock()
	_ = gen // reserving a generation invalidates any in-flight pump for paneKey
	if slot != nil && slot.stream != nil {
		_ = slot.stream.Close()
	}
}

func previewPump(paneKey string, stream *agentexec.CommandExecutionStream, gen uint64) {
	out := stream.Stdout()
	buf := make([]byte, 32*1024)
	opened := false
	lastRows, lastCols := uint16(0), uint16(0)
	for {
		n, err := out.Read(buf)
		if n > 0 {
			rows, cols, ok := stream.LastWindowSize()
			if !opened {
				opened = true
				if !previewCall(paneKey, gen, "harness_previewOpen", int(rows), int(cols), ok) {
					return
				}
				lastRows, lastCols = rows, cols
			} else if ok && (rows != lastRows || cols != lastCols) {
				if !previewCall(paneKey, gen, "harness_previewResize", int(rows), int(cols)) {
					return
				}
				lastRows, lastCols = rows, cols
			}
			u8 := js.Global().Get("Uint8Array").New(n)
			js.CopyBytesToJS(u8, buf[:n])
			if !previewCall(paneKey, gen, "harness_previewWrite", u8) {
				return
			}
		}
		if err != nil {
			previewCall(paneKey, gen, "harness_previewClosed")
			return
		}
	}
}

// previewCall invokes a global JS hook with paneKey as the first arg, gated on
// the pane's generation still being current; a superseded pump no-ops and its
// caller returns.
func previewCall(paneKey string, gen uint64, fn string, args ...any) bool {
	previewMu.Lock()
	slot := previewSlots[paneKey]
	live := slot != nil && slot.gen == gen
	previewMu.Unlock()
	if !live {
		return false
	}
	f := js.Global().Get(fn)
	if f.Type() != js.TypeFunction {
		return true
	}
	all := make([]any, 0, len(args)+1)
	all = append(all, paneKey)
	all = append(all, args...)
	f.Invoke(all...)
	return true
}
```

- [ ] **Step 2: Update the bridge in `cmd/harness-webui-wasm/main.go`**

`harnessPreviewStart` reads two args (`paneKey`, `taskID`); `harnessPreviewStop` reads `paneKey`. The registration keys (`"previewStart"`/`"previewStop"`) stay the same. Replace the two functions:

```go
func harnessPreviewStart(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(_ js.Value, promiseArgs []js.Value) any {
		resolve, reject := promiseArgs[0], promiseArgs[1]
		go func() {
			c, err := currentClient()
			if err != nil {
				reject.Invoke(errObject(err))
				return
			}
			paneKey := args[0].String()
			taskID := args[1].String()
			if err := c.StartPreview(rootCtx, paneKey, taskID); err != nil {
				reject.Invoke(errObject(err))
				return
			}
			resolve.Invoke(taskID)
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

func harnessPreviewStop(this js.Value, args []js.Value) any {
	cli.StopPreview(args[0].String())
	return js.Undefined()
}
```

(Use whatever the existing error-marshal helper is — quote `harnessPreviewStart`'s current reject shape and match it; the map called it `errObject`/`{message}`.)

- [ ] **Step 3: wasm compile-check**

Run: `GOOS=js GOARCH=wasm go build ./cmd/harness-webui-wasm`
Expected: builds clean (no binary in worktree — wasm build to default output is discarded? NO: this writes `harness-webui-wasm`. Use `-o /dev/null`.)

Run: `GOOS=js GOARCH=wasm go build -o /dev/null ./cmd/harness-webui-wasm && GOOS=js GOARCH=wasm go vet ./cli/ ./cmd/harness-webui-wasm`
Expected: PASS, worktree clean (`git status` shows only the two edited files).

- [ ] **Step 4: Build the real wasm artifact** (JS side needs it; hot-reload, no server restart)

Run: `make wasm` (or the repo's documented wasm build target — grep the Makefile for `main.wasm`).
Expected: `webui/static/main.wasm` rebuilt. Confirm the target from `Makefile` before running.

- [ ] **Step 5: Commit**

```bash
git add cli/preview_wasm.go cmd/harness-webui-wasm/main.go webui/static/main.wasm
git commit -m "feat(webui): pane-keyed preview engine (wasm)"
```

---

## Task 3: TUI — native PaneStreamer (view-attach → vt.Emulator pump)

Build the native continuous pump that does not exist today: one view-attach stream feeding one `vt.Emulator`, with a dirty flag for repaint coalescing, mid-stream resize handling, and a bottom-left crop renderer.

**Files:**
- Create: `tui/pane_streamer.go`
- Test: `tui/pane_streamer_test.go`

**Interfaces:**
- Consumes: `c.AttachSession(ctx, taskIDHex, protocol.AttachMode_View) (*agentexec.CommandExecutionStream, uint64, error)`, `vt.NewEmulator(cols, rows int) *vt.Emulator`, `emu.Write`, `emu.Resize(w, h int)`, `emu.CellAt(x, y int) *uv.Cell`, `emu.Close`, `cell.Content string`, `cell.Width int`.
- Produces:
  - `type PaneStreamer struct { ... }`
  - `func NewPaneStreamer(taskID string, defRows, defCols int) *PaneStreamer`
  - `func (p *PaneStreamer) Start(ctx context.Context, c *cli.Client)` — spawns the read pump goroutine.
  - `func (p *PaneStreamer) Stop()` — closes stream + emulator, idempotent.
  - `func (p *PaneStreamer) TakeDirty() bool` — returns true and clears if new bytes arrived since last call (repaint gate).
  - `func (p *PaneStreamer) Render(width, height int) string` — bottom-left crop of the emulator grid to `width`×`height` cells, plain text (no SGR in v1).
  - `func (p *PaneStreamer) TaskID() string`
  - `func (p *PaneStreamer) Err() error` — non-nil once the stream ended (drives the "(stream ended)" pane note).

- [ ] **Step 1: Write the failing test**

`tui/pane_streamer_test.go`:

```go
package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/vt"
)

// bottomLeftCrop renders the bottom-left region: given a 3-row emulator with
// text on the last line, a 1-row crop must show that last line.
func TestPaneStreamer_RenderBottomLeftCrop(t *testing.T) {
	p := &PaneStreamer{emu: vt.NewEmulator(20, 3), cols: 20, rows: 3}
	go drainEmu(p.emu) // mirror the production io.Copy(io.Discard, emu) drain
	p.emu.Write([]byte("top\r\nmid\r\nbottom"))

	out := p.Render(20, 1)
	if !strings.Contains(out, "bottom") {
		t.Fatalf("1-row crop must show the bottom line, got %q", out)
	}
	if strings.Contains(out, "top") {
		t.Fatalf("1-row crop must NOT show the top line, got %q", out)
	}
}

// A crop wider/taller than the grid renders the whole grid without panicking on
// out-of-range CellAt (nil cells render as blanks).
func TestPaneStreamer_RenderLargerThanGrid(t *testing.T) {
	p := &PaneStreamer{emu: vt.NewEmulator(5, 2), cols: 5, rows: 2}
	go drainEmu(p.emu)
	p.emu.Write([]byte("ab\r\ncd"))
	out := p.Render(10, 5) // larger than 5x2
	if !strings.Contains(out, "ab") || !strings.Contains(out, "cd") {
		t.Fatalf("full grid must be visible, got %q", out)
	}
}
```

Add the tiny test helper `drainEmu` in the test file:

```go
func drainEmu(emu *vt.Emulator) {
	buf := make([]byte, 4096)
	for {
		if _, err := emu.Read(buf); err != nil {
			return
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./tui/ -run 'TestPaneStreamer_Render' -v`
Expected: FAIL — `PaneStreamer` / `Render` undefined (compile error).

- [ ] **Step 3: Write minimal implementation**

`tui/pane_streamer.go`:

```go
package tui

import (
	"context"
	"io"
	"strings"
	"sync"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"

	"github.com/on-keyday/agent-harness/cli"
	agentexec "github.com/on-keyday/objtrsf/exec"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// PaneStreamer view-attaches one session read-only and renders its live screen
// into a headless VT emulator, croppable to a pane. It NEVER sends a size — the
// grid has no size authority (Global Constraint) — it sizes its emulator to the
// size the server replays and resizes on mid-stream winsize frames.
type PaneStreamer struct {
	taskID string

	mu     sync.Mutex
	emu    *vt.Emulator
	cols   int
	rows   int
	dirty  bool
	err    error
	stream *agentexec.CommandExecutionStream
	cancel context.CancelFunc
	drainC chan struct{} // closed to stop the emulator drain goroutine
}

func NewPaneStreamer(taskID string, defRows, defCols int) *PaneStreamer {
	if defRows <= 0 {
		defRows = 24
	}
	if defCols <= 0 {
		defCols = 80
	}
	emu := vt.NewEmulator(defCols, defRows)
	return &PaneStreamer{taskID: taskID, emu: emu, cols: defCols, rows: defRows}
}

func (p *PaneStreamer) TaskID() string { return p.taskID }

func (p *PaneStreamer) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// TakeDirty reports whether new bytes arrived since the last call, clearing the
// flag. The grid polls this on a repaint tick to avoid re-rendering idle panes.
func (p *PaneStreamer) TakeDirty() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	d := p.dirty
	p.dirty = false
	return d
}

func (p *PaneStreamer) Start(ctx context.Context, c *cli.Client) {
	cctx, cancel := context.WithCancel(ctx)
	p.mu.Lock()
	p.cancel = cancel
	p.drainC = make(chan struct{})
	drainC := p.drainC
	emu := p.emu
	p.mu.Unlock()

	// x/vt answers DA1/DA2/DSR queries by writing to its own output side; drain
	// and discard or emu.Write blocks forever (cli/snapshot_native.go pattern).
	go func() {
		buf := make([]byte, 4096)
		for {
			select {
			case <-drainC:
				return
			default:
			}
			if _, err := emu.Read(buf); err != nil {
				return
			}
		}
	}()

	go p.pump(cctx, c)
}

func (p *PaneStreamer) pump(ctx context.Context, c *cli.Client) {
	stream, _, err := c.AttachSession(ctx, p.taskID, protocol.AttachMode_View)
	if err != nil {
		p.setErr(err)
		return
	}
	p.mu.Lock()
	p.stream = stream
	p.mu.Unlock()

	out := stream.Stdout()
	buf := make([]byte, 32*1024)
	lastRows, lastCols := 0, 0
	for {
		n, rerr := out.Read(buf)
		if n > 0 {
			rows, cols, ok := stream.LastWindowSize()
			p.mu.Lock()
			if ok && (int(rows) != lastRows || int(cols) != lastCols) && rows > 0 && cols > 0 {
				p.emu.Resize(int(cols), int(rows))
				p.cols, p.rows = int(cols), int(rows)
				lastRows, lastCols = int(rows), int(cols)
			}
			p.emu.Write(buf[:n])
			p.dirty = true
			p.mu.Unlock()
		}
		if rerr != nil {
			p.setErr(rerr)
			return
		}
	}
}

func (p *PaneStreamer) setErr(err error) {
	p.mu.Lock()
	if p.err == nil {
		p.err = err
	}
	p.dirty = true
	p.mu.Unlock()
}

func (p *PaneStreamer) Stop() {
	p.mu.Lock()
	cancel := p.cancel
	stream := p.stream
	emu := p.emu
	drainC := p.drainC
	p.cancel = nil
	p.stream = nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if stream != nil {
		_ = stream.Close()
	}
	if drainC != nil {
		close(drainC)
	}
	if emu != nil {
		_ = emu.Close()
	}
}

// Render returns a bottom-left crop of the emulator grid at width×height cells,
// plain text. Activity in shells and full-screen agents concentrates at the
// bottom, so the bottom rows are the informative ones when a pane is smaller
// than the real grid. Wide (CJK) cells advance the scan by cell.Width.
func (p *PaneStreamer) Render(width, height int) string {
	p.mu.Lock()
	emu := p.emu
	cols, rows := p.cols, p.rows
	p.mu.Unlock()
	if emu == nil || width <= 0 || height <= 0 {
		return ""
	}
	startY := rows - height
	if startY < 0 {
		startY = 0
	}
	var b strings.Builder
	for y := startY; y < rows; y++ {
		if y > startY {
			b.WriteByte('\n')
		}
		x := 0
		painted := 0
		for x < cols && painted < width {
			cell := emu.CellAt(x, y)
			w := cellPaneWidth(cell)
			if cell == nil || cell.Content == "" {
				b.WriteByte(' ')
				painted++
				x++
				continue
			}
			b.WriteString(cell.Content)
			painted += w
			x += w
		}
	}
	return b.String()
}

func cellPaneWidth(cell *uv.Cell) int {
	if cell == nil || cell.Width < 1 {
		return 1
	}
	return cell.Width
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./tui/ -run 'TestPaneStreamer' -v`
Expected: PASS (both render tests).

- [ ] **Step 5: Compile + vet + worktree clean**

Run: `go build ./... && go vet ./tui/ && git status --porcelain`
Expected: builds, vets clean; `git status` shows only the two new tui files (no stray binaries).

- [ ] **Step 6: Commit**

```bash
git add tui/pane_streamer.go tui/pane_streamer_test.go
git commit -m "feat(tui): native PaneStreamer (view-attach into vt emulator with crop render)"
```

---

## Task 4: WebUI JS — pane-keyed hooks + grid modal + entry points

Arrayify the JS preview state to a per-pane record, route the four hooks by `paneKey`, and add the grid `<dialog>` + a `grid` command + a task-list entry. The single-preview modal keeps working by using a fixed key `"preview"`.

**Files:**
- Modify: `webui/static/main.js` (preview state ~L2043-2204; `runCmd` switch ~L1227-1391; task-sheet buttons ~L2441; notify feed ~L1144)
- Modify: `webui/index.html` (add grid dialog near the preview modal ~L201-214)
- Modify: `webui/static/style.css` (grid pane layout)

**Interfaces:**
- Consumes: `window.harness.previewStart(paneKey, taskID) -> Promise`, `window.harness.previewStop(paneKey)`, hooks `harness_previewOpen/Write/Resize/Closed(paneKey, ...)` (from Task 2).
- Produces: `openSessionGrid(ids /* string[] */)`, `closeSessionGrid()`, a `grid <id...>` cmdline case, a "🔲 グリッド" task-list action.

- [ ] **Step 1: Route the four hooks by paneKey (keep single preview working)**

Introduce a pane registry keyed by `paneKey`. Each record holds `{ term, taskId, live, epoch, container, scaleBox, spacer, note }`. The single preview registers under key `"preview"` reusing the existing modal body; grid panes register under `"grid:<taskId>"`.

Replace the four global hooks (`main.js:2150-2172`) so they dispatch by the first arg:

```js
// paneKey -> { term, taskId, live, epoch, bodyEl, scaleBox, spacer, noteEl, onResize }
const previewPanes = new Map();

window.harness_previewOpen = (paneKey, rows, cols, hasSize) => {
  const p = previewPanes.get(paneKey);
  if (!p || !p.live) return;
  const r = hasSize && rows > 0 ? rows : 24;
  const c = hasSize && cols > 0 ? cols : 80;
  buildPaneTerm(p, r, c);
};
window.harness_previewWrite = (paneKey, u8) => {
  const p = previewPanes.get(paneKey);
  if (!p || !p.live || !p.term) return;
  p.term.write(u8);
};
window.harness_previewResize = (paneKey, rows, cols) => {
  const p = previewPanes.get(paneKey);
  if (!p || !p.live || !p.term) return;
  p.term.resize(cols, rows);
  p.onResize && p.onResize();
};
window.harness_previewClosed = (paneKey) => {
  const p = previewPanes.get(paneKey);
  if (!p) return;
  p.live = false;
  paneNote(p, "(ストリーム終了 — ▶ で再接続)");
  if (p.onClosed) p.onClosed();
};
```

`buildPaneTerm(p, rows, cols)` generalizes `buildPreviewTerm` (the map quoted the exact Terminal options + scale divs at `main.js:2108-2145`): create `new Terminal({ cols, rows, disableStdin:true, convertEol:true, fontSize:13, fontFamily:'"Cascadia Mono", ... monospace' })`, `term.open(p.scaleBox's termBox)`, then run the scale-to-fit measuring against `p`'s own container width (not the whole body). Store the scale closure as `p.onResize`. Reuse the exact `fitPreviewScale` math but parameterized on the pane's `avail = p.bodyEl.clientWidth - 12`.

- [ ] **Step 2: Reimplement single preview over the pane registry**

`openSessionPreview(id)` (funnel at `main.js:2174-2179`) now:

```js
function openSessionPreview(id) {
  const key = "preview";
  sessionPreviewModal.querySelector("#session-preview-title").textContent = "プレビュー: " + id;
  const p = {
    taskId: id, live: false, epoch: 0, term: null,
    bodyEl: document.getElementById("session-preview-body"),
    scaleBox: null, spacer: null, noteEl: null, onResize: null,
    onClosed: () => setPreviewPauseLabel(),
  };
  previewPanes.set(key, p);
  sessionPreviewModal.showModal();
  startPane(key, p);
}
```

`startPane(key, p)` generalizes `startSessionPreviewStream` (epoch bump, dispose old term, "connecting…" note, `p.live=true`, `await window.harness.previewStart(key, p.taskId)`, the epoch/live reconciliation from `main.js:2084-2096`). `stopPane(key)` flips `p.live=false` then `window.harness.previewStop(key)`. Pause/close/reattach handlers call `stopPane("preview")`. The modal `close` handler also `previewPanes.delete("preview")`.

- [ ] **Step 3: Add the grid dialog to `webui/index.html`**

After the preview modal (`index.html:214`):

```html
<dialog id="session-grid-modal" class="preview-modal grid-modal">
  <div class="preview-header">
    <span id="session-grid-title">セッショングリッド</span>
    <button id="session-grid-close" class="preview-btn" autofocus>✕</button>
  </div>
  <div id="session-grid-body" class="grid-body"></div>
</dialog>
```

- [ ] **Step 4: Grid engine in `main.js`**

```js
const gridModal = document.getElementById("session-grid-modal");
const gridBody = document.getElementById("session-grid-body");
let gridKeys = [];

function openSessionGrid(ids) {
  closeSessionGrid();
  gridBody.innerHTML = "";
  gridKeys = [];
  const capped = ids.slice(0, 9); // pane cap (spec: v1 cap 9)
  for (const id of capped) {
    const key = "grid:" + id;
    const cell = document.createElement("div");
    cell.className = "grid-cell";
    const head = document.createElement("div");
    head.className = "grid-cell-head";
    const label = document.createElement("span");
    label.textContent = id.slice(0, 8);
    const attach = document.createElement("button");
    attach.className = "preview-btn";
    attach.textContent = "↪";
    attach.title = "リアタッチ";
    attach.addEventListener("click", () => { closeSessionGrid(); reattachTo(id); });
    const dismiss = document.createElement("button");
    dismiss.className = "preview-btn";
    dismiss.textContent = "✕";
    dismiss.addEventListener("click", () => dismissPane(key, cell));
    head.append(label, attach, dismiss);
    const body = document.createElement("div");
    body.className = "grid-cell-body";
    cell.append(head, body);
    gridBody.append(cell);

    const p = { taskId: id, live: false, epoch: 0, term: null, bodyEl: body,
                scaleBox: null, spacer: null, noteEl: null, onResize: null };
    previewPanes.set(key, p);
    gridKeys.push(key);
    startPane(key, p);
  }
  gridModal.showModal();
}

function dismissPane(key, cell) {
  stopPane(key);
  previewPanes.delete(key);
  gridKeys = gridKeys.filter((k) => k !== key);
  cell.remove();
}

function closeSessionGrid() {
  for (const key of gridKeys) { stopPane(key); previewPanes.delete(key); }
  gridKeys = [];
  if (gridModal.open) gridModal.close();
}

gridModal.addEventListener("close", closeSessionGrid);
document.getElementById("session-grid-close").addEventListener("click", () => gridModal.close());
```

- [ ] **Step 5: Entry points — cmdline `grid` + task-list button**

In `runCmd`'s switch (`main.js:1227-1391`), add a case (mirror the `preview` case at `:1318-1322`):

```js
case "grid": {
  // grid with explicit ids, else all live interactive sessions.
  let ids = tokens.slice(1);
  if (ids.length === 0) {
    ids = (lastSnapshot?.tasks || [])
      .filter((t) => t.kind === "Interactive" && (t.status === "Running" || t.status === "Detached"))
      .sort((a, b) => (b.lastActivityUnixNano || 0) - (a.lastActivityUnixNano || 0))
      .map((t) => t.id);
  }
  if (ids.length === 0) { appendResult("grid: no live interactive sessions"); break; }
  openSessionGrid(ids);
  break;
}
```

(Match the real snapshot field names — grep `lastSnapshot`/`refreshSnapshot` for the task shape and activity field; the map noted activity-desc sorting already exists for the task list.) Add a help line next to the other command help (`main.js` ~L1366): `grid [id...] — live monitor grid of sessions`.

Add a task-list action button where the preview button is added (`main.js:2441`, gated identically on live interactive): `addItem("🔲 グリッド", "", () => openSessionGrid([t.id]))` — opens a 1-pane grid centered on that task (a convenience entry; the full grid is via the `grid` command).

- [ ] **Step 6: CSS in `webui/static/style.css`**

```css
.grid-modal { width: 96vw; max-width: 1600px; height: 90vh; }
.grid-body {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(320px, 1fr));
  gap: 8px;
  overflow: auto;
  padding: 8px;
  background: #1e1e1e;
}
.grid-cell { border: 1px solid #333; background: #1e1e1e; display: flex; flex-direction: column; }
.grid-cell-head { display: flex; align-items: center; gap: 6px; padding: 2px 6px; color: #d4d4d4; background: #252526; font-size: 12px; }
.grid-cell-head span { flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.grid-cell-body { overflow: hidden; min-height: 120px; }
@media (max-width: 600px) {
  .grid-body { grid-template-columns: 1fr; }
  .grid-modal { width: 100vw; height: 100vh; max-width: none; }
}
```

- [ ] **Step 7: Verify in a real browser (Playwright)** — no server restart (wasm hot-reloads; browser refresh only)

Drive per `project_playwright_webui_visual_check`: open the WebUI (URL from `HARNESS_SERVER_CID`), start ≥2 sandbox interactive sessions (resume bash-runner tasks are cheapest), run `grid` in the command input. Assert (via `browser_snapshot`, since xterm is DOM-rendered):
- (a) both panes render their session screens with no manual refresh;
- (b) `echo GRIDMARK` in one session's main terminal appears in that pane;
- (c) resize a session by attaching from a different-sized terminal → pane re-renders without corruption (this exercises Task 1's fan-out);
- (d) 390px viewport (`browser_resize`) → 1-column stack;
- (e) single-preview still works (open 🔍 プレビュー, liveness intact).

- [ ] **Step 8: Commit**

```bash
git add webui/static/main.js webui/index.html webui/static/style.css
git commit -m "feat(webui): live session viewer grid + pane-keyed preview routing"
```

---

## Task 5: TUI — GridModel overlay + app wiring

Add a full-screen bubbletea overlay (the `connsModal` template) tiling N `PaneStreamer`s, with focus movement, Enter=attach, `x`=dismiss, and a repaint tick.

**Files:**
- Create: `tui/grid.go`
- Test: `tui/grid_test.go`
- Modify: `tui/app.go` (App struct field; `WindowSizeMsg` ~L656-663; `tea.KeyMsg` open key + intercept ~L665-886; `View()` render ~L1279-1302; footer hint ~L1266; a repaint tick)

**Interfaces:**
- Consumes: `NewPaneStreamer`, `(*PaneStreamer).Start/Stop/TakeDirty/Render/TaskID/Err`, `a.client *cli.Client`, `a.program *tea.Program`, `protocol.TaskInfo` from `a.tasksByID`, `DoAttachSession(a.client, id, protocol.AttachMode_View)` for Enter, `lipgloss.JoinHorizontal/JoinVertical`, `PanelStyle`/`PanelStyleFocused`, `lipgloss.Place`.
- Produces:
  - `type GridModel struct { ... }`
  - `func NewGridModel() GridModel`
  - `func (m *GridModel) Open(ctx context.Context, c *cli.Client, program *tea.Program, tasks []protocol.TaskInfo)` — creates ≤9 panes from live interactive sessions (activity-desc), starts them.
  - `func (m *GridModel) Close()` — stops all panes.
  - `func (m GridModel) IsOpen() bool`
  - `func (m *GridModel) SetSize(w, h int)`
  - `func (m GridModel) Update(msg tea.Msg) (GridModel, tea.Cmd)` — focus keys (h/j/k/l + arrows), `x` dismiss focused, Esc/`q` close, Enter returns an attach cmd.
  - `func (m GridModel) View() string` — tiled panes, focused pane bordered `PanelStyleFocused`.
  - `func (m GridModel) FocusedTaskID() string`
  - A `gridTickMsg` + `gridTick() tea.Cmd` repaint pump (~10 Hz) that re-renders only when some pane `TakeDirty()` is true.

- [ ] **Step 1: Write the failing test**

`tui/grid_test.go`:

```go
package tui

import (
	"strings"
	"testing"
)

func TestGridModel_LayoutAndFocusMove(t *testing.T) {
	m := NewGridModel()
	// Inject panes directly (no live streams needed for layout).
	m.panes = []*PaneStreamer{
		NewPaneStreamer("aaaaaaaa", 24, 80),
		NewPaneStreamer("bbbbbbbb", 24, 80),
		NewPaneStreamer("cccccccc", 24, 80),
	}
	m.open = true
	m.SetSize(120, 40)

	if got := m.FocusedTaskID(); got != "aaaaaaaa" {
		t.Fatalf("initial focus should be first pane, got %q", got)
	}
	// Move focus right/down; with 3 panes and a 2-col grid, "l" then "j".
	m2, _ := m.Update(keyMsg("l"))
	if m2.FocusedTaskID() != "bbbbbbbb" {
		t.Fatalf("after 'l' focus should be pane 2, got %q", m2.FocusedTaskID())
	}
	view := m2.View()
	if !strings.Contains(view, "aaaaaaa") || !strings.Contains(view, "bbbbbbb") {
		t.Fatalf("view must show pane task-id labels, got:\n%s", view)
	}
}

func TestGridModel_DismissFocusedPane(t *testing.T) {
	m := NewGridModel()
	m.panes = []*PaneStreamer{
		NewPaneStreamer("aaaaaaaa", 24, 80),
		NewPaneStreamer("bbbbbbbb", 24, 80),
	}
	m.open = true
	m.SetSize(120, 40)
	m2, _ := m.Update(keyMsg("x"))
	if len(m2.panes) != 1 {
		t.Fatalf("dismiss should drop the focused pane, have %d", len(m2.panes))
	}
	if m2.FocusedTaskID() != "bbbbbbbb" {
		t.Fatalf("after dismissing pane 1, focus should land on pane 2, got %q", m2.FocusedTaskID())
	}
}
```

Add the `keyMsg` helper if the package doesn't already have one (grep `tui/*_test.go` — `app_act_test.go` likely has one; reuse it, don't redefine):

```go
// only if not already defined in the package's test files:
func keyMsg(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./tui/ -run 'TestGridModel' -v`
Expected: FAIL — `GridModel` undefined.

- [ ] **Step 3: Write minimal implementation**

`tui/grid.go`:

```go
package tui

import (
	"context"
	"fmt"
	"sort"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

const gridMaxPanes = 9

type gridTickMsg struct{}

func gridTick() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg { return gridTickMsg{} })
}

type GridModel struct {
	open    bool
	width   int
	height  int
	panes   []*PaneStreamer
	focus   int
	cols    int // computed pane columns for the current size
	ctx     context.Context
	client  *cli.Client
	program *tea.Program
}

func NewGridModel() GridModel { return GridModel{} }

func (m GridModel) IsOpen() bool { return m.open }

func (m *GridModel) SetSize(w, h int) {
	m.width, m.height = w, h
	m.cols = gridCols(len(m.panes))
}

// gridCols picks a column count so panes stay roughly square-ish: 1 for ≤1,
// 2 for ≤4, 3 for ≤9.
func gridCols(n int) int {
	switch {
	case n <= 1:
		return 1
	case n <= 4:
		return 2
	default:
		return 3
	}
}

func (m *GridModel) Open(ctx context.Context, c *cli.Client, program *tea.Program, tasks []protocol.TaskInfo) {
	live := make([]protocol.TaskInfo, 0, len(tasks))
	for _, t := range tasks {
		if t.Kind == protocol.TaskKind_Interactive &&
			(t.Status == protocol.TaskStatus_Running || t.Status == protocol.TaskStatus_Detached) {
			live = append(live, t)
		}
	}
	sort.Slice(live, func(i, j int) bool {
		return live[i].LastActivityUnixNano > live[j].LastActivityUnixNano
	})
	if len(live) > gridMaxPanes {
		live = live[:gridMaxPanes]
	}
	m.panes = m.panes[:0]
	for _, t := range live {
		p := NewPaneStreamer(t.IdHex(), 24, 80)
		p.Start(ctx, c)
		m.panes = append(m.panes, p)
	}
	m.open = true
	m.focus = 0
	m.ctx, m.client, m.program = ctx, c, program
	m.cols = gridCols(len(m.panes))
}

func (m *GridModel) Close() {
	for _, p := range m.panes {
		p.Stop()
	}
	m.panes = nil
	m.open = false
	m.focus = 0
}

func (m GridModel) FocusedTaskID() string {
	if m.focus < 0 || m.focus >= len(m.panes) {
		return ""
	}
	return m.panes[m.focus].TaskID()
}

func (m GridModel) Update(msg tea.Msg) (GridModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "esc", "q":
			m.Close()
			return m, nil
		case "enter":
			if id := m.FocusedTaskID(); id != "" && m.client != nil {
				cmd := DoAttachSession(m.client, id, protocol.AttachMode_View)
				m.Close()
				return m, cmd
			}
			return m, nil
		case "x":
			m.dismissFocused()
			return m, nil
		case "left", "h":
			m.moveFocus(-1)
		case "right", "l":
			m.moveFocus(1)
		case "up", "k":
			m.moveFocus(-m.cols)
		case "down", "j":
			m.moveFocus(m.cols)
		}
	case gridTickMsg:
		if !m.open {
			return m, nil
		}
		return m, gridTick()
	}
	return m, nil
}

func (m *GridModel) moveFocus(delta int) {
	if len(m.panes) == 0 {
		return
	}
	f := m.focus + delta
	if f < 0 {
		f = 0
	}
	if f >= len(m.panes) {
		f = len(m.panes) - 1
	}
	m.focus = f
}

func (m *GridModel) dismissFocused() {
	if m.focus < 0 || m.focus >= len(m.panes) {
		return
	}
	m.panes[m.focus].Stop()
	m.panes = append(m.panes[:m.focus], m.panes[m.focus+1:]...)
	if m.focus >= len(m.panes) {
		m.focus = len(m.panes) - 1
	}
	if m.focus < 0 {
		m.focus = 0
	}
	m.cols = gridCols(len(m.panes))
}

func (m GridModel) View() string {
	if len(m.panes) == 0 {
		return PanelStyleFocused.Padding(0, 1).Render("グリッド: ライブなインタラクティブセッションなし (Esc で閉じる)")
	}
	cols := m.cols
	rows := (len(m.panes) + cols - 1) / cols
	// Cell interior size, minus borders (2) and header line (1).
	cellW := m.width/cols - 2
	cellH := m.height/rows - 3
	if cellW < 8 {
		cellW = 8
	}
	if cellH < 2 {
		cellH = 2
	}
	var rowsOut []string
	for r := 0; r < rows; r++ {
		var cells []string
		for c := 0; c < cols; c++ {
			idx := r*cols + c
			if idx >= len(m.panes) {
				break
			}
			cells = append(cells, m.renderPane(idx, cellW, cellH))
		}
		rowsOut = append(rowsOut, lipgloss.JoinHorizontal(lipgloss.Top, cells...))
	}
	return lipgloss.JoinVertical(lipgloss.Left, rowsOut...)
}

func (m GridModel) renderPane(idx, w, h int) string {
	p := m.panes[idx]
	head := p.TaskID()
	if len(head) > 8 {
		head = head[:8]
	}
	if err := p.Err(); err != nil {
		head += " (終了)"
	}
	body := p.Render(w, h)
	style := PanelStyle
	if idx == m.focus {
		style = PanelStyleFocused
	}
	return style.Width(w).Height(h + 1).Render(head + "\n" + body)
}
```

Notes for the implementer:
- `IdHex()`, `LastActivityUnixNano`, `TaskKind_Interactive`, `TaskStatus_Running/Detached` — confirm the EXACT `protocol.TaskInfo` field/method names by grepping `runner/protocol` and existing `tui/` usage (the task list already reads status + activity; copy its accessors). If `IdHex()` isn't the accessor, use whatever the task list uses to get the hex id.
- The tick is started when the grid opens (Step 4 wires `gridTick()` into the open path). The View re-renders every frame; `TakeDirty` is an optimization — for v1 correctness, re-rendering each tick is acceptable and simpler; keep `TakeDirty` on `PaneStreamer` for a later optimization but the grid may ignore it in v1. (Decision: v1 re-renders each tick; do NOT gate on TakeDirty yet — simpler and 10 Hz over ≤9 small crops is cheap.)

- [ ] **Step 4: Wire into `tui/app.go`**

1. Add field to `App` struct (near `connsModal ConnsModal`): `grid GridModel`.
2. Initialize in the App constructor: `grid: NewGridModel(),` (match how `connsModal` is initialized — grep the constructor).
3. `WindowSizeMsg` handler (~L656-663): add `a.grid.SetSize(a.width, a.height)` alongside the other modals.
4. Open key — in the `tea.KeyMsg` global-keys section (mirror the `C`/connsModal open at ~L878-886), add:

```go
if a.focus != focusCmdline && !logsEditing && msg.String() == "g" {
	if a.client == nil {
		a.cmdresult.Append(WarnStyle.Render("grid: not connected"))
		return a, nil
	}
	a.grid.Open(a.appCtx, a.client, a.program, a.snapshotTasks())
	a.grid.SetSize(a.width, a.height)
	return a, gridTick()
}
```

(`a.snapshotTasks()` = whatever slice of `protocol.TaskInfo` the app holds — likely derived from `a.tasksByID` or `a.tasks`. Grep how `DoConnSnapshot`/task list gets the task slice and reuse it. If tasks are only in `a.tasksByID map[string]protocol.TaskInfo`, build the slice inline.)

5. Intercept-while-open — at the TOP of the `tea.KeyMsg` case, before global keys (mirror connsModal ~L676-684):

```go
if a.grid.IsOpen() {
	var cmd tea.Cmd
	a.grid, cmd = a.grid.Update(msg)
	return a, cmd
}
```

6. Also forward `gridTickMsg` to the grid in `Update` (add a top-level case so the tick keeps firing):

```go
case gridTickMsg:
	if a.grid.IsOpen() {
		var cmd tea.Cmd
		a.grid, cmd = a.grid.Update(msg)
		return a, cmd
	}
	return a, nil
```

7. Render — in `View()` if-ladder (mirror connsModal ~L1297-1299), BEFORE the base return:

```go
if a.grid.IsOpen() {
	return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.grid.View())
}
```

8. Footer hint (~L1266): add ` g:grid` to the key hint string.

- [ ] **Step 5: Run tests + full build**

Run: `go test ./tui/ -run 'TestGridModel|TestPaneStreamer' -v && go build ./... && go vet ./tui/`
Expected: PASS, clean build/vet.

- [ ] **Step 6: Run the full TUI test suite (guard against layout invariant regressions)**

Run: `go test ./tui/`
Expected: PASS — including `layout_test.go`'s height invariant (the grid renders via `lipgloss.Place` sized to exactly `width×height`, so it satisfies it).

- [ ] **Step 7: Manual TUI verification** (nested run, per `feedback_verify_interactive_input_not_just_render`)

Build the TUI (`make build` or `go build -o bin/harness-tui ./cmd/harness-tui`), start ≥2 sandbox interactive sessions, launch the TUI, press `g`. Confirm: panes show live session screens; `echo TUIMARK` in a session appears in its pane (crop shows bottom); h/j/k/l moves the focus border; `x` dismisses a pane; Enter view-attaches the focused session; Esc/`q` returns to the task list. Feed real keystrokes — assert the grid responds, not just that it rendered once.

- [ ] **Step 8: Commit**

```bash
git add tui/grid.go tui/grid_test.go tui/app.go
git commit -m "feat(tui): live session viewer grid overlay (g)"
```

---

## Final integration verification (controller, after all tasks)

- [ ] `make check` (or the repo's aggregate target — confirm from `Makefile`) + `GOOS=js GOARCH=wasm go build -o /dev/null ./cmd/harness-webui-wasm`.
- [ ] `git status --porcelain` shows no stray binaries; only the intended files changed.
- [ ] Operator-surface note: CLI grid is an intentional non-goal (a grid is a full-screen display; CLI keeps `session snapshot` / `attach --view`). Documented in the spec's Non-goals — no CLI task.
- [ ] Land per Mode A (landing-to-main): FF-push `harness/5b65…` to `origin/main`, advance local main, then `make build` in the main checkout.

---

## Self-Review

**Spec coverage:**
- Window-size model (PTY untouched; WebUI scale / TUI crop / 80×24 fallback) → Tasks 3 (crop + fallback in `NewPaneStreamer`), 4 (scale + fallback in hooks).
- Server winsize fan-out (latent-bug fix + prerequisite) → Task 1, with verification (b) re-checking the pre-existing surfaces.
- WebUI grid (pane-keyed engine, shared client, cap 9, per-pane dismiss, tap→reattach, stream-death note, dark/390px) → Tasks 2 + 4.
- TUI grid (normal overlay not tea.Exec, per-pane vt.Emulator, bottom-left crop, focus/Enter/x/q, reuse a.client) → Tasks 3 + 5.
- Call-site enumeration (attach --view / TUI v / WebUI preview / cowrite / grids receive fanned-out winsize) → Task 1 tests cover viewer + cowriter; Task 4 verify (c) and Task 5 verify (7) exercise the grids; existing surfaces are unchanged consumers that now simply receive the extra frame.
- Non-goals (no CLI grid, no PTY resize, no server-side render, no pane input) → enforced by Global Constraints; no task violates them.

**Placeholder scan:** No "TBD"/"implement later". Two explicit "confirm the exact field/accessor name by grepping" notes (protocol.TaskInfo accessors, the error-marshal helper, the snapshot task field) are verification instructions, not deferred decisions — the surrounding code is fully specified and the names are the only repo-specific unknowns an implementer must confirm against real symbols.

**Type consistency:** `paneKey` string threads identically through StartPreview/StopPreview/previewPump/previewCall (Task 2) and the JS hooks + registry (Task 4). `PaneStreamer` method set (Start/Stop/TakeDirty/Render/TaskID/Err) is defined in Task 3 and consumed unchanged in Task 5. `GridModel` method set matches between grid.go and the app.go wiring. `DoAttachSession(c, id, protocol.AttachMode_View)` is the existing signature reused in both Task 5 Enter and the current `v` key.
