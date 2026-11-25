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

// stateBadgeColor is a lifecycle / program badge's colour, by the three tiers
// the TUI uses too: asking for the operator (wait, err, prog blocked / error)
// the Failed red, busy (work, prog working) gray, waiting for the next prompt
// (ready, prog idle / done) the default task blue (taskStatusColor's values).
// Not yellow / green as in the TUI: those are the Detached / Running status
// colours on the same row here, and a badge in them read as a status. "" for
// no badge.
function stateBadgeColor(b) {
  if (!b) return "";
  return b.attention ? "#f14c4c" : b.quiet ? "#888" : "#61afef";
}

// paneAboutText is a session pane header's About text, mirroring the TUI's
// (tui aboutCell): the progress line with its age, else the live window title
// marked "(title)" (a title comes from the first prompt and drifts from the
// work, so it is labelled), else "" -- a pane is already a session, and a
// placeholder like <interactive> would add nothing.
function paneAboutText(t) {
  const now = taskNowModel(t || {});
  if (now.progress) return `${now.progress} (${now.age})`;
  if (now.title) return `(title) ${now.title}`;
  return "";
}

// paneStateBadge is a session pane header's badge: the lifecycle badge, or for
// an agent with no hooks its program's report (the TUI's Life cell rule).
function paneStateBadge(t) {
  return taskLifecycleModel(t) || taskProgramModel(t);
}

// resumeRegrantFlags decides what a resume re-grants: caps when the caller
// gave caps, the scope when it gave a scope or a scope-for, nothing otherwise.
// null means "not given" (a typed flag, or the resume dialog's open override);
// a new task and an exact resume re-grant nothing.
function resumeRegrantFlags({ resumeTaskId, resumeExact, caps, scope, scopeFor }) {
  const on = !!resumeTaskId && !resumeExact;
  return {
    resumeCapsOverride: on && caps !== null && caps !== undefined,
    scopePresent: on && ((scope !== null && scope !== undefined) || (scopeFor !== null && scopeFor !== undefined)),
  };
}

// sessionScopeFields fills the scope and scope-for a request carries. A fresh
// spawn takes the half not given from the 新規作成 panel. A resume never does:
// the server writes the scope and its overrides together (one presence bit),
// so a panel value filled into the other half would rewrite the task's own --
// the half not given is the default ("" = subtree, no overrides), as on the CLI.
function sessionScopeFields({ resumeTaskId, scope, scopeFor }, panel) {
  if (resumeTaskId) {
    return { scope: scope !== null && scope !== undefined ? scope : "", scopeFor: scopeFor !== null && scopeFor !== undefined ? scopeFor : [] };
  }
  return {
    scope: scope !== null && scope !== undefined ? scope : panel.scope,
    scopeFor: scopeFor !== null && scopeFor !== undefined ? scopeFor : panel.scopeFor,
  };
}

// orderedRowsModel lays the task list out from harness.orderTasks' groups
// ([{group, ids}], ordered by Go's cli.OrderTasks): a {head} row before each
// labelled group, counting the rows under it, then a {task} row per task.
// tasks are the rows the filter admits; a group id not among them is skipped,
// and a task no group named (the wasm's snapshot lagged the page's) is kept,
// last and without a header.
function orderedRowsModel(groups, tasks) {
  const byId = new Map(tasks.map((t) => [t.id, t]));
  const seen = new Set();
  const rows = [];
  for (const g of groups || []) {
    const members = [];
    for (const id of g.ids || []) {
      const t = byId.get(id);
      if (t && !seen.has(id)) {
        seen.add(id);
        members.push({ task: t });
      }
    }
    if (members.length === 0) continue;
    if (g.group) rows.push({ head: `${g.group} (${members.length})` });
    rows.push(...members);
  }
  for (const t of tasks) if (!seen.has(t.id)) rows.push({ task: t });
  return rows;
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
// routine one: a busy/idle edge, a progress write or a schedule binding change fires per session per
// change, and a banner each time would bury the lifecycle lines. The caller
// still refreshes for the routine kinds that change a row's text.
function taskEventBanner(line) {
  if (!line || line.includes("kind=TaskActivity") || line.includes("kind=TaskProgress") ||
      line.includes("kind=TaskLifecycle") || line.includes("kind=TaskSchedule")) return null;
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
  const live = (t.kind === "Interactive" || t.kind === "Oneshot" || t.kind === "Stream") &&
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
    // The dial-record URL a named cid (@<hex>) stands for, as `ls` prints
    // target=; "" when the runner has none.
    target: r.linkTarget || "",
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
    meta.textContent = `${m.id}…  key=${m.key}  link=${m.link}${m.target ? `  target=${m.target}` : ""}  ${m.cid}`;

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

// --- PTY records (pillar A spec, A-D28) -------------------------------------

// composeRecordOption is Compose's record select as the open's `record`
// option: "" (the server's --record default), "on" or "off".
function composeRecordOption(v) {
  return v === "on" || v === "off" ? v : "";
}

// composePtyOption is Compose's PTY select as the open's `pty` option (A-D33):
// "" (the mode's default, which the server resolves), "on" or "off".
function composePtyOption(v) {
  return v === "on" || v === "off" ? v : "";
}

// composeBoxesFromTask is the kind half of prefilling the Resume… dialog from
// a task: the stream box, the one-shot box (A3, A-D29) and the PTY select
// (A-D33) each say what its last run was. The select names the PTY choice only where it differs
// from the mode's default (a PTY for an interactive run, none for a one-shot),
// so the prefilled open resolves to what the last run had.
function composeBoxesFromTask(t) {
  const stream = !!t && t.kind === "Stream";
  const oneshot = !!t && (t.kind === "Oneshot" || !!t.oneshot);
  let pty = "default";
  if (t && !stream && !!t.noPty !== oneshot) pty = t.noPty ? "off" : "on";
  return { stream, oneshot, pty };
}

// recordDownloadName names a downloaded record by its task and run.
function recordDownloadName(taskId, run, format) {
  return `${String(taskId).slice(0, 8)}-run${run}.${format === "asciicast" ? "cast" : "raw"}`;
}

// --- Agent transcripts (pillar B) --------------------------------------------

// transcriptDownloadName names a downloaded transcript by its task, agent and
// session: the whole session as a .tar, one file by its own base name (a
// companion's directories do not become the download's).
function transcriptDownloadName(taskId, agent, session, file) {
  const head = `${String(taskId).slice(0, 8)}-${agent}-${String(session).slice(0, 8)}`;
  if (!file) return `${head}.tar`;
  const parts = String(file).split("/");
  return `${head}-${parts[parts.length - 1]}`;
}

// transcriptSheetModel is what the transcripts dialog shows for a task's
// agent sessions, newest first: each session's row (rendered in Go as
// `transcript ls` renders it) with its actions, the files of each session the
// operator opened (null when not opened), and the note naming any store that
// could not be read.
function transcriptSheetModel(list, filesBySession) {
  const files = filesBySession || new Map();
  const sessions = [...((list && list.sessions) || [])].sort((a, b) => (b.updatedMs || 0) - (a.updatedMs || 0));
  return {
    note: (list && list.note) || "",
    rows: sessions.map((s) => ({
      agent: s.agent,
      id: s.id,
      text: s.row,
      actions: ["files", "tar"],
      files: files.has(s.id) ? files.get(s.id).map((f) => ({ name: f.name, text: f.row })) : null,
    })),
  };
}

// recordSheetModel is what the task sheet shows for a task's records: each
// record's row (size and expiry, rendered in Go as `record ls` renders it)
// with what can be done to it, and which switch a live terminal offers ("on",
// "off", or "" when there is no live terminal to record).
function recordSheetModel(t, recs) {
  const live = !!t.liveTerminal && (t.status === "Running" || t.status === "Detached");
  return {
    rows: (recs || []).map((r) => ({
      run: r.run,
      text: r.row + (r.recording ? "  (recording)" : ""),
      actions: ["raw", "asciicast", "keep", "delete"],
    })),
    switchTo: live ? (t.recording ? "off" : "on") : "",
  };
}

// --- Server-kept cron schedules (pillar S) -------------------------------------
//
// Every word here was rendered in Go (cli.Schedule*Cell, cli.ScheduleDetailParts
// -- the cells the TUI table and `schedule ls` / `schedule show` print); these
// models only arrange it, so the page holds no second spelling of a time, a
// task cell or a wait statistic.

// scheduleRowModel is one row of the Schedules dialog from the bridge's row.
// taskLink is true only for a bound task, whose id the row can open; a
// schedule with no task yet says so in words (created-at-first-firing,
// no-task), never a blank cell. Nothing is dropped at zero: a schedule that
// never fired reads last=none, one with nothing due next=none.
function scheduleRowModel(r) {
  const bound = r.taskState === "bound" && !!r.taskId;
  return {
    id: r.id,
    id8: String(r.id).slice(0, 8),
    cron: r.cron,
    zone: r.tz,
    next: r.next,
    last: r.last,
    taskText: r.task,
    taskId: bound ? r.taskId : "",
    taskLink: bound,
    taskState: r.taskState,
    agent: r.agent,
    repo: r.repo,
    about: r.about,
    line: r.line,
  };
}

// scheduleDetailModel is the dialog's detail view: the definition lines, the
// wait statistics, and the history in the order Go rendered it (oldest first,
// each time in the schedule's zone with its absolute instant beside it).
function scheduleDetailModel(d) {
  return {
    id: d.id,
    head: [...(d.head || [])],
    waits: d.waits,
    waitN: d.waitN || 0,
    waitMedianMs: d.waitMedianMs || 0,
    waitMaxMs: d.waitMaxMs || 0,
    historyTitle: d.historyTitle,
    history: (d.history || []).map((text, n) => ({ n, text })),
  };
}

// taskScheduleModel names the schedule a task is bound to, from the snapshot's
// raw scheduleId: schedule=<id8> as an ls row prints it, the full id for the
// sheet. null only when the task is bound to none -- gated on the binding
// existing, never on its value.
function taskScheduleModel(t) {
  if (!t || !t.scheduleId) return null;
  return { label: `schedule=${t.scheduleId.slice(0, 8)}`, id: t.scheduleId };
}

// taskEventRefreshes says whether a watch event re-reads the rows. Every
// kind does -- a task_schedule event (a binding changed) included, since the
// row's schedule= comes from the snapshot -- except a busy/idle edge, which
// fires per session per change and is kept current by the poll.
function taskEventRefreshes(line) {
  return !(line && line.includes("kind=TaskActivity"));
}

// scheduleZoneOptions is Compose's zone select: the server's own zone first as
// the empty value (the page cannot read it; the server resolves "" to it and
// stores it by name), then the IANA names the browser knows.
function scheduleZoneOptions(zones) {
  return [{ value: "", label: "(server's zone)" }, ...(zones || []).map((z) => ({ value: z, label: z }))];
}

// composeScheduleTokens serializes Compose's form to the argument line of
// `schedule new`, so the PARSER stays in Go (cli/verb) and the form cannot
// accept what the CLI refuses. A foreground-only option is refused here with a
// message instead of being dropped.
function composeScheduleTokens(f) {
  const t = ["--cron", String(f.cron).trim()];
  const tz = String(f.tz || "").trim();
  if (tz) t.push("--tz", tz);
  if (f.task) t.push("--task", f.task);
  else if (f.repo) t.push("--repo", f.repo);
  else throw new Error("schedule: select a repo (or pick a task under 対象 to bind it)");
  if (f.host) t.push("--host", f.host);
  if (f.agent) t.push("--agent", f.agent);
  for (const a of f.agentArgs || []) t.push("--agent-arg", a);
  if (f.prompt) t.push("--prompt", f.prompt);
  if (f.deadline) t.push("--deadline", f.deadline);
  if (f.turnEnd) t.push("--turn-end", f.turnEnd);
  if (f.quiet) t.push("--quiet", f.quiet);
  if (f.blocked) t.push("--blocked", f.blocked);
  if (f.queueTimeout) t.push("--queue-timeout", f.queueTimeout);
  if (f.oneshot) t.push("--oneshot");
  // The task kind each firing runs (2026-11-17-schedule-stream-kind-design.md).
  if (f.stream) t.push("--stream");
  if (f.record === "on") t.push("--record");
  else if (f.record === "off") t.push("--no-record");
  if (f.pty === "on") t.push("--pty");
  else if (f.pty === "off") t.push("--no-pty");
  if (f.resumeConversation) t.push("--resume-conversation");
  // The authority block, as the chips and pickers hold it: the stored
  // authority is bounded by the operator's, from the same flags session new has.
  if (f.caps) t.push("--caps", f.caps);
  if (f.scope) t.push("--scope", f.scope);
  for (const s of f.scopeFor || []) t.push("--scope-for", s);
  if (f.delegateCaps) t.push("--delegate-caps", f.delegateCaps);
  if (f.delegateScope) t.push("--delegate-scope", f.delegateScope);
  if (f.spawnDepth && f.spawnDepth !== "unbounded") t.push("--spawn-depth", f.spawnDepth);
  if (f.maxDescendants && f.maxDescendants !== "unbounded") t.push("--max-descendants", f.maxDescendants);
  return t;
}

// --- Schedules: refresh, failures, Compose routing (pillar S, fix round 1) ----

// scheduleEventRefresher is what a task event does to an open Schedules
// dialog: an event naming a schedule (kind=TaskSchedule, or any event of a
// bound task, which the watch line tags schedule=<id8>) reloads the dialog,
// coalesced -- one load in flight and at most one follow-up for the burst that
// arrived meanwhile. A closed dialog and an unbound task's event do nothing.
function scheduleEventRefresher({ isOpen, load }) {
  let inflight = false, pending = false;
  const run = async () => {
    inflight = true;
    try { await load(); } finally { inflight = false; }
    if (pending) { pending = false; run(); }
  };
  return (line) => {
    if (!isOpen() || !line || !(line.includes("kind=TaskSchedule") || line.includes(" schedule="))) return;
    if (inflight) pending = true;
    else run();
  };
}

// scheduleFailureLine is a failed Run now / Delete as the result surface says
// it: the verb and the schedule (id8), then cli's status and message without
// its own `<verb>: ` prefix, as the TUI's scheduleFailureLine words it.
function scheduleFailureLine(verbName, id, err) {
  const msg = String((err && err.message) || err);
  return `${verbName} ${String(id).slice(0, 8)}: ${msg.startsWith(verbName + ": ") ? msg.slice(verbName.length + 2) : msg}`;
}

// The 新規作成 (Compose) form's tabs. The tab alone decides what the button
// does, and the button names it: no field's content changes the action.
const COMPOSE_TABS = ["interactive", "oneshot", "background", "schedule"];

// The fields every tab reads, and each tab's own. Keys are composeForm()'s;
// the authority panel and the PTY record are shared and always read.
const COMPOSE_SHARED_FIELDS = ["repo", "host", "agent", "agentArgs", "prompt", "record",
  "caps", "scope", "scopeFor", "delegateCaps", "delegateScope", "spawnDepth", "maxDescendants"];
const COMPOSE_COND_FIELDS = ["deadline", "turnEnd", "quiet", "blocked", "queueTimeout"];
const COMPOSE_TAB_DEFS = {
  interactive: { label: "session を開く", action: "open-session", fields: ["stream", "pty"] },
  oneshot: { label: "one-shot を実行", action: "open-session", fields: ["pty"] },
  background: { label: "background で投入", action: "open-session", fields: [...COMPOSE_COND_FIELDS, "oneshot", "stream", "pty"] },
  schedule: { label: "schedule を作る", action: "create-schedule",
    fields: [...COMPOSE_COND_FIELDS, "cron", "tz", "task", "oneshot", "stream", "pty", "resumeConversation"] },
};

// composeTabModel is a tab's button label, its action and the fields it reads.
function composeTabModel(tab) {
  const d = COMPOSE_TAB_DEFS[tab] || COMPOSE_TAB_DEFS.interactive;
  return { label: d.label, action: d.action, fields: [...COMPOSE_SHARED_FIELDS, ...d.fields] };
}

// composeFormFor is raw (composeForm()) with every field the tab does not read
// reset to its empty value: what a hidden tab holds is never sent.
function composeFormFor(tab, raw) {
  const read = new Set(composeTabModel(tab).fields);
  const out = { ...raw };
  for (const k of Object.keys(out)) {
    if (read.has(k)) continue;
    const v = out[k];
    out[k] = typeof v === "boolean" ? false : Array.isArray(v) ? [] : k === "pty" || k === "record" ? "default" : "";
  }
  return out;
}

// composeTargetOptions is the schedule tab's target select: 新規に作る (the
// first firing creates the task), each listed task a schedule can bind (ended,
// or live in background), and "id で指定" for a pasted id.
function composeTargetOptions(tasks) {
  const ended = new Set(["Succeeded", "Failed", "Cancelled"]);
  const out = [{ value: "", label: "新規に作る" }];
  for (const t of tasks || []) {
    if (!ended.has(t.status) && t.runMode !== "background") continue;
    // A credential has no process to resume; the server refuses it as a
    // schedule's task, naming the kind.
    if (t.kind === "Credential") continue;
    const about = paneAboutText(t);
    out.push({ value: t.id, label: `${t.id.slice(0, 8)} ${repoTail(t.repoPath || "")}${about ? " " + about : ""}` });
  }
  out.push({ value: "#id", label: "id で指定" });
  return out;
}

// composeTargetPick keeps the target across a rebuilt option list. A picked
// task no longer listed (pruned, or now unbindable) stays the pick as a
// pasted id with a note, so the create binds it or fails loudly -- never a
// silent switch to 新規に作る.
function composeTargetPick(keep, options) {
  if (!keep || keep === "#id" || (options || []).some((o) => o.value === keep)) {
    return { value: keep || "", idText: null, note: "" };
  }
  return { value: "#id", idText: keep, note: `対象 ${keep.slice(0, 8)} は一覧から外れました (id で指定に切り替え)` };
}

// resolveTaskIdInput reads a pasted task id: a full id or a unique prefix
// (any case, spaces trimmed) names one task.
function resolveTaskIdInput(input, tasks) {
  const q = String(input || "").trim().toLowerCase();
  if (!q) return { kind: "empty" };
  const hits = (tasks || []).filter((t) => t.id.startsWith(q));
  if (hits.length === 1) return { kind: "one", task: hits[0] };
  if (hits.length > 1) return { kind: "many", tasks: hits };
  return { kind: "none" };
}

// taskResumeActionsModel is a finished task's one-click resumes: where it last
// ran (assignedTo, when it has one) then any runner, each without and with the
// agent's conversation. Assigned mirrors the TUI's r/R, any runner u/U.
function taskResumeActionsModel(t) {
  const runner = typeof t.assignedTo === "string" ? t.assignedTo : "";
  const out = [];
  if (runner) {
    out.push({ label: "▶ Resume assigned", note: "resumed assigned", conversation: false, runner });
    out.push({ label: "▶ Resume conversation assigned", note: "resumed conversation assigned", conversation: true, runner });
  }
  out.push({ label: "▶ Resume any runner", note: "resumed any runner", conversation: false, runner: "" });
  out.push({ label: "▶ Resume conversation any runner", note: "resumed conversation any runner", conversation: true, runner: "" });
  return out;
}

// parseDurationMs reads Go's duration grammar ("90s", "5m", "1h30m"); blank
// is 0, which the server reads as "its default" (none for a deadline). name
// heads the error.
const DUR_UNIT_MS = { ms: 1, s: 1000, m: 60000, h: 3600000 };
function parseDurationMs(name, v) {
  v = (v || "").trim();
  if (v === "") return 0;
  const re = /(\d+(?:\.\d+)?)(ms|s|m|h)/gy;
  let total = 0, at = 0, m;
  while ((m = re.exec(v)) !== null) {
    total += parseFloat(m[1]) * DUR_UNIT_MS[m[2]];
    at = re.lastIndex;
  }
  if (at !== v.length || at === 0) throw new Error(`${name}: "${v}" is not a duration (e.g. 90s, 5m, 1h)`);
  return Math.round(total);
}

// msDurationStr is parseDurationMs's inverse for a prefill ("" for 0).
function msDurationStr(ms) {
  if (!ms) return "";
  const h = Math.floor(ms / 3600000), m = Math.floor(ms % 3600000 / 60000), s = (ms % 60000) / 1000;
  return `${h ? h + "h" : ""}${m ? m + "m" : ""}${s ? s + "s" : ""}`;
}

// resumeDialogDefaults is the Resume… dialog's starting values: the task's
// last run (its mode, one-shot / stream, and a background run's conditions),
// where it last ran when that runner is connected (connectedRunnerIds: a
// background open is never refused for no runner, so a pin to a gone one
// would queue it forever), its own agent, and the agent's conversation on. A
// resume reuses nothing of the last run (A-D7), so these are only what the
// form starts from.
function resumeDialogDefaults(t, connectedRunnerIds) {
  const boxes = composeBoxesFromTask(t);
  const c = t.conditions || {};
  const mode = t.runMode === "background" ? "background" : boxes.oneshot ? "oneshot" : "interactive";
  return {
    mode, stream: boxes.stream, oneshot: mode === "background" && boxes.oneshot,
    runner: typeof t.assignedTo === "string" && t.assignedTo && (connectedRunnerIds || []).includes(t.assignedTo) ? "assigned" : "any",
    conversation: true,
    agent: t.agentProfile || "",
    cond: {
      deadline: msDurationStr(c.deadlineMs) || "1h",
      turnEnd: c.turnEnd && c.turnEnd !== "unset" ? c.turnEnd : "",
      quiet: msDurationStr(c.quietMs), blocked: msDurationStr(c.blockedMs), queueTimeout: msDurationStr(c.queueTimeoutMs),
    },
  };
}

// capHas / capOr / capXor are the page's capability-mask arithmetic. A mask
// is a Number from the wasm bridge (exact to 2^53, capability_width_test.go
// holds Capability under that); JavaScript's & | ^ truncate to 32 signed bits,
// so the bits are combined as BigInts. Every bit test on a cap mask goes
// through these.
// An unset mask (undefined / null) reads as 0, as it did under & | ^.
function capBig(v) { return BigInt(v || 0); }
function capHas(mask, bit) {
  const b = capBig(bit);
  return (capBig(mask) & b) === b;
}
function capOr(a, b) { return Number(capBig(a) | capBig(b)); }
function capXor(a, b) { return Number(capBig(a) ^ capBig(b)); }

// composeAuthoritySummary is the 新規作成 権限 section's closed line: caps,
// scope, and the scope-for overrides when there are any.
function composeAuthoritySummary(capsLabel, scope, scopeFor) {
  let line = `権限  caps: ${capsLabel} · scope: ${scope || "subtree"}`;
  if (scopeFor && scopeFor.length > 0) line += "  +scope-for " + scopeFor.join(" ");
  return line;
}

// findTaskFresh is lookup(), asking the server once (refresh) when the last
// listing does not have the task yet — a feed entry can name a task the poll
// has not shown. null when the server does not list it either.
async function findTaskFresh(lookup, refresh) {
  const t = lookup();
  if (t) return t;
  await refresh();
  return lookup() || null;
}

// resumeDialogRequest turns the dialog's form into sessionReq's argument. The
// mode decides the run: one-shot is always one-shot and never a stream, a
// background resume needs a prompt and takes the conditions. The authority is
// re-granted only when the override is open (null = not given).
function resumeDialogRequest(f, t) {
  const bg = f.mode === "background";
  if (bg && !String(f.prompt || "").trim()) throw new Error("prompt: a background resume needs one");
  const r = {
    resumeTaskId: t.id, agent: f.agent || "", agentArgs: f.args || [],
    resumeConversation: !!f.conversation,
    runner: f.runner === "assigned" && typeof t.assignedTo === "string" ? t.assignedTo : "",
    prompt: f.prompt || "",
    oneshot: f.mode === "oneshot" || (bg && !!f.oneshot),
    eventStream: f.mode !== "oneshot" && !!f.stream,
    runMode: bg ? "background" : "",
    caps: f.override ? f.caps : null,
    scope: f.override ? f.scope : null,
    // The dialog does not edit per-cap overrides; an open override keeps the
    // task's own, since they travel with the scope.
    scopeFor: f.override ? (t.scopeForSpecs || []) : null,
  };
  if (bg) {
    const c = f.cond || {};
    r.deadlineMs = parseDurationMs("deadline", c.deadline);
    r.quietMs = parseDurationMs("quiet", c.quiet);
    r.blockedMs = parseDurationMs("blocked", c.blocked);
    r.queueTimeoutMs = parseDurationMs("queue timeout", c.queueTimeout);
    r.turnEnd = c.turnEnd || "";
  }
  return r;
}

// taskIdDialogModel is which actions the id dialog offers, gated as the task
// sheet and the notification feed gate them: a terminal reattach only for a
// live PTY session, the chat for a live event stream, resume once it ended.
function taskIdDialogModel(t) {
  const alive = t.status === "Running" || t.status === "Detached";
  return {
    reattach: alive && (t.kind === "Interactive" || t.kind === "Oneshot"),
    chat: alive && t.kind === "Stream",
    resume: ["Succeeded", "Failed", "Cancelled"].includes(t.status),
  };
}

// latestLoad lets only the newest of overlapping async loads render.
function latestLoad() {
  let n = 0;
  return { begin: () => ++n, current: (t) => t === n };
}

// --- Credential tasks (docs/superpowers/specs/2026-11-21-credential-tasks-design.md)
//
// The words are Go's (cli.CredentialExpiryLabel / CredentialLastUsedLabel /
// CredentialRowLine, via harness.credentialList); these models only arrange
// them, and say `unknown` / `-` rather than leave a cell blank.

// credentialRowModel is one row of the Credentials dialog from the bridge's
// row. A zero last-use reading is `unknown` (the server keeps it in memory, so
// it means "not seen since the server started", never "never used"); live is
// whether there is anything to revoke.
function credentialRowModel(r) {
  return {
    id: r.id,
    id8: String(r.id).slice(0, 8),
    status: r.status,
    live: r.status === "Active",
    name: r.name || "-",
    expires: r.expires,
    lastUsed: r.lastUsedAt ? r.lastUsed : "unknown",
    caps: r.caps,
    scope: r.scope,
    line: r.line,
  };
}

// credentialIssueTokens serializes the dialog's issue / re-key form to the
// argument line of `credential new` / `credential rekey`, so the PARSER stays
// in Go (cli/verb) and the form cannot send what the CLI refuses. f.mode is
// "new" or "rekey" (f.id then names the credential); f.key is "generate" or
// "pubkey" (f.pubkey the 64 hex). caps and scope are new-only: a re-key from
// the dialog keeps what the credential recorded (narrowing one is typed on
// the command line).
function credentialIssueTokens(f) {
  const ttl = String(f.ttl || "").trim();
  if (!ttl) throw new Error("credential: a ttl is required (30d, 12h, 1d12h; at most 90d)");
  const out = f.mode === "rekey" ? [f.id, "--ttl", ttl] : ["--ttl", ttl];
  if (f.mode !== "rekey") {
    const name = String(f.name || "").trim();
    if (name) out.push("--name", name);
    if (f.caps) out.push("--caps", f.caps);
    if (f.scope) out.push("--scope", f.scope);
  }
  if (f.key === "pubkey") {
    const pk = String(f.pubkey || "").trim();
    if (!pk) throw new Error("credential: paste the holder's public key (64 hex, from `credential keygen`)");
    out.push("--pubkey", pk);
  }
  return out;
}

// credentialSecretModel is the one-time view of a generated secret: what to
// show, the download's file name, and how a script uses it. The secret is
// shown here and nowhere else; the page drops it when the dialog closes.
function credentialSecretModel(r) {
  const id8 = String(r.id).slice(0, 8);
  return {
    title: `credential ${id8}: secret`,
    head: r.text,
    secret: r.secret,
    filename: `credential-${id8}.cred`,
    once: "shown once: copy or download it now. Closing this dialog removes it, and nothing here keeps a copy.",
    hint: "save it as a file only you can read (chmod 600), then run the script with HARNESS_CREDENTIAL_FILE=<that file>",
  };
}
