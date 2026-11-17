// The overview tab's pure decisions, under `node --test`: which tab a reload
// lands on, and what the tab shows for an Overview from the bridge. The
// drawing itself is checked in the browser.
import { test } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { loadPage } from "./harness_env.mjs";

const dir = path.dirname(fileURLToPath(import.meta.url));
const page = await loadPage(dir);
const names = ["overview", "terminal", "tasks", "files", "git", "notify", "conns", "board"];

test("pickInitialTab restores a known tab and falls back to tasks", () => {
  assert.equal(page.pickInitialTab("overview", names), "overview");
  assert.equal(page.pickInitialTab("board", names), "board");
  assert.equal(page.pickInitialTab("bogus", names), "tasks");
  assert.equal(page.pickInitialTab(null, names), "tasks");
  assert.equal(page.pickInitialTab(undefined, names), "tasks");
});

const empty = {
  needs_you: [], needs_you_count: 0, live: [],
  group_counts: { ready: 0, working: 0, "no report": 0, queued: 0, finished: 0 },
  hosts: [], host_count: 0, slot_count: 0, operator_clients: 0,
  forwards: [], forward_count: 0, topics: [], topic_count: 0, msg_total: 0,
};

test("an empty overview still has five sections, each with its zero", () => {
  const s = page.overviewSectionsModel(empty);
  // join: the array is from the page's realm, so deepEqual would compare prototypes.
  assert.equal(s.map((x) => x.section).join(","), "needs-you,tasks,hosts,forwards,board");
  for (const x of s) {
    assert.match(x.header, /\b0\b/, `${x.section}: ${x.header}`);
    assert.equal(x.rows.length, 0);
    assert.equal(x.error, "");
  }
});

test("rows carry what a tap opens, and the bridge's text as given", () => {
  const o = {
    ...empty,
    needs_you: [{ id: "ab".repeat(16), repo: "web", badge: "wait:3m", now: "<b>x</b>", group: "needs you" }],
    needs_you_count: 1,
    topics: [{ name: "chat.x", msg_count: 2, last_publish_ms: 0, head: "<script>" }],
    topic_count: 1, msg_total: 2,
    board_err: "denied",
  };
  const s = page.overviewSectionsModel(o, 0);
  const task = s[0].rows[0];
  assert.equal(task.kind, "task");
  assert.equal(task.key, "ab".repeat(16));
  assert.equal(task.tier, "attention");
  assert.ok(task.text.includes("<b>x</b>"), task.text);
  const topic = s[4].rows[0];
  assert.equal(topic.kind, "topic");
  assert.equal(topic.key, "chat.x");
  assert.equal(topic.head, "<script>");
  assert.equal(s[4].error, "denied");
});
