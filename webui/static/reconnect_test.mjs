// What the page does when the phone wakes: which events poke the bridge, what a
// timed-out preview start says, and which panes come back after 'connected'.
// Only reconnect.js is reachable here (main.js's IIFE needs a live DOM), which
// is why the decisions live in that file.
import { test } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { loadPage } from "./harness_env.mjs";

const dir = path.dirname(fileURLToPath(import.meta.url));
const page = await loadPage(dir);

function fakeEnv({ up }) {
  const calls = [];
  const listeners = { doc: {}, win: {} };
  const doc = { hidden: true, addEventListener: (n, f) => { listeners.doc[n] = f; } };
  const win = { addEventListener: (n, f) => { listeners.win[n] = f; } };
  const harness = {
    reconnectNow: () => calls.push(["reconnectNow"]),
    setPageVisible: (v) => calls.push(["setPageVisible", v]),
    probeConnection: async (ms) => { calls.push(["probeConnection", ms]); return true; },
  };
  const state = { up };
  page.installWakeReconnect(win, doc, harness, () => state.up);
  return { calls, listeners, doc, state };
}
const tick = () => new Promise((r) => setTimeout(r, 0));

test("visible / online / pageshow(persisted) kick a connection that is down", async () => {
  const e = fakeEnv({ up: false });
  e.doc.hidden = false; e.listeners.doc.visibilitychange();
  e.listeners.win.online();
  e.listeners.win.pageshow({ persisted: true });
  await tick();
  assert.equal(e.calls.filter((c) => c[0] === "reconnectNow").length, 3);
  assert.equal(e.calls.filter((c) => c[0] === "probeConnection").length, 0);
});

test("a plain (non-bfcache) pageshow and a hide do not wake anything", async () => {
  const e = fakeEnv({ up: false });
  e.listeners.win.pageshow({ persisted: false });
  e.doc.hidden = true; e.listeners.doc.visibilitychange();
  await tick();
  assert.equal(e.calls.filter((c) => c[0] === "reconnectNow" || c[0] === "probeConnection").length, 0);
});

test("on becoming visible while connected the page probes with a 3s budget", async () => {
  const e = fakeEnv({ up: true });
  e.doc.hidden = false; e.listeners.doc.visibilitychange();
  await tick();
  assert.deepEqual(e.calls.filter((c) => c[0] === "probeConnection"), [["probeConnection", 3000]]);
  assert.equal(e.calls.filter((c) => c[0] === "reconnectNow").length, 0);
});

test("visibility is reported to the bridge so the backoff cap follows it", () => {
  const e = fakeEnv({ up: true });
  e.doc.hidden = true; e.listeners.doc.visibilitychange();
  e.doc.hidden = false; e.listeners.doc.visibilitychange();
  const v = e.calls.filter((c) => c[0] === "setPageVisible").map((c) => c[1]);
  assert.deepEqual(v.slice(-2), [false, true]);
});

test("a start that timed out reads as waiting for the connection, not as an error", () => {
  const f = page.previewStartFailure(Object.assign(new Error("preview: no answer in 10s"), { code: "timeout" }));
  assert.equal(f.waiting, true);
  assert.match(f.text, /接続待ち/);
  const g = page.previewStartFailure(new Error("no such task"));
  assert.equal(g.waiting, false);
  assert.match(g.text, /preview error: no such task/);
  // A server-side "runner not connected" is an answer about the session, not a
  // silent link: it must not be mistaken for the waiting case.
  assert.equal(page.previewStartFailure(new Error("runner not connected")).waiting, false);
});

test("after 'connected' every pane that wants to be live restarts; a paused one does not", async () => {
  const panes = new Map([
    ["preview", { wantLive: true, live: false, waiting: true }],   // start timed out
    ["g1", { wantLive: true, live: true }],                        // stream died with the old connection
    ["g2", { wantLive: false, live: false }],                      // operator paused it
  ]);
  const started = [];
  page.resumePanes(panes, (k) => started.push(k));
  assert.deepEqual(started.sort(), ["g1", "preview"]);
});

test("a timed-out start retries when the connection turns out to be alive, at most 3 times", async () => {
  const mk = (alive) => {
    const p = { wantLive: true, live: false, epoch: 1 };
    const panes = new Map([["k", p]]);
    const started = [];
    const harness = { probeConnection: async () => alive };
    return { p, panes, started, harness, start: (k) => started.push(k) };
  };
  // Alive link: the pane is started again.
  let t = mk(true);
  assert.equal(await page.retryTimedOutStart(t.harness, t.panes, "k", t.p, 1, t.start), "retried");
  assert.deepEqual(t.started, ["k"]);
  // Dead link: the probe closes it and 'connected' restarts the pane (resumePanes); nothing here.
  t = mk(false);
  assert.equal(await page.retryTimedOutStart(t.harness, t.panes, "k", t.p, 1, t.start), "left-to-reconnect");
  assert.deepEqual(t.started, []);
  // Superseded / paused / replaced panes are left alone.
  t = mk(true); t.p.epoch = 2;
  assert.equal(await page.retryTimedOutStart(t.harness, t.panes, "k", t.p, 1, t.start), "stale");
  t = mk(true); t.p.wantLive = false;
  assert.equal(await page.retryTimedOutStart(t.harness, t.panes, "k", t.p, 1, t.start), "stale");
  t = mk(true); t.panes.set("k", {});
  assert.equal(await page.retryTimedOutStart(t.harness, t.panes, "k", t.p, 1, t.start), "stale");
  // A link that answers probes while the attach keeps timing out is a server
  // problem, not a sleeping phone: stop after 3 retries.
  t = mk(true);
  const r = [];
  for (let i = 0; i < 4; i++) r.push(await page.retryTimedOutStart(t.harness, t.panes, "k", t.p, 1, t.start));
  assert.deepEqual(r, ["retried", "retried", "retried", "gave-up"]);
});
