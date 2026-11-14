// Agent transcripts on the WebUI (pillar B), under `node --test` with the
// real wasm bridge (harness_env.mjs). `make js-test` runs this.
import { test } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { loadPage } from "./harness_env.mjs";

const dir = path.dirname(fileURLToPath(import.meta.url));
const page = await loadPage(dir);
const ID = "0123456789abcdef".repeat(2);

test("the bridge exports every transcript call", () => {
  for (const fn of ["transcriptSessions", "transcriptFiles", "transcriptGet"]) {
    assert.equal(typeof page.harness[fn], "function", fn);
  }
});

test("a transcript download is named for its task, agent and session", () => {
  const sess = "0f3c2a1e-5b6d-4e7f-8a9b-0c1d2e3f4a5b";
  assert.equal(page.transcriptDownloadName(ID, "claude", sess, ""), "01234567-claude-0f3c2a1e.tar");
  assert.equal(page.transcriptDownloadName(ID, "claude", sess, `${sess}.jsonl`), `01234567-claude-0f3c2a1e-${sess}.jsonl`);
  // A companion file's directories do not become directories of the download.
  assert.equal(page.transcriptDownloadName(ID, "claude", sess, `${sess}/subagents/agent-a1.jsonl`), "01234567-claude-0f3c2a1e-agent-a1.jsonl");
});

test("the sheet lists each session, its note, and an opened session's files", () => {
  const list = {
    sessions: [
      { agent: "claude", id: "s-1", row: "claude s-1  created=…  size=3.0MiB" },
      { agent: "opencode", id: "ses_2", row: "opencode ses_2  created=…  size=unknown" },
    ],
    note: "unavailable: agy: not built",
  };
  const files = new Map([["s-1", [{ name: "s-1.jsonl", row: "s-1.jsonl  size=3.0MiB" }]]]);
  const m = page.transcriptSheetModel(list, files);
  assert.equal(m.note, "unavailable: agy: not built");
  assert.deepEqual([...m.rows.map((r) => r.text)], [list.sessions[0].row, list.sessions[1].row]); // copied out of the vm realm
  assert.deepEqual([...m.rows[0].actions], ["files", "tar"]);
  assert.deepEqual(m.rows[0].files.map((f) => [f.name, f.text]), [["s-1.jsonl", "s-1.jsonl  size=3.0MiB"]]);
  assert.equal(m.rows[1].files, null, "a session not opened lists no files");
  const empty = page.transcriptSheetModel({ sessions: [], note: "" }, new Map());
  assert.equal(empty.rows.length, 0);
});

test("the newest session comes first", () => {
  const list = {
    sessions: [
      { agent: "claude", id: "old", row: "old", updatedMs: 1000 },
      { agent: "codex", id: "new", row: "new", updatedMs: 3000 },
      { agent: "pi", id: "mid", row: "mid", updatedMs: 2000 },
    ],
    note: "",
  };
  assert.deepEqual([...page.transcriptSheetModel(list, new Map()).rows.map((r) => r.id)], ["new", "mid", "old"]);
});
