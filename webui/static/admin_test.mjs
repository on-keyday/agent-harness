// The admin screens' models (actions, grant-condition checklist, runner keys)
// under `node --test` with the real wasm bridge (harness_env.mjs). `make
// js-test` runs this. The rules are Go's (actiondef), reached through the
// bridge; what is tested here is that the page asks them and routes the
// answers, and the form state the dialogs edit.
import { test } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { loadPage } from "./harness_env.mjs";

const dir = path.dirname(fileURLToPath(import.meta.url));
const plain = (v) => JSON.parse(JSON.stringify(v ?? null));
const eq = (a, b, m) => assert.deepEqual(plain(a), plain(b), m);
const page = await loadPage(dir);

const VIEW = {
  id: "a1".repeat(16), name: "deploy", description: "ship it", revision: 3,
  params: [{ name: "env", type: "choice", choices: ["staging", "prod"] }, { name: "ref", type: "token", choices: [] }],
  template: [
    { literal: "./deploy.sh", param: "", isParam: false },
    { literal: "{env}", param: "", isParam: false },
    { literal: "", param: "env", isParam: true },
    { literal: "", param: "ref", isParam: true },
  ],
  templateText: "./deploy.sh \\{env} {env} {ref}", editedBy: "operator", editedAtMs: 0,
};

test("a view opened in the form and submitted unchanged is the same action, as an edit", () => {
  const f = page.actionFormModel(VIEW);
  eq(page.actionFormToObject(f), {
    ref: VIEW.id, revision: 3, name: "deploy", description: "ship it",
    params: VIEW.params, template: VIEW.template,
  });
  eq(page.actionFormErrors(f), []);
});

test("a new form starts with one literal word and no params", () => {
  const f = page.actionFormModel(null);
  eq(page.actionFormToObject(f), { ref: "", revision: 0, name: "", description: "", params: [], template: [{ literal: "", param: "", isParam: false }] });
});

test("the form is refused by Go's rules, each naming what is wrong", () => {
  const cases = [
    ["unused param", (f) => { f.template = [f.template[0]]; }, "env"],
    ["no choices", (f) => { f.params[0].choices = []; }, "choice"],
    ["comma in a choice", (f) => { f.params[0].choices = ["a,b"]; }, "','"],
    ["empty name", (f) => { f.name = ""; }, "name"],
  ];
  for (const [what, mutate, want] of cases) {
    const f = page.actionFormModel(VIEW);
    mutate(f);
    const errs = page.actionFormErrors(f);
    assert.ok(errs.length > 0 && errs.join(" ").includes(want), `${what}: ${errs}`);
  }
});

test("the program word cannot be made a param", () => {
  const f = page.actionFormModel(VIEW);
  assert.equal(page.actionFormSetElement(f, 0, "env"), false);
  assert.equal(f.template[0].isParam, false);
  assert.equal(page.actionFormSetElement(f, 1, "ref"), true);
  eq(f.template[1], { literal: "", param: "ref", isParam: true });
  assert.equal(page.actionFormSetElement(f, 1, ""), true); // back to a literal
  assert.equal(f.template[1].isParam, false);
});

test("the run form: a select per choice, a text box per token, and no actions said plainly", () => {
  eq(page.actionRunModel([]), { empty: true, actions: [] });
  const m = page.actionRunModel([VIEW]);
  assert.equal(m.empty, false);
  eq(m.actions[0].params, [
    { name: "env", kind: "select", choices: ["staging", "prod"] },
    { name: "ref", kind: "text", choices: [] },
  ]);
});

test("run values: a token Go refuses is refused before sending", () => {
  const a = page.actionRunModel([VIEW]).actions[0];
  eq(page.actionRunValueSpecs(a, { env: "prod", ref: "v1.2" }), { specs: ["env=prod", "ref=v1.2"], error: "" });
  const bad = page.actionRunValueSpecs(a, { env: "prod", ref: "-x" });
  assert.ok(bad.error.includes("ref"), bad.error);
  const missing = page.actionRunValueSpecs(a, { env: "prod" });
  assert.ok(missing.error.includes("ref"), missing.error);
});
