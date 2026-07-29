# Port-Forward Registry (remote list & kill) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make every harness port forward — including `-L`, whose listener lives inside the client process — enumerable and killable from any client, so a stray `harness-cli forward -L` terminal can be stopped without finding it.

**Architecture:** The client registers each forward with the server after its listener is bound; the server keeps one registry for both directions and holds a per-forward control stream. Listing is a plain server-side read (mirroring `ListConns`), kill is a `closed` record pushed down that control stream, and the stream's EOF deregisters a client that died. `-R` migrates onto the same registration RPC.

**Tech Stack:** Go 1.x, brgen `.bgn` schema codegen (`make protoregen`), objtrsf `trsf` streams, bubbletea (TUI), Go/wasm + vanilla JS (WebUI).

**Spec:** `docs/superpowers/specs/2026-07-30-port-forward-registry-design.md` — read it in full before starting any task.

## Global Constraints

- **Work in this worktree only:** `/home/kforfk/workspace/remote-agent-harness/.harness-worktrees/70fbad4a6eb6f1e992be8a669f1bcefd`. Before writing code, run `git rev-parse --show-toplevel` and confirm it prints exactly that path. **Never** use paths of the form `/home/kforfk/workspace/remote-agent-harness/<rel>` — they resolve to the parent checkout and your work will silently land on another branch.
- **Read `.claude/skills/implementation-pitfalls/SKILL.md` in full before writing code.**
- **`.bgn` is the single source of truth.** Any byte on the wire must be described in `runner/protocol/message.bgn`. Never hand-edit `runner/protocol/message.go` — regenerate with `make protoregen`.
- **The runner protocol does not change.** `RunnerOpenPortForwardRequest`, `ClosePortForwardRequest`, `RemoteForwardConn`, `RemoteForwardBindResult` keep their exact current layout. If a change would alter any of them, stop and report instead of proceeding.
- **Build hygiene:** compile-check with `go build ./...` (writes no binary) or `go build -o /dev/null ./cmd/<x>`. Never bare `go build ./cmd/<x>/`. `git status` must be as clean after your checks as before.
- **Individual dogfood scope:** no external users. No migration shims, no deprecation periods, no backward-compat layers. Delete dead fields rather than leaving them on the wire.
- **Reuse the long-lived client:** every new client helper ships as a pair — `(*Client).XWith(ctx, …)` for TUI/WebUI which already hold a `*cli.Client`, and package-level `X(ctx, serverCID, …)` (dial + defer Close) for the short-lived `harness-cli` binary. TUI/WebUI must call the `*With` form.
- **Commit after every task**, with the task number in the message body.

---

### Task 1: Wire schema + `-R` migration onto it

The complete final schema lands here — additions *and* the removals from
`OpenPortForwardRequest`/`OpenPortForwardResponse` — so a reviewer sees the whole
byte layout in one diff. The `-R` registration path moves onto the new
`RegisterPortForward` RPC in the same task because the fields it used are the
ones being removed; nothing else changes behaviour.

**Files:**
- Modify: `runner/protocol/message.bgn` (schema)
- Modify: `runner/protocol/message.go` (generated — via `make protoregen`, never by hand)
- Modify: `server/remote_forward_registry.go` → rename to `server/port_forward_registry.go`
- Modify: `server/port_forward.go`, `server/task_handler.go`, `server/server.go:811`
- Modify: `cli/port_forward.go`
- Test: `runner/protocol/port_forward_test.go`, `server/port_forward_test.go`

**Interfaces:**
- Consumes: nothing (first task).
- Produces, for later tasks:
  - `protocol.TaskControlKind_RegisterPortForward` / `_ListPortForwards` / `_KillPortForward`
  - `protocol.RegisterPortForwardRequest{TaskId, Direction, BindAddr []byte, BindPort uint16, TargetHost []byte, TargetPort uint16}` with setters `SetBindAddr`/`SetTargetHost`
  - `protocol.RegisterPortForwardResponse{Status protocol.OpenPortForwardStatus, ForwardId, StreamId uint64}`
  - `protocol.PortForwardListQuery{TaskId}`, `protocol.PortForwardListResult{StreamId}`
  - `protocol.PortForwardInfo{ForwardId, Direction, TaskId, BindAddr, BindPort, TargetHost, TargetPort, OriginKind protocol.ClientKind, OriginCid []byte}`
  - `protocol.PortForwardListResultBody` with `SetForwards([]protocol.PortForwardInfo)` and field `Forwards`
  - `protocol.KillPortForwardRequest{ForwardId}`, `protocol.KillPortForwardResponse{Status}` , `protocol.KillPortForwardStatus_Ok/_NoSuchForward/_InternalError`
  - `protocol.PortForwardEvent` (variant on `Kind`), `protocol.PortForwardEventKind_ConnNotify/_Closed`, `protocol.PortForwardClosed{Reason}`, `protocol.PortForwardCloseReason_Killed/_TaskGone`
  - `server.portForward` struct + `(*TaskHandler).pforwards() *portForwardRegistry`
  - `(*Client).RegisterPortForward(ctx context.Context, taskIDHex string, dir protocol.PortForwardDirection, bindAddr string, bindPort int, targetHost string, targetPort int) (trsf.BidirectionalStream, uint64, error)`

- [ ] **Step 1: Read the spec and the current code**

Read `docs/superpowers/specs/2026-07-30-port-forward-registry-design.md` in full,
then `runner/protocol/message.bgn` lines 1195-1290 (the port-forward block),
`server/port_forward.go`, `server/remote_forward_registry.go`, `cli/port_forward.go`.
In your final report, quote the spec's **Problem statement** bullets and say which
ones this task addresses.

- [ ] **Step 2: Add the new schema to `runner/protocol/message.bgn`**

Append the three new kinds to `enum TaskControlKind` **at the end** (ordinals of
existing members must not move):

```
    register_port_forward   # register a -L/-R forward; returns forward_id + control stream
    list_port_forwards      # enumerate the caller's visible registrations
    kill_port_forward       # close one registration by id
```

Add to the `TaskControlRequest` match block:

```
        TaskControlKind.register_port_forward => register_port_forward :RegisterPortForwardRequest
        TaskControlKind.list_port_forwards    => list_port_forwards    :PortForwardListQuery
        TaskControlKind.kill_port_forward     => kill_port_forward     :KillPortForwardRequest
```

and to the `TaskControlResponse` match block:

```
        TaskControlKind.register_port_forward => register_port_forward :RegisterPortForwardResponse
        TaskControlKind.list_port_forwards    => list_port_forwards    :PortForwardListResult
        TaskControlKind.kill_port_forward     => kill_port_forward     :KillPortForwardResponse
```

In the port-forward section (next to `OpenPortForwardRequest`), add:

```
# RegisterPortForwardRequest: one registration, either direction. Direction
# decides what the address pair means:
#   local  : bind_* is the CLIENT's already-bound listener; target_* is what the
#            RUNNER dials per connection.
#   remote : bind_* is the listener the RUNNER is asked to open; target_* is what
#            the CLIENT dials per connection.
format RegisterPortForwardRequest:
    task_id         :TaskID
    direction       :PortForwardDirection
    bind_addr_len   :u16
    bind_addr       :[bind_addr_len]u8
    bind_port       :u16
    target_host_len :u16
    target_host     :[target_host_len]u8
    target_port     :u16

# Reuses OpenPortForwardStatus. A local registration can only answer ok,
# no_such_task or internal_error: its listener is bound client-side before the
# request is sent, and the runner is not consulted.
format RegisterPortForwardResponse:
    status     :OpenPortForwardStatus
    forward_id :u64
    stream_id  :u64   # server-created control stream; 0 on failure

format PortForwardListQuery:
    task_id :TaskID   # all-zero = every forward visible to the caller

format PortForwardListResult:
    stream_id :u64    # server-initiated send-stream carrying PortForwardListResultBody
                      # until EOF (mirrors ConnListResult). 0 = error.

format PortForwardInfo:
    forward_id      :u64
    direction       :PortForwardDirection
    task_id         :TaskID
    bind_addr_len   :u16
    bind_addr       :[bind_addr_len]u8
    bind_port       :u16
    target_host_len :u16
    target_host     :[target_host_len]u8
    target_port     :u16
    origin_kind     :ClientKind          # which surface holds it
    origin_cid_len  :u8
    origin_cid      :[origin_cid_len]u8  # ConnectionID canonical String(), as in ConnInfo.cid;
                                         # the only way to tell two identical specs apart

format PortForwardListResultBody:
    forwards_len :u16
    forwards     :[forwards_len]PortForwardInfo

enum KillPortForwardStatus:
    :u8
    ok              = "ok"
    no_such_forward = "no_such_forward"   # unknown id OR not visible to the caller
                                          # (an invisible id must not become an
                                          # existence oracle for a confined agent)
    internal_error  = "internal_error"

format KillPortForwardRequest:
    forward_id :u64

format KillPortForwardResponse:
    status :KillPortForwardStatus

# --- control stream records (server -> client) ---
enum PortForwardEventKind:
    :u8
    conn_notify   # remote forwards only: dial your local target, pick up stream_id
    closed        # both directions: stop this forward

enum PortForwardCloseReason:
    :u8
    killed     # kill_port_forward was called
    task_gone  # the owning task left Running/Detached

format PortForwardClosed:
    reason :PortForwardCloseReason

format PortForwardEvent:
    kind :PortForwardEventKind
    match kind:
        PortForwardEventKind.conn_notify => conn_notify :RemoteForwardConnNotify
        PortForwardEventKind.closed      => closed      :PortForwardClosed
        .. => error("Unexpected port forward event")
```

Then **slim the two now-overloaded messages** (delete the listed fields — do not
leave them on the wire):

```
format OpenPortForwardRequest:
    task_id         :TaskID
    remote_host_len :u16
    remote_host     :[remote_host_len]u8
    remote_port     :u16
    # direction / bind_addr / bind_port removed: this message now means exactly
    # "open the data stream for one accepted local-forward connection".

format OpenPortForwardResponse:
    status    :OpenPortForwardStatus
    stream_id :u64
    # forward_id removed: it was the -R registration handle, now returned by
    # RegisterPortForwardResponse.
```

Leave `RunnerOpenPortForwardRequest`, `ClosePortForwardRequest`,
`RemoteForwardConn`, `RemoteForwardBindResult` and `RemoteForwardConnNotify`
byte-for-byte unchanged.

- [ ] **Step 3: Regenerate and confirm the tree does not yet build**

Run:

```bash
make protoregen
go build ./... 2>&1 | head -30
```

Expected: `message.go` regenerates, then `go build` FAILS in `server/port_forward.go`
and `cli/port_forward.go` on the removed fields (`req.Direction`, `BindPort`,
`BindAddr`, `r.ForwardId`). That failure list is your migration worklist for
Steps 5-7.

- [ ] **Step 4: Write the failing protocol round-trip test**

Append to `runner/protocol/port_forward_test.go`:

```go
func TestRegisterPortForwardRoundTrip(t *testing.T) {
	var req protocol.RegisterPortForwardRequest
	req.TaskId = protocol.TaskID{Id: [16]byte{1, 2, 3}}
	req.Direction = protocol.PortForwardDirection_Local
	req.SetBindAddr([]byte("127.0.0.1"))
	req.BindPort = 8080
	req.SetTargetHost([]byte("db.internal"))
	req.TargetPort = 5432

	buf := req.MustAppend(nil)
	var got protocol.RegisterPortForwardRequest
	if err := got.DecodeExact(buf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Direction != protocol.PortForwardDirection_Local ||
		string(got.BindAddr) != "127.0.0.1" || got.BindPort != 8080 ||
		string(got.TargetHost) != "db.internal" || got.TargetPort != 5432 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestPortForwardEventVariants(t *testing.T) {
	var notify protocol.PortForwardEvent
	notify.Kind = protocol.PortForwardEventKind_ConnNotify
	notify.SetConnNotify(protocol.RemoteForwardConnNotify{StreamId: 42})
	buf := notify.MustAppend(nil)

	var closed protocol.PortForwardEvent
	closed.Kind = protocol.PortForwardEventKind_Closed
	closed.SetClosed(protocol.PortForwardClosed{Reason: protocol.PortForwardCloseReason_Killed})
	buf = closed.MustAppend(buf)

	// Two events, back to back, decoded from one buffer.
	var a protocol.PortForwardEvent
	rest, err := a.Decode(buf)
	if err != nil {
		t.Fatalf("decode first: %v", err)
	}
	if a.Kind != protocol.PortForwardEventKind_ConnNotify || a.ConnNotify().StreamId != 42 {
		t.Fatalf("first event wrong: %+v", a)
	}
	var b protocol.PortForwardEvent
	if _, err := b.Decode(rest); err != nil {
		t.Fatalf("decode second: %v", err)
	}
	if b.Kind != protocol.PortForwardEventKind_Closed ||
		b.Closed().Reason != protocol.PortForwardCloseReason_Killed {
		t.Fatalf("second event wrong: %+v", b)
	}
}

func TestPortForwardListBodyRoundTrip(t *testing.T) {
	var info protocol.PortForwardInfo
	info.ForwardId = 7
	info.Direction = protocol.PortForwardDirection_Remote
	info.SetBindAddr([]byte("127.0.0.1"))
	info.BindPort = 6001
	info.SetTargetHost([]byte("localhost"))
	info.TargetPort = 6000
	info.OriginKind = protocol.ClientKind_Tui
	info.SetOriginCid([]byte("ws:127.0.0.1:1234-ab"))

	var body protocol.PortForwardListResultBody
	body.SetForwards([]protocol.PortForwardInfo{info})
	buf := body.MustAppend(nil)

	var got protocol.PortForwardListResultBody
	if err := got.DecodeExact(buf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Forwards) != 1 || got.Forwards[0].ForwardId != 7 ||
		string(got.Forwards[0].OriginCid) != "ws:127.0.0.1:1234-ab" {
		t.Fatalf("round-trip mismatch: %+v", got.Forwards)
	}
}
```

- [ ] **Step 5: Generalise the registry**

`git mv server/remote_forward_registry.go server/port_forward_registry.go`, then
rename `remoteForward` → `portForward`, `remoteForwardRegistry` →
`portForwardRegistry`, `newRemoteForwardRegistry` → `newPortForwardRegistry`,
`(*TaskHandler).rforwards()` → `pforwards()`, and the `TaskHandler` fields
`remoteForwards`/`remoteForwardsOnce` → `portForwards`/`portForwardsOnce`
(`server/task_handler.go:51`). Add the entry fields:

```go
type portForward struct {
	forwardID  uint64
	direction  protocol.PortForwardDirection
	taskIDHex  string
	runnerID   string // = TaskEntry.AssignedTo; used to re-find the runner at teardown
	control    trsf.BidirectionalStream
	clientCxn  ConnHandle
	clientCID  string
	clientKind protocol.ClientKind
	bindAddr   string
	bindPort   uint16
	targetHost string
	targetPort uint16
}
```

Add a `list()` accessor returning a `forwardID`-ordered copy (ids are assigned by
`next++`, so id order is creation order):

```go
// list returns the registrations ordered by forwardID (== creation order).
func (r *portForwardRegistry) list() []*portForward {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*portForward, 0, len(r.m))
	for _, pf := range r.m {
		out = append(out, pf)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].forwardID < out[j].forwardID })
	return out
}
```

Extend `remoteForwardInfo`/`snapshot()` with the direction so the trsf debug dump
(`server/server.go:811`) reports both kinds; update that log line's key from
`"trsf dump: remote-forward"` to `"trsf dump: port-forward"` and add `"dir"`.

- [ ] **Step 6: Move registration to the new RPC (server)**

In `server/port_forward.go`: delete the `if req.Direction == …Remote` branch from
`handleOpenPortForward` (it is now local-only) and add the registration handler.
`registerRemoteForward` keeps its body (runner bind request, pending-bind wait,
`watchRemoteForwardControl`) but is called from the new handler and fills the new
entry fields:

```go
// handleRegisterPortForward registers one forward and returns its id plus the
// server-created control stream. Local registrations never contact the runner:
// the listener is already bound client-side, so only task liveness is checked.
func (h *TaskHandler) handleRegisterPortForward(conn ConnHandle, req *protocol.RegisterPortForwardRequest, cid string) protocol.RegisterPortForwardResponse {
	errResp := func(s protocol.OpenPortForwardStatus) protocol.RegisterPortForwardResponse {
		return protocol.RegisterPortForwardResponse{Status: s}
	}
	taskIDHex := hex.EncodeToString(req.TaskId.Id[:])
	task, ok := h.Tasks.Get(taskIDHex)
	if !ok || (task.Status != protocol.TaskStatus_Running && task.Status != protocol.TaskStatus_Detached) {
		return errResp(protocol.OpenPortForwardStatus_NoSuchTask)
	}
	if conn == nil {
		slog.Error("port_forward: nil client conn (programmer error)")
		return errResp(protocol.OpenPortForwardStatus_InternalError)
	}
	pf := &portForward{
		direction:  req.Direction,
		taskIDHex:  taskIDHex,
		runnerID:   task.AssignedTo,
		clientCxn:  conn,
		clientCID:  cid,
		clientKind: h.clientKinds[cid],
		bindAddr:   string(req.BindAddr),
		bindPort:   req.BindPort,
		targetHost: string(req.TargetHost),
		targetPort: req.TargetPort,
	}
	if req.Direction == protocol.PortForwardDirection_Remote {
		runner, ok := h.Registry.Get(task.AssignedTo)
		if !ok || runner.Conn == nil {
			return errResp(protocol.OpenPortForwardStatus_RunnerOffline)
		}
		return h.registerRemoteForward(pf, req, runner)
	}
	ctrl := conn.CreateBidirectionalStream()
	if ctrl == nil {
		return errResp(protocol.OpenPortForwardStatus_InternalError)
	}
	pf.control = ctrl
	fid := h.pforwards().add(pf)
	go h.watchRemoteForwardControl(pf)
	return protocol.RegisterPortForwardResponse{
		Status:   protocol.OpenPortForwardStatus_Ok,
		ForwardId: fid,
		StreamId: uint64(ctrl.ID()),
	}
}
```

Read `h.clientKinds` under whatever mutex `server/task_handler.go` already uses
for it — grep its existing readers first and copy that pattern rather than
touching the map bare.

Wire the dispatch in `server/task_handler.go` next to the existing
`TaskControlKind_OpenPortForward` case, moving the direction-dependent capability
gate onto the new kind (the per-connection `OpenPortForward` keeps requiring
`Capability_ForwardLocal`):

```go
	case protocol.TaskControlKind_RegisterPortForward:
		rf := req.RegisterPortForward()
		if rf == nil {
			slog.Error("TaskHandler: RegisterPortForward variant is nil")
			return
		}
		{
			need := protocol.Capability_ForwardLocal
			if rf.Direction == protocol.PortForwardDirection_Remote {
				need = protocol.Capability_ForwardRemote
			}
			if !hasCap(h.callerCaps(cid), need) {
				h.denyTaskControl(conn, req.Kind, req.RequestId, need)
				return
			}
		}
		rresp := h.handleRegisterPortForward(conn, rf, cid)
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_RegisterPortForward, RequestId: req.RequestId}
		resp.SetRegisterPortForward(rresp)
		out := resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck
```

- [ ] **Step 7: Move registration to the new RPC (client)**

In `cli/port_forward.go`, replace the body of `OpenRemoteForward` with a call to a
new shared helper, and drop the removed fields from `OpenPortForward`:

```go
// RegisterPortForward registers one forward with the server and returns the
// server-created control stream plus the assigned forward id. The caller has
// already bound its listener (local) or is asking the runner to bind (remote).
func (c *Client) RegisterPortForward(ctx context.Context, taskIDHex string, dir protocol.PortForwardDirection,
	bindAddr string, bindPort int, targetHost string, targetPort int) (trsf.BidirectionalStream, uint64, error) {
	tid, err := parseTaskIDHex(taskIDHex)
	if err != nil {
		return nil, 0, fmt.Errorf("forward: parse task id: %w", err)
	}
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_RegisterPortForward}
	body := protocol.RegisterPortForwardRequest{TaskId: tid, Direction: dir,
		BindPort: uint16(bindPort), TargetPort: uint16(targetPort)}
	body.SetBindAddr([]byte(bindAddr))
	body.SetTargetHost([]byte(targetHost))
	req.SetRegisterPortForward(body)

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, 0, err
	}
	r := resp.RegisterPortForward()
	if resp.Kind != protocol.TaskControlKind_RegisterPortForward || r == nil {
		return nil, 0, fmt.Errorf("forward: unexpected response kind %v", resp.Kind)
	}
	switch r.Status {
	case protocol.OpenPortForwardStatus_Ok:
	case protocol.OpenPortForwardStatus_NoSuchTask:
		return nil, 0, errors.New("forward: no such task (id unknown or task not running)")
	case protocol.OpenPortForwardStatus_RunnerOffline:
		return nil, 0, errors.New("forward: runner offline")
	case protocol.OpenPortForwardStatus_BindFailed:
		return nil, 0, errors.New("forward: runner failed to bind the listen port")
	default:
		return nil, 0, fmt.Errorf("forward: server error (status=%d)", r.Status)
	}
	ctrl := peer.WaitForBidirectionalStream(ctx, c.Transport(), trsf.StreamID(r.StreamId))
	if ctrl == nil {
		return nil, 0, fmt.Errorf("forward: control stream %d not visible", r.StreamId)
	}
	return ctrl, r.ForwardId, nil
}

// OpenRemoteForward registers a remote forward. Kept as a thin wrapper so the
// TUI's existing call site (tui/portforward.go:262) is unchanged.
func (c *Client) OpenRemoteForward(ctx context.Context, taskIDHex string, sp RemoteForwardSpec) (trsf.BidirectionalStream, uint64, error) {
	return c.RegisterPortForward(ctx, taskIDHex, protocol.PortForwardDirection_Remote,
		sp.BindAddr, sp.RunnerPort, sp.DialHost, sp.DialPort)
}
```

- [ ] **Step 8: Update the existing server tests to the new request type**

`TestHandleOpenPortForward_RemoteRegisters` (`server/port_forward_test.go:74`) and
its `runRemoteRegister` helper build an `OpenPortForwardRequest` with
`Direction`/`BindPort`/`BindAddr` — fields this task removed. Convert both to
`RegisterPortForwardRequest` (and `runRemoteRegister` to return
`RegisterPortForwardResponse`), keeping every assertion: status Ok, non-zero
`ForwardId`, control-stream id, registration stored, and the runner receiving a
`RunnerRequestType_OpenPortForward` whose `BindPort` and `ForwardId` match. The
runner-facing assertions must still pass **unchanged** — that is the proof the
runner protocol did not move. Rename `h.rforwards()` to `h.pforwards()` in the
assertions.

- [ ] **Step 9: Run the tests**

Run:

```bash
go test ./runner/protocol/ -run 'PortForward' -v
go build ./... && go test ./server/ ./cli/ -count=1
```

Expected: PASS, and `go build ./...` clean.

- [ ] **Step 10: Verify the runner protocol really did not move**

Run:

```bash
git diff runner/protocol/message.bgn | grep -E '^[-+]' | grep -iE 'runner_open_port_forward|close_port_forward|remote_forward_conn:|remote_forward_bind_result' || echo "runner messages untouched"
```

Expected: `runner messages untouched`. If anything prints, you changed a
runner-visible message — revert that hunk.

- [ ] **Step 11: Commit**

`git mv` already staged the registry rename, so stage the whole `server/` change
set rather than listing files (a missed path would leave the rename half-staged):

```bash
git add runner/protocol/message.bgn runner/protocol/message.go runner/protocol/port_forward_test.go \
        server/ cli/port_forward.go
git status --short   # confirm: R server/remote_forward_registry.go -> server/port_forward_registry.go
git commit -m "feat(protocol): port-forward registration RPC + registry entry metadata (Task 1)"
```

---

### Task 2: Tagged control-stream records

Both ends of the `-R` control stream flip together — a half-flipped framing
breaks remote forwards.

**Files:**
- Modify: `server/port_forward.go` (`handleRemoteForwardConn` writer)
- Modify: `cli/port_forward.go` (`parseConnNotifies` → `parsePortForwardEvents`, `ServeRemoteForwardControl`)
- Test: `cli/port_forward_test.go`

**Interfaces:**
- Consumes: `protocol.PortForwardEvent`, `protocol.PortForwardEventKind_*`, `protocol.PortForwardClosed` (Task 1).
- Produces: `cli.parsePortForwardEvents(buf []byte) (evs []protocol.PortForwardEvent, rest []byte)`; `ServeRemoteForwardControl` now returns when a `closed` event arrives.

- [ ] **Step 1: Write the failing parser test**

Replace the `parseConnNotifies` tests in `cli/port_forward_test.go` with:

```go
func TestParsePortForwardEvents_SplitAndCoalesced(t *testing.T) {
	var n protocol.PortForwardEvent
	n.Kind = protocol.PortForwardEventKind_ConnNotify
	n.SetConnNotify(protocol.RemoteForwardConnNotify{StreamId: 11})
	var cl protocol.PortForwardEvent
	cl.Kind = protocol.PortForwardEventKind_Closed
	cl.SetClosed(protocol.PortForwardClosed{Reason: protocol.PortForwardCloseReason_Killed})

	full := cl.MustAppend(n.MustAppend(nil))

	// Coalesced: both events in one buffer.
	evs, rest := parsePortForwardEvents(full)
	if len(evs) != 2 || len(rest) != 0 {
		t.Fatalf("coalesced: got %d events, %d rest bytes", len(evs), len(rest))
	}
	if evs[0].ConnNotify().StreamId != 11 || evs[1].Closed().Reason != protocol.PortForwardCloseReason_Killed {
		t.Fatalf("coalesced: wrong payloads: %+v", evs)
	}

	// Split: one byte at a time; events must appear exactly once, in order.
	var got []protocol.PortForwardEvent
	var buf []byte
	for i := 0; i < len(full); i++ {
		buf = append(buf, full[i])
		var evs []protocol.PortForwardEvent
		evs, buf = parsePortForwardEvents(buf)
		got = append(got, evs...)
	}
	if len(got) != 2 || len(buf) != 0 {
		t.Fatalf("split: got %d events, %d leftover bytes", len(got), len(buf))
	}
	if got[0].Kind != protocol.PortForwardEventKind_ConnNotify || got[1].Kind != protocol.PortForwardEventKind_Closed {
		t.Fatalf("split: wrong order: %+v", got)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./cli/ -run TestParsePortForwardEvents -v`
Expected: FAIL — `undefined: parsePortForwardEvents`.

- [ ] **Step 3: Implement the parser**

In `cli/port_forward.go`, delete `remoteForwardConnNotifySize` and
`parseConnNotifies`, and add:

```go
// parsePortForwardEvents consumes as many whole PortForwardEvent records from buf
// as possible, returning them and the unconsumed remainder. Records are no longer
// a fixed size (the tag selects the payload), so the decoder itself reports where
// each record ends.
func parsePortForwardEvents(buf []byte) (evs []protocol.PortForwardEvent, rest []byte) {
	for len(buf) > 0 {
		var ev protocol.PortForwardEvent
		r, err := ev.Decode(buf)
		if err != nil {
			break // partial record: keep it for the next read
		}
		evs = append(evs, ev)
		buf = r
	}
	return evs, buf
}
```

- [ ] **Step 4: Emit tagged records on the server**

In `server/port_forward.go`, `handleRemoteForwardConn` currently encodes a bare
`RemoteForwardConnNotify`. Wrap it, and add the close pusher used by Task 3:

```go
	var ev protocol.PortForwardEvent
	ev.Kind = protocol.PortForwardEventKind_ConnNotify
	ev.SetConnNotify(protocol.RemoteForwardConnNotify{StreamId: uint64(clientStream.ID())})
	nb, err := ev.Append(nil)
```

```go
// pushPortForwardClosed tells the client to stop this forward. Sent as an
// explicit record — never as a bare stream close — so the client can tell
// "killed" apart from "the server went away" (which arrives as EOF).
func pushPortForwardClosed(pf *portForward, reason protocol.PortForwardCloseReason) {
	if pf == nil || pf.control == nil {
		return
	}
	var ev protocol.PortForwardEvent
	ev.Kind = protocol.PortForwardEventKind_Closed
	ev.SetClosed(protocol.PortForwardClosed{Reason: reason})
	b, err := ev.Append(nil)
	if err != nil {
		slog.Error("port_forward: encode closed event", "fwd", pf.forwardID, "err", err)
		return
	}
	if werr := pf.control.AppendData(false, b); werr != nil {
		slog.Info("port_forward: closed event not delivered", "fwd", pf.forwardID, "err", werr)
	}
}
```

- [ ] **Step 5: Consume the events on the client**

Rewrite the read loop in `ServeRemoteForwardControl` to dispatch on the tag, and
return on `closed`:

```go
		data, eof, err := ctrl.ReadDirectContext(ctx, 64*1024)
		if len(data) > 0 {
			buf = append(buf, data...)
			var evs []protocol.PortForwardEvent
			evs, buf = parsePortForwardEvents(buf)
			for _, ev := range evs {
				switch ev.Kind {
				case protocol.PortForwardEventKind_ConnNotify:
					go c.dialAndSplice(ctx, sp, trsf.StreamID(ev.ConnNotify().StreamId), logf)
				case protocol.PortForwardEventKind_Closed:
					logf(closedReasonLine(ev.Closed().Reason))
					return
				}
			}
		}
		if eof || err != nil {
			// ReadDirectContext returns ctx.Err() on cancellation, and
			// cancellation IS the ordinary stop path (X11 session end, Ctrl-C,
			// TUI stop). Reporting a lost server there would make the
			// server-died message the one thing printed on every clean exit —
			// destroying the very distinction this task exists to create.
			if ctx.Err() == nil {
				logf("remote-forward: server connection lost")
			}
			return
		}
```

```go
// closedReasonLine renders why a forward stopped. Kept distinct from the EOF
// path so an operator can tell a deliberate kill from a dead server.
func closedReasonLine(r protocol.PortForwardCloseReason) string {
	switch r {
	case protocol.PortForwardCloseReason_TaskGone:
		return "forward stopped: task is no longer running"
	default:
		return "forward stopped: killed remotely"
	}
}
```

- [ ] **Step 6: Run the tests**

Run: `go test ./cli/ ./server/ -count=1 && go build ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add cli/port_forward.go cli/port_forward_test.go server/port_forward.go
git commit -m "feat(protocol): tagged port-forward control records with explicit close (Task 2)"
```

---

### Task 3: Server list & kill + client API

**Files:**
- Create: `server/port_forward_list.go`
- Modify: `server/task_handler.go` (dispatch)
- Create: `cli/port_forward_list.go`
- Test: `server/port_forward_test.go`, `cli/port_forward_list_test.go`

**Interfaces:**
- Consumes: `portForwardRegistry.list()`, `pushPortForwardClosed`, `protocol.PortForwardListQuery/Result/Info/ListResultBody`, `protocol.KillPortForward*`.
- Produces:
  - `(*Client).PortForwardListWith(ctx context.Context, taskFilter string) ([]protocol.PortForwardInfo, error)` and `cli.PortForwardList(ctx, serverCID objproto.ConnectionID, taskFilter string) ([]protocol.PortForwardInfo, error)`
  - `(*Client).KillPortForwardWith(ctx context.Context, id uint64) error` and `cli.KillPortForward(ctx, serverCID objproto.ConnectionID, id uint64) error`
  - `cli.PortForwardSpecString(fi *protocol.PortForwardInfo) string`
  - `cli.PortForwardInfoLines(fs []protocol.PortForwardInfo) []string`
  - `cli.PortForwardInfoJSONLine(fi *protocol.PortForwardInfo) string`

- [ ] **Step 1: Write the failing server test**

Add to `server/port_forward_test.go`. That file has no `newTestHandler` helper —
its tests build the handler literally and insert tasks through the store's
internals (see `TestHandleOpenPortForward_RemoteRegisters` at line 74). Match
that, and reuse the `fakeConn` and `recordingBidiStream` fakes already defined
there:

```go
// addRunningTask inserts a Running task assigned to runnerID and returns its hex id.
func addRunningTask(t *testing.T, h *TaskHandler, first byte, runnerID string) string {
	t.Helper()
	var raw [16]byte
	raw[0] = first
	idHex := hex.EncodeToString(raw[:])
	h.Tasks.mu.Lock()
	h.Tasks.tasks[idHex] = &TaskEntry{ID: idHex, Status: protocol.TaskStatus_Running, AssignedTo: runnerID}
	h.Tasks.order = append(h.Tasks.order, idHex)
	h.Tasks.mu.Unlock()
	return idHex
}

func TestVisiblePortForwards_ReapsDeadTaskAndListsRest(t *testing.T) {
	h := &TaskHandler{Tasks: NewTaskStore(), Registry: NewRegistry()}
	liveHex := addRunningTask(t, h, 0x11, "runner-1")
	deadHex := addRunningTask(t, h, 0x22, "runner-1")

	conn := &fakeConn{nextStreamID: 100}
	live := &portForward{direction: protocol.PortForwardDirection_Local, taskIDHex: liveHex,
		control: newRecordingBidiStream(1), clientCxn: conn, bindAddr: "127.0.0.1", bindPort: 8080,
		targetHost: "db", targetPort: 5432}
	dead := &portForward{direction: protocol.PortForwardDirection_Local, taskIDHex: deadHex,
		control: newRecordingBidiStream(2), clientCxn: conn, bindAddr: "127.0.0.1", bindPort: 8081,
		targetHost: "db", targetPort: 5432}
	liveID := h.pforwards().add(live)
	deadID := h.pforwards().add(dead)

	// The dead task leaves Running only AFTER both forwards were registered.
	h.Tasks.Cancel(deadHex)

	got := h.visiblePortForwards("", protocol.TaskID{})
	if len(got) != 1 || got[0].ForwardId != liveID {
		t.Fatalf("expected only forward %d, got %+v", liveID, got)
	}
	if got[0].BindPort != 8080 || string(got[0].TargetHost) != "db" {
		t.Fatalf("info fields not populated: %+v", got[0])
	}
	if _, ok := h.pforwards().get(deadID); ok {
		t.Fatal("the dead task's forward should have been reaped by the list call")
	}
}

func TestKillPortForward_UnknownIDAndDoubleKill(t *testing.T) {
	h := &TaskHandler{Tasks: NewTaskStore(), Registry: NewRegistry()}
	if st := h.killPortForward("", 9999); st != protocol.KillPortForwardStatus_NoSuchForward {
		t.Fatalf("unknown id: status = %v, want no_such_forward", st)
	}

	taskHex := addRunningTask(t, h, 0x33, "runner-1")
	pf := &portForward{direction: protocol.PortForwardDirection_Local, taskIDHex: taskHex,
		control: newRecordingBidiStream(3), clientCxn: &fakeConn{nextStreamID: 200}}
	id := h.pforwards().add(pf)

	if st := h.killPortForward("", id); st != protocol.KillPortForwardStatus_Ok {
		t.Fatalf("first kill: status = %v, want ok", st)
	}
	// Exactly one caller may win: the second kill must not also report ok.
	if st := h.killPortForward("", id); st != protocol.KillPortForwardStatus_NoSuchForward {
		t.Fatalf("second kill: status = %v, want no_such_forward", st)
	}
}

// TestPortForwardControlEOFDeregisters covers the stray-terminal case: the
// client process dies, its control stream EOFs, and the registration goes away
// with no RPC involved.
func TestPortForwardControlEOFDeregisters(t *testing.T) {
	h := &TaskHandler{Tasks: NewTaskStore(), Registry: NewRegistry()}
	taskHex := addRunningTask(t, h, 0x44, "runner-1")
	ctrl := newRecordingBidiStream(4)
	pf := &portForward{direction: protocol.PortForwardDirection_Local, taskIDHex: taskHex,
		control: ctrl, clientCxn: &fakeConn{nextStreamID: 300}}
	id := h.pforwards().add(pf)
	go h.watchRemoteForwardControl(pf)

	_ = ctrl.CloseBoth() // client went away
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := h.pforwards().get(id); !ok {
			return // deregistered
		}
		if time.Now().After(deadline) {
			t.Fatal("registration survived control-stream EOF")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./server/ -run 'PortForward' -v`
Expected: FAIL — `visiblePortForwards`/`killPortForward` undefined.

- [ ] **Step 3: Implement the server side**

Create `server/port_forward_list.go`:

```go
// visiblePortForwards returns the registrations connID may see, reaping any whose
// task has left Running/Detached on the way past. Visibility mirrors
// handleListConns: an operator (zero principal) or an InfoGlobal holder sees
// everything; anyone else sees only forwards for tasks in its own subtree.
//
// The reap is deliberately lazy — here, at the single call site — rather than
// hooked into TaskStore.Finish/Cancel/MarkFailed and the runner-loss paths.
// Intercepting a shared transition at some of its call sites is a failure mode
// this repo has already paid for (see implementation-pitfalls Pitfall 3 / 10).
func (h *TaskHandler) visiblePortForwards(connID string, filter protocol.TaskID) []protocol.PortForwardInfo {
	all, allowed := h.visibleToCaller(connID)
	filterHex := ""
	if filter.Id != ([16]byte{}) {
		filterHex = hex.EncodeToString(filter.Id[:])
	}
	var out []protocol.PortForwardInfo
	for _, pf := range h.pforwards().list() {
		task, ok := h.Tasks.Get(pf.taskIDHex)
		if !ok || (task.Status != protocol.TaskStatus_Running && task.Status != protocol.TaskStatus_Detached) {
			h.closePortForward(pf, protocol.PortForwardCloseReason_TaskGone)
			continue
		}
		if !all && !allowed[pf.taskIDHex] {
			continue
		}
		if filterHex != "" && pf.taskIDHex != filterHex {
			continue
		}
		out = append(out, portForwardInfo(pf))
	}
	return out
}

// portForwardInfo converts a registration into its wire form.
func portForwardInfo(pf *portForward) protocol.PortForwardInfo {
	var info protocol.PortForwardInfo
	info.ForwardId = pf.forwardID
	info.Direction = pf.direction
	if b, err := hex.DecodeString(pf.taskIDHex); err == nil && len(b) == 16 {
		copy(info.TaskId.Id[:], b)
	}
	info.SetBindAddr([]byte(pf.bindAddr))
	info.BindPort = pf.bindPort
	info.SetTargetHost([]byte(pf.targetHost))
	info.TargetPort = pf.targetPort
	info.OriginKind = pf.clientKind
	info.SetOriginCid([]byte(pf.clientCID))
	return info
}

// closePortForward drops a registration, tells the client why, and releases the
// runner listener for remote forwards. Reports whether THIS call removed it:
// registry.remove is the single point of arbitration, so two concurrent killers
// cannot both be told they succeeded.
func (h *TaskHandler) closePortForward(pf *portForward, reason protocol.PortForwardCloseReason) bool {
	if _, ok := h.pforwards().remove(pf.forwardID); !ok {
		return false
	}
	pushPortForwardClosed(pf, reason)
	if pf.direction != protocol.PortForwardDirection_Remote {
		return true
	}
	runner, ok := h.Registry.Get(pf.runnerID)
	if !ok || runner.Conn == nil {
		return true
	}
	sendClosePortForward(runner.Conn, pf.forwardID)
	return true
}

// killPortForward closes one registration on request. An id the caller cannot see
// answers no_such_forward, identical to an unknown id: a distinct "denied" would
// let a confined agent probe ids to learn which forwards exist.
func (h *TaskHandler) killPortForward(connID string, id uint64) protocol.KillPortForwardStatus {
	pf, ok := h.pforwards().get(id)
	if !ok {
		return protocol.KillPortForwardStatus_NoSuchForward
	}
	all, allowed := h.visibleToCaller(connID)
	if !all && !allowed[pf.taskIDHex] {
		return protocol.KillPortForwardStatus_NoSuchForward
	}
	if !h.closePortForward(pf, protocol.PortForwardCloseReason_Killed) {
		// Someone else removed it between the get and the remove.
		return protocol.KillPortForwardStatus_NoSuchForward
	}
	return protocol.KillPortForwardStatus_Ok
}
```

The confined-caller *visibility* predicate itself (`visibleToCaller`) already has
coverage in `server/capabilities_test.go`; the tests above deliberately cover only
what is new here — the lazy reap, the single-winner kill arbitration, and
EOF-driven deregistration.

Add the two dispatch cases in `server/task_handler.go`. List streams its body
exactly like `handleListConns` (`server/task_handler.go:1355`) — copy that
`respond(streamID)` + `CreateSendStream` + `AppendData(false, body)` +
`AppendData(true)` shape. Kill resolves the direction from the registry **before**
the capability check, because the required bit depends on it:

```go
	case protocol.TaskControlKind_KillPortForward:
		kr := req.KillPortForward()
		if kr == nil {
			slog.Error("TaskHandler: KillPortForward variant is nil")
			return
		}
		// Visibility FIRST, then the capability. Denying on a missing bit for a
		// forward the caller cannot see would answer "exists, but not yours" —
		// and with ids coming from a dense next++ counter, that is exactly the
		// enumeration oracle the no_such_forward rule exists to close. An
		// invisible id must fall through to killPortForward and be answered
		// identically to an unknown one.
		if pf, ok := h.pforwards().get(kr.ForwardId); ok {
			if all, allowed := h.visibleToCaller(cid); all || allowed[pf.taskIDHex] {
				need := protocol.Capability_ForwardLocal
				if pf.direction == protocol.PortForwardDirection_Remote {
					need = protocol.Capability_ForwardRemote
				}
				if !hasCap(h.callerCaps(cid), need) {
					h.denyTaskControl(conn, req.Kind, req.RequestId, need)
					return
				}
			}
		}
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_KillPortForward, RequestId: req.RequestId}
		resp.SetKillPortForward(protocol.KillPortForwardResponse{Status: h.killPortForward(cid, kr.ForwardId)})
		out := resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck
```

- [ ] **Step 4: Run the server tests**

Run: `go test ./server/ -run 'PortForward' -v`
Expected: PASS.

- [ ] **Step 5: Write the failing client-render test**

Create `cli/port_forward_list_test.go`:

```go
func TestPortForwardSpecString(t *testing.T) {
	var l protocol.PortForwardInfo
	l.Direction = protocol.PortForwardDirection_Local
	l.SetBindAddr([]byte("127.0.0.1"))
	l.BindPort = 8080
	l.SetTargetHost([]byte("db.internal"))
	l.TargetPort = 5432
	if got, want := PortForwardSpecString(&l), "127.0.0.1:8080 -> db.internal:5432"; got != want {
		t.Errorf("local: got %q, want %q", got, want)
	}

	var r protocol.PortForwardInfo
	r.Direction = protocol.PortForwardDirection_Remote
	r.SetBindAddr([]byte("127.0.0.1"))
	r.BindPort = 6001
	r.SetTargetHost([]byte("localhost"))
	r.TargetPort = 6000
	if got, want := PortForwardSpecString(&r), "runner:127.0.0.1:6001 -> localhost:6000"; got != want {
		t.Errorf("remote: got %q, want %q", got, want)
	}
}

func TestPortForwardInfoJSONLine(t *testing.T) {
	var fi protocol.PortForwardInfo
	fi.ForwardId = 3
	fi.Direction = protocol.PortForwardDirection_Local
	fi.SetBindAddr([]byte("127.0.0.1"))
	fi.BindPort = 9000
	fi.SetTargetHost([]byte("svc"))
	fi.TargetPort = 80
	fi.OriginKind = protocol.ClientKind_Cli
	fi.SetOriginCid([]byte("ws:127.0.0.1:1-a"))
	line := PortForwardInfoJSONLine(&fi)
	for _, want := range []string{`"forward_id":3`, `"dir":"-L"`, `"origin_kind":"cli"`, `"origin_cid":"ws:127.0.0.1:1-a"`} {
		if !strings.Contains(line, want) {
			t.Errorf("JSON line %q missing %q", line, want)
		}
	}
}
```

- [ ] **Step 6: Run it and watch it fail**

Run: `go test ./cli/ -run 'PortForward(SpecString|InfoJSONLine)' -v`
Expected: FAIL — undefined symbols.

- [ ] **Step 7: Implement the client API**

Create `cli/port_forward_list.go`:

```go
// PortForwardListWith queries the server for the forwards this caller may see.
// Reuses the caller's existing *Client — no extra dial. Wire path is the same
// three steps as ConnListWith (cli/conns.go:28): round-trip, pick up the
// server-initiated send-stream by id, read to EOF, decode.
func (c *Client) PortForwardListWith(ctx context.Context, taskFilter string) ([]protocol.PortForwardInfo, error) {
	var q protocol.PortForwardListQuery
	if taskFilter != "" {
		tid, err := parseTaskIDHex(taskFilter)
		if err != nil {
			return nil, fmt.Errorf("forward ls: parse task id: %w", err)
		}
		q.TaskId = tid
	}
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_ListPortForwards}
	req.SetListPortForwards(q)
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, err
	}
	lr := resp.ListPortForwards()
	if lr == nil {
		return nil, fmt.Errorf("expected ListPortForwards response, got kind=%v", resp.Kind)
	}
	if lr.StreamId == 0 {
		return nil, fmt.Errorf("server returned no stream id (could not allocate)")
	}
	st := waitForReceiveStream(ctx, c.Transport(), trsf.StreamID(lr.StreamId))
	if st == nil {
		return nil, fmt.Errorf("forward-list stream %d not visible after response", lr.StreamId)
	}
	var raw []byte
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, eof, err := st.ReadDirect(64 * 1024)
		if err != nil {
			return nil, fmt.Errorf("forward-list stream read: %w", err)
		}
		if len(data) > 0 {
			raw = append(raw, data...)
		}
		if eof {
			break
		}
	}
	body := &protocol.PortForwardListResultBody{}
	if err := body.DecodeExact(raw); err != nil {
		return nil, fmt.Errorf("decode PortForwardListResultBody (%d bytes): %w", len(raw), err)
	}
	return body.Forwards, nil
}

// PortForwardList opens a fresh Client, lists, and closes it. For short-lived
// harness-cli invocations only — TUI/WebUI hold a *Client and call the With form.
func PortForwardList(ctx context.Context, peerCID objproto.ConnectionID, taskFilter string) ([]protocol.PortForwardInfo, error) {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.PortForwardListWith(ctx, taskFilter)
}

// KillPortForwardWith closes one registered forward by id.
func (c *Client) KillPortForwardWith(ctx context.Context, id uint64) error {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_KillPortForward}
	req.SetKillPortForward(protocol.KillPortForwardRequest{ForwardId: id})
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return err
	}
	kr := resp.KillPortForward()
	if kr == nil {
		return fmt.Errorf("expected KillPortForward response, got kind=%v", resp.Kind)
	}
	switch kr.Status {
	case protocol.KillPortForwardStatus_Ok:
		return nil
	case protocol.KillPortForwardStatus_NoSuchForward:
		return fmt.Errorf("forward kill: no such forward %d", id)
	default:
		return fmt.Errorf("forward kill: server error (status=%d)", kr.Status)
	}
}

// KillPortForward is the short-lived-CLI form of KillPortForwardWith.
func KillPortForward(ctx context.Context, peerCID objproto.ConnectionID, id uint64) error {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.KillPortForwardWith(ctx, id)
}
// PortForwardSpecString renders a forward's address pair. The "runner:" prefix
// marks where the listener lives for a remote forward.
func PortForwardSpecString(fi *protocol.PortForwardInfo) string {
	listen := fmt.Sprintf("%s:%d", fi.BindAddr, fi.BindPort)
	if fi.Direction == protocol.PortForwardDirection_Remote {
		listen = "runner:" + listen
	}
	return fmt.Sprintf("%s -> %s:%d", listen, fi.TargetHost, fi.TargetPort)
}

// PortForwardDirFlag renders the direction as the CLI flag that creates it.
func PortForwardDirFlag(d protocol.PortForwardDirection) string {
	if d == protocol.PortForwardDirection_Remote {
		return "-R"
	}
	return "-L"
}

// PortForwardInfoLines returns the header plus one line per forward, matching
// the shape of ConnInfoLines.
func PortForwardInfoLines(fs []protocol.PortForwardInfo) []string {
	lines := []string{"PORT FORWARDS"}
	if len(fs) == 0 {
		return append(lines, "  (none)")
	}
	lines = append(lines, fmt.Sprintf("  %-6s  %-3s  %-12s  %-40s  %s", "ID", "DIR", "TASK", "SPEC", "ORIGIN"))
	for i := range fs {
		fi := &fs[i]
		lines = append(lines, fmt.Sprintf("  %-6d  %-3s  %-12s  %-40s  %s",
			fi.ForwardId, PortForwardDirFlag(fi.Direction), principalShort(fi.TaskId.Id[:]),
			PortForwardSpecString(fi), portForwardOrigin(fi)))
	}
	return lines
}
```

`principalShort` and `taskIDStr` already live in `cli/` (`cli/conns.go:162`,
`cli/list.go`) — use them rather than adding a third task-id renderer. The JSON
view gets its own struct so field order is deterministic, same rationale as
`connInfoJSON` (`cli/conns.go:129`):

```go
// portForwardJSON is the single source of truth for the JSON shape of a
// PortForwardInfo. A struct (not map[string]any) keeps field order stable
// across JSON Lines output.
type portForwardJSON struct {
	ForwardID  uint64 `json:"forward_id"`
	Dir        string `json:"dir"`
	Task       string `json:"task"`
	BindAddr   string `json:"bind_addr"`
	BindPort   uint16 `json:"bind_port"`
	TargetHost string `json:"target_host"`
	TargetPort uint16 `json:"target_port"`
	OriginKind string `json:"origin_kind"`
	OriginCid  string `json:"origin_cid"`
}

// PortForwardInfoJSONLine returns one JSON object (single line, no trailing
// newline) for a PortForwardInfo.
func PortForwardInfoJSONLine(fi *protocol.PortForwardInfo) string {
	b, _ := json.Marshal(portForwardJSON{
		ForwardID:  fi.ForwardId,
		Dir:        PortForwardDirFlag(fi.Direction),
		Task:       taskIDStr(fi.TaskId.Id[:]),
		BindAddr:   string(fi.BindAddr),
		BindPort:   fi.BindPort,
		TargetHost: string(fi.TargetHost),
		TargetPort: fi.TargetPort,
		OriginKind: strings.ToLower(fi.OriginKind.String()),
		OriginCid:  string(fi.OriginCid),
	})
	return string(b)
}

// portForwardOrigin renders "kind cid" for the ORIGIN column, e.g. "cli ws:…-ab".
func portForwardOrigin(fi *protocol.PortForwardInfo) string {
	return strings.ToLower(fi.OriginKind.String()) + " " + string(fi.OriginCid)
}
```

- [ ] **Step 8: Run the tests**

Run: `go test ./cli/ ./server/ -count=1 && go build ./...`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add server/port_forward_list.go server/task_handler.go server/port_forward_test.go \
        cli/port_forward_list.go cli/port_forward_list_test.go
git commit -m "feat(server,cli): list and kill port forwards (Task 3)"
```

---

### Task 4: Register `-L` forwards from the client

**Files:**
- Modify: `cli/port_forward.go` (`RunForward`, `acceptLoop`)
- Test: `integration/port_forward_test.go`

**Interfaces:**
- Consumes: `(*Client).RegisterPortForward` (Task 1), `parsePortForwardEvents` + `closedReasonLine` (Task 2).
- Produces: `RunForward` returns once every spec's forward has stopped; a killed forward closes its listener *and* its established connections.

- [ ] **Step 1: Write the failing integration test**

Add to `integration/port_forward_test.go` (build tag `integration`). There are no
harness helpers in that package — `TestPortForwardE2E` builds everything inline.
Copy its setup preamble verbatim (`integration/port_forward_test.go:32-88`:
`clearAgentEnv`, `initRepo`, the `fake-claude-slow.sh` path, `server.New` +
`s.Run`, `runner.Run`, `cli.Submit`, and the worktree-appears poll), changing only
the listen address to a port no other test uses (e.g. `127.0.0.1:18549`). Then:

```go
func TestLocalForwardRegisterListKill(t *testing.T) {
	// ---- setup preamble copied from TestPortForwardE2E (addr 127.0.0.1:18549),
	// producing: ctx, peerCID, taskID, and a ready worktree ----

	// The forward client: holds the listener, like a `harness-cli forward` terminal.
	fwdClient, err := cli.Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatalf("dial forward client: %v", err)
	}
	defer fwdClient.Close()

	fwdCtx, cancelFwd := context.WithCancel(ctx)
	defer cancelFwd()
	done := make(chan error, 1)
	go func() {
		// LocalPort 0 asks the kernel for a free port, which is exactly the case
		// that proves RunForward registers the port it actually bound.
		done <- cli.RunForward(fwdCtx, fwdClient, taskID,
			[]cli.ForwardSpec{{BindAddr: "127.0.0.1", LocalPort: 0, RemoteHost: "127.0.0.1", RemotePort: 9}},
			func(s string) { t.Logf("forward: %s", s) })
	}()

	// A SECOND, independent client must be able to see and kill it.
	observer, err := cli.Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatalf("dial observer: %v", err)
	}
	defer observer.Close()

	var fs []protocol.PortForwardInfo
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		fs, _ = observer.PortForwardListWith(ctx, "")
		if len(fs) == 1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(fs) != 1 {
		t.Fatalf("observer saw %d forwards, want 1", len(fs))
	}
	if fs[0].Direction != protocol.PortForwardDirection_Local {
		t.Fatalf("direction = %v, want Local", fs[0].Direction)
	}
	if fs[0].BindPort == 0 {
		t.Fatal("BindPort is 0 — the kernel-assigned port was not registered")
	}

	if err := observer.KillPortForwardWith(ctx, fs[0].ForwardId); err != nil {
		t.Fatalf("kill: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunForward returned %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunForward did not return after the forward was killed")
	}

	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if fs, _ = observer.PortForwardListWith(ctx, ""); len(fs) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("registration survived the kill: %+v", fs)
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test -tags integration ./integration/ -run TestLocalForwardRegisterListKill -count=1 -timeout 120s`
Expected: FAIL — the observer sees zero forwards (nothing registers today).

- [ ] **Step 3: Register in `RunForward`**

Rewrite `RunForward` so each spec owns a sub-context, registers after binding, and
stops when its control stream says so:

```go
func RunForward(ctx context.Context, c *Client, taskIDHex string, specs []ForwardSpec, logf func(string)) error {
	if logf == nil {
		logf = func(s string) { slog.Info(s) }
	}
	var lns []net.Listener
	closeAll := func() {
		for _, l := range lns {
			_ = l.Close()
		}
	}
	// One context for the whole call, cancelled on every exit path including the
	// error returns below. Without this, a mid-loop failure would abandon the
	// specs already started: their listeners close but their control-stream
	// readers stay parked and their server-side registrations stay live until the
	// peer connection eventually errors out — a forward the list shows that is
	// not actually running. The spec's failure-mode table claims stale entries
	// cannot happen because control-stream EOF deregisters; that only holds if
	// every exit path actually closes the streams.
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	var wg sync.WaitGroup
	// abort cancels every started spec, waits for its goroutines, and closes the
	// listeners. Use it on every error return, never a bare closeAll().
	abort := func() {
		runCancel()
		wg.Wait()
		closeAll()
	}
	for _, sp := range specs {
		ln, err := net.Listen("tcp", net.JoinHostPort(sp.BindAddr, strconv.Itoa(sp.LocalPort)))
		if err != nil {
			abort()
			return fmt.Errorf("forward: listen %s:%d: %w", sp.BindAddr, sp.LocalPort, err)
		}
		lns = append(lns, ln)
		// Register the port the kernel actually gave us (sp.LocalPort may be 0).
		bound := ln.Addr().(*net.TCPAddr).Port
		ctrl, fid, rerr := c.RegisterPortForward(ctx, taskIDHex, protocol.PortForwardDirection_Local,
			sp.BindAddr, bound, sp.RemoteHost, sp.RemotePort)
		if rerr != nil {
			// A forward the server does not know about cannot be listed or
			// killed, which is the whole point of registering — fail loudly.
			abort()
			return fmt.Errorf("forward: register %s:%d: %w", sp.BindAddr, bound, rerr)
		}
		fwdCtx, cancel := context.WithCancel(runCtx)
		logf(fmt.Sprintf("forwarding %s:%d -> %s:%d (task %s, fwd %d)",
			sp.BindAddr, bound, sp.RemoteHost, sp.RemotePort, taskIDHex[:min(12, len(taskIDHex))], fid))
		go acceptLoop(fwdCtx, c, taskIDHex, sp, ln, logf)
		wg.Add(1)
		go func(ctrl trsf.BidirectionalStream) {
			defer wg.Done()
			defer cancel()
			defer ctrl.CloseBoth()
			c.serveLocalForwardControl(fwdCtx, ctrl, logf)
		}(ctrl)
	}
	wg.Wait()
	closeAll()
	return nil
}

// serveLocalForwardControl reads the forward's control stream. The client never
// writes on it; it returns on a closed event (deliberate stop) or on EOF/error
// (the server went away). Returning cancels the forward's context, which closes
// the listener and every connection spliced through it.
func (c *Client) serveLocalForwardControl(ctx context.Context, ctrl trsf.BidirectionalStream, logf func(string)) {
	var buf []byte
	for {
		data, eof, err := ctrl.ReadDirectContext(ctx, 64*1024)
		if len(data) > 0 {
			buf = append(buf, data...)
			var evs []protocol.PortForwardEvent
			evs, buf = parsePortForwardEvents(buf)
			for _, ev := range evs {
				if ev.Kind == protocol.PortForwardEventKind_Closed {
					logf(closedReasonLine(ev.Closed().Reason))
					return
				}
			}
		}
		if eof || err != nil {
			if ctx.Err() == nil {
				logf("forward stopped: server connection lost")
			}
			return
		}
	}
}
```

- [ ] **Step 4: Tear down established connections on kill**

In `acceptLoop`, bind each spliced connection's lifetime to the forward context:

```go
		go func() {
			st, err := c.OpenPortForward(ctx, taskIDHex, sp.RemoteHost, sp.RemotePort)
			if err != nil {
				logf("forward: " + err.Error())
				_ = conn.Close()
				return
			}
			// A killed forward drops its established connections too. The CLI
			// exits straight after RunForward returns and would drop them
			// anyway; doing it here makes TUI/WebUI-started forwards behave
			// the same instead of leaking a live splice.
			stop := make(chan struct{})
			defer close(stop)
			go func() {
				select {
				case <-ctx.Done():
					_ = conn.Close()
					_ = st.CloseBoth()
				case <-stop:
				}
			}()
			spliceConnStream(conn, st)
		}()
```

- [ ] **Step 5: Fix the caller whose contract this just changed**

`RunForward` used to return only on `ctx.Done()`. It now returns when the `-L`
forwards stop, which breaks the assumption in `cmd/harness-cli/main.go:510-526`:
for a mixed `harness-cli forward <task> -L … -R …`, `RunForward` returning ends
the `forward` case, `main` returns, and the background `RunRemoteForward`
goroutine dies with the process — silently killing a `-R` forward nobody asked to
stop. The stale comment there ("local (-L) forwards … hold the foreground until
Ctrl-C") must go too.

Fix: after `RunForward` returns, keep holding the foreground while any `-R`
forward is still live — e.g. fall through to `<-fctx.Done()` when
`len(parsedR) > 0` — so the process exits only once **all** of its forwards are
gone, which is what the plan's constraint says.

- [ ] **Step 6: Run the integration test**

Run: `go test -tags integration ./integration/ -run TestLocalForwardRegisterListKill -count=1 -timeout 120s`
Expected: PASS.

- [ ] **Step 7: Run the unit suites**

Run: `go test ./cli/ ./server/ -count=1 && go build ./...`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add cli/port_forward.go integration/port_forward_test.go
git commit -m "feat(cli): register -L forwards so they can be listed and killed (Task 4)"
```

---

### Task 5: CLI surface — `forward ls` / `forward kill`

**Files:**
- Modify: `cmd/harness-cli/main.go` (the `case "forward":` block at ~line 470, and `usage()` at ~line 697)
- Test: `cmd/harness-cli/main_test.go`

**Interfaces:**
- Consumes: `cli.PortForwardList`, `cli.KillPortForward`, `cli.PortForwardInfoLines`, `cli.PortForwardInfoJSONLine` (Task 3).
- Produces: `harness-cli forward ls [--task <id>] [--json]`, `harness-cli forward kill <forward-id>…`.

- [ ] **Step 1: Write the failing subcommand-routing test**

Add to `cmd/harness-cli/main_test.go` (this file already tests arg parsing without
a server — follow its existing style):

```go
func TestForwardSubcommandRouting(t *testing.T) {
	// "ls" and "kill" are not hex, so they can never collide with a task id.
	for _, sub := range []string{"ls", "kill"} {
		if isTaskIDLike(sub) {
			t.Errorf("%q must not parse as a task id", sub)
		}
	}
	for _, id := range []string{"deadbeef", "0123456789abcdef0123456789abcdef"} {
		if !isTaskIDLike(id) {
			t.Errorf("%q should parse as a task id", id)
		}
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./cmd/harness-cli/ -run TestForwardSubcommandRouting -v`
Expected: FAIL — `undefined: isTaskIDLike`.

- [ ] **Step 3: Implement routing and the two subcommands**

In `cmd/harness-cli/main.go`, add the predicate and branch on it at the top of
`case "forward":`, before the existing task-id path:

```go
// isTaskIDLike reports whether s could be a task id (hex digits only). Used to
// keep `forward ls` / `forward kill` from being mistaken for a task id — neither
// word is hex.
func isTaskIDLike(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}
```

```go
	case "forward":
		if len(args) < 1 {
			fmt.Fprintln(os.Stderr, "usage: harness-cli forward <task-id> -L [bind:]localport:remotehost:remoteport [-L ...]")
			fmt.Fprintln(os.Stderr, "       harness-cli forward ls [--task <task-id>] [--json]")
			fmt.Fprintln(os.Stderr, "       harness-cli forward kill <forward-id> [<forward-id> ...]")
			os.Exit(2)
		}
		switch args[0] {
		case "ls":
			fs := flag.NewFlagSet("forward ls", flag.ExitOnError)
			taskFilter := fs.String("task", "", "only forwards for this task id")
			asJSON := fs.Bool("json", false, "one JSON object per forward")
			if err := fs.Parse(args[1:]); err != nil {
				die(err)
			}
			forwards, err := cli.PortForwardList(ctx, parseCID(), *taskFilter)
			if err != nil {
				die(err)
			}
			if *asJSON {
				for i := range forwards {
					fmt.Println(cli.PortForwardInfoJSONLine(&forwards[i]))
				}
			} else {
				for _, line := range cli.PortForwardInfoLines(forwards) {
					fmt.Println(line)
				}
			}
			return
		case "kill":
			if len(args) < 2 {
				fmt.Fprintln(os.Stderr, "usage: harness-cli forward kill <forward-id> [<forward-id> ...]")
				os.Exit(2)
			}
			for _, raw := range args[1:] {
				id, perr := strconv.ParseUint(raw, 10, 64)
				if perr != nil {
					die(fmt.Errorf("forward kill: bad forward id %q", raw))
				}
				if err := cli.KillPortForward(ctx, parseCID(), id); err != nil {
					die(err)
				}
				fmt.Printf("killed forward %d\n", id)
			}
			return
		}
		// ... existing `forward <task-id> -L/-R ...` path unchanged
```

Killing several ids in one invocation dials once per id via the package-level
helper; that is acceptable for a short-lived CLI. Do **not** use the package-level
form anywhere in TUI/WebUI.

Extend the `usage()` text (`cmd/harness-cli/main.go:697`) with the two new lines.

- [ ] **Step 4: Run the test**

Run: `go test ./cmd/harness-cli/ -count=1 && go build ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/harness-cli/main.go cmd/harness-cli/main_test.go
git commit -m "feat(cli): harness-cli forward ls / forward kill (Task 5)"
```

---

### Task 6: TUI — server-backed forwards modal, kill key, cmdline verbs

**Files:**
- Modify: `tui/portforward.go` (`ForwardsModal`)
- Modify: `tui/app.go` (`f` key handler, new messages, `k` routing)
- Modify: `tui/cmdline.go` (+ `tui/cmdline_test.go`)
- Test: `tui/portforward_test.go`

**Interfaces:**
- Consumes: `(*cli.Client).PortForwardListWith`, `.KillPortForwardWith`, `cli.PortForwardSpecString`, `cli.PortForwardDirFlag` (Task 3).
- Produces: `ForwardsSnapshotMsg{Forwards []protocol.PortForwardInfo; Err error}`, `DoListForwards(c *cli.Client) tea.Cmd`, `DoKillForward(c *cli.Client, id uint64) tea.Cmd`, `(*ForwardsModal).ApplySnapshot([]protocol.PortForwardInfo)`, `(*ForwardsModal).SelectedID() (uint64, bool)`.

- [ ] **Step 1: Grep the sibling pattern before writing anything**

`ConnsModal` is the modal in this layer that is fed from the server. Read
`tui/conns.go` and the `ConnsSnapshotMsg` handling in `tui/app.go:335-342`, plus
how existing `Do*` helpers thread the long-lived `a.client` (e.g.
`DoStartRemoteForward` at `tui/app.go:905`). Your new `Do*` functions must take
`*cli.Client` and call the `*With` methods — never `cli.Dial`. State in your
report which sibling you matched.

- [ ] **Step 2: Write the failing modal test**

Add to `tui/portforward_test.go`:

```go
func TestForwardsModalApplySnapshot(t *testing.T) {
	m := NewForwardsModal()
	m.SetSize(120, 40)
	var a, b protocol.PortForwardInfo
	a.ForwardId = 1
	a.Direction = protocol.PortForwardDirection_Local
	a.SetBindAddr([]byte("127.0.0.1"))
	a.BindPort = 8080
	a.SetTargetHost([]byte("svc"))
	a.TargetPort = 80
	b.ForwardId = 2
	b.Direction = protocol.PortForwardDirection_Remote
	b.SetBindAddr([]byte("127.0.0.1"))
	b.BindPort = 6001
	b.SetTargetHost([]byte("localhost"))
	b.TargetPort = 6000
	m.ApplySnapshot([]protocol.PortForwardInfo{a, b})
	m.Open()

	view := m.View()
	for _, want := range []string{"-L", "-R", "8080", "6001", "(2)"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}
	if id, ok := m.SelectedID(); !ok || id != 1 {
		t.Errorf("SelectedID() = (%d,%v), want (1,true)", id, ok)
	}
}

func TestForwardsModalEmpty(t *testing.T) {
	m := NewForwardsModal()
	m.SetSize(80, 24)
	m.ApplySnapshot(nil)
	m.Open()
	if !strings.Contains(m.View(), "no active forwards") {
		t.Errorf("empty view should say 'no active forwards':\n%s", m.View())
	}
	if _, ok := m.SelectedID(); ok {
		t.Error("SelectedID() should report false on an empty list")
	}
}
```

- [ ] **Step 3: Run it and watch it fail**

Run: `go test ./tui/ -run TestForwardsModal -v`
Expected: FAIL — `ApplySnapshot`/`SelectedID` undefined.

- [ ] **Step 4: Repoint the modal at server data**

Replace `SetSessions([]*PortForwardSession)` with
`ApplySnapshot([]protocol.PortForwardInfo)`, keep the `table.Model` rendering, and
change the columns to `id · dir · task · spec · origin`, building cells with
`cli.PortForwardDirFlag`, `pfShortID`, and `cli.PortForwardSpecString`. Header
becomes `active port forwards (N)`; footer becomes
`k: kill · Esc: close`. Add:

```go
// SelectedID returns the forward id under the cursor.
func (m *ForwardsModal) SelectedID() (uint64, bool) {
	if len(m.forwards) == 0 {
		return 0, false
	}
	i := m.table.Cursor()
	if i < 0 || i >= len(m.forwards) {
		return 0, false
	}
	return m.forwards[i].ForwardId, true
}
```

Delete `sortedForwards` if nothing else uses it (the server returns
forward-id order already); grep before deleting.

- [ ] **Step 5: Wire the App**

In `tui/app.go`: on `f`, dispatch `DoListForwards(a.client)` instead of reading
`a.activeForwards`; handle `ForwardsSnapshotMsg` by calling `ApplySnapshot` (and
appending the error to `cmdresult` when `Err != nil`, mirroring the conns handler
at `tui/app.go:335`); while the modal is open, map `k` to
`DoKillForward(a.client, id)` for `SelectedID()` followed by a refresh. Route the
tasks-pane `P`/`B` stop action through `DoKillForward` too, so there is exactly
one way to stop a forward; the existing `PortForwardSession.Cancel` stays as the
plumbing that the `closed` event triggers.

```go
// DoListForwards fetches every forward visible to this operator. Uses the
// long-lived client (a.client), like every other Do* in this file.
func DoListForwards(c *cli.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		fs, err := c.PortForwardListWith(ctx, "")
		return ForwardsSnapshotMsg{Forwards: fs, Err: err}
	}
}
```

Update the footer hint string (`tui/app.go:1362`) so `f forwards` reads
`f forwards (k kill)`.

- [ ] **Step 6: Add the cmdline verbs**

`tui/cmdline.go` parses a line into an Action; `runAction` in `tui/app.go`
executes it. Add the verb next to `case "file":` (`tui/cmdline.go:261`) and follow
`parseFile`'s shape:

```go
// ForwardLsAction lists every port forward visible to this operator.
type ForwardLsAction struct{}

// ForwardKillAction closes one registered forward by id.
type ForwardKillAction struct{ ForwardID uint64 }

// parseForward handles `forward ls` and `forward kill <id>`. Starting a forward
// stays on the P/B keys — this is the list/kill surface only.
func parseForward(args []string) (Action, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("forward: sub-verb required (ls | kill)")
	}
	switch args[0] {
	case "ls":
		if len(args) != 1 {
			return nil, fmt.Errorf("forward ls: usage: forward ls")
		}
		return ForwardLsAction{}, nil
	case "kill":
		if len(args) != 2 {
			return nil, fmt.Errorf("forward kill: usage: forward kill <forward-id>")
		}
		id, err := strconv.ParseUint(args[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("forward kill: bad forward id %q", args[1])
		}
		return ForwardKillAction{ForwardID: id}, nil
	default:
		return nil, fmt.Errorf("forward: unknown sub-verb %q (want ls | kill)", args[0])
	}
}
```

In `runAction`, `ForwardLsAction` dispatches `DoListForwards(a.client)` and the
snapshot handler appends `cli.PortForwardInfoLines(...)` to `cmdresult`;
`ForwardKillAction` dispatches `DoKillForward(a.client, id)`. Both use the
long-lived `a.client`, never `cli.Dial`.

Add to `tui/cmdline_test.go`:

```go
func TestParseForward(t *testing.T) {
	act, err := ParseCommand("forward kill 12", "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	kill, ok := act.(ForwardKillAction)
	if !ok || kill.ForwardID != 12 {
		t.Fatalf("got %#v, want ForwardKillAction{12}", act)
	}
	if _, err := ParseCommand("forward kill", ""); err == nil {
		t.Error("forward kill with no id should be a usage error")
	}
	if _, err := ParseCommand("forward ls", ""); err != nil {
		t.Errorf("forward ls: %v", err)
	}
}
```

`ParseCommand(input, defaultRepo string) (Action, error)` is the entry point
(`tui/cmdline.go:232`). Add both forms to the `help` output (`tui/app.go:1550`
block).

- [ ] **Step 7: Run the tests**

Run: `go test ./tui/ -count=1 && go build ./...`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add tui/portforward.go tui/app.go tui/cmdline.go tui/portforward_test.go tui/cmdline_test.go
git commit -m "feat(tui): server-backed forwards modal with kill + forward cmdline verbs (Task 6)"
```

---

### Task 7: WebUI — forwards panel, kill button, `forward` command

**Files:**
- Modify: `cmd/harness-webui-wasm/main.go` (snapshot payload + new export)
- Modify: `webui/index.html` (panel markup in the 接続 tab)
- Modify: `webui/static/main.js` (render + `runCmd` verb + help)

**Interfaces:**
- Consumes: `(*cli.Client).PortForwardListWith`, `.KillPortForwardWith` (Task 3).
- Produces: `harness.snapshot()` result gains `forwards: [{forward_id, dir, task, spec, origin_kind, origin_cid}]`; new export `harness.forwardKill(forwardId)` returning a Promise.

- [ ] **Step 1: Grep the sibling pattern**

Read how `conns` reaches the browser: `harnessSnapshot` in
`cmd/harness-webui-wasm/main.go`, then `renderConnList` and its call site
(`webui/static/main.js:462`). Read `harnessBoardPurge` as the shape of a
one-argument action export returning a Promise. Match those, and state in your
report which functions you mirrored.

- [ ] **Step 2: Extend the snapshot**

In `harnessSnapshot`, after the conns section, call `PortForwardListWith` on the
live client and append a `forwards` array whose objects carry `forward_id` (number),
`dir` (`"-L"`/`"-R"` via `cli.PortForwardDirFlag`), `task` (hex string), `spec`
(via `cli.PortForwardSpecString`), `origin_kind`, `origin_cid`. A failure here
must not fail the whole snapshot — log and emit an empty array, exactly as the
conns section already degrades.

- [ ] **Step 3: Add the kill export**

```go
// harnessForwardKill closes one registered port forward. Unlike board seq (which
// must travel as a decimal string because it is UnixNano-seeded and exceeds JS's
// 2^53 safe range), forward ids come from a small `next++` counter, so a JS
// number round-trips exactly.
func harnessForwardKill(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			if len(args) < 1 {
				rejectErr(reject, errors.New("forwardKill: missing forward id"))
				return
			}
			id := uint64(args[0].Float())
			if err := c.KillPortForwardWith(rootCtx, id); err != nil {
				rejectErr(reject, err)
				return
			}
			resolve.Invoke(js.Null())
		}()
		return nil
	})
	return js.Global().Get("Promise").New(executor)
}
```

Register it in the `js.Global().Set("harness", …)` map (`cmd/harness-webui-wasm/main.go:78`)
as `"forwardKill": js.FuncOf(harnessForwardKill),`. Check the exact Promise
construction and `rejectErr` usage against `harnessBoardPurge`
(`cmd/harness-webui-wasm/main.go:664`) — copy its tail verbatim if it differs
from the sketch above.

- [ ] **Step 4: Render the panel**

In `webui/index.html`, inside `<section id="connections" data-tabgroup="n">`, add:

```html
      <h3>ポートフォワード</h3>
      <div id="forward-list"></div>
```

In `webui/static/main.js`, call `renderForwardList(snap.forwards || [])` right
after the existing `renderConnList(conns, allTasks)` (line ~463) and add:

```js
// renderForwardList draws one row per registered port forward, each with a kill
// button. Mirrors renderConnList: build DOM, no innerHTML with server strings.
function renderForwardList(forwards) {
  const host = document.getElementById("forward-list");
  if (!host) return;
  host.textContent = "";
  if (!forwards.length) {
    const empty = document.createElement("div");
    empty.className = "muted";
    empty.textContent = "アクティブなポートフォワードはありません";
    host.appendChild(empty);
    return;
  }
  for (const f of forwards) {
    const row = document.createElement("div");
    row.className = "forward-row";
    for (const text of [`#${f.forward_id}`, f.dir, shortID(f.task), f.spec, f.origin_kind]) {
      const cell = document.createElement("span");
      cell.className = "forward-cell";
      cell.textContent = text;
      row.appendChild(cell);
    }
    const kill = document.createElement("button");
    kill.type = "button";
    kill.textContent = "kill";
    kill.addEventListener("click", async () => {
      kill.disabled = true;
      try {
        await harness.forwardKill(f.forward_id);
        await refreshSnapshot();
      } catch (e) {
        appendCmdOutput(`forward kill: ${e}`);
        kill.disabled = false;
      }
    });
    row.appendChild(kill);
    host.appendChild(row);
  }
}
```

Use whatever this file already calls its short-id helper and its snapshot
refresher — grep for the helper used by `renderConnList` and for the function the
existing action buttons call after mutating state, and use those names instead of
`shortID` / `refreshSnapshot` if they differ.

In `webui/static/style.css`, style `.forward-row` on the existing dark palette
(`#1e1e1e` background, `#d4d4d4` text) as a flex row that **wraps to stacked
cells under 600px** — the page body must never scroll sideways on a phone.

- [ ] **Step 5: Add the command verb**

In `runCmd` (`webui/static/main.js:1253`), add `case "forward":` alongside
`case "file":`:

```js
        case "forward": {
          const sub = tokens[1];
          if (sub === "ls") {
            const fs = (snap.forwards || []);
            out = fs.length
              ? fs.map((f) => `#${f.forward_id}  ${f.dir}  ${shortID(f.task)}  ${f.spec}  ${f.origin_kind}`).join("\n")
              : "(no active port forwards)";
          } else if (sub === "kill") {
            const id = parseInt(tokens[2], 10);
            if (!Number.isFinite(id)) throw new Error("forward kill: usage: forward kill <forward-id>");
            await harness.forwardKill(id);
            await refreshSnapshot();
            out = `killed forward ${id}`;
          } else {
            throw new Error("forward: usage: forward ls | forward kill <forward-id>");
          }
          break;
        }
```

`forward ls` renders from the snapshot the page already polls — no extra RPC and
no second wasm export. Add both forms to the `case "help":` output.

- [ ] **Step 6: Build and verify in a browser**

Run:

```bash
make wasm-check
make webui-build
```

Then load the WebUI (`HARNESS_SERVER_CID` from the environment), open a `-L`
forward from a CLI terminal, and confirm it appears in the 接続 tab, that the
kill button removes it, and that the CLI terminal exits. Check the layout at both
desktop width and 390px. WebUI/wasm changes hot-reload — a browser refresh is
enough, no server restart.

- [ ] **Step 7: Commit**

```bash
git add cmd/harness-webui-wasm/main.go webui/index.html webui/static/main.js
git commit -m "feat(webui): port-forward panel with kill + forward command (Task 7)"
```

---

### Task 8: Full verification

**Files:**
- Modify: none expected (fix whatever the checks surface).

**Interfaces:**
- Consumes: everything above.
- Produces: evidence that the feature works end to end.

- [ ] **Step 1: Run every static check**

Run:

```bash
make check && make wasm-check && make vet && make test
```

Expected: all pass. `make check`/`make wasm-check` use explicit package patterns —
they catch breakage that `go build ./...` alone hides.

- [ ] **Step 2: Run the integration suite**

Run: `make test-integration`
Expected: PASS, including `TestLocalForwardRegisterListKill`.

- [ ] **Step 3: Run the wire-skew check**

Run: `scripts/wire-skew-check.sh`
Expected: PASS. This task changed `.bgn`, so the check is not a no-op — it proves
a NEW runner against an OLD server fails *recoverably*. If it reports a setup
error (exit 2), fix the setup and re-run; do not treat exit 2 as a pass.

- [ ] **Step 4: Drive it on a dummy harness**

Follow `.claude/skills/dummy-harness/SKILL.md` to stand up a local dummy server +
runner **from this worktree's own `bin/`** (`make build` first — `go build ./...`
does not refresh `bin/`). Then, with a real interactive task running:

1. In terminal A: `bin/harness-cli forward <task> -L 18080:127.0.0.1:9`
2. In terminal B: `bin/harness-cli forward ls` — the forward appears with an id.
3. In terminal B: `bin/harness-cli forward kill <id>`
4. Terminal A prints `forward stopped: killed remotely` and **returns to its prompt**.
5. `bin/harness-cli forward ls` now lists nothing.
6. Repeat 1-5 driving the kill from the TUI modal (`f`, then `k`) and from the
   WebUI panel.
7. Start two forwards from one terminal (`-L 18080:… -L 18081:…`), kill one, and
   confirm the terminal stays alive with the other still forwarding.

Record the actual terminal output in your report. A unit test passing is not
evidence for this step.

- [ ] **Step 5: Commit any fixes**

```bash
git add -A
git commit -m "fix: address verification findings (Task 8)"
```

(Skip the commit if nothing needed fixing — say so instead of inventing a change.)

---

## Landing

Not part of the task list — the controller runs this after Task 8 passes, using
the `landing-to-main` skill: rebase this branch onto current local `main`,
fast-forward `main`, push, then `make build` in the main checkout. **Restart the
server before the runners** if the fleet is upgraded: this change alters
client↔server messages, and an old server with new clients breaks the forward
path. Runner daemons themselves need no restart for this change.
