# UDP port forwarding — Implementation Plan (harness half)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Carry UDP through `harness-cli forward`, on the datagram frame the objtrsf half landed, with the protocol and route axes visible on every surface.

**Architecture:** A `protocol` axis (`tcp`/`udp`) and a `route` axis (the existing three data-plane paths, renamed off `FileTransferRoute`) on the forward registration. UDP flows are multiplexed inside a `ForwardDatagram` framing the harness owns; `trsf` sees an opaque payload. `peer.Conn` grows a datagram pump beside its control pump.

**Tech Stack:** Go 1.25, objtrsf `cc8d092` (already bumped), `.bgn` via `scripts/protoregen.sh`.

**Spec:** `docs/superpowers/specs/2026-09-17-trsf-datagram-udp-forward-design.md` — §4b, §4c, §6, §7, §8.

## Global Constraints

- **Verify with make targets**, never a bare `go build ./...`: `make check`, `make vet`, `make test`. `./...` hides the wasm build and the pattern breaks.
- **Regen churn is a constant.** Any `.bgn` addition rewrites `message.go` in bulk. Do not investigate it; check `make check` / `vet` / `test` plus the new symbols.
- **Zero is never elided, absence is.** `portForwardJSON`'s own comment is the rule: "Always emitted, zeros included". The only permitted gate on a new field is `protocol == udp` — an existence condition, never a value.
- **One renderer per display concept**, in `cli`, shared by CLI/TUI/WebUI. `cli/forward_snapshot_row.go` documents this; new axes go into the existing renderers, not into three formatting sites.
- **`route` is named by the caller and never silently substituted.** A route that cannot be taken is refused with `route_unavailable`. `message.bgn`'s own comment says why: a caller who asked for `forwarded` asked for the server not to read these bytes.

## Staging, and what the spec permits

The spec's §2 says the six `protocol × route` cells need not all ship at once, citing `PortForwardDirection` (`local` implemented, `remote` reserved) as the precedent. This plan lands:

- **schema: all six cells**, so the wire is not reshaped later;
- **`udp × splice`**: implemented end to end. Splice is where the existing forward machinery already lives, so it is the shortest path to a working tunnel;
- **`udp × forwarded` / `udp × direct`**: refused with `route_unavailable` until wired. Task 8 is where they land, and it is explicitly out of this plan's required set;
- **`tcp × *`**: unchanged behaviour, `route` recorded and displayed.

---

## Task 1: The schema — protocol and route axes

**Files:**
- Modify: `appwire/app.bgn`, `runner/protocol/message.bgn`
- Regenerate: `appwire/app.go`, `runner/protocol/message.go`
- Modify (rename fallout): the 20 files referencing `FileTransferRoute`

**Interfaces:**
- Produces: `protocol.ForwardProtocol_Tcp` / `_Udp`, `protocol.DataPlaneRoute_Splice` / `_Forwarded` / `_Direct`, `protocol.ForwardDatagram{ForwardId, FlowId, Payload}`, `appwire.AppKind_ForwardDatagram` (0x48), and `protocol` / `route` / `max_datagram_size` / `dropped_*` on `PortForwardInfo`.

- [ ] **Step 1: Rename the route enum**

`FileTransferRoute` → `DataPlaneRoute` in `message.bgn`. Members, ordinals and string tags unchanged — this is not a wire change. Move the ~100-line comment at `:2104` with it; it describes the data plane, not files.

- [ ] **Step 2: Add the new types to `message.bgn`**

```
enum ForwardProtocol:
    :u8
    tcp = "tcp"
    udp = "udp"

# One UDP datagram crossing a forward. forward_id is redundant on a route
# whose carrier belongs to one registration, and is carried anyway so ONE
# decoder serves every route -- twelve bytes is cheaper than two framings.
format ForwardDatagram:
    forward_id :u64
    flow_id    :u32
    payload    :[..]u8
```

Append to `RegisterPortForwardRequest` and `PortForwardInfo`: `protocol :ForwardProtocol`, `route :DataPlaneRoute`. Append to `PortForwardInfo` only: `max_datagram_size :u16`, `dropped_oversize :u64`, `dropped_congestion :u64`, `dropped_queue :u64`.

Append to `OpenPortForwardStatus`: `route_unavailable = "route_unavailable"`.

- [ ] **Step 3: Add the appwire kind**

`forward_datagram = 0x48` in `appwire/app.bgn`.

- [ ] **Step 4: Regenerate and rename**

```bash
./scripts/protoregen.sh runner/protocol/message.bgn
./scripts/protoregen.sh appwire/app.bgn
grep -rl 'FileTransferRoute' --include='*.go' . | xargs sed -i 's/FileTransferRoute/DataPlaneRoute/g'
```

- [ ] **Step 5: Verify**

```bash
make check && make vet && make test
```

- [ ] **Step 6: Commit**

---

## Task 2: `/udp` in the spec grammar

**Files:** `cli/port_forward.go` (`ParseForwardSpec`, `ParseRemoteForwardSpec`, the specs' structs), `cli/port_forward_test.go`, `cli/workspace/validate.go`

- [ ] **Step 1: Write the failing tests** — every existing spelling still parses as tcp; `5353/udp`, `5353:h:5353/udp`, `[bind:]5353:h:5353/udp` parse as udp; an unknown suffix is an error naming what it accepts; the workspace file inherits it.
- [ ] **Step 2: Run them; expect failures on the `Protocol` field not existing.**
- [ ] **Step 3: Add `Protocol ForwardProtocol` to `ForwardSpec` / `RemoteForwardSpec`, strip a trailing `/tcp` or `/udp` before the existing colon split.** Absent suffix means tcp, so no existing spelling changes.
- [ ] **Step 4: Update the grammar restated in `cli/workspace/validate.go:33`'s error string** — that restatement is a pre-existing split in the grammar's SSOT, and leaving it stale is how the file's rules and the parser's diverge.
- [ ] **Step 5: `make test`; commit.**

---

## Task 3: The operator surface

**Files:** `cli/port_forward_list.go` (`PortForwardSpecString`, `PortForwardTrafficLine`, `portForwardJSON`, `PortForwardInfoLines`), `cli/forward_snapshot_row.go`, their tests

- [ ] **Step 1: Write the failing tests.**
  - a udp row's spec string round-trips the typed form: `127.0.0.1:5353 -> 127.0.0.1:5353/udp`
  - a tcp row's spec string is UNCHANGED (no `/tcp` suffix)
  - a udp row's traffic line carries `mtu=` and the three drop counts, **including when they are zero**
  - a tcp row's traffic line carries NO `mtu=` — gated on existence, not value
  - the JSON form carries `protocol` and `route` on every row, and the datagram fields on udp rows
- [ ] **Step 2–4: run, implement, re-run.**
- [ ] **Step 5: Commit.**

---

## Task 4: The datagram pump on `peer.Conn`

**Files:** `peer/conn.go`, `peer/datagram_test.go`

**Interfaces:**
- Produces: `peer.DatagramHandler func(kind appwire.AppKind, payload []byte)`, `(*Conn).SetOnDatagram(h)`, `(*Conn).SendDatagram(b []byte) error`, `(*Conn).MaxDatagramSize() int`.

`trsf` delivers datagrams on their own queue, not through `AutoReceive`'s seam, so `Start` needs a second goroutine draining `ReceiveDatagram` and dispatching by the leading `appwire.AppKind` byte — the same shape `dispatch` already uses for control messages.

- [ ] **Step 1: Write the failing test** — a datagram sent on one end reaches the other end's handler with the kind byte stripped.
- [ ] **Step 2–4: run, implement, re-run.**
- [ ] **Step 5: Commit.**

---

## Task 5: The flow table

**Files:** `cli/forward_flows.go` (create), `cli/forward_flows_test.go` (create)

One type used by both ends, because both keep the same mapping in opposite directions.

**Interfaces:**
- Produces: `type flowTable struct{...}` with `lookupOrCreate(addr string) (uint32, bool)`, `get(id uint32) (*flow, bool)`, `reapIdle(now time.Time)`, `close()`.

- [ ] **Step 1: Write the failing tests** — a repeated source address reuses its id; a new one allocates; an idle flow is reaped after the timeout and its socket closed; the cap evicts the oldest idle flow and counts it.
- [ ] **Step 2–4: run, implement, re-run.** Idle timeout 60s (between conntrack's 30s unreplied and 180s assured); cap so a peer varying its source port cannot make the runner open sockets without bound.
- [ ] **Step 5: Commit.**

---

## Task 6: UDP `-L` end to end, on the splice route

**Files:** `cli/port_forward.go` (client listener), `runner/port_forward.go` (runner dial side), `server/port_forward.go` (relay), `integration/port_forward_test.go`

- [ ] **Step 1: Write the failing integration test** — a UDP echo server behind `-L`, one request and one reply, then a second flow from a different source port.
- [ ] **Step 2–4: run, implement, re-run.**
  - client: `net.ListenUDP`, per-source flow, `SendDatagram` with the `ForwardDatagram` framing
  - runner: on an unseen `flow_id`, `net.Dial("udp", target)`; replies carry the same id back
  - server: decode `forward_id`, look up the registration, re-emit on the other conn; drop and count when the far leg cannot take it
- [ ] **Step 5: Commit.**

---

## Task 7: UDP `-R`

**Files:** `runner/port_forward.go` (listener), `cli/port_forward.go` (dial side), `integration/port_forward_test.go`

**`-R` needs no notification frame.** TCP's `-R` needs `RemoteForwardConn` because a stream must be created and named before bytes move; a UDP flow is created by its first datagram arriving, so `PortForwardEventKind.conn_notify` stays TCP-only.

- [ ] **Step 1: Write the failing integration test.** **Steps 2–4: run, implement, re-run. Step 5: commit.**

---

## Task 8 (beyond this plan's required set): `udp × forwarded` / `udp × direct`

One data-plane connection per registration, so the cold-window artifact §6a describes does not arise. Refused with `route_unavailable` until this lands.

---

## Task 9: TUI and WebUI

**Files:** `tui/portforward.go`, `cmd/harness-webui-wasm/main.go`

- [ ] Protocol and route in the forward form and the rows, through the `cli` renderers rather than new formatting.
- [ ] Walk `surface-parity-checklist` items 1–39 with a verdict per number.

## Self-review notes

Spec coverage: §4b→T1/3, §4c→T1, §6a/6b→T1 (schema) + T8 (routes), §6c→T5/6/7, §6d→T1 (`ForwardDatagram` is where fragmentation would go), §7a→T2, §7b→T3, §7c/7d→T3, §8→T9.

**Not covered, deliberately:** the spec's §6a prediction (a long-lived forward does not pay the cold window) is only falsifiable once Task 8 exists. Its test is written down in the spec's §11 and stays open.
