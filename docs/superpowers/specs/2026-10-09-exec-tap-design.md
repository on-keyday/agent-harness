# `exec tap` — seeing what an exec carries — design

## Problem

An out-of-band exec (`harness-cli exec`, and every `ssh host cmd` through the
ssh gateway) moves bytes that only the client which started it can see.

- **P1.** A remote-development tool that speaks its own protocol over the
  stdio of one long-lived `ssh host cmd` is run through the gateway, which the
  operator hosts in the TUI. Most of its requests are answered; one specific
  request is not. Nobody can say whether that request's bytes ever crossed the
  harness, or whether a reply came back — the gateway is inside the TUI
  process, and nothing else has a view of the stream.
- **P2.** `exec ls` reports an exec's argv, origin and age. Nothing says
  whether it is moving bytes at all, in which direction, or when it last did.
  A wedged exec and a busy one render identically.

P1 needs the payload, at a point that a client other than the starter can
reach. Counters alone do not answer it: the tool multiplexes many requests onto
one stdin, so "stdin moved" does not say whether THE request moved. P2 is a
glance and needs counters, not a subscription.

`server/exec_run.go:108` is where the bytes go past —
`spliceBidiHalfClose(dataStream, runnerStream, taskIDHex)` — and it copies them
and forgets them, exactly as `relayBytes` did for forwards before
`docs/superpowers/specs/2026-08-29-port-forward-tap-design.md`.

## Decisions taken

The third column records who decided: **operator** means the human chose it in
conversation, **this spec** means the author chose it while writing — those are
the rows worth a second look.

| # | Decision | Decided by |
| --- | --- | --- |
| D1 | A live content tap on an exec, at the server's splice — not a trace file in the gateway | operator |
| D2 | A new capability `exec_tap = 0x40000`; `exec_view` does not imply it. `all` widens `0x3ffff` → `0x7ffff` | operator |
| D3 | stdin is shown by default; the default filter is all three channels | operator |
| D4 | `exec ls` gains per-channel byte counters, last activity and `taps=N`, pushed by an `exec_stats` event | operator |
| D5 | No recording. A tap sees bytes from the moment it opens; the server buffers and writes nothing | this spec (inherited from forward tap D4) |
| D6 | A tap that cannot keep up gets a `gap` record and stays alive | this spec (inherited from forward tap D7) |
| D7 | The server tracks exec frame boundaries on every exec from byte 0, tapped or not | this spec |
| D8 | Records are per CHANNEL (stdin / stdout / stderr), not per direction | this spec |
| D9 | Synth frames count and render as stdout | this spec |
| D10 | Control frames (signal, window size) and unknown frame types are skipped, not shown and not counted | this spec |
| D11 | The tap queue / gap mechanics become one generic type that forward tap and exec tap both use | this spec |
| D12 | Stream offsets come from the splice's own per-stream count, not from the tap; forward tap is moved onto that too | this spec |
| D13 | `removeExec` takes the outcome as an argument, so every removal path tells taps how the exec ended | this spec |
| D14 | CLI, TUI and WebUI all get the tap and the counters in v1 | this spec (`feedback_features_span_all_three_uis`) |
| D15 | A tap's stream ENDS after its last record (`exec_ended`, and `forward_closed` for forwards); the end does not go through the bounded queue, so a full queue cannot drop it | this spec (found while planning, 2026-10-09) |

## Why the tap is at the server (D1)

Two places can see an exec's bytes in the clear: the client that started it,
and the server's splice. `peer.Conn` encryption is per hop, so the server holds
the plaintext — which is the power this feature exposes and the reason for D2.

The client-side alternative (a `--trace-dir` on the gateway) was considered and
not chosen. It needs no wire change, but it sees only execs that one gateway
started, and it puts the observation inside the process the operator is trying
to debug. The server tap is readable from any client — a CLI next to the TUI
that hosts the gateway — and covers `harness-cli exec`, the TUI and the WebUI
too. The objection that a live tap misses what happened before it opened does
not bite P1: the exec under investigation lives for the whole session (an
`exec ls` of a VS Code Remote-SSH connection showed one exec at age 193s and
counting), so the operator opens the tap first and then triggers the request.

`spliceBidiHalfClose` is shared with file transfer (`server/file_transfer.go:92`,
`:163`) and git query (`server/git_query.go:66`), so the hook does not go
there. Exec gets its own splice, as forwards got `spliceBidiCounted`.

## Why the server must parse frames (D7, D8)

The data stream does not carry the child's bytes. It carries the objtrsf exec
frame protocol (`objtrsf/exec/frame/frame.go:40`): a `FrameHeader{type :u8,
len :u32}` followed by `len` payload bytes, with stdin, stdout, stderr,
control and synth frames interleaved on one stream. There is no sync marker.

Forward tap hands the tap the relayed chunk as-is. Doing that here breaks in
two cases:

- A tap opened mid-exec starts at an arbitrary byte, usually inside a frame,
  and the reader cannot find the next header.
- A `gap` drops whole chunks. After it, the reader's idea of where the next
  header starts is wrong, and stays wrong for the rest of the stream.

So the frame boundaries are tracked where every byte is seen — the splice —
and from the first byte of the exec, which is why the scanner runs whether or
not a tap is attached. Records then carry payload by channel, and a reader
never sees a header. The cost on the relay goroutine is a five-byte state
machine per frame and a length subtraction per chunk; the payload itself is not
inspected.

Direction is not named separately: stdin only ever flows client → runner and
stdout/stderr only runner → client, so the channel already says it.

**Synth (D9).** The client's demux sends Synth frames to the same destination
as Stdout (`objtrsf/exec/exec_stream.go:68`). The tap counts and renders them
as stdout so its stdout stream matches the bytes the exec's client wrote out.

**Control and unknown types (D10).** Showing a control frame means decoding
objtrsf's `Control` union, which would have to be restated in this repo's
`.bgn` (`feedback_decode_foreign_wire_in_bgn`) as a second copy of objtrsf's
schema. The exec path that P1 is about does not send them: the gateway's
`runExec` sets no window size and sends no signal. The scanner skips their
payload by length and does not count it.

## Shape

```
harness-cli exec ls [--task T] [--json]
  #4  a1b2c3d4  27s  tui ws:…-ab  powershell -c …
      stdin=1.2kB  stdout=48.3MB  stderr=0  last=2s ago  taps=1

harness-cli exec tap <exec-id> [--chan all|stdin|stdout|stderr]
                               [--max-bytes N]
                               [--hex | --text | --raw | --json]
```

```
 ssh client ⇄ gateway (TUI) ══ trsf ══▶ harness-server ══ trsf ══▶ agent-runner ⇄ child
                                              │
                                      spliceExecCounted
                                              ├── frame scanner (per direction, always on)
                                              ├── atomic counters   → ExecRunInfo / exec_stats
                                              └── tap fan-out       → ExecTapRecord stream
                                                  (bounded queue, non-blocking)
```

## Wire

All of it, in one place — this is the authoritative interface
(`feedback_no_split_schemas`). Added to `runner/protocol/message.bgn`.

```
enum TaskControlKind:      # client ↔ server
    …
    open_exec_tap          # appended: attach a tap to one exec; returns the
                           # stream its records arrive on

enum Capability:
    …
    # exec_tap authorizes READING THE PAYLOAD of an out-of-band exec — its
    # stdin as well as its output. Through the ssh gateway that is whatever
    # the remote tool speaks, including anything typed at a password prompt.
    # A different power from running an exec or viewing a session, so neither
    # exec_run nor exec_view implies it.
    exec_tap       = 0x40000, "exec_tap"
    all            = 0x7ffff, "all"

enum StatusEventKind:
    …
    exec_stats             # appended: an exec's COUNTERS moved. Same reason
                           # forward_stats exists, one object over.

# --- counters: appended to the existing listing row ---
format ExecRunInfo:
    …                          # unchanged through origin_cid
    stdin_bytes           :u64 # payload bytes since the exec started; frame
    stdout_bytes          :u64 # headers are not counted, Synth counts as
    stderr_bytes          :u64 # stdout, control frames count nowhere
    last_activity_unix_ms :u64 # 0 = no payload byte has ever crossed
    taps                  :u16 # how many taps are open on this exec RIGHT NOW

# --- the tap ---
enum ExecTapFilter:
    :u8
    all
    stdin
    stdout
    stderr

format OpenExecTapRequest:
    exec_id          :u64
    channel_filter   :ExecTapFilter
    # 0 = whole payload. Otherwise each record's data is cut to this many
    # bytes and truncated_bytes reports what was cut from THAT record.
    max_record_bytes :u32

enum OpenExecTapStatus:
    :u8
    ok
    no_such_exec      # unknown id, not visible to the caller, OR outside its
                      # scope — deliberately one answer, so the id space is not
                      # an existence oracle
    internal_error
    # No `denied`: a capability failure is answered before dispatch with a
    # PermissionDenied response, so nothing could set it here.

format OpenExecTapResponse:
    status    :OpenExecTapStatus
    stream_id :u64

enum ExecTapChannel:
    :u8
    stdin
    stdout            # includes Synth frames
    stderr

enum ExecTapRecordKind:
    :u8
    data
    gap               # the tap could not keep up; dropped_bytes were missed
    eof               # a zero-length frame on this channel: for stdin, the
                      # client closed the child's stdin
    exec_ended        # the exec left the registry; the stream EOFs after this

format ExecTapData:
    channel         :ExecTapChannel
    # Where this payload sits in its channel, counted by the SERVER from the
    # exec's first byte. A tap opened mid-exec starts at a non-zero offset, and
    # the value lines up with the *_bytes counter on the listing.
    stream_offset   :u64
    # Bytes cut by max_record_bytes. The next record's stream_offset counts
    # them, so a cut never reads as a shorter stream.
    truncated_bytes :u32
    data_len        :u32
    data            :[data_len]u8

format ExecTapGap:
    channel       :ExecTapChannel
    dropped_bytes :u64

format ExecTapEof:
    channel :ExecTapChannel

format ExecTapExecEnded:
    kind      :ExecEventKind  # exited | killed | failed — as ExecEvent
    exit_code :i32            # the child's own for exited, -1 otherwise

format ExecTapRecord:
    # One per relayed payload piece, on the relay goroutine — the same reason
    # ForwardTapRecord is noheap.
    config.go.union = "noheap"
    kind    :ExecTapRecordKind
    unix_ms :u64
    match kind:
        ExecTapRecordKind.data       => data       :ExecTapData
        ExecTapRecordKind.gap        => gap        :ExecTapGap
        ExecTapRecordKind.eof        => eof        :ExecTapEof
        ExecTapRecordKind.exec_ended => exec_ended :ExecTapExecEnded
        .. => error("Unexpected exec tap record")
```

Records travel on the tap stream concatenated, with no length prefix of their
own — each is self-delimiting under its schema, as `streamTapSink`
(`server/forward_tap_handler.go:18`) already writes forward records.

Two existing comments state the old design and are rewritten in the same
change, because this spec reverses them on purpose:
`message.bgn` at `exec_ended` ("Execs have no counters, so they need no stats
kind") and at `ExecStatusEvent` ("Two kinds, not three"), and `topics/topics.go:17`
("minus the stats kind").

**The runner wire is unchanged.** Server and clients move together; runners do
not need a restart. `ExecRunInfo` is fixed-layout and grows at the end, so a
skewed server/client pair fails at decode — `exec ls` and `execs.status` are
where it shows — rather than misreading a value. It is not persisted:
`execRun` lives only in memory (`server/exec_registry.go:20`), so there is no
disk axis (`project_wal_persists_wire_bytes_schema_skew`).

The capability number IS persisted, in task records. A task already granted
`all` holds `0x3ffff` and does not gain `exec_tap` until it is re-granted.
Operator connections are unaffected: `callerCaps`
(`server/capabilities.go:108`) hands them `Capability_All`, which is the
widened literal.

## Server behaviour

### The splice

`spliceExecCounted(client, runner, e *execRun)` replaces the call at
`server/exec_run.go:108`. Teardown stays half-close, for the reason the comment
there gives: a command that finishes in milliseconds must not have the client's
data stream torn down before the client has resolved it by id.

Each direction runs `relayBytes`' loop with a `frameScanner` attached:

- client → runner: Stdin frames are payload on `stdin`; Control frames are
  skipped.
- runner → client: Stdout and Synth on `stdout`, Stderr on `stderr`.
- either direction: unknown types are skipped by length.

The scanner keeps `{header [5]byte, have int, remaining uint32, channel}` and
walks each chunk without copying it. For every payload run inside a chunk it
calls `e.notePayload(channel, run)`, which adds to that channel's counter,
stores `last_activity`, and offers the run to each tap with the counter's value
BEFORE the add as `stream_offset`. A zero-length frame calls
`e.noteEOF(channel)`. A frame split across chunks yields several runs at
consecutive offsets; a reader that needs frame boundaries does not need them,
because records carry payload, not frames.

The scanner never fails. Any type byte and any length are legal, so there is no
parse error to handle on the relay; the bytes are forwarded unchanged whatever
the scanner concluded.

### Counters

On `execRun`: `stdinBytes`, `stdoutBytes`, `stderrBytes` as `atomic.Uint64`,
`lastActivityMs` as `atomic.Int64`, and `lastPublished` + its mutex for the
sweep. `execRunInfo` (`server/exec_run.go:269`) fills the five new fields.

`runForwardStatsSweeper` (`server/forward_events.go:71`, started at
`server/server.go:894`) also sweeps execs on the same tick, emitting
`exec_stats` for each exec whose counters or tap count changed since the last
publish. One goroutine, as now; an idle exec costs one comparison. It is
renamed `runStatsSweeper`, since it no longer sweeps only forwards.

`execs.status` delivery keeps its filter, `execVisibleTo`.

### The tap (D11, D12)

`forwardTap` (`server/forward_tap.go`) is the bounded queue, the per-stream
missed count, the gap-before-next-record flush and the reaper. Copying it for
exec would put the gap logic in two places. It becomes a generic
`recordTap[K comparable, R any]` in `server/record_tap.go`, parameterised by:

- `keyOf(R) K` — which stream a record belongs to
- `gapFor(K, missed uint64) R` — the gap record for that stream

and `forwardTap` / `execTap` become thin users of it. Each keeps its own
filter and truncation, because those differ in the type they filter on.

**Offsets come from the caller (D12).** `forwardTap.offer` currently counts
`stream_offset` in the TAP's own per-stream state (`forward_tap.go:110-115`),
so a tap opened mid-connection starts at 0. The forward spec says the opposite
— "counted by the SERVER from that connection's first byte. A tap opened
mid-connection therefore starts at a non-zero offset" (2026-08-29 spec,
§ Wire, `ForwardTapData.stream_offset`) — and the existing tests only cover
taps open from the first byte, where the two agree. The generic tap therefore
takes the offset as an argument; exec passes its channel counter, and forward
passes the connection's `connBytes` half before the add (`forward_counters.go:143-152`
already updates it before offering). Forward taps opened mid-connection change
from 0-based to connection-based offsets, which is what their spec promised.

**The last record ends the tap (D15).** The forward spec says the stream
"EOFs after" `forward_closed`, but nothing ends `forwardTap.run` today:
`closeTaps` (`server/forward_tap.go:314`) queues the record and the tap keeps
waiting, so `forward tap` stays open until the operator interrupts it. The
generic tap gets a `finish(final)` that is not a queue push: `run` delivers
what is queued, any outstanding gaps, then the final record, and returns — the
handler then closes the reader's stream. Exec uses it for `exec_ended`; forward
uses it for `forward_closed`. Going around the queue matters because the end is
the record a fallen-behind reader needs most, and a queue push is exactly what
gets dropped when it is full. A tap attached to an exec that has already ended
is finished at once with the stored outcome, so a handler that looked the exec
up an instant before its removal does not leave a tap waiting forever.

The overflow rule is unchanged from forward tap: a tap that cannot keep up
accumulates `missed` per stream and stays attached; the writer emits a `gap`
before that stream's next record. `eof` and `exec_ended` records carry no bytes
and, like forward's brackets, are dropped without a gap when the queue is full.

### Ending (D13)

`removeExec` (`server/forward_events.go:57`) is the single funnel for dropping
a registration, with four callers. It gains the outcome:

```go
func (h *TaskHandler) removeExec(execID uint64, kind protocol.ExecEventKind, exitCode int32) (*execRun, bool)
```

| Caller | Outcome passed |
| --- | --- |
| send-to-runner failure rollback (`exec_run.go:90`) | `failed`, -1 |
| `onExecRunFinished` (`exec_run.go:160`) | the runner's `fin.Kind`, `fin.ExitCode` |
| `handleExecRunKill` (`exec_run.go:305`) | `killed`, -1 |
| `DropExecRunsForConn` (`exec_run.go:358`) | `killed`, -1 |

An argument rather than a second call each site must remember: a caller that
omits it is a compile error. `removeExec` emits `exec_ended` to every tap after
the registration is gone, then the taps' streams EOF.

### The handler

`handleOpenExecTap(conn, req, connID)` beside `handleOpenForwardTap`, same
three gates in the same order:

1. `requiredCap[OpenExecTap] = Capability_ExecTap` — direction-independent, so
   it belongs in the pre-dispatch map (`server/capabilities.go:37`). A failure
   is a `PermissionDenied` naming `exec_tap`; the handler is never entered.
2. Visibility — `execVisibleTo(connID, e)`. Invisible answers `no_such_exec`.
3. Scope — `h.inScope(connID, Capability_ExecTap, e.taskIDHex)`. Out of scope
   answers `no_such_exec`, after visibility so a scope refusal does not leak
   existence.

It then creates a bidirectional stream, attaches the tap, runs it, and watches
the stream for the tapper going away with the same read-until-EOF goroutine
forward tap needed (`forward_tap_handler.go:87-99`) — without it a tap closed
on a quiet exec is never reaped and `taps=` keeps counting it.

`TestEveryCapabilityDeclaresHowItsTargetIsResolved` and
`TestCapabilityTargetClassesMatchTheSource` will fail until `exec_tap` is
classified. That is expected; they are watched going red before the
classification lands.

## Rendering

The server formats nothing. One renderer in `cli/`, used by all three surfaces
— the WebUI reaches it through the wasm bridge (`surface-parity-checklist`
item 32).

One header line per record, then the body for `data`. ASCII, fixed columns:

```
stdin         12:34:56.789  64B
0000  7b 22 6a 73 6f 6e 72 70  63 22 3a 22 32 2e 30 22  |{"jsonrpc":"2.0"|
stdout        12:34:56.902  1.4kB  (truncated, 1.3kB cut)
0000  …
stderr  gap   12:34:57.100  3.2MB missed
stdin   eof   12:34:57.500
-- exec #4 ended: exited 0 --
```

| Column | Content |
| --- | --- |
| 1 | channel, `stdin` / `stdout` / `stderr`, fixed width; empty on the closing line |
| 2 | `gap` / `eof`, blank for data |
| 3 | wall clock from `unix_ms`, `HH:MM:SS.mmm` |
| 4 | payload size for data, the missed count for `gap` |

The hex body's offset column is `stream_offset` from the wire, as in forward
tap, so it lines up with the listing's counters.

Four modes, matching forward tap:

- `--hex` (default) — the above.
- `--text` — same headers, body with non-printables as `.`, no offset column.
  A JSON-RPC-over-stdio tool is the motivating case and is readable this way.
- `--raw` — payload only, no headers. Requires `--chan` naming one channel:
  three channels concatenated on one stdout is not a stream a decoder can read.
- `--json` — JSON Lines, one object per record, a struct (stable field order),
  `data` base64.

```
{"kind":"data","unix_ms":1760...,"chan":"stdin","offset":0,"len":64,"truncated_bytes":0,"data":"eyJqc29ucnBjIjoiMi4wIi..."}
{"kind":"gap","unix_ms":1760...,"chan":"stderr","dropped_bytes":3355443}
{"kind":"eof","unix_ms":1760...,"chan":"stdin"}
{"kind":"exec_ended","unix_ms":1760...,"result":"exited","exit_code":0}
```

`hexDumpLines`, `printableASCII` and the timestamp / byte-count helpers in
`cli/forward_tap_render.go` are shared, not copied; only the header line and
the JSON struct are exec-specific.

The flag is `--chan`, not forward tap's `--dir`: forward selects a direction of
travel, this selects one of three channels, and two of the channels travel the
same way.

## Capability and scope

Two checks in the handler beyond the cap bit, both described above. As with
forward tap, there is no operator branch: operator connections pass because
`callerCaps` gives them the full mask, so **`exec_tap` bounds agents; it does
not bound the human.**

`taps=N` on the listing is the counterweight available: an exec being read is
visible on a surface its owner already looks at.

`cli/caps.go` gains `exec_tap` in `GrantableCaps` and `CapDescription`, worded
to say it includes stdin and ssh-gateway traffic.

## Surfaces

| Surface | What |
| --- | --- |
| CLI rows | a second line under each `exec ls` row: `stdin= stdout= stderr= last= taps=` — `cli.ExecRunTrafficLine`, the sibling of `PortForwardTrafficLine` (`cli/port_forward_list.go:201`) |
| CLI JSON | the five fields on the `exec ls --json` object |
| CLI verb | `exec tap <id>` with `--chan` / `--max-bytes` / `--hex` / `--text` / `--raw` / `--json`, declared once in `cli/verb/table.go` |
| CLI caps catalog | `exec_tap` in `GrantableCaps` + `CapDescription` |
| TUI execs modal | columns `stdin` `stdout` `stderr` `last` `taps`; `ApplyEvent` upserts on `exec_started` / `exec_stats`, drops on `exec_ended` |
| TUI tap | `t` on the execs modal (`modalKeys.ExecTap = "t"`, same letter and reasoning as `ForwardTap`) opens the tap view |
| TUI tap view | `ForwardTapView` (`tui/forwardtap.go`) is a line viewport with no forward-specific logic; it becomes `TapView` and both taps use it |
| TUI cmdline | `exec tap` |
| WebUI row | traffic line under each exec row in `renderExecList` (`webui/static/main.js:1195`) |
| WebUI tap | a per-row button and panel; `toggleForwardTap` (`main.js:1136`) is generalised and shared |
| WebUI command input | `exec tap` |
| wasm | `execTap` bridge beside `forwardTap`; the five fields in the snapshot's exec map (`cmd/harness-webui-wasm/main.go:1350`) as raw numbers plus the rendered line |
| README | the exec section and the TUI verb list |

The WebUI stays on its 5s snapshot poll and does not subscribe to
`execs.status`, as forward tap's Amendment 2 decided for forwards: the wasm
bridge has no pubsub machinery, and the gap it would close is latency, not
correctness.

Zero is printed, never elided (`stderr=0`); `last=never` until the first
payload byte (`feedback_never_elide_zero_in_operator_output`).

The `surface-parity-checklist` skill is walked item by item while writing the
plan, and this table is checked back against the code when the feature is done
(item 39).

## Testing

Unit, server:

- the frame scanner, fed one recorded stream split at EVERY byte position, and
  at random positions, produces the same per-channel runs, offsets and
  counters as the unsplit stream
- Synth counts as stdout; a zero-length frame yields `eof`; Control and
  unknown-type payloads are skipped and counted nowhere
- a tap opened after N stdout bytes receives a first `data` with
  `stream_offset == N`
- a tap whose consumer never reads gets `gap` records with non-zero
  `dropped_bytes`; the relay is not blocked; the tap is still attached
- `max_record_bytes` truncates, reports `truncated_bytes`, and the next offset
  counts the cut bytes
- gates: no cap → `PermissionDenied` naming `exec_tap`, handler not entered;
  invisible, out-of-scope and unknown ids → `no_such_exec`, no stream created
- each of the four `removeExec` callers delivers its outcome as `exec_ended`
- `exec_stats` fires when counters or `taps` change and not otherwise
- forward tap: the existing tests pass unchanged on the generic tap, plus a new
  one — a tap opened mid-connection starts at that connection's byte count
  (D12)
- the capability completeness guards are seen failing before `exec_tap` is
  classified

Unit, cli / tui:

- renderer golden lines for every record kind in every mode; `--raw` without a
  single `--chan` is refused
- the `t` key dispatch on the execs modal; `ApplyEvent` on `exec_stats`; the
  forward tap view unchanged after becoming `TapView`

Live, against `scripts/dummy-harness.sh`, in the spelling the help text prints
(Pitfall 13 — the argv, keystroke and click paths are not reached by Go tests):

1. `harness-cli exec <task> -- cat` with stdin piped in; from a SECOND client,
   `exec tap <id>`. See `stdin` and `stdout` records, the counters on `exec ls`,
   `stdin eof` when the input ends, and `exec #N ended: exited 0`.
2. P1's own path: the gateway started from the TUI, `ssh … <task>@host cat`
   through it, tapped from the CLI.
3. A task holding `exec_run` but not `exec_tap` is refused on the same exec
   that the task succeeds on once granted `exec_tap`.
4. TUI: `t` pressed on the execs modal opens the tap; closing it returns the
   row to `taps=0`.
5. WebUI at desktop width and at 390px through Playwright: traffic line, tap
   panel, and the command input's `exec tap`. Screenshots are kept and their
   paths reported.

Before landing: `scripts/wire-skew-check.sh` (it is a no-op for runners here,
and is run anyway), and the `make` targets rather than an ad-hoc `go build`.

Restart for the operator: the server, and every client — **including the TUI**,
because the gateway runs inside it. Runners are not affected.

## Non-goals

These are v1 boundaries chosen while writing, not refusals:

- **No recording.** Nothing before the tap opens is retained.
- **Control frames are not shown** (D10). A signal or resize sent to an exec
  does not appear in the tap.
- **No reassembly of the tool's protocol.** `--text` and `--raw` are the
  escape hatches; a JSON-RPC framer is the tool's business.
- **`session exec` is not covered.** It types into a live PTY session and its
  bytes go through the session mux, which already has `session snapshot` and a
  view attach.
- **The WebUI does not subscribe to `exec_stats`** — see § Surfaces.

## Amendment — what shipped, 2026-10-09

Commits `9d290f18..4510cf9a` (plan: `docs/superpowers/plans/2026-10-09-exec-tap.md`).

### Where the shipped code differs from the text above

- **The TUI honours the render mode.** The spec listed the four modes as CLI
  flags and said nothing per surface. What shipped: the TUI's `exec tap` renders
  `--hex` / `--text` / `--json` as the CLI prints them (`--text` is the mode a
  stdio protocol is read in), and `--raw` is declared CLI-only — the TUI view
  and the browser panel are lines, and payload with no framing is a stdout
  stream. The WebUI takes none of the four, as for forward tap. Why: the verb
  table's consumer check (`TestEverySurfaceReadsEveryActionField`) found the
  TUI dropping `Mode`, and a typed option must take effect or be refused. The
  mode-name mapping moved from `cmd/harness-cli` to `cli.TapModeByName` so both
  surfaces use one. Forward tap's TUI still ignores its modes under a
  `surfaceLocal` exemption; that is unchanged here.
- **`frameHeaderSize` is the existing constant.** § The splice described the
  scanner's header as five bytes; `server/session_mux.go` already declares
  `frameHeaderSize = 5`, so the scanner uses it, and
  `TestFrameHeaderSizeMatchesTheSchema` pins it to `FrameHeader`'s encoder.
- **The client's tap reader hands over a chunk's records together**
  (`cli/tap_stream.go`), one callback per chunk rather than per record, which is
  what `StreamForwardTap` already did: a UI that redraws per callback redraws
  once per burst.
- **No `stdout eof` in practice.** The runner does not send a
  zero-length stdout frame before the child exits, so a tap on a finishing
  exec shows `stdin eof` (when the client closes stdin) and then `exec_ended`;
  `stdout eof` appears only if a runner sends one. Nothing depends on it:
  `exec_ended` is the end signal.
- § Capability and scope names `cli/caps.go`; the catalog lives in
  `cli/verb/caps.go`.
- The last-kind sentinel test is now `TestOpenExecTapIsStillTheLastKind`, and
  `open_exec_tap` is classified in `cap_completeness_test.go` as well as the two
  scope tables § The handler named. All three guards were watched failing on
  `open_exec_tap` / `exec_tap` before the classification landed.

### § Surfaces, checked against the code

| Surface | Shipped |
| --- | --- |
| CLI rows | `cli.ExecRunTrafficLine`, second line under each `exec ls` row |
| CLI JSON | `stdin_bytes`, `stdout_bytes`, `stderr_bytes`, `last_activity_unix_ms`, `taps` on `ExecRunInfoJSONLine` |
| CLI verb | `exec tap` row in `cli/verb/table.go`; `--raw` with `--chan all` refused in `Validate` |
| CLI caps catalog | `exec_tap` in `GrantableCaps` (after `exec_run`) and `CapDescription` |
| TUI execs modal | ten columns incl. `stdin` `stdout` `stderr` `last` `taps`; `ApplyEvent` upserts on started/stats |
| TUI tap | `modalKeys.ExecTap = "t"` in `inExecsModal` |
| TUI tap view | `tui/tapview.go:TapView`, `tapSubject{Kind, ID}`; forward tap uses it too |
| TUI cmdline | `exec tap` (CLI \| TUI \| WebUI on the row) |
| WebUI row | traffic line in `renderExecList` |
| WebUI tap | `tap` button per row; `toggleTapPanel` shared by `toggleForwardTap` / `toggleExecTap`; the panel survives the snapshot poll |
| WebUI command input | `exec tap` via `WebUIDispatch{Cache: "lastExecs"}`; `exec ls` prints the traffic line |
| wasm | `harness.execTap`; `cli.ExecSnapshotRow` carries the five fields raw plus `traffic`; `execRunList` rows carry `traffic` |
| README | summary, the exec section, the list-view paragraph, the TUI verb list, the capability list |

### Live verification (dummy harness, `scripts/dummy-harness.sh up --agent fake`)

Against a `session new -d --agent bash` task, in the spellings the help prints:

1. `harness-cli exec <task> -- cat < fifo`, then from another client
   `harness-cli exec ls` (traffic line, `taps=0`) and `harness-cli exec tap <id>`
   (`taps=1`). One JSON line written: a `stdin` and a `stdout` record of 25B, both
   at offset 0; `exec ls` then read `stdin=25B stdout=25B … taps=1`. Closing the
   FIFO produced `stdin  eof`, then `-- exec #1 ended: exited 0 --`, and
   `exec tap` exited 0 on its own.
2. `ssh-gateway start 127.0.0.1:<port>` in the TUI's command line, then
   `ssh -p <port> <task>@127.0.0.1 cat` — the exec listed with origin `Tui` —
   tapped from the CLI with `--text`: an LSP-style `Content-Length` frame showed
   on stdin and stdout, then `stdin eof`, `exec ended`, and the tap exited.
3. `submit --agent bash --caps exec_run --scope ids:<task>` running
   `harness-cli exec tap <id>`: `permission denied: OpenExecTap requires
   capability exec_tap`. The same with `--caps exec_run,exec_tap` on the same
   exec: the stdin and stdout records arrived.
4. TUI: `e`, `t` on the row → `tap on exec #4` over the modal, `taps=1` on
   `exec ls`; Esc → back on the modal, the row read `taps=0` with the new
   counters.
5. WebUI (Playwright, 1280px and 390px): traffic line under the row, `tap` →
   panel with the stdin/stdout records, still attached after a snapshot poll;
   no horizontal page overflow at 390px. From the command input,
   `exec tap <id>` toggled the panel off and `exec tap <id> --chan stdin` on
   again, after which a written line showed only on stdin, at offset `0x18` —
   the channel's byte count, not the tap's.

`make test`, `make check`, `make wasm-check` and `scripts/wire-skew-check.sh`
passed; the skew check reported both directions handshake-compatible.
