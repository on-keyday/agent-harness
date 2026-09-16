// The preview's pure decisions, driven under `node --test`. Same vm realm as
// cmd_test.mjs: only what lives OUTSIDE the page's IIFE is reachable here, so
// the logic these cover was hoisted out on purpose.
//
// What is covered is the part that decides, not the part that draws: which
// bytes count as a page, what a typed path means, and which realm the shim
// tells the page it is in. The drawing is checked in the browser.
import { test } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { loadPage } from "./harness_env.mjs";

const dir = path.dirname(fileURLToPath(import.meta.url));
const page = await loadPage(dir);

// normalizePagePath turns what an operator types into a request target.
// A bare "api/payload" is the likely typo, and rejecting it would surface as a
// request the far end 404s rather than as an input error.
test("normalizePagePath defaults to / and forces a leading slash", () => {
  assert.equal(page.normalizePagePath(""), "/");
  assert.equal(page.normalizePagePath("   "), "/");
  assert.equal(page.normalizePagePath(null), "/");
  assert.equal(page.normalizePagePath("/"), "/");
  assert.equal(page.normalizePagePath("api/payload"), "/api/payload");
  assert.equal(page.normalizePagePath("  /out.html  "), "/out.html");
  assert.equal(page.normalizePagePath("/x?y=1&z=2"), "/x?y=1&z=2");
});

// contentTypeOf reads the header off the [[name, value], ...] shape httpFetch
// resolves with. Header names arrive in whatever case the far end wrote them,
// so a case-sensitive lookup would miss python's "Content-Type" on one server
// and go's "content-type" on the next.
test("contentTypeOf finds the header whatever its case", () => {
  assert.equal(page.contentTypeOf([["Content-Type", "text/html; charset=utf-8"]]),
    "text/html; charset=utf-8");
  assert.equal(page.contentTypeOf([["content-type", "application/json"]]),
    "application/json");
  assert.equal(page.contentTypeOf([["Server", "x"], ["CONTENT-TYPE", "text/plain"]]),
    "text/plain");
  assert.equal(page.contentTypeOf([]), "");
  assert.equal(page.contentTypeOf(null), "");
});

// isHtmlContentType gates the iframe. Anything it says no to is shown as text,
// so a JSON endpoint opened by mistake reads as JSON instead of as a blank
// frame.
test("isHtmlContentType accepts html with parameters, rejects the rest", () => {
  assert.equal(page.isHtmlContentType("text/html"), true);
  assert.equal(page.isHtmlContentType("text/html; charset=utf-8"), true);
  assert.equal(page.isHtmlContentType("TEXT/HTML"), true);
  assert.equal(page.isHtmlContentType("  text/html  "), true);
  assert.equal(page.isHtmlContentType("application/xhtml+xml"), true);
  assert.equal(page.isHtmlContentType("application/json"), false);
  assert.equal(page.isHtmlContentType("text/plain"), false);
  assert.equal(page.isHtmlContentType(""), false);
  assert.equal(page.isHtmlContentType(null), false);
});

// previewHttpLabel is what the modal title shows AND what the shim reports as
// the page's own identity. One function so the two cannot disagree.
test("previewHttpLabel is the absolute URL that was fetched", () => {
  const pin = page.parsePinnedTarget("127.0.0.1:8791");
  assert.equal(page.previewHttpLabel(pin, "/"), "http://127.0.0.1:8791/");
  assert.equal(page.previewHttpLabel(pin, "/api/payload"), "http://127.0.0.1:8791/api/payload");
});

// The marker tells a page which of the two previews it is in. A page served
// over the forward can reach its own relative paths; a previewed FILE cannot
// unless the operator pinned something, so the two are not interchangeable and
// the marker must not claim otherwise.
test("previewShimSource reports the realm it was built for", () => {
  const pin = page.parsePinnedTarget("127.0.0.1:8791");
  const file = page.previewShimSource(pin, "artifacts/rpg.html");
  assert.match(file, /preview: "html-file"/);
  assert.match(file, /artifacts\\?\/rpg\.html/);

  const http = page.previewShimSource(pin, "http://127.0.0.1:8791/", "http");
  assert.match(http, /preview: "http"/);
  assert.ok(!/preview: "html-file"/.test(http), "http shim must not claim to be a file preview");
});

// The config is spliced into a <script> tag, so a path that closes the tag
// would end the shim early and hand the rest to the page as markup. Escaping
// is checked here because the injected path is no longer only a filename: it
// now comes from a URL the operator typed.
test("previewShimSource escapes < in the config it splices", () => {
  const pin = page.parsePinnedTarget("8791");
  const src = page.previewShimSource(pin, "/x</script><script>alert(1)</script>", "http");
  assert.ok(!src.includes("</script><script>alert(1)"),
    "a literal </script> in the path must not survive into the shim");
  assert.match(src, /\\u003c/);
});
