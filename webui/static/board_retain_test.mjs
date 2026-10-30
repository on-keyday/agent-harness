// Topic retention on the board view, under `node --test` against the real wasm
// bridge: the bridge function exists and refuses without a client, the topic
// row's retention text, and the result line. The button and its prompt are
// DOM, driven in a real browser.
import { test } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { loadPage } from "./harness_env.mjs";

const dir = path.dirname(fileURLToPath(import.meta.url));
const page = await loadPage(dir);

test("the bridge exposes boardRetain", () => {
  assert.equal(typeof page.harness.boardRetain, "function");
});

test("boardRetain with no client rejects instead of hanging", async () => {
  await assert.rejects(page.harness.boardRetain("tree.x.d", "72h"));
});

test("a retained topic's row names its period, setter and disk use", () => {
  assert.equal(page.boardRetainMeta({ retain: "forever", retainBy: "operator", diskBytes: 321 }),
    "  retain=forever by=operator disk=321");
  assert.equal(page.boardRetainMeta({ msgCount: 1 }), "");
});

test("the result line names the target and the change", () => {
  assert.equal(page.boardRetainResultLine("tree.x.d", "168h0m0s"), "board retain: tree.x.d retain=168h0m0s");
  assert.equal(page.boardRetainResultLine("tree.x.d", "cleared"), "board retain: tree.x.d retention cleared");
});

test("the bridge says which topics can be retained, by the server's own rule", () => {
  assert.equal(page.harness.boardRetainable("tree.x.d"), true);
  assert.equal(page.harness.boardRetainable("chat.0123abcd"), false);
  assert.equal(page.harness.boardRetainable("chat.operator"), false);
  assert.equal(page.harness.boardRetainable(""), false);
});
