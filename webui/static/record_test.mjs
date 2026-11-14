// PTY records on the WebUI (pillar A spec, A-D28), under `node --test` with
// the real wasm bridge (harness_env.mjs). `make js-test` runs this.
import { test } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { loadPage } from "./harness_env.mjs";

const dir = path.dirname(fileURLToPath(import.meta.url));
const page = await loadPage(dir);
const ID = "0123456789abcdef".repeat(2);

test("the bridge exports every record call", () => {
  for (const fn of ["recordList", "recordGet", "recordDelete", "recordKeep", "recordSwitch"]) {
    assert.equal(typeof page.harness[fn], "function", fn);
  }
});

test("Compose's record select maps to the open's record option; default sends nothing", () => {
  assert.equal(page.composeRecordOption("default"), "");
  assert.equal(page.composeRecordOption(""), "");
  assert.equal(page.composeRecordOption("on"), "on");
  assert.equal(page.composeRecordOption("off"), "off");
});

test("a record download is named for its task and run", () => {
  assert.equal(page.recordDownloadName(ID, 3, "raw"), "01234567-run3.raw");
  assert.equal(page.recordDownloadName(ID, 3, "asciicast"), "01234567-run3.cast");
});

test("the sheet lists each record with its size and expiry, and what can be done", () => {
  const recs = [
    { run: 2, row: "#2  size=3.0MiB  expires=forever", recording: true },
    { run: 1, row: "#1  size=1.0KiB  expires=2026-12-01 00:00:00", recording: false },
  ];
  const m = page.recordSheetModel({ status: "Running", liveTerminal: true, recording: true }, recs);
  assert.deepEqual(m.rows.map((r) => r.text), [
    "#2  size=3.0MiB  expires=forever  (recording)",
    "#1  size=1.0KiB  expires=2026-12-01 00:00:00",
  ]);
  assert.deepEqual([...m.rows[0].actions], ["raw", "asciicast", "keep", "delete"]); // copied out of the vm realm
  assert.equal(m.switchTo, "off", "a live recorded session offers switching off");
  const idle = page.recordSheetModel({ status: "Running", liveTerminal: true, recording: false }, []);
  assert.equal(idle.switchTo, "on");
  assert.equal(idle.rows.length, 0);
  const ended = page.recordSheetModel({ status: "Succeeded", liveTerminal: false, recording: false }, recs);
  assert.equal(ended.switchTo, "", "no switch without a live terminal");
});

// Gap F (A3): a prefill from a task sets BOTH kind boxes from its last run, so
// neither is left as the previous use of Compose left it.
test("a prefill from a task sets Compose's stream and one-shot boxes", () => {
  assert.deepEqual({ ...page.composeBoxesFromTask({ kind: "Interactive", oneshot: true, noPty: true }) }, { stream: false, oneshot: true, pty: "default" });
  assert.deepEqual({ ...page.composeBoxesFromTask({ kind: "Interactive", oneshot: false }) }, { stream: false, oneshot: false, pty: "default" });
  assert.deepEqual({ ...page.composeBoxesFromTask({ kind: "Stream" }) }, { stream: true, oneshot: false, pty: "default" });
});

// A-D33: the PTY select prefills to what the last run had, stated only where
// it differs from the mode's default, so a prefilled resume runs as it did.
test("a prefill from a task sets Compose's PTY select", () => {
  const pty = (t) => page.composeBoxesFromTask(t).pty;
  assert.equal(pty({ kind: "Interactive", oneshot: false, noPty: true }), "off");
  assert.equal(pty({ kind: "Interactive", oneshot: true, noPty: false }), "on");
  assert.equal(pty({ kind: "Interactive", oneshot: true, noPty: true }), "default");
  assert.equal(pty({ kind: "Interactive", oneshot: false, noPty: false }), "default");
});

test("Compose's PTY select maps to the open's pty option; default sends nothing", () => {
  assert.equal(page.composePtyOption("default"), "");
  assert.equal(page.composePtyOption(""), "");
  assert.equal(page.composePtyOption("on"), "on");
  assert.equal(page.composePtyOption("off"), "off");
});
