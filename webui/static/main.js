"use strict";

const SERVER_CID = location.protocol.startsWith("https")
  ? `wss:${location.hostname}:${location.port || 443}-*`
  : `ws:${location.hostname}:${location.port || 80}-*`;

const POLL_INTERVAL_MS = 5000;

(async () => {
  const status = document.getElementById("status");
  const setStatus = (s, cls) => {
    status.textContent = s;
    status.className = cls || "";
  };

  // 1. Load and start the wasm module.
  const go = new Go();
  setStatus("loading wasm…");
  const result = await WebAssembly.instantiateStreaming(fetch("/static/main.wasm"), go.importObject);
  go.run(result.instance);

  // 2. Wait for window.harness to appear.
  const start = Date.now();
  while (typeof window.harness === "undefined") {
    if (Date.now() - start > 5000) {
      setStatus("wasm timeout", "error");
      return;
    }
    await new Promise(r => setTimeout(r, 50));
  }

  // 3. Connect (options-bag form; persist=true enables auto-reconnect loop).
  const connectedHandlers = [];
  let connectionIsUp = false;

  // Toast popups for incoming notifications (see harness_onNotifyEvent /
  // showToast). Declared here — before onConnectionChange is wired below — so
  // the 'connected' handler can set the grace window without a TDZ throw.
  const TOAST_TTL_MS = 5000;       // info/warn auto-dismiss
  const TOAST_ERROR_TTL_MS = 8000; // error lingers a bit longer (not sticky — feed persists it)
  const TOAST_SUPPRESS_MS = 1500;  // suppress the backlog burst after each (re)connect
  let toastSuppressUntil = 0;      // Date.now() before which incoming events do NOT toast
  function registerOnConnected(fn) {
    connectedHandlers.push(fn);
    // The connection can reach 'connected' during `await harness.connect()`,
    // before the registrations further down in init run — so a handler added
    // after that event would be stranded until the next reconnect. If we're
    // already up, invoke it now so it attaches immediately.
    if (connectionIsUp) {
      try { fn(); } catch (e) { console.error('connected handler (late)', e); }
    }
  }

  function paintBanner(state) {
    const el = document.getElementById('harness-conn-banner');
    if (!el) return;
    el.classList.remove('error', 'online');
    if (state.phase === 'connected') {
      el.textContent = 'connected';
      el.classList.add('online');
      el.hidden = false;
      setTimeout(() => { el.hidden = true; }, 1500);
    } else if (state.phase === 'reconnecting') {
      const secs = state.nextRetryMs ? Math.round(state.nextRetryMs / 1000) : '?';
      el.textContent = `reconnecting (attempt ${state.attempt}, next try in ${secs}s)`;
      el.hidden = false;
    } else if (state.phase === 'closed') {
      el.textContent = state.error ? `disconnected: ${state.error}` : 'disconnected';
      el.classList.add('error');
      el.hidden = false;
    } else {
      el.textContent = `connecting (attempt ${state.attempt})…`;
      el.hidden = false;
    }
  }

  window.harness.onConnectionChange((state) => {
    paintBanner(state);
    if (state.phase === 'connected') {
      setStatus("connected", "connected");
      connectionIsUp = true;
      // The re-subscribe in the handler loop below makes the server replay its
      // backlog ring; hold off toasting until that burst settles.
      toastSuppressUntil = Date.now() + TOAST_SUPPRESS_MS;
      for (const fn of connectedHandlers) {
        try { fn(); } catch (e) { console.error('connected handler', e); }
      }
    } else if (state.phase === 'closed') {
      connectionIsUp = false;
      setStatus("disconnected", "error");
    } else if (state.phase === 'reconnecting') {
      connectionIsUp = false;
      setStatus("reconnecting…");
    }
  });

  // Cap chip state — declared early so the function definitions below can close
  // over them; initCaps() is called once after connect (capList needs no conn
  // but the DOM is always ready by then, which is what renderCaps() touches).
  let capDefs = [];          // [{name:string, bit:number}] — populated by initCaps()
  let spawnCaps = 0;         // bitmask; read by openInteractive / submit on spawn
  let applyCapsOnResume = false; // mirrors #caps-on-resume checkbox; default OFF

  setStatus("connecting…");
  try {
    await window.harness.connect(SERVER_CID, { persist: true });
    setStatus("connected", "connected");
  } catch (e) {
    setStatus(`connect failed: ${e.message}`, "error");
    return;
  }

  // Cap chips: wasm is ready (harness.capList is synchronous) — build the row.
  initCaps();

  // 4. Snapshot polling — single source of truth for runner-select +
  //    runner-list + task-list. Replaces the old refreshList(harness.list)
  //    string-based renderer.
  const runnerSelect = document.getElementById("runner-select");
  const hostSelect   = document.getElementById("host-select");
  const claudeArgs   = document.getElementById("claude-args-input");
  // Single unified task-id field, shared by reattach (target a detached
  // session) and resume (reuse a terminal task's worktree via Submit / Open).
  const taskIdInput  = document.getElementById("task-id-input");
  const runnerList   = document.getElementById("runner-list");
  const taskList     = document.getElementById("task-list");

  // currentClaudeArgs returns the shell-tokenised args from the input box.
  // Reused by submit (cmdline) and Open buttons so the user only edits one field.
  const currentClaudeArgs = () => {
    if (!claudeArgs) return [];
    const raw = claudeArgs.value.trim();
    if (!raw) return [];
    return tokenize(raw);
  };

  // currentResumeTaskID returns the trimmed resume input, or "" when blank.
  // The wasm bridge translates "" to "no resume" before serializing.
  const currentResumeTaskID = () => {
    if (!taskIdInput) return "";
    return taskIdInput.value.trim();
  };

  // File picker DOM refs + state need to exist BEFORE refreshSnapshot()
  // is first awaited, because the very first invocation calls
  // renderFileTaskSelect, which reads fileTaskSelect — a `const` whose
  // temporal dead zone would otherwise be violated. Declaring them up
  // here also lets the setInterval-driven refreshes use the same
  // closures.
  const fileTaskSelect    = document.getElementById("file-task-select");
  const fileCurPathSpan   = document.getElementById("file-cur-path");
  const fileUpBtn         = document.getElementById("file-up-btn");
  const fileRefreshBtn    = document.getElementById("file-refresh-btn");
  const fileEntriesUL     = document.getElementById("file-entries");
  const filePushBtn       = document.getElementById("file-push-btn");
  const filePullBtn       = document.getElementById("file-pull-btn");
  const filePreviewBtn    = document.getElementById("file-preview-btn");
  const fileDeleteBtn     = document.getElementById("file-delete-btn");
  const fileResultPre     = document.getElementById("file-result");
  const filePreviewModal  = document.getElementById("file-preview-modal");
  const filePreviewTitle  = document.getElementById("file-preview-title");
  const filePreviewBody   = document.getElementById("file-preview-body");
  const filePreviewClose  = document.getElementById("file-preview-close");

  // Preview never pulls more than this into browser memory; oversize files
  // are rejected up front using the size from fileLs (no fetch attempted).
  const PREVIEW_MAX_BYTES = 1 * 1024 * 1024; // 1 MiB
  // Binary files are rendered as a hex dump truncated to this many bytes.
  const HEX_PREVIEW_MAX_BYTES = 4 * 1024;    // 4 KiB
  // Object URL held open for an image preview; revoked when the modal closes.
  let filePreviewObjectURL = null;

  let filePickerCurDir   = "";
  let filePickerEntries  = [];
  let filePickerSelected = null; // {name, size, mode, isDir} or null

  // Terminal (finished) task states; gates Resume vs Cancel in the action sheet.
  const TERMINAL_STATES = new Set(["Succeeded", "Failed", "Cancelled"]);

  const refreshSnapshot = async () => {
    let snap;
    try {
      snap = await window.harness.snapshot();
    } catch (e) {
      taskList.textContent = `snapshot error: ${e.message}`;
      return;
    }
    // The server-side snapshot iterates a registry map whose Go iteration
    // order is randomized, so consecutive polls return the same runners in
    // a different sequence — visibly shuffling the list / dropdown options
    // on every refresh. Sort once here on a stable key composed of
    // (hostname asc, connectedAt asc, joined roots asc) so the three
    // render functions below all observe the same stable ordering.
    const sortedRunners = sortRunners(snap.runners || []);
    renderRunnerSelect(runnerSelect, sortedRunners);
    renderHostSelect(hostSelect, sortedRunners);
    runnerList.textContent = renderRunners(sortedRunners);
    renderTaskList(snap.tasks);
    renderFileTaskSelect(snap.tasks);
  };
  await refreshSnapshot();
  setInterval(refreshSnapshot, POLL_INTERVAL_MS);

  function renderFileTaskSelect(tasks) {
    const prev = fileTaskSelect.value;
    fileTaskSelect.innerHTML = "";
    const placeholder = document.createElement("option");
    placeholder.value = "";
    placeholder.textContent = "(select task)";
    fileTaskSelect.appendChild(placeholder);
    if (!tasks) return;
    for (const t of tasks) {
      const opt = document.createElement("option");
      opt.value = t.id;
      const short = (t.id || "").slice(0, 12);
      opt.textContent = `${short}  ${t.status}  ${t.repoPath}`;
      fileTaskSelect.appendChild(opt);
    }
    if (prev) fileTaskSelect.value = prev; // preserve selection across refresh
    updateFilePickerButtons();
  }

  function updateFilePickerButtons() {
    const hasTask = !!fileTaskSelect.value;
    const hasSel = filePickerSelected !== null;
    fileUpBtn.disabled = !hasTask || filePickerCurDir === "";
    fileRefreshBtn.disabled = !hasTask;
    filePushBtn.disabled = !hasTask;
    filePullBtn.disabled = !hasTask || !hasSel || filePickerSelected.isDir;
    filePreviewBtn.disabled = !hasTask || !hasSel || filePickerSelected.isDir;
    fileDeleteBtn.disabled = !hasTask || !hasSel;
  }

  async function refreshFilePicker() {
    if (!fileTaskSelect.value) {
      filePickerEntries = [];
      filePickerSelected = null;
      fileCurPathSpan.textContent = "/";
      fileEntriesUL.innerHTML = "";
      updateFilePickerButtons();
      return;
    }
    const taskID = fileTaskSelect.value;
    fileCurPathSpan.textContent = "/" + filePickerCurDir;
    try {
      const entries = await window.harness.fileLs(taskID, filePickerCurDir);
      filePickerEntries = entries.slice().sort((a, b) => {
        if (a.isDir !== b.isDir) return a.isDir ? -1 : 1;
        return a.name < b.name ? -1 : (a.name > b.name ? 1 : 0);
      });
      filePickerSelected = null;
      renderFileEntries();
      updateFilePickerButtons();
    } catch (e) {
      fileResultPre.textContent = `ls error: ${e.message}`;
    }
  }

  function renderFileEntries() {
    fileEntriesUL.innerHTML = "";
    if (filePickerEntries.length === 0) {
      const li = document.createElement("li");
      li.textContent = "(empty)";
      li.style.color = "#888";
      li.style.padding = "0.25em 0.5em";
      fileEntriesUL.appendChild(li);
      return;
    }
    for (const e of filePickerEntries) {
      const li = document.createElement("li");
      const sz = e.isDir ? "" : String(e.size).padStart(10);
      const name = e.isDir ? `${e.name}/` : e.name;
      li.textContent = `${sz}  ${name}`;
      li.style.padding = "0.15em 0.5em";
      li.style.cursor = "pointer";
      if (e.isDir) li.style.color = "#06c";
      li.addEventListener("click", () => {
        if (e.isDir) {
          // Descend
          filePickerCurDir = joinFsPath(filePickerCurDir, e.name);
          refreshFilePicker();
          return;
        }
        // Select (clear prior highlight, set this one)
        for (const c of fileEntriesUL.children) {
          c.style.backgroundColor = "";
        }
        li.style.backgroundColor = "#ffeb3b";
        filePickerSelected = e;
        updateFilePickerButtons();
      });
      fileEntriesUL.appendChild(li);
    }
  }

  function joinFsPath(a, b) {
    a = (a || "").replace(/\/+$/, "");
    b = (b || "").replace(/^\/+/, "");
    if (!a) return b;
    if (!b) return a;
    return `${a}/${b}`;
  }

  function parentFsPath(p) {
    p = (p || "").replace(/\/+$/, "");
    const i = p.lastIndexOf("/");
    if (i < 0) return "";
    return p.slice(0, i);
  }

  fileTaskSelect.addEventListener("change", () => {
    filePickerCurDir = "";
    filePickerSelected = null;
    refreshFilePicker();
  });
  fileUpBtn.addEventListener("click", () => {
    if (!filePickerCurDir) return;
    filePickerCurDir = parentFsPath(filePickerCurDir);
    refreshFilePicker();
  });
  fileRefreshBtn.addEventListener("click", refreshFilePicker);

  filePushBtn.addEventListener("click", async () => {
    const taskID = fileTaskSelect.value;
    if (!taskID) return;
    const file = await pickLocalFile();
    if (!file) {
      fileResultPre.textContent = "push cancelled (no file)";
      return;
    }
    const buf = new Uint8Array(await file.arrayBuffer());
    const remoteRel = joinFsPath(filePickerCurDir, file.name);
    try {
      await window.harness.filePushBytes(taskID, remoteRel, buf, false);
      fileResultPre.textContent = `push ok: ${file.name} -> ${remoteRel} (${buf.byteLength} bytes)`;
    } catch (e) {
      if (e && e.code === "already_exists") {
        if (!window.confirm(`${remoteRel} already exists on the runner. Overwrite?`)) {
          fileResultPre.textContent = "push cancelled (overwrite declined)";
          return;
        }
        try {
          await window.harness.filePushBytes(taskID, remoteRel, buf, true);
          fileResultPre.textContent = `push ok (overwritten): ${file.name} -> ${remoteRel} (${buf.byteLength} bytes)`;
        } catch (e2) {
          fileResultPre.textContent = `push error: ${e2.message}`;
          return;
        }
      } else {
        fileResultPre.textContent = `push error: ${e.message}`;
        return;
      }
    }
    refreshFilePicker();
  });

  filePullBtn.addEventListener("click", async () => {
    const taskID = fileTaskSelect.value;
    if (!taskID || !filePickerSelected || filePickerSelected.isDir) return;
    const rel = joinFsPath(filePickerCurDir, filePickerSelected.name);
    try {
      const bytes = await window.harness.filePullBytes(taskID, rel);
      triggerDownload(bytes, filePickerSelected.name);
      fileResultPre.textContent = `pull ok: ${rel} (${bytes.byteLength} bytes) — browser save dialog`;
    } catch (e) {
      fileResultPre.textContent = `pull error: ${e.message}`;
    }
  });

  filePreviewBtn.addEventListener("click", async () => {
    const taskID = fileTaskSelect.value;
    if (!taskID || !filePickerSelected || filePickerSelected.isDir) return;
    const sel = filePickerSelected;
    const rel = joinFsPath(filePickerCurDir, sel.name);
    // Reject oversize before fetching — sel.size comes from fileLs, so we
    // never pull a huge file into browser memory just to refuse it.
    if (sel.size > PREVIEW_MAX_BYTES) {
      openFilePreview(rel, sel.size, null,
        `File is too large to preview (${sel.size} bytes, limit ${PREVIEW_MAX_BYTES}). Use Pull to download it.`);
      return;
    }
    try {
      const bytes = await window.harness.filePullBytes(taskID, rel);
      renderFilePreview(rel, sel.size, sel.name, bytes);
    } catch (e) {
      openFilePreview(rel, sel.size, null, `preview error: ${e.message}`);
    }
  });

  filePreviewClose.addEventListener("click", () => filePreviewModal.close());
  // Backdrop click (the dialog element itself, outside its content) closes.
  filePreviewModal.addEventListener("click", (ev) => {
    if (ev.target === filePreviewModal) filePreviewModal.close();
  });
  // Esc-triggered native close also needs to release any image object URL.
  filePreviewModal.addEventListener("close", () => {
    if (filePreviewObjectURL) {
      URL.revokeObjectURL(filePreviewObjectURL);
      filePreviewObjectURL = null;
    }
  });

  // openFilePreview shows the modal with a header and a body built from the
  // given DOM node (or a plain note string for errors / oversize messages).
  function openFilePreview(rel, size, bodyNode, note) {
    if (filePreviewObjectURL) {
      URL.revokeObjectURL(filePreviewObjectURL);
      filePreviewObjectURL = null;
    }
    filePreviewTitle.textContent = `${rel}  (${size} bytes)`;
    filePreviewBody.innerHTML = "";
    if (bodyNode) {
      filePreviewBody.appendChild(bodyNode);
    }
    if (note) {
      const p = document.createElement("p");
      p.className = "preview-note";
      p.textContent = note;
      filePreviewBody.appendChild(p);
    }
    if (!filePreviewModal.open) filePreviewModal.showModal();
  }

  // renderFilePreview picks a renderer based on extension (images) and a
  // byte sniff (text vs binary), then opens the modal.
  function renderFilePreview(rel, size, name, bytes) {
    if (isImageExt(name)) {
      const blob = new Blob([bytes], { type: imageMimeForName(name) });
      filePreviewObjectURL = URL.createObjectURL(blob);
      const img = document.createElement("img");
      img.src = filePreviewObjectURL;
      img.alt = name;
      openFilePreview(rel, size, img, null);
      return;
    }
    if (isLikelyBinary(bytes)) {
      const pre = document.createElement("pre");
      pre.textContent = hexDump(bytes, HEX_PREVIEW_MAX_BYTES);
      const truncated = bytes.byteLength > HEX_PREVIEW_MAX_BYTES;
      openFilePreview(rel, size, pre,
        truncated ? `binary — showing first ${HEX_PREVIEW_MAX_BYTES} of ${bytes.byteLength} bytes` : "binary");
      return;
    }
    const pre = document.createElement("pre");
    pre.textContent = new TextDecoder("utf-8", { fatal: false }).decode(bytes);
    openFilePreview(rel, size, pre, null);
  }

  fileDeleteBtn.addEventListener("click", async () => {
    const taskID = fileTaskSelect.value;
    if (!taskID || !filePickerSelected) return;
    const rel = joinFsPath(filePickerCurDir, filePickerSelected.name);
    const isDir = filePickerSelected.isDir;
    let recursive = false, force = false;
    if (isDir) {
      if (!window.confirm(`Delete directory ${rel} recursively (rm -rf)?`)) {
        fileResultPre.textContent = "delete cancelled";
        return;
      }
      recursive = true;
      force = true;
    } else {
      if (!window.confirm(`Delete ${rel}?`)) {
        fileResultPre.textContent = "delete cancelled";
        return;
      }
    }
    try {
      await window.harness.fileDelete(taskID, rel, recursive, force);
      fileResultPre.textContent = `delete ok: ${rel}`;
      filePickerSelected = null;
      refreshFilePicker();
    } catch (e) {
      fileResultPre.textContent = `delete error: ${e.message}`;
    }
  });

  // 5. Cmdline submit / cancel / prune.
  const cmdInput  = document.getElementById("cmd-input");
  const cmdRun    = document.getElementById("cmd-run");
  const cmdOutput = document.getElementById("cmd-output");

  // 6. Watch (server push). Registered after cmdOutput is in scope so the
  //    handler can append into it. On any push we trigger an extra snapshot
  //    refresh so the UI reflects the latest state without waiting for the
  //    next poll tick.
  //    Re-registered via registerOnConnected so the watch re-attaches to the
  //    new live client each time the persist loop reconnects.
  window.harness_onTaskEvent = (jsonStr) => {
    try {
      const evt = JSON.parse(jsonStr);
      const banner = `[${new Date().toISOString()}] ${evt.line}`;
      appendCmdOutput(banner);
    } catch (e) { /* ignore */ }
    refreshSnapshot();
  };
  registerOnConnected(() => {
    window.harness.watch().catch(e => console.error("watch:", e));
  });

  // Notification feed: window.harness_onNotifyEvent receives one raw JSON
  // object per event from the wasm notifyPipe.  ts is unix seconds.
  //
  // Dedup: the server replays its backlog ring (server/notify_ring.go, cap 64)
  // to EVERY new subscriber — including the re-subscribe that happens after a
  // reconnect — so recent events would re-render as duplicates. NotifyEvent
  // carries no unique id (ts is only seconds), so we key on content. Events
  // that genuinely arrived while disconnected have unseen keys and still render;
  // only an already-shown event is suppressed. Same-second byte-identical events
  // collapse to one, which is acceptable (they are indistinguishable anyway).
  const seenNotify = new Set();
  const seenNotifyOrder = [];
  const SEEN_NOTIFY_MAX = 512; // > ring cap (64) and feed cap (200)
  const notifyKey = (e) =>
    JSON.stringify([e.ts, e.level, e.origin, e.hostname, e.task_id, e.title, e.text]);
  // notifyParts derives the display fields shared by the feed entry and the
  // toast popup, so the two renderings never diverge.
  function notifyParts(e) {
    const lvl = e.level || "info";
    const time = new Date((e.ts ? e.ts * 1000 : Date.now())).toLocaleTimeString();
    // "title — text" with both; just one side alone — no dangling separator.
    let body = e.title || "";
    if (e.text) body = body ? `${body} — ${e.text}` : e.text;
    let src = e.hostname ? `${e.origin || ""}@${e.hostname}` : (e.origin || "");
    if (e.task_id) src += " · " + String(e.task_id); // full id, copy-pasteable
    return { lvl, time, body, src };
  }

  // showToast pops a transient copy of an incoming notification: top-right on
  // desktop, a top banner on mobile (style.css). Tap → reveal the 通知 feed
  // (where the entry is actionable); ✕ or auto-dismiss closes it. All levels
  // auto-dismiss (the feed is the persistent record); error just lingers longer.
  function showToast(e) {
    const host = document.getElementById("toast-host");
    if (!host) return;
    const { lvl, time, body, src } = notifyParts(e);
    const t = document.createElement("div");
    t.className = "toast notify-level-" + lvl;
    if (lvl === "error") t.setAttribute("role", "alert");

    const head = document.createElement("div");
    head.className = "notify-head";
    const badge = document.createElement("span");
    badge.className = "notify-badge";
    badge.textContent = lvl.toUpperCase();
    const tEl = document.createElement("span");
    tEl.className = "notify-time";
    tEl.textContent = time;
    const x = document.createElement("button");
    x.type = "button";
    x.className = "toast-close";
    x.textContent = "✕";
    x.addEventListener("click", (ev) => { ev.stopPropagation(); dismissToast(t); });
    head.append(badge, tEl, x);

    const bodyEl = document.createElement("div");
    bodyEl.className = "notify-body";
    bodyEl.textContent = body || "(no body)";
    t.append(head, bodyEl);
    if (src) {
      const metaEl = document.createElement("div");
      metaEl.className = "notify-meta";
      metaEl.textContent = src;
      t.append(metaEl);
    }

    t.addEventListener("click", () => {
      document.querySelector('.tab-btn[data-tab="notify"]')?.click(); // mobile: switch tab
      if (!window.matchMedia("(max-width: 600px)").matches) {
        document.getElementById("notifications")?.scrollIntoView({ behavior: "smooth", block: "start" });
      }
      dismissToast(t);
    });

    host.appendChild(t);
    // Cap the visible stack; drop the oldest beyond the cap (fewer on mobile).
    const cap = window.matchMedia("(max-width: 600px)").matches ? 2 : 4;
    while (host.children.length > cap) host.removeChild(host.firstChild);
    setTimeout(() => dismissToast(t), lvl === "error" ? TOAST_ERROR_TTL_MS : TOAST_TTL_MS);
  }

  function dismissToast(t) {
    if (!t.isConnected) return;
    t.classList.add("toast-leaving");
    setTimeout(() => t.remove(), 200); // matches the CSS leave transition
  }

  // --- Notification sound (Web Audio synth beep) ------------------------------
  // A short blip on each live notification, gated like the toast. Browsers block
  // audio until the page has seen a user gesture, so we lazily create + resume an
  // AudioContext and also unlock it on the first pointer/key event. Toggle is
  // persisted in localStorage (default on); iOS may still mute via the ring
  // switch — that's a platform limit, not something we can override.
  const SOUND_KEY = "harness.notifySound";
  const soundEnabled = () => localStorage.getItem(SOUND_KEY) !== "off"; // default on
  let audioCtx = null;
  function ensureAudio() {
    if (!audioCtx) {
      const AC = window.AudioContext || window.webkitAudioContext;
      if (!AC) return null;
      try { audioCtx = new AC(); } catch (_) { return null; }
    }
    if (audioCtx.state === "suspended") audioCtx.resume().catch(() => {});
    return audioCtx;
  }
  window.addEventListener("pointerdown", ensureAudio, { once: true });
  window.addEventListener("keydown", ensureAudio, { once: true });
  function playBeep() {
    const ctx = ensureAudio();
    if (!ctx || ctx.state !== "running") return; // not unlocked yet (no gesture)
    const t0 = ctx.currentTime;
    const osc = ctx.createOscillator();
    const gain = ctx.createGain();
    osc.type = "sine";
    osc.frequency.setValueAtTime(880, t0);
    osc.frequency.setValueAtTime(1175, t0 + 0.08); // two-note "ding"
    gain.gain.setValueAtTime(0.0001, t0);
    gain.gain.exponentialRampToValueAtTime(0.16, t0 + 0.01); // attack (ramp avoids click)
    gain.gain.exponentialRampToValueAtTime(0.0001, t0 + 0.25); // decay
    osc.connect(gain).connect(ctx.destination);
    osc.start(t0);
    osc.stop(t0 + 0.26);
  }

  // maybeToast gates the toast + sound by the post-connect grace window so the
  // backlog ring replay (server/notify_replay.go) doesn't fire a burst on
  // connect / reconnect. Already-seen events never reach here (deduped upstream).
  function maybeToast(e) {
    if (Date.now() < toastSuppressUntil) return;
    showToast(e);
    if (soundEnabled()) playBeep();
  }

  window.harness_onNotifyEvent = (jsonStr) => {
    try {
      const e = JSON.parse(jsonStr);
      const key = notifyKey(e);
      if (seenNotify.has(key)) return; // already shown (e.g. backlog replay after reconnect)
      seenNotify.add(key);
      seenNotifyOrder.push(key);
      if (seenNotifyOrder.length > SEEN_NOTIFY_MAX) seenNotify.delete(seenNotifyOrder.shift());
      const feed = document.getElementById("notify-feed");
      if (!feed) return;
      const { lvl, time, body, src } = notifyParts(e);

      // Structured entry: colored level badge + short time, prominent message,
      // muted source/task-id below. Color-coding + spacing are in style.css.
      const entry = document.createElement("div");
      entry.className = "notify-entry notify-level-" + lvl;
      const head = document.createElement("div");
      head.className = "notify-head";
      const badge = document.createElement("span");
      badge.className = "notify-badge";
      badge.textContent = lvl.toUpperCase();
      const tEl = document.createElement("span");
      tEl.className = "notify-time";
      tEl.textContent = time;
      head.append(badge, tEl);
      const bodyEl = document.createElement("div");
      bodyEl.className = "notify-body";
      bodyEl.textContent = body || "(no body)";
      entry.append(head, bodyEl);
      if (src) {
        const metaEl = document.createElement("div");
        metaEl.className = "notify-meta";
        metaEl.textContent = src;
        entry.append(metaEl);
      }

      // A worker-origin notification carries a task id — make the entry tappable
      // to reattach to / resume that task from the feed (WebUI only). The action
      // is gated by the task's CURRENT state, looked up at tap time (the status
      // may have changed since the notification): a live interactive session →
      // Reattach; a terminal task → Resume; same gating as the task sheet.
      if (e.task_id) {
        const taskID = String(e.task_id);
        entry.classList.add("notify-actionable");
        const actions = document.createElement("div");
        actions.className = "notify-actions";
        actions.hidden = true;
        entry.append(actions);
        entry.addEventListener("click", async () => {
          if (!actions.hidden) { actions.hidden = true; return; } // tap again closes
          actions.replaceChildren();
          let t = null;
          try {
            const snap = await window.harness.snapshot();
            t = (snap.tasks || []).find((x) => x.id === taskID);
          } catch (_) { /* snapshot unavailable — fall through to unknown */ }
          const mkBtn = (label, fn) => {
            const b = document.createElement("button");
            b.type = "button";
            b.className = "notify-action-btn";
            b.textContent = label;
            b.addEventListener("click", (ev) => { ev.stopPropagation(); actions.hidden = true; fn(taskID); });
            actions.appendChild(b);
          };
          const live = t && t.kind === "Interactive" && (t.status === "Running" || t.status === "Detached");
          const terminal = t && TERMINAL_STATES.has(t.status);
          if (live) {
            mkBtn("↪ Reattach", (id) => reattachTo(id, false));
            mkBtn("👁 View", (id) => reattachTo(id, true));
          }
          if (terminal) mkBtn("▶ Resume", resumeTaskById);
          if (!t) { // not in the snapshot (pruned/unknown) — offer both as a fallback
            mkBtn("↪ Reattach", (id) => reattachTo(id, false));
            mkBtn("👁 View", (id) => reattachTo(id, true));
            mkBtn("▶ Resume", resumeTaskById);
          }
          if (!actions.childElementCount) { // known, but neither applies (e.g. a running one-shot)
            const note = document.createElement("span");
            note.className = "notify-meta";
            note.textContent = `(${t.status} ${t.kind} — no reattach/resume)`;
            actions.appendChild(note);
          }
          actions.hidden = false;
        });
      }

      // Chronological order (oldest top, newest bottom). Auto-scroll to the
      // newest only if the user was already at the bottom.
      const atBottom = feed.scrollHeight - feed.scrollTop - feed.clientHeight < 4;
      feed.appendChild(entry);
      // Cap feed at 200 entries (drop the oldest from the top).
      while (feed.children.length > 200) feed.removeChild(feed.firstChild);
      if (atBottom) feed.scrollTop = feed.scrollHeight;
      maybeToast(e); // pop a transient toast for live events (grace-gated)
    } catch (_) {}
  };
  registerOnConnected(() => {
    window.harness.watchNotifications().catch(e => console.error("watchNotifications:", e));
  });

  // Notification send form.
  const notifySend = document.getElementById("notify-send");
  const notifyResult = document.getElementById("notify-result");
  if (notifySend && notifyResult) {
    notifySend.addEventListener("click", async () => {
      const level = (document.getElementById("notify-level") || {}).value || "info";
      const title = (document.getElementById("notify-title") || {}).value || "";
      const text  = (document.getElementById("notify-text")  || {}).value || "";
      notifyResult.textContent = "sending…";
      try {
        await window.harness.sendNotification({ level, title, text });
        notifyResult.textContent = "sent";
      } catch (e) {
        notifyResult.textContent = `error: ${e.message}`;
      }
    });
  }

  // Notification-sound on/off (persisted; default on). Toggling is a user
  // gesture, so enabling also unlocks the AudioContext and previews the beep.
  const soundToggle = document.getElementById("notify-sound");
  if (soundToggle) {
    soundToggle.checked = soundEnabled();
    soundToggle.addEventListener("change", () => {
      localStorage.setItem(SOUND_KEY, soundToggle.checked ? "on" : "off");
      if (soundToggle.checked) playBeep();
    });
  }

  // appendCmdOutput appends a line to the cmd-output history pane
  // (newest at the bottom, terminal-style) and scrolls the pane / page
  // so the new entry is visible. Caps the buffer at MAX_OUTPUT_LINES
  // by dropping the oldest entries.
  const MAX_OUTPUT_LINES = 2000;
  const appendCmdOutput = (text, scroll = false) => {
    const cur = cmdOutput.textContent;
    let next = cur === "" ? text : cur + "\n" + text;
    const lines = next.split("\n");
    if (lines.length > MAX_OUTPUT_LINES) {
      next = lines.slice(lines.length - MAX_OUTPUT_LINES).join("\n");
    }
    cmdOutput.textContent = next;
    // Always keep the pane's own tail visible (harmless, in-element scroll).
    cmdOutput.scrollTop = cmdOutput.scrollHeight;
    // Only scroll the *page* to the pane when the user ran a command — doing it
    // for background appends (task events, takeover notices) yanks the page
    // (e.g. jumps to the top on desktop, where #cmdline sits above the terminal).
    if (scroll) cmdOutput.scrollIntoView({ block: "end", behavior: "auto" });
  };

  const runCmd = async () => {
    const line = cmdInput.value.trim();
    if (!line) return;
    cmdInput.value = "";
    appendCmdOutput(`> ${line}`, true);
    try {
      const tokens = tokenize(line);   // quote-aware
      const cmd = tokens[0];
      let out;
      switch (cmd) {
        case "submit": {
          const repo = runnerSelect.value || "";
          const resumeTaskId = currentResumeTaskID();
          // repo is optional on resume — server uses the existing task's
          // RepoPath. Reject only when neither is supplied.
          if (!repo && !resumeTaskId) {
            throw new Error("no runner selected (pick one from the dropdown, or fill in Resume task id)");
          }
          // Everything after `submit` is the task prompt. We join the
          // tokenize() result with single spaces — quoted segments have
          // already been collapsed into single tokens, so a multi-word
          // task is preserved verbatim.
          const task = tokens.slice(1).join(" ");
          if (!task) throw new Error("submit: missing task prompt");
          const host = hostSelect ? (hostSelect.value || "") : "";
          const claudeArgsList = currentClaudeArgs();
          out = await window.harness.submit({ repo, task, host, claudeArgs: claudeArgsList, resumeTaskId, caps: spawnCaps, resumeCapsOverride: resumeTaskId ? applyCapsOnResume : false });
          break;
        }
        case "list":
          // Force a snapshot refresh, then echo the rendered task rows
          // (newline-joined) into cmd-output.
          await refreshSnapshot();
          out = Array.from(taskList.querySelectorAll(".task-row"))
                  .map(r => r.textContent).join("\n") || "(none)";
          break;
        case "cancel":
          if (!tokens[1]) throw new Error("cancel: missing task id");
          await window.harness.cancel(tokens[1]);
          out = "cancelled";
          break;
        case "prune": {
          const flags = parseFlags(tokens.slice(1));
          out = await window.harness.prune({ before: flags.before || "168h" });
          break;
        }
        case "file": {
          out = await runFileCmd(tokens.slice(1));
          break;
        }
        case "server": {
          if (tokens[1] !== "dial-runner") {
            throw new Error(`server: unknown subcommand ${tokens[1] || "(empty)"} (try: dial-runner)`);
          }
          let via = null, target = null;
          for (let i = 2; i < tokens.length; i++) {
            const t = tokens[i];
            if (t === "--via") {
              i++;
              if (i >= tokens.length) throw new Error("--via: missing CID");
              via = tokens[i];
            } else if (t.startsWith("--via=")) {
              via = t.slice("--via=".length);
            } else if (!target) {
              target = t;
            } else {
              throw new Error(`unexpected arg: ${t}`);
            }
          }
          if (!target) throw new Error("server dial-runner: missing runner CID");
          const status = await window.harness.serverDialRunner(target, via || undefined);
          out = `server dial-runner ${target}${via ? ` --via=${via}` : ""}: ${status}`;
          break;
        }
        case "help":
          out = [
            "commands:",
            "  submit <prompt...>        submit task (use repo dropdown / Resume task id)",
            "  list                      refresh the snapshot",
            "  cancel <task-id>          cancel a task",
            "  prune [--before=DUR]      forget terminal tasks older than DUR",
            "  file ls <task> [rel]      list a worktree directory",
            "  file delete [-r] [-f] <task> <rel>",
            "                            remove a file (no -r) or directory (-r [-f])",
            "  file push <task> <rel>    upload a local file (file picker opens)",
            "  file pull <task> <rel>    download a remote file (browser save dialog)",
            "  server dial-runner <cid> [--via <cid>]",
            "                            ask the server to reverse-dial a Listen-mode runner; --via routes through a registered relay-runner",
            "  help                      this list",
          ].join("\n");
          break;
        default:
          out = `unknown command: ${cmd} (type 'help' for the list)`;
      }
      appendCmdOutput(out, true);
      refreshSnapshot();
    } catch (e) {
      appendCmdOutput(`error: ${e.message}`, true);
    }
  };
  cmdRun.addEventListener("click", runCmd);
  cmdInput.addEventListener("keydown", (e) => { if (e.key === "Enter") runCmd(); });

  // 7. Interactive PTY.
  // Explicit monospace stack: generic `monospace` rendered soft under the browser/OS
  // anti-aliasing. fontSize stays 13 — 14 cut the column count enough to break TUI
  // layouts (Claude Code's box-drawing) that fit at 13.
  const term = new Terminal({
    convertEol: true,
    fontSize: 13,
    fontFamily: '"Cascadia Mono", "JetBrains Mono", "DejaVu Sans Mono", "Liberation Mono", Menlo, Consolas, "Courier New", monospace',
  });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open(document.getElementById("terminal"));
  fit.fit();
  window.harness_xtermWrite = (uint8Array) => term.write(uint8Array);

  // --- Mobile tab switching (active only at <=600px via CSS). On desktop
  //     this only sets a body data-attr; the media query makes it a no-op. ---
  const tabbar = document.getElementById("tabbar");
  const interactiveSection = document.getElementById("interactive");
  const vv = window.visualViewport;

  // fitTerminalToViewport sizes the terminal tab to the *visual* viewport, so
  // the terminal AND its touch-key bar both stay above the on-screen keyboard.
  // iOS/Android overlay the keyboard over content (dvh does NOT shrink), which
  // otherwise leaves the in-flow bar — and the lines you're typing — hidden
  // behind it. Pinning the section height to vv.height keeps everything above
  // the keyboard with the bar resting on the keyboard's top edge. No-op (clears
  // the inline height, falling back to the CSS dvh rule) on desktop / off the
  // terminal tab / when visualViewport is unavailable.
  let lastTermHeight = "", lastCols = 0, lastRows = 0;
  const fitTerminalToViewport = () => {
    const onTerminal = window.matchMedia("(max-width: 600px)").matches
      && document.body.dataset.activeTab === "terminal";
    if (!vv || !onTerminal) {
      if (interactiveSection.style.height) { interactiveSection.style.height = ""; lastTermHeight = ""; }
      return;
    }
    const top = interactiveSection.getBoundingClientRect().top - vv.offsetTop;
    const h = Math.max(120, vv.height - top) + "px";
    // Skip when the height is unchanged. Without this, re-applying the same
    // height can re-fire a visualViewport scroll/resize and spin a per-frame
    // fit loop that pegs the main thread.
    if (h === lastTermHeight) return;
    lastTermHeight = h;
    interactiveSection.style.height = h;
    try { fit.fit(); } catch (_) { /* not laid out yet */ }
    // Only tell the PTY when the grid actually changed. Pixel-level keyboard
    // open/close animation yields dozens of identical-dimension fits per
    // toggle; sending a resize frame for each floods and eventually wedges the
    // interactive stream (symptom: terminal freezes until a reattach opens a
    // fresh stream).
    if (term.cols !== lastCols || term.rows !== lastRows) {
      lastCols = term.cols; lastRows = term.rows;
      window.harness.resizeInteractive({ cols: term.cols, rows: term.rows });
    }
  };
  // Coalesce the burst of visualViewport events (keyboard open/close, URL-bar
  // show/hide, scroll) into one fit per frame.
  let vvRAF = 0;
  const onVVChange = () => {
    if (vvRAF) return;
    vvRAF = requestAnimationFrame(() => { vvRAF = 0; fitTerminalToViewport(); });
  };
  if (vv) {
    vv.addEventListener("resize", onVVChange);
    vv.addEventListener("scroll", onVVChange);
  }

  const setActiveTab = (name) => {
    const mobile = window.matchMedia("(max-width: 600px)").matches;
    document.body.dataset.activeTab = name;
    for (const b of tabbar.querySelectorAll(".tab-btn")) {
      b.classList.toggle("is-active", b.dataset.tab === name);
    }
    // Reset scroll so the newly-shown tab starts from the top. Only when the
    // tab UI is actually live (<=600px); on desktop all sections show at once
    // and a tap on a task action shouldn't jump the page.
    if (mobile) window.scrollTo(0, 0);
    // Size (or release) the terminal tab to the visible viewport; this also
    // re-fits the grid that went stale while the tab was display:none.
    fitTerminalToViewport();
    // On desktop there are no tabs — the section lives below the controls, so
    // activating it should scroll the page down to it; otherwise the user has
    // to scroll manually to see what they just opened. Applies to the terminal
    // (Open / Reattach / Resume) and to the file picker (📁 ファイル), both of
    // which sit below the fold.
    if (!mobile && (name === "terminal" || name === "files")) {
      const target = name === "terminal"
        ? interactiveSection
        : document.querySelector(`[data-tabgroup="files"]`);
      if (target) target.scrollIntoView({ behavior: "smooth", block: "start" });
    }
    // Intentionally NOT focusing the terminal here: focusing pops the soft
    // keyboard on mobile every time you merely switch to the terminal tab to
    // read output, and adds keyboard-toggle churn. The open / reattach / resume
    // paths focus explicitly when you actually intend to type; otherwise tap
    // the terminal to focus.
  };
  tabbar.addEventListener("click", (e) => {
    const btn = e.target.closest(".tab-btn");
    if (btn) setActiveTab(btn.dataset.tab);
  });
  // Land on the task list on first connect — no session exists yet, so the
  // empty terminal isn't a useful default.
  setActiveTab("tasks");

  // scrollTermToBottom pins the viewport to the latest output. Called after
  // Reattach, whose replay otherwise leaves the viewport scrolled up. Triple
  // call (now + next frame + 120ms) catches async replay frames arriving via
  // recvPump after attachSession resolves. No-op/harmless in alt-screen apps.
  const scrollTermToBottom = () => {
    term.scrollToBottom();
    requestAnimationFrame(() => term.scrollToBottom());
    setTimeout(() => term.scrollToBottom(), 120);
  };

  // harness_onInteractiveClosed fires (from wasm) when the active session ends
  // from the far side: another client took it over, or the session itself
  // exited. We leave the terminal completely untouched (no marker write, no
  // clear) — its output stays intact for debugging — and surface the event only
  // via the attached indicator and the command log. A snapshot tells the two
  // cases apart: a still-running task means we were taken over; a terminal/
  // absent task means the session ended.
  // attachEpoch bumps on every (re)attach / open. The close handler below awaits
  // a snapshot; if the user (re)attaches during that await, the epoch changes
  // and the handler must NOT clobber the now-correct "attached" display.
  let attachEpoch = 0;
  // pendingReattachTaskID is the id of a session whose interactive stream
  // closed while it (probably) stayed alive on the runner — i.e. the quick-
  // reattach button is/should be offered. It is maintained by show/
  // hideQuickReattach and re-verified by the registerOnConnected handler below
  // after the persist loop reconnects.
  let pendingReattachTaskID = null;
  window.harness_onInteractiveClosed = async (taskID) => {
    const myEpoch = attachEpoch;
    let kind = "切断 (takeover またはセッション終了)";
    let reattachable = false;
    try {
      const snap = await window.harness.snapshot();
      const t = (snap.tasks || []).find(x => x.id === taskID);
      if (t && (t.status === "Running" || t.status === "Detached")) {
        kind = "他のクライアントが takeover しました";
        reattachable = true;   // session still alive elsewhere → can re-attach
      } else if (t) {
        kind = `セッション終了 (${t.status})`;
      } else {
        kind = "セッション終了";
      }
    } catch (_) {
      // Snapshot failed — almost always because the connection itself dropped
      // (the interactive stream rides the same connection, so a network/sleep
      // disconnect ends the stream AND breaks this snapshot). We cannot confirm
      // the session ended, so bias toward offering reattach: re-attaching a dead
      // session fails gracefully, whereas hiding the button strands the user
      // exactly when they most want it (right after a drop). The
      // registerOnConnected handler below re-verifies once the connection is
      // back and clears the button if the session turns out to be gone.
      kind = "接続が切れました (復帰後に再アタッチできます)";
      reattachable = true;
    }
    // A (re)attach happened while we awaited the snapshot — its display is the
    // truth now; don't overwrite it with a stale "detached" notice.
    if (attachEpoch !== myEpoch) return;
    const short = (taskID || "").slice(0, 12);
    attachedTask.textContent = `detached: ${short}… (${kind})`;
    // Echo into the command log so it's visible from the タスク/ファイル tab too.
    appendCmdOutput(`[interactive] ${short}… ${kind}`);
    // Offer one-tap re-attach right here when the session is still alive, so the
    // user doesn't have to go back to the task list.
    if (reattachable) showQuickReattach(taskID); else hideQuickReattach();
  };

  // Touch-keys: virtual modifier toggles + special-key buttons for soft keyboards.
  const mods = { ctrl: false, shift: false };

  const setMod = (name, on) => {
      mods[name] = on;
      const btn = document.getElementById(`tk-${name}`);
      if (btn) btn.classList.toggle("active", on);
  };

  const sendSeq = (seq) => {
      // Send straight to the PTY — no term.focus(), so touch-key-only
      // operations (e.g. Shift+Tab to toggle auto mode) don't pop the OS
      // soft keyboard. The keyboard opens only when the user taps the
      // terminal to type.
      window.harness.sendInteractive(seq);
  };

  // Apply Ctrl/Shift modifiers to a CSI base sequence (Esc Tab arrows).
  // Standard xterm-style modifier encoding:
  //   modVal = 1 + (Shift?1:0) + (Alt?2:0) + (Ctrl?4:0)
  // Shift+Tab is the special case: xterm sends ESC [ Z (BackTab).
  const KEY_BASE = {
      esc:   "\x1b",
      tab:   "\t",
      enter: "\r",
      up:    "\x1b[A",
      down:  "\x1b[B",
      left:  "\x1b[D",
      right: "\x1b[C",
  };

  const applyMods = (key) => {
      const base = KEY_BASE[key];
      if (!base) return null;
      // Shift+Tab → BackTab
      if (key === "tab" && mods.shift && !mods.ctrl) return "\x1b[Z";
      // Esc has no modifier encoding; send as-is.
      if (key === "esc") return base;
      // Tab with Ctrl only or Ctrl+Shift: no widely-supported sequence, send Tab.
      if (key === "tab") return base;
      // Arrow keys: use CSI 1;<mod><letter> when modifiers set.
      const m = /^\x1b\[([A-Z])$/.exec(base);
      if (m) {
          const modVal = 1 + (mods.shift ? 1 : 0) + (mods.ctrl ? 4 : 0);
          if (modVal === 1) return base;
          return `\x1b[1;${modVal}${m[1]}`;
      }
      return base;
  };

  document.querySelectorAll("#touch-keys button[data-mod]").forEach(btn => {
      btn.addEventListener("click", () => {
          const name = btn.getAttribute("data-mod");
          setMod(name, !mods[name]);
      });
  });

  document.querySelectorAll("#touch-keys button[data-key]").forEach(btn => {
      btn.addEventListener("click", () => {
          const key = btn.getAttribute("data-key");
          const seq = applyMods(key);
          if (seq != null) sendSeq(seq);
          // Auto-clear shift after a special key press (one-shot semantics).
          if (mods.shift) setMod("shift", false);
          if (mods.ctrl) setMod("ctrl", false);
      });
  });

  // Scroll buttons act on xterm's local scrollback viewport — NOT sent to the
  // PTY. xterm's touch scrolling is finger-1:1 with no momentum (see
  // Viewport.handleTouchMove), so a flick won't carry; these give reliable
  // page-at-a-time navigation plus a jump back to the live bottom.
  document.querySelectorAll("#touch-keys button[data-scroll]").forEach(btn => {
      btn.addEventListener("click", () => {
          switch (btn.getAttribute("data-scroll")) {
              case "pageup":   term.scrollPages(-1); break;
              case "pagedown": term.scrollPages(1);  break;
              case "bottom":   term.scrollToBottom(); break;
          }
      });
  });

  term.onData((data) => {
      // If Ctrl is armed and the data is a single ASCII letter, transform to
      // Ctrl+<letter> (control code = letter AND 0x1f). Auto-clear Ctrl after.
      if (mods.ctrl && data.length === 1) {
          const c = data.charCodeAt(0);
          if (c >= 0x40 && c <= 0x7e) {
              window.harness.sendInteractive(String.fromCharCode(c & 0x1f));
              setMod("ctrl", false);
              // Note: Shift on a letter is already applied by the OS
              // (uppercase comes through as the char itself), so we don't
              // touch shift state here.
              return;
          }
      }
      // Shift modifier doesn't apply to free-typed characters (the OS sends
      // the already-shifted character). Only the special-key buttons consult
      // mods.shift.
      window.harness.sendInteractive(data);
  });
  const ro = new ResizeObserver(() => {
    // ResizeObserver gives us pixel-size changes on the container. xterm
    // does not recompute its grid on its own, so call fit.fit() to derive
    // new cols/rows from the current font metrics + container size, then
    // forward that to the PTY side.
    try { fit.fit(); } catch (_) { /* element not yet laid out */ }
    window.harness.resizeInteractive({ cols: term.cols, rows: term.rows });
  });
  ro.observe(document.getElementById("terminal"));

  const attachedTask = document.getElementById("attached-task");

  // showError appends an error into attachedTask for inline feedback.
  const showError = (err) => {
    attachedTask.textContent = `error: ${err.message || err}`;
  };

  // composeRequest assembles the shared fields from the Compose section.
  const composeRequest = () => {
    return {
      repo: runnerSelect.value || "",
      host: hostSelect ? (hostSelect.value || "") : "",
      claudeArgs: currentClaudeArgs(),
      resumeTaskId: currentResumeTaskID(),
    };
  };

  // --- Cap chips (session-default capability set for new-session spawns) ---
  // capDefs / spawnCaps declared early in the IIFE (before connect) so the
  // wasm-independent state survives reconnects. initCaps() is called once after
  // connect (capList is synchronous; needs no active connection).

  function capsAllBits() {
    return capDefs.reduce((m, c) => m | c.bit, 0);
  }

  function capsLabel() {
    const allBits = capsAllBits();
    if (spawnCaps === allBits) return "all";
    if (spawnCaps === 0)       return "none";
    return capDefs.filter(c => (spawnCaps & c.bit) === c.bit).map(c => c.name).join(",");
  }

  function renderCaps() {
    const row = document.getElementById("caps-row");
    if (!row) return;
    // Re-render the whole row on every state change (small list — no perf concern).
    row.innerHTML = "";

    // [all] / [none] quick-set buttons
    const allBtn = document.createElement("button");
    allBtn.className = "cap-quick";
    allBtn.textContent = "[all]";
    allBtn.addEventListener("click", () => { spawnCaps = capsAllBits(); renderCaps(); });
    row.appendChild(allBtn);

    const noneBtn = document.createElement("button");
    noneBtn.className = "cap-quick";
    noneBtn.textContent = "[none]";
    noneBtn.addEventListener("click", () => { spawnCaps = 0; renderCaps(); });
    row.appendChild(noneBtn);

    // One chip per granular cap
    for (const c of capDefs) {
      const btn = document.createElement("button");
      btn.className = "cap-chip" + ((spawnCaps & c.bit) === c.bit ? " on" : "");
      btn.dataset.bit = String(c.bit);
      btn.textContent = c.name;
      btn.addEventListener("click", () => { spawnCaps ^= c.bit; renderCaps(); });
      row.appendChild(btn);
    }

    // Readout span
    const readout = document.createElement("span");
    readout.id = "caps-readout";
    readout.className = "caps-readout";
    readout.textContent = "caps: " + capsLabel();
    row.appendChild(readout);
  }

  function initCaps() {
    if (typeof window.harness.capList !== "function") return;
    capDefs = window.harness.capList();
    spawnCaps = capsAllBits();  // all granular bits on by default
    renderCaps();
    const capsOnResumeCb = document.getElementById("caps-on-resume");
    if (capsOnResumeCb) {
      capsOnResumeCb.checked = false; // explicit default OFF
      capsOnResumeCb.addEventListener("change", () => {
        applyCapsOnResume = capsOnResumeCb.checked;
      });
    }
  }

  // Quick-reattach button (terminal tab): shown after a takeover so the user
  // can re-attach to the same session in one tap, without going back to the
  // task list. Carries the task id in a data attribute.
  const reattachQuick = document.getElementById("reattach-quick");
  const showQuickReattach = (id) => {
    pendingReattachTaskID = id;   // remember across a reconnect (see below)
    if (!reattachQuick) return;
    reattachQuick.dataset.taskId = id;
    reattachQuick.hidden = false;
  };
  const hideQuickReattach = () => {
    pendingReattachTaskID = null;
    if (!reattachQuick) return;
    reattachQuick.hidden = true;
    delete reattachQuick.dataset.taskId;
  };

  // After the persist loop reconnects, re-verify any session left pending by a
  // disconnect-time close (where onInteractiveClosed couldn't fetch a snapshot
  // and defaulted to offering reattach). Now that the connection is back, a
  // working snapshot decides the truth: keep the quick-reattach button if the
  // session is still alive, drop it if it ended while we were away.
  registerOnConnected(async () => {
    const id = pendingReattachTaskID;
    if (!id) return;
    try {
      const snap = await window.harness.snapshot();
      const t = (snap.tasks || []).find(x => x.id === id);
      if (t && (t.status === "Running" || t.status === "Detached")) {
        showQuickReattach(id);
      } else {
        hideQuickReattach();
      }
    } catch (_) { /* still flaky — leave the button as-is for the next reconnect */ }
  });

  // reattachTo re-attaches to an existing live session by id. Shared by the
  // Reattach button, the task-row Reattach action, and the post-takeover quick
  // button (DRY). Switches to the terminal tab, replays, and pins to the bottom.
  // Pass view=true for read-only attach (AttachMode_View); default is control.
  const reattachTo = async (id, view = false) => {
    if (!id) { attachedTask.textContent = "(session id required)"; return; }
    attachEpoch++;            // invalidate any in-flight close handler
    hideQuickReattach();
    setActiveTab("terminal");
    term.reset();
    try {
      const taskID = await window.harness.attachSession(id, view ? "view" : "control");
      attachedTask.textContent = `attached: ${taskID} (${view ? "view" : "reattached"})`;
      scrollTermToBottom();
    } catch (err) {
      attachedTask.textContent = "";
      showError(err);
    }
    try { fit.fit(); } catch (_) { /* element not yet laid out */ }
    window.harness.resizeInteractive({ cols: term.cols, rows: term.rows });
  };

  // resumeTaskById opens a (terminal) task's worktree as a fresh interactive
  // session with --continue — "pick up where it left off". Shared by the notify
  // feed tap; mirrors the task sheet's Resume (--continue) action.
  const resumeTaskById = async (id) => {
    if (!id) return;
    setActiveTab("terminal");
    term.reset();
    const args = currentClaudeArgs();
    if (!args.includes("--continue")) args.push("--continue");
    try {
      const taskID = await window.harness.startInteractive({ repo: "", host: "", claudeArgs: args, resumeTaskId: id, detachable: true, caps: spawnCaps, resumeCapsOverride: applyCapsOnResume });
      attachedTask.textContent = `attached: ${taskID} (resumed --continue)`;
      scrollTermToBottom();
    } catch (err) {
      attachedTask.textContent = "";
      showError(err);
    }
    try { fit.fit(); } catch (_) { /* element not yet laid out */ }
    window.harness.resizeInteractive({ cols: term.cols, rows: term.rows });
  };

  // openInteractive is the shared helper for one-shot and detachable opens.
  const openInteractive = async (detachable, label) => {
    const req = composeRequest();
    if (!req.repo && !req.resumeTaskId) {
      alert("select a repo or fill in Resume task id");
      return;
    }
    attachEpoch++;            // invalidate any in-flight close handler
    hideQuickReattach();
    setActiveTab("terminal");
    term.reset();
    try {
      const taskID = await window.harness.startInteractive({...req, detachable, caps: spawnCaps, resumeCapsOverride: req.resumeTaskId ? applyCapsOnResume : false});
      attachedTask.textContent = `attached: ${taskID} (${label})`;
    } catch (e) {
      attachedTask.textContent = "";
      alert(`startInteractive: ${e.message}`);
    }
    try { fit.fit(); } catch (_) { /* element not yet laid out */ }
    window.harness.resizeInteractive({ cols: term.cols, rows: term.rows });
  };

  document.getElementById("open-oneshot").addEventListener("click", () => openInteractive(false, "one-shot"));
  document.getElementById("open-detachable").addEventListener("click", () => openInteractive(true, "detachable"));

  document.getElementById("stop-streaming").addEventListener("click", () => {
    window.harness.detachInteractive();
    attachedTask.textContent = "";
    hideQuickReattach();
  });

  if (reattachQuick) {
    reattachQuick.addEventListener("click", () => reattachTo(reattachQuick.dataset.taskId));
  }

  document.getElementById("reattach").addEventListener("click", () => reattachTo(taskIdInput.value.trim()));

  // renderTaskList builds clickable task rows into #task-list. Each row toggles
  // an inline action sheet; every action derives the id from the row, so the
  // user never copies a 32-hex id by hand. Modeled on the file-picker list.
  // Function declaration so refreshSnapshot() (called earlier textually) can
  // invoke it via hoisting.
  function renderTaskList(tasks) {
    taskList.innerHTML = "";
    if (!tasks || tasks.length === 0) {
      const empty = document.createElement("div");
      empty.className = "task-empty";
      empty.textContent = "(none)";
      taskList.appendChild(empty);
      return;
    }
    for (const t of tasks) {
      const wrap = document.createElement("div");
      const row = document.createElement("div");
      row.className = "task-row";
      const promptShort = (t.prompt || "").slice(0, 60);
      let attr = `  from=${t.origin || "-"}`;
      if (t.createdBy) attr += `  by=${t.createdBy}`;
      if (t.resumedBy) attr += `  resumed_by=${t.resumedBy}`;
      row.textContent = `${t.id.slice(0, 12)}…  ${t.status}  ${t.kind}  ${t.repoPath}${attr}  ${JSON.stringify(promptShort)}`;
      const sheet = document.createElement("div");
      sheet.className = "task-sheet";
      sheet.hidden = true;
      buildTaskSheet(sheet, t);
      row.addEventListener("click", () => {
        for (const s of taskList.querySelectorAll(".task-sheet")) {
          if (s !== sheet) s.hidden = true;   // single open sheet at a time
        }
        sheet.hidden = !sheet.hidden;
      });
      wrap.appendChild(row);
      wrap.appendChild(sheet);
      taskList.appendChild(wrap);
    }
  }

  // buildTaskSheet fills one task's action sheet, gating items by status/kind.
  // Each item stops propagation (so it doesn't re-toggle the row), runs its
  // harness call, and switches tabs where relevant.
  function buildTaskSheet(sheet, t) {
    const isTerminal = TERMINAL_STATES.has(t.status);
    const addItem = (label, cls, fn) => {
      const item = document.createElement("button");
      item.type = "button";
      item.className = "task-action" + (cls ? " " + cls : "");
      item.textContent = label;
      item.addEventListener("click", (e) => { e.stopPropagation(); fn(); });
      sheet.appendChild(item);
    };

    // Reattach / View — live interactive session only.
    if (t.kind === "Interactive" && (t.status === "Running" || t.status === "Detached")) {
      addItem("↪ Reattach", "", () => reattachTo(t.id, false));
      addItem("👁 View", "", () => reattachTo(t.id, true));
    }

    // Resume — finished task's worktree, opened as a fresh interactive session.
    // Reflect the Compose "Extra claude args" box (same as Submit / Open) so a
    // resume can carry --permission-mode etc. without going through the cmdline.
    // Two variants mirror the TUI's r/R: plain Resume (R) and Resume (--continue)
    // (r), the latter appending --continue so claude reloads its prior session.
    if (isTerminal) {
      const doResume = async (claudeArgs, note) => {
        setActiveTab("terminal");
        term.reset();
        try {
          const id = await window.harness.startInteractive({ repo: "", host: "", claudeArgs, resumeTaskId: t.id, detachable: true, caps: spawnCaps, resumeCapsOverride: applyCapsOnResume });
          attachedTask.textContent = `attached: ${id} (${note})`;
        } catch (err) { attachedTask.textContent = ""; alert(`resume: ${err.message}`); }
        try { fit.fit(); } catch (_) {}
        window.harness.resizeInteractive({ cols: term.cols, rows: term.rows });
      };
      addItem("▶ Resume", "", () => doResume(currentClaudeArgs(), "resumed"));
      addItem("▶ Resume (--continue)", "", () => {
        const args = currentClaudeArgs();
        if (!args.includes("--continue")) args.push("--continue");
        doResume(args, "resumed --continue");
      });
    }

    // Files — always available.
    addItem("📁 ファイル", "", () => {
      fileTaskSelect.value = t.id;
      filePickerCurDir = "";
      filePickerSelected = null;
      setActiveTab("files");
      refreshFilePicker();
    });

    // Cancel — non-terminal only.
    if (!isTerminal) {
      addItem("✕ Cancel", "danger", async () => {
        if (!window.confirm(`Cancel task ${t.id.slice(0, 12)}…?`)) return;
        try {
          await window.harness.cancel(t.id);
          appendCmdOutput(`cancelled ${t.id.slice(0, 12)}…`);
          refreshSnapshot();
        } catch (err) { appendCmdOutput(`cancel error: ${err.message}`); }
      });
    }
  }
})();

// sortRunners returns a new array sorted by (hostname asc, connectedAt
// asc, joined-roots asc). Used by refreshSnapshot to stabilise the UI
// against Go-map iteration randomness on the server side. The keys are
// chosen so the typical case (a handful of hosts, each with a few slots)
// renders as host-grouped blocks whose order does not change as long as
// no runner re-registers.
function sortRunners(runners) {
  const key = (r) => [
    r.hostname || "",
    Number(r.connectedAt || 0),
    Array.isArray(r.roots) ? r.roots.join(",") : "",
  ];
  return [...runners].sort((a, b) => {
    const ka = key(a);
    const kb = key(b);
    for (let i = 0; i < ka.length; i++) {
      if (ka[i] < kb[i]) return -1;
      if (ka[i] > kb[i]) return  1;
    }
    return 0;
  });
}

// renderRunnerSelect rebuilds the repo <select> options from the snapshot.
// Each option value is a root path. We de-duplicate across runners and
// preserve the previously-selected value when still present.
function renderRunnerSelect(sel, runners) {
  const prev = sel.value;
  sel.innerHTML = "";
  if (!runners || runners.length === 0) {
    const opt = document.createElement("option");
    opt.value = "";
    opt.textContent = "(no runners)";
    sel.appendChild(opt);
    return;
  }
  // Collect unique root paths; annotate with the first runner's status.
  const seen = new Map(); // path → status
  for (const r of runners) {
    if (!r.roots || r.roots.length === 0) continue;
    for (const root of r.roots) {
      if (root && !seen.has(root)) seen.set(root, r.status);
    }
  }
  if (seen.size === 0) {
    // Runners have no specific roots — fall back to "(any root)" per runner.
    for (const r of runners) {
      const opt = document.createElement("option");
      opt.value = r.hostname || "";
      const idle = r.status === "Idle";
      opt.disabled = !idle;
      opt.textContent = `${r.hostname || "(unknown)"}  [${r.status}]`;
      sel.appendChild(opt);
    }
    return;
  }
  let prevStillPresent = false;
  let firstIdle = "";
  for (const [root, status] of seen) {
    const opt = document.createElement("option");
    opt.value = root;
    const idle = status === "Idle";
    opt.disabled = !idle;
    opt.textContent = `${root}  [${status}]`;
    sel.appendChild(opt);
    if (idle && !firstIdle) firstIdle = root;
    if (root === prev) prevStillPresent = true;
  }
  sel.value = prevStillPresent ? prev : firstIdle;
}

// renderHostSelect rebuilds the host pin <select>. First option is always
// "(any)" (value=""). Subsequent options are unique runner hostnames.
function renderHostSelect(sel, runners) {
  if (!sel) return;
  const prev = sel.value;
  sel.innerHTML = "";
  const anyOpt = document.createElement("option");
  anyOpt.value = "";
  anyOpt.textContent = "(any host)";
  sel.appendChild(anyOpt);
  if (!runners) return;
  const seen = new Set();
  for (const r of runners) {
    const h = r.hostname || "";
    if (h && !seen.has(h)) {
      seen.add(h);
      const opt = document.createElement("option");
      opt.value = h;
      opt.textContent = `${h}  [${r.status}]`;
      sel.appendChild(opt);
    }
  }
  // Preserve previous selection if still available.
  if (prev && seen.has(prev)) sel.value = prev;
}

function renderRunners(runners) {
  if (!runners || runners.length === 0) return "(none)";
  return runners.map(r => {
    const roots = (r.roots && r.roots.length > 0) ? r.roots.join(", ") : "(any)";
    return `  ${pad(r.status, 8)} host=${r.hostname || "-"}  tasks=${r.tasks}/${r.maxTasks}  roots=${roots}`;
  }).join("\n");
}

function pad(s, n) {
  s = String(s);
  return s.length >= n ? s : s + " ".repeat(n - s.length);
}

// tokenize is a tiny quote-aware splitter. Single and double quotes group
// content as a single token; backslash escapes the next character. Unclosed
// quotes are treated as if closed at end-of-string (forgiving for dogfood).
function tokenize(line) {
  const out = [];
  let cur = "";
  let quote = "";
  let escaped = false;
  for (let i = 0; i < line.length; i++) {
    const ch = line[i];
    if (escaped) { cur += ch; escaped = false; continue; }
    if (ch === "\\") { escaped = true; continue; }
    if (quote) {
      if (ch === quote) { quote = ""; continue; }
      cur += ch;
      continue;
    }
    if (ch === '"' || ch === "'") { quote = ch; continue; }
    if (/\s/.test(ch)) {
      if (cur.length > 0) { out.push(cur); cur = ""; }
      continue;
    }
    cur += ch;
  }
  if (cur.length > 0) out.push(cur);
  return out;
}

// parseFlags is retained for `prune --before 168h` style flags.
function parseFlags(tokens) {
  const out = {};
  for (let i = 0; i < tokens.length; i++) {
    const t = tokens[i];
    if (t.startsWith("--")) {
      const eq = t.indexOf("=");
      if (eq !== -1) {
        out[t.slice(2, eq)] = t.slice(eq + 1);
      } else {
        out[t.slice(2)] = tokens[i + 1] || "";
        i++;
      }
    }
  }
  return out;
}

// --- file ops dispatch -------------------------------------------------

// runFileCmd handles the `file <verb> ...` family from the cmd-input.
// Returns a string to be appended to cmd-output. Throws on usage error;
// non-fatal "Cancelled by user" outcomes return a short string instead.
async function runFileCmd(rest) {
  if (rest.length === 0) {
    throw new Error("file: sub-verb required (ls | delete | push | pull)");
  }
  const verb = rest[0];
  const args = rest.slice(1);
  switch (verb) {
    case "ls":
      return fileLsCmd(args);
    case "delete":
      return fileDeleteCmd(args);
    case "push":
      return filePushCmd(args);
    case "pull":
      return filePullCmd(args);
    default:
      throw new Error(`file: unknown sub-verb ${verb}`);
  }
}

async function fileLsCmd(args) {
  if (args.length < 1 || args.length > 2) {
    throw new Error("usage: file ls <task-id> [<worktree-rel-dir>]");
  }
  const taskID = args[0];
  const rel = args[1] || "";
  const entries = await window.harness.fileLs(taskID, rel);
  if (entries.length === 0) return "(empty)";
  return entries.map(e => {
    const name = e.isDir ? `${e.name}/` : e.name;
    const sz = e.isDir ? "" : String(e.size);
    return `${sz.padStart(10)} ${name}`;
  }).join("\n");
}

async function fileDeleteCmd(args) {
  // Parse flags before positional args.
  let recursive = false, force = false;
  const pos = [];
  for (const a of args) {
    if (a === "-r" || a === "--recursive") { recursive = true; continue; }
    if (a === "-f" || a === "--force")     { force = true; continue; }
    pos.push(a);
  }
  if (pos.length !== 2) {
    throw new Error("usage: file delete [-r [-f]] <task-id> <rel>");
  }
  const [taskID, rel] = pos;
  // Confirm before destructive action. Browser native dialog.
  const verb = recursive ? (force ? "rm -rf" : "rmdir") : "rm";
  if (!window.confirm(`${verb} ${rel} on task ${taskID.slice(0, 12)} — proceed?`)) {
    return "delete cancelled";
  }
  await window.harness.fileDelete(taskID, rel, recursive, force);
  return `${verb} ok: ${rel}`;
}

async function filePushCmd(args) {
  if (args.length !== 2) {
    throw new Error("usage: file push <task-id> <worktree-rel-dst>");
  }
  const [taskID, remoteRel] = args;
  // Open the hidden file picker; abort if the user closes it without
  // selecting anything.
  const file = await pickLocalFile();
  if (!file) return "push cancelled (no file selected)";
  const buf = new Uint8Array(await file.arrayBuffer());
  try {
    await window.harness.filePushBytes(taskID, remoteRel, buf, false);
    return `push ok: ${file.name} -> ${remoteRel} (${buf.byteLength} bytes)`;
  } catch (e) {
    if (e && e.code === "already_exists") {
      if (!window.confirm(`${remoteRel} already exists on the runner. Overwrite?`)) {
        return "push cancelled (overwrite declined)";
      }
      await window.harness.filePushBytes(taskID, remoteRel, buf, true);
      return `push ok (overwritten): ${file.name} -> ${remoteRel} (${buf.byteLength} bytes)`;
    }
    throw e;
  }
}

async function filePullCmd(args) {
  if (args.length !== 2) {
    throw new Error("usage: file pull <task-id> <worktree-rel-src>");
  }
  const [taskID, remoteRel] = args;
  const bytes = await window.harness.filePullBytes(taskID, remoteRel);
  triggerDownload(bytes, basename(remoteRel));
  return `pull ok: ${remoteRel} (${bytes.byteLength} bytes) — browser save dialog`;
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

function imageMimeForName(name) {
  return IMAGE_MIME_BY_EXT[fileExt(name)] || "application/octet-stream";
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
