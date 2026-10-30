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

test("the open-by-name input takes a trimmed name and nothing else", () => {
  assert.equal(page.boardOpenTopicName("  tree.x.new  "), "tree.x.new");
  assert.equal(page.boardOpenTopicName("   "), null);
  assert.equal(page.boardOpenTopicName(""), null);
});

test("the bridge has a separate unretain, and it refuses without a client", async () => {
  assert.equal(typeof page.harness.boardUnretain, "function");
  await assert.rejects(page.harness.boardUnretain("tree.x.d"));
});

test("the Retain button says what pressing it does on this topic", () => {
  assert.equal(page.boardRetainButtonLabel(true), "⏳ 期間を変更");
  assert.equal(page.boardRetainButtonLabel(false), "⏳ Retain");
});

test("one unformattable timestamp renders as - instead of throwing", () => {
  // What a server before 84ba1a35 sent for a retained topic nothing was
  // published to: the zero time's negative UnixMilli wrapped to u64.
  assert.equal(page.boardTimeLabel(18446681938112751616), "-");
  assert.equal(page.boardTimeLabel(0), "-");
  assert.equal(page.boardTimeLabel(1793376778357), new Date(1793376778357).toISOString());
});

test("a timed retention says until when; forever does not", () => {
  const until = Date.UTC(2026, 9, 31, 2, 15, 0);
  assert.equal(page.boardRetainMeta({ retain: "1h0m0s", retainUntilMs: until, retainBy: "operator", diskBytes: 7 }),
    "  retain=1h0m0s until=2026-10-31T02:15:00.000Z by=operator disk=7");
  assert.equal(page.boardRetainMeta({ retain: "forever", retainUntilMs: 0, retainBy: "operator", diskBytes: 7 }),
    "  retain=forever by=operator disk=7");
});
