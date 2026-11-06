"use strict";

// The connection topology, the task tree graph and the trsf table.

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

// tasksOnRunner returns the active tasks assigned to one runner CONNECTION,
// sorted by id, for both places that hang tasks off a runner row.
//
// One function because the join broke in both of them at once and neither
// noticed: they compared task.assignedTo against conn.cid, which was correct
// only while a runner's identity WAS its connection id. After identity was
// decoupled, both silently matched nothing and every runner rendered with no
// tasks however many it was running -- the pure-render kind of defect that a
// screenshot shows and no assertion did. `principalRunner` is the field that
// answers "which runner process is this connection", and assignedTo is that
// same identity, so this is the only pair that joins.
//
// The empty check is not decoration: the bridge sends "" for a task with no
// runner and for every non-runner conn, and without it two absences would
// compare equal and every unassigned task would attach to every client row.
function tasksOnRunner(tasks, conn) {
  if (!conn || conn.role !== "runner" || !conn.principalRunner) return [];
  return (tasks || [])
    .filter(tk => isActiveTask(tk) && tk.assignedTo && tk.assignedTo === conn.principalRunner)
    .sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
}

// connTooltip words a connection for the topology's native SVG tooltip: what
// it is, where it is, and — for a runner — WHICH RUNNER PROCESS.
//
// The topology's leaf labels are three characters ("run", "web"), so the
// identity cannot go in one. Without a tooltip the desktop view of this tab
// named no runner at all: the identity was added to the conn LIST, which the
// >=601px media query hides, so the half of the tab a desktop operator
// actually sees still could not answer "which slot is that". Surface-parity
// item 34a — carry it to the sibling in the same walk.
function connTooltip(conn) {
  const lines = [`${conn.role || "unspecified"}  ${conn.cid || "?"}`];
  if (conn.principalRunner) lines.push(`runner: ${conn.principalRunner}`);
  if (conn.principalTask && conn.principalTask !== "0".repeat(32)) {
    lines.push(`task: ${conn.principalTask}`);
  }
  if (!conn.identified) lines.push("handshake not completed");
  return lines.join("\n");
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
//
// grantEdges and each node's global / offscreen are the authority the creator
// edges do not show (cli.BuildTaskGrants): a task naming another by id in its
// scope, or reaching every task through a global base.
// opts.markBit marks the holders of one capability (a ring; the rest dimmed),
// ended tasks included. opts.focusId pulses the node the caps audit pointed at.
// WHATIF_PHANTOM is the id a spawn what-if's child carries (authority.PhantomID).
const WHATIF_PHANTOM = "new";

function renderTaskTreeGraph(nodes, grantEdges, tasks, statusColor, onSelect, opts = {}) {
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
  const ROW_H = 100;  // four label lines per node (id / status+agent / repo / progress)
  // Asymmetric vertical padding: a node's labels sit ABOVE it (id) and BELOW
  // it (status, repo, progress), and the lowest one reaches R+38. Equal
  // padding clipped the bottom row's repo against the panel edge.
  const PAD_X = 70, PAD_TOP = 34, PAD_BOTTOM = 58;
  const R = 9;

  const byID = new Map((tasks || []).map((t) => [t.id, t]));
  const onScreen = new Set(nodes.map((n) => n.id));
  const edges = (grantEdges || []).filter((e) => onScreen.has(e.from) && onScreen.has(e.to));
  // A grant between two nodes on the same row bows UP (see below), over the
  // id labels; this band keeps the top row's arcs inside the panel.
  const GRANT_BAND = edges.length > 0 ? 44 : 0;
  const pos = new Map();
  let maxCol = 0, maxDepth = 0;
  for (const n of nodes) {
    pos.set(n.id, { x: PAD_X + n.col * COL_W, y: PAD_TOP + GRANT_BAND + n.depth * ROW_H });
    if (n.col > maxCol) maxCol = n.col;
    if (n.depth > maxDepth) maxDepth = n.depth;
  }
  const W = PAD_X * 2 + maxCol * COL_W;
  const H = PAD_TOP + GRANT_BAND + PAD_BOTTOM + maxDepth * ROW_H;

  host.innerHTML = "";
  // A legend only when there is something to decode: a plain creator tree
  // needs none.
  if (edges.length > 0 || nodes.some((n) => n.global || n.offscreen > 0)) {
    const legend = document.createElement("div");
    legend.className = "tt-legend";
    legend.innerHTML =
      '<span class="tt-lg tt-lg-action"><i></i>id で全 cap</span>' +
      '<span class="tt-lg tt-lg-override"><i></i>id で一部の cap</span>' +
      '<span class="tt-lg tt-lg-view"><i></i>id で閲覧のみ</span>' +
      '<span class="tt-lg tt-lg-global"><b></b>global</span>';
    host.appendChild(legend);
  }
  const svg = svgEl("svg", {
    viewBox: `0 0 ${W} ${H}`,
    width: W,
    height: H,
    class: "task-tree-svg",
  });
  const defs = svgEl("defs", {});
  for (const kind of ["action", "override", "view"]) {
    const marker = svgEl("marker", {
      id: `tt-arrow-${kind}`, viewBox: "0 0 10 10", refX: 9, refY: 5,
      markerWidth: 7, markerHeight: 7, orient: "auto-start-reverse",
    });
    marker.appendChild(svgEl("path", { d: "M 0 0 L 10 5 L 0 10 z", class: `tt-arrow tt-arrow-${kind}` }));
    defs.appendChild(marker);
  }
  svg.appendChild(defs);

  // Edges first so the nodes paint over them.
  for (const n of nodes) {
    if (!n.parent || !pos.has(n.parent)) continue;
    const a = pos.get(n.parent), b = pos.get(n.id);
    const midY = (a.y + b.y) / 2;
    // What the creator reaches of this child (cli.BuildTaskGrants
    // ParentReach). Always written, "なし" included: an edge with no label
    // would read the same as one whose label failed to render.
    const caps = n.reachCaps || "none";
    const reachText = caps !== "none" ? caps : (n.reachVisible ? "閲覧のみ" : "なし");
    const g = svgEl("g", { class: "tt-creator" + (caps === "none" ? " tt-creator-noreach" : "") +
      (n.id === WHATIF_PHANTOM ? " tt-phantom" : "") });
    // Elbow rather than a straight diagonal: with several children the
    // straight lines fan into a starburst that is hard to trace back.
    const d = `M ${a.x} ${a.y + R} V ${midY} H ${b.x} V ${b.y - R}`;
    g.appendChild(svgEl("path", { class: "tt-edge-hit", d }));
    g.appendChild(svgEl("path", { class: "tt-edge", d }));
    // Beside the child's own vertical segment: the horizontal run is shared
    // by every sibling, so a label there would belong to none of them.
    const label = svgEl("text", { class: "tt-reach", x: b.x + 4, y: midY + 12 });
    label.textContent = reachText.length > 20 ? reachText.slice(0, 19) + "…" : reachText;
    g.appendChild(label);
    const title = svgEl("title", {});
    title.textContent = `${n.parent.slice(0, 8)} → ${n.id.slice(0, 8)}（作成者から子へ）\n` +
      (caps !== "none" ? `届く caps: ${caps}` : reachText) +
      (caps !== "none" && !n.reachVisible ? "\n（一覧には出ない: 閲覧範囲の外）" : "") +
      "\n直接の作成者の分のみ。祖先の subtree もこの子に届く";
    g.appendChild(title);
    svg.appendChild(g);
  }

  // Grant edges: curved and arrowed, so they never read as one more elbow of
  // the hierarchy. Across rows the bow is to the LEFT of travel, so a→b and
  // b→a bend to opposite sides. On one row "left" would send half of them down
  // through the status and repo labels, so they all bow up into GRANT_BAND,
  // and a reverse pair is told apart by height instead.
  for (const e of edges) {
    const a = pos.get(e.from), b = pos.get(e.to);
    const dx = b.x - a.x, dy = b.y - a.y;
    const len = Math.hypot(dx, dy) || 1;
    let nx = dy / len, ny = -dx / len;
    let bow = Math.min(60, Math.max(20, len * 0.25));
    if (dy === 0) {
      nx = 0; ny = -1;
      bow = Math.min(bow, 2 * GRANT_BAND - 30) + (e.from > e.to ? 24 : 0);
    }
    const cx = (a.x + b.x) / 2 + nx * bow, cy = (a.y + b.y) / 2 + ny * bow;
    // Start and end just outside the dots (global ring included).
    const trim = (px, py, qx, qy, r) => {
      const l = Math.hypot(qx - px, qy - py) || 1;
      return [px + (qx - px) / l * r, py + (qy - py) / l * r];
    };
    const [sx, sy] = trim(a.x, a.y, cx, cy, R + 5);
    const [ex, ey] = trim(b.x, b.y, cx, cy, R + 6);
    const d = `M ${sx} ${sy} Q ${cx} ${cy} ${ex} ${ey}`;
    const g = svgEl("g", { class: `tt-grant tt-grant-${e.kind}` });
    // A wide transparent twin carries the hover: a 1.5px line is too thin to
    // land a tooltip on.
    g.appendChild(svgEl("path", { d, class: "tt-grant-hit" }));
    g.appendChild(svgEl("path", { d, class: "tt-grant-line", "marker-end": `url(#tt-arrow-${e.kind})` }));
    if (e.kind === "override") {
      const label = svgEl("text", {
        class: "tt-grant-label",
        x: 0.25 * sx + 0.5 * cx + 0.25 * ex,
        y: 0.25 * sy + 0.5 * cy + 0.25 * ey - 3,
      });
      label.textContent = e.caps.length > 20 ? e.caps.slice(0, 19) + "…" : e.caps;
      g.appendChild(label);
    }
    const title = svgEl("title", {});
    const what = e.kind === "view" ? "閲覧のみ" : `caps: ${e.caps}`;
    title.textContent = `${e.from.slice(0, 8)} → ${e.to.slice(0, 8)}\n${e.kind} (${what})`;
    g.appendChild(title);
    svg.appendChild(g);
  }

  for (const n of nodes) {
    const p = pos.get(n.id);
    const t = byID.get(n.id);
    const markBit = opts.markBit || 0;
    const holds = markBit !== 0 && t && (t.capsBits & markBit) === markBit;
    // Reach mode (opts.reach, harness.reachOf): filled = acted on, outlined =
    // only seen, the rest dimmed. A global answer fills nothing — the banner
    // says global instead, since a listing is never every task.
    const reach = opts.reach;
    const reachAll = reach && (opts.reachSel === "view" ? reach.seeAll : reach.actAll);
    let reachKind = "";
    if (reach && !reachAll) {
      if (reach.act.includes(n.id)) reachKind = "act";
      else if (reach.seeAll || reach.see.includes(n.id)) reachKind = "see";
      else reachKind = "dim";
    }
    // A what-if (opts.whatIf, the ids harness.whatIf diffed): changed nodes
    // ringed, a spawn's phantom child dashed.
    const phantom = n.id === WHATIF_PHANTOM;
    const g = svgEl("g", {
      class: "tt-node" + (n.orphan ? " tt-orphan" : "") +
        ((markBit !== 0 && !holds) || reachKind === "dim" ? " tt-dim" : "") +
        (n.id === opts.focusId ? " tt-focus" : "") + (phantom ? " tt-phantom" : ""),
      transform: `translate(${p.x},${p.y})`,
      "data-task-id": n.id,
    });
    // Reach mode's radius: the two never draw at once, and a wider ring runs
    // into the id label above and the status line below.
    if (opts.whatIf && opts.whatIf.has(n.id)) g.appendChild(svgEl("circle", { r: R + 6, class: "tt-whatif" }));
    if (holds || n.id === opts.focusId) g.appendChild(svgEl("circle", { r: R + 8, class: "tt-holds-ring" }));
    // Global reach has no target to draw an edge to, so it is a ring on the
    // node itself — under the dot, so the status colour stays readable.
    if (n.global) g.appendChild(svgEl("circle", { r: R + 4, class: `tt-global tt-global-${n.global}` }));
    if (reachKind === "act") g.appendChild(svgEl("circle", { r: R + 6, class: "tt-reach-act" }));
    if (reachKind === "see") g.appendChild(svgEl("circle", { r: R + 6, class: "tt-reach-see" }));
    // Wider than its parent: one mark, never drawn as a defect (V15); the
    // tooltip carries the line `caps reach` prints, granter included.
    if (n.wider) g.appendChild(svgEl("circle", { r: R + 2, class: "tt-wider" }));
    const circle = svgEl("circle", { r: R, class: "tt-dot" });
    // The palette comes in from the caller rather than being restated here:
    // the list's status dots use the same function, and two copies of a colour
    // table drift the moment one status is added.
    if (t && statusColor) circle.setAttribute("fill", statusColor(t.status));
    g.appendChild(circle);

    const label = svgEl("text", { class: "tt-label", y: -R - 6 });
    label.textContent = (n.orphan ? "† " : "") + n.id.slice(0, 8);
    g.appendChild(label);

    // The strongest tier the task holds and how many bits at it (cli.TierMark,
    // rendered in Go). Ended tasks too: a resume brings the caps back.
    if (t && t.tierMark) {
      // Right of the dot, clear of the highlight ring and of the id label
      // above it.
      const tier = svgEl("text", { class: `tt-tier tt-tier-${t.tierClass}`, x: R + 11, y: 4 });
      tier.textContent = t.tierMark;
      g.appendChild(tier);
    }

    const sub = svgEl("text", { class: "tt-sub", y: R + 14 });
    sub.textContent = t ? `${t.status}${t.agentProfile ? " " + agentLabel(t.agentProfile, t.skillsInjected) : ""}`
      : (phantom ? "spawn される子" : "(gone)");
    // The lifecycle badge rides the status line, red when it asks for the
    // operator.
    const nodeLc = t ? taskLifecycleModel(t) : null;
    if (nodeLc) {
      const lcSpan = svgEl("tspan", { class: "tt-lifecycle" });
      if (nodeLc.attention && statusColor) lcSpan.setAttribute("fill", statusColor("Failed"));
      lcSpan.textContent = ` ${nodeLc.text}`;
      sub.appendChild(lcSpan);
    }
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

    // What the task says it is doing, cut to the slot; the tooltip has it
    // whole. textContent, as everywhere: the text is the task's.
    const prog = t ? treeNodeProgress(t) : null;
    if (prog && prog.line !== null) {
      const pl = svgEl("text", { class: prog.cleared ? "tt-progress tt-progress-cleared" : "tt-progress", y: R + 38 });
      pl.textContent = prog.line;
      g.appendChild(pl);
    }

    // Grants whose target is not in the listing (pruned): no edge can be
    // drawn, so the count stands in for them. Shown only when there are any.
    if (n.offscreen > 0) {
      // Below the tier mark when there is one, which takes this spot.
      const off = svgEl("text", { class: "tt-offscreen", x: R + 6, y: t && t.tierMark ? 16 : 4 });
      off.textContent = `+${n.offscreen} 一覧外`;
      g.appendChild(off);
    }

    const title = svgEl("title", {});
    const lines = t ? [n.id, `${t.status}  ${t.repoPath || ""}`] : [n.id];
    if (n.orphan) lines.push("作成者が一覧に居ない（prune 済み / スコープ外）");
    if (n.global) {
      lines.push(n.global === "view"
        ? "global: 全タスクを閲覧"
        : `global: ${n.globalCaps} が全タスクに届く`);
    }
    if (n.offscreen > 0) lines.push(`id 指定の対象のうち ${n.offscreen} 件が一覧に居ない（prune 済み）`);
    if (n.wider) lines.push(n.wider);
    if (prog) lines.push(...prog.tooltip);
    title.textContent = lines.join("\n");
    g.appendChild(title);

    // The diagram is a navigator: clicking a node reveals that task in the
    // list below. The list's own rules (which sheet may be open, what the
    // filters currently hide) belong to the list, so this hands the id over
    // rather than reaching into it.
    // A phantom is in no list.
    if (onSelect && !phantom) g.addEventListener("click", () => onSelect(n.id));
    svg.appendChild(g);
  }
  host.appendChild(svg);
}

// The trsf reading's columns, in the order the CLI table and the TUI modal
// print them — the same twelve, because it is the same reading from the same
// cli.TrsfSampler. num marks the ones that read as quantities and are
// right-aligned.
const TRSF_COLUMNS = [
  { key: "cid", title: "CID" },
  { key: "role", title: "ROLE" },
  { key: "task", title: "TASK" },
  { key: "cwnd", title: "CWND", num: true },
  { key: "inflight", title: "INFLIGHT", num: true },
  { key: "srtt", title: "SRTT", num: true },
  { key: "queue", title: "QUEUE", num: true },
  { key: "loss", title: "LOSS+", num: true },
  { key: "spur", title: "SPUR+", num: true },
  { key: "loop", title: "LOOP+", num: true },
  { key: "block", title: "BLOCK%", num: true },
  { key: "wait", title: "WAIT" },
];

// renderTrsfTable draws one reading into #trsf-table.
//
// Every cell arrives already rendered from cli.TrsfSampler over the bridge, so
// nothing here decides what "-" means — it only dims it, because a screen of
// absences should not read as a screen of zero measurements.
function renderTrsfTable(rows) {
  const table = document.getElementById("trsf-table");
  if (!table) return;
  table.textContent = "";
  const head = document.createElement("tr");
  for (const c of TRSF_COLUMNS) {
    const th = document.createElement("th");
    th.textContent = c.title;
    if (c.num) th.classList.add("trsf-num");
    head.appendChild(th);
  }
  table.appendChild(head);
  for (const r of rows || []) {
    const tr = document.createElement("tr");
    for (const c of TRSF_COLUMNS) {
      const td = document.createElement("td");
      const v = r[c.key] ?? "-";
      td.textContent = v;
      if (c.num) td.classList.add("trsf-num");
      if (v === "-") td.classList.add("trsf-absent");
      tr.appendChild(td);
    }
    table.appendChild(tr);
  }
}

// renderTrsfTargets fills the answerer picker: the server, plus every peer in
// the snapshot that can be asked about its OWN transport.
//
// It listed runners only, and said "any other role is one end of a connection
// the server already reports, so there is nothing separate to ask it". That
// stopped being true when TrsfTarget_Client landed: what the server reports is
// the SERVER's end, and the client's end has its own window, its own send
// queue and its own drops. `conns --trsf --client` could already ask; only
// this picker could not offer it.
//
// keep is a cid to preserve even when the snapshot does not carry it — a
// `conns --trsf --runner|--client <cid>` naming a peer this page has not
// polled yet, or one that has just gone — and keepKind says which end it is,
// which nothing else can supply for a cid the snapshot is missing. Shown
// rather than silently dropped, so the picker cannot disagree with what the
// reading is actually aimed at.
// TRSF_CLIENT_ROLES are the roles whose own transport can be asked about, as
// distinct from the SERVER's end of their connection — which is what the server
// already reports, and is a different window, queue and drop count.
//
// Mirrors peerTargetFor in tui/conns.go. Runner is handled separately because
// it is a different wire target, not because it is a different kind of answer.
const TRSF_CLIENT_ROLES = new Set(["cli", "tui", "webui", "agent"]);

// trsfTargetRows is which peers the picker offers, and in which order — the
// whole DECISION, with no DOM in it, so a node test can assert what the picker
// would contain. renderTrsfTargets below is then only the drawing.
function trsfTargetRows(conns, want, wantKind) {
  const rows = (conns || [])
    .map((c) => ({ cid: c.cid, kind: trsfTargetKind(c.role) }))
    .filter((r) => r.kind)
    .sort((a, b) => (a.cid < b.cid ? -1 : a.cid > b.cid ? 1 : 0));
  // A target set by the command line can name a connection this snapshot does
  // not carry yet; keeping it is what stops the picker from silently moving
  // the reading back to the server under an operator who aimed it elsewhere.
  if (want && !rows.some((r) => r.cid === want)) {
    rows.push({ cid: want, kind: wantKind || "runner" });
  }
  return rows;
}

// trsfTargetKind maps a connection row's role to which of the bridge's two
// peer fields carries it. Derived HERE and stored on the option, so the poll
// site does not re-derive it and the two cannot disagree.
function trsfTargetKind(role) {
  if (role === "runner") return "runner";
  if (TRSF_CLIENT_ROLES.has(role)) return "client";
  return "";
}

function renderTrsfTargets(conns, keep, keepKind) {
  const sel = document.getElementById("trsf-target");
  if (!sel) return;
  const want = keep !== undefined ? keep : sel.value;
  const wantKind = keep !== undefined ? (keepKind || "") : trsfSelectedKind(sel);
  const rows = trsfTargetRows(conns, want, wantKind);
  sel.textContent = "";
  const server = document.createElement("option");
  server.value = "";
  server.dataset.kind = "";
  server.textContent = "server";
  sel.appendChild(server);
  for (const r of rows) {
    const o = document.createElement("option");
    o.value = r.cid;
    o.dataset.kind = r.kind;
    // The kind is shown, not only stored: two rows can differ only by it, and
    // an operator picking a cid has to be able to see which end it means.
    o.textContent = `${r.cid}  (${r.kind})`;
    sel.appendChild(o);
  }
  sel.value = want || "";
}

// trsfSelectedKind reads the kind off the selected option, "" for the server.
function trsfSelectedKind(sel) {
  const o = sel && sel.selectedOptions && sel.selectedOptions[0];
  return o ? (o.dataset.kind || "") : "";
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
      // First child, which is what a browser shows as the node's tooltip.
      const leafTitle = svgEl("title", {});
      leafTitle.textContent = connTooltip(conn);
      leafG.appendChild(leafTitle);
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
      // render as squares on dashed branches rather than on the solid ones.
      {
        const myTasks = tasksOnRunner(tasks, conn);
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

      // Runner identity (runner conns only). Its own element rather than a
      // second use of conn-principal: a task id and a runner id are different
      // namespaces, and an 8-hex prefix with no label is exactly where an
      // operator correlating rows would confuse them. Empty for every other
      // role — the wasm bridge decides that, so no all-zero check here.
      if (conn.principalRunner) {
        const rEl = document.createElement("span");
        rEl.className = "conn-principal conn-runner-id";
        rEl.title = `runner: ${conn.principalRunner}`;
        rEl.textContent = "⚙" + conn.principalRunner.slice(0, 8);
        row.appendChild(rEl);
      }

      // Age (right-aligned)
      const ageEl = document.createElement("span");
      ageEl.className = "conn-age";
      ageEl.textContent = connAgeStr(conn.connectedAt);
      row.appendChild(ageEl);

      card.appendChild(row);

      // Active tasks running on this runner.
      {
        const myTasks = tasksOnRunner(tasks, conn);
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
