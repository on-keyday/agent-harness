// The delegation bound on the WebUI command line, under `node --test` against
// the real wasm bridge (harness_env.mjs). `make js-test` runs this.
import { test } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { loadPage, recordingCtx, named } from "./harness_env.mjs";

const dir = path.dirname(fileURLToPath(import.meta.url));
const page = await loadPage(dir);

const run = async (line, overrides) => {
  const ctx = recordingCtx(page, overrides);
  let out, err;
  try {
    out = await page.runVerbCommand(page.tokenize(line), ctx);
  } catch (e) {
    err = e.message;
  }
  return { out, err, calls: ctx.calls };
};

test("caps set-defaults writes the bound into the panel through the bridge", async () => {
  const { calls, out, err } = await run(`caps set-defaults --delegate-caps file_read --delegate-scope none`);
  assert.equal(err, undefined);
  const [, d] = named(calls, "setSpawnDefaults")[0];
  assert.equal(d.delegateCapsLabel, "file_read");
  assert.equal(d.delegateScopeBase, "none");
  assert.match(out, /delegate set: file_read/);
  assert.match(out, /delegate-scope set: none/);
});

test("scopeSpec spells an excluded-self subtree as descendants", () => {
  assert.equal(page.bridge.scopeSpec({ base: "subtree", excludeSelf: true, ids: [], visBase: "", visIds: [] }), "descendants");
});
