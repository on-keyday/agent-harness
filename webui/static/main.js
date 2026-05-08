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
  function registerOnConnected(fn) { connectedHandlers.push(fn); }

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
      for (const fn of connectedHandlers) {
        try { fn(); } catch (e) { console.error('connected handler', e); }
      }
    } else if (state.phase === 'closed') {
      setStatus("disconnected", "error");
    } else if (state.phase === 'reconnecting') {
      setStatus("reconnecting…");
    }
  });

  setStatus("connecting…");
  try {
    await window.harness.connect(SERVER_CID, { persist: true });
    setStatus("connected", "connected");
  } catch (e) {
    setStatus(`connect failed: ${e.message}`, "error");
    return;
  }

  // 4. Snapshot polling — single source of truth for runner-select +
  //    runner-list + task-list. Replaces the old refreshList(harness.list)
  //    string-based renderer.
  const runnerSelect = document.getElementById("runner-select");
  const hostSelect   = document.getElementById("host-select");
  const claudeArgs   = document.getElementById("claude-args-input");
  const resumeInput  = document.getElementById("resume-task-input");
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
    if (!resumeInput) return "";
    return resumeInput.value.trim();
  };

  const refreshSnapshot = async () => {
    let snap;
    try {
      snap = await window.harness.snapshot();
    } catch (e) {
      taskList.textContent = `snapshot error: ${e.message}`;
      return;
    }
    renderRunnerSelect(runnerSelect, snap.runners);
    renderHostSelect(hostSelect, snap.runners);
    runnerList.textContent = renderRunners(snap.runners);
    taskList.textContent   = renderTasks(snap.tasks);
  };
  await refreshSnapshot();
  setInterval(refreshSnapshot, POLL_INTERVAL_MS);

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
      cmdOutput.textContent = `${banner}\n` + cmdOutput.textContent;
    } catch (e) { /* ignore */ }
    refreshSnapshot();
  };
  registerOnConnected(() => {
    window.harness.watch().catch(e => console.error("watch:", e));
  });

  const runCmd = async () => {
    const line = cmdInput.value.trim();
    if (!line) return;
    cmdInput.value = "";
    cmdOutput.textContent = `> ${line}\n` + cmdOutput.textContent;
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
          out = await window.harness.submit({ repo, task, host, claudeArgs: claudeArgsList, resumeTaskId });
          break;
        }
        case "list":
          // Force a snapshot refresh and render the structured task list
          // into cmd-output for parity with the prior `harness.list()`
          // string output.
          await refreshSnapshot();
          out = taskList.textContent;
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
        default:
          out = `unknown command: ${cmd}`;
      }
      cmdOutput.textContent = `${out}\n` + cmdOutput.textContent;
      refreshSnapshot();
    } catch (e) {
      cmdOutput.textContent = `error: ${e.message}\n` + cmdOutput.textContent;
    }
  };
  cmdRun.addEventListener("click", runCmd);
  cmdInput.addEventListener("keydown", (e) => { if (e.key === "Enter") runCmd(); });

  // 7. Interactive PTY.
  const term = new Terminal({ convertEol: true, fontSize: 13 });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open(document.getElementById("terminal"));
  fit.fit();
  window.harness_xtermWrite = (uint8Array) => term.write(uint8Array);

  // Touch-keys: virtual modifier toggles + special-key buttons for soft keyboards.
  const mods = { ctrl: false, shift: false };

  const setMod = (name, on) => {
      mods[name] = on;
      const btn = document.getElementById(`tk-${name}`);
      if (btn) btn.classList.toggle("active", on);
  };

  const sendSeq = (seq) => {
      window.harness.sendInteractive(seq);
      term.focus();
  };

  // Apply Ctrl/Shift modifiers to a CSI base sequence (Esc Tab arrows).
  // Standard xterm-style modifier encoding:
  //   modVal = 1 + (Shift?1:0) + (Alt?2:0) + (Ctrl?4:0)
  // Shift+Tab is the special case: xterm sends ESC [ Z (BackTab).
  const KEY_BASE = {
      esc:   "\x1b",
      tab:   "\t",
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

  // openInteractive is the shared helper for one-shot and detachable opens.
  const openInteractive = async (detachable, label) => {
    const req = composeRequest();
    if (!req.repo && !req.resumeTaskId) {
      alert("select a repo or fill in Resume task id");
      return;
    }
    term.reset();
    try {
      const taskID = await window.harness.startInteractive({...req, detachable});
      attachedTask.textContent = `attached: ${taskID} (${label})`;
      term.focus();
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
  });

  document.getElementById("reattach").addEventListener("click", async () => {
    const id = document.getElementById("reattach-session-id").value.trim();
    if (!id) {
      attachedTask.textContent = "(session id required)";
      return;
    }
    term.reset();
    try {
      const taskID = await window.harness.attachSession(id);
      attachedTask.textContent = `attached: ${taskID} (reattached)`;
      term.focus();
    } catch (err) {
      attachedTask.textContent = "";
      showError(err);
    }
    try { fit.fit(); } catch (_) { /* element not yet laid out */ }
    window.harness.resizeInteractive({ cols: term.cols, rows: term.rows });
  });
})();

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

function renderTasks(tasks) {
  if (!tasks || tasks.length === 0) return "(none)";
  return tasks.map(t => {
    const promptShort = (t.prompt || "").slice(0, 60);
    return `  ${t.id}  ${pad(t.status, 10)} ${pad(t.kind, 12)} repo=${t.repoPath}  prompt=${JSON.stringify(promptShort)}`;
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
