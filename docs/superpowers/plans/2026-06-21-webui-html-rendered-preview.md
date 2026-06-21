# WebUI rendered HTML preview Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Render `.html` files in the WebUI file preview as a live page inside a sandboxed iframe, with a toggle back to source text.

**Architecture:** Pure front-end delta. The existing file picker already pulls bytes over the PSK-authenticated trsf channel via `window.harness.filePullBytes` (capability-gated by `OpenFileTransfer`). We add an HTML branch to `renderFilePreview` in `webui/static/main.js` that injects the decoded bytes into `<iframe sandbox="allow-scripts" srcdoc=...>` instead of showing source in `<pre>`, plus a header toggle to switch between rendered and source views. No server / runner / protocol / wasm changes.

**Tech Stack:** Vanilla JS + HTML `<dialog>` + CSS already in `webui/`. No build step for JS/CSS/HTML (wasm untouched). WebUI assets hot-reload; browser refresh suffices to test — no server restart.

## Global Constraints

- Dark theme only: `#1e1e1e` background / `#d4d4d4` text family, consistent with existing `webui/static/style.css`. No UA-default light.
- Must be usable at ≤600px (phone) AND desktop. Verify at 390px and desktop in Playwright.
- Security: the rendered iframe MUST use `sandbox="allow-scripts"` and MUST NOT include `allow-same-origin`. This isolates preview content from the WebUI origin/trsf. Do not loosen it.
- No new HTTP route, no token, no server-side change. Reads stay on the existing `filePullBytes` path (inherits the file-transfer capability gate).
- Keep the existing `PREVIEW_MAX_BYTES = 1 MiB` cap for HTML; do not raise it in this plan.
- There is NO JavaScript unit-test harness in this repo (no `package.json`, no JS runner). Do not introduce one. Verification for WebUI is via Playwright MCP per project convention (`webui-build` only builds wasm, which we do not touch).

---

### Task 1: Markup + CSS for the rendered-HTML iframe and view toggle

**Files:**
- Modify: `webui/index.html` (the `#file-preview-modal` `<dialog>`, around lines 129-135)
- Modify: `webui/static/style.css` (preview-modal section, near `.preview-close` and `.preview-body img`)

**Interfaces:**
- Produces: a header button with id `file-preview-toggle` (initially `hidden`); a CSS class `preview-iframe` for the rendered view. Task 2's JS reads `document.getElementById("file-preview-toggle")` and creates `<iframe class="preview-iframe">`.

- [ ] **Step 1: Add the toggle button to the preview header**

In `webui/index.html`, change the preview header block so it contains a toggle button between the title and the close button:

```html
<dialog id="file-preview-modal" class="preview-modal">
  <div class="preview-header">
    <span id="file-preview-title" class="preview-title"></span>
    <button id="file-preview-toggle" class="preview-toggle" hidden>View source</button>
    <button id="file-preview-close" class="preview-close" aria-label="Close">✕</button>
  </div>
  <div id="file-preview-body" class="preview-body"></div>
</dialog>
```

- [ ] **Step 2: Add CSS for the iframe and toggle button**

In `webui/static/style.css`, after the `.preview-close:hover` rule, add:

```css
.preview-toggle {
  flex: 0 0 auto;
  margin-left: auto;          /* group toggle + close at the right edge */
  background: #2a2a2a;
  border: 1px solid #555;
  border-radius: 4px;
  color: #d4d4d4;
  font-size: 0.8em;
  cursor: pointer;
  /* comfortable touch target on phones */
  min-height: 2.4rem;
  padding: 0 0.6em;
}
.preview-toggle:hover { background: #3a3a3a; }
.preview-toggle[hidden] { display: none; }
.preview-body iframe.preview-iframe {
  width: 100%;
  height: 75vh;
  border: 0;
  background: #fff;           /* let the page paint its own background */
  display: block;
}
```

- [ ] **Step 3: Verify the markup loads without breaking the existing preview**

Run the WebUI (existing server) and open it in the browser (Playwright MCP `browser_navigate` to the WebUI URL from `HARNESS_SERVER_CID`). Preview any existing **non-HTML** text file and confirm the modal still works and the toggle button is NOT visible (still `hidden`).
Expected: source `<pre>` preview unchanged; no toggle button shown.

- [ ] **Step 4: Commit**

```bash
git add webui/index.html webui/static/style.css
git commit -m "feat(webui): add toggle button + iframe styles for HTML preview"
```

---

### Task 2: Render HTML in a sandboxed iframe with a source toggle

**Files:**
- Modify: `webui/static/main.js` — add `isHtmlExt`; rework `renderFilePreview` (currently ~line 408 region) and the close handler; cache element + state.

**Interfaces:**
- Consumes: `fileExt(name)` (existing), `openFilePreview(rel, size, bodyNode, note)` (existing), `filePullBytes` result `Uint8Array` (existing), `#file-preview-toggle` (Task 1).
- Produces: HTML files render by default in `<iframe sandbox="allow-scripts" srcdoc>`; the toggle flips between rendered and source without re-pulling.

- [ ] **Step 1: Add the `isHtmlExt` helper**

In `webui/static/main.js`, next to `isImageExt` (near line 1864), add:

```javascript
function isHtmlExt(name) {
  const e = fileExt(name);
  return e === "html" || e === "htm";
}
```

- [ ] **Step 2: Cache the toggle element and add preview state**

In the element-lookup block near the other `filePreview*` consts (around line 178-189), add:

```javascript
  const filePreviewToggle = document.getElementById("file-preview-toggle");
  // Set when the current preview is HTML, so the toggle can rebuild the body
  // from already-fetched bytes without re-pulling. Reset on modal close.
  let filePreviewHtml = null; // { rel, size, bytes, mode: "render" | "source" }
```

- [ ] **Step 3: Add a builder that renders either mode and a toggle handler**

In `webui/static/main.js`, add these two functions next to `renderFilePreview`:

```javascript
  // showHtmlPreview renders the cached HTML bytes in the requested mode and
  // shows/updates the toggle label. mode "render" => sandboxed iframe;
  // mode "source" => plain text in <pre> (same as a normal text preview).
  function showHtmlPreview() {
    const { rel, size, bytes, mode } = filePreviewHtml;
    const text = new TextDecoder("utf-8", { fatal: false }).decode(bytes);
    let node;
    if (mode === "render") {
      const iframe = document.createElement("iframe");
      iframe.className = "preview-iframe";
      // SECURITY: allow-scripts WITHOUT allow-same-origin => opaque origin,
      // cannot reach the WebUI origin / trsf / DOM / storage. Do not add
      // allow-same-origin.
      iframe.setAttribute("sandbox", "allow-scripts");
      iframe.srcdoc = text;
      node = iframe;
    } else {
      const pre = document.createElement("pre");
      pre.textContent = text;
      node = pre;
    }
    openFilePreview(rel, size, node, null);
    filePreviewToggle.hidden = false;
    filePreviewToggle.textContent = mode === "render" ? "View source" : "View rendered";
  }

  filePreviewToggle.addEventListener("click", () => {
    if (!filePreviewHtml) return;
    filePreviewHtml.mode = filePreviewHtml.mode === "render" ? "source" : "render";
    showHtmlPreview();
  });
```

- [ ] **Step 4: Branch `renderFilePreview` to HTML**

In `renderFilePreview` (the function shown below as it exists today), add the HTML branch as the FIRST check, before the image branch:

```javascript
  function renderFilePreview(rel, size, name, bytes) {
    if (isHtmlExt(name)) {
      filePreviewHtml = { rel, size, bytes, mode: "render" };
      showHtmlPreview();
      return;
    }
    if (isImageExt(name)) {
      // ...existing image branch unchanged...
```

Leave the image / binary / text branches exactly as they are.

- [ ] **Step 5: Reset HTML state and hide the toggle on modal close**

Find the existing `filePreviewModal.addEventListener("close", ...)` handler (it revokes `filePreviewObjectURL`). Add the HTML-state reset inside it:

```javascript
  filePreviewModal.addEventListener("close", () => {
    if (filePreviewObjectURL) {
      URL.revokeObjectURL(filePreviewObjectURL);
      filePreviewObjectURL = null;
    }
    filePreviewHtml = null;
    filePreviewToggle.hidden = true;
  });
```

- [ ] **Step 6: Verify rendering and toggle in the browser**

Precondition: a Running/Detached task whose worktree contains a self-contained HTML file. If none exists, push one into a test task's worktree first via the WebUI file picker (Push) — e.g. push the repo's `chronicle.html`.

Using Playwright MCP against the WebUI URL:
1. Select the task, open the file picker, select the `.html`, click Preview.
2. Expected: the page RENDERS (not source text). The toggle reads "View source".
3. Use `browser_evaluate` to assert the iframe attributes:
   `document.querySelector('#file-preview-body iframe.preview-iframe').getAttribute('sandbox')` === `"allow-scripts"` and the iframe has no `allow-same-origin` token.
4. Click the toggle. Expected: body switches to `<pre>` source; toggle reads "View rendered". Click again returns to rendered.

- [ ] **Step 7: Commit**

```bash
git add webui/static/main.js
git commit -m "feat(webui): render .html preview in a sandboxed iframe with source toggle"
```

---

### Task 3: Playwright verification at phone + desktop, and oversize/non-HTML regression

**Files:**
- None (verification only). Capture screenshots as evidence.

**Interfaces:**
- Consumes: the behavior shipped by Tasks 1-2.

- [ ] **Step 1: Verify rendered preview at desktop**

Playwright MCP: `browser_resize` to a desktop size (e.g. 1280×800), preview the `.html`, confirm it renders and any in-page script runs (e.g. a canvas/animation/game draws). Screenshot.

- [ ] **Step 2: Verify rendered preview at 390px (phone)**

`browser_resize` to 390×844. Re-open the preview. Confirm the modal and iframe are usable full-bleed, dark chrome around them, and the page renders. Screenshot.

- [ ] **Step 3: Verify sandbox isolation in the DOM**

`browser_evaluate`:
```javascript
const f = document.querySelector('#file-preview-body iframe.preview-iframe');
({ sandbox: f.getAttribute('sandbox') })
```
Expected: `{ sandbox: "allow-scripts" }` — assert the string does NOT contain `allow-same-origin`.

- [ ] **Step 4: Verify oversize HTML is refused without fetching**

If a >1 MiB `.html` is available (or temporarily lower nothing — just reason from `sel.size`), select it and Preview. Expected: the existing oversize notice is shown ("File is too large to preview ... Use Pull to download it.") and no iframe is created. (The size check in the Preview button handler runs before `renderFilePreview`, so this path is already covered; confirm it still holds.)

- [ ] **Step 5: Verify non-HTML preview is unchanged**

Preview a `.txt`/`.go`/`.json` file. Expected: source `<pre>` as before; the toggle button is hidden.

- [ ] **Step 6: Record evidence**

Note the screenshots and the `browser_evaluate` outputs in the task completion summary. No commit (verification only) unless screenshots are saved to the repo.

---

## Self-Review

**Spec coverage:**
- Rendered HTML in sandboxed iframe → Task 2 Step 4 + showHtmlPreview.
- `sandbox="allow-scripts"`, no `allow-same-origin` → Task 2 Step 3 + asserted Task 2 Step 6 / Task 3 Step 3.
- Rendering default + source toggle → Task 2 Steps 3-4 (default `mode:"render"`), toggle handler.
- Reuse picker / modal / caps gate / path defenses → unchanged code paths (no new read path).
- 1 MiB cap kept → Global Constraints + Task 3 Step 4 (existing handler unchanged).
- Dark + ≤600px → Task 1 CSS + Task 3 Steps 1-2.
- localStorage non-persistence limitation → inherent to `sandbox` without `allow-same-origin`; documented in spec; no code needed.
- No server/runner/wasm change → confirmed; only `index.html`, `style.css`, `main.js`.

**Placeholder scan:** none — every step has concrete code or concrete Playwright actions.

**Type consistency:** `filePreviewHtml` shape `{ rel, size, bytes, mode }` is consistent across Steps 2-5; `showHtmlPreview()` is argument-less and reads the cached object in all call sites; `isHtmlExt(name)` used in Task 2 Step 4 matches its Step 1 definition; `file-preview-toggle` id consistent between Task 1 markup and Task 2 JS.
