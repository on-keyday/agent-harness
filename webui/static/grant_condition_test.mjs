// Grant conditions on the WebUI (docs/superpowers/specs/2026-11-26-grant-conditions-and-bound-enrollment-design.md),
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

test("a typed --grant-condition rides session new beside its --scope-for", async () => {
  const { calls, err } = await run("session new --scope-for forward_local=global --grant-condition forward_local=ports:8000-8100");
  assert.equal(err, undefined);
  const [, req] = named(calls, "sessionReq")[0];
  eq(req.scopeFor, ["forward_local=global"]);
  eq(req.grantConditions, ["forward_local=ports:8000-8100"]);
});

test("--grant-condition without --scope-for is refused by the declaration", async () => {
  const { err } = await run("session new --grant-condition forward_local=ports:22");
  assert.match(err || "", /scope-for/);
});

// The conditions belong to the overrides, so they follow scopeFor's ladder:
// typed beats the panel, and a resume never takes the panel's.
test("sessionScopeFields carries the conditions with the overrides", () => {
  const panel = { scope: "global", scopeFor: ["forward_local=global"], grantConditions: ["forward_local=ports:22"] };
  eq(page.sessionScopeFields({ resumeTaskId: "", scope: null, scopeFor: null, grantConditions: null }, panel),
    { scope: "global", scopeFor: ["forward_local=global"], grantConditions: ["forward_local=ports:22"] });
  eq(page.sessionScopeFields({ resumeTaskId: "", scope: null, scopeFor: ["cancel=none"], grantConditions: null }, panel),
    { scope: "global", scopeFor: ["cancel=none"], grantConditions: [] }, "typed overrides do not take the panel's conditions");
  eq(page.sessionScopeFields({ resumeTaskId: ID, scope: null, scopeFor: null, grantConditions: null }, panel),
    { scope: "", scopeFor: [], grantConditions: [] });
});

// An open override sends the task's overrides back; it must send their
// conditions too, or the re-grant widens the task.
test("resumeDialogRequest keeps the task's conditions with its overrides", () => {
  const form = { kind: "interactive", runMode: "foreground", runner: "any", override: true, caps: "forward_local", scope: "none", cond: {} };
  const t = { id: ID, scopeForSpecs: ["forward_local=global"], grantConditionSpecs: ["forward_local=ports:22"] };
  const r = page.resumeDialogRequest(form, t);
  eq([r.scopeFor, r.grantConditions], [["forward_local=global"], ["forward_local=ports:22"]]);
  eq(page.resumeDialogRequest({ ...form, override: false }, t).grantConditions, null);
});

test("a schedule's tokens carry its conditions", () => {
  const t = page.composeScheduleTokens({ cron: "@daily", repo: "/r", agent: "claude", deadline: "1h", prompt: "p",
    scope: "subtree", scopeFor: ["forward_local=global"], grantConditions: ["forward_local=ports:22"] });
  const i = t.indexOf("--grant-condition");
  assert.ok(i > 0 && t[i + 1] === "forward_local=ports:22", t.join(" "));
});

// The row offers a condition only where one kind applies to every bit, so an
// inapplicable condition cannot be built (item 34a).
test("conditionKindForCaps", () => {
  eq(page.conditionKindForCaps(["forward_local"]), "ports");
  eq(page.conditionKindForCaps(["forward_local", "forward_remote"]), "ports");
  eq(page.conditionKindForCaps(["exec_run"]), "actions");
  eq(page.conditionKindForCaps(["exec_run", "forward_local"]), "");
  eq(page.conditionKindForCaps(["cancel"]), "");
  eq(page.conditionKindForCaps([]), "");
});

test("conditionForOverride finds a re-grant row's condition by its exact capability list", () => {
  const specs = ["forward_local,forward_remote=ports:22", "exec_run=actions:" + "0a".repeat(16)];
  eq(page.conditionForOverride("forward_local,forward_remote", specs), "ports:22");
  eq(page.conditionForOverride("forward_local", specs), "");
  eq(page.conditionForOverride("exec_run", specs), "actions:" + "0a".repeat(16));
});

test("composeAuthoritySummary names the conditions beside the overrides", () => {
  eq(page.composeAuthoritySummary("forward_local", "", ["forward_local=global"], ["forward_local=ports:22"]),
    "権限  caps: forward_local · scope: subtree  +scope-for forward_local=global  +grant-condition forward_local=ports:22");
  eq(page.composeAuthoritySummary("all", "", []), "権限  caps: all · scope: subtree");
});
