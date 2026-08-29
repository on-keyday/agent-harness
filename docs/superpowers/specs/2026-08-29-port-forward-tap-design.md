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
| D6 | `OpenPortForwardRequest` gains `forward_id`, so a data stream names its registration instead of being matched by heuristic | this spec |
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

`forward_id = 0` means "no registration" and is accepted: an open that never
registers is counted nowhere and cannot be tapped. Nothing in the tree is known
to do this today, but the field must have a defined zero, and a peer older than
this change sends one.

## Shape

```
harness-cli forward ls [--json]
  #7  -L  a1b2c3d4  127.0.0.1:8080 -> localhost:3000  cli ws:…-ab
      conns=3/41  to-target=1.2MB  from-target=48.3MB  last=2s  taps=1

harness-cli forward tap <forward-id> [--dir to-target|from-target|both]
                                     [--max-bytes N] [--hex | --raw]
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
    denied            # capability check failed
    internal_error

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

format ForwardTapRecord:
    kind            :ForwardTapRecordKind
    direction       :ForwardTapDirection      # to_target on non-data kinds
    conn_seq        :u64   # per-forward, 1-based, assigned at accept
    unix_ms         :u64
    dropped_bytes   :u64   # gap only, else 0
    truncated_bytes :u32   # data only: cut by max_record_bytes, else 0
    close_reason    :PortForwardCloseReason   # forward_closed only
    data_len        :u32
    data            :[data_len]u8
```

The runner wire is **unchanged**. `RunnerOpenPortForwardRequest` does not learn
the forward id, because the runner dials and relays and has nothing to attribute.
Restart order therefore excludes runners: server and clients move together,
runners do not need to (`project_wire_change_kills_runners_server_first` applies
to `RunnerHello`/PSK, not here). An old client against a new server misparses
the longer `PortForwardInfo`, so `forward ls` is the surface that breaks if the
pair is skewed.

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
the entry to the splice; a zero id or an unknown one relays with a nil entry, so
an unattributable stream still works and is simply not counted. `conn_seq` is
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
   never `denied`, so the two cannot be differenced into an existence oracle.

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
| CLI verb | `forward tap <id>` with `--dir` / `--max-bytes` / `--hex` / `--raw` |
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

`--raw` requires an explicit `--dir`: writing both directions' payloads to one
stdout interleaves two conversations into a byte soup that no decoder can read.
The default is `--hex`, one framed record per header line.

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
- an invisible forward id answers `no_such_forward`; a visible one without the
  cap answers `denied`
- `max_record_bytes` cuts the payload and reports `truncated_bytes`
- two taps on one forward both receive, and `taps` reports 2

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
- **No tap on an unregistered open** (`forward_id = 0`): there is no id to name
  it by.
