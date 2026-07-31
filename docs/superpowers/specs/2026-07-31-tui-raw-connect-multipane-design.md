# TUI raw connect: tabbed multi-pane modal

Spec A of two. Spec B (`2026-07-31-http-request-builder-design.md`) adds an
HTTP request form as a mode inside the pane view this spec defines, and depends
on it.

## Problem

The WebUI holds many raw-forward panes at once (`rawPanes`, keyed by pane, with
a tab strip). The TUI holds exactly one: `RawConnectModal` owns a single
`conn *cli.RawConn`, and `App` carries a single `rawGen` guarding its messages.
Three consequences the operator meets:

- Only one target can be open. Watching two ports means closing one.
- `esc` closes the modal AND the connection — there is no "hide this, keep it
  running". The forward is deregistered server-side, so it also vanishes from
  `forward ls`.
- Behaviour differs from the WebUI for the same feature, so instructions,
  habits, and bug reports do not transfer between the two surfaces.

## What already exists

- `tui/rawforward.go` — `RawConnectModal`: one `textinput`, dual purpose (target
  before connect, bytes to send after), `out []byte` ring, `conn`, `cancel`,
  and the `connecting` flag that caps in-flight connect attempts at one.
- `tui/app.go` — `rawGen uint64`; every `RawForward{Opened,Data,Closed}Msg`
  carries a `Gen` and is dropped when it does not match. `Open` bumps the gen;
  `esc` bumps it before closing so late replies from the closed session cannot
  be applied.
- `DoStartRawForward(c, taskID, host, port, gen, program)` — opens on the
  long-lived client and pumps received bytes back as gen-tagged messages.
- `cli.RawConn` — `Send`, `Recv`, `Close`, `ForwardID`. Closing deregisters the
  forward, which is why the pump closes on the way out.
- WebUI equivalents: `rawPanes` / `rawActiveKey` / `renderRawTabs` in
  `webui/static/main.js`, `rawSlots` + per-pane generations in
  `cli/raw_forward_wasm.go`.

## Decisions

1. **Panes live on the modal, not on `App`.** `RawConnectModal` holds
   `panes []rawPane` plus `active int`. `App` keeps no connection state; it
   routes gen-tagged messages to the pane that owns the gen.
2. **Generations become per-pane.** `App.rawGen` stays as the *allocator* (a
   monotonic counter) but the guard moves: a message applies to the pane whose
   `gen` equals `msg.Gen`, and is dropped when no pane matches. The existing
   "one connect attempt in flight" rule becomes per-pane, unchanged in spirit —
   two attempts within one pane still share a gen, so `connecting` still gates
   the second dispatch.
3. **`esc` hides, `x` closes.** `esc` leaves every pane connected and the
   forwards registered. `x` closes the active pane: bump its gen, `Close` the
   conn, drop the pane. Quitting the TUI closes all panes (a live `RawConn`
   whose process is gone would leave a registration behind).
4. **`[+ new]` is a pane slot, not a separate screen.** It is index 0 of the tab
   strip and shows the target input. Connecting from it appends a pane and makes
   it active; the `[+ new]` slot resets to empty and stays.
5. **Panes are per-task and the modal is opened from a task.** `t` keeps its
   current meaning (tasks pane, selected task). Panes record their task id and
   the tab strip shows it when panes span more than one task.
6. **Closed panes stay until dismissed.** A pane whose connection ended keeps
   its received bytes and shows the close reason, styled muted/dashed to match
   the WebUI tab. `x` removes it.
7. **No hex input.** The WebUI has a hex toggle; the TUI does not, and this spec
   does not add one. Called out so the asymmetry is a recorded decision rather
   than an oversight.

## Architecture

### `tui/rawforward.go`

```go
type rawPane struct {
    taskID string
    host   string
    port   int
    gen    uint64
    conn   *cli.RawConn
    cancel context.CancelFunc
    out    []byte // ring, rawTUIRingBytes
    live   bool
    connecting bool
    note   string // connect error, close reason, or "connecting…"
}

type RawConnectModal struct {
    open   bool
    taskID string          // task the modal was opened from; [+ new] connects here
    input  textinput.Model // target for [+ new], bytes to send for a pane
    panes  []rawPane
    active int             // 0 = the [+ new] slot; panes are 1-based on screen
}
```

Methods mirror the current ones but take the active pane: `SendLine`,
`AppendOutput`, `MarkLive`, `MarkClosed`, `SetConn` all resolve `active` first,
and gain a `paneFor(gen int) *rawPane` used by the App's message routing.

### `tui/app.go`

- `RawForwardOpenedMsg` / `DataMsg` / `ClosedMsg` handling looks the pane up by
  `msg.Gen` instead of comparing to a single `a.rawGen`. No match = drop, same
  as today.
- Key handling inside the modal: `←`/`→` switch tabs, `x` closes the active
  pane, `esc` hides the modal, `Enter` connects (on `[+ new]`) or sends (on a
  pane). Every other key goes to the text input, unchanged.
- On `tea.Quit`, close every pane.

### View

```
╭─ raw connect ─────────────────────────────────────╮
│ [+ new] [127.0.0.1:8080]* [10.0.0.2:22]           │
│ connected · fwd 7                                 │
│ > _                                               │
│ HTTP/1.1 200 OK                                   │
│ ok                                                │
│ ←/→ tab · Enter send · x close pane · esc hide    │
╰───────────────────────────────────────────────────╯
```

The active tab is marked; closed panes render muted. The status line is the
active pane's `note`, or the target prompt on `[+ new]`.

## Failure modes

- **A pane's connect fails.** The pane exists with `live=false` and the error as
  its note; it is not silently dropped, so the operator can read why.
- **The client connection drops.** Each pane's pump ends and marks its pane
  closed with the reason — the same path as today, now per pane.
- **Modal hidden while bytes arrive.** Messages still route by gen and append to
  the pane's ring; reopening shows them. The ring bound is unchanged.
- **Task ends while a pane is open.** The forward dies with it; the pane closes
  with whatever reason the control stream gave.

## Verification

- Unit: pane routing by gen (message for pane B does not touch pane A); `esc`
  leaves conns live and `x` closes exactly one; `[+ new]` appends and stays.
- Unit: quitting closes every pane.
- Live: one dummy harness, two panes to two ports, switch tabs, send on each,
  `esc` then reopen and confirm both still live and `forward ls` still lists
  both. One run — the unit tests carry the routing logic.

## Out of scope

Hex input, per-pane scrollback beyond the existing ring, reordering tabs,
opening panes for a task other than the one the modal was opened from.
