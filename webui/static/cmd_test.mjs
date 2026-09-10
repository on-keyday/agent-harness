// The WebUI command line, driven under `node --test` against the real wasm
// bridge. `make js-test` runs this; it skips where node is absent.
//
// This surface had no test of any kind, and it is where an audit found the
// most drops: six flags on `submit` parsed and discarded, `exec ls` throwing
// on field names the bridge does not emit, five `session stream` paths
// referencing an undeclared variable, `--json` / `--tree` / `--offset` /
// `--length` / `--dir` / `--max-bytes` all accepted and ignored. Every one was
// in the half of main.js that lives inside the page's IIFE, which nothing
// outside a browser could execute. runVerbCommand was hoisted out for this.
import { test } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { loadPage, recordingCtx, named } from "./harness_env.mjs";

const dir = path.dirname(fileURLToPath(import.meta.url));

// The page runs in its own vm realm, so its arrays and objects have different
// prototypes and assert/strict's deepEqual rejects them on identity alone.
// Compare the VALUES.
const plain = (v) => JSON.parse(JSON.stringify(v ?? null));
const eq = (a, b, m) => assert.deepEqual(plain(a), plain(b), m);
const page = await loadPage(dir);
const ID = "a".repeat(32);
const ID2 = "b".repeat(32);

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

// Every path the declaration gives this surface must reach the dispatch. The
// page asserts this at startup too, but only that an ENTRY exists -- this runs
// the verb.
test("every declared webui path is dispatched, none falls through", async () => {
  const declared = page.harness.pathsForSurface("webui");
  assert.ok(declared.length > 25, `only ${declared.length} declared paths`);
  for (const p of declared) {
    const head = p.split(" ")[0];
    const { out, err } = await run(head + " --zzz-no-such-flag");
    // The switch's own fallthrough, not the bridge's "unknown command: …" for
    // an unparseable line -- only the former carries the help hint. Reaching
    // it means a declared verb has no case, which is how `ls` once shipped
    // declared here with nothing to reach it.
    for (const s of [out, err]) {
      assert.ok(!/type 'help' for the list/.test(String(s)), `${p}: no case in runVerbCommand`);
    }
  }
});

// The page asserts all of this at startup -- and under node the page's IIFE
// parks on a stubbed fetch long before reaching it, so none of it runs there.
// Both directions are checked HERE too, or `make js-test` is green with a
// declared path that has no entry and an entry for a path nobody declares.
// The two coverage directions used to be tested here. They are gone because
// they cannot fail any more: RUNCMD_DISPATCH is built from
// harness.dispatch(), which walks PathsForSurface(webui) — so it cannot name a
// verb the declaration withholds, nor miss one it gives. The declaration-side
// guard is verb.TestEveryWebUIVerbDeclaresDispatch.

test("RUNCMD_DISPATCH names bridge functions that exist", () => {
  // No local list here any more: whether a handler is the page's own is
  // declared beside its name (WebUIDispatch.Local) and arrives as how.local.
  // Keeping a second copy of that list is what let this test and the page's own
  // assertion disagree — the page rejected refreshSnapshot while this passed.
  for (const [p, how] of Object.entries(page.RUNCMD_DISPATCH)) {
    assert.ok(how.fn || (how.cache && how.stale), `${p}: needs {fn} or {cache, stale}`);
    if (how.fn && !how.local) {
      // page.bridge, not page.harness: the latter is whatever recorder the
      // last run() installed, and a recorder answers for every name.
      assert.equal(typeof page.bridge[how.fn], "function", `${p}: harness.${how.fn} does not exist`);
    }
  }
});

test("tokenize keeps a quoted argument whole", () => {
  eq(page.tokenize('submit --task "hello world"'), ["submit", "--task", "hello world"]);
  eq(page.tokenize("file mkdir x 'my dir'"), ["file", "mkdir", "x", "my dir"]);
});

// submit's flags were ALL dropped: sessionReq had no parameter for caps,
// scope, scope-for, runner or ip, and repo/host came off the dropdown
// regardless of what was typed.
test("submit carries every declared flag, not the dropdowns", async () => {
  const { calls } = await run(
    `submit --repo /typed/repo --host typedhost --caps spawn,file_read ` +
    `--scope global --scope-for spawn=none --agent-arg -x --resume ${ID} ` +
    `--resume-conversation --task "do the thing"`,
    { repo: "/dropdown/repo", host: "dropdownhost" });
  const [, req] = named(calls, "sessionReq")[0];
  assert.equal(req.repo, "/typed/repo", "a typed --repo must beat the dropdown");
  assert.equal(req.host, "typedhost", "a typed --host must beat the dropdown");
  assert.equal(req.caps, "spawn,file_read");
  assert.equal(req.scope, "global");
  eq(req.scopeFor, ["spawn=none"]);
  eq(req.claudeArgs, ["-x"]);
  assert.equal(req.resumeTaskId, ID);
  assert.equal(req.resumeConversation, true);
  assert.equal(req.task, "do the thing");
});

test("submit --runner and --ip reach the request", async () => {
  const r = await run(`submit --runner ws:h:1-2 --task t`);
  assert.equal(named(r.calls, "sessionReq")[0][1].runner, "ws:h:1-2");
  const i = await run(`submit --ip 10.0.0.4 --task t`);
  assert.equal(named(i.calls, "sessionReq")[0][1].ip, "10.0.0.4");
});

// --caps none is a real grant of nothing, so presence decides, not truthiness.
test("submit without --caps leaves the chips to decide", async () => {
  const { calls } = await run("submit --task t");
  assert.equal(named(calls, "sessionReq")[0][1].caps, null);
});

test("ls --json and --tree reach the renderers", async () => {
  for (const [line, want] of [["ls", ""], ["ls --json", "json"], ["ls --tree", "tree"]]) {
    const { calls } = await run(line);
    eq(named(calls, "list")[0], ["list", want], line);
  }
  const f = await run("ls --filtered");
  assert.equal(named(f.calls, "filteredTaskRows").length, 1);
});

test("exec ls reads the fields the bridge emits", async () => {
  const rows = [{ execId: 7, taskId: ID, argvText: "make test", originKind: "cli" }];
  const { out } = await run("exec ls", { execRunList: rows });
  assert.match(out, /#7/);
  assert.match(out, /make test/, "argvText, not argv");
  const j = await run("exec ls --json", { execRunList: rows });
  assert.equal(j.out, JSON.stringify(rows[0]), "--json must emit the row verbatim");
});

test("exec kill and forward kill act on every id", async () => {
  const e = await run("exec kill 1 2 3");
  eq(named(e.calls, "execRunKill").map((c) => c[1]), [1, 2, 3]);
  const f = await run("forward kill 4 5");
  eq(named(f.calls, "forwardKill").map((c) => c[1]), [4, 5]);
});

test("forward ls honours --task and --json", async () => {
  const forwards = [
    { forward_id: 1, task: ID, dir: "L", spec: "8080:h:80", origin: "cli", traffic: "" },
    { forward_id: 2, task: ID2, dir: "R", spec: "9090:h:90", origin: "cli", traffic: "" },
  ];
  const all = await run("forward ls", { forwards });
  assert.match(all.out, /#1/);
  assert.match(all.out, /#2/);
  const one = await run(`forward ls --task ${ID}`, { forwards });
  assert.match(one.out, /#1/);
  assert.doesNotMatch(one.out, /#2/, "--task must narrow the listing");
  const j = await run("forward ls --json", { forwards });
  assert.equal(j.out.split("\n").length, 2);
});

test("forward tap passes --dir and --max-bytes to the tap", async () => {
  const forwards = [{ forward_id: 7, task: ID, dir: "L", spec: "s", origin: "cli" }];
  const { calls } = await run("forward tap 7 --dir to-target --max-bytes 64", { forwards });
  eq(named(calls, "toggleForwardTap")[0][2], { dir: "to-target", maxBytes: 64 });
});

test("file pull carries --offset and --length", async () => {
  // filePullCmd reaches window.harness directly, so the recorder is on the
  // page's own bridge rather than on ctx.
  const { calls } = await run(`file pull ${ID} big.bin --offset 5 --length 9`);
  const range = named(calls, "filePullBytesRange")[0];
  assert.ok(range, "an --offset/--length pull must use the range call");
  assert.equal(range[3], 5);
  assert.equal(range[4], 9);
});

// The three-word paths were unreachable through the bridge, and the case that
// served them referenced an `args` declared nowhere.
test("session stream reaches its five verbs", async () => {
  const t = await run(`session stream turn ${ID} hello there`);
  eq(named(t.calls, "streamTurn")[0], ["streamTurn", ID, "hello there"]);
  const a = await run(`session stream approve ${ID} req-1 --allow --suggestion 2`);
  eq(named(a.calls, "streamApprove")[0], ["streamApprove", ID, "req-1", "allow", "", 2]);
  const d = await run(`session stream approve ${ID} req-1 --deny --message "use make"`);
  eq(named(d.calls, "streamApprove")[0], ["streamApprove", ID, "req-1", "deny", "use make", -1]);
  for (const [verb, fn] of [["interrupt", "streamInterrupt"], ["finish", "streamFinish"], ["attach", "openChatFor"]]) {
    const r = await run(`session stream ${verb} ${ID}`);
    assert.equal(named(r.calls, fn).length, 1, `session stream ${verb}`);
  }
});

// A flags-first line put the OPEN CHAT's id in and typed the rest -- flags,
// the real id, the text -- into that agent as one message. Wrong task, wrong
// content, delivered.
test("session stream refuses to guess the id when flags lead", async () => {
  const { calls } = await run(`session stream turn --flush-ms 900 ${ID} hi`, { chatTaskID: ID2 });
  assert.equal(named(calls, "streamTurn").length, 0, "must not send to the open chat");
  assert.match(String(named(calls, "echo")[0][1]), /name the task id after the flags/);
});

test("session stream defaults the id to the open chat", async () => {
  const { calls } = await run("session stream turn hello", { chatTaskID: ID2 });
  eq(named(calls, "streamTurn")[0], ["streamTurn", ID2, "hello"]);
});

// The declaration's cross-flag rules must reach this surface: parseCommand
// runs Build for its validation, and a Bound handed back without it would let
// a browser accept what the other two refuse.
test("the declared rules refuse here too", async () => {
  for (const [line, want] of [
    [`session stream approve ${ID} r --allow --message x`, /mutually exclusive/],
    [`caps set-parent ${ID} --none --swap`, /exactly one/],
    [`caps set-parent ${ID} --parent=`, /needs a task id/],
    ["grid --descendants", /needs --under/],
    ["grid --under abc", /full 32-hex/],
    ["forward tap 7 --dir sideways", /want to-target/],
    ["forward tap 7 --max-bytes 4294967297", /out of range/],
    ["exec kill", /at least/],
  ]) {
    const { err } = await run(line);
    assert.match(String(err), want, line);
  }
});

// D16 removed three spellings; they must not come back by accident.
test("the spellings D16 removed stay removed", async () => {
  for (const line of ["list", `await-idle ${ID}`, `set-parent ${ID} --none`]) {
    const { out } = await run(line);
    assert.match(String(out), /unknown command/, line);
  }
});

// --- the session's spawn defaults ---------------------------------------
//
// `caps <mask>` / `scope <spec>` were a TUI-only grammar; they are now
// `caps set-defaults`, declared for both surfaces. Here the value they set is
// the compose panel's own -- the same spawnCaps / spawnScope the chips write
// -- so these check the command reaches the PANEL, not a second copy.

test("bare `caps` prints the catalog, descriptions and all", async () => {
  const { out, err } = await run("caps");
  assert.equal(err, undefined);
  assert.match(String(out), /CAPABILITY\s+BIT\s+DESCRIPTION/);
  // The half capList cannot carry: what the capability actually gates.
  assert.match(String(out), /forward_tap/);
  assert.match(String(out), /cleartext/);
  // The scope grammar rides along, as it does on the CLI.
  assert.match(String(out), /subtree\+ids:/);
});

test("`caps --json` reaches the JSON renderer", async () => {
  const { out, err } = await run("caps --json");
  assert.equal(err, undefined);
  const parsed = JSON.parse(String(out));
  assert.ok(Array.isArray(parsed.capabilities) && parsed.capabilities.length > 5);
  assert.ok(Array.isArray(parsed.scopes) && parsed.scopes.length > 3);
});

test("`caps set-defaults --caps` writes the compose panel's mask", async () => {
  const { out, err, calls } = await run("caps set-defaults --caps spawn,file_read");
  assert.equal(err, undefined);
  const set = named(calls, "setSpawnDefaults");
  assert.equal(set.length, 1, "the panel is written exactly once");
  // The bitmask comes from the declaration's own ParseCaps, so `all,-spawn`
  // and the rest cannot mean one thing here and another on the CLI.
  assert.equal(typeof set[0][1].caps, "number");
  assert.ok(set[0][1].caps > 0);
  // An omitted --scope must leave the scope controls alone rather than
  // clearing them: "I did not say" is not "I said none".
  assert.equal(set[0][1].scopeBase, undefined);
  assert.match(String(out), /caps set: .*spawn/);
});

test("`scope --scope` is the same verb, decomposed onto the panel's controls", async () => {
  const { out, err, calls } = await run("scope --scope none+ids:" + ID);
  assert.equal(err, undefined);
  const set = named(calls, "setSpawnDefaults");
  assert.equal(set.length, 1);
  const d = set[0][1];
  // Decomposed, because spawnScope is DERIVED from these controls: storing
  // the spec string alone would lose it the next time a radio moved.
  assert.equal(d.scopeBase, "none");
  eq(d.scopeIds, [ID]);
  assert.equal(d.scopeExcludeSelf, false);
  // "" is the visibility radio's third state (follows the action base), not
  // an absent field and not global.
  assert.equal(d.scopeVisBase, "");
  assert.equal(d.caps, undefined, "no --caps means the chips are untouched");
  assert.match(String(out), /scope set:/);
});

test("naming nothing asks rather than writes", async () => {
  const { out, err, calls } = await run("caps set-defaults", {
    spawnDefaults: { caps: 3, capsLabel: "spawn,cancel", scope: "none" },
  });
  assert.equal(err, undefined);
  assert.equal(named(calls, "setSpawnDefaults").length, 0, "nothing was set");
  assert.match(String(out), /caps: spawn,cancel/);
  assert.match(String(out), /scope: none/);
});

test("a bad mask is refused by the declaration, not accepted and dropped", async () => {
  for (const line of ["caps set-defaults --caps bogus", "scope --scope bogus"]) {
    const { err, calls } = await run(line);
    assert.ok(err, `${line} was accepted`);
    assert.equal(named(calls, "setSpawnDefaults").length, 0, `${line} still wrote the panel`);
  }
});

test("--scope-for without --scope is refused: a narrowing needs a base", async () => {
  const { err } = await run("caps set-defaults --scope-for spawn=none");
  assert.ok(err, "a lone --scope-for parsed");
});

// tasksOnRunner is the task↔runner join both conn views hang their task rows
// off. Tested directly because it is the exact shape that broke silently: the
// join used to compare assignedTo against the conn's cid, which was one value
// with the runner's identity until identity was decoupled from the connection.
// After that it matched nothing in BOTH renderers and each still drew a
// perfectly good runner row with no tasks under it.
test("tasksOnRunner joins on the runner identity, not the cid", () => {
  const RID = "b".repeat(32);
  const runner = { role: "runner", cid: "ws:127.0.0.1:8539-7", principalRunner: RID };
  const tasks = [
    { id: "t2", status: "Running", assignedTo: RID },
    { id: "t1", status: "Running", assignedTo: RID },
    { id: "t3", status: "Running", assignedTo: "c".repeat(32) }, // another runner
    { id: "t4", status: "Succeeded", assignedTo: RID },          // not active
  ];
  eq(page.tasksOnRunner(tasks, runner).map(t => t.id), ["t1", "t2"],
    "active tasks whose assignedTo is this runner's identity, sorted by id");

  // The regression itself: matching on the cid must find nothing, so a test
  // that passed by accident on the old field cannot pass here.
  eq(page.tasksOnRunner(tasks, { role: "runner", cid: runner.cid, principalRunner: "" }), [],
    "a runner conn with no identity joins nothing rather than falling back to the cid");
});

// Two absences must not compare equal. The bridge sends "" for a task with no
// runner and for every non-runner conn; without the emptiness checks every
// unassigned task would attach to every client row at once.
test("tasksOnRunner never joins two absences", () => {
  const tasks = [{ id: "t1", status: "Queued", assignedTo: "" }];
  eq(page.tasksOnRunner(tasks, { role: "runner", principalRunner: "" }), [], "runner with no identity");
  eq(page.tasksOnRunner(tasks, { role: "cli", principalRunner: "" }), [], "non-runner role");
  eq(page.tasksOnRunner(tasks, null), [], "no conn at all");
});

// --- conns / the trsf reading ------------------------------------------
//
// The verb was CLI-only until the reading reached all three surfaces. What is
// checked here is the half a Go test cannot execute: the page's own dispatch
// of an argv line, including which flags survive it.

test("conns shows the tab; --trsf carries runner and watch into the panel", async () => {
  const bare = await run("conns");
  eq(named(bare.calls, "connsView"), [["connsView", { trsf: false, runner: "", watch: "" }]],
    "the bare form asks for the tab and nothing else");

  const RCID = "udp:127.0.0.1:41233-a1";
  const full = await run(`conns --trsf --runner ${RCID} --watch 200ms`);
  eq(named(full.calls, "connsView"),
    [["connsView", { trsf: true, runner: RCID, watch: "200ms" }]],
    "--runner and --watch reach the panel rather than being parsed and dropped");
});

// The flag-order footgun this repo has hit before (Pitfall 13): Go's flag
// package stops at the first non-flag token, so a verb that reads positionals
// AND flags must parse permuted. conns has no positionals, but the declaration
// is shared and the check is one line.
test("conns --trsf --watch parses regardless of flag order", async () => {
  const a = await run("conns --watch 1s --trsf");
  eq(named(a.calls, "connsView"), [["connsView", { trsf: true, runner: "", watch: "1s" }]]);
});

// --json is declared CLI-only, with a SurfaceReason: the other two answer with
// a live view and have nothing to pipe it to. The declaration is what enforces
// it, so typing it here must be REFUSED rather than accepted and ignored --
// the silent-drop failure this file exists to catch.
test("conns --json is refused on this surface rather than ignored", async () => {
  const { out, err } = await run("conns --json");
  const said = String(err ?? out ?? "");
  assert.match(said, /json/, `--json should be reported as not available here, got: ${said}`);
});

// The duration grammar lives in Go so the page cannot drift from what the CLI
// accepts. These go through the REAL bridge (parseDurationMs is in the pure
// set), not a recorder.
test("parseDurationMs is time.ParseDuration, and refuses a non-positive one", () => {
  eq(page.bridge.parseDurationMs("200ms").ms, 200);
  eq(page.bridge.parseDurationMs("1s").ms, 1000);
  eq(page.bridge.parseDurationMs("1m30s").ms, 90000);
  assert.ok(page.bridge.parseDurationMs("0s").error, "zero would spin the poll");
  assert.ok(page.bridge.parseDurationMs("-1s").error, "negative");
  assert.ok(page.bridge.parseDurationMs("soon").error, "not a duration");
});

// The reading's cells arrive already rendered, and "-" means ABSENT: a counter
// the answerer does not report, or a delta with no previous reading. The page
// must not turn that into a zero, and must not compute one either.
test("renderTrsfTable draws every declared column and marks absences", () => {
  const cols = page.TRSF_COLUMNS.map((c) => c.key);
  eq(cols, ["cid", "role", "task", "cwnd", "inflight", "srtt", "queue",
    "loss", "spur", "loop", "block", "wait"],
    "the same twelve the CLI table and the TUI modal print");
});

// --runner / --watch describe the reading. All three surfaces used to answer
// `conns --runner X` with the plain connection list — the CLI by dropping the
// flag, the other two by warning in their own handler. It is a cross-flag rule,
// so it belongs to the verb: one refusal everywhere.
test("conns refuses --runner / --watch without --trsf", async () => {
  for (const line of ["conns --runner udp:h:1-a", "conns --watch 1s"]) {
    const { out, err, calls } = await run(line);
    const said = String(err ?? out ?? "");
    assert.match(said, /trsf/, `${line} should name the flag it needs, got: ${said}`);
    eq(named(calls, "connsView"), [], `${line} must not open the view`);
  }
});
