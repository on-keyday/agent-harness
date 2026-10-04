// The board view's operator send, under `node --test` against the real wasm
// bridge. What runs here is the half outside the page's IIFE: the request the
// send dialog builds and the line it reports. The dialog itself is DOM and is
// driven in a real browser (Playwright), not here.
import { test } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { loadPage } from "./harness_env.mjs";

const dir = path.dirname(fileURLToPath(import.meta.url));
const page = await loadPage(dir);
const plain = (v) => JSON.parse(JSON.stringify(v ?? null));

test("the bridge exposes boardSend and boardWake", () => {
  assert.equal(typeof page.harness.boardSend, "function");
  assert.equal(typeof page.harness.boardWake, "function");
});

test("boardSend with no client rejects instead of hanging", async () => {
  await assert.rejects(page.harness.boardSend({ topic: "chat.x", body: "hi" }));
});

test("a new message goes to the open topic and wakes by default", () => {
  assert.deepEqual(plain(page.boardSendRequest({ topic: "chat.x", body: "hi", wake: true, replyTo: "" })),
    { topic: "chat.x", inReplyTo: "0", replyTo: "", noWake: false, body: "hi" });
});

test("a reply names the parent as a decimal string and no topic", () => {
  // A board seq exceeds 2^53, so it crosses the bridge as a string.
  const seq = "9007199254740993";
  assert.deepEqual(plain(page.boardSendRequest({ inReplyTo: seq, body: "a", wake: false, replyTo: "chat.operator" })),
    { topic: "", inReplyTo: seq, replyTo: "chat.operator", noWake: true, body: "a" });
});

test("the result line names the target and the wake", () => {
  assert.equal(page.boardSendResultLine({ seq: "12", deliveredTo: 1 }, { topic: "chat.x", noWake: true }),
    "board send: sent #12 to chat.x (delivered_to=1, wake off)");
  assert.equal(page.boardSendResultLine({ seq: "13", deliveredTo: 0 }, { inReplyTo: "12", noWake: false }),
    "board send: sent #13 as a reply to #12 (delivered_to=0, wake on)");
});

test("the send dialog asks for answers on chat.operator, new message or reply", () => {
  for (const target of [{ topic: "chat.x" }, { inReplyTo: "12" }]) {
    assert.deepEqual(plain(page.boardSendDefaults(target)), { replyTo: "chat.operator", wake: true });
  }
});
