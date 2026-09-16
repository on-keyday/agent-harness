# memviewer whole-project link map — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give memviewer a full-width, zoomable force-directed map of one project's `[[link]]` graph, with switchable colour/size/label channels and an area filter.

**Architecture:** Two pure JavaScript functions (`projectGraph`, `mapLayout`) compute the graph and its coordinates with no DOM and no globals, so they can be asserted under `node --test`. Everything else is rendering: one `<svg>` with a single camera `<g>` that zoom and pan transform. The map replaces the two panes while open and hands control back to the existing detail pane on a node click.

**Tech Stack:** Python 3 stdlib (memviewer is one self-contained script, no packages — see its module docstring), plain ES2020 in the `JS` constant, `node --test` + `node:vm` for the two pure functions, SVG for rendering. No libraries, no build step.

**Spec:** `docs/superpowers/specs/2026-09-16-memviewer-project-map-design.md`

## Global Constraints

- **No third-party dependencies, Python or JavaScript.** memviewer's docstring: *"Standard library only, by design: this runs on a box where installing a package is a decision, not a detail."* This binds the test file too — `node --test` and `node:vm` ship with node.
- **The layout is deterministic.** Same nodes and edges in, byte-identical coordinates out. Spec decision 2.
- **Nothing is added to the always-loaded path.** The map is computed only when opened. Spec decision 3.
- **The map is read-only.** No writes, no fetches, no node dragging. Spec non-goals.
- **memviewer holds its CSS and JS as module-level constants**, so a running `--serve` keeps serving the old page after an edit. Restart it before checking anything in a browser. Spec implementation note.
- Measured budget to stay under: layout of the largest project (168 nodes / 373 edges) took 94.4 ms. Re-derive corpus figures with `python3 examples/memory-viewer/graphstats.py`.
- Verify the browser steps at **1400×950 and at 390×844**, and the page itself must never scroll horizontally at 390 (`document.documentElement.scrollWidth === clientWidth`).

---

### Task 1: `mapLayout` — deterministic coordinates, with a test harness

**Files:**
- Create: `examples/memory-viewer/maplayout_test.mjs`
- Modify: `examples/memory-viewer/memviewer.py` (the `JS` constant, insert before the line `// Collapsed by default: this is a reading tool, and the form should not sit`)

**Interfaces:**
- Consumes: nothing.
- Produces: `mapLayout(n, edges, seed)` where `n` is a node count (integer), `edges` is an array of `[i, j]` index pairs, and `seed` is an integer. Returns `{X: Float64Array, Y: Float64Array}`, both length `n`.

- [ ] **Step 1: Write the failing test**

Create `examples/memory-viewer/maplayout_test.mjs`:

```js
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
    document: { getElementById: () => el, addEventListener: () => {},
                querySelectorAll: () => [], querySelector: () => el,
                createElement: () => el },
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd examples/memory-viewer && node --test maplayout_test.mjs`
Expected: FAIL — `could not find the JS constant` is fine at this point only if the regex is wrong; the intended failure is every test erroring because `ctx.mapLayout` is not a function.

- [ ] **Step 3: Write the implementation**

In `memviewer.py`, insert into the `JS` constant immediately before the line
`// Collapsed by default: this is a reading tool, and the form should not sit`:

```js
// mapLayout: where the whole-project map's nodes sit.
//
// Force-directed, run to completion once rather than animated — measured at
// 94.4 ms for the largest project here (168 nodes, 373 edges), so there is
// nothing worth streaming. Pure: indices in, coordinates out, no DOM, no
// globals, which is what lets maplayout_test.mjs assert it.
//
// The seed is a PARAMETER and the caller passes a constant. This tool is
// revisited; a layout that moved between visits would make a change in the
// notes indistinguishable from a change in the drawing.
function mapLayout(n, edges, seed) {
  const rnd = (() => { let s = seed | 0; return () => {
    s = s + 0x6D2B79F5 | 0;
    let t = Math.imul(s ^ s >>> 15, 1 | s);
    t = t + Math.imul(t ^ t >>> 7, 61 | t) ^ t;
    return ((t ^ t >>> 14) >>> 0) / 4294967296;
  }; })();
  const X = new Float64Array(n), Y = new Float64Array(n);
  const vx = new Float64Array(n), vy = new Float64Array(n);
  for (let i = 0; i < n; i++) {
    const a = rnd() * Math.PI * 2, d = 200 + rnd() * 260;
    X[i] = Math.cos(a) * d; Y[i] = Math.sin(a) * d;
  }
  const ITER = 400, SPRING = 260, REPEL = 9000;
  for (let it = 0; it < ITER; it++) {
    const cool = 1 - it / ITER;
    for (let i = 0; i < n; i++) { vx[i] *= 0.85; vy[i] *= 0.85; }
    for (let i = 0; i < n; i++) for (let j = i + 1; j < n; j++) {
      const dx = X[i] - X[j], dy = Y[i] - Y[j];
      const d2 = Math.max(1, dx * dx + dy * dy), d = Math.sqrt(d2);
      const f = REPEL / d2, ux = dx / d, uy = dy / d;
      vx[i] += ux * f; vy[i] += uy * f; vx[j] -= ux * f; vy[j] -= uy * f;
    }
    for (const [a, b] of edges) {
      const dx = X[b] - X[a], dy = Y[b] - Y[a];
      const d = Math.hypot(dx, dy) || 1;
      const f = (d - SPRING) * 0.0004 * d, ux = dx / d, uy = dy / d;
      vx[a] += ux * f; vy[a] += uy * f; vx[b] -= ux * f; vy[b] -= uy * f;
    }
    // Weak pull to the origin, or the unconnected drift away without limit.
    for (let i = 0; i < n; i++) {
      vx[i] -= X[i] * 0.0012; vy[i] -= Y[i] * 0.0012;
      X[i] += Math.max(-30, Math.min(30, vx[i])) * cool;
      Y[i] += Math.max(-30, Math.min(30, vy[i])) * cool;
    }
  }
  return { X, Y };
}

```

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd examples/memory-viewer && node --test maplayout_test.mjs`
Expected: PASS, 4/4.

- [ ] **Step 5: Check the script still parses and `--check` still runs**

Run: `cd examples/memory-viewer && python3 -c "import ast; ast.parse(open('memviewer.py').read())" && python3 memviewer.py --check > /dev/null; echo "exit $?"`
Expected: no output from the first, `exit 1` from the second (findings exist; 1 is its normal result).

- [ ] **Step 6: Commit**

```bash
git add examples/memory-viewer/memviewer.py examples/memory-viewer/maplayout_test.mjs
git commit -m "feat(memory-viewer): mapLayout, deterministic coordinates for the project map

Pure: node count and edge indices in, Float64Arrays out. No DOM and no
globals, so maplayout_test.mjs can assert the one property that matters —
the same graph gives byte-identical coordinates, because a layout that
moved between visits would make a change in the notes indistinguishable
from a change in the drawing.

Run to completion rather than animated: 94.4 ms measured for the largest
project here (168 nodes, 373 edges)."
```

---

### Task 2: `projectGraph` — nodes and edges from one project's memories

**Files:**
- Modify: `examples/memory-viewer/memviewer.py` (the `JS` constant, insert immediately before `function mapLayout`)
- Modify: `examples/memory-viewer/maplayout_test.mjs` (append tests)

**Interfaces:**
- Consumes: `mapLayout` from Task 1 (not called here, only neighbouring it).
- Produces: `projectGraph(memories)` taking the array from `P.memories`, returning
  `{names: string[], index: Map<string, number>, edges: [number, number][], mutual: Set<string>}`.
  `mutual` holds `"i:j"` keys with `i < j` for pairs that link both ways.

- [ ] **Step 1: Write the failing test**

Append to `examples/memory-viewer/maplayout_test.mjs`:

```js
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
  assert.deepEqual(g.edges[0], [0, 1]);
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd examples/memory-viewer && node --test maplayout_test.mjs`
Expected: the four Task 1 tests PASS, the six new ones FAIL with `page.projectGraph is not a function`.

- [ ] **Step 3: Write the implementation**

In `memviewer.py`, insert into the `JS` constant immediately before `function mapLayout`:

```js
// projectGraph: one project's memories as an undirected graph of indices.
//
// Resolution matches the ego graph's, and for the same reason: a [[target]]
// names a memory by its `name` or by its filename stem, and links never cross
// projects, so the pool is one project's memories and nothing else.
//
// Edges are undirected — 354 of the 373 edges in the largest project here are
// one-way, so arrowheads on all of them would restate what is true of nearly
// all of them. The rare reciprocal pairs are marked instead, in `mutual`,
// keyed "i:j" with i < j.
function projectGraph(memories) {
  const names = memories.map((m) => m.name);
  const index = new Map();
  memories.forEach((m, i) => index.set(m.name, i));
  const byStem = new Map();
  memories.forEach((m, i) => {
    const stem = m.file.replace(/\.md$/, "");
    if (!index.has(stem)) byStem.set(stem, i);
  });
  const resolve = (t) => {
    const k = t.replace(/\.md$/, "");
    return index.has(k) ? index.get(k) : (byStem.has(k) ? byStem.get(k) : -1);
  };
  const directed = new Set();
  memories.forEach((m, i) => {
    for (const raw of m.links) {
      const j = resolve(raw);
      if (j >= 0 && j !== i) directed.add(i + ":" + j);
    }
  });
  const edges = [], mutual = new Set(), seen = new Set();
  for (const key of directed) {
    const [a, b] = key.split(":").map(Number);
    const lo = Math.min(a, b), hi = Math.max(a, b), k = lo + ":" + hi;
    if (seen.has(k)) continue;
    seen.add(k);
    edges.push([lo, hi]);
    if (directed.has(lo + ":" + hi) && directed.has(hi + ":" + lo)) mutual.add(k);
  }
  return { names, index, edges, mutual };
}

```

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd examples/memory-viewer && node --test maplayout_test.mjs`
Expected: PASS, 10/10.

- [ ] **Step 5: Check the real corpus agrees with graphstats.py**

Run: `cd examples/memory-viewer && python3 graphstats.py | sed -n '/graph shape/,/^$/p'`
Expected: remote-agent-harness reports `168` nodes and `373` edges. The browser check in Task 3 will confirm `projectGraph` produces the same counts; they come from the same resolution rule and must not disagree.

- [ ] **Step 6: Commit**

```bash
git add examples/memory-viewer/memviewer.py examples/memory-viewer/maplayout_test.mjs
git commit -m "feat(memory-viewer): projectGraph, one project's links as an index graph

Same resolution rule as the ego graph — name first, filename stem second,
never across projects — so the two cannot disagree about what a [[link]]
points at.

Edges come out undirected with the reciprocal ones marked separately.
Measured: 354 of 373 edges in the largest project are one-way, so the
rare mutual pairs are the part worth styling apart."
```

---

### Task 3: The map surface — toggle, container, first render

**Files:**
- Modify: `examples/memory-viewer/memviewer.py` (`CSS`, the `page()` HTML, and the `JS` constant)

**Interfaces:**
- Consumes: `projectGraph(memories)` and `mapLayout(n, edges, seed)`.
- Produces: `mapState` (module-level object `{open, cam:{x,y,k}, colorBy, sizeBy, labels, area, neighbours, g, pos}`), `openMap()`, `closeMap()`, `renderMap()`.

- [ ] **Step 1: Add the CSS**

In `memviewer.py`, insert into the `CSS` constant immediately before the line
`.send{margin:.8rem 0;`:

```css
/* The map takes both panes: half a screen is not enough for 162 nodes, and on
   a phone the panes stack anyway. */
#mapview{position:relative;height:calc(100vh - 3.2rem);background:#161616;overflow:hidden}
#mapview[hidden]{display:none}
#mapsvg{width:100%;height:100%;display:block;cursor:grab;touch-action:none}
#mapsvg.drag{cursor:grabbing}
#mapsvg line{stroke:#3a3a3a;stroke-width:.8}
#mapsvg circle{stroke:#161616;stroke-width:1}
#mapsvg text{font:9px ui-monospace,Menlo,Consolas,monospace;fill:#9a9a9a;pointer-events:none}
.maphud{position:absolute;right:.6rem;bottom:.6rem;background:#202020cc;border:1px solid #333;
  border-radius:5px;padding:.3rem .55rem;font-size:.75rem;color:#9a9a9a}
.mapbar{display:flex;gap:.5rem;align-items:center;flex-wrap:wrap;padding:.4rem .8rem;
  border-bottom:1px solid #333;background:#1b1b1b}
.mapbar[hidden]{display:none}
```

- [ ] **Step 2: Add the markup**

In `memviewer.py`, in `page()`, replace the line

```html
<div id="areas" class="arearow"></div>
```

with

```html
<div id="areas" class="arearow"></div>
<div id="mapbar" class="mapbar" hidden></div>
<div id="mapview" hidden><svg id="mapsvg"><g id="mapcam"></g></svg><div class="maphud" id="maphud"></div></div>
```

and add a button to the header, immediately before `<span class="meta" id="count"></span>`:

```html
<button class="chip" id="mapbtn">🗺 地図</button>
```

- [ ] **Step 3: Write the open/close/render code**

In `memviewer.py`, insert into the `JS` constant immediately before
`function render() {`:

```js
// The map. Its state lives here rather than in the DOM because the camera has
// to survive a round trip out to a memory and back — walking out to a node and
// returning is the expected loop, and a camera that reset would undo the pan
// that got you there.
const mapState = {open:false, cam:{x:0,y:0,k:1}, colorBy:"area", sizeBy:"in",
                  labels:false, area:"", neighbours:false, g:null, pos:null};

function openMap(){
  if (P.cross) return;           // links never cross projects; nothing to draw
  mapState.open = true;
  // Recomputed per open rather than cached: the corpus is re-read on every
  // request, so a cached layout could describe memories that have changed.
  mapState.g = projectGraph(P.memories);
  mapState.pos = mapLayout(mapState.g.names.length, mapState.g.edges, 1234567);
  $("mapview").hidden = false; $("mapbar").hidden = false;
  document.querySelector("main").hidden = true;
  $("mapbtn").classList.add("on");
  renderMap();
  if (mapState.cam.k === 1 && mapState.cam.x === 0 && mapState.cam.y === 0) mapFit();
  else mapCam();
}

function closeMap(){
  mapState.open = false;
  $("mapview").hidden = true; $("mapbar").hidden = true;
  document.querySelector("main").hidden = false;
  $("mapbtn").classList.remove("on");
}

function renderMap(){
  const {names, edges} = mapState.g, {X, Y} = mapState.pos;
  const xy = (v) => v.toFixed(1);
  let h = "";
  for (const [a, b] of edges)
    h += `<line x1="${xy(X[a])}" y1="${xy(Y[a])}" x2="${xy(X[b])}" y2="${xy(Y[b])}"/>`;
  names.forEach((name, i) => {
    h += `<g data-mapnode="${esc(name)}"><title>${esc(name)}</title>`
       + `<circle cx="${xy(X[i])}" cy="${xy(Y[i])}" r="4" fill="#6bb3f7"/></g>`;
  });
  $("mapcam").innerHTML = h;
  $("maphud").textContent = `${names.length} nodes · ${edges.length} edges · zoom ${mapState.cam.k.toFixed(2)}x`;
}

function mapCam(){
  $("mapcam").setAttribute("transform",
    `translate(${mapState.cam.x} ${mapState.cam.y}) scale(${mapState.cam.k})`);
  $("maphud").textContent = `${mapState.g.names.length} nodes · ${mapState.g.edges.length} edges · zoom ${mapState.cam.k.toFixed(2)}x`;
}

function mapFit(){
  const {X, Y} = mapState.pos;
  if (!X.length) return;
  let x0=Infinity, y0=Infinity, x1=-Infinity, y1=-Infinity;
  for (let i=0;i<X.length;i++){ x0=Math.min(x0,X[i]); x1=Math.max(x1,X[i]);
                                y0=Math.min(y0,Y[i]); y1=Math.max(y1,Y[i]); }
  const w = $("mapsvg").clientWidth, h = $("mapsvg").clientHeight;
  const k = Math.min(w/(x1-x0+120), h/(y1-y0+120));
  mapState.cam.k = k;
  mapState.cam.x = w/2 - ((x0+x1)/2)*k;
  mapState.cam.y = h/2 - ((y0+y1)/2)*k;
  mapCam();
}

```

- [ ] **Step 4: Wire the button**

In `memviewer.py`, in the `JS` constant, inside the `document.addEventListener("click", …)` handler, immediately after the line `if (e.target.id === "sd-go") { doSend(); return; }`:

```js
  if (e.target.id === "mapbtn") { mapState.open ? closeMap() : openMap(); return; }
```

And in the `$("proj").addEventListener("change", …)` handler, immediately after the line `sel = null; area = ""; projFilter = "";`:

```js
  // A different project is a different graph, so the camera from the last one
  // means nothing over it.
  if (mapState.open) closeMap();
  mapState.cam = {x:0, y:0, k:1};
```

- [ ] **Step 5: Restart and check in the browser**

Run:
```bash
pkill -f 'memviewer.py --serve --port 8797'
cd examples/memory-viewer && python3 memviewer.py --serve --port 8797 &
```
Then in a browser at 1400×950, open `http://127.0.0.1:8797/`, click 🗺 地図, and confirm:
- the two panes are replaced by a dark canvas with a graph in it,
- the HUD bottom-right reads `168 nodes · 373 edges` for remote-agent-harness — the same figures `python3 graphstats.py` prints,
- clicking 🗺 地図 again brings the panes back.

- [ ] **Step 6: Commit**

```bash
git add examples/memory-viewer/memviewer.py
git commit -m "feat(memory-viewer): the project map surface, opened from the header

Full width rather than inside the detail pane: half a screen is not enough
for 162 nodes and on a phone the panes stack anyway. State lives in
mapState because the camera has to survive a round trip out to a memory
and back.

The graph and layout are recomputed per open. memviewer re-reads the
corpus on every request, so a cached layout could describe memories that
are no longer there."
```

---

### Task 4: Zoom, pan and fit

**Files:**
- Modify: `examples/memory-viewer/memviewer.py` (the `JS` constant, and one button in the `page()` markup)

**Interfaces:**
- Consumes: `mapState`, `mapCam()`, `mapFit()` from Task 3.
- Produces: no new names; the wheel/pointer handlers and a `全体に合わせる` button.

- [ ] **Step 1: Add the button to the bar**

In `memviewer.py`, in `page()`, replace

```html
<div id="mapbar" class="mapbar" hidden></div>
```

with

```html
<div id="mapbar" class="mapbar" hidden>
  <button class="chip" id="mapfit">全体に合わせる</button>
</div>
```

- [ ] **Step 2: Write the handlers**

In `memviewer.py`, insert into the `JS` constant immediately before `function render() {`:

```js
// Zoom about the CURSOR, not the centre: zooming toward a corner otherwise
// walks the thing you were pointing at off the screen and you pan it back by
// hand every time.
$("mapsvg").addEventListener("wheel", (e) => {
  if (!mapState.open) return;
  e.preventDefault();
  const r = $("mapsvg").getBoundingClientRect();
  const mx = e.clientX - r.left, my = e.clientY - r.top;
  const nk = Math.max(0.05, Math.min(12, mapState.cam.k * Math.exp(-e.deltaY * 0.0015)));
  mapState.cam.x = mx - (mx - mapState.cam.x) * (nk / mapState.cam.k);
  mapState.cam.y = my - (my - mapState.cam.y) * (nk / mapState.cam.k);
  mapState.cam.k = nk;
  mapCam();
}, {passive:false});

let mapDrag = null;
$("mapsvg").addEventListener("pointerdown", (e) => {
  if (!mapState.open) return;
  mapDrag = {x:e.clientX, y:e.clientY, cx:mapState.cam.x, cy:mapState.cam.y, moved:false};
  $("mapsvg").classList.add("drag");
  $("mapsvg").setPointerCapture(e.pointerId);
});
$("mapsvg").addEventListener("pointermove", (e) => {
  if (!mapDrag) return;
  const dx = e.clientX - mapDrag.x, dy = e.clientY - mapDrag.y;
  if (Math.hypot(dx, dy) > 3) mapDrag.moved = true;
  mapState.cam.x = mapDrag.cx + dx; mapState.cam.y = mapDrag.cy + dy;
  mapCam();
});
$("mapsvg").addEventListener("pointerup", () => { mapDrag = null; $("mapsvg").classList.remove("drag"); });

```

- [ ] **Step 3: Wire the fit button**

In the `document.addEventListener("click", …)` handler, immediately after the
`mapbtn` line added in Task 3:

```js
  if (e.target.id === "mapfit") { mapFit(); return; }
```

- [ ] **Step 4: Restart and check in the browser**

Run:
```bash
pkill -f 'memviewer.py --serve --port 8797'
cd examples/memory-viewer && python3 memviewer.py --serve --port 8797 &
```
At 1400×950 with the map open:
- wheel up over a node in a corner: that node stays under the cursor, and the HUD's zoom figure rises,
- drag: the graph follows the pointer,
- 全体に合わせる: the whole graph is back in view and the HUD reads roughly `0.4x` for remote-agent-harness.

- [ ] **Step 5: Commit**

```bash
git add examples/memory-viewer/memviewer.py
git commit -m "feat(memory-viewer): zoom, pan and fit on the project map

Zoom is anchored to the cursor rather than the centre. Required by the
operator and confirmed necessary by the spike: at fit zoom the map is a
shape, and only at 2-3x are the labels and sizes readable, so a map
without zoom would have been judged unreadable and dropped."
```

---

### Task 5: The channels — colour, size, labels

**Files:**
- Modify: `examples/memory-viewer/memviewer.py` (the `page()` markup and the `JS` constant)

**Interfaces:**
- Consumes: `mapState`, `renderMap()`.
- Produces: `mapColor(nd, i)`, `mapRadius(nd)`, `mapLabel(name)`; `mapState.colorBy` ∈ `area|type|age|deg`, `mapState.sizeBy` ∈ `in|sz|flat`, `mapState.labels` boolean.

- [ ] **Step 1: Add the controls**

In `memviewer.py`, in `page()`, replace the `mapbar` block with:

```html
<div id="mapbar" class="mapbar" hidden>
  <label>色 <select id="mapcolor">
    <option value="area">area</option><option value="type">type</option>
    <option value="age">古さ</option><option value="deg">被リンク数</option>
  </select></label>
  <label>大きさ <select id="mapsize">
    <option value="in">被リンク数</option><option value="sz">サイズ</option><option value="flat">一定</option>
  </select></label>
  <label><input type="checkbox" id="maplabels"> ラベル</label>
  <button class="chip" id="mapfit">全体に合わせる</button>
  <span class="meta" id="maplegend"></span>
</div>
```

The label checkbox is **unchecked** by default: measured, labels at fit zoom are
noise and the overview only reads as a shape without them.

- [ ] **Step 2: Write the channel functions**

In `memviewer.py`, insert into the `JS` constant immediately before `function renderMap()`:

```js
const MAP_AREA_COLORS = ["#6bb3f7","#5cc98a","#f0a060","#c080f0","#e06c75",
                         "#56b6c2","#d19a66","#98c379","#c678dd","#abb2bf"];
const MAP_TYPE_COLORS = {feedback:"#f0a060", project:"#5cc98a",
                         reference:"#5aabf7", user:"#c080f0", "?":"#888"};

function mapColor(nd){
  if (mapState.colorBy === "type") return MAP_TYPE_COLORS[nd.type] || "#888";
  if (mapState.colorBy === "deg") {
    const t = Math.min(1, nd.inbound.length / 12);
    return `hsl(${210 - t*200},70%,${45 + t*15}%)`;
  }
  if (mapState.colorBy === "age") {
    const t = Math.min(1, (Date.now()/1000 - nd.mtime) / 86400 / 180);
    return `hsl(${200 - t*200},55%,${60 - t*15}%)`;
  }
  const areas = mapAreas();
  return MAP_AREA_COLORS[areas.indexOf(nd.area || "")%MAP_AREA_COLORS.length];
}
function mapAreas(){ return [...new Set(P.memories.map(m => m.area || ""))].sort(); }

function mapRadius(nd){
  if (mapState.sizeBy === "flat") return 4;
  if (mapState.sizeBy === "sz") return 3 + Math.sqrt(nd.size / 700);
  return 3 + Math.sqrt(nd.inbound.length) * 1.6;
}

// Same trimming the ego graph uses, and for the same reason: nearly every name
// starts with its type, so without this the visible characters are the ones
// every node shares.
function mapLabel(name){
  const t = name.replace(/^(feedback|project|reference|user)[_-]/, "");
  return t.length > 20 ? t.slice(0, 19) + "\\u2026" : t;
}

```

- [ ] **Step 3: Use them in `renderMap`**

In `memviewer.py`, replace the body of the `names.forEach(...)` loop inside
`renderMap()` (added in Task 3) with:

```js
  names.forEach((name, i) => {
    const nd = P.memories[i], r = mapRadius(nd);
    h += `<g data-mapnode="${esc(name)}"><title>${esc(name)}${nd.area ? "  ["+esc(nd.area)+"/]" : ""}  \\u2190${nd.inbound.length}</title>`
       + `<circle cx="${xy(X[i])}" cy="${xy(Y[i])}" r="${r.toFixed(1)}" fill="${mapColor(nd)}"/>`
       + (mapState.labels
            ? `<text x="${xy(X[i]+r+3)}" y="${xy(Y[i]+3)}">${esc(mapLabel(name))}</text>` : "")
       + `</g>`;
  });
  $("maplegend").innerHTML = mapState.colorBy === "area"
    ? mapAreas().map((a,i) => `<span style="color:${MAP_AREA_COLORS[i%MAP_AREA_COLORS.length]}">●</span>${esc(a||"(top)")} `).join("")
    : (mapState.colorBy === "type"
        ? Object.entries(MAP_TYPE_COLORS).map(([k,v]) => `<span style="color:${v}">●</span>${k} `).join("") : "");
```

`P.memories[i]` is correct because `projectGraph` builds `names` by mapping
`memories` in order, so index `i` is the same record in both.

- [ ] **Step 4: Wire the controls**

In `memviewer.py`, insert into the `JS` constant immediately before `function render() {`:

```js
$("mapcolor").addEventListener("change", (e) => { mapState.colorBy = e.target.value; renderMap(); });
$("mapsize").addEventListener("change", (e) => { mapState.sizeBy = e.target.value; renderMap(); });
$("maplabels").addEventListener("change", (e) => { mapState.labels = e.target.checked; renderMap(); });

```

- [ ] **Step 5: Restart and check in the browser**

Run:
```bash
pkill -f 'memviewer.py --serve --port 8797'
cd examples/memory-viewer && python3 memviewer.py --serve --port 8797 &
```
At 1400×950 with the map open on remote-agent-harness:
- each of the four colour options repaints and the legend appears for `area` and `type` only,
- `大きさ` = 被リンク数 makes `feedback_verify_llm_framing` (19 inbound) visibly the largest node,
- ラベル off at fit zoom shows a shape; ラベル on at ~2.5× is readable.

- [ ] **Step 6: Commit**

```bash
git add examples/memory-viewer/memviewer.py
git commit -m "feat(memory-viewer): colour, size and label channels on the map

The exploratory motive is served by varying what the marks MEAN rather
than where they sit — one layout to get right instead of three.

Labels default off. Measured in the spike: at fit zoom they are noise and
the overview only reads as a shape without them, so the checkbox is the
line between the two modes rather than a convenience."
```

---

### Task 6: Mutual edges, and direction on hover

**Files:**
- Modify: `examples/memory-viewer/memviewer.py` (`CSS` and the `JS` constant)

**Interfaces:**
- Consumes: `mapState.g.mutual` from Task 2, `renderMap()` from Task 3.
- Produces: `mapHover(name)` — restyles edges for one node; `null` clears.

- [ ] **Step 1: Add the CSS**

In `memviewer.py`, insert into the `CSS` constant immediately after the line
`#mapsvg line{stroke:#3a3a3a;stroke-width:.8}`:

```css
#mapsvg line.mut{stroke:#6a5630;stroke-width:1.4}
#mapsvg line.hout{stroke:#46603f;stroke-width:1.8}
#mapsvg line.hin{stroke:#3c5068;stroke-width:1.8}
#mapsvg g.dim circle{opacity:.25}
```

- [ ] **Step 2: Mark the mutual edges when rendering**

In `renderMap()`, replace the edge loop with:

```js
  for (const [a, b] of edges) {
    const mut = mapState.g.mutual.has(a + ":" + b) ? " mut" : "";
    h += `<line class="e${mut}" data-a="${a}" data-b="${b}" x1="${xy(X[a])}" y1="${xy(Y[a])}" x2="${xy(X[b])}" y2="${xy(Y[b])}"/>`;
  }
```

- [ ] **Step 3: Write the hover handler**

In `memviewer.py`, insert into the `JS` constant immediately before `function render() {`:

```js
// Direction, scoped to one node. Globally it says nothing — every edge is
// somebody's outbound and somebody else's inbound, so a global direction
// filter selects the whole set. Against ONE node it is the real question:
// what does this reach, and what reaches it.
function mapHover(name){
  const lines = $("mapcam").querySelectorAll("line");
  if (!name) { lines.forEach(l => l.classList.remove("hout","hin")); return; }
  const i = mapState.g.index.get(name);
  const out = new Set(), inn = new Set();
  const mem = P.memories[i];
  const g = mapState.g;
  for (const raw of mem.links) {
    const k = raw.replace(/\.md$/, "");
    const j = g.index.has(k) ? g.index.get(k) : g.names.findIndex((n, x) => P.memories[x].file.replace(/\.md$/,"") === k);
    if (j >= 0 && j !== i) out.add(j);
  }
  for (const n of mem.inbound) if (g.index.has(n)) inn.add(g.index.get(n));
  lines.forEach((l) => {
    const a = +l.dataset.a, b = +l.dataset.b;
    const other = a === i ? b : (b === i ? a : -1);
    l.classList.remove("hout","hin");
    if (other < 0) return;
    if (out.has(other)) l.classList.add("hout");
    else if (inn.has(other)) l.classList.add("hin");
  });
}

$("mapsvg").addEventListener("pointerover", (e) => {
  if (!mapState.open) return;
  const g = e.target.closest("[data-mapnode]");
  mapHover(g ? g.dataset.mapnode : null);
});

```

- [ ] **Step 4: Restart and check in the browser**

Run:
```bash
pkill -f 'memviewer.py --serve --port 8797'
cd examples/memory-viewer && python3 memviewer.py --serve --port 8797 &
```
At 1400×950, map open on remote-agent-harness, zoomed to ~2×:
- count the mutual edges with
  `document.querySelectorAll('#mapcam line.mut').length` — expected **19**, the
  figure `python3 graphstats.py` prints for this project,
- hover a node: its own edges split into two colours and nothing else changes,
- move off: the colours clear.

- [ ] **Step 5: Commit**

```bash
git add examples/memory-viewer/memviewer.py
git commit -m "feat(memory-viewer): mark mutual edges, and show direction on hover

354 of 373 edges here are one-way, so arrowheads on all of them would put
354 marks in the core to restate what is true of nearly all of them. The
19 reciprocal pairs get a stroke of their own instead.

Direction only means something against a node — globally every edge is
somebody's outbound and somebody else's inbound — so it is a hover,
bounded by that node's degree."
```

---

### Task 7: Area filter, and the neighbours toggle

**Files:**
- Modify: `examples/memory-viewer/memviewer.py` (the `page()` markup and the `JS` constant)

**Interfaces:**
- Consumes: `mapState`, `renderMap()`, `mapAreas()`.
- Produces: `mapVisible()` returning `{show: Set<number>, dim: Set<number>}`.

- [ ] **Step 1: Add the controls**

In `memviewer.py`, in `page()`, insert into the `mapbar` immediately before
`<button class="chip" id="mapfit">`:

```html
  <label>area <select id="maparea"></select></label>
  <label><input type="checkbox" id="mapneigh"> 隣接も含める</label>
```

- [ ] **Step 2: Write the visibility rule**

In `memviewer.py`, insert into the `JS` constant immediately before `function renderMap()`:

```js
// Which nodes the map draws. Narrowing WHICH nodes appear is not the same as
// forcing WHERE they sit: the layout stays innocent of areas, because the links
// do not group by area and a layout that pretended otherwise would be asserting
// a structure the data does not have.
//
// The neighbours toggle exists because a subsystem on its own is mostly dots —
// measured: user has 5 memories and 0 internal edges, history 9 and 1. It is
// off by default because on (top) the neighbours are everything else.
function mapVisible(){
  const g = mapState.g;
  const all = new Set(g.names.map((_, i) => i));
  if (!mapState.area) return {show: all, dim: new Set()};
  const show = new Set();
  g.names.forEach((_, i) => {
    if ((P.memories[i].area || "") === mapState.area) show.add(i);
  });
  const dim = new Set();
  if (mapState.neighbours) {
    for (const [a, b] of g.edges) {
      if (show.has(a) && !show.has(b)) dim.add(b);
      if (show.has(b) && !show.has(a)) dim.add(a);
    }
  }
  return {show, dim};
}
```

- [ ] **Step 3: Apply it in `renderMap`**

At the top of `renderMap()`, immediately after
`const {names, edges} = mapState.g, {X, Y} = mapState.pos;`, add:

```js
  const vis = mapVisible();
  const drawn = (i) => vis.show.has(i) || vis.dim.has(i);
```

Change the edge loop's first line to skip hidden edges:

```js
  for (const [a, b] of edges) {
    if (!drawn(a) || !drawn(b)) continue;
```

and the node loop's first line to skip hidden nodes and dim the outsiders:

```js
  names.forEach((name, i) => {
    if (!drawn(i)) return;
    const nd = P.memories[i], r = mapRadius(nd);
    const cls = vis.dim.has(i) ? " dim" : "";
    h += `<g class="${cls.trim()}" data-mapnode="${esc(name)}">`
```

(the rest of the node loop is unchanged).

- [ ] **Step 4: Populate and wire the select**

In `openMap()`, immediately before `renderMap();`:

```js
  $("maparea").innerHTML = `<option value="">全部</option>`
    + mapAreas().filter(Boolean).map(a => `<option value="${esc(a)}">${esc(a)}/</option>`).join("");
  $("maparea").value = mapState.area;
```

and insert into the `JS` constant immediately before `function render() {`:

```js
$("maparea").addEventListener("change", (e) => { mapState.area = e.target.value; renderMap(); });
$("mapneigh").addEventListener("change", (e) => { mapState.neighbours = e.target.checked; renderMap(); });

```

- [ ] **Step 5: Restart and check in the browser**

Run:
```bash
pkill -f 'memviewer.py --serve --port 8797'
cd examples/memory-viewer && python3 memviewer.py --serve --port 8797 &
```
At 1400×950, map open on remote-agent-harness. Counting with
`document.querySelectorAll('#mapcam [data-mapnode]').length` and
`document.querySelectorAll('#mapcam line').length`:

| selection | nodes | edges |
|---|---:|---:|
| 全部 | 168 | 373 |
| `trsf/` | 10 | 7 |
| `user/` | 5 | 0 |
| `trsf/` + 隣接も含める | more than 10; the added ones are dimmed | more than 7 |

The first three come from `python3 graphstats.py --areas`. `user/` showing five
unconnected dots is the corpus, not a bug — the areas are filed by when a memory
fires, not by what it links to.

- [ ] **Step 6: Commit**

```bash
git add examples/memory-viewer/memviewer.py
git commit -m "feat(memory-viewer): filter the map by area, optionally with neighbours

The area select narrows which nodes are drawn; the layout stays innocent
of areas, because the links do not group by area and pinning them apart
would assert a structure the data does not have.

Measured: a subsystem on its own is mostly dots — user is 5 memories with
0 internal edges, history 9 with 1 — so 隣接も含める brings in what they
actually link to, dimmed. Off by default, because on (top) the neighbours
are everything else."
```

---

### Task 8: Node click hands back to the detail pane; the cross-project entry has no map

**Files:**
- Modify: `examples/memory-viewer/memviewer.py` (the `JS` constant)

**Interfaces:**
- Consumes: `openMap()`, `closeMap()`, `goto(name)` (existing).
- Produces: no new names.

- [ ] **Step 1: Make a node click select and close**

In the `document.addEventListener("click", …)` handler, immediately after the
`mapfit` line added in Task 4:

```js
  const mn = e.target.closest("[data-mapnode]");
  if (mn) {
    // Closing is not incidental: the map occupies both panes, so a click that
    // only selected would look like nothing happened. mapState.cam is kept, so
    // re-opening returns to the same view — walking out to a memory and back is
    // the expected loop.
    if (mapDrag && mapDrag.moved) return;   // a pan that ended on a node is not a click
    closeMap();
    goto(mn.dataset.mapnode);
    return;
  }
```

- [ ] **Step 2: Hide the button in the cross-project view**

In `memviewer.py`, in `render()`, immediately after the line `areaChips();`:

```js
  // Links never cross projects, so a union map is fifteen disconnected islands
  // whose largest is the one already reachable by picking that project.
  $("mapbtn").hidden = !!P.cross;
  if (P.cross && mapState.open) closeMap();
```

- [ ] **Step 3: Restart and check in the browser**

Run:
```bash
pkill -f 'memviewer.py --serve --port 8797'
cd examples/memory-viewer && python3 memviewer.py --serve --port 8797 &
```
At 1400×950:
- open the map, pan and zoom somewhere, click a node: the panes come back with
  that memory selected and its ego graph drawn,
- click 🗺 地図 again: the map returns at the same zoom and position,
- select 全プロジェクト in the project dropdown: the 🗺 地図 button is gone,
- select a real project again: the button is back, and the camera has been reset
  (it fits on open rather than restoring the previous project's view).

- [ ] **Step 4: Check 390px**

Resize to 390×844 and with the map open, run in the console:

```js
({clientWidth: document.documentElement.clientWidth,
  scrollWidth: document.documentElement.scrollWidth})
```

Expected: the two are equal. The map zooms and pans inside its own box; the page
must not scroll sideways.

- [ ] **Step 5: Run the whole check once more**

Run:
```bash
cd examples/memory-viewer && node --test maplayout_test.mjs && python3 memviewer.py --check > /dev/null; echo "check exit $?"
```
Expected: 10/10 passing, and `check exit 1` (findings exist, its normal result).

- [ ] **Step 6: Commit**

```bash
git add examples/memory-viewer/memviewer.py
git commit -m "feat(memory-viewer): a map node hands back to the detail pane

The map occupies both panes, so a click that only selected would look
like nothing happened; it closes and selects. The camera is kept, because
walking out to a memory and coming back is the expected loop.

No map on the cross-project entry: links never cross projects, so a union
map is fifteen disconnected islands whose largest is the one already
reachable by picking that project."
```

---

## Self-Review

**Spec coverage.** Decision 1 (single force layout) → Task 1. 2 (deterministic)
→ Task 1 Step 1, asserted. 3 (computed once, not animated) → Task 3 `openMap`.
4 (channels) → Task 5. 5 (labels are a mode, default off) → Task 5 Step 1.
6 and 7 (no community detection, no pinned layout) → nothing implements them,
which is correct for a decision not to build something; Task 7's comment records
why the filter is not a pinned layout. 8 (zoom/pan) → Task 4. 9 (click closes
and selects, camera kept) → Task 8. 10 (full width, project from the header) →
Task 3. 11 (absent in the cross view) → Task 8 Step 2. 12 (undirected, mutual
styled) → Task 2 and Task 6. 13 (direction on hover) → Task 6. 14 (area chips
filter, neighbours toggle) → Task 7 — **with one deviation: the spec says the
existing area chips filter the map, and this plan uses a `<select>` inside the
map bar instead.** The chip strip lives above `main`, which the map hides, so
reusing it would mean keeping a control visible whose other meaning (filtering
the list) has no target while the map is open. The select carries the same
choice with the same values. Flagging rather than silently diverging.

**Placeholder scan.** No TBD/TODO; every code step carries the code. The browser
steps name the exact figures to expect (168/373, 19 mutual, 10/7, 5/0) and where
they come from (`graphstats.py`), rather than saying "verify it looks right".

**Type consistency.** `projectGraph` returns `{names, index, edges, mutual}` in
Task 2 and every later use reads those four. `mapLayout(n, edges, seed)` returns
`{X, Y}`, used as such in Task 3 and Task 7. `mapState` gains no field after
Task 3 that Task 3 did not declare (`colorBy`, `sizeBy`, `labels`, `area`,
`neighbours` are all in its initialiser). `mapHover` and `mapVisible` are each
defined once and called once.

**One risk worth naming for the executor.** Task 6's `mapHover` resolves a raw
`[[target]]` with a `findIndex` fallback for the filename-stem case, which is
O(n) per link. At 24 links on the worst node that is 24 × 168 comparisons on a
pointer move — fine, but if it ever shows up as lag, the fix is to build the
stem map once in `projectGraph` and return it, not to memoise at the call site.
