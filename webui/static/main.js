"use strict";

const SERVER_CID = location.protocol.startsWith("https")
  ? `wss:${location.hostname}:${location.port || 443}-*`
  : `ws:${location.hostname}:${location.port || 80}-*`;

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
  go.run(result.instance);   // does NOT block; harness object becomes available shortly

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

  // 4. Initial list refresh.
  const refreshList = async () => {
    try {
      const out = await window.harness.list();
      document.getElementById("task-list").textContent = out;
    } catch (e) {
      document.getElementById("task-list").textContent = `list error: ${e.message}`;
    }
  };
  refreshList();
  setInterval(refreshList, 5000);

  // 5. Watch (server push).
  window.harness_onTaskEvent = (jsonStr) => {
    try {
      const evt = JSON.parse(jsonStr);
      const el = document.getElementById("task-list");
      el.textContent = `[${new Date().toISOString()}] ${evt.line}\n` + el.textContent;
    } catch (e) { /* ignore */ }
  };
  window.harness.watch().catch(e => console.error("watch:", e));

  // 6. Cmdline submit / cancel / prune.
  const cmdInput  = document.getElementById("cmd-input");
  const cmdRun    = document.getElementById("cmd-run");
  const cmdOutput = document.getElementById("cmd-output");

  const runCmd = async () => {
    const line = cmdInput.value.trim();
    if (!line) return;
    cmdInput.value = "";
    cmdOutput.textContent = `> ${line}\n` + cmdOutput.textContent;
    try {
      const tokens = line.split(/\s+/);
      const cmd = tokens[0];
      const flags = parseFlags(tokens.slice(1));
      let out;
      switch (cmd) {
        case "submit":
          out = await window.harness.submit({ repo: flags.repo || "", task: flags.task || "" });
          break;
        case "list":
          out = await window.harness.list();
          break;
        case "cancel":
          out = await window.harness.cancel(tokens[1] || "");
          out = "cancelled";
          break;
        case "prune":
          out = await window.harness.prune({ before: flags.before || "168h" });
          break;
        default:
          out = `unknown command: ${cmd}`;
      }
      cmdOutput.textContent = `${out}\n` + cmdOutput.textContent;
      refreshList();
    } catch (e) {
      cmdOutput.textContent = `error: ${e.message}\n` + cmdOutput.textContent;
    }
  };
  cmdRun.addEventListener("click", runCmd);
  cmdInput.addEventListener("keydown", (e) => { if (e.key === "Enter") runCmd(); });

  // 7. Interactive PTY.
  const term = new Terminal({ convertEol: true, fontSize: 13 });
  term.open(document.getElementById("terminal"));
  window.harness_xtermWrite = (uint8Array) => term.write(uint8Array);
  term.onData((data) => window.harness.sendInteractive(data));
  // Best-effort resize on container resize.
  const ro = new ResizeObserver(() => {
    window.harness.resizeInteractive({ cols: term.cols, rows: term.rows });
  });
  ro.observe(document.getElementById("terminal"));

  const repoInput    = document.getElementById("repo-input");
  const attachedTask = document.getElementById("attached-task");

  document.getElementById("attach").addEventListener("click", async () => {
    const repo = repoInput.value.trim();
    try {
      const taskID = await window.harness.startInteractive({ repo });
      attachedTask.textContent = `attached: ${taskID}`;
      term.focus();
    } catch (e) {
      attachedTask.textContent = "";
      alert(`startInteractive: ${e.message}`);
    }
  });
  document.getElementById("detach").addEventListener("click", () => {
    window.harness.detachInteractive();
    attachedTask.textContent = "";
  });
})();

function parseFlags(tokens) {
  const out = {};
  for (let i = 0; i < tokens.length; i++) {
    const t = tokens[i];
    if (t.startsWith("--")) {
      const eq = t.indexOf("=");
      if (eq !== -1) {
        out[t.slice(2, eq)] = t.slice(eq + 1).replace(/^"(.*)"$/, "$1");
      } else {
        out[t.slice(2)] = tokens[i + 1] || "";
        i++;
      }
    }
  }
  return out;
}
