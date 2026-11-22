// session rename on the WebUI (delegated-regrant spec R3), under `node --test`
// with the real wasm bridge (harness_env.mjs). `make js-test` runs this.
import { test } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { loadPage, recordingCtx } from "./harness_env.mjs";

const dir = path.dirname(fileURLToPath(import.meta.url));
const page = await loadPage(dir);
const ID = "d4".repeat(16);

test("the bridge exports taskRename and the dispatch names it", () => {
  assert.equal(typeof page.bridge.taskRename, "function");
  const how = page.bridge.dispatch()["session rename"];
  assert.ok(how && how.fn === "taskRename", JSON.stringify(how));
});

test("the command input routes session rename to the bridge with id and name", async () => {
  const ctx = recordingCtx(page, { taskRename: "session rename d4d4d4d4: name=worker-1" });
  const out = await page.runVerbCommand(page.tokenize(`session rename ${ID} worker-1`), ctx);
  const call = ctx.calls.find((c) => c[0] === "taskRename");
  assert.ok(call, JSON.stringify(ctx.calls));
  assert.equal(call[1], ID);
  assert.equal(call[2], "worker-1");
  assert.match(String(out), /worker-1/);
});
