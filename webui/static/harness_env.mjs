// A Node home for main.js's top-level half, with the REAL wasm bridge.
//
// The page is one `(async () => { … })()` over a live DOM, so nothing inside
// it runs outside a browser. What was hoisted out of it -- runVerbCommand,
// runFileAction, runGitAction, tokenize and ~50 helpers -- is plain JavaScript,
// and this loads that half into a vm context whose `window.harness` is
// cmd/harness-webui-wasm compiled to wasm. So a test drives the surface's
// dispatch against the same declaration table the CLI and the TUI parse from,
// not a mock of it.
//
// Everything the page owns and a test does not -- dropdowns, snapshot cache,
// panels -- arrives as the ctx object runVerbCommand takes, so a test records
// calls instead of mimicking widgets.
import fs from "node:fs";
import path from "node:path";
import vm from "node:vm";

// loadPage returns the vm context with main.js's top-level declarations and a
// live `harness`. The page's IIFE throws on the first DOM call it cannot fake;
// that is expected and ignored, because function declarations are hoisted
// before it runs.
export async function loadPage(dir) {
  // One element stub, permissive in both directions: any method is a no-op,
  // any unknown property is another stub so `el.dataset.fpid = 1` and
  // `el.style.width = "2px"` work, and the handful the page reads as text
  // answer with a string.
  const text = new Set(["value", "textContent", "innerHTML", "innerText", "className", "id", "type", "src"]);
  const mkEl = () => new Proxy(Object.create(null), {
    get(t, k) {
      if (k === Symbol.toPrimitive || k === "toString") return () => "";
      if (typeof k !== "string") return undefined;
      if (k in t) return t[k];
      if (text.has(k)) return "";
      if (k === "classList") return { add() {}, remove() {}, toggle() {}, contains: () => false };
      if (k === "children" || k === "childNodes") return [];
      if (["addEventListener", "removeEventListener", "appendChild", "removeChild",
           "remove", "setAttribute", "getAttribute", "focus", "blur", "click",
           "scrollIntoView", "insertBefore", "replaceChildren"].includes(k)) return () => {};
      if (k === "querySelectorAll") return () => [];
      if (k === "querySelector" || k === "closest") return () => mkEl();
      return (t[k] = mkEl());
    },
    set(t, k, v) { t[k] = v; return true; },
    has: () => true,
  });
  const el = mkEl();
  const ctx = {
    console, process, TextEncoder, TextDecoder, performance, crypto, fs, Buffer,
    setTimeout, clearTimeout, setInterval: () => 0, clearInterval: () => {},
    Uint8Array, Error, URL: { createObjectURL: () => "blob:", revokeObjectURL: () => {} },
    Blob: class {},
    location: { protocol: "http:", host: "127.0.0.1:0", hash: "", href: "http://127.0.0.1:0/" },
    localStorage: { getItem: () => null, setItem: () => {}, removeItem: () => {} },
    document: {
      getElementById: () => el, addEventListener: () => {},
      querySelectorAll: () => [], querySelector: () => el,
      createElement: () => el, body: el, documentElement: el,
    },
    WebSocket: class { constructor() { this.readyState = 0; } send() {} close() {} },
    // The page body is `(async () => { … })()` with no catch, so anything it
    // reaches for and does not find becomes an unhandled rejection -- and one
    // that surfaces long after load, because the chain awaits timers, which
    // node --test reports as a failure of the whole file. Its first such reach
    // is `WebAssembly.instantiateStreaming(fetch("/static/main.wasm"))`, and
    // the bridge is already loaded by then, so this parks it there instead:
    // a promise that never settles holds no handle and rejects nothing.
    fetch: () => new Promise(() => {}),
  };
  ctx.window = ctx;
  ctx.globalThis = ctx;
  vm.createContext(ctx);

  // wasm_exec.js checks `instanceof WebAssembly.Instance`, which fails across
  // realms -- so the instance has to be built with the CONTEXT's WebAssembly,
  // not this module's.
  vm.runInContext("globalThis.__wa = WebAssembly", ctx);
  vm.runInContext(fs.readFileSync(path.join(dir, "wasm_exec.js"), "utf8"), ctx, { filename: "wasm_exec.js" });
  ctx.__bytes = fs.readFileSync(path.join(dir, "main.wasm"));
  await vm.runInContext(
    `(async () => { const go = new Go();
       const m = await __wa.instantiate(__bytes, go.importObject);
       go.run(m.instance); })()`, ctx);
  for (let i = 0; i < 50 && typeof ctx.harness !== "object"; i++) {
    await new Promise((r) => setTimeout(r, 20));
  }
  if (typeof ctx.harness !== "object") throw new Error("wasm bridge did not register window.harness");

  try {
    vm.runInContext(fs.readFileSync(path.join(dir, "main.js"), "utf8"), ctx, { filename: "main.js" });
  } catch (e) {
    // Same thing, thrown synchronously. The hoisted declarations are already
    // in the context by then.
    if (typeof ctx.runVerbCommand !== "function") throw e;
  }
  // A top-level `const` in a vm script is a LEXICAL binding: reachable from a
  // later runInContext in the same context, but never a property of the
  // sandbox object -- unlike a `function` declaration, which is why
  // runVerbCommand appears on ctx and RUNCMD_DISPATCH does not. Lifted here so
  // a test can read it.
  // The pristine bridge, kept before any recorder wraps the page global. A
  // test that asks "does harness.X exist" must ask THIS: the recorder answers
  // for every name, so checking the wrapped one made the question meaningless
  // -- a dispatch entry naming `streamFinsh` passed.
  ctx.bridge = ctx.harness;
  for (const name of ["RUNCMD_DISPATCH"]) {
    ctx[name] = vm.runInContext(`typeof ${name} !== "undefined" ? ${name} : undefined`, ctx);
  }
  return ctx;
}

// recordingCtx is the ctx runVerbCommand takes, with every page operation
// replaced by a recorder. `calls` ends up holding one entry per bridge call
// AND per page operation, in order, with the arguments.
export function recordingCtx(page, overrides = {}) {
  const calls = [];
  const record = (name) => (...a) => { calls.push([name, ...a]); return undefined; };
  // The bridge is the REAL one for parseCommand / parseGit / pathsForSurface
  // (pure, no client) and a recorder for everything that would need a server.
  // capsCatalog and parseAuthority are pure too: one renders a compiled-in
  // table, the other parses a string. Recording them instead would test that
  // the page CALLS the grammar rather than that the grammar answers, and the
  // whole point of routing `caps set-defaults` through the bridge is that the
  // page does not own a second copy of it.
  const pure = new Set(["parseCommand", "parseGit", "pathsForSurface",
    "capsCatalog", "parseAuthority"]);
  const harness = new Proxy({}, {
    get: (_, name) => {
      if (pure.has(name)) return page.bridge[name];
      return async (...a) => {
        calls.push([name, ...a]);
        return overrides[name] !== undefined ? overrides[name] : "";
      };
    },
    has: (_, name) => name in page.bridge,
  });
  // runFileAction and its helpers reach `window.harness` directly rather than
  // through ctx, so the page global is pointed at the recorder too. Always
  // built from the PRISTINE bridge, never from whatever the last call left
  // there, so the wrappers do not stack.
  page.harness = harness;
  const ctx = {
    calls,
    harness,
    echo: record("echo"),
    refreshSnapshot: async () => { calls.push(["refreshSnapshot"]); },
    composeRepo: () => overrides.repo ?? "/dropdown/repo",
    composeHost: () => overrides.host ?? "",
    composeAgent: () => overrides.agent ?? "",
    resumeTaskID: () => overrides.resumeTaskID ?? "",
    claudeArgs: () => overrides.claudeArgs ?? [],
    sessionReq: (o) => { calls.push(["sessionReq", o]); return o; },
    setParentMessage: (id, swap, r) => { calls.push(["setParentMessage", id, swap, r]); return "reparented"; },
    filteredTaskRows: () => { calls.push(["filteredTaskRows"]); return "(rows)"; },
    openSessionPreview: record("openSessionPreview"),
    openGridSet: async (o) => { calls.push(["openGridSet", o]); return "grid"; },
    execRunToOutput: async (...a) => { calls.push(["execRunToOutput", ...a]); return "ran"; },
    forwards: () => overrides.forwards ?? [],
    findForwardEntry: (id) => { calls.push(["findForwardEntry", id]); return { wrap: {}, button: {} }; },
    toggleForwardTap: (f, wrap, btn, opts) => { calls.push(["toggleForwardTap", f.forward_id, opts]); },
    tapOpen: () => true,
    chatTaskID: () => overrides.chatTaskID ?? "",
    openChatFor: async (id) => { calls.push(["openChatFor", id]); },
    spawnDefaults: () => {
      calls.push(["spawnDefaults"]);
      return overrides.spawnDefaults ?? { caps: 0, capsLabel: "none", scope: "" };
    },
    setSpawnDefaults: (d) => { calls.push(["setSpawnDefaults", d]); },
  };
  return ctx;
}

// named returns the recorded calls to one name.
export const named = (calls, name) => calls.filter((c) => c[0] === name);
