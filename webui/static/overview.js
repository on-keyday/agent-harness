// The 概要 tab: cli.BuildOverview's sections, as harness.overview() hands them
// over (the same object `overview --json` prints). Nothing here computes a
// count, an order, an age or a badge -- the page lays out strings it is given.
// Declared outside main.js's IIFE so `node --test` reaches it
// (overview_test.mjs).

// pickInitialTab is the tab a page load opens: the one remembered in this
// browser when it is still a tab, else タスク (the default before the tab was
// remembered).
function pickInitialTab(stored, names) {
  return typeof stored === "string" && names.includes(stored) ? stored : "tasks";
}

// overviewTier is a task row's colour tier, from the state group Go put it
// in: the tiers stateBadgeColor paints (attention / quiet / the rest).
function overviewTier(group) {
  if (group === "needs you") return "attention";
  if (group === "working") return "quiet";
  return "ready";
}

// overviewSectionsModel lays the Overview out as five sections, in the
// spec's order: a header line with the count (a zero included), the error
// when that section's source failed, and rows that say what a tap opens.
function overviewSectionsModel(o) {
  const short = (id) => String(id || "").slice(0, 8);
  const taskRow = (t) => ({
    kind: "task", key: t.id, tier: overviewTier(t.group),
    badge: t.badge, text: `${t.repo}  ${short(t.id)}  ${t.now}`,
  });
  const g = o.group_counts || {};
  return [
    { section: "needs-you", header: `介入待ち ${o.needs_you_count}`, error: "",
      rows: (o.needs_you || []).map(taskRow) },
    { section: "tasks",
      header: `タスク ready ${g.ready || 0} · working ${g.working || 0} · no report ${g["no report"] || 0} · queued ${g.queued || 0} · finished ${g.finished || 0}`,
      error: "", rows: (o.live || []).map(taskRow) },
    { section: "hosts", header: `ホスト ${o.host_count} (slot ${o.slot_count} · client ${o.operator_clients})`,
      error: o.hosts_err || "",
      rows: (o.hosts || []).map((h) => ({
        kind: "host", key: h.hostname,
        text: `${h.hostname}  ${h.goos}  idle ${h.idle} busy ${h.busy} offline ${h.offline}  tasks ${h.active_tasks}/${h.max_tasks}`,
      })) },
    { section: "forwards", header: `ポート ${o.forward_count}`, error: o.forwards_err || "",
      rows: (o.forwards || []).map((f) => ({
        kind: "forward", key: String(f.id), text: `${f.dir} ${f.spec}  ${f.repo} ${short(f.task_id)}`,
      })) },
    { section: "board", header: `Board ${o.topic_count} topics · ${o.msg_total} msgs`, error: o.board_err || "",
      rows: (o.topics || []).map((t) => ({
        kind: "topic", key: t.name, head: t.head, text: `${t.name}  ${t.msg_count} msgs  ${t.age}`,
      })) },
  ];
}

// renderOverview draws the model into container with textContent only: every
// string in it can come from an agent. h.open(kind, key) is a row tap;
// h.openSection(section) is a header tap.
function renderOverview(container, o, h) {
  container.replaceChildren();
  for (const s of overviewSectionsModel(o)) {
    const box = document.createElement("div");
    box.className = "ov-section";
    const head = document.createElement("button");
    head.type = "button";
    head.className = "ov-head";
    head.textContent = s.header;
    head.addEventListener("click", () => h.openSection(s.section));
    box.appendChild(head);
    if (s.error) {
      const e = document.createElement("div");
      e.className = "ov-err";
      e.textContent = s.error;
      box.appendChild(e);
    }
    for (const r of s.rows) {
      const row = document.createElement("button");
      row.type = "button";
      row.className = "ov-row";
      const line = document.createElement("span");
      line.className = "ov-line";
      if (r.badge) {
        const b = document.createElement("span");
        b.className = "ov-badge";
        b.style.color = stateBadgeColor({ attention: r.tier === "attention", quiet: r.tier === "quiet" });
        b.textContent = r.badge;
        line.appendChild(b);
      }
      const t = document.createElement("span");
      t.className = "ov-text";
      t.textContent = r.text;
      line.appendChild(t);
      row.appendChild(line);
      if (r.head) {
        const hd = document.createElement("span");
        hd.className = "ov-headline";
        hd.textContent = r.head;
        row.appendChild(hd);
      }
      row.addEventListener("click", () => h.open(r.kind, r.key));
      box.appendChild(row);
    }
    container.appendChild(box);
  }
}
