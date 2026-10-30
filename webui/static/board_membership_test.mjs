// Board membership on the board view, under `node --test` against the real
// wasm bridge: the bridge function exists and refuses without a client, and
// the pure helpers that decide a subscriber row's button, its topic marks and
// the composer's reply-to. The dialog and buttons are DOM, driven in a real
// browser.
import { test } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { loadPage } from "./harness_env.mjs";

const dir = path.dirname(fileURLToPath(import.meta.url));
const page = await loadPage(dir);

const member = { taskId: "aaaa1111", patterns: [{ name: "grp", joined: true }] };
const plain = { taskId: "bbbb2222", patterns: [{ name: "grp", joined: false }] };

test("the bridge exposes boardMembership", () => {
  assert.equal(typeof page.harness.boardMembership, "function");
});

test("boardMembership with no client rejects instead of hanging", async () => {
  await assert.rejects(page.harness.boardMembership("a".repeat(32), "grp", "join"));
});

test("the bridge says which topics can be joined, by the server's own rule", () => {
  assert.equal(page.harness.boardJoinable("grp"), true);
  assert.equal(page.harness.boardJoinable("chat.0123abcd"), false);
  assert.equal(page.harness.boardJoinable("chat.operator"), false);
  assert.equal(page.harness.boardJoinable(""), false);
});

test("a member's row offers leave, a subscriber's unsubscribe", () => {
  assert.deepEqual({ ...page.boardRowRemoval(member, "grp") }, { label: "外す", op: "leave" });
  assert.deepEqual({ ...page.boardRowRemoval(plain, "grp") }, { label: "購読を外す", op: "unsubscribe" });
});

test("a joined topic is marked as board subscribers marks it", () => {
  assert.equal(page.boardPatternLabel({ name: "grp", joined: true, shown: "0", pending: 1 }), "joined:grp(shown=0 pending=1)");
  assert.equal(page.boardPatternLabel({ name: "grp", shown: "3", pending: 0 }), "grp(shown=3 pending=0)");
});

test("the composer asks for answers on the topic when it has a member", () => {
  assert.equal(page.boardComposeReplyDefault("grp", [plain, member]), "grp");
  assert.equal(page.boardComposeReplyDefault("grp", [plain]), "chat.operator");
  assert.equal(page.boardComposeReplyDefault("grp", []), "chat.operator");
});
