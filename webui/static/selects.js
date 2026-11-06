"use strict";

// The compose form's runner, host and agent <select> builders.

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
      // The OS belongs here, not only on the runner row: this dropdown is
      // where an operator CHOOSES where to spawn, and the fleet is mixed
      // (Windows and Linux runners on the same server). Which platform a
      // task lands on decides its paths and its shell.
      opt.textContent = `${h}  [${r.status}]  ${r.goos || "unknown"}`;
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
