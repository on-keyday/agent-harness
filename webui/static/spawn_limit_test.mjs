// The spawn limits on the WebUI command line, under `node --test` against the
// real wasm bridge (harness_env.mjs). `make js-test` runs this.
import { test } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { loadPage, recordingCtx, named } from "./harness_env.mjs";

const dir = path.dirname(fileURLToPath(import.meta.url));
const page = await loadPage(dir);
const ID = "a".repeat(32);

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

test("typed limits reach the request, beating the panel", async () => {
  const { calls, err } = await run(`submit --spawn-depth 2 --max-descendants unbounded --task t`);
  assert.equal(err, undefined);
  const [, req] = named(calls, "sessionReq")[0];
  assert.equal(req.spawnDepth, "2");
  assert.equal(req.maxDescendants, "unbounded");
});

test("untyped limits leave the panel's to sessionReq, and a resume re-grants nothing", async () => {
  const { calls } = await run(`submit --resume ${ID} --task t`);
  const [, req] = named(calls, "sessionReq")[0];
  assert.equal(req.spawnDepth, null);
  assert.equal(req.maxDescendants, null);
});

test("caps set-defaults writes the limits into the panel through the bridge", async () => {
  const { calls, out, err } = await run(`caps set-defaults --max-descendants 3 --spawn-depth 1`);
  assert.equal(err, undefined);
  const [, d] = named(calls, "setSpawnDefaults")[0];
  assert.equal(d.maxDescendants, "3");
  assert.equal(d.spawnDepth, "1");
  assert.match(out, /max-descendants set: 3/);
  assert.match(out, /spawn-depth set: 1/);
});

test("a bad limit is refused at the parse", async () => {
  const { err } = await run(`caps set-defaults --max-descendants lots`);
  assert.ok(err, "a non-number limit was accepted");
});
