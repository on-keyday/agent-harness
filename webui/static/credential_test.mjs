// Credential tasks on the WebUI, under `node --test` with the real wasm bridge
// (harness_env.mjs). `make js-test` runs this.
//
// What node cannot reach is a server: the generated secret's shape and that it
// authenticates are tested in Go against one (cli TestIssueCredentialHands-
// OverOneWorkingSecret, the core harness.credentialNew calls). Here: the
// page's routing of that secret, the rows, the form's serialization and the
// bridge's own parse.
import { test } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { loadPage, recordingCtx, named } from "./harness_env.mjs";

const dir = path.dirname(fileURLToPath(import.meta.url));
const plain = (v) => JSON.parse(JSON.stringify(v ?? null));
const eq = (a, b, m) => assert.deepEqual(plain(a), plain(b), m);
const page = await loadPage(dir);
const ID = "c1".repeat(16);
const SEED = "5e".repeat(32);
const SECRET = `${ID}:${SEED}`;
const PK = "0a".repeat(32);

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

// storageSpy replaces the page's localStorage / sessionStorage for one test
// and reports every write.
const storageSpy = () => {
  const writes = [];
  const store = (name) => ({
    getItem: () => null,
    setItem: (k, v) => writes.push([name, k, v]),
    removeItem: () => {},
  });
  const saved = [page.localStorage, page.sessionStorage];
  page.localStorage = store("localStorage");
  page.sessionStorage = store("sessionStorage");
  return { writes, restore: () => { [page.localStorage, page.sessionStorage] = saved; } };
};

test("the bridge exports the four credential calls, and the dispatch names them", () => {
  const fns = { new: "credentialNew", rekey: "credentialRekey", ls: "credentialList", revoke: "credentialRevoke" };
  for (const [sub, fn] of Object.entries(fns)) {
    assert.equal(typeof page.bridge[fn], "function", fn);
    const how = page.bridge.dispatch()[`credential ${sub}`];
    assert.ok(how && how.fn === fn, `credential ${sub}: dispatch ${JSON.stringify(how)}`);
  }
  // keygen and bind write a file on the host that will use the credential.
  for (const sub of ["keygen", "bind"]) {
    assert.equal(page.bridge.dispatch()[`credential ${sub}`], undefined, `credential ${sub} is CLI only`);
  }
});

const row = (over) => ({
  id: ID, status: "Active", name: "nightly", expires: "2026-12-22T15:40:00+09:00",
  expiresAt: 1798, lastUsed: "unknown", lastUsedAt: 0, caps: "schedule_run", scope: "global",
  line: "LINE", ...over,
});

test("credentialRowModel: name, expiry and last use, unknown for a zero reading", () => {
  const m = page.credentialRowModel(row());
  assert.equal(m.id8, ID.slice(0, 8));
  assert.equal(m.name, "nightly");
  assert.equal(m.expires, "2026-12-22T15:40:00+09:00");
  assert.equal(m.lastUsed, "unknown", "zero is 'not seen since the server started', said in words");
  assert.equal(m.live, true);
  const used = page.credentialRowModel(row({ lastUsed: "2026-11-22T07:00:00+09:00", lastUsedAt: 5 }));
  assert.equal(used.lastUsed, "2026-11-22T07:00:00+09:00");
  // Even with no label from the bridge, a zero reading is never a blank cell.
  assert.equal(page.credentialRowModel(row({ lastUsed: "" })).lastUsed, "unknown");
  const unnamed = page.credentialRowModel(row({ name: "", status: "Cancelled" }));
  assert.equal(unnamed.name, "-", "an unnamed credential says so");
  assert.equal(unnamed.live, false, "an ended one offers no revoke");
});

test("credentialIssueTokens: the form serializes to a line the shared declaration parses", () => {
  const gen = page.credentialIssueTokens({ mode: "new", ttl: "30d", name: "nightly", key: "generate", caps: "schedule_run,resume_exact", scope: "global" });
  const b = page.bridge.parseCommand(["credential", "new", ...gen], {});
  assert.equal(b.error, undefined, b.error);
  assert.equal(b.flags.ttl, "30d");
  assert.equal(b.flags.name, "nightly");
  assert.equal(b.flags.caps, "schedule_run,resume_exact");
  assert.equal(b.flags.scope, "global");
  assert.ok(!gen.includes("--pubkey"), "generate sends no --pubkey");

  const reg = page.credentialIssueTokens({ mode: "new", ttl: "7d", name: "", key: "pubkey", pubkey: ` ${PK} `, caps: "none", scope: "subtree" });
  const rb = page.bridge.parseCommand(["credential", "new", ...reg], {});
  assert.equal(rb.error, undefined, rb.error);
  assert.equal(rb.flags.pubkey, PK);
  assert.ok(!reg.includes("--name"), "an empty name is not sent");

  const rk = page.credentialIssueTokens({ mode: "rekey", id: ID, ttl: "1d", key: "generate" });
  const kb = page.bridge.parseCommand(["credential", "rekey", ...rk], {});
  assert.equal(kb.error, undefined, kb.error);
  eq(kb.args, [ID]);
  assert.ok(!rk.includes("--caps") && !rk.includes("--scope"), "a re-key from the dialog keeps the recorded authority");

  assert.throws(() => page.credentialIssueTokens({ mode: "new", ttl: "", key: "generate" }), /ttl/);
  assert.throws(() => page.credentialIssueTokens({ mode: "new", ttl: "1d", key: "pubkey", pubkey: "" }), /public key/);
});

test("credential new (generate) on the command line: the secret goes to the dialog, never the output or storage", async () => {
  const spy = storageSpy();
  try {
    const { out, err, calls } = await run("credential new --ttl 1d --name ci", {
      credentialNew: { id: ID, text: `credential ${ID.slice(0, 8)}: issued, expires X`, secret: SECRET },
    });
    assert.equal(err, undefined, err);
    eq(named(calls, "credentialNew")[0].slice(1), [["--ttl", "1d", "--name", "ci"]]);
    const shown = named(calls, "credentialSecret");
    assert.equal(shown.length, 1, "the dialog is handed the secret");
    assert.equal(shown[0][1].secret, SECRET);
    assert.ok(!String(out).includes(SEED), `the output carries the seed: ${out}`);
    assert.ok(String(out).includes("issued") && String(out).includes("Credentials dialog"), out);
    eq(spy.writes, [], "nothing was written to browser storage");
  } finally {
    spy.restore();
  }
});

test("credential new --pubkey prints the bind command and hands nothing to the dialog", async () => {
  const text = `credential ${ID.slice(0, 8)}: issued, expires X\ntask id: ${ID}\n  harness-cli credential bind ${ID} --file <your seed file>`;
  const { out, err, calls } = await run(`credential new --ttl 1d --pubkey ${PK}`, { credentialNew: { id: ID, text } });
  assert.equal(err, undefined, err);
  assert.equal(out, text);
  assert.equal(named(calls, "credentialSecret").length, 0);
});

test("credential rekey, ls and revoke reach the bridge", async () => {
  const rk = await run(`credential rekey ${ID} --ttl 2h`, { credentialRekey: { id: ID, text: "rekeyed", secret: SECRET } });
  assert.equal(rk.err, undefined, rk.err);
  eq(named(rk.calls, "credentialRekey")[0].slice(1), [[ID, "--ttl", "2h"]]);
  assert.equal(named(rk.calls, "credentialSecret").length, 1);
  assert.ok(!String(rk.out).includes(SEED));

  const ls = await run("credential ls", { credentialList: { rows: [], text: "no credentials" } });
  assert.equal(ls.out, "no credentials");
  eq(named(ls.calls, "credentialList")[0].slice(1), [{ json: false }]);
  const lj = await run("credential ls --json", { credentialList: { rows: [], text: "[]" } });
  eq(named(lj.calls, "credentialList")[0].slice(1), [{ json: true }]);

  const rv = await run(`credential revoke ${ID}`, { credentialRevoke: `credential ${ID.slice(0, 8)}: revoked` });
  assert.equal(rv.out, `credential ${ID.slice(0, 8)}: revoked`);
  eq(named(rv.calls, "credentialRevoke")[0].slice(1), [ID]);
  assert.equal(named(rv.calls, "credentialsChanged").length, 1, "an open dialog reloads");
});

test("credential new --out is not a flag here: a browser writes no path", async () => {
  const { err, calls } = await run("credential new --ttl 1d --out ./x.cred");
  assert.ok(err, "accepted --out");
  assert.equal(named(calls, "credentialNew").length, 0);
});

test("the real bridge parses before it needs a server", async () => {
  await assert.rejects(page.bridge.credentialNew(["--ttl", "1d", "--out", "x"]), /out/);
  await assert.rejects(page.bridge.credentialNew(["--name", "x"]), /ttl/);
  // A well-formed line gets as far as the connection, which node has none of.
  await assert.rejects(page.bridge.credentialNew(["--ttl", "1d"]), (e) => !/ttl|out/.test(e.message));
});

test("credentialSecretModel: a download name and the env the script runs with", () => {
  const m = page.credentialSecretModel({ id: ID, text: "credential c1c1c1c1: issued, expires X", secret: SECRET });
  assert.equal(m.secret, SECRET);
  assert.equal(m.filename, `credential-${ID.slice(0, 8)}.cred`);
  assert.ok(m.hint.includes("HARNESS_CREDENTIAL_FILE"), m.hint);
  assert.ok(m.once.length > 0);
});
