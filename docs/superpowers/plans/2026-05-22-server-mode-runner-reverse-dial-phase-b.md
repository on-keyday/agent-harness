# Phase B: Agent leg via objproto negotiated proxy — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Allow `harness-cli agent send / wait / inbox / ...` (and other harness-cli
calls from inside the runner host) to reach the server even when the runner network
cannot dial the server directly. The runner becomes a transparent objproto packet
relay; agent↔server retains end-to-end ECDH + PSK + AuthTicket. No server-side code
changes.

**Architecture:** Use objproto's existing `Endpoint.SetProxy` /
`Connection.RehandshakeForProxy` (ksdk-era, no harness caller yet). Agent dials the
runner's Mutual endpoint (Phase A built it). Agent sends `ProxyRequest{task_id}`;
runner validates and calls `SetProxy(agent_cid, server_addr_cid)` plus sends ack.
Agent then `RehandshakeForProxy` with a fresh ECDH key — the new handshake packets
go agent → runner → server (via the existing Phase A reverse-dial conn, reused via
`transport.connMap`), and the resulting peer.Conn is end-to-end agent↔server.

**Tech Stack:** Go, brgen (`.bgn` schema), objproto.SetProxy, peer.WrapAcceptedConn.

**Spec:** `docs/superpowers/specs/2026-05-22-server-mode-runner-reverse-dial-design.md` Phase B.
**Prereq:** Phase A merged (commit `dba8dd5` on main, 2026-05-22).

---

## File Structure

| File | Action | Responsibility |
|---|---|---|
| `trsf/wire/stream.bgn` | Modify | Add `agent_proxy_control` to `ApplicationPayloadKind` enum |
| `trsf/wire/stream.go` | Regenerated | Output of protoregen |
| `runner/protocol/message.bgn` | Modify | Add `ProxyControlKind`, `ProxyRequest`, `ProxyEstablishStatus`, `ProxyEstablishResponse`, `ProxyControl` envelope |
| `runner/protocol/message.go` | Regenerated | Output of protoregen |
| `runner/protocol/proxy_test.go` | Create | Round-trip tests for ProxyControl variants |
| `runner/session.go` | Modify | Add `ServerPeerCID` field on Session (set in driveAfterConn), readable by agent proxy handler |
| `runner/agent_proxy.go` | Create | Runner-side: on incoming peer.Conn that's NOT the server, expect ProxyControl first; validate; SetProxy; ack; Close |
| `runner/agent_proxy_test.go` | Create | Unit: collision check, unknown task, happy path |
| `runner/listen.go` | Modify | Route accepted conn to agent-proxy handler when it's not the first (server) conn; expose runner's server peer.Conn to the handler |
| `cli/proxy_dial.go` | Create | Client-side: `DialViaProxy(ctx, proxyAddr, serverCID, taskID)` — does peer.Dial(runner) → ProxyRequest → ack → RehandshakeForProxy → returns new *peer.Conn |
| `cli/proxy_dial_test.go` | Create | Unit with a fake runner endpoint that simulates the ceremony |
| `cli/agent/conn.go` | Modify | When `HARNESS_PROXY_VIA_RUNNER` env is set, call `cli.DialViaProxy` instead of `peer.Dial(server.Addr)` |
| `cli/client.go` | Modify | `cli.Dial` also honors `HARNESS_PROXY_VIA_RUNNER` (with empty taskID — proxy ceremony works for non-task callers; see Task 5 detail) |
| `runner/agentenv.go` | Modify | Add `HARNESS_PROXY_VIA_RUNNER` to spawn env when in listen mode |
| `runner/session.go` | Modify | Expose listen-mode addr to AgentEnvSpec construction so BuildAgentEnv can inject it |
| `integration/server_dial_runner_agent_proxy_test.go` | Create | End-to-end: server + runner --listen + ServerDialRunner + spawn agent process via direct API + assert agentboard message reaches server |

---

## Task 1: Wire schema for proxy ceremony

**Files:**
- Modify: `trsf/wire/stream.bgn`
- Regenerated: `trsf/wire/stream.go`
- Modify: `runner/protocol/message.bgn`
- Regenerated: `runner/protocol/message.go`
- Test: `runner/protocol/proxy_test.go`

- [ ] **Step 1: Write the failing test**

Create `runner/protocol/proxy_test.go`:

```go
package protocol

import (
	"bytes"
	"testing"
)

func TestProxyControlRequestRoundTrip(t *testing.T) {
	var inner ProxyRequest
	copy(inner.TaskId.Id[:], []byte("0123456789abcdef"))

	var pc ProxyControl
	pc.Kind = ProxyControlKind_Request
	pc.SetRequest(inner)

	buf, err := pc.Append(nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	var got ProxyControl
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Kind != ProxyControlKind_Request {
		t.Errorf("kind: got %v", got.Kind)
	}
	req := got.Request()
	if req == nil {
		t.Fatal("Request() returned nil")
	}
	if !bytes.Equal(req.TaskId.Id[:], inner.TaskId.Id[:]) {
		t.Errorf("task_id: got %x want %x", req.TaskId.Id, inner.TaskId.Id)
	}
}

func TestProxyControlEstablishResponseRoundTrip(t *testing.T) {
	resp := ProxyEstablishResponse{Status: ProxyEstablishStatus_IdCollision}

	var pc ProxyControl
	pc.Kind = ProxyControlKind_EstablishResponse
	pc.SetEstablishResponse(resp)

	buf, err := pc.Append(nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	var got ProxyControl
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	er := got.EstablishResponse()
	if er == nil {
		t.Fatal("EstablishResponse() returned nil")
	}
	if er.Status != ProxyEstablishStatus_IdCollision {
		t.Errorf("status: got %v", er.Status)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./runner/protocol/ -run TestProxyControl -v`
Expected: FAIL — `undefined: ProxyRequest` / `ProxyControlKind_Request` etc.

- [ ] **Step 3: Add wire kind for ProxyControl**

In `trsf/wire/stream.bgn`, append `agent_proxy_control` to the `ApplicationPayloadKind` enum (after `psk_auth`):

```bgn
enum ApplicationPayloadKind:
    :u8
    ping
    pong
    stream_data
    stream_cancel
    stream_ack
    stream_window_update
    pubsub
    task_control
    relay_control
    runner_control
    close
    agent_message
    psk_auth
    agent_proxy_control   # Phase B: agent↔runner negotiated-proxy ceremony
```

- [ ] **Step 4: Add ProxyControl schema to message.bgn**

In `runner/protocol/message.bgn`, insert this block immediately after the existing
`DialRunnerResponse` block (which is around line 197-198):

```bgn
# Phase B: agent ↔ runner negotiated-proxy ceremony.
# Agent sends ProxyRequest as the first app message after peer.Dial-ing the
# runner; runner replies with ProxyEstablishResponse. See spec
# docs/superpowers/specs/2026-05-22-server-mode-runner-reverse-dial-design.md Phase B.

format ProxyRequest:
    task_id :TaskID

enum ProxyEstablishStatus:
    :u8
    ok                  = "ok"
    id_collision        = "id_collision"        # agent's connection_id collides with runner's server-conn CID; retry with a different ID
    server_not_connected = "server_not_connected" # runner has no live server conn; admin must re-trigger dial-runner
    unknown_task         = "unknown_task"        # task_id is not in runner's active task map

format ProxyEstablishResponse:
    status :ProxyEstablishStatus

enum ProxyControlKind:
    :u8
    request
    establish_response

format ProxyControl:
    kind :ProxyControlKind
    match kind:
        ProxyControlKind.request              => request              :ProxyRequest
        ProxyControlKind.establish_response   => establish_response   :ProxyEstablishResponse
```

- [ ] **Step 5: Regenerate Go code**

Run: `make protoregen`
Expected: success; `trsf/wire/stream.go` and `runner/protocol/message.go` updated.
`git diff --stat` should show only the 4 expected files.

- [ ] **Step 6: Run the test**

Run: `go test ./runner/protocol/ -run TestProxyControl -v`
Expected: both subtests PASS.

- [ ] **Step 7: Run full package tests**

Run: `go test ./runner/protocol/ ./trsf/wire/ -count=1`
Expected: all PASS (additive changes).

- [ ] **Step 8: Commit**

```bash
git add trsf/wire/stream.bgn trsf/wire/stream.go runner/protocol/message.bgn runner/protocol/message.go runner/protocol/proxy_test.go
git commit -m "feat(protocol): add ProxyControl wire (ProxyRequest/EstablishResponse) for Phase B"
```

---

## Task 2: Expose runner's server-conn CID on Session

**Files:**
- Modify: `runner/session.go` — add `ServerPeerCID objproto.ConnectionID` field
- Modify: `runner/connect.go` — populate it in `driveAfterConn`

**Background:** Currently `Session.ServerCID` is the server CID (from runner's view —
already populated from `pc.Connection().ConnectionID()` per Phase A's fix). For Phase
B, the agent proxy handler needs the **server-side endpoint coordinates** so it can
construct the SetProxy "allocate" CID using the server's addr + transport. The
`Session.ServerCID` already has this — we'll just confirm it's the right field and
add a struct comment if needed. NO new field required.

→ **This task is a documentation/clarity-only refinement: confirm Session.ServerCID
is the right source, add a comment, and provide a deterministic accessor.**

- [ ] **Step 1: Verify Session.ServerCID semantics**

Read `runner/session.go` and `runner/connect.go`. Confirm: after Phase A's fix,
`Session.ServerCID = pc.Connection().ConnectionID()` (the peer-side CID from runner's
view). This is exactly what Phase B's SetProxy needs for the allocate side.

- [ ] **Step 2: Add accessor on Session**

In `runner/session.go`, add a small helper for clarity:

```go
// ServerCIDForProxyAllocate returns the ConnectionID the runner uses for its
// server peer.Conn. SetProxy's "allocate" CID for a proxied agent uses this
// transport + addr and the agent-chosen connection_id; see Phase B spec.
func (s *Session) ServerCIDForProxyAllocate() objproto.ConnectionID {
	return s.ServerCID
}
```

(This is intentionally a trivial wrapper — it gives the agent-proxy handler a single,
well-named call site so future refactors can change the source without touching the
proxy handler.)

- [ ] **Step 3: Run tests**

Run: `go test ./runner/ -count=1`
Expected: all PASS (no behavior change).

- [ ] **Step 4: Commit**

```bash
git add runner/session.go
git commit -m "refactor(runner): add Session.ServerCIDForProxyAllocate accessor for Phase B"
```

---

## Task 3: Runner agent-proxy handler

**Files:**
- Create: `runner/agent_proxy.go`
- Create: `runner/agent_proxy_test.go`
- Modify: `runner/listen.go` — route accepted conn to either driveAfterConn (server)
  or the agent-proxy handler based on the first app message

**Background:** The Phase A `ListenAndServe` accept loop calls `driveAfterConn` for
every accepted conn. After Phase A, that's fine — there's only the server conn. In
Phase B, agent processes also dial into the same endpoint. The runner needs to
discriminate between server conns (Phase A reverse-dial) and agent conns (Phase B
proxy ceremony) BEFORE proceeding.

**Discrimination strategy — timing-based peek:**

Reading Phase A's flow:
- Server dials runner → runner accepts → `driveAfterConn` calls `SendAndWaitPSK`
  which SENDS PskAuth out. Server's `pskGate.Check` validates and replies. So on a
  server conn, the first INBOUND message arrives only AFTER the runner sends PSK.
- Agent dials runner (Phase B) → agent sends `ProxyRequest` immediately as its first
  outbound message. So on an agent conn, the first INBOUND message arrives
  proactively without runner needing to send anything.

Addr-based discrimination fails for localhost-only dev setups (both server and agent
appear from `127.0.0.1`). Source-port differs but is not deterministically tied to
role. So use timing:

1. Accept conn → wrap with `peer.WrapAcceptedConn`
2. Install OnControl that pushes first inbound payload to a channel
3. Start AutoReceive
4. Wait up to 300ms for an inbound message:
   - If `agent_proxy_control` arrives → agent path (runAgentProxyCeremony with the
     payload already in hand)
   - If 300ms timeout → server path (driveAfterConn — re-installs its own OnControl
     and proceeds with PSK send)
   - If `PskAuth` arrives (unexpected — would mean someone is dialing in and sending
     PSK proactively, which neither Phase A nor Phase B does) → log warning, close

The 300ms threshold is comfortably larger than localhost ECDH+app-message latency
(typically <10ms) and well below typical user-facing timeouts. If an agent fails to
send ProxyRequest within 300ms (e.g., network glitch on a real remote agent), the
runner falls through to server-conn handling and the PSK timeout will eventually
clean up.

After discrimination, the chosen handler proceeds:
- Server path: `driveAfterConn` runs normally; if it succeeds, store its session
  in a runner-wide reference so subsequent agent-proxy handlers can read serverCID.
- Agent path: decode the captured `ProxyControl{kind=Request}`; run the proxy
  ceremony (SetProxy → ack → caller closes pc).

The agent proxy handler:
1. Decodes the inbound `ProxyControl{kind=Request}` → reads task_id
2. Validates task_id against `session.tasks`. Unknown task → reply `UnknownTask`, close
3. Validates connection_id doesn't collide with the server-conn CID. Collision → reply
   `IdCollision`, close
4. If session has no live server conn → reply `ServerNotConnected`, close
5. Builds `allocateCID = NewConnectionID(serverCID.Transport, serverCID.Addr, agentCID.ID)`
6. Calls `endpoint.SetProxy(agentCID, allocateCID)` (failure → reply Ok=false-equivalent;
   actually unrecoverable, log + close)
7. Sends `ProxyControl{kind=EstablishResponse, status=Ok}` to agent
8. Closes the agent peer.Conn (the underlying transport conn stays in connMap so proxy
   forward works)

- [ ] **Step 1: Write the failing tests**

Create `runner/agent_proxy_test.go`:

```go
package runner

import (
	"net/netip"
	"testing"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TestProxyHandlerCollisionDetection: when the agent's CID has the same
// connection_id as the runner's existing server-conn CID, the handler
// returns IdCollision.
func TestProxyHandlerCollisionDetection(t *testing.T) {
	const sharedID = 0x1234
	serverCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort("127.0.0.1:8539"), sharedID)
	agentCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort("127.0.0.1:55001"), sharedID)

	st := &proxyHandlerState{
		serverCID: serverCID,
		hasServerConn: true,
		taskExists:    func(taskID protocol.TaskID) bool { return true },
	}

	var taskID protocol.TaskID
	resp := st.validateProxyRequest(agentCID, taskID)
	if resp.Status != protocol.ProxyEstablishStatus_IdCollision {
		t.Errorf("status: got %v want IdCollision", resp.Status)
	}
}

// TestProxyHandlerUnknownTask: when the requested task_id is not in the
// runner's active task map, the handler returns UnknownTask.
func TestProxyHandlerUnknownTask(t *testing.T) {
	serverCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort("127.0.0.1:8539"), 0x0001)
	agentCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort("127.0.0.1:55001"), 0x0002)

	st := &proxyHandlerState{
		serverCID:     serverCID,
		hasServerConn: true,
		taskExists:    func(taskID protocol.TaskID) bool { return false },
	}

	var taskID protocol.TaskID
	taskID.Id[0] = 0xAA
	resp := st.validateProxyRequest(agentCID, taskID)
	if resp.Status != protocol.ProxyEstablishStatus_UnknownTask {
		t.Errorf("status: got %v want UnknownTask", resp.Status)
	}
}

// TestProxyHandlerServerNotConnected: when the runner has no live server
// conn (e.g. Phase A reverse-dial hasn't fired yet, or the conn was lost),
// the handler returns ServerNotConnected.
func TestProxyHandlerServerNotConnected(t *testing.T) {
	serverCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort("127.0.0.1:8539"), 0x0001)
	agentCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort("127.0.0.1:55001"), 0x0002)

	st := &proxyHandlerState{
		serverCID:     serverCID,
		hasServerConn: false,
		taskExists:    func(taskID protocol.TaskID) bool { return true },
	}

	var taskID protocol.TaskID
	resp := st.validateProxyRequest(agentCID, taskID)
	if resp.Status != protocol.ProxyEstablishStatus_ServerNotConnected {
		t.Errorf("status: got %v want ServerNotConnected", resp.Status)
	}
}

// TestProxyHandlerHappyPath: when task_id is valid and IDs don't collide,
// the handler returns Ok and the constructed allocate CID has the server's
// transport + addr and the agent's connection_id.
func TestProxyHandlerHappyPath(t *testing.T) {
	serverCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort("127.0.0.1:8539"), 0x0001)
	agentCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort("127.0.0.1:55001"), 0x0002)

	st := &proxyHandlerState{
		serverCID:     serverCID,
		hasServerConn: true,
		taskExists:    func(taskID protocol.TaskID) bool { return true },
	}

	var taskID protocol.TaskID
	resp := st.validateProxyRequest(agentCID, taskID)
	if resp.Status != protocol.ProxyEstablishStatus_Ok {
		t.Errorf("status: got %v want Ok", resp.Status)
	}

	alloc := st.allocateCID(agentCID)
	if alloc.Transport != serverCID.Transport {
		t.Errorf("alloc.Transport: got %q want %q", alloc.Transport, serverCID.Transport)
	}
	if alloc.Addr != serverCID.Addr {
		t.Errorf("alloc.Addr: got %v want %v", alloc.Addr, serverCID.Addr)
	}
	if alloc.ID != agentCID.ID {
		t.Errorf("alloc.ID: got %v want %v", alloc.ID, agentCID.ID)
	}
}
```

- [ ] **Step 2: Run tests, verify failure**

Run: `go test ./runner/ -run TestProxyHandler -v`
Expected: FAIL — `undefined: proxyHandlerState` etc.

- [ ] **Step 3: Implement the proxy handler state machine**

Create `runner/agent_proxy.go`:

```go
package runner

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// proxyHandlerState bundles the inputs the agent-proxy ceremony needs from
// the runner: the current server-conn CID (for collision detection and the
// SetProxy allocate-side construction), whether the server conn is live,
// and a task lookup. Extracted as a struct so the validation logic is
// pure-function-testable without spinning up real endpoints.
type proxyHandlerState struct {
	serverCID     objproto.ConnectionID
	hasServerConn bool
	taskExists    func(taskID protocol.TaskID) bool
}

// validateProxyRequest computes the response status for a given agent CID
// and requested task ID, applying the four error conditions in the spec.
func (s *proxyHandlerState) validateProxyRequest(agentCID objproto.ConnectionID, taskID protocol.TaskID) protocol.ProxyEstablishResponse {
	if !s.hasServerConn {
		return protocol.ProxyEstablishResponse{Status: protocol.ProxyEstablishStatus_ServerNotConnected}
	}
	if agentCID.ID == s.serverCID.ID {
		return protocol.ProxyEstablishResponse{Status: protocol.ProxyEstablishStatus_IdCollision}
	}
	if !s.taskExists(taskID) {
		return protocol.ProxyEstablishResponse{Status: protocol.ProxyEstablishStatus_UnknownTask}
	}
	return protocol.ProxyEstablishResponse{Status: protocol.ProxyEstablishStatus_Ok}
}

// allocateCID constructs the SetProxy "allocate" CID. It uses the server's
// transport + addr (so the forward goes via the existing server-conn entry
// in transport.connMap) with the agent's connection_id (so the server sees
// a new conn rather than collision with the runner-server conn).
func (s *proxyHandlerState) allocateCID(agentCID objproto.ConnectionID) objproto.ConnectionID {
	return objproto.NewConnectionID(s.serverCID.Transport, s.serverCID.Addr, agentCID.ID)
}

// runAgentProxyCeremony handles a single accepted peer.Conn that has been
// determined to be an agent (first message is agent_proxy_control). It:
//
//  1. Reads the first inbound ProxyControl{kind=Request} via the pc's
//     OnControl callback (already wired by handleAcceptedConn — see Step 5).
//  2. Validates with proxyHandlerState; on error, sends the error response
//     and returns (caller closes pc).
//  3. On Ok: SetProxy(pc.ConnectionID, allocateCID); sends Ok response;
//     returns (caller closes pc, which removes the activeConn but leaves
//     the transport WS entry in connMap so the proxy forward works).
//
// The order is: SetProxy → ack → caller closes (per spec's order constraints).
func runAgentProxyCeremony(
	ctx context.Context,
	logger *slog.Logger,
	st *proxyHandlerState,
	ep objproto.Endpoint,
	pc *peer.Conn,
	req protocol.ProxyRequest,
) error {
	agentCID := pc.Connection().ConnectionID()
	resp := st.validateProxyRequest(agentCID, req.TaskId)

	if resp.Status == protocol.ProxyEstablishStatus_Ok {
		alloc := st.allocateCID(agentCID)
		if err := ep.SetProxy(agentCID, alloc); err != nil {
			logger.Error("agent proxy: SetProxy failed",
				"agent_cid", agentCID.String(),
				"alloc_cid", alloc.String(),
				"err", err)
			return fmt.Errorf("SetProxy: %w", err)
		}
		logger.Info("agent proxy: established",
			"agent_cid", agentCID.String(),
			"alloc_cid", alloc.String(),
			"task_id", fmt.Sprintf("%x", req.TaskId.Id))
	}

	// Send response on the active peer.Conn. Order: SetProxy (above) →
	// SendMessage (here) → caller's pc.Close().
	var pc_ protocol.ProxyControl
	pc_.Kind = protocol.ProxyControlKind_EstablishResponse
	pc_.SetEstablishResponse(resp)
	payload := pc_.MustAppend([]byte{byte(wire.ApplicationPayloadKind_AgentProxyControl)})
	if _, _, err := pc.Connection().SendMessage(payload); err != nil {
		return fmt.Errorf("send EstablishResponse: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Wire dispatch in `runner/listen.go`**

Modify `handleAcceptedConn` to install OnControl + start AutoReceive + wait up to
300ms for first inbound message, then dispatch:

```go
// In runner/listen.go, replace handleAcceptedConn body:

type firstMsgT struct {
	kind    wire.ApplicationPayloadKind
	payload []byte
}

func handleAcceptedConn(ctx context.Context, cfg Config, sessionRef *atomic.Pointer[Session], ep objproto.Endpoint, pc *peer.Conn) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	// Peek the first inbound app message. Agent conns (Phase B) send
	// ProxyRequest immediately; server conns (Phase A) wait for the
	// runner to send PSK first and only respond after.
	firstMsg := make(chan firstMsgT, 1)
	pc.SetOnControl(func(kind wire.ApplicationPayloadKind, payload []byte) {
		select {
		case firstMsg <- firstMsgT{kind: kind, payload: payload}:
		default:
		}
	})
	pc.Start(ctx)

	const discrimTimeout = 300 * time.Millisecond
	select {
	case <-ctx.Done():
		pc.Close()
		return
	case msg := <-firstMsg:
		if msg.kind == wire.ApplicationPayloadKind_AgentProxyControl {
			handleAgentProxyConn(ctx, cfg, sessionRef, ep, pc, msg)
			return
		}
		cfg.Logger.Warn("accepted conn sent unexpected first payload",
			"kind", msg.kind,
			"remote", pc.Connection().ConnectionID().String())
		pc.Close()
		return
	case <-time.After(discrimTimeout):
		// Timeout → server conn path (no proactive inbound, server is
		// waiting for our PSK). Fall through to driveAfterConn-style
		// handling. driveAfterConn re-installs its own OnControl handler
		// (replacing the peek handler) before sending PSK.
		handleServerConn(ctx, cfg, sessionRef, pc)
	}
}

func handleServerConn(ctx context.Context, cfg Config, sessionRef *atomic.Pointer[Session], pc *peer.Conn) {
	h, err := driveAfterConn(ctx, cfg, pc)
	if err != nil {
		cfg.Logger.Error("server conn: PSK/setup failed", "err", err)
		return
	}
	sessionRef.Store(h.session)
	defer h.Close()
	if err := OnConnect(ctx, h); err != nil {
		cfg.Logger.Error("server conn: OnConnect failed", "err", err)
	}
}

func handleAgentProxyConn(ctx context.Context, cfg Config, sessionRef *atomic.Pointer[Session], ep objproto.Endpoint, pc *peer.Conn, first firstMsgT) {
	defer pc.Close()

	var pcEnvelope protocol.ProxyControl
	if _, err := pcEnvelope.Decode(first.payload[1:]); err != nil {
		cfg.Logger.Warn("agent proxy: decode ProxyControl failed", "err", err)
		return
	}
	if pcEnvelope.Kind != protocol.ProxyControlKind_Request {
		cfg.Logger.Warn("agent proxy: first message is not Request", "kind", pcEnvelope.Kind)
		return
	}
	req := pcEnvelope.Request()
	if req == nil {
		cfg.Logger.Warn("agent proxy: Request variant nil")
		return
	}

	sess := sessionRef.Load()
	var serverCID objproto.ConnectionID
	hasServerConn := sess != nil
	if hasServerConn {
		serverCID = sess.ServerCIDForProxyAllocate()
	}
	taskExists := func(t protocol.TaskID) bool {
		if sess == nil {
			return false
		}
		return sess.HasTask(t)
	}

	st := &proxyHandlerState{
		serverCID:     serverCID,
		hasServerConn: hasServerConn,
		taskExists:    taskExists,
	}

	if err := runAgentProxyCeremony(ctx, cfg.Logger, st, ep, pc, *req); err != nil {
		cfg.Logger.Error("agent proxy ceremony failed", "err", err)
	}
}
```

And in `ListenAndServe`, set up the shared `sessionRef` before the accept loop:

```go
var sessionRef atomic.Pointer[Session]
// ... after endpoint built ...
for {
    select {
    ...
    case conn, ok := <-connCh:
        if !ok { return nil }
        pc := peer.WrapAcceptedConn(ctx, conn, peer.DialConfig{
            Logger:       cfg.Logger,
            PingInterval: cfg.PingInterval,
        })
        go handleAcceptedConn(ctx, cfg.Config, &sessionRef, ep, pc)
    }
}
```

Add the supporting changes:
- Pass `ep` (the endpoint built by `buildListenEndpoint`) into `handleAcceptedConn`
  as an explicit parameter so the agent-proxy handler can call `SetProxy` on it.
- Use `atomic.Pointer[Session]` for the shared session reference so concurrent
  agents arriving while the server-conn path is still completing setup don't
  see torn state.
- Add `Session.HasTask(t protocol.TaskID) bool` helper in `runner/session.go`:

```go
// HasTask reports whether t is currently an active task on this session.
// Used by the agent proxy handler to validate ProxyRequest.task_id.
func (s *Session) HasTask(t protocol.TaskID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.tasks[hex.EncodeToString(t.Id[:])]
	return ok
}
```

(Verify `s.mu` is the right lock by reading session.go's tasks-map locking — search
for `s.mu.Lock()` near `s.tasks` references.)

- [ ] **Step 5: Run unit tests**

Run: `go test ./runner/ -run TestProxyHandler -v -count=1`
Expected: 4/4 PASS.

- [ ] **Step 6: Run full runner tests**

Run: `go test ./runner/... -count=1`
Expected: PASS (existing Phase A integration test in listen mode still works because
the server-conn path is preserved).

- [ ] **Step 7: Commit**

```bash
git add runner/agent_proxy.go runner/agent_proxy_test.go runner/listen.go runner/connect.go runner/session.go
git commit -m "feat(runner): agent-proxy ceremony handler (Phase B objproto SetProxy)"
```

---

## Task 4: Client-side proxy dial helper

**Files:**
- Create: `cli/proxy_dial.go`
- Create: `cli/proxy_dial_test.go`

**Background:** Client side of the ceremony. `DialViaProxy` takes (ctx, proxyAddr,
serverCID, taskID) and returns a `*peer.Conn` that is end-to-end with the server.

Steps inside DialViaProxy:
1. Build a client endpoint for proxyAddr's transport
2. Choose a random uint16 connection_id (`X`) — should not collide with anything else
   (collision detection is on the server side; we retry on `IdCollision`)
3. `peer.Dial(ctx, ep, NewConnectionID(transport, proxyAddr, X), cfg)` → agent↔runner
   peer.Conn
4. Install OnControl callback for `agent_proxy_control`
5. Send `ProxyControl{kind=Request, request={task_id=taskID}}`
6. Wait for `ProxyControl{kind=EstablishResponse}` via the callback
7. On non-Ok: return error (caller may retry)
8. On Ok: call `localConn.Connection().RehandshakeForProxy(newKey, newHS)` where
   `newKey, newHS = objproto.NewECDHHandshake(ecdh.P521(), AES128GCM)`
9. Wait for the new `objproto.Connection` from rh.C
10. Wrap with `peer.WrapAcceptedConn(ctx, newConn, peer.DialConfig{...})` and return

- [ ] **Step 1: Write the failing test**

Create `cli/proxy_dial_test.go`:

```go
package cli

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/transport"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// TestDialViaProxyHandlesIdCollision: when the runner replies with
// IdCollision, DialViaProxy returns a typed error indicating the caller
// should retry with a different ID.
func TestDialViaProxyHandlesIdCollision(t *testing.T) {
	// Start a tiny fake runner endpoint that always replies IdCollision.
	mux, runnerAddr := startFakeRunner(t, func(pc *peer.Conn, req protocol.ProxyRequest) protocol.ProxyEstablishResponse {
		return protocol.ProxyEstablishResponse{
			Status: protocol.ProxyEstablishStatus_IdCollision,
		}
	})
	_ = mux

	var taskID protocol.TaskID
	taskID.Id[0] = 0xAB

	proxyCID, err := objproto.ParseConnectionID("ws:"+runnerAddr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err = DialViaProxy(ctx, proxyCID, taskID)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrProxyIdCollision) {
		t.Errorf("expected ErrProxyIdCollision, got %v", err)
	}
}

// startFakeRunner spins up a localhost Mutual-mode WS endpoint that accepts
// one peer.Conn, decodes the first ProxyControl, calls respond(), sends the
// reply, and returns. Used by the proxy-dial unit tests.
func startFakeRunner(t *testing.T, respond func(*peer.Conn, protocol.ProxyRequest) protocol.ProxyEstablishResponse) (net.Listener, string) {
	t.Helper()
	// Implementation: build a transport.WebSocketEndpoint with
	// EndpointModeMutual, attach to a localhost http.Server, accept one
	// conn from GetNewActiveConnectionChannel, wrap with
	// peer.WrapAcceptedConn, install OnControl that waits for the first
	// ProxyControl payload, then calls respond + sends the reply.
	// See runner/listen.go for the established pattern.
	t.Skip("startFakeRunner needs implementation against transport package — see hint in test body")
	return nil, ""
}
```

(The test is partial — the fake runner needs filling in during implementation. Plan it
as a real implementation task: copy the relevant Mutual endpoint setup from
`runner/listen.go`'s `buildListenEndpoint` and adapt for test scope.)

- [ ] **Step 2: Run test, verify failure**

Run: `go test ./cli/ -run TestDialViaProxy -v`
Expected: undefined `DialViaProxy` / `ErrProxyIdCollision`.

- [ ] **Step 3: Implement DialViaProxy**

Create `cli/proxy_dial.go`:

```go
package cli

import (
	"context"
	"crypto/ecdh"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// Sentinel errors returned by DialViaProxy. Callers can errors.Is-check to
// decide whether to retry (ErrProxyIdCollision) or fail fast (others).
var (
	ErrProxyIdCollision      = errors.New("proxy: connection_id collision with runner's server conn (retry with different id)")
	ErrProxyServerNotConnected = errors.New("proxy: runner has no live server conn")
	ErrProxyUnknownTask      = errors.New("proxy: runner does not have this task")
	ErrProxyUnexpectedStatus = errors.New("proxy: runner returned unexpected status")
)

// DialViaProxy establishes an end-to-end peer.Conn to the harness server,
// going through a runner that acts as an objproto-level packet relay.
//
//   - proxyCID:   the runner's listen-side ConnectionID (e.g. ws:host:port-*)
//   - taskID:     the task this agent is bound to (server validates via AuthTicket
//                 later; runner validates that the task exists in its session)
//
// On IdCollision, the function retries up to 3 times with fresh random IDs.
// Other non-Ok statuses are returned as typed errors so the caller can
// distinguish (e.g. surface to the user).
func DialViaProxy(ctx context.Context, proxyCID objproto.ConnectionID, taskID protocol.TaskID) (*peer.Conn, error) {
	const maxRetries = 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		// Randomize the connection_id on each attempt.
		proxyCID.ID = uint16(rand.Uint32() & 0xFFFF) // #nosec G404 — collision retry, not crypto

		pc, err := dialViaProxyAttempt(ctx, proxyCID, taskID)
		if err == nil {
			return pc, nil
		}
		if errors.Is(err, ErrProxyIdCollision) && attempt < maxRetries-1 {
			continue
		}
		return nil, err
	}
	return nil, ErrProxyIdCollision
}

func dialViaProxyAttempt(ctx context.Context, proxyCID objproto.ConnectionID, taskID protocol.TaskID) (*peer.Conn, error) {
	ep, err := BuildClientEndpoint(proxyCID)
	if err != nil {
		return nil, fmt.Errorf("build client endpoint: %w", err)
	}
	go objproto.AutoGarbageCollect(ep, 10*time.Second, 30*time.Second, 1*time.Minute, 5*time.Minute)

	localConn, err := peer.Dial(ctx, ep, proxyCID, peer.DialConfig{
		Logger:       slog.Default(),
		PingInterval: 15 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("peer.Dial(proxy): %w", err)
	}
	// localConn becomes a "child" of the new conn after RehandshakeForProxy;
	// do NOT defer Close here.

	// Install handler for the EstablishResponse.
	respCh := make(chan protocol.ProxyEstablishResponse, 1)
	localConn.SetOnControl(func(kind wire.ApplicationPayloadKind, payload []byte) {
		if kind != wire.ApplicationPayloadKind_AgentProxyControl {
			return
		}
		var pc protocol.ProxyControl
		if _, err := pc.Decode(payload[1:]); err != nil {
			return
		}
		if pc.Kind != protocol.ProxyControlKind_EstablishResponse {
			return
		}
		if resp := pc.EstablishResponse(); resp != nil {
			select {
			case respCh <- *resp:
			default:
			}
		}
	})
	localConn.Start(ctx)

	// Send ProxyRequest.
	var req protocol.ProxyControl
	req.Kind = protocol.ProxyControlKind_Request
	req.SetRequest(protocol.ProxyRequest{TaskId: taskID})
	if _, _, err := localConn.Connection().SendMessage(req.MustAppend([]byte{byte(wire.ApplicationPayloadKind_AgentProxyControl)})); err != nil {
		localConn.Close()
		return nil, fmt.Errorf("send ProxyRequest: %w", err)
	}

	// Wait for the EstablishResponse (or timeout).
	respCtx, respCancel := context.WithTimeout(ctx, 10*time.Second)
	defer respCancel()
	var resp protocol.ProxyEstablishResponse
	select {
	case <-respCtx.Done():
		localConn.Close()
		return nil, fmt.Errorf("waiting for EstablishResponse: %w", respCtx.Err())
	case resp = <-respCh:
	}

	switch resp.Status {
	case protocol.ProxyEstablishStatus_Ok:
		// fall through to rehandshake
	case protocol.ProxyEstablishStatus_IdCollision:
		localConn.Close()
		return nil, ErrProxyIdCollision
	case protocol.ProxyEstablishStatus_ServerNotConnected:
		localConn.Close()
		return nil, ErrProxyServerNotConnected
	case protocol.ProxyEstablishStatus_UnknownTask:
		localConn.Close()
		return nil, ErrProxyUnknownTask
	default:
		localConn.Close()
		return nil, fmt.Errorf("%w: %v", ErrProxyUnexpectedStatus, resp.Status)
	}

	// SetProxy is in effect on the runner. Rehandshake to derive new keys
	// with the actual server (the handshake packets are forwarded by the
	// runner). The returned channel yields the new objproto.Connection
	// once HandshakeAck arrives back through the proxy.
	newKey, newHS, err := objproto.NewECDHHandshake(ecdh.P521(), objproto.AES128GCM)
	if err != nil {
		return nil, fmt.Errorf("new ECDH handshake: %w", err)
	}
	rh, err := localConn.Connection().RehandshakeForProxy(newKey, newHS)
	if err != nil {
		return nil, fmt.Errorf("RehandshakeForProxy: %w", err)
	}
	rhCtx, rhCancel := context.WithTimeout(ctx, 10*time.Second)
	defer rhCancel()
	var newConn objproto.Connection
	select {
	case <-rhCtx.Done():
		return nil, fmt.Errorf("waiting for rehandshake: %w", rhCtx.Err())
	case newConn = <-rh.C:
	}

	// Wrap the new objproto.Connection in a peer.Conn ready for PSK +
	// AgentBridgeHello. The old localConn is automatically closed when
	// newConn closes (proxyConnection field in handshakeInfo).
	return peer.WrapAcceptedConn(ctx, newConn, peer.DialConfig{
		Logger:       slog.Default(),
		PingInterval: 15 * time.Second,
	}), nil
}
```

- [ ] **Step 4: Implement the fake runner helper in the test file**

Fill in `startFakeRunner` against the `transport` package. Pattern: copy from
`runner/listen.go`'s `buildListenEndpoint` for the WS-only branch, then accept ONE
conn via `GetNewActiveConnectionChannel`, wrap, install OnControl waiting for the
first `agent_proxy_control` payload, decode, call `respond()`, send back the reply.

- [ ] **Step 5: Run tests**

Run: `go test ./cli/ -run TestDialViaProxy -v -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add cli/proxy_dial.go cli/proxy_dial_test.go
git commit -m "feat(cli): DialViaProxy — client side of objproto negotiated-proxy ceremony"
```

---

## Task 5: Integrate proxy dial into cli/agent.ConnectAgent

**Files:**
- Modify: `cli/agent/conn.go`

**Background:** When `HARNESS_PROXY_VIA_RUNNER` env is set, ConnectAgent must dial via
the proxy instead of directly to the server. The taskID is in env (`HARNESS_TASK_ID`)
already used by cli/agent. After DialViaProxy returns the end-to-end peer.Conn, the
remaining flow (PSK + AgentBridgeHello + control loop) runs unchanged because the
peer.Conn is functionally identical to a directly-dialed one.

- [ ] **Step 1: Write the failing test**

This is hard to unit-test without spinning up server + runner. Defer behavioural
verification to the integration test in Task 7. For this task, write a small test
that confirms the env-gated branch exists:

Create or extend `cli/agent/conn_proxy_test.go`:

```go
package agent

import (
	"os"
	"testing"
)

// TestConnectAgentSelectsProxyWhenEnvSet: confirms that when
// HARNESS_PROXY_VIA_RUNNER is set, the dial routine picks the proxy path
// rather than direct peer.Dial. The test only verifies the dispatch
// decision; full ceremony is exercised by the integration test.
func TestConnectAgentSelectsProxyWhenEnvSet(t *testing.T) {
	// Save+restore env
	prev := os.Getenv("HARNESS_PROXY_VIA_RUNNER")
	defer os.Setenv("HARNESS_PROXY_VIA_RUNNER", prev)
	os.Setenv("HARNESS_PROXY_VIA_RUNNER", "ws:127.0.0.1:9999-*")

	// We can't fully invoke ConnectAgent without a running runner+server.
	// Instead, call the dispatch helper directly.
	useProxy, addr := shouldUseProxy()
	if !useProxy {
		t.Fatal("expected shouldUseProxy()=true when env set")
	}
	if addr != "ws:127.0.0.1:9999-*" {
		t.Errorf("addr: got %q", addr)
	}
}
```

- [ ] **Step 2: Run test, verify failure**

Run: `go test ./cli/agent/ -run TestConnectAgentSelectsProxy -v`
Expected: FAIL — `undefined: shouldUseProxy`.

- [ ] **Step 3: Modify ConnectAgent**

In `cli/agent/conn.go`, before the existing `peer.Dial(ctx, ep, cid, ...)` call:

```go
// shouldUseProxy reports whether the agent should go through a
// runner-mediated proxy (Phase B objproto negotiated proxy) instead of
// dialing the server directly. Decision is purely env-based:
//   HARNESS_PROXY_VIA_RUNNER=<cid-string> → proxy mode, addr = env value
//   unset / empty → direct mode
func shouldUseProxy() (bool, string) {
	v := strings.TrimSpace(os.Getenv("HARNESS_PROXY_VIA_RUNNER"))
	return v != "", v
}
```

Then in `ConnectAgent`, after parsing serverCID and before calling peer.Dial:

```go
var pc *peer.Conn
if useProxy, proxyAddr := shouldUseProxy(); useProxy {
	proxyCID, err := cliopts.ResolveServerCID(proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("HARNESS_PROXY_VIA_RUNNER: %w", err)
	}
	pc, err = cli.DialViaProxy(ctx, proxyCID, tid)
	if err != nil {
		return nil, fmt.Errorf("DialViaProxy: %w", err)
	}
} else {
	// existing direct-dial path:
	ep, err := cli.BuildClientEndpoint(cid)
	if err != nil { ... }
	pc, err = peer.Dial(ctx, ep, cid, peer.DialConfig{...})
	if err != nil { ... }
}
// continue with PSK + AgentBridgeHello as before, on pc.
```

(`tid` is the already-resolved task ID at this point in ConnectAgent. The existing
PSK + Hello flow operates on `pc` and is independent of how it was created.)

- [ ] **Step 4: Run unit test**

Run: `go test ./cli/agent/ -run TestConnectAgentSelectsProxy -v`
Expected: PASS.

- [ ] **Step 5: Verify existing agent tests still pass**

Run: `go test ./cli/agent/... -count=1`
Expected: all PASS — the direct-dial path is unchanged.

- [ ] **Step 6: Commit**

```bash
git add cli/agent/conn.go cli/agent/conn_proxy_test.go
git commit -m "feat(cli/agent): route ConnectAgent through DialViaProxy when env set"
```

---

## Task 6: Runner injects HARNESS_PROXY_VIA_RUNNER into agent env

**Files:**
- Modify: `runner/agentenv.go` — add field + emit env line
- Modify: `runner/session.go` — populate the field from listen-mode addr

- [ ] **Step 1: Write the failing test**

In `runner/agentenv_test.go`, add:

```go
func TestBuildAgentEnvIncludesProxyVia(t *testing.T) {
	spec := AgentEnvSpec{
		ServerCID:  mustParseCID(t, "ws:127.0.0.1:8539-1"),
		RunnerID:   mustParseCID(t, "ws:127.0.0.1:8540-2"),
		ProxyVia:   "ws:127.0.0.1:8540-*", // listen-mode runner addr
	}
	env := BuildAgentEnv(spec)
	want := "HARNESS_PROXY_VIA_RUNNER=ws:127.0.0.1:8540-*"
	found := false
	for _, e := range env {
		if e == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("env missing %q; got %v", want, env)
	}
}

func TestBuildAgentEnvOmitsProxyViaWhenEmpty(t *testing.T) {
	spec := AgentEnvSpec{
		ServerCID: mustParseCID(t, "ws:127.0.0.1:8539-1"),
		RunnerID:  mustParseCID(t, "ws:127.0.0.1:8540-2"),
		// ProxyVia intentionally empty
	}
	env := BuildAgentEnv(spec)
	for _, e := range env {
		if strings.HasPrefix(e, "HARNESS_PROXY_VIA_RUNNER=") {
			t.Errorf("env should not contain HARNESS_PROXY_VIA_RUNNER, got %q", e)
		}
	}
}
```

- [ ] **Step 2: Run test, verify failure**

Run: `go test ./runner/ -run TestBuildAgentEnvIncludesProxyVia -v`
Expected: undefined `ProxyVia`.

- [ ] **Step 3: Add field + emit logic**

In `runner/agentenv.go`:

```go
type AgentEnvSpec struct {
	ServerCID  objproto.ConnectionID
	RunnerID   objproto.ConnectionID
	TaskID     protocol.TaskID
	RepoPath   string
	Hostname   string
	WSPath     string
	AuthTicket [16]byte
	BinDir     string
	PSK        []byte

	// ProxyVia, when non-empty, is the runner's listen-side ConnectionID
	// string (e.g. "ws:127.0.0.1:8540-*"). Set in listen mode. Injected as
	// HARNESS_PROXY_VIA_RUNNER so agent processes use the objproto
	// negotiated-proxy path (Phase B) instead of dialing the server directly.
	ProxyVia string
}
```

And in `BuildAgentEnv`, after the existing PSK append:

```go
if s.ProxyVia != "" {
	env = append(env, "HARNESS_PROXY_VIA_RUNNER="+s.ProxyVia)
}
```

- [ ] **Step 4: Populate from listen config**

In `runner/session.go`, find where `AgentEnvSpec` is constructed (search for `BuildAgentEnv(`).
Pass `s.ProxyVia` from a new Session field. Add:

```go
// In Session struct:
// ProxyVia, when non-empty, is propagated into spawned agent env as
// HARNESS_PROXY_VIA_RUNNER. Set by ListenAndServe in listen mode.
ProxyVia string
```

And populate at construction in `driveAfterConn` (or in `ListenAndServe` before
calling driveAfterConn — depends on which has access to listen config). The
listen-mode addr lives in `ListenConfig.WSListen` (or `UDPListen`). Format as a CID
string with random ID: `fmt.Sprintf("%s:%s-*", transport, addr)`.

Actually — simplest place: ListenAndServe runs the accept loop and knows its own
listen config. Add it to ListenConfig:

```go
// runner/listen.go: in ListenAndServe, after determining the actual listen addr:
proxyVia := fmt.Sprintf("ws:%s-*", cfg.WSListen)  // or "udp:..." for UDP-only
cfg.Config.ProxyVia = proxyVia  // Config embeds ProxyVia, flows into Session
```

Then in `runner/connect.go`'s `driveAfterConn`, when building Session:

```go
session := &Session{
	...
	ProxyVia: cfg.ProxyVia,
	...
}
```

And `runner/session.go` `handleAssign` (where `BuildAgentEnv` is called) plumbs
`s.ProxyVia` into `AgentEnvSpec.ProxyVia`.

- [ ] **Step 5: Run tests**

Run: `go test ./runner/ -run TestBuildAgentEnv -v -count=1`
Expected: both PASS.

- [ ] **Step 6: Full runner test**

Run: `go test ./runner/... -count=1`
Expected: PASS (dial mode existing tests should still set ProxyVia=""; listen mode
tests get it set).

- [ ] **Step 7: Commit**

```bash
git add runner/agentenv.go runner/agentenv_test.go runner/session.go runner/connect.go runner/listen.go
git commit -m "feat(runner): inject HARNESS_PROXY_VIA_RUNNER into agent env in listen mode"
```

---

## Task 7: End-to-end integration test for Phase B

**Files:**
- Create: `integration/agent_proxy_e2e_test.go`

**Test scope:** Spin up server + runner (listen mode), trigger reverse-dial,
submit a task to the runner so it has an active task, then directly call
`cli.DialViaProxy` with the task ID and verify the returned conn can complete a PSK
+ AgentBridgeHello + a simple agent message round-trip with the server's agentboard.

The test exercises:
- Phase A reverse-dial registration (server → runner)
- Phase B proxy ceremony (agent → runner → server via SetProxy)
- End-to-end ECDH (agent ↔ server) survives the proxy
- AuthTicket validation succeeds end-to-end

- [ ] **Step 1: Write the test**

Create `integration/agent_proxy_e2e_test.go`:

```go
//go:build integration

package integration

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/agent"
	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/server"
)

func TestAgentProxyE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}

	serverAddr := "127.0.0.1:18560"
	runnerListen := "127.0.0.1:18561"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Start server (Mutual mode by Phase A).
	srv := server.New(server.Config{Addr: serverAddr})
	srvDone := make(chan error, 1)
	go func() { srvDone <- srv.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-srvDone:
		case <-time.After(2 * time.Second):
		}
	}()

	// 2. Start runner in --listen mode.
	listenDone := make(chan error, 1)
	go func() {
		listenDone <- runner.ListenAndServe(ctx, runner.ListenConfig{
			Config: runner.Config{
				AllowedRoots: []string{t.TempDir()},
				MaxTasks:     1,
				Hostname:     "proxy-e2e-runner",
			},
			WSListen: runnerListen,
		})
	}()

	time.Sleep(500 * time.Millisecond)

	// 3. Trigger reverse-dial.
	serverCID := mustParseCID(t, "ws:"+serverAddr+"-*")
	runnerCID := mustParseCID(t, "ws:"+runnerListen+"-*")
	dialResp, err := cli.ServerDialRunner(ctx, serverCID, runnerCID)
	if err != nil {
		t.Fatalf("ServerDialRunner: %v", err)
	}
	if dialResp.Status != protocol.DialRunnerStatus_Ok {
		t.Fatalf("dial-runner status: %v", dialResp.Status)
	}

	// 4. Submit a task to register a task_id on the runner.
	c, err := cli.Dial(ctx, serverCID)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.SayHello(ctx, protocol.ClientKind_Cli); err != nil {
		t.Fatal(err)
	}
	taskHex, err := c.SubmitWithSelectorAndArgs(ctx, t.TempDir(), "echo proxy-e2e",
		protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	var taskID protocol.TaskID
	raw, _ := hex.DecodeString(taskHex)
	copy(taskID.Id[:], raw)

	// Give the runner a moment to receive AssignTask and register the task.
	time.Sleep(500 * time.Millisecond)

	// 5. Call DialViaProxy and run a tiny round-trip via the agentboard.
	proxyConn, err := cli.DialViaProxy(ctx, runnerCID, taskID)
	if err != nil {
		t.Fatalf("DialViaProxy: %v", err)
	}
	defer proxyConn.Close()

	// 6. Validate end-to-end: send AgentBridgeHello (synthesises ConnectAgent's
	//    payload manually because env-driven dispatch is tested separately).
	//    Confirm the server responds with HelloStatus_Ok.

	hello := agentboard.AgentBridgeHello{
		// Fill RunnerId, TaskId, AuthTicket — get the ticket from the
		// agentboard for this taskID:
	}
	_ = hello
	// (Implementation: drive the existing agent Hello+Ack code path against
	// proxyConn. See cli/agent/conn.go ConnectAgent post-PSK section for the
	// exact AgentMessage shape; mirror that here.)

	t.Log("Phase B proxy round-trip success")
}

func mustParseCID(t *testing.T, s string) objproto.ConnectionID {
	t.Helper()
	cid, err := objproto.ParseConnectionID(s,
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse cid %q: %v", s, err)
	}
	return cid
}
```

(The Hello payload synthesis section is best filled in by reading
`cli/agent/conn.go:230-258` and reproducing the same SendMessage + waitForHelloResponse
pattern against proxyConn.)

- [ ] **Step 2: Run the E2E test**

Run: `go test -tags integration ./integration/ -run TestAgentProxyE2E -v -count=1`
Expected: PASS — DialViaProxy completes the proxy ceremony, the rehandshake derives
end-to-end keys with the server, and the AgentBridgeHello round-trip succeeds.

- [ ] **Step 3: Run full integration suite to confirm no regression**

Run: `go test -tags integration ./integration/ -count=1`
Expected: all PASS.

- [ ] **Step 4: Commit**

```bash
git add integration/agent_proxy_e2e_test.go
git commit -m "test(integration): end-to-end Phase B proxy ceremony with agentboard Hello"
```

---

## Final verification

- [ ] **Run full test suite**

Run: `go test ./... -count=1 && go test -tags integration ./integration/ -count=1`
Expected: all PASS.

- [ ] **Smoke run (manual binary verification)**

Start server, runner (listen mode), trigger reverse-dial, submit a task with
`fake-claude.sh` that runs `harness-cli agent send`:

```sh
# Terminal 1: server
./bin/harness-server --listen 127.0.0.1:18565 &

# Terminal 2: runner in listen mode
mkdir -p /tmp/phaseb-roots
./bin/agent-runner --listen 127.0.0.1:18566 --hostname proxy-smoke --roots /tmp/phaseb-roots --no-persist --claude-bin "$(pwd)/testdata/fake-claude.sh" &

# Terminal 3: reverse-dial + submit
./bin/harness-cli --server-cid ws:127.0.0.1:18565-* server dial-runner ws:127.0.0.1:18566-*
./bin/harness-cli --server-cid ws:127.0.0.1:18565-* submit --repo /tmp/phaseb-roots --task "say hello"
```

Expected: dial-runner returns `ok`; submit returns a task hex; runner spawns
fake-claude with `HARNESS_PROXY_VIA_RUNNER` set; fake-claude (or any embedded
harness-cli agent send call) succeeds via the proxy path; task transitions
through Queued → Running → Succeeded.

- [ ] **Cleanup**

```sh
kill %1 %2
rm -rf /tmp/phaseb-roots
```
