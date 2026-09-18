# A reply-chain view of the agentboard, beside the topic view

The board is keyed by topic. A conversation is not: it crosses topics by
construction, because each agent receives on its own `chat.<short-id>`. So every
existing view shows one side of every exchange. This adds a second view that
follows `in_reply_to` instead of `topic`, on all three surfaces, and leaves the
topic view untouched.

## Decisions taken

| # | Decision | Decided by |
|---|----------|------------|
| D1 | The existing topic-centric board view stays as it is. The chain view is a **second** view, not a replacement. | operator, 2026-09-18 |
| D2 | Live only. No persistence, no change to the board's ring/TTL/in-memory design. | operator, 2026-09-18 |
| D3 | All three surfaces (CLI / TUI / WebUI), fed by one assembly written in Go. | operator, 2026-09-18 |
| D4 | Three entry axes: a seq, the whole visible board, and a set of tasks. | operator, 2026-09-18 |
| D5 | Two verbs, `board thread` (operator) and `agent thread` (agent), because one capability answer does not fit both faces. | author, from `server/agent_handler.go:660` vs `:804-810` |
| D6 | `seq` and `in_reply_to` cross the wasm boundary as decimal strings. | forced by `cmd/harness-webui-wasm/main.go:1483` |
| D7 | Payload escaping is a **terminal-only presentation step**: it applies when stdout is a character device and at no other time, so redirected and piped output stays byte-exact. `--json` stays byte-exact unconditionally. | operator, 2026-09-18; mechanism from `cmd/harness-cli/git.go:95` |

## Problem

**A conversation lives in two topics, and no view joins them.** An agent's
inbound topic is `chat.<first-8-hex-of-its-task-id>`, so when A and B talk, A's
messages are retained under B's topic and B's under A's. Measured on the
2026-09-18 exchange between task `70fbad4a…` (claude) and task `c96af19d…`
(pi): five messages, three on `chat.c96af19d` and two on `chat.70fbad4a`. Open
either topic in the WebUI board panel and you read one half of a dialogue.

**Nothing renders a chain, on any surface.** `in_reply_to` is carried
everywhere — `agentboard`, the wire row, `agent inbox --json`, `board read
--json`, the TUI and the WebUI — and every consumer treats it as a scalar to
print. The closest existing behaviour is `board read --in-reply-to N`
(`cli/cmd_board.go:243`), which keeps the messages whose `InReplyTo` equals N:
one level of direct replies, within one topic. A chain of five messages needs
four such calls against two topics, and the operator interleaves the results by
hand.

**An agent cannot reconstruct what it was answering.** A resumed or
context-reset agent holds a seq in an `in_reply_to` field and has
`harness-cli agent read <seq>` to fetch that one parent — one hop, one call,
and nothing that walks the chain.

### What is NOT the problem

- **The board forgetting things.** Ring of 64 per topic, 30-minute TTL after
  the last publish, in memory only (`cmd/harness-server/main.go:38-39`). That is
  a deliberate design and D2 keeps it. The consequence — see Risks — is that
  this view shows recent activity, never history.
- **Rendering one message's payload readably.** `examples/board-render/`
  already does that as a worked sample. It is not a dependency of this work and
  is not consumed by it.
- **The topic view being wrong.** It answers "what is on this topic", correctly.
  It is simply not the question a reader asks when following an exchange.

## Scope

In: a shared assembly in Go; two CLI verbs; a TUI view; a WebUI tab; the wasm
bridge function behind it; payload escaping (D7) applied to both the new view
and `board read`.

Out, explicitly:

- Persistence of any kind (D2).
- A tidy-tree diagram. `cli/tasktree.go` has `TaskTreeLayout` for the task
  graph; the chain view renders indented rows only. If a diagram is wanted
  later it reads the same rows.
- Any change to what the board stores, retains, or evicts.
- Any change to the existing topic view's behaviour (D1).

## The assembly — `cli/boardthread.go`

One walk in Go, three renderers that only draw. This mirrors `cli/tasktree.go`,
whose own comment states the reason: handing each surface a nested structure
would make all three walk the tree themselves, "three walks that can and will
disagree".

```go
// ThreadRow is one board message placed in its reply chain, flattened into
// the order a renderer draws it in.
type ThreadRow struct {
    Msg    BoardMessage
    Topic  string // which topic retained it; a chain spans several
    Depth  int
    IsLast []bool // last-child flags per ancestor level, for the gutter
    Orphan bool   // its in_reply_to names a seq not in the input set
}

// BuildThreads arranges messages under the messages they reply to.
func BuildThreads(msgs []BoardMessage, topicOf map[uint64]string) []ThreadRow
```

**Corrected 2026-09-18, after the implementation caught it.** This sketch first
read `[]protocol.BoardMessageRow` and `topicOf func(seq uint64) string`, which
is wrong twice over, and the worker that implemented Task 1 raised it rather
than quietly following one of the two documents.

`BuildTaskTree` takes `protocol.TaskInfo`, so the protocol type looks like the
house pattern — but the board path differs: `cli.BoardMessage` is the decoded
form the CLI already works in (`FromTaskHex` is a hex string there, not raw
id bytes), and every board consumer in `cli` already holds one. Taking the wire
row would make each caller re-decode on the way in.

`topicOf` is a map rather than a function because every caller builds the
seq→topic association while collecting the messages anyway; a lookup function
would buy laziness nobody asked for and give each caller a second thing to get
right.

Rules, each of which a test pins:

1. **Roots** are messages with `InReplyTo == 0`, plus every orphan.
2. **Sibling order is by `Seq` ascending.** Board seq is globally monotonic, so
   this is a total order and stable across polls. `ReceivedAtMs` is not: two
   messages can share a millisecond, and an unstable order moves rows under a
   reader's cursor between refreshes.
3. **An orphan is shown at root, never hidden.** Its parent is missing because
   the parent rotated out of its ring, its topic TTL-expired, it was purged, or
   it sits on a topic this caller cannot read. All four are normal. This is the
   rule `BuildTaskTree` already applies to a task whose creator is out of
   scope, for the reason its comment gives: a tree view re-orders a listing, it
   never filters one.
4. **Every input message comes back exactly once**, and a cycle in the
   `in_reply_to` links terminates. The server mints `seq` monotonically and a
   reply always names an earlier seq, so a cycle is unreachable in normal
   operation; the guard is against a malformed or hand-built input set, matching
   `BuildTaskTree`'s treatment of a creator cycle.
5. **A chain spans topics.** `in_reply_to` names a global seq that is very often
   retained under a different topic. `BuildThreads` therefore takes a flat
   message list and never assumes a single topic; `Topic` is carried per row so
   the renderer can show where each message landed.

### One gutter renderer, not two

`TreePrefix` currently takes a `TaskTreeRow` (`cli/tasktree.go:167`). Change its
parameter to the `[]bool` it actually reads, so the chain view draws an
identical gutter. Two non-test call sites: `cli/list.go:326` and
`tui/tasks.go:219`. Its own comment already claims to be the "single
implementation shared by every surface"; a second copy for chains would make
that false.

## Two faces, because one capability answer does not fit both

`board topics`, `board read` and `board subscribers` require `board_observe`
(enforced centrally before dispatch — `server/board_handler.go:14`,
`server/agent_handler.go:660`). The agent-side reads — `inbox`, `wait`, `send`,
`subscribe`, `retained` — require none, and `server/agent_handler.go:804-810`
records why: a cap there "would gate a read more tightly than the content it
summarizes, for no gain", since subscribing already exposes the same content.

A single `board thread` verb would therefore inherit `board_observe` and be
unusable by exactly the agents this view is meant to serve — a worker spawned
with `--caps` omitted holds nothing, and that is the common case.

| Verb | Capability | Visible set |
|------|-----------|-------------|
| `harness-cli board thread` | `board_observe` | every topic on the board |
| `harness-cli agent thread` | none | the topics this task subscribes to |

The two differ only in how they collect messages. Both hand the same flat list
to `BuildThreads` and print the same rows. `agent thread` on a task that
subscribes to one topic shows its own side plus whatever replies landed there —
which is the whole chain whenever the peer replied to it, and an orphan-rooted
fragment otherwise. That asymmetry is honest: it is exactly what that task can
see.

## The three axes

Both verbs take the same selectors; they compose as an AND.

| Flag | Meaning |
|------|---------|
| *(none)* | every chain in the visible set, roots in seq order |
| `--seq N` | the chain containing N: walk up to its root, then expand all descendants |
| `--task <id>` (repeatable) | keep chains that involve the named tasks. One id: every chain that task sent into or received on. Two: the pair's exchange. |

`--task` is repeatable rather than a two-argument `--between` so that one id and
three ids are expressible in the same grammar; a pair is not a special case.

A `--seq` naming a message that is not in the visible set is an error naming the
seq, not an empty result — the same distinction `board read` already draws when
a topic holds messages but none reply to the requested seq
(`cli/cmd_board.go:290-293`).

## Payload rendering: two obligations that must not be traded for each other

The chain view prints bodies by default; a conversation viewer that printed only
headers would not answer the question it exists for. `--headers-only` suppresses
them.

Two obligations apply at once, and an earlier draft of this spec sacrificed the
second to the first.

**Obligation 1 — a body drawn on a terminal must not control it.** Printing a
body means handing an untrusted peer's bytes to a terminal.
`tui/rawforward.go:74` already decided what to do, and its comment says the
WebUI pane decided the same: keep `\n` and `\t`, replace every byte below 0x20,
`0x7f`, and `0x80`–`0x9f`. C1 is in that set because `U+009B` is CSI — a
terminal honouring 8-bit controls acts on it the way it acts on `ESC [`.
`cli/cmd_board.go:287` does not apply it: `out.Write(m.Payload)` sends raw
bytes, so a message carrying `ESC [ 2 J` clears the operator's screen today.

**Obligation 2 — the CLI is a data path, not only a display.** `harness-cli
board read topic > out` and `… | jq` must yield the bytes the sender published.
Escaping unconditionally would corrupt every such extraction, which is a worse
defect than the one it fixes: a cleared screen is visible, silently altered
bytes are not.

This repo already resolved exactly this tension, for the same reason, in
`cmd/harness-cli/git.go:95`:

> `isTTY` reports whether f is a terminal, which is the only condition under
> which colour escapes belong in the output — a redirected diff has to stay
> byte-clean for `git apply`.

So escaping is a **presentation step gated on the destination**, never a
transformation of the data:

| Destination | Body bytes |
|-------------|-----------|
| stdout is a character device (`isTTY`) | escaped |
| stdout redirected or piped | exact, unchanged |
| `--json` (either verb, any destination) | exact, as `payload_b64` — already how `emitBoardMessageJSON` carries it (`cli/cmd_board.go:77`) |
| `--raw` | exact, even on a terminal |

`--raw` exists so an operator reading interactively can still copy exact bytes
without redirecting. There is deliberately no inverse flag forcing escapes into
a redirected stream: no current consumer wants one, and adding it would create a
second way to produce a file whose bytes are not the message.

The escape helper lives in `cli/`, is used by the chain view and by `board
read`'s body printing, and writes a disallowed byte as `\xNN` rather than
mapping it to `.` as `sanitizeOutput` does. The two are right for different
jobs: a bordered panel must preserve the column count, so it maps one rune to
one rune; a transcript has no border to protect and gains from keeping the byte
identifiable. `cli` does not import `tui` (and `tui` imports `cli`), so the
helper belongs in `cli`; this change does not alter `sanitizeOutput`.

JSON bodies keep their `json.Indent` path and are escaped under the same gate:
`encoding/json` escapes C0 inside strings but leaves C1 raw, so an indented
JSON body still reaches a terminal carrying a live `U+009B`.

The TUI and the WebUI always escape. The gate has no second case to serve
there: neither writes to a stdout anyone can redirect — the TUI draws into a
terminal UI and the WebUI into a DOM — so there is no byte-extraction path
through them to protect. Extraction from those surfaces is `--json` on the CLI.

## The wasm boundary: seq is a string

`cmd/harness-webui-wasm/main.go:1483` states it for `lastSeq`: board seq is
UnixNano-seeded (~1.9e18), past `Number.MAX_SAFE_INTEGER` (2^53-1), and a
float64 "silently rounds to the nearest ULP (~256)".

The chain view joins parent to child on seq equality alone. At ~256 granularity
distinct messages collide onto one key, so the failure is not a misprinted
number: replies attach to the wrong parent, or to each other, and the view looks
plausible while being wrong. Every `seq` and `in_reply_to` crossing into JS is a
decimal string, and the JS side compares them as strings.

## Surface matrix

| Surface | Change |
|---------|--------|
| CLI | `board thread`, `agent thread` verbs, both with `--headers-only` and `--raw`; `board read` body printing routed through the escape helper behind the `isTTY` gate, and `board read` gains `--raw` |
| CLI (shared) | `cli/boardthread.go`; `TreePrefix` signature change |
| TUI | a chain view beside the existing board view, reachable from it; the topic view unchanged |
| WebUI | a tab beside Board; JS draws rows returned by wasm and performs no walk |
| wasm bridge | `boardThread` beside `boardRead` / `boardTopics` (`main.go:144-148`), seq fields as strings |
| server | none — both verbs are built from calls that already exist |

The `surface-parity-checklist` skill is walked item by item before this is
called done, with a verdict per number.

## Testing

Go unit tests on `BuildThreads`, mirroring `cli/tasktree_test.go` and
`cli/tasktree_render_test.go`:

- empty input; a single root; a linear chain of four
- two roots interleaved by seq
- an orphan (parent absent) rendered at root, keeping its own children
- a chain whose messages come from three different topics
- sibling order stable when two messages share `ReceivedAtMs`
- a cycle in `in_reply_to` terminating with every message emitted once
- `TreePrefix` output unchanged for task rows after the signature change

Plus, for the two obligations in the payload section — the second is the one
that needs a test most, because nothing about it is visible when it breaks:

- the escape helper pins the byte set (C0 except `\n`/`\t`, `0x7f`,
  `0x80`–`0x9f`), including inside an indented JSON body carrying `U+009B`
- `board read` and both new verbs, with stdout NOT a character device, emit the
  published bytes **byte-for-byte** — asserted by comparing against the
  published payload, for a body containing ESC, CR, `U+009B` and a non-UTF-8
  run. This is the regression test for the defect this spec shipped in its
  first draft
- `--raw` yields those same exact bytes when stdout IS a character device
- `--json` carries the exact bytes as `payload_b64` in every case

## Risks

**Orphans are the normal case, not an edge case.** With a 30-minute TTL, a chain
older than the window has lost its root, so the view will frequently show
orphan-rooted fragments. Hiding them would empty the view during ordinary
operation; marking them is what keeps it legible. Each surface states the window
(64 per topic, 30 minutes) so an empty view reads as "nothing recent" rather
than "broken".

**`agent thread` shows a partial conversation by design.** A task sees its own
subscribed topics. The view says so rather than implying it is the whole
exchange.

**Nothing here survives a server restart.** The board is in memory and a restart
drops every retained message and re-seeds only `chat.<short-id>` subscriptions.
Accepted under D2.

## Completion

1. `BuildThreads` + tests green.
2. Escape helper + tests green; `board read` routed through it behind the
   `isTTY` gate, with the byte-exactness of redirected output asserted.
3. `board thread` and `agent thread`, the capability split verified by running
   `agent thread` from a task holding no capabilities.
4. TUI view and WebUI tab, both drawing rows they did not walk.
5. `surface-parity-checklist` walked, verdict per number.
6. `make check` green.
