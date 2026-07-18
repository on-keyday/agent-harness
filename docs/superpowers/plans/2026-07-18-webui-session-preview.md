# WebUI Session Preview Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Tap-to-open read-only preview of a live interactive session's current screen from the WebUI task list, without disturbing the main terminal or the session's controlling client.

**Architecture:** Reuse the `AttachMode_View` byte-collection path (`cli.collectRaw`) that `harness-cli session snapshot` uses; expose it to the browser via a new wasm bridge function `harness.sessionPreview`; render the raw replay bytes in a throwaway xterm.js instance inside a body-level `<dialog>` (sibling pattern: the existing file-preview modal). No server or wire changes.

**Tech Stack:** Go (wasm build via `syscall/js`), vanilla JS, xterm.js (vendored), CSS.

**Spec:** `docs/superpowers/specs/2026-07-18-webui-session-preview-design.md` — read the Problem section, not just Mechanism.

## Global Constraints

- **Worktree only**: ALL file operations use absolute paths under
  `/home/kforfk/workspace/remote-agent-harness/.harness-worktrees/5b65231c141da46b2239912efd108196/`.
  NEVER touch `/home/kforfk/workspace/remote-agent-harness/<rel>` directly — those paths route to the parent repo checkout where the user is doing unrelated work. Before the first edit run `git rev-parse --abbrev-ref HEAD` from the worktree and confirm it prints `harness/5b65231c141da46b2239912efd108196`; abort if not.
- **Read `.claude/skills/implementation-pitfalls/SKILL.md` in full before writing code** (worktree copy).
- **Build hygiene**: compile-check with `go build ./...` / `GOOS=js GOARCH=wasm go build -o /dev/null ./cmd/harness-webui-wasm/`. NEVER bare `go build ./cmd/<x>/` (drops a binary in the worktree root). `make webui-build` writing `webui/static/main.wasm` + refreshed `wasm_exec.js` is expected and fine (gitignored / vendored respectively — do not commit `main.wasm`).
- **Public repo**: no LAN IPs, internal hostnames, or private paths in code, comments, or commits.
- **Dark theme**: WebUI additions must track #1e1e1e/#d4d4d4 (reuse `.preview-modal` styles) and work at ≤600px.
- **No server/wire changes**: this feature must not touch `server/`, `runner/`, or any `.bgn` schema.

---

### Task 1: Go/wasm — share `collectRaw`, add `harness.sessionPreview`

**Files:**
- Create: `cli/snapshot_raw.go`
- Modify: `cli/snapshot_native.go` (remove the moved function + now-unused imports)
- Modify: `cmd/harness-webui-wasm/main.go` (register + implement `sessionPreview`)

**Interfaces:**
- Consumes: existing `(*cli.Client).AttachSession(ctx, taskIDHex, protocol.AttachMode_View)`, `currentClient()`, `rootCtx`, `rejectErr` in the wasm main.
- Produces: JS-visible `harness.sessionPreview(taskIDHex[, settleMs]) -> Promise<{bytes: Uint8Array, rows: number, cols: number, hasSize: boolean}>` — Task 2 relies on this exact shape, including the property names.

- [ ] **Step 1: Move `collectRaw` verbatim into a new shared file**

Create `cli/snapshot_raw.go` containing exactly the existing function and its doc comment, moved from `cli/snapshot_native.go` (lines 20–71 there). No build tag — this file must compile for both native and `GOOS=js`:

```go
package cli

import (
	"context"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// collectRaw view-attaches to a detachable interactive session and drains the
// replayed (and briefly-live) PTY byte burst for `settle`, returning the
// verbatim bytes — escape sequences intact — plus the terminal size the server
// replays ahead of the ring (hasSize=false when the session reports none, e.g.
// an older server). It uses AttachMode_View, so it never takes over the
// controlling client (a live operator keeps typing undisturbed). Shared by the
// raw path (SessionSnapshotRaw, which returns these bytes as-is), the rendered
// path (collectScreen, which feeds them through a VT emulator), and the wasm
// WebUI preview (which renders them in the browser's xterm instead — the VT
// emulator stays native-only).
func (c *Client) collectRaw(ctx context.Context, taskIDHex string, settle time.Duration) (captured []byte, rows, cols uint16, hasSize bool, err error) {
	stream, _, err := c.AttachSession(ctx, taskIDHex, protocol.AttachMode_View)
	if err != nil {
		return nil, 0, 0, false, err
	}
	defer stream.Close()

	var mu sync.Mutex
	var data []byte
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 32*1024)
		out := stream.Stdout()
		for {
			n, rerr := out.Read(buf)
			if n > 0 {
				mu.Lock()
				data = append(data, buf[:n]...)
				full := len(data) > 8*1024*1024
				mu.Unlock()
				if full {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	select {
	case <-time.After(settle):
	case <-done:
	case <-ctx.Done():
	}

	mu.Lock()
	captured = append([]byte(nil), data...)
	mu.Unlock()

	rows, cols, hasSize = stream.LastWindowSize()
	return captured, rows, cols, hasSize, nil
}
```

(The doc comment gains one clause about the wasm consumer — that is the only permitted deviation from verbatim.)

Then delete the function and its comment from `cli/snapshot_native.go` and fix that file's imports: remove `sync` and `github.com/on-keyday/agent-harness/runner/protocol` (after the move, nothing in the native file references them — verify with grep before deleting; `context`, `time`, `fmt`, `image/color`, `io`, `os`, `strings`, `uv`, `vt` all remain in use).

- [ ] **Step 2: Verify both build modes and existing tests**

```bash
go build ./... && GOOS=js GOARCH=wasm go build ./cli/... ./cmd/harness-webui-wasm/ && go test ./cli/...
```

Expected: all succeed with no output changes. (These are the `make check` / `make wasm-check` bodies without the wasm artifact write.)

- [ ] **Step 3: Add the wasm bridge function**

In `cmd/harness-webui-wasm/main.go`, register in the `js.Global().Set("harness", ...)` map, alphabetically near the other session functions:

```go
"sessionPreview":     js.FuncOf(harnessSessionPreview),
```

Add the implementation next to `harnessAttachSession` (grep for it), following the same Promise-executor pattern as `harnessFilePullBytes` (Uint8Array via `js.CopyBytesToJS`):

```go
// harnessSessionPreview view-attaches (AttachMode_View — non-takeover) to a
// detachable interactive session on an independent stream, collects the
// replayed PTY byte burst for settleMs, and resolves with the verbatim bytes
// plus the terminal size the server replays ahead of the ring. It never
// touches the activeInteractiveSession singleton, so the user's live attached
// session keeps working while a preview loads. The JS layer feeds the bytes
// to a throwaway xterm sized rows×cols (hasSize=false → caller falls back).
//
//	harness.sessionPreview(taskIDHex[, settleMs]) ->
//	    Promise<{bytes: Uint8Array, rows, cols, hasSize}>
func harnessSessionPreview(this js.Value, args []js.Value) any {
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
				rejectErr(reject, errors.New("sessionPreview: missing taskIDHex arg"))
				return
			}
			taskID := args[0].String()
			settle := 1500 * time.Millisecond // default matches `session snapshot --settle-ms`
			if len(args) >= 2 && args[1].Type() == js.TypeNumber && args[1].Int() > 0 {
				settle = time.Duration(args[1].Int()) * time.Millisecond
			}
			data, rows, cols, hasSize, err := c.collectRaw(rootCtx, taskID, settle)
			if err != nil {
				rejectErr(reject, err)
				return
			}
			u8 := js.Global().Get("Uint8Array").New(len(data))
			js.CopyBytesToJS(u8, data)
			out := js.Global().Get("Object").New()
			out.Set("bytes", u8)
			out.Set("rows", int(rows))
			out.Set("cols", int(cols))
			out.Set("hasSize", hasSize)
			resolve.Invoke(out)
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}
```

Check the file's existing imports — `errors` and `time` are almost certainly already imported (grep); add if missing.

- [ ] **Step 4: Re-verify builds**

```bash
go build ./... && GOOS=js GOARCH=wasm go build ./cli/... ./cmd/harness-webui-wasm/ && go vet ./cli/... ./cmd/harness-webui-wasm/
```

Expected: clean. Confirm `git status --short` shows ONLY the three intended files (no stray binaries).

- [ ] **Step 5: Commit**

```bash
git add cli/snapshot_raw.go cli/snapshot_native.go cmd/harness-webui-wasm/main.go
git commit -m "feat(wasm): harness.sessionPreview — view-attach byte snapshot for the WebUI

Moves cli.collectRaw to a shared (untagged) file so the js build can use
it; the VT-emulator render path stays native-only."
```

---

### Task 2: WebUI frontend — preview modal, task-sheet / feed / cmdline entry points

**Files:**
- Modify: `webui/index.html` (new body-level dialog, after `#runner-picker-modal`)
- Modify: `webui/static/style.css` (small additions after the `.preview-modal` block)
- Modify: `webui/static/main.js` (preview logic + three entry points + help text)

**Interfaces:**
- Consumes: `window.harness.sessionPreview(taskIDHex)` from Task 1 (shape `{bytes, rows, cols, hasSize}`); existing `reattachTo(id, view)`, `buildTaskSheet`, `addItem`, notify-feed `mkBtn`, `runCmd` switch, `appendCmdOutput`, global `Terminal` (xterm.js).
- Produces: `openSessionPreview(taskIDHex)` (plain function in main.js's main scope) used by all three entry points.

- [ ] **Step 1: Add the dialog to `webui/index.html`**

Directly after the closing `</dialog>` of `#runner-picker-modal`:

```html
<!-- Session preview modal: one-shot read-only screen snapshot of a live
     interactive session (view-attach — never takes over the controlling
     client). Body-level for the same top-layer reason as file-preview-modal
     above. -->
<dialog id="session-preview-modal" class="preview-modal">
  <div class="preview-header">
    <span id="session-preview-title" class="preview-title"></span>
    <button id="session-preview-reattach" class="preview-toggle">↪ Reattach</button>
    <button id="session-preview-refresh" class="preview-toggle" title="再取得">🔄</button>
    <button id="session-preview-close" class="preview-close" aria-label="Close">✕</button>
  </div>
  <div id="session-preview-body" class="preview-body session-preview-body"></div>
</dialog>
```

- [ ] **Step 2: Add CSS after the `.picker-modal` block in `webui/static/style.css`**

```css
/* Session preview modal: the body hosts a throwaway xterm at the session's
   TRUE grid size (re-rendering at a smaller grid would corrupt full-screen
   TUI layouts), scaled down with transform to fit the pane width. transform
   doesn't affect layout, so the spacer gets an explicit scaled height to
   avoid a dead scroll area below the screen. */
.session-preview-body { padding: 0.5em; }
.session-preview-spacer { overflow: hidden; }
.session-preview-scale { transform-origin: top left; }
```

- [ ] **Step 3: Add the preview logic to `webui/static/main.js`**

Place immediately BEFORE the `// renderTaskList builds clickable task rows…` comment block (same scope as `buildTaskSheet` / `reattachTo`; `function` declarations hoist, so the notify feed defined earlier in the file can call `openSessionPreview` at runtime):

```js
  // --- Session preview modal: one-shot view-attach snapshot of a live
  //     interactive session, rendered in a throwaway read-only xterm sized to
  //     the session's real grid and CSS-scaled to fit the pane. View mode
  //     never takes over the controlling client, and the preview stream is
  //     independent of the main terminal's singleton, so peeking is always
  //     safe — including at the session currently attached here.
  const sessionPreviewModal    = document.getElementById("session-preview-modal");
  const sessionPreviewTitle    = document.getElementById("session-preview-title");
  const sessionPreviewBody     = document.getElementById("session-preview-body");
  const sessionPreviewRefresh  = document.getElementById("session-preview-refresh");
  const sessionPreviewReattach = document.getElementById("session-preview-reattach");
  const sessionPreviewClose    = document.getElementById("session-preview-close");
  let sessionPreviewTerm = null;   // throwaway xterm; disposed on close/refresh
  let sessionPreviewTaskId = "";
  let sessionPreviewEpoch = 0;     // guards against a stale load resolving after close/refresh

  function disposeSessionPreviewTerm() {
    if (sessionPreviewTerm) { sessionPreviewTerm.dispose(); sessionPreviewTerm = null; }
    sessionPreviewBody.replaceChildren();
  }

  async function renderSessionPreview() {
    const epoch = ++sessionPreviewEpoch;
    disposeSessionPreviewTerm();
    const note = document.createElement("p");
    note.className = "preview-note";
    note.textContent = "loading… (収集 ~1.5s)";
    sessionPreviewBody.appendChild(note);
    let snap;
    try {
      snap = await window.harness.sessionPreview(sessionPreviewTaskId);
    } catch (e) {
      if (epoch === sessionPreviewEpoch) note.textContent = `preview error: ${e.message}`;
      return;
    }
    // Closed or superseded (refresh / another row) while collecting — drop it.
    if (epoch !== sessionPreviewEpoch || !sessionPreviewModal.open) return;
    sessionPreviewBody.replaceChildren();
    const rows = snap.hasSize && snap.rows > 0 ? snap.rows : 24;
    const cols = snap.hasSize && snap.cols > 0 ? snap.cols : 80;
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
      convertEol: true,  // match the main terminal so the replay renders identically
      fontSize: 13,
      fontFamily: '"Cascadia Mono", "JetBrains Mono", "DejaVu Sans Mono", "Liberation Mono", Menlo, Consolas, "Courier New", monospace',
    });
    sessionPreviewTerm.open(termBox);
    sessionPreviewTerm.write(snap.bytes);
    // Fit-to-width: measure the rendered screen and scale the whole grid down.
    const screenEl = termBox.querySelector(".xterm-screen");
    const rect = screenEl ? screenEl.getBoundingClientRect() : termBox.getBoundingClientRect();
    const avail = sessionPreviewBody.clientWidth - 12; // body padding allowance
    if (rect.width > 0 && avail > 0) {
      const scale = Math.min(1, avail / rect.width);
      scaleBox.style.width = `${rect.width}px`;
      scaleBox.style.transform = `scale(${scale})`;
      spacer.style.height = `${Math.ceil(rect.height * scale)}px`;
    }
  }

  function openSessionPreview(id) {
    sessionPreviewTaskId = id;
    sessionPreviewTitle.textContent = `🔍 ${id.slice(0, 12)}…`;
    if (!sessionPreviewModal.open) sessionPreviewModal.showModal();
    renderSessionPreview();
  }

  sessionPreviewClose.addEventListener("click", () => sessionPreviewModal.close());
  // Backdrop click (the dialog element itself, outside its content) closes —
  // same convention as file-preview-modal.
  sessionPreviewModal.addEventListener("click", (ev) => {
    if (ev.target === sessionPreviewModal) sessionPreviewModal.close();
  });
  sessionPreviewModal.addEventListener("close", () => {
    sessionPreviewEpoch++;         // invalidate any in-flight load
    disposeSessionPreviewTerm();
  });
  sessionPreviewRefresh.addEventListener("click", renderSessionPreview);
  sessionPreviewReattach.addEventListener("click", () => {
    const id = sessionPreviewTaskId;
    sessionPreviewModal.close();
    reattachTo(id, false);
  });
```

- [ ] **Step 4: Wire the three entry points**

(a) **Task sheet** — in `buildTaskSheet`, inside the existing `if (t.kind === "Interactive" && (t.status === "Running" || t.status === "Detached"))` block, add as the FIRST item (peek before committing to a reattach):

```js
      addItem("🔍 プレビュー", "", () => openSessionPreview(t.id));
```

(b) **Notification feed** — in the notify-feed entry click handler (grep `mkBtn("↪ Reattach"`), add to BOTH the `if (live)` block and the `if (!t)` fallback block, before their Reattach buttons:

```js
            mkBtn("🔍 プレビュー", openSessionPreview);
```

(c) **WebUI cmdline** — in the `runCmd` switch, add a `preview` case after `case "cancel":`:

```js
        case "preview":
          if (!tokens[1]) throw new Error("preview: missing task id");
          openSessionPreview(tokens[1]);
          out = `preview ${tokens[1].slice(0, 12)}…`;
          break;
```

and a help line after the `cancel` line in the `help` array:

```js
            "  preview <task-id>         one-shot screen preview of a live session",
```

- [ ] **Step 5: Syntax-check and build**

```bash
node --check webui/static/main.js && make webui-build
```

Expected: no output from `node --check`; wasm build succeeds. `git status --short` must show only the three edited files (plus `webui/static/wasm_exec.js` if the Go SDK refreshed it — restore with `git checkout -- webui/static/wasm_exec.js` if its diff is only a version refresh; do NOT commit `main.wasm`).

- [ ] **Step 6: Commit**

```bash
git add webui/index.html webui/static/style.css webui/static/main.js
git commit -m "feat(webui): session preview modal — peek at a live session before reattaching

Entry points: task-sheet 🔍 button, notify-feed action, cmdline 'preview <id>'."
```

---

### Task 3: E2E verification (controller-run, Playwright)

Run by the controller (not a subagent) against the live WebUI — wasm hot-reloads on browser refresh, no server restart. Resume a bash-runner task for a cheap real PTY (task-list ids go stale — take ids from a fresh snapshot).

- [ ] **(a) Preview shows the real screen**: echo a marker string in the bash session, open its 🔍 プレビュー from the task list, assert the marker text appears in the preview xterm's DOM (xterm is DOM-rendered).
- [ ] **(b) Non-interference**: with the main terminal live-attached to the SAME session, open a preview, close it, then verify an echo round-trip in the main terminal still works (input AND output — not just render).
- [ ] **(c) Mobile layout**: resize to 390px width, open a preview, screenshot — near-fullscreen sheet, scaled screen visible, close button reachable.
- [ ] **(d) cmdline route**: `preview <id>` from the Command box opens the same modal.
- [ ] Fix-forward any findings, re-verify, commit.
