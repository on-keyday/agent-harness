// Server-kept cron schedules on the WebUI (pillar S), under `node --test` with
// the real wasm bridge (harness_env.mjs). `make js-test` runs this.
import { test } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { loadPage, recordingCtx, named } from "./harness_env.mjs";

const dir = path.dirname(fileURLToPath(import.meta.url));
const plain = (v) => JSON.parse(JSON.stringify(v ?? null));
const eq = (a, b, m) => assert.deepEqual(plain(a), plain(b), m);
const page = await loadPage(dir);
const ID = "0123456789abcdef".repeat(2);

const run = async (line, overrides) => {
  const ctx = recordingCtx(page, overrides);
  let out, err;
  try {
    out = await page.runVerbCommand(page.tokenize(line), ctx);
  } catch (e) {
    err = e.message;
  }
  return { out, err, calls: ctx.calls };
};

test("the bridge exports the six schedule calls", () => {
  for (const fn of ["scheduleCreate", "scheduleEdit", "scheduleDelete", "scheduleRun", "scheduleList", "scheduleShow"]) {
    assert.equal(typeof page.bridge[fn], "function", fn);
  }
});

test("every schedule verb declares a webui dispatch naming a bridge export", () => {
  for (const sub of ["new", "edit", "run", "rm", "ls", "show"]) {
    const how = page.bridge.dispatch()[`schedule ${sub}`];
    assert.ok(how && how.fn, `schedule ${sub}: no dispatch`);
    assert.equal(typeof page.bridge[how.fn], "function", `schedule ${sub} names harness.${how.fn}`);
  }
});

const row = (over) => ({
  id: ID, cron: "0 9 * * *", tz: "Asia/Tokyo", next: "2026-11-16 09:00 JST", last: "none",
  task: "no-task", taskState: "unbound", taskId: "", agent: "claude", repo: "/r", about: '"triage"',
  line: "LINE", ...over,
});

test("scheduleRowModel: a bound task is a link and its status is the Go cell", () => {
  const m = page.scheduleRowModel(row({ task: "aaaaaaaa:running", taskState: "bound", taskId: "a".repeat(32) }));
  assert.equal(m.taskText, "aaaaaaaa:running");
  assert.equal(m.taskId, "a".repeat(32));
  assert.equal(m.taskLink, true);
  assert.equal(m.id8, "01234567");
  assert.equal(m.zone, "Asia/Tokyo");
  assert.equal(m.last, "none", "a never-fired schedule says so, it is not blank");
});

test("scheduleRowModel: created-at-first-firing and no-task have no link", () => {
  const p = page.scheduleRowModel(row({ task: "created-at-first-firing", taskState: "pending_create" }));
  assert.equal(p.taskText, "created-at-first-firing");
  assert.equal(p.taskLink, false);
  const n = page.scheduleRowModel(row());
  assert.equal(n.taskText, "no-task");
  assert.equal(n.taskLink, false);
});

test("scheduleRowModel: next=none and a hidden prompt are shown as words", () => {
  const m = page.scheduleRowModel(row({ next: "none", about: "(hidden)" }));
  assert.equal(m.next, "none");
  assert.equal(m.about, "(hidden)");
});

test("scheduleDetailModel: history rows in the order Go sent, wait statistics beside them", () => {
  const d = {
    id: ID,
    head: ["id: " + ID, "cron: 0 9 * * *", "zone: Asia/Tokyo"],
    waits: "waits: n=2 median=1m30s max=2m0s",
    waitN: 2, waitMedianMs: 90000, waitMaxMs: 120000,
    historyTitle: "history (2, oldest first):",
    history: ["2026-11-16 09:00:00 JST (2026-11-16T00:00:00Z)  fired  due=-", "2026-11-17 09:00:00 JST (2026-11-17T00:00:00Z)  run_now  due=-"],
  };
  const m = page.scheduleDetailModel(d);
  eq(m.head, d.head);
  assert.equal(m.waits, d.waits);
  assert.equal(m.waitN, 2);
  assert.equal(m.historyTitle, "history (2, oldest first):");
  eq(m.history.map((h) => h.text), d.history);
  assert.ok(m.history[0].text.includes("JST") && m.history[0].text.includes("Z)"), "zone and absolute instant both");
});

test("scheduleDetailModel: no history still says so", () => {
  const m = page.scheduleDetailModel({ id: ID, head: [], waits: "waits: n=0 (x)", waitN: 0, historyTitle: "history: none yet", history: [] });
  assert.equal(m.history.length, 0);
  assert.equal(m.historyTitle, "history: none yet");
  assert.equal(m.waitN, 0);
});

const form = (over) => ({
  cron: "0 9 * * *", tz: "Asia/Tokyo", repo: "/r", task: "", host: "", agent: "claude", agentArgs: [],
  prompt: "triage", oneshot: false, record: "default", pty: "default", resumeConversation: false, stream: false,
  deadline: "1h", turnEnd: "", quiet: "", blocked: "", queueTimeout: "",
  caps: "none", scope: "subtree", scopeFor: [], delegateCaps: "none", delegateScope: "subtree",
  spawnDepth: "", maxDescendants: "", ...over,
});

test("Compose's tokens are a line the shared declaration parses, carrying the background fields", async () => {
  const f = form({
    host: "box", agentArgs: ["--a b", "c"], oneshot: true, record: "on", pty: "off", resumeConversation: true,
    turnEnd: "quiet", quiet: "5m", blocked: "2m", queueTimeout: "10m",
    caps: "spawn,file_read", scope: "subtree", scopeFor: [], spawnDepth: "3", maxDescendants: "10",
  });
  const tokens = page.composeScheduleTokens(f);
  const b = page.bridge.parseCommand(["schedule", "new", ...tokens], {});
  assert.equal(b.error, undefined, b.error);
  assert.equal(b.flags.cron, "0 9 * * *");
  assert.equal(b.flags.tz, "Asia/Tokyo");
  assert.equal(b.flags.repo, "/r");
  assert.equal(b.flags.host, "box");
  assert.equal(b.flags.agent, "claude");
  assert.equal(b.flags.prompt, "triage");
  assert.equal(b.flags.deadline, "1h0m0s");
  assert.equal(b.flags.quiet, "5m0s");
  assert.equal(b.flags["turn-end"], "quiet");
  assert.equal(b.flags.oneshot, true);
  assert.equal(b.flags.record, true);
  assert.equal(b.flags["no-pty"], true);
  assert.equal(b.flags["resume-conversation"], true);
  eq(b.custom["agent-arg"], ["--a b", "c"]);
  assert.equal(b.flags.caps, "spawn,file_read");
  assert.equal(b.flags["spawn-depth"], "3");
});

test("Compose: an empty zone is the server's zone, a named one is sent; Task id binds instead of --repo", () => {
  assert.ok(!page.composeScheduleTokens(form({ tz: "" })).includes("--tz"));
  const bound = page.composeScheduleTokens(form({ task: ID }));
  assert.ok(bound.includes("--task") && !bound.includes("--repo"));
  const b = page.bridge.parseCommand(["schedule", "new", ...bound], {});
  assert.equal(b.error, undefined, b.error);
});

test("Compose with a cron refuses what a firing cannot carry, with a message", () => {
  assert.throws(() => page.composeScheduleTokens(form({ stream: true })), /event-stream/);
  assert.throws(() => page.composeScheduleTokens(form({ repo: "", task: "" })), /repo/i);
});

test("schedule … on the command line dispatches all six verbs", async () => {
  let r = await run(`schedule new --cron "0 9 * * *" --repo /r --agent claude --prompt hi --deadline 1h`, { scheduleCreate: "created" });
  assert.equal(r.err, undefined);
  assert.equal(r.out, "created");
  const c = named(r.calls, "scheduleCreate")[0];
  assert.ok(c, "scheduleCreate called");
  eq(c[1].slice(0, 2), ["--cron", "0 9 * * *"]);

  r = await run(`schedule edit ${ID} --cron @daily`, { scheduleEdit: "edited" });
  assert.equal(r.err, undefined);
  assert.equal(r.out, "edited");
  assert.equal(named(r.calls, "scheduleEdit").length, 1);

  r = await run(`schedule run ${ID}`, { scheduleRun: "ran" });
  eq(named(r.calls, "scheduleRun")[0].slice(1), [ID]);
  assert.equal(r.out, "ran");

  r = await run(`schedule rm ${ID}`, { scheduleDelete: "gone" });
  eq(named(r.calls, "scheduleDelete")[0].slice(1), [ID]);
  assert.equal(r.out, "gone");

  r = await run("schedule ls", { scheduleList: { text: "ROW", rows: [] } });
  assert.equal(r.out, "ROW");
  assert.equal(named(r.calls, "scheduleList")[0][1].json, false);

  r = await run(`schedule show ${ID} --json`, { scheduleShow: { text: "{}" } });
  assert.equal(r.out, "{}");
  const s = named(r.calls, "scheduleShow")[0];
  assert.equal(s[1], ID);
  assert.equal(s[2].json, true);
});

test("a parse error on a schedule line is the shared declaration's, and nothing is sent", async () => {
  const r = await run("schedule new --repo /r --agent claude --prompt hi --deadline 1h");
  assert.match(r.err || "", /cron/, "--cron is required, the declaration says so");
  assert.equal(named(r.calls, "scheduleCreate").length, 0);
  const x = await run("schedule new --cron @daily --repo /r --stream");
  assert.ok(x.err, "--stream is not a schedule flag");
  assert.equal(named(x.calls, "scheduleCreate").length, 0);
});

test("the bound schedule is named on a task row, never elided when present", () => {
  const sid = "fedcba9876543210".repeat(2);
  const m = page.taskScheduleModel({ id: ID, scheduleId: sid });
  assert.equal(m.label, "schedule=fedcba98");
  assert.equal(m.id, sid);
  assert.equal(page.taskScheduleModel({ id: ID, scheduleId: "" }), null);
  assert.equal(page.taskScheduleModel({ id: ID }), null);
});

test("every task event but a busy/idle edge refreshes the rows, task_schedule included", () => {
  assert.equal(page.taskEventRefreshes("task aabbccddeeff kind=TaskSchedule status=Running ts=1 exit=0 schedule=fedcba98"), true);
  assert.equal(page.taskEventRefreshes("task aabbccddeeff kind=TaskStatus status=Running ts=1 exit=0"), true);
  assert.equal(page.taskEventRefreshes("task aabbccddeeff kind=TaskActivity status=Running ts=1 exit=0"), false);
});

test("the zone select offers the server's zone first, as the empty value", () => {
  const o = page.scheduleZoneOptions(["Asia/Tokyo", "UTC"]);
  eq(o[0], { value: "", label: "(server's zone)" });
  eq(o.slice(1).map((x) => x.value), ["Asia/Tokyo", "UTC"]);
});

// --- fix round 1 ---

const tick = () => new Promise((r) => setTimeout(r, 0));
const schedEv = "task aabbccddeeff kind=TaskSchedule status=Running ts=1 exit=0 schedule=fedcba98";
const boundEv = "task aabbccddeeff kind=TaskStarted status=Running ts=1 exit=0 schedule=fedcba98";
const plainEv = "task aabbccddeeff kind=TaskStarted status=Running ts=1 exit=0";

test("task events refresh an open Schedules dialog, coalesced to one load plus one follow-up", async () => {
  let loads = 0, release;
  const gate = () => new Promise((r) => { release = r; });
  let open = true;
  const ref = page.scheduleEventRefresher({ isOpen: () => open, load: async () => { loads++; await gate(); } });
  for (let i = 0; i < 10; i++) ref(i % 2 ? schedEv : boundEv);
  await tick();
  assert.equal(loads, 1, "one load in flight");
  release(); await tick(); await tick();
  assert.equal(loads, 2, "at most one follow-up for the burst");
  release(); await tick(); await tick();
  assert.equal(loads, 2);
});

test("events of unbound tasks and a closed dialog load nothing", async () => {
  let loads = 0, open = true;
  const ref = page.scheduleEventRefresher({ isOpen: () => open, load: async () => { loads++; } });
  ref(plainEv); ref("task aa kind=TaskActivity status=Running ts=1 exit=0");
  await tick();
  assert.equal(loads, 0, "no schedule binding");
  open = false;
  ref(schedEv); ref(boundEv);
  await tick();
  assert.equal(loads, 0, "dialog closed");
});

test("a failed Run now / Delete names the verb and the schedule, without cli's own prefix", () => {
  assert.equal(
    page.scheduleFailureLine("schedule run", ID, new Error("schedule run: no_such_schedule: gone")),
    "schedule run 01234567: no_such_schedule: gone");
  assert.equal(
    page.scheduleFailureLine("schedule rm", ID, new Error("schedule: not connected; call harness.connect first")),
    "schedule rm 01234567: schedule: not connected; call harness.connect first");
});

test("only the latest load renders", () => {
  const l = page.latestLoad();
  const a = l.begin(), b = l.begin();
  assert.equal(l.current(a), false);
  assert.equal(l.current(b), true);
});
