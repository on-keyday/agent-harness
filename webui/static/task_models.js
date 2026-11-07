"use strict";

// Task row models: the now line, the lifecycle and program badges,
// tree-node progress, watcher banners, runner rows.

// taskNowModel is the "now" line under a task row: the task's self-reported
// progress with its server-computed age, and its window title. null means
// "not shown". A line never written ("0" stamp) shows nothing; a cleared one
// (empty text with a stamp) is cleared: true, which the row draws as a gray
// "(cleared)" MARKER — a styled element, never text — so it cannot be
// mistaken for a task that wrote "(cleared)" itself. The title shows on a
// live terminal. Ages mirror cli.AgeStr.
function taskNowModel(t) {
  const written = t.progressAt !== undefined && t.progressAt !== "0" && t.progressAt !== "";
  let age = null;
  if (written) {
    const ms = t.progressAgeMs || 0;
    if (ms >= 3600000) age = `${Math.floor(ms / 3600000)}h ago`;
    else if (ms >= 60000) age = `${Math.floor(ms / 60000)}m ago`;
    else age = `${Math.floor(ms / 1000)}s ago`;
  }
  return {
    progress: written ? (t.progress || "") : null,
    cleared: written && !t.progress,
    age,
    title: t.liveTerminal ? (t.title || "") : null,
  };
}

// programStatusLine is the program badge's tooltip, mirroring
// cli.ProgramDetail: the program's own OSC 7501 report, kind only with
// blocked, the msg when it sent one; null when it never reported.
function programStatusLine(t) {
  if (!t.programState) return null;
  let s = t.programState;
  if (t.programKind) s += ` (${t.programKind})`;
  if (t.programMsg) s += ` — ${t.programMsg}`;
  return s;
}

// taskLifecycleModel is the lifecycle badge, mirroring cli.LifecycleBadge:
// work / wait / ready / err with the short age, null for one that has ended.
// Unlike the CLI and the TUI it is also null for a task that never reported:
// here the badge appears only when there is a report, as the program badge
// does, rather than as lc:- (D28 (5)). The words are not the wire names so
// `ready` cannot be read as the activity badge's `idle:3m`. attention marks the
// two that ask for the operator, quiet the one that needs no look.
function taskLifecycleModel(t) {
  if (["Succeeded", "Failed", "Cancelled"].includes(t.status)) return null;
  if (!t.lifecycleAt || t.lifecycleAt === "0") return null;
  const word = { working: "work", waiting: "wait", idle: "ready", error: "err" }[t.lifecycle];
  if (!word) return null;
  const ms = t.lifecycleAgeMs || 0;
  const age = ms >= 60000 ? `${Math.floor(ms / 60000)}m` : `${Math.floor(ms / 1000)}s`;
  return { text: `${word}:${age}`, attention: word === "wait" || word === "err", quiet: word === "work" };
}

// taskProgramModel is the program badge beside it, mirroring cli.ProgramStr:
// `prog blocked:question`, kind only with blocked; the full line, msg
// included, as its tooltip. null without a live terminal or a report. Its
// colors follow the lifecycle badge's three tiers, since for an agent without
// hooks this badge is the only state on the row: blocked / error ask for the
// operator like wait / err, idle / done wait for the next prompt like ready,
// and only working is quiet.
function taskProgramModel(t) {
  if (!t.liveTerminal || !t.programState) return null;
  const s = t.programState;
  return {
    text: `prog ${s}${t.programKind ? ":" + t.programKind : ""}`,
    attention: s === "blocked" || s === "error",
    quiet: s === "working",
    tooltip: programStatusLine(t),
  };
}

// treeNodeProgress is what a task-tree node shows of the task's progress: a
// fourth line cut to the node's slot (a node is 132px wide), and tooltip lines
// with the full text, its age and the window title. Built on taskNowModel, so
// the tree and the list agree: never written shows no line, cleared is the
// same gray "(cleared)" marker. null when there is nothing to show at all.
const TREE_PROGRESS_MAX = 20;
function treeNodeProgress(t) {
  const now = taskNowModel(t);
  if (now.progress === null && now.title === null) return null;
  const tooltip = [];
  let line = null;
  if (now.progress !== null) {
    if (now.cleared) {
      line = "(cleared)";
      tooltip.push(`progress: (cleared) (${now.age})`);
    } else {
      const chars = Array.from(now.progress);
      line = chars.length > TREE_PROGRESS_MAX ? chars.slice(0, TREE_PROGRESS_MAX - 1).join("") + "…" : now.progress;
      tooltip.push(`progress: ${now.progress} (${now.age})`);
    }
  }
  if (now.title) tooltip.push(`title: ${now.title}`);
  if (line === null && tooltip.length === 0) return null;
  return { line, cleared: now.cleared, tooltip };
}

// watcherWaitsFor is the condition cell of a watcher row. The wording is Go's
// (cli.AwaitWatcherWaitsFor), carried on the snapshot row as waits_for, so the
// page never respells it.
function watcherWaitsFor(w) {
  return w.waits_for;
}

// taskEventBanner is the command-output line for a task event, or null for a
// routine one: a busy/idle edge or a progress write fires per session per
// change, and a banner each time would bury the lifecycle lines. The caller
// still refreshes for the routine kinds that change a row's text.
function taskEventBanner(line) {
  if (!line || line.includes("kind=TaskActivity") || line.includes("kind=TaskProgress") ||
      line.includes("kind=TaskLifecycle")) return null;
  return line;
}

// canArmProgress: await-progress needs no live session (a oneshot task writes
// progress too), so any task that can still write — any non-terminal one —
// can be watched.
function canArmProgress(t) {
  return !["Succeeded", "Failed", "Cancelled"].includes(t.status);
}

// watchChoices is the one watch menu (🔔): what a task can be watched for.
// The four await-idle signals need a live session; the next progress line
// works on any task that can still write. The CLI keeps the two verbs apart;
// the page asks one question — "wake me when what?". The TUI's `w`/`W`
// picker offers the same five in the same order.
const WATCH_CHOICES = [
  { key: "quiet", label: "出力が止まったら", signal: "quiet" },
  { key: "lifecycle", label: "hook が turn の終わりを報告したら", signal: "lifecycle" },
  { key: "program", label: "program が状態で知らせたら", signal: "program" },
  { key: "any", label: "どれか早いもの", signal: "any" },
  { key: "progress", label: "次の進捗行が書かれたら", progress: true },
];
function watchChoices(t) {
  const live = (t.kind === "Interactive" || t.kind === "Stream") &&
    (t.status === "Running" || t.status === "Detached");
  return WATCH_CHOICES.filter((c) => (c.progress ? canArmProgress(t) : live));
}

// watchArm is the bridge call a choice makes: the verb and its options. Every
// menu arm notifies (the menu is the "tell me later" button); the progress
// baseline is the row's progressAt, an exact decimal string.
function watchArm(c, t) {
  if (c.progress) return { fn: "awaitProgress", opts: { taskId: t.id, since: t.progressAt || "0", sink: "notify" } };
  return { fn: "awaitIdle", opts: { taskId: t.id, sink: "notify", signal: c.signal } };
}

// canClearProgress: the sheet offers to empty a line only when there is one;
// whether this operator may (cancel + scope) is the server's answer.
function canClearProgress(t) {
  return !!t.progress && t.progressAt !== undefined && t.progressAt !== "0" && t.progressAt !== "";
}

// runnerRowModel is what one runner row in the Runners list shows, before any
// DOM: the list is rows like the task list's, and this keeps what a row says
// separate from how it is drawn. A field a runner did not report reads as
// that, not as blank: "-" host and id, "unknown" os, "(any)" roots.
function runnerRowModel(r) {
  let agents = (r.agentProfiles && r.agentProfiles.length > 0) ? r.agentProfiles.slice() : [];
  if (agents.length === 0 && r.agentBin) agents = [r.agentBin];
  return {
    status: r.status,
    host: r.hostname || "-",
    // Always shown, "unknown" included: a runner has a platform whether or
    // not it reported one, and it decides which shell an `ssh host cmd`
    // reaches on it.
    os: r.goos || "unknown",
    load: `${r.tasks}/${r.maxTasks}`,
    full: r.maxTasks > 0 && r.tasks >= r.maxTasks,
    agents,
    // "+skills" belongs to a runner that advertises an agent; with none it
    // would be noise.
    skills: !!r.skillsInjected && agents.length > 0,
    roots: (r.roots && r.roots.length > 0) ? r.roots.slice() : ["(any)"],
    id: r.id ? r.id.slice(0, 8) : "-",
    cid: r.cid || "-",
    // The enrolled key's label or its prefix — the same string `ls` prints
    // as key=, rendered by the bridge (cli.RunnerKeyStr).
    key: r.keyLabel || "-",
    // How it is connected — in / out / dial:<id> / via:<id>, the string `ls`
    // prints as link=, rendered by the bridge (cli.RunnerLinkStr).
    link: r.link || "-",
  };
}

// runnerTasks is the live tasks a runner holds, for its row in the Runners
// list: joined on identity (task.assignedTo is the runner's id), live only and
// in id order — the rule tasksOnRunner applies for the connection view.
function runnerTasks(tasks, runnerId) {
  if (!runnerId) return [];
  return (tasks || [])
    .filter((t) => isActiveTask(t) && t.assignedTo === runnerId)
    .sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0))
    .map((t) => ({ id: t.id, status: t.status, agent: agentLabel(t.agentProfile, t.skillsInjected), repoPath: t.repoPath || "" }));
}

function runnerStatusColor(status) {
  switch (status) {
    case "Idle":    return "#2d5";
    case "Busy":    return "#e5c07b";
    case "Offline": return "#888";
    default:        return "#61afef";
  }
}

// renderRunnerRows draws the Runners list into container, one row per runner:
// status and host, then the agents it offers as chips, then its roots one per
// line, then its id and connection, then the live tasks it holds. The runner
// row itself is display only; a task line jumps to that task's row (via.onTask).
// via carries what lives in the page's scope: tasks, statusColor, repoTail.
function renderRunnerRows(container, runners, via) {
  const v = via || {};
  container.innerHTML = "";
  if (!runners || runners.length === 0) {
    const empty = document.createElement("div");
    empty.className = "task-empty";
    empty.textContent = "(none)";
    container.appendChild(empty);
    return;
  }
  const span = (cls, text) => {
    const el = document.createElement("span");
    el.className = cls;
    el.textContent = text;
    return el;
  };
  for (const r of runners) {
    const m = runnerRowModel(r);
    const row = document.createElement("div");
    row.className = "runner-row";
    row.title = r.id || "";

    const line1 = document.createElement("div");
    line1.className = "runner-row-line1";
    const dot = span("task-status-dot", "");
    dot.style.background = runnerStatusColor(m.status);
    const st = span("task-status-label", m.status);
    st.style.color = runnerStatusColor(m.status);
    const load = span(m.full ? "runner-load is-full" : "runner-load", m.load);
    line1.append(dot, st, span("runner-host", m.host), load, span("runner-os", m.os));

    const agents = document.createElement("div");
    agents.className = "runner-agents";
    for (const a of m.agents) agents.appendChild(span("runner-agent-chip", a));
    if (m.skills) agents.appendChild(span("runner-skills", "+skills"));

    const roots = document.createElement("div");
    roots.className = "runner-roots";
    for (const root of m.roots) roots.appendChild(span("runner-root", root));

    const meta = document.createElement("div");
    meta.className = "task-row-meta";
    meta.textContent = `${m.id}…  key=${m.key}  link=${m.link}  ${m.cid}`;

    row.append(line1, agents, roots, meta);
    // Nothing for a runner holding no task: its load already reads 0/N.
    for (const t of runnerTasks(v.tasks, r.id)) {
      const line = document.createElement("div");
      line.className = "runner-task";
      line.title = t.id;
      const tdot = span("task-status-dot", "");
      if (v.statusColor) tdot.style.background = v.statusColor(t.status);
      const tst = span("task-status-label", t.status);
      if (v.statusColor) tst.style.color = v.statusColor(t.status);
      line.append(tdot, tst, span("runner-task-id", t.id.slice(0, 8) + "…"), span("task-agent", t.agent),
        span("runner-task-repo", v.repoTail ? v.repoTail(t.repoPath) : t.repoPath));
      if (v.onTask) line.addEventListener("click", () => v.onTask(t.id));
      row.appendChild(line);
    }
    container.appendChild(row);
  }
}
