"use strict";

// File verbs, transfer progress, and the preview's pure decisions (crop,
// HTTP target, shim, binary sniffing, hex dump).

async function runFileAction(b) {
  const sub = b.path[1];
  const a = b.args;
  switch (sub) {
    case "ls":
      return fileLsCmd(a[0], a[1] || "");
    case "delete":
      return fileDeleteCmd(a[0], a[1], !!b.flags.recursive, !!b.flags.force);
    case "push":
      // -r / -f / -p parsed here and were dropped: pushBytesWithPrompts
      // started from false and only escalated through a confirm dialog.
      return filePushCmd(a[0], a[1], {
        recursive: !!b.flags.recursive, force: !!b.flags.force, parents: !!b.flags.parents,
      });
    case "pull":
      // --offset / --length reach filePullBytesRange, which this surface has
      // had all along for the HTML preview. They parsed here and were dropped.
      return filePullCmd(a[0], a[1], !!b.flags.recursive,
        Number(b.flags.offset || 0), Number(b.flags.length || 0));
    case "mkdir":
      return fileMkdirCmd(a[0], a[1], !!b.flags.parents);
    case "new":
      return fileNewCmd(a[0], a[1]);
    case "edit":
      return fileEditCmd(a[0], a[1]);
    default:
      throw new Error(`file: unknown sub-verb ${sub}`);
  }
}

async function fileLsCmd(taskID, rel) {
  const entries = await window.harness.fileLs(taskID, rel);
  if (entries.length === 0) return "(empty)";
  return entries.map(e => {
    const name = e.isDir ? `${e.name}/` : e.name;
    const sz = e.isDir ? "" : String(e.size);
    return `${sz.padStart(10)} ${name}`;
  }).join("\n");
}

async function fileDeleteCmd(taskID, rel, recursive, force) {
  // Confirm before destructive action. Browser native dialog.
  const verb = recursive ? (force ? "rm -rf" : "rmdir") : "rm";
  if (!window.confirm(`${verb} ${rel} on task ${taskID.slice(0, 12)} — proceed?`)) {
    return "delete cancelled";
  }
  await window.harness.fileDelete(taskID, rel, recursive, force);
  return `${verb} ok: ${rel}`;
}

async function fileMkdirCmd(taskID, rel, parents) {
  await window.harness.fileMkdir(taskID, rel, parents);
  return `mkdir ok: ${rel}`;
}

async function filePushCmd(taskID, remoteRel, flags = {}) {
  // Open the hidden file picker; abort if the user closes it without
  // selecting anything.
  const file = await pickLocalFile();
  if (!file) return "push cancelled (no file selected)";
  const buf = new Uint8Array(await file.arrayBuffer());
  const fp = beginFileProgress(file.name);
  try {
    const res = await pushBytesWithPrompts(taskID, remoteRel, buf, file.name, fp.onProgress, flags);
    return res.msg;
  } finally {
    fp.end();
  }
}

async function fileNewCmd(taskID, remoteRel) {
  // The editor's name field is seeded with the requested destination and
  // stays editable; whatever it holds on Save is the final worktree-rel dst.
  const edited = await openFileEditor({ name: remoteRel });
  if (!edited) return "new file cancelled";
  const buf = new TextEncoder().encode(edited.text);
  const fp = beginFileProgress(basename(edited.name));
  try {
    const res = await pushBytesWithPrompts(taskID, edited.name, buf, basename(edited.name), fp.onProgress);
    return res.msg;
  } finally {
    fp.end();
  }
}

async function fileEditCmd(taskID, rel) {
  return editRemoteFile(taskID, rel);
}

// editRemoteFile loads rel from the task's worktree, opens it in the editor
// modal, and writes it back. Returns a result line for whichever surface
// called it (the Files-tab #file-result pane or the cmd output).
//
// Two save paths. Unchanged name => in-place: fileEditCommit re-reads the
// runner-side file and reports "conflict" if it moved while the modal was
// open, which becomes a confirm and a forced retry here. Changed name =>
// save-as: there is no baseline at the new path, so the bytes go through
// pushBytesWithPrompts and its already_exists / not_found prompts.
async function editRemoteFile(taskID, rel) {
  let doc;
  try {
    doc = await window.harness.fileEditLoad(taskID, rel);
  } catch (e) {
    if (e && e.code === "too_large") {
      return `edit error: ${rel} is too large to edit — use Pull to download it`;
    }
    if (e && e.code === "not_text") {
      return `edit error: ${rel} is not editable text — use Preview to inspect it`;
    }
    throw e;
  }

  // Buffer carried across re-opens: declining a conflict overwrite must not
  // throw away what was typed. The editor comes back with it so the operator
  // can retarget the path (a save-as) or copy the text out.
  let text = doc.text;
  let title = `Edit ${rel}`;
  for (;;) {
    const edited = await openFileEditor({ name: rel, text, title, saveLabel: "Save & push" });
    if (!edited) return "edit cancelled";
    text = edited.text;

    if (edited.name !== rel) {
      const buf = window.harness.fileEditEncode(text, doc.crlf, doc.bom);
      const fp = beginFileProgress(basename(edited.name));
      try {
        const res = await pushBytesWithPrompts(taskID, edited.name, buf, basename(edited.name), fp.onProgress);
        return res.msg;
      } finally {
        fp.end();
      }
    }

    let res = await window.harness.fileEditCommit(
      taskID, rel, doc.orig, text, doc.crlf, doc.bom, false);
    if (res.status === "unchanged") return `no change: ${rel}`;
    if (res.status === "pushed") return `edit ok: ${rel} (${text.length} chars)`;
    if (window.confirm(`${rel} は runner 側で変更されています。上書きしますか?`)) {
      res = await window.harness.fileEditCommit(
        taskID, rel, doc.orig, text, doc.crlf, doc.bom, true);
      return res.status === "unchanged"
        ? `no change: ${rel}`
        : `edit ok (overwritten): ${rel} (${text.length} chars)`;
    }
    title = `Edit ${rel} — runner-side change kept; save under another name or close`;
  }
}

// pushBytesWithPrompts uploads buf to <taskID>:<remoteRel> via
// window.harness.filePushBytes, driving the two interactive retries shared
// by every push surface (Push button, cmd `file push`, text-file editor):
// overwrite confirmation on already_exists and parent-dir creation on
// not_found. Returns { ok, msg }; ok=false means the user declined a
// confirm. Any other error is thrown for the caller to render on its own
// surface (fileResultPre vs cmd-output).
async function pushBytesWithPrompts(taskID, remoteRel, buf, displayName, onProgress, flags = {}) {
  // Typed flags START the loop rather than being asked for: `file push -f -p`
  // parsed on this surface and was dropped, so an operator who said
  // "overwrite, make parents" still got both confirm dialogs.
  let force = !!flags.force;
  let parents = !!flags.parents;
  for (;;) {
    try {
      await window.harness.filePushBytes(taskID, remoteRel, buf, force, parents, onProgress);
      return { ok: true, msg: `${force ? "push ok (overwritten)" : "push ok"}: ${displayName} -> ${remoteRel} (${buf.byteLength} bytes)` };
    } catch (e) {
      if (!force && e && e.code === "already_exists") {
        if (!window.confirm(`${remoteRel} already exists on the runner. Overwrite?`)) {
          return { ok: false, msg: "push cancelled (overwrite declined)" };
        }
        force = true;
        continue; // retry with overwrite
      }
      if (!parents && e && e.code === "not_found") {
        if (!window.confirm(`${remoteRel} の親ディレクトリが存在しません。作成して再試行しますか?`)) {
          return { ok: false, msg: "push cancelled (missing parent dir)" };
        }
        parents = true;
        continue; // retry creating parent dirs
      }
      throw e;
    }
  }
}

// openFileEditor shows the text-file editor modal and resolves {name, text}
// on Save, or null when dismissed (✕ / Cancel / Esc). opts:
//   name      seeds the file-name field — cmd `file new` passes its
//             <worktree-rel-dst>, the Files-tab New button passes "" so the
//             file lands in the picker's current directory, and edit passes
//             the path it loaded. It stays editable in every mode: retargeting
//             it on an edit is a save-as.
//   text      seeds the body; "" for a new file.
//   title     modal header.
//   saveLabel the Save button's label.
function openFileEditor(opts) {
  const { name = "", text = "", title = "New text file", saveLabel = "Save & upload" } = opts || {};
  const modal     = document.getElementById("file-editor-modal");
  const titleEl   = document.getElementById("file-editor-title");
  const nameIn    = document.getElementById("file-editor-name");
  const textIn    = document.getElementById("file-editor-text");
  const saveBtn   = document.getElementById("file-editor-save");
  const cancelBtn = document.getElementById("file-editor-cancel");
  const closeBtn  = document.getElementById("file-editor-close");
  if (modal.open) return Promise.resolve(null); // one editor at a time
  titleEl.textContent = title;
  saveBtn.textContent = saveLabel;
  nameIn.value = name;
  textIn.value = text;
  return new Promise((resolve) => {
    const finish = (result) => {
      saveBtn.removeEventListener("click", onSave);
      cancelBtn.removeEventListener("click", onDismiss);
      closeBtn.removeEventListener("click", onDismiss);
      modal.removeEventListener("cancel", onEsc);
      if (modal.open) modal.close();
      resolve(result);
    };
    const onSave = () => {
      const name = nameIn.value.trim();
      if (!name) { nameIn.focus(); return; } // name required; keep editing
      finish({ name, text: textIn.value });
    };
    const onDismiss = () => finish(null);
    const onEsc = () => finish(null); // dialog "cancel" event = Esc key
    saveBtn.addEventListener("click", onSave);
    cancelBtn.addEventListener("click", onDismiss);
    closeBtn.addEventListener("click", onDismiss);
    modal.addEventListener("cancel", onEsc);
    modal.showModal();
    nameIn.focus();
  });
}

async function filePullCmd(taskID, remoteRel, recursive, offset = 0, length = 0) {
  const fp = beginFileProgress(basename(remoteRel) + (recursive ? ".tar" : ""));
  try {
    if (recursive) {
      const bytes = await window.harness.filePullDirBytes(taskID, remoteRel, fp.onProgress);
      triggerDownload(bytes, basename(remoteRel) + ".tar");
      return `pull ok (tar): ${remoteRel} (${bytes.byteLength} bytes) — browser save dialog`;
    }
    if (offset || length) {
      const slice = await pullPreviewSlice(taskID, remoteRel, offset, length, fp);
      triggerDownload(slice.bytes, basename(remoteRel));
      return `pull ok: ${remoteRel} bytes ${offset}..${offset + slice.bytes.byteLength} of ${slice.total} — browser save dialog`;
    }
    const bytes = await window.harness.filePullBytes(taskID, remoteRel, fp.onProgress);
    triggerDownload(bytes, basename(remoteRel));
    return `pull ok: ${remoteRel} (${bytes.byteLength} bytes) — browser save dialog`;
  } finally {
    fp.end();
  }
}

// pickLocalFile programmatically opens the hidden <input type="file">
// in index.html, returning the File the user selected (or null when
// they dismissed the dialog).
function pickLocalFile() {
  const input = document.getElementById("hidden-file-input");
  if (!input) {
    return Promise.reject(new Error("hidden-file-input element missing from index.html"));
  }
  return new Promise((resolve) => {
    input.value = ""; // clear any prior selection so onchange re-fires
    const onChange = () => {
      input.removeEventListener("change", onChange);
      input.removeEventListener("cancel", onCancel);
      resolve(input.files && input.files[0] ? input.files[0] : null);
    };
    const onCancel = () => {
      input.removeEventListener("change", onChange);
      input.removeEventListener("cancel", onCancel);
      resolve(null);
    };
    input.addEventListener("change", onChange);
    input.addEventListener("cancel", onCancel);
    input.click();
  });
}

// formatBytes renders a byte count as B / KB / MB for progress display.
function formatBytes(n) {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / 1024 / 1024).toFixed(1)} MB`;
}

// beginFileProgress creates a dedicated progress row for ONE transfer and
// returns { onProgress, end }. Each concurrent push/pull gets its own row, so
// parallel transfers don't clobber a shared bar (and one finishing doesn't
// wipe another's progress). Pass .onProgress to harness.file{Pull,PullDir,
// Push}Bytes (the wasm side throttles to ~10/s; total 0 = unknown size → an
// indeterminate bar) and call .end() in a finally to remove the row.
let fileProgressSeq = 0;
function beginFileProgress(label) {
  const list = document.getElementById("file-progress-list");
  if (!list) return { onProgress: undefined, end: () => {} };
  const id = ++fileProgressSeq;
  const row = document.createElement("div");
  row.className = "file-progress-row";
  row.dataset.fpid = String(id);
  const bar = document.createElement("progress");
  const txt = document.createElement("span");
  txt.className = "file-progress-text";
  txt.textContent = `${label}: starting…`;
  row.appendChild(bar);
  row.appendChild(txt);
  list.appendChild(row);
  const onProgress = (transferred, total) => {
    if (total > 0) {
      bar.max = total;
      bar.value = transferred;
      const pct = Math.floor((transferred / total) * 100);
      txt.textContent = `${label}: ${pct}%  (${formatBytes(transferred)} / ${formatBytes(total)})`;
    } else {
      bar.removeAttribute("value"); // no value attr => indeterminate animation
      txt.textContent = `${label}: ${formatBytes(transferred)} transferred…`;
    }
  };
  return { onProgress, end: () => row.remove() };
}

// triggerDownload wraps bytes (Uint8Array) in a Blob and programmatically
// clicks an anchor with the download attribute. The browser shows its
// native save dialog (which handles overwrite confirmation per its own
// rules — Firefox prompts every time, Chrome's behavior depends on the
// "ask where to save each file" preference).
function triggerDownload(bytes, filename) {
  const blob = new Blob([bytes], { type: "application/octet-stream" });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename || "download";
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  // Defer revoke so the download has started before we drop the object
  // URL. 1s is generous; modern browsers detach the download from the
  // URL once the navigation begins, but revoking too eagerly has been
  // observed to truncate large downloads on some configurations.
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

// basename returns the last component of a forward-slash path (the
// wire side uses POSIX paths regardless of host OS).
function basename(p) {
  const i = p.lastIndexOf("/");
  return i >= 0 ? p.slice(i + 1) : p;
}

// --- File preview helpers (pure; used by the Files-tab Preview modal) ---

const IMAGE_MIME_BY_EXT = {
  png: "image/png", jpg: "image/jpeg", jpeg: "image/jpeg", gif: "image/gif",
  webp: "image/webp", bmp: "image/bmp", svg: "image/svg+xml", ico: "image/x-icon",
  avif: "image/avif",
};

function fileExt(name) {
  const b = basename(name || "");
  const i = b.lastIndexOf(".");
  return i > 0 ? b.slice(i + 1).toLowerCase() : "";
}

function isImageExt(name) {
  return Object.prototype.hasOwnProperty.call(IMAGE_MIME_BY_EXT, fileExt(name));
}

function isHtmlExt(name) {
  const e = fileExt(name);
  return e === "html" || e === "htm";
}

function imageMimeForName(name) {
  return IMAGE_MIME_BY_EXT[fileExt(name)] || "application/octet-stream";
}

// --- Rendered HTML preview: pinned API target ------------------------------
// See docs/superpowers/specs/2026-08-12-webui-preview-pinned-api-target-design.md.
// These live at top level (like isHtmlExt above) so they are reachable from a
// Playwright evaluate: this repo has no JS test runner, and the injection
// position in particular is worth being able to assert directly.

// pullPreviewSlice returns {bytes, total} for one byte range of a file. total is
// the size of the whole file, so a caller can tell a head from a complete read
// without asking fileLs again and racing itself.
async function pullPreviewSlice(taskID, rel, offset, length, fp) {
  const res = await window.harness.filePullBytesRange(taskID, rel, offset, length, fp && fp.onProgress);
  return { bytes: new Uint8Array(res.bytes), total: Number(res.total) };
}

// parsePinnedTarget accepts "host:port" or a bare "port" (host defaults to
// 127.0.0.1, which is what a task's own dev server almost always binds).
// Returns null for anything it cannot read, which is how the caller decides
// not to inject a shim at all.
// cropBottomRow is how many rows of a session's screen a cropped grid cell
// shows above its bottom edge: down to the last row with anything drawn on it,
// or to the cursor's row if that is lower — tui/pane_streamer.go Render's
// rule, so both grids crop the same screen to the same place. Not the
// screen's last row: a short shell at the top of a tall screen would crop to
// blank rows. Not the cursor alone: claude parks it above its newest output.
// lines are the visible rows' text, top first; cursorY is the cursor's row in
// them.
function cropBottomRow(lines, cursorY) {
  let bottom = 0;
  for (let y = lines.length - 1; y >= 0; y--) {
    if (lines[y].trim() !== "") { bottom = y + 1; break; }
  }
  if (cursorY + 1 > bottom) bottom = cursorY + 1;
  return Math.max(1, Math.min(bottom, lines.length));
}

// cropScrollTop is the scroll offset that puts row `bottom` of a terminal
// whose top is termTop px into the scroll content at the bottom of a
// viewHeight-px view. 0 when that row is already in view from the top.
function cropScrollTop(termTop, bottom, rowPx, viewHeight) {
  return Math.max(0, Math.round(termTop + bottom * rowPx - viewHeight));
}

// cropDefault picks a terminal pane's display mode for one opening of the
// grid or the single preview: what the operator last toggled to in that modal
// on this page, else crop on a narrow screen (where
// fitting shrinks text below reading size) and fit on a wide one (where it
// shows whole screens at once).
function cropDefault(narrow, choice) {
  return choice === null || choice === undefined ? !!narrow : !!choice;
}

function parsePinnedTarget(text) {
  const s = String(text || "").trim();
  if (!s) return null;
  let host = "127.0.0.1";
  let portStr = s;
  const idx = s.lastIndexOf(":");
  if (idx >= 0) {
    host = s.slice(0, idx).trim() || "127.0.0.1";
    portStr = s.slice(idx + 1).trim();
  }
  if (!/^\d+$/.test(portStr)) return null;
  const port = Number(portStr);
  if (port < 1 || port > 65535) return null;
  if (/[\s/\\?#@]/.test(host)) return null;
  return { host, port, origin: `http://${host}:${port}` };
}

// normalizePagePath turns what the operator typed into a request target.
// Empty means the site root, which is what "open this server as a page" means
// with nothing else said. A bare "api/payload" gets its slash rather than
// being refused: refusing would surface as a 404 from the far end, which reads
// as the server's problem instead of as a typo.
function normalizePagePath(text) {
  const s = String(text ?? "").trim();
  if (!s) return "/";
  return s.startsWith("/") ? s : "/" + s;
}

// contentTypeOf reads Content-Type out of the [[name, value], ...] list
// httpFetch resolves with. Case-insensitive because the case is whatever the
// far end wrote, and these responses come from servers this repo does not own
// — python's http.server sends "Content-Type", net/http sends "Content-Type",
// and neither is promised by anything.
function contentTypeOf(headers) {
  for (const h of headers || []) {
    if (h && h.length >= 2 && String(h[0]).toLowerCase() === "content-type") {
      return String(h[1]);
    }
  }
  return "";
}

// isHtmlContentType decides whether a response may be handed to an iframe.
// Everything else is shown as text, so opening a JSON endpoint by mistake
// reads as JSON rather than as a blank frame.
function isHtmlContentType(ct) {
  const base = String(ct ?? "").split(";")[0].trim().toLowerCase();
  return base === "text/html" || base === "application/xhtml+xml";
}

// previewHttpLabel is the absolute URL a page preview was fetched from. It is
// both the modal's title and the `rel` the shim reports as the page's own
// identity, so one function keeps those two from drifting apart.
function previewHttpLabel(pin, path) {
  return pin.origin + path;
}

// previewFetchTargetAllowed decides whether a URL the page asked for may be
// tunneled. Relative URLs resolve against the pinned origin so they are always
// in scope; an absolute URL has to match the pin exactly.
//
// This runs in BOTH realms on purpose: inside the iframe so the page gets a
// familiar cross-origin-shaped failure, and again in the parent, where it is
// the actual control. Only the parent's copy is trusted — the page can delete
// the shim and post a message itself.
function previewFetchTargetAllowed(rawURL, pin) {
  if (!pin) return false;
  let u;
  try {
    u = new URL(String(rawURL), pin.origin + "/");
  } catch {
    return false;
  }
  return u.origin === pin.origin;
}

// injectPreviewShim places scriptText so it runs before any of the page's own
// scripts.
//
// Position, in order: after the first <head ...>, else after <html ...>, else
// after <!doctype ...>, else at the very start. NEVER before a doctype —
// displacing it drops the page into quirks mode, so the preview would render
// differently from the real file, which is the worst thing this feature could
// do.
function injectPreviewShim(html, scriptText) {
  const tag = `<script>${scriptText}<\/script>`;
  const at = (re) => {
    const m = re.exec(html);
    return m ? m.index + m[0].length : -1;
  };
  let pos = at(/<head\b[^>]*>/i);
  if (pos < 0) pos = at(/<html\b[^>]*>/i);
  if (pos < 0) pos = at(/<!doctype\b[^>]*>/i);
  if (pos < 0) pos = 0;
  return html.slice(0, pos) + tag + html.slice(pos);
}

// previewShimSource is the text of the shim. It runs inside the iframe's own
// realm, so nothing it holds is a secret and nothing it claims is trusted by
// the parent.
//
// The config is escaped for "<" because it carries a path: a file named with a
// literal </script>, or a URL an operator typed one into, would otherwise
// close the tag it is embedded in.
//
// kind is "html-file" (a file pulled out of the worktree) or "http" (a
// document fetched through the forward). The page can tell them apart because
// they are not interchangeable: a document fetched through the forward reaches
// its own relative paths, while a previewed file reaches nothing unless the
// operator pinned a target by hand.
function previewShimSource(pin, rel, kind) {
  const cfg = JSON.stringify({ origin: pin.origin, host: pin.host, port: pin.port, rel: rel || "" })
    .replace(/</g, "\\u003c");
  const marker = JSON.stringify(kind === "http" ? "http" : "html-file");
  return `(() => {
  "use strict";
  const CFG = ${cfg};
  // The marker is an authoring convenience, not a control: the page shares this
  // realm and could define an identical object itself. Frozen and
  // non-configurable only so the page's own later code cannot clobber it by
  // accident.
  Object.defineProperty(window, "__harness", {
    value: Object.freeze({
      v: 1,
      preview: ${marker},
      rel: CFG.rel,
      api: Object.freeze({ host: CFG.host, port: CFG.port }),
    }),
    writable: false, configurable: false, enumerable: true,
  });

  let seq = 0;
  const pending = new Map();
  window.addEventListener("message", (ev) => {
    if (ev.source !== parent) return;
    const m = ev.data;
    if (!m || m.__harnessFetch !== "reply") return;
    const p = pending.get(m.id);
    if (!p) return;
    pending.delete(m.id);
    if (m.error) p.reject(new TypeError(m.error));
    else p.resolve(m);
  });

  function headerLines(h) {
    const out = [];
    if (!h) return out;
    if (typeof Headers !== "undefined" && h instanceof Headers) {
      h.forEach((v, k) => out.push(k + ": " + v));
      return out;
    }
    // Array BEFORE the plain-object branch: an array of [name, value] pairs
    // also has forEach, and taking that path would emit "0: name,value".
    if (Array.isArray(h)) {
      for (const p of h) if (p && p.length >= 2) out.push(p[0] + ": " + p[1]);
      return out;
    }
    for (const k of Object.keys(h)) out.push(k + ": " + h[k]);
    return out;
  }

  function send(url, init) {
    const id = ++seq;
    return new Promise((resolve, reject) => {
      let u;
      try { u = new URL(String(url), CFG.origin + "/"); }
      catch { reject(new TypeError("Failed to fetch")); return; }
      if (u.origin !== CFG.origin) { reject(new TypeError("Failed to fetch")); return; }
      const raw = init && init.body;
      // A non-string body is refused rather than coerced: String(Uint8Array)
      // yields "1,2,3", which would be sent as a plausible-looking wrong body.
      if (raw != null && typeof raw !== "string") {
        reject(new TypeError("harness preview: only string request bodies are supported"));
        return;
      }
      pending.set(id, { resolve, reject });
      parent.postMessage({
        __harnessFetch: "request",
        id,
        url: u.href,
        method: (init && init.method) || "GET",
        path: u.pathname + u.search,
        headers: headerLines(init && init.headers),
        body: raw == null ? null : raw,
      }, "*");
    });
  }

  window.fetch = (input, init) => {
    const url = (input && typeof input === "object" && "url" in input) ? input.url : input;
    return send(url, init).then((m) => {
      // 204/205/304 are null-body statuses; the Response constructor throws if
      // one is given a body, even an empty one.
      const nullBody = m.status === 204 || m.status === 205 || m.status === 304;
      return new Response(nullBody ? null : m.body, {
        status: m.status,
        statusText: m.statusText,
        headers: m.headers,
      });
    });
  };

  window.XMLHttpRequest = class {
    constructor() {
      this.readyState = 0; this.status = 0; this.statusText = "";
      this.responseText = ""; this.response = "";
      this._h = []; this._resp = [];
    }
    open(method, url) { this._m = method; this._u = url; this.readyState = 1; }
    setRequestHeader(k, v) { this._h.push(k + ": " + v); }
    getResponseHeader(k) {
      const hit = this._resp.find((p) => p[0].toLowerCase() === String(k).toLowerCase());
      return hit ? hit[1] : null;
    }
    getAllResponseHeaders() { return this._resp.map((p) => p[0] + ": " + p[1]).join("\\r\\n"); }
    send(body) {
      send(this._u, { method: this._m, headers: this._h, body }).then((m) => {
        this.status = m.status;
        this.statusText = m.statusText;
        this._resp = m.headers || [];
        this.responseText = new TextDecoder().decode(m.body || new Uint8Array());
        this.response = this.responseText;
        this.readyState = 4;
        if (this.onreadystatechange) this.onreadystatechange();
        if (this.onload) this.onload();
      }).catch(() => {
        this.readyState = 4;
        this.status = 0;
        if (this.onreadystatechange) this.onreadystatechange();
        if (this.onerror) this.onerror();
      });
    }
  };
})();`;
}

// isLikelyBinary sniffs the first 8 KiB: a NUL byte or a high ratio of
// non-text control bytes (outside tab/newline/CR and the printable range)
// marks the content as binary. UTF-8 multibyte sequences (>=0x80) are
// treated as text so non-ASCII source files still render.
function isLikelyBinary(bytes) {
  const n = Math.min(bytes.byteLength, 8 * 1024);
  if (n === 0) return false;
  let suspicious = 0;
  for (let i = 0; i < n; i++) {
    const b = bytes[i];
    if (b === 0) return true;
    const isText = b === 0x09 || b === 0x0a || b === 0x0d || (b >= 0x20 && b <= 0x7e) || b >= 0x80;
    if (!isText) suspicious++;
  }
  return suspicious / n > 0.30;
}

// hexDump formats up to limit bytes as `offset  hex  ASCII` rows of 16.
function hexDump(bytes, limit) {
  const n = Math.min(bytes.byteLength, limit);
  const rows = [];
  for (let off = 0; off < n; off += 16) {
    const end = Math.min(off + 16, n);
    let hex = "";
    let ascii = "";
    for (let i = off; i < end; i++) {
      hex += bytes[i].toString(16).padStart(2, "0") + " ";
      const b = bytes[i];
      ascii += (b >= 0x20 && b <= 0x7e) ? String.fromCharCode(b) : ".";
    }
    rows.push(off.toString(16).padStart(8, "0") + "  " + hex.padEnd(16 * 3, " ") + " " + ascii);
  }
  return rows.join("\n");
}
