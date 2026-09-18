# Agentboard reply-chain viewer — Implementation Plan

> **For agentic workers:** implement task-by-task, in order. Each task ends with
> its own tests green and its own commit. Do not start a task before the
> previous one is committed.

**Goal:** a second view of the agentboard that follows `in_reply_to` across
topics, on CLI, TUI and WebUI, without changing the existing topic view.

**Architecture:** one walk in Go (`cli.BuildThreads`) produces flat rows; three
surfaces draw those rows and none of them walks the links themselves. Two CLI
verbs differ only in how they collect the messages they hand to that walk, which
is what lets the agent-facing one require no capability.

**Tech Stack:** Go (stdlib + this repo's `protocol` package), the existing wasm
bridge, vanilla JS in `webui/static/main.js`.

**Spec:** `docs/superpowers/specs/2026-09-18-agentboard-thread-viewer-design.md`
— read it in full before Task 1. This plan argues from it and does not restate
its reasoning.

## Global Constraints

- Work only in this worktree. A bare `/home/kforfk/workspace/remote-agent-harness/<rel>`
  path resolves to the PARENT checkout. Confirm with
  `git rev-parse --abbrev-ref HEAD` before writing.
- Never run bare `git stash` / `git stash pop`: the stash stack is shared with
  every other worktree and other sessions are live.
- Build hygiene: compile-check with `go build ./...` (writes no binary) or
  `go vet ./cli`. Never `go build ./cmd/<x>/` — it drops an executable in the
  worktree root. The tree must be as clean after a check as before.
- Verify with make targets where one exists (`make check`), not ad-hoc
  `go build` over a hand-picked package set.
- Do not push. Do not touch `main`. Commit to this branch only.
- Board seq is `uint64` and exceeds JS safe-integer range. It is a string at the
  wasm boundary and a `uint64` everywhere in Go. Never a float.

---

### Task 1: `BuildThreads` — the one walk

**Files:**
- Create: `cli/boardthread.go`
- Test: `cli/boardthread_test.go`

**Interfaces:**
- Produces, relied on by every later task:

```go
type ThreadRow struct {
    Msg    BoardMessage // the type cli/board.go already uses for a retained message
    Topic  string
    Depth  int
    IsLast []bool
    Orphan bool
}

func BuildThreads(msgs []BoardMessage, topicOf map[uint64]string) []ThreadRow
```

`topicOf` maps seq to the topic that retained it; a seq absent from it yields
`Topic == ""`. Read `cli/board.go` first and use the message type it already
defines rather than introducing a parallel one.

- [ ] **Step 1: Read the sibling before writing anything**

Read `cli/tasktree.go` in full — `BuildTaskTree` solves the same shape (parent
links → flat rows with depth, last-child flags and an orphan rule) and this
task mirrors it deliberately. Read `cli/board.go` to find the message type.

- [ ] **Step 2: Write the failing tests**

Mirror `cli/tasktree_test.go`'s table style. These are the contract:

```go
// A chain whose parent is absent renders at root and keeps its children.
func TestBuildThreadsOrphanKeepsChildren(t *testing.T) {
    msgs := []BoardMessage{
        {Seq: 20, InReplyTo: 10}, // 10 is NOT in the input
        {Seq: 21, InReplyTo: 20},
    }
    rows := BuildThreads(msgs, map[uint64]string{20: "chat.a", 21: "chat.b"})
    if len(rows) != 2 {
        t.Fatalf("rows = %d, want 2", len(rows))
    }
    if !rows[0].Orphan || rows[0].Depth != 0 {
        t.Errorf("row 0 = {orphan:%v depth:%d}, want {true 0}", rows[0].Orphan, rows[0].Depth)
    }
    if rows[1].Depth != 1 {
        t.Errorf("row 1 depth = %d, want 1: an orphan keeps its own children", rows[1].Depth)
    }
}

// A chain spans topics; the row carries where each message landed.
func TestBuildThreadsSpansTopics(t *testing.T) {
    msgs := []BoardMessage{
        {Seq: 1},
        {Seq: 2, InReplyTo: 1},
        {Seq: 3, InReplyTo: 2},
    }
    rows := BuildThreads(msgs, map[uint64]string{1: "chat.aaa", 2: "chat.bbb", 3: "chat.aaa"})
    got := []string{rows[0].Topic, rows[1].Topic, rows[2].Topic}
    want := []string{"chat.aaa", "chat.bbb", "chat.aaa"}
    if !reflect.DeepEqual(got, want) {
        t.Errorf("topics = %v, want %v", got, want)
    }
    if rows[2].Depth != 2 {
        t.Errorf("depth = %d, want 2", rows[2].Depth)
    }
}

// Siblings order by Seq, not by arrival time, and the order is total.
func TestBuildThreadsSiblingOrderIsSeq(t *testing.T) {
    msgs := []BoardMessage{
        {Seq: 1},
        {Seq: 7, InReplyTo: 1, ReceivedAtMs: 500},
        {Seq: 3, InReplyTo: 1, ReceivedAtMs: 500}, // same ms, lower seq
    }
    rows := BuildThreads(msgs, nil)
    if rows[1].Msg.Seq != 3 || rows[2].Msg.Seq != 7 {
        t.Errorf("sibling order = %d,%d, want 3,7", rows[1].Msg.Seq, rows[2].Msg.Seq)
    }
}

// A malformed input set with a cycle must terminate and emit each message once.
func TestBuildThreadsCycleTerminates(t *testing.T) {
    msgs := []BoardMessage{
        {Seq: 1, InReplyTo: 2},
        {Seq: 2, InReplyTo: 1},
    }
    rows := BuildThreads(msgs, nil)
    if len(rows) != 2 {
        t.Fatalf("rows = %d, want 2 (each message exactly once)", len(rows))
    }
}

// Every input message comes back exactly once, roots in seq order.
func TestBuildThreadsTwoRootsInterleaved(t *testing.T) {
    msgs := []BoardMessage{
        {Seq: 10}, {Seq: 11, InReplyTo: 10}, {Seq: 5}, {Seq: 6, InReplyTo: 5},
    }
    rows := BuildThreads(msgs, nil)
    got := []uint64{rows[0].Msg.Seq, rows[1].Msg.Seq, rows[2].Msg.Seq, rows[3].Msg.Seq}
    want := []uint64{5, 6, 10, 11}
    if !reflect.DeepEqual(got, want) {
        t.Errorf("order = %v, want %v", got, want)
    }
}

func TestBuildThreadsEmpty(t *testing.T) {
    if rows := BuildThreads(nil, nil); rows != nil {
        t.Errorf("rows = %v, want nil", rows)
    }
}
```

Add one more test of your own for `IsLast`: a root with three children must
produce `IsLast` `false, false, true` on them in order.

- [ ] **Step 3: Run them and confirm they fail**

`go test ./cli -run TestBuildThreads -v` → FAIL, undefined: BuildThreads.

- [ ] **Step 4: Implement `BuildThreads`**

Match `BuildTaskTree`'s structure. The orphan rule, the visited set that makes a
cycle terminate, and the flat pre-order output are all in that function already.

- [ ] **Step 5: Green**

`go test ./cli -run TestBuildThreads -v` → PASS. Then `go vet ./cli`.

- [ ] **Step 6: Commit**

```bash
git add cli/boardthread.go cli/boardthread_test.go
git commit -m "feat(board): assemble reply chains across topics"
```

---

### Task 2: one gutter renderer, not two

**Files:**
- Modify: `cli/tasktree.go:167` (`TreePrefix`)
- Modify: `cli/list.go:326`, `tui/tasks.go:219` (the two non-test call sites)
- Test: `cli/tasktree_render_test.go` (existing — must still pass unchanged in behaviour)

**Interfaces:**
- Produces: `func TreePrefix(isLast []bool) string` — same output, different parameter.

- [ ] **Step 1: Change the signature and the call sites**

`TreePrefix` reads only `row.IsLast`. Take `[]bool` instead, so `ThreadRow` can
use it. Update both call sites to pass `row.IsLast` / `r.IsLast`.

- [ ] **Step 2: Prove the rendering did not change**

`go test ./cli -run TreePrefix -v` and `go test ./cli ./tui` → PASS, with the
existing expectations unedited. If you find yourself editing an existing
expectation, stop: the refactor changed behaviour and that is a bug.

- [ ] **Step 3: Commit**

```bash
git add cli/tasktree.go cli/list.go tui/tasks.go
git commit -m "refactor(cli): TreePrefix takes the flags it reads"
```

---

### Task 3: payload escaping, gated on the destination

This is the task the spec's "two obligations" section is about. Read that
section again before starting. The dangerous half is obligation 2: a broken
gate corrupts extracted bytes and nothing on screen says so.

**Files:**
- Create: `cli/payloadescape.go`
- Test: `cli/payloadescape_test.go`
- Modify: `cli/cmd_board.go` (the body-printing branch at :283-289, and the
  `board read` flag set)

**Interfaces:**
- Produces:

```go
// EscapeForTerminal returns s with every byte that can steer a terminal
// rendered as a visible \xNN escape: C0 except \n and \t, 0x7f, and C1
// (0x80-0x9f). Returns the input unchanged when it holds none of them.
func EscapeForTerminal(b []byte) string

// IsTTY reports whether f is a character device. Mirrors isTTY in
// cmd/harness-cli/git.go:95, which exists for the same reason.
func IsTTY(f *os.File) bool
```

- [ ] **Step 1: Write the failing tests**

```go
func TestEscapeForTerminalCoversTheByteSet(t *testing.T) {
    in := []byte("a\x1b[2Jb\rc\x07d\x7fef")
    got := EscapeForTerminal(in)
    for _, bad := range []string{"\x1b", "\r", "\x07", "\x7f", ""} {
        if strings.Contains(got, bad) {
            t.Errorf("output still holds %q raw: %q", bad, got)
        }
    }
    if !strings.Contains(got, `\x1b`) || !strings.Contains(got, `\x9b`) {
        t.Errorf("escapes not visible: %q", got)
    }
}

func TestEscapeForTerminalKeepsNewlineAndTab(t *testing.T) {
    in := []byte("keep\tthis\nand this")
    if got := EscapeForTerminal(in); got != string(in) {
        t.Errorf("got %q, want it unchanged", got)
    }
}

// Obligation 2. The regression test for the defect this spec's first draft had.
func TestBoardReadRedirectedIsByteExact(t *testing.T) {
    // Render a message body to a non-TTY writer through the same path
    // `board read` uses, and compare against the published bytes.
    body := []byte("x\x1b[2J\r\x9b\xff\xfe binary")
    var buf bytes.Buffer // not a character device
    writeBoardBody(&buf, body, false /* isTTY */, false /* raw */)
    if !bytes.Equal(bytes.TrimSuffix(buf.Bytes(), []byte("\n")), body) {
        t.Errorf("redirected output altered the bytes:\n got %q\nwant %q", buf.Bytes(), body)
    }
}

func TestBoardReadRawIsByteExactOnATerminal(t *testing.T) {
    body := []byte("x\x1b[2J\xff")
    var buf bytes.Buffer
    writeBoardBody(&buf, body, true /* isTTY */, true /* raw */)
    if !bytes.Equal(bytes.TrimSuffix(buf.Bytes(), []byte("\n")), body) {
        t.Errorf("--raw altered the bytes: %q", buf.Bytes())
    }
}

func TestBoardReadTerminalIsEscaped(t *testing.T) {
    var buf bytes.Buffer
    writeBoardBody(&buf, []byte("x\x1b[2J"), true /* isTTY */, false /* raw */)
    if bytes.Contains(buf.Bytes(), []byte{0x1b}) {
        t.Errorf("a terminal got a raw ESC: %q", buf.Bytes())
    }
}

// An indented JSON body still reaches a terminal carrying a live C1 unless
// the same gate covers it: encoding/json escapes C0 and leaves C1 raw.
func TestJSONBodyC1IsEscapedForTerminal(t *testing.T) {
    var buf bytes.Buffer
    writeBoardBody(&buf, []byte(`{"k":"ab"}`), true, false)
    if bytes.Contains(buf.Bytes(), []byte{0xc2, 0x9b}) {
        t.Errorf("C1 survived into terminal output: %q", buf.Bytes())
    }
}
```

`writeBoardBody(w io.Writer, payload []byte, isTTY, raw bool)` is the single
body-printing function both `board read` and the new verbs call. Extract it
from the existing branch at `cli/cmd_board.go:283-289`; it keeps that branch's
JSON-indent behaviour.

- [ ] **Step 2: Fail, implement, green**

`go test ./cli -run 'Escape|BoardRead|JSONBody' -v`.

- [ ] **Step 3: Route `board read` through it and add `--raw`**

`board read` decides `isTTY` once from `os.Stdout`. `--json` does not go through
`writeBoardBody` at all — it already carries exact bytes as `payload_b64`
(`cli/cmd_board.go:77`) and must stay that way.

- [ ] **Step 4: Prove it end to end, not only in the unit test**

```bash
go build -o /dev/null ./cmd/harness-cli    # writes nothing
```
Then publish a message whose body holds an ESC and read it back two ways:

```bash
harness-cli board read <topic> | cat -v    # piped: stdout is NOT a terminal
```

Expected: `^[` — the RAW byte, because the destination is a pipe and obligation
2 says a pipe gets the published bytes. If this shows `\x1b` instead, the gate
is inverted: you are escaping for a non-terminal and corrupting extraction,
which is the defect this task exists to prevent. Escaping is observable only
when stdout is an actual terminal, which a test harness cannot fake with a pipe
— so state plainly that you verified the escaped side through
`writeBoardBody(..., isTTY=true, ...)` in the unit test rather than claiming you
saw it on a real terminal.

- [ ] **Step 5: Commit**

```bash
git add cli/payloadescape.go cli/payloadescape_test.go cli/cmd_board.go
git commit -m "fix(board): escape payloads for terminals, keep redirected bytes exact"
```

---

### Task 4: `board thread` (operator face)

**Files:**
- Modify: `cli/cmd_board.go` (subcommand), `cli/verb/` declarations as that
  package requires — read `docs/superpowers/specs/2026-09-03-cli-verb-ssot-design.md`
  first; verbs in this repo are declared once and derived per surface, so adding
  one by hand in three places is wrong here.

**Interfaces:**
- Consumes: `BuildThreads`, `TreePrefix`, `writeBoardBody`.
- Produces: `harness-cli board thread [--seq N] [--task ID]... [--headers-only] [--raw] [--json]`

- [ ] **Step 1: Find how a verb is declared**

Read the verb SSOT spec and `cli/verb/table.go`. Declare `thread` the way the
existing board subcommands are declared. Do not hand-write a parallel path.

- [ ] **Step 2: Collect, assemble, render**

Collect: every topic (`BoardTopics`) then each topic's messages (`BoardRead`),
building the `topicOf` map as you go. Filter by `--seq` / `--task`. Hand the
flat list to `BuildThreads`. Render one line per row:

```
<gutter>#<seq>[ re=<n>][ reply-to=<topic>] topic=<topic> from=<task8> host=<h> agent=<a> size=<n> at=<rfc3339>[ ORPHAN][ RETRACTED ...]
```

then the body via `writeBoardBody` unless `--headers-only`.

- [ ] **Step 3: A `--seq` outside the visible set is an error, not silence**

Match `cli/cmd_board.go:290-293`'s existing distinction. Test it.

- [ ] **Step 4: Tests + commit**

```bash
git add -A cli/
git commit -m "feat(board): board thread renders a reply chain across topics"
```

---

### Task 5: `agent thread` (agent face, no capability)

**Files:**
- Create: `cli/agent/thread.go`
- Test: `cli/agent/thread_test.go`

**Interfaces:**
- Consumes: `cli.BuildThreads`, `cli.TreePrefix`, the body writer from Task 3.
- Produces: `harness-cli agent thread [--seq N] [--task ID]... [--headers-only] [--raw] [--json]`

- [ ] **Step 1: Read why this verb exists separately**

`server/agent_handler.go:804-810` for the uncapped-read rationale, and `:660`
for the gate `board topics` is behind. The whole point of this task is that a
task holding no capabilities can run it. If you find yourself calling
`BoardTopics`, you have rebuilt the operator verb and it will be denied.

- [ ] **Step 2: Collect from the agent's own subscriptions**

`agent subscriptions` for the topic set, `agent retained` / `agent inbox` for
the messages. Same assembly, same rendering.

- [ ] **Step 3: Prove the capability claim by running it, not by reading it**

This is the acceptance criterion for the task. Report the exact command and
output. Do not claim it works because no cap check appears in the code.

- [ ] **Step 4: Commit**

```bash
git add -A cli/agent/
git commit -m "feat(agent): agent thread, readable without a capability"
```

---

### Task 6: wasm bridge

**Files:**
- Modify: `cmd/harness-webui-wasm/main.go` (register beside `boardRead` at :144-148)

- [ ] **Step 1: Expose `boardThread`**

Returns the rows `BuildThreads` produced. **`seq` and `in_reply_to` are decimal
strings**, for the reason at `main.go:1483` — copy that comment's pattern. The
JS side never receives a board seq as a number.

- [ ] **Step 2: `make wasm-check`, then commit**

```bash
git add cmd/harness-webui-wasm/main.go
git commit -m "feat(webui): expose the reply-chain assembly to the browser"
```

---

### Task 7: WebUI tab

**Files:**
- Modify: `webui/index.html` (a tab beside `data-tab="board"` at :41 / the
  section at :303), `webui/static/main.js` (the board panel block at :5201+),
  `webui/static/style.css` as needed

- [ ] **Step 1: A tab that draws rows and walks nothing**

The JS receives ordered rows with a depth and last-child flags. It draws them.
If you write a loop in JS that follows `in_reply_to`, you have created the
second walk this design exists to prevent.

- [ ] **Step 2: Say what the window is**

The panel states "last 30 minutes, 64 per topic" and marks orphans, so an empty
view reads as "nothing recent" rather than "broken".

- [ ] **Step 3: `make webui-build`, screenshot the panel with a real chain, commit**

The screenshot is a deliverable — report its path, do not delete it.

---

### Task 8: TUI view

**Files:**
- Modify: `tui/board.go` (or a sibling file beside it), plus wherever its
  keybinding is declared

- [ ] **Step 1: A chain view reachable from the board view, leaving it unchanged**

Read `tui/board.go` in full first and follow its existing view/keybinding
pattern rather than inventing one.

- [ ] **Step 2: Tests where the TUI has them, `make check`, commit**

---

### Task 9: parity walk and completion

- [ ] **Step 0: Every surface states its window**

The spec's Risks section requires each surface to state "64 per topic, 30
minutes" and to mark orphans, so an empty view reads as "nothing recent" rather
than "broken". Task 7 does it for the WebUI. Confirm the CLI verbs and the TUI
view do too, and add it where they do not.

- [ ] **Step 1: Walk `surface-parity-checklist` item by item**

`harness-cli skill surface-parity-checklist`. A verdict per number, written out.
Not a summary — the checklist is numbered because each number is a separate
question.

- [ ] **Step 2: `make check` green**

- [ ] **Step 3: Report**

The spec's Completion section, item by item, with the evidence for each.
