# `forward tap` — seeing what a port forward carries — design

## Problem

A port forward is the one operator-visible object whose behaviour cannot be
observed at all. `forward ls` reports the SPEC and, since `04fd771`, WHICH
client holds it — bind address, target, direction, origin kind and cid. It
reports nothing about the forward as a running thing:

- **P1.** "Is this forward doing anything, or is it an abandoned registration?"
  has no answer. There is no byte count, no connection count, no last-activity
  time. An idle forward and a saturated one render identically.
- **P2.** "The service behind this forward is misbehaving — what is actually
  going over it?" has no answer either. The bytes cross the server in the clear
  and are dropped on the floor.
- **P3.** The two are asked at different moments and want different answers.
  P1 is a glance at a list; P2 is a decision to watch one forward for a while.

Everything else in this repo already has its observation surface. A session has
`session snapshot`, an attach and a task log. An exec has `exec ls` and its two
streams. A forward has a row that describes how it was configured.

`relayBytes` (`server/task_handler.go:1540`) is where the bytes go past, and it
copies them and forgets them. `PortForwardInfo` (`runner/protocol/message.bgn:1883`)
has no field that changes after registration.

## Decisions taken

Every line here is decided. The third column records who decided: **operator**
means the human chose it in conversation, **this spec** means the author chose
it while writing — those are the rows worth a second look.

| # | Decision | Decided by |
| --- | --- | --- |
| D1 | Both halves ship: counters on the listing (A) **and** a live content tap (B) | operator |
| D2 | The tap lives at the server's splice, one place, not at each client | this spec |
| D3 | A dedicated capability gates the content tap — `forward_local` does not imply it | operator |
| D4 | No recording. A tap sees bytes from the moment it opens; the server buffers nothing and writes nothing | operator |
| D5 | Counters are always-on (atomic adds, no storage) — they are the answer to P1, which is a glance, not a subscription | this spec |
| D6 | `OpenPortForwardRequest` gains `forward_id`, so a data stream names its registration instead of being matched by heuristic. An id that names nothing — 0 included — is refused, never relayed uncounted | this spec |
| D7 | A tap that cannot keep up gets a `gap` record and stays alive; it is never dropped | operator (confirmed) |
| D8 | Directions are named `to_target` / `from_target`, never tx/rx | this spec |
| D9 | Every record carries a `conn_seq` — one forward multiplexes many TCP connections | this spec |
| D10 | `forward ls` reports `taps=N`, so a forward cannot be watched invisibly | this spec |
| D11 | New capability `forward_tap = 0x10000`, and `all` widens `0xffff` → `0x1ffff` | this spec |
| D12 | CLI, TUI and WebUI all get both halves in v1 | operator |

## Why the tap is at the server (D2)

Both port-forward data planes cross one function on the server:

- local — `server/port_forward.go:65`, `go spliceBidi(clientStream, runnerStream, taskIDHex)`, one splice per accepted connection.
- remote — `server/port_forward.go:252`, the same call on the conn-notify path.

`spliceBidi` (the tear-down-on-either-close variant) has exactly those two call
sites; file transfer, git query and exec all use `spliceBidiHalfClose`
instead. So a hook there reaches every forward and nothing else.

The client side would need the same hook in four places and would still miss
forwards held by other clients: `RunForward` (`cli/port_forward.go:307`),
`OpenRawForward` (`cli/forward_endpoint.go:58`) and the wasm preview
(`cli/preview_forward_wasm.go:139`) each splice their own end, and the WebUI's
splice runs inside the browser. That is the shape `feedback_enumerate_all_callsites_when_intercepting`
names: an interceptor with partial wiring.

The bytes are in the clear at that point. `peer.Conn` encryption is per hop, so
the server holds plaintext of whatever the forwarded protocol is — which is the
capability this feature exposes, and the reason for D3.

## Why a data stream must name its forward (D6)

`OpenPortForwardRequest` (`message.bgn:1940`) carries `task_id`, `remote_host`
and `remote_port`. The forward id is minted by a different RPC,
`RegisterPortForward`, so the server cannot attribute a splice's bytes to a
registration. Two forwards with an identical spec from the same client are
indistinguishable — which is the exact ambiguity `origin_cid` was added to
resolve for the listing, one commit ago.

Matching on `(cid, task, host, port)` is refused: it is a payload convention
standing in for a field, which `feedback_protocol_explicit_over_convention`
rules out, and it is wrong in the case above rather than merely inelegant.

So `forward_id :u64` joins the request. Consequence, stated because it is a
real edit and not a formality: **`OpenRawForward` must register before it
opens.** It currently opens the data stream (`cli/forward_endpoint.go:58`) and
registers afterwards (`:62`), so the id does not exist when the data stream is
created. Swapping the two is a few lines; the failure arm changes from "close
the data stream" to "close the control stream", and the brief window where a
registration exists with no data stream behind it is a state `RunForward`
already produces (it registers at listener-bind time, `cli/port_forward.go:229`,
and opens per accepted connection).

There is exactly one caller that opens a data stream without registering one of
its own, and it is deliberate: `PreviewPinFetch`
(`cli/preview_forward_wasm.go:123`) opens a fresh stream per HTTP request while
the PIN holds the registration. Per-fetch registrations lived tens of
milliseconds against a 5s poll, so nothing the page did ever appeared in
`forward ls`
(`docs/superpowers/specs/2026-08-12-preview-pin-forward-registration-design.md`).

That stays as it is, and still gets attributed: the pin's id is already in
`slot.forwardID`, so the fetch passes it and its bytes land on the pin's row —
which is the row the pin was invented to be. WebUI preview traffic is counted
and tappable without a row per request.

**`forward_id = 0` is refused, not relayed uncounted.** It has no legitimate
sender. A client older than this change does not send a zero — it sends a
message eight bytes short, `OpenPortForwardRequest.Read` fails on `io.ReadFull`,
and the server drops the request with a decode error
(`server/task_handler.go:247`); version skew is loud on its own. The only way
to reach a zero is a Go zero-valued struct inside this repository, i.e. a call
site that forgot the field.

Accepting that would produce a data stream that no listing shows, no counter
counts and no `forward kill` can name — the exact state `OpenRawForward`'s own
doc says the registry exists to prevent (`cli/forward_endpoint.go:36-38`), and
a silently-ignored typed field, which `surface-parity-checklist` item 33 rules
out. So the open answers `no_such_forward` and the caller learns immediately.
A non-zero id that is not in the registry gets the same answer, which also
covers the real race: a forward killed between its registration and an open
underneath it.

## Shape

```
harness-cli forward ls [--json]
  #7  -L  a1b2c3d4  127.0.0.1:8080 -> localhost:3000  cli ws:…-ab
      conns=3/41  to-target=1.2MB  from-target=48.3MB  last=2s  taps=1

harness-cli forward tap <forward-id> [--dir to-target|from-target|both]
                                     [--max-bytes N]
                                     [--hex | --text | --raw | --json]
```

```
   accepted TCP conn                                        runner dials
   ────────────────▶ client ══ trsf ══▶ harness-server ══ trsf ══▶ agent-runner
                                            │
                                       spliceBidi
                                            ├── atomic counters  → PortForwardInfo
                                            └── tap fan-out      → ForwardTapRecord stream
                                                (bounded queue, non-blocking)
```

## Wire

All of it, in one place — this is the authoritative interface
(`feedback_no_split_schemas`). Added to `runner/protocol/message.bgn`.

```
enum TaskControlKind:      # client ↔ server
    …
    open_forward_tap       # returns the stream the records arrive on

enum Capability:
    …
    # forward_tap authorizes READING THE PAYLOAD of a port forward — the
    # cleartext of whatever protocol it carries, which routinely means an
    # Authorization header, a git credential or a database password. That is
    # a different power from holding a forward, so forward_local does not
    # imply it and neither does forward_remote.
    forward_tap    = 0x10000, "forward_tap"
    all            = 0x1ffff, "all"
```

`all` is a literal that widens by hand, and widening it here is **required, not
conventional**: `callerCaps` (`server/capabilities.go:100`) hands operator
connections `Capability_All`, so an un-widened literal would deny the operator
its own feature. Tasks are unaffected in the other direction — the WAL persists
the number, so a task already granted `all` holds `0xffff` and does not gain
`forward_tap` until it is re-granted.

```
# --- (A) counters: appended to the existing listing row ---
format PortForwardInfo:
    …                          # unchanged through client_endpoint
    bytes_to_target       :u64
    bytes_from_target     :u64
    conns_total           :u64  # accepted since registration
    conns_open            :u32
    taps                  :u16  # how many taps are open on this forward RIGHT NOW
    last_activity_unix_ms :u64  # 0 = no byte has ever crossed

# --- (A) attribution: the data stream names its registration (D6) ---
format OpenPortForwardRequest:
    task_id         :TaskID
    remote_host_len :u16
    remote_host     :[remote_host_len]u8
    remote_port     :u16
    # The registration these bytes belong to. Appended, so the field order the
    # existing three keep is unchanged. There is no "unattributed" value: 0 is
    # refused like any other id that resolves to nothing.
    forward_id      :u64

enum OpenPortForwardStatus:
    …
    no_such_forward   # forward_id names no live registration (0 included)

# --- (B) the tap ---
format OpenForwardTapRequest:
    forward_id       :u64
    direction_filter :ForwardTapFilter
    # 0 = whole payload. Otherwise each record's data is cut to this many
    # bytes and truncated_bytes reports what was cut from THAT record — a
    # protocol-identification tap wants 64 bytes per record, not 64 KiB.
    max_record_bytes :u32

enum ForwardTapFilter:
    :u8
    both
    to_target
    from_target

enum OpenForwardTapStatus:
    :u8
    ok
    no_such_forward   # unknown id OR not visible to the caller — the same
                      # deliberate conflation KillPortForward already makes,
                      # so an invisible id is not an existence oracle
    internal_error
    # No `denied` member: a capability failure never reaches this handler.
    # requiredCap is checked before dispatch and answers with a different
    # response KIND (PermissionDenied), so a status here could not be set by
    # anything.

format OpenForwardTapResponse:
    status    :OpenForwardTapStatus
    stream_id :u64

# Server → client, on that stream, until the forward ends or the client stops
# reading. Direction is named against the forward's own target_host:target_port,
# so it means the same thing for -L and -R; tx/rx would invert between them
# because the connection initiator is on the other side.
enum ForwardTapDirection:
    :u8
    to_target
    from_target

enum ForwardTapRecordKind:
    :u8
    conn_open       # a connection was accepted; data records follow
    data
    gap             # the tap could not keep up; dropped_bytes were missed
    conn_close      # that connection ended, the forward is still up
    forward_closed  # the forward itself ended; the stream EOFs after this

format ForwardTapConnOpen:
    conn_seq        :u64   # per-forward, 1-based, assigned at accept
    target_host_len :u16
    target_host     :[target_host_len]u8
    target_port     :u16

format ForwardTapData:
    conn_seq      :u64
    direction     :ForwardTapDirection
    # Where this payload sits in its (conn_seq, direction) stream, counted by
    # the SERVER from that connection's first byte. A tap opened mid-connection
    # therefore starts at a non-zero offset — it says so rather than pretending
    # the stream began when the tap did — and the jump across a gap needs no
    # arithmetic to see.
    stream_offset :u64
    # Bytes cut by max_record_bytes. The NEXT record's stream_offset counts
    # them, so a cut never reads as a shorter stream.
    truncated_bytes :u32
    data_len        :u32
    data            :[data_len]u8

format ForwardTapGap:
    conn_seq      :u64
    direction     :ForwardTapDirection
    dropped_bytes :u64

format ForwardTapConnClose:
    conn_seq          :u64
    bytes_to_target   :u64   # what this ONE connection carried, each way
    bytes_from_target :u64

format ForwardTapForwardClosed:
    reason :PortForwardCloseReason

format ForwardTapRecord:
    # One of these exists per relayed chunk, on the relay goroutine that must
    # never be slowed (§ Server behaviour). The default union storage boxes the
    # arm in an interface and type-asserts on every access
    # (runner/protocol/message.go:23564) — a heap allocation per chunk on the
    # hottest path this feature has.
    config.go.union = "noheap"
    kind    :ForwardTapRecordKind
    unix_ms :u64
    match kind:
        ForwardTapRecordKind.conn_open      => conn_open      :ForwardTapConnOpen
        ForwardTapRecordKind.data           => data           :ForwardTapData
        ForwardTapRecordKind.gap            => gap            :ForwardTapGap
        ForwardTapRecordKind.conn_close     => conn_close     :ForwardTapConnClose
        ForwardTapRecordKind.forward_closed => forward_closed :ForwardTapForwardClosed
        .. => error("Unexpected forward tap record")
```

The arms are not decoration. A flat record would have to carry
`dropped_bytes`, `truncated_bytes`, a direction and a close reason on every
record, meaningless on four kinds out of five — bytes the schema would be
describing as present and the reader would have to know to ignore
(`feedback_no_schema_invisible_bytes`). `PortForwardEvent`
(`runner/protocol/message.bgn:1931`), the control-stream record for this same
feature, is already this shape, down to the `.. => error(…)` arm that makes an
unknown kind from a newer peer fail closed.

`conn_seq` repeats in four arms rather than sitting above the match, because
`forward_closed` is about the forward and has no connection. Hoisting it would
need the record to say "0 means not-a-connection", or an `if kind != …` — and
nothing in any `.bgn` in this tree conditions on anything but `==`, so the
design does not lean on an operator it has not seen used. Four declared lines
cost nothing on the wire; a sentinel costs a rule every reader has to remember.

The runner wire is **unchanged**. `RunnerOpenPortForwardRequest` does not learn
the forward id, because the runner dials and relays and has nothing to attribute.
Restart order therefore excludes runners: server and clients move together,
runners do not need to (`project_wire_change_kills_runners_server_first` applies
to `RunnerHello`/PSK, not here).

Both formats that grow are fixed-layout, so a skewed pair fails at decode rather
than misreading a value, in both directions. A new server reading an old
client's `OpenPortForwardRequest` runs out of bytes in `io.ReadFull` and drops
the request with `TaskHandler: failed to decode TaskControlRequest`
(`server/task_handler.go:247`) — the client's round-trip then times out. An old
client reading a new server's longer `PortForwardInfo` rows fails the same way,
so `forward ls` is the surface that reports the skew. Neither direction
degrades quietly, which is why D6 does not need a compatibility value.

## Server behaviour

The registry entry (`server/port_forward_registry.go`) gains, per forward:

```
bytesToTarget, bytesFromTarget  atomic.Uint64
connsTotal                      atomic.Uint64
connsOpen                       atomic.Int32
lastActivityMs                  atomic.Int64
nextConnSeq                     atomic.Uint64
taps                            []*forwardTap   // under the registry mutex
```

`handleOpenPortForward` looks the forward up by the new `forward_id` and hands
the entry to the splice. There is no nil-entry path: an id that resolves to
nothing answers `no_such_forward` before any stream is created, so every splice
has a registration behind it. `conn_seq` is
taken from `nextConnSeq` there, and `connsTotal` / `connsOpen` move around the
splice call rather than inside `relayBytes`, which never learns a connection
started or ended.

The `-R` path needs no id lookup and no schema change: `handleRemoteForwardConn`
(`server/port_forward.go:223`, splicing at `:252`) already holds the `pf`, so it
passes the same entry and assigns the same `conn_seq`. Both paths converge on
one tapped splice; there is not a local variant and a remote variant.

The observer runs on the relay goroutine and must never block it — the same
constraint the session fan-out has, and the same shape it uses
(`server/session_mux.go:378-402`, `:502`): a bounded channel per consumer and a
non-blocking send.

It diverges in what happens on overflow (D7). `SessionMux` drops the VIEWER
(`dropViewerLocked`); a tap instead accumulates the missed byte count and stays.
The writer goroutine swaps that counter to zero before each write and emits a
`gap` record first when it was non-zero. The reason is the operator's, not
symmetry: a tap is open because something is already wrong, and a tap that
vanishes reads as "the forward closed" — a false fact about the thing under
investigation. A gap record is a true one.

Payload buffers are copied into the tap queue. `relayBytes` passes the slice it
just read straight to `AppendData`, and the tap must not hold a reference into
a buffer the relay is about to reuse — the same rule `spliceConnStream` already
documents at `cli/port_forward.go:143`. With `max_record_bytes` set, the copy is
of the truncated length, so a narrow tap on a fast forward is cheap.

## Rendering

**The server formats nothing.** It emits `ForwardTapRecord` — bytes plus the
metadata only the crossing point can know: `conn_seq`, direction, `unix_ms`,
`dropped_bytes`. Every line below is produced by the `cli/` package in the
CLIENT process, which for the WebUI is the same Go compiled to wasm and running
in the browser. `--json` is therefore a re-encode of the record, not a re-parse
of text.

The one exception, and it is deliberate: `max_record_bytes` truncation happens
server-side, along with the `truncated_bytes` count. Cutting the payload at the
client would first spend the bandwidth the option exists to save. That is the
only place the server touches the payload at all.

A tap record has to be readable on a Windows console, in a bubbles viewport and
in a `<pre>`, so the line is ASCII and fixed-column. One header line per record,
then the body for `data`:

```
#3 open   12:34:56.700  localhost:3000
#3 ->     12:34:56.789  64B
0000  47 45 54 20 2f 61 70 69  2f 78 20 48 54 54 50 2f  |GET /api/x HTTP/|
0010  31 2e 31 0d 0a 48 6f 73  74 3a 20 6c 6f 63 61 6c  |1.1..Host: local|
#3 <-     12:34:56.902  1.4kB  (truncated, 1.3kB cut)
0000  48 54 54 50 2f 31 2e 31  20 32 30 30 20 4f 4b 0d  |HTTP/1.1 200 OK.|
#3 gap    12:34:57.100  3.2MB missed
#3 close  12:34:57.900  ->4.1kB <-1.2MB
-- forward #7 closed (killed) --
```

| Column | Content |
| --- | --- |
| 1 | `#<conn_seq>`, or nothing on the forward-level closing line |
| 2 | kind, left-aligned in a fixed width: `->` / `<-` for data, `open` / `close` / `gap` |
| 3 | wall clock from `unix_ms`, `HH:MM:SS.mmm` |
| 4 | payload size for data, the missed count for `gap`, the dialled target for `open`, that connection's two totals for `close` |

`->` / `<-` rather than arrows or `to_target`/`from_target`: `forward ls`
already writes its row as `127.0.0.1:8080 -> localhost:3000`, so an arrow
pointing right already means "toward the target" on the surface next to this
one, and both survive a non-UTF-8 console.

The hex body is `xxd` layout — offset, 16 bytes in two groups of eight, ASCII
gutter. The offset column is the record's `stream_offset` straight off the wire,
not a count the renderer keeps: record boundaries are an artifact of `ReadDirect`
chunking (§ Wire), so a per-record offset would restart at 0 mid-message, and a
renderer-side counter would start at 0 when the TAP opened rather than when the
connection did. The server's number lines up with `bytes_to_target` on the
listing and makes the jump across a `gap` self-evident.

Four renderings, one decision each:

- `--hex` (default) — the above.
- `--text` — the same header lines, body as the payload with non-printables as
  `.` and no offset column. HTTP over a forward is the likeliest use and a
  hexdump of it is unreadable.
- `--raw` — payload bytes only, no headers, for piping into a decoder.
  Requires `--dir`: two directions concatenated into one stdout is not a
  stream any decoder can read.
- `--json` — JSON Lines, one object per record, matching what `ls --json` and
  `forward ls --json` already are. A struct, not a map, so field order is
  stable (`PortForwardInfoJSONLine`'s reason). `data` is base64, being bytes.

```
{"kind":"conn_open","unix_ms":1756...,"conn":3,"target":"localhost:3000"}
{"kind":"data","unix_ms":1756...,"conn":3,"dir":"to_target","offset":0,"len":64,"truncated_bytes":0,"data":"R0VUIC9hcGkveCBIVFRQLzEuMQ0K"}
{"kind":"gap","unix_ms":1756...,"conn":3,"dir":"to_target","dropped_bytes":3355443}
{"kind":"conn_close","unix_ms":1756...,"conn":3,"bytes_to_target":4198,"bytes_from_target":1258291}
{"kind":"forward_closed","unix_ms":1756...,"reason":"killed"}
```

**One renderer in `cli/`, called by all three surfaces** (`surface-parity-checklist`
item 32). The TUI view and the WebUI panel display the same string the CLI
prints; the browser reaches it over the wasm bridge rather than re-deriving the
format in JS, which is the mistake `scopeSpecJS` made and paid for.

`formatByteCount` (`tui/rawforward.go:333`) is the existing renderer for the
size column and moves to `cli/` in this change, because the new counters put
byte sizes on all three surfaces and a second copy in JS would drift the same
way.

## Capability and scope

Two gates, both required:

1. `forward_tap` — in the `requiredCap` map (`server/capabilities.go:37`), which
   is a plain `hasCap` check run before dispatch (`server/task_handler.go:253`).
   It belongs there, and not inline like `KillPortForward`'s, precisely because
   it is direction-INDEPENDENT: reading the payload of a `-R` forward is the
   same power as reading a `-L` one, so the cap is known without the registry
   lookup. `KillPortForward` is inline because its cap is not.
2. Visibility — the forward must be in `visiblePortForwards(connID, …)`
   (`server/port_forward_list.go:66`). An invisible id answers `no_such_forward`,
   the same answer a genuinely unknown one gets, so the two cannot be
   differenced into an existence oracle.

The two gates report through different channels, and that is the existing
design rather than a choice made here. A cap failure is answered by
`denyTaskControl` (`server/task_handler.go:224`) with a `PermissionDenied`
response naming the missing bit — the handler is never entered — and
`RoundTripTaskControl` turns it into a `CapabilityError` at a single point
(`cli/client.go:228`), so `forward tap` inherits the message every capped verb
already prints. Only the visibility failure travels as a status on the tap's own
response.

What this does and does not confine, stated plainly because it was the first
thing to be got wrong in conversation: there is no operator branch in any gate.
Every check is `hasCap(h.callerCaps(cid), want)`, and `callerCaps` returns
`Capability_All` for a connection with no principal task. Operator connections
therefore pass because their mask is full, not because they skip the check —
and which connections may be operator is decided elsewhere, by `OperatorPSK`
(`server/server.go:57`). **`forward_tap` bounds agents; it does not bound the
human.**

`taps=N` on the listing (D10) is the counterweight that is actually available:
a forward being read is a fact its holder can see, on a surface they already
look at.

## Surfaces

| Surface | What |
| --- | --- |
| CLI rows | six new fields on the `forward ls` row (`cli/port_forward_list.go` renderer) |
| CLI JSON | the same six on `portForwardJSON` (`cli/port_forward_list.go:151`) |
| CLI verb | `forward tap <id>` with `--dir` / `--max-bytes` / `--hex` / `--text` / `--raw` / `--json` (§ Rendering) |
| CLI caps catalog | `forward_tap` in `cli/caps.go` `GrantableCaps` + `WriteCaps` |
| TUI forwards pane | new columns on the `f` pane; the column set and the rows swap together (`applyColumns`), never one then the other |
| TUI tap | its own view, the way the exec listing got one — not a dump into the cmdline result region |
| TUI keys | a key on the forwards pane, in `mainKeyMap` **and** its `mainKeyBindings` row |
| WebUI row | new cells in `renderForwardList` (`webui/static/main.js:953`) |
| WebUI tap | a per-row control opening a hexdump panel, fed by a wasm hook like `raw_forward_wasm`'s |
| wasm snapshot | the six fields in the forwards map (`cmd/harness-webui-wasm/main.go:945`) as raw numbers, plus a `forwardTap` bridge |
| README | the port-forward section and the TUI verb list |

Zero is printed, never elided: a forward that has carried nothing shows
`conns=0/0 to-target=0 from-target=0 taps=0`, because 0 is a measurement and
blank is not (`surface-parity-checklist` item 31). `last=` is the one field that
renders as `never` rather than a number, because it has no measurement to
report until the first byte.

The line format, its four renderings and the shared Go renderer are specified in
§ Rendering.

The `surface-parity-checklist` skill is walked item by item during plan writing,
and this table is checked back against the code when the feature is finished
(item 39).

## Testing

Unit, server:

- both counters advance, and advance on the correct field for `-L` and for `-R`
  — the direction naming is the decision most likely to be implemented backwards
- `conns_open` returns to zero when a connection ends, `conns_total` does not
- a tap whose consumer never reads gets `gap` records with a non-zero
  `dropped_bytes`, the relay is not blocked, and the tap is still open afterwards
- an invisible forward id answers `no_such_forward`; a caller without the cap
  gets a `PermissionDenied` response naming `forward_tap`, and the tap handler
  is never entered
- `max_record_bytes` cuts the payload and reports `truncated_bytes`
- two taps on one forward both receive, and `taps` reports 2
- an open with `forward_id = 0`, and one naming a forward killed a moment
  earlier, both answer `no_such_forward` and create no stream

E2E against a dummy harness (`scripts/dummy-harness.sh`): `-L` to a local echo
server, drive known bytes through it, and confirm `forward ls` reports them and
`forward tap` shows them — a client's usage, not the canonical one
(`feedback_test_the_callers_usage_not_the_canonical_one`).

## Non-goals

These are v1 boundaries chosen while writing, not refusals:

- **No recording.** Nothing is written server-side and no history exists before
  a tap opens. A tap on a live connection starts mid-stream, and says so by
  never emitting a `conn_open` for it.
- **No decryption.** A forward carrying TLS or SSH yields ciphertext. The
  feature is "the payload as it crosses the server", which is plaintext only
  when the forwarded protocol is.
- **No pcap or session-reassembly output.** `--raw` piped into an existing
  decoder is the escape hatch.
- **Traffic outside a forward stream is not covered** — notably the sandbox
  connect-proxy, which is not a port forward.
- **There is no unattributed forward.** Every data stream names a registration
  or is refused (D6), so "a forward that exists but is not counted" is not a
  state this design has.

---

## Amendment — what shipped, 2026-08-29

Recorded so the spec never contradicts the behaviour a later reader verifies
against.

**The scope gate was missing from this spec.** § Capability and scope named two
gates — the capability and visibility. The server has a third, and the
repository's own guard said so: `TestEveryCapabilityDeclaresHowItsTargetIsResolved`
went red until `forward_tap` was classified, because a forward VISIBLE through a
global visibility rank may still belong to a task outside the caller's ACTION
scope. That is the distinction `kill_port_forward` already draws
(`server/task_handler.go:541-552`). The handler therefore calls
`h.inScope(connID, Capability_ForwardTap, pf.taskIDHex)` after the visibility
check — `inScope` rather than `authorize`, because `authorize`'s `hasCap` half
has already run in the pre-dispatch `requiredCap` gate. An out-of-scope forward
answers `no_such_forward`, like the other two refusals.

**`spliceBidi` is gone, not extended.** Both port-forward call sites moved to
`spliceBidiCounted` (`server/forward_splice.go`), which left the original with
no callers; it is deleted rather than kept as an unused near-duplicate. Its log
line still said `OpenInteractive: splice ended`, from a caller it had not had
for some time.

**Task boundaries merged during execution.** The plan's Tasks 1, 2, 5 and 6 
landed as one commit, and 3 and 4 as parts of it. The repository's completeness
guards made them inseparable: `TestPortForwardInfoMapsEveryField` fails the
moment a field is added to the wire struct, so the schema could not land before
the counters that fill it, and `TestCapabilityTargetClassesMatchTheSource` fails
until a handler passes the new bit by name, so the classification could not land
before the handler. That is the guards working as intended, not a planning miss
— but the plan drew boundaries the tree does not allow.

**`conn_close` carries per-connection totals** (`bytes_to_target` /
`bytes_from_target`), added while implementing: the forward's running totals
move under every other connection at once, so a close record built from them
would report the wrong thing. `connBytes` keeps the halves per connection and
`closeConn` releases the entry, so a long-lived forward does not grow a map
entry per connection forever.

**`max_record_bytes` does not shorten the stream.** `stream_offset` advances by
what CROSSED, not by what was kept, so a truncated tap's offsets still line up
with `bytes_to_target` on the listing. Pinned by
`TestTapTruncatesAndKeepsTheOffsetHonest`.

### Surfaces table, checked against the code

| Row | Verdict |
| --- | --- |
| CLI rows | **done** — `PortForwardTrafficLine`, on a second line rather than five more columns |
| CLI JSON | **done** — six fields on `portForwardJSON` |
| CLI verb | **done** — `forward tap` with `--dir` / `--max-bytes` / `--hex` / `--text` / `--raw` / `--json` |
| CLI caps catalog | **done** — `GrantableCaps` + `CapDescription` |
| TUI forwards pane | **done** — four columns, before `origin`, so the column set never varies |
| TUI tap | **done** — `ForwardTapView`, header outside the viewport |
| TUI keys | **done** — `modalKeys.ForwardTap` = `t`; the `f` row's help text names it. Not in `mainKeyBindings`, which lists MAIN keys — this is a modal key, like `ForwardKill` |
| WebUI row | **done** — traffic line under the row |
| WebUI tap | **done** — per-row button and panel, plus the command input's `forward tap` |
| wasm snapshot | **done** — `cli.ForwardSnapshotRow`, raw numbers plus the rendered line |
| README | **done** |

### Verified live, 2026-08-29

Against `scripts/dummy-harness.sh` with a local HTTP target behind a `-L`
forward:

- `forward ls` on an untouched forward: `conns=0/0  to-target=0  from-target=0
  last=never  taps=0` — every zero printed.
- After one request: `conns=0/1  to-target=91B  from-target=160B  last=2s ago
  taps=1`. The two directions are counted separately and correctly (91 bytes of
  request toward the target, 160 of response back).
- `forward tap` printed `conn open` / `->` / `<-` / `conn close ->91B <-160B`
  with the xxd body; `--json`, `--text` and the `--dir` filter all behave as
  specified, and `--raw` without `--dir` is refused.
- WebUI at desktop and at 390px: the row's traffic line and the tap panel render,
  the panel scrolls inside itself, the page body does not scroll sideways, and
  the panel survives the 5s snapshot poll that rebuilds the rows around it. Both
  entry points work — the row's button and the command input's `forward tap`.

Not verified live: the capability refusal for a confined AGENT. `harness-cli`
from inside an `exec`-spawned child of the dummy could not authenticate at all
(`psk: server rejected: BadTicket`, which `whoami` reproduces), so the failure
is upstream of this feature and unrelated to it. The gate has direct unit
coverage instead — the `requiredCap` entry, the invisible-forward refusal and
the out-of-action-scope refusal.
