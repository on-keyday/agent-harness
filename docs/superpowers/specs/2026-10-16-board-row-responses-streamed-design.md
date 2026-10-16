# Board responses carry their rows on a stream — design

Status: DECIDED (operator, 2026-10-16) except the rows marked **this spec**.

## Problem

The agentboard's list-shaped responses put every row inline in one
task-control message. Every task-control message is one objproto application
message, which over UDP is one datagram. A datagram that does not fit is
dropped with no error at either end, and the budget that fits every path is
about 1170 bytes (`2026-10-10-exec-request-streamed-design.md`, Problem).

The delivery responses — wait, inbox, inbox_advance, read_seq — carry
`DeliveredMessage` rows with no limit on how many
(`server/agent_taskcontrol.go:226-299`). Measured by encoding
`TaskControlResponse{AgentInboxAdvance}` with N rows (topic `chat.70fbad4a`,
hostname 7 bytes, profile `claude`, no reply route):

| rows | bytes |
| --- | --- |
| 1 | 101 |
| 12 | 1080 |
| 13 | 1169 |
| 64 | 5708 |

89 bytes a row, so the 14th unread message pushes the response over budget.
With a 45-byte topic and reply route (`tree.<32-hex>.build`,
`2026-10-16-agentboard-reach-design.md`) a row is 162 bytes, and 8 rows are
over budget. The measurement is a throwaway test in `runner/protocol`:

```go
rows := make([]DeliveredMessage, n)
for i := range rows {
	rows[i].SetTopic([]byte("chat.70fbad4a"))
	rows[i].SetFromHostname([]byte("gmkhost"))
	rows[i].SetFromAgentProfile([]byte("claude"))
}
var out AgentInboxAdvanceResponse
out.SetMsgs(rows)
resp := TaskControlResponse{Kind: TaskControlKind_AgentInboxAdvance}
resp.SetAgentInboxAdvance(out)
len(resp.MustAppend([]byte{0})) // the appwire kind byte, as respondAgent sends it
```

Agents use whichever transport their runner connected on
(`runner/servercandidates.go:28`), UDP included. On inbox_advance the failure
is worse than a lost read. `Board.InboxAdvance` marks rows as delivered when it
collects them, before the response is sent, so the hook loses those messages
from the automatic path for good. A dummy harness cannot show it, because
loopback's MTU is 65536; `scripts/netem-lab` can, and did (Verification).

`2026-05-09-udp-dualstack-design.md` §12.1 listed the Wait/Inbox envelope as a
"remaining theoretical offender" and judged it "~5 KB at default — fits MTU".
5 KB does not fit a 1170-byte budget. That row is wrong, and this spec
supersedes it. The same section listed the topic list (1024 topics at the cap)
as over budget.

The other board responses have the same shape:

| Response | Rows | Bound on row count |
| --- | --- | --- |
| `AgentListRetainedResponse` | `RetainedMeta` | the ring (`--agentboard-ring`, 64) |
| `AgentListSubscriptionsResponse` | `AgentSubscriptionSummary` | none |
| `BoardTopicsResponse` (operator, and agent `list_topics`) | `BoardTopicRow` | `--agentboard-max-topics`, 1024 |
| `BoardReadResponse` | `BoardMessageRow` | the ring |
| `BoardSubscribersResponse` | `BoardSubscriberRow` | tasks known to the board |

## Decisions taken

**operator** = chosen in conversation (2026-10-16); **this spec** = chosen by
the author while writing.

| # | Decision | Decided by |
| --- | --- | --- |
| D1 | Rows move onto a trsf stream. The response keeps its scalars and gains the stream id — the snapshot's shape (`ListResult` / `ListResultBody`, `bce2af8`) | operator |
| D2 | All nine responses above (the four delivery responses and the five in the table), in one change | this spec |
| D3 | A message body travels inside its row, on the same stream. `DeliveredMessage` loses `payload_stream_id` and carries `payload`. `BoardMessageRow` loses `size` and carries `body`. One stream per response | this spec |
| D4 | The response is sent first and the stream written after it, on all nine | this spec |
| D5 | One server function sends every one of these responses, and one client function reads every body | this spec |
| D6 | A test fails when a `*Response` format in `message.bgn` holds a list of rows and is not on a written allowlist | this spec |

## 1. Wire (D1, D3)

Each response keeps what it has besides its rows (`request_id`, `status`,
`next_cursor`, `timed_out`) and gains `stream_id :u64`. Each list moves into a
`…Body` format on the stream, named after the response:

| Response | Body on the stream |
| --- | --- |
| `AgentWaitResponse` / `AgentInboxResponse` / `AgentInboxAdvanceResponse` / `AgentReadSeqResponse` | `DeliveredBody { msgs_len :u16, msgs :[msgs_len]DeliveredMessage }` — one format for all four, because the rows are the same |
| `AgentListRetainedResponse` | `RetainedBody { metas }` |
| `AgentListSubscriptionsResponse` | `SubscriptionsBody { subscriptions }` |
| `BoardTopicsResponse` | `BoardTopicsBody { topics }` |
| `BoardReadResponse` | `BoardReadBody { msgs }` |
| `BoardSubscribersResponse` | `BoardSubscribersBody { rows }` |

`DeliveredMessage` gains `payload_len :u32, payload :[payload_len]u8` in place
of `payload_stream_id`. `BoardMessageRow` gains `body_len :u32,
body :[body_len]u8` in place of `size`.

**Why bodies go inline again (D3).** They left the row in `7e42988` because
the row rode in a datagram. On a stream that reason is gone, and keeping them
on separate streams costs two things:

- The agent face opens one stream per message (`deliveredRows`,
  `server/agent_taskcontrol.go:190`), and the client polls up to 2 s for each
  to become visible (`fetchDeliveredPayload`, `cli/agent/client.go:68-108`).
- `board read` concatenates every body on one stream and slices it by each
  row's `size` (`cli/board.go:273-300`). Those bytes are not described by any
  schema: the schema shows a `size` and a stream id, not that the stream is the
  bodies laid end to end in row order.

With the body in its row, the stream is one format decoded once, and every byte
on it is in the schema.

**Memory.** A body stream holds every row's body. The server already holds them
in the ring, and the client already reads every body before emitting anything
(`cli/agent/inbox.go:97-105`), so the peak per response does not change. No new
cap is added.

## 2. Ordering (D4)

`deliveredRows`' comment states the rule: the peer reads a stream only once it
has been told about it, so a body written before the response lands only as far
as the peer's receive window. Past that the write never completes. The
snapshot writes its body first (`server/task_handler.go:1925-1953`) and gets
away with it because a snapshot is small. These bodies are up to
`--agentboard-max-payload` (1 MiB) per row, so all nine send the response
first, then write the body and EOF on a goroutine, as
`flushDeliveredPayloads` did for the per-message streams.

On a stream-creation or encode failure, the response goes out with
`stream_id = 0`, and the client reports that as an error. That is the
snapshot's convention.

## 3. One writer, one reader (D5)

- **Server:** one function takes the connection, the response with its stream
  id left zero, and the encoded body. It creates the stream, fills in the id,
  sends the response, then writes the body. The nine handlers in
  `server/agent_taskcontrol.go` and `server/board_handler.go` call it.
  `deliveredRows` becomes a plain rows-to-wire conversion. `pendingPayload`,
  `openDeliveredPayloadStream` and `flushDeliveredPayloads` go.
- **Client:** one function in `cli` waits for the stream (`waitForReceiveStream`,
  `cli/get_log.go:89`) and reads it to EOF. `fetchDeliveredPayload` goes, along
  with the five call sites that use it (`cli/agent/inbox.go`, `read.go`,
  `wait.go`, `dispatch.go`, `thread.go`), and so does `board read`'s slicing
  loop. Every reader of the nine responses goes through the new function:
  `cli/board.go` (topics, read, subscribers) and `cli/agent/` (`inbox.go`,
  `read.go`, `wait.go`, `dispatch.go`, `thread.go`, `retained.go`,
  `subscriptions.go`, `topics.go`).
- The TUI and the WebUI reach these responses only through `cli`: a grep of
  the Go tree for the nine response accessors (`.AgentWait()`, `.BoardRead()`,
  …) hits only `cli/` and `server/`. They
  need no change of their own beyond what `cli` gives them.

The other nine `ReadDirect` loops in `cli` (`list.go`, `conns.go`,
`get_log.go`, `await_idle.go`, `exec_run.go` ×2, `port_forward.go`,
`port_forward_list.go`, `trsf_state.go`) are not migrated. They are not part of this
defect, and each has its own error text that callers match on.

## 4. Guard (D6)

`TestResponseRowsAreNotInline` (`runner/protocol/response_rows_test.go`) reads
`message.bgn` and fails when a format named `…Response` or `…Result` holds a
list of anything but bytes and is not on an allowlist. Each allowlist entry
carries the reason it may stay inline. That reason is what the next person
adding a list to a response has to write, instead of finding out over UDP. An
entry whose format no longer holds a list fails too, so the list cannot go
stale.

It reads the schema rather than the generated Go, which an earlier draft of
this section said. Go reflection cannot enumerate a package's types, and the
schema is the file a person adding a field edits. Seen red first: against the
schema before this change it named exactly the nine responses of D2 and
accepted the six below.

The allowlist starts with the six list-carrying responses this change leaves
alone. Rough worst cases, computed from their formats and not measured:

| Response | Bytes a row | Over budget at |
| --- | --- | --- |
| `RestoreTasksResponse` | 38 + repo path + **the whole prompt** (`RestorableTask.prompt`, `:u32` length) | **one candidate with a prompt of about 1 KB** |
| `OpenInteractiveResponse` (ambiguous runner only) | about 95 | about 12 candidate runners |
| `RunnerHelloResponse.accepted` | 16 | about 72 tasks re-adopted by one runner |
| `SetCapsResponse.affected` | 16 | about 72 descendants changed at once |
| `RunnerTrsfStateResponse` | 45 + counters | depends on the counter count |
| `WhoAmIResponse.overrides` | 8 + 16 per id | overrides are few |

`RestoreTasksResponse` is the one most likely to be hit, because a long prompt
alone fills the budget. It stays on the allowlist (operator, 2026-10-16): it is
not a board response, and oneshot tasks, which are the ones carrying long
prompts, see little use. Its allowlist entry says so, and gives that as the
trigger to revisit.

## 5. Version skew

None of these formats is decoded by the runner. The readers are `harness-cli`
(which agents run from the runner host's `bin/`), the TUI, and the WebUI's
wasm.

- **Old client × new server:** the client finds `stream_id` where it expected a
  row count. An agent's inbox hook stops delivering until `harness-cli` is
  rebuilt. `make build` on the runner host replaces the binary in place, and
  the next hook run picks it up. Runners need no restart.
- **Order:** server first, then `make build` on every runner host, then
  operator clients. The WebUI picks up the new wasm on reload.
- `scripts/wire-skew-check.sh` covers runner × server, which this does not
  change. It is run anyway.

## Testing

- **Size and structure:** D6's guard, which makes an inline list a test
  failure rather than a size to remember. With the rows gone, every one of the
  nine responses is fixed-size. `TestAgentInboxAdvanceBodyCarriesEveryMarkedRow`
  also asserts the encoded response stays under 1170 bytes with 40 rows queued.
- **Round trip:** `TestBoardReadBodyRoundTrip` and `TestDeliveredBodyRoundTrip`
  (`runner/protocol/board_view_test.go`), including an empty body that must not
  swallow the row after it.
- **inbox_advance is not lossy:** `TestAgentInboxAdvanceBodyCarriesEveryMarkedRow`
  (`server/agent_taskcontrol_test.go`): 40 rows across five topics arrive in the
  body, and a second advance returns none.
- **Live, reproduced first:** `scripts/netem-lab`, with no `--mtu`: the lab's
  veths have the default 1500-byte MTU, which is enough, since loopback's 65536 is
  what hides the defect. Results under Verification.
- `board read`, `board topics`, `board subscribers`, `board thread`,
  `agent retained`, `agent subscriptions`, `agent read`, `agent inbox`,
  `agent wait` and `agent thread` each run once through the CLI, over UDP in
  the lab. Those paths are only reached through argv, which Go tests do not
  execute (implementation-pitfalls, Pitfall 13). The TUI's and the WebUI's
  board views were not run: neither has code of its own in this change, and
  both reach these responses through the same `cli.Client` methods the CLI
  verbs exercised.

## Verification (2026-10-16)

Lab: `scripts/netem-lab/netem-lab.py --name rows up --profile lan`, with
binaries from `make build` in the worktree, so `bin/` holds this tree's code
(`buildinfo.stampedDirty=1`). Every command ran through
`netem-lab.py --name rows exec cli -- bin/harness-cli --server-cid "$CID" …`.
Before the lab would start, `netem-lab.py` called `dummy_harness.bash_profile`,
which `2efea7d8` had replaced with `agent_profiles_json`. It is fixed in this
change.

| Step | Before (`1d0efc33`) | After |
| --- | --- | --- |
| `board send --topic probe.rows --no-wake m$n` ×3, then `board read probe.rows` | 3 rows, 0.5 s | — |
| the same with 40 messages | no output; killed by `timeout 40` (exit 124); nothing in the server log | 40 rows, 0.58 s |
| 20 × `board send --topic chat.<task> --no-wake`, then `exec <task> -- harness-cli agent inbox --json --user-prompt-submit-hook` (the hook the runner installs, `runner/settings.go:38`) | no output for 30 s; `board subscribers` went from `pending=20` to `pending=0` — all 20 marked delivered and never received; a second run returned nothing | 20 records (`x1`…`x20`) in 0.51 s, then `pending=0` |

The task was a detached bash session (`session new --agent bash -d`).
`harness-cli exec` runs a command with that task's agent environment
(`runner/exec_run.go:234`), which is how the hook was driven from inside it.

## Out of scope

- The six responses on the D6 allowlist, `RestoreTasksResponse` included (§4).
- A send-side size check for responses. The exec change (D2 there) chose
  request paths only. This change removes the oversized responses it knows of,
  and D6 stops new ones from being added silently.
- Migrating the other stream read loops in `cli` (§3).
