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

  // 3. Connect.
  setStatus("connecting…");
  try {
    await window.harness.connect(SERVER_CID);
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
  const runnerList   = document.getElementById("runner-list");
  const taskList     = document.getElementById("task-list");

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
  window.harness_onTaskEvent = (jsonStr) => {
    try {
      const evt = JSON.parse(jsonStr);
      const banner = `[${new Date().toISOString()}] ${evt.line}`;
      cmdOutput.textContent = `${banner}\n` + cmdOutput.textContent;
    } catch (e) { /* ignore */ }
    refreshSnapshot();
  };
  window.harness.watch().catch(e => console.error("watch:", e));

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
          if (!repo) {
            throw new Error("no runner selected (pick one from the dropdown)");
          }
          // Everything after `submit` is the task prompt. We join the
          // tokenize() result with single spaces — quoted segments have
          // already been collapsed into single tokens, so a multi-word
          // task is preserved verbatim.
          const task = tokens.slice(1).join(" ");
          if (!task) throw new Error("submit: missing task prompt");
          const host = hostSelect ? (hostSelect.value || "") : "";
          out = await window.harness.submit({ repo, task, host });
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
  term.onData((data) => window.harness.sendInteractive(data));
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

  document.getElementById("attach").addEventListener("click", async () => {
    const repo = runnerSelect.value || "";
    if (!repo) {
      attachedTask.textContent = "";
      alert("select a runner from the dropdown first");
      return;
    }
    const host = hostSelect ? (hostSelect.value || "") : "";
    // Reset xterm so the new session starts on a clean canvas (no leftover
    // output, escape state, or scrollback from the previous attach).
    term.reset();
    try {
      const taskID = await window.harness.startInteractive({ repo, host });
      attachedTask.textContent = `attached: ${taskID}`;
      term.focus();
    } catch (e) {
      attachedTask.textContent = "";
      alert(`startInteractive: ${e.message}`);
    }
    try { fit.fit(); } catch (_) { /* element not yet laid out */ }
  });
  document.getElementById("detach").addEventListener("click", () => {
    window.harness.detachInteractive();
    attachedTask.textContent = "";
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
    const idShort = t.id.slice(0, 12);
    const promptShort = (t.prompt || "").slice(0, 60);
    return `  ${idShort}  ${pad(t.status, 10)} ${pad(t.kind, 12)} repo=${t.repoPath}  prompt=${JSON.stringify(promptShort)}`;
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
