// Paging a topic back past the 64-message ring on the board view, under
// `node --test` against the real wasm bridge. The button is DOM, driven in a
// real browser.
import { test } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { loadPage } from "./harness_env.mjs";

const dir = path.dirname(fileURLToPath(import.meta.url));
const page = await loadPage(dir);

test("the oldest seq is found on decimal strings, past 2^53", () => {
  // Board seqs exceed Number.MAX_SAFE_INTEGER: compared as numbers these two
  // round to the same value.
  assert.equal(page.boardMinSeq(["1880503666824183923", "1880503666824183922", "1880503666824183924"]),
    "1880503666824183922");
  assert.equal(page.boardMinSeq(["99", "100"]), "99");
  assert.equal(page.boardMinSeq([]), "");
});

test("boardRead refuses a page cursor that is not a seq, before it needs a client", async () => {
  await assert.rejects(page.harness.boardRead("grp", "abc", 64), /beforeSeq/);
});
