# WebUI rendered HTML preview (sandboxed iframe)

Date: 2026-06-21
Status: design, awaiting implementation plan

## Problem

The WebUI already has a file picker with a Preview action (`webui/static/main.js`).
For HTML files it shows the **source text** in a `<pre>`, not a rendered page.
The dogfood artifacts are self-contained single-file HTML pages
(`chronicle.html`, `rpg.html`, `neonbreak.html`, `physarum.html`). The user wants
to *see the rendered page* — primarily from a phone, where the PC `port-forward`
path is unavailable (a phone has no local listener; the browser only reaches the
server).

## Why this is small

The capability already exists end to end and needs no new network surface:

- `window.harness.fileLs` / `window.harness.filePullBytes` (wasm bindings in
  `cmd/harness-webui-wasm/main.go`) already list and read worktree files over the
  existing PSK-authenticated trsf channel.
- `renderFilePreview` in `webui/static/main.js` already routes by content type
  (image → object URL, binary → hex dump, else → `<pre>` source) inside an
  existing modal (`file-preview-modal`) with a 1 MiB cap (`PREVIEW_MAX_BYTES`).
- Path traversal and symlink escape are already enforced runner-side by
  `ValidateRelPath` / `rejectIfSymlinkInPath` in `runner/file_transfer.go`,
  rooted at the worktree.
- `filePullBytes` flows through the `OpenFileTransfer` runner RPC, which is
  capability-gated on the server. The preview inherits that gate unchanged.

So the change is a **front-end delta only**: add one HTML branch to
`renderFilePreview`. No server / runner / protocol / wasm changes.

## Design

### Data flow (unchanged transport)

Existing path: file picker → `fileLs(task, dir)` to browse → select file →
`filePullBytes(task, rel)` → bytes in the browser. The only new behavior is how
HTML bytes are presented.

### Rendering

In `renderFilePreview`, add a branch before the generic `<pre>` fallback:

- If the file is HTML (extension `.html` / `.htm`, case-insensitive), decode the
  bytes as UTF-8 and render into an iframe:
  `<iframe sandbox="allow-scripts" srcdoc="<decoded html>">`.
- Default view for HTML is **rendered**. The modal gains a toggle to switch to
  **View source** (the current `<pre>` text view) and back. The toggle rebuilds
  the modal body from the already-fetched bytes; it does not re-pull.
- Non-HTML behavior (image / binary / other text) is unchanged.

### Modal

Reuse `file-preview-modal`. The iframe fills the modal body; on a ≤600px
viewport it must be usable full-bleed (dark theme `#1e1e1e` / `#d4d4d4`,
consistent with the rest of the WebUI). The existing close / backdrop-click /
object-URL-revoke teardown is reused; the toggle state resets when the modal
closes.

### Security boundary

The harness trust boundary is the trsf PSK handshake, not the HTTP layer (the
HTTP mux that serves `/` and `/static/` is intentionally unauthenticated and
ships only public shell + wasm; the secret lives in the `#psk` URL fragment and
is never sent to the server). This design adds **no new HTTP surface** and stays
entirely inside that boundary:

- Reads go through `filePullBytes` → `OpenFileTransfer`, so the existing
  capability gate applies: a confined task without the file-transfer capability
  cannot preview, exactly as today.
- The iframe uses `sandbox="allow-scripts"` and **deliberately omits
  `allow-same-origin`**. The rendered page therefore runs in an opaque origin and
  cannot reach the WebUI's origin, its trsf connection, or its DOM/storage. This
  is the isolation that makes rendering attacker-influenced HTML acceptable.
- Path traversal / symlink escape remain blocked runner-side (unchanged).

## Scope / non-goals (YAGNI)

- **In scope:** single self-contained HTML files (inline CSS/JS, no external
  sibling assets), rendered from a phone or desktop browser.
- **Out of scope (deferred):** multi-file relative-path sites and absolute-path
  sites. Faithful URL resolution for those requires a real HTTP origin, which
  requires a token-gated `/preview` route on a separate origin with CSP — a
  larger change to be designed only if a real need appears.

## Known limitations (trade-offs)

- Because `allow-same-origin` is omitted, `localStorage` / persistent storage is
  unavailable inside the iframe. Games that save progress (e.g. the dogfood RPG's
  save/continue) will render and play but **will not persist saves**. The preview
  is view-only by design. Lifting this would require serving from a distinct real
  origin (the deferred extension), not loosening the sandbox.
- The existing 1 MiB `PREVIEW_MAX_BYTES` cap is kept for the MVP. HTML with large
  inline (e.g. base64) images may exceed it and be refused with the existing
  oversize notice. If real dogfood artifacts routinely exceed 1 MiB, raise the
  cap for HTML specifically in a follow-up; do not raise it blindly for all types
  (memory-safety reason the cap exists).

## Testing

- JS logic: HTML-extension detection, UTF-8 decode, rendered/source toggle
  switching the modal body without re-pulling.
- Playwright (per project convention — dark theme + ≤600px from the first cut):
  preview a real task's `chronicle.html` at desktop and 390px; confirm the page
  renders and its script runs inside the iframe; assert the iframe has
  `sandbox="allow-scripts"` and **no** `allow-same-origin` in the DOM.
- Confirm a confined task lacking the file-transfer capability still cannot
  preview (gate inherited, unchanged).

## Implementation note

WebUI JS/CSS/HTML changes hot-reload — no wasm rebuild and no server restart are
needed to test this (browser refresh suffices).
