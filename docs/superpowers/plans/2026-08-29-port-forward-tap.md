# Port forward counters and tap — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `forward ls` reports what a forward has carried, and `forward tap <id>` streams the bytes crossing it live, behind a dedicated capability.

**Architecture:** Both halves hook the server's port-forward splice
(`server/port_forward.go:65` local, `:252` remote) — the single point every
forward's bytes cross. Counters are atomic adds on the registry entry and ride
the existing `PortForwardInfo` listing. The tap is a server-initiated stream of
`ForwardTapRecord` unions, fanned out through bounded per-tap queues that drop
into a `gap` record rather than blocking the relay or dropping the tapper. The
server never formats: the `cli/` package renders records, natively and in wasm.

**Tech Stack:** Go 1.x, brgen `.bgn` schemas (regenerated via `make protoregen`),
objtrsf streams, bubbletea/bubbles TUI, vanilla JS + Go/wasm WebUI.

**Spec:** `docs/superpowers/specs/2026-08-29-port-forward-tap-design.md`

## Global Constraints

- **Schema lives in one place.** Every wire change in this plan is made in
  `runner/protocol/message.bgn` in Task 1 and nowhere else. No later task edits
  the schema.
- **Regeneration:** `make protoregen` (defaults to `runner/protocol/message.bgn`).
  Never hand-edit `runner/protocol/message.go`.
- **Verification targets:** `make check`, `make wasm-check`, `make vet`,
  `make test`. Never `go build ./...` — it hides pattern breaks the make
  targets catch.
- **Capability numbering:** `forward_tap = 0x10000`; `all` widens `0xffff` →
  `0x1ffff`. Widening is required, not stylistic: `callerCaps` hands operator
  connections `Capability_All`, so an un-widened literal denies the operator.
- **Direction naming:** `to_target` / `from_target` everywhere — fields, enum
  members, JSON keys, column headers. Never `tx`/`rx`.
- **The server formats nothing.** Only `max_record_bytes` truncation happens
  server-side. All rendering is in `cli/`.
- **Restart order after Task 1 lands:** server and clients together; runners are
  untouched (no runner-facing message changes).
- **Zero is printed, never elided** (`surface-parity-checklist` item 31).
- **Commit style:** `feat(forward): …` / `fix(forward): …`, imperative, matching
  recent history.

---

### Task 1: The whole wire change

**Files:**
- Modify: `runner/protocol/message.bgn` (Capability enum ~1684-1775, `PortForwardInfo` 1883, `OpenPortForwardRequest` 1940, `TaskControlKind`, new tap formats after `PortForwardEvent` 1931)
- Regenerate: `runner/protocol/message.go`
- Test: `runner/protocol/port_forward_test.go`, `runner/protocol/capability_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `protocol.Capability_ForwardTap`, `protocol.TaskControlKind_OpenForwardTap`,
  `protocol.OpenForwardTapRequest{ForwardId uint64; DirectionFilter ForwardTapFilter; MaxRecordBytes uint32}`,
  `protocol.OpenForwardTapResponse{Status OpenForwardTapStatus; StreamId uint64}`,
  `protocol.ForwardTapRecord` with arms `ConnOpen() *ForwardTapConnOpen`, `Data() *ForwardTapData`,
  `Gap() *ForwardTapGap`, `ConnClose() *ForwardTapConnClose`, `ForwardClosed() *ForwardTapForwardClosed`,
  `protocol.OpenPortForwardRequest.ForwardId`,
  `protocol.OpenPortForwardStatus_NoSuchForward`,
  and six new `PortForwardInfo` fields: `BytesToTarget`, `BytesFromTarget`,
  `ConnsTotal` (u64), `ConnsOpen` (u32), `Taps` (u16), `LastActivityUnixMs` (u64).

- [ ] **Step 1: Write the failing round-trip test**

Append to `runner/protocol/port_forward_test.go`:

```go
func TestForwardTapRecordRoundTrip(t *testing.T) {
	var rec ForwardTapRecord
	rec.Kind = ForwardTapRecordKind_Data
	rec.UnixMs = 1756000000000
	var d ForwardTapData
	d.ConnSeq = 3
	d.Direction = ForwardTapDirection_ToTarget
	d.StreamOffset = 4096
	d.TruncatedBytes = 12
	d.SetData([]byte("GET /x HTTP/1.1\r\n"))
	rec.SetData(d)

	buf := rec.MustEncodeCopy(nil)
	var got ForwardTapRecord
	if err := got.DecodeExact(buf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	arm := got.Data()
	if arm == nil {
		t.Fatalf("data arm missing, kind=%v", got.Kind)
	}
	if arm.ConnSeq != 3 || arm.StreamOffset != 4096 || arm.TruncatedBytes != 12 {
		t.Fatalf("arm fields: %+v", arm)
	}
	if string(arm.Data) != "GET /x HTTP/1.1\r\n" {
		t.Fatalf("payload: %q", arm.Data)
	}
	if got.Gap() != nil || got.ConnOpen() != nil {
		t.Fatalf("wrong arm readable on a data record")
	}
}

func TestForwardTapForwardClosedCarriesReason(t *testing.T) {
	var rec ForwardTapRecord
	rec.Kind = ForwardTapRecordKind_ForwardClosed
	rec.UnixMs = 1
	rec.SetForwardClosed(ForwardTapForwardClosed{Reason: PortForwardCloseReason_Killed})
	var got ForwardTapRecord
	if err := got.DecodeExact(rec.MustEncodeCopy(nil)); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ForwardClosed() == nil || got.ForwardClosed().Reason != PortForwardCloseReason_Killed {
		t.Fatalf("reason lost: %+v", got.ForwardClosed())
	}
}

func TestPortForwardInfoCarriesCounters(t *testing.T) {
	var in PortForwardInfo
	in.ForwardId = 7
	in.SetBindAddr([]byte("127.0.0.1"))
	in.SetTargetHost([]byte("localhost"))
	in.SetOriginCid([]byte("ws:abc"))
	in.BytesToTarget = 1 << 20
	in.BytesFromTarget = 48
	in.ConnsTotal = 41
	in.ConnsOpen = 3
	in.Taps = 1
	in.LastActivityUnixMs = 1756000000000

	var got PortForwardInfo
	if err := got.DecodeExact(in.MustEncodeCopy(nil)); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.BytesToTarget != 1<<20 || got.BytesFromTarget != 48 ||
		got.ConnsTotal != 41 || got.ConnsOpen != 3 || got.Taps != 1 ||
		got.LastActivityUnixMs != 1756000000000 {
		t.Fatalf("counters lost: %+v", got)
	}
}

func TestOpenPortForwardRequestCarriesForwardID(t *testing.T) {
	var in OpenPortForwardRequest
	in.SetRemoteHost([]byte("localhost"))
	in.RemotePort = 3000
	in.ForwardId = 9
	var got OpenPortForwardRequest
	if err := got.DecodeExact(in.MustEncodeCopy(nil)); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ForwardId != 9 {
		t.Fatalf("forward id lost: %d", got.ForwardId)
	}
}
```

Append to `runner/protocol/capability_test.go`:

```go
func TestForwardTapBitIsInAll(t *testing.T) {
	if Capability_ForwardTap != 0x10000 {
		t.Fatalf("forward_tap bit moved: %#x", uint32(Capability_ForwardTap))
	}
	if Capability_All&Capability_ForwardTap == 0 {
		t.Fatal("all does not include forward_tap; callerCaps would deny the operator")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./runner/protocol/ -run 'ForwardTap|PortForwardInfoCarries|OpenPortForwardRequestCarries' -v
```

Expected: compile failure — `ForwardTapRecord`, `Capability_ForwardTap`,
`OpenPortForwardRequest.ForwardId` undefined.

- [ ] **Step 3: Edit the schema**

In `runner/protocol/message.bgn`, Capability enum, after `exec_run = 0x8000`:

```
    # forward_tap authorizes READING THE PAYLOAD of a port forward — the
    # cleartext of whatever protocol it carries, which routinely means an
    # Authorization header, a git credential or a database password. Holding a
    # forward is a different power, so forward_local does not imply this and
    # neither does forward_remote.
    forward_tap    = 0x10000, "forward_tap"
```

and widen the literal (it is a literal, not "every defined bit"):

```
    all            = 0x1ffff, "all"
```

Append to `PortForwardInfo`, after `client_endpoint`:

```
    # Always-on counters. They answer "is this forward doing anything", which
    # the spec's row could not: an idle forward and a saturated one rendered
    # identically. Named against the forward's own target, because a -R
    # forward's initiator is on the other side and tx/rx would invert.
    bytes_to_target       :u64
    bytes_from_target     :u64
    conns_total           :u64
    conns_open            :u32
    taps                  :u16  # taps open on this forward right now
    last_activity_unix_ms :u64  # 0 = no byte has ever crossed
```

Append to `OpenPortForwardRequest`:

```
    # The registration these bytes belong to. The id is minted by
    # register_port_forward, so without it the server cannot attribute a splice
    # to a row — and two forwards with an identical spec from one client are
    # indistinguishable, which is the ambiguity origin_cid exists to resolve on
    # the listing. There is no "unattributed" value: 0 is refused like any other
    # id that resolves to nothing.
    forward_id      :u64
```

Add to `OpenPortForwardStatus`:

```
    no_such_forward   # forward_id names no live registration (0 included)
```

Add to `TaskControlKind`:

```
    open_forward_tap       # returns the stream tap records arrive on
```

After `PortForwardEvent` (the sibling this shape follows), add:

```
enum ForwardTapDirection:
    :u8
    to_target
    from_target

enum ForwardTapFilter:
    :u8
    both
    to_target
    from_target

enum OpenForwardTapStatus:
    :u8
    ok
    no_such_forward   # unknown id OR not visible to the caller — the same
                      # deliberate conflation kill_port_forward makes, so an
                      # invisible id is not an existence oracle
    internal_error
    # No `denied` member: required_cap is checked before dispatch and answers
    # with a different response KIND (permission_denied), so the handler is
    # never entered on a capability failure and a status here would have no
    # writer.

format OpenForwardTapRequest:
    forward_id       :u64
    direction_filter :ForwardTapFilter
    # 0 = whole payload. Otherwise each record's data is cut to this many bytes
    # and truncated_bytes reports what was cut from THAT record.
    max_record_bytes :u32

format OpenForwardTapResponse:
    status    :OpenForwardTapStatus
    stream_id :u64

enum ForwardTapRecordKind:
    :u8
    conn_open
    data
    gap
    conn_close
    forward_closed

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
    # starts at a non-zero offset rather than pretending the stream began when
    # the tap did.
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
    # never be slowed. The default union storage boxes the arm in an interface
    # and type-asserts on every access — a heap allocation per chunk on the
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

Also add the `open_forward_tap` arm to the `TaskControlRequest` and
`TaskControlResponse` unions, beside the existing port-forward arms, following
their exact form.

- [ ] **Step 4: Regenerate and run the tests**

```bash
make protoregen
go test ./runner/protocol/ -run 'ForwardTap|PortForwardInfoCarries|OpenPortForwardRequestCarries' -v
```

Expected: PASS. If `SetData` on `ForwardTapData` collides with a generated
codec method name, rename the arm — this project has hit exactly that
(`project_bgn_union_arm_name_collision`); `payload` is the fallback name, and
if you use it, update every later task that says `Data()`.

- [ ] **Step 5: Whole-tree check and commit**

```bash
make vet && make check && make wasm-check
git add runner/protocol/message.bgn runner/protocol/message.go runner/protocol/port_forward_test.go runner/protocol/capability_test.go
git commit -m "feat(forward): the wire can say what a forward carried, and stream it"
```

---

### Task 2: The registry entry counts

**Files:**
- Modify: `server/port_forward_registry.go` (the `portForward` struct, ~line 16)
- Create: `server/forward_counters.go`
- Test: `server/forward_counters_test.go`

**Interfaces:**
- Consumes: Task 1's enums.
- Produces: on `*portForward` — `noteBytes(dir protocol.ForwardTapDirection, n int)`,
  `openConn() uint64` (returns the new `conn_seq`, bumps `connsTotal`/`connsOpen`),
  `closeConn(seq uint64, toTarget, fromTarget uint64)`, and
  `counters() (toTarget, fromTarget, connsTotal uint64, connsOpen uint32, lastMs uint64)`.

- [ ] **Step 1: Write the failing test**

Create `server/forward_counters_test.go`:

```go
package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestForwardCountersBothDirections(t *testing.T) {
	pf := &portForward{forwardID: 1}
	pf.noteBytes(protocol.ForwardTapDirection_ToTarget, 100)
	pf.noteBytes(protocol.ForwardTapDirection_FromTarget, 250)
	pf.noteBytes(protocol.ForwardTapDirection_ToTarget, 5)

	to, from, total, open, last := pf.counters()
	if to != 105 || from != 250 {
		t.Fatalf("to=%d from=%d, want 105/250 — a swapped direction here inverts every row", to, from)
	}
	if total != 0 || open != 0 {
		t.Fatalf("bytes must not invent connections: total=%d open=%d", total, open)
	}
	if last == 0 {
		t.Fatal("last activity still zero after bytes crossed")
	}
}

func TestForwardCountersConnLifecycle(t *testing.T) {
	pf := &portForward{forwardID: 1}
	a := pf.openConn()
	b := pf.openConn()
	if a != 1 || b != 2 {
		t.Fatalf("conn_seq must be 1-based and per forward: %d %d", a, b)
	}
	_, _, total, open, _ := pf.counters()
	if total != 2 || open != 2 {
		t.Fatalf("after two opens: total=%d open=%d", total, open)
	}
	pf.closeConn(a, 10, 20)
	_, _, total, open, _ = pf.counters()
	if total != 2 {
		t.Fatalf("conns_total must not go down: %d", total)
	}
	if open != 1 {
		t.Fatalf("conns_open after one close: %d", open)
	}
}

func TestForwardCountersStartAtZeroNotAbsent(t *testing.T) {
	pf := &portForward{forwardID: 1}
	to, from, total, open, last := pf.counters()
	if to != 0 || from != 0 || total != 0 || open != 0 {
		t.Fatalf("fresh forward: %d %d %d %d", to, from, total, open)
	}
	if last != 0 {
		t.Fatal("last activity must be 0 until a byte crosses; the renderer distinguishes it")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

```bash
go test ./server/ -run TestForwardCounters -v
```

Expected: compile failure — `noteBytes`, `openConn`, `closeConn`, `counters` undefined.

- [ ] **Step 3: Add the fields and methods**

In `server/port_forward_registry.go`, add to the `portForward` struct:

```go
	// Always-on traffic accounting. Atomics rather than the registry mutex:
	// these are written from every relay goroutine of every connection under
	// this forward, and the relay must never wait on a lock a listing holds.
	bytesToTarget   atomic.Uint64
	bytesFromTarget atomic.Uint64
	connsTotal      atomic.Uint64
	connsOpen       atomic.Int64
	lastActivityMs  atomic.Int64
	nextConnSeq     atomic.Uint64
```

Add `"sync/atomic"` to the imports. Create `server/forward_counters.go`:

```go
package server

import (
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// noteBytes records n bytes crossing this forward in dir. Called from the relay
// goroutine, so it does exactly two atomic adds and no allocation.
func (pf *portForward) noteBytes(dir protocol.ForwardTapDirection, n int) {
	if pf == nil || n <= 0 {
		return
	}
	if dir == protocol.ForwardTapDirection_ToTarget {
		pf.bytesToTarget.Add(uint64(n))
	} else {
		pf.bytesFromTarget.Add(uint64(n))
	}
	pf.lastActivityMs.Store(time.Now().UnixMilli())
}

// openConn assigns this forward's next conn_seq and counts the connection.
// 1-based: 0 is not a connection, which is why the tap's forward_closed arm
// carries no seq at all rather than a zero one.
func (pf *portForward) openConn() uint64 {
	if pf == nil {
		return 0
	}
	pf.connsTotal.Add(1)
	pf.connsOpen.Add(1)
	return pf.nextConnSeq.Add(1)
}

// closeConn releases a connection. conns_total is a lifetime count and never
// goes down; only conns_open does.
func (pf *portForward) closeConn(seq uint64, toTarget, fromTarget uint64) {
	if pf == nil {
		return
	}
	pf.connsOpen.Add(-1)
}

// counters reads the set the listing renders. conns_open is clamped at zero:
// a double close would otherwise report a negative as a huge unsigned value on
// an operator's screen.
func (pf *portForward) counters() (toTarget, fromTarget, connsTotal uint64, connsOpen uint32, lastMs uint64) {
	if pf == nil {
		return 0, 0, 0, 0, 0
	}
	open := pf.connsOpen.Load()
	if open < 0 {
		open = 0
	}
	last := pf.lastActivityMs.Load()
	if last < 0 {
		last = 0
	}
	return pf.bytesToTarget.Load(), pf.bytesFromTarget.Load(),
		pf.connsTotal.Load(), uint32(open), uint64(last)
}
```

`closeConn` takes the per-connection totals it does not yet use; Task 5 feeds
them to the tap's `conn_close` record. Leaving the parameters out now would mean
changing every call site then.

- [ ] **Step 4: Run the tests**

```bash
go test ./server/ -run TestForwardCounters -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add server/port_forward_registry.go server/forward_counters.go server/forward_counters_test.go
git commit -m "feat(forward): a registration counts the bytes and connections under it"
```

---

### Task 3: Every data stream names its forward

**Files:**
- Modify: `cli/port_forward.go` (`OpenPortForward` ~line 81, its call site ~307)
- Modify: `cli/forward_endpoint.go` (`OpenRawForward` 55-71)
- Modify: `cli/preview_forward_wasm.go` (`PreviewPinFetch` 123-159)
- Modify: `server/port_forward.go` (`handleOpenPortForward` 22-70)
- Test: `server/port_forward_test.go`, `cli/forward_endpoint_test.go`

**Interfaces:**
- Consumes: Task 1's `OpenPortForwardRequest.ForwardId`, `OpenPortForwardStatus_NoSuchForward`; Task 2's `openConn`.
- Produces: `(*cli.Client).OpenPortForward(ctx, taskIDHex, remoteHost string, remotePort int, forwardID uint64)` — a signature change, every caller updated in this task.

- [ ] **Step 1: Write the failing tests**

Append to `server/port_forward_test.go`:

```go
func TestOpenPortForwardRefusesUnknownForwardID(t *testing.T) {
	h, conn, taskID := newRunningTaskHandler(t) // existing helper in this package
	var req protocol.OpenPortForwardRequest
	req.TaskId = taskID
	req.SetRemoteHost([]byte("localhost"))
	req.RemotePort = 3000

	req.ForwardId = 0
	if got := h.handleOpenPortForward(conn, &req); got.Status != protocol.OpenPortForwardStatus_NoSuchForward {
		t.Fatalf("forward_id=0 must be refused, got %v", got.Status)
	}
	req.ForwardId = 4242
	if got := h.handleOpenPortForward(conn, &req); got.Status != protocol.OpenPortForwardStatus_NoSuchForward {
		t.Fatalf("unknown forward id must be refused, got %v", got.Status)
	}
}
```

Append to `cli/forward_endpoint_test.go`:

```go
// OpenRawForward must register BEFORE it opens: the data stream now names the
// forward id, which does not exist until the registration returns.
func TestOpenRawForwardRegistersBeforeOpening(t *testing.T) {
	var order []string
	c := newFakeClient(t, func(kind protocol.TaskControlKind) {
		switch kind {
		case protocol.TaskControlKind_RegisterPortForward:
			order = append(order, "register")
		case protocol.TaskControlKind_OpenPortForward:
			order = append(order, "open")
		}
	})
	_, err := OpenRawForward(context.Background(), c, testTaskIDHex, "localhost", 3000,
		protocol.ClientEndpointKind_InProcessStdio, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if len(order) != 2 || order[0] != "register" || order[1] != "open" {
		t.Fatalf("call order %v, want [register open]", order)
	}
}
```

If `newFakeClient` with a per-kind hook does not exist in `cli/`, add it to
`cli/forward_endpoint_test.go` modelled on the existing fake in that file.

- [ ] **Step 2: Run them to verify they fail**

```bash
go test ./server/ -run TestOpenPortForwardRefusesUnknownForwardID -v
go test ./cli/ -run TestOpenRawForwardRegistersBeforeOpening -v
```

Expected: server test fails with `Ok` (no gate yet); cli test fails with
`[open register]`.

- [ ] **Step 3: Thread the id**

`cli/port_forward.go` — add the parameter and set the field:

```go
func (c *Client) OpenPortForward(ctx context.Context, taskIDHex, remoteHost string, remotePort int, forwardID uint64) (trsf.BidirectionalStream, error) {
	...
	body := protocol.OpenPortForwardRequest{
		TaskId:     tid,
		RemotePort: uint16(remotePort),
		ForwardId:  forwardID,
	}
```

and add the new status to the switch:

```go
	case protocol.OpenPortForwardStatus_NoSuchForward:
		return nil, errors.New("forward: registration is gone (killed, or the task ended)")
```

In `RunForward`, the accept loop already holds the id from `RegisterPortForward`
(`cli/port_forward.go:229`); pass it at the `OpenPortForward` call (~:307).

`cli/forward_endpoint.go` — swap the two RPCs. Register first, then open, and
close the CONTROL stream on the open's failure arm:

```go
	ctrl, fid, err := c.RegisterPortForward(ctx, taskIDHex, protocol.PortForwardDirection_Local,
		"", 0, host, port, kind)
	if err != nil {
		return nil, fmt.Errorf("raw forward: register: %w", err)
	}
	data, err := c.OpenPortForward(ctx, taskIDHex, host, port, fid)
	if err != nil {
		_ = ctrl.CloseBoth()
		return nil, err
	}
```

Update the doc comment above it: it currently explains closing the DATA stream
on a registration failure, which is now the other way round.

`cli/preview_forward_wasm.go` — `PreviewPinFetch` reads the pin's id under the
same lock it already takes:

```go
	pinMu.Lock()
	slot := pinSlots[key]
	var taskID, host string
	var port int
	var fid uint64
	if slot != nil {
		taskID, host, port, fid = slot.taskID, slot.host, slot.port, slot.forwardID
	}
	pinMu.Unlock()
	...
	st, err := c.OpenPortForward(ctx, taskID, host, port, fid)
```

`server/port_forward.go` — `handleOpenPortForward`, after the runner lookup and
before creating any stream:

```go
	pf, ok := h.pforwards().get(req.ForwardId)
	if !ok {
		// Includes forward_id = 0. A stream naming no registration is a
		// forward no listing shows, no counter counts and no kill can name —
		// the state cli/forward_endpoint.go:36 says the registry exists to
		// prevent.
		return errResp(protocol.OpenPortForwardStatus_NoSuchForward)
	}
	connSeq := pf.openConn()
	_ = connSeq // Task 5 stamps tap records with it
```

- [ ] **Step 4: Run the tests**

```bash
go test ./server/ ./cli/ -run 'OpenPortForward|OpenRawForward|Forward' -v
make vet && make check && make wasm-check
```

Expected: PASS. `make wasm-check` is not optional here — `preview_forward_wasm.go`
is a wasm-only file and `make check` does not compile it.

- [ ] **Step 5: Commit**

```bash
git add cli/port_forward.go cli/forward_endpoint.go cli/preview_forward_wasm.go server/port_forward.go server/port_forward_test.go cli/forward_endpoint_test.go
git commit -m "feat(forward): a data stream names the registration it belongs to"
```

---

### Task 4: The splice counts, and the listing reports it

**Files:**
- Modify: `server/task_handler.go` (`spliceBidi` 1501, `relayBytes` 1540)
- Modify: `server/port_forward.go` (both splice call sites, 65 and 252)
- Modify: `server/port_forward_list.go` (`portForwardInfo` 91)
- Modify: `cli/port_forward_list.go` (`PortForwardInfoLines` 134, `portForwardJSON` 152, `PortForwardInfoJSONLine`)
- Create: `cli/format_bytes.go` (moved from `tui/rawforward.go:333`)
- Modify: `tui/rawforward.go` (delete `formatByteCount`, call `cli.FormatByteCount`)
- Test: `server/port_forward_test.go`, `cli/port_forward_list_test.go`

**Interfaces:**
- Consumes: Task 2's `noteBytes`/`counters`, Task 3's `pf` lookup.
- Produces: `cli.FormatByteCount(n uint64) string`;
  `server.spliceBidiCounted(a, b trsf.BidirectionalStream, taskIDHex string, pf *portForward, connSeq uint64)`.

- [ ] **Step 1: Write the failing tests**

Append to `server/port_forward_test.go`:

```go
func TestSpliceCountsEachDirectionOnce(t *testing.T) {
	a, b := newStreamPair(t) // existing test helper for paired bidi streams
	pf := &portForward{forwardID: 1}
	go spliceBidiCounted(a.server, b.server, "task", pf, pf.openConn())

	writeAll(t, a.client, []byte("hello"))    // 5 bytes toward the target
	writeAll(t, b.client, []byte("worlds!!")) // 8 bytes back
	waitFor(t, func() bool {
		to, from, _, _, _ := pf.counters()
		return to == 5 && from == 8
	})
}

func TestPortForwardInfoReportsCounters(t *testing.T) {
	pf := &portForward{forwardID: 7, taskIDHex: "aa", targetHost: "localhost", targetPort: 3000}
	pf.noteBytes(protocol.ForwardTapDirection_ToTarget, 1024)
	_ = pf.openConn()

	info := portForwardInfo(pf)
	if info.BytesToTarget != 1024 || info.ConnsTotal != 1 || info.ConnsOpen != 1 {
		t.Fatalf("counters not carried onto the row: %+v", info)
	}
	if info.LastActivityUnixMs == 0 {
		t.Fatal("last activity not carried")
	}
}
```

Append to `cli/port_forward_list_test.go`:

```go
func TestForwardRowPrintsZeroCounters(t *testing.T) {
	var fi protocol.PortForwardInfo
	fi.ForwardId = 7
	fi.SetBindAddr([]byte("127.0.0.1"))
	fi.BindPort = 8080
	fi.SetTargetHost([]byte("localhost"))
	fi.TargetPort = 3000
	fi.SetOriginCid([]byte("ws:abc"))

	lines := PortForwardInfoLines([]protocol.PortForwardInfo{fi})
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"conns=0/0", "to-target=0", "from-target=0", "taps=0", "last=never"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("row elides a zero measurement (%q missing):\n%s", want, joined)
		}
	}
}

func TestForwardJSONCarriesCounters(t *testing.T) {
	var fi protocol.PortForwardInfo
	fi.ForwardId = 7
	fi.SetBindAddr([]byte("127.0.0.1"))
	fi.SetTargetHost([]byte("localhost"))
	fi.SetOriginCid([]byte("ws:abc"))
	fi.BytesToTarget = 1 << 20
	fi.ConnsOpen = 2
	fi.Taps = 1

	var got map[string]any
	if err := json.Unmarshal([]byte(PortForwardInfoJSONLine(&fi)), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"bytes_to_target", "bytes_from_target", "conns_total", "conns_open", "taps", "last_activity_unix_ms"} {
		if _, ok := got[k]; !ok {
			t.Fatalf("JSON contract missing %q: %v", k, got)
		}
	}
	if got["bytes_to_target"].(float64) != float64(1<<20) {
		t.Fatalf("bytes_to_target: %v", got["bytes_to_target"])
	}
}

func TestFormatByteCount(t *testing.T) {
	for _, c := range []struct {
		in   uint64
		want string
	}{{0, "0"}, {512, "512B"}, {2048, "2.0kB"}, {3 << 20, "3.0MB"}} {
		if got := FormatByteCount(c.in); got != c.want {
			t.Fatalf("FormatByteCount(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

```bash
go test ./server/ -run 'TestSpliceCounts|TestPortForwardInfoReports' -v
go test ./cli/ -run 'TestForwardRow|TestForwardJSON|TestFormatByteCount' -v
```

Expected: undefined `spliceBidiCounted` / `FormatByteCount`, and missing fields.

- [ ] **Step 3: Implement**

`server/task_handler.go` — add the counted variant beside `spliceBidi`, leaving
`spliceBidi` for callers with nothing to attribute:

```go
// spliceBidiCounted is spliceBidi with attribution: every byte is counted
// against pf, and (from Task 5) offered to pf's taps. Direction is named
// against the forward's target, so it means the same for -L and -R.
func spliceBidiCounted(a, b trsf.BidirectionalStream, taskIDHex string, pf *portForward, connSeq uint64) {
	var wg sync.WaitGroup
	var once sync.Once
	teardown := func() {
		once.Do(func() {
			_ = a.CloseBoth()
			_ = b.CloseBoth()
		})
	}
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer teardown()
		relayBytesCounted(a, b, pf, connSeq, protocol.ForwardTapDirection_ToTarget)
	}()
	go func() {
		defer wg.Done()
		defer teardown()
		relayBytesCounted(b, a, pf, connSeq, protocol.ForwardTapDirection_FromTarget)
	}()
	wg.Wait()
	to, from := pf.connBytes(connSeq)
	pf.closeConn(connSeq, to, from)
	slog.Info("port_forward: splice ended", "task_id", taskIDHex, "conn_seq", connSeq)
}

// relayBytesCounted is relayBytes plus the two observers. It must not allocate
// or block beyond what relayBytes already does — the tap fan-out in Task 5 is
// non-blocking for the same reason.
func relayBytesCounted(src, dst trsf.BidirectionalStream, pf *portForward, connSeq uint64, dir protocol.ForwardTapDirection) {
	for {
		data, eof, err := src.ReadDirect(64 * 1024)
		if err != nil {
			return
		}
		if len(data) > 0 {
			pf.noteBytes(dir, len(data))
			pf.observe(connSeq, dir, data) // no-op until Task 5
			if werr := dst.AppendData(eof, data); werr != nil {
				return
			}
		} else if eof {
			_ = dst.AppendData(true)
		}
		if eof {
			return
		}
	}
}
```

Add to `server/forward_counters.go` the per-connection halves
`spliceBidiCounted` needs — a small map under the registry mutex, because
`conn_close` reports one connection's totals, not the forward's:

```go
// connBytes returns what one connection has carried, for its conn_close
// record. Kept per connection rather than derived from the forward totals,
// which move under every other connection at the same time.
func (pf *portForward) connBytes(seq uint64) (toTarget, fromTarget uint64) {
	if pf == nil {
		return 0, 0
	}
	pf.connMu.Lock()
	defer pf.connMu.Unlock()
	c := pf.conns[seq]
	return c.toTarget, c.fromTarget
}

// observe is the tap fan-out hook. Task 5 fills it in; until then it also
// accumulates the per-connection halves connBytes reads.
func (pf *portForward) observe(seq uint64, dir protocol.ForwardTapDirection, data []byte) {
	if pf == nil || len(data) == 0 {
		return
	}
	pf.connMu.Lock()
	c := pf.conns[seq]
	if dir == protocol.ForwardTapDirection_ToTarget {
		c.toTarget += uint64(len(data))
	} else {
		c.fromTarget += uint64(len(data))
	}
	pf.conns[seq] = c
	pf.connMu.Unlock()
}
```

with, on the struct in `server/port_forward_registry.go`:

```go
	connMu sync.Mutex
	conns  map[uint64]connBytes
```

```go
type connBytes struct{ toTarget, fromTarget uint64 }
```

and `openConn` creating the map lazily. `closeConn` deletes the entry after
reading it, so a long-lived forward does not grow a map entry per connection
forever.

`server/port_forward.go` — both call sites take the counted variant:

```go
	go spliceBidiCounted(clientStream, runnerStream, taskIDHex, pf, connSeq)
```

For the `-R` path (`handleRemoteForwardConn`, `:252`) `pf` is already in hand;
call `pf.openConn()` there for the seq.

`server/port_forward_list.go` — `portForwardInfo` fills the six fields:

```go
	to, from, total, open, last := pf.counters()
	info.BytesToTarget = to
	info.BytesFromTarget = from
	info.ConnsTotal = total
	info.ConnsOpen = open
	info.LastActivityUnixMs = last
	info.Taps = pf.tapCount() // 0 until Task 5; add a stub returning 0 now
```

`cli/format_bytes.go` — moved out of the TUI, because three surfaces now render
byte sizes and a JS copy would drift:

```go
package cli

import "fmt"

// FormatByteCount renders a byte total for a narrow column. Zero renders as
// "0", not "": a forward that has carried nothing is a measurement, and the
// row must not read as if it does not report traffic.
func FormatByteCount(n uint64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fkB", float64(n)/(1<<10))
	case n == 0:
		return "0"
	}
	return fmt.Sprintf("%dB", n)
}
```

Delete `formatByteCount` from `tui/rawforward.go` and call `cli.FormatByteCount`
at its use site (the int there becomes a uint64 conversion).

`cli/port_forward_list.go` — extend the row and the JSON struct:

```go
	lines = append(lines, fmt.Sprintf("  %-6s  %-3s  %-12s  %-40s  %s", "ID", "DIR", "TASK", "SPEC", "ORIGIN"))
	for i := range fs {
		fi := &fs[i]
		lines = append(lines, fmt.Sprintf("  %-6d  %-3s  %-12s  %-40s  %s",
			fi.ForwardId, PortForwardDirFlag(fi.Direction), principalShort(fi.TaskId.Id[:]),
			PortForwardSpecString(fi), PortForwardOrigin(fi)))
		lines = append(lines, "      "+PortForwardTrafficLine(fi))
	}
```

```go
// PortForwardTrafficLine is the traffic half of a forward's row, on its own
// line so the spec/origin columns keep their widths. Exported because the TUI
// detail view and the WebUI row render the same values.
func PortForwardTrafficLine(fi *protocol.PortForwardInfo) string {
	last := "never"
	if fi.LastActivityUnixMs != 0 {
		last = time.Since(time.UnixMilli(int64(fi.LastActivityUnixMs))).Truncate(time.Second).String() + " ago"
	}
	return fmt.Sprintf("conns=%d/%d  to-target=%s  from-target=%s  last=%s  taps=%d",
		fi.ConnsOpen, fi.ConnsTotal,
		FormatByteCount(fi.BytesToTarget), FormatByteCount(fi.BytesFromTarget),
		last, fi.Taps)
}
```

and on `portForwardJSON`, after `OriginCid`:

```go
	BytesToTarget      uint64 `json:"bytes_to_target"`
	BytesFromTarget    uint64 `json:"bytes_from_target"`
	ConnsTotal         uint64 `json:"conns_total"`
	ConnsOpen          uint32 `json:"conns_open"`
	Taps               uint16 `json:"taps"`
	LastActivityUnixMs uint64 `json:"last_activity_unix_ms"`
```

populated in `PortForwardInfoJSONLine`.

- [ ] **Step 4: Run the tests**

```bash
go test ./server/ ./cli/ ./tui/ -run 'Splice|PortForward|Forward|FormatByte' -v
make vet && make check
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add server/task_handler.go server/port_forward.go server/port_forward_list.go server/port_forward_registry.go server/forward_counters.go cli/format_bytes.go cli/port_forward_list.go tui/rawforward.go server/port_forward_test.go cli/port_forward_list_test.go
git commit -m "feat(forward): the listing says what the forward has carried"
```

---

### Task 5: The tap fan-out

**Files:**
- Create: `server/forward_tap.go`
- Modify: `server/port_forward_registry.go` (tap slice on the entry)
- Modify: `server/forward_counters.go` (`observe` gains the fan-out; `tapCount` becomes real)
- Modify: `server/port_forward_list.go` (`teardownPortForward` closes taps)
- Test: `server/forward_tap_test.go`

**Interfaces:**
- Consumes: Task 4's `observe` hook.
- Produces: on `*portForward` — `addTap(t *forwardTap)`, `removeTap(t *forwardTap)`, `tapCount() uint16`, `closeTaps(reason protocol.PortForwardCloseReason)`;
  `newForwardTap(stream trsf.BidirectionalStream, filter protocol.ForwardTapFilter, maxRecordBytes uint32) *forwardTap`;
  `(*forwardTap).run(ctx)`.

- [ ] **Step 1: Write the failing test**

Create `server/forward_tap_test.go`:

```go
package server

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestTapReceivesBothDirections(t *testing.T) {
	pf := newTestForward()
	tap, recs := newTestTap(t, pf, protocol.ForwardTapFilter_Both, 0)
	defer pf.removeTap(tap)

	seq := pf.openConn()
	pf.observe(seq, protocol.ForwardTapDirection_ToTarget, []byte("ping"))
	pf.observe(seq, protocol.ForwardTapDirection_FromTarget, []byte("pong"))

	got := drain(t, recs, 2)
	if string(got[0].Data().Data) != "ping" || got[0].Data().Direction != protocol.ForwardTapDirection_ToTarget {
		t.Fatalf("first record: %+v", got[0].Data())
	}
	if string(got[1].Data().Data) != "pong" {
		t.Fatalf("second record: %+v", got[1].Data())
	}
}

func TestTapStreamOffsetIsCumulativePerDirection(t *testing.T) {
	pf := newTestForward()
	tap, recs := newTestTap(t, pf, protocol.ForwardTapFilter_Both, 0)
	defer pf.removeTap(tap)

	seq := pf.openConn()
	pf.observe(seq, protocol.ForwardTapDirection_ToTarget, []byte("abcd"))
	pf.observe(seq, protocol.ForwardTapDirection_FromTarget, []byte("xy"))
	pf.observe(seq, protocol.ForwardTapDirection_ToTarget, []byte("ef"))

	got := drain(t, recs, 3)
	if got[0].Data().StreamOffset != 0 {
		t.Fatalf("first to_target offset %d", got[0].Data().StreamOffset)
	}
	if got[1].Data().StreamOffset != 0 {
		t.Fatalf("from_target has its own offset space, got %d", got[1].Data().StreamOffset)
	}
	if got[2].Data().StreamOffset != 4 {
		t.Fatalf("second to_target offset %d, want 4", got[2].Data().StreamOffset)
	}
}

func TestTapFilterDropsTheOtherDirection(t *testing.T) {
	pf := newTestForward()
	tap, recs := newTestTap(t, pf, protocol.ForwardTapFilter_ToTarget, 0)
	defer pf.removeTap(tap)

	seq := pf.openConn()
	pf.observe(seq, protocol.ForwardTapDirection_FromTarget, []byte("ignored"))
	pf.observe(seq, protocol.ForwardTapDirection_ToTarget, []byte("kept"))

	got := drain(t, recs, 1)
	if string(got[0].Data().Data) != "kept" {
		t.Fatalf("filter let the wrong direction through: %q", got[0].Data().Data)
	}
}

func TestTapTruncatesAndReportsTheCut(t *testing.T) {
	pf := newTestForward()
	tap, recs := newTestTap(t, pf, protocol.ForwardTapFilter_Both, 4)
	defer pf.removeTap(tap)

	seq := pf.openConn()
	pf.observe(seq, protocol.ForwardTapDirection_ToTarget, []byte("0123456789"))
	pf.observe(seq, protocol.ForwardTapDirection_ToTarget, []byte("ab"))

	got := drain(t, recs, 2)
	if string(got[0].Data().Data) != "0123" || got[0].Data().TruncatedBytes != 6 {
		t.Fatalf("truncation: %q cut=%d", got[0].Data().Data, got[0].Data().TruncatedBytes)
	}
	if got[1].Data().StreamOffset != 10 {
		t.Fatalf("offset must count the CUT bytes too, got %d want 10", got[1].Data().StreamOffset)
	}
}

// The load-bearing one: a tap that cannot keep up must neither block the
// relay nor disappear. It gets a gap record and stays.
func TestSlowTapGetsAGapAndSurvives(t *testing.T) {
	pf := newTestForward()
	tap := newForwardTap(nil, protocol.ForwardTapFilter_Both, 0) // nil stream: nothing drains it
	pf.addTap(tap)
	defer pf.removeTap(tap)

	seq := pf.openConn()
	done := make(chan struct{})
	go func() {
		for i := 0; i < forwardTapQueueDepth*4; i++ {
			pf.observe(seq, protocol.ForwardTapDirection_ToTarget, []byte("0123456789"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("observe blocked on a tap that is not draining — the relay would stall")
	}
	if pf.tapCount() != 1 {
		t.Fatal("a slow tap was dropped; it must survive with a gap instead")
	}
	if tap.missedBytes() == 0 {
		t.Fatal("overflow was not accounted as missed bytes")
	}
}
```

Add the helpers `newTestForward`, `newTestTap` and `drain` at the bottom of the
same file: `newTestForward` returns `&portForward{forwardID: 1, conns: map[uint64]connBytes{}}`;
`newTestTap` builds a tap whose records go to a channel instead of a stream (an
interface seam on `forwardTap`, see Step 3); `drain` reads n records with a
2-second timeout and `t.Fatal`s otherwise.

- [ ] **Step 2: Run to verify it fails**

```bash
go test ./server/ -run 'TestTap|TestSlowTap' -v
```

Expected: undefined `newForwardTap`, `addTap`, `forwardTapQueueDepth`.

- [ ] **Step 3: Implement**

Create `server/forward_tap.go`:

```go
package server

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// forwardTapQueueDepth bounds one tap's backlog. Same shape as the session
// mux's viewer queue (server/session_mux.go:502) and for the same reason: the
// producer is a relay that must never wait. It differs in what overflow does —
// SessionMux drops the VIEWER, a tap keeps the consumer and reports a gap,
// because a tap is open precisely because something is already wrong and a
// vanished tap reads as "the forward closed".
const forwardTapQueueDepth = 256

// forwardTapSink is where a tap's records go. The stream implementation is the
// real one; tests substitute a channel.
type forwardTapSink interface {
	send(rec *protocol.ForwardTapRecord) error
}

type forwardTap struct {
	filter         protocol.ForwardTapFilter
	maxRecordBytes uint32
	ch             chan *protocol.ForwardTapRecord
	sink           forwardTapSink
	missed         atomic.Uint64
	// offsets are per (conn_seq, direction) and belong to the TAP, not the
	// forward: a tap opened mid-connection still reports the connection's own
	// offsets, so they are seeded from the connection's running totals.
	mu      sync.Mutex
	offsets map[offsetKey]uint64
	closed  atomic.Bool
}

type offsetKey struct {
	seq uint64
	dir protocol.ForwardTapDirection
}

func newForwardTap(sink forwardTapSink, filter protocol.ForwardTapFilter, maxRecordBytes uint32) *forwardTap {
	return &forwardTap{
		filter:         filter,
		maxRecordBytes: maxRecordBytes,
		ch:             make(chan *protocol.ForwardTapRecord, forwardTapQueueDepth),
		sink:           sink,
		offsets:        map[offsetKey]uint64{},
	}
}

func (t *forwardTap) missedBytes() uint64 { return t.missed.Load() }

func (t *forwardTap) wants(dir protocol.ForwardTapDirection) bool {
	switch t.filter {
	case protocol.ForwardTapFilter_ToTarget:
		return dir == protocol.ForwardTapDirection_ToTarget
	case protocol.ForwardTapFilter_FromTarget:
		return dir == protocol.ForwardTapDirection_FromTarget
	}
	return true
}

// offer is called from the relay goroutine. It copies what it keeps (the relay
// reuses its buffer), never blocks, and never returns an error the relay would
// have to handle.
func (t *forwardTap) offer(seq uint64, dir protocol.ForwardTapDirection, data []byte) {
	if t.closed.Load() || !t.wants(dir) {
		return
	}
	key := offsetKey{seq: seq, dir: dir}
	t.mu.Lock()
	offset := t.offsets[key]
	t.offsets[key] = offset + uint64(len(data))
	t.mu.Unlock()

	keep := data
	var cut uint32
	if t.maxRecordBytes > 0 && uint32(len(keep)) > t.maxRecordBytes {
		cut = uint32(len(keep)) - t.maxRecordBytes
		keep = keep[:t.maxRecordBytes]
	}
	payload := make([]byte, len(keep))
	copy(payload, keep)

	var d protocol.ForwardTapData
	d.ConnSeq = seq
	d.Direction = dir
	d.StreamOffset = offset
	d.TruncatedBytes = cut
	d.SetData(payload)
	rec := &protocol.ForwardTapRecord{Kind: protocol.ForwardTapRecordKind_Data, UnixMs: uint64(time.Now().UnixMilli())}
	rec.SetData(d)

	select {
	case t.ch <- rec:
	default:
		t.missed.Add(uint64(len(data)))
	}
}

// emit queues a non-data record. Same non-blocking rule; a dropped conn_open is
// accounted as zero missed bytes because none were missed — only the bracket.
func (t *forwardTap) emit(rec *protocol.ForwardTapRecord) {
	if t.closed.Load() {
		return
	}
	select {
	case t.ch <- rec:
	default:
	}
}

// run drains the queue onto the sink until ctx ends or the sink fails. Before
// each record it flushes any accumulated overflow as a gap, so the consumer
// learns what it missed at the point it missed it.
func (t *forwardTap) run(ctx context.Context, seqForGap uint64, dir protocol.ForwardTapDirection) {
	defer t.closed.Store(true)
	for {
		select {
		case <-ctx.Done():
			return
		case rec := <-t.ch:
			if missed := t.missed.Swap(0); missed > 0 {
				var g protocol.ForwardTapGap
				g.ConnSeq = seqForGap
				g.Direction = dir
				g.DroppedBytes = missed
				gap := &protocol.ForwardTapRecord{Kind: protocol.ForwardTapRecordKind_Gap, UnixMs: uint64(time.Now().UnixMilli())}
				gap.SetGap(g)
				if err := t.sink.send(gap); err != nil {
					return
				}
			}
			if err := t.sink.send(rec); err != nil {
				slog.Debug("forward tap: sink ended", "err", err)
				return
			}
		}
	}
}
```

The gap record's `conn_seq`/`direction` come from the record that follows it —
adjust `run` to read them off `rec` rather than taking them as parameters if
that reads better; the test only requires that a gap appears with the missed
count.

On `portForward` in `server/port_forward_registry.go`:

```go
	tapMu sync.Mutex
	taps  []*forwardTap
```

and in `server/forward_counters.go`, `observe` gains the fan-out after its
per-connection accounting:

```go
	pf.tapMu.Lock()
	taps := pf.taps
	pf.tapMu.Unlock()
	for _, t := range taps {
		t.offer(seq, dir, data)
	}
```

plus `addTap` / `removeTap` / `tapCount` / `closeTaps`. `closeTaps` emits one
`forward_closed` record to each tap and marks them closed; call it from
`teardownPortForward` (`server/port_forward_list.go:127`) beside the existing
`pushPortForwardClosed`, so a tapper learns why rather than seeing a bare EOF.

Also emit `conn_open` from `openConn`'s caller (both splice sites, which know
the target host and port) and `conn_close` from `spliceBidiCounted`'s tail,
carrying the two totals `connBytes` returns.

- [ ] **Step 4: Run the tests**

```bash
go test ./server/ -run 'TestTap|TestSlowTap|TestForwardCounters' -v -race
```

Expected: PASS. Run with `-race`: this task adds concurrent access to the tap
slice from every relay goroutine.

- [ ] **Step 5: Commit**

```bash
git add server/forward_tap.go server/forward_counters.go server/port_forward_registry.go server/port_forward_list.go server/forward_tap_test.go
git commit -m "feat(forward): a tap sees the bytes, and says what it missed"
```

---

### Task 6: The OpenForwardTap handler and its gate

**Files:**
- Modify: `server/capabilities.go` (`requiredCap` map, ~line 37, and its doc comment)
- Modify: `server/task_handler.go` (dispatch switch, beside the other port-forward cases)
- Create: `server/forward_tap_handler.go`
- Test: `server/forward_tap_handler_test.go`, `server/capabilities_test.go`

**Interfaces:**
- Consumes: Task 5's `newForwardTap`, `addTap`, `run`.
- Produces: `(*TaskHandler).handleOpenForwardTap(conn ConnHandle, req *protocol.OpenForwardTapRequest, connID string) protocol.OpenForwardTapResponse`.

- [ ] **Step 1: Write the failing tests**

Create `server/forward_tap_handler_test.go`:

```go
package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestOpenForwardTapUnknownIDIsNoSuchForward(t *testing.T) {
	h, conn, _ := newRunningTaskHandler(t)
	got := h.handleOpenForwardTap(conn, &protocol.OpenForwardTapRequest{ForwardId: 999}, conn.ConnectionID().String())
	if got.Status != protocol.OpenForwardTapStatus_NoSuchForward {
		t.Fatalf("status %v", got.Status)
	}
	if got.StreamId != 0 {
		t.Fatal("a refused tap must not leave a stream behind")
	}
}

func TestOpenForwardTapInvisibleForwardIsNotAnOracle(t *testing.T) {
	h, conn, _ := newRunningTaskHandler(t)
	pf := registerTestForward(t, h, "other-conn-id") // owned by a connection this caller cannot see
	got := h.handleOpenForwardTap(conn, &protocol.OpenForwardTapRequest{ForwardId: pf.forwardID}, "confined-cid")
	if got.Status != protocol.OpenForwardTapStatus_NoSuchForward {
		t.Fatalf("an invisible forward must answer no_such_forward, not a distinguishable error: %v", got.Status)
	}
}

func TestOpenForwardTapCountsOnTheListing(t *testing.T) {
	h, conn, _ := newRunningTaskHandler(t)
	pf := registerTestForward(t, h, conn.ConnectionID().String())
	got := h.handleOpenForwardTap(conn, &protocol.OpenForwardTapRequest{ForwardId: pf.forwardID}, conn.ConnectionID().String())
	if got.Status != protocol.OpenForwardTapStatus_Ok {
		t.Fatalf("status %v", got.Status)
	}
	if portForwardInfo(pf).Taps != 1 {
		t.Fatal("taps= must report the open tap; a forward must not be watchable invisibly")
	}
}
```

Append to `server/capabilities_test.go`:

```go
func TestOpenForwardTapNeedsForwardTapCap(t *testing.T) {
	want, gated := requiredCap[protocol.TaskControlKind_OpenForwardTap]
	if !gated {
		t.Fatal("open_forward_tap is not in requiredCap: reading a forward's payload would be ungated")
	}
	if want != protocol.Capability_ForwardTap {
		t.Fatalf("gated on %v, want forward_tap", want)
	}
	if want&protocol.Capability_ForwardLocal != 0 {
		t.Fatal("forward_local must not satisfy a tap")
	}
}
```

- [ ] **Step 2: Run to verify they fail**

```bash
go test ./server/ -run 'TestOpenForwardTap' -v
```

Expected: undefined `handleOpenForwardTap`; the cap test fails on the missing map entry.

- [ ] **Step 3: Implement**

`server/capabilities.go` — add to the map and extend its doc comment to say why
this one is in the map while `KillPortForward`'s gate is inline:

```go
	// OpenForwardTap sits here, not inline like KillPortForward's, because it
	// is direction-INDEPENDENT: reading a -R forward's payload is the same
	// power as reading a -L one, so the cap is known without the registry
	// lookup. The VISIBILITY half still happens in the handler.
	protocol.TaskControlKind_OpenForwardTap: protocol.Capability_ForwardTap,
```

Create `server/forward_tap_handler.go`:

```go
package server

import (
	"context"
	"log/slog"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// handleOpenForwardTap attaches a tap to one forward and answers with the
// stream its records arrive on. The capability half of the gate already ran
// before dispatch (requiredCap); this is the visibility half.
//
// An id the caller cannot see answers no_such_forward — the same answer an
// unknown id gets, so the two cannot be differenced into an existence oracle.
// Same rule KillPortForward follows.
func (h *TaskHandler) handleOpenForwardTap(conn ConnHandle, req *protocol.OpenForwardTapRequest, connID string) protocol.OpenForwardTapResponse {
	errResp := func(s protocol.OpenForwardTapStatus) protocol.OpenForwardTapResponse {
		return protocol.OpenForwardTapResponse{Status: s}
	}
	pf, ok := h.pforwards().get(req.ForwardId)
	if !ok || !h.forwardVisibleTo(connID, pf) {
		return errResp(protocol.OpenForwardTapStatus_NoSuchForward)
	}
	if conn == nil {
		slog.Error("forward tap: nil client conn (programmer error)")
		return errResp(protocol.OpenForwardTapStatus_InternalError)
	}
	stream := conn.CreateBidirectionalStream()
	if stream == nil {
		return errResp(protocol.OpenForwardTapStatus_InternalError)
	}
	tap := newForwardTap(&streamTapSink{stream: stream}, req.DirectionFilter, req.MaxRecordBytes)
	pf.addTap(tap)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer cancel()
		defer pf.removeTap(tap)
		tap.run(ctx)
	}()
	return protocol.OpenForwardTapResponse{
		Status:   protocol.OpenForwardTapStatus_Ok,
		StreamId: uint64(stream.ID()),
	}
}
```

with `streamTapSink` encoding one record and appending it to the stream. Factor
`forwardVisibleTo(connID string, pf *portForward) bool` out of
`visiblePortForwards` (`server/port_forward_list.go:66`) so both use one
predicate — do not re-derive the rule here.

Wire the dispatch case in `server/task_handler.go` beside the other port-forward
cases, following their exact response-sending form.

- [ ] **Step 4: Run the tests**

```bash
go test ./server/ -run 'TestOpenForwardTap|TestForwardTap|Capabilit' -v -race
make vet && make check
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add server/capabilities.go server/task_handler.go server/forward_tap_handler.go server/forward_tap_handler_test.go server/capabilities_test.go
git commit -m "feat(forward): tapping a forward is its own capability"
```

---

### Task 7: The client reads and renders tap records

**Files:**
- Create: `cli/forward_tap.go`
- Create: `cli/forward_tap_render.go`
- Test: `cli/forward_tap_render_test.go`

**Interfaces:**
- Consumes: Task 1's records, Task 6's handler.
- Produces: `cli.OpenForwardTap(ctx, c *Client, forwardID uint64, opts ForwardTapOpts) (trsf.BidirectionalStream, error)`;
  `cli.ForwardTapOpts{Dir string; MaxRecordBytes uint32}`;
  `cli.ReadForwardTapRecord(st trsf.BidirectionalStream) (*protocol.ForwardTapRecord, error)`;
  `cli.RenderTapRecord(rec *protocol.ForwardTapRecord, mode TapRenderMode) []string`;
  `cli.TapRenderMode` with `TapHex`, `TapText`, `TapRaw`, `TapJSON`;
  `cli.ParseTapFilter(dir string) (protocol.ForwardTapFilter, error)`.

- [ ] **Step 1: Write the failing test**

Create `cli/forward_tap_render_test.go`:

```go
package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func dataRec(seq uint64, dir protocol.ForwardTapDirection, off uint64, cut uint32, payload string) *protocol.ForwardTapRecord {
	var d protocol.ForwardTapData
	d.ConnSeq = seq
	d.Direction = dir
	d.StreamOffset = off
	d.TruncatedBytes = cut
	d.SetData([]byte(payload))
	rec := &protocol.ForwardTapRecord{Kind: protocol.ForwardTapRecordKind_Data, UnixMs: 1756000000000}
	rec.SetData(d)
	return rec
}

func TestRenderHexHeaderAndBody(t *testing.T) {
	lines := RenderTapRecord(dataRec(3, protocol.ForwardTapDirection_ToTarget, 0, 0, "GET /x"), TapHex)
	if len(lines) < 2 {
		t.Fatalf("want a header and a body line, got %v", lines)
	}
	head := lines[0]
	if !strings.HasPrefix(head, "#3 ") {
		t.Fatalf("header must lead with the connection: %q", head)
	}
	if !strings.Contains(head, "->") {
		t.Fatalf("to_target must render as -> (ASCII, and the direction forward ls already uses): %q", head)
	}
	if !strings.Contains(head, "6B") {
		t.Fatalf("header must carry the payload size: %q", head)
	}
	if !strings.Contains(lines[1], "0000") || !strings.Contains(lines[1], "|GET /x") {
		t.Fatalf("body is not xxd layout: %q", lines[1])
	}
}

func TestRenderHexOffsetComesFromTheWire(t *testing.T) {
	lines := RenderTapRecord(dataRec(3, protocol.ForwardTapDirection_ToTarget, 0x1000, 0, "ab"), TapHex)
	if !strings.Contains(lines[1], "1000") {
		t.Fatalf("offset column must be the record's stream_offset, not a local counter: %q", lines[1])
	}
}

func TestRenderHexNamesTheCut(t *testing.T) {
	lines := RenderTapRecord(dataRec(3, protocol.ForwardTapDirection_FromTarget, 0, 1331, "HTTP"), TapHex)
	if !strings.Contains(lines[0], "<-") {
		t.Fatalf("from_target must render as <-: %q", lines[0])
	}
	if !strings.Contains(lines[0], "truncated") {
		t.Fatalf("a truncated record must say so: %q", lines[0])
	}
}

func TestRenderGapAndCloseLines(t *testing.T) {
	var g protocol.ForwardTapGap
	g.ConnSeq = 3
	g.Direction = protocol.ForwardTapDirection_ToTarget
	g.DroppedBytes = 3 << 20
	gap := &protocol.ForwardTapRecord{Kind: protocol.ForwardTapRecordKind_Gap, UnixMs: 1756000000000}
	gap.SetGap(g)
	if line := RenderTapRecord(gap, TapHex)[0]; !strings.Contains(line, "gap") || !strings.Contains(line, "missed") {
		t.Fatalf("gap line: %q", line)
	}

	var cc protocol.ForwardTapConnClose
	cc.ConnSeq = 3
	cc.BytesToTarget = 4198
	cc.BytesFromTarget = 1 << 20
	closed := &protocol.ForwardTapRecord{Kind: protocol.ForwardTapRecordKind_ConnClose, UnixMs: 1756000000000}
	closed.SetConnClose(cc)
	line := RenderTapRecord(closed, TapHex)[0]
	if !strings.Contains(line, "close") || !strings.Contains(line, "->4.1kB") || !strings.Contains(line, "<-1.0MB") {
		t.Fatalf("close line must carry that connection's two totals: %q", line)
	}
}

func TestRenderJSONIsOneObjectPerRecord(t *testing.T) {
	lines := RenderTapRecord(dataRec(3, protocol.ForwardTapDirection_ToTarget, 4096, 2, "hi"), TapJSON)
	if len(lines) != 1 {
		t.Fatalf("one line per record, got %d", len(lines))
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	for _, k := range []string{"kind", "unix_ms", "conn", "dir", "offset", "len", "truncated_bytes", "data"} {
		if _, ok := got[k]; !ok {
			t.Fatalf("JSON contract missing %q: %v", k, got)
		}
	}
	if got["data"].(string) != "aGk=" {
		t.Fatalf("payload must be base64: %v", got["data"])
	}
}

func TestRenderRawIsPayloadOnly(t *testing.T) {
	lines := RenderTapRecord(dataRec(3, protocol.ForwardTapDirection_ToTarget, 0, 0, "hi"), TapRaw)
	if len(lines) != 1 || lines[0] != "hi" {
		t.Fatalf("raw must be the payload and nothing else: %v", lines)
	}
	if got := RenderTapRecord(nonDataRec(), TapRaw); len(got) != 0 {
		t.Fatalf("raw must emit nothing for a non-data record, got %v", got)
	}
}

func TestParseTapFilterRejectsGarbage(t *testing.T) {
	if _, err := ParseTapFilter("inbound"); err == nil {
		t.Fatal("a bad --dir must error, not silently mean both")
	}
	for in, want := range map[string]protocol.ForwardTapFilter{
		"both":        protocol.ForwardTapFilter_Both,
		"to-target":   protocol.ForwardTapFilter_ToTarget,
		"from-target": protocol.ForwardTapFilter_FromTarget,
	} {
		got, err := ParseTapFilter(in)
		if err != nil || got != want {
			t.Fatalf("ParseTapFilter(%q) = %v, %v", in, got, err)
		}
	}
}
```

Add a `nonDataRec()` helper returning a `conn_close` record.

- [ ] **Step 2: Run to verify it fails**

```bash
go test ./cli/ -run 'TestRender|TestParseTapFilter' -v
```

Expected: undefined `RenderTapRecord`, `TapHex`, `ParseTapFilter`.

- [ ] **Step 3: Implement**

`cli/forward_tap_render.go` holds `TapRenderMode`, `RenderTapRecord`, the header
builder, the xxd body and the JSON struct (a struct, not a map — same reason
`portForwardJSON` is one). Header columns, fixed width:

```go
// tapHeader renders the fixed-column header every record gets. ASCII only: a
// Windows console is a first-class client here, and `forward ls` already writes
// its row as "127.0.0.1:8080 -> localhost:3000", so a right arrow already means
// "toward the target" on the surface beside this one.
func tapHeader(connField, kind, ts, detail string) string {
	return fmt.Sprintf("%-4s %-6s %-12s %s", connField, kind, ts, detail)
}
```

`cli/forward_tap.go` holds `ForwardTapOpts`, `OpenForwardTap` (the round-trip,
modelled on `OpenPortForward` at `cli/port_forward.go:81`, including
`peer.WaitForBidirectionalStream`) and `ReadForwardTapRecord` (length-prefixed
read off the stream, then `DecodeExact`).

`OpenForwardTap` takes the long-lived `*Client` rather than dialling: the TUI
and the WebUI both hold one, and a fresh dial there would throw away a
handshake (`feedback_reuse_long_lived_client`).

- [ ] **Step 4: Run the tests**

```bash
go test ./cli/ -run 'TestRender|TestParseTapFilter' -v
make vet && make check && make wasm-check
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cli/forward_tap.go cli/forward_tap_render.go cli/forward_tap_render_test.go
git commit -m "feat(forward): the client reads a tap and renders it four ways"
```

---

### Task 8: `harness-cli forward tap`

**Files:**
- Modify: `cmd/harness-cli/main.go` (the `forward` case, 630-675; usage text 631-636)
- Modify: `cli/caps.go` (`GrantableCaps` ~41, `WriteCaps` ~89, catalog description)
- Test: `cmd/harness-cli/forward_test.go`, `cli/caps_test.go`

**Interfaces:**
- Consumes: Task 7's `OpenForwardTap`, `RenderTapRecord`, `ParseTapFilter`.
- Produces: the `forward tap` sub-verb.

- [ ] **Step 1: Write the failing tests**

Append to `cli/caps_test.go`:

```go
func TestForwardTapIsGrantableAndListed(t *testing.T) {
	found := false
	for _, c := range GrantableCaps() {
		if c == protocol.Capability_ForwardTap {
			found = true
		}
	}
	if !found {
		t.Fatal("forward_tap is not grantable: --caps could never hand it to an agent")
	}
	parsed, err := ParseCaps("forward_tap")
	if err != nil || parsed != protocol.Capability_ForwardTap {
		t.Fatalf("ParseCaps(forward_tap) = %v, %v", parsed, err)
	}
	if !strings.Contains(CapsLabel(protocol.Capability_ForwardTap), "forward_tap") {
		t.Fatal("CapsLabel does not render the bit")
	}
	var sb strings.Builder
	WriteCaps(&sb)
	if !strings.Contains(sb.String(), "forward_tap") {
		t.Fatal("the caps catalog does not document forward_tap")
	}
}
```

Append to `cmd/harness-cli/forward_test.go`:

```go
func TestForwardTapUsageMentionsTheVerb(t *testing.T) {
	out := runCLIExpectExit(t, 2, "forward") // existing helper: captures stderr and exit code
	if !strings.Contains(out, "forward tap") {
		t.Fatalf("bare `forward` usage does not mention tap:\n%s", out)
	}
}

func TestForwardTapRawRequiresDir(t *testing.T) {
	out := runCLIExpectExit(t, 2, "forward", "tap", "7", "--raw")
	if !strings.Contains(out, "--dir") {
		t.Fatalf("--raw without --dir must say why it is refused:\n%s", out)
	}
}
```

If `runCLIExpectExit` does not exist, add it to `cmd/harness-cli/forward_test.go`
following the existing test's process-invocation pattern.

- [ ] **Step 2: Run to verify they fail**

```bash
go test ./cli/ -run TestForwardTapIsGrantable -v
go test ./cmd/harness-cli/ -run TestForwardTap -v
```

- [ ] **Step 3: Implement**

`cli/caps.go` — add `protocol.Capability_ForwardTap` to `GrantableCaps()` in bit
order (after `forward_remote` reads best for a human even though the bit is
higher; keep the slice in the order `WriteCaps` prints) and give it a catalog
description naming what it exposes:

```go
	case protocol.Capability_ForwardTap:
		return "read the payload crossing a port forward (cleartext: headers, credentials)"
```

`cmd/harness-cli/main.go` — add to the usage block:

```go
		fmt.Fprintln(os.Stderr, "       harness-cli forward tap <forward-id> [--dir to-target|from-target|both] [--max-bytes N] [--hex|--text|--raw|--json]")
```

and the sub-verb, beside `ls` and `kill`:

```go
		case "tap":
			if len(args) < 2 {
				fmt.Fprintln(os.Stderr, "usage: harness-cli forward tap <forward-id> [--dir …] [--max-bytes N] [--hex|--text|--raw|--json]")
				os.Exit(2)
			}
			id, perr := strconv.ParseUint(args[1], 10, 64)
			if perr != nil {
				die(fmt.Errorf("forward tap: bad forward id %q", args[1]))
			}
			fs := flag.NewFlagSet("forward tap", flag.ExitOnError)
			dir := fs.String("dir", "both", "to-target, from-target or both")
			maxBytes := fs.Uint("max-bytes", 0, "cut each record's payload to this many bytes (0 = whole payload)")
			asHex := fs.Bool("hex", false, "hexdump body (default)")
			asText := fs.Bool("text", false, "printable body, no offset column")
			asRaw := fs.Bool("raw", false, "payload bytes only; requires --dir")
			asJSON := fs.Bool("json", false, "one JSON object per record")
			if err := fs.Parse(args[2:]); err != nil {
				die(err)
			}
			mode, merr := tapMode(*asHex, *asText, *asRaw, *asJSON)
			if merr != nil {
				fmt.Fprintln(os.Stderr, merr)
				os.Exit(2)
			}
			if mode == cli.TapRaw && *dir == "both" {
				fmt.Fprintln(os.Stderr, "forward tap: --raw needs an explicit --dir; two directions concatenated into one stdout is not a stream any decoder can read")
				os.Exit(2)
			}
			filter, ferr := cli.ParseTapFilter(*dir)
			if ferr != nil {
				die(ferr)
			}
			tctx, cancel := interruptContext("forward tap", ctx)
			defer cancel()
			if err := cli.RunForwardTap(tctx, parseCID(), id, cli.ForwardTapOpts{
				Filter: filter, MaxRecordBytes: uint32(*maxBytes), Mode: mode,
			}, os.Stdout); err != nil {
				die(err)
			}
			return
```

Add `tapMode` (rejects two mode flags at once) and `cli.RunForwardTap` (dial,
open, loop `ReadForwardTapRecord` → `RenderTapRecord` → write) in
`cli/forward_tap.go`.

- [ ] **Step 4: Run the tests**

```bash
go test ./cli/ ./cmd/harness-cli/ -run 'ForwardTap|Caps' -v
make vet && make check
```

- [ ] **Step 5: Commit**

```bash
git add cmd/harness-cli/main.go cli/caps.go cli/forward_tap.go cli/caps_test.go cmd/harness-cli/forward_test.go
git commit -m "feat(forward): forward tap, and the capability that gates it"
```

---

### Task 9: TUI — counters on the forwards pane

**Files:**
- Modify: `tui/portforward.go` (`ForwardsModal` columns ~421-428, row build, `SetSize` 439)
- Test: `tui/portforward_test.go`

**Interfaces:**
- Consumes: `cli.FormatByteCount`, the new `PortForwardInfo` fields.
- Produces: nothing later tasks need.

- [ ] **Step 1: Write the failing test**

Append to `tui/portforward_test.go`:

```go
func TestForwardsModalShowsTrafficIncludingZero(t *testing.T) {
	var fi protocol.PortForwardInfo
	fi.ForwardId = 7
	fi.SetBindAddr([]byte("127.0.0.1"))
	fi.BindPort = 8080
	fi.SetTargetHost([]byte("localhost"))
	fi.TargetPort = 3000
	fi.SetOriginCid([]byte("ws:abc"))

	m := NewForwardsModal()
	m.SetSize(200, 24)
	m.SetRows([]protocol.PortForwardInfo{fi})
	view := m.View()
	for _, want := range []string{"conns", "0/0", "taps"} {
		if !strings.Contains(view, want) {
			t.Fatalf("forwards pane hides a zero measurement (%q):\n%s", want, view)
		}
	}
}

// The columns and the rows must swap together: bubbles' renderRow indexes
// row[i] per COLUMN, so a moment holding N columns against N-1-cell rows
// panics. Resizing must also not move the selection.
func TestForwardsModalResizeKeepsRowsAndCursor(t *testing.T) {
	m := NewForwardsModal()
	m.SetSize(200, 24)
	m.SetRows(threeTestForwards(t))
	m.MoveCursor(2)
	for _, w := range []int{60, 200, 40, 120} {
		m.SetSize(w, 24)
		_ = m.View() // must not panic
	}
	if m.Cursor() != 2 {
		t.Fatalf("resize moved the selection to %d", m.Cursor())
	}
}
```

- [ ] **Step 2: Run to verify it fails**

```bash
go test ./tui/ -run TestForwardsModal -v
```

- [ ] **Step 3: Implement**

Add the traffic columns to `baseCols` and build their cells in the same function
that builds the existing ones. If the column SET becomes width-conditional, add
an `applyColumns` helper that empties the rows, sets the columns, rebuilds and
restores the cursor — and route every column change through it. Do not "always
emit the widest row": the traffic columns sit before `origin`, so a stale extra
cell would render every following value under the wrong header.

- [ ] **Step 4: Run the tests**

```bash
go test ./tui/ -run TestForwardsModal -v
make check
```

- [ ] **Step 5: Commit**

```bash
git add tui/portforward.go tui/portforward_test.go
git commit -m "feat(tui): the forwards pane reports traffic, zeros included"
```

---

### Task 10: TUI — the tap view

**Files:**
- Create: `tui/forwardtap.go`
- Modify: `tui/keys.go` (`mainKeyMap` + the `mainKeyBindings` row — `keys_test` enforces the pair)
- Modify: `tui/app.go` (dispatch, the pump goroutine)
- Modify: `tui/cmdline.go` (`forward tap <id>` verb)
- Test: `tui/forwardtap_test.go`, `tui/cmdline_test.go`

**Interfaces:**
- Consumes: `cli.OpenForwardTap`, `cli.RenderTapRecord`, `a.client`.
- Produces: nothing later tasks need.

- [ ] **Step 1: Write the failing tests**

Create `tui/forwardtap_test.go`:

```go
func TestForwardTapViewKeepsItsHeaderWhenFull(t *testing.T) {
	v := NewForwardTapView(7)
	v.SetSize(120, 20)
	for i := 0; i < 500; i++ {
		v.Append([]string{fmt.Sprintf("#1 ->     12:00:%02d.000  4B", i%60)})
	}
	out := v.View()
	if !strings.Contains(out, "forward #7") {
		t.Fatalf("the view scrolled its own header away — the exec listing's mistake:\n%s", out)
	}
}

func TestForwardTapViewIsItsOwnView(t *testing.T) {
	if NewForwardTapView(7).Height(24) < 10 {
		t.Fatal("the tap must get a real viewport, not a five-line strip")
	}
}
```

Append to `tui/cmdline_test.go`:

```go
func TestCmdlineParsesForwardTap(t *testing.T) {
	act, err := parseCommand("forward tap 7 --dir to-target --max-bytes 64")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ta, ok := act.(ForwardTapAction)
	if !ok {
		t.Fatalf("action type %T", act)
	}
	if ta.ForwardID != 7 || ta.Dir != "to-target" || ta.MaxRecordBytes != 64 {
		t.Fatalf("parsed %+v", ta)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

```bash
go test ./tui/ -run 'ForwardTap|CmdlineParsesForwardTap' -v
```

- [ ] **Step 3: Implement**

`tui/forwardtap.go` — a viewport-backed view with a pinned header line, an
append that trims from the front, and `SetSize` deriving the frame from the
terminal size (never from content).

`tui/app.go` — the pump reads records with `cli.ReadForwardTapRecord` on
`a.client` (NOT a fresh dial — every `Do*` in this file threads `a.client`, and
a `Dial`+`Close` here would throw away a handshake) and posts rendered lines as
a tea.Msg.

`tui/keys.go` — add the key to `mainKeyMap` AND its `mainKeyBindings` row; the
existing `keys_test` fails if only one is added.

- [ ] **Step 4: Run the tests**

```bash
go test ./tui/ -run 'ForwardTap|Cmdline|Keys' -v
make check
```

- [ ] **Step 5: Commit**

```bash
git add tui/forwardtap.go tui/keys.go tui/app.go tui/cmdline.go tui/forwardtap_test.go tui/cmdline_test.go
git commit -m "feat(tui): a tap gets a view of its own"
```

---

### Task 11: WebUI — counters in the row and the snapshot

**Files:**
- Modify: `cmd/harness-webui-wasm/main.go` (the forwards map, 945-965; the doc comment at 771)
- Modify: `webui/static/main.js` (`renderForwardList` 953-997)
- Modify: `webui/index.html` / the stylesheet if a cell needs a class
- Test: `cmd/harness-webui-wasm/` snapshot test if one exists; otherwise a `cli` round-trip test for the map keys

**Interfaces:**
- Consumes: the new `PortForwardInfo` fields.
- Produces: snapshot keys `bytes_to_target`, `bytes_from_target`, `conns_total`, `conns_open`, `taps`, `last_activity_unix_ms` (raw numbers) plus `traffic` (the string from `cli.PortForwardTrafficLine`).

- [ ] **Step 1: Write the failing test**

Append to the wasm package's test file (create `cmd/harness-webui-wasm/snapshot_test.go` if absent):

```go
func TestForwardSnapshotCarriesRawCountersAndLabel(t *testing.T) {
	var fi protocol.PortForwardInfo
	fi.ForwardId = 7
	fi.SetBindAddr([]byte("127.0.0.1"))
	fi.SetTargetHost([]byte("localhost"))
	fi.SetOriginCid([]byte("ws:abc"))
	fi.BytesToTarget = 1 << 20
	fi.Taps = 2

	m := forwardSnapshotRow(&fi) // the map builder, factored out of the js.FuncOf body
	for _, k := range []string{"bytes_to_target", "bytes_from_target", "conns_total", "conns_open", "taps", "last_activity_unix_ms", "traffic"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("snapshot row missing %q: %v", k, m)
		}
	}
	if m["bytes_to_target"].(float64) != float64(1<<20) {
		t.Fatalf("raw value must be a number the dialog can re-read, got %T", m["bytes_to_target"])
	}
}
```

- [ ] **Step 2: Run to verify it fails**

```bash
GOOS=js GOARCH=wasm go test ./cmd/harness-webui-wasm/ -run TestForwardSnapshot -v
```

If that cannot run in this environment, factor `forwardSnapshotRow` into a file
without a `js` build tag and test it under the host target instead — the map
builder has no `syscall/js` dependency.

- [ ] **Step 3: Implement**

Factor the row map out of the snapshot closure into `forwardSnapshotRow`, add
the six raw numbers plus the rendered `traffic` string (from
`cli.PortForwardTrafficLine`, so the browser does not re-derive the format).
`renderForwardList` gains the cells; keep the existing `forward-cell` class so
the mobile rules apply, and add the traffic as a second line inside the row
rather than a sixth column — five columns already fill 390px.

- [ ] **Step 4: Verify in a browser**

```bash
make build
scripts/dummy-harness.sh up --detach --agent fake --name TAPUI
```

Open the WebUI, create a forward, and confirm the row shows `conns=0/0` and
`taps=0` at 390px and at desktop width. Screenshot both. Then
`scripts/dummy-harness.sh down`.

- [ ] **Step 5: Commit**

```bash
git add cmd/harness-webui-wasm/main.go webui/static/main.js webui/index.html
git commit -m "feat(webui): the forward row reports traffic"
```

---

### Task 12: WebUI — the tap panel

**Files:**
- Modify: `cmd/harness-webui-wasm/main.go` (a `forwardTap` bridge beside `forwardKill` at 140 / 1397)
- Modify: `webui/static/main.js` (a per-row tap control and its panel)
- Modify: `webui/index.html` + stylesheet (the panel)

**Interfaces:**
- Consumes: `cli.OpenForwardTap`, `cli.RenderTapRecord`, `currentClient()`.
- Produces: `window.harness.forwardTap(forwardId, opts, onLine)` and `window.harness.forwardTapClose(forwardId)`.

- [ ] **Step 1: Write the bridge**

`harnessForwardTap` follows `harnessRawOpen`'s shape (`cmd/harness-webui-wasm/main.go:1430`):
a pane-keyed slot, a goroutine reading records off the stream, each rendered
line handed to a JS callback. It uses `currentClient()` — the long-lived client
— exactly as `harnessForwardKill` does.

- [ ] **Step 2: Write the panel**

A `tap` button on each forward row opens a `<pre>` panel in the dark palette
(`#1e1e1e` / `#d4d4d4`), monospace, `overflow-x: auto` so a long hex line
scrolls inside the panel rather than the page. Build the control on the
`<details>` `toggle` as well as on snapshot arrival, so an operator who opens
the section before the first poll does not find it blank.

- [ ] **Step 3: Verify in a browser**

```bash
make build
scripts/dummy-harness.sh up --detach --agent fake --name TAPUI
```

Forward a port to something that talks (the dummy harness's own HTTP endpoint
is enough), tap it from the WebUI, drive traffic, and confirm lines appear.
Check 390px and desktop. Screenshot both, and name the paths in the report —
they are deliverables, not scratch files. Then `scripts/dummy-harness.sh down`.

- [ ] **Step 4: Run the checks**

```bash
make wasm-check && make check && make vet
```

- [ ] **Step 5: Commit**

```bash
git add cmd/harness-webui-wasm/main.go webui/static/main.js webui/index.html
git commit -m "feat(webui): a forward can be tapped from its row"
```

---

### Task 13: Docs, the parity walk, and landing

**Files:**
- Modify: `README.md` (port-forward section, the TUI cmdline verb list, the capabilities section)
- Modify: `docs/superpowers/specs/2026-08-29-port-forward-tap-design.md` (amendments, if anything shipped differently)
- Modify: `.claude/skills/surface-parity-checklist/firing-log.md`

- [ ] **Step 1: Walk the checklist**

Walk `surface-parity-checklist` items **1 through 39 in order**, recording
`done` / `n/a` / `omitted` with a reason for each. Do not summarise. S1-S6 are
`n/a` here (no agent added or changed) — say so once, with that reason.

- [ ] **Step 2: Walk the spec's own Surfaces table (item 39)**

Open the spec's Surfaces table and check each row against the code, one row at a
time. A row you decided not to build gets struck through in the spec with its
reason, so the table stops claiming it.

- [ ] **Step 3: Update the README**

The port-forward section gains the traffic columns and `forward tap`; the
capabilities section gains `forward_tap`; the TUI cmdline verb list gains
`forward tap`.

- [ ] **Step 4: Record the walk**

Add one entry to `firing-log.md` listing only the items that came back
`done`/`omitted`.

- [ ] **Step 5: Full verification**

```bash
make vet && make check && make wasm-check && make test
```

Expected: all green. `server` has a known flaky test
(`project_flaky_test_open_interactive_sessionmux`, ~1 in 4 runs, pre-existing) —
re-run before blaming this diff.

- [ ] **Step 6: E2E against a dummy harness**

```bash
scripts/dummy-harness.sh up --detach --agent fake --name TAPE2E
```

Eval its env, then: register a `-L` forward to a local echo server, push known
bytes, confirm `forward ls` reports them, run `forward tap` in another shell and
confirm the bytes appear. Then `scripts/dummy-harness.sh down`.

- [ ] **Step 7: Land**

Follow the `landing-to-main` skill: this repo is Mode A local-trunk. Rebase onto
current `main`, fast-forward `main`, push. Never cherry-pick to the remote.
Then, in the main checkout, `make build` — the binaries under `bin/` do not
refresh on their own, and a wire change means the server and every client must
be restarted together (runners are unaffected; no runner-facing message changed).

```bash
git commit -m "docs(forward): the tap's surfaces, walked and written down"
```

---

## Self-Review

**Spec coverage:** Problem P1 (is it doing anything) → Tasks 2, 4, 9, 11. P2
(what is going over it) → Tasks 5-8, 10, 12. P3 (different moments, different
answers) → the split between always-on counters and an opt-in tap, Tasks 4 and
6. D1-D12 each map to a task: D1→all, D2→4/5, D3→6/8, D4→5, D5→2, D6→1/3,
D7→5, D8→1, D9→1/5, D10→5/6, D11→1, D12→9-12.

**Type consistency:** `noteBytes`/`openConn`/`closeConn`/`counters` (Task 2) are
used with those names in Tasks 4 and 5. `observe` is introduced as a stub in
Task 4 and filled in Task 5 — deliberate, and stated at both ends.
`FormatByteCount` (Task 4) is used in Tasks 7 and 9. `RenderTapRecord` /
`TapHex` / `ParseTapFilter` (Task 7) are used in Tasks 8, 10 and 12.

**Known risk:** if `SetData` collides with a generated codec method on
`ForwardTapData` (`project_bgn_union_arm_name_collision`), Task 1 Step 4 renames
the arm to `payload` and every `Data()` in Tasks 5, 7, 8, 10 and 12 becomes
`Payload()`.
