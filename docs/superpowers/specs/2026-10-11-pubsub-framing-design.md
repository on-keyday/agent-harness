# Pubsub framing — design

## Problem

A subscriber's pubsub stream carries no message boundaries, and one reader
assumes it does.

The broker writes each published message to every subscriber's stream as raw
bytes (`pubsub/pubsub.go`, `PublishFiltered`: `p.conn.AppendData(false, msg)`),
and the notifications replay does the same (`server/notify_replay.go`). A trsf
read does not return one write: `ReadDirect` returns every contiguous queued
chunk up to its limit (objtrsf `trsf/ordering.go`, `OrderingQueue.ReadDirect`),
so a read can hold part of a message, or several.

`tui/events.go`'s `subscribeAndStream` decodes each read as exactly one event.
It serves `tasks.status`, `runners.status`, `conns.status`, `forwards.status`,
`execs.status` and the task log. An `ExecStatusEvent` carries the exec's whole
argv (`ExecRunInfo.argv`); an argv of a few KB spans several segments, a read
returns the first of them, and the TUI logs

```
WARN decode exec event err=decode ExecStatusEvent: not enough data to read for field "ExecArg::Arg"
```

and drops the event. A dropped `exec_ended` leaves a finished exec on the
pane. Two events coalesced into one read lose the second without a word: the
reader uses `Decode`, which ignores trailing bytes.

Every other reader already accumulates and drains — decode, advance by what
was consumed, keep the tail (`cli/watch.go`, `cli/conns.go`,
`cli/notify_watch.go`, `tui/events.go` `SubscribeNotifications`). That works
only while a reader knows every field of every event. When a server appends a
field to an event format (as `ExecRunInfo` gained `pty` on 2026-10-10), an
older reader decodes the event short, takes the new bytes as the start of the
next event, and every later event on that stream is misaligned. The stream
does not say where an event ends; only the reader's schema does.

## Decisions taken

**operator** = the human chose it in conversation (2026-10-11), including
everything in the design presented and approved there; **this spec** = the
author chose it while writing — those rows are the ones worth a second look.

| # | Decision | Decided by |
| --- | --- | --- |
| D1 | Frame on the wire: the broker length-prefixes every message, and the frame is described in `.bgn` | operator |
| D2 | Every topic is framed, the task log included — one rule for every subscriber stream, no per-topic knowledge of whether it is framed | operator |
| D3 | Only the broker → subscriber direction is framed. A subscriber publishing to the broker still writes raw bytes | operator |
| D4 | One writer and one reader live in `pubsub`; every producer and every reader goes through them, and the per-format `drain*` helpers are deleted | operator |
| D5 | A frame's payload is decoded with `Decode`, not `DecodeExact`, so an older reader accepts an event a newer server appended fields to | operator |
| D6 | A payload that does not decode is logged and skipped; the reader moves on to the next frame | operator |
| D7 | A frame length at or over a cap ends the stream with an error that names a server older than the client as the likely cause | operator |
| D8 | An old client against a new server is not detected or tolerated; the order is server first, then clients | operator |
| D9 | The cap is 16 MiB | operator |
| D10 | `PubSub.OnSubscribe` hands its callback a function that sends one message, not the stream, so a replay cannot write unframed bytes | this spec |
| D11 | The length is `u32` | this spec |

## Wire

`pubsub/protocol/*.bgn` gains:

```
# PubSubFrame is the unit of a broker → subscriber pubsub stream: every byte
# the broker writes to a subscriber's stream is a sequence of these. The other
# direction — a subscriber publishing — is raw bytes, and each read the broker
# makes of it becomes one message.
format PubSubFrame:
    data_len :u32
    data :[data_len]u8
```

`u32` (D11) because the broker republishes a publisher's stream one read at a
time, and that read's limit is `trsf.InitialFlowWindow`, which exceeds 64 KiB.

### Why the publishing direction stays raw (D3)

The only subscriber that publishes is the runner, and what it publishes is the
task log (`runner/connect.go`, `Session.Publish` → `peer.Conn.Publish`): bytes
with no structure, for which "each read is one message" is already correct.
Framing that direction would make a runner/server pair whose versions differ
corrupt task logs, for no reader that needs it. A structured message published
by a subscriber, should one ever exist, frames that direction then.

## Producer

`pubsub` gets one way to write a message to a subscriber, used by both places
that write to subscriber streams:

- `PublishFiltered` — every live message on every topic;
- the notifications replay (`server/notify_replay.go`), reached through
  `PubSub.OnSubscribe`.

`OnSubscribe`'s callback receives a `send func(msg []byte) error` that frames,
instead of the `trsf.BidirectionalStream` (D10). The callback has no other use
for the stream, and without it there is no way to write a subscriber an
unframed message.

Nothing persisted changes: `LogStore` stores what the log tap receives (the
message, not the frame), and the notify ring stores `NotifyEvent`s. Framing
happens only when bytes go to a subscriber.

## Readers

`pubsub` gets a reader that wraps a subscriber stream and returns one frame's
payload per call, accumulating across reads and holding back a partial frame.
Every subscriber goes through it:

| Reader | Today |
| --- | --- |
| `tui/events.go` `subscribeAndStream` (five status topics, task log) | one read = one event |
| `tui/events.go` `SubscribeNotifications` | `drainNotifyEvents` |
| `cli/watch.go` `Watch` (CLI and WebUI) | `drainTaskEvents`, `drainRunnerEvents` |
| `cli/conns.go` `watchConnStatusEvents` | `drainConnStatusEvents` |
| `cli/notify_watch.go` (CLI and WebUI) | `drainNotifyEvents` |
| `cli/logs.go` `Logs` with follow | raw bytes |

The `drain*` helpers go (D4). The task log readers write each frame's payload
as it arrives; the event readers decode it with `Decode` (D5).

The WebUI reaches the topics only through `cli.Client.Watch` and
`cli.Client.WatchNotifications`, so it needs no change of its own. The `pubsub`
reader builds under `js/wasm` like the rest of `cli`.

## Errors and version skew

- **A payload that does not decode** (D6): a WARN naming the topic and the
  error, and the next frame. The length already delimited it, so nothing after
  it is misaligned.
- **A frame length of 16 MiB or more** (D7, D9): the reader returns an error and the
  stream's consumer ends — the TUI's `resubscribeLoop` rejoins with backoff, the
  CLI verbs return it. Text:
  `pubsub: frame of N bytes exceeds the 16 MiB cap — is the server older than this client?`
  This is the new-client × old-server case, detected on a best-effort basis:
  the first four bytes of an unframed message read as a big-endian length, and
  the cap is 16 MiB = `0x01000000`, so the length reaches it whenever the
  first byte is non-zero — a `NotifyEvent` (it begins with a nanosecond
  timestamp) and a status event of any kind but `task_queued`, ordinal 0. A
  `task_queued` event, or a task log (text), can read as a length under the
  cap; that reader then waits for a frame that never completes and shows
  nothing.
- **Old client × new server** (D8): the old reader takes the length as event
  bytes and misreads the stream. Not detected. The runner is not a subscriber
  and is unaffected (D3), so a server restart does not need a runner restart;
  operator clients (TUI, `harness-cli watch` / `logs -f`, WebUI) are restarted
  after the server. The WebUI gets the new code when its page is reloaded,
  since the server serves it.

`scripts/wire-skew-check.sh` exercises runner × server; neither side of that
pair changes how it reads pubsub, so it needs no new phase. Run it anyway.

## Testing

- `pubsub` reader unit tests: the same frame sequence returned whether the
  stream delivers it one byte at a time, all at once, or split mid-length and
  mid-payload; a zero-length frame; a length of exactly 16 MiB is an error carrying the
  "older than this client" text; a payload a caller rejects does not disturb
  the next frame.
- Reproduction, written and seen failing first: through a real server, an
  `ExecStatusEvent` whose argv is about 3 KiB reaches the TUI's
  `execs.status` consumer as one decoded event. Before the change it fails
  with the `ExecArg::Arg` error above.
- Existing tests that read a subscriber stream raw (`pubsub/pubsub_test.go`,
  `pubsub/pubsub_tap_test.go`, `server` tests asserting on published bytes,
  the notify replay test) are updated to the frame.
- Live, on a dummy harness: an exec with a long argv shows on the TUI's exec
  pane with no decode WARN; `harness-cli watch`, `harness-cli logs -f` and the
  WebUI's notifications and task list still update.

## Out of scope

- Framing the publishing direction (D3).
- Negotiating or detecting an old client against a new server (D8).
- agentboard delivery, which does not ride `pubsub` streams.
