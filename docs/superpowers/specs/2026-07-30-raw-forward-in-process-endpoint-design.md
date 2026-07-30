# Port forwarding with an in-process endpoint (raw connect / `-W`)

**Date:** 2026-07-30
**Status:** Approved (brainstorming)

## Problem

The WebUI cannot port-forward at all. `2026-06-06-remote-port-forwarding-design.md:199`
declares it out of scope — *"WebUI (wasm) surface — CLI + TUI only for now."* —
and the reason is structural, not effort: `-L` needs `net.Listen`
(`cli/port_forward.go:202`) and `-R` needs `net.Dial`
(`cli/port_forward.go:538`), and a browser can do neither. The WebUI can
already *see* forwards (registry list at `cmd/harness-webui-wasm/main.go:539`,
topology edges at `webui/static/main.js:4061`) and kill them
(`main.go:109`), but cannot create one.

Verbatim: *"今ってさwebuiはportforwarding使えませんけどもあれって理論上端っこの
端点持ってお好き勝手できなくはないじゃないですかと思うんですが"*

That observation is correct, and the reason it is correct is narrow enough to
state exactly: of the two socket operations a forward needs, **the client-side
one is the only one the browser lacks**. The runner still dials. So a forward
whose client-side endpoint lives inside the client process, rather than being a
bound socket, needs no socket capability on the client at all.

This is not a new invention: `ssh -W host:port` is exactly that — a `-L` whose
client endpoint is the process's stdin/stdout instead of a listener. This design
adds that endpoint kind to the harness, which makes the WebUI a viable forward
client as a consequence.

## What already exists

The per-connection data path already works with **no registration at all**:

- `handleOpenPortForward` (`server/port_forward.go:22`) checks exactly two
  things — task is `Running` or `Detached`, runner is online — then creates a
  client stream and a runner stream and splices them. There is no registry
  lookup.
- The `RunnerOpenPortForwardRequest` it sends (`server/port_forward.go:49-57`)
  carries task_id / stream_id / direction / remote_host / remote_port. It has
  **no `forward_id` field**, so there is no way for the runner to consult a
  registration even in principle.
- The runner's `handleOpenPortForward` (`runner/port_forward.go:74`) waits for
  the named stream, dials, splices.
- `2026-07-30-port-forward-registry-design.md` confirms this historically: before
  the registry existed, *"the server only ever sees a per-connection
  `OpenPortForward` request at the moment a connection is accepted, so an idle
  `-L` forward is invisible server-side"* — i.e. `-L` worked with zero
  registrations. Its decision 5 states the current meaning: *"`OpenPortForward`
  is reduced to its per-connection meaning."*

So `cli.Client.OpenPortForward` (`cli/port_forward.go:62`) is a task-control
round trip plus `WaitForBidirectionalStream` and touches no socket. **It already
compiles and works under `js/wasm`.**

Registration is bookkeeping for list / kill / topology, and its local path never
contacts the runner:

- `handleRegisterPortForward` (`server/port_forward.go:75`) for a non-remote
  direction creates a control stream, `add()`s the entry, and starts
  `watchRemoteForwardControl` (`:273`, which despite the name runs for both
  directions and deregisters on control-stream EOF). Nothing else.
- Kill pushes a `Closed` record onto the control stream
  (`pushPortForwardClosed`, `:244`); the client's `serveLocalForwardControl`
  (`cli/port_forward.go:244`) returns when it sees one.

Capabilities need no change: `OpenPortForward` is unconditionally
`Capability_ForwardLocal` and `RegisterPortForward` is direction-dependent
(`server/capabilities.go:10-16`), so a `direction=local` registration requires
`ForwardLocal` — the same cap the operator surfaces already hold.

The wasm side has a directly reusable precedent for holding a stream on behalf
of a UI pane: `previewSlots` / `previewGen` in `cli/preview_wasm.go` — keyed
slots plus a monotonic generation reserved *before* the RPC, so a pane closed
while the open is still in flight discards the stream instead of installing an
orphan (stop-wins).

## Decisions

1. **`direction` is not extended.** A raw connect has the runner dialling, which
   is `direction=local` exactly. What differs is whether the client-side
   endpoint is an OS socket or lives inside the client process. Folding that into
   `direction` would destroy `direction`'s meaning ("which side listens").
   A separate `client_endpoint` field carries it.
2. **Whichever address pair describes the client side goes empty when the
   endpoint is in-process.** Which pair that is depends on direction, because the
   two pairs swap roles: for `local` the client owns `bind_addr` / `bind_port`
   (its listener, bound at `cli/port_forward.go:202`) and the runner owns
   `target_host` / `target_port` (its dial target); for `remote` it is the
   reverse — the runner listens on `bind_addr` / `bind_port` and the client dials
   `target_host` / `target_port` (`dialForwardTarget`, `:538`). This design
   defines only `local` × `in_process`, so `bind_addr` / `bind_port` are sent
   empty / 0 while `target_host` / `target_port` carry the runner's dial target
   as usual. Nothing invents a placeholder address to fill the empty pair.
3. **`direction=remote` × `client_endpoint=in_process` is rejected server-side**
   with `internal_error`. That combination is the follow-up design (browser as a
   service endpoint the runner dials into); refusing it now keeps an
   unimplemented combination from half-working.
4. **Every raw connection registers**, upholding the registry spec's decision 4
   ("a forward that is running but not listed must not exist"). Registration
   happens *after* the data stream is established — the analogue of "register
   after a successful local bind" — and registration failure closes the data
   stream.
5. **One registration per connection.** There is no listener to represent, so
   there is nothing longer-lived than a connection to register.
6. **The `DIR` column keeps `-L` / `-R`.** Direction is still the axis it
   displays. The endpoint kind shows up in the `SPEC` column, which renders
   `(in-process) -> host:port` in place of a bind address. Which surface owns it is
   already answerable from the existing `ORIGIN` column (`origin_kind` + cid),
   so nothing about the owning process is encoded into the spec string.
7. **The CLI flag is `-W host:port`**, matching SSH, where `-W` is this exact
   mechanism. `-W` is unused in the `forward` subcommand today
   (`cmd/harness-cli/main.go:521-522` defines only `-L` / `-R`).
8. **Raw bytes only.** No HTTP request builder, no response formatting, no TLS.
   A protocol-aware view is a separate layer on top of the same stream, and the
   HTML-rendering use case belongs to the preview path
   (`2026-06-21-webui-html-rendered-preview-design.md`).

## Architecture

### Schema delta

All of it, in one place. `runner/protocol/message.bgn`, regenerated to
`message.go` via `make protoregen`.

New enum, next to `PortForwardDirection` (`message.bgn:1192`):

```
enum ClientEndpointKind:
    :u8
    os_socket   # client-side endpoint is an OS socket: the listener for -L, the dial for -R.
    in_process  # client-side endpoint lives inside the client process (stdio for -W, a UI
                # pane in TUI/WebUI). The address pair describing the client side is therefore
                # empty: bind_addr / bind_port for a local forward, target_host / target_port
                # for a remote one. Only local x in_process is defined today (decision 3).
```

One field appended to each of two client↔server formats:

```
format RegisterPortForwardRequest:      # message.bgn:1236
    task_id         :TaskID
    direction       :PortForwardDirection
    bind_addr_len   :u16
    bind_addr       :[bind_addr_len]u8
    bind_port       :u16
    target_host_len :u16
    target_host     :[target_host_len]u8
    target_port     :u16
    client_endpoint :ClientEndpointKind   # NEW

format PortForwardInfo:                  # message.bgn:1261
    forward_id      :u64
    direction       :PortForwardDirection
    task_id         :TaskID
    bind_addr_len   :u16
    bind_addr       :[bind_addr_len]u8
    bind_port       :u16
    target_host_len :u16
    target_host     :[target_host_len]u8
    target_port     :u16
    origin_kind     :ClientKind
    origin_cid_len  :u8
    origin_cid      :[origin_cid_len]u8
    client_endpoint :ClientEndpointKind   # NEW
```

`PortForwardDirection` is untouched, and so is `RunnerOpenPortForwardRequest`
(`message.bgn:1336`). **No runner-facing message changes**, because a raw
connection reaches the runner as an ordinary per-connection
`direction=local` open. The runner's wire interpretation is unchanged.

### Server

`server/port_forward.go`:

- `portForward` gains `clientEndpoint protocol.ClientEndpointKind`, populated in
  `handleRegisterPortForward` (`:75`) alongside the existing fields.
- Before the direction branch: `direction == Remote && clientEndpoint ==
  InProcess` → `internal_error` (decision 3).
- Otherwise the existing local path is reused **verbatim** — control stream,
  `add()`, `watchRemoteForwardControl`. Kill, teardown, and deregister-on-EOF
  need no changes.
- `portForwardInfo` construction copies `clientEndpoint` into `PortForwardInfo`.

`cli/port_forward_list.go`:

- `PortForwardSpecString` (`:112`): when `ClientEndpoint == InProcess`, the listen
  side renders as `(in-process)` instead of `bind_addr:bind_port`. Remote keeps its
  `runner:` prefix.
- `PortForwardDirFlag` (`:121`) is unchanged (decision 6).

Routing the endpoint kind through the spec string rather than a new display field
means **no diagram or panel change is needed**: the WebUI list rows
(`webui/static/main.js:495`) and the topology edge label (`:4148`, which renders
`${fwd.dir} ${fwd.spec}`) already consume `spec` verbatim, and the wasm snapshot
already supplies it via `cli.PortForwardSpecString`
(`cmd/harness-webui-wasm/main.go:550`). A `bind_port` of 0 therefore never
reaches a label as `:0`.

### Client shared layer

New `cli/forward_endpoint.go`, platform-independent:

```go
// RawConn is one forward whose client-side endpoint is this process.
type RawConn struct {
    data      trsf.BidirectionalStream
    ctrl      trsf.BidirectionalStream
    forwardID uint64
}

func OpenRawForward(ctx context.Context, c *Client, taskIDHex, host string, port int) (*RawConn, error)
func (r *RawConn) Send(b []byte) error
func (r *RawConn) Close() error
```

`OpenRawForward`:

1. `c.OpenPortForward(ctx, taskIDHex, host, port)` → data stream.
2. `c.RegisterPortForward(ctx, taskIDHex, Local, "", 0, host, port, InProcess)` →
   control stream + forward id. (`RegisterPortForward`'s signature gains the
   endpoint-kind argument; the two existing call sites pass `OsSocket`.)
3. Registration failure closes the data stream and returns the error
   (decision 4). The two existing callers of `RegisterPortForward` —
   `cli/port_forward.go:210` (`-L`) and `:425` (`-R`) — pass `OsSocket`.
4. A goroutine runs `serveLocalForwardControl` on the control stream; when it
   returns (a `Closed` record from a remote kill, or EOF from a dead server) it
   closes the data stream, which is what makes `forward kill` reach a raw
   connection.

Reads are consumer-shaped, so `RawConn` exposes the data stream rather than
owning a read loop: the CLI splices it to stdout, the wasm layer pumps it to a JS
hook.

### CLI: `forward <task> -W host:port`

`cmd/harness-cli/main.go` (`case "forward"`, `:471`) gains `-W`, which is
**mutually exclusive with `-L` / `-R`** and not repeatable. A process has one
stdin/stdout pair; `-W` owns the foreground and exits with its peer, whereas
`-L` / `-R` are long-lived listeners whose lifetime is the operator's Ctrl-C.
`ssh -W` draws the same line for the same reason — it implies
`ClearAllForwardings`. Combining them is rejected with exit status 2.

The splice is `spliceConnStream`'s shape (`cli/port_forward.go:106`) against
stdin/stdout instead of a `net.Conn`; the existing helper is `net.Conn`-typed, so
the stdio direction gets a thin adapter rather than a copy of the pump.

Composition this buys, and the reason `-W` is worth having beyond being a test
vehicle:

```
ssh -o ProxyCommand="harness-cli forward <task> -W %h:%p" user@host
```

reaches a host only the runner can see, without picking a local port.

### WebUI: raw connect pane

`cli/raw_forward_wasm.go` holds keyed slots with a generation reserved before
the RPC, mirroring `previewSlots` / `previewGen` (`cli/preview_wasm.go`) for the
same reason: a pane closed while the open is in flight must discard the stream,
not install an orphan.

Bindings on `window.harness` (`cmd/harness-webui-wasm/main.go`):

| Binding | Meaning |
| --- | --- |
| `rawOpen(taskIDHex, host, port) -> Promise<key>` | open + register; resolves with the slot key |
| `rawSend(key, Uint8Array) -> Promise<void>` | append to the data stream |
| `rawClose(key) -> Promise<void>` | close both streams; deregisters via control EOF |

JS hooks, matching the preview hook convention (paneKey first):
`harness_rawData(key, Uint8Array)`, `harness_rawClosed(key, reason)`.

UI (`webui/static/main.js`, `webui/static/style.css`):

- Task selector + host + port + Connect; several connections held as tabs, each
  its own slot key.
- Output view is a `<pre>`, **not the shared xterm**. The shared xterm
  (interactive / preview) assumes VT-interpretable text and a single writer;
  raw bytes break both.
- `text | hex` toggle on the output; send box with a newline selector
  (`CRLF | LF | none`) and a hex-input mode.
- Received bytes are capped as a ring (256 KiB), the same reasoning as the
  preview replay cap.
- Dark palette (`#1e1e1e` / `#d4d4d4`) and the ≤600px layout from the first cut.

### TUI

`tui/portforward.go`'s `ForwardsModal` gains an entry point that opens a raw
connection (task already selected in the app), with a single-line send input and
a scrollable output viewport. Text view only — the hex toggle stays a WebUI
affordance.

## Failure modes

| Situation | Behaviour |
| --- | --- |
| Target refuses / times out | The runner's dial fails; the data stream closes. The pane shows the close and the registration is dropped by control-stream teardown. |
| Registration fails after the data stream opened | Data stream closed, error surfaced. No unlisted live forward (decision 4). |
| `forward kill <id>` from another surface | `Closed` on the control stream → the client closes the data stream → pane/`-W` process ends. |
| Client vanishes (tab closed, SIGKILL) | Control stream EOF → `watchRemoteForwardControl` deregisters. Same path as `-L` today. |
| Task exits while connected | Streams close as they do for any per-connection forward; registration drops on control EOF. |
| `remote` × `in_process` requested | `internal_error` (decision 3). |

## Verification

- **Protocol**: round-trip tests for the new field on both formats
  (`runner/protocol/port_forward_test.go`).
- **Server**: `remote` × `in_process` rejected; a `local` × `in_process` registration
  lands in the registry and appears in a list result with the field set
  (`server/port_forward_test.go`).
- **Client**: registration failure closes the data stream; a `Closed` record on
  the control stream closes the data stream (`cli/`).
- **Integration**: `integration/port_forward_test.go` — raw connect to an echo
  listener, bytes round-trip, the forward appears in `forward ls` with
  `(in-process)`, `forward kill` ends it.
- **Dummy harness E2E**: a dummy server + runner started from this checkout's own
  `bin/`, driving `harness-cli forward <task> -W 127.0.0.1:<echo port>` and
  confirming a real byte round-trip through real binaries. Unit and integration
  tests do not cover the built-binary path.
- **WebUI**: Playwright at desktop and 390px — connect to an echo listener, type
  real keystrokes into the send box, confirm the echoed bytes appear in the
  output view, confirm the hex toggle, confirm the forward shows up in the
  existing list panel and as a topology edge, confirm closing the pane removes
  it. Rendering alone is not proof; the input path is what gets asserted.
- `make check`, `make wasm-check`, vet, test.

## Rollout

Server and all client surfaces must be updated together — both changed formats
are client↔server. **Runner binaries are unaffected** by the wire change (no
runner-facing message changed), so the fleet does not need the
server-first-then-runners dance that wire changes normally force.

## Security / trade-offs

- A raw connection reaches exactly what `-L` already reaches — any `host:port`
  the runner can dial, unsandboxed, in plaintext through the server — and needs
  the same `Capability_ForwardLocal`. No new authority is created.
- What does change is that a **permanent UI** for "send arbitrary bytes to
  arbitrary `host:port`" exists in the WebUI. An XSS in the WebUI becomes a
  proxy into the runner's network without needing to construct anything. This
  sits inside the README's stated toy scope (trusted hub, unsandboxed dialling),
  but it widens the consequence of a WebUI compromise and is recorded here
  deliberately rather than discovered later.
- Short-lived registrations will make `forward ls` noisier during
  poke-and-close use. The entry disappears when the pane closes, so the cost is
  transient listing noise, not leakage.

## Out of scope

- **Browser as a service endpoint** (`direction=remote` × `in_process`: the runner
  listens and an in-browser endpoint answers — "ask the operator over HTTP",
  file-pick from a phone, artifact sink). Follow-up design; it lands on the same
  `client_endpoint` axis, which is why the axis is a field rather than a third
  direction value.
- Task-to-task splicing through a client. Bytes would cross the server twice and
  be bounded by wasm throughput; if it is wanted, it belongs server-side.
- HTTP-aware request/response UI, TLS termination, protocol decoders.
- Rendering a runner-side web app in an iframe (server-side HTTP proxy path).
- Creating a forward by dragging on the topology diagram.
