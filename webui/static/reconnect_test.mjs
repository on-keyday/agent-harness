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
