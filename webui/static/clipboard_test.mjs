// The WebUI is commonly served over plain http, where navigator.clipboard does
// not exist (it needs a secure context). writeClipboardText (main.js) is the
// one copy path that falls back to execCommand("copy"); a direct
// navigator.clipboard.writeText anywhere else is a button that can only fail
// there. This scans the page's scripts so a new call site cannot bypass it.
import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const dir = path.dirname(fileURLToPath(import.meta.url));

// bodyOf returns the text of `async function <name>(...) { ... }` in src.
function bodyOf(src, name) {
  const start = src.indexOf(`async function ${name}(`);
  if (start < 0) return "";
  let depth = 0;
  for (let i = src.indexOf("{", start); i < src.length; i++) {
    if (src[i] === "{") depth++;
    else if (src[i] === "}" && --depth === 0) return src.slice(start, i + 1);
  }
  return "";
}

test("navigator.clipboard.writeText is called only inside writeClipboardText", () => {
  const offenders = [];
  for (const f of fs.readdirSync(dir).filter((n) => n.endsWith(".js") && !n.endsWith(".min.js"))) {
    let src = fs.readFileSync(path.join(dir, f), "utf8");
    if (f === "main.js") {
      const helper = bodyOf(src, "writeClipboardText");
      assert.ok(helper.includes("navigator.clipboard.writeText"), "writeClipboardText not found in main.js");
      src = src.replace(helper, helper.replace(/[^\n]/g, "")); // keep line numbers
    }
    src.split("\n").forEach((line, i) => {
      if (line.includes("navigator.clipboard.writeText")) offenders.push(`${f}:${i + 1}: ${line.trim()}`);
    });
  }
  assert.deepEqual(offenders, [], "copy text through writeClipboardText (main.js), which also works over plain http:\n" + offenders.join("\n"));
});
