// Server-defined actions on the WebUI command line
// (docs/superpowers/specs/2026-11-26-grant-conditions-and-bound-enrollment-design.md, C8, C13),
// under `node --test` with the real wasm bridge (harness_env.mjs). `make
// js-test` runs this.
import { test } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { loadPage, recordingCtx, named } from "./harness_env.mjs";

const dir = path.dirname(fileURLToPath(import.meta.url));
const plain = (v) => JSON.parse(JSON.stringify(v ?? null));
const eq = (a, b, m) => assert.deepEqual(plain(a), plain(b), m);
const page = await loadPage(dir);
const ID = "a".repeat(32);
const OK = { ok: true, text: "done\n" };

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

test("action new hands the raw params and template to the bridge", async () => {
  const { calls, err } = await run("action new --name deploy --param env=choice:staging,prod -- ./deploy.sh --env {env}", { actionPut: OK });
  assert.equal(err, undefined);
  const [, req] = named(calls, "actionPut")[0];
  eq(req, { edit: false, ref: "", revision: 0, name: "deploy", description: "", descriptionSet: false,
    params: ["env=choice:staging,prod"], template: ["./deploy.sh", "--env", "{env}"] });
});

test("action edit names the action and the revision it edits", async () => {
  const { calls, err } = await run("action edit --revision 2 --description x deploy -- true", { actionPut: OK });
  assert.equal(err, undefined);
  const [, req] = named(calls, "actionPut")[0];
  eq([req.edit, req.ref, req.revision, req.description, req.descriptionSet], [true, "deploy", 2, "x", true]);
});

test("a refused put is an error, not output", async () => {
  const { err } = await run("action rm deploy", { actionRemove: { ok: false, text: "action rm deploy: not_found\n" } });
  assert.match(err || "", /not_found/);
});

test("exec --action runs the action with its raw --value words and no argv", async () => {
  const { calls, err } = await run(`exec --action deploy --value env=prod --value ref=v1 ${ID}`);
  assert.equal(err, undefined);
  const [, id, argv, opts] = named(calls, "execRunToOutput")[0];
  eq([id, argv, opts.action, opts.valueSpecs], [ID, [], "deploy", ["env=prod", "ref=v1"]]);
});

test("exec ls names the action an exec runs", async () => {
  const rows = [{ execId: 3, taskId: ID, argvText: "echo hi", actionLabel: "action=hello@r2", traffic: "io=pipe" }];
  const { out, err } = await run("exec ls", { execRunList: rows });
  assert.equal(err, undefined);
  assert.match(out, /echo hi {2}action=hello@r2/);
});
