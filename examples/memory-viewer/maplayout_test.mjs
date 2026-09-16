// The two pure halves of the map, under node's test runner.
//
// memviewer is one self-contained script and has no test runner, so this pulls
// the JS constant straight out of the Python source and evaluates it in a vm
// with a stub DOM. Function declarations hoist, so the page's own top-level
// statements are allowed to throw on the way past — the same trick
// webui/static/harness_env.mjs uses for main.js.
import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import vm from "node:vm";
import { fileURLToPath } from "node:url";

const dir = path.dirname(fileURLToPath(import.meta.url));

function loadJS() {
  const src = fs.readFileSync(path.join(dir, "memviewer.py"), "utf8");
  const m = src.match(/\nJS = r"""\n([\s\S]*?)\n"""\n/);
  if (!m) throw new Error("could not find the JS constant in memviewer.py");
  const el = new Proxy({}, {
    get: (t, k) => (k in t ? t[k] : () => {}),
    set: (t, k, v) => { t[k] = v; return true; },
  });
  const ctx = {
    console, TextEncoder, TextDecoder, Math, Date, Float64Array, Int32Array,
    Map, Set, JSON, performance,
    // `body` is not optional: boot() ends by giving up through blank(), which
    // writes document.body.innerHTML. Without it the give-up itself throws, as
    // an unhandled rejection after the tests have finished — every test passes
    // and the file still fails.
    document: { getElementById: () => el, addEventListener: () => {},
                querySelectorAll: () => [], querySelector: () => el,
                createElement: () => el, body: el },
    addEventListener: () => {}, fetch: () => new Promise(() => {}),
    window: null, matchMedia: () => ({ matches: false }),
  };
  ctx.window = ctx;
  ctx.globalThis = ctx;
  vm.createContext(ctx);
  try {
    vm.runInContext(m[1], ctx, { filename: "memviewer.js" });
  } catch (e) {
    // boot() reaches for a live DOM and gives up; the declarations above it are
    // already in the context by then.
    if (typeof ctx.mapLayout !== "function") throw e;
  }
  return ctx;
}

const page = loadJS();

// A small graph with one isolated node, so placement of the unconnected is
// covered rather than assumed.
const EDGES = [[0, 1], [1, 2], [2, 0], [3, 1]];
const N = 5;

test("mapLayout returns one finite coordinate pair per node", () => {
  const { X, Y } = page.mapLayout(N, EDGES, 1234567);
  assert.equal(X.length, N);
  assert.equal(Y.length, N);
  for (let i = 0; i < N; i++) {
    assert.ok(Number.isFinite(X[i]), `X[${i}] is ${X[i]}`);
    assert.ok(Number.isFinite(Y[i]), `Y[${i}] is ${Y[i]}`);
  }
});

test("mapLayout is deterministic for the same input", () => {
  // The whole point: this tool is revisited, and if the picture moved between
  // visits you could not tell a change in the notes from a change in layout.
  const a = page.mapLayout(N, EDGES, 1234567);
  const b = page.mapLayout(N, EDGES, 1234567);
  assert.deepEqual([...a.X], [...b.X]);
  assert.deepEqual([...a.Y], [...b.Y]);
});

test("mapLayout separates the nodes it is given", () => {
  // A layout that collapses everything onto one point satisfies "finite" and
  // "deterministic" and is still useless.
  const { X, Y } = page.mapLayout(N, EDGES, 1234567);
  let minSep = Infinity;
  for (let i = 0; i < N; i++)
    for (let j = i + 1; j < N; j++)
      minSep = Math.min(minSep, Math.hypot(X[i] - X[j], Y[i] - Y[j]));
  assert.ok(minSep > 5, `closest pair is ${minSep.toFixed(2)} apart`);
});

test("mapLayout places a graph with no edges at all", () => {
  const { X } = page.mapLayout(3, [], 1234567);
  assert.equal(X.length, 3);
  assert.ok([...X].every(Number.isFinite));
});

// A memory record carries only what the map reads. `links` are raw [[targets]]
// as written; `inbound` is already resolved by the Python side.
const mem = (name, file, links) => ({ name, file, links, inbound: [], area: "", type: "feedback", size: 100, mtime: 0 });

test("projectGraph resolves a link by name and by filename stem", () => {
  const g = page.projectGraph([
    mem("alpha", "alpha.md", ["beta"]),
    mem("beta-display-name", "beta.md", []),
  ]);
  // "beta" is nobody's name; it is beta.md's stem, and that is how half the
  // corpus writes its links.
  assert.equal(g.edges.length, 1);
  // Spread first. The array was built inside the vm realm, so its prototype is
  // that realm's Array.prototype and assert/strict rejects it on identity even
  // when every value matches — webui/static/cmd_test.mjs records the same trap
  // and answers it the same way: compare the VALUES.
  assert.deepEqual([...g.edges[0]], [0, 1]);
});

test("projectGraph accepts a link written with the .md suffix", () => {
  const g = page.projectGraph([
    mem("alpha", "alpha.md", ["beta.md"]),
    mem("beta", "beta.md", []),
  ]);
  assert.equal(g.edges.length, 1);
});

test("projectGraph drops a link that resolves to nothing", () => {
  // Dangling links are the 要保守 panel's business. On the map they would be
  // an edge to a node that is not there.
  const g = page.projectGraph([mem("alpha", "alpha.md", ["nowhere"])]);
  assert.equal(g.edges.length, 0);
  assert.equal(g.names.length, 1);
});

test("projectGraph drops a self-link and emits one edge for a repeated one", () => {
  const g = page.projectGraph([
    mem("alpha", "alpha.md", ["alpha", "beta", "beta"]),
    mem("beta", "beta.md", []),
  ]);
  assert.equal(g.edges.length, 1);
});

test("projectGraph collapses a reciprocal pair into one edge and marks it", () => {
  // Measured over this corpus: 354 of harness's 373 edges are one-way, so the
  // rare reciprocal ones are the part carrying information.
  const g = page.projectGraph([
    mem("alpha", "alpha.md", ["beta"]),
    mem("beta", "beta.md", ["alpha"]),
  ]);
  assert.equal(g.edges.length, 1);
  assert.ok(g.mutual.has("0:1"), [...g.mutual].join(","));
});

test("projectGraph leaves a one-way pair out of mutual", () => {
  const g = page.projectGraph([
    mem("alpha", "alpha.md", ["beta"]),
    mem("beta", "beta.md", []),
  ]);
  assert.equal(g.mutual.size, 0);
});
