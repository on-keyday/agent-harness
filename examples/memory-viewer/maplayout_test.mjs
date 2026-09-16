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

// A vertical bias by inbound count: the memories many others point at are the
// load-bearing ones, and putting them along one edge makes that readable at a
// glance. y grows DOWNWARD in SVG, so "higher on screen" is a SMALLER y.
const CHAIN = (n) => Array.from({length: n - 1}, (_, i) => [i, i + 1]);

test("mapLayout without a bias is unchanged", () => {
  // The bias is opt-in; passing nothing must give exactly the old layout, or
  // every picture anyone has already looked at moves.
  const a = page.mapLayout(N, EDGES, 1234567);
  const b = page.mapLayout(N, EDGES, 1234567, null);
  assert.deepEqual([...a.X], [...b.X]);
  assert.deepEqual([...a.Y], [...b.Y]);
});

test("mapLayout with a bias puts the heavy nodes above the light ones", () => {
  const n = 24;
  // weight 1 for the first half, 0 for the second
  const bias = Array.from({length: n}, (_, i) => (i < n / 2 ? 1 : 0));
  const { Y } = page.mapLayout(n, CHAIN(n), 1234567, bias);
  const mean = (xs) => xs.reduce((s, v) => s + v, 0) / xs.length;
  const heavy = mean([...Y].filter((_, i) => bias[i] === 1));
  const light = mean([...Y].filter((_, i) => bias[i] === 0));
  assert.ok(heavy < light,
    `heavy mean y ${heavy.toFixed(1)} should be above light ${light.toFixed(1)}`);
});

test("mapLayout with a bias is still deterministic", () => {
  const n = 12;
  const bias = Array.from({length: n}, (_, i) => i / (n - 1));
  const a = page.mapLayout(n, CHAIN(n), 1234567, bias);
  const b = page.mapLayout(n, CHAIN(n), 1234567, bias);
  assert.deepEqual([...a.Y], [...b.Y]);
});

test("mapLayout with a bias still separates nodes", () => {
  // A bias strong enough to stack every heavy node on one point would satisfy
  // the ordering test and ruin the drawing.
  const n = 24;
  const bias = Array.from({length: n}, (_, i) => (i < n / 2 ? 1 : 0));
  const { X, Y } = page.mapLayout(n, CHAIN(n), 1234567, bias);
  let minSep = Infinity;
  for (let i = 0; i < n; i++)
    for (let j = i + 1; j < n; j++)
      minSep = Math.min(minSep, Math.hypot(X[i] - X[j], Y[i] - Y[j]));
  assert.ok(minSep > 5, `closest pair is ${minSep.toFixed(2)} apart`);
});

// Spearman's rho between the vertical order and the bias order. 1 means the
// drawing is perfectly layered by weight; 0 means the weight decided nothing.
function verticalRankCorrelation(Y, bias) {
  const n = Y.length;
  const byY = [...Y.keys()].sort((a, b) => Y[a] - Y[b]);
  const byW = [...Y.keys()].sort((a, b) => bias[b] - bias[a]);
  const rY = new Map(byY.map((k, i) => [k, i]));
  const rW = new Map(byW.map((k, i) => [k, i]));
  let d2 = 0;
  for (const k of Y.keys()) d2 += (rY.get(k) - rW.get(k)) ** 2;
  return 1 - (6 * d2) / (n * (n * n - 1));
}

test("mapLayout's bias strength is a real dial, not a label", () => {
  // Two settings ship: a lean that leaves the springs in charge, and a layering
  // that puts weight in charge. If the stronger one did not actually order the
  // drawing better, offering both would be offering the same thing twice.
  //
  // The graph has to FIGHT the bias or the test proves nothing. A chain whose
  // weights run along it is trivially layerable and the lean setting scores
  // 0.987 on it — which is what this test asserted against first. Here the edges
  // are a deterministic scramble, so every spring pulls a heavy node toward a
  // light one, the way a real corpus does.
  // Density matched to the corpus this was built for: 169 nodes / 375 edges,
  // mean degree 4.4. A sparser fixture sits in a different regime where both
  // settings score ~0.92 and the comparison says nothing.
  const n = 60;
  const seen = new Set(), edges = [];
  for (const step of [7, 13, 23, 31]) {
    for (let i = 0; i < n; i++) {
      const j = (i * step + 3) % n;
      if (i === j) continue;
      const k = Math.min(i, j) + ":" + Math.max(i, j);
      if (seen.has(k)) continue;
      seen.add(k);
      edges.push([Math.min(i, j), Math.max(i, j)]);
    }
  }
  const bias = Array.from({length: n}, (_, i) => 1 - i / (n - 1));
  const lean = page.mapLayout(n, edges, 1234567, bias);                 // defaults
  const layered = page.mapLayout(n, edges, 1234567, bias, 1800, 0.08);
  const rl = verticalRankCorrelation(lean.Y, bias);
  const rr = verticalRankCorrelation(layered.Y, bias);
  assert.ok(rr > rl + 0.05,
    `layered rho ${rr.toFixed(3)} should clear lean rho ${rl.toFixed(3)} by a margin`);
});

test("mapLayout's defaults are the lean setting", () => {
  // Passing a bias and nothing else must equal passing the lean numbers, or the
  // two call sites drift and the committed picture moves for no stated reason.
  const n = 12;
  const bias = Array.from({length: n}, (_, i) => i / (n - 1));
  const a = page.mapLayout(n, CHAIN(n), 1234567, bias);
  const b = page.mapLayout(n, CHAIN(n), 1234567, bias, 900, 0.02);
  assert.deepEqual([...a.Y], [...b.Y]);
});

test("biasWeights keeps the heavy end apart, which is the end being asked about", () => {
  // A corpus-shaped distribution: a long tail of nothing and a few that
  // everything points at.
  const counts = [...Array(80).fill(0), ...Array(40).fill(1), 6, 8, 13, 19];
  const w = page.biasWeights(counts);
  assert.equal(w[w.length - 1], 1);                       // the heaviest is the top
  assert.equal(w[0], 0);                                  // nothing inbound is the floor
  // 19 and 13 must not be neighbours in weight — that is the failure the rank
  // mapping had, and it put the most-linked memory below several lighter ones.
  const gap = w[w.length - 1] - w[w.length - 2];
  assert.ok(gap > 0.25, `19 and 13 are only ${gap.toFixed(3)} apart in weight`);
  // and the weight must be proportional, so twice the inbound is twice the pull
  assert.ok(Math.abs(page.biasWeights([0, 5, 10])[1] - 0.5) < 1e-9);
});

test("the layered setting puts the most-linked node near the top", () => {
  // The claim the whole bias exists to support, asserted on the shape of graph
  // it was built for: a long tail, dense enough that the springs have a say.
  const counts = [...Array(80).fill(0), ...Array(40).fill(1), 6, 8, 13, 19];
  const n = counts.length;
  const seen = new Set(), edges = [];
  for (const step of [7, 13, 23, 31]) {
    for (let i = 0; i < n; i++) {
      const j = (i * step + 3) % n;
      if (i === j) continue;
      const k = Math.min(i, j) + ":" + Math.max(i, j);
      if (seen.has(k)) continue;
      seen.add(k);
      edges.push([Math.min(i, j), Math.max(i, j)]);
    }
  }
  const { Y } = page.mapLayout(n, edges, 1234567, page.biasWeights(counts), 1800, 0.08);
  const heaviest = counts.indexOf(19);
  const above = [...Y].filter((v) => v < Y[heaviest]).length;
  assert.ok(above / n < 0.15,
    `the most-linked node has ${above} of ${n} above it (${(above / n * 100).toFixed(0)}%)`);
});
