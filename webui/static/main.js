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

  // Explicit reload button (header). Pull-to-refresh is intentionally blocked
  // on mobile, so this is the deliberate way to force a reload. Wired before
  // the wasm load so it stays usable even if wasm never finishes loading.
  // The confirm() keeps the "no accidental reload" property the overscroll
  // guard provides.
  const reloadBtn = document.getElementById("reload-btn");
  if (reloadBtn) {
    reloadBtn.addEventListener("click", () => {
      if (confirm("ページを再読み込みしますか？\n(端末セッションの表示はリセットされます)")) {
        location.reload();
      }
    });
  }

  // Keep-awake toggle (header). The session/terminal view is static — no playing
  // media — so nothing holds the phone screen awake and it hits the OS
  // screen-timeout fast. Two-tier, mirroring the clipboard fallbacks elsewhere:
  // the Screen Wake Lock API needs a secure context (https/wss) which the
  // plain-ws deployment lacks, so we fall back to the NoSleep technique — a
  // looping <video> with a silent audio track, played unmuted, keeps the screen
  // lit even over plain http (see makeVideo for why audio, not size). Wired here
  // (before the wasm load) so it works regardless of connection state. Default
  // off — the user opts in, so there's no silent battery drain.
  const KEEPAWAKE_KEY = "harness.keepAwake";
  const keepAwakeBtn = document.getElementById("keepawake-btn");
  const keepAwake = (() => {
    let wantOn = localStorage.getItem(KEEPAWAKE_KEY) === "on";
    let lock = null;      // WakeLockSentinel when the API path is engaged
    let video = null;     // fallback <video> when the API is unavailable
    let engaged = false;  // true once a real lock/video is actually active
    // Re-checked lazily (not cached) so a secure context that appears later — or
    // a test that strips navigator.wakeLock — is honoured at engage time.
    const hasWakeLock = () => !!(navigator.wakeLock && typeof navigator.wakeLock.request === "function");

    function makeVideo() {
      const v = document.createElement("video");
      // NOT muted, on purpose. Blink's VideoWakeLock (core/html/media/
      // video_wake_lock.cc) only takes the screen lock for a hidden video via
      // its has_audio branch — `HasAudio() && EffectiveMediaVolume() > 0`. A
      // muted video falls to the size/visibility branch (>20% of viewport AND
      // ≥75% on-screen), which a 1px hidden element can never satisfy. So the
      // keepawake.{webm,mp4} carry a SILENT audio track and we play unmuted —
      // the NoSleep.js technique. Autoplay is fine because engage() runs inside
      // the toggle's click gesture. (Caveat: playing audio takes Android audio
      // focus, so it can pause other apps' background playback.)
      v.volume = 1;
      v.setAttribute("playsinline", "");
      v.setAttribute("loop", "");
      v.setAttribute("aria-hidden", "true");
      // 1px / opacity:0 is fine now that the has_audio branch ignores size and
      // visibility — no visible artifact. NOT display:none (won't play hidden).
      v.style.cssText = "position:fixed;left:0;bottom:0;width:1px;height:1px;opacity:0;pointer-events:none;";
      const webm = document.createElement("source");
      webm.src = "/static/keepawake.webm"; webm.type = "video/webm";
      const mp4 = document.createElement("source");
      mp4.src = "/static/keepawake.mp4"; mp4.type = "video/mp4";
      v.append(webm, mp4);
      document.body.appendChild(v);
      return v;
    }

    async function engage() {
      if (engaged) return true;
      if (hasWakeLock()) {
        try {
          lock = await navigator.wakeLock.request("screen");
          // The sentinel auto-releases when the tab is hidden; note it so the
          // visibilitychange handler knows to re-acquire on return.
          lock.addEventListener("release", () => { lock = null; engaged = false; reflect(); });
          engaged = true;
          reflect();
          return true;
        } catch (e) { /* fall through to the video path */ }
      }
      try {
        if (!video) video = makeVideo();
        await video.play();
        engaged = true;
        reflect();
        return true;
      } catch (e) {
        return false; // autoplay blocked — needs a fresh user gesture
      }
    }

    function disengage() {
      engaged = false;
      if (lock) { lock.release().catch(() => {}); lock = null; }
      if (video) video.pause();
      reflect();
    }

    function reflect() {
      if (!keepAwakeBtn) return;
      keepAwakeBtn.setAttribute("aria-pressed", wantOn ? "true" : "false");
      keepAwakeBtn.classList.toggle("is-active", wantOn && engaged);
      keepAwakeBtn.classList.toggle("is-pending", wantOn && !engaged);
      keepAwakeBtn.title = !wantOn ? "画面の自動消灯を抑止"
        : engaged ? "画面の自動消灯を抑止中（タップで解除）"
                  : "タップで消灯抑止を有効化";
    }

    async function turnOn() {
      wantOn = true;
      localStorage.setItem(KEEPAWAKE_KEY, "on");
      reflect();
      const ok = await engage();
      if (!ok) {
        // Couldn't engage (autoplay needs a gesture). Keep wantOn so the next
        // tap retries; surface a hint via the existing toast path.
        showToast({ level: "warn", title: "消灯抑止を有効化できませんでした",
          text: "端末の自動再生制限の可能性。もう一度タップしてください。" });
        reflect();
      }
    }

    function turnOff() {
      wantOn = false;
      localStorage.setItem(KEEPAWAKE_KEY, "off");
      disengage();
    }

    function toggle() { return wantOn ? turnOff() : turnOn(); }

    // The Wake Lock sentinel auto-releases on tab-hide and the fallback video
    // pauses; re-engage when we become visible again and the toggle is still on.
    document.addEventListener("visibilitychange", () => {
      if (document.visibilityState !== "visible" || !wantOn) return;
      if (!engaged) engage();
      else if (video) video.play().catch(() => {});
    });

    if (keepAwakeBtn) keepAwakeBtn.addEventListener("click", toggle);

    // On load, honour persisted intent. The Wake Lock path can re-acquire now
    // (no gesture needed while visible); the video fallback needs a gesture, so
    // if engage() can't, arm a one-shot that engages on the first tap anywhere.
    reflect();
    if (wantOn) {
      engage().then((ok) => {
        if (ok) return;
        const once = () => { if (wantOn && !engaged) engage(); };
        document.addEventListener("pointerdown", once, { once: true });
      });
    }

    return { toggle, turnOn, turnOff, isEngaged: () => engaged, wants: () => wantOn };
  })();
  void keepAwake; // referenced for debugging / future programmatic control

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

  // Live-apply a #psk edit. A rejected connect (BadPsk) is fatal in the persist
  // loop (cli/persist.go), so after it the wasm never re-dials and editing the
  // #psk fragment has no effect until a reload re-runs GetPSK (cli/psk_js.go).
  // So when the #psk value changes WHILE NOT CONNECTED, reload to pick up the
  // new secret. Scoped to the disconnected state so a live session is never
  // dropped, and only on an actual psk change. hashchange fires for address-bar
  // fragment edits / location.hash= / anchors (not history.pushState — fine, the
  // manual "add #psk" flow is an address-bar edit).
  let lastHashPsk = new URLSearchParams(location.hash.replace(/^#/, '')).get('psk');
  window.addEventListener('hashchange', () => {
    const psk = new URLSearchParams(location.hash.replace(/^#/, '')).get('psk');
    if (psk !== lastHashPsk && !connectionIsUp) {
      location.reload();
      return;
    }
    lastHashPsk = psk;
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
  const agentSelect  = document.getElementById("agent-select");
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
  const filePullDirBtn    = document.getElementById("file-pull-dir-btn");
  const filePreviewBtn    = document.getElementById("file-preview-btn");
  const fileDeleteBtn     = document.getElementById("file-delete-btn");
  const fileResultPre     = document.getElementById("file-result");
  const filePreviewModal  = document.getElementById("file-preview-modal");
  const filePreviewTitle  = document.getElementById("file-preview-title");
  const filePreviewBody   = document.getElementById("file-preview-body");
  const filePreviewClose  = document.getElementById("file-preview-close");
  const filePreviewToggle = document.getElementById("file-preview-toggle");
  const filePreviewCopy   = document.getElementById("file-preview-copy");
  // Set when the current preview is HTML, so the toggle can rebuild the body
  // from already-fetched bytes without re-pulling. Reset on modal close.
  let filePreviewHtml = null; // { rel, size, bytes, mode: "render" | "source" }
  // What the Copy button writes to the clipboard for the current preview:
  //   { text }            => clipboard.writeText
  //   { blob, type }      => clipboard.write (image, best-effort by MIME)
  // null while showing an error/oversize note (nothing copyable) => button hidden.
  let filePreviewCopyPayload = null;

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

  // knownAgentProfiles is the deduplicated union of every connected runner's
  // advertised agent_profiles, refreshed each snapshot poll. Shared by the
  // Compose agent dropdown (#agent-select) and each task-sheet's per-resume
  // agent dropdown (multi-agent-profile design §6).
  let knownAgentProfiles = [];

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
    knownAgentProfiles = collectAgentProfiles(sortedRunners);
    renderAgentSelect(agentSelect, knownAgentProfiles);
    runnerList.textContent = renderRunners(sortedRunners);
    renderTaskList(snap.tasks);
    renderFileTaskSelect(snap.tasks);
    // Connection topology — rides the same ~5s poll (spec decision #3:
    // no separate event subscription in wasm). snap.conns may be absent if
    // the server doesn't have the list_conns capability yet; guard with [].
    const conns = snap.conns || [];
    const allTasks = snap.tasks || [];
    renderConnTopology(conns, allTasks);
    renderConnList(conns, allTasks);
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
    // Two always-present pull buttons (kept independent because there is no
    // way to *deselect* a file in-place — clicking only moves the highlight —
    // so a single context-switching button would strand you on "file mode"
    // with no path back to dir-pull):
    //   Pull      — the selected file's raw bytes (needs a selection).
    //   Pull dir  — the current directory as a .tar; available whenever a task
    //               is picked, including the worktree root (rel ".").
    filePullBtn.disabled = !hasTask || !hasSel;
    filePullDirBtn.disabled = !hasTask;
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
    const fp = beginFileProgress(file.name);
    try {
      let force = false;
      for (;;) {
        try {
          await window.harness.filePushBytes(taskID, remoteRel, buf, force, fp.onProgress);
          fileResultPre.textContent = `${force ? "push ok (overwritten)" : "push ok"}: ${file.name} -> ${remoteRel} (${buf.byteLength} bytes)`;
          break;
        } catch (e) {
          if (!force && e && e.code === "already_exists") {
            if (!window.confirm(`${remoteRel} already exists on the runner. Overwrite?`)) {
              fileResultPre.textContent = "push cancelled (overwrite declined)";
              return;
            }
            force = true;
            continue; // retry with overwrite
          }
          fileResultPre.textContent = `push error: ${e.message}`;
          return;
        }
      }
      refreshFilePicker();
    } finally {
      fp.end();
    }
  });

  filePullBtn.addEventListener("click", async () => {
    const taskID = fileTaskSelect.value;
    if (!taskID || !filePickerSelected) return;
    const rel = joinFsPath(filePickerCurDir, filePickerSelected.name);
    const fp = beginFileProgress(filePickerSelected.name);
    try {
      const bytes = await window.harness.filePullBytes(taskID, rel, fp.onProgress);
      triggerDownload(bytes, filePickerSelected.name);
      fileResultPre.textContent = `pull ok: ${rel} (${bytes.byteLength} bytes) — browser save dialog`;
    } catch (e) {
      fileResultPre.textContent = `pull error: ${e.message}`;
    } finally {
      fp.end();
    }
  });

  filePullDirBtn.addEventListener("click", async () => {
    const taskID = fileTaskSelect.value;
    if (!taskID) return;
    // At root filePickerCurDir is "" — the runner rejects an empty rel, so
    // send "." (resolves to the worktree root) and name the archive for it.
    const rel = filePickerCurDir || ".";
    const name = (filePickerCurDir ? basename(filePickerCurDir) : "worktree") + ".tar";
    const fp = beginFileProgress(name);
    try {
      const bytes = await window.harness.filePullDirBytes(taskID, rel, fp.onProgress);
      triggerDownload(bytes, name);
      fileResultPre.textContent = `pull ok (tar): ${rel} (${bytes.byteLength} bytes) -> ${name} — browser save dialog`;
    } catch (e) {
      fileResultPre.textContent = `pull error: ${e.message}`;
    } finally {
      fp.end();
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
    filePreviewHtml = null;
    filePreviewToggle.hidden = true;
    filePreviewCopyPayload = null;
    filePreviewCopy.hidden = true;
  });

  // showPreviewCopy enables the Copy button for the current preview with the
  // given clipboard payload. Render paths call this after openFilePreview,
  // which clears the payload up front, so error/oversize paths stay hidden.
  function showPreviewCopy(payload) {
    filePreviewCopyPayload = payload;
    filePreviewCopy.hidden = false;
    filePreviewCopy.textContent = "Copy";
  }

  // openFilePreview shows the modal with a header and a body built from the
  // given DOM node (or a plain note string for errors / oversize messages).
  function openFilePreview(rel, size, bodyNode, note) {
    if (filePreviewObjectURL) {
      URL.revokeObjectURL(filePreviewObjectURL);
      filePreviewObjectURL = null;
    }
    // Default to no copy target; render paths re-enable it via showPreviewCopy.
    filePreviewCopyPayload = null;
    filePreviewCopy.hidden = true;
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
    // Copy always yields the raw HTML source, regardless of render/source view.
    showPreviewCopy({ text });
  }

  filePreviewToggle.addEventListener("click", () => {
    if (!filePreviewHtml) return;
    filePreviewHtml.mode = filePreviewHtml.mode === "render" ? "source" : "render";
    showHtmlPreview();
  });

  let filePreviewCopyTimer = null;
  function flashCopyLabel(label) {
    filePreviewCopy.textContent = label;
    if (filePreviewCopyTimer) clearTimeout(filePreviewCopyTimer);
    filePreviewCopyTimer = setTimeout(() => {
      if (!filePreviewCopy.hidden) filePreviewCopy.textContent = "Copy";
    }, 1800);
  }

  // writeClipboardText copies text in a way that also works over plain http.
  // The async Clipboard API requires a secure context (https/localhost), which
  // the WebUI does NOT have when served over plain ws — so we fall back to the
  // legacy execCommand("copy") path, which has no secure-context requirement.
  // Returns true on success, false if both paths fail.
  async function writeClipboardText(text) {
    if (navigator.clipboard && window.isSecureContext) {
      try { await navigator.clipboard.writeText(text); return true; } catch (e) { /* fall through */ }
    }
    try {
      const ta = document.createElement("textarea");
      ta.value = text;
      ta.setAttribute("readonly", "");
      // Keep it on-screen-but-invisible: off-screen elements can make the
      // selection/copy a no-op on some mobile browsers.
      ta.style.position = "fixed";
      ta.style.top = "0";
      ta.style.left = "0";
      ta.style.opacity = "0";
      document.body.appendChild(ta);
      ta.focus();
      ta.select();
      ta.setSelectionRange(0, ta.value.length);
      const ok = document.execCommand("copy");
      document.body.removeChild(ta);
      return !!ok;
    } catch (e) {
      return false;
    }
  }

  // selectNodeText highlights a node's contents so the user can copy manually
  // (Ctrl/⌘-C, or the mobile selection menu) — the last-resort path when even
  // execCommand is blocked.
  function selectNodeText(node) {
    const sel = window.getSelection();
    if (!sel) return;
    const range = document.createRange();
    range.selectNodeContents(node);
    sel.removeAllRanges();
    sel.addRange(range);
  }

  filePreviewCopy.addEventListener("click", async () => {
    const p = filePreviewCopyPayload;
    if (!p) return;
    // Text/hex/HTML-source: clipboard write with an execCommand fallback that
    // works over plain http; if that also fails, fall back to selecting the
    // visible text so the user can copy it by hand.
    if (p.text != null) {
      if (await writeClipboardText(p.text)) { flashCopyLabel("Copied ✓"); return; }
      const node = filePreviewBody.querySelector("pre") || filePreviewBody;
      selectNodeText(node);
      flashCopyLabel("Selected ✓");
      return;
    }
    // Images: only the async Clipboard API can carry a blob, and it needs a
    // secure context — so this works on https/localhost but not plain ws.
    if (p.blob) {
      try {
        if (!navigator.clipboard || !window.isSecureContext) throw new Error("needs https");
        await navigator.clipboard.write([new ClipboardItem({ [p.type]: p.blob })]);
        flashCopyLabel("Copied ✓");
      } catch (e) {
        flashCopyLabel("Needs https");
      }
    }
  });

  // renderFilePreview picks a renderer based on extension (images) and a
  // byte sniff (text vs binary), then opens the modal.
  function renderFilePreview(rel, size, name, bytes) {
    if (isHtmlExt(name)) {
      filePreviewHtml = { rel, size, bytes, mode: "render" };
      showHtmlPreview();
      return;
    }
    if (isImageExt(name)) {
      const blob = new Blob([bytes], { type: imageMimeForName(name) });
      filePreviewObjectURL = URL.createObjectURL(blob);
      const img = document.createElement("img");
      img.src = filePreviewObjectURL;
      img.alt = name;
      openFilePreview(rel, size, img, null);
      showPreviewCopy({ blob, type: imageMimeForName(name) });
      return;
    }
    if (isLikelyBinary(bytes)) {
      const pre = document.createElement("pre");
      const hex = hexDump(bytes, HEX_PREVIEW_MAX_BYTES);
      pre.textContent = hex;
      const truncated = bytes.byteLength > HEX_PREVIEW_MAX_BYTES;
      openFilePreview(rel, size, pre,
        truncated ? `binary — showing first ${HEX_PREVIEW_MAX_BYTES} of ${bytes.byteLength} bytes` : "binary");
      // Copy the displayed hex dump (which may be truncated), matching the view.
      showPreviewCopy({ text: hex });
      return;
    }
    const pre = document.createElement("pre");
    const text = new TextDecoder("utf-8", { fatal: false }).decode(bytes);
    pre.textContent = text;
    openFilePreview(rel, size, pre, null);
    showPreviewCopy({ text });
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
      // task_activity events fire on every busy/idle edge of every live
      // session — routine badge refreshes, not lifecycle changes. Skip the
      // banner AND the snapshot kick (the 5s poll keeps the table current);
      // rendering each edge would spam the cmd output.
      if (evt.line && evt.line.includes("kind=TaskActivity")) return;
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
          let resumeConversation = false;
          // agent defaults to the Compose dropdown's current selection (mirrors
          // repo/host, which also fall back to the Compose selects); --agent
          // overrides it, same pattern as the TUI cmdline's --agent (Task 9/10).
          let agent = agentSelect ? (agentSelect.value || "") : "";
          const promptTokens = [];
          for (let i = 1; i < tokens.length; i++) {
            const t = tokens[i];
            if (t === "--resume-conversation") {
              resumeConversation = true;
            } else if (t === "--agent") {
              i++;
              if (i >= tokens.length) throw new Error("--agent: missing profile name");
              agent = tokens[i];
            } else if (t.startsWith("--agent=")) {
              agent = t.slice("--agent=".length);
            } else {
              promptTokens.push(t);
            }
          }
          // Everything after `submit` (except command flags) is the task prompt. We join the
          // tokenize() result with single spaces — quoted segments have
          // already been collapsed into single tokens, so a multi-word
          // task is preserved verbatim.
          const task = promptTokens.join(" ");
          if (!task) throw new Error("submit: missing task prompt");
          const host = hostSelect ? (hostSelect.value || "") : "";
          const claudeArgsList = currentClaudeArgs();
          out = await window.harness.submit({ repo, task, host, agent, claudeArgs: claudeArgsList, resumeTaskId, caps: spawnCaps, resumeCapsOverride: resumeTaskId ? applyCapsOnResume : false, resumeConversation });
          break;
        }
        case "list":
          // Force a snapshot refresh, then echo the rendered task rows
          // (newline-joined) into cmd-output.
          await refreshSnapshot();
          out = Array.from(taskList.querySelectorAll(".task-row"))
                  .map(r => r.textContent).join("\n") || "(none)";
          break;
        case "refresh":
        case "sync":
          // Force a snapshot re-sync without echoing the rows (TUI parity).
          await refreshSnapshot();
          out = "snapshot refreshed";
          break;
        case "await-idle": {
          // await-idle <task-id> [--notify | --topic T] [--threshold-ms N]
          // Default (reply sink) keeps the promise open until the session
          // goes idle, then prints; --notify arms and returns immediately
          // (fire lands in the notification feed + notify-hook egress).
          let notify = false, topic = null, thresholdMs = 0, target = null;
          for (let i = 1; i < tokens.length; i++) {
            const t = tokens[i];
            if (t === "--notify") notify = true;
            else if (t === "--topic") { i++; topic = tokens[i]; }
            else if (t.startsWith("--topic=")) topic = t.slice("--topic=".length);
            else if (t === "--threshold-ms") { i++; thresholdMs = parseInt(tokens[i], 10) || 0; }
            else if (t.startsWith("--threshold-ms=")) thresholdMs = parseInt(t.slice("--threshold-ms=".length), 10) || 0;
            else if (!target) target = t;
            else throw new Error(`await-idle: unexpected arg ${t}`);
          }
          if (!target) throw new Error("await-idle: missing task id (32 hex)");
          if (notify && topic) throw new Error("await-idle: --notify and --topic are mutually exclusive");
          const sink = notify ? "notify" : (topic ? "board" : "reply");
          if (sink === "reply") appendCmdOutput("await-idle: waiting for the session to go idle…", true);
          const r = await window.harness.awaitIdle({ taskId: target, thresholdMs, sink, topic: topic || undefined });
          out = `await-idle ${target.slice(0, 12)}: ${r.status}`;
          break;
        }
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
            "  submit [--resume-conversation] [--agent <name>] <prompt...>",
            "                            submit task (use repo dropdown / Resume task id; --agent overrides the Agent dropdown)",
            "  list                      refresh the snapshot and echo task rows",
            "  refresh (alias: sync)     force a snapshot re-sync",
            "  await-idle <task-id> [--notify | --topic T] [--threshold-ms N]",
            "                            fire when the session's output goes idle (default: prints here on fire; --notify: notification feed + hook)",
            "  cancel <task-id>          cancel a task",
            "  prune [--before=DUR]      forget terminal tasks older than DUR",
            "  file ls <task> [rel]      list a worktree directory",
            "  file delete [-r] [-f] <task> <rel>",
            "                            remove a file (no -r) or directory (-r [-f])",
            "  file push <task> <rel>    upload a local file (file picker opens)",
            "  file pull [-r] <task> <rel>",
            "                            download a remote file, or -r for a directory as a .tar",
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
  // currentSessionTaskId mirrors the task id of the session the terminal is
  // attached to ("" when none). Kept in lockstep with attachedTask's text by
  // every attach/open/stop path; the await-idle button reads it.
  let currentSessionTaskId = "";

  // showError appends an error into attachedTask for inline feedback.
  const showError = (err) => {
    attachedTask.textContent = `error: ${err.message || err}`;
  };

  // composeRequest assembles the shared fields from the Compose section.
  const composeRequest = () => {
    return {
      repo: runnerSelect.value || "",
      host: hostSelect ? (hostSelect.value || "") : "",
      agent: agentSelect ? (agentSelect.value || "") : "",
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
      currentSessionTaskId = taskID;
      scrollTermToBottom();
    } catch (err) {
      attachedTask.textContent = "";
      currentSessionTaskId = "";
      showError(err);
    }
    try { fit.fit(); } catch (_) { /* element not yet laid out */ }
    window.harness.resizeInteractive({ cols: term.cols, rows: term.rows });
  };

  // resumeTaskById opens a terminal task's worktree as a fresh interactive
  // session and asks the runner to resume the agent conversation too.
  const resumeTaskById = async (id) => {
    if (!id) return;
    const args = currentClaudeArgs();
    // agent reuses the Compose dropdown's current selection (this quick-resume
    // path — from the notification feed — only has the task id, not the task's
    // own AgentProfile, so it can't default per-task the way buildTaskSheet's
    // per-row dropdown does).
    const agent = agentSelect ? (agentSelect.value || "") : "";
    const req = { repo: "", host: "", agent, claudeArgs: args, resumeTaskId: id, caps: spawnCaps, resumeCapsOverride: applyCapsOnResume, resumeConversation: true };
    try {
      const taskID = await window.harness.startInteractive(req);
      setActiveTab("terminal");
      term.reset();
      attachedTask.textContent = `attached: ${taskID} (resumed conversation)`;
      currentSessionTaskId = taskID;
      scrollTermToBottom();
    } catch (err) {
      attachedTask.textContent = "";
      currentSessionTaskId = "";
      if (routeAmbiguous(err, req)) return;
      showError(err);
    }
    try { fit.fit(); } catch (_) { /* element not yet laid out */ }
    window.harness.resizeInteractive({ cols: term.cols, rows: term.rows });
  };

  // onInteractiveOpened is the taskID-handling tail shared by every successful
  // startInteractive call: the initial (non-ambiguous) open below, and the
  // cid-pinned retry from pickRunnerAndRetry after the runner-picker modal.
  // Factored out so the two call sites can't drift (see pickRunnerAndRetry).
  const onInteractiveOpened = (taskID, label) => {
    attachedTask.textContent = `attached: ${taskID} (${label})`;
    currentSessionTaskId = taskID;
  };

  // pickRunnerAndRetry shows the runner-picker modal for an ambiguous_runner
  // rejection and, on a candidate click, re-issues startInteractive pinned by
  // that candidate's cid. baseReq is the original compose-request plus
  // (the request that just failed); host is cleared and runner
  // set instead, because pinning by cid is unambiguous even when host is not
  // (a hostname can itself be shared by >=2 runners, which is the whole
  // reason this modal exists).
  function pickRunnerAndRetry(candidates, baseReq) {
    const modal = document.getElementById("runner-picker-modal");
    const list = document.getElementById("runner-picker-list");
    const cancel = document.getElementById("runner-picker-cancel");
    if (modal && !modal.dataset.stopBackdropClick) {
      // Some mobile browsers can retarget the closing tap to the page behind
      // the top-layer dialog. Keep picker pointer/click events inside it so
      // Cancel cannot also activate the terminal tab underneath.
      for (const evName of ["pointerdown", "pointerup", "click"]) {
        modal.addEventListener(evName, (ev) => ev.stopPropagation());
      }
      modal.dataset.stopBackdropClick = "1";
    }
    if (cancel && !cancel.dataset.bound) {
      cancel.addEventListener("click", (ev) => {
        ev.preventDefault();
        ev.stopPropagation();
        modal.close();
      });
      cancel.dataset.bound = "1";
    }
    list.innerHTML = "";
    candidates.forEach((c) => {
      const btn = document.createElement("button");
      btn.type = "button";
      btn.className = "runner-choice";
      btn.innerHTML = "";
      const head = document.createElement("span");
      head.className = "runner-choice-head";
      const host = document.createElement("span");
      host.className = "runner-choice-host";
      host.textContent = c.hostname || "(unknown)";
      // agent — each candidate row is a (runner, profile) combo (§4a); with a
      // single multi-profile runner this is what makes the two rows distinct.
      const agent = document.createElement("span");
      agent.className = "runner-choice-agent";
      agent.textContent = c.profile || "(default)";
      const load = document.createElement("span");
      load.className = "runner-choice-load";
      load.textContent = `[${c.activeTasks}/${c.maxTasks}]`;
      head.append(host, agent, load);
      const root = document.createElement("span");
      root.className = "runner-choice-root";
      root.textContent = c.matchedRoot || "(no matched root)";
      const cid = document.createElement("span");
      cid.className = "runner-choice-cid";
      cid.textContent = c.cid || "";
      btn.append(head, root, cid);
      btn.onclick = async (ev) => {
        ev.preventDefault();
        ev.stopPropagation();
        modal.close();
        try {
          // Pin by cid (host can itself be ambiguous) and by profile — this
          // candidate row *is* the (runner, profile) combo the user picked
          // (§4a), so agent overrides whatever baseReq carried. Clear host so
          // the selector is unambiguous.
          const taskID = await window.harness.startInteractive({
            ...baseReq, host: "", runner: c.cid, agent: c.profile || "",
            caps: spawnCaps, resumeCapsOverride: baseReq.resumeTaskId ? applyCapsOnResume : false,
          });
          setActiveTab("terminal");
          term.reset();
          // mirrors the success path used by openInteractive; every
          // interactive open is a detachable session now.
          onInteractiveOpened(taskID, "session");
        } catch (e2) {
          alert(`startInteractive: ${e2.message}`);
        }
        // Same trailing fit/resize tail as openInteractive/reattachTo/
        // resumeTaskById, run unconditionally so a freshly-attached PTY gets
        // the actual terminal size instead of waiting on the next incidental
        // ResizeObserver fire.
        try { fit.fit(); } catch (_) { /* element not yet laid out */ }
        window.harness.resizeInteractive({ cols: term.cols, rows: term.rows });
      };
      list.appendChild(btn);
    });
    modal.showModal();
  }

  // routeAmbiguous opens the runner-picker modal when e is an ambiguous_runner
  // rejection and returns true (the caller should then `return`); false
  // otherwise. Shared by openInteractive AND the resume paths (resumeTaskById /
  // doResume) so every interactive-open surface gets the picker — not just the
  // Compose "Open" button. baseReq is the request that just failed; its
  // resumeTaskId/claudeArgs are reused for the cid-pinned retry.
  function routeAmbiguous(e, baseReq) {
    if (e && e.code === "ambiguous_runner" && Array.isArray(e.candidates)) {
      pickRunnerAndRetry(e.candidates, baseReq);
      return true;
    }
    return false;
  }

  // openInteractive opens a new interactive session — every interactive PTY
  // is a detachable, takeover-able session (the one-shot/non-detachable
  // variant was removed; a session you cannot re-enter had no upside).
  const openInteractive = async (label) => {
    const req = composeRequest();
    if (!req.repo && !req.resumeTaskId) {
      alert("select a repo or fill in Resume task id");
      return;
    }
    attachEpoch++;            // invalidate any in-flight close handler
    hideQuickReattach();
    term.reset();
    try {
      const taskID = await window.harness.startInteractive({...req, caps: spawnCaps, resumeCapsOverride: req.resumeTaskId ? applyCapsOnResume : false});
      setActiveTab("terminal");
      onInteractiveOpened(taskID, label);
    } catch (e) {
      attachedTask.textContent = "";
      currentSessionTaskId = "";
      if (routeAmbiguous(e, req)) return;
      alert(`startInteractive: ${e.message}`);
    }
    try { fit.fit(); } catch (_) { /* element not yet laid out */ }
    window.harness.resizeInteractive({ cols: term.cols, rows: term.rows });
  };

  document.getElementById("open-detachable").addEventListener("click", () => openInteractive("session"));

  document.getElementById("stop-streaming").addEventListener("click", () => {
    window.harness.detachInteractive();
    attachedTask.textContent = "";
    currentSessionTaskId = "";
    hideQuickReattach();
  });

  // "🔔 idleで通知": arm a one-shot await-idle watcher (sink=notify) on the
  // session the terminal is attached to. The fire arrives in the notification
  // feed AND the server's --notify-hook egress (e.g. the phone), so this is
  // the "tell me when it's done thinking" button before walking away.
  {
    const awaitIdleBtn = document.getElementById("await-idle-btn");
    const origLabel = awaitIdleBtn.textContent;
    let revertTimer = 0;
    const flash = (text) => {
      awaitIdleBtn.textContent = text;
      clearTimeout(revertTimer);
      revertTimer = setTimeout(() => { awaitIdleBtn.textContent = origLabel; }, 2500);
    };
    awaitIdleBtn.addEventListener("click", async () => {
      if (!currentSessionTaskId) { flash("🔔 セッション未接続"); return; }
      try {
        const r = await window.harness.awaitIdle({ taskId: currentSessionTaskId, sink: "notify" });
        flash(r.status === "armed" ? "🔔 armed ✓" : `🔔 ${r.status}`);
      } catch (e) {
        console.error("awaitIdle:", e);
        flash("🔔 error");
      }
    });
  }

  if (reattachQuick) {
    reattachQuick.addEventListener("click", () => reattachTo(reattachQuick.dataset.taskId));
  }

  document.getElementById("reattach").addEventListener("click", () => reattachTo(taskIdInput.value.trim()));

  // renderTaskList builds clickable task rows into #task-list. Each row toggles
  // an inline action sheet; every action derives the id from the row, so the
  // user never copies a 32-hex id by hand. Modeled on the file-picker list.
  // Function declaration so refreshSnapshot() (called earlier textually) can
  // invoke it via hoisting.
  // activityBadge renders the busy/idle label from a server-computed idle
  // age in ms (caller filters out the -1 "no output" sentinel).
  function activityBadge(idleMs) {
    if (idleMs < 3000) return "busy";
    if (idleMs >= 60000) return `idle:${Math.floor(idleMs / 60000)}m`;
    return `idle:${Math.floor(idleMs / 1000)}s`;
  }

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
      // Busy/idle badge from the server-computed idle age (-1 = no live
      // session output). Threshold mirrors cli.ActivityBusyThreshold (3s):
      // an in-flight agent TUI repaints ~every 100ms, an idle prompt emits
      // nothing, so 3s separates the two with wide margin.
      if (t.outputIdleMs >= 0) attr += `  act=${activityBadge(t.outputIdleMs)}`;
      if (t.caps) attr += `  caps=${t.caps}`;
      row.textContent = `${t.id.slice(0, 12)}…  ${t.status}  ${t.kind}  ${t.repoPath}${attr}  ${JSON.stringify(promptShort)}`;
      row.title = t.id; // full id on hover (desktop); tap the row → sheet has Copy id
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

  // copyText copies s to the clipboard. The WebUI is commonly served over plain
  // http on a LAN, where navigator.clipboard is unavailable (it needs a secure
  // context), so fall back to a hidden-textarea + execCommand("copy"). Returns
  // whether the copy succeeded.
  async function copyText(s) {
    try {
      if (navigator.clipboard && window.isSecureContext) {
        await navigator.clipboard.writeText(s);
        return true;
      }
    } catch (_) { /* fall through to legacy path */ }
    try {
      const ta = document.createElement("textarea");
      ta.value = s;
      ta.style.position = "fixed";
      ta.style.opacity = "0";
      document.body.appendChild(ta);
      ta.focus();
      ta.select();
      const ok = document.execCommand("copy");
      document.body.removeChild(ta);
      return ok;
    } catch (_) { return false; }
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

    // Full task id — selectable text + one-tap copy. The row only shows the
    // first 12 chars (cf78719 truncated it for the tappable layout), so this is
    // the way to recover the full id for pasting into a command (cmd-input or an
    // external shell). user-select:all (style.css) makes one tap select it all.
    const idRow = document.createElement("div");
    idRow.className = "task-id";
    const idText = document.createElement("span");
    idText.className = "task-id-text";
    idText.textContent = t.id;
    const copyBtn = document.createElement("button");
    copyBtn.type = "button";
    copyBtn.className = "task-action";
    copyBtn.textContent = "⧉ Copy id";
    copyBtn.addEventListener("click", async (e) => {
      e.stopPropagation();
      const ok = await copyText(t.id);
      copyBtn.textContent = ok ? "✓ copied" : "copy failed";
      setTimeout(() => { copyBtn.textContent = "⧉ Copy id"; }, 1200);
    });
    idRow.append(idText, copyBtn);
    sheet.appendChild(idRow);

    // Reattach / View / idle-notify — live interactive session only.
    if (t.kind === "Interactive" && (t.status === "Running" || t.status === "Detached")) {
      addItem("↪ Reattach", "", () => reattachTo(t.id, false));
      addItem("👁 View", "", () => reattachTo(t.id, true));
      addItem("🔔 idleで通知", "", async () => {
        try {
          const r = await window.harness.awaitIdle({ taskId: t.id, sink: "notify" });
          appendCmdOutput(`await-idle ${t.id.slice(0, 12)}: ${r.status}`, true);
        } catch (e) {
          appendCmdOutput(`await-idle: ${e.message}`, true);
        }
      });
    }

    // Resume — finished task's worktree, opened as a fresh interactive session.
    // Reflect the Compose "Extra claude args" box (same as Submit / Open) so a
    // resume can carry --permission-mode etc. without going through the cmdline.
    // Assigned variants mirror the TUI's r/R; any-runner variants mirror u/U and
    // intentionally skip t.assignedTo so the ambiguous runner picker can reopen.
    if (isTerminal) {
      const assignedRunner = typeof t.assignedTo === "string" && t.assignedTo && !t.assignedTo.startsWith(":") ? t.assignedTo : "";

      // Agent dropdown — defaults to this task's own last-run profile (§4b:
      // pinned resume resolves to the resumed task's own agent_profile unless
      // the caller overrides). Picking a different advertised profile here
      // reopens the same worktree under a different agent directly, without
      // needing the ambiguous-runner picker to supply it. `extra` keeps the
      // task's own profile selectable even if its runner is offline.
      const agentRow = document.createElement("div");
      agentRow.className = "task-agent-row";
      const agentLabel = document.createElement("span");
      agentLabel.className = "task-agent-label";
      agentLabel.textContent = "Agent:";
      const agentSel = document.createElement("select");
      agentSel.className = "task-agent-select";
      populateAgentSelect(agentSel, knownAgentProfiles, t.agentProfile || "", t.agentProfile);
      agentRow.append(agentLabel, agentSel);
      sheet.appendChild(agentRow);

      const doResume = async (claudeArgs, note, resumeConversation = false, runner = "") => {
        const req = { repo: "", host: "", agent: agentSel.value || "", claudeArgs, resumeTaskId: t.id, caps: spawnCaps, resumeCapsOverride: applyCapsOnResume, resumeConversation };
        if (runner) req.runner = runner;
        try {
          const id = await window.harness.startInteractive(req);
          setActiveTab("terminal");
          term.reset();
          attachedTask.textContent = `attached: ${id} (${note})`;
          currentSessionTaskId = id;
        } catch (err) {
          attachedTask.textContent = "";
          currentSessionTaskId = "";
          if (routeAmbiguous(err, req)) return;
          alert(`resume: ${err.message}`);
        }
        try { fit.fit(); } catch (_) {}
        window.harness.resizeInteractive({ cols: term.cols, rows: term.rows });
      };
      if (assignedRunner) {
        addItem("▶ Resume assigned", "", () => doResume(currentClaudeArgs(), "resumed assigned", false, assignedRunner));
        addItem("▶ Resume conversation assigned", "", () => doResume(currentClaudeArgs(), "resumed conversation assigned", true, assignedRunner));
      }
      addItem("▶ Resume any runner", "", () => doResume(currentClaudeArgs(), "resumed any runner"));
      addItem("▶ Resume conversation any runner", "", () => doResume(currentClaudeArgs(), "resumed conversation any runner", true));
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

  // ── Agentboard board panel ────────────────────────────────────────────────
  // Mirrors renderTaskList (main.js:1443) for list building and the Cancel
  // action pattern (main.js:1579) for confirm → harness.X → refresh.

  const boardTopicsEl   = document.getElementById("board-topics");
  const boardDetailEl   = document.getElementById("board-detail");
  const boardMessagesEl = document.getElementById("board-messages");
  const boardDetailTitle = document.getElementById("board-detail-title");
  const boardBackBtn    = document.getElementById("board-back-btn");
  const boardPurgeTopicBtn = document.getElementById("board-purge-topic-btn");
  const boardRefreshBtn = document.getElementById("board-refresh-btn");

  // currentBoardTopic tracks the topic open in the detail view.
  let currentBoardTopic = null;

  // prettyPayload tries JSON.parse + JSON.stringify(null,2); falls back raw.
  function prettyPayload(raw) {
    try {
      return JSON.stringify(JSON.parse(raw), null, 2);
    } catch (_) {
      return raw;
    }
  }

  // renderBoardTopics fetches and renders the topic list.
  async function renderBoardTopics() {
    boardDetailEl.hidden = true;
    boardTopicsEl.hidden = false;
    boardTopicsEl.innerHTML = "";
    if (!window.harness) {
      boardTopicsEl.textContent = "(not connected)";
      return;
    }
    try {
      const topics = await window.harness.boardTopics();
      if (!topics || topics.length === 0) {
        const empty = document.createElement("div");
        empty.style.color = "#666";
        empty.style.fontFamily = "monospace";
        empty.textContent = "(no topics)";
        boardTopicsEl.appendChild(empty);
        return;
      }
      for (const t of topics) {
        const row = document.createElement("div");
        row.className = "board-topic-row";
        const nameSpan = document.createElement("span");
        nameSpan.className = "board-topic-name";
        nameSpan.textContent = t.name;
        const metaSpan = document.createElement("span");
        metaSpan.className = "board-topic-meta";
        const lastTime = t.lastPublishedAtMs
          ? new Date(t.lastPublishedAtMs).toISOString()
          : "-";
        metaSpan.textContent = `msgs=${t.msgCount}  last=${lastTime}`;
        row.appendChild(nameSpan);
        row.appendChild(metaSpan);
        row.addEventListener("click", () => openBoardTopic(t.name));
        boardTopicsEl.appendChild(row);
      }
    } catch (err) {
      boardTopicsEl.textContent = `error: ${err.message}`;
    }
  }

  // openBoardTopic shows the detail view for one topic.
  async function openBoardTopic(topic) {
    currentBoardTopic = topic;
    boardTopicsEl.hidden = true;
    boardDetailEl.hidden = false;
    boardDetailTitle.textContent = topic;
    boardMessagesEl.innerHTML = "";
    if (!window.harness) {
      boardMessagesEl.textContent = "(not connected)";
      return;
    }
    try {
      const r = await window.harness.boardRead(topic);
      if (!r.found) {
        boardMessagesEl.textContent = "(topic not found)";
        return;
      }
      if (!r.msgs || r.msgs.length === 0) {
        boardMessagesEl.textContent = "(no messages)";
        return;
      }
      for (const m of r.msgs) {
        const card = document.createElement("div");
        card.className = "board-msg";

        const hdr = document.createElement("div");
        hdr.className = "board-msg-header";

        const seqSpan = document.createElement("span");
        seqSpan.className = "board-msg-seq";
        seqSpan.textContent = `#${m.seq}`;

        const fromSpan = document.createElement("span");
        fromSpan.className = "board-msg-from";
        fromSpan.textContent = `from=${m.fromTask ? m.fromTask.slice(0, 8) : "-"}`;

        const hostSpan = document.createElement("span");
        hostSpan.className = "board-msg-host";
        hostSpan.textContent = `host=${m.fromHostname || "-"}`;

        const timeSpan = document.createElement("span");
        timeSpan.className = "board-msg-time";
        timeSpan.textContent = m.receivedAtMs
          ? new Date(m.receivedAtMs).toISOString()
          : "-";

        const purgeBtn = document.createElement("button");
        purgeBtn.className = "board-msg-purge";
        purgeBtn.textContent = "✕";
        purgeBtn.title = `Purge message #${m.seq}`;
        purgeBtn.addEventListener("click", async (e) => {
          e.stopPropagation();
          if (!window.confirm(`Purge message #${m.seq} from "${topic}"?`)) return;
          try {
            await window.harness.boardPurge(topic, m.seq);
            openBoardTopic(topic);
          } catch (err) {
            appendCmdOutput(`boardPurge error: ${err.message}`);
          }
        });

        hdr.appendChild(seqSpan);
        hdr.appendChild(fromSpan);
        hdr.appendChild(hostSpan);
        hdr.appendChild(timeSpan);
        hdr.appendChild(purgeBtn);

        const pre = document.createElement("pre");
        pre.textContent = prettyPayload(m.payload || "");

        card.appendChild(hdr);
        card.appendChild(pre);
        boardMessagesEl.appendChild(card);
      }
    } catch (err) {
      boardMessagesEl.textContent = `error: ${err.message}`;
    }
  }

  if (boardBackBtn) {
    boardBackBtn.addEventListener("click", () => renderBoardTopics());
  }

  if (boardPurgeTopicBtn) {
    boardPurgeTopicBtn.addEventListener("click", async () => {
      if (!currentBoardTopic) return;
      if (!window.confirm(`Purge entire topic "${currentBoardTopic}"?`)) return;
      try {
        await window.harness.boardPurge(currentBoardTopic, 0);
        renderBoardTopics();
      } catch (err) {
        appendCmdOutput(`boardPurge topic error: ${err.message}`);
      }
    });
  }

  if (boardRefreshBtn) {
    boardRefreshBtn.addEventListener("click", () => {
      if (boardDetailEl && !boardDetailEl.hidden && currentBoardTopic) {
        openBoardTopic(currentBoardTopic);
      } else {
        renderBoardTopics();
      }
    });
  }

  // Activate renderBoardTopics when the board tab is selected.
  tabbar.addEventListener("click", (e) => {
    const btn = e.target.closest(".tab-btn");
    if (btn && btn.dataset.tab === "board") renderBoardTopics();
  });
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

// collectAgentProfiles returns the deduplicated union of every runner's
// advertised agent_profiles (in first-seen order). Shared by the Compose
// agent dropdown and each task-sheet's per-resume agent dropdown
// (multi-agent-profile design §6).
function collectAgentProfiles(runners) {
  const seen = new Set();
  const out = [];
  for (const r of (runners || [])) {
    for (const p of (r.agentProfiles || [])) {
      if (p && !seen.has(p)) { seen.add(p); out.push(p); }
    }
  }
  return out;
}

// populateAgentSelect rebuilds sel's options: "(default)" (value "") first,
// then one option per name in profiles. `extra`, when set and not already in
// profiles, is appended too — used to keep a task's own last-run profile
// selectable even if its runner is currently offline / not advertising it
// (e.g. a task resumed under "codex" while only a "claude" runner is up).
// Selects `selected` if present among the options, else falls back to "".
function populateAgentSelect(sel, profiles, selected, extra) {
  if (!sel) return;
  sel.innerHTML = "";
  const defOpt = document.createElement("option");
  defOpt.value = "";
  defOpt.textContent = "(default)";
  sel.appendChild(defOpt);
  const names = (profiles || []).slice();
  if (extra && !names.includes(extra)) names.push(extra);
  for (const name of names) {
    const opt = document.createElement("option");
    opt.value = name;
    opt.textContent = name;
    sel.appendChild(opt);
  }
  sel.value = names.includes(selected) ? selected : "";
}

// renderAgentSelect rebuilds the Compose agent <select>, preserving the
// previously-selected profile when it is still advertised.
function renderAgentSelect(sel, profiles) {
  if (!sel) return;
  populateAgentSelect(sel, profiles, sel.value);
}

function renderRunners(runners) {
  if (!runners || runners.length === 0) return "(none)";
  return runners.map(r => {
    const roots = (r.roots && r.roots.length > 0) ? r.roots.join(", ") : "(any)";
    const agents = (r.agentProfiles && r.agentProfiles.length > 0) ? r.agentProfiles.join(",") : (r.agentBin || "-");
    return `  ${pad(r.status, 8)} host=${r.hostname || "-"}  tasks=${r.tasks}/${r.maxTasks}  agents=${agents}  roots=${roots}`;
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
  const fp = beginFileProgress(file.name);
  try {
    try {
      await window.harness.filePushBytes(taskID, remoteRel, buf, false, fp.onProgress);
      return `push ok: ${file.name} -> ${remoteRel} (${buf.byteLength} bytes)`;
    } catch (e) {
      if (e && e.code === "already_exists") {
        if (!window.confirm(`${remoteRel} already exists on the runner. Overwrite?`)) {
          return "push cancelled (overwrite declined)";
        }
        await window.harness.filePushBytes(taskID, remoteRel, buf, true, fp.onProgress);
        return `push ok (overwritten): ${file.name} -> ${remoteRel} (${buf.byteLength} bytes)`;
      }
      throw e;
    }
  } finally {
    fp.end();
  }
}

async function filePullCmd(args) {
  // Parse flags before positional args (-r / --recursive => tar a directory).
  let recursive = false;
  const pos = [];
  for (const a of args) {
    if (a === "-r" || a === "--recursive") recursive = true;
    else pos.push(a);
  }
  if (pos.length !== 2) {
    throw new Error("usage: file pull [-r] <task-id> <worktree-rel-src>");
  }
  const [taskID, remoteRel] = pos;
  const fp = beginFileProgress(basename(remoteRel) + (recursive ? ".tar" : ""));
  try {
    if (recursive) {
      const bytes = await window.harness.filePullDirBytes(taskID, remoteRel, fp.onProgress);
      triggerDownload(bytes, basename(remoteRel) + ".tar");
      return `pull ok (tar): ${remoteRel} (${bytes.byteLength} bytes) — browser save dialog`;
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

// ============================================================
// Connection topology rendering
// ============================================================

// prevConnCids is the set of cid strings from the last render, used to key
// the enter/exit animation by diffing against the current set.
const prevConnCids = new Set();

// Topology zoom/pan state. The SVG is rebuilt on every ~5s poll, so the
// current viewBox must persist HERE (module scope) and be re-applied to each
// freshly-built SVG; otherwise a poll would snap the view back to default.
// topoBase is the unzoomed viewBox; topoView is the current (zoomed/panned) one.
let topoBase = null;   // {x,y,w,h} set on first render from W×H
let topoView = null;   // {x,y,w,h} current view; null = follow topoBase
let topoZoomWired = false;
let topoResetBtn = null; // re-appended after each rebuild (host.innerHTML clears children)

function topoApplyView(svg) {
  const v = topoView || topoBase;
  if (svg && v) svg.setAttribute("viewBox", `${v.x} ${v.y} ${v.w} ${v.h}`);
}

// attachTopoZoom wires wheel-zoom (around the cursor) and drag-pan onto the
// persistent topology container exactly once, plus a reset button. Handlers
// read the live <svg> each time (it is replaced on every poll).
function attachTopoZoom(host) {
  if (topoZoomWired) return;
  topoZoomWired = true;
  const svgOf = () => host.querySelector("svg");

  host.addEventListener("wheel", (e) => {
    const svg = svgOf();
    if (!svg || !topoBase) return;
    e.preventDefault();
    const v = topoView || { ...topoBase };
    const rect = svg.getBoundingClientRect();
    const fx = (e.clientX - rect.left) / rect.width;
    const fy = (e.clientY - rect.top) / rect.height;
    const factor = e.deltaY < 0 ? 0.9 : 1 / 0.9; // wheel up = zoom in
    let nw = v.w * factor, nh = v.h * factor;
    // clamp zoom to [0.3x, 3x] of base
    const minW = topoBase.w / 3, maxW = topoBase.w * 3;
    if (nw < minW) { nw = minW; nh = topoBase.h / 3; }
    if (nw > maxW) { nw = maxW; nh = topoBase.h * 3; }
    topoView = { x: v.x + (v.w - nw) * fx, y: v.y + (v.h - nh) * fy, w: nw, h: nh };
    topoApplyView(svg);
  }, { passive: false });

  let drag = null;
  host.addEventListener("mousedown", (e) => {
    const svg = svgOf();
    if (!svg) return;
    const rect = svg.getBoundingClientRect();
    drag = { sx: e.clientX, sy: e.clientY, rect, start: topoView || { ...topoBase } };
    host.classList.add("ct-grabbing");
  });
  window.addEventListener("mousemove", (e) => {
    if (!drag) return;
    const sxPerPx = drag.start.w / drag.rect.width;
    const syPerPx = drag.start.h / drag.rect.height;
    topoView = {
      x: drag.start.x - (e.clientX - drag.sx) * sxPerPx,
      y: drag.start.y - (e.clientY - drag.sy) * syPerPx,
      w: drag.start.w, h: drag.start.h,
    };
    topoApplyView(svgOf());
  });
  window.addEventListener("mouseup", () => { drag = null; host.classList.remove("ct-grabbing"); });

  const reset = document.createElement("button");
  reset.className = "ct-zoom-reset";
  reset.type = "button";
  reset.textContent = "⤢ reset";
  reset.title = "reset zoom";
  reset.addEventListener("click", () => { topoView = null; topoApplyView(svgOf()); });
  topoResetBtn = reset; // renderConnTopology re-appends it after each rebuild
}

// connAgeSec returns the age of a connection in seconds from its connectedAt
// unix-nano timestamp (as a JS number). Returns 0 for unset (0) values.
function connAgeSec(connectedAtNano) {
  if (!connectedAtNano) return 0;
  // connectedAt is unix nano; JS Date.now() is milliseconds.
  const nowNano = Date.now() * 1e6;
  const ageSec = (nowNano - connectedAtNano) / 1e9;
  return ageSec < 0 ? 0 : ageSec;
}

// connAgeStr returns a human-readable age string like "5s" or "3m12s".
function connAgeStr(connectedAtNano) {
  const secs = Math.floor(connAgeSec(connectedAtNano));
  if (secs < 60) return `${secs}s`;
  const m = Math.floor(secs / 60);
  const s = secs % 60;
  return `${m}m${s}s`;
}

// connIpPart extracts the IP portion from a "ip:port" remote address.
// Falls back to the full address if there's no ":" (unusual).
function connIpPart(remoteAddr) {
  // IPv6 addresses look like "[::1]:8540" — strip brackets too.
  if (remoteAddr.startsWith("[")) {
    const close = remoteAddr.lastIndexOf("]");
    if (close > 0) return remoteAddr.slice(1, close);
  }
  const lastColon = remoteAddr.lastIndexOf(":");
  return lastColon > 0 ? remoteAddr.slice(0, lastColon) : remoteAddr;
}

// groupConnsByIP groups the conns array into a Map<ip, connInfo[]>.
// isActiveTask: a task currently alive on its runner. Running = actively
// executing; Detached = interactive session alive with no client attached.
// Terminal (Succeeded/Failed/Cancelled) and not-yet-assigned Queued are excluded.
function isActiveTask(tk) {
  return tk && (tk.status === "Running" || tk.status === "Detached");
}

function groupConnsByIP(conns) {
  const map = new Map();
  for (const c of conns) {
    const ip = connIpPart(c.remoteAddr || "");
    if (!map.has(ip)) map.set(ip, []);
    map.get(ip).push(c);
  }
  return map;
}

// svgEl creates an SVG element with the given tag name and attributes.
function svgEl(tag, attrs) {
  const el = document.createElementNS("http://www.w3.org/2000/svg", tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    el.setAttribute(k, v);
  }
  return el;
}

// renderConnTopology renders the radial hub-and-spoke SVG topology into
// #conn-topology. Called on every snapshot poll.
//
// Layout:
//   - Server node at center (cx, cy).
//   - IP cluster nodes placed radially around the server at radius R1.
//   - Each connection a smaller leaf node on a spoke from its cluster toward
//     the server, at radius R2 (R2 < R1).
//   - New cids (not in prevConnCids) start with class "entering" then get
//     "visible" after a frame (CSS transition animates opacity/scale in).
//   - Removed cids get class "leaving" and are removed after the CSS
//     transition completes.
function renderConnTopology(conns, tasks) {
  const host = document.getElementById("conn-topology");
  if (!host) return;
  tasks = tasks || [];

  // On mobile, the topology container is hidden by CSS; skip heavy DOM work.
  if (window.matchMedia("(max-width: 600px)").matches) {
    // Still update prevConnCids so mobile list diff is correct.
    _updatePrevConnCids(conns);
    return;
  }

  if (!conns || conns.length === 0) {
    host.innerHTML = '<span class="ct-empty">(no connections)</span>';
    _updatePrevConnCids(conns);
    return;
  }

  const byIP = groupConnsByIP(conns);
  // Stable angular layout: sort IPs so a given IP always lands in the same
  // slot across polls. The server's snapshot order is non-deterministic
  // (it ranges a Go map), so without this the clusters swap positions on
  // every refresh.
  const clusters = [...byIP.keys()].sort();
  const nClusters = clusters.length;

  // SVG viewport: server at center, hierarchy radiates strictly OUTWARD —
  // server → cluster ring → connection leaves → tasks. Each level is further
  // from the centre, so depth reads as distance and the outer rings (longer
  // circumference) give crowded hosts more room. Squarer viewport since a
  // radial layout needs vertical room, not just width.
  const W = 640, H = 560; // taller so the outermost ring (dense cluster's
                          // tier-2 leaves + their tasks + labels) fits with
                          // margin; overflow:hidden then only clips on zoom-in
  const cx = W / 2, cy = H / 2;
  const R1 = 95;  // cluster ring (inner — closest to server)
  const R2 = 165; // connection-leaf ring (outside its cluster)
  const SERVER_R = 22;
  const CLUSTER_R = 14;
  const LEAF_R = 8;

  // Build a new SVG (replace the old one entirely; diff is handled via
  // class-based animation on the node group keyed by cid).
  topoBase = { x: 0, y: 0, w: W, h: H };
  attachTopoZoom(host);
  const svg = svgEl("svg", { viewBox: `0 0 ${W} ${H}` });
  topoApplyView(svg); // re-apply any persisted zoom/pan across the poll rebuild

  // --- Server node ---
  const serverG = svgEl("g", { class: "ct-server-node" });
  serverG.appendChild(svgEl("circle", { cx, cy, r: SERVER_R }));
  serverG.appendChild(Object.assign(svgEl("text", {
    class: "ct-server-label", x: cx, y: cy + SERVER_R + 3,
  }), { textContent: "server" }));
  // The server's address as this browser reached it. The WebUI is served by
  // the same process that owns the WS transport (see SERVER_CID), so
  // location.host IS the server address from this client's viewpoint —
  // there is no separate wire field for it.
  serverG.appendChild(Object.assign(svgEl("text", {
    class: "ct-server-addr", x: cx, y: cy + SERVER_R + 16,
  }), { textContent: location.host }));
  svg.appendChild(serverG);

  // --- Cluster nodes and their leaves ---
  const currentCids = new Set(conns.map(c => c.cid));

  clusters.forEach((ip, idx) => {
    const angle = (2 * Math.PI * idx) / nClusters - Math.PI / 2;
    const clx = cx + R1 * Math.cos(angle);
    const cly = cy + R1 * Math.sin(angle);

    // Spoke from server to cluster
    svg.appendChild(svgEl("line", {
      class: "ct-spoke",
      x1: cx, y1: cy, x2: clx, y2: cly,
    }));

    // Cluster circle
    const clG = svgEl("g", { class: "ct-cluster-node" });
    clG.appendChild(svgEl("circle", { cx: clx, cy: cly, r: CLUSTER_R }));
    const ipLabel = Object.assign(svgEl("text", {
      class: "ct-cluster-label",
      x: clx,
      y: cly + CLUSTER_R + 2,
    }), { textContent: ip });
    clG.appendChild(ipLabel);
    svg.appendChild(clG);

    // Leaf nodes for each connection in this IP cluster (sorted by cid so
    // leaves keep a stable position within the cluster across polls).
    const clConns = byIP.get(ip).slice().sort((a, b) =>
      a.cid < b.cid ? -1 : a.cid > b.cid ? 1 : 0);
    const nLeaves = clConns.length;
    // Bounded, tiered leaf layout. Keep each cluster's leaves inside its OWN
    // angular sector (2π/nClusters) so a crowded host can't overlap neighbouring
    // clusters, and spill onto concentric arcs (tiers) once one arc would pack
    // the circles too tightly — so leaves never overlap each other either.
    const sector = (2 * Math.PI) / nClusters;
    const fanHalf = Math.min(sector * 0.38, 0.55); // half-fan, with a gutter to neighbours
    const perTier = Math.max(1, Math.floor((2 * fanHalf * R2) / (LEAF_R * 2.6)));
    const showLeafLabel = nLeaves <= 6;            // hide per-leaf labels when dense
    clConns.forEach((conn, li) => {
      // Distribute within the bounded fan; extra leaves go to outer arcs.
      const tier = Math.floor(li / perTier);
      const inTier = li % perTier;
      const cntInTier = Math.min(perTier, nLeaves - tier * perTier);
      const t = cntInTier > 1 ? inTier / (cntInTier - 1) - 0.5 : 0; // -0.5..0.5
      const fanAngle = angle + t * 2 * fanHalf;
      const r = R2 + tier * (LEAF_R * 2.4);
      const lx = cx + r * Math.cos(fanAngle);
      const ly = cy + r * Math.sin(fanAngle);

      // Thin line from cluster to leaf
      svg.appendChild(svgEl("line", {
        class: "ct-leaf-spoke",
        x1: clx, y1: cly, x2: lx, y2: ly,
      }));

      // Leaf node group: keyed by cid for diff animation.
      const isNew    = !prevConnCids.has(conn.cid);
      const roleClass = `role-${conn.role || "unspecified"}`;
      const unidentCls = conn.identified ? "" : " unident";
      const leafG = svgEl("g", {
        class: `ct-conn-node ${roleClass}${unidentCls}`,
        "data-cid": conn.cid,
      });
      const leafCircle = svgEl("circle", { cx: lx, cy: ly, r: LEAF_R });
      // Age shade (spec: opacity/shade encodes age). Newer = brighter, older =
      // dimmer, flooring at ~0.45 for conns older than ~1h. Applied ONLY to
      // identified leaves — unidentified nodes keep the CSS dashed+dim (0.55)
      // styling, so we must not set an inline opacity that would override it.
      if (conn.identified) {
        const ageSec = connAgeSec(conn.connectedAt);
        const ageOpacity = Math.max(0.45, Math.min(1.0, 1 - ageSec / 3600));
        leafCircle.setAttribute("opacity", ageOpacity.toFixed(3));
      }
      leafG.appendChild(leafCircle);
      // Short role label below the leaf (suppressed for dense clusters, where
      // per-leaf labels would overlap; role is still conveyed by colour + legend).
      if (showLeafLabel) {
        const lLabelY = ly < cy ? ly - LEAF_R - 3 : ly + LEAF_R + 11;
        const roleLabel = Object.assign(svgEl("text", {
          class: "ct-conn-label",
          x: lx, y: lLabelY,
        }), { textContent: conn.role ? conn.role.slice(0, 3) : "?" });
        leafG.appendChild(roleLabel);
      }
      svg.appendChild(leafG);

      // Trigger enter animation: start "entering", flip to "visible" next frame.
      if (isNew) {
        leafG.classList.add("entering");
        requestAnimationFrame(() => {
          leafG.classList.remove("entering");
          leafG.classList.add("visible");
        });
      } else {
        leafG.classList.add("visible");
      }

      // Hang this runner's currently-active tasks off its leaf. This is an
      // ASSIGNMENT relationship (distinct from the connection lines), so tasks
      // render as squares on dashed branches. Pure client-side join: a runner
      // conn's cid equals the runner's registry id equals task.assignedTo.
      if (conn.role === "runner") {
        const myTasks = tasks
          .filter(tk => isActiveTask(tk) && tk.assignedTo === conn.cid)
          .sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
        const nT = myTasks.length;
        const tFanHalf = Math.min(fanHalf, 0.12 * Math.max(1, nT - 1));
        myTasks.forEach((tk, ti) => {
          const tt = nT > 1 ? ti / (nT - 1) - 0.5 : 0; // -0.5..0.5
          const tAngle = fanAngle + tt * 2 * tFanHalf;
          const tr = r + 24 + (ti % 2) * 12; // just OUTWARD from this leaf, staggered
          const tx = cx + tr * Math.cos(tAngle);
          const ty = cy + tr * Math.sin(tAngle);
          svg.appendChild(svgEl("line", {
            class: "ct-task-spoke", x1: lx, y1: ly, x2: tx, y2: ty,
          }));
          const taskG = svgEl("g", { class: "ct-task-node", "data-task": tk.id });
          const s = 6;
          taskG.appendChild(svgEl("rect", {
            x: tx - s, y: ty - s, width: 2 * s, height: 2 * s, rx: 1.5,
          }));
          const base = (tk.repoPath || "").split(/[\\/]/).filter(Boolean).pop();
          const label = base || (tk.id ? tk.id.slice(0, 6) : "task");
          // Place the label on the OUTWARD side (away from centre): nodes above
          // centre label upward into open space, nodes below label downward —
          // keeps text out of the crowded inner region.
          const tLabelY = ty < cy ? ty - s - 4 : ty + s + 11;
          taskG.appendChild(Object.assign(svgEl("text", {
            class: "ct-task-label", x: tx, y: tLabelY,
          }), { textContent: label }));
          svg.appendChild(taskG);
        });
      }
    });
  });

  // --- Legend ---
  // Cover every distinct marker in the graph, not just conn-role colours:
  // the server hub, the per-IP host clusters, and the task squares hung off
  // runner leaves each get an entry, so a screenshot of the topology is
  // self-describing (LLMs and humans alike misread unlabeled markers).
  const legendDiv = document.createElement("div");
  legendDiv.className = "ct-legend";
  const legendEntries = [
    ["kind-server", "server"],
    ["kind-host", "host (ip)"],
    ["role-cli", "cli"],
    ["role-tui", "tui"],
    ["role-webui", "webui"],
    ["role-agent", "agent"],
    ["role-runner", "runner"],
    ["role-unspecified", "unspecified"],
    ["kind-task", "task"],
  ];
  for (const [cls, label] of legendEntries) {
    const item = document.createElement("span");
    item.className = "ct-legend-item";
    const dot = document.createElement("span");
    dot.className = `ct-legend-dot ${cls}`;
    item.appendChild(dot);
    item.appendChild(document.createTextNode(label));
    legendDiv.appendChild(item);
  }

  // Replace existing content
  host.innerHTML = "";
  host.appendChild(svg);
  host.appendChild(legendDiv);
  if (topoResetBtn) host.appendChild(topoResetBtn); // survives the innerHTML clear

  // Handle leaving nodes: nodes that were in prevConnCids but are not in
  // currentCids. We do this before updating prevConnCids.
  // Since we rebuild the SVG each poll, we can't animate removals from the
  // OLD SVG (it's been replaced). Instead we track departures and add a
  // brief "phantom" leaving node to the NEW svg.
  const leavingCids = [...prevConnCids].filter(cid => !currentCids.has(cid));
  if (leavingCids.length > 0) {
    // Briefly show a dimmed leaving indicator at the server center.
    for (const cid of leavingCids) {
      const phantom = svgEl("circle", {
        cx: String(cx + (Math.random() - 0.5) * 30),
        cy: String(cy + (Math.random() - 0.5) * 30),
        r: String(LEAF_R),
        class: "leaving",
        fill: "#444",
        stroke: "#777",
        "stroke-width": "1",
        opacity: "0.7",
      });
      svg.appendChild(phantom);
      setTimeout(() => phantom.remove(), 400);
    }
  }

  _updatePrevConnCids(conns);
}

// _updatePrevConnCids syncs prevConnCids to the current conn set.
function _updatePrevConnCids(conns) {
  prevConnCids.clear();
  for (const c of conns || []) prevConnCids.add(c.cid);
}

// renderConnList renders the mobile grouped-list view into #conn-list.
// One card per IP, listing its connections with role badge, age, principal.
function renderConnList(conns, tasks) {
  const host = document.getElementById("conn-list");
  if (!host) return;
  tasks = tasks || [];

  host.innerHTML = "";

  if (!conns || conns.length === 0) {
    const empty = document.createElement("div");
    empty.className = "conn-list-empty";
    empty.textContent = "(no connections)";
    host.appendChild(empty);
    return;
  }

  const byIP = groupConnsByIP(conns);
  for (const [ip, ipConns] of byIP) {
    const card = document.createElement("div");
    card.className = "conn-ip-card";

    // IP header with a server-connector indicator
    const header = document.createElement("div");
    header.className = "conn-ip-header";
    const dot = document.createElement("span");
    dot.className = "conn-ip-connector";
    dot.title = "connected to server";
    header.appendChild(dot);
    header.appendChild(document.createTextNode(ip));
    card.appendChild(header);

    // One row per connection
    for (const conn of ipConns) {
      const row = document.createElement("div");
      row.className = "conn-row";

      // Role badge
      const badge = document.createElement("span");
      badge.className = `conn-role-badge role-${conn.role || "unspecified"}`;
      badge.textContent = conn.role || "?";
      row.appendChild(badge);

      // Unidentified badge
      if (!conn.identified) {
        const unidentBadge = document.createElement("span");
        unidentBadge.className = "conn-unident-badge";
        unidentBadge.textContent = "unident";
        unidentBadge.title = "handshake not yet completed (probe / failed auth)";
        row.appendChild(unidentBadge);
      }

      // Principal task (agent conns only — non-zero hex)
      const princ = conn.principalTask || "";
      // principalTask is 32 hex chars; all-zero means no principal
      if (princ && princ !== "0".repeat(32)) {
        const pEl = document.createElement("span");
        pEl.className = "conn-principal";
        pEl.title = `principal: ${princ}`;
        pEl.textContent = princ.slice(0, 8) + "…";
        row.appendChild(pEl);
      }

      // Age (right-aligned)
      const ageEl = document.createElement("span");
      ageEl.className = "conn-age";
      ageEl.textContent = connAgeStr(conn.connectedAt);
      row.appendChild(ageEl);

      card.appendChild(row);

      // Active tasks running on this runner (assignment join: cid == assignedTo).
      if (conn.role === "runner") {
        const myTasks = tasks
          .filter(tk => isActiveTask(tk) && tk.assignedTo === conn.cid)
          .sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
        for (const tk of myTasks) {
          const trow = document.createElement("div");
          trow.className = "conn-task-row";
          const base = (tk.repoPath || "").split(/[\\/]/).filter(Boolean).pop();
          trow.textContent = "↳ " + (base || (tk.id ? tk.id.slice(0, 6) : "task"));
          if (tk.id) trow.title = tk.id;
          card.appendChild(trow);
        }
      }
    }
    host.appendChild(card);
  }
}
