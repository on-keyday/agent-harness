# WebUI Live Session Preview (pause/resume) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Swap the session-preview modal's one-shot snapshot engine for a live view-attach stream with a ⏸/▶ pause/resume control; closing the modal disconnects immediately.

**Architecture:** A wasm-side preview singleton (separate from the interactive singleton) holds a view-mode attach wrapped in objtrsf's `CommandExecutionStream`; its pump goroutine feeds JS hooks (`harness_previewOpen/Write/Resize/Closed`) guarded by a generation counter. Pause = close the stream (frozen frame stays); resume = fresh attach whose ring replay reconstructs the current screen. The one-shot `harness.sessionPreview` bridge and JS render path are removed.

**Tech Stack:** Go (wasm, `syscall/js`), objtrsf `exec.CommandExecutionStream`, vanilla JS, xterm.js.

**Spec:** `docs/superpowers/specs/2026-07-18-webui-live-session-preview-design.md` — read the Problem AND Decisions-taken sections, not just Mechanism.

## Global Constraints

- **Worktree only**: ALL file operations use absolute paths under
  `/home/kforfk/workspace/remote-agent-harness/.harness-worktrees/5b65231c141da46b2239912efd108196/`.
  NEVER touch `/home/kforfk/workspace/remote-agent-harness/<rel>` (routes to the parent checkout). Before the first edit run `git rev-parse --abbrev-ref HEAD` from the worktree and confirm `harness/5b65231c141da46b2239912efd108196`; abort if not.
- **Read `.claude/skills/implementation-pitfalls/SKILL.md` in full before writing code** (worktree copy).
- **Build hygiene**: compile-check ONLY with `go build ./...` / `GOOS=js GOARCH=wasm go build ./cli/... ./cmd/harness-webui-wasm/`. NEVER bare `go build ./cmd/<x>/`. `make webui-build` writing `webui/static/main.wasm` is fine; never commit `main.wasm`; restore `webui/static/wasm_exec.js` via `git checkout --` if only version-refreshed.
- **Public repo**: no LAN IPs, internal hostnames, or private paths anywhere.
- **No server/wire changes**: nothing under `server/`, `runner/`, or any `.bgn`.
- **The preview must never touch `activeInteractiveSession` / `installAndPumpSession` / `interactiveGen`** — the main terminal keeps working during a preview.
- **Preview stays read-only**: no stdin path to the preview stream.

---

### Task 1: wasm — preview stream singleton + previewStart/previewStop bridge (one-shot bridge removed)

**Files:**
- Create: `cli/preview_wasm.go`
- Modify: `cmd/harness-webui-wasm/main.go` (replace the `sessionPreview` registration with `previewStart`/`previewStop`; delete `harnessSessionPreview`; add the two new bridge funcs)

**Interfaces:**
- Consumes: existing `(c *Client).attachSessionRPC(ctx, taskIDHex, protocol.AttachMode_View)`, `agentexec.NewCommandExecutionStream` (js-buildable since the objtrsf exec_stream split), `currentClient()`, `rootCtx`, `rejectErr`.
- Produces (Task 2 relies on these exact names):
  - `harness.previewStart(taskIDHex) -> Promise<taskIDHex>` (rejects `{message}` on attach failure)
  - `harness.previewStop()` (synchronous, idempotent)
  - JS hooks invoked by the pump: `harness_previewOpen(rows, cols, hasSize)` once per stream before the first write; `harness_previewWrite(Uint8Array)`; `harness_previewResize(rows, cols)` on mid-stream size change; `harness_previewClosed()` on stream end.
  - Go: `func (c *Client) StartPreview(ctx context.Context, taskIDHex string) error`, `func StopPreview()` in package cli.

- [ ] **Step 1: Create `cli/preview_wasm.go`**

```go
//go:build js

package cli

import (
	"context"
	"sync"
	"sync/atomic"
	"syscall/js"

	"github.com/on-keyday/agent-harness/runner/protocol"
	agentexec "github.com/on-keyday/objtrsf/exec"
)

// The preview singleton: a read-only AttachMode_View stream feeding the
// WebUI's session-preview modal, fully independent of the interactive
// singleton (activeInteractiveSession) so a preview can never disturb the
// main terminal. previewGen mirrors the interactiveGen pattern: every
// Start/Stop bumps it, and a pump whose generation is stale stops invoking
// JS hooks and exits. Pause in the UI is just StopPreview (the frozen xterm
// stays client-side); resume is a fresh StartPreview whose ring replay
// reconstructs the current screen — no bytes are buffered while paused.
var (
	previewMu     sync.Mutex
	previewStream *agentexec.CommandExecutionStream
	previewGen    atomic.Uint64
)

// StartPreview view-attaches to taskIDHex and starts pumping its output to
// the harness_preview* JS hooks. Any previous preview stream is superseded
// and closed first. The attach uses AttachMode_View, so it never takes over
// the session's controlling client.
func (c *Client) StartPreview(ctx context.Context, taskIDHex string) error {
	st, _, err := c.attachSessionRPC(ctx, taskIDHex, protocol.AttachMode_View)
	if err != nil {
		return err
	}
	stream := agentexec.NewCommandExecutionStream(st)

	previewMu.Lock()
	old := previewStream
	gen := previewGen.Add(1)
	previewStream = stream
	previewMu.Unlock()
	if old != nil {
		_ = old.Close()
	}

	go previewPump(stream, gen)
	return nil
}

// StopPreview closes the current preview stream, if any. Idempotent. The
// generation bump silences the pump's remaining callbacks immediately, so
// JS sees no harness_previewClosed for a locally-initiated stop.
func StopPreview() {
	previewMu.Lock()
	old := previewStream
	previewStream = nil
	previewGen.Add(1)
	previewMu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

// previewPump reads the view stream and forwards it to the JS hooks. The
// size control frame the server replays ahead of the ring is parsed by the
// CommandExecutionStream demux before the first Stdout bytes become
// readable, so LastWindowSize at the first successful read is the replayed
// size; harness_previewOpen fires exactly once, before the first write.
// Mid-stream size changes surface as harness_previewResize. All hooks are
// generation-gated; a raced late invoke after StopPreview is additionally
// ignored by the JS side's modal-open/live flags.
func previewPump(stream *agentexec.CommandExecutionStream, gen uint64) {
	defer stream.Close()
	out := stream.Stdout()
	buf := make([]byte, 32*1024)
	opened := false
	var lastRows, lastCols uint16
	for {
		n, err := out.Read(buf)
		if n > 0 {
			rows, cols, ok := stream.LastWindowSize()
			if !opened {
				opened = true
				lastRows, lastCols = rows, cols
				if !previewCall(gen, "harness_previewOpen", int(rows), int(cols), ok) {
					return
				}
			} else if ok && rows > 0 && cols > 0 && (rows != lastRows || cols != lastCols) {
				lastRows, lastCols = rows, cols
				if !previewCall(gen, "harness_previewResize", int(rows), int(cols)) {
					return
				}
			}
			arr := js.Global().Get("Uint8Array").New(n)
			js.CopyBytesToJS(arr, buf[:n])
			if !previewCall(gen, "harness_previewWrite", arr) {
				return
			}
		}
		if err != nil {
			previewCall(gen, "harness_previewClosed")
			return
		}
	}
}

// previewCall invokes the named JS hook iff gen is still the current
// generation; returns false when superseded so the pump exits silently. A
// missing hook (non-WebUI wasm host) is a no-op that keeps the pump alive.
func previewCall(gen uint64, fn string, args ...any) bool {
	if previewGen.Load() != gen {
		return false
	}
	f := js.Global().Get(fn)
	if f.Type() == js.TypeFunction {
		f.Invoke(args...)
	}
	return true
}
```

- [ ] **Step 2: Replace the bridge registration and function in `cmd/harness-webui-wasm/main.go`**

In the `js.Global().Set("harness", ...)` map, DELETE the line

```go
		"sessionPreview":     js.FuncOf(harnessSessionPreview),
```

and add (keeping the map's alignment style):

```go
		"previewStart":       js.FuncOf(harnessPreviewStart),
		"previewStop":        js.FuncOf(harnessPreviewStop),
```

DELETE the whole `harnessSessionPreview` function (grep for it; it is the
one whose doc comment mentions `settleMs`). Add in its place:

```go
// harnessPreviewStart opens a LIVE read-only preview of a detachable
// interactive session: AttachMode_View (non-takeover), independent of the
// activeInteractiveSession singleton. Output flows via the JS hooks
// harness_previewOpen / harness_previewWrite / harness_previewResize /
// harness_previewClosed until harness.previewStop() or a fresh
// previewStart supersedes it.
//
//	harness.previewStart(taskIDHex) -> Promise<taskIDHex>
func harnessPreviewStart(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			if len(args) < 1 {
				rejectErr(reject, errors.New("previewStart: missing taskIDHex arg"))
				return
			}
			taskID := args[0].String()
			if err := c.StartPreview(rootCtx, taskID); err != nil {
				rejectErr(reject, err)
				return
			}
			resolve.Invoke(taskID)
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessPreviewStop tears down the live preview stream, if any.
// Synchronous and idempotent; a paused/never-started preview is a no-op.
//
//	harness.previewStop()
func harnessPreviewStop(this js.Value, args []js.Value) any {
	cli.StopPreview()
	return js.Undefined()
}
```

(Check the file's import alias for the cli package — it imports
`github.com/on-keyday/agent-harness/cli`; call `cli.StopPreview()` matching
however other free functions like `cli.SendInteractive` are referenced.)

- [ ] **Step 3: Verify builds and tests**

```bash
go build ./... && GOOS=js GOARCH=wasm go build ./cli/... ./cmd/harness-webui-wasm/ && go test ./cli/... && gofmt -l cli/preview_wasm.go cmd/harness-webui-wasm/main.go
```

Expected: builds pass, tests pass, gofmt prints nothing. `git status --short` shows only the two intended files.

- [ ] **Step 4: Commit**

```bash
git add cli/preview_wasm.go cmd/harness-webui-wasm/main.go
git commit -m "feat(wasm): live preview stream — previewStart/previewStop replace one-shot sessionPreview"
```

---

### Task 2: WebUI — modal engine swap to live + ⏸/▶

**Files:**
- Modify: `webui/index.html` (rename the refresh button to a pause button)
- Modify: `webui/static/main.js` (replace the one-shot engine block; entry points stay)

**Interfaces:**
- Consumes: Task 1's `harness.previewStart/previewStop` and the four `harness_preview*` hooks, exactly as specified there.
- Produces: `openSessionPreview(id)` keeps its existing name/signature (the three entry points — task sheet, notify feed, cmdline `preview` — remain untouched).

- [ ] **Step 1: `webui/index.html` — swap the 🔄 button for ⏸/▶**

Replace

```html
    <button id="session-preview-refresh" class="preview-toggle" title="再取得">🔄</button>
```

with

```html
    <button id="session-preview-pause" class="preview-toggle" title="一時停止">⏸</button>
```

Also update the dialog's leading HTML comment from "one-shot read-only
screen snapshot" to "live read-only view (⏸ pauses by disconnecting; ▶
re-attaches and jumps to the current screen)".

- [ ] **Step 2: `webui/static/main.js` — replace the preview engine block**

The block starts at the `// --- Session preview modal: one-shot view-attach snapshot…` comment (~line 2012) and ends just before the `// renderTaskList builds clickable task rows…` comment (~line 2105). Replace that whole block (comment through the `sessionPreviewReattach` listener inclusive) with:

```js
  // --- Session preview modal: LIVE read-only view of an interactive
  //     session. A view-mode attach stream stays open while the modal shows;
  //     bytes flow into a throwaway xterm sized to the session's real grid
  //     and CSS-scaled to fit. ⏸ pauses by CLOSING the stream (the frozen
  //     frame stays; zero load while paused); ▶ re-attaches — the server's
  //     ring replay reconstructs the current screen, so resume jumps to now.
  //     Closing the modal disconnects immediately. View mode never takes
  //     over the controlling client, and the stream is independent of the
  //     main terminal's singleton, so peeking is always safe — including at
  //     the session currently attached here.
  const sessionPreviewModal    = document.getElementById("session-preview-modal");
  const sessionPreviewTitle    = document.getElementById("session-preview-title");
  const sessionPreviewBody     = document.getElementById("session-preview-body");
  const sessionPreviewPause    = document.getElementById("session-preview-pause");
  const sessionPreviewReattach = document.getElementById("session-preview-reattach");
  const sessionPreviewClose    = document.getElementById("session-preview-close");
  let sessionPreviewTerm = null;   // throwaway xterm; disposed on close/resume
  let sessionPreviewTaskId = "";
  let sessionPreviewEpoch = 0;     // guards the async previewStart promise across close/reopen
  let sessionPreviewLive = false;  // stream open (⏸ offered) vs paused/dead (▶ offered)

  function disposeSessionPreviewTerm() {
    if (sessionPreviewTerm) { sessionPreviewTerm.dispose(); sessionPreviewTerm = null; }
    sessionPreviewBody.replaceChildren();
  }

  function setPreviewPauseLabel() {
    sessionPreviewPause.textContent = sessionPreviewLive ? "⏸" : "▶";
    sessionPreviewPause.title = sessionPreviewLive ? "一時停止" : "再開";
  }

  function previewNote(text) {
    const p = document.createElement("p");
    p.className = "preview-note";
    p.textContent = text;
    sessionPreviewBody.appendChild(p);
    return p;
  }

  // startSessionPreviewStream (re)opens the live stream for the current
  // task. The grid size arrives asynchronously via harness_previewOpen,
  // which builds the term; until then the pane shows a connecting note.
  async function startSessionPreviewStream() {
    const epoch = ++sessionPreviewEpoch;
    disposeSessionPreviewTerm();
    const note = previewNote("connecting…");
    sessionPreviewLive = true;
    setPreviewPauseLabel();
    try {
      await window.harness.previewStart(sessionPreviewTaskId);
    } catch (e) {
      if (epoch !== sessionPreviewEpoch) return; // superseded/closed meanwhile
      note.textContent = `preview error: ${e.message}`;
      sessionPreviewLive = false;
      setPreviewPauseLabel();
    }
  }

  function pauseSessionPreview() {
    sessionPreviewLive = false;   // flip BEFORE stop: raced late hooks below no-op
    setPreviewPauseLabel();
    window.harness.previewStop();
  }

  // buildPreviewTerm creates the throwaway xterm at the session's true grid
  // (re-rendering at a smaller grid would corrupt full-screen TUI layouts)
  // inside the scale/spacer pair, then fits it to the pane width.
  function buildPreviewTerm(rows, cols) {
    const spacer = document.createElement("div");
    spacer.className = "session-preview-spacer";
    const scaleBox = document.createElement("div");
    scaleBox.className = "session-preview-scale";
    const termBox = document.createElement("div");
    scaleBox.appendChild(termBox);
    spacer.appendChild(scaleBox);
    sessionPreviewBody.appendChild(spacer);
    sessionPreviewTerm = new Terminal({
      cols, rows,
      disableStdin: true,
      convertEol: true,  // match the main terminal so the stream renders identically
      fontSize: 13,
      fontFamily: '"Cascadia Mono", "JetBrains Mono", "DejaVu Sans Mono", "Liberation Mono", Menlo, Consolas, "Courier New", monospace',
    });
    sessionPreviewTerm.open(termBox);
    fitPreviewScale();
  }

  // fitPreviewScale measures the rendered screen at scale 1 (transform reset
  // first — getBoundingClientRect returns the TRANSFORMED box, so measuring
  // with a leftover scale would compound) and scales the grid to the pane.
  function fitPreviewScale() {
    const scaleBox = sessionPreviewBody.querySelector(".session-preview-scale");
    const spacer = sessionPreviewBody.querySelector(".session-preview-spacer");
    if (!scaleBox || !spacer) return;
    scaleBox.style.transform = "";
    const screenEl = scaleBox.querySelector(".xterm-screen");
    const rect = screenEl ? screenEl.getBoundingClientRect() : scaleBox.getBoundingClientRect();
    const avail = sessionPreviewBody.clientWidth - 12; // body padding allowance
    if (rect.width > 0 && avail > 0) {
      const scale = Math.min(1, avail / rect.width);
      scaleBox.style.width = `${rect.width}px`;
      scaleBox.style.transform = `scale(${scale})`;
      spacer.style.height = `${Math.ceil(rect.height * scale)}px`;
    }
  }

  // wasm→JS hooks. The wasm pump is generation-gated (stale pumps go
  // silent); these flags close the residual check-then-invoke race: after a
  // pause/close, sessionPreviewLive is already false, so a late hook no-ops.
  window.harness_previewOpen = (rows, cols, hasSize) => {
    if (!sessionPreviewModal.open || !sessionPreviewLive) return;
    sessionPreviewBody.replaceChildren();  // drop the connecting note
    const r = hasSize && rows > 0 ? rows : 24;
    const c = hasSize && cols > 0 ? cols : 80;
    buildPreviewTerm(r, c);
  };
  window.harness_previewWrite = (u8) => {
    if (!sessionPreviewModal.open || !sessionPreviewLive || !sessionPreviewTerm) return;
    sessionPreviewTerm.write(u8);
  };
  window.harness_previewResize = (rows, cols) => {
    if (!sessionPreviewModal.open || !sessionPreviewLive || !sessionPreviewTerm) return;
    sessionPreviewTerm.resize(cols, rows);
    fitPreviewScale();
  };
  window.harness_previewClosed = () => {
    if (!sessionPreviewModal.open || !sessionPreviewLive) return;
    sessionPreviewLive = false;
    setPreviewPauseLabel();
    if (!sessionPreviewTerm) sessionPreviewBody.replaceChildren(); // died before the grid arrived
    previewNote("(ストリーム終了 — ▶ で再接続)");
  };

  function openSessionPreview(id) {
    sessionPreviewTaskId = id;
    sessionPreviewTitle.textContent = `🔍 ${id.slice(0, 12)}…`;
    if (!sessionPreviewModal.open) sessionPreviewModal.showModal();
    startSessionPreviewStream();
  }

  sessionPreviewClose.addEventListener("click", () => sessionPreviewModal.close());
  // Backdrop click (the dialog element itself, outside its content) closes —
  // same convention as file-preview-modal.
  sessionPreviewModal.addEventListener("click", (ev) => {
    if (ev.target === sessionPreviewModal) sessionPreviewModal.close();
  });
  sessionPreviewModal.addEventListener("close", () => {
    sessionPreviewEpoch++;         // invalidate any in-flight previewStart
    sessionPreviewLive = false;    // silence raced late hooks
    window.harness.previewStop();  // close = disconnect immediately
    disposeSessionPreviewTerm();
  });
  sessionPreviewPause.addEventListener("click", () => {
    if (sessionPreviewLive) pauseSessionPreview();
    else startSessionPreviewStream();
  });
  sessionPreviewReattach.addEventListener("click", () => {
    const id = sessionPreviewTaskId;
    sessionPreviewModal.close();   // close handler stops the stream
    reattachTo(id, false);
  });
```

The three entry points (task sheet 🔍, notify feed, cmdline `preview`) call `openSessionPreview(id)` and need NO changes. Do not touch them.

- [ ] **Step 3: Syntax-check and build**

```bash
node --check webui/static/main.js && make webui-build
```

Expected: clean; only the two edited files in `git status --short` (restore `wasm_exec.js` if version-refreshed; never commit `main.wasm`).

- [ ] **Step 4: Commit**

```bash
git add webui/index.html webui/static/main.js
git commit -m "feat(webui): live session preview — stream while open, ⏸ disconnect-pause / ▶ ring-replay resume"
```

---

### Task 3: E2E verification (controller-run, Playwright + sandbox)

Local sandbox as before (`bin/harness-server --webui-dir` + bash-agent runner). Per the spec's Verification section:

- [ ] (a) Liveness: modal open on the session attached in the MAIN terminal; `echo LIVE_MARKER` there; the marker appears in the preview xterm with no manual action (poll the preview DOM, no reopen).
- [ ] (b) Pause/resume: ⏸ → `echo PAUSED_MARKER` in main terminal → assert it does NOT appear in the preview; ▶ → both markers visible (ring replay caught up) and a further echo streams in live.
- [ ] (c) Close = disconnect: ✕, then echo again — no preview DOM change and no `harness_preview*` console activity; server log/`conns` shows the exec stream gone.
- [ ] (d) Non-interference: after open/pause/resume/close cycles, main-terminal echo round-trip still works (input AND output).
- [ ] (e) Stream death: exit the bash session (type `exit` in main terminal) with the modal open → "(ストリーム終了 — ▶ で再接続)" + ▶; pressing ▶ renders the attach error in the pane.
- [ ] (f) 390px: fullscreen sheet with the live term; wide-session scale still fits.
- [ ] Fix-forward findings, re-verify, commit.
