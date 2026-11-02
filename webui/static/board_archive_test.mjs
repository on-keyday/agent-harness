// Flush and archives on the board view, under `node --test` against the real
// wasm bridge: the bridge functions exist and refuse without a client or with
// a cursor that is not a seq, the row text, and where "older" pages from. The
// buttons are DOM, driven in a real browser.
import { test } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { loadPage } from "./harness_env.mjs";

const dir = path.dirname(fileURLToPath(import.meta.url));
const page = await loadPage(dir);

test("the bridge exposes flush and the archive calls", () => {
  for (const f of ["boardFlush", "boardArchive", "boardArchives", "boardArchiveRead", "boardArchivePurge"]) {
    assert.equal(typeof page.harness[f], "function", f);
  }
});

test("flush and archive with no client reject instead of hanging", async () => {
  await assert.rejects(page.harness.boardFlush("tree.x.d"));
  await assert.rejects(page.harness.boardArchive("tree.x.d"));
  await assert.rejects(page.harness.boardArchives(""));
});

test("an archive read refuses a page cursor that is not a seq", async () => {
  await assert.rejects(page.harness.boardArchiveRead("0a1b2c3d4e5f", "abc", 64), /beforeSeq|not connected|client/);
});

test("older pages from the newest on disk when nothing is shown", () => {
  // A flushed topic's ring is empty; "0" is the newest, not an error.
  assert.equal(page.boardOlderCursor([]), "0");
  assert.equal(page.boardOlderCursor(["1880503666824183923", "1880503666824183922"]), "1880503666824183922");
});

test("a flushed topic's row says where its ring starts", () => {
  assert.equal(page.boardRetainMeta({ retain: "forever", retainBy: "operator", diskBytes: 9, retainFlushed: "41" }),
    "  retain=forever by=operator disk=9 flushed≤41");
  assert.equal(page.boardRetainMeta({ retain: "forever", retainBy: "operator", diskBytes: 9, retainFlushed: "" }),
    "  retain=forever by=operator disk=9");
});

test("the result lines name the target and the result", () => {
  assert.equal(page.boardFlushResultLine("kept", { through: "41", dropped: 3 }), "board flush: kept through=41 dropped=3");
  assert.equal(page.boardArchiveResultLine("kept", { id: "0a1b2c3d4e5f", count: 3, through: "41" }),
    "board archive: kept → 0a1b2c3d4e5f (3 msgs through 41)");
});
