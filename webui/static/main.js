"use strict";

const SERVER_CID = location.protocol.startsWith("https")
  ? `wss:${location.hostname}:${location.port || 443}-*`
  : `ws:${location.hostname}:${location.port || 80}-*`;

const POLL_INTERVAL_MS = 5000;
// Mobile mode's period. refreshSnapshot refetches the WHOLE list every tick —
// runners, tasks, conns, forwards, execs in one ListResultBody — which measured
// ~12KB on a fleet of 46 tasks, i.e. ~8MB/h with nothing happening at all. Fine
// on wifi, expensive on a phone's data plan.
const POLL_INTERVAL_MOBILE_MS = 60000;

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

  // Mobile mode toggle (header). Opt-in rather than a navigator.connection
  // sniff: whether the bytes are worth spending is the user's call, not the
  // link type's, and this page runs in a non-secure context (plain ws) where
  // that API's availability is not something to lean on.
  //
  // What it does NOT delay: task and runner changes still land immediately —
  // harness_onTaskEvent kicks a refresh per event. What it does delay is the
  // conns / forwards / execs panels, which have no event subscription in wasm
  // and so are only as current as the last poll.
  //
  // Wired here, before the wasm load, so the persisted state is on the button
  // from the first paint. The poll it governs is armed much further down and
  // subscribes via onChange.
  const MOBILE_KEY = "harness.mobileMode";
  const mobileBtn = document.getElementById("mobile-btn");
  const mobileMode = (() => {
    let on = localStorage.getItem(MOBILE_KEY) === "on";
    const listeners = [];

    function reflect() {
      if (!mobileBtn) return;
      mobileBtn.setAttribute("aria-pressed", on ? "true" : "false");
      mobileBtn.classList.toggle("is-active", on);
      const sec = Math.round((on ? POLL_INTERVAL_MOBILE_MS : POLL_INTERVAL_MS) / 1000);
      mobileBtn.title = on
        ? `モバイルモード中：一覧の自動更新は${sec}秒ごと（タップで解除）`
        : "モバイルモード：一覧の自動更新を間引いてデータ通信を節約";
    }

    function toggle() {
      on = !on;
      localStorage.setItem(MOBILE_KEY, on ? "on" : "off");
      reflect();
      for (const fn of listeners) {
        try { fn(); } catch (e) { console.error("mobileMode listener", e); }
      }
    }

    if (mobileBtn) mobileBtn.addEventListener("click", toggle);
    reflect();
    return {
      isOn: () => on,
      intervalMs: () => (on ? POLL_INTERVAL_MOBILE_MS : POLL_INTERVAL_MS),
      onChange: (fn) => listeners.push(fn),
      toggle,
    };
  })();

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
  // The delegation bound a FRESH spawn carries: what the new task may hand to
  // the tasks it creates. A resume re-grants it only from a typed
  // --delegate-caps / --delegate-scope, never from this panel.
  const spawnDeleg = newScopeState();
  // Mirrors protocol.IsPTYKind / IsSessionKind (runner/protocol/task_kind.go).
  // Two kinds are sessions; only one drives a TERMINAL, so every site has to
  // say which question it is asking — a bare `kind === "Interactive"` reads as
  // an oversight the next reader will "fix" by adding "Stream" to it.
  const isPTYKind = (t) => !!t && t.kind === "Interactive";
  // The other session kind. Spelled out for the same reason isPTYKind is: a
  // bare kind === "Stream" reads as an oversight the next reader will "fix".
  const isStreamKind = (t) => !!t && t.kind === "Stream";
  const isSessionKind = (t) => isPTYKind(t) || isStreamKind(t);

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
  function sessionReq({ repo = "", task = "", host = "", runner = "", ip = "", agent = "",
                        agentArgs = [], resumeTaskId = "", resumeConversation = false,
                        eventStream = false, resumeExact = false,
                        caps = null, scope = null, scopeFor = null,
                        delegateCaps = null, delegateScope = null }) {
    // An exact resume takes the task's recorded agent, args, caps and scope,
    // and the server refuses a request that carries any of them, so none of
    // the Compose state rides along.
    if (resumeExact) {
      agent = "";
      agentArgs = [];
    }
    const req = {
      repo, task, host, agent,
      agentArgs,
      resumeTaskId,
      resumeConversation,
      // TaskKind_Stream: structured events instead of a PTY. The wasm side
      // declines to mount the xterm for it; the chat panel attaches instead.
      eventStream,
      // A typed --caps / --scope / --scope-for wins over the Compose chips.
      // That is the declaration's own ladder -- flag beats surface context --
      // and this surface had no parameter for them at all, so the three
      // parsed and were dropped while the chips decided the authority.
      caps: caps !== null ? caps : spawnCaps,
      scope: scope !== null ? scope : spawnScope,
      scopeFor: scopeFor !== null ? scopeFor : overrideSpecsFrom("spawn"),
      // One checkbox gates BOTH halves of the resume re-grant: silently
      // applying the Compose scope picker's leftover state to a resumed
      // task would be the exact invisible rewrite scope_present exists to
      // prevent.
      resumeCapsOverride: resumeTaskId && !resumeExact ? applyCapsOnResume : false,
      scopePresent: resumeTaskId && !resumeExact ? applyCapsOnResume : false,
      // The bound: a typed flag wins over the panel, and only a typed flag
      // re-grants it on a resume — the caps-on-resume checkbox names caps and
      // scope, and widening what it silently rewrites is item 26's failure.
      delegateCaps: delegateCaps !== null ? delegateCaps : spawnDeleg.caps,
      delegateScope: delegateScope !== null ? delegateScope : scopeStateSpec(spawnDeleg),
      delegateCapsPresent: !!resumeTaskId && !resumeExact && delegateCaps !== null,
      delegateScopePresent: !!resumeTaskId && !resumeExact && delegateScope !== null,
      resumeExact,
    };
    if (runner) req.runner = runner;
    if (ip) req.ip = ip;
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
  const agentArgs    = document.getElementById("agent-args-input");
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
  // 権限で強調: the capability whose holders the tree marks (0 = none), and a
  // task the caps audit just pointed at, pulsed for a moment.
  const taskTreeBit = document.getElementById("task-tree-bit");
  const capsAuditDetails = document.getElementById("caps-audit");
  let treeFocusId = "";
  let treeFocusTimer = 0;
  // Reach mode: the tree shows what ONE task reaches through one selector
  // entry — a capability it exercises, `view` or `delegate` — answered by
  // harness.reachOf, the same authority functions `caps reach` prints. It
  // replaces 権限で強調 while on and survives the snapshot poll's redraw,
  // because the poll calls drawTaskTree and drawTaskTree asks again.
  let reachMode = null; // {taskId, sel} | null
  const reachSel = document.getElementById("task-tree-reach-sel");
  const reachBanner = document.getElementById("task-tree-reach");
  // What-if: the diff of a change NOT sent (harness.whatIf, the rows `caps
  // what-if` prints) over the tree — changed nodes ringed, a spawn's phantom
  // child dashed — with the command that would make it, to copy. It keeps the
  // answer it was given across the poll's redraws: that answer is about the
  // listing it was asked over.
  let whatIfState = null; // {result} | null
  const whatIfPanel = document.getElementById("task-tree-whatif");
  // drawTaskTree is the ONE call into renderTaskTreeGraph, so the chip, the
  // snapshot poll, the selector and the audit cannot draw it differently.
  function drawTaskTree() {
    fillTaskTreeBit();
    // A task that left the listing (pruned, or now out of sight) has no reach
    // to draw; say so rather than leave every node dimmed with no reason.
    if (reachMode && !(lastTasks || []).some((t) => t.id === reachMode.taskId)) {
      appendCmdOutput(`caps reach ${reachMode.taskId.slice(0, 8)}: the task left the listing; reach mode closed`);
      reachMode = null;
    }
    let reach = null;
    if (reachMode && window.harness && typeof window.harness.reachOf === "function") {
      reach = window.harness.reachOf(reachMode);
    }
    syncReachControls(reach);
    const wi = whatIfState && whatIfState.result;
    if (whatIfPanel) whatIfPanel.hidden = !wi;
    // 権限で強調 draws nothing under a what-if (markBit is 0 below), so it is
    // put away as reach mode puts it away, rather than left to do nothing.
    if (taskTreeBit && wi) taskTreeBit.hidden = true;
    renderTaskTreeGraph(taskTreeMode ? ((wi && wi.tree) || lastTaskTree) : null,
      (wi && wi.grantEdges) || lastGrantEdges, lastTasks,
      taskStatusColor, revealTaskInList,
      { markBit: reach || wi ? 0 : Number(taskTreeBit && taskTreeBit.value) || 0,
        // Not the reach subject: the banner names it, and the ring this would
        // draw is 権限で強調's holder ring, which read as a reach of its own.
        focusId: treeFocusId,
        reach, reachSel: reachMode && reachMode.sel,
        whatIf: wi && !wi.error ? new Set(wi.diffs.map((d) => d.id)) : null });
  }
  // showWhatIf puts a harness.whatIf answer on the tree: the panel is built
  // once per answer, so a poll's redraw does not reset its copy button.
  // reopen, when given, returns to the dialog the answer came from with its
  // edits intact, so looking before applying costs no re-entry. what names
  // the change previewed ("caps set" / "spawn").
  function showWhatIf(result, reopen, what) {
    reachMode = null;
    whatIfState = { result };
    if (whatIfPanel) {
      const head = document.createElement("div");
      head.className = "tt-whatif-head";
      const title = document.createElement("span");
      title.textContent = `プレビュー: ${what || "caps what-if"}（未送信）`;
      const close = document.createElement("button");
      close.type = "button";
      close.className = "task-chip";
      close.textContent = "✕";
      close.title = "Esc";
      close.addEventListener("click", exitWhatIf);
      head.append(title);
      if (reopen) {
        const back = document.createElement("button");
        back.type = "button";
        back.className = "task-chip";
        back.textContent = "↩ ダイアログに戻る";
        back.title = "入力したままのダイアログを開き直す";
        back.addEventListener("click", reopen);
        head.append(back);
      }
      head.append(close);
      const parts = [head];
      if (result.error) {
        const err = document.createElement("div");
        err.className = "task-err";
        err.textContent = result.error;
        parts.push(err);
      } else {
        const pre = document.createElement("pre");
        pre.className = "tt-whatif-diff";
        const lines = [];
        for (const d of result.diffs) {
          lines.push("  " + d.head);
          for (const b of d.bits) lines.push("      " + b.line);
        }
        pre.textContent = lines.length ? lines.join("\n") : "  nothing changes";
        parts.push(pre);
        if (result.diffs.length) {
          // Shown, never sent: the operator runs it (or, for a spawn, the
          // creator does).
          const row = document.createElement("div");
          row.className = "tt-whatif-cmd";
          const label = document.createElement("span");
          label.textContent = result.runBy ? `command (run by ${result.runBy.slice(0, 8)}):` : "command:";
          const code = document.createElement("code");
          code.textContent = result.command;
          const copy = document.createElement("button");
          copy.type = "button";
          copy.className = "task-chip";
          copy.textContent = "📋";
          copy.title = "コピー（送信はしない）";
          copy.addEventListener("click", async () => {
            try {
              await navigator.clipboard.writeText(result.command);
              copy.textContent = "✓";
            } catch (e) {
              appendCmdOutput("copy failed: " + (e && e.message ? e.message : e));
            }
          });
          row.append(label, code, copy);
          parts.push(row);
        }
      }
      whatIfPanel.replaceChildren(...parts);
    }
    if (!taskTreeMode) {
      taskTreeMode = true;
      taskTreeChip.classList.add("is-active");
    }
    drawTaskTree();
    if (whatIfPanel) whatIfPanel.scrollIntoView({ block: "nearest" });
  }
  function exitWhatIf() {
    whatIfState = null;
    drawTaskTree();
  }
  function syncReachControls(reach) {
    if (taskTreeBit) taskTreeBit.hidden = !!reach;
    if (reachSel) reachSel.hidden = !reach;
    if (!reachBanner) return;
    reachBanner.hidden = !reach;
    if (!reach) return;
    const names = reach.bits.map((b) => b.name);
    // The entries follow the task's caps, which a caps set can change while
    // the mode is on: compare the names, not just their count.
    if (reachSel && (reachSel.dataset.for !== reachMode.taskId || reachSel.dataset.names !== names.join(","))) {
      reachSel.replaceChildren(...names.map((n) => {
        const o = document.createElement("option");
        o.value = n;
        o.textContent = n;
        return o;
      }));
      reachSel.dataset.for = reachMode.taskId;
      reachSel.dataset.names = names.join(",");
    }
    if (!names.includes(reachMode.sel)) {
      // The bit was taken away: fall back to the first entry and redraw with it.
      reachMode.sel = names[0];
      queueMicrotask(drawTaskTree);
    }
    if (reachSel) reachSel.value = reachMode.sel;
    const all = reachMode.sel === "view" ? reach.seeAll : reach.actAll;
    const text = document.createElement("span");
    text.textContent = `caps reach ${reachMode.taskId.slice(0, 8)} ${reachMode.sel}: ` + (all
      ? "global (+ tasks you cannot list)"
      : (reachMode.sel === "view" ? "" : `act ${reach.act.length}  `) +
        (reach.seeAll ? "see global (+ tasks you cannot list)" : `see ${reachMode.sel === "view" ? "" : "+"}${reach.see.length}`));
    const close = document.createElement("button");
    close.type = "button";
    close.className = "task-chip";
    close.textContent = "✕";
    close.title = "Esc";
    close.addEventListener("click", exitReachMode);
    reachBanner.replaceChildren(text, close);
  }
  function enterReachMode(id) {
    if (!window.harness || typeof window.harness.reachOf !== "function") return;
    const first = window.harness.reachOf({ taskId: id, sel: "view" }).bits[0];
    reachMode = { taskId: id, sel: first ? first.name : "view" };
    whatIfState = null;
    if (!taskTreeMode) {
      taskTreeMode = true;
      taskTreeChip.classList.add("is-active");
    }
    drawTaskTree();
    const node = document.querySelector(`#task-tree-graph .tt-node[data-task-id="${id}"]`);
    if (node) node.scrollIntoView({ block: "center", inline: "center" });
  }
  function exitReachMode() {
    reachMode = null;
    drawTaskTree();
  }
  if (reachSel) reachSel.addEventListener("change", () => {
    if (reachMode) { reachMode.sel = reachSel.value; drawTaskTree(); }
  });
  document.addEventListener("keydown", (e) => {
    if (e.key !== "Escape" || (!reachMode && !whatIfState) || document.querySelector("dialog[open]")) return;
    // Escape typed into a field or a terminal belongs to that field or to the
    // program in the terminal (vim), not to the tree.
    const el = e.target;
    if (el && el.closest && el.closest("input, textarea, select, [contenteditable], .xterm")) return;
    reachMode = null;
    exitWhatIf();
  });
  // The selector's bits come from the bridge (verb.AuditOrder), filled once
  // the wasm is up.
  function fillTaskTreeBit() {
    if (!taskTreeBit || taskTreeBit.options.length > 1) return;
    if (!window.harness || typeof window.harness.capsAuditOrder !== "function") return;
    for (const c of window.harness.capsAuditOrder()) {
      const o = document.createElement("option");
      o.value = String(c.bit);
      o.dataset.name = c.name;
      o.textContent = `${c.name} (${c.tier})`;
      taskTreeBit.append(o);
    }
  }
  if (taskTreeBit) taskTreeBit.addEventListener("change", drawTaskTree);
  // showTaskInTree is where a caps audit holder leads: the tree, marked for
  // that holder's capability, with the node brought into view and pulsed.
  // Re-grant is then that node's own task sheet. The sheet's 🌳 item comes
  // here without a capName, which leaves the mark as it was.
  function showTaskInTree(id, capName) {
    // The audit leads to a capability's holders, which reach mode or a
    // what-if would draw over; leave them first.
    reachMode = null;
    whatIfState = null;
    fillTaskTreeBit();
    const opt = taskTreeBit && [...taskTreeBit.options].find((o) => o.dataset.name === capName);
    if (opt) taskTreeBit.value = opt.value;
    if (!taskTreeMode) {
      taskTreeMode = true;
      taskTreeChip.classList.add("is-active");
    }
    treeFocusId = id;
    drawTaskTree();
    const node = document.querySelector(`#task-tree-graph .tt-node[data-task-id="${id}"]`);
    if (node) node.scrollIntoView({ block: "center", inline: "center" });
    clearTimeout(treeFocusTimer);
    treeFocusTimer = setTimeout(() => { treeFocusId = ""; drawTaskTree(); }, 2000);
    return !!node;
  }
  let lastGrantEdges = [];         // id-named grants between tasks (cli.BuildTaskGrants)
  let lastForwards = [];           // latest snapshot; `forward ls` reads this, no second RPC
  const expandedExecCommands = new Set(); // exec ids whose command is shown whole
  let lastExecs = [];              // latest snapshot; `exec tap` reads this, no second RPC
  let lastConns = [];              // same, for the trsf panel's answerer picker
  for (const [key, btn] of Object.entries(taskChips)) {
    btn.addEventListener("click", () => {
      taskStatusFilter = key;
      for (const b of Object.values(taskChips)) b.classList.toggle("is-active", b === btn);
      renderTaskList(lastTasks);
    });
  }
  // revealTaskInList scrolls to one task's sheet, widening the filters first if
  // that task is not currently listed.
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
      // Reach mode and a what-if are modes OF the tree: closing the tree ends
      // them.
      if (!taskTreeMode) { reachMode = null; whatIfState = null; }
      drawTaskTree();
    });
  }
  taskFilterInput.addEventListener("input", () => renderTaskList(lastTasks));

  // Grid selection. A live session (either kind) is INCLUDED in the grid by
  // default; gridExcluded holds the ids the user unchecked. Keyed by id so it
  // survives the snapshot poll's task-list re-render, and a newly-created
  // session is auto-included. The global "グリッド表示" button opens the grid of
  // all currently-included sessions (activity-desc).
  const gridExcluded = new Set();
  const gridShowBtn = document.getElementById("grid-show-btn");
  const gridAllOn   = document.getElementById("grid-all-on");
  const gridAllOff  = document.getElementById("grid-all-off");
  const liveSessionTasks = () =>
    (lastTasks || []).filter(
      // isSessionKind: a PTY session tiles as a terminal, a stream session as
      // its transcript (openSessionGrid).
      (t) => isSessionKind(t) && (t.status === "Running" || t.status === "Detached"));
  gridShowBtn.addEventListener("click", () => {
    const ids = liveSessionTasks()
      .filter((t) => !gridExcluded.has(t.id))
      .sort((a, b) => taskActivityMs(b) - taskActivityMs(a))
      .map((t) => t.id);
    if (!ids.length) { appendCmdOutput("grid: no sessions selected", true); return; }
    openSessionGrid(ids, "all");
  });
  gridAllOn.addEventListener("click", () => { gridExcluded.clear(); renderTaskList(lastTasks); });
  gridAllOff.addEventListener("click", () => {
    for (const t of liveSessionTasks()) gridExcluded.add(t.id);
    renderTaskList(lastTasks);
  });

  // currentAgentArgs returns the shell-tokenised args from the input box.
  // Reused by submit (cmdline) and Open buttons so the user only edits one field.
  const currentAgentArgs = () => {
    if (!agentArgs) return [];
    const raw = agentArgs.value.trim();
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
  // Set instead of previewSource when the open preview is a document fetched
  // through the forward rather than pulled out of the worktree. The two are
  // mutually exclusive, and which one is set decides where Reload goes, which
  // task the pin is opened against, and whether Edit is offered at all —
  // there is no file behind an http preview to edit.
  //   { taskID, host, port, path }
  let previewHttpSource = null;
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
    lastTaskTree = snap.taskTree || [];
    lastGrantEdges = snap.grantEdges || [];
    renderTaskList(snap.tasks);
    // After renderTaskList, so a task line's jump finds the row it opens.
    renderRunnerRows(runnerList, sortedRunners, {
      tasks: snap.tasks, statusColor: taskStatusColor, repoTail, onTask: revealTaskInList,
    });
    drawTaskTree();
    renderFileTaskSelect(snap.tasks);
    if (window.__renderGitTaskSelect) window.__renderGitTaskSelect(snap.tasks);
    if (window.__refreshCapsAudit && capsAuditDetails && capsAuditDetails.open) {
      window.__refreshCapsAudit();
    }
    renderRawTaskSelect(snap.tasks);
    // Connection topology — rides the same ~5s poll (spec decision #3:
    // no separate event subscription in wasm). snap.conns may be absent if
    // the server doesn't have the list_conns capability yet; guard with [].
    const conns = snap.conns || [];
    const allTasks = snap.tasks || [];
    lastConns = conns; // mirrors lastForwards: the trsf panel builds its
                       // answerer picker from this without a second RPC
    renderConnTopology(conns, allTasks, snap.forwards || []);
    renderConnList(conns, allTasks);
    renderTrsfTargets(conns);
    renderForwardList(snap.forwards || []);
    renderExecList(snap.execs || []);
    renderIdleWatcherList(snap.idle_watchers || []);
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

  // Open as page. Deliberately NOT gated on a live raw pane: this does not use
  // one. It reads the same three fields Connect reads — the target — and opens
  // its own forward, so requiring a connection first would force an unused byte
  // stream to exist just to justify the button's position.
  const rawPageBtn = document.getElementById("raw-page-btn");
  const rawPagePath = document.getElementById("raw-page-path");
  rawPageBtn?.addEventListener("click", () => {
    const task = rawTaskSelect.value;
    const host = document.getElementById("raw-host-input").value.trim();
    const port = parseInt(document.getElementById("raw-port-input").value, 10);
    if (!task || !host || !(port > 0 && port < 65536)) {
      appendCmdOutput("open as page: task, host and port are required");
      return;
    }
    openHttpPreview(task, host, port, normalizePagePath(rawPagePath && rawPagePath.value));
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
      const wrap = document.createElement("div");
      wrap.className = "forward-entry";
      const row = document.createElement("div");
      row.className = "forward-row";
      const taskShort = f.task ? f.task.slice(0, 8) + "…" : "-";
      for (const text of [`#${f.forward_id}`, f.dir, taskShort, f.spec, f.origin]) {
        const cell = document.createElement("span");
        cell.className = "forward-cell";
        cell.textContent = text;
        row.appendChild(cell);
      }
      const tap = document.createElement("button");
      tap.type = "button";
      tap.className = "btn-secondary";
      tap.textContent = "tap";
      tap.addEventListener("click", () => toggleForwardTap(f, wrap, tap));
      row.appendChild(tap);
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
      wrap.appendChild(row);

      // The traffic line is a SECOND line inside the entry, not a sixth
      // column: five columns already fill 390px, and the string comes straight
      // from cli.PortForwardTrafficLine so this reads exactly like `forward ls`.
      const traffic = document.createElement("div");
      traffic.className = "forward-traffic";
      traffic.textContent = f.traffic || "";
      wrap.appendChild(traffic);

      // A tap panel already open for this forward survives the poll that
      // rebuilt the list: without this, every 5s refresh would close it.
      const live = openTaps.get(`fwd:${f.forward_id}`);
      if (live) {
        wrap.appendChild(live.panel);
        tap.textContent = "untap";
        tap.classList.add("btn-active");
      }
      host.appendChild(wrap);
    }
  }

  // openTaps maps a tap key (`fwd:<id>` / `exec:<id>`) -> { panel, close }. A
  // tap is a live subscription,
  // so it outlives the 5s snapshot poll that rebuilds the rows around it — the
  // panel element is re-attached rather than recreated.
  const openTaps = new Map();

  // findForwardEntry locates a rendered forward row so the command input can
  // drive the same panel the row's own button does.
  function findForwardEntry(forwardID) {
    const host = document.getElementById("forward-list");
    if (!host) return null;
    for (const wrap of host.querySelectorAll(".forward-entry")) {
      const first = wrap.querySelector(".forward-cell");
      if (first && first.textContent === `#${forwardID}`) {
        const buttons = wrap.querySelectorAll(".forward-row > button");
        return { wrap, button: buttons[0] };
      }
    }
    return null;
  }

  // findExecEntry locates a rendered exec row so the command input can drive
  // the same panel the row's own button does.
  function findExecEntry(execID) {
    const host = document.getElementById("exec-list");
    if (!host) return null;
    for (const wrap of host.querySelectorAll(".exec-entry")) {
      const first = wrap.querySelector(".forward-cell");
      if (first && first.textContent === `#${execID}`) {
        const buttons = wrap.querySelectorAll(".exec-row > button");
        return { wrap, button: buttons[0] };
      }
    }
    return null;
  }

  // toggleTapPanel starts or stops one tap and shows its lines in a panel under
  // the row. start(append, onEnd) opens the bridge subscription and returns its
  // handle. Lines arrive already rendered by the Go renderer over the wasm
  // bridge, so the panel prints what harness-cli prints.
  function toggleTapPanel(key, wrap, btn, start) {
    const existing = openTaps.get(key);
    if (existing) {
      existing.close();
      openTaps.delete(key);
      existing.panel.remove();
      btn.textContent = "tap";
      btn.classList.remove("btn-active");
      return;
    }
    const panel = document.createElement("pre");
    panel.className = "forward-tap-panel";
    wrap.appendChild(panel);

    let stopped = false;
    const append = (lines) => {
      if (stopped) return;
      // Pinned to the bottom unless the reader has scrolled up: a tap on a
      // busy subject would otherwise make its own scrollback unreadable.
      const atBottom = panel.scrollHeight - panel.scrollTop - panel.clientHeight < 24;
      panel.textContent += lines.join("\n") + "\n";
      const lineCount = panel.textContent.split("\n").length;
      if (lineCount > 4000) {
        panel.textContent = panel.textContent.split("\n").slice(-2000).join("\n");
      }
      if (atBottom) panel.scrollTop = panel.scrollHeight;
    };
    let closeFn = () => { stopped = true; };
    try {
      const handle = start(append, (err) => {
        append([err ? `-- tap ended: ${err} --` : "-- tap ended --"]);
      });
      if (handle && typeof handle.close === "function") {
        closeFn = () => { stopped = true; handle.close(); };
      }
    } catch (err) {
      append([`-- tap failed: ${err.message} --`]);
    }
    openTaps.set(key, { panel, close: closeFn });
    btn.textContent = "untap";
    btn.classList.add("btn-active");
  }

  function toggleForwardTap(f, wrap, btn, opts) {
    // The row's button taps both directions whole; the command line can
    // narrow either. They were discarded here once, so `forward tap 7 --dir
    // to-target` parsed and tapped both.
    const tapOpts = { dir: (opts && opts.dir) || "both" };
    if (opts && opts.maxBytes) tapOpts.maxBytes = opts.maxBytes;
    toggleTapPanel(`fwd:${f.forward_id}`, wrap, btn,
      (append, onEnd) => window.harness.forwardTap(f.forward_id, tapOpts, append, onEnd));
  }

  function toggleExecTap(e, wrap, btn, opts) {
    const tapOpts = { chan: (opts && opts.chan) || "all" };
    if (opts && opts.maxBytes) tapOpts.maxBytes = opts.maxBytes;
    toggleTapPanel(`exec:${e.exec_id}`, wrap, btn,
      (append, onEnd) => window.harness.execTap(e.exec_id, tapOpts, append, onEnd));
  }

  // renderExecList draws one row per running exec (every exec visible to this
  // operator on the server, not just ones this WebUI started), each with a kill
  // button. The sibling of renderForwardList in every respect — `exec ls` /
  // `exec kill` is the same list-and-kill pair as `forward ls` / `forward kill`
  // against the same kind of shared registry — so it shares that function's DOM
  // shape and its CSS, and the two must keep agreeing.
  //
  // The task row's `execs=N` says HOW MANY are running; this is the one WebUI
  // surface that says WHICH, whose they are, and lets one be stopped.
  function renderExecList(execs) {
    lastExecs = execs || [];
    const host = document.getElementById("exec-list");
    if (!host) return;
    host.textContent = "";
    if (!execs.length) {
      const empty = document.createElement("div");
      empty.className = "forward-list-empty";
      empty.textContent = "実行中の exec はありません";
      host.appendChild(empty);
      return;
    }
    const now = Date.now();
    for (const e of execs) {
      const wrap = document.createElement("div");
      wrap.className = "forward-entry exec-entry";
      const row = document.createElement("div");
      row.className = "exec-row";
      const taskShort = e.task ? e.task.slice(0, 8) + "…" : "-";
      // Formatted per render, not per snapshot: the poll redraws this list, so
      // the age ticks. Whole seconds — a sub-second figure on a 2s poll would
      // be precision the number does not have.
      const age = e.started_unix_ms
        ? `${Math.max(0, Math.round((now - e.started_unix_ms) / 1000))}s`
        : "-";
      for (const text of [`#${e.exec_id}`, taskShort, age, e.origin]) {
        const cell = document.createElement("span");
        cell.className = "forward-cell";
        cell.textContent = text;
        row.appendChild(cell);
      }
      // A command line has no bound — an editor's remote terminal is ~2.8 KB —
      // so it is one ellipsised line in the row's leftover width, the whole of
      // it in the tooltip, and a click shows it wrapped. Remembered by id:
      // the poll rebuilds this list, and an expanded row must survive that.
      const cmd = document.createElement("span");
      cmd.className = "forward-cell exec-command";
      if (expandedExecCommands.has(e.exec_id)) cmd.classList.add("expanded");
      cmd.textContent = e.command;
      cmd.title = e.command;
      cmd.addEventListener("click", () => {
        if (expandedExecCommands.has(e.exec_id)) {
          expandedExecCommands.delete(e.exec_id);
          cmd.classList.remove("expanded");
        } else {
          expandedExecCommands.add(e.exec_id);
          cmd.classList.add("expanded");
        }
      });
      row.appendChild(cmd);
      const tap = document.createElement("button");
      tap.type = "button";
      tap.className = "btn-secondary";
      tap.textContent = "tap";
      tap.addEventListener("click", () => toggleExecTap(e, wrap, tap));
      row.appendChild(tap);
      const kill = document.createElement("button");
      kill.type = "button";
      kill.className = "btn-danger";
      kill.textContent = "kill";
      kill.addEventListener("click", async () => {
        // Confirmed for the modal's reason: the registry is shared, so this row
        // may be another operator's build — or the long-lived bootstrap holding
        // somebody's editor session open, now that `ssh host cmd` maps here.
        if (!window.confirm(`Kill exec #${e.exec_id} (${e.command})?`)) return;
        kill.disabled = true;
        try {
          await window.harness.execRunKill(e.exec_id);
          appendCmdOutput(`killed exec #${e.exec_id}`);
          refreshSnapshot();
        } catch (err) {
          appendCmdOutput(`exec kill error: ${err.message}`);
          kill.disabled = false;
        }
      });
      row.appendChild(kill);
      wrap.appendChild(row);

      // The traffic line is the exact string `exec ls` prints
      // (cli.ExecRunTrafficLine over the bridge), under the row as a forward's
      // is, because five columns already fill 390px.
      const traffic = document.createElement("div");
      traffic.className = "forward-traffic";
      traffic.textContent = e.traffic || "";
      wrap.appendChild(traffic);

      // A tap panel already open for this exec survives the poll that rebuilt
      // the list: without this, every refresh would close it.
      const live = openTaps.get(`exec:${e.exec_id}`);
      if (live) {
        wrap.appendChild(live.panel);
        tap.textContent = "untap";
        tap.classList.add("btn-active");
      }
      host.appendChild(wrap);
    }
  }

  // renderIdleWatcherList draws one row per armed await-idle watcher this
  // operator can see, each with a kill button — renderExecList's shape and
  // CSS, because `session await-idle ls` / `kill` is the same list-and-kill
  // pair. It is the answer to "did I arm one on that task?".
  function renderIdleWatcherList(watchers) {
    const host = document.getElementById("await-idle-list");
    if (!host) return;
    host.textContent = "";
    if (!watchers.length) {
      const empty = document.createElement("div");
      empty.className = "forward-list-empty";
      empty.textContent = "armed な watcher はありません";
      host.appendChild(empty);
      return;
    }
    const now = Date.now();
    for (const w of watchers) {
      const row = document.createElement("div");
      row.className = "exec-row";
      const taskShort = w.task ? w.task.slice(0, 8) + "…" : "-";
      const age = w.armed_unix_ms
        ? `${Math.max(0, Math.round((now - w.armed_unix_ms) / 1000))}s`
        : "-";
      const sink = w.sink === "board" ? `board ${w.topic}` : w.sink;
      for (const text of [`#${w.watcher_id}`, taskShort, sink, watcherWaitsFor(w), age, `by ${w.by}`]) {
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
        // Confirmed: the operator sees every agent's watchers, so this row
        // may be someone else's insurance.
        if (!window.confirm(`Kill ${w.condition === "progress" ? "await-progress" : "await-idle"} watcher #${w.watcher_id} on ${taskShort}?`)) return;
        kill.disabled = true;
        try {
          await window.harness.awaitIdleKill(w.watcher_id);
          appendCmdOutput(`killed await-idle watcher #${w.watcher_id}`);
          refreshSnapshot();
        } catch (err) {
          appendCmdOutput(`await-idle kill error: ${err.message}`);
          kill.disabled = false;
        }
      });
      row.appendChild(kill);
      host.appendChild(row);
    }
  }

  await refreshSnapshot();

  // The interval handle is held because the period is not a constant: mobile
  // mode changes it and a hidden tab drops it. setInterval bakes its period in
  // at arm time, so "change the interval" is clear + set, not assignment.
  let pollTimer = null;
  function armPoll() {
    if (pollTimer !== null) { clearInterval(pollTimer); pollTimer = null; }
    // A hidden tab paints nothing, so every poll it runs spends bytes on a
    // frame nobody sees. Browsers already throttle background timers to about
    // one tick a minute; this removes the traffic rather than slowing it.
    if (document.hidden) return;
    pollTimer = setInterval(refreshSnapshot, mobileMode.intervalMs());
  }
  armPoll();
  mobileMode.onChange(armPoll);
  document.addEventListener("visibilitychange", () => {
    armPoll();
    // Coming back from an unbounded hide: repaint once now instead of showing
    // rows up to a whole period stale while the first tick is pending.
    if (!document.hidden) refreshSnapshot();
  });

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

  // openHttpPreview fetches ONE document through a forward to host:port and
  // renders it in the same modal a file preview uses. This is the other way in:
  // loadPreview shows a file that is in the worktree, this shows what a server
  // inside the task answers with. A phone has no local listener, so this is the
  // only way to see a task's dev server from one.
  //
  // The document goes over httpFetch, which opens and closes its own forward
  // for the one request. The PIN — the longer-lived reach the rendered page's
  // own fetch() calls use — is opened afterwards by showHtmlPreview, from the
  // value dropped into filePreviewApi below. Opening a pin here as well would
  // put two registrations in `forward ls` for one preview.
  async function openHttpPreview(taskID, host, port, path, preferredMode) {
    const pin = parsePinnedTarget(`${host}:${port}`);
    if (!pin) {
      openFilePreview(`${host}:${port}`, 0, null, "preview: unusable target");
      return;
    }
    const label = previewHttpLabel(pin, path);
    // Same progress row a file pull uses: a page that is slow to answer
    // otherwise leaves the operator looking at a UI that has simply stopped.
    const fp = beginFileProgress(label);
    try {
      const res = await window.harness.httpFetch(taskID, pin.host, pin.port,
        { method: "GET", path, headers: "", body: null });
      const bytes = new Uint8Array(res.body);
      const ct = contentTypeOf(res.headers);
      previewSource = null;
      previewHttpSource = { taskID, host: pin.host, port: pin.port, path };
      // Pre-fill the pin rather than making the operator retype it: naming
      // this target is how they got here, and the rendered page's relative
      // fetches resolve against this same origin. They can still change it —
      // that path re-fetches the document from the new target.
      filePreviewApi.value = `${pin.host}:${pin.port}`;
      if (!isHtmlContentType(ct)) {
        // Not a page. Show the bytes as text and say why, because a blank
        // frame is indistinguishable from a server that answered with nothing.
        filePreviewHtml = null;
        const text = new TextDecoder("utf-8", { fatal: false }).decode(bytes);
        const pre = document.createElement("pre");
        pre.textContent = text;
        openFilePreview(label, bytes.byteLength, pre,
          `not HTML (${ct || "no Content-Type"}) — shown as text`);
        showPreviewCopy({ text });
        showPreviewReload();
        return;
      }
      filePreviewHtml = {
        rel: label, size: bytes.byteLength, bytes,
        mode: preferredMode === "source" ? "source" : "render",
      };
      showHtmlPreview();
      // Notes go after showHtmlPreview: it rebuilds the body, so anything
      // appended before it would be wiped.
      if (res.status >= 400) {
        appendPreviewNote(`HTTP ${res.status} ${res.statusText} — showing what the server returned`);
      }
      if (res.truncated) {
        appendPreviewNote("the response hit the 8 MiB cap and was cut short");
      }
    } catch (e) {
      previewHttpSource = null;
      openFilePreview(label, 0, null, `preview error: ${e.message}`);
    } finally {
      fp.end();
    }
  }

  // Reload re-pulls the open preview. The file is usually being written by an
  // agent while it is on screen, so the bytes the modal opened with go stale
  // within seconds.
  filePreviewReload.addEventListener("click", () => {
    const mode = filePreviewHtml && filePreviewHtml.mode;
    if (previewHttpSource) {
      const { taskID, host, port, path } = previewHttpSource;
      openHttpPreview(taskID, host, port, path, mode);
      return;
    }
    if (!previewSource) return;
    const { taskID, rel, name } = previewSource;
    loadPreview(taskID, rel, name, mode);
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
    previewHttpSource = null;
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
    // The rendered-HTML controls belong to the iframe, and showHtmlPreview is
    // the only thing that builds one — so it is the only thing that turns them
    // back on. Clearing them HERE rather than in each non-HTML renderer is
    // what stops them being left over: an http preview whose far end stopped
    // answering with HTML re-renders as text through this function, and
    // without this it kept an API pin and a "View source" toggle belonging to
    // a frame that is gone. Visibility only — filePreviewApi.value is the
    // operator's, and every rebuild reads it back.
    filePreviewToggle.hidden = true;
    filePreviewApi.hidden = true;
    filePreviewModalsLabel.hidden = true;
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
        // The task is the one the preview was opened against, which is NOT
        // always the file picker's: a document fetched through the forward
        // came from the raw pane's task, and the picker may be pointing
        // somewhere else entirely by now.
        const pinTask = previewHttpSource ? previewHttpSource.taskID : fileTaskSelect.value;
        window.harness.previewPinOpen(PREVIEW_PIN_KEY, pinTask, previewPin.host, previewPin.port)
          .then((id) => {
            previewPinForwardID = id;
            // The reach the operator just granted should be listed NOW, not up
            // to one poll later — the registration exists to be seen.
            refreshSnapshot();
          })
          .catch((e) => {
            previewPin = null;
            appendPreviewNote(`preview: could not register the forward (${e.message}) — the page has no network`);
          });
      }
      iframe.srcdoc = previewPin
        ? injectPreviewShim(text, previewShimSource(previewPin, rel, previewHttpSource ? "http" : "html-file"))
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
    // HTML is text: offer Edit in both the rendered and the source view — but
    // only for a FILE. An http preview's `rel` is a URL, and handing that to
    // the editor would open a path that does not exist in the worktree.
    if (!previewHttpSource) showPreviewEdit(rel);
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
    if (!filePreviewHtml || filePreviewHtml.mode !== "render") return;
    // For an http preview the target IS where the document came from, so a new
    // target means a new document. Re-rendering the bytes already in hand would
    // show one server's page wired to another server's API.
    if (previewHttpSource) {
      const pin = parsePinnedTarget(filePreviewApi.value);
      if (!pin) return;
      openHttpPreview(previewHttpSource.taskID, pin.host, pin.port, previewHttpSource.path, "render");
      return;
    }
    rebuildPreview("render");
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

  // flashButtonLabel swaps a button's label for a moment, then restores it.
  // The timer hangs off the element so two buttons flashing at once cannot
  // cancel each other — which a single module-level timer did when this was
  // specific to the file preview's Copy button.
  function flashButtonLabel(btn, label, restore) {
    if (!btn) return;
    btn.textContent = label;
    if (btn._flashTimer) clearTimeout(btn._flashTimer);
    btn._flashTimer = setTimeout(() => {
      if (!btn.hidden) btn.textContent = restore;
    }, 1800);
  }

  function flashCopyLabel(label) {
    flashButtonLabel(filePreviewCopy, label, "Copy");
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
    if (previewHttpSource) {
      // Reload re-issues the request. The refetch toggle stays hidden: it
      // means "re-pull the file before re-rendering", and an http preview has
      // no cached file to go stale — its Reload already goes to the far end.
      filePreviewReload.hidden = false;
      return;
    }
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
      // A progress write changes the row's text, so it refreshes — but it is
      // routine, so it prints no banner (taskEventBanner).
      const line = taskEventBanner(evt.line);
      if (line !== null) appendCmdOutput(`[${new Date().toISOString()}] ${line}`);
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

  // makeExecSink buffers one of an exec's two streams into whole lines and
  // marks each with its stream number.
  //
  // The command output pane is plain text (cmdOutput.textContent), so there is
  // no per-line colour to distinguish stdout from stderr — and merging them
  // would throw away, at the very last hop, the property this verb exists to
  // provide. "1| " / "2| " is the same marking the TUI uses, so an operator
  // reading either surface reads the same thing.
  //
  // TextDecoder with stream:true because a chunk boundary can land inside a
  // multi-byte character; decoding each chunk independently would corrupt it.
  const makeExecSink = (prefix) => {
    const dec = new TextDecoder();
    let buf = "";
    return {
      write(chunk) {
        buf += dec.decode(chunk, { stream: true });
        let i;
        while ((i = buf.indexOf("\n")) >= 0) {
          appendCmdOutput(prefix + buf.slice(0, i).replace(/\r$/, ""));
          buf = buf.slice(i + 1);
        }
      },
      // flush emits a last line that had no trailing newline. A command whose
      // output does not end in "\n" still printed that line.
      flush() {
        buf += dec.decode();
        if (buf) { appendCmdOutput(prefix + buf); buf = ""; }
      },
    };
  };

  // execRunToOutput runs one command in a task's worktree, streaming both
  // streams into the command output pane, and returns the result line.
  //
  // A non-zero exit is reported, not thrown: the command ran and said what it
  // meant. Only a refusal (no such task, no worktree, denied) reaches the catch.
  const execRunToOutput = async (taskId, argv, flags = {}) => {
    const { shell = false, sshdParent = false } = flags;
    const out = makeExecSink("1| ");
    const err = makeExecSink("2| ");
    const label = `exec ${taskId.slice(0, 12)}…: ${window.harness.execArgvText(argv)}`;
    try {
      const res = await window.harness.execRun(taskId, argv, {
        onStdout: (c) => out.write(c),
        onStderr: (c) => err.write(c),
        // One line for the RUNNER's own shell, which is what makes a pipe or a
        // redirect mean anything. The runner picks sh -c or cmd /c.
        shell,
        // A parent process named sshd, for a client that checks its own
        // ancestry by process name. Windows only.
        sshdParent,
      });
      out.flush();
      err.flush();
      if (res.exited) return `${label}: exit ${res.exitCode}`;
      return `${label}: ${res.kind}${res.detail ? `: ${res.detail}` : ""}`;
    } catch (e) {
      // Whatever the command managed to print before the failure is still
      // output it produced; dropping it would misreport the run.
      out.flush();
      err.flush();
      throw e;
    }
  };


  // Every verb the declaration marks as reachable here must be dispatchable.
  // Asserted from inside the runtime that owns the dispatch: scanning this
  // file from a Go test would be a regex over JavaScript.
  {
    // Built from the declaration, so the two coverage checks that used to live
    // here are gone rather than passing: a map derived from PathsForSurface
    // cannot miss a declared verb nor name an undeclared one. What is NOT
    // structural is whether the function a declaration NAMES exists — `{ fn:
    // "streamFinsh" }` once passed every check and failed at typing time — so
    // that is what this still asserts.
    RUNCMD_DISPATCH = window.harness.dispatch();
    for (const [p, how] of Object.entries(RUNCMD_DISPATCH)) {
      if (!how.fn && !(how.cache && how.stale)) {
        throw new Error(`webui dispatch ${p}: needs {fn} or {cache, stale}`);
      }
      // how.local is declared beside the name now (WebUIDispatch.Local), so
      // the page and its test no longer keep separate lists of exceptions.
      if (how.fn && !how.local && typeof window.harness[how.fn] !== "function") {
        throw new Error(`webui dispatch ${p}: names harness.${how.fn}, which the bridge does not export`);
      }
    }
  }


  // --- the trsf reading ---------------------------------------------------
  //
  // Read only while the panel is expanded. It is a live call to the server (or
  // to a runner), not part of the 5s snapshot, so a collapsed panel — the
  // default — costs nothing, and "open" means the same thing here as it does
  // for the TUI's modal: the poll runs exactly as long as somebody is looking.
  const trsfPanelEl = document.getElementById("trsf-panel");
  const trsfTargetSel = document.getElementById("trsf-target");
  const trsfEverySel = document.getElementById("trsf-every");
  const trsfNoteEl = document.getElementById("trsf-note");
  let trsfTimer = 0;
  let trsfInFlight = false;

  const setTrsfNote = (text, isError) => {
    if (!trsfNoteEl) return;
    trsfNoteEl.textContent = text || "";
    trsfNoteEl.classList.toggle("is-error", !!isError);
  };

  // reset starts a fresh series: a reading taken long after the last one is
  // not a delta over an interval anybody watched.
  const readTrsf = async (reset) => {
    // A read slower than the interval must not stack ticks — each would open
    // its own round trip and they would land out of order.
    if (trsfInFlight) return;
    trsfInFlight = true;
    try {
      const rows = await window.harness.trsfState({
        // Which of the two the cid goes in is the option's own kind: the
        // bridge resolves the pair into one peer, and sending a client's cid
        // as a runner would ask the registry for a runner that is not there.
        runner: trsfTargetSel && trsfSelectedKind(trsfTargetSel) === "runner" ? trsfTargetSel.value : "",
        client: trsfTargetSel && trsfSelectedKind(trsfTargetSel) === "client" ? trsfTargetSel.value : "",
        reset: !!reset,
      });
      renderTrsfTable(rows);
      // Say why the right-hand columns are empty on the first reading, rather
      // than letting a screen of "-" read as "this end reports nothing".
      if (!rows.length) setTrsfNote("(no connections visible to you)");
      else if (reset) setTrsfNote("deltas appear on the next reading — they need two");
      else setTrsfNote("");
    } catch (e) {
      // The rows already drawn stay: a failed read says nothing about whether
      // they were true when they were taken.
      setTrsfNote(e.message, true);
    } finally {
      trsfInFlight = false;
    }
  };

  const stopTrsfPoll = () => {
    if (trsfTimer) clearInterval(trsfTimer);
    trsfTimer = 0;
  };
  const startTrsfPoll = (reset) => {
    stopTrsfPoll();
    readTrsf(reset);
    trsfTimer = setInterval(() => readTrsf(false), Number(trsfEverySel.value) || 1000);
  };

  if (trsfPanelEl && trsfTargetSel && trsfEverySel) {
    trsfPanelEl.addEventListener("toggle", () => {
      if (!trsfPanelEl.open) {
        stopTrsfPoll();
        return;
      }
      // Built on open as well as from the snapshot: opening the panel before
      // the first poll has landed would otherwise find an empty picker.
      renderTrsfTargets(lastConns);
      startTrsfPoll(true);
    });
    // A different answerer is a different series, so its reading restarts.
    trsfTargetSel.addEventListener("change", () => {
      if (trsfPanelEl.open) startTrsfPoll(true);
    });
    // A different cadence is the same series measured differently: the next
    // delta is simply over a new interval, so this does not reset.
    trsfEverySel.addEventListener("change", () => {
      if (trsfPanelEl.open) startTrsfPoll(false);
    });
  }

  // connsView is what the `conns` verb does here — the WebUIDispatch the
  // declaration names. The bare form shows the tab; --trsf also expands the
  // panel, with --runner and --watch applied to it first so the poll it starts
  // is already aimed and paced.
  const connsView = (o) => {
    const opts = o || {};
    setActiveTab("conns");
    if (!trsfPanelEl || !trsfTargetSel || !trsfEverySel) return;
    // --runner / --watch cannot arrive without --trsf: the declaration's
    // Requires refuses that combination before it reaches any surface.
    if (!opts.trsf) return;
    if (opts.watch) {
      // Parsed in Go, by the same time.ParseDuration the CLI uses: a second
      // duration parser in JS is a copy that cannot fail loudly when the
      // first one grows.
      const p = window.harness.parseDurationMs(opts.watch);
      if (p.error) throw new Error(`conns --watch ${opts.watch}: ${p.error}`);
      // The presets are a convenience, not the range: a typed interval the
      // list does not carry becomes an option of its own rather than being
      // rounded to one that IS listed.
      if (!Array.from(trsfEverySel.options).some((x) => x.value === String(p.ms))) {
        const opt = document.createElement("option");
        opt.value = String(p.ms);
        opt.textContent = opts.watch;
        trsfEverySel.appendChild(opt);
      }
      trsfEverySel.value = String(p.ms);
    }
    // The kind travels beside the cid: a target named on the command line may
    // not be in this snapshot yet, and then nothing else can say which end it
    // is.
    renderTrsfTargets(lastConns, opts.client || opts.runner || "",
      opts.client ? "client" : opts.runner ? "runner" : "");
    const wasOpen = trsfPanelEl.open;
    trsfPanelEl.open = true;
    // Setting .open fires `toggle` only when it CHANGES, so an already-open
    // panel has to be restarted here or a retarget would not take effect.
    if (wasOpen) startTrsfPoll(true);
  };

  // forwardLsView is what the `forward ls` verb does here — the WebUIDispatch
  // the declaration names. Two answers behind one verb, and the page is what
  // picks: the bare form reads the snapshot cache, so the text it prints
  // cannot disagree with the forward panel drawn from the same rows; --drops
  // asks the endpoints, which the snapshot never does, so it is a live call.
  // The same split `conns --trsf` makes between its tab and its panel.
  const forwardLsView = async (o) => {
    const opts = o || {};
    const rows = opts.drops
      ? await window.harness.forwardList({ askEndpoints: true })
      : (lastForwards || []);
    return (rows || []).filter((f) => !opts.task || f.task === opts.task);
  };

  // What the page owns and runVerbCommand does not: the compose dropdowns,
  // the snapshot cache, the panels. Built once; every entry is a function so
  // the Node test can record the call instead of mimicking the widget.
  const cmdCtx = {
    get harness() { return window.harness; },
    echo: (text) => appendCmdOutput(text, true),
    refreshSnapshot: () => refreshSnapshot(),
    composeRepo: () => runnerSelect.value || "",
    composeHost: () => (hostSelect ? hostSelect.value || "" : ""),
    composeAgent: () => (agentSelect ? agentSelect.value || "" : ""),
    resumeTaskID: () => currentResumeTaskID(),
    agentArgs: () => currentAgentArgs(),
    sessionReq: (o) => sessionReq(o),
    setParentMessage: (id, swap, r) => setParentMessage(id, swap, r),
    filteredTaskRows: () =>
      Array.from(taskList.querySelectorAll(".task-row")).map((r) => r.textContent).join("\n") || "(none)",
    openSessionPreview: (id) => openSessionPreview(id),
    openGridSet: (o) => openGridSet(o),
    execRunToOutput: (id, argv, opts) => execRunToOutput(id, argv, opts),
    forwards: () => lastForwards || [],
    forwardLsView: (o) => forwardLsView(o),
    connsView: (o) => connsView(o),
    routeAmbiguous: (e, req, reissue) => routeAmbiguous(e, req, reissue),
    findForwardEntry: (id) => findForwardEntry(id),
    toggleForwardTap: (f, wrap, btn, opts) => toggleForwardTap(f, wrap, btn, opts),
    tapOpen: (id) => openTaps.has(`fwd:${id}`),
    execs: () => lastExecs || [],
    findExecEntry: (id) => findExecEntry(id),
    toggleExecTap: (e, wrap, btn, opts) => toggleExecTap(e, wrap, btn, opts),
    execTapOpen: (id) => openTaps.has(`exec:${id}`),
    chatTaskID: () => chatTaskId,
    openChatFor: (id) => openChatFor(id),
    // The compose panel's spawn defaults, as ONE value with two doors: the
    // chips write them and `caps set-defaults` writes them, and both read
    // back through here. spawnScope is derived from the scope controls, so
    // the setter drives the CONTROLS and lets the existing updateSpawnScope
    // recompute the spec -- writing spawnScope directly would show the
    // operator their scope and lose it the next time a radio moved.
    spawnDefaults: () => ({ caps: spawnCaps, capsLabel: capsLabelFor(spawnCaps), scope: spawnScope,
      delegateCapsLabel: capsLabelFor(spawnDeleg.caps), delegateScope: scopeStateSpec(spawnDeleg) }),
    setSpawnDefaults: (d) => setSpawnDefaults(d),
  };

  const runCmd = async () => {
    const line = cmdInput.value.trim();
    if (!line) return;
    cmdInput.value = "";
    appendCmdOutput(`> ${line}`, true);
    try {
      const out = await runVerbCommand(tokenize(line), cmdCtx);
      // Only when a case produced one. Several report through ctx.echo
      // themselves and return undefined, which printed a bare "undefined"
      // under the real message.
      if (out !== undefined) appendCmdOutput(out, true);
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
  // Two rows use them: #touch-keys under the attached terminal, and its copy in
  // the session-preview modal. wireTouchKeys gives each row its own Ctrl/Shift.

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

  const applyMods = (key, mods) => {
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

  // wireTouchKeys wires the buttons under `root` with their own modifier state.
  // send receives the bytes a key produces; getTerm returns the xterm the
  // scroll buttons move, or null while there is none. It returns typed, which
  // the same terminal's onData passes its data through so an armed Ctrl
  // applies to the next letter typed on the OS keyboard, and reset, which
  // disarms both modifiers.
  function wireTouchKeys(root, send, getTerm) {
      const mods = { ctrl: false, shift: false };

      const setMod = (name, on) => {
          mods[name] = on;
          const btn = root.querySelector(`button[data-mod="${name}"]`);
          if (btn) btn.classList.toggle("active", on);
      };

      root.querySelectorAll("button[data-mod]").forEach(btn => {
          btn.addEventListener("click", () => {
              const name = btn.getAttribute("data-mod");
              setMod(name, !mods[name]);
          });
      });

      root.querySelectorAll("button[data-key]").forEach(btn => {
          btn.addEventListener("click", () => {
              const key = btn.getAttribute("data-key");
              const seq = applyMods(key, mods);
              if (seq != null) send(seq);
              // Auto-clear shift after a special key press (one-shot semantics).
              if (mods.shift) setMod("shift", false);
              if (mods.ctrl) setMod("ctrl", false);
          });
      });

      // Scroll buttons act on xterm's local scrollback viewport — NOT sent to the
      // PTY. xterm's touch scrolling is finger-1:1 with no momentum (see
      // Viewport.handleTouchMove), so a flick won't carry; these give reliable
      // page-at-a-time navigation plus a jump back to the live bottom.
      root.querySelectorAll("button[data-scroll]").forEach(btn => {
          btn.addEventListener("click", () => {
              const t = getTerm();
              if (!t) return;
              switch (btn.getAttribute("data-scroll")) {
                  case "pageup":   t.scrollPages(-1); break;
                  case "pagedown": t.scrollPages(1);  break;
                  case "bottom":   t.scrollToBottom(); break;
              }
          });
      });

      const typed = (data) => {
          // If Ctrl is armed and the data is a single ASCII letter, transform to
          // Ctrl+<letter> (control code = letter AND 0x1f). Auto-clear Ctrl after.
          if (mods.ctrl && data.length === 1) {
              const c = data.charCodeAt(0);
              if (c >= 0x40 && c <= 0x7e) {
                  setMod("ctrl", false);
                  // Note: Shift on a letter is already applied by the OS
                  // (uppercase comes through as the char itself), so we don't
                  // touch shift state here.
                  return String.fromCharCode(c & 0x1f);
              }
          }
          // Shift modifier doesn't apply to free-typed characters (the OS sends
          // the already-shifted character). Only the special-key buttons consult
          // mods.shift.
          return data;
      };
      const reset = () => { setMod("ctrl", false); setMod("shift", false); };
      return { typed, reset };
  }

  // Send straight to the PTY — no term.focus(), so touch-key-only operations
  // (e.g. Shift+Tab to toggle auto mode) don't pop the OS soft keyboard. The
  // keyboard opens only when the user taps the terminal to type.
  const attachKeys = wireTouchKeys(document.getElementById("touch-keys"),
      (seq) => window.harness.sendInteractive(seq), () => term);

  term.onData((data) => window.harness.sendInteractive(attachKeys.typed(data)));
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

  // createStreamView is one event-stream transcript with its approval and
  // question control: the chat panel is one, and each grid cell over a stream
  // session is another (openSessionGrid). Both drive the session the same way
  // for the same reason the TUI's chat and grid pane share streamDriver — an
  // approval answered in a cell must be the approval the chat would send.
  //
  // els is { log, approval, status }; taskId() names the task it drives (the
  // chat's changes as it is reopened); onPending(req|null), if given, hears
  // the pending request change — the grid cell marks its header with it.
  function createStreamView(els, taskId, onPending) {
    let pending = null;  // the request awaiting an answer, with its Input

    const setStatus = (t) => { if (els.status) els.status.textContent = t || ""; };
    const setPending = (req) => {
      pending = req;
      if (onPending) onPending(req);
    };

    const append = (text, cls) => {
      const log = els.log;
      if (!log) return;
      const atBottom = log.scrollTop + log.clientHeight >= log.scrollHeight - 8;
      const div = document.createElement("div");
      div.className = cls || "c-text";
      div.textContent = text;
      log.appendChild(div);
      while (log.childElementCount > CHAT_LINE_LIMIT) log.removeChild(log.firstChild);
      // Only follow the tail if the reader was already there — scrolling up to
      // read something must not be yanked back by the next event.
      if (atBottom) log.scrollTop = log.scrollHeight;
    };

    // applyDisplay applies a streamagent.Display the wasm pump decided in Go
    // (cli/streamchat_wasm.go displayForJS): the line, its tone as a `c-<tone>`
    // class, and what it does to the status line. The page no longer words or
    // classifies an event itself — the TUI chat reads the same Display, so the
    // two chats cannot drift apart again (they already had once: a replayed
    // event drove the status line in one of them).
    const applyDisplay = (d) => {
      if (!d) return false;
      if (d.text) append(d.text, "c-" + (d.tone || "muted"));
      if (d.setStatus) setStatus(d.status || "");
      // A request settled anywhere — this view, another client, or before this
      // view attached and replayed — is no longer waiting on anyone.
      if (d.resolves && pending && pending.id === d.resolves) {
        setPending(null);
        showApproval();
      }
      return true;
    };

    // showQuestion renders a QUESTION request (the adapter filled
    // req.questions): per question a radio row (pick one) or checkboxes (pick
    // any) plus an "その他" text box, then a freeform reply and 送信 / 拒否. The
    // controls are the kinds the page already uses; the wording of the notice
    // and the rule that enables 送信 come from Go over the bridge.
    const showQuestion = (req) => {
      const box = els.approval;
      box.replaceChildren();
      const h = document.createElement("h3");
      h.textContent = "❓ 質問に答えてください  (" + (req.id || "") + ")";
      box.appendChild(h);
      // Its own element, rebuilt on every render: listeners hang off it rather
      // than off the approval box, which outlives each render and would collect
      // one stale set per call.
      const form = document.createElement("div");
      box.appendChild(form);
      const rows = [];
      req.questions.forEach((q, i) => {
        const fs = document.createElement("fieldset");
        fs.className = "chat-question";
        const lg = document.createElement("legend");
        lg.textContent = (q.header ? q.header + ": " : "") + q.question +
          (q.multi_select ? "（複数選択）" : "（1つ選択）");
        fs.appendChild(lg);
        const boxes = [];
        (q.options || []).forEach((o) => {
          const lab = document.createElement("label");
          lab.className = "chat-question-opt";
          const inp = document.createElement("input");
          inp.type = q.multi_select ? "checkbox" : "radio";
          // Scoped by the task as well: two cells can show the same request
          // id's form, and radios sharing a name are one group page-wide.
          inp.name = "q-" + taskId() + "-" + req.id + "-" + i;
          inp.value = o.label;
          lab.append(inp, " " + o.label);
          if (o.description) {
            const d = document.createElement("span");
            d.className = "notify-meta";
            d.textContent = " — " + o.description;
            lab.appendChild(d);
          }
          fs.appendChild(lab);
          boxes.push(inp);
        });
        const other = document.createElement("input");
        other.type = "text";
        other.placeholder = "その他（自分の言葉で答える）";
        other.className = "chat-question-other";
        fs.appendChild(other);
        form.appendChild(fs);
        rows.push({ q, boxes, other });
      });
      const reply = document.createElement("textarea");
      reply.rows = 2;
      reply.placeholder = "自由回答（どの質問にも当てはまらない返事）";
      reply.className = "chat-question-reply";
      form.appendChild(reply);
      const answersOf = () => {
        const out = {};
        rows.forEach(({ q, boxes, other }) => {
          const text = other.value.trim();
          let vals = boxes.filter((b) => b.checked).map((b) => b.value);
          if (text) vals = q.multi_select ? vals.concat([text]) : [text];
          if (vals.length) out[q.question] = vals;
        });
        return out;
      };
      const btns = document.createElement("div");
      btns.className = "chat-approval-btns";
      const send = document.createElement("button");
      send.type = "button";
      send.textContent = "✔ 送信";
      const refresh = () => {
        send.disabled = !window.harness.questionComplete(
          JSON.stringify(req), JSON.stringify(answersOf()), reply.value);
      };
      form.addEventListener("input", refresh);
      form.addEventListener("change", refresh);
      send.addEventListener("click", () => sendAnswer(req, answersOf(), reply.value.trim()));
      const deny = document.createElement("button");
      deny.type = "button";
      deny.textContent = "✘ 拒否";
      deny.title = "質問に答えない。理由はエージェントに verbatim で届く";
      deny.addEventListener("click", () => {
        const reason = window.prompt("答えない理由（エージェントがそのまま読みます。空でも可）");
        if (reason === null) return;
        answer("deny", reason, -1);
      });
      btns.append(send, deny);
      box.appendChild(btns);
      refresh();
      box.hidden = false;
    };

    const sendAnswer = async (req, answers, reply) => {
      const id = taskId();
      if (!pending || !id || pending.id !== req.id) return;
      setPending(null);
      showApproval();
      const parts = (req.questions || []).filter((q) => answers[q.question])
        .map((q) => (q.header || q.question) + "=" + answers[q.question].join(", "));
      if (reply) parts.push("reply: " + reply);
      append("▶ 回答: " + parts.join("; "), "c-you");
      setStatus("resuming…");
      try {
        await window.harness.streamAnswer(id, req.id, answers, reply);
      } catch (err) {
        append("✗ 回答を送れませんでした: " + err.message, "c-err");
      }
    };

    // showApproval renders the pending request: the tool, its input WHOLE
    // (this is the payload the log drops), and the choices as a button row —
    // buttons because that is this surface's native control, where the TUI uses
    // keys.
    const showApproval = () => {
      const box = els.approval;
      if (!box) return;
      if (!pending) { box.hidden = true; box.replaceChildren(); return; }
      const req = pending;
      if (req.questions && req.questions.length) { showQuestion(req); return; }
      box.replaceChildren();
      const h = document.createElement("h3");
      h.textContent = "⚑ " + (req.tool || "?") + " を実行してよいか  (" + (req.id || "") + ")";
      box.appendChild(h);
      if (req.description) {
        const d = document.createElement("div");
        d.className = "notify-meta";
        d.textContent = req.description;
        box.appendChild(d);
      }
      if (req.input !== undefined) {
        const pre = document.createElement("pre");
        try { pre.textContent = JSON.stringify(req.input, null, 2); }
        catch (_) { pre.textContent = String(req.input); }
        box.appendChild(pre);
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
      mk("✔ 許可", "このツール呼び出しを実行させる", () => answer("allow", "", -1));
      mk("✘ 拒否", "実行させない。理由はエージェントに verbatim で届く", () => {
        const reason = window.prompt("拒否の理由（エージェントがそのまま読みます。空でも可）") ;
        if (reason === null) return; // cancelled — the request stays pending
        answer("deny", reason, -1);
      });
      (req.suggestions || []).forEach((sg, i) => {
        const label = [sg.type, sg.mode, sg.destination ? "(" + sg.destination + ")" : ""].filter(Boolean).join(" ");
        mk("＋ " + label, "許可した上で、この提案も受け入れる（以後の挙動が変わる標準の変更）",
           () => answer("allow", "", i));
      });
      box.appendChild(row);
      box.hidden = false;
    };

    const answer = async (behavior, message, suggestion) => {
      const id = taskId();
      if (!pending || !id) return;
      const req = pending;
      setPending(null);
      showApproval();
      append((behavior === "deny" ? "▶ 拒否 " : "▶ 許可 ") + (req.tool || "") +
             (message ? ": " + message : ""), behavior === "deny" ? "c-err" : "c-you");
      setStatus("resuming…");
      try {
        await window.harness.streamApprove(id, req.id, behavior, message, suggestion);
      } catch (err) {
        append("✗ 応答を送れませんでした: " + err.message, "c-err");
      }
    };

    // line folds one stream line in. Go decided its display (cli.LineDisplay
    // over the bridge): the wording, the tone, the status-line effect, and —
    // for a line that is not the protocol — the escaping. null means a decoded
    // request, the one kind the page renders itself, because its payload and
    // choices go into the approval control rather than onto a line.
    const line = (raw, display) => {
      if (applyDisplay(display)) return;
      let msg = null;
      try { msg = JSON.parse(raw); } catch (_) { return; }
      if (msg.kind !== "request") return;
      const req = msg.request || null;
      // The same request again is a replay after a reattach: a form half
      // filled in for it stays as it is.
      const same = !!(req && pending && pending.id === req.id);
      setPending(req);
      if (req && req.questions && req.questions.length) {
        append(window.harness.questionSummary(JSON.stringify(req)), "c-warn");
      } else {
        append("⚑ 承認待ち: " + ((req && req.tool) || "?"), "c-warn");
      }
      if (!same) showApproval();
    };

    const reset = () => {
      setPending(null);
      if (els.log) els.log.replaceChildren();
      showApproval();
    };

    // send is a turn. No local echo: the adapter puts the turn on the stream,
    // so this view shows it the way every other client and a later replay do.
    const send = async (text) => {
      const id = taskId();
      if (!id || !text) return;
      setStatus("");
      try {
        await window.harness.streamTurn(id, text);
      } catch (err) {
        append("✗ 送信できませんでした: " + err.message, "c-err");
      }
    };

    const interrupt = async () => {
      const id = taskId();
      if (!id) return;
      append("▶ 中断", "c-you");
      try { await window.harness.streamInterrupt(id); }
      catch (err) { append("✗ " + err.message, "c-err"); }
    };

    const finish = async () => {
      const id = taskId();
      if (!id) return;
      append("▶ finish（このターンを完走して終了します）", "c-you");
      try { await window.harness.streamFinish(id); }
      catch (err) { append("✗ " + err.message, "c-err"); }
    };

    return {
      line, reset, send, interrupt, finish, setStatus,
      opened: () => setStatus("attached"),
      closed: (err) => setStatus(err ? ("stream ended: " + err) : "stream ended"),
    };
  }

  // wireStreamInput makes a textarea the turn box of view: Enter sends,
  // Shift+Enter is a newline, and Esc interrupts. Esc never leaves: in a grid
  // cell its default is to close the whole grid dialog, which is the accident
  // an operator reaching for an interrupt used to get from the TUI chat too.
  function wireStreamInput(input, view) {
    const sendNow = () => {
      const text = input.value.trim();
      if (!text) return;
      input.value = "";
      view.send(text);
    };
    input.addEventListener("keydown", (e) => {
      if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); sendNow(); return; }
      if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); view.interrupt(); }
    });
    return sendNow;
  }

  const chatView = createStreamView(
    { log: chatLog, approval: chatApproval, status: chatStatus }, () => chatTaskId);

  // showChat swaps the terminal tab between the xterm and the chat. The touch
  // keys go with the xterm: they send terminal control bytes, which mean
  // nothing to a session whose input is JSON.
  const showChat = (on) => {
    if (chatPanel) chatPanel.hidden = !on;
    if (terminalEl) terminalEl.style.display = on ? "none" : "";
    if (touchKeys) touchKeys.style.display = on ? "none" : "";
  };

  // The three JS hooks cli/streamchat_wasm.go calls for the chat. Globals,
  // matching the harness_preview* pattern the xterm panes already use; a grid
  // cell's are harness_streamPane* (openSessionGrid).
  window.harness_streamOpen = (taskID) => {
    if (taskID !== chatTaskId) return;
    chatView.opened();
  };
  window.harness_streamLine = (taskID, line, display) => {
    if (taskID !== chatTaskId) return;
    chatView.line(line, display);
  };
  window.harness_streamClosed = (taskID, err) => {
    if (taskID !== chatTaskId) return;
    chatView.closed(err);
  };

  // openChatFor attaches the chat to a stream task and shows it.
  const openChatFor = async (taskID) => {
    if (!taskID) return;
    try { window.harness.streamStop(); } catch (_) { /* nothing attached */ }
    chatTaskId = taskID;
    chatView.reset();
    showChat(true);
    setActiveTab("terminal");
    if (attachedTask) attachedTask.textContent = `chat: ${taskID}`;
    // Said explicitly: a chat opened on a freshly RESUMED task shows nothing
    // for a while — the replay comes from the mux ring and a resume starts a
    // new one, while the agent reads its own history first. A blank panel there
    // reads as broken.
    chatView.setStatus("attaching… (resume 直後はエージェントが履歴を読み終えるまで無言のことがあります)");
    try {
      await window.harness.streamStart(taskID);
    } catch (err) {
      chatView.setStatus("attach failed: " + err.message);
      showError(err);
    }
  };

  const closeChat = () => {
    try { window.harness.streamStop(); } catch (_) { /* already closed */ }
    chatTaskId = "";
    chatView.reset();
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

  // A textarea is the right control for free text — and it is the thing the
  // TUI's one-line input cannot do.
  const chatSend = chatInput ? wireStreamInput(chatInput, chatView) : () => {};
  document.getElementById("chat-send")?.addEventListener("click", chatSend);
  document.getElementById("chat-interrupt")?.addEventListener("click", () => chatView.interrupt());
  document.getElementById("chat-finish")?.addEventListener("click", () => chatView.finish());

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
    agentArgs: currentAgentArgs(),
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
    // Built on open as well as now: a section first opened on an empty server
    // would otherwise list no tasks (checklist item 34a).
    renderSpawnDeleg();
    const delegDetails = document.getElementById("spawn-deleg-details");
    if (delegDetails) delegDetails.addEventListener("toggle", () => { if (delegDetails.open) renderSpawnDeleg(); });
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

  // setSpawnDefaults applies a parsed `caps set-defaults` to the compose
  // panel's own controls. Fields are applied only when present: an omitted
  // --caps leaves the chips alone rather than clearing them, which is the
  // difference between "I did not say" and "I said none".
  function setSpawnDefaults(d) {
    if (typeof d.caps === "number") {
      spawnCaps = d.caps;
      renderCaps(); // the chips re-render FROM spawnCaps, so this is the echo
    }
    if (typeof d.scopeBase === "string") {
      spawnBase = d.scopeBase;
      spawnExcludeSelf = !!d.scopeExcludeSelf;
      spawnVisBase = d.scopeVisBase || "";
      spawnScopeIds.clear();
      for (const id of (d.scopeIds || [])) spawnScopeIds.add(id);
      spawnVisIds.clear();
      for (const id of (d.scopeVisIds || [])) spawnVisIds.add(id);
      // Re-render the radios and checklists from the new state, then let the
      // existing updater derive spawnScope -- one derivation, not two.
      syncScopeControls();
      refreshSpawnScopeChecklist();
    }
    if (typeof d.delegateCaps === "number") spawnDeleg.caps = d.delegateCaps;
    if (typeof d.delegateScopeBase === "string") setScopeState(spawnDeleg, {
      base: d.delegateScopeBase, excludeSelf: d.delegateScopeExcludeSelf, visBase: d.delegateScopeVisBase,
      ids: d.delegateScopeIds, visIds: d.delegateScopeVisIds,
    });
    if (typeof d.delegateCaps === "number" || typeof d.delegateScopeBase === "string") renderSpawnDeleg();
  }

  // ---- delegation bound ----------------------------------------------------
  //
  // The bound is a caps mask plus a scope, so it is built from the controls
  // the task's own authority already uses — chips, the base and visibility
  // radio rows, the two task checklists — and serialized through the same
  // harness.scopeSpec. A scope state is {caps, base, excludeSelf, visBase,
  // ids, visIds}; "" visBase is the unstated rank, as everywhere.
  function newScopeState() {
    return { caps: 0, base: "subtree", excludeSelf: false, visBase: "", ids: new Set(), visIds: new Set() };
  }

  function setScopeState(st, { base, excludeSelf, visBase, ids, visIds }) {
    st.base = base || "subtree";
    st.excludeSelf = !!excludeSelf;
    st.visBase = visBase || "";
    st.ids = new Set(ids || []);
    st.visIds = new Set(visIds || []);
  }

  // Always explicit: the bound has no "not named" form on this page.
  function scopeStateSpec(st) {
    return scopeSpec(st.base, st.excludeSelf, st.ids, false, st.visBase, st.visIds);
  }

  // buildScopeControls renders one scope state's controls into root and
  // calls onChange after every edit. Radio names are derived from root's id,
  // so two of these on one page do not share a radio group.
  function buildScopeControls(root, st, excludeId, onChange) {
    if (!root) return;
    root.replaceChildren();
    const echo = document.createElement("code");
    echo.className = "scope-echo";
    const paint = () => {
      echo.textContent = "delegate=" + capsLabelFor(st.caps) + "  delegate-scope=" + scopeStateSpec(st);
    };
    const changed = () => { onChange(); paint(); };
    const chips = document.createElement("div");
    chips.className = "caps-row";
    root.appendChild(chips);
    buildCapChips(chips, () => st.caps, (v) => { st.caps = v; changed(); });

    const radioRow = (label, name, values, current, set) => {
      const row = document.createElement("div");
      row.className = "scope-base-row";
      const head = document.createElement("span");
      head.className = "scope-row-label";
      head.textContent = label;
      row.appendChild(head);
      for (const [value, text] of values) {
        const l = document.createElement("label");
        const r = document.createElement("input");
        r.type = "radio";
        r.name = name;
        r.value = value;
        r.checked = value === current;
        r.addEventListener("change", () => { if (r.checked) { set(value); changed(); } });
        l.append(r, " " + text);
        row.appendChild(l);
      }
      return row;
    };
    const baseRow = radioRow("base:", root.id + "-base",
      [["subtree", "subtree"], ["none", "none"], ["global", "global"]], st.base, (v) => { st.base = v; });
    const selfLabel = document.createElement("label");
    const selfCb = document.createElement("input");
    selfCb.type = "checkbox";
    selfCb.checked = st.excludeSelf;
    selfCb.addEventListener("change", () => { st.excludeSelf = selfCb.checked; changed(); });
    selfLabel.append(selfCb, " 自分自身を除く");
    baseRow.appendChild(selfLabel);
    root.appendChild(baseRow);
    root.appendChild(radioRow("見える範囲:", root.id + "-vis",
      [["", "base に従う"], ["subtree", "subtree"], ["none", "none"], ["global", "global"]],
      st.visBase, (v) => { st.visBase = v; }));

    const section = (summary, set) => {
      const d = document.createElement("details");
      const s = document.createElement("summary");
      s.textContent = summary;
      const list = document.createElement("div");
      list.className = "task-checklist";
      d.append(s, list);
      buildTaskChecklist(list, set, excludeId, changed);
      root.appendChild(d);
    };
    section("渡してよい対象タスク", st.ids);
    section("見えるだけなら渡してよいタスク", st.visIds);

    paint();
    root.appendChild(echo);
  }

  function renderSpawnDeleg() {
    buildScopeControls(document.getElementById("spawn-deleg"), spawnDeleg, "", () => {});
  }

  // syncScopeControls pushes spawnBase / spawnExcludeSelf / spawnVisBase back
  // onto the radios and checkbox. Without it the command line would change the
  // value and leave the controls showing the old one, and the next click on an
  // untouched radio would restore what the operator just replaced.
  function syncScopeControls() {
    for (const r of document.querySelectorAll('input[name="spawn-base"]')) {
      r.checked = r.value === spawnBase;
    }
    for (const r of document.querySelectorAll('input[name="spawn-vis-base"]')) {
      r.checked = r.value === spawnVisBase;
    }
    const ex = document.getElementById("spawn-exclude-self");
    if (ex) ex.checked = spawnExcludeSelf;
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
  // The target's delegation bound, edited; sent only when it differs from
  // what the dialog opened with (regrantDelegOpened), because with --cascade
  // writing the bound clamps every descendant to it.
  const regrantDeleg = newScopeState();
  let regrantDelegOpened = "";
  const delegKey = (st) => st.caps + " " + scopeStateSpec(st);

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

  // opts.whatIf opens it from the sheet's 🔮 as a preview: 適用 and
  // --keep-conns are gone (nothing is sent), and the mode row picks whether the
  // controls describe this task's caps set or a child it spawns. From 🔑,
  // プレビュー sits beside 適用 over the same controls.
  function openRegrantDialog(t, opts = {}) {
    if (!regrantModal || typeof window.harness.setCaps !== "function") return;
    const whatIfOnly = !!opts.whatIf;
    document.getElementById("regrant-heading").textContent = whatIfOnly ? "プレビュー" : "caps/scope 再付与";
    document.getElementById("regrant-apply").hidden = whatIfOnly;
    document.getElementById("regrant-keep-conns-label").hidden = whatIfOnly;
    document.getElementById("regrant-mode-row").hidden = !whatIfOnly;
    regrantModal.querySelector('input[name="regrant-mode"][value="set"]').checked = true;
    document.getElementById("regrant-cascade").disabled = false;
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
    regrantDeleg.caps = typeof t.delegateCapsBits === "number" ? t.delegateCapsBits : 0;
    setScopeState(regrantDeleg, {
      base: t.delegateScopeBase, excludeSelf: t.delegateScopeExcludeSelf, visBase: t.delegateScopeVisBase,
      ids: t.delegateScopeIds, visIds: t.delegateScopeVisIds,
    });
    regrantDelegOpened = delegKey(regrantDeleg);
    buildScopeControls(document.getElementById("regrant-deleg"), regrantDeleg, t.id, () => {});
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
    // A spawn has no descendants to cascade into.
    const regrantSpawnMode = () => regrantModal.querySelector('input[name="regrant-mode"][value="spawn"]').checked;
    for (const r of regrantModal.querySelectorAll('input[name="regrant-mode"]')) {
      r.addEventListener("change", () => {
        const cascade = document.getElementById("regrant-cascade");
        cascade.disabled = regrantSpawnMode();
        if (cascade.disabled) cascade.checked = false;
      });
    }
    // プレビュー: the dialog's request as `caps what-if` flags, answered over the
    // listing the tree was drawn from. Nothing is sent.
    document.getElementById("regrant-whatif").addEventListener("click", () => {
      if (typeof window.harness.whatIf !== "function") return;
      const req = {
        taskId: regrantTaskId,
        caps: capsLabelFor(regrantBits),
        scope: scopeSpec(regrantBase, regrantExcludeSelf, regrantIds, false,
          regrantVisBase, regrantVisIds),
        "scope-for": overrideSpecsFrom("regrant"),
      };
      const delegate = () => {
        req["delegate-caps"] = capsLabelFor(regrantDeleg.caps);
        req["delegate-scope"] = scopeStateSpec(regrantDeleg);
      };
      const spawn = regrantSpawnMode();
      if (spawn) {
        // The child requests every half as shown, as a spawn carries them.
        req.spawn = true;
        delegate();
      } else {
        req.cascade = document.getElementById("regrant-cascade").checked;
        // As 適用: the bound is written only when it was changed.
        if (delegKey(regrantDeleg) !== regrantDelegOpened) delegate();
      }
      regrantModal.close();
      // Closing keeps the dialog's controls and state as they are, so showing
      // it again IS the dialog as left — unless another task's dialog was
      // opened since, which reset it; that task then gets a fresh one.
      const forId = regrantTaskId;
      const forWhatIfOnly = document.getElementById("regrant-apply").hidden;
      showWhatIf(window.harness.whatIf(req), () => {
        if (regrantTaskId === forId) {
          regrantModal.showModal();
          return;
        }
        const t = (lastTasks || []).find((x) => x.id === forId);
        if (t) openRegrantDialog(t, { whatIf: forWhatIfOnly });
      }, spawn ? "spawn" : "caps set");
    });
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
      const delegChanged = delegKey(regrantDeleg) !== regrantDelegOpened;
      if (delegChanged) {
        req.delegateCaps = regrantDeleg.caps;
        req.delegateScope = scopeStateSpec(regrantDeleg);
      }
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
        if (delegChanged) msg += "  delegate=" + capsLabelFor(req.delegateCaps) + " delegate-scope=" + req.delegateScope;
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
    const args = currentAgentArgs();
    // agent reuses the Compose dropdown's current selection (this quick-resume
    // path — from the notification feed — only has the task id, not the task's
    // own AgentProfile, so it can't default per-task the way buildTaskSheet's
    // per-row dropdown does).
    const agent = agentSelect ? (agentSelect.value || "") : "";
    const req = sessionReq({ agent, agentArgs: args, resumeTaskId: id, resumeConversation: true });
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
  // rejection: one block per runner, one button per agent it offers. A click
  // re-issues baseReq (the request that just failed) through reissue, pinned
  // by the candidate's `pin` — its identity, or its cid for a runner without
  // one, decided in Go so the page never chooses — and with that agent. host
  // and ip are cleared: the pin is unambiguous even when a hostname is not (a
  // hostname can itself be shared by >=2 runners, which is the whole reason
  // this modal exists).
  function pickRunnerAndRetry(candidates, baseReq, reissue) {
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
      const row = document.createElement("div");
      row.className = "runner-choice";
      const top = document.createElement("span");
      top.className = "runner-choice-head";
      const host = document.createElement("span");
      host.className = "runner-choice-host";
      host.textContent = c.hostname || "(unknown)";
      const load = document.createElement("span");
      load.className = "runner-choice-load";
      load.textContent = `[${c.activeTasks}/${c.maxTasks}]`;
      top.append(host, load);
      const root = document.createElement("span");
      root.className = "runner-choice-root";
      root.textContent = c.matchedRoot || "(no matched root)";
      const cid = document.createElement("span");
      cid.className = "runner-choice-cid";
      cid.textContent = c.cid || "";
      const agents = document.createElement("div");
      agents.className = "runner-choice-agents";
      for (const p of (c.profiles && c.profiles.length ? c.profiles : [""])) {
        const btn = document.createElement("button");
        btn.type = "button";
        btn.className = "runner-choice-agent";
        btn.textContent = p || "(default)";
        btn.onclick = async (ev) => {
          ev.preventDefault();
          ev.stopPropagation();
          modal.close();
          // baseReq came from sessionReq, so caps/resumeCapsOverride are
          // already correct; only the picked (runner, agent) differ.
          const retryReq = { ...baseReq, host: "", ip: "", runner: c.pin, agent: p };
          try {
            await reissue(retryReq);
          } catch (e2) {
            alert(`retry: ${e2.message}`);
          }
        };
        agents.appendChild(btn);
      }
      row.append(top, root, cid, agents);
      list.appendChild(row);
    });
    modal.showModal();
  }

  // reissueInteractive is the default re-issue for an ambiguous interactive
  // open: the same success tail the first open would have run. The picker is
  // a SECOND request build for the same intent and has to end where the first
  // would have — without the eventStream branch an ambiguous --stream spawn
  // silently became a PTY session (checklist 28a's shape).
  const reissueInteractive = async (retryReq) => {
    const taskID = await window.harness.startInteractive(retryReq);
    if (retryReq.eventStream) {
      await openChatFor(taskID);
      return;
    }
    showTerminalView();
    onInteractiveOpened(taskID, "session");
    // Same trailing fit/resize tail as openInteractive/reattachTo/
    // resumeTaskById, so a freshly-attached PTY gets the actual terminal size.
    try { fit.fit(); } catch (_) { /* element not yet laid out */ }
    window.harness.resizeInteractive({ cols: term.cols, rows: term.rows });
  };

  // routeAmbiguous opens the runner-picker modal when e is an ambiguous_runner
  // rejection and returns true (the caller should then `return`); false
  // otherwise. Shared by openInteractive, the resume paths (resumeTaskById /
  // doResume) and the command input's submit. baseReq is the request that just
  // failed; reissue sends it again pinned, and defaults to an interactive open.
  function routeAmbiguous(e, baseReq, reissue = reissueInteractive) {
    if (e && e.code === "ambiguous_runner" && Array.isArray(e.candidates)) {
      pickRunnerAndRetry(e.candidates, baseReq, reissue);
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
        flash(r.status === "armed" ? `🔔 armed #${r.watcherId} ✓` : `🔔 ${r.status}`);
      } catch (e) {
        console.error("awaitIdle:", e);
        flash("🔔 error");
      }
    });
  }

  // "📝 進捗で通知": arm a one-shot await-progress watcher (sink=notify) on the
  // attached session's task. The baseline is the row's progressAt — carried as
  // a decimal string because a nanosecond stamp does not fit a JS number — so
  // a line written since the list was drawn fires at once.
  {
    const btn = document.getElementById("await-progress-btn");
    const origLabel = btn.textContent;
    let revertTimer = 0;
    const flash = (text) => {
      btn.textContent = text;
      clearTimeout(revertTimer);
      revertTimer = setTimeout(() => { btn.textContent = origLabel; }, 2500);
    };
    btn.addEventListener("click", async () => {
      if (!currentSessionTaskId) { flash("📝 セッション未接続"); return; }
      const row = (lastTasks || []).find((t) => t.id === currentSessionTaskId);
      try {
        const r = await window.harness.awaitProgress({
          taskId: currentSessionTaskId, since: row ? row.progressAt : "0", sink: "notify",
        });
        flash(r.status === "armed" ? `📝 armed #${r.watcherId} ✓` : `📝 ${r.status}`);
      } catch (e) {
        console.error("awaitProgress:", e);
        flash("📝 error");
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
  //     noteEl, onResize, onClosed, key, capReplay, typed?, cropped, follow,
  //     followRaf, cropAt, paintFollow } — the last five are crop mode's
  //     (cropPane, wireCropBody).
  const sessionPreviewModal    = document.getElementById("session-preview-modal");
  const sessionPreviewTitle    = document.getElementById("session-preview-title");
  const sessionPreviewBody     = document.getElementById("session-preview-body");
  const sessionPreviewPause    = document.getElementById("session-preview-pause");
  const sessionPreviewReattach = document.getElementById("session-preview-reattach");
  const sessionPreviewClose    = document.getElementById("session-preview-close");
  const sessionPreviewKeys     = document.getElementById("session-preview-keys");

  const previewPanes = new Map();

  // The preview's touch keys are the attached terminal's buttons, copied so
  // the two rows cannot offer different keys. ids are dropped: they name the
  // original row's buttons.
  for (const btn of document.getElementById("touch-keys").children) {
    const copy = btn.cloneNode(true);
    copy.removeAttribute("id");
    sessionPreviewKeys.appendChild(copy);
  }
  const previewKeys = wireTouchKeys(sessionPreviewKeys,
    (seq) => { const p = previewPanes.get("preview"); if (p) paneInput(p, seq); },
    () => previewPanes.get("preview")?.term || null);

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

  // paintPauseBtn reflects a pane's live flag onto its ⏸/▶ toggle: the single
  // preview's header button, or a grid cell's.
  function paintPauseBtn(btn, p) {
    const live = !!(p && p.live);
    btn.textContent = live ? "⏸" : "▶";
    btn.title = live ? "一時停止" : "再開";
  }

  function setPreviewPauseLabel() {
    paintPauseBtn(sessionPreviewPause, previewPanes.get("preview"));
  }

  // togglePaneLive is ⏸/▶: pause closes the stream and leaves the frame;
  // resume re-attaches, and the replay jumps to the current screen, so a
  // cropped pane follows again from the bottom-left (an offset panned in the
  // old frame means nothing in the new one).
  function togglePaneLive(key, p) {
    if (p.live) { stopPane(key); return; }
    p.follow = true;
    if (p.paintFollow) p.paintFollow();
    resetPaneScroll(p);
    startPane(key, p);
  }

  // resetPaneScroll puts a pane body at its top-left and records that as our
  // own position. Without the record, the scroll event the reset raises — or
  // the browser's clamp when startPane empties the body under a panned offset
  // — would read as the operator's pan and stop the pane following
  // (wireCropBody).
  function resetPaneScroll(p) {
    if (!p.bodyEl) return;
    p.cropAt = { top: 0, left: 0 };
    p.bodyEl.scrollTop = 0;
    p.bodyEl.scrollLeft = 0;
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
    // p.typed is the pane's touch-key row, when it has one (the single
    // preview does; grid cells do not). A paused pane leaves an armed Ctrl
    // armed: the keystroke is not sent, so it has not used the modifier.
    p.term.onData((data) => paneInput(p, p.live && p.typed ? p.typed(data) : data));
    // p.cropped is the grid's or the single preview's display mode
    // (cropDefault and the 表示 toggle of each modal).
    p.onResize = () => (p.cropped && p.cropped() ? cropPane(p) : fitPaneScale(p));
    p.term.onWriteParsed(() => scheduleCropFollow(p));
    p.onResize();
  }

  // paneInput sends data to the pane's session. Typing into a PAUSED pane says
  // so instead of vanishing: ⏸ closes the stream, so there is nothing to write
  // to, and silently swallowing the keystroke is the same failure the
  // read-only preview had.
  function paneInput(p, data) {
    if (p.live) { window.harness.previewInput(p.key, data); return; }
    if (p.pausedNote) return; // already said it; don't stack notes per key
    p.pausedNote = paneNote(p, "一時停止中 — ▶ で再開すると入力できます");
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

  // cropPane is a pane's other display mode, the TUI grid's: the terminal
  // stays at PANE_FONT_BASE and the pane shows the part of it that fits,
  // scrolled to the bottom-left of what the session has drawn
  // (cropBottomRow). fitPaneScale on a phone shrinks a 120-column session to
  // ~6px glyphs; this keeps them readable and gives up the right-hand columns.
  //
  // The pane body is the scroll container and the xterm inside it takes no
  // pointer events (style.css, .grid-crop / .preview-crop): xterm 6 claims
  // touch for its own scrollback gesture and turns a wheel over an
  // alternate-screen app into arrow keys sent to the session, so a pan that
  // reached it would either not pan or type into the agent. A tap on the body
  // focuses the terminal instead (wireCropBody), which is the only thing the
  // pointer did there.
  function cropPane(p) {
    if (!p.term) return;
    if (p.term.options.fontSize !== PANE_FONT_BASE) p.term.options.fontSize = PANE_FONT_BASE;
    p.scaleBox.style.transform = "";
    p.scaleBox.style.width = "";
    p.spacer.style.height = "";
    followCrop(p);
  }

  // scheduleCropFollow coalesces a burst of parsed writes into one follow per
  // frame: onWriteParsed fires per chunk, and each follow reads layout.
  function scheduleCropFollow(p) {
    if (!p.cropped || !p.cropped() || !p.follow || p.followRaf) return;
    p.followRaf = requestAnimationFrame(() => {
      p.followRaf = 0;
      followCrop(p);
    });
  }

  // followCrop scrolls a following cell so the session's last drawn row sits
  // at the bottom of the cell and column 0 at its left — the TUI's default
  // crop (PaneStreamer.Render). It records the position it set so the scroll
  // event that this assignment raises is not mistaken for the operator's pan.
  function followCrop(p) {
    if (!p.follow || !p.term || !p.bodyEl || !p.scaleBox) return;
    if (!p.cropped || !p.cropped()) return;
    const screenEl = p.scaleBox.querySelector(".xterm-screen");
    if (!screenEl) return;
    const buf = p.term.buffer.active;
    const lines = [];
    for (let y = 0; y < p.term.rows; y++) {
      const line = buf.getLine(buf.viewportY + y);
      lines.push(line ? line.translateToString(true) : "");
    }
    const bottom = cropBottomRow(lines, buf.baseY + buf.cursorY - buf.viewportY);
    const body = p.bodyEl;
    const sr = screenEl.getBoundingClientRect();
    const br = body.getBoundingClientRect();
    const termTop = sr.top - br.top - body.clientTop + body.scrollTop;
    body.scrollTop = cropScrollTop(termTop, bottom, sr.height / p.term.rows, body.clientHeight);
    body.scrollLeft = 0;
    // Read back: the browser clamps to the scrollable range.
    p.cropAt = { top: body.scrollTop, left: body.scrollLeft };
  }

  // wireCropBody gives a pane body the crop's pointer behaviour. A scroll the
  // operator made stops the pane following and shows its ⤓ (p.paintFollow) —
  // a pane that does not follow and says nothing reads as a session that went
  // quiet. A tap focuses the terminal, which takes no pointer events while
  // cropped; a touch that scrolled raises no click. paneOf is looked up per
  // event because the single preview reuses one body across openings.
  function wireCropBody(body, paneOf) {
    body.addEventListener("scroll", () => {
      const p = paneOf();
      if (!p || !p.cropped() || !p.follow) return;
      const at = p.cropAt;
      // Our own followCrop assignment raises this event too; only a
      // position it did not set is the operator's pan.
      if (at && Math.abs(body.scrollTop - at.top) < 1 && Math.abs(body.scrollLeft - at.left) < 1) return;
      p.follow = false;
      p.paintFollow();
    });
    body.addEventListener("click", () => {
      const p = paneOf();
      if (p && p.cropped() && p.term) p.term.focus();
    });
  }

  // refollowPane is ⤓: back to the bottom-left, following again.
  function refollowPane(p) {
    p.follow = true;
    p.paintFollow();
    followCrop(p);
  }

  // paintCropToggle labels a modal's 表示 button with the mode it is in.
  function paintCropToggle(btn, crop) {
    btn.textContent = crop ? "表示: 等倍" : "表示: 縮小";
    btn.title = crop
      ? "等倍: 左下を切り出して表示 (スクロールで移動)。押すと全体を縮小表示"
      : "縮小: 全体を幅に収める。押すと等倍で左下を切り出す";
    btn.setAttribute("aria-pressed", crop ? "true" : "false");
  }

  // applyCropMode redraws one open terminal pane in its modal's current mode.
  // A mode change restarts following: an offset panned in one font size means
  // nothing in the other.
  function applyCropMode(p) {
    p.follow = true;
    if (p.paintFollow) p.paintFollow();
    // The fit view starts at the top-left; a cropped offset would linger.
    if (!p.cropped()) resetPaneScroll(p);
    if (p.onResize) p.onResize();
  }

  // How a terminal pane is drawn: crop (cropPane, the TUI's way) or fit
  // (fitPaneScale). The grid and the single preview each keep their own
  // toggle; until the operator presses one, each opening picks by the 600px
  // breakpoint the phone layout uses (cropDefault). A choice lasts until the
  // page reloads.
  const paneNarrow = window.matchMedia("(max-width: 600px)");

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

  const sessionPreviewMode   = document.getElementById("session-preview-mode");
  const sessionPreviewFollow = document.getElementById("session-preview-follow");
  let previewCropChoice = null;
  let previewCrop = false;

  function paintPreviewMode() {
    sessionPreviewBody.classList.toggle("preview-crop", previewCrop);
    paintCropToggle(sessionPreviewMode, previewCrop);
  }

  sessionPreviewMode.addEventListener("click", () => {
    previewCrop = !previewCrop;
    previewCropChoice = previewCrop;
    paintPreviewMode();
    const p = previewPanes.get("preview");
    if (p) applyCropMode(p);
  });
  sessionPreviewFollow.addEventListener("click", () => {
    const p = previewPanes.get("preview");
    if (p) refollowPane(p);
  });
  wireCropBody(sessionPreviewBody, () => previewPanes.get("preview"));

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
      typed: previewKeys.typed,
      cropped: () => previewCrop,
      follow: true, followRaf: 0, cropAt: null,
      paintFollow: () => { sessionPreviewFollow.hidden = !(previewCrop && !p.follow); },
    };
    previewCrop = cropDefault(paneNarrow.matches, previewCropChoice);
    paintPreviewMode();
    p.paintFollow();
    // The body is reused across openings: the last session's offset must not
    // carry over (fit mode never resets it on its own).
    resetPaneScroll(p);
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
    previewKeys.reset();            // a Ctrl armed here must not carry to the next session
  });
  sessionPreviewPause.addEventListener("click", () => {
    const p = previewPanes.get("preview");
    if (!p) return;
    togglePaneLive("preview", p);
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
  const gridModeBtn = document.getElementById("session-grid-mode");
  let gridKeys = [];

  // The grid's display mode (see paneNarrow).
  let gridCropChoice = null;
  let gridCrop = false;

  function paintGridMode() {
    gridBody.classList.toggle("grid-crop", gridCrop);
    paintCropToggle(gridModeBtn, gridCrop);
  }

  gridModeBtn.addEventListener("click", () => {
    gridCrop = !gridCrop;
    gridCropChoice = gridCrop;
    paintGridMode();
    for (const key of gridKeys) {
      const p = previewPanes.get(key);
      if (p) applyCropMode(p);
    }
  });

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
    for (const key of gridKeys) stopGridPane(key);
    gridKeys = [];
  }

  // stopGridPane stops one cell's stream, whichever kind it is: a PTY cell is a
  // previewPanes entry, a stream cell a streamPanes one.
  function stopGridPane(key) {
    if (streamPanes.has(key)) {
      streamPanes.delete(key); // before the stop, so a racing hook finds nothing
      window.harness.streamPaneStop(key);
      return;
    }
    stopPane(key);
    const p = previewPanes.get(key);
    if (p) disposePaneTerm(p);
    previewPanes.delete(key);
  }

  // streamPanes holds the grid cells over event-stream sessions, keyed like
  // previewPanes ("grid:<taskId>"): each is a createStreamView over its own
  // cowrite attach (harness.streamPaneStart), so a cell answers an approval or
  // a question the way the chat does.
  const streamPanes = new Map();

  window.harness_streamPaneOpen = (key) => {
    const c = streamPanes.get(key);
    if (c) c.view.opened();
  };
  window.harness_streamPaneLine = (key, line, display) => {
    const c = streamPanes.get(key);
    if (c) c.view.line(line, display);
  };
  window.harness_streamPaneClosed = (key, err) => {
    const c = streamPanes.get(key);
    if (c) c.view.closed(err);
  };

  // buildStreamCell fills a grid cell's body with a stream session's
  // transcript, its pending request's control, and a turn box, and attaches it.
  // The header label carries the kind, and ⚑ while a request waits.
  function buildStreamCell(id, key, body, label) {
    body.classList.add("grid-stream-body");
    const log = document.createElement("div");
    log.className = "chat-log";
    const approval = document.createElement("div");
    approval.className = "chat-approval";
    approval.hidden = true;
    const input = document.createElement("textarea");
    input.rows = 1;
    input.placeholder = "message the agent… (Enter 送信 · Esc 中断)";
    const row = document.createElement("div");
    row.className = "chat-input-row";
    row.appendChild(input);
    const status = document.createElement("div");
    status.className = "chat-status";
    body.append(log, approval, row, status);
    const paintLabel = (req) => {
      label.textContent = id.slice(0, 8) + " ‹stream›" + (req ? " ⚑" : "");
    };
    paintLabel(null);
    const view = createStreamView({ log, approval, status }, () => id, paintLabel);
    wireStreamInput(input, view);
    streamPanes.set(key, { view, input });
    view.setStatus("attaching…");
    window.harness.streamPaneStart(key, id).catch((err) => {
      if (streamPanes.get(key)?.view === view) view.setStatus("attach failed: " + err.message);
    });
  }

  // liveSessionIds returns every live (Running/Detached) session id,
  // activity-desc (same ordering as the task list). Used by the `grid`
  // command's no-arg path; the show button reads lastTasks directly.
  async function liveSessionIds() {
    const snap = await window.harness.snapshot();
    return (snap.tasks || [])
      // isSessionKind: feeds the same grid as above.
      .filter((t) => isSessionKind(t) && (t.status === "Running" || t.status === "Detached"))
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
    gridCrop = cropDefault(paneNarrow.matches, gridCropChoice);
    paintGridMode();
    const capped = [...new Set(ids)].slice(0, 9); // dedupe (same id twice must not orphan a cell), then pane cap (v1)
    for (const id of capped) {
      const key = "grid:" + id;
      const stream = isStreamKind((lastTasks || []).find((t) => t.id === id));
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
      // A stream session has no terminal to reattach: the cell's ↪ opens its
      // chat, which is that kind's full-size view.
      attach.textContent = stream ? "💬" : "↪";
      attach.title = stream ? "チャット" : "リアタッチ";
      attach.addEventListener("click", () => {
        closeSessionGrid();
        if (stream) openChatFor(id); else reattachTo(id);
      });
      const notify = document.createElement("button");
      notify.type = "button";
      notify.className = "grid-cell-btn";
      notify.textContent = "🔔";
      notify.title = "idleで通知";
      notify.addEventListener("click", async () => {
        try {
          const r = await window.harness.awaitIdle({ taskId: id, sink: "notify" });
          appendCmdOutput(`await-idle ${id.slice(0, 12)}: ${r.status}${r.watcherId ? ` (watcher ${r.watcherId})` : ""}`, true);
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
      gridKeys.push(key);

      if (stream) {
        buildStreamCell(id, key, body, label);
        continue;
      }
      const p = {
        taskId: id, live: false, epoch: 0, term: null,
        bodyEl: body, scaleBox: null, spacer: null, noteEl: null,
        onResize: null, onClosed: null,
        key, capReplay: true, // small crop: 128 KiB of replay is plenty
        cropped: () => gridCrop,
        follow: true, followRaf: 0, cropAt: null, paintFollow: null,
      };
      // ⤓ appears once the operator pans a cropped cell away from the
      // bottom-left (wireCropBody) — the TUI's ↑N/→M marker and `0` key, as
      // one button.
      const follow = document.createElement("button");
      follow.type = "button";
      follow.className = "grid-cell-btn";
      follow.textContent = "⤓";
      follow.title = "左下に戻して追従";
      p.paintFollow = () => { follow.hidden = !(gridCrop && !p.follow); };
      p.paintFollow();
      follow.addEventListener("click", () => refollowPane(p));
      // ⏸/▶ is the single preview's: pause closes this cell's stream and
      // keeps its frame, ▶ re-attaches at the current screen. onClosed
      // repaints it when the stream ends on its own, whose note says ▶.
      const pause = document.createElement("button");
      pause.type = "button";
      pause.className = "grid-cell-btn";
      p.onClosed = () => paintPauseBtn(pause, p);
      pause.addEventListener("click", () => {
        togglePaneLive(key, p);
        paintPauseBtn(pause, p);
      });
      head.insertBefore(follow, attach);
      head.insertBefore(pause, attach);
      wireCropBody(body, () => p);
      previewPanes.set(key, p);
      startPane(key, p);   // p.live flips true synchronously before the first await
      paintPauseBtn(pause, p);
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
  // liveSessionTasks + gridExcluded pair the show button uses: the set
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
    const ids = liveSessionTasks()
      .filter((t) => inSet.has(t.id) && !gridExcluded.has(t.id))
      .sort((a, b) => taskActivityMs(b) - taskActivityMs(a))
      .map((t) => t.id);
    if (ids.length === 0) {
      return `grid ${label}: no live session in this set (${inSet.size} task(s) in it)`;
    }
    openSessionGrid(ids, label);
    return `grid ${label}: ${ids.length} pane(s)` + (ids.length > 9 ? " (capped at 9)" : "");
  }

  function dismissPane(key, cell) {
    stopGridPane(key);
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
  // Esc in a stream cell's turn box interrupts that agent (wireStreamInput);
  // it must not ALSO close the grid. The keydown's preventDefault is not
  // relied on alone: the dialog's cancel is the close request itself.
  gridModal.addEventListener("cancel", (ev) => {
    const el = document.activeElement;
    if (el && el.closest && el.closest(".grid-stream-body")) ev.preventDefault();
  });
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
    // The rebuild below wipes all sheet DOM, which would reset any agent
    // dropdown the user changed — every 5s poll. Capture those picks first and
    // restore them after the rebuild.
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
      line1.append(dot, statusEl);
      // What the task's hooks report, then what its program reports (OSC
      // 7501), side by side so a disagreement is on the row (D3, D28). Red for
      // the states that ask for the operator, gray for the ones that do not
      // need a look (work, prog working), the default task color for the ones
      // waiting for the next prompt (ready, prog idle / done).
      for (const b of [taskLifecycleModel(t), taskProgramModel(t)]) {
        if (!b) continue;
        const el = document.createElement("span");
        el.className = "task-lifecycle";
        el.style.color = b.attention ? taskStatusColor("Failed") : b.quiet ? "#888" : taskStatusColor("");
        el.textContent = b.text;
        if (b.tooltip) el.title = b.tooltip;
        line1.appendChild(el);
      }
      line1.appendChild(repoEl);
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
      // The bound, always — the default too, as ls prints it.
      if (t.delegateCaps !== undefined) metaText += `  delegate=${t.delegateCaps} delegate-scope=${t.delegateScope}`;
      // Who last granted that authority, last in the authority group as on an
      // ls row (cli.AuthorityLabel, rendered in Go; `unknown` included).
      if (t.authority) metaText += `  granter=${t.authority}`;
      // A held task's remaining window. Present only when there IS a hold —
      // a task with none has nothing to describe here, unlike the observer
      // counts below, whose zeros are measurements. Counted down from the raw
      // deadline rather than shown as a timestamp: the operator's question is
      // "how long have I got", and this is the surface they act on.
      if (t.holdDeadline) {
        const leftMs = t.holdDeadline / 1e6 - Date.now();
        metaText += `  held_for=${leftMs > 0 ? Math.ceil(leftMs / 1000) + "s" : "expired"}`;
      }
      // Who is on the live session, cowriters first — same wording and the same
      // always-printed rule as the CLI row: zeros included, so "nobody is
      // watching" never looks like "this row does not report watchers". This is
      // the surface where it matters most: opening the preview here IS an
      // attach, and it does not move the task out of Detached, so without this
      // the page gives no sign anyone is on it.
      if (taskSessionAlive(t)) {
        metaText += `  cowrite=${t.cowriters || 0} viewer=${t.viewers || 0}`;
      }
      // Commands running in the task's WORKTREE. A different subject from the
      // session above and NOT gated on one — a task that ended with
      // uncommitted work keeps its tree and can still be exec'd — and printed
      // at zero for the same reason the pair above is: 0 is a measurement,
      // absence is not.
      metaText += `  execs=${t.execCount || 0}`;
      meta.textContent = metaText;
      if (t.errorMsg) {
        const err = document.createElement("span");
        err.className = "task-err";
        err.textContent = `  err=${t.errorMsg}`;
        meta.appendChild(err);
      }

      row.append(line1, meta);
      // What the task says it is doing, then what its program titled itself.
      // textContent only: both strings come from the task.
      const now = taskNowModel(t);
      if (now.progress !== null || now.title !== null) {
        const nowEl = document.createElement("div");
        nowEl.className = "task-now";
        if (now.progress !== null) {
          const p = document.createElement("span");
          // The cleared marker is a class, not the task's text: gray, and
          // distinct from any line a task could write.
          p.className = now.cleared ? "task-progress-cleared" : "task-progress";
          p.textContent = now.cleared ? "(cleared)" : now.progress;
          const a = document.createElement("span");
          a.className = "task-progress-age";
          a.textContent = ` ${now.age}`;
          nowEl.append(p, a);
        }
        if (now.title !== null && now.title !== "") {
          const ti = document.createElement("span");
          ti.className = "task-title";
          ti.textContent = now.title;
          nowEl.appendChild(ti);
        }
        row.appendChild(nowEl);
      }
      if (t.prompt) {
        const promptEl = document.createElement("div");
        promptEl.className = "task-prompt";
        promptEl.textContent = t.prompt;
        row.appendChild(promptEl);
      }
      const sheet = document.createElement("div");
      sheet.className = "task-sheet";
      sheet.dataset.taskId = t.id;
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
  // Each item runs its harness call and switches tabs where relevant.
  function buildTaskSheet(sheet, t) {
    const isTerminal = TERMINAL_STATES.has(t.status);
    const addItem = (label, cls, fn) => {
      const item = document.createElement("button");
      item.type = "button";
      item.className = "task-action" + (cls ? " " + cls : "");
      item.textContent = label;
      item.addEventListener("click", () => fn());
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

    // The idle-notify item is shared by both live kinds: await-idle watches
    // output quiescence, which a stream session has as much as a PTY one.
    const addIdleNotify = () => addItem("🔔 idleで通知", "", async () => {
      try {
        const r = await window.harness.awaitIdle({ taskId: t.id, sink: "notify" });
        appendCmdOutput(`await-idle ${t.id.slice(0, 12)}: ${r.status}${r.watcherId ? ` (watcher ${r.watcherId})` : ""}`, true);
      } catch (e) {
        appendCmdOutput(`await-idle: ${e.message}`, true);
      }
    });
    const liveSession = t.status === "Running" || t.status === "Detached";

    // Grid include/exclude toggle (default included), for both live kinds — the
    // grid tiles both. Updates its own label in place so the whole sheet need
    // not re-render on each click; the global "グリッド表示" button reads the
    // same gridExcluded set.
    const addGridToggle = () => {
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
    };

    // A live event-stream session: the chat is this kind's Reattach — turns
    // and approvals — and there is no terminal to preview. Until this was
    // added the sheet offered a stream task nothing at all, so the only way to
    // open its chat was typing `session stream attach <id>`.
    if (isStreamKind(t) && liveSession) {
      addItem("💬 チャット", "", () => openChatFor(t.id));
      addGridToggle();
      addIdleNotify();
    }

    // Reattach / Preview / grid-include toggle / idle-notify — live interactive
    // session only. Order: Reattach first, then Preview, then the grid toggle.
    if (isPTYKind(t) && liveSession) {
      addItem("↪ Reattach", "", () => reattachTo(t.id));
      addItem("🔍 プレビュー", "", () => openSessionPreview(t.id));
      addGridToggle();
      addIdleNotify();
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
    // await-progress on any task that can still write, oneshot included —
    // the reason the verb is top-level rather than under `session`. The
    // baseline is the row's progressAt (exact decimal string), so a line
    // written since the list was drawn fires at once.
    if (canArmProgress(t)) {
      addItem("📝 進捗で通知", "", async () => {
        try {
          const r = await window.harness.awaitProgress({ taskId: t.id, since: t.progressAt || "0", sink: "notify" });
          appendCmdOutput(`await-progress ${t.id.slice(0, 12)}: ${r.status}${r.watcherId ? ` (watcher ${r.watcherId})` : ""}`, true);
        } catch (e) {
          appendCmdOutput(`await-progress: ${e.message}`, true);
        }
      });
    }

    // Empty a stale line its task will not replace. The server checks cancel
    // and its scope; a refusal is reported like any other.
    if (canClearProgress(t)) {
      addItem("🧹 進捗を消す", "", async () => {
        try {
          const r = await window.harness.progressClear(t.id);
          appendCmdOutput(`clear progress ${t.id.slice(0, 12)}: ${r.status}`, true);
          refreshSnapshot();
        } catch (e) {
          appendCmdOutput(`clear progress: ${e.message}`, true);
        }
      });
    }

    addItem("▦ この配下をグリッド", "", async () => {
      appendCmdOutput(await openGridSet({ mode: "subtree", anchor: t.id }), true);
    });
    addItem("▦ 配下のみ (自分を除く)", "", async () => {
      appendCmdOutput(await openGridSet({ mode: "descendants", anchor: t.id }), true);
    });

    // Resume — finished task's worktree, opened as a fresh interactive session.
    // Reflect the Compose "Extra agent args" box (same as Submit / Open) so a
    // resume can carry --permission-mode etc. without going through the cmdline.
    // Assigned variants mirror the TUI's r/R; any-runner variants mirror u/U and
    // intentionally skip t.assignedTo so the ambiguous runner picker can reopen.
    if (isTerminal) {
      // assignedTo is the runner's 32-hex IDENTITY and the bridge sends "" when
      // there is none, so presence is the whole test. The old guard read
      // `!startsWith(":")` because an absent ADDRESS-shaped id rendered as
      // ":0:0-0"; an absent identity rendered as 32 zeros instead, which that
      // guard passes — a pinned resume would then ask for runner 000…0.
      const assignedRunner = typeof t.assignedTo === "string" ? t.assignedTo : "";

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

      const doResume = async (agentArgs, note, resumeConversation = false, runner = "") => {
        const req = sessionReq({ agent: agentSel.value || "", agentArgs, resumeTaskId: t.id, resumeConversation, runner });
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
        addItem("▶ Resume assigned", "", () => doResume(currentAgentArgs(), "resumed assigned", false, assignedRunner));
        addItem("▶ Resume conversation assigned", "", () => doResume(currentAgentArgs(), "resumed conversation assigned", true, assignedRunner));
      }
      addItem("▶ Resume any runner", "", () => doResume(currentAgentArgs(), "resumed any runner"));
      addItem("▶ Resume conversation any runner", "", () => doResume(currentAgentArgs(), "resumed conversation any runner", true));
    }

    // Files — always available.
    // Live re-grant. Shown for every task: a WebUI connection is an operator
    // connection by construction (the PSK gate makes any non-agent client
    // prove operatorPSK), so there is no non-operator state to hide it in.
    addItem("🌳 ツリーで見る", "", () => {
      if (!showTaskInTree(t.id)) appendCmdOutput(`tree: task ${t.id.slice(0, 8)} is not in the tree`, true);
    });
    addItem("🎯 到達範囲を見る", "", () => enterReachMode(t.id));
    addItem("🔮 caps set / spawn をプレビュー", "", () => openRegrantDialog(t, { whatIf: true }));
    addItem("🔑 caps/scope 再付与", "", () => openRegrantDialog(t));
    addItem("⇄ 親タスク変更", "", () => openParentDialog(t));

    // Runs in the task's WORKTREE as its own process — not in the session's
    // shell. Available for every task, including terminal ones: the gate is
    // whether the tree is still on disk, which a task that ended with
    // uncommitted work keeps.
    //
    // A text prompt rather than a built control, and deliberately: a command
    // line IS free text, unlike caps or a scope, which decompose into things
    // this form already has chips and radios for.
    addItem("⌨ コマンド実行", "", async () => {
      const line = window.prompt(`${t.id.slice(0, 12)}… の worktree で実行するコマンド (シェルで解釈)`, "git status");
      if (!line || !line.trim()) return;
      setActiveTab("tasks");
      appendCmdOutput(`> exec ${t.id.slice(0, 12)}… ${line}`, true);
      try {
        // The LINE goes to the runner's shell, not a split argv. A one-line
        // prompt labelled "command" is the shell case by construction: nobody
        // types an argv into one, and `ls | wc -l` — the first thing anyone
        // tries — reached ls with a literal "|" as an argument until this.
        // The typed-argv form lives on the command line, after `--`.
        appendCmdOutput(await execRunToOutput(t.id, [line], { shell: true }));
      } catch (err) {
        appendCmdOutput(`exec error: ${err.message}`);
      }
      refreshSnapshot();
    });

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
    const openForm = document.getElementById("board-open-form");
    if (openForm) openForm.hidden = false;
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
          const lastTime = boardTimeLabel(t.lastPublishedAtMs);
          // retracted= shows only when the topic holds withdrawn messages, and
          // is never folded into msgs= — that count answers "how much would a
          // subscriber receive", so a topic emptied by retraction reads msgs=0
          // while still saying there is something here to audit.
          const retracted = t.retractedCount
            ? `  retracted=${t.retractedCount}`
            : "";
          metaSpan.textContent = `msgs=${t.msgCount}${retracted}${subs}${boardRetainMeta(t)}  last=${lastTime}`;
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
    // A different topic starts a fresh composer; re-reading the same one (a
    // refresh after a send) leaves what is being written alone.
    if (topic !== currentBoardTopic && typeof resetBoardCompose === "function") resetBoardCompose();
    // A membership result belongs to the topic it changed, and so does a
    // flush / archive result.
    if (topic !== currentBoardTopic) {
      boardMemberStatus = "";
      boardDetailSay("", false);
    }
    currentBoardTopic = topic;
    boardTopicsEl.hidden = true;
    boardDetailEl.hidden = false;
    const openForm = document.getElementById("board-open-form");
    if (openForm) openForm.hidden = true;
    boardDetailTitle.textContent = topic;
    refreshBoardRetainBadge(topic);
    // Offered only where the server would accept it: chat.* topics stay
    // temporary, and a button that only ever answers "refused" reads as if
    // the topic could be kept.
    // Archive follows the same rule: a chat.* topic is never archived.
    const archiveBtn = document.getElementById("board-archive-btn");
    if (archiveBtn) {
      const ok = !!(window.harness && window.harness.boardRetainable(topic));
      archiveBtn.disabled = !ok;
      archiveBtn.title = ok
        ? "ここまでの履歴を追記されないアーカイブに固定する。topic は変わらない (board archive)"
        : "chat.* topics stay temporary and cannot be archived";
    }
    const retainBtn = document.getElementById("board-retain-btn");
    if (retainBtn) {
      const ok = !!(window.harness && window.harness.boardRetainable(topic));
      retainBtn.disabled = !ok;
      retainBtn.title = ok
        ? "Keep this topic past the TTL and a server restart (board retain)"
        : "chat.* topics stay temporary and cannot be retained";
    }
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
      // No early return on an empty ring: a flushed topic holds nothing in
      // memory and everything on disk, which the older button reaches.
      if (!r.msgs || r.msgs.length === 0) {
        const note = document.createElement("div");
        note.className = "board-empty-note";
        note.textContent = "(no messages in the ring)";
        boardMessagesEl.appendChild(note);
        r.msgs = [];
      }
      const renderCard = (m) => {
        const card = document.createElement("div");
        card.className = "board-msg";
        // A withdrawn message — by its author (`agent retract`) or by a
        // purge-capable caller (the ⊘ button below / `board retract`). It
        // reaches no agent any more; this pane is the only place it still
        // exists, so it is marked and dimmed rather than hidden — hiding it
        // would give the operator the same blank the agents get, which defeats
        // the point of keeping it.
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

        // Same rule as re= above: only a sender that declared a reply
        // destination gets this. It is a plain topic name, not a seq, so it
        // needs none of inReplyTo's string handling.
        const replyToSpan = document.createElement("span");
        replyToSpan.className = "board-msg-reply-to";
        if (m.replyToTopic) {
          replyToSpan.textContent = `reply-to=${m.replyToTopic}`;
        }

        const fromSpan = document.createElement("span");
        fromSpan.className = "board-msg-from";
        // senderParty is decided in Go (cli.SenderParty): "operator" /
        // "server" by kind, else the 8-hex prefix.
        fromSpan.textContent = `from=${m.senderParty || "-"}`;

        const hostSpan = document.createElement("span");
        hostSpan.className = "board-msg-host";
        hostSpan.textContent = `host=${m.fromHostname || "-"}`;

        const agentSpan = document.createElement("span");
        agentSpan.className = "board-msg-agent";
        agentSpan.textContent = `agent=${m.agentProfile || "-"}`;

        const timeSpan = document.createElement("span");
        timeSpan.className = "board-msg-time";
        timeSpan.textContent = boardTimeLabel(m.receivedAtMs);

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
          // by= rides with the badge: two verbs can withdraw a message now, so
          // a bare RETRACTED would read as the author having done it whoever
          // actually did. The label is rendered in Go (cli.RetractedByLabel)
          // so this pane and the CLI cannot drift apart on the wording.
          retractedSpan.textContent = `RETRACTED ${when} by=${m.retractedBy || "unknown"}`;
        }

        // Retract: withdraw from every agent path, keep it readable here.
        // Offered only on a live message — a second retract of the same seq
        // answers not_found, which would read as a failure rather than as
        // "already withdrawn".
        const retractBtn = document.createElement("button");
        retractBtn.className = "board-msg-retract";
        retractBtn.textContent = "⊘";
        retractBtn.title = `Retract message #${m.seq} (leaves every agent path; stays readable here)`;
        retractBtn.addEventListener("click", async (e) => {
          e.stopPropagation();
          if (!window.confirm(
            `Retract message #${m.seq} from "${topic}"?\n\n` +
            `It leaves every agent path immediately, and stays readable here until the topic ages out.`)) return;
          try {
            const r = await window.harness.boardRetract(topic, m.seq);
            if (!r.found) {
              appendCmdOutput(`boardRetract: #${m.seq} not found (already withdrawn, purged, or aged out)`);
            }
            openBoardTopic(topic);
          } catch (err) {
            appendCmdOutput(`boardRetract error: ${err.message}`);
          }
        });

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
        if (replyToSpan.textContent) hdr.appendChild(replyToSpan);
        hdr.appendChild(fromSpan);
        hdr.appendChild(hostSpan);
        hdr.appendChild(agentSpan);
        hdr.appendChild(timeSpan);
        hdr.appendChild(shownSpan);
        if (retractedSpan.textContent) hdr.appendChild(retractedSpan);
        if (!m.retracted) hdr.appendChild(retractBtn);
        hdr.appendChild(purgeBtn);

        // Reply as the operator. Offered on withdrawn messages too: replying
        // destroys nothing.
        const replyBtn = document.createElement("button");
        replyBtn.className = "board-msg-reply-btn";
        replyBtn.textContent = "↩";
        replyBtn.title = `Reply to #${m.seq} as the operator`;
        replyBtn.addEventListener("click", (e) => {
          e.stopPropagation();
          armBoardReply(m.seq);
        });
        hdr.appendChild(replyBtn);

        const pre = document.createElement("pre");
        pre.textContent = prettyPayload(m.payload || "");

        card.appendChild(hdr);
        card.appendChild(pre);
        return card;
      };
      // Older than what was read: a retained topic keeps its whole log on
      // disk, past the ring's 64 messages (board read --before).
      const older = document.createElement("button");
      older.type = "button";
      older.className = "board-older-btn";
      older.textContent = "さらに古いメッセージを読み込む";
      let oldest = boardOlderCursor(r.msgs.map((m) => m.seq));
      older.addEventListener("click", async () => {
        older.disabled = true;
        try {
          const p = await window.harness.boardRead(topic, oldest, 64);
          if (topic !== currentBoardTopic) return;
          if (!p.msgs || p.msgs.length === 0) {
            older.textContent = "これより古いメッセージはありません(64 件より前が残るのは retain した topic だけ)";
            return;
          }
          const first = older.nextSibling;
          for (const m of p.msgs) boardMessagesEl.insertBefore(renderCard(m), first);
          oldest = boardMinSeq(p.msgs.map((m) => m.seq));
          older.disabled = false;
        } catch (err) {
          appendCmdOutput(`board read (older) error: ${err.message}`);
          older.disabled = false;
        }
      });
      boardMessagesEl.appendChild(older);
      for (const m of r.msgs) boardMessagesEl.appendChild(renderCard(m));
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

  // ── Board membership (board subscribe / join / unsubscribe / leave) ──────
  // The result of the last request, shown in the subscribers panel it
  // changed: cmd-output is not on screen while the board is.
  let boardMemberStatus = "";
  async function boardMembershipApply(ids, op) {
    const topic = currentBoardTopic;
    const lines = [];
    for (const id of ids) {
      try {
        const res = await window.harness.boardMembership(id, topic, op);
        lines.push(res.label);
      } catch (err) {
        // A refusal already names the op, task and topic, as the CLI's does.
        const named = err.message.startsWith(`board ${op} `);
        lines.push(named ? err.message : `board ${op} ${id.slice(0, 8)} ${topic}: ${err.message}`);
      }
    }
    lines.forEach((l) => appendCmdOutput(l));
    // Shown in the panel only while it still shows the topic it changed.
    if (topic !== currentBoardTopic) return;
    boardMemberStatus = lines.join(" / ");
    renderBoardSubscribers();
  }

  // 購読させる / 参加させる: pick tasks from the checklist the spawn form uses.
  const boardMemberModal = document.getElementById("board-member-modal");
  let boardMemberOp = "";
  let boardMemberIds = new Set();
  function openBoardMemberDialog(op) {
    if (!boardMemberModal || !currentBoardTopic) return;
    boardMemberOp = op;
    boardMemberIds = new Set();
    document.getElementById("board-member-title").textContent =
      `${op === "join" ? "参加させる" : "購読させる"}: ${currentBoardTopic}`;
    document.getElementById("board-member-hint").textContent = op === "join"
      ? "member になる: 購読し、この topic に投稿でき、投稿への返信はこの topic に届く (board join)"
      : "購読だけ: 読めるようになるだけで、投稿権は増えない (board subscribe)";
    buildTaskChecklist(document.getElementById("board-member-tasks"), boardMemberIds, "", () => {});
    boardMemberModal.showModal();
  }
  if (boardMemberModal) {
    document.getElementById("board-member-apply").addEventListener("click", () => {
      boardMemberModal.close();
      if (boardMemberIds.size) boardMembershipApply([...boardMemberIds], boardMemberOp);
    });
    document.getElementById("board-member-cancel").addEventListener("click", () => boardMemberModal.close());
  }
  function boardMemberActions() {
    const bar = document.createElement("div");
    bar.className = "board-sub-actions";
    for (const [op, label, title] of [
      ["subscribe", "購読させる", "Subscribe tasks to this topic: they read it, nothing more (board subscribe)"],
      ["join", "参加させる", "Make tasks members: subscribed, may publish here, answers to their posts stay here (board join)"],
    ]) {
      const b = document.createElement("button");
      b.type = "button";
      b.textContent = label;
      b.title = title;
      b.addEventListener("click", () => openBoardMemberDialog(op));
      // Offered only where the server could accept it: no task joins chat.*.
      if (op === "join" && !window.harness.boardJoinable(currentBoardTopic)) {
        b.disabled = true;
        b.title = "chat.* topics cannot be joined: direct messages have no members";
      }
      bar.appendChild(b);
    }
    return bar;
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
    const topic = currentBoardTopic;
    try {
      const rows = await window.harness.boardSubscribers(topic);
      // The operator may have opened another topic meanwhile: these rows, and
      // the buttons built from them, belong to the old one.
      if (topic !== currentBoardTopic) return;
      const n = rows ? rows.length : 0;
      boardSubscribersEl.appendChild(
        boardSubHeading(`subscribers of ${currentBoardTopic} (${n})`));
      boardSubscribersEl.appendChild(boardMemberActions());
      if (boardMemberStatus) {
        const st = document.createElement("div");
        st.className = "board-sub-status";
        st.textContent = boardMemberStatus;
        boardSubscribersEl.appendChild(st);
      }
      applyBoardReplyDefault(rows || []);
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
          ? r.patterns.map(boardPatternLabel).join(",")
          : "-";
        const removal = boardRowRemoval(r, currentBoardTopic);
        // The open topic's membership first: the topics list wraps, and this
        // is what the panel is read for.
        const joined = removal.op === "leave" ? "  joined" : "";
        line.appendChild(document.createTextNode(`\u2022 ${id}${joined}  host=${host}  agent=${agent}  topics=${pats} `));
        const rm = document.createElement("button");
        rm.type = "button";
        rm.className = "board-sub-remove";
        rm.textContent = removal.label;
        rm.title = removal.op === "leave"
          ? `End ${id}'s membership of ${currentBoardTopic} and its subscription (board leave)`
          : `Unsubscribe ${id} from ${currentBoardTopic} (board unsubscribe)`;
        rm.addEventListener("click", () => boardMembershipApply([r.taskId], removal.op));
        line.appendChild(rm);
        boardSubscribersEl.appendChild(line);
        // The task's reply routes -- what it may publish to without
        // board_peer beyond its lineage -- on a line of their own that wraps:
        // there can be 64 of them. "-" is a measurement, not missing data.
        // Each carries a forget button, and the line a forget-all one.
        const contacts = document.createElement("div");
        contacts.className = "board-sub-contacts";
        const list = r.contacts || [];
        contacts.appendChild(document.createTextNode(list.length ? "contacts=" : "contacts=-"));
        const forget = async (topic) => {
          const what = topic ? `contact ${topic}` : `all ${list.length} contact(s)`;
          if (!window.confirm(`Forget ${what} of task ${id}?\n\n` +
            `The task needs board_peer to publish there again until it is addressed from that route.`)) return;
          try {
            const res = await window.harness.boardContactsClear(r.taskId, topic);
            appendCmdOutput(res.label);
          } catch (err) {
            appendCmdOutput(`boardContactsClear error: ${err.message}`);
          }
          renderBoardSubscribers();
        };
        for (const ct of list) {
          const chip = document.createElement("span");
          chip.className = "board-sub-contact";
          chip.appendChild(document.createTextNode(ct));
          const x = document.createElement("button");
          x.type = "button";
          x.className = "board-sub-contact-forget";
          x.textContent = "\u00d7";
          x.title = `Forget contact ${ct} of ${id}`;
          x.addEventListener("click", () => forget(ct));
          chip.appendChild(x);
          contacts.appendChild(chip);
        }
        if (list.length) {
          const all = document.createElement("button");
          all.type = "button";
          all.className = "board-sub-contacts-clear";
          all.textContent = "全消去";
          all.title = `Forget every contact of ${id}`;
          all.addEventListener("click", () => forget(""));
          contacts.appendChild(all);
        }
        boardSubscribersEl.appendChild(contacts);
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

  // ── Board composer / wake (as the operator) ───────────────────────────────
  // The composer sits under the topic's messages; ↩ on a card arms a reply.
  // What it sends is built by boardComposeTarget / boardSendRequest and
  // reported by boardSendResultLine (top level, so node --test reaches them).
  const boardComposeBody = document.getElementById("board-compose-body");
  const boardComposeWake = document.getElementById("board-compose-wake");
  const boardComposeKeep = document.getElementById("board-compose-keep");
  const boardComposeReplyTo = document.getElementById("board-compose-reply-to");
  const boardComposeReplying = document.getElementById("board-compose-replying");
  let boardReplyingSeq = ""; // decimal string; "" = a new message on the open topic
  // The reply-to the composer last set by itself. A subscribers refresh moves
  // the field to the topic's default only while it still holds this, so what
  // the operator typed there is never overwritten.
  let boardAutoReplyTo = "";
  function applyBoardReplyDefault(rows) {
    if (!boardComposeReplyTo || boardComposeReplyTo.value !== boardAutoReplyTo) return;
    boardAutoReplyTo = boardComposeReplyDefault(currentBoardTopic, rows);
    boardComposeReplyTo.value = boardAutoReplyTo;
  }

  // Back to a new message with the defaults, so a reply-to cleared for one
  // note does not silently carry over to the next.
  function resetBoardCompose() {
    boardReplyingSeq = "";
    if (boardComposeReplying) boardComposeReplying.hidden = true;
    const d = boardSendDefaults({});
    if (boardComposeWake) boardComposeWake.checked = d.wake;
    if (boardComposeKeep) boardComposeKeep.checked = d.keep;
    if (boardComposeReplyTo) boardComposeReplyTo.value = d.replyTo;
    boardAutoReplyTo = d.replyTo;
  }
  resetBoardCompose();

  function armBoardReply(seq) {
    boardReplyingSeq = String(seq);
    document.getElementById("board-compose-replying-label").textContent = `↩ #${seq} に返信`;
    boardComposeReplying.hidden = false;
    boardComposeBody.focus();
  }

  async function sendBoardCompose() {
    const body = boardComposeBody.value;
    if (!body.trim() || !window.harness) return;
    // The chains view spans topics, so it has none to send a new message to.
    const target = boardComposeTarget(boardView === "chains" ? null : currentBoardTopic, boardReplyingSeq);
    if (!target) {
      appendCmdOutput("board send: the chains view only replies — press ↩ on the message to answer");
      return;
    }
    const req = boardSendRequest({
      ...target,
      replyTo: boardComposeReplyTo.value.trim(),
      wake: boardComposeWake.checked,
      keep: boardComposeKeep.checked,
      body,
    });
    try {
      const r = await window.harness.boardSend(req);
      appendCmdOutput(boardSendResultLine(r, req));
      // Cleared only on success: a refused send (no_reply_route, say) keeps
      // what was typed.
      boardComposeBody.value = "";
      resetBoardCompose();
    } catch (err) {
      appendCmdOutput(`board send error: ${err.message}`);
    }
    if (boardView === "chains") renderBoardChains();
    else openBoardTopic(currentBoardTopic);
  }

  // The composer is one element shared by both views, moved to whichever is
  // showing, so the two cannot drift apart in what they send.
  const boardComposeEl = document.getElementById("board-compose");
  const boardComposeHomeEl = document.getElementById("board-detail");
  function placeBoardCompose(view) {
    if (!boardComposeEl) return;
    const host = view === "chains" ? document.getElementById("board-chains-view") : boardComposeHomeEl;
    if (host && boardComposeEl.parentElement !== host) {
      host.appendChild(boardComposeEl);
      resetBoardCompose();
    }
    boardComposeBody.placeholder = view === "chains"
      ? "↩ を押したメッセージに operator として返信（Enter 送信 / Shift+Enter 改行）"
      : "operator として送信（Enter 送信 / Shift+Enter 改行）";
  }

  document.getElementById("board-compose-send")?.addEventListener("click", sendBoardCompose);
  // Enter sends; Shift+Enter is a newline -- the stream chat's binding.
  boardComposeBody?.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) { e.preventDefault(); sendBoardCompose(); }
  });
  document.getElementById("board-compose-cancel-reply")?.addEventListener("click", () => {
    boardReplyingSeq = "";
    boardComposeReplying.hidden = true;
  });

  const boardOpenForm = document.getElementById("board-open-form");
  if (boardOpenForm) {
    boardOpenForm.addEventListener("submit", (ev) => {
      ev.preventDefault();
      const input = document.getElementById("board-open-topic");
      const name = boardOpenTopicName(input ? input.value : "");
      if (!name) return;
      if (input) input.value = "";
      openBoardTopic(name);
    });
  }

  const boardRetainBadge = document.getElementById("board-detail-retain");

  // refreshBoardRetainBadge shows the open topic's retention in its header,
  // read back from the server rather than echoed from the request, so what is
  // on screen is what the board holds.
  async function refreshBoardRetainBadge(topic) {
    if (!boardRetainBadge) return;
    const unretainBtn = document.getElementById("board-unretain-btn");
    const retainBtn = document.getElementById("board-retain-btn");
    boardRetainBadge.classList.remove("error");
    boardRetainBadge.textContent = "";
    // Hidden until the server says the topic is retained: offered only where
    // it acts.
    if (unretainBtn) unretainBtn.hidden = true;
    const flushBtnReset = document.getElementById("board-flush-btn");
    if (flushBtnReset) flushBtnReset.hidden = true;
    if (retainBtn) retainBtn.textContent = boardRetainButtonLabel(false);
    if (!window.harness) return;
    try {
      const topics = await window.harness.boardTopics();
      if (topic !== currentBoardTopic) return; // the operator moved on
      const t = (topics || []).find((x) => x.name === topic);
      const retained = !!(t && t.retain);
      boardRetainBadge.textContent = boardRetainMeta(t).trim();
      if (unretainBtn) unretainBtn.hidden = !retained;
      const flushBtn = document.getElementById("board-flush-btn");
      if (flushBtn) flushBtn.hidden = !retained;
      if (retainBtn) retainBtn.textContent = boardRetainButtonLabel(retained);
    } catch (_) {
      // The badge is a nicety; the topic view itself must still render.
    }
  }

  const boardUnretainBtn = document.getElementById("board-unretain-btn");
  if (boardUnretainBtn) {
    boardUnretainBtn.addEventListener("click", async () => {
      if (!currentBoardTopic || !window.harness) return;
      const topic = currentBoardTopic;
      if (!window.confirm(`${topic} の保持を解除しますか？\n（disk の写しを消し、最後の投稿から 30 分で消える通常の topic に戻ります）`)) return;
      try {
        const r = await window.harness.boardUnretain(topic);
        appendCmdOutput(boardRetainResultLine(topic, r.label));
        await refreshBoardRetainBadge(topic);
      } catch (err) {
        appendCmdOutput(`board retain error: ${err.message}`);
        if (boardRetainBadge && topic === currentBoardTopic) {
          boardRetainBadge.classList.add("error");
          boardRetainBadge.textContent = `unretain error: ${err.message}`;
        }
      }
    });
  }

  const boardRetainBtn = document.getElementById("board-retain-btn");
  if (boardRetainBtn) {
    boardRetainBtn.addEventListener("click", async () => {
      if (!currentBoardTopic || !window.harness) return;
      const topic = currentBoardTopic;
      // Typed, because a period is free-form; parsed in Go (harness.boardRetain
      // calls the CLI's parser), so this holds no copy of the grammar.
      const value = window.prompt(`${topic} の保持期間 (例: 72h / forever)`, "168h");
      if (value === null || value.trim() === "") return;
      try {
        const r = await window.harness.boardRetain(topic, value.trim());
        appendCmdOutput(boardRetainResultLine(topic, r.label));
        await refreshBoardRetainBadge(topic);
      } catch (err) {
        appendCmdOutput(`board retain error: ${err.message}`);
        // The command output is not on this tab: say it where the press was.
        if (boardRetainBadge && topic === currentBoardTopic) {
          boardRetainBadge.classList.add("error");
          boardRetainBadge.textContent = `retain error: ${err.message}`;
        }
      }
    });
  }

  // ── Flush and archives ──
  // See docs/superpowers/specs/2026-11-02-agentboard-flush-archive-design.md.
  const boardDetailStatus = document.getElementById("board-detail-status");
  const boardArchivesViewEl = document.getElementById("board-archives-view");
  const boardArchivesListEl = document.getElementById("board-archives-list");
  const boardArchivesTitle = document.getElementById("board-archives-title");
  const boardArchiveDeleteBtn = document.getElementById("board-archive-delete-btn");
  // archivesFilter is the topic the archive list was opened for ("" = all);
  // currentArchive the archive row being read, null on the list.
  let archivesFilter = "";
  let currentArchive = null;

  function boardDetailSay(text, isError) {
    if (!boardDetailStatus) return;
    boardDetailStatus.textContent = text;
    boardDetailStatus.classList.toggle("error", !!isError);
  }

  const boardFlushBtn = document.getElementById("board-flush-btn");
  if (boardFlushBtn) {
    boardFlushBtn.addEventListener("click", async () => {
      if (!currentBoardTopic || !window.harness) return;
      const topic = currentBoardTopic;
      if (!window.confirm(`${topic} の ring を空にしますか？\n（disk の log には全部残ります。「さらに古いメッセージ」で読めます）`)) return;
      try {
        const r = await window.harness.boardFlush(topic);
        const line = boardFlushResultLine(topic, r);
        appendCmdOutput(line);
        await openBoardTopic(topic);
        boardDetailSay(line, false);
      } catch (err) {
        appendCmdOutput(`board flush error: ${err.message}`);
        boardDetailSay(`flush error: ${err.message}`, true);
      }
    });
  }

  const boardArchiveBtn = document.getElementById("board-archive-btn");
  if (boardArchiveBtn) {
    boardArchiveBtn.addEventListener("click", async () => {
      if (!currentBoardTopic || !window.harness) return;
      const topic = currentBoardTopic;
      try {
        const r = await window.harness.boardArchive(topic);
        const line = boardArchiveResultLine(topic, r);
        appendCmdOutput(line);
        boardDetailSay(line, false);
      } catch (err) {
        appendCmdOutput(`board archive error: ${err.message}`);
        boardDetailSay(`archive error: ${err.message}`, true);
      }
    });
  }

  // renderBoardArchives lists archives of topic, or every archive for "".
  async function renderBoardArchives(topic) {
    archivesFilter = topic || "";
    currentArchive = null;
    // Archives belong to the Topics side: opened from Chains, the view
    // switches, so Refresh re-reads this list and Back lands on Topics with
    // the Topics button lit.
    boardView = "topics";
    if (boardViewTopicsBtn) boardViewTopicsBtn.classList.add("is-active");
    if (boardViewChainsBtn) boardViewChainsBtn.classList.remove("is-active");
    if (boardExportBtn) boardExportBtn.hidden = true;
    if (typeof placeBoardCompose === "function") placeBoardCompose("topics");
    if (boardTopicsViewEl) boardTopicsViewEl.hidden = true;
    if (boardChainsViewEl) boardChainsViewEl.hidden = true;
    boardArchivesViewEl.hidden = false;
    boardArchiveDeleteBtn.hidden = true;
    boardArchivesTitle.textContent = `archives of ${archivesFilter || "all topics"}`;
    boardArchivesListEl.innerHTML = "";
    if (!window.harness) {
      boardArchivesListEl.textContent = "(not connected)";
      return;
    }
    try {
      const rows = await window.harness.boardArchives(archivesFilter);
      if (!rows || rows.length === 0) {
        boardArchivesListEl.textContent = "(no archives — 🗄 Archive on a topic makes one)";
        return;
      }
      for (const row of rows) {
        const el = document.createElement("div");
        el.className = "board-topic-row board-archive-row";
        // The CLI's line, rendered in Go: one spelling on every surface.
        el.textContent = row.summary;
        el.addEventListener("click", () => openBoardArchive(row));
        boardArchivesListEl.appendChild(el);
      }
    } catch (err) {
      boardArchivesListEl.textContent = `error: ${err.message}`;
    }
  }

  // openBoardArchive reads one archive: newest page first, an older button
  // above, purge per message. Nothing else changes an archive.
  async function openBoardArchive(row) {
    currentArchive = row;
    boardArchivesTitle.textContent = `archive ${row.id} of ${row.topic}`;
    boardArchiveDeleteBtn.hidden = false;
    boardArchivesListEl.innerHTML = "";
    const card = (m) => {
      const el = document.createElement("div");
      el.className = "board-msg";
      if (m.retracted) el.classList.add("board-msg-retracted");
      const hdr = document.createElement("div");
      hdr.className = "board-msg-header";
      const parts = [`#${m.seq}`];
      if (m.inReplyTo && m.inReplyTo !== "0") parts.push(`re=${m.inReplyTo}`);
      parts.push(`from=${m.senderParty || "-"}`, `agent=${m.agentProfile || "-"}`, boardTimeLabel(m.receivedAtMs));
      if (m.retracted) parts.push(`RETRACTED by=${m.retractedBy || "unknown"}`);
      const label = document.createElement("span");
      label.textContent = parts.join("  ");
      const purgeBtn = document.createElement("button");
      purgeBtn.className = "board-msg-purge";
      purgeBtn.textContent = "✕";
      purgeBtn.title = `Purge message #${m.seq} from archive ${row.id}`;
      purgeBtn.addEventListener("click", async (e) => {
        e.stopPropagation();
        if (!window.confirm(`Purge message #${m.seq} from archive ${row.id}?`)) return;
        try {
          await window.harness.boardArchivePurge(row.id, m.seq);
          openBoardArchive(row);
        } catch (err) {
          appendCmdOutput(`board purge --archive error: ${err.message}`);
        }
      });
      hdr.appendChild(label);
      hdr.appendChild(purgeBtn);
      const pre = document.createElement("pre");
      pre.textContent = prettyPayload(m.payload || "");
      el.appendChild(hdr);
      el.appendChild(pre);
      return el;
    };
    try {
      const r = await window.harness.boardArchiveRead(row.id, "0", 64);
      if (currentArchive !== row) return;
      if (!r.found) {
        boardArchivesListEl.textContent = "(this archive no longer exists)";
        return;
      }
      const older = document.createElement("button");
      older.type = "button";
      older.className = "board-older-btn";
      older.textContent = "さらに古いメッセージを読み込む";
      let oldest = boardOlderCursor(r.msgs.map((m) => m.seq));
      older.addEventListener("click", async () => {
        older.disabled = true;
        try {
          const p = await window.harness.boardArchiveRead(row.id, oldest, 64);
          if (currentArchive !== row) return;
          if (!p.msgs || p.msgs.length === 0) {
            older.textContent = "これより古いメッセージはありません";
            return;
          }
          const first = older.nextSibling;
          for (const m of p.msgs) boardArchivesListEl.insertBefore(card(m), first);
          oldest = boardOlderCursor(p.msgs.map((m) => m.seq));
          older.disabled = false;
        } catch (err) {
          appendCmdOutput(`board read --archive (older) error: ${err.message}`);
          older.disabled = false;
        }
      });
      boardArchivesListEl.appendChild(older);
      for (const m of r.msgs) boardArchivesListEl.appendChild(card(m));
    } catch (err) {
      boardArchivesListEl.textContent = `error: ${err.message}`;
    }
  }

  const boardArchivesBtn = document.getElementById("board-archives-btn");
  if (boardArchivesBtn) boardArchivesBtn.addEventListener("click", () => renderBoardArchives(""));
  const boardTopicArchivesBtn = document.getElementById("board-topic-archives-btn");
  if (boardTopicArchivesBtn) {
    boardTopicArchivesBtn.addEventListener("click", () => {
      if (currentBoardTopic) renderBoardArchives(currentBoardTopic);
    });
  }
  const boardArchivesBackBtn = document.getElementById("board-archives-back-btn");
  if (boardArchivesBackBtn) {
    boardArchivesBackBtn.addEventListener("click", () => {
      if (currentArchive) {
        renderBoardArchives(archivesFilter);
        return;
      }
      boardArchivesViewEl.hidden = true;
      if (boardTopicsViewEl) boardTopicsViewEl.hidden = false;
      if (archivesFilter) openBoardTopic(archivesFilter);
      else renderBoardTopics();
    });
  }
  if (boardArchiveDeleteBtn) {
    boardArchiveDeleteBtn.addEventListener("click", async () => {
      const row = currentArchive;
      if (!row || !window.harness) return;
      if (!window.confirm(`アーカイブ ${row.id}（${row.topic}、${row.count} 件）を削除しますか？\n（ファイルごと消え、元に戻せません）`)) return;
      try {
        const r = await window.harness.boardArchivePurge(row.id, "0");
        appendCmdOutput(`board purge --archive ${row.id}: purged=${r.purged}`);
        renderBoardArchives(archivesFilter);
      } catch (err) {
        appendCmdOutput(`board purge --archive error: ${err.message}`);
      }
    });
  }

  const boardWakeBtn = document.getElementById("board-wake-btn");
  if (boardWakeBtn) {
    boardWakeBtn.addEventListener("click", async () => {
      if (!currentBoardTopic || !window.harness) return;
      try {
        const r = await window.harness.boardWake(currentBoardTopic);
        appendCmdOutput(`board wake: woke ${r.woken} task(s) on ${currentBoardTopic}`);
      } catch (err) {
        appendCmdOutput(`board wake error: ${err.message}`);
      }
    });
  }

  // ── Agentboard chain view ─────────────────────────────────────────────────
  // The second view of the same board. Topics answers "what is on this topic";
  // this follows in_reply_to across topics, which is the only way to see a
  // whole exchange: each agent receives on its own chat.<short-id>, so a
  // conversation is split across at least two topics by construction.
  //
  // The walk is NOT here. harness.boardThread() returns rows already in draw
  // order with depth, the last-child flags and the rendered gutter decided in
  // Go, and this code only paints them. Following inReplyTo in JS would be a
  // second walk, free to disagree with the one the CLI and TUI draw.
  //
  // Seqs arrive as decimal strings and stay strings: a board seq is ~1.9e18,
  // past Number.MAX_SAFE_INTEGER, so parsing one to a Number rounds it to the
  // nearest ~256 and would silently merge distinct messages.

  const boardTopicsViewEl  = document.getElementById("board-topics-view");
  const boardChainsViewEl  = document.getElementById("board-chains-view");
  const boardChainsWindowEl = document.getElementById("board-chains-window");
  const boardChainsRowsEl  = document.getElementById("board-chains-rows");
  const boardViewTopicsBtn = document.getElementById("board-view-topics");
  const boardViewChainsBtn = document.getElementById("board-view-chains");

  // boardView is which of the two views the Board tab is showing.
  let boardView = "topics";

  function setBoardView(name) {
    boardView = name;
    if (boardTopicsViewEl) boardTopicsViewEl.hidden = name !== "topics";
    if (boardArchivesViewEl) boardArchivesViewEl.hidden = true;
    if (boardChainsViewEl) boardChainsViewEl.hidden = name !== "chains";
    if (boardViewTopicsBtn) boardViewTopicsBtn.classList.toggle("is-active", name === "topics");
    if (boardViewChainsBtn) boardViewChainsBtn.classList.toggle("is-active", name === "chains");
    // Export belongs to the chains view: what it produces is the conversations
    // as one sheet, which the topic view does not assemble.
    if (boardExportBtn) boardExportBtn.hidden = name !== "chains";
    if (typeof placeBoardCompose === "function") placeBoardCompose(name);
    if (name === "chains") renderBoardChains();
    else renderBoardTopics();
  }

  // boardConvActionBtn builds one thread-scoped destructive button.
  //
  // The key is the one the row was GIVEN — Go stamped it on every row — and it
  // goes straight back to the bridge. Nothing here constructs or parses a
  // conversation key: a second implementation in JS would be free to disagree
  // with the Go one about which exchange a message belongs to.
  //
  // Both confirm first, matching the topic purge button beside them. Retract is
  // confirmed too, not only purge: it reaches every message of an exchange
  // across several topics, which is a larger move than the one-message ⊘ even
  // though what it leaves behind is the same.
  function boardConvActionBtn(key, kind) {
    const purge = kind === "purge";
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "board-chain-conv-btn";
    btn.textContent = purge ? "🗑 purge thread" : "⊘ retract thread";
    btn.title = purge
      ? "この会話のメッセージを全トピックから破棄する（operator の表示からも消える・取り消し不可）"
      : "この会話のメッセージを全トピックで撤回する（agent には届かなくなり、ここには残る）";
    btn.addEventListener("click", async (ev) => {
      ev.stopPropagation();
      const what = purge
        ? `Destroy every message of this conversation?\n\n${key}\n\nThis cannot be undone and the board is the only copy — Export first if you want to keep it.`
        : `Withdraw every message of this conversation from the agents?\n\n${key}\n\nThey stay readable here.`;
      if (!window.confirm(what)) return;
      btn.disabled = true;
      try {
        const res = purge
          ? await window.harness.boardPurgeThread(key)
          : await window.harness.boardRetractThread(key);
        // The summary carries every category with its count, zeros included, so
        // the line says what happened rather than only that something did.
        appendCmdOutput(`board ${kind}-thread: ${res.header}\n${res.summary}`);
        if (res.stoppedBy) appendCmdOutput(`board ${kind}-thread stopped: ${res.stoppedBy}`);
        await renderBoardChains();
      } catch (err) {
        appendCmdOutput(`board ${kind}-thread: ${err.message}`);
        btn.disabled = false;
      }
    });
    return btn;
  }

  // renderBoardChains paints the rows the bridge returns.
  async function renderBoardChains() {
    if (!boardChainsRowsEl) return;
    boardChainsRowsEl.innerHTML = "";
    if (!window.harness) {
      boardChainsRowsEl.textContent = "(not connected)";
      return;
    }
    let res;
    try {
      res = await window.harness.boardThread();
    } catch (err) {
      boardChainsRowsEl.textContent = "boardThread failed: " + err;
      return;
    }
    // The window statement comes from Go (cli.ThreadWindowOperator) so this
    // surface cannot drift from the CLI's wording. It is shown even when there
    // are rows: it is what tells a reader that an ORPHAN is the retention
    // window, not a broken view.
    if (boardChainsWindowEl) boardChainsWindowEl.textContent = (res && res.window) || "";
    // Stash what this paint was built from. The export modal renders from it
    // rather than fetching again, so the sheet is this view, not a later one.
    lastBoardThread = res || null;
    if (boardExportBtn) boardExportBtn.disabled = !lastBoardThread;
    const rows = (res && res.rows) || [];
    if (!rows.length) {
      boardChainsRowsEl.textContent = "(nothing on the board within that window)";
      return;
    }
    // Section headers come from Go (cli.ConversationHeader), keyed by the same
    // conversation key the rows carry. A chain is not a conversation — an
    // unanswered message is its own chain — so without this the sections of
    // different exchanges interleave with nothing marking the boundary.
    const headerOf = new Map();
    for (const c of ((res && res.conversations) || [])) headerOf.set(c.key, c.header);
    let currentConv = null;

    for (const r of rows) {
      if (r.conversation !== currentConv) {
        currentConv = r.conversation;
        const h = document.createElement("div");
        h.className = "board-chain-conv";
        const label = document.createElement("span");
        label.className = "board-chain-conv-label";
        label.textContent = headerOf.get(currentConv) || currentConv;
        h.appendChild(label);
        // Scoped to THIS conversation, never to what the view is showing:
        // harness.boardThread() takes no selector and returns every
        // conversation on the board, so a button acting on "what is displayed"
        // would be the whole-board form the CLI declares unreachable. That is
        // also why these live here and not in the Export modal, whose content
        // is the whole sheet.
        h.appendChild(boardConvActionBtn(currentConv, "retract"));
        h.appendChild(boardConvActionBtn(currentConv, "purge"));
        boardChainsRowsEl.appendChild(h);
      }
      const row = document.createElement("div");
      row.className = "board-chain-row";
      if (r.orphan) row.classList.add("is-orphan");
      if (r.retracted) row.classList.add("is-retracted");

      const head = document.createElement("div");
      head.className = "board-chain-head";
      // The gutter is monospace and pre-formatted: it is the same ├─ / └─ / │
      // string the CLI and TUI draw, rendered by cli.TreePrefix.
      const gutter = document.createElement("span");
      gutter.className = "board-chain-gutter";
      gutter.textContent = r.gutter || "";
      head.appendChild(gutter);

      const meta = document.createElement("span");
      meta.className = "board-chain-meta";
      const bits = ["#" + r.seq];
      if (r.inReplyTo && r.inReplyTo !== "0") bits.push("re=" + r.inReplyTo);
      if (r.replyToTopic) bits.push("reply-to=" + r.replyToTopic);
      bits.push("topic=" + r.topic);
      bits.push("from=" + String(r.from && r.from.senderParty || ""));
      if (r.from && r.from.agentProfile) bits.push("agent=" + r.from.agentProfile);
      // size is always populated, so a 0 here is a zero-byte publish rather
      // than a field nobody filled in — printed rather than hidden.
      bits.push("size=" + r.size);
      meta.textContent = bits.join(" ");
      head.appendChild(meta);

      if (r.orphan) {
        const tag = document.createElement("span");
        tag.className = "board-chain-tag";
        tag.textContent = "ORPHAN";
        tag.title = "its parent is outside the retention window, or on a topic this view cannot read";
        head.appendChild(tag);
      }
      if (r.retracted) {
        const tag = document.createElement("span");
        tag.className = "board-chain-tag is-retracted";
        tag.textContent = "RETRACTED" + (r.retractedBy ? " by=" + r.retractedBy : "");
        head.appendChild(tag);
      }
      // Reply as the operator, from the conversation itself. Same composer as
      // the topic view; withdrawn messages included, since replying destroys
      // nothing and a withdrawn parent still routes.
      const replyBtn = document.createElement("button");
      replyBtn.type = "button";
      replyBtn.className = "board-msg-reply-btn";
      replyBtn.textContent = "↩";
      replyBtn.title = `Reply to #${r.seq} as the operator`;
      replyBtn.addEventListener("click", () => {
        armBoardReply(r.seq);
        boardComposeEl.scrollIntoView({ block: "nearest" });
      });
      head.appendChild(replyBtn);
      row.appendChild(head);

      // Body bytes arrive already escaped by Go, so nothing here has to decide
      // what a control byte means.
      const body = document.createElement("pre");
      body.className = "board-chain-body";
      body.textContent = prettyPayload(r.body || "");
      row.appendChild(body);

      boardChainsRowsEl.appendChild(row);
    }
  }

  // ---- Board export -------------------------------------------------------
  //
  // The sheet is built from lastBoardThread — the SAME response the chains view
  // was painted from — rather than a fetch at button-press time. The board
  // moves on its own, so a second fetch could hand the operator a document that
  // is not what they were looking at when they pressed export.
  //
  // Its `text` arrives already rendered by Go (cli.RenderThreads), so the sheet
  // and the saved file are byte-for-byte `harness-cli board thread`. Rebuilding
  // that layout in JS would be a second implementation of one grammar, free to
  // drift the next time the row format grows.
  let lastBoardThread = null;
  let boardExportFormat = "text";

  const boardExportBtn      = document.getElementById("board-export-btn");
  const boardExportModal    = document.getElementById("board-export-modal");
  const boardExportTitle    = document.getElementById("board-export-title");
  const boardExportTextBtn  = document.getElementById("board-export-text");
  const boardExportJsonBtn  = document.getElementById("board-export-json");
  const boardExportCopyBtn  = document.getElementById("board-export-copy");
  const boardExportDlBtn    = document.getElementById("board-export-download");
  const boardExportCloseBtn = document.getElementById("board-export-close");
  const boardExportBody     = document.getElementById("board-export-body");

  // What the sheet currently shows, which is also exactly what Copy and
  // Download act on — so "what I saw" and "what I took" cannot differ.
  function boardExportPayload() {
    if (!lastBoardThread) return { text: "", ext: "txt", mime: "text/plain" };
    if (boardExportFormat === "json") {
      return {
        text: JSON.stringify({
          window: lastBoardThread.window || "",
          conversations: lastBoardThread.conversations || [],
          rows: lastBoardThread.rows || [],
        }, null, 2),
        ext: "json",
        mime: "application/json",
      };
    }
    return { text: lastBoardThread.text || "", ext: "txt", mime: "text/plain" };
  }

  function renderBoardExport() {
    if (!boardExportBody) return;
    const p = boardExportPayload();
    boardExportBody.innerHTML = "";
    const pre = document.createElement("pre");
    pre.className = "board-export-sheet";
    pre.textContent = p.text;
    boardExportBody.appendChild(pre);
    if (boardExportTextBtn) boardExportTextBtn.classList.toggle("is-active", boardExportFormat === "text");
    if (boardExportJsonBtn) boardExportJsonBtn.classList.toggle("is-active", boardExportFormat === "json");
    if (boardExportTitle) {
      const convs = (lastBoardThread && lastBoardThread.conversations || []).length;
      const msgs  = (lastBoardThread && lastBoardThread.rows || []).length;
      boardExportTitle.textContent =
        `Board chains — ${convs} conversation${convs === 1 ? "" : "s"}, ${msgs} message${msgs === 1 ? "" : "s"}`;
    }
  }

  // downloadText saves a string as a file. Blob + object URL rather than a
  // data: URI: a board with 64 messages per topic across many topics is well
  // past what a URL length is safe for. The URL is revoked on the next tick,
  // not immediately — Firefox cancels an in-flight download if it is revoked
  // in the same task.
  function downloadText(name, mime, text) {
    const url = URL.createObjectURL(new Blob([text], { type: mime + ";charset=utf-8" }));
    const a = document.createElement("a");
    a.href = url;
    a.download = name;
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 0);
  }

  // Colons are not usable in a filename on every platform this gets saved to,
  // so the timestamp is the compact ISO form: sorts, and says UTC out loud.
  function exportStamp() {
    return new Date().toISOString().replace(/[-:]/g, "").replace(/\.\d+Z$/, "Z");
  }

  if (boardExportBtn) {
    boardExportBtn.addEventListener("click", () => {
      if (!lastBoardThread) return;
      renderBoardExport();
      if (boardExportModal && !boardExportModal.open) boardExportModal.showModal();
    });
  }
  if (boardExportTextBtn) boardExportTextBtn.addEventListener("click", () => { boardExportFormat = "text"; renderBoardExport(); });
  if (boardExportJsonBtn) boardExportJsonBtn.addEventListener("click", () => { boardExportFormat = "json"; renderBoardExport(); });
  if (boardExportCloseBtn) boardExportCloseBtn.addEventListener("click", () => boardExportModal && boardExportModal.close());
  if (boardExportModal) {
    boardExportModal.addEventListener("click", (ev) => {
      if (ev.target === boardExportModal) boardExportModal.close();
    });
  }
  if (boardExportCopyBtn) {
    boardExportCopyBtn.addEventListener("click", async () => {
      const p = boardExportPayload();
      if (await writeClipboardText(p.text)) {
        flashButtonLabel(boardExportCopyBtn, "Copied ✓", "⧉ Copy");
        return;
      }
      // Both clipboard paths failed (plain http with execCommand disabled, or
      // a permission refusal). Select the sheet so the operator can copy it by
      // hand rather than being told nothing happened.
      const node = boardExportBody.querySelector("pre") || boardExportBody;
      selectNodeText(node);
      flashButtonLabel(boardExportCopyBtn, "Selected ✓", "⧉ Copy");
    });
  }
  if (boardExportDlBtn) {
    boardExportDlBtn.addEventListener("click", () => {
      const p = boardExportPayload();
      downloadText(`board-chains-${exportStamp()}.${p.ext}`, p.mime, p.text);
      flashButtonLabel(boardExportDlBtn, "Saved ✓", "⭳ Download");
    });
  }

  if (boardViewTopicsBtn) boardViewTopicsBtn.addEventListener("click", () => setBoardView("topics"));
  if (boardViewChainsBtn) boardViewChainsBtn.addEventListener("click", () => setBoardView("chains"));

  if (boardRefreshBtn) {
    boardRefreshBtn.addEventListener("click", () => {
      if (boardView === "chains") {
        renderBoardChains();
      } else if (boardArchivesViewEl && !boardArchivesViewEl.hidden) {
        if (currentArchive) openBoardArchive(currentArchive);
        else renderBoardArchives(archivesFilter);
      } else if (boardDetailEl && !boardDetailEl.hidden && currentBoardTopic) {
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

  async function refreshGit(preserveSelection, limits = {}) {
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

    gitQuery("log", { ...limits }).then(log => {
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
    // --max and --max-bytes are declared for this surface and reached
    // nothing: runGitAction forwarded maxBytes and dropped max, and every
    // gitQuery below built its own options object without either. `git <id>
    // log --max 5` showed the panel's default count.
    const limits = {};
    if (opts && opts.max) limits.maxCommits = opts.max;
    if (opts && opts.maxBytes) limits.maxBytes = opts.maxBytes;
    const btn = tabbar.querySelector('.tab-btn[data-tab="git"]');
    if (btn) btn.click();
    if (gitTaskSelect) gitTaskSelect.value = taskID;
    // One entry, not one per path segment: a path typed on the command line
    // names one repository, so up from it is the worktree.
    gitSubrepoStack = opts.subrepo ? [opts.subrepo] : [];
    gitSubmodule = !!opts.submodule;
    gitSetBase(opts.baseRev || "HEAD");
    await refreshGit(false, limits);
    if (sub === "status") {
      await gitShowStatusListing();
      return;
    }
    if (sub === "file") {
      await openGitFileFromCmd(opts, limits);
      return;
    }
    if (sub === "subrepos") {
      const res = await gitQuery("subrepos", { ...limits });
      gitContentEl.textContent = (res.subrepos || []).length
        ? (res.subrepos || []).join("\n")
        : "(no nested repositories)";
      return;
    }
    if (sub === "show") {
      try {
        gitLastContent = { kind: "show", baseRev: opts.baseRev };
        const res = await gitQuery("show", { baseRev: opts.baseRev, path: opts.path, ...limits });
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
          baseRev: opts.baseRev, targetRev: opts.targetRev, target, path: opts.path, ...limits,
        });
        renderGitText(res.text || "(no difference)", true);
      } catch (err) {
        gitSetError(err.message);
      }
    }
  };

  // openGitFileFromCmd is the cmdline route into the file view; the side comes
  // from the flags rather than from whatever diff happens to be on screen.
  async function openGitFileFromCmd(opts, limits) {
    const q = { path: opts.path, ...(limits || {}) };
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

  // 権限の棚卸し (under the task tree): who holds which capability. Rows come
  // from harness.capsAudit (cli.CapsAudit) on each call; this side keeps only
  // the toggles and which bits are expanded. Labels carry prompt text, so
  // everything is built with textContent, never innerHTML.
  const capsAuditRows = document.getElementById("caps-audit-rows");
  const capsAuditError = document.getElementById("caps-audit-error");
  const capsAuditEnded = document.getElementById("caps-audit-ended");
  const capsAuditEmpty = document.getElementById("caps-audit-empty");
  const capsAuditOpen = new Set();
  const tierClass = { "強": "strong", "中": "mid", "弱": "weak" };
  let capsAuditSeq = 0;
  const auditEl = (tag, cls, text) => {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined) e.textContent = text;
    return e;
  };
  // The text is the CLI's own scope cell (scope_text); a bit no task scope
  // bounds reads global（固定） and is coloured as global.
  const auditScopeChip = (h) =>
    auditEl("span", "sc " + (h.scope_kind === "none" ? "global" : h.breadth), h.scope_text);
  const renderCapsAudit = (rows) => {
    capsAuditRows.replaceChildren();
    if (!rows.length) {
      capsAuditRows.append(auditEl("p", "hint",
        "この条件で capability を持つ task はない（終了した task も、で広げられる）"));
      return;
    }
    for (const r of rows) {
      const box = auditEl("div", "audit-cap" + (r.holders.length ? "" : " empty"));
      const head = auditEl("div", "head");
      head.dataset.cap = r.cap;
      head.append(auditEl("span", "name", r.cap),
        auditEl("span", "audit-tier " + (tierClass[r.tier] || ""), r.tier),
        auditEl("span", "n", String(r.holders.length)));
      if (r.implied_by.length) {
        head.append(auditEl("span", "note", "(also: " + r.implied_by.join(", ") + ")"));
      }
      const chips = auditEl("span", "audit-holders");
      for (const h of r.holders) {
        const chip = auditEl("button", "audit-holder" + (h.live ? "" : " ended"), h.label);
        chip.type = "button";
        chip.dataset.taskId = h.task_id;
        chip.append(auditScopeChip(h));
        chips.append(chip);
      }
      if (!r.holders.length) chips.append(auditEl("span", "hint", "—"));
      head.append(chips);
      box.append(head);
      if (capsAuditOpen.has(r.cap) && r.holders.length) {
        const detail = auditEl("div", "audit-detail");
        for (const h of r.holders) {
          const line = auditEl("div", "", h.label + " — 親 " + h.parent + " / scope ");
          line.append(auditScopeChip(h));
          if (h.override) line.append(" (task 全体は " + h.task_scope + ")");
          line.append(" / " + (h.live ? "live" : "ended"));
          // May hand this bit to tasks it creates (cli's `deleg`).
          if (h.delegates) line.append(" deleg");
          if (h.authority) line.append(" granter=" + h.authority);
          detail.append(line);
        }
        box.append(detail);
      }
      capsAuditRows.append(box);
    }
  };
  let capsAuditLast = [];
  const refreshCapsAudit = async () => {
    if (typeof window.harness.capsAudit !== "function") return;
    const seq = ++capsAuditSeq;
    try {
      const rows = await window.harness.capsAudit({
        includeEnded: capsAuditEnded.checked, includeEmpty: capsAuditEmpty.checked,
      });
      if (seq !== capsAuditSeq) return; // a later refresh owns the pane
      capsAuditError.hidden = true;
      capsAuditLast = rows;
      renderCapsAudit(rows);
    } catch (err) {
      if (seq !== capsAuditSeq) return;
      capsAuditError.textContent = "caps audit: " + err.message;
      capsAuditError.hidden = false;
    }
  };
  window.__refreshCapsAudit = refreshCapsAudit;
  capsAuditEnded.addEventListener("change", refreshCapsAudit);
  capsAuditEmpty.addEventListener("change", refreshCapsAudit);
  document.getElementById("caps-audit-refresh").addEventListener("click", refreshCapsAudit);
  capsAuditRows.addEventListener("click", (e) => {
    const chip = e.target.closest(".audit-holder");
    if (chip) {
      const cap = chip.closest(".audit-cap").querySelector(".head").dataset.cap;
      if (!showTaskInTree(chip.dataset.taskId, cap)) {
        capsAuditError.textContent = "caps audit: task " + chip.dataset.taskId.slice(0, 8) +
          " is not in the tree";
        capsAuditError.hidden = false;
      }
      return;
    }
    const head = e.target.closest(".head");
    if (head) {
      const c = head.dataset.cap;
      if (capsAuditOpen.has(c)) capsAuditOpen.delete(c);
      else capsAuditOpen.add(c);
      renderCapsAudit(capsAuditLast);
    }
  });
  capsAuditDetails.addEventListener("toggle", () => {
    if (capsAuditDetails.open) refreshCapsAudit();
  });

  // Activate whichever board view is showing when the board tab is selected.
  tabbar.addEventListener("click", (e) => {
    const btn = e.target.closest(".tab-btn");
    if (btn && btn.dataset.tab === "board") {
      if (boardView === "chains") renderBoardChains();
      else renderBoardTopics();
    }
  });
})();
