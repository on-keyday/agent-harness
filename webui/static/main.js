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
  let spawnScope = "";       // serialized scope grammar for spawns; "" = the subtree default
  // Mirrors protocol.IsPTYKind (runner/protocol/task_kind.go). Two kinds are
  // sessions; only one drives a TERMINAL, so every site has to say which
  // question it is asking — a bare `kind === "Interactive"` reads as an
  // oversight the next reader will "fix" by adding "Stream" to it.
  //
  // The IsSessionKind half has no caller here yet, so it is not defined: an
  // unused predicate is one more thing to keep in step with Go for nothing.
  const isPTYKind = (t) => !!t && t.kind === "Interactive";
  // The other session kind. Spelled out for the same reason isPTYKind is: a
  // bare kind === "Stream" reads as an oversight the next reader will "fix".
  const isStreamKind = (t) => !!t && t.kind === "Stream";

  let spawnBase = "subtree"; // scope base radio state (spawn picker)
  // The base's other half. Declared beside spawnBase rather than near its
  // checkbox: the echo updater runs during init, before the control is wired.
  let spawnExcludeSelf = false;
  const spawnScopeIds = new Set(); // checked task ids (spawn picker)
  // The visibility half. "" is the third radio state, "follows the action
  // base", and is the default — not an absent value.
  let spawnVisBase = "";
  const spawnVisIds = new Set(); // +vis-ids: see-only task ids (spawn picker)
  // Per-capability override rows, one list per dialog. Declared HERE with the
  // other spawn state rather than beside the builder, because initCaps ->
  // initScope runs before that point in the file and a const in the temporal
  // dead zone throws rather than reading as undefined.
  const overrideRowsFor = { spawn: [], regrant: [] };
  // Declared here (not beside the snapshot poller) because initCaps →
  // initScope → the spawn checklist reads it before the poller section runs.
  let lastTasks = [];        // latest snapshot; re-render source for filter events

  // sessionReq builds the ONE request object shape the harness.submit /
  // harness.startInteractive wasm bridges consume. Every session request goes
  // through here so the JS↔Go key names cannot drift and the caps logic is not
  // hand-copied per call site (it was: five sites, and two of them dropped the
  // "override only applies on resume" guard — harmless only because those two
  // always had a resumeTaskId, i.e. a latent footgun). Callers pass just what
  // varies; spawnCaps / applyCapsOnResume are read from the live closure here.
  //   resumeCapsOverride is forced false for a NEW task (no resumeTaskId): the
  //   override re-grants caps on RESUME and is a no-op / misleading otherwise.
  // Keys here MUST match cmd/harness-webui-wasm/main.go's opts.Get("…") names.
  function sessionReq({ repo = "", task = "", host = "", runner = "", agent = "",
                        claudeArgs = [], resumeTaskId = "", resumeConversation = false,
                        eventStream = false }) {
    const req = {
      repo, task, host, agent,
      claudeArgs,
      resumeTaskId,
      resumeConversation,
      // TaskKind_Stream: structured events instead of a PTY. The wasm side
      // declines to mount the xterm for it; the chat panel attaches instead.
      eventStream,
      caps: spawnCaps,
      scope: spawnScope,
      scopeFor: overrideSpecsFrom("spawn"),
      // One checkbox gates BOTH halves of the resume re-grant: silently
      // applying the Compose scope picker's leftover state to a resumed
      // task would be the exact invisible rewrite scope_present exists to
      // prevent.
      resumeCapsOverride: resumeTaskId ? applyCapsOnResume : false,
      scopePresent: resumeTaskId ? applyCapsOnResume : false,
    };
    if (runner) req.runner = runner;
    return req;
  }

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

  // Task-list filter bar. Lives OUTSIDE #task-list so the snapshot poll's
  // re-render never steals input focus or resets chip selection.
  const taskFilterInput = document.getElementById("task-filter-input");
  const taskChips = {
    active:   document.getElementById("task-chip-active"),
    finished: document.getElementById("task-chip-finished"),
    all:      document.getElementById("task-chip-all"),
  };
  let taskStatusFilter = "active"; // "active" | "finished" | "all"
  // Creator-tree ordering. A MODE, not a filter: while it is on the status
  // chips are ignored, because a tree with its middle links filtered out is a
  // set of disconnected fragments pretending to be a hierarchy.
  let taskTreeMode = false;
  let lastTaskTree = [];           // wasm-computed order+gutter; see snapshot()
  let lastForwards = [];           // latest snapshot; `forward ls` reads this, no second RPC
  for (const [key, btn] of Object.entries(taskChips)) {
    btn.addEventListener("click", () => {
      taskStatusFilter = key;
      for (const b of Object.values(taskChips)) b.classList.toggle("is-active", b === btn);
      renderTaskList(lastTasks);
    });
  }
  // revealTaskInList opens one task's sheet, widening the filters first if that
  // task is not currently listed.
  //
  // The graph shows every visible task; the list shows the filtered ones. A
  // click on a node outside the filter used to do nothing at all — no sheet, no
  // message — which is the worst of the three possible behaviours. Widening is
  // visible in itself (the chip moves, the search box empties), so the operator
  // can see why the list changed under them.
  function revealTaskInList(id) {
    const find = () => taskList.querySelector(`.task-sheet[data-task-id="${id}"]`);
    if (!find()) {
      taskStatusFilter = "all";
      for (const [k, b] of Object.entries(taskChips)) b.classList.toggle("is-active", k === "all");
      taskFilterInput.value = "";
      renderTaskList(lastTasks);
    }
    const sheet = find();
    if (!sheet) return;
    // Same single-open rule a row click follows. Without it two sheets end up
    // open, and the next poll's rebuild restores only the first one in DOM
    // order — so the sheet this click opened could quietly close again.
    for (const s of taskList.querySelectorAll(".task-sheet")) {
      if (s !== sheet) s.hidden = true;
    }
    sheet.hidden = false;
    sheet.scrollIntoView({ block: "center", behavior: "smooth" });
  }

  const taskTreeChip = document.getElementById("task-chip-tree");
  if (taskTreeChip) {
    taskTreeChip.addEventListener("click", () => {
      // The chip opens a DIAGRAM above the list; it does not touch the list.
      // An earlier version reordered and indented the rows instead, which meant
      // the status chips and the text filter had to stop applying (filtering a
      // hierarchy leaves disconnected fragments) — and the indent was unreadable
      // anyway, because a task card is tall enough that its neighbours never sit
      // close enough for the gutter to line up. The graph carries the hierarchy;
      // the list stays a list.
      taskTreeMode = !taskTreeMode;
      taskTreeChip.classList.toggle("is-active", taskTreeMode);
      renderTaskTreeGraph(taskTreeMode ? lastTaskTree : null, lastTasks, taskStatusColor, revealTaskInList);
    });
  }
  taskFilterInput.addEventListener("input", () => renderTaskList(lastTasks));

  // Grid selection. A live interactive session is INCLUDED in the grid by
  // default; gridExcluded holds the ids the user unchecked. Keyed by id so it
  // survives the snapshot poll's task-list re-render, and a newly-created
  // session is auto-included. The global "グリッド表示" button opens the grid of
  // all currently-included sessions (activity-desc).
  const gridExcluded = new Set();
  const gridShowBtn = document.getElementById("grid-show-btn");
  const gridAllOn   = document.getElementById("grid-all-on");
  const gridAllOff  = document.getElementById("grid-all-off");
  const liveInteractiveTasks = () =>
    (lastTasks || []).filter(
      // isPTYKind: the grid paints terminal panes.
      (t) => isPTYKind(t) && (t.status === "Running" || t.status === "Detached"));
  gridShowBtn.addEventListener("click", () => {
    const ids = liveInteractiveTasks()
      .filter((t) => !gridExcluded.has(t.id))
      .sort((a, b) => taskActivityMs(b) - taskActivityMs(a))
      .map((t) => t.id);
    if (!ids.length) { appendCmdOutput("grid: no sessions selected", true); return; }
    openSessionGrid(ids, "all");
  });
  gridAllOn.addEventListener("click", () => { gridExcluded.clear(); renderTaskList(lastTasks); });
  gridAllOff.addEventListener("click", () => {
    for (const t of liveInteractiveTasks()) gridExcluded.add(t.id);
    renderTaskList(lastTasks);
  });

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
  const fileMkdirBtn      = document.getElementById("file-mkdir-btn");
  const fileNewBtn        = document.getElementById("file-new-btn");
  const fileEntriesUL     = document.getElementById("file-entries");
  const filePushBtn       = document.getElementById("file-push-btn");
  const filePullBtn       = document.getElementById("file-pull-btn");
  const filePullDirBtn    = document.getElementById("file-pull-dir-btn");
  const filePreviewBtn    = document.getElementById("file-preview-btn");
  const fileEditBtn       = document.getElementById("file-edit-btn");
  const fileDeleteBtn     = document.getElementById("file-delete-btn");
  const fileResultPre     = document.getElementById("file-result");
  const filePreviewModal  = document.getElementById("file-preview-modal");
  const filePreviewTitle  = document.getElementById("file-preview-title");
  const filePreviewBody   = document.getElementById("file-preview-body");
  const filePreviewClose  = document.getElementById("file-preview-close");
  const filePreviewToggle = document.getElementById("file-preview-toggle");
  const filePreviewCopy   = document.getElementById("file-preview-copy");
  const filePreviewEdit   = document.getElementById("file-preview-edit");
  const filePreviewApi    = document.getElementById("file-preview-api");
  const filePreviewReload = document.getElementById("file-preview-reload");
  const filePreviewRefetch = document.getElementById("file-preview-refetch");
  const filePreviewRefetchLabel = document.getElementById("file-preview-refetch-label");
  // What the open preview was loaded from, so Reload and the opt-in refetch can
  // re-pull it. filePickerSelected is not usable for that: the picker keeps
  // working behind the modal.
  let previewSource = null;
  const filePreviewModals = document.getElementById("file-preview-modals");
  const filePreviewModalsLabel = document.getElementById("file-preview-modals-label");
  // The host:port the OPERATOR pinned for the rendered preview, or null. This
  // is the only thing the bridge below trusts: the shim runs in the page's own
  // realm and can be deleted or lied to by the page it is meant to constrain.
  let previewPin = null;
  // Bumped on every close and every re-render. A reply that arrives for an
  // older generation belongs to a preview that is gone, so it is dropped
  // rather than posted into whatever iframe is there now.
  let previewApiGen = 0;
  let previewApiInFlight = 0;
  // Key for the pin's registration, and the forward id an operator would kill.
  // One preview modal at a time, so the key is constant; it exists because the
  // wasm side is keyed like every other pane holder.
  const PREVIEW_PIN_KEY = "file-preview";
  let previewPinForwardID = null;
  const PREVIEW_FETCH_MAX_INFLIGHT = 4;
  // Set when the current preview is HTML, so the toggle can rebuild the body
  // from already-fetched bytes without re-pulling. Reset on modal close.
  let filePreviewHtml = null; // { rel, size, bytes, mode: "render" | "source" }
  // What the Copy button writes to the clipboard for the current preview:
  //   { text }            => clipboard.writeText
  //   { blob, type }      => clipboard.write (image, best-effort by MIME)
  // null while showing an error/oversize note (nothing copyable) => button hidden.
  let filePreviewCopyPayload = null;
  // Worktree-relative path of the open preview when it rendered as editable
  // text — the only case that offers Edit. null hides the button.
  let filePreviewEditRel = null;

  // Preview never pulls more than this into browser memory; oversize files
  // are rejected up front using the size from fileLs (no fetch attempted).
  // Preview caps are per renderer, not one number, because the renderers do
  // very different things with the bytes. Only the EXTENSION is known here:
  // the check runs on the size from fileLs so an oversize file is refused
  // without being pulled, and isLikelyBinary needs the bytes themselves, so a
  // binary file with no telling extension is capped as text.
  const PREVIEW_MAX_BYTES_HTML  = 8 * 1024 * 1024; // decoded to a string, then srcdoc
  const PREVIEW_MAX_BYTES_IMAGE = 8 * 1024 * 1024; // Blob + object URL; the browser owns the decode
  const PREVIEW_MAX_BYTES       = 4 * 1024 * 1024; // <pre>: layout cost grows with the line count
  function previewMaxBytesFor(name) {
    if (isHtmlExt(name)) return PREVIEW_MAX_BYTES_HTML;
    if (isImageExt(name)) return PREVIEW_MAX_BYTES_IMAGE;
    return PREVIEW_MAX_BYTES;
  }
  // Binary files are rendered as a hex dump truncated to this many bytes.
  const HEX_PREVIEW_MAX_BYTES = 4 * 1024;    // 4 KiB
  // Sandbox tokens for the rendered-HTML iframe. See showHtmlPreview for why
  // the split is "does this interrupt the operator", and why allow-same-origin
  // appears in neither list.
  const PREVIEW_SANDBOX_BASE = "allow-scripts allow-forms allow-pointer-lock";
  const PREVIEW_SANDBOX_INTERRUPTING = "allow-modals allow-popups allow-downloads";
  // Object URL held open for an image preview; revoked when the modal closes.
  let filePreviewObjectURL = null;

  let filePickerCurDir   = "";
  let filePickerEntries  = [];
  let filePickerSelected = null; // {name, size, mode, isDir} or null

  // Terminal (finished) task states; gates Resume vs Cancel in the action sheet.
  const TERMINAL_STATES = new Set(["Succeeded", "Failed", "Cancelled"]);
  // taskSessionAlive is the one predicate behind several questions that look
  // different but are not: can it hold a forward, does it have a worktree the
  // file panel can list, can it be reattached. The server draws the same line.
  // Status strings are TaskStatus.String(), not lowercase.
  const taskSessionAlive = (t) => t && (t.status === "Running" || t.status === "Detached");

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
    lastTaskTree = snap.taskTree || [];
    renderTaskList(snap.tasks);
    renderTaskTreeGraph(taskTreeMode ? lastTaskTree : null, lastTasks, taskStatusColor, revealTaskInList);
    renderFileTaskSelect(snap.tasks);
    if (window.__renderGitTaskSelect) window.__renderGitTaskSelect(snap.tasks);
    renderRawTaskSelect(snap.tasks);
    // Connection topology — rides the same ~5s poll (spec decision #3:
    // no separate event subscription in wasm). snap.conns may be absent if
    // the server doesn't have the list_conns capability yet; guard with [].
    const conns = snap.conns || [];
    const allTasks = snap.tasks || [];
    renderConnTopology(conns, allTasks, snap.forwards || []);
    renderConnList(conns, allTasks);
    renderForwardList(snap.forwards || []);
  };

  // --- Raw connect pane -------------------------------------------------------
  // One entry per open connection, keyed by the pane key wasm returned. A
  // connection's bytes are kept as a chunk list capped at RAW_RING_BYTES: the
  // output view is a debugging window, not a transcript, and an unbounded buffer
  // on a chatty port would grow until the tab died.
  const RAW_RING_BYTES = 256 * 1024;
  // The front of a connection's output is never dropped. A pure keep-the-newest
  // ring is right for a log and wrong here: what arrives FIRST is the status
  // line and the headers, and a response larger than the ring pushed exactly
  // that out — a body with no indication of what it answered, with the opening
  // tag unreachable however far back you scrolled.
  const RAW_HEAD_KEEP_BYTES = 32 * 1024;
  const rawPanes = new Map(); // key -> {key, task, host, port, head, chunks, elided, bytes, sent, open, note}
  let rawActiveKey = null;
  let rawViewMode = "text"; // "text" | "hex"

  function rawPane(key) { return rawPanes.get(key) || null; }

  function rawAppend(key, bytes) {
    const p = rawPane(key);
    if (!p) return;
    p.bytes += bytes.length;

    // Fill the head reserve first; only the remainder joins the ring.
    let headLen = p.head.reduce((n, c) => n + c.length, 0);
    if (headLen < RAW_HEAD_KEEP_BYTES) {
      const take = Math.min(RAW_HEAD_KEEP_BYTES - headLen, bytes.length);
      p.head.push(bytes.subarray(0, take));
      bytes = bytes.subarray(take);
      if (bytes.length === 0) { if (key === rawActiveKey) renderRawOutput(); return; }
    }

    p.chunks.push(bytes);
    let total = p.chunks.reduce((n, c) => n + c.length, 0);
    const cap = RAW_RING_BYTES - RAW_HEAD_KEEP_BYTES;
    while (total > cap && p.chunks.length > 1) {
      const gone = p.chunks.shift();
      total -= gone.length;
      p.elided += gone.length;
    }
    if (key === rawActiveKey) renderRawOutput();
  }

  // rawBytesOf splices head + (marker) + tail. The marker is explicit because a
  // silent gap reads as a complete response.
  function rawBytesOf(p) {
    const marker = p.elided ? new TextEncoder().encode(`\n… ${p.elided} bytes elided …\n`) : null;
    const parts = marker ? [...p.head, marker, ...p.chunks] : [...p.head, ...p.chunks];
    const total = parts.reduce((n, c) => n + c.length, 0);
    const out = new Uint8Array(total);
    let off = 0;
    for (const c of parts) { out.set(c, off); off += c.length; }
    return out;
  }

  // Text view keeps newlines and tabs and replaces every other control byte with
  // "." — the output is arbitrary bytes, and letting them through would let a
  // remote service drive the page's rendering.
  function rawRenderText(bytes) {
    const decoded = new TextDecoder("utf-8", { fatal: false }).decode(bytes);
    // Keep \n, \r and \t; replace every other C0/C1 control and DEL with ".".
    // Letting them through would let a remote service drive the page's rendering.
    return decoded.replace(/[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f-\u009f]/g, ".");
  }

  function rawRenderHex(bytes) {
    const lines = [];
    for (let i = 0; i < bytes.length; i += 16) {
      const slice = bytes.subarray(i, i + 16);
      const hex = Array.from(slice, (b) => b.toString(16).padStart(2, "0")).join(" ").padEnd(47, " ");
      const ascii = Array.from(slice, (b) => (b >= 0x20 && b < 0x7f ? String.fromCharCode(b) : ".")).join("");
      lines.push(`${i.toString(16).padStart(8, "0")}  ${hex}  ${ascii}`);
    }
    return lines.join("\n");
  }

  function renderRawOutput() {
    const out = document.getElementById("raw-output");
    const counters = document.getElementById("raw-counters");
    if (!out) return;
    const p = rawActiveKey ? rawPane(rawActiveKey) : null;
    if (!p) {
      out.textContent = "";
      if (counters) counters.textContent = "";
      return;
    }
    const bytes = rawBytesOf(p);
    // Stick to the bottom only while the reader is already there. This used to
    // scroll to the bottom unconditionally on every render, so a response
    // taller than the pane could not be scrolled back to — the top looked
    // truncated when it was merely out of view. Same rule as the TUI log
    // pane's stickToBottom. Measured BEFORE the content is replaced, because
    // replacing it changes scrollHeight.
    const atBottom = out.scrollHeight - out.scrollTop - out.clientHeight < 8;
    out.textContent = rawViewMode === "hex" ? rawRenderHex(bytes) : rawRenderText(bytes);
    if (atBottom) out.scrollTop = out.scrollHeight;
    if (counters) {
      counters.textContent = `${p.open ? "● open" : "○ closed"}  in ${p.bytes}B / out ${p.sent}B` +
        (p.note ? `  ${p.note}` : "");
      // A pane that has not resolved yet reads "○ closed  connecting…", which is
      // accurate: nothing can be sent on it and × cancels it.
    }
    const sendBtn = document.getElementById("raw-send-btn");
    const closeBtn = document.getElementById("raw-close-btn");
    const httpBtn = document.getElementById("raw-http-send-btn");
    if (sendBtn) sendBtn.disabled = !p.open;
    if (closeBtn) closeBtn.disabled = !p.open;
    if (httpBtn) httpBtn.disabled = !p.open;
  }

  function renderRawTabs() {
    const host = document.getElementById("raw-tabs");
    if (!host) return;
    host.textContent = "";
    for (const p of rawPanes.values()) {
      const tab = document.createElement("button");
      tab.type = "button";
      tab.className = "raw-tab" + (p.key === rawActiveKey ? " is-active" : "") + (p.open ? "" : " is-closed");
      // note carries "connecting…", a close reason, or a connect failure.
      tab.textContent = `${p.host}:${p.port}`;
      // Why it ended, on hover. The tab styling says a pane is dead; only the
      // note says whether that was a local close, a remote kill, or the client
      // connection dropping.
      if (p.note) tab.title = p.note;
      tab.addEventListener("click", () => { rawActiveKey = p.key; renderRawTabs(); renderRawOutput(); });
      const drop = document.createElement("span");
      drop.className = "raw-tab-x";
      drop.textContent = "×";
      drop.addEventListener("click", async (ev) => {
        ev.stopPropagation();
        // Unconditional: a pane still connecting has no `open` flag yet, and it is
        // exactly the case that must be cancellable.
        try { await window.harness.rawClose(p.key); } catch (err) { appendCmdOutput(`rawClose: ${err.message}`); }
        rawPanes.delete(p.key);
        if (rawActiveKey === p.key) rawActiveKey = rawPanes.size ? [...rawPanes.keys()][0] : null;
        renderRawTabs();
        renderRawOutput();
        refreshSnapshot();
      });
      tab.appendChild(drop);
      host.appendChild(tab);
    }
  }

  // Hooks invoked from wasm (see cli/raw_forward_wasm.go). Both are keyed by pane.
  window.harness_rawData = (key, arr) => rawAppend(key, new Uint8Array(arr));
  window.harness_rawClosed = (key, reason) => {
    const p = rawPane(key);
    if (!p) return;
    p.open = false;
    p.note = reason || "closed";
    renderRawTabs();
    renderRawOutput();
    refreshSnapshot(); // the registration is gone; the forward list should agree
  };

  const rawTaskSelect = document.getElementById("raw-task-select");
  const rawConnectBtn = document.getElementById("raw-connect-btn");
  const rawSendInput  = document.getElementById("raw-send-input");
  const rawSendBtn    = document.getElementById("raw-send-btn");
  const rawCloseBtn   = document.getElementById("raw-close-btn");
  const rawHexInput   = document.getElementById("raw-hex-input");

  // HTTP request form. The page never assembles request bytes: it hands the
  // four fields to wasm, which builds them with the same cli.BuildHTTPRequest
  // the CLI and the TUI use, so a request that works from one surface works
  // from all three. Headers go over as a newline-separated string, one per
  // line, matching the TUI's textarea — no separator syntax to invent.
  const rawModeBytes = document.getElementById("raw-mode-bytes");
  const rawModeHTTP  = document.getElementById("raw-mode-http");
  const rawHTTPForm  = document.getElementById("raw-http-form");
  const rawBytesRow  = document.getElementById("raw-bytes-row");
  function setRawMode(mode) {
    const http = mode === "http";
    if (rawHTTPForm) rawHTTPForm.hidden = !http;
    if (rawBytesRow) rawBytesRow.hidden = http;
    if (rawModeBytes) rawModeBytes.classList.toggle("is-active", !http);
    if (rawModeHTTP) rawModeHTTP.classList.toggle("is-active", http);
  }
  if (rawModeBytes) rawModeBytes.addEventListener("click", () => setRawMode("bytes"));
  if (rawModeHTTP) rawModeHTTP.addEventListener("click", () => setRawMode("http"));
  const rawHTTPSendBtn = document.getElementById("raw-http-send-btn");
  if (rawHTTPSendBtn) rawHTTPSendBtn.addEventListener("click", async () => {
    if (!rawActiveKey) return;
    const spec = {
      method:  document.getElementById("raw-http-method").value,
      path:    document.getElementById("raw-http-path").value,
      headers: document.getElementById("raw-http-headers").value,
      body:    document.getElementById("raw-http-body").value,
    };
    try {
      const p = rawPane(rawActiveKey);
      const sent = await window.harness.rawSendHTTP(rawActiveKey, spec);
      // wasm reports the count because the page never builds the bytes; without
      // this the "out" counter read 0B after every HTTP send.
      if (p) { p.sent += sent; renderRawOutput(); }
    } catch (err) {
      // A build error (bad path, CR in a header) is reported on the pane the
      // way a close reason is; nothing was sent.
      const p = rawPane(rawActiveKey);
      if (p) { p.note = `http: ${err.message}`; renderRawTabs(); }
      appendCmdOutput(`rawSendHTTP: ${err.message}`);
    }
  });

  // renderRawTaskSelect mirrors renderFileTaskSelect: only Running/Detached
  // tasks can hold a forward, so only those are offered.
  function renderRawTaskSelect(tasks) {
    if (!rawTaskSelect) return;
    const prev = rawTaskSelect.value;
    rawTaskSelect.textContent = "";
    const none = document.createElement("option");
    none.value = "";
    none.textContent = "(select task)";
    rawTaskSelect.appendChild(none);
    for (const t of tasks || []) {
      if (!taskSessionAlive(t)) continue;
      const opt = document.createElement("option");
      opt.value = t.id;
      opt.textContent = `${t.id.slice(0, 8)}… ${t.repoPath || ""}`.trim();
      rawTaskSelect.appendChild(opt);
    }
    rawTaskSelect.value = prev;
  }

  let rawKeySeq = 0;

  rawConnectBtn?.addEventListener("click", async () => {
    const task = rawTaskSelect.value;
    const host = document.getElementById("raw-host-input").value.trim();
    const port = parseInt(document.getElementById("raw-port-input").value, 10);
    if (!task || !host || !(port > 0 && port < 65536)) {
      appendCmdOutput("raw connect: task, host and port are required");
      return;
    }
    // The page mints the key and shows the tab before the open resolves, so a
    // connect to an unreachable host can be abandoned with × while it is still
    // in flight — rawClose(key) supersedes the open rather than leaving a
    // registered forward behind.
    const key = `raw${++rawKeySeq}`;
    rawPanes.set(key, { key, task, host, port, head: [], chunks: [], elided: 0, bytes: 0, sent: 0, open: false, note: "connecting…" });
    rawActiveKey = key;
    renderRawTabs();
    renderRawOutput();
    try {
      await window.harness.rawOpen(key, task, host, port);
      const p = rawPane(key);
      if (!p) return; // closed with × while connecting; wasm discarded the open
      p.open = true;
      p.note = "";
      renderRawTabs();
      renderRawOutput();
      refreshSnapshot(); // the new registration should appear in the forward list
    } catch (err) {
      const p = rawPane(key);
      // p is null when the operator already cancelled with × while the open
      // was in flight — that pane was deliberately discarded, so logging a
      // "raw connect error" for it here would be a spurious line about a
      // connection the operator already dismissed.
      if (p) {
        p.open = false;
        p.note = `connect failed: ${err.message}`;
        appendCmdOutput(`raw connect error: ${err.message}`);
      }
      renderRawTabs();
      renderRawOutput();
    }
  });

  // hexToBytes accepts "48 65 6c" / "48656c" and rejects anything else, so a
  // typo sends nothing rather than sending garbage.
  function hexToBytes(s) {
    const clean = s.replace(/\s+/g, "");
    if (clean.length === 0 || clean.length % 2 !== 0 || /[^0-9a-fA-F]/.test(clean)) return null;
    const out = new Uint8Array(clean.length / 2);
    for (let i = 0; i < out.length; i++) out[i] = parseInt(clean.substr(i * 2, 2), 16);
    return out;
  }

  async function rawSendCurrent() {
    const p = rawActiveKey ? rawPane(rawActiveKey) : null;
    if (!p || !p.open) return;
    const text = rawSendInput.value;
    let bytes;
    if (rawHexInput.checked) {
      bytes = hexToBytes(text);
      if (!bytes) { appendCmdOutput("raw send: not valid hex"); return; }
    } else {
      const nl = document.getElementById("raw-newline").value;
      const suffix = nl === "crlf" ? "\r\n" : nl === "lf" ? "\n" : "";
      bytes = new TextEncoder().encode(text + suffix);
    }
    try {
      await window.harness.rawSend(p.key, bytes);
      p.sent += bytes.length;
      rawSendInput.value = "";
      renderRawOutput();
    } catch (err) {
      appendCmdOutput(`raw send error: ${err.message}`);
    }
  }

  rawSendBtn?.addEventListener("click", rawSendCurrent);
  rawSendInput?.addEventListener("keydown", (ev) => { if (ev.key === "Enter") rawSendCurrent(); });
  rawCloseBtn?.addEventListener("click", async () => {
    const p = rawActiveKey ? rawPane(rawActiveKey) : null;
    if (!p || !p.open) return;
    try { await window.harness.rawClose(p.key); } catch (err) { appendCmdOutput(`rawClose: ${err.message}`); }
    p.open = false;
    p.note = "closed locally";
    renderRawTabs();
    renderRawOutput();
    refreshSnapshot();
  });
  document.getElementById("raw-view-text")?.addEventListener("click", () => {
    rawViewMode = "text";
    document.getElementById("raw-view-text").classList.add("is-active");
    document.getElementById("raw-view-hex").classList.remove("is-active");
    renderRawOutput();
  });
  document.getElementById("raw-view-hex")?.addEventListener("click", () => {
    rawViewMode = "hex";
    document.getElementById("raw-view-hex").classList.add("is-active");
    document.getElementById("raw-view-text").classList.remove("is-active");
    renderRawOutput();
  });

  // renderForwardList draws one row per registered port forward (every
  // forward visible to this operator on the server, not just ones this
  // WebUI started), each with a kill button. Mirrors renderConnList's DOM
  // building (no innerHTML with server strings), but — like renderTaskList's
  // Cancel button — needs appendCmdOutput/refreshSnapshot, so unlike
  // renderConnList it is declared inside this closure rather than as a
  // top-level function. A browser still cannot bind a local listener, so there
  // is no -L here; the raw-connect pane below opens forwards whose client
  // endpoint is this page instead, and they appear in this list as
  // `(in-process)`.
  function renderForwardList(forwards) {
    lastForwards = forwards || []; // mirrors renderTaskList's lastTasks cache
    const host = document.getElementById("forward-list");
    if (!host) return;
    host.textContent = "";
    if (!forwards.length) {
      const empty = document.createElement("div");
      empty.className = "forward-list-empty";
      empty.textContent = "アクティブなポートフォワードはありません";
      host.appendChild(empty);
      return;
    }
    for (const f of forwards) {
      const row = document.createElement("div");
      row.className = "forward-row";
      const taskShort = f.task ? f.task.slice(0, 8) + "…" : "-";
      for (const text of [`#${f.forward_id}`, f.dir, taskShort, f.spec, f.origin]) {
        const cell = document.createElement("span");
        cell.className = "forward-cell";
        cell.textContent = text;
        row.appendChild(cell);
      }
      const kill = document.createElement("button");
      kill.type = "button";
      kill.className = "btn-danger";
      kill.textContent = "kill";
      kill.addEventListener("click", async () => {
        if (!window.confirm(`Kill forward #${f.forward_id} (${f.spec})?`)) return;
        kill.disabled = true;
        try {
          await window.harness.forwardKill(f.forward_id);
          appendCmdOutput(`killed forward #${f.forward_id}`);
          refreshSnapshot();
        } catch (err) {
          appendCmdOutput(`forward kill error: ${err.message}`);
          kill.disabled = false;
        }
      });
      row.appendChild(kill);
      host.appendChild(row);
    }
  }

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
      // Only a live session has a worktree the runner can reach: the server
      // answers NoSuchTask for anything else (server/file_transfer.go).
      if (!taskSessionAlive(t)) continue;
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
    fileMkdirBtn.disabled = !hasTask;
    fileNewBtn.disabled = !hasTask;
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
    fileEditBtn.disabled = !hasTask || !hasSel || filePickerSelected.isDir;
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
      // Double-click is a file-only shortcut into the editor. Directory rows
      // descend on the first click and are re-rendered, so they never see a
      // second one.
      if (!e.isDir) {
        li.addEventListener("dblclick", async () => {
          const taskID = fileTaskSelect.value;
          if (!taskID) return;
          const rel = joinFsPath(filePickerCurDir, e.name);
          try {
            fileResultPre.textContent = await editRemoteFile(taskID, rel);
            refreshFilePicker();
          } catch (err) {
            fileResultPre.textContent = `edit error: ${err.message}`;
          }
        });
      }
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

  fileMkdirBtn.addEventListener("click", async () => {
    const taskID = fileTaskSelect.value;
    if (!taskID) return;
    const name = window.prompt("新しいディレクトリ名 (ネスト可, 例: a/b/c):");
    if (!name) return;
    const rel = joinFsPath(filePickerCurDir, name);
    try {
      // parents=true: 入力名がネストしていても作成でき、結果は直後の
      // 一覧更新で見える(TUI picker の + キーと同じ semantics)。
      await window.harness.fileMkdir(taskID, rel, true);
      fileResultPre.textContent = `mkdir ok: ${rel}`;
      refreshFilePicker();
    } catch (e) {
      fileResultPre.textContent = `mkdir error: ${e.message}`;
    }
  });

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
      const res = await pushBytesWithPrompts(taskID, remoteRel, buf, file.name, fp.onProgress);
      fileResultPre.textContent = res.msg;
      if (res.ok) refreshFilePicker();
    } catch (e) {
      fileResultPre.textContent = `push error: ${e.message}`;
    } finally {
      fp.end();
    }
  });

  fileNewBtn.addEventListener("click", async () => {
    const taskID = fileTaskSelect.value;
    if (!taskID) return;
    const edited = await openFileEditor({ name: "" });
    if (!edited) {
      fileResultPre.textContent = "new file cancelled";
      return;
    }
    const buf = new TextEncoder().encode(edited.text);
    const remoteRel = joinFsPath(filePickerCurDir, edited.name);
    const fp = beginFileProgress(edited.name);
    try {
      const res = await pushBytesWithPrompts(taskID, remoteRel, buf, edited.name, fp.onProgress);
      fileResultPre.textContent = res.msg;
      if (res.ok) refreshFilePicker();
    } catch (e) {
      fileResultPre.textContent = `push error: ${e.message}`;
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

  // loadPreview pulls the file and renders it. Split out of the Preview button
  // so Reload and the opt-in refetch drive exactly the same path — a second
  // copy would drift, and the caps and the two-step sniff are the fiddly part.
  //
  // The length asked for is the CAP, never the size from the listing: the
  // runner clamps a range past EOF, so this stays correct when the file has
  // changed since the picker last listed it — which is the whole point of a
  // reload.
  async function loadPreview(taskID, rel, name, preferredMode) {
    const cap = previewMaxBytesFor(name);
    // Same progress row the Pull button uses. At these caps a pull is seconds
    // long over wasm, and without it the modal opens on a page that has simply
    // stopped.
    const fp = beginFileProgress(name);
    try {
      let bytes, total;
      if (isHtmlExt(name) || isImageExt(name)) {
        // The extension settles which renderer runs, so one request is enough.
        ({ bytes, total } = await pullPreviewSlice(taskID, rel, 0, cap, fp));
      } else {
        // Unknown type: fetch only what a hex dump would show, decide from
        // those bytes, and continue only if it turns out to be text. This is
        // what stops a multi-megabyte binary being pulled in full to render
        // 4 KiB of hex — isLikelyBinary needs the bytes, so the decision
        // cannot be made before the first read.
        const head = await pullPreviewSlice(taskID, rel, 0, Math.min(cap, HEX_PREVIEW_MAX_BYTES), fp);
        total = head.total;
        if (isLikelyBinary(head.bytes) || head.total <= HEX_PREVIEW_MAX_BYTES) {
          bytes = head.bytes;
        } else {
          const rest = await pullPreviewSlice(taskID, rel, HEX_PREVIEW_MAX_BYTES,
            Math.min(head.total, cap) - HEX_PREVIEW_MAX_BYTES, fp);
          bytes = new Uint8Array(head.bytes.length + rest.bytes.length);
          bytes.set(head.bytes, 0);
          bytes.set(rest.bytes, head.bytes.length);
        }
      }
      // total, not the listing's size: after a reload the title should say how
      // big the file is NOW.
      previewSource = { taskID, rel, name };
      renderFilePreview(rel, total, name, bytes);
      // renderFilePreview always opens HTML rendered; a reload triggered from
      // the source view must not silently flip the view under the operator.
      if (preferredMode === "source" && filePreviewHtml) {
        filePreviewHtml.mode = "source";
        showHtmlPreview();
      }
      if (bytes.byteLength < total) {
        // Say what is MISSING, not two totals. formatBytes rounds to one
        // decimal, so a file 200 bytes past the cap rendered as "showing the
        // first 4.0 MB of 4.0 MB" — the same number twice, next to a demand to
        // download the rest. The remainder cannot collide: the note only exists
        // when it is at least one byte.
        appendPreviewNote(`showing the first ${formatBytes(bytes.byteLength)} — ${formatBytes(total - bytes.byteLength)} not shown. Use Pull to download the whole file.`);
      }
      showPreviewReload();
    } catch (e) {
      openFilePreview(rel, 0, null, `preview error: ${e.message}`);
    } finally {
      fp.end();
    }
  }

  filePreviewBtn.addEventListener("click", async () => {
    const taskID = fileTaskSelect.value;
    if (!taskID || !filePickerSelected || filePickerSelected.isDir) return;
    const sel = filePickerSelected;
    await loadPreview(taskID, joinFsPath(filePickerCurDir, sel.name), sel.name);
  });

  // Reload re-pulls the open preview. The file is usually being written by an
  // agent while it is on screen, so the bytes the modal opened with go stale
  // within seconds.
  filePreviewReload.addEventListener("click", () => {
    if (!previewSource) return;
    const { taskID, rel, name } = previewSource;
    loadPreview(taskID, rel, name, filePreviewHtml && filePreviewHtml.mode);
  });

  fileEditBtn.addEventListener("click", async () => {
    const taskID = fileTaskSelect.value;
    if (!taskID || !filePickerSelected || filePickerSelected.isDir) return;
    const rel = joinFsPath(filePickerCurDir, filePickerSelected.name);
    try {
      fileResultPre.textContent = await editRemoteFile(taskID, rel);
      refreshFilePicker();
    } catch (e) {
      fileResultPre.textContent = `edit error: ${e.message}`;
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
    previewPin = null;
    previewApiGen++; // in-flight replies now belong to a preview that is gone
    previewApiInFlight = 0;
    // Drop the registration with the preview: the row must not outlive the
    // reach it stands for.
    releasePreviewPin();
    filePreviewApi.hidden = true;
    filePreviewApi.value = "";
    filePreviewReload.hidden = true;
    filePreviewRefetchLabel.hidden = true;
    filePreviewRefetch.checked = false;
    previewSource = null;
    filePreviewModalsLabel.hidden = true;
    filePreviewModals.checked = false;
    filePreviewCopyPayload = null;
    filePreviewCopy.hidden = true;
    filePreviewEditRel = null;
    filePreviewEdit.hidden = true;
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
    // Same for Edit: only the text render paths turn it back on, so images,
    // hex dumps and the oversize note never offer it.
    filePreviewEditRel = null;
    filePreviewEdit.hidden = true;
    // Reload and refetch likewise: loadPreview re-enables them on success, so
    // the error path does not offer to reload something that never loaded.
    filePreviewReload.hidden = true;
    filePreviewRefetchLabel.hidden = true;
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
      // SECURITY: allow-same-origin is NEVER granted, with or without the
      // toggle — an opaque origin is what keeps the page away from the WebUI
      // origin / trsf / DOM / storage, and it is the whole basis of rendering
      // attacker-influenced HTML at all.
      //
      // The rest are graded by whether they INTERRUPT THE OPERATOR, not by
      // whether the page can reach the network: srcdoc previews carry no CSP,
      // so a page can already send requests out through ordinary subresource
      // loads and fetch (CORS hides the responses, not the requests). Forms
      // and pointer-lock therefore add no reach and ride along by default,
      // while modals, popups and downloads each seize the operator's tab —
      // a page looping on alert() makes the tab unusable until it is closed,
      // which on a phone means killing it. Those wait for an explicit opt-in,
      // default off, like the API pin.
      iframe.setAttribute("sandbox", filePreviewModals.checked
        ? PREVIEW_SANDBOX_BASE + " " + PREVIEW_SANDBOX_INTERRUPTING
        : PREVIEW_SANDBOX_BASE);
      // An empty pin injects nothing: no shim, no marker, no reach. That is
      // byte-for-byte the behaviour that shipped before the pin existed, and it
      // is the default every time the modal opens.
      previewPin = parsePinnedTarget(filePreviewApi.value);
      previewApiGen++;
      previewApiInFlight = 0;
      // Register the REACH, not each fetch. Fetch-scoped registrations lived
      // milliseconds against a 5s poll, so nothing a previewed page did was
      // ever visible in `forward ls`; this row stays up while the preview does.
      // Fire-and-forget: the iframe is built either way, and a fetch without a
      // live pin is refused wasm-side.
      releasePreviewPin();
      if (previewPin) {
        window.harness.previewPinOpen(PREVIEW_PIN_KEY, fileTaskSelect.value, previewPin.host, previewPin.port)
          .then((id) => { previewPinForwardID = id; })
          .catch((e) => {
            previewPin = null;
            appendPreviewNote(`preview: could not register the forward (${e.message}) — the page has no network`);
          });
      }
      iframe.srcdoc = previewPin
        ? injectPreviewShim(text, previewShimSource(previewPin, rel))
        : text;
      node = iframe;
    } else {
      const pre = document.createElement("pre");
      pre.textContent = text;
      node = pre;
    }
    openFilePreview(rel, size, node, null);
    filePreviewToggle.hidden = false;
    filePreviewToggle.textContent = mode === "render" ? "View source" : "View rendered";
    // Source view has no realm to constrain, so the input only makes sense
    // while something is actually rendering.
    filePreviewApi.hidden = mode !== "render";
    filePreviewModalsLabel.hidden = mode !== "render";
    // Copy always yields the raw HTML source, regardless of render/source view.
    showPreviewCopy({ text });
    // Re-enable Reload here too: this path rebuilds from cached bytes without
    // going through loadPreview, and openFilePreview has just hidden them.
    showPreviewReload();
    // HTML is text: offer Edit in both the rendered and the source view.
    showPreviewEdit(rel);
  }

  filePreviewEdit.addEventListener("click", async () => {
    const taskID = fileTaskSelect.value;
    const rel = filePreviewEditRel;
    if (!taskID || !rel) return;
    // Close the preview first — the editor is a modal too. editRemoteFile
    // re-loads rather than reusing the bytes on screen: a preview opened
    // minutes ago is a stale baseline and would conflict on the first save.
    filePreviewModal.close();
    try {
      fileResultPre.textContent = await editRemoteFile(taskID, rel);
      refreshFilePicker();
    } catch (e) {
      fileResultPre.textContent = `edit error: ${e.message}`;
    }
  });

  // rebuildPreview is every path that re-renders the open preview. With refetch
  // on it re-pulls first, which is the point: the bytes the modal opened with
  // go stale the moment an agent writes the file again.
  function rebuildPreview(mode) {
    if (filePreviewRefetch.checked && previewSource) {
      const { taskID, rel, name } = previewSource;
      loadPreview(taskID, rel, name, mode);
      return;
    }
    showHtmlPreview();
  }

  filePreviewToggle.addEventListener("click", () => {
    if (!filePreviewHtml) return;
    filePreviewHtml.mode = filePreviewHtml.mode === "render" ? "source" : "render";
    rebuildPreview(filePreviewHtml.mode);
  });

  // Changing the target rebuilds the iframe from the bytes already in hand —
  // the file is not re-pulled. A fresh realm is required anyway: the shim has
  // to exist before the page's scripts run, so it cannot be added to a page
  // that is already running.
  filePreviewApi.addEventListener("change", () => {
    if (filePreviewHtml && filePreviewHtml.mode === "render") rebuildPreview("render");
  });

  // releasePreviewPin drops the registration if there is one. Safe to call when
  // there is none, which is why every path calls it unconditionally.
  function releasePreviewPin() {
    previewPinForwardID = null;
    if (window.harness && window.harness.previewPinClose) {
      window.harness.previewPinClose(PREVIEW_PIN_KEY).catch(() => {});
    }
  }

  function appendPreviewNote(text) {
    const p = document.createElement("p");
    p.className = "preview-note";
    p.textContent = text;
    filePreviewBody.appendChild(p);
  }

  // Fired by wasm when the registration ends without us asking — an operator ran
  // `forward kill` from the CLI, the TUI or the Connections tab, or the server
  // connection went. The reach is gone, so the pin goes with it; saying so beats
  // the page's requests silently starting to fail.
  window.harness_previewPinClosed = (key, reason) => {
    if (key !== PREVIEW_PIN_KEY) return;
    previewPin = null;
    previewPinForwardID = null;
    appendPreviewNote(`preview forward revoked (${reason}) — the page can no longer reach its API target`);
  };

  // sandbox is read when the iframe is created, so granting dialogs means
  // building a new frame — the page restarts, exactly as it does for the pin.
  filePreviewModals.addEventListener("change", () => {
    if (filePreviewHtml && filePreviewHtml.mode === "render") rebuildPreview("render");
  });

  // The preview shim relays the page's fetch()/XHR here. Nothing in this
  // message is trusted: the iframe is an opaque origin the page fully controls,
  // so it can delete the shim and post whatever it likes. Two checks stand
  // between it and the task:
  //
  //   1. window identity. Every sandboxed frame reports origin "null", so
  //      ev.origin distinguishes nothing; ev.source does.
  //   2. the pin the OPERATOR typed, re-checked here. The shim's own check
  //      exists to give the page a familiar failure, not to constrain it.
  window.addEventListener("message", async (ev) => {
    const m = ev.data;
    if (!m || m.__harnessFetch !== "request") return;
    const iframe = filePreviewBody.querySelector("iframe.preview-iframe");
    if (!iframe || ev.source !== iframe.contentWindow) return;

    const gen = previewApiGen;
    const pin = previewPin;
    const reply = (payload) => {
      if (gen !== previewApiGen) return; // preview closed or re-rendered
      iframe.contentWindow.postMessage({ __harnessFetch: "reply", id: m.id, ...payload }, "*");
    };
    if (!pin || !previewFetchTargetAllowed(m.url, pin)) {
      reply({ error: "Failed to fetch" });
      return;
    }
    // A runaway page (setInterval + fetch) would otherwise open forwards
    // without bound. Refusing is visible to the page as a failed request;
    // queueing would just hide the loop.
    if (previewApiInFlight >= PREVIEW_FETCH_MAX_INFLIGHT) {
      reply({ error: "harness preview: too many concurrent requests" });
      return;
    }
    previewApiInFlight++;
    try {
      const res = await window.harness.previewPinFetch(PREVIEW_PIN_KEY, {
        method: m.method, path: m.path, headers: m.headers, body: m.body,
      });
      reply({
        status: res.status, statusText: res.statusText,
        headers: res.headers, body: res.body,
      });
    } catch (e) {
      reply({ error: e.message || "Failed to fetch" });
    } finally {
      previewApiInFlight--;
    }
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
      // No truncation note here any more. bytes is what the preview PULLED, and
      // for an unknown-extension file the sniff step pulls at most
      // HEX_PREVIEW_MAX_BYTES, so this could only ever have compared 4096
      // against 4096. How much of the FILE is missing is the caller's note,
      // which is the only place that knows the total.
      openFilePreview(rel, size, pre, "binary");
      // Copy the displayed hex dump (which may be truncated), matching the view.
      showPreviewCopy({ text: hex });
      return;
    }
    const pre = document.createElement("pre");
    const text = new TextDecoder("utf-8", { fatal: false }).decode(bytes);
    pre.textContent = text;
    openFilePreview(rel, size, pre, null);
    showPreviewCopy({ text });
    showPreviewEdit(rel);
  }

  // showPreviewReload enables Reload and the refetch toggle. Same contract as
  // showPreviewCopy / showPreviewEdit above: openFilePreview clears them on
  // every render, so each path that produced a real preview turns them back on
  // and the error path stays without them.
  function showPreviewReload() {
    if (!previewSource) return;
    filePreviewReload.hidden = false;
    filePreviewRefetchLabel.hidden = false;
  }

  // showPreviewEdit enables the Edit button for the open preview. Called only
  // from the text render paths — openFilePreview clears it up front, so the
  // image / hex / error paths stay hidden.
  function showPreviewEdit(rel) {
    filePreviewEditRel = rel;
    filePreviewEdit.hidden = false;
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
      // Switching the tab is the whole move at every width now. It used to be
      // mobile-only, with desktop scrolling to a section that was always on
      // screen somewhere; the tab is that section today.
      document.querySelector('.tab-btn[data-tab="notify"]')?.click();
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
          // isPTYKind: the buttons below are a preview (an xterm) and a
          // reattach (a PTY splice), neither of which an event stream has.
          const live = isPTYKind(t) && (t.status === "Running" || t.status === "Detached");
          // The event-stream kind's equivalent of Reattach: no terminal to take
          // over, but it is driven from the chat — turns and approvals.
          const liveStream = isStreamKind(t) && (t.status === "Running" || t.status === "Detached");
          const terminal = t && TERMINAL_STATES.has(t.status);
          if (live) {
            mkBtn("🔍 プレビュー", openSessionPreview);
            mkBtn("↪ Reattach", (id) => reattachTo(id));
          }
          if (liveStream) mkBtn("💬 チャット", (id) => openChatFor(id));
          if (terminal) mkBtn("▶ Resume", resumeTaskById);
          if (!t) { // not in the snapshot (pruned/unknown) — offer both as a fallback
            mkBtn("🔍 プレビュー", openSessionPreview);
            mkBtn("↪ Reattach", (id) => reattachTo(id));
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
    // for background appends (task events, takeover notices) yanks the page out
    // from under whatever the operator was reading in the tasks tab.
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
          out = await window.harness.submit(sessionReq({ repo, task, host, agent, claudeArgs: claudeArgsList, resumeTaskId, resumeConversation }));
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
        case "set-parent": {
          // set-parent <task-id> (--parent <id> | --none | --swap)
          // Mirrors harness-cli caps set-parent; ids are full 32-hex.
          let parent = null, none = false, swap = false, target = null;
          for (let i = 1; i < tokens.length; i++) {
            const t = tokens[i];
            if (t === "--none") none = true;
            else if (t === "--swap") swap = true;
            else if (t === "--parent") { i++; parent = tokens[i]; }
            else if (t.startsWith("--parent=")) parent = t.slice("--parent=".length);
            else if (t.startsWith("-")) throw new Error(`set-parent: unknown flag ${t}`);
            else if (!target) target = t;
            else throw new Error(`set-parent: unexpected arg ${t}`);
          }
          if (!target) throw new Error("set-parent: missing task id");
          const picked = [parent !== null, none, swap].filter(Boolean).length;
          if (picked !== 1) throw new Error("set-parent: pass exactly one of --parent <id>, --none, --swap");
          const req = { taskId: target };
          if (swap) req.swap = true;
          else if (parent !== null) req.parentId = parent;
          const r = await window.harness.setParent(req);
          out = setParentMessage(target, swap, r);
          await refreshSnapshot();
          break;
        }
        case "preview":
          if (!tokens[1]) throw new Error("preview: missing task id");
          openSessionPreview(tokens[1]);
          out = `preview ${tokens[1].slice(0, 12)}…`;
          break;
        case "grid": {
          // grid [--under <id> [--descendants]] [id...] — flags before
          // positionals, as in the file verbs, and the same spellings the TUI's
          // grid verb takes. --under is a task's WORKING set: its subtree plus
          // whatever its own scope names individually; --descendants drops the
          // task itself, for when you are watching that one elsewhere. Bare ids
          // stay an EXPLICIT list and are never expanded into subtrees, matching
          // `--scope ids:` which also names tasks one at a time.
          let under = null;
          let descendants = false;
          const gridIds = [];
          for (let i = 1; i < tokens.length; i++) {
            const tok = tokens[i];
            if (tok === "--descendants") {
              descendants = true;
            } else if (tok === "--under") {
              i++;
              if (i >= tokens.length) throw new Error("grid: --under: missing task id");
              under = tokens[i];
            } else if (tok.startsWith("--under=")) {
              under = tok.slice("--under=".length);
            } else if (tok.startsWith("-")) {
              throw new Error(`grid: unknown flag ${tok}`);
            } else {
              gridIds.push(tok);
            }
          }
          // Full 32-hex only, no prefix resolution: same rule as `prune`, so a
          // mistype misses instead of resolving onto a neighbouring task.
          const full = (id) => /^[0-9a-fA-F]{32}$/.test(id);
          if (under !== null) {
            if (gridIds.length) throw new Error("grid: --under names one subtree — drop the extra ids");
            if (!full(under)) throw new Error("grid: --under needs a full 32-hex task id");
            out = await openGridSet({ mode: descendants ? "descendants" : "subtree", anchor: under });
            break;
          }
          if (descendants) {
            throw new Error("grid: --descendants needs --under <task-id> to take the descendants OF");
          }
          if (gridIds.length) {
            const bad = gridIds.filter((id) => !full(id));
            if (bad.length) throw new Error(`grid: not a full 32-hex task id: ${bad.join(", ")}`);
            out = await openGridSet({ mode: "ids", ids: gridIds });
            break;
          }
          // No ids at all: the currently-included live sessions (respects the
          // per-session グリッドに含める toggles), same as the show button.
          const paneIds = (await liveInteractiveIds()).filter((id) => !gridExcluded.has(id));
          if (paneIds.length === 0) { out = "grid: no included live interactive sessions"; break; }
          openSessionGrid(paneIds, "all");
          out = `grid: ${paneIds.length} pane(s)${paneIds.length > 9 ? " (capped at 9)" : ""}`;
          break;
        }
        case "prune": {
          // Two modes, mirroring harness-cli: positional task-ids switch to
          // id mode (--before ignored; active tasks skipped unless --force);
          // no ids = time mode. Ids are full 32-hex, no prefix resolution —
          // a mistype misses (safe no-op) rather than hitting another task.
          let before = null, force = false;
          const taskIds = [];
          const rest = tokens.slice(1);
          for (let i = 0; i < rest.length; i++) {
            const t = rest[i];
            if (t === "--force" || t === "-f") {
              force = true;
            } else if (t === "--before") {
              i++;
              if (i >= rest.length) throw new Error("prune: --before: missing DUR");
              before = rest[i];
            } else if (t.startsWith("--before=")) {
              before = t.slice("--before=".length);
            } else if (t.startsWith("-")) {
              throw new Error(`prune: unknown flag ${t}`);
            } else {
              taskIds.push(t);
            }
          }
          if (taskIds.length > 0) {
            out = await window.harness.prune({ taskIds, force });
          } else {
            out = await window.harness.prune({ before: before || "168h" });
          }
          break;
        }
        case "file": {
          out = await runFileCmd(tokens.slice(1));
          break;
        }
        case "git": {
          out = await runGitCmd(tokens.slice(1));
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
        case "forward": {
          // forward ls renders from the snapshot the page already polls — no
          // extra RPC and no second wasm export (Task 7). forward kill goes
          // through the wasm bridge; starting a socket-bound forward is
          // CLI/TUI-only (a browser cannot bind a local listener) — a
          // browser-endpoint forward is opened from the raw-connect pane
          // instead, not through this command.
          const sub = tokens[1];
          if (sub === "ls") {
            const fs = lastForwards || [];
            out = fs.length
              ? fs.map((f) => `#${f.forward_id}  ${f.dir}  ${f.task.slice(0, 8)}…  ${f.spec}  ${f.origin}`).join("\n")
              : "(no active port forwards)";
          } else if (sub === "kill") {
            const id = parseInt(tokens[2], 10);
            if (!Number.isFinite(id)) throw new Error("forward kill: usage: forward kill <forward-id>");
            await window.harness.forwardKill(id);
            out = `killed forward ${id}`;
          } else {
            throw new Error("forward: usage: forward ls | forward kill <forward-id>");
          }
          break;
        }
        // The `session` family reaches this surface for the first time here,
        // and only its `stream` namespace: the lifecycle verbs (new/ls/kill)
        // have buttons, while these four had no route at all. Recorded as a
        // partial family rather than pretending the rest exists.
        case "session": {
          if (args[0] !== "stream") {
            appendCmdOutput("session: only the `stream` namespace is available here — new/ls/kill are the buttons above");
            break;
          }
          const verb = args[1] || "";
          const id = args[2] || chatTaskId;
          if (!id) { appendCmdOutput("session stream: a task id is required (or open a chat first)"); break; }
          try {
            switch (verb) {
              case "turn": {
                const text = args.slice(3).join(" ");
                if (!text) { appendCmdOutput("session stream turn: <id> <text...>"); break; }
                await window.harness.streamTurn(id, text);
                appendCmdOutput(`stream turn ${id.slice(0, 8)}: sent`);
                break;
              }
              case "approve": {
                // The verdict is a bare word, as in the TUI's command line and
                // for the same reason: this input is whitespace-split with no
                // flag parser, so a word cannot be silently dropped the way a
                // misplaced --allow can — and this is the verb where that would
                // be worst, since it decides the answer.
                const reqID = args[3] || "";
                const verdict = args[4] || "";
                if (!reqID || (verdict !== "allow" && verdict !== "deny")) {
                  appendCmdOutput("session stream approve: <id> <request-id> allow|deny [reason...]");
                  break;
                }
                await window.harness.streamApprove(id, reqID, verdict, args.slice(5).join(" "), -1);
                appendCmdOutput(`stream approve ${id.slice(0, 8)} ${reqID}: ${verdict}`);
                break;
              }
              case "interrupt":
                await window.harness.streamInterrupt(id);
                appendCmdOutput(`stream interrupt ${id.slice(0, 8)}: sent`);
                break;
              case "finish":
                await window.harness.streamFinish(id);
                appendCmdOutput(`stream finish ${id.slice(0, 8)}: sent`);
                break;
              case "attach":
                await openChatFor(id);
                appendCmdOutput(`stream attach ${id.slice(0, 8)}: chat opened`);
                break;
              case "requests":
              case "snapshot":
                appendCmdOutput(`session stream ${verb}: specified (design §3) but not built yet`);
                break;
              default:
                appendCmdOutput(`unknown session stream verb ${JSON.stringify(verb)}`);
            }
          } catch (err) {
            appendCmdOutput(`session stream ${verb}: ${err.message}`);
          }
          break;
        }

        case "help":
          out = [
            "commands:",
            "  submit [--resume-conversation] [--agent <name>] <prompt...>",
            "                            submit task (use repo dropdown / Resume task id; --agent overrides the Agent dropdown)",
            "  list                      refresh the snapshot and echo task rows",
            "  session stream turn [<id>] <text...>       send a user turn (id defaults to the open chat)",
            "  session stream approve [<id>] <req-id> allow|deny [reason...]",
            "  session stream interrupt|finish|attach [<id>]",
            "  refresh (alias: sync)     force a snapshot re-sync",
            "  await-idle <task-id> [--notify | --topic T] [--threshold-ms N]",
            "                            fire when the session's output goes idle (default: prints here on fire; --notify: notification feed + hook)",
            "  cancel <task-id>          cancel a task",
            "  set-parent <task-id> (--parent <id> | --none | --swap)",
            "                            re-point the task's parent link (--none: to root; --swap: invert with its current parent); operator-only",
            "  preview <task-id>         live screen preview of a session — click it to type (⏸/▶ pause-resume)",
            "  grid [id...]              live monitor grid of sessions (default: all live interactive, cap 9)",
            "  grid --under <task-id> [--descendants]",
            "                            that task's working set: its subtree PLUS the tasks its own scope names (ids:);",
            "                            --descendants leaves the task itself out (watching that one elsewhere)",
            "  prune [--before=DUR]      forget terminal tasks older than DUR",
            "  prune [--force] <task-id>...",
            "                            forget specific tasks by id (--force: also active tasks)",
            "  git <task> log [--max N] [-- <path>]",
            "                            the task's commits (also: the Git tab)",
            "  git <task> diff [--staged] [<base>] [<target>] [-- <path>]",
            "                            revisions counted as git counts them: none=unstaged, one=<base> vs working tree, two=commit vs commit",
            "  git <task> show [<rev>] [-- <path>]",
            "                            one commit and its diff",
            "  git <task> status [-- <path>]",
            "                            uncommitted and untracked paths (untracked appear in no diff)",
            "  git <task> subrepos       list git repos nested inside the worktree",
            "  git <task> file [--staged | --rev REV] <path>",
            "                            one file's whole content (also: click a file header in a diff)",
            "                            --subrepo DIR runs any of the above inside one; --submodule inlines submodule content",
            "  file ls <task> [rel]      list a worktree directory",
            "  file delete [-r] [-f] <task> <rel>",
            "                            remove a file (no -r) or directory (-r [-f])",
            "  file push <task> <rel>    upload a local file (file picker opens)",
            "  file new <task> <rel>     write a new text file in a browser editor and upload it",
            "  file edit <task> <rel>    pull a text file into the browser editor and push it back",
            "  file mkdir [-p] <task> <rel>",
            "                            create a worktree directory (-p: parents, idempotent)",
            "  file pull [-r] <task> <rel>",
            "                            download a remote file, or -r for a directory as a .tar",
            "  server dial-runner <cid> [--via <cid>]",
            "                            ask the server to reverse-dial a Listen-mode runner; --via routes through a registered relay-runner",
            "  forward ls                list registered port forwards (from the last snapshot poll)",
            "  forward kill <forward-id> close a registered port forward (starting a socket-bound forward is CLI/TUI-only; open a browser-endpoint forward from the raw-connect pane instead)",
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

  // --- Tab switching (every width). The CSS hides every section outside the
  //     active group, so this is what makes any section reachable. ---
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
    document.body.dataset.activeTab = name;
    for (const b of tabbar.querySelectorAll(".tab-btn")) {
      b.classList.toggle("is-active", b.dataset.tab === name);
    }
    // Reset scroll so the newly-shown tab starts from the top.
    window.scrollTo(0, 0);
    // Size (or release) the terminal tab to the visible viewport; this also
    // re-fits the grid that went stale while the tab was display:none.
    fitTerminalToViewport();
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
    // Only tell the PTY when the grid actually changed — same guard, and the
    // same lastCols/lastRows, as fitTerminalToViewport above. Switching away
    // from the terminal tab hides the section, which fires this observer at
    // 0x0; fit() bails on that (proposeDimensions reads a display:none
    // height as "auto" -> NaN), leaving cols/rows at their last good values,
    // so without the check we'd resend the geometry the PTY already has.
    if (term.cols === lastCols && term.rows === lastRows) return;
    lastCols = term.cols; lastRows = term.rows;
    window.harness.resizeInteractive({ cols: term.cols, rows: term.rows });
  });
  ro.observe(document.getElementById("terminal"));

  // --- event-stream chat ---------------------------------------------------
  //
  // The driving surface for TaskKind_Stream, living in the terminal tab beside
  // the xterm: the tab means "the session I am driving", and which renderer it
  // uses follows the kind. Exactly one of #terminal / #chat is shown.
  //
  // It reads the STREAM, not the task log. RenderText drops a request's Input
  // and agentlog truncates a tool's args at 200 bytes — right for a progress
  // feed, useless for deciding whether to allow a Write whose content is the
  // thing at stake. The Input rides the stream verbatim, so this decodes NDJSON
  // off a cowrite attach (harness.streamStart) and renders it here.
  //
  // WRITING never builds a protocol line in JS: harness.streamTurn/Approve/
  // Interrupt/Finish go through cli.EncodeStreamMsg, the same builder the CLI
  // and the TUI use, so the three surfaces cannot drift.
  const chatPanel    = document.getElementById("chat");
  const chatLog      = document.getElementById("chat-log");
  const chatApproval = document.getElementById("chat-approval");
  const chatInput    = document.getElementById("chat-input");
  const chatStatus   = document.getElementById("chat-status");
  const terminalEl   = document.getElementById("terminal");
  const touchKeys    = document.getElementById("touch-keys");

  const CHAT_LINE_LIMIT = 400;   // bounded transcript, like the TUI's
  let chatTaskId = "";           // "" = no chat attached
  let chatPending = null;        // the request awaiting an answer, with its Input
  let chatBusy = false;

  const chatSetStatus = (t) => { if (chatStatus) chatStatus.textContent = t || ""; };

  // showChat swaps the terminal tab between the xterm and the chat. The touch
  // keys go with the xterm: they send terminal control bytes, which mean
  // nothing to a session whose input is JSON.
  const showChat = (on) => {
    if (chatPanel) chatPanel.hidden = !on;
    if (terminalEl) terminalEl.style.display = on ? "none" : "";
    if (touchKeys) touchKeys.style.display = on ? "none" : "";
  };

  const chatAppend = (text, cls) => {
    if (!chatLog) return;
    const atBottom = chatLog.scrollTop + chatLog.clientHeight >= chatLog.scrollHeight - 8;
    const div = document.createElement("div");
    div.className = cls || "c-text";
    div.textContent = text;
    chatLog.appendChild(div);
    while (chatLog.childElementCount > CHAT_LINE_LIMIT) chatLog.removeChild(chatLog.firstChild);
    // Only follow the tail if the reader was already there — scrolling up to
    // read something must not be yanked back by the next event.
    if (atBottom) chatLog.scrollTop = chatLog.scrollHeight;
  };

  // chatRenderEvent is the JS side of streamagent.RenderText. It is a RENDERER,
  // not a second parser: the shapes come from the adapter protocol's own JSON.
  // Kept deliberately close to the Go one so the three surfaces read alike.
  const chatRenderEvent = (ev) => {
    if (!ev) return;
    const trunc = (v, n) => {
      const t = (v === undefined || v === null) ? "" : String(v);
      return t.length > n ? t.slice(0, n) + "…" : t;
    };
    switch (ev.kind) {
      case "session_start": chatAppend("▶ session " + (ev.text || ""), "c-muted"); break;
      case "thinking":      chatAppend("· thinking", "c-muted"); chatSetStatus("thinking…"); break;
      case "tool_start":
        chatAppend("→ " + (ev.tool || "") + ": " + trunc(ev.args, 200), "c-muted");
        chatSetStatus("running " + (ev.tool || "") + "…");
        break;
      case "tool_end":      chatAppend("← " + trunc(ev.result, 200), "c-muted"); break;
      case "text":          chatAppend(ev.text || "", "c-text"); break;
      case "finish":        chatAppend("✓ done", "c-muted"); chatBusy = false; chatSetStatus(""); break;
      case "error":         chatAppend((ev.warning ? "⚠ " : "✗ ") + (ev.text || ""), ev.warning ? "c-warn" : "c-err"); break;
      default:              chatAppend(JSON.stringify(ev), "c-muted"); break;
    }
  };

  // chatShowApproval renders the pending request: the tool, its input WHOLE
  // (this is the payload the log drops), and the choices as a button row —
  // buttons because that is this surface's native control, where the TUI uses
  // keys.
  const chatShowApproval = () => {
    if (!chatApproval) return;
    if (!chatPending) { chatApproval.hidden = true; chatApproval.replaceChildren(); return; }
    const req = chatPending;
    chatApproval.replaceChildren();
    const h = document.createElement("h3");
    h.textContent = "⚑ " + (req.tool || "?") + " を実行してよいか  (" + (req.id || "") + ")";
    chatApproval.appendChild(h);
    if (req.description) {
      const d = document.createElement("div");
      d.className = "notify-meta";
      d.textContent = req.description;
      chatApproval.appendChild(d);
    }
    if (req.input !== undefined) {
      const pre = document.createElement("pre");
      try { pre.textContent = JSON.stringify(req.input, null, 2); }
      catch (_) { pre.textContent = String(req.input); }
      chatApproval.appendChild(pre);
    }
    const row = document.createElement("div");
    row.className = "chat-approval-btns";
    const mk = (label, title, fn) => {
      const b = document.createElement("button");
      b.type = "button";
      b.textContent = label;
      if (title) b.title = title;
      b.addEventListener("click", fn);
      row.appendChild(b);
    };
    mk("✔ 許可", "このツール呼び出しを実行させる", () => chatAnswer("allow", "", -1));
    mk("✘ 拒否", "実行させない。理由はエージェントに verbatim で届く", () => {
      const reason = window.prompt("拒否の理由（エージェントがそのまま読みます。空でも可）") ;
      if (reason === null) return; // cancelled — the request stays pending
      chatAnswer("deny", reason, -1);
    });
    (req.suggestions || []).forEach((sg, i) => {
      const label = [sg.type, sg.mode, sg.destination ? "(" + sg.destination + ")" : ""].filter(Boolean).join(" ");
      mk("＋ " + label, "許可した上で、この提案も受け入れる（以後の挙動が変わる標準の変更）",
         () => chatAnswer("allow", "", i));
    });
    chatApproval.appendChild(row);
    chatApproval.hidden = false;
  };

  const chatAnswer = async (behavior, message, suggestion) => {
    if (!chatPending || !chatTaskId) return;
    const req = chatPending;
    chatPending = null;
    chatShowApproval();
    chatAppend((behavior === "deny" ? "▶ 拒否 " : "▶ 許可 ") + (req.tool || "") +
               (message ? ": " + message : ""), behavior === "deny" ? "c-err" : "c-you");
    chatBusy = true;
    chatSetStatus("resuming…");
    try {
      await window.harness.streamApprove(chatTaskId, req.id, behavior, message, suggestion);
    } catch (err) {
      chatAppend("✗ 応答を送れませんでした: " + err.message, "c-err");
    }
  };

  // The three JS hooks cli/streamchat_wasm.go calls. Globals, matching the
  // harness_preview* pattern the xterm panes already use.
  window.harness_streamOpen = (taskID) => {
    if (taskID !== chatTaskId) return;
    chatSetStatus("attached");
  };
  window.harness_streamLine = (taskID, line) => {
    if (taskID !== chatTaskId) return;
    let msg = null;
    try { msg = JSON.parse(line); } catch (_) {
      // `session send` can lawfully put a non-protocol line on this stream. A
      // follower that hides it cannot explain what the adapter does next.
      chatAppend("(not the protocol) " + line, "c-raw");
      return;
    }
    switch (msg.kind) {
      case "hello":
        chatSetStatus("attached · " + (msg.hello && msg.hello.vendor) + " protocol " + (msg.hello && msg.hello.protocol));
        break;
      case "event":
        chatRenderEvent(msg.event);
        break;
      case "request":
        chatPending = msg.request || null;
        chatBusy = false;
        chatAppend("⚑ 承認待ち: " + ((msg.request && msg.request.tool) || "?"), "c-warn");
        chatShowApproval();
        break;
      case "exit": {
        const ex = msg.exit || {};
        chatAppend("agent exited: code=" + ex.code + (ex.err ? " err=" + ex.err : ""), ex.err ? "c-err" : "c-muted");
        chatBusy = false;
        chatSetStatus("session ended");
        break;
      }
      default: break; // client→adapter kinds do not appear on this direction
    }
  };
  window.harness_streamClosed = (taskID, err) => {
    if (taskID !== chatTaskId) return;
    chatBusy = false;
    chatSetStatus(err ? ("stream ended: " + err) : "stream ended");
  };

  // openChatFor attaches the chat to a stream task and shows it.
  const openChatFor = async (taskID) => {
    if (!taskID) return;
    try { window.harness.streamStop(); } catch (_) { /* nothing attached */ }
    chatTaskId = taskID;
    chatPending = null;
    chatBusy = false;
    if (chatLog) chatLog.replaceChildren();
    chatShowApproval();
    showChat(true);
    setActiveTab("terminal");
    if (attachedTask) attachedTask.textContent = `chat: ${taskID}`;
    // Said explicitly: a chat opened on a freshly RESUMED task shows nothing
    // for a while — the replay comes from the mux ring and a resume starts a
    // new one, while the agent reads its own history first. A blank panel there
    // reads as broken.
    chatSetStatus("attaching… (resume 直後はエージェントが履歴を読み終えるまで無言のことがあります)");
    try {
      await window.harness.streamStart(taskID);
    } catch (err) {
      chatSetStatus("attach failed: " + err.message);
      showError(err);
    }
  };

  const closeChat = () => {
    try { window.harness.streamStop(); } catch (_) { /* already closed */ }
    chatTaskId = "";
    chatPending = null;
    chatShowApproval();
    showChat(false);
  };

  // showTerminalView is THE way the xterm becomes the visible view. Every PTY
  // path goes through it, because the chat has to be torn down when one does
  // and "remember to call closeChat" is not a mechanism: it was wired at two of
  // five call sites and the operator found the other three by resuming into a
  // chat that never went away.
  //
  // The pair it replaces (setActiveTab + term.reset) is exactly the moment "the
  // terminal is now what you are looking at", which is why that pair is the
  // right place for the teardown rather than each caller's tail.
  const showTerminalView = () => {
    closeChat();
    setActiveTab("terminal");
    term.reset();
  };

  const chatSend = async () => {
    if (!chatTaskId || !chatInput) return;
    const text = chatInput.value.trim();
    if (!text) return;
    chatInput.value = "";
    chatAppend("you ▶ " + text, "c-you");
    chatBusy = true;
    chatSetStatus("");
    try {
      await window.harness.streamTurn(chatTaskId, text);
    } catch (err) {
      chatAppend("✗ 送信できませんでした: " + err.message, "c-err");
    }
  };

  document.getElementById("chat-send")?.addEventListener("click", chatSend);
  // Enter sends; Shift+Enter is a newline. A textarea is the right control for
  // free text — and it is the thing the TUI's one-line input cannot do.
  chatInput?.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); chatSend(); }
  });
  document.getElementById("chat-interrupt")?.addEventListener("click", async () => {
    if (!chatTaskId) return;
    chatAppend("▶ 中断", "c-you");
    try { await window.harness.streamInterrupt(chatTaskId); }
    catch (err) { chatAppend("✗ " + err.message, "c-err"); }
  });
  document.getElementById("chat-finish")?.addEventListener("click", async () => {
    if (!chatTaskId) return;
    chatAppend("▶ finish（このターンを完走して終了します）", "c-you");
    try { await window.harness.streamFinish(chatTaskId); }
    catch (err) { chatAppend("✗ " + err.message, "c-err"); }
  });

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
  const spawnStreamBox = document.getElementById("spawn-stream");
  const resumeConvBox  = document.getElementById("resume-conversation");
  const composeRequest = () => sessionReq({
    repo: runnerSelect.value || "",
    host: hostSelect ? (hostSelect.value || "") : "",
    agent: agentSelect ? (agentSelect.value || "") : "",
    claudeArgs: currentClaudeArgs(),
    resumeTaskId: currentResumeTaskID(),
    // Compose was the one surface with no way to say this. The task sheet
    // offers all four combinations as buttons and the command input takes
    // --resume-conversation, so a Compose resume silently meant "keep the
    // worktree, drop the conversation" with nothing on screen saying so.
    resumeConversation: !!(resumeConvBox && resumeConvBox.checked),
    // A checkbox, not a typed flag: this decomposes into a control the form
    // already has a kind for, which is what checklist 34a asks.
    eventStream: !!(spawnStreamBox && spawnStreamBox.checked),
  });

  // --- Cap chips (session-default capability set for new-session spawns) ---
  // capDefs / spawnCaps declared early in the IIFE (before connect) so the
  // wasm-independent state survives reconnects. initCaps() is called once after
  // connect (capList is synchronous; needs no active connection).

  function capsAllBits() {
    return capDefs.reduce((m, c) => m | c.bit, 0);
  }

  function capsLabel() { return capsLabelFor(spawnCaps); }
  function capsLabelFor(bits) {
    const allBits = capsAllBits();
    if (bits === allBits) return "all";
    if (bits === 0)       return "none";
    return capDefs.filter(c => (bits & c.bit) === c.bit).map(c => c.name).join(",");
  }

  // buildCapChips renders the quick-set buttons + one chip per granular cap
  // + the readout into row, bound to a bitmask through the two accessors.
  // Shared by Compose and the re-grant dialog so chip behaviour cannot drift
  // between them. Re-renders the whole row on every change (small list).
  function buildCapChips(row, getBits, setBits) {
    if (!row) return;
    const rerender = () => {
      row.innerHTML = "";

      const allBtn = document.createElement("button");
      allBtn.type = "button";
      allBtn.className = "cap-quick";
      allBtn.textContent = "[all]";
      allBtn.addEventListener("click", () => { setBits(capsAllBits()); rerender(); });
      row.appendChild(allBtn);

      const noneBtn = document.createElement("button");
      noneBtn.type = "button";
      noneBtn.className = "cap-quick";
      noneBtn.textContent = "[none]";
      noneBtn.addEventListener("click", () => { setBits(0); rerender(); });
      row.appendChild(noneBtn);

      for (const c of capDefs) {
        const btn = document.createElement("button");
        btn.type = "button";
        btn.className = "cap-chip" + ((getBits() & c.bit) === c.bit ? " on" : "");
        btn.dataset.bit = String(c.bit);
        btn.textContent = c.name;
        btn.addEventListener("click", () => { setBits(getBits() ^ c.bit); rerender(); });
        row.appendChild(btn);
      }

      const readout = document.createElement("span");
      readout.className = "caps-readout";
      readout.textContent = "caps: " + capsLabelFor(getBits());
      row.appendChild(readout);
    };
    rerender();
  }

  function renderCaps() {
    buildCapChips(document.getElementById("caps-row"),
      () => spawnCaps, (v) => { spawnCaps = v; });
  }

  function initCaps() {
    if (typeof window.harness.capList !== "function") return;
    capDefs = window.harness.capList();
    spawnCaps = 0;  // default-deny: no bits on until the operator picks some
    renderCaps();
    const capsOnResumeCb = document.getElementById("caps-on-resume");
    if (capsOnResumeCb) {
      capsOnResumeCb.checked = false; // explicit default OFF
      capsOnResumeCb.addEventListener("change", () => {
        applyCapsOnResume = capsOnResumeCb.checked;
      });
    }
    initScope();
  }

  // ---- per-capability overrides -----------------------------------------
  //
  // An override IS a capability mask plus a scope, and this form already has a
  // control for each half: chips for the mask, a base radio and a task
  // checklist for the scope. So it is built from those rather than from a text
  // box — typing `CAPS=SCOPE` belongs in the Command input, which is the
  // surface for typing, not in a form whose every other field is a control.
  //
  // Model: overrideRows = [{caps:number, base:string, ids:Set<string>}].
  // Serialization to the `CAPS=SCOPE` strings the wasm bridge parses happens
  // at send time, so ParseScopeFor and the disjointness check still run in Go
  // and the browser cannot drift from what the CLI accepts.
  //
  // Disjointness is ALSO enforced here, by disabling a chip already claimed by
  // another row: the server rejects an overlap, but a control that lets you
  // build one and then refuses is a worse control than one that cannot.

  function overrideSpecsFrom(which) {
    const out = [];
    for (const r of overrideRowsFor[which]) {
      if (!r.caps) continue; // a row with no chip lit describes nothing
      // base x excludeSelf is 3 x 2, and the grammar spells the six as
      // subtree / descendants / none / none-self / global / global-self.
      // Collapsing them into four radio labels — which the first version of
      // this did — makes none-self and global-self unreachable from here while
      // the CLI and TUI can say both.
      let base = r.base;
      if (r.excludeSelf) base = r.base === "subtree" ? "descendants" : r.base + "-self";
      let scope = base;
      if (r.base !== "global" && r.ids.size > 0) {
        const ids = [...r.ids].sort().join(",");
        scope = (r.base === "none" && !r.excludeSelf) ? "ids:" + ids : base + "+ids:" + ids;
      }
      out.push(capsLabelFor(r.caps) + "=" + scope);
    }
    return out;
  }

  // parseOverrideSpec turns a stored "CAPS=SCOPE" string back into a row, so
  // the re-grant dialog opens showing the task's existing rules as controls
  // rather than as text the operator has to re-type to keep.
  function parseOverrideSpec(spec) {
    const eq = spec.indexOf("=");
    if (eq < 0) return null;
    const capNames = spec.slice(0, eq).split(",").map((x) => x.trim());
    let caps = 0;
    for (const n of capNames) {
      const d = capDefs.find((c) => c.name === n);
      if (d) caps |= d.bit;
    }
    const rest = spec.slice(eq + 1).trim();
    const ids = new Set();
    let base = rest, idPart = "";
    const i = rest.indexOf("ids:");
    if (i >= 0) {
      base = rest.slice(0, i).replace(/\+$/, "").trim() || "none";
      idPart = rest.slice(i + 4);
      for (const id of idPart.split(",")) { const t = id.trim(); if (t) ids.add(t); }
    }
    let excludeSelf = false;
    if (base === "descendants") { base = "subtree"; excludeSelf = true; }
    else if (base.endsWith("-self")) { base = base.slice(0, -5); excludeSelf = true; }
    if (!["subtree", "none", "global"].includes(base)) base = "subtree";
    return { caps, base, excludeSelf, ids };
  }

  // claimedElsewhere is the mask every OTHER row already holds, which is what
  // the chip disabling reads.
  function claimedElsewhere(which, self) {
    let m = 0;
    for (const r of overrideRowsFor[which]) if (r !== self) m |= r.caps;
    return m;
  }

  function buildOverrideRows(which, containerId, onChange) {
    const container = document.getElementById(containerId);
    if (!container) return;
    const rows = overrideRowsFor[which];
    container.innerHTML = "";

    rows.forEach((row, idx) => {
      const box = document.createElement("div");
      box.className = "override-row";

      const head = document.createElement("div");
      head.className = "override-head";
      const title = document.createElement("span");
      title.textContent = "絞り込み " + (idx + 1);
      head.appendChild(title);
      const del = document.createElement("button");
      del.type = "button";
      del.className = "cap-quick";
      del.textContent = "✕ 削除";
      del.addEventListener("click", () => {
        rows.splice(idx, 1);
        buildOverrideRows(which, containerId, onChange);
        onChange();
      });
      head.appendChild(del);
      box.appendChild(head);

      // Capability chips, with the ones another row already claims disabled.
      const chips = document.createElement("div");
      chips.className = "caps-row";
      const taken = claimedElsewhere(which, row);
      for (const c of capDefs) {
        if (c.name === "all" || c.name === "none") continue;
        const btn = document.createElement("button");
        btn.type = "button";
        const on = (row.caps & c.bit) === c.bit;
        const blocked = (taken & c.bit) === c.bit;
        btn.className = "cap-chip" + (on ? " on" : "") + (blocked ? " disabled" : "");
        btn.textContent = c.name;
        btn.disabled = blocked;
        if (blocked) btn.title = "別の絞り込みが既にこの capability を持っています";
        btn.addEventListener("click", () => {
          row.caps ^= c.bit;
          buildOverrideRows(which, containerId, onChange);
          onChange();
        });
        chips.appendChild(btn);
      }
      box.appendChild(chips);

      // Scope base. `descendants` is offered here and NOT on the task's own
      // scope radios, because it is the shape an override is usually for:
      // "may drive its workers, may not drive itself".
      const baseRow = document.createElement("div");
      baseRow.className = "scope-base-row";
      for (const b of ["subtree", "none", "global"]) {
        const label = document.createElement("label");
        const radio = document.createElement("input");
        radio.type = "radio";
        radio.name = which + "-override-base-" + idx;
        radio.value = b;
        radio.checked = row.base === b;
        radio.addEventListener("change", () => {
          if (radio.checked) {
            row.base = b;
            buildOverrideRows(which, containerId, onChange);
            onChange();
          }
        });
        label.appendChild(radio);
        label.appendChild(document.createTextNode(" " + b));
        baseRow.appendChild(label);
      }
      // exclude_self is ORTHOGONAL to the base, so it is a checkbox beside the
      // radios rather than a fourth radio. As a radio it could only ever mean
      // "subtree without self", leaving the other two bases unable to say it.
      const selfLabel = document.createElement("label");
      const selfCb = document.createElement("input");
      selfCb.type = "checkbox";
      selfCb.checked = !!row.excludeSelf;
      selfCb.addEventListener("change", () => {
        row.excludeSelf = selfCb.checked;
        buildOverrideRows(which, containerId, onChange);
        onChange();
      });
      selfLabel.appendChild(selfCb);
      selfLabel.appendChild(document.createTextNode(" 自分自身を除く"));
      selfLabel.title = "subtree なら descendants、none なら空集合（ビットは持つが何にも向かない）";
      baseRow.appendChild(selfLabel);

      box.appendChild(baseRow);

      // Ids, only where they mean something. Under `global` the base already
      // covers every task, so the list is hidden rather than shown-and-ignored.
      if (row.base !== "global") {
        const det = document.createElement("details");
        const sum = document.createElement("summary");
        sum.textContent = "対象タスクを追加 (" + row.ids.size + ")";
        det.appendChild(sum);
        const list = document.createElement("div");
        list.className = "task-checklist";
        det.appendChild(list);
        box.appendChild(det);
        buildTaskChecklist(list, row.ids, "", () => {
          sum.textContent = "対象タスクを追加 (" + row.ids.size + ")";
          onChange();
        });
      }

      container.appendChild(box);
    });

    const add = document.createElement("button");
    add.type = "button";
    add.className = "cap-quick";
    add.textContent = "＋ 絞り込みを追加";
    add.addEventListener("click", () => {
      // A new row starts at `none` with the self box CLEAR.
      //
      // exclude_self is at its wire zero, because that flag was phrased
      // negatively precisely so every default reads as the pre-change meaning;
      // a box that starts checked removes self without the operator asking,
      // and the task-level base radios right above default to the wire value
      // too. An earlier version defaulted this row to `descendants` on the
      // guess that it is the common case — a guess about usage, not a rule.
      //
      // The base starts at `none` rather than `subtree` so the row narrows
      // from the moment it exists: a control called "絞り込み" that defaults to
      // the same set the base already grants would sit there doing nothing,
      // and `none` fails closed while the operator picks.
      rows.push({ caps: 0, base: "none", excludeSelf: false, ids: new Set() });
      buildOverrideRows(which, containerId, onChange);
      onChange();
    });
    container.appendChild(add);
  }

  // scopeSpec serializes a base + exclude-self + id set to the --scope grammar
  // by calling cli.ScopeSpec through the bridge, so the browser holds no copy
  // of the grammar. The JS copy this replaced knew three of the six bases and
  // neither half of the visibility pair, which is why a re-grant on a
  // `descendants` task silently handed self back.
  //
  // visBase is the visibility rank, "" meaning NOT STATED — its own value, and
  // the default: an unstated rank follows the action base. It used to be a
  // `carry` argument holding the task's whole current scope, which ScopeSpec
  // read the visibility half back out of, because no control here could edit
  // it. Carrying was never the goal; not erasing was.
  //
  // sessionMode collapses the plain default to "", which is what "--scope not
  // named" means on a spawn.
  function scopeSpec(base, excludeSelf, idsSet, sessionMode, visBase, visIdsSet) {
    // The ACTION set dies at global because `global+ids:` does not parse. The
    // see-only set does not: `global/…+vis-ids:` parses, redundantly but
    // legally, so dropping it would make an untouched apply narrow the scope.
    const ids = base === "global" ? [] : [...idsSet].sort();
    const visIds = [...(visIdsSet || [])].sort();
    if (sessionMode && base === "subtree" && !excludeSelf && ids.length === 0 &&
        !visBase && visIds.length === 0) return "";
    return window.harness.scopeSpec({ base, excludeSelf, ids, visBase: visBase || "", visIds });
  }

  // buildTaskChecklist renders one checkbox row per snapshot task (terminal
  // included, table order) into container, bound to idsSet. excludeId drops
  // the re-grant target from its own list ({self} is always in scope).
  // Checked ids whose task is no longer in the snapshot still get a row —
  // silently dropping them would make an apply narrow the scope invisibly.
  function buildTaskChecklist(container, idsSet, excludeId, onChange) {
    if (!container) return;
    container.innerHTML = "";
    const addRow = (id, text, known) => {
      const label = document.createElement("label");
      const cb = document.createElement("input");
      cb.type = "checkbox";
      cb.checked = idsSet.has(id);
      cb.addEventListener("change", () => {
        if (cb.checked) idsSet.add(id); else idsSet.delete(id);
        onChange();
      });
      label.appendChild(cb);
      label.appendChild(document.createTextNode(" " + text));
      if (!known) label.style.color = "#b8864b";
      container.appendChild(label);
    };
    // Repo tail beside the agent, left-truncated like the TUI picker rows —
    // multi-root runners tell tasks apart by repo, not agent.
    const truncLeft = (s, n) => (s && s.length > n ? "…" + s.slice(-n) : (s || ""));
    const seen = new Set();
    for (const t of (lastTasks || [])) {
      if (t.id === excludeId) continue;
      seen.add(t.id);
      const head = (t.prompt || "").slice(0, 30);
      addRow(t.id, `${t.id.slice(0, 8)} ${t.status} ${agentLabel(t.agentProfile, t.skillsInjected)} ${truncLeft(t.repoPath, 20)} ${head}`, true);
    }
    for (const id of [...idsSet].sort()) {
      if (!seen.has(id) && id !== excludeId) addRow(id, `${id.slice(0, 8)} (不明なタスク)`, false);
    }
    if (!container.childElementCount) {
      const d = document.createElement("div");
      d.className = "empty";
      d.textContent = "(タスクなし)";
      container.appendChild(d);
    }
  }

  // Spawn scope picker: base radios + task checklist, serialized into
  // spawnScope exactly where the old free-text field's value went.
  function updateSpawnScope() {
    spawnScope = scopeSpec(spawnBase, spawnExcludeSelf, spawnScopeIds, true,
      spawnVisBase, spawnVisIds);
    const list = document.getElementById("spawn-scope-tasks");
    if (list) list.classList.toggle("disabled", spawnBase === "global");
    const sum = document.getElementById("spawn-scope-summary");
    if (sum) {
      const n = spawnBase === "global" ? 0 : spawnScopeIds.size;
      sum.textContent = `対象タスクを選択 (${n})`;
    }
    // Never disabled: unlike the action list, a global visibility rank makes
    // this clause redundant rather than illegal, and it is still serialized —
    // greying out a control whose value keeps being sent is the not-shown /
    // still-kept split this dialog already got wrong once.
    const visSum = document.getElementById("spawn-vis-summary");
    if (visSum) visSum.textContent = `追加で見えるだけのタスク (${spawnVisIds.size})`;
    const echo = document.getElementById("spawn-scope-echo");
    if (echo) {
      let line = "scope: " + (spawnScope || "subtree (default)");
      const sf = overrideSpecsFrom("spawn");
      if (sf.length > 0) line += "  +" + sf.join(" ");
      echo.textContent = line;
    }
  }

  function refreshSpawnScopeChecklist() {
    buildTaskChecklist(document.getElementById("spawn-scope-tasks"),
      spawnScopeIds, "", updateSpawnScope);
    buildTaskChecklist(document.getElementById("spawn-vis-tasks"),
      spawnVisIds, "", updateSpawnScope);
    // Override rows carry their own checklists, so they need the same refresh:
    // a row opened before the first snapshot would otherwise offer no tasks.
    buildOverrideRows("spawn", "spawn-scope-for-rows", updateSpawnScope);
    updateSpawnScope();
  }

  function initScope() {
    const baseRow = document.getElementById("spawn-base-row");
    if (!baseRow) return;
    for (const r of baseRow.querySelectorAll('input[type="radio"]')) {
      r.addEventListener("change", () => {
        if (r.checked) { spawnBase = r.value; updateSpawnScope(); }
      });
    }
    const selfCb = document.getElementById("spawn-exclude-self");
    if (selfCb) {
      selfCb.addEventListener("change", () => {
        spawnExcludeSelf = selfCb.checked;
        updateSpawnScope();
      });
    }
    const visRow = document.getElementById("spawn-vis-row");
    if (visRow) {
      for (const r of visRow.querySelectorAll('input[type="radio"]')) {
        r.addEventListener("change", () => {
          if (r.checked) { spawnVisBase = r.value; updateSpawnScope(); }
        });
      }
    }
    // Built on open as well as on snapshot, for the same reason the override
    // rows are: expanding this before the first snapshot would find it empty.
    const visDetails = document.getElementById("spawn-vis-details");
    if (visDetails) {
      visDetails.addEventListener("toggle", () => {
        if (visDetails.open) {
          buildTaskChecklist(document.getElementById("spawn-vis-tasks"),
            spawnVisIds, "", updateSpawnScope);
        }
      });
    }
    buildOverrideRows("spawn", "spawn-scope-for-rows", updateSpawnScope);
    // Also on open. The snapshot-driven rebuild below only fires once a task
    // list arrives, so an operator who expands this before the first snapshot
    // — or on a server with no tasks yet — would otherwise find it empty.
    const sfDetails = document.getElementById("spawn-scope-for-details");
    if (sfDetails) {
      sfDetails.addEventListener("toggle", () => {
        if (sfDetails.open) buildOverrideRows("spawn", "spawn-scope-for-rows", updateSpawnScope);
      });
    }
    refreshSpawnScopeChecklist();
  }

  // Re-grant dialog: selection UI over a live task's authority. Operator-only
  // server-side; a WebUI connection is an operator connection by construction,
  // so the control is shown unconditionally. An apply always sends BOTH
  // fields explicitly — the dialog is prefilled with the current values, so
  // an untouched field sends its current value, which is the same keep.
  const regrantModal = document.getElementById("regrant-modal");
  let regrantTaskId = "";
  let regrantBits = 0;
  let regrantBase = "subtree";
  let regrantExcludeSelf = false;
  const regrantIds = new Set();
  // The visibility half, seeded from the target and then EDITED. It used to be
  // a `regrantCarry` holding the whole scope string, which ScopeSpec read the
  // visibility half back out of — the dialog could not erase what it showed no
  // control for, but it could not change it either.
  let regrantVisBase = "";
  const regrantVisIds = new Set();

  function updateRegrantEcho() {
    const list = document.getElementById("regrant-tasks");
    if (list) list.classList.toggle("disabled", regrantBase === "global");
    // Never disabled — see updateSpawnScope.
    const visSum = document.getElementById("regrant-vis-summary");
    if (visSum) visSum.textContent = `追加で見えるだけのタスク (${regrantVisIds.size})`;
    const echo = document.getElementById("regrant-echo");
    if (echo) {
      let line = "→ caps=" + capsLabelFor(regrantBits) +
        "  scope=" + scopeSpec(regrantBase, regrantExcludeSelf, regrantIds, false,
          regrantVisBase, regrantVisIds);
      const sf = overrideSpecsFrom("regrant");
      if (sf.length > 0) line += "  +" + sf.join(" ");
      echo.textContent = line;
    }
  }

  function openRegrantDialog(t) {
    if (!regrantModal || typeof window.harness.setCaps !== "function") return;
    regrantTaskId = t.id;
    regrantBits = typeof t.capsBits === "number" ? t.capsBits : 0;
    regrantBase = t.scopeBase || "subtree";
    regrantExcludeSelf = !!t.scopeExcludeSelf;
    regrantIds.clear();
    for (const id of (t.scopeIds || [])) regrantIds.add(id);
    // The visibility half from its own RAW snapshot fields. scopeVisBase is ""
    // when the rank is unstated, which is a value the radio row has to be able
    // to show and to return to — reading it off the label would collapse it
    // into the action rank.
    regrantVisBase = t.scopeVisBase || "";
    regrantVisIds.clear();
    for (const id of (t.scopeVisIds || [])) regrantVisIds.add(id);
    // Seeded, not blank: overrides travel with the scope under one presence
    // bit, so applying with no rows CLEARS them. Showing them as rows is what
    // makes that an operator's choice rather than a silent erase.
    overrideRowsFor.regrant = (t.scopeForSpecs || [])
      .map(parseOverrideSpec)
      .filter((r) => r !== null);
    const title = document.getElementById("regrant-task");
    if (title) title.textContent = t.id.slice(0, 8);
    buildCapChips(document.getElementById("regrant-chips"),
      () => regrantBits, (v) => { regrantBits = v; updateRegrantEcho(); });
    for (const r of document.querySelectorAll('#regrant-base-row input[type="radio"]')) {
      r.checked = (r.value === regrantBase);
    }
    const rSelf = document.getElementById("regrant-exclude-self");
    if (rSelf) rSelf.checked = regrantExcludeSelf;
    for (const r of document.querySelectorAll('#regrant-vis-row input[type="radio"]')) {
      r.checked = (r.value === regrantVisBase);
    }
    buildTaskChecklist(document.getElementById("regrant-tasks"),
      regrantIds, t.id, updateRegrantEcho);
    // Self is always visible, so the target is excluded from its own see-list
    // for the same reason it is excluded from its own target list.
    buildTaskChecklist(document.getElementById("regrant-vis-tasks"),
      regrantVisIds, t.id, updateRegrantEcho);
    buildOverrideRows("regrant", "regrant-scope-for-rows", updateRegrantEcho);
    document.getElementById("regrant-cascade").checked = false;
    document.getElementById("regrant-keep-conns").checked = false;
    updateRegrantEcho();
    regrantModal.showModal();
  }

  if (regrantModal) {
    for (const r of document.querySelectorAll('#regrant-base-row input[type="radio"]')) {
      r.addEventListener("change", () => {
        if (r.checked) { regrantBase = r.value; updateRegrantEcho(); }
      });
    }
    const rSelfCb = document.getElementById("regrant-exclude-self");
    if (rSelfCb) {
      rSelfCb.addEventListener("change", () => {
        regrantExcludeSelf = rSelfCb.checked;
        updateRegrantEcho();
      });
    }
    for (const r of document.querySelectorAll('#regrant-vis-row input[type="radio"]')) {
      r.addEventListener("change", () => {
        if (r.checked) { regrantVisBase = r.value; updateRegrantEcho(); }
      });
    }
    document.getElementById("regrant-cancel").addEventListener("click", () => regrantModal.close());
    document.getElementById("regrant-apply").addEventListener("click", async () => {
      const req = {
        taskId: regrantTaskId,
        caps: regrantBits,
        scope: scopeSpec(regrantBase, regrantExcludeSelf, regrantIds, false,
          regrantVisBase, regrantVisIds),
        scopeFor: overrideSpecsFrom("regrant"),
        cascade: document.getElementById("regrant-cascade").checked,
        keepConns: document.getElementById("regrant-keep-conns").checked,
      };
      regrantModal.close();
      // Results go to the Command output like the other task-sheet actions
      // (see the ✕ Cancel handler) — setStatus is the CONNECTION indicator,
      // and writing here left "caps set: …" sitting where "connected" lives.
      try {
        const res = await window.harness.setCaps(req);
        // Name the target and the change — "1 task changed" says nothing on
        // the usual single-target call. The count appears only when a
        // cascade actually reached descendants.
        let msg = "caps set " + regrantTaskId.slice(0, 8) +
          ": caps=" + capsLabelFor(regrantBits) + "  scope=" + req.scope;
        const extra = res.affected.length - 1;
        if (extra > 0) msg += `  (+${extra} descendant(s) clamped)`;
        if (res.connsClosed > 0) msg += ", " + res.connsClosed + " connection(s) closed";
        appendCmdOutput(msg);
        await refreshSnapshot();
      } catch (e) {
        appendCmdOutput("caps set failed: " + (e && e.message ? e.message : e));
      }
    });
  }

  // Parent-picker modal: re-points a task's parent link via harness.setParent.
  // Single-choice radios; the current parent (or root when the task has none)
  // is pre-checked so an accidental apply is a no-op.
  const parentModal = document.getElementById("parent-modal");
  let parentTaskId = "";

  // setParentMessage renders the same result shapes as cli.SetParentMessage —
  // one line naming the target and the change (shared by runCmd and the
  // dialog; "" from the wasm bridge means the root).
  const setParentMessage = (taskId, swap, r) => {
    const s8 = (h) => (h ? h.slice(0, 8) : "(root)");
    const t8 = taskId.slice(0, 8);
    if (swap) {
      return `set-parent ${t8} --swap: ${t8} now under ${s8(r.newParent)}, ${s8(r.swappedId)} now under ${t8}`;
    }
    return `set-parent ${t8}: parent=${s8(r.oldParent)} → ${s8(r.newParent)}`;
  };

  function openParentDialog(t) {
    if (!parentModal || typeof window.harness.setParent !== "function") return;
    parentTaskId = t.id;
    const title = document.getElementById("parent-task");
    if (title) title.textContent = t.id.slice(0, 8);
    const rows = document.getElementById("parent-rows");
    rows.innerHTML = "";
    const currentParent = t.createdById || "";
    const addRow = (value, text, checked) => {
      const label = document.createElement("label");
      const rb = document.createElement("input");
      rb.type = "radio";
      rb.name = "parent-choice";
      rb.value = value;
      rb.checked = checked;
      label.appendChild(rb);
      label.appendChild(document.createTextNode(" " + text));
      rows.appendChild(label);
    };
    addRow("root", "(root — 親から切り離す)", currentParent === "");
    if (currentParent) {
      addRow("swap", `(swap with ${currentParent.slice(0, 8)} — 入れ替えて自分が親になる)`, false);
    }
    const truncLeft = (s, n) => (s && s.length > n ? "…" + s.slice(-n) : (s || ""));
    for (const row of (lastTasks || [])) {
      if (row.id === t.id) continue;
      const head = (row.prompt || "").slice(0, 30);
      let text = `${row.id.slice(0, 8)} ${row.status} ${agentLabel(row.agentProfile, row.skillsInjected)} ${truncLeft(row.repoPath, 20)} ${head}`;
      if (row.id === currentParent) text += " ← 現在の親";
      addRow(row.id, text, row.id === currentParent);
    }
    parentModal.showModal();
  }

  if (parentModal) {
    document.getElementById("parent-cancel").addEventListener("click", () => parentModal.close());
    document.getElementById("parent-apply").addEventListener("click", async () => {
      const picked = parentModal.querySelector('input[name="parent-choice"]:checked');
      parentModal.close();
      if (!picked) return;
      const req = { taskId: parentTaskId };
      if (picked.value === "swap") req.swap = true;
      else if (picked.value !== "root") req.parentId = picked.value;
      // Results go to the Command output, like the other task-sheet actions
      // (see the ✕ Cancel handler) — setStatus is the CONNECTION indicator.
      try {
        const r = await window.harness.setParent(req);
        appendCmdOutput(setParentMessage(parentTaskId, req.swap === true, r));
        await refreshSnapshot();
      } catch (e) {
        appendCmdOutput("set-parent failed: " + (e && e.message ? e.message : e));
      }
    });
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

  // reattachTo re-attaches (control mode) to an existing live session by id.
  // Shared by the Reattach button, the task-row Reattach action, the preview
  // modal's shortcut, and the post-takeover quick button (DRY). Switches to
  // the terminal tab, replays, and pins to the bottom. Read-only observation
  // is the session-preview modal's job (live view-attach at the true grid) —
  // the old 👁 View route that view-attached the MAIN xterm was removed: a
  // viewer has no size authority, so it mis-rendered whenever the grids
  // differed, and it cost the browser its own control attach to boot.
  const reattachTo = async (id) => {
    if (!id) { attachedTask.textContent = "(session id required)"; return; }
    attachEpoch++;            // invalidate any in-flight close handler
    hideQuickReattach();
    showTerminalView();
    try {
      const taskID = await window.harness.attachSession(id, "control");
      attachedTask.textContent = `attached: ${taskID} (reattached)`;
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
    const req = sessionReq({ agent, claudeArgs: args, resumeTaskId: id, resumeConversation: true });
    try {
      const taskID = await window.harness.startInteractive(req);
      showTerminalView();
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
          // baseReq came from sessionReq, so caps/resumeCapsOverride are already
          // correct; only the picked (runner, profile) overrides differ.
          const retryReq = { ...baseReq, host: "", runner: c.cid, agent: c.profile || "" };
          const taskID = await window.harness.startInteractive(retryReq);
          if (retryReq.eventStream) {
            // The picker is a SECOND request build for the same intent, and it
            // has to end where the first one would have. Without this an
            // ambiguous --stream spawn silently became a PTY session: the flag
            // rode the request correctly and the success path did not know
            // about it. Exactly checklist 28a's shape — one input surface, two
            // builds, one of them older than the field.
            await openChatFor(taskID);
            return;
          }
          showTerminalView();
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
    if (req.eventStream) {
      // This kind has no PTY: the wasm side opens and detaches rather than
      // mounting the xterm, and the chat attaches to the stream instead.
      try {
        const taskID = await window.harness.startInteractive(req);
        await openChatFor(taskID);
      } catch (e) {
        if (routeAmbiguous(e, req)) return;
        alert(`startInteractive --stream: ${e.message}`);
      }
      return;
    }
    try {
      const taskID = await window.harness.startInteractive(req);
      showTerminalView();
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

  // --- Session preview: LIVE view of interactive session(s), typable. A
  //     cowrite attach stream stays open per pane while shown; bytes flow
  //     into a throwaway xterm sized to the session's real grid and
  //     CSS-scaled to fit. ⏸ pauses by CLOSING the stream (the frozen frame
  //     stays; zero load while paused); ▶ re-attaches — the server's ring
  //     replay reconstructs the current screen, so resume jumps to now.
  //     Closing disconnects immediately. A cowrite attach never takes over
  //     the controlling client and carries no size authority, and each stream
  //     is independent of the main terminal's singleton, so peeking is always
  //     safe — including at the session currently attached here — while
  //     click-to-focus still lets you type into what you are watching.
  //
  //     previewPanes is a registry of every live pane, keyed by an opaque
  //     paneKey that matches the wasm side's previewSlots map
  //     (cli/preview_wasm.go) 1:1 — the wasm pump tags every
  //     harness_preview* hook call with the same paneKey, so N independent
  //     panes can share the one wasm client. The single-preview modal
  //     registers exactly one pane under the fixed key "preview"; the
  //     session grid registers one pane per cell under "grid:<taskId>".
  //     Record shape: { taskId, live, epoch, term, bodyEl, scaleBox, spacer,
  //     noteEl, onResize, onClosed }.
  const sessionPreviewModal    = document.getElementById("session-preview-modal");
  const sessionPreviewTitle    = document.getElementById("session-preview-title");
  const sessionPreviewBody     = document.getElementById("session-preview-body");
  const sessionPreviewPause    = document.getElementById("session-preview-pause");
  const sessionPreviewReattach = document.getElementById("session-preview-reattach");
  const sessionPreviewClose    = document.getElementById("session-preview-close");

  const previewPanes = new Map();

  function disposePaneTerm(p) {
    if (p.term) { p.term.dispose(); p.term = null; }
    if (p.bodyEl) p.bodyEl.replaceChildren();
    p.scaleBox = null;
    p.spacer = null;
    p.noteEl = null;
  }

  function paneNote(p, text) {
    const el = document.createElement("p");
    el.className = "preview-note";
    el.textContent = text;
    if (p.bodyEl) p.bodyEl.appendChild(el);
    p.noteEl = el;
    return el;
  }

  // setPreviewPauseLabel reflects the "preview" pane's live flag onto the
  // single-preview modal's ⏸/▶ toggle. Grid panes have no such per-pane
  // control (dismiss (✕) is the only lifecycle action there), so this stays
  // specific to the "preview" key rather than becoming a per-pane callback.
  function setPreviewPauseLabel() {
    const live = !!previewPanes.get("preview")?.live;
    sessionPreviewPause.textContent = live ? "⏸" : "▶";
    sessionPreviewPause.title = live ? "一時停止" : "再開";
  }

  // startPane (re)opens the live stream for paneKey/p.taskId, generalizing
  // the single preview's former startSessionPreviewStream over the pane
  // registry so grid cells share the same start/stop/epoch-guard logic. The
  // grid size arrives asynchronously via harness_previewOpen, which builds
  // the term; until then the pane shows a connecting note. p.live flips to
  // true synchronously (before the awaited RPC settles), matching the
  // original's immediate ⏸-available feedback.
  async function startPane(key, p) {
    const epoch = ++p.epoch;
    p.pausedNote = null; // a fresh stream invalidates the "paused" hint
    disposePaneTerm(p);
    const note = paneNote(p, "connecting…");
    p.live = true;
    try {
      await window.harness.previewStart(key, p.taskId, !!p.capReplay);
    } catch (e) {
      // Guard against a stale continuation: teardown paths (stopPane /
      // teardownGridPanes / dismissPane) flip p.live off and stop the wasm
      // stream but never bump p.epoch, so a torn-down-then-recreated
      // same-key pane looks identical to "still current" under the epoch
      // check alone. Confirming the registry still maps key -> this exact
      // object is what actually distinguishes them.
      if (epoch !== p.epoch) return; // superseded/closed meanwhile
      if (previewPanes.get(key) !== p) return; // key now owned by a different (recreated) pane
      note.textContent = `preview error: ${e.message}`;
      p.live = false;
      if (p.onClosed) p.onClosed();
      return;
    }
    // Reconcile a narrow race: if the PREVIOUS stream's death was delivered
    // while our attach RPC was in flight, harness_previewClosed already
    // flipped p.live off — the stream we just installed would run
    // unrendered behind a paused (▶) UI. Stop it; the death note stands.
    //
    // The `previewPanes.get(key) === p` check additionally guards against a
    // dismissed-then-recreated same-key pane: grid teardown (stopPane /
    // teardownGridPanes / dismissPane) sets live=false and calls
    // previewStop(key) but never bumps epoch, so a stale in-flight
    // previewStart for the OLD pane can land here with epoch === p.epoch &&
    // !p.live even after a brand-new pane has taken over `key`. Without this
    // identity check, the stale continuation would call previewStop(key)
    // and kill the NEW pane's live stream.
    if (epoch === p.epoch && !p.live && previewPanes.get(key) === p) {
      window.harness.previewStop(key);
    }
  }

  // stopPane flips paneKey's pane to not-live (BEFORE calling previewStop,
  // so any hook racing the wasm-side teardown no-ops) then disconnects the
  // stream. Idempotent — a no-op if paneKey isn't registered.
  function stopPane(key) {
    const p = previewPanes.get(key);
    if (!p) return;
    p.live = false;
    window.harness.previewStop(key);
  }

  // Pane terminals open at PANE_FONT_BASE and are shrunk by fitPaneScale to fit
  // their pane. PANE_FONT_MIN is only a positivity guard, NOT a legibility
  // choice: xterm's cell width tracks fontSize proportionally and sub-pixel all
  // the way down (measured — 240 cols is 1560px at 13px and exactly 240px at
  // 2px, cell width 1px), so anything higher would clip wide terminals instead
  // of shrinking them. A grid pane that small is an activity indicator rather
  // than readable text, which was equally true of the CSS transform this
  // replaced — a 240-col session scaled into a 300px cell is ~2.4px of glyph
  // either way.
  const PANE_FONT_BASE = 13;
  const PANE_FONT_MIN = 1;

  // buildPaneTerm creates the throwaway xterm at the session's true grid
  // (re-rendering at a smaller grid would corrupt full-screen TUI layouts)
  // inside the scale/spacer pair, then fits it to the pane's own width.
  function buildPaneTerm(p, rows, cols) {
    if (!p.bodyEl) return;
    p.bodyEl.replaceChildren(); // drop the connecting note
    const spacer = document.createElement("div");
    spacer.className = "session-preview-spacer";
    const scaleBox = document.createElement("div");
    scaleBox.className = "session-preview-scale";
    const termBox = document.createElement("div");
    scaleBox.appendChild(termBox);
    spacer.appendChild(scaleBox);
    p.bodyEl.appendChild(spacer);
    p.spacer = spacer;
    p.scaleBox = scaleBox;
    p.term = new Terminal({
      cols, rows,
      // EVERY pane accepts keystrokes when focused — grid cell and single
      // preview alike. The preview used to be read-only, which looked
      // identical to a typable one: you clicked in, typed, and the keys went
      // nowhere with no feedback at all. xterm fires onData only for the
      // focused terminal, so click/tap-to-focus routes typing to exactly one
      // pane — no explicit focus bookkeeping needed.
      disableStdin: false,
      convertEol: true,  // match the main terminal so the stream renders identically
      fontSize: PANE_FONT_BASE,
      fontFamily: '"Cascadia Mono", "JetBrains Mono", "DejaVu Sans Mono", "Liberation Mono", Menlo, Consolas, "Courier New", monospace',
    });
    p.term.open(termBox);
    // Forward this pane's keystrokes to its session over the cowrite stream.
    // Guarded by p.live so a keystroke racing teardown is dropped rather than
    // sent to a stopped stream (the wasm side additionally no-ops an unknown
    // paneKey). The pane claims no size authority, so no resize is forwarded.
    //
    // Typing into a PAUSED pane says so instead of vanishing: ⏸ closes the
    // stream, so there is nothing to write to, and silently swallowing the
    // keystroke is the same failure the read-only preview had.
    p.term.onData((data) => {
      if (p.live) { window.harness.previewInput(p.key, data); return; }
      if (p.pausedNote) return; // already said it; don't stack notes per key
      p.pausedNote = paneNote(p, "一時停止中 — ▶ で再開すると入力できます");
    });
    p.onResize = () => fitPaneScale(p);
    p.onResize();
  }

  // fitPaneScale shrinks the pane's terminal to the PANE's own body width
  // (p.bodyEl) — for a grid cell that's the cell body, not the whole modal, so
  // each pane fits independently of the others.
  //
  // It scales by lowering the FONT SIZE, not with a CSS transform. xterm derives
  // mouse -> cell coordinates from the screen element's getBoundingClientRect()
  // (which DOES reflect a CSS transform) divided by the cell size it measured
  // when the terminal opened (which does NOT). A transform applied after open()
  // therefore desyncs selection from the pointer by exactly 1/scale: the
  // highlight lands at scale x the pointer's distance from the pane's top-left,
  // so dragging across a 0.67-scaled pane selected text a quarter of a
  // pane-width to the left of the cursor. Driving fontSize instead keeps
  // cols x rows intact and keeps both sides of that division in one space.
  function fitPaneScale(p) {
    if (!p.term || !p.scaleBox || !p.spacer || !p.bodyEl) return;
    const screenEl = p.scaleBox.querySelector(".xterm-screen");
    if (!screenEl) return;
    const avail = p.bodyEl.clientWidth - 12; // body padding allowance
    if (avail <= 0) return;

    // Cell width is proportional to font size, so a single measurement at the
    // base size solves for the size that fits. Cached per CELL (not per pane
    // width) so a previewResize, which changes cols, can reuse it.
    if (!p.baseCellW) {
      if (p.term.options.fontSize !== PANE_FONT_BASE) {
        p.term.options.fontSize = PANE_FONT_BASE;
      }
      const w = screenEl.getBoundingClientRect().width;
      if (w <= 0) return;
      p.baseCellW = w / p.term.cols;
    }

    const needed = p.baseCellW * p.term.cols;
    const size = Math.max(
      PANE_FONT_MIN,
      Math.min(PANE_FONT_BASE, (PANE_FONT_BASE * avail) / needed),
    );
    if (p.term.options.fontSize !== size) p.term.options.fontSize = size;

    // Layout tracks the font now, so the transform-era overrides — which only
    // existed because a transform leaves layout untouched — are cleared.
    p.scaleBox.style.transform = "";
    p.scaleBox.style.width = "";
    p.spacer.style.height = "";
  }

  // wasm→JS hooks, routed by paneKey (cli/preview_wasm.go tags every call
  // with the pane's key; see previewCall there). The wasm pump is
  // generation-gated (stale pumps go silent); p.live additionally closes the
  // residual check-then-invoke race: after a pause/close, p.live is already
  // false, so a late hook no-ops. An unknown/already-dismissed paneKey is
  // silently ignored.
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
    if (p.onResize) p.onResize();
  };
  window.harness_previewClosed = (paneKey) => {
    const p = previewPanes.get(paneKey);
    if (!p || !p.live) return;
    p.live = false;
    if (!p.term && p.bodyEl) p.bodyEl.replaceChildren(); // died before the grid arrived
    paneNote(p, "(ストリーム終了 — ▶ で再接続)");
    if (p.onClosed) p.onClosed();
  };

  // openSessionPreview shows the single-preview modal for `id`, registering
  // its pane under the fixed key "preview".
  function openSessionPreview(id) {
    const key = "preview";
    // The hint rides in the title because the affordance is invisible
    // otherwise: a pane you can type into looks exactly like one you cannot,
    // and the previous read-only version taught exactly the wrong reflex.
    sessionPreviewTitle.textContent = `🔍 ${id.slice(0, 12)}… · クリックで入力`;
    const p = {
      taskId: id, live: false, epoch: 0, term: null,
      bodyEl: sessionPreviewBody, scaleBox: null, spacer: null, noteEl: null,
      onResize: null,
      onClosed: () => setPreviewPauseLabel(),
      key, capReplay: false, // full-size terminal: keep the whole scrollback
    };
    previewPanes.set(key, p);
    if (!sessionPreviewModal.open) sessionPreviewModal.showModal();
    startPane(key, p);       // p.live flips true synchronously before the first await
    setPreviewPauseLabel();
  }

  sessionPreviewClose.addEventListener("click", () => sessionPreviewModal.close());
  // Backdrop click (the dialog element itself, outside its content) closes —
  // same convention as file-preview-modal.
  sessionPreviewModal.addEventListener("click", (ev) => {
    if (ev.target === sessionPreviewModal) sessionPreviewModal.close();
  });
  sessionPreviewModal.addEventListener("close", () => {
    const p = previewPanes.get("preview");
    if (p) p.epoch++;              // invalidate any in-flight previewStart
    stopPane("preview");            // close = disconnect immediately
    if (p) disposePaneTerm(p);
    previewPanes.delete("preview");
  });
  sessionPreviewPause.addEventListener("click", () => {
    const p = previewPanes.get("preview");
    if (!p) return;
    if (p.live) stopPane("preview");
    else startPane("preview", p);
    setPreviewPauseLabel();
  });
  sessionPreviewReattach.addEventListener("click", () => {
    const p = previewPanes.get("preview");
    const id = p ? p.taskId : "";
    // close() queues the "close" event as a task, so previewStop runs just
    // AFTER reattachTo below kicks off — harmless: the view stream is
    // independent of the control attach and is torn down moments later.
    sessionPreviewModal.close();
    reattachTo(id);
  });

  // --- Session grid: an at-a-glance live monitor of multiple interactive
  //     sessions at once, each cell its own pane over the same previewPanes
  //     registry (key "grid:<taskId>"), so start/stop/hook-routing is shared
  //     verbatim with the single preview above. Pane cap: 9 (v1).
  const gridModal = document.getElementById("session-grid-modal");
  const gridBody  = document.getElementById("session-grid-body");
  const gridTitle = document.getElementById("session-grid-title");
  let gridKeys = [];

  // teardownGridPanes stops every current grid pane's stream and clears the
  // registry, WITHOUT touching the dialog's open state. Kept separate from
  // closeSessionGrid because dialog.close() fires "close" as a queued task
  // (HTML spec), not synchronously — calling it from inside openSessionGrid
  // (to reset a currently-open grid before repopulating it) would otherwise
  // let that queued event tear down the brand-new panes we're about to
  // build, and showModal() throws InvalidStateError on an already-open
  // dialog. openSessionGrid calls this directly and never closes/reopens
  // the dialog when it's already showing.
  function teardownGridPanes() {
    for (const key of gridKeys) {
      stopPane(key);
      const p = previewPanes.get(key);
      if (p) disposePaneTerm(p);
      previewPanes.delete(key);
    }
    gridKeys = [];
  }

  // liveInteractiveIds returns every live (Running/Detached) interactive
  // session id, activity-desc (same ordering as the task list). Used by the
  // `grid` command's no-arg path; the show button reads lastTasks directly.
  async function liveInteractiveIds() {
    const snap = await window.harness.snapshot();
    return (snap.tasks || [])
      // isPTYKind: feeds the same grid as above.
      .filter((t) => isPTYKind(t) && (t.status === "Running" || t.status === "Detached"))
      .sort((a, b) => taskActivityMs(b) - taskActivityMs(a))
      .map((t) => t.id);
  }

  // openSessionGrid tiles ids. scopeLabel is cli.GridSet's name for how they
  // were chosen — display only (the narrowing already happened), but the title
  // has to carry it: a grid showing three of eleven sessions with no word about
  // why is indistinguishable from eight dead ones. It falls back to "all" for
  // the same reason the TUI's status bar spells that out — a blank reads as "no
  // narrowing", which is what a narrowed grid missing its label also looks like.
  function openSessionGrid(ids, scopeLabel) {
    if (gridTitle) {
      gridTitle.textContent = `セッショングリッド (scope: ${scopeLabel || "all"})`;
    }
    teardownGridPanes();
    gridBody.replaceChildren();
    const capped = [...new Set(ids)].slice(0, 9); // dedupe (same id twice must not orphan a cell), then pane cap (v1)
    for (const id of capped) {
      const key = "grid:" + id;
      const cell = document.createElement("div");
      cell.className = "grid-cell";
      const head = document.createElement("div");
      head.className = "grid-cell-head";
      const label = document.createElement("span");
      label.className = "grid-cell-label";
      label.textContent = id.slice(0, 8);
      label.title = id;
      const attach = document.createElement("button");
      attach.type = "button";
      attach.className = "grid-cell-btn";
      attach.textContent = "↪";
      attach.title = "リアタッチ";
      attach.addEventListener("click", () => { closeSessionGrid(); reattachTo(id); });
      const notify = document.createElement("button");
      notify.type = "button";
      notify.className = "grid-cell-btn";
      notify.textContent = "🔔";
      notify.title = "idleで通知";
      notify.addEventListener("click", async () => {
        try {
          const r = await window.harness.awaitIdle({ taskId: id, sink: "notify" });
          appendCmdOutput(`await-idle ${id.slice(0, 12)}: ${r.status}`, true);
        } catch (e) {
          appendCmdOutput(`await-idle: ${e.message}`, true);
        }
      });
      const dismiss = document.createElement("button");
      dismiss.type = "button";
      dismiss.className = "grid-cell-btn";
      dismiss.textContent = "✕";
      dismiss.title = "閉じる";
      dismiss.addEventListener("click", () => dismissPane(key, cell));
      head.append(label, attach, notify, dismiss);
      const body = document.createElement("div");
      body.className = "grid-cell-body";
      cell.append(head, body);
      gridBody.append(cell);

      const p = {
        taskId: id, live: false, epoch: 0, term: null,
        bodyEl: body, scaleBox: null, spacer: null, noteEl: null,
        onResize: null, onClosed: null,
        key, capReplay: true, // small crop: 128 KiB of replay is plenty
      };
      previewPanes.set(key, p);
      gridKeys.push(key);
      startPane(key, p);
    }
    if (!gridModal.open) gridModal.showModal();
  }

  // openGridSet opens the session grid over the set cli.GridSet picks, and
  // returns the line to report. It never throws: every caller (the task
  // sheet's ▦ button, the `grid` command's every form) renders the outcome the
  // same way, through appendCmdOutput.
  //
  // req is {mode, anchor, ids} — see harness.gridSet. The wasm side answers
  // which TASKS and what the set is called, so the two surfaces cannot disagree
  // about either. WHICH of those tasks is tileable is decided here, by the same
  // liveInteractiveTasks + gridExcluded pair the show button uses: the set
  // picks the candidates, the per-session toggles still subtract from them.
  //
  // An empty result opens nothing. A modal that says "no sessions" costs a tap
  // to dismiss and tells the operator less than this line does — including the
  // two counts, since a set of four tasks none of which is watchable is a
  // different situation from a set of none.
  async function openGridSet(req) {
    let res;
    try {
      res = await window.harness.gridSet(req);
    } catch (e) {
      return `grid: ${e.message}`;
    }
    const inSet = new Set(res.ids || []);
    const label = res.label || "";
    const ids = liveInteractiveTasks()
      .filter((t) => inSet.has(t.id) && !gridExcluded.has(t.id))
      .sort((a, b) => taskActivityMs(b) - taskActivityMs(a))
      .map((t) => t.id);
    if (ids.length === 0) {
      return `grid ${label}: no live interactive session in this set (${inSet.size} task(s) in it)`;
    }
    openSessionGrid(ids, label);
    return `grid ${label}: ${ids.length} pane(s)` + (ids.length > 9 ? " (capped at 9)" : "");
  }

  function dismissPane(key, cell) {
    stopPane(key);
    const p = previewPanes.get(key);
    if (p) disposePaneTerm(p);
    previewPanes.delete(key);
    gridKeys = gridKeys.filter((k) => k !== key);
    cell.remove();
  }

  function closeSessionGrid() {
    teardownGridPanes();
    if (gridModal.open) gridModal.close();
  }

  // The dialog's native "close" (user hit Escape, or the ✕/backdrop handlers
  // below called .close()) only needs the pane teardown — calling
  // closeSessionGrid() here too would re-invoke .close() on an already-
  // closing dialog, which is harmless but redundant; teardownGridPanes is
  // the correct, minimal action for this event.
  gridModal.addEventListener("close", teardownGridPanes);
  // Backdrop click closes — same convention as the single-preview modal.
  gridModal.addEventListener("click", (ev) => {
    if (ev.target === gridModal) gridModal.close();
  });
  document.getElementById("session-grid-close").addEventListener("click", () => gridModal.close());

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

  // taskActivityMs returns the task's last-activity time in unix-ms, for
  // most-recently-active-first sorting. Live sessions (outputIdleMs >= 0)
  // derive from the server-computed idle age; finished / never-started tasks
  // fall back to wire timestamps, which are unix NANOseconds (toTaskInfo uses
  // UnixNano) — divide by 1e6. Zero wire values mean "unset" and lose to any
  // set value inside max().
  function taskActivityMs(t) {
    if (t.outputIdleMs >= 0) return Date.now() - t.outputIdleMs;
    return Math.max(t.endedAt || 0, t.startedAt || 0, t.createdAt || 0) / 1e6;
  }

  // taskMatchesFilter applies the status chip + lowercased search terms.
  // Terms are whitespace-split and ANDed: every term must substring-match at
  // least one search key (terms may hit different keys — "failed harness" =
  // status Failed AND repo contains harness). Keys: id, repoPath, status,
  // agentProfile, prompt (prompt is usually empty on interactive tasks —
  // repo and id are the effective keys).
  function taskMatchesFilter(t, terms) {
    if (taskStatusFilter === "active" && TERMINAL_STATES.has(t.status)) return false;
    if (taskStatusFilter === "finished" && !TERMINAL_STATES.has(t.status)) return false;
    if (terms.length === 0) return true;
    const keys = [t.id, t.repoPath, t.status, t.agentProfile, t.prompt]
      .map((v) => (v || "").toLowerCase());
    return terms.every((term) => keys.some((k) => k.includes(term)));
  }

  // repoTail returns the last path segment for display ("/a/b/repo" -> "repo",
  // "C:/x/y" -> "y"); full path stays available via the row's title attr.
  function repoTail(p) {
    const parts = (p || "").split(/[\\/]/).filter(Boolean);
    return parts.length ? parts[parts.length - 1] : (p || "-");
  }

  // taskStatusColor returns the dot/label color for a status. Terminal states
  // are muted so live rows pop; unknown (Pending/Assigned/...) falls back to
  // blue. Function declaration (not a const map) so the initial
  // refreshSnapshot() — which runs before this point in the file — can reach
  // it through hoisting without a TDZ error.
  function taskStatusColor(status) {
    switch (status) {
      case "Running":   return "#2d5";
      case "Detached":  return "#e5c07b";
      case "Failed":    return "#f14c4c";
      case "Succeeded":
      case "Cancelled": return "#888";
      default:          return "#61afef";
    }
  }

  function renderTaskList(tasks) {
    lastTasks = tasks || [];
    // The spawn scope checklist renders from the same snapshot; rebuild it
    // here so new/finished tasks appear without a manual refresh. The
    // re-grant dialog's list is deliberately NOT rebuilt mid-open — its
    // checked set is the source of truth and a rebuild would only reorder
    // rows under the operator's cursor.
    refreshSpawnScopeChecklist();
    const finished = lastTasks.filter((t) => TERMINAL_STATES.has(t.status)).length;
    taskChips.active.textContent   = `Active (${lastTasks.length - finished})`;
    taskChips.finished.textContent = `Finished (${finished})`;
    taskChips.all.textContent      = `All (${lastTasks.length})`;
    const terms = taskFilterInput.value.trim().toLowerCase().split(/\s+/).filter(Boolean);
    const visible = lastTasks
      .filter((t) => taskMatchesFilter(t, terms))
      .sort((a, b) => taskActivityMs(b) - taskActivityMs(a));
    // The rebuild below wipes all sheet DOM, which would close the open sheet
    // and reset any agent dropdown the user changed — every 5s poll. Capture
    // that per-task UI state first and restore it after the rebuild.
    const openSheetTaskId = taskList.querySelector(".task-sheet:not([hidden])")?.dataset.taskId ?? null;
    const agentPicks = {};
    for (const s of taskList.querySelectorAll(".task-sheet")) {
      const sel = s.querySelector(".task-agent-select");
      if (sel && s.dataset.taskId) agentPicks[s.dataset.taskId] = sel.value;
    }
    taskList.innerHTML = "";
    if (visible.length === 0) {
      const empty = document.createElement("div");
      empty.className = "task-empty";
      empty.textContent = lastTasks.length === 0 ? "(none)" : "(no matching tasks)";
      taskList.appendChild(empty);
      return;
    }
    for (const t of visible) {
      const wrap = document.createElement("div");
      const row = document.createElement("div");
      row.className = "task-row";
      row.title = `${t.id}\n${t.repoPath}`; // full id + path on hover; sheet has Copy id

      const line1 = document.createElement("div");
      line1.className = "task-row-line1";
      const dot = document.createElement("span");
      dot.className = "task-status-dot";
      dot.style.background = taskStatusColor(t.status);
      const statusEl = document.createElement("span");
      statusEl.className = "task-status-label";
      statusEl.style.color = taskStatusColor(t.status);
      statusEl.textContent = t.status;
      const repoEl = document.createElement("span");
      repoEl.className = "task-repo";
      repoEl.textContent = repoTail(t.repoPath);
      line1.append(dot, statusEl, repoEl);
      // Busy/idle badge from the server-computed idle age (-1 = no live
      // session output). Threshold mirrors cli.ActivityBusyThreshold (3s):
      // an in-flight agent TUI repaints ~every 100ms, an idle prompt emits
      // nothing, so 3s separates the two with wide margin.
      if (t.outputIdleMs >= 0) {
        const act = document.createElement("span");
        act.className = "task-act";
        act.textContent = activityBadge(t.outputIdleMs);
        line1.appendChild(act);
      }
      if (t.agentProfile) {
        const ag = document.createElement("span");
        ag.className = "task-agent";
        ag.textContent = agentLabel(t.agentProfile, t.skillsInjected);
        line1.appendChild(ag);
      }

      const meta = document.createElement("div");
      meta.className = "task-row-meta";
      let metaText = `${t.id.slice(0, 12)}…  ${t.kind}  from=${t.origin || "-"}`;
      if (t.createdBy) metaText += `  by=${t.createdBy}`;
      if (t.resumedBy) metaText += `  resumed_by=${t.resumedBy}`;
      if (t.caps) metaText += `  caps=${t.caps}`;
      // Always shown, subtree included: hiding the default read as "this
      // task has no scope", which is never true.
      if (t.scope) metaText += `  scope=${t.scope}`;
      // Appended only when present, matching ls and the TUI detail popup: the
      // scope is half a task's authority and is never absent, a narrowing that
      // is not there has nothing to report.
      if (t.scopeFor) metaText += `  +${t.scopeFor}`;
      // Who is on the live session, cowriters first — same wording and the same
      // always-printed rule as the CLI row: zeros included, so "nobody is
      // watching" never looks like "this row does not report watchers". This is
      // the surface where it matters most: opening the preview here IS an
      // attach, and it does not move the task out of Detached, so without this
      // the page gives no sign anyone is on it.
      if (taskSessionAlive(t)) {
        metaText += `  cowrite=${t.cowriters || 0} viewer=${t.viewers || 0}`;
      }
      meta.textContent = metaText;
      if (t.errorMsg) {
        const err = document.createElement("span");
        err.className = "task-err";
        err.textContent = `  err=${t.errorMsg}`;
        meta.appendChild(err);
      }

      row.append(line1, meta);
      if (t.prompt) {
        const promptEl = document.createElement("div");
        promptEl.className = "task-prompt";
        promptEl.textContent = t.prompt;
        row.appendChild(promptEl);
      }
      const sheet = document.createElement("div");
      sheet.className = "task-sheet";
      sheet.dataset.taskId = t.id;
      sheet.hidden = t.id !== openSheetTaskId;
      buildTaskSheet(sheet, t);
      // Restore the user's agent pick from before the rebuild (only if that
      // profile is still among the options — otherwise keep the default).
      const savedAgent = agentPicks[t.id];
      if (savedAgent !== undefined) {
        const agentSel = sheet.querySelector(".task-agent-select");
        if (agentSel && [...agentSel.options].some((o) => o.value === savedAgent)) {
          agentSel.value = savedAgent;
        }
      }
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

    // Reattach / Preview / grid-include toggle / idle-notify — live interactive
    // session only. Order: Reattach first, then Preview, then the grid toggle.
    if (t.kind === "Interactive" && (t.status === "Running" || t.status === "Detached")) {
      addItem("↪ Reattach", "", () => reattachTo(t.id));
      addItem("🔍 プレビュー", "", () => openSessionPreview(t.id));
      // Grid include/exclude toggle (default included). Updates its own label in
      // place so the whole sheet need not re-render on each click; the global
      // "グリッド表示" button reads the same gridExcluded set.
      const gridToggle = document.createElement("button");
      gridToggle.type = "button";
      gridToggle.className = "task-action";
      const paintGridToggle = () => {
        gridToggle.textContent = gridExcluded.has(t.id)
          ? "☐ グリッドから除外中"
          : "☑ グリッドに含める";
      };
      paintGridToggle();
      gridToggle.addEventListener("click", (e) => {
        e.stopPropagation();
        if (gridExcluded.has(t.id)) gridExcluded.delete(t.id);
        else gridExcluded.add(t.id);
        paintGridToggle();
      });
      sheet.appendChild(gridToggle);
      addItem("🔔 idleで通知", "", async () => {
        try {
          const r = await window.harness.awaitIdle({ taskId: t.id, sink: "notify" });
          appendCmdOutput(`await-idle ${t.id.slice(0, 12)}: ${r.status}`, true);
        } catch (e) {
          appendCmdOutput(`await-idle: ${e.message}`, true);
        }
      });
    }

    // Working-set grids — NOT gated on this task being a live interactive
    // session. The useful anchor is often a supervisor that is itself a
    // one-shot, or a finished parent whose workers are still running; gating
    // these on the row's own kind would hide them exactly where they are
    // wanted. An anchor with nothing tileable under it reports that instead of
    // opening.
    //
    // Two buttons, because the second is not a refinement of the first: it is
    // for when THIS session is already on screen in another terminal and its
    // workers are what is missing.
    addItem("▦ この配下をグリッド", "", async () => {
      appendCmdOutput(await openGridSet({ mode: "subtree", anchor: t.id }), true);
    });
    addItem("▦ 配下のみ (自分を除く)", "", async () => {
      appendCmdOutput(await openGridSet({ mode: "descendants", anchor: t.id }), true);
    });

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
        const req = sessionReq({ agent: agentSel.value || "", claudeArgs, resumeTaskId: t.id, resumeConversation, runner });
        try {
          const id = await window.harness.startInteractive(req);
          showTerminalView();
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
    // Live re-grant. Shown for every task: a WebUI connection is an operator
    // connection by construction (the PSK gate makes any non-agent client
    // prove operatorPSK), so there is no non-operator state to hide it in.
    addItem("🔑 caps/scope 再付与", "", () => openRegrantDialog(t));
    addItem("⇄ 親タスク変更", "", () => openParentDialog(t));

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
  const boardSubscribersEl  = document.getElementById("board-subscribers");
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
      // One boardSubscribers("") call returns every task with its full pattern
      // set, which gives both the per-topic count AND every subscribed name.
      let subCounts = null;
      try {
        const all = await window.harness.boardSubscribers("");
        subCounts = new Map();
        for (const r of (all || [])) {
          for (const pat of (r.patterns || [])) {
            subCounts.set(pat.name, (subCounts.get(pat.name) || 0) + 1);
          }
        }
      } catch (_) {
        // Counting is a nicety; a failure here must not blank the topic list.
        subCounts = null;
      }

      // The list is the UNION of topics that exist and patterns that are
      // subscribed. A topic only comes into existence when something is
      // published to it, so listing only board topics hides the state an
      // operator most wants to see: subscribed, nothing published yet. Those
      // rows carry msgs=0 and no last-publish time.
      const byName = new Map();
      for (const t of (topics || [])) byName.set(t.name, t);
      const names = new Set(byName.keys());
      if (subCounts) for (const pat of subCounts.keys()) names.add(pat);
      const listed = [...names].sort();

      if (listed.length === 0) {
        const empty = document.createElement("div");
        empty.style.color = "#666";
        empty.style.fontFamily = "monospace";
        empty.textContent = "(no topics)";
        boardTopicsEl.appendChild(empty);
        return;
      }
      for (const name of listed) {
        const t = byName.get(name);
        const row = document.createElement("div");
        row.className = t ? "board-topic-row" : "board-topic-row board-topic-unpublished";
        const nameSpan = document.createElement("span");
        nameSpan.className = "board-topic-name";
        nameSpan.textContent = name;
        const metaSpan = document.createElement("span");
        metaSpan.className = "board-topic-meta";
        const subs = subCounts ? `  subs=${subCounts.get(name) || 0}` : "";
        if (t) {
          const lastTime = t.lastPublishedAtMs
            ? new Date(t.lastPublishedAtMs).toISOString()
            : "-";
          // retracted= shows only when the topic holds withdrawn messages, and
          // is never folded into msgs= — that count answers "how much would a
          // subscriber receive", so a topic emptied by retraction reads msgs=0
          // while still saying there is something here to audit.
          const retracted = t.retractedCount
            ? `  retracted=${t.retractedCount}`
            : "";
          metaSpan.textContent = `msgs=${t.msgCount}${retracted}${subs}  last=${lastTime}`;
        } else {
          metaSpan.textContent = `msgs=0${subs}  (nothing published yet)`;
        }
        row.appendChild(nameSpan);
        row.appendChild(metaSpan);
        row.addEventListener("click", () => openBoardTopic(name));
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
    // Always shown, so there is no toggle state to keep in step with the topic
    // or with Refresh — which is where this pane's only two bugs came from.
    renderBoardSubscribers();
    if (!window.harness) {
      boardMessagesEl.textContent = "(not connected)";
      return;
    }
    try {
      const r = await window.harness.boardRead(topic);
      if (!r.found) {
        // Reached from a subscribed-but-unpublished row, or from a topic that
        // was evicted/purged since the list was drawn. Either way what is true
        // is "nothing is retained"; the subscribers pane above still says who
        // would receive a publish here.
        boardMessagesEl.textContent = "(nothing published to this topic)";
        return;
      }
      if (!r.msgs || r.msgs.length === 0) {
        boardMessagesEl.textContent = "(no messages)";
        return;
      }
      for (const m of r.msgs) {
        const card = document.createElement("div");
        card.className = "board-msg";
        // A message its author withdrew (agent retract). It reaches no agent
        // any more; this pane is the only place it still exists, so it is
        // marked and dimmed rather than hidden — hiding it would give the
        // operator the same blank the agents get, which defeats the point of
        // keeping it.
        if (m.retracted) card.classList.add("board-msg-retracted");

        const hdr = document.createElement("div");
        hdr.className = "board-msg-header";

        const seqSpan = document.createElement("span");
        seqSpan.className = "board-msg-seq";
        seqSpan.textContent = `#${m.seq}`;

        // inReplyTo is a decimal STRING (board seq exceeds 2^53); compare as
        // one and never Number() it. Shown only on replies, so a board where
        // nothing replies is not littered with re=0.
        const replySpan = document.createElement("span");
        replySpan.className = "board-msg-reply";
        if (m.inReplyTo && m.inReplyTo !== "0") {
          replySpan.textContent = `re=${m.inReplyTo}`;
        }

        const fromSpan = document.createElement("span");
        fromSpan.className = "board-msg-from";
        fromSpan.textContent = `from=${m.fromTask ? m.fromTask.slice(0, 8) : "-"}`;

        const hostSpan = document.createElement("span");
        hostSpan.className = "board-msg-host";
        hostSpan.textContent = `host=${m.fromHostname || "-"}`;

        const agentSpan = document.createElement("span");
        agentSpan.className = "board-msg-agent";
        agentSpan.textContent = `agent=${m.agentProfile || "-"}`;

        const timeSpan = document.createElement("span");
        timeSpan.className = "board-msg-time";
        timeSpan.textContent = m.receivedAtMs
          ? new Date(m.receivedAtMs).toISOString()
          : "-";

        // How many subscribers have been handed this message. Printed on every
        // row, zeros included: a message nobody has been given yet is exactly
        // what an operator reading a topic is looking for, and eliding 0/1
        // would make it look like the row does not report delivery at all.
        // The comparison happens in Go (cli.ShownTo) -- board seqs exceed
        // Number.MAX_SAFE_INTEGER, so doing it here would need BigInt and a
        // second implementation of the same rule.
        const shownSpan = document.createElement("span");
        const shownN = m.shownTo || 0;
        const shownTotal = m.shownToTotal || 0;
        shownSpan.className = shownN < shownTotal
          ? "board-msg-shown board-msg-shown-pending"
          : "board-msg-shown";
        shownSpan.textContent = `shown_to=${shownN}/${shownTotal}`;

        // Same rule as re= above: the badge appears only when it applies.
        const retractedSpan = document.createElement("span");
        retractedSpan.className = "board-msg-retracted-badge";
        if (m.retracted) {
          const when = m.retractedAtMs
            ? new Date(m.retractedAtMs).toISOString()
            : "-";
          retractedSpan.textContent = `RETRACTED ${when}`;
        }

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
        if (replySpan.textContent) hdr.appendChild(replySpan);
        hdr.appendChild(fromSpan);
        hdr.appendChild(hostSpan);
        hdr.appendChild(agentSpan);
        hdr.appendChild(timeSpan);
        hdr.appendChild(shownSpan);
        if (retractedSpan.textContent) hdr.appendChild(retractedSpan);
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

  // renderBoardSubscribers lists the tasks a publish to the open topic would
  // reach. An empty result is the finding, not an error: the topic is retained
  // and nothing would receive a publish to it.
  // headed() builds the pane's title line. The pane sits directly above the
  // message cards and is styled like them, so without a heading a subscriber
  // row is indistinguishable from a message row. Wording matches the TUI's
  // "subscribers of <topic> (N)" so the two surfaces read the same.
  function boardSubHeading(text) {
    const h = document.createElement("div");
    h.className = "board-sub-head";
    h.textContent = text;
    return h;
  }

  async function renderBoardSubscribers() {
    if (!boardSubscribersEl || !currentBoardTopic) return;
    boardSubscribersEl.innerHTML = "";
    if (!window.harness) {
      boardSubscribersEl.appendChild(boardSubHeading("subscribers"));
      boardSubscribersEl.appendChild(
        Object.assign(document.createElement("div"), { textContent: "(not connected)" }));
      return;
    }
    try {
      const rows = await window.harness.boardSubscribers(currentBoardTopic);
      const n = rows ? rows.length : 0;
      boardSubscribersEl.appendChild(
        boardSubHeading(`subscribers of ${currentBoardTopic} (${n})`));
      if (n === 0) {
        const empty = document.createElement("div");
        empty.className = "board-sub-empty";
        empty.textContent = "nobody subscribes \u2014 a publish here reaches no inbox";
        boardSubscribersEl.appendChild(empty);
        return;
      }
      for (const r of rows) {
        const line = document.createElement("div");
        line.className = "board-sub-row";
        const id = (r.taskId || "").slice(0, 8);
        // An empty hostname means registered but not yet attached; show it as
        // "-" rather than dropping the row.
        const host = r.hostname || "-";
        const agent = r.agentProfile || "-";
        // shown / pending per topic: how far the automatic injection path
        // has reached for this task, and how much sits above it. shown is a
        // board seq, delivered as a decimal STRING because those exceed
        // Number.MAX_SAFE_INTEGER -- render it, never arithmetic on it.
        const pats = (r.patterns && r.patterns.length)
          ? r.patterns.map((p) => `${p.name}(shown=${p.shown} pending=${p.pending})`).join(",")
          : "-";
        line.textContent = `\u2022 ${id}  host=${host}  agent=${agent}  topics=${pats}`;
        boardSubscribersEl.appendChild(line);
      }
    } catch (err) {
      boardSubscribersEl.appendChild(boardSubHeading("subscribers"));
      boardSubscribersEl.appendChild(
        Object.assign(document.createElement("div"), { textContent: `error: ${err.message}` }));
    }
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


  // ── Git panel ─────────────────────────────────────────────────────────────
  // The two pane structure the TUI's git modal uses: a row picker (working
  // tree, index, then the commits) over a diff view. Selecting a commit as the
  // BASE is the whole point — an agent that has committed shows nothing under
  // a plain diff, and which baseline is wanted changes with the situation.

  const gitTaskSelect   = document.getElementById("git-task-select");
  const gitRefreshBtn   = document.getElementById("git-refresh-btn");
  const gitBaseEl       = document.getElementById("git-base");
  const gitBaseResetBtn = document.getElementById("git-base-reset-btn");
  const gitRowsEl       = document.getElementById("git-rows");
  const gitNoteEl       = document.getElementById("git-note");
  const gitContentEl    = document.getElementById("git-content");
  const gitRepoEl       = document.getElementById("git-repo");
  const gitUpBtn        = document.getElementById("git-up-btn");
  const gitSubmoduleBox = document.getElementById("git-submodule");
  const gitBackBtn      = document.getElementById("git-back-btn");

  let gitBaseRev = "HEAD";
  let gitRows = [];        // {kind: "worktree"|"index"|"commit"|"subrepo", ...}
  let gitActiveIndex = 0;
  let gitStatusSummary = "";
  // gitSubrepoStack is the chain of nested repositories entered so far, each
  // element relative to the one before it. A LEVEL is one entry, not one path
  // segment: the runner reports "pkg/inner" as a single nested repo two
  // directories deep, and up from it is the worktree, not "pkg", where there is
  // no repository at all. gitSubmodule inlines a submodule's own changes.
  let gitSubrepoStack = [];
  let gitSubmodule = false;
  const gitSubrepo = () => gitSubrepoStack.join("/");
  // gitGeneration rises on every root change / refresh so a slow answer for a
  // root the operator has left cannot repaint the current one.
  let gitGeneration = 0;
  let gitCommits = [];
  let gitSubrepoList = [];
  // gitFileView names the file whose whole content is showing, "" for a diff.
  // gitLastContent is the query that produced the diff, so opening a file asks
  // for THE SAME SIDE and closing it puts the diff back.
  let gitFileView = "";
  let gitLastContent = null;

  // rebuildGitRows lays the picker out from whichever answers have arrived: the
  // two pseudo rows, then the commits, then the nested repositories. It is
  // called once per answer, so a slow one only adds its own rows late.
  function rebuildGitRows(wanted) {
    gitRows = [{ kind: "worktree" }, { kind: "index" }];
    for (const c of gitCommits) gitRows.push({ kind: "commit", commit: c });
    for (const sr of gitSubrepoList) gitRows.push({ kind: "subrepo", subrepo: sr });
    if (gitActiveIndex >= gitRows.length) gitActiveIndex = wanted < gitRows.length ? wanted : 0;
    renderGitRows();
  }

  // classifyGitLine mirrors cli.ClassifyGitLine (cli/git_query.go) exactly,
  // header checks first: "--- a/x" and "+++ b/x" start with the same bytes as
  // a deletion and an addition, and reading them as such miscolours every
  // file header in the diff.
  function classifyGitLine(line) {
    if (line.startsWith("diff --git ") || line.startsWith("diff --cc ")) return "gd-file";
    if (line.startsWith("--- ") || line.startsWith("+++ ")) return "gd-meta";
    if (line.startsWith("index ") || line.startsWith("new file mode ") ||
        line.startsWith("deleted file mode ") || line.startsWith("old mode ") ||
        line.startsWith("new mode ") || line.startsWith("similarity index ") ||
        line.startsWith("rename from ") || line.startsWith("rename to ") ||
        line.startsWith("Binary files ")) return "gd-meta";
    if (line.startsWith("@@")) return "gd-hunk";
    if (line.startsWith("+")) return "gd-add";
    if (line.startsWith("-")) return "gd-del";
    return "";
  }

  // diffFilePath / diffFilePathAt mirror cli.DiffFilePath / DiffFilePathAt.
  // The `+++` line is read rather than the `diff --git a/x b/x` header: the
  // header is `a/<p1> b/<p2>` with no delimiter that cannot also occur inside a
  // path, so it has no unambiguous parse.
  function diffFilePath(line) {
    if (!line.startsWith("+++ ")) return "";
    let p = line.slice(4);
    if (p.startsWith("b/")) p = p.slice(2);
    if (p === "" || p === "/dev/null" || p === "dev/null") return "";
    return p;
  }

  // The section is resolved first — the header at or above i, up to the next
  // one — because the `+++` sits BELOW the header a reader usually clicks.
  function diffFilePathAt(lines, i) {
    if (!lines.length) return "";
    if (i >= lines.length) i = lines.length - 1;
    if (i < 0) i = 0;
    let start = -1;
    for (let j = i; j >= 0; j--) {
      if (classifyGitLine(lines[j]) === "gd-file") { start = j; break; }
    }
    if (start < 0) return "";
    for (let j = start + 1; j < lines.length; j++) {
      if (classifyGitLine(lines[j]) === "gd-file") return "";
      if (lines[j].startsWith("+++ ")) return diffFilePath(lines[j]);
    }
    return "";
  }

  // renderGitText builds the diff from DOM nodes rather than an HTML string,
  // so a diff that happens to contain markup renders as the text it is.
  function renderGitText(text, clickableFiles) {
    gitContentEl.textContent = "";
    const lines = text.replace(/\n$/, "").split("\n");
    lines.forEach((line, i) => {
      const cls = classifyGitLine(line);
      if (!cls) {
        gitContentEl.appendChild(document.createTextNode(line + "\n"));
        return;
      }
      const span = document.createElement("span");
      span.className = cls;
      span.textContent = line + "\n";
      if (clickableFiles && cls === "gd-file") {
        const path = diffFilePathAt(lines, i);
        if (path) {
          span.classList.add("gd-file-link");
          span.title = "open " + path;
          span.addEventListener("click", () => openGitFile(path));
        }
      }
      gitContentEl.appendChild(span);
    });
  }

  // openGitFile shows one file's whole content, asking for THE SAME SIDE the
  // diff on screen was showing — the working tree for a worktree diff, the
  // staged blob for a staged one, that commit for a commit-to-commit diff or a
  // shown commit. Answering with a different side would be a different
  // question than the one being read.
  async function openGitFile(path) {
    const last = gitLastContent || {};
    const q = { path };
    if (last.kind === "show") {
      q.target = "rev";
      q.targetRev = last.baseRev || "";
    } else if (last.target === "rev") {
      q.target = "rev";
      q.targetRev = last.targetRev || "";
    } else if (last.target === "index") {
      q.target = "index";
    } else {
      q.target = "worktree";
    }
    try {
      const res = await gitQuery("file", q);
      gitFileView = path;
      renderGitText(res.text || "(empty file)", false);
      if (gitNoteEl) {
        gitNoteEl.textContent = res.truncated
          ? `${path} — truncated`
          : `${path} — whole file`;
      }
      if (gitBackBtn) gitBackBtn.hidden = false;
    } catch (err) {
      gitSetError(err.message);
    }
  }

  // gitLeaveFileView puts the diff back by re-issuing the query it came from.
  async function gitLeaveFileView() {
    if (!gitFileView) return;
    gitFileView = "";
    if (gitBackBtn) gitBackBtn.hidden = true;
    if (gitNoteEl) gitNoteEl.textContent = "";
    await openGitRow(gitActiveIndex);
  }

  function gitSetError(msg) {
    gitContentEl.textContent = "";
    const span = document.createElement("span");
    span.className = "gd-err";
    span.textContent = msg;
    gitContentEl.appendChild(span);
  }

  function renderGitTaskSelect(tasks) {
    if (!gitTaskSelect) return;
    const prev = gitTaskSelect.value;
    gitTaskSelect.innerHTML = "";
    const placeholder = document.createElement("option");
    placeholder.value = "";
    placeholder.textContent = "(select task)";
    gitTaskSelect.appendChild(placeholder);
    // Every task, not only the live ones: a finished task still answers
    // through its retained harness/<id> branch (server/git_query.go), which
    // is exactly what the file picker cannot do.
    for (const t of tasks || []) {
      const opt = document.createElement("option");
      opt.value = t.id;
      opt.textContent = `${(t.id || "").slice(0, 12)}  ${t.status}  ${t.repoPath}`;
      gitTaskSelect.appendChild(opt);
    }
    if (prev) gitTaskSelect.value = prev;
    updateGitButtons();
  }

  function updateGitButtons() {
    const hasTask = !!(gitTaskSelect && gitTaskSelect.value);
    if (gitRefreshBtn) gitRefreshBtn.disabled = !hasTask;
    if (gitBaseResetBtn) gitBaseResetBtn.disabled = !hasTask || gitBaseRev === "HEAD";
  }

  function gitSetBase(rev) {
    gitBaseRev = rev || "HEAD";
    if (gitBaseEl) gitBaseEl.textContent = gitBaseRev.slice(0, 12);
    if (gitRepoEl) gitRepoEl.textContent = gitSubrepo() || "(root)";
    if (gitUpBtn) gitUpBtn.disabled = gitSubrepoStack.length === 0;
    if (gitSubmoduleBox) gitSubmoduleBox.checked = gitSubmodule;
    updateGitButtons();
  }

  // gitLeaveSubrepo goes back up one level, to the parent repository.
  async function gitLeaveSubrepo() {
    if (!gitSubrepoStack.length) return;
    gitSubrepoStack.pop();
    gitSetBase("HEAD");
    await refreshGit(false);
  }

  // Every query goes through here so the panel's current root and submodule
  // setting are attached in ONE place — a caller that forgot would silently
  // answer about the outer repository.
  async function gitQuery(kind, opts) {
    const taskID = gitTaskSelect ? gitTaskSelect.value : "";
    if (!taskID) throw new Error("no task selected");
    if (!window.harness) throw new Error("not connected");
    const merged = Object.assign({ subrepo: gitSubrepo(), submodule: gitSubmodule }, opts || {});
    const res = await window.harness.gitQuery(taskID, kind, merged);
    if (!res.ok) throw new Error(res.stderr || res.status);
    return res;
  }

  function renderGitRows() {
    if (!gitRowsEl) return;
    gitRowsEl.innerHTML = "";
    gitRows.forEach((row, i) => {
      const el = document.createElement("div");
      el.className = "git-row" + (i === gitActiveIndex ? " is-active" : "");
      const label = document.createElement("span");
      label.className = "git-row-label";
      const meta = document.createElement("span");
      meta.className = "git-row-meta";
      if (row.kind === "worktree") {
        label.textContent = "[WORKTREE]";
        meta.textContent = gitStatusSummary || "uncommitted";
      } else if (row.kind === "index") {
        label.textContent = "[INDEX]";
        meta.textContent = "staged";
      } else if (row.kind === "subrepo") {
        label.textContent = "[REPO]";
        meta.textContent = row.subrepo;
      } else {
        label.textContent = row.commit.short;
        meta.textContent = `${new Date(row.commit.when * 1000).toLocaleString()}  ${row.commit.author}`;
      }
      el.appendChild(label);
      el.appendChild(meta);
      if (row.kind === "commit") {
        const subj = document.createElement("span");
        subj.className = "git-row-subject";
        subj.textContent = row.commit.subject;
        el.appendChild(subj);
        const baseBtn = document.createElement("button");
        baseBtn.type = "button";
        baseBtn.className = "git-base-btn";
        baseBtn.textContent = "set base";
        baseBtn.title = "diff the working tree against this commit";
        baseBtn.addEventListener("click", (e) => {
          e.stopPropagation();   // setting the base is not selecting the row
          gitSetBase(row.commit.sha);
          renderGitRows();
        });
        el.appendChild(baseBtn);
      }
      el.addEventListener("click", () => openGitRow(i));
      gitRowsEl.appendChild(el);
    });
  }

  async function openGitRow(i) {
    gitActiveIndex = i;
    renderGitRows();
    const row = gitRows[i];
    if (!row) return;
    if (row.kind === "subrepo") {
      // A [REPO] row is a destination, not content.
      gitSubrepoStack.push(row.subrepo);
      gitSetBase("HEAD");
      await refreshGit(false);
      return;
    }
    if (gitNoteEl) gitNoteEl.textContent = "";
    try {
      let res;
      if (row.kind === "commit") {
        gitLastContent = { kind: "show", baseRev: row.commit.sha };
        res = await gitQuery("show", { baseRev: row.commit.sha });
      } else {
        gitLastContent = { kind: "diff", baseRev: gitBaseRev, target: row.kind };
        res = await gitQuery("diff", { baseRev: gitBaseRev, target: row.kind });
      }
      gitFileView = "";
      if (gitBackBtn) gitBackBtn.hidden = true;
      if (!res.text) {
        gitContentEl.textContent = "(no difference)";
      } else {
        renderGitText(res.text, true);
      }
      if (res.truncated && gitNoteEl) gitNoteEl.textContent = "truncated — raise maxBytes to see more";
    } catch (err) {
      gitSetError(err.message);
    }
  }

  async function refreshGit(preserveSelection) {
    if (!gitTaskSelect || !gitTaskSelect.value) {
      gitRows = [];
      renderGitRows();
      gitContentEl.textContent = "(select a task)";
      return;
    }
    const wanted = preserveSelection ? gitActiveIndex : 0;
    // The four answers are rendered AS THEY ARRIVE rather than joined: the
    // subrepos walk enumerates the whole tree and on a Windows runner takes
    // seconds, and awaiting all of them together made every open cost the
    // slowest one. The generation counter drops answers for a root the
    // operator has already navigated away from.
    const gen = ++gitGeneration;
    gitCommits = [];
    gitSubrepoList = [];
    gitRows = [{ kind: "worktree" }, { kind: "index" }];
    gitActiveIndex = wanted < gitRows.length ? wanted : 0;
    renderGitRows();

    const fail = (err) => { if (gen === gitGeneration) gitSetError(err.message); };

    gitQuery("log", {}).then(log => {
      if (gen !== gitGeneration) return;
      gitCommits = log.commits || [];
      if (gitNoteEl) {
        gitNoteEl.textContent = log.truncated ? `commit list truncated at ${gitCommits.length}` : "";
      }
      rebuildGitRows(wanted);
    }).catch(fail);

    gitQuery("status", {}).then(status => {
      if (gen !== gitGeneration) return;
      let changed = 0, untracked = 0;
      for (const e of status.entries || []) {
        if (e.xy === "??") untracked++; else changed++;
      }
      gitStatusSummary = (changed === 0 && untracked === 0)
        ? "clean"
        : (untracked === 0 ? `${changed} changed` : `${changed} changed, ${untracked} untracked`);
      renderGitRows();
    }).catch(fail);

    gitQuery("subrepos", {}).then(subs => {
      if (gen !== gitGeneration) return;
      gitSubrepoList = subs.subrepos || [];
      rebuildGitRows(wanted);
    }).catch(fail);

    // The content the operator came for goes out immediately, not behind the
    // row list.
    await openGitRow(gitActiveIndex);
  }

  // gitShowStatusListing renders the porcelain listing into the content pane.
  // It is the only place untracked files are visible: they appear in no diff.
  async function gitShowStatusListing() {
    try {
      const res = await gitQuery("status", {});
      if (!(res.entries || []).length) {
        gitContentEl.textContent = "(nothing uncommitted)";
        return;
      }
      renderGitText(res.entries.map(e => `${e.xy} ${e.path}`).join("\n"), false);
    } catch (err) {
      gitSetError(err.message);
    }
  }

  if (gitTaskSelect) {
    gitTaskSelect.addEventListener("change", () => {
      // A different task keeps none of the previous one's root or baseline.
      gitSubrepoStack = [];
      gitSubmodule = false;
      gitSetBase("HEAD");
      refreshGit(false);
    });
  }
  if (gitRefreshBtn) gitRefreshBtn.addEventListener("click", () => refreshGit(true));
  if (gitUpBtn) gitUpBtn.addEventListener("click", () => gitLeaveSubrepo());
  if (gitBackBtn) gitBackBtn.addEventListener("click", () => gitLeaveFileView());
  if (gitSubmoduleBox) {
    gitSubmoduleBox.addEventListener("change", () => {
      gitSubmodule = gitSubmoduleBox.checked;
      openGitRow(gitActiveIndex);
    });
  }
  if (gitBaseResetBtn) {
    gitBaseResetBtn.addEventListener("click", () => {
      gitSetBase("HEAD");
      renderGitRows();
      openGitRow(gitActiveIndex);
    });
  }
  tabbar.addEventListener("click", (e) => {
    const btn = e.target.closest(".tab-btn");
    if (btn && btn.dataset.tab === "git" && gitTaskSelect && gitTaskSelect.value && !gitRows.length) {
      refreshGit(false);
    }
  });

  // openGitTabFor is the cmdline's entry point: switch to the tab, point it at
  // a task, and run one query there rather than dumping a diff into the
  // command output where it cannot be scrolled.
  window.__openGitTabFor = async (taskID, sub, opts) => {
    const btn = tabbar.querySelector('.tab-btn[data-tab="git"]');
    if (btn) btn.click();
    if (gitTaskSelect) gitTaskSelect.value = taskID;
    // One entry, not one per path segment: a path typed on the command line
    // names one repository, so up from it is the worktree.
    gitSubrepoStack = opts.subrepo ? [opts.subrepo] : [];
    gitSubmodule = !!opts.submodule;
    gitSetBase(opts.baseRev || "HEAD");
    await refreshGit(false);
    if (sub === "status") {
      await gitShowStatusListing();
      return;
    }
    if (sub === "file") {
      await openGitFileFromCmd(opts);
      return;
    }
    if (sub === "subrepos") {
      const res = await gitQuery("subrepos", {});
      gitContentEl.textContent = (res.subrepos || []).length
        ? (res.subrepos || []).join("\n")
        : "(no nested repositories)";
      return;
    }
    if (sub === "show") {
      try {
        gitLastContent = { kind: "show", baseRev: opts.baseRev };
        const res = await gitQuery("show", { baseRev: opts.baseRev, path: opts.path });
        renderGitText(res.text || "(no difference)", true);
      } catch (err) {
        gitSetError(err.message);
      }
      return;
    }
    if (sub === "diff") {
      const target = opts.staged ? "index" : (opts.targetRev ? "rev" : "worktree");
      try {
        gitLastContent = { kind: "diff", baseRev: opts.baseRev, targetRev: opts.targetRev, target };
        const res = await gitQuery("diff", {
          baseRev: opts.baseRev, targetRev: opts.targetRev, target, path: opts.path,
        });
        renderGitText(res.text || "(no difference)", true);
      } catch (err) {
        gitSetError(err.message);
      }
    }
  };

  // openGitFileFromCmd is the cmdline route into the file view; the side comes
  // from the flags rather than from whatever diff happens to be on screen.
  async function openGitFileFromCmd(opts) {
    const q = { path: opts.path };
    if (opts.rev) { q.target = "rev"; q.targetRev = opts.rev; }
    else if (opts.staged) { q.target = "index"; }
    else { q.target = "worktree"; }
    try {
      const res = await gitQuery("file", q);
      gitFileView = opts.path;
      renderGitText(res.text || "(empty file)", false);
      if (gitNoteEl) gitNoteEl.textContent = `${opts.path} — whole file`;
      if (gitBackBtn) gitBackBtn.hidden = false;
    } catch (err) {
      gitSetError(err.message);
    }
  }

  // Keep the git task dropdown in step with the snapshot poll.
  window.__renderGitTaskSelect = renderGitTaskSelect;

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
    let agents = (r.agentProfiles && r.agentProfiles.length > 0) ? r.agentProfiles.join(",") : (r.agentBin || "-");
    // "-" means the runner advertised no bin at all; "-+skills" would be noise.
    if (r.skillsInjected && agents !== "-") agents += "+skills";
    return `  ${pad(r.status, 8)} host=${r.hostname || "-"}  tasks=${r.tasks}/${r.maxTasks}  agents=${agents}  roots=${roots}`;
  }).join("\n");
}

function pad(s, n) {
  s = String(s);
  return s.length >= n ? s : s + " ".repeat(n - s.length);
}

// agentLabel renders a TASK's agent identity the way `harness-cli ls` rows and
// the TUI table do: the resolved profile name, plus a "+skills" marker when the
// runner it was assigned to declares it injects the harness skill + inbox hook.
// An empty profile yields "" so each caller keeps its own placeholder.
//
// Mirrors cli.agentStr (Go) and tui.agentDescriptor (Go) — one grammar, one
// renderer per runtime. The marker reads off the TASK, never the runners array:
// a confined caller is served zero runners, so a runner-side lookup is blank for
// exactly the readers this marker exists for.
function agentLabel(profile, skillsInjected) {
  if (!profile) return "";
  return skillsInjected ? profile + "+skills" : profile;
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
// runGitCmd parses `git <task> {log|diff|show|status} ...` with the same
// grammar harness-cli and the TUI cmdline use, and hands the result to the Git
// tab. The pathspec is split off at "--" BEFORE flags are read, matching the
// other two surfaces, and the revisions are counted the way git counts them.
async function runGitCmd(rest) {
  if (rest.length < 2) {
    throw new Error("git: usage: git <task-id> {log | diff | show | status} [...]");
  }
  const taskID = rest[0];
  const sub = rest[1];
  let args = rest.slice(2);
  let path = "";
  const sepIdx = args.indexOf("--");
  if (sepIdx >= 0) {
    path = args.slice(sepIdx + 1).join(" ");
    args = args.slice(0, sepIdx);
  }

  let staged = false, submodule = false, subrepo = "", rev = "";
  const pos = [];
  for (let i = 0; i < args.length; i++) {
    const a = args[i];
    if (a === "--staged" || a === "--cached") { staged = true; continue; }
    if (a === "--submodule") { submodule = true; continue; }
    if (a === "--rev") { i++; rev = args[i] || ""; continue; }
    if (a.startsWith("--rev=")) { rev = a.slice("--rev=".length); continue; }
    if (a === "--subrepo") { i++; subrepo = args[i] || ""; continue; }
    if (a.startsWith("--subrepo=")) { subrepo = a.slice("--subrepo=".length); continue; }
    if (a === "--max" || a === "--max-bytes") { i++; continue; }   // caps are the runner's job
    if (a.startsWith("--")) throw new Error(`git ${sub}: unknown flag ${a}`);
    pos.push(a);
  }

  let baseRev = "", targetRev = "";
  switch (sub) {
    case "log":
    case "show":
      if (pos.length > 1) throw new Error(`git ${sub}: at most one revision (got ${pos.length})`);
      baseRev = pos[0] || "";
      break;
    case "diff":
      if (pos.length > 2) throw new Error(`git diff: at most two revisions (got ${pos.length})`);
      if (pos.length === 2) {
        if (staged) throw new Error("git diff: --staged names the index as the right-hand side, so a second revision has nowhere to go");
        baseRev = pos[0];
        targetRev = pos[1];
      } else {
        baseRev = pos[0] || "";
      }
      break;
    case "status":
    case "subrepos":
      if (pos.length) throw new Error(`git ${sub}: takes no revision (got ${pos[0]})`);
      break;
    case "file":
      // The path may arrive as a positional or after --, so one lifted out of
      // a diff header works either way.
      if (pos.length > 1) throw new Error(`git file: one path (got ${pos.length})`);
      if (pos.length === 1) {
        if (path) throw new Error(`git file: path given twice (${pos[0]} and ${path})`);
        path = pos[0];
      }
      if (!path) throw new Error("git file: usage: git <task> file [--staged | --rev REV] <path>");
      break;
    default:
      throw new Error(`git: unknown sub-verb ${sub} (log | diff | show | status | subrepos | file)`);
  }

  if (!window.__openGitTabFor) throw new Error("git: panel not ready");
  await window.__openGitTabFor(taskID, sub, { baseRev, targetRev, path, staged, submodule, subrepo, rev });
  return `git ${sub}: shown in the Git tab`;
}

async function runFileCmd(rest) {
  if (rest.length === 0) {
    throw new Error("file: sub-verb required (ls | delete | push | pull | mkdir | new)");
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
    case "mkdir":
      return fileMkdirCmd(args);
    case "new":
      return fileNewCmd(args);
    case "edit":
      return fileEditCmd(args);
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

async function fileMkdirCmd(args) {
  let parents = false;
  const pos = [];
  for (const a of args) {
    if (a === "-p" || a === "--parents") { parents = true; continue; }
    pos.push(a);
  }
  if (pos.length !== 2) {
    throw new Error("usage: file mkdir [-p] <task-id> <worktree-rel-dir>");
  }
  const [taskID, rel] = pos;
  await window.harness.fileMkdir(taskID, rel, parents);
  return `mkdir ok: ${rel}`;
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
    const res = await pushBytesWithPrompts(taskID, remoteRel, buf, file.name, fp.onProgress);
    return res.msg;
  } finally {
    fp.end();
  }
}

async function fileNewCmd(args) {
  if (args.length !== 2) {
    throw new Error("usage: file new <task-id> <worktree-rel-dst>");
  }
  const [taskID, remoteRel] = args;
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

async function fileEditCmd(args) {
  if (args.length !== 2) {
    throw new Error("usage: file edit <task-id> <worktree-rel-path>");
  }
  return editRemoteFile(args[0], args[1]);
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
async function pushBytesWithPrompts(taskID, remoteRel, buf, displayName, onProgress) {
  let force = false;
  let parents = false;
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
// The config is escaped for "<" because it carries a file path: a file named
// with a literal </script> would otherwise close the tag it is embedded in.
function previewShimSource(pin, rel) {
  const cfg = JSON.stringify({ origin: pin.origin, host: pin.host, port: pin.port, rel: rel || "" })
    .replace(/</g, "\\u003c");
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
      preview: "html-file",
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
let topoZoomHint = null; // ditto — names the modifier the wheel handler requires

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
    // Plain wheel belongs to the page. This panel is tall and sits mid-tab, so
    // an unconditional preventDefault() made it a wall: every wheel event that
    // crossed it became a zoom and the scroll underneath it never happened.
    // Requiring a modifier also gets pinch-to-zoom for free — a trackpad pinch
    // arrives as a wheel event with ctrlKey set.
    if (!e.ctrlKey && !e.metaKey) return;
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

  const hint = document.createElement("div");
  hint.className = "ct-zoom-hint";
  hint.textContent = "Ctrl+ホイールで拡大縮小 · ドラッグで移動";
  topoZoomHint = hint;
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
// fwdFanIndex returns how many EARLIER forwards share this one's endpoint pair,
// so several forwards between the same two nodes fan out instead of stacking
// into one thick line. Order follows the server's forward_id, which is a
// monotonic counter, so the fan is stable across polls.
function fwdFanIndex(fwd, forwards) {
  const key = f => `${f.origin_cid}|${f.task}`;
  const k = key(fwd);
  return forwards.filter(f => key(f) === k && f.forward_id < fwd.forward_id).length;
}

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

// repoBasename is the last path segment of a repo path, for labels too narrow
// to carry the whole thing. Handles both separators: runners on Windows report
// backslash paths, and this runs in the operator's browser, not on that host.
function repoBasename(p) {
  const cut = Math.max(p.lastIndexOf("/"), p.lastIndexOf("\\"));
  return cut >= 0 ? p.slice(cut + 1) || p : p;
}

// renderTaskTreeGraph draws the creator hierarchy as a node-link diagram into
// #task-tree-graph, or hides the panel when the toggle is off.
//
// It is a PICTURE, not a second task list: the list below keeps its own order,
// its filter and its action sheets. An earlier version indented the list rows
// instead, which forced the filter off (a filtered hierarchy is disconnected
// fragments) and still did not read as a tree, because a task card is tall
// enough that a parent and its child never sit close enough for the gutter to
// connect them.
//
// Every position comes from the wasm side (cli.TaskTreeLayout): `col` and
// `depth` are unitless grid slots, multiplied here by the pixel spacing. This
// file decides what a node LOOKS like and nothing about where it goes.
function renderTaskTreeGraph(nodes, tasks, statusColor, onSelect) {
  const host = document.getElementById("task-tree-graph");
  if (!host) return;
  // nodes null/empty is how the caller says "the toggle is off": the state
  // lives with the chip, and this stays a renderer that is handed its data —
  // the same shape renderConnTopology uses.
  if (!nodes) {
    host.hidden = true;
    host.innerHTML = "";
    return;
  }
  host.hidden = false;
  if (nodes.length === 0) {
    host.innerHTML = '<span class="ct-empty">(no tasks)</span>';
    return;
  }

  const COL_W = 132;   // horizontal slot; wide enough for an 8-hex label
  const ROW_H = 88;   // three label lines per node (id / status+agent / repo)
  // Asymmetric vertical padding: a node's labels sit ABOVE it (id) and BELOW
  // it (status, repo), and the lowest one reaches R+26. Equal padding clipped
  // the bottom row's repo against the panel edge.
  const PAD_X = 70, PAD_TOP = 34, PAD_BOTTOM = 46;
  const R = 9;

  const byID = new Map((tasks || []).map((t) => [t.id, t]));
  const pos = new Map();
  let maxCol = 0, maxDepth = 0;
  for (const n of nodes) {
    pos.set(n.id, { x: PAD_X + n.col * COL_W, y: PAD_TOP + n.depth * ROW_H });
    if (n.col > maxCol) maxCol = n.col;
    if (n.depth > maxDepth) maxDepth = n.depth;
  }
  const W = PAD_X * 2 + maxCol * COL_W;
  const H = PAD_TOP + PAD_BOTTOM + maxDepth * ROW_H;

  host.innerHTML = "";
  const svg = svgEl("svg", {
    viewBox: `0 0 ${W} ${H}`,
    width: W,
    height: H,
    class: "task-tree-svg",
  });

  // Edges first so the nodes paint over them.
  for (const n of nodes) {
    if (!n.parent || !pos.has(n.parent)) continue;
    const a = pos.get(n.parent), b = pos.get(n.id);
    const midY = (a.y + b.y) / 2;
    svg.appendChild(svgEl("path", {
      class: "tt-edge",
      // Elbow rather than a straight diagonal: with several children the
      // straight lines fan into a starburst that is hard to trace back.
      d: `M ${a.x} ${a.y + R} V ${midY} H ${b.x} V ${b.y - R}`,
    }));
  }

  for (const n of nodes) {
    const p = pos.get(n.id);
    const t = byID.get(n.id);
    const g = svgEl("g", {
      class: "tt-node" + (n.orphan ? " tt-orphan" : ""),
      transform: `translate(${p.x},${p.y})`,
    });
    const circle = svgEl("circle", { r: R, class: "tt-dot" });
    // The palette comes in from the caller rather than being restated here:
    // the list's status dots use the same function, and two copies of a colour
    // table drift the moment one status is added.
    if (t && statusColor) circle.setAttribute("fill", statusColor(t.status));
    g.appendChild(circle);

    const label = svgEl("text", { class: "tt-label", y: -R - 6 });
    label.textContent = (n.orphan ? "† " : "") + n.id.slice(0, 8);
    g.appendChild(label);

    const sub = svgEl("text", { class: "tt-sub", y: R + 14 });
    sub.textContent = t ? `${t.status}${t.agentProfile ? " " + agentLabel(t.agentProfile, t.skillsInjected) : ""}` : "(gone)";
    g.appendChild(sub);

    // Which repo the task belongs to. The fleet serves several, and without
    // this a subtree is a set of ids with no idea what they are working on.
    // Basename only — a full path is far wider than a node's slot, and the
    // tooltip already carries it.
    if (t && t.repoPath) {
        const repo = svgEl("text", { class: "tt-repo", y: R + 26 });
        repo.textContent = repoBasename(t.repoPath);
        g.appendChild(repo);
    }

    const title = svgEl("title", {});
    title.textContent = t
      ? `${n.id}\n${t.status}  ${t.repoPath || ""}\n${n.orphan ? "作成者が一覧に居ない（prune 済み / スコープ外）" : ""}`
      : n.id;
    g.appendChild(title);

    // The diagram is a navigator: clicking a node reveals that task in the
    // list below. The list's own rules (which sheet may be open, what the
    // filters currently hide) belong to the list, so this hands the id over
    // rather than reaching into it.
    if (onSelect) g.addEventListener("click", () => onSelect(n.id));
    svg.appendChild(g);
  }
  host.appendChild(svg);
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
function renderConnTopology(conns, tasks, forwards) {
  const host = document.getElementById("conn-topology");
  if (!host) return;
  tasks = tasks || [];
  forwards = forwards || [];

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

  // Recorded while drawing, consumed by the port-forward pass after the loops:
  // a forward joins its owning client's LEAF to its task's RECT, and neither
  // coordinate survives the loop that computes it.
  const leafPos = new Map(); // conn cid -> {x, y}
  const taskPos = new Map(); // task id  -> {x, y}

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
      leafPos.set(conn.cid, { x: lx, y: ly });

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
          taskPos.set(tk.id, { x: tx, y: ty });
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

  // --- Port-forward edges ---
  // One curve per registered forward, joining its owning client's leaf to its
  // task's rect. Appended after every spoke/node/task so it paints on top.
  // Design: docs/superpowers/specs/2026-07-30-webui-topology-port-forward-edges-design.md
  const MIN_CLEAR = SERVER_R + 12; // keep the curve off the server hub
  for (const fwd of forwards) {
    const clientEnd = leafPos.get(fwd.origin_cid);
    const taskEnd = taskPos.get(fwd.task);
    // Either end can be absent for a beat: a registration outlives its
    // connection until the server tears that connection down, and the task rect
    // is only drawn for Running/Detached. Draw nothing rather than guess — the
    // forward is still listed in the panel.
    if (!clientEnd || !taskEnd) continue;
    // -L: the CLIENT accepts, so it is the tail and the hue comes from its role.
    // -R: the RUNNER accepts, so the task end is the tail and the hue is the
    // constant runner orange (a task is not a role, so the rect's amber must not
    // colour the edge).
    const isRemote = fwd.dir === "-R";
    const tail = isRemote ? taskEnd : clientEnd;
    const head = isRemote ? clientEnd : taskEnd;
    const role = isRemote ? "runner" : ((conns.find(c => c.cid === fwd.origin_cid) || {}).role || "unspecified");
    if (tail.x === head.x && tail.y === head.y) continue; // degenerate: no spike

    const mx = (tail.x + head.x) / 2, my = (tail.y + head.y) / 2;
    const dx = head.x - tail.x, dy = head.y - tail.y;
    const len = Math.hypot(dx, dy) || 1;
    const nx = -dy / len, ny = dx / len; // unit perpendicular to the chord

    // Annulus rule: the curve's own midpoint is only HALF way to the control
    // point (B(0.5) = ¼P₀ + ½C + ¼P₂), so a control-point offset of `off` moves
    // the curve by off/2. Pick the SIDE that lands inside the ring and clear of
    // the hub — which side that is depends on the pair, so try both rather than
    // always bowing outward.
    const spread = 2 * MIN_CLEAR + 40 * (fwdFanIndex(fwd, forwards) || 0);
    let off = spread;
    let best = null;
    for (const sign of [1, -1]) {
      const qx = mx + sign * nx * (spread / 2), qy = my + sign * ny * (spread / 2);
      const rad = Math.hypot(qx - cx, qy - cy);
      if (rad >= MIN_CLEAR && rad <= R2) { off = sign * spread; best = rad; break; }
    }
    if (best === null) {
      // Neither side fits the annulus (endpoints far out and close together):
      // bow toward the centre, which always has room.
      const inR = Math.hypot(mx + nx - cx, my + ny - cy);
      const outR = Math.hypot(mx - nx - cx, my - ny - cy);
      off = (inR < outR ? 1 : -1) * spread;
    }
    const ctlx = mx + nx * off, ctly = my + ny * off;

    // Pull both ends back off the node they touch so the stroke and the
    // arrowhead do not sit inside the circle/rect.
    const backOff = (px, py, fromx, fromy, by) => {
      const vx = px - fromx, vy = py - fromy, l = Math.hypot(vx, vy) || 1;
      return { x: px - (vx / l) * by, y: py - (vy / l) * by };
    };
    const p0 = backOff(tail.x, tail.y, ctlx, ctly, 10);
    const p2 = backOff(head.x, head.y, ctlx, ctly, 11);

    const d = `M ${p0.x} ${p0.y} Q ${ctlx} ${ctly} ${p2.x} ${p2.y}`;
    // Dark casing first, coloured stroke over it — see .ct-forward-casing.
    svg.appendChild(svgEl("path", { class: "ct-forward-casing", d }));
    svg.appendChild(svgEl("path", {
      class: `ct-forward from-${role}`,
      d,
      "data-forward": String(fwd.forward_id),
    }));

    // Arrowhead at the head end, oriented along the curve's end tangent
    // (the derivative of a quadratic at t=1 is 2(P₂ - C)).
    const tgx = p2.x - ctlx, tgy = p2.y - ctly, tl = Math.hypot(tgx, tgy) || 1;
    const ux = tgx / tl, uy = tgy / tl;
    const A = 8, Wd = 3.6;
    svg.appendChild(svgEl("polygon", {
      class: `ct-forward-head from-${role}`,
      points: [
        `${p2.x},${p2.y}`,
        `${p2.x - ux * A - uy * Wd},${p2.y - uy * A + ux * Wd}`,
        `${p2.x - ux * A + uy * Wd},${p2.y - uy * A - ux * Wd}`,
      ].join(" "),
    }));

    // Label at the curve's midpoint. The spec's format was
    // "<dir> <bind_port> → <target>", but `spec` is a display string and
    // splitting it to pull the bind port out would be exactly the convention
    // dependency origin_cid exists to avoid — so the whole spec string is used.
    const qx = 0.25 * p0.x + 0.5 * ctlx + 0.25 * p2.x;
    const qy = 0.25 * p0.y + 0.5 * ctly + 0.25 * p2.y;
    svg.appendChild(Object.assign(svgEl("text", {
      class: "ct-forward-label", x: qx, y: qy - 3,
    }), { textContent: `${fwd.dir} ${fwd.spec}` }));
  }

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
  if (topoZoomHint) host.appendChild(topoZoomHint); // ditto

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
