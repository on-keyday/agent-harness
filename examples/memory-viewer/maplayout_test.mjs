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
