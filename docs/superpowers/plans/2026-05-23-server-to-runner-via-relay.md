# Server → runner via relay-runner — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Allow `harness-cli server dial-runner <target-cid> --via <proxy-cid>`
to register a target_runner that the server
cannot reach directly, by routing the registration handshake through an already
registered relay-runner using objproto's SetProxy + RehandshakeForProxy primitives.

**Architecture:** server sends `EstablishRelay{target, slot_id}` to proxy_runner
on the existing PSK-validated registered conn. proxy_runner dials target and
sets up a SetProxy entry so server's subsequent rehandshake at slot_id reaches
target. After end-to-end ECDH, the normal Phase A flow (DialGreeting → PSK →
RunnerHello → registry insert) runs server↔target with proxy_runner as opaque
relay.

**Tech Stack:** Go, brgen `.bgn` schema, objproto.SetProxy /
Connection.RehandshakeForProxy, peer.WrapAcceptedConn (Phase A).

**Spec:** `docs/superpowers/specs/2026-05-23-server-to-runner-via-relay-design.md`.
**Prereq:** Phase A + Phase B merged on main (commit `cac15e9` and earlier).

---

## File Structure

| File | Action | Responsibility |
|---|---|---|
| `integration/relay_poc_test.go` | Create (Task 0) | Standalone POC: 3 in-process endpoints verifying SetProxy + rehandshake + DialGreeting forwarding work as a unit |
| `trsf/wire/stream.bgn` / `.go` | No change | All Phase B wire kinds already exist |
| `runner/protocol/message.bgn` | Modify (Task 1) | Add `EstablishRelayRequest` / `EstablishRelayStatus` / `EstablishRelayResponse`; `RunnerRequestType.establish_relay`; `DialRunnerRequest.via :RunnerID`; `DialRunnerStatus_{ViaNotFound,ViaRelayFailed}` |
| `runner/protocol/message.go` | Regenerated | Output of protoregen |
| `runner/protocol/relay_test.go` | Create (Task 1) | Round-trip tests for the new wire types |
| `runner/relay_handler.go` | Create (Task 2) | Runner side: receive EstablishRelay → expect a slot_id dial → dial target → SetProxy → DialGreeting → ack |
| `runner/relay_handler_test.go` | Create (Task 2) | Unit tests for slot collision / target-dial-fail / happy path validation |
| `runner/session.go` | Modify (Task 2) | Add `Endpoint` accessor (or similar) so relay_handler can call SetProxy |
| `runner/dispatch.go` (or wherever RunnerRequest is dispatched) | Modify (Task 2) | Add `case RunnerRequestType_EstablishRelay` |
| `runner/listen.go` | Modify (Task 2) | `handleAcceptedConn` recognises "expected relay" slot_id and short-circuits to the relay setup flow instead of the Phase A driveAfterConn / Phase B proxy ceremony |
| `server/dial_runner_handler.go` | Modify (Task 3) | If `DialRunnerRequest.via.Kind != Any`: resolve selector → registry entry → send EstablishRelay → wait response → SendHandshake → RehandshakeForProxy → DialGreeting → handleConnection |
| `server/server.go` | Modify (Task 3) | Wire any new accessors needed (e.g. registry resolve helper) |
| `cli/server_dial_runner.go` | Modify (Task 4) | `ServerDialRunner` signature extension to accept via selector |
| `cmd/harness-cli/main.go` | Modify (Task 4) | `--via <proxy-cid>` flag on `server dial-runner` |
| `tui/cmdline.go` | Modify (Task 4) | Extend `parseServer` to accept `--via`; extend `ServerDialRunnerAction` with `Via string` |
| `tui/app.go` | Modify (Task 4) | Plumb via into DoServerDialRunner |
| `tui/server_dial.go` | Modify (Task 4) | DoServerDialRunner takes optional via CID string |
| `cmd/harness-webui-wasm/main.go` | Modify (Task 4) | `harnessServerDialRunner` accepts optional `via` CID as 2nd arg |
| `webui/static/main.js` | Modify (Task 4) | Parse `--via=<cid>` from cmd-input |
| `integration/relay_e2e_test.go` | Create (Task 5) | End-to-end: server + proxy_runner (registered) + target_runner (listen mode) → dial-runner via proxy succeeds, target appears in registry |

---

## Task 0: Proof-of-concept for relay protocol mechanics

**Files:**
- Create: `integration/relay_poc_test.go`

**Why this exists:** The spec ceremony is non-trivial (initial ECDH server↔proxy at slot_id, SetProxy, rehandshake to derive server↔target keys, plus DialGreeting at the right moment). Before writing the production handler we want a small standalone test that proves the wire-level mechanics work, mirroring the ksdk-era `TestWebSocketNegotiatedProxy` pattern but with our flavor (DialGreeting + listen-mode-style handshake).

The POC test does NOT use any `runner.relay_handler` or `server.DialRunnerHandler` code. It builds 3 raw objproto Endpoints + manually drives:

1. server endpoint dials proxy endpoint at slot_id (initial ECDH; mode=Mutual both sides)
2. proxy endpoint dials target endpoint at slot_id (initial ECDH)
3. proxy.SetProxy(owned=(transport, server.SrcAddr, slot_id), allocate=(transport, target.Addr, slot_id))
4. proxy sends DialGreeting{Version:1} to target via proxy↔target activeConn
5. proxy closes the server↔proxy activeConn (peerConn.Close)
6. server calls RehandshakeForProxy on its server↔proxy peer.Conn
7. Wait for rh.C → new conn at server, end-to-end with target
8. Wait for newActiveSession on target → new activeConn from server's keys
9. Verify: send a small AgentMessage from server, receive at target, AEAD validates → end-to-end keys are server's not proxy's

If step 8 / 9 work, the protocol mechanics are sound and we can build the production handler with confidence.

**Critical questions the POC needs to answer:**
- Does target's listen handler (handleAcceptedConn-style logic, if we run it on target) see DialGreeting from step 4 as the first inbound? The POC can skip the listen handler and verify the rehandshake replaces target's activeConn cleanly.
- Does target's activeConn at slot_id get replaced by the rehandshake (step 6→8)? Or does the old conn coexist? `addActiveConnection` semantics need to be understood.

- [ ] **Step 1: Write the POC**

Create `integration/relay_poc_test.go`. Sketch:

```go
//go:build integration

package integration

import (
	"context"
	"crypto/ecdh"
	"log/slog"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/transport"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

func TestRelayPOC(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E POC")
	}

	const (
		proxyAddr  = "127.0.0.1:18620"
		targetAddr = "127.0.0.1:18621"
		slotID     = uint16(0x1234)
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// --- proxy endpoint (Mutual, listens on proxyAddr) ---
	proxyMux := http.NewServeMux()
	proxyEP, err := transport.WebSocketEndpoint(proxyMux, transport.WebSocketConfig{
		Logger: slog.Default(),
		Path:   "/ws",
		Mode:   objproto.EndpointModeMutual,
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = (&http.Server{Addr: proxyAddr, Handler: proxyMux}).ListenAndServe() }()
	go objproto.AutoGarbageCollect(proxyEP, 10*time.Second, 30*time.Second, 1*time.Minute, 5*time.Minute)

	// --- target endpoint (Mutual, listens on targetAddr) ---
	targetMux := http.NewServeMux()
	targetEP, err := transport.WebSocketEndpoint(targetMux, transport.WebSocketConfig{
		Logger: slog.Default(),
		Path:   "/ws",
		Mode:   objproto.EndpointModeMutual,
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = (&http.Server{Addr: targetAddr, Handler: targetMux}).ListenAndServe() }()
	go objproto.AutoGarbageCollect(targetEP, 10*time.Second, 30*time.Second, 1*time.Minute, 5*time.Minute)

	// --- server endpoint (Client mode is enough — only dials out) ---
	serverEP, err := transport.WebSocketEndpoint(nil, transport.WebSocketConfig{
		Logger: slog.Default(),
		Mode:   objproto.EndpointModeClient,
	})
	if err != nil {
		t.Fatal(err)
	}
	go objproto.AutoGarbageCollect(serverEP, 10*time.Second, 30*time.Second, 1*time.Minute, 5*time.Minute)

	time.Sleep(300 * time.Millisecond) // listeners bind

	// === Step 1: server dials proxy at slot_id ===
	proxyCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort(proxyAddr), slotID)
	priv1, hs1, err := objproto.NewECDHHandshake(ecdh.P521(), objproto.AES128GCM)
	if err != nil {
		t.Fatal(err)
	}
	ch1, err := serverEP.SendHandshake(proxyCID, priv1, hs1)
	if err != nil {
		t.Fatalf("server.SendHandshake: %v", err)
	}
	serverProxyConn := <-ch1.C
	proxyServerConn := <-proxyEP.GetNewActiveConnectionChannel()
	t.Logf("server↔proxy ECDH done: server cid=%v proxy cid=%v",
		serverProxyConn.ConnectionID(), proxyServerConn.ConnectionID())

	// === Step 2: proxy dials target at slot_id ===
	targetCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort(targetAddr), slotID)
	priv2, hs2, err := objproto.NewECDHHandshake(ecdh.P521(), objproto.AES128GCM)
	if err != nil {
		t.Fatal(err)
	}
	ch2, err := proxyEP.SendHandshake(targetCID, priv2, hs2)
	if err != nil {
		t.Fatalf("proxy.SendHandshake target: %v", err)
	}
	proxyTargetConn := <-ch2.C
	targetProxyConn := <-targetEP.GetNewActiveConnectionChannel()
	t.Logf("proxy↔target ECDH done: proxy cid=%v target cid=%v",
		proxyTargetConn.ConnectionID(), targetProxyConn.ConnectionID())

	// === Step 3: proxy.SetProxy ===
	// owned = proxy's view of server conn; allocate = proxy's view of target conn
	if err := proxyEP.SetProxy(proxyServerConn.ConnectionID(), proxyTargetConn.ConnectionID()); err != nil {
		t.Fatalf("SetProxy: %v", err)
	}

	// === Step 4: proxy sends DialGreeting to target ===
	greeting := protocol.DialGreeting{Version: 1}
	greetingPayload := greeting.MustAppend([]byte{byte(wire.ApplicationPayloadKind_DialGreeting)})
	if _, _, err := proxyTargetConn.SendMessage(greetingPayload); err != nil {
		t.Fatalf("proxy send DialGreeting: %v", err)
	}

	// === Step 5: proxy closes peerConn (the server-side activeConn) ===
	proxyServerConn.Close()

	// === Step 6: server rehandshakes ===
	priv3, hs3, err := objproto.NewECDHHandshake(ecdh.P521(), objproto.AES128GCM)
	if err != nil {
		t.Fatal(err)
	}
	rh, err := serverProxyConn.RehandshakeForProxy(priv3, hs3)
	if err != nil {
		t.Fatalf("RehandshakeForProxy: %v", err)
	}

	// === Step 7: receive new conn at server ===
	rhCtx, rhCancel := context.WithTimeout(ctx, 5*time.Second)
	defer rhCancel()
	var newServerConn objproto.Connection
	select {
	case <-rhCtx.Done():
		t.Fatalf("rehandshake timed out: %v", rhCtx.Err())
	case newServerConn = <-rh.C:
	}
	t.Logf("server end-to-end conn ready: cid=%v", newServerConn.ConnectionID())

	// === Step 8: receive new active conn at target ===
	newCtx, newCancel := context.WithTimeout(ctx, 5*time.Second)
	defer newCancel()
	newTargetConn, err := targetEP.WaitNewActiveConnection(5 * time.Second)
	if err != nil {
		t.Fatalf("target.WaitNewActiveConnection: %v", err)
	}
	_ = newCtx
	t.Logf("target end-to-end conn ready: cid=%v", newTargetConn.ConnectionID())

	// === Step 9: round-trip a small AgentMessage end-to-end ===
	// Use AgentMessage payload kind because it's the smallest existing app
	// payload we can stuff. Just want to confirm the AEAD validates with
	// server's new keys (not proxy's old keys).
	payload := []byte{byte(wire.ApplicationPayloadKind_AgentMessage), 0x01, 0x02}
	if _, _, err := newServerConn.SendMessage(payload); err != nil {
		t.Fatalf("server send via end-to-end: %v", err)
	}
	msg, err := newTargetConn.ReceiveMessage()
	if err != nil {
		t.Fatalf("target receive: %v", err)
	}
	if len(msg.Data) < 3 || msg.Data[0] != byte(wire.ApplicationPayloadKind_AgentMessage) {
		t.Fatalf("unexpected payload at target: %v", msg.Data)
	}
	t.Log("Relay POC: end-to-end conn server↔target through proxy confirmed")
}
```

(Note: the exact API names — `objproto.Connection.ReceiveMessage()`, `Endpoint.WaitNewActiveConnection`, `Connection.Close()` etc — should be verified against current code. Adjust if the actual API differs.)

- [ ] **Step 2: Run the POC**

```bash
cd /home/kforfk/workspace/remote-agent-harness
go test -tags integration ./integration/ -run TestRelayPOC -v -count=1
```

**Expected:** PASS. Test logs show successful round-trip server→target with end-to-end keys.

**If FAIL:** The protocol design needs revision. Common failure modes to investigate:
- "no sent probe for handshake ack" at server: server's sentHandshake key doesn't match the HandshakeAck source addr/connection_id. SetProxy's bidirectional behavior may not work as assumed.
- target's HandshakeAck never reaches server: proxy's reverse forwarding (allocate→owned) doesn't fire correctly.
- AEAD validation fails on payload at target: server and target derived different shared secrets (i.e., proxy's old key got mixed in somehow).

If FAIL with any of the above, STOP and report the failure mode. Do not proceed to subsequent tasks — the spec needs revision.

- [ ] **Step 3: Commit**

```bash
git add integration/relay_poc_test.go
git commit -m "test(integration): POC for server→proxy_runner→target_runner relay protocol"
```

---

## Task 1: Wire schema additions

**Files:**
- Modify: `runner/protocol/message.bgn`
- Regenerated: `runner/protocol/message.go`
- Create: `runner/protocol/relay_test.go`

- [ ] **Step 1: Write the failing tests**

Create `runner/protocol/relay_test.go`:

```go
package protocol

import (
	"testing"
)

func TestEstablishRelayRequestRoundTrip(t *testing.T) {
	var inner EstablishRelayRequest
	inner.Target.SetTransport([]byte("ws"))
	inner.Target.SetIpAddr([]byte{10, 0, 0, 5})
	inner.Target.Port = 8540
	inner.Target.UniqueNumber = 0xABCD
	inner.SlotId = 0x1234

	var req RunnerRequest
	req.Kind = RunnerRequestType_EstablishRelay
	req.SetEstablishRelay(inner)

	buf, err := req.Append(nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	var got RunnerRequest
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Kind != RunnerRequestType_EstablishRelay {
		t.Errorf("kind: got %v want EstablishRelay", got.Kind)
	}
	er := got.EstablishRelay()
	if er == nil {
		t.Fatal("EstablishRelay variant nil after decode")
	}
	if er.SlotId != 0x1234 {
		t.Errorf("slot_id: got %x", er.SlotId)
	}
	if string(er.Target.Transport) != "ws" {
		t.Errorf("transport: got %q", er.Target.Transport)
	}
}

func TestEstablishRelayResponseRoundTrip(t *testing.T) {
	resp := EstablishRelayResponse{Status: EstablishRelayStatus_SlotCollision}
	buf, err := resp.Append(nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	var got EstablishRelayResponse
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Status != EstablishRelayStatus_SlotCollision {
		t.Errorf("status: got %v", got.Status)
	}
}

func TestDialRunnerRequestWithViaRoundTrip(t *testing.T) {
	var req DialRunnerRequest
	req.Target.SetTransport([]byte("ws"))
	req.Target.SetIpAddr([]byte{10, 0, 0, 9})
	req.Target.Port = 8540
	req.Via.SetTransport([]byte("ws"))
	req.Via.SetIpAddr([]byte{192, 168, 3, 14})
	req.Via.Port = 52036
	req.Via.UniqueNumber = 51357

	buf, err := req.Append(nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	var got DialRunnerRequest
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if string(got.Via.Transport) != "ws" {
		t.Errorf("via.transport: got %q", got.Via.Transport)
	}
	if got.Via.Port != 52036 || got.Via.UniqueNumber != 51357 {
		t.Errorf("via fields: got port=%d uniq=%d", got.Via.Port, got.Via.UniqueNumber)
	}
}

func TestDialRunnerRequestViaEmptyRoundTrip(t *testing.T) {
	// transport_len == 0 → "via 未指定" のマーカー
	var req DialRunnerRequest
	req.Target.SetTransport([]byte("ws"))
	req.Target.SetIpAddr([]byte{10, 0, 0, 9})
	req.Target.Port = 8540
	// Via 全フィールド zero (transport_len=0 含む)

	buf, err := req.Append(nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	var got DialRunnerRequest
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(got.Via.Transport) != 0 {
		t.Errorf("via.transport should be empty, got %q", got.Via.Transport)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./runner/protocol/ -run "TestEstablishRelay|TestDialRunnerRequestWithVia" -v
```

Expected: FAIL with undefined symbols.

- [ ] **Step 3: Update `runner/protocol/message.bgn`**

Find the `DialRunnerRequest` definition (around line 184 if Phase A's spec landed in order) and extend with `via`:

```bgn
# admin → server: dial out to this runner endpoint and complete registration.
# When via.transport_len == 0, server does direct DoECDHHandshake (Phase A flow).
# When via.transport_len != 0, server looks up via in its registry (exact match
# by CID), and instead of dialing target directly, sends EstablishRelay to the
# resolved runner over its existing registered conn. See spec
# docs/superpowers/specs/2026-05-23-server-to-runner-via-relay-design.md.
format DialRunnerRequest:
    target :RunnerID
    via    :RunnerID
```

Find `DialRunnerStatus` and add two new values:

```bgn
enum DialRunnerStatus:
    :u8
    ok                = "ok"
    dial_failed       = "dial_failed"
    psk_failed        = "psk_failed"
    hello_timeout     = "hello_timeout"
    invalid_target    = "invalid_target"
    via_not_found     = "via_not_found"      # via selector matched no registered runner
    via_relay_failed  = "via_relay_failed"   # proxy_runner returned non-Ok EstablishRelay status
```

Find the existing `RunnerRequestType` enum (~line 11) and append:

```bgn
enum RunnerRequestType:
    :u8
    assign_task
    cancel_task
    open_exec
    runner_hello_response
    task_wake
    open_file_transfer
    list_files
    establish_relay         # ← add at end
```

Add the new format declarations near the other RunnerRequest variant formats:

```bgn
# server → proxy_runner: please dial target at slot_id and SetProxy so that
# my subsequent rehandshake at the same slot_id reaches target.
format EstablishRelayRequest:
    target  :RunnerID
    slot_id :u16

enum EstablishRelayStatus:
    :u8
    ok                       = "ok"
    target_dial_failed       = "target_dial_failed"
    slot_collision           = "slot_collision"
    set_proxy_failed         = "set_proxy_failed"
    invalid_target           = "invalid_target"

format EstablishRelayResponse:
    status :EstablishRelayStatus
```

Add the variant to `RunnerRequest` match block:

```bgn
format RunnerRequest:
    kind :RunnerRequestType
    match kind:
        RunnerRequestType.assign_task          => assign_task          :AssignTask
        RunnerRequestType.cancel_task          => cancel_task          :CancelTask
        RunnerRequestType.open_exec            => open_exec            :OpenExecRunnerRequest
        RunnerRequestType.runner_hello_response => runner_hello_response :RunnerHelloResponse
        RunnerRequestType.task_wake            => task_wake            :TaskWakeRequest
        RunnerRequestType.open_file_transfer   => open_file_transfer   :RunnerOpenFileTransferRequest
        RunnerRequestType.list_files           => list_files           :RunnerListFilesRequest
        RunnerRequestType.establish_relay      => establish_relay      :EstablishRelayRequest
```

For the `EstablishRelayResponse` — the runner→server response channel is the
existing `RunnerMessage` family (look at how `RunnerHelloResponse` is currently
wired — it goes through `RunnerRequestType.runner_hello_response`). Add a
similar entry: extend `RunnerMessageType` if needed, OR (simpler) bounce it as
a top-level `EstablishRelayResponse` payload tagged with a new
`ApplicationPayloadKind`. Read the existing pattern (`server/dispatch.go` how
`RunnerHelloResponse` flows back) and follow the same approach.

If the simplest path is a new `RunnerMessageType.establish_relay_response`,
add it:

```bgn
enum RunnerMessageType:
    :u8
    hello
    task_accepted
    task_started
    task_finished
    heartbeat
    establish_relay_response   # ← add

format RunnerMessage:
    kind :RunnerMessageType
    match kind:
        ...existing variants...
        RunnerMessageType.establish_relay_response => establish_relay_response :EstablishRelayResponse
```

- [ ] **Step 4: Regenerate Go code**

```bash
make protoregen
```

Expected: success; `runner/protocol/message.go` updates. Confirm via `git diff --stat` shows only the expected files.

- [ ] **Step 5: Run the tests to verify they pass**

```bash
go test ./runner/protocol/ -run "TestEstablishRelay|TestDialRunnerRequest" -v -count=1
```

Expected: 4 tests PASS (2 EstablishRelay + 2 DialRunnerRequest variants).

- [ ] **Step 6: Run full package test**

```bash
go test ./runner/protocol/ -count=1
```

Expected: all PASS (additive changes; Phase A's existing `DialRunnerRequest` tests should be backward-compatible since `via` defaults to `RunnerSelectorKind_Any`).

- [ ] **Step 7: Commit**

```bash
git add runner/protocol/message.bgn runner/protocol/message.go runner/protocol/relay_test.go
git commit -m "feat(protocol): add EstablishRelay + DialRunnerRequest.via for proxy-runner relay"
```

---

## Task 2: Runner-side relay handler

**Files:**
- Create: `runner/relay_handler.go`
- Create: `runner/relay_handler_test.go`
- Modify: `runner/dispatch.go` (or wherever RunnerRequest is dispatched in the runner)
- Modify: `runner/listen.go` (handleAcceptedConn discriminates expected-relay slot_id)
- Modify: `runner/session.go` (Endpoint accessor)

**Background:** When the server sends `EstablishRelay{target, slot_id}` over the
existing registered conn, the runner needs to:

1. Validate slot_id doesn't collide with its own server-conn ID (well, actually it shouldn't — slot_id is the server-chosen connection_id for the server↔proxy_runner relay conn; the existing registered conn has a different CID).
2. ECDH-dial target at slot_id (proxy_runner↔target conn).
3. SetProxy(owned=(server.Addr, slot_id), allocate=(target.Addr, slot_id)).
   - But "owned" doesn't exist as activeConn yet — that's why we pre-register expecting state.
4. Send DialGreeting to target.
5. Reply EstablishRelayResponse{Ok} on registered conn.
6. Wait for server to dial at slot_id → handleAcceptedConn picks it up → recognises slot_id is in "expected relay" set → instead of normal flow, sets up SetProxy retroactively (since owned now exists).

This ordering is tricky. **Important**: re-read Task 0 POC result before implementing — the POC dictates the exact sequence that works.

A reasonable design (subject to revision based on POC findings):

- Maintain `relayState map[u16]target_cid` on the session
- On EstablishRelayRequest, store slot_id → target in relayState (no work done yet)
- Reply EstablishRelayResponse{Ok} immediately
- When server dials at slot_id (handleAcceptedConn fires), check relayState[slot_id]:
  - If set: this is a relay setup. Don't run driveAfterConn / agent proxy. Instead:
    - ECDH-dial target_cid
    - SetProxy
    - Send DialGreeting to target
    - Close peerConn
  - If not set: normal Phase A / Phase B flow

This keeps the "owned must exist as activeConn first" invariant.

- [ ] **Step 1: Write the failing tests**

Create `runner/relay_handler_test.go` covering:

- `TestRelayHandlerValidateRequest`: slot_id collision with the runner's own server-conn ID → returns SlotCollision (this might not actually fire in practice, but validation should be there).
- `TestRelayHandlerInvalidTarget`: target with empty transport → InvalidTarget.
- (Happy path is covered by Task 5's E2E test.)

```go
package runner

import (
	"net/netip"
	"testing"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TestRelayHandlerSlotCollision: slot_id equal to the runner's existing
// server-conn ID is rejected with SlotCollision before any target dial.
func TestRelayHandlerSlotCollision(t *testing.T) {
	const sharedID = 0xABCD
	serverCID := objproto.NewConnectionID("ws",
		netip.MustParseAddrPort("127.0.0.1:8549"), sharedID)

	st := &relayHandlerState{
		serverCID: serverCID,
	}
	var req protocol.EstablishRelayRequest
	req.Target.SetTransport([]byte("ws"))
	req.Target.SetIpAddr([]byte{10, 0, 0, 5})
	req.Target.Port = 8540
	req.SlotId = sharedID

	resp := st.validate(req)
	if resp.Status != protocol.EstablishRelayStatus_SlotCollision {
		t.Errorf("status: got %v want SlotCollision", resp.Status)
	}
}

// TestRelayHandlerInvalidTarget: empty transport → InvalidTarget.
func TestRelayHandlerInvalidTarget(t *testing.T) {
	st := &relayHandlerState{
		serverCID: objproto.NewConnectionID("ws",
			netip.MustParseAddrPort("127.0.0.1:8549"), 0x1),
	}
	var req protocol.EstablishRelayRequest
	// target.Transport intentionally left empty
	req.SlotId = 0x42

	resp := st.validate(req)
	if resp.Status != protocol.EstablishRelayStatus_InvalidTarget {
		t.Errorf("status: got %v want InvalidTarget", resp.Status)
	}
}
```

- [ ] **Step 2: Run tests, verify they fail**

```bash
go test ./runner/ -run TestRelayHandler -v
```

Expected: FAIL — undefined `relayHandlerState`.

- [ ] **Step 3: Implement `runner/relay_handler.go`**

```go
package runner

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// relayHandlerState holds the inputs the relay setup needs, extracted as a
// struct so the validation logic is pure-function-testable.
type relayHandlerState struct {
	serverCID objproto.ConnectionID
}

// validate checks slot collision and target validity. Returns the response
// to send back to server. Status=Ok means the runner will proceed with the
// asynchronous setup (dial target, SetProxy) when the server's slot_id dial
// arrives.
func (s *relayHandlerState) validate(req protocol.EstablishRelayRequest) protocol.EstablishRelayResponse {
	if len(req.Target.Transport) == 0 {
		return protocol.EstablishRelayResponse{Status: protocol.EstablishRelayStatus_InvalidTarget}
	}
	if req.SlotId == s.serverCID.ID {
		return protocol.EstablishRelayResponse{Status: protocol.EstablishRelayStatus_SlotCollision}
	}
	return protocol.EstablishRelayResponse{Status: protocol.EstablishRelayStatus_Ok}
}

// expectedRelays tracks slot_ids the runner has agreed to relay for.
// Keyed by slot_id; value is the target CID to dial when server dials at slot_id.
// Thread-safe; written by the EstablishRelay dispatch goroutine, read by the
// listen accept loop.
type expectedRelays struct {
	mu sync.Mutex
	m  map[uint16]objproto.ConnectionID
}

func newExpectedRelays() *expectedRelays {
	return &expectedRelays{m: make(map[uint16]objproto.ConnectionID)}
}

func (e *expectedRelays) Put(slotID uint16, target objproto.ConnectionID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.m[slotID] = target
}

func (e *expectedRelays) Take(slotID uint16) (objproto.ConnectionID, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	target, ok := e.m[slotID]
	if ok {
		delete(e.m, slotID)
	}
	return target, ok
}

// handleEstablishRelay is invoked from the RunnerRequest dispatcher when a
// new EstablishRelay arrives over the registered conn. It validates, records
// expectation, and replies via the sender callback.
func handleEstablishRelay(
	ctx context.Context,
	logger *slog.Logger,
	expected *expectedRelays,
	st *relayHandlerState,
	req protocol.EstablishRelayRequest,
	sendResponse func(protocol.EstablishRelayResponse) error,
) {
	resp := st.validate(req)
	if resp.Status == protocol.EstablishRelayStatus_Ok {
		target := protocolRunnerIDToConnectionID(req.Target)
		expected.Put(req.SlotId, target)
		logger.Info("relay: expecting server dial",
			"slot_id", req.SlotId,
			"target", target.String())
	}
	if err := sendResponse(resp); err != nil {
		logger.Warn("relay: send response failed", "err", err)
	}
}

// protocolRunnerIDToConnectionID converts the schema RunnerID to objproto.ConnectionID.
// Mirror server/dial_runner_handler.go's runnerIDToConnectionID helper.
func protocolRunnerIDToConnectionID(r protocol.RunnerID) objproto.ConnectionID {
	// ... implementation parallels server/dial_runner_handler.go ...
}

// completeRelayForSlot runs when the listen accept loop sees a slot_id dial
// that matches expectedRelays. It dials target, SetProxy's, and sends DialGreeting.
// On any error, closes the server-side pc.
func completeRelayForSlot(
	ctx context.Context,
	logger *slog.Logger,
	ep objproto.Endpoint,
	target objproto.ConnectionID,
	serverProxyConn objproto.Connection, // proxy_runner's view of server's incoming conn
	slotID uint16,
) error {
	// 1. ECDH-dial target at slot_id
	targetCID := objproto.NewConnectionID(target.Transport, target.Addr, slotID)
	priv, hs, err := objproto.NewECDHHandshake(/* parameters per Phase B pattern */)
	if err != nil {
		return fmt.Errorf("NewECDHHandshake: %w", err)
	}
	ch, err := ep.SendHandshake(targetCID, priv, hs)
	if err != nil {
		return fmt.Errorf("SendHandshake target: %w", err)
	}
	var proxyTargetConn objproto.Connection
	select {
	case <-ctx.Done():
		return ctx.Err()
	case proxyTargetConn = <-ch.C:
	}

	// 2. SetProxy
	if err := ep.SetProxy(serverProxyConn.ConnectionID(), proxyTargetConn.ConnectionID()); err != nil {
		return fmt.Errorf("SetProxy: %w", err)
	}

	// 3. Send DialGreeting to target
	greeting := protocol.DialGreeting{Version: 1}
	payload := greeting.MustAppend([]byte{byte(wire.ApplicationPayloadKind_DialGreeting)})
	if _, _, err := proxyTargetConn.SendMessage(payload); err != nil {
		return fmt.Errorf("send DialGreeting: %w", err)
	}

	// 4. Close the proxy_runner↔server activeConn for slot_id so SetProxy
	//    fully owns the forwarding from now on.
	_ = serverProxyConn.Close()
	logger.Info("relay: completed", "slot_id", slotID, "target", target.String())
	return nil
}
```

(Note: API names like `objproto.NewECDHHandshake` parameter count etc — verify against current code.)

- [ ] **Step 4: Wire dispatch in `runner/dispatch.go` (or wherever)**

Find where `RunnerRequest{kind=...}` are dispatched (look for `case protocol.RunnerRequestType_AssignTask` in the runner side).

Add:

```go
case protocol.RunnerRequestType_EstablishRelay:
    er := req.EstablishRelay()
    if er == nil {
        return
    }
    handleEstablishRelay(ctx, logger, session.expectedRelays, &relayHandlerState{
        serverCID: session.ServerCID,
    }, *er, func(resp protocol.EstablishRelayResponse) error {
        var rm protocol.RunnerMessage
        rm.Kind = protocol.RunnerMessageType_EstablishRelayResponse
        rm.SetEstablishRelayResponse(resp)
        // Send via session's runner-message sender, parallel to how
        // RunnerHello is sent.
        return session.sender.Send(rm.MustAppend([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)}))
    })
```

- [ ] **Step 5: Add `expectedRelays` to Session and Endpoint accessor**

In `runner/session.go`:

```go
type Session struct {
    // ... existing fields ...

    // expectedRelays is set by EstablishRelay handler and consumed by the
    // listen-mode accept loop when a slot_id dial arrives.
    expectedRelays *expectedRelays

    // Endpoint is the objproto endpoint the runner is bound to; the relay
    // handler needs it to call SetProxy and SendHandshake.
    Endpoint objproto.Endpoint
}
```

Initialize `expectedRelays` in `driveAfterConn` (or wherever Session is constructed). Pass `Endpoint` from ListenAndServe (or Connect) into the Session at construction.

- [ ] **Step 6: Modify listen.go to recognise expected relay slot_id**

In `handleAcceptedConn`, before the OnControl-peek discrimination (which is for DialGreeting vs AgentProxyControl), check if the conn's CID matches an expected relay slot_id:

```go
func handleAcceptedConn(ctx context.Context, cfg Config, sessionRef *atomic.Pointer[Session], ep objproto.Endpoint, pc *peer.Conn) {
    if cfg.Logger == nil {
        cfg.Logger = slog.Default()
    }

    // Check expected relays BEFORE installing OnControl. The CID's
    // connection_id is the slot_id the server picked when dialing.
    slotID := pc.Connection().ConnectionID().ID
    if sess := sessionRef.Load(); sess != nil && sess.expectedRelays != nil {
        if target, ok := sess.expectedRelays.Take(slotID); ok {
            // This is a relay setup conn. Run the completion flow instead
            // of the normal accept dispatch.
            go func() {
                if err := completeRelayForSlot(ctx, cfg.Logger, ep, target, pc.Connection(), slotID); err != nil {
                    cfg.Logger.Error("relay completion failed", "slot_id", slotID, "err", err)
                    pc.Close()
                }
                // completeRelayForSlot closes pc.Connection internally via
                // serverProxyConn.Close().
            }()
            return
        }
    }

    // ... existing Phase A/B discrimination follows ...
}
```

- [ ] **Step 7: Run tests**

```bash
go build ./...
go test ./runner/ -run TestRelayHandler -v -count=1
go test ./runner/... -count=1
```

Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add runner/relay_handler.go runner/relay_handler_test.go \
        runner/dispatch.go runner/listen.go runner/session.go runner/connect.go
git commit -m "feat(runner): relay handler for server-via-relay (Phase A extension)"
```

---

## Task 3: Server-side dial-runner via dispatch

**Files:**
- Modify: `server/dial_runner_handler.go`
- Modify: `server/server.go` (helper accessors as needed)

- [ ] **Step 1: Write the failing test**

Add to `server/dial_runner_handler_test.go`:

```go
// TestDialRunnerViaNotFound: when via CID doesn't match any registered runner,
// status=ViaNotFound is returned.
func TestDialRunnerViaNotFound(t *testing.T) {
    h := &DialRunnerHandler{
        Logger:   slog.Default(),
        Endpoint: nil, // not reached
        ResolveVia: func(_ objproto.ConnectionID) (*RunnerEntry, bool) {
            return nil, false
        },
    }
    var target protocol.RunnerID
    target.SetTransport([]byte("ws"))
    target.SetIpAddr([]byte{10, 0, 0, 5})
    target.Port = 8540

    var via protocol.RunnerID
    via.SetTransport([]byte("ws"))
    via.SetIpAddr([]byte{1, 2, 3, 4})
    via.Port = 9999
    via.UniqueNumber = 12345

    resp := h.HandleWithVia(context.Background(), target, via)
    if resp.Status != protocol.DialRunnerStatus_ViaNotFound {
        t.Errorf("status: got %v want ViaNotFound", resp.Status)
    }
}
```

- [ ] **Step 2: Run, verify fail**

```bash
go test ./server/ -run TestDialRunnerViaNotFound -v
```

Expected: FAIL — undefined ResolveVia / HandleWithVia.

- [ ] **Step 3: Extend `DialRunnerHandler` with via support**

In `server/dial_runner_handler.go`:

```go
type DialRunnerHandler struct {
    Logger      *slog.Logger
    Endpoint    objproto.Endpoint
    DialTimeout time.Duration
    OnDialed    func(ctx context.Context, conn objproto.Connection)

    // ResolveVia, when non-nil, is called when DialRunnerRequest.via has a
    // non-empty transport (i.e. relay path requested) to look up the
    // registered runner that should act as the relay. Lookup is exact-match
    // by ConnectionID against the registry.
    ResolveVia func(cid objproto.ConnectionID) (*RunnerEntry, bool)

    // ViaSendEstablishRelay, when non-nil, sends an EstablishRelayRequest
    // over the given RunnerEntry's conn and blocks until the response
    // arrives or the context cancels.
    ViaSendEstablishRelay func(ctx context.Context, entry *RunnerEntry, req protocol.EstablishRelayRequest) (protocol.EstablishRelayResponse, error)
}

// HandleWithVia is the via-aware path. Mirrors Handle but uses the via CID
// to look up an existing registered runner and route through EstablishRelay +
// RehandshakeForProxy.
func (h *DialRunnerHandler) HandleWithVia(ctx context.Context, target, via protocol.RunnerID) protocol.DialRunnerResponse {
    if len(via.Transport) == 0 {
        return h.Handle(ctx, target) // direct dial
    }
    if h.ResolveVia == nil || h.ViaSendEstablishRelay == nil {
        return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_InvalidTarget} // unconfigured
    }
    viaCID, err := runnerIDToConnectionID(via)
    if err != nil {
        return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_InvalidTarget}
    }
    entry, ok := h.ResolveVia(viaCID)
    if !ok {
        return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_ViaNotFound}
    }

    slotID := uint16(rand.Uint32() & 0xFFFF) // #nosec G404

    relayReq := protocol.EstablishRelayRequest{
        Target: target,
        SlotId: slotID,
    }
    relayResp, err := h.ViaSendEstablishRelay(ctx, entry, relayReq)
    if err != nil || relayResp.Status != protocol.EstablishRelayStatus_Ok {
        if h.Logger != nil {
            h.Logger.Warn("via-relay: EstablishRelay failed", "err", err, "relay_status", relayResp.Status)
        }
        return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_ViaRelayFailed}
    }

    // Now SendHandshake to (proxy_runner.Addr, slotID) — using the entry's
    // conn's addr (this is what transport.connMap is keyed by).
    proxyConnCID := entry.Conn.ConnectionID()
    slotCID := objproto.NewConnectionID(proxyConnCID.Transport, proxyConnCID.Addr, slotID)

    timeout := h.DialTimeout
    if timeout == 0 {
        timeout = 10 * time.Second
    }
    dialCtx, cancel := context.WithTimeout(ctx, timeout)
    defer cancel()

    priv, hs, err := objproto.NewECDHHandshake(ecdh.P521(), objproto.AES128GCM)
    if err != nil {
        return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_InvalidTarget}
    }
    ch, err := h.Endpoint.SendHandshake(slotCID, priv, hs)
    if err != nil {
        if h.Logger != nil {
            h.Logger.Warn("via-relay: SendHandshake failed", "err", err)
        }
        return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_DialFailed}
    }

    var conn objproto.Connection
    select {
    case <-dialCtx.Done():
        return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_DialFailed}
    case conn = <-ch.C:
    }

    // At this point server has activeConn at (proxy.Addr, slotID).
    // The runner side has SetProxy in effect (set by completeRelayForSlot
    // after receiving our handshake).
    // Now RehandshakeForProxy to derive server↔target keys.
    priv2, hs2, err := objproto.NewECDHHandshake(ecdh.P521(), objproto.AES128GCM)
    if err != nil {
        return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_InvalidTarget}
    }
    rh, err := conn.RehandshakeForProxy(priv2, hs2)
    if err != nil {
        return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_DialFailed}
    }
    var endToEndConn objproto.Connection
    select {
    case <-dialCtx.Done():
        return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_DialFailed}
    case endToEndConn = <-rh.C:
    }

    // Send DialGreeting to target (server's identity → marks first message kind = DialGreeting)
    greeting := protocol.DialGreeting{Version: 1}
    payload := greeting.MustAppend([]byte{byte(wire.ApplicationPayloadKind_DialGreeting)})
    if _, _, err := endToEndConn.SendMessage(payload); err != nil {
        if h.Logger != nil {
            h.Logger.Warn("via-relay: send DialGreeting failed", "err", err)
        }
        _ = endToEndConn.Close()
        return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_DialFailed}
    }

    // Hand off to handleConnection just like the direct-dial path.
    if h.OnDialed != nil {
        h.OnDialed(ctx, endToEndConn)
    }
    return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_Ok}
}
```

- [ ] **Step 4: Wire ResolveVia + ViaSendEstablishRelay in server.go**

In `server/server.go` where `s.taskHandler.Endpoint = ep` is set today (after `buildEndpoint`), also wire:

```go
s.taskHandler.dialRunnerHandler.ResolveVia = s.registry.GetByConnectionID
s.taskHandler.dialRunnerHandler.ViaSendEstablishRelay = s.sendEstablishRelayRequest
```

Add `Registry.GetByConnectionID(cid objproto.ConnectionID) (*RunnerEntry, bool)` (lookup the registered entry whose peer.Conn ConnectionID exactly matches).

(Field naming and indirection adjust to your actual code; if `DialRunnerHandler`
is constructed inline in task_handler.go's case dispatch today, refactor to
make it a field on Server so we can wire these once.)

Add `s.sendEstablishRelayRequest(ctx, entry, req)`:

- Encode the EstablishRelayRequest as a `RunnerRequest` payload.
- Send via `entry.Conn.SendMessage`.
- Wait for a matching `RunnerMessage{establish_relay_response}` reply via a per-entry response channel.
- Timeout 10s.

For the reply, the runner→server dispatcher needs to recognise `establish_relay_response` and route to the right channel. Look at how `RunnerHelloResponse` is handled today (it's an exact parallel — sent over RunnerControl, decoded by server's runner_handler, then matched to whatever sent the Hello).

- [ ] **Step 5: Run tests**

```bash
go build ./...
go test ./server/ -run TestDialRunner -v -count=1
go test ./server/... -count=1
```

Expected: PASS (existing direct-dial tests stay green; new via test passes).

- [ ] **Step 6: Commit**

```bash
git add server/dial_runner_handler.go server/dial_runner_handler_test.go server/server.go server/registry.go server/runner_handler.go
git commit -m "feat(server): dial-runner --via via EstablishRelay + RehandshakeForProxy"
```

---

## Task 4: CLI / TUI / WebUI surface

**Files:**
- Modify: `cli/server_dial_runner.go` (extend helper to accept via selector)
- Modify: `cmd/harness-cli/main.go` (`--via-host` / `--via-runner` / `--via-ip` flags)
- Modify: `tui/cmdline.go` (parse via flags), `tui/server_dial.go` (DoServerDialRunner accepts via)
- Modify: `tui/app.go` (pass via through)
- Modify: `cmd/harness-webui-wasm/main.go` (`harnessServerDialRunner` accepts selector object)
- Modify: `webui/static/main.js` (parse `--via-host=` etc from cmd-input)

- [ ] **Step 1: Extend `cli.ServerDialRunner`**

In `cli/server_dial_runner.go`:

```go
// ServerDialRunner is the high-level helper invoked by
// `harness-cli server dial-runner <cid>`. via is optional: when its
// Transport is empty, behaves like Phase A direct dial.
func ServerDialRunner(ctx context.Context, serverCID, targetCID objproto.ConnectionID, viaCID objproto.ConnectionID) (protocol.DialRunnerResponse, error) {
    client, err := Dial(ctx, serverCID)
    if err != nil {
        return protocol.DialRunnerResponse{}, fmt.Errorf("dial server: %w", err)
    }
    defer client.Close()
    return ServerDialRunnerWith(ctx, client, protocol.ConnIDToRunnerID(targetCID), protocol.ConnIDToRunnerID(viaCID))
}

func ServerDialRunnerWith(ctx context.Context, c taskControlClient, target, via protocol.RunnerID) (protocol.DialRunnerResponse, error) {
    req := &protocol.TaskControlRequest{
        Kind:      protocol.TaskControlKind_DialRunner,
        RequestId: nextRequestID(),
    }
    req.SetDialRunner(protocol.DialRunnerRequest{Target: target, Via: via})
    // ... rest unchanged ...
}
```

Existing callers can pass zero `objproto.ConnectionID` for `viaCID` (direct
dial, backward compat). `ConnIDToRunnerID` of a zero ConnectionID produces a
RunnerID with empty Transport — the marker for "no via".

- [ ] **Step 2: Add flags to harness-cli**

In `cmd/harness-cli/main.go` `case "server"` block:

```go
case "dial-runner":
    fs := flag.NewFlagSet("server dial-runner", flag.ExitOnError)
    viaCIDStr := fs.String("via", "", "relay through this registered runner CID (copy from `ls` output)")
    fs.Parse(rest)
    if fs.NArg() != 1 {
        fmt.Fprintln(os.Stderr, "usage: harness-cli server dial-runner [--via <runner-cid>] <runner-cid>")
        os.Exit(2)
    }
    targetCID, err := objproto.ParseConnectionID(fs.Arg(0),
        objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
    if err != nil {
        die(fmt.Errorf("parse runner-cid: %w", err))
    }
    var viaCID objproto.ConnectionID
    if strings.TrimSpace(*viaCIDStr) != "" {
        viaCID, err = objproto.ParseConnectionID(*viaCIDStr,
            objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
        if err != nil {
            die(fmt.Errorf("parse --via: %w", err))
        }
    }
    resp, err := cli.ServerDialRunner(ctx, parseCID(), targetCID, viaCID)
    // ... rest unchanged ...
```

- [ ] **Step 3: Extend TUI cmdline**

In `tui/cmdline.go`:

- `ServerDialRunnerAction` gets `Via string` field (the CID string, or "" for direct dial)
- `parseServer` reads `--via <cid>` flag from args

- [ ] **Step 4: Extend `DoServerDialRunner`**

In `tui/server_dial.go`:

```go
func DoServerDialRunner(serverCID objproto.ConnectionID, runnerCIDStr, viaCIDStr string) tea.Cmd {
    return func() tea.Msg {
        ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
        defer cancel()
        targetCID, err := objproto.ParseConnectionID(runnerCIDStr,
            objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
        if err != nil {
            return ServerDialResultMsg{RunnerCID: runnerCIDStr, Err: err}
        }
        var viaCID objproto.ConnectionID
        if strings.TrimSpace(viaCIDStr) != "" {
            viaCID, err = objproto.ParseConnectionID(viaCIDStr,
                objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
            if err != nil {
                return ServerDialResultMsg{RunnerCID: runnerCIDStr, Err: fmt.Errorf("--via: %w", err)}
            }
        }
        resp, err := cli.ServerDialRunner(ctx, serverCID, targetCID, viaCID)
        if err != nil {
            return ServerDialResultMsg{RunnerCID: runnerCIDStr, Err: err}
        }
        return ServerDialResultMsg{RunnerCID: runnerCIDStr, Status: resp.Status}
    }
}
```

- [ ] **Step 5: Extend WebUI WASM bridge**

In `cmd/harness-webui-wasm/main.go` `harnessServerDialRunner`:

```go
func harnessServerDialRunner(this js.Value, args []js.Value) any {
    // args[0] = target runner CID string
    // args[1] = optional via runner CID string (or undefined/empty for direct dial)
    // ...
    var viaCID objproto.ConnectionID
    if len(args) >= 2 && args[1].Type() == js.TypeString {
        if s := strings.TrimSpace(args[1].String()); s != "" {
            viaCID, err = objproto.ParseConnectionID(s, objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
            if err != nil { rejectErr(...); return nil }
        }
    }
    resp, err := cli.ServerDialRunner(rootCtx, peerCID, targetCID, viaCID)
    // ...
}
```

- [ ] **Step 6: Extend WebUI cmd-input**

In `webui/static/main.js`:

```js
case "server": {
    if (tokens[1] !== "dial-runner") { throw new Error("server: unknown subcommand"); }
    let via = null, target = null;
    for (let i = 2; i < tokens.length; i++) {
        const t = tokens[i];
        if (t === "--via") {
            // next token
            i++;
            if (i >= tokens.length) throw new Error("--via: missing CID");
            via = tokens[i];
        } else if (t.startsWith("--via=")) {
            via = t.slice("--via=".length);
        } else if (!target) {
            target = t;
        } else {
            throw new Error(`unexpected arg: ${t}`);
        }
    }
    if (!target) throw new Error("server dial-runner: missing runner CID");
    const status = await window.harness.serverDialRunner(target, via || undefined);
    out = `server dial-runner ${target}${via ? ` --via=${via}` : ''}: ${status}`;
    break;
}
```

- [ ] **Step 7: Run tests + build**

```bash
go build ./...
make webui-build
go test ./... -count=1
```

Expected: all PASS.

- [ ] **Step 8: Commit**

```bash
git add cli/server_dial_runner.go cli/server_dial_runner_test.go cmd/harness-cli/main.go tui/cmdline.go tui/cmdline_test.go tui/server_dial.go tui/app.go cmd/harness-webui-wasm/main.go webui/static/main.js webui/static/main.wasm webui/index.html
git commit -m "feat(surface): --via-host/--via-runner/--via-ip on server dial-runner (CLI + TUI + WebUI)"
```

(Skip main.wasm from git if it's in .gitignore — check first.)

---

## Task 5: End-to-end integration test

**Files:**
- Create: `integration/relay_e2e_test.go`

```go
//go:build integration

package integration

import (
    "context"
    "testing"
    "time"

    "github.com/on-keyday/agent-harness/cli"
    "github.com/on-keyday/agent-harness/objproto"
    "github.com/on-keyday/agent-harness/runner"
    "github.com/on-keyday/agent-harness/runner/protocol"
    "github.com/on-keyday/agent-harness/server"
)

func TestRelayE2E(t *testing.T) {
    if testing.Short() {
        t.Skip("E2E test skipped in -short mode")
    }

    const (
        serverAddr   = "127.0.0.1:18610"
        proxyListen  = "127.0.0.1:18611" // proxy_runner registers via Phase A reverse-dial
        targetListen = "127.0.0.1:18612" // target_runner accepts via relay
    )

    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    // 1. Start server.
    srv := server.New(server.Config{Addr: serverAddr})
    go func() { _ = srv.Run(ctx) }()

    // 2. Start proxy_runner in listen mode.
    go func() {
        _ = runner.ListenAndServe(ctx, runner.ListenConfig{
            Config: runner.Config{
                AllowedRoots: []string{t.TempDir()},
                MaxTasks:     1,
                Hostname:     "proxy-runner-host",
            },
            WSListen: proxyListen,
        })
    }()

    // 3. Start target_runner in listen mode.
    go func() {
        _ = runner.ListenAndServe(ctx, runner.ListenConfig{
            Config: runner.Config{
                AllowedRoots: []string{t.TempDir()},
                MaxTasks:     1,
                Hostname:     "target-runner-host",
            },
            WSListen: targetListen,
        })
    }()

    time.Sleep(500 * time.Millisecond)

    serverCID := mustParseCID(t, "ws:"+serverAddr+"-*")

    // 4. Reverse-dial proxy_runner (direct, Phase A).
    proxyDialCID := mustParseCID(t, "ws:"+proxyListen+"-*")
    var noVia objproto.ConnectionID // zero = no via
    if resp, err := cli.ServerDialRunner(ctx, serverCID, proxyDialCID, noVia); err != nil || resp.Status != protocol.DialRunnerStatus_Ok {
        t.Fatalf("dial proxy_runner: err=%v status=%v", err, resp.Status)
    }

    // 5. Wait for proxy_runner to appear in registry, capture its CID.
    proxyRegisteredCID := waitForRunnerCID(t, srv, "proxy-runner-host", 5*time.Second)

    // 6. Now dial target_runner --via proxyRegisteredCID.
    targetCID := mustParseCID(t, "ws:"+targetListen+"-*")
    resp, err := cli.ServerDialRunner(ctx, serverCID, targetCID, proxyRegisteredCID)
    if err != nil {
        t.Fatalf("dial target via proxy: %v", err)
    }
    if resp.Status != protocol.DialRunnerStatus_Ok {
        t.Fatalf("via-relay status: got %v want Ok", resp.Status)
    }

    // 7. Verify target_runner appears in registry.
    waitForRunnerInRegistry(t, srv, "target-runner-host", 5*time.Second)
}

func waitForRunnerCID(t *testing.T, srv *server.Server, hostname string, timeout time.Duration) objproto.ConnectionID {
    t.Helper()
    deadline := time.Now().Add(timeout)
    for time.Now().Before(deadline) {
        for _, r := range srv.RegisteredRunners() {
            if r.Hostname == hostname {
                return r.ID // peer.Conn's CID, exact value used by --via
            }
        }
        time.Sleep(100 * time.Millisecond)
    }
    t.Fatalf("runner %q did not appear in registry within %v", hostname, timeout)
    return objproto.ConnectionID{}
}

func waitForRunnerInRegistry(t *testing.T, srv *server.Server, hostname string, timeout time.Duration) {
    waitForRunnerCID(t, srv, hostname, timeout)
}
```

- [ ] **Step 1: Write the test**

(Above.)

- [ ] **Step 2: Run**

```bash
go test -tags integration ./integration/ -run TestRelayE2E -v -count=1
```

Expected: PASS — both runners (proxy and target) appear in registry.

- [ ] **Step 3: Commit**

```bash
git add integration/relay_e2e_test.go
git commit -m "test(integration): end-to-end server→proxy_runner→target_runner relay"
```

---

## Task 6: Final review + finishing

- [ ] **Step 1: Final cross-task review (dispatch subagent)**

- [ ] **Step 2: Smoke test on real binaries**

```sh
./bin/harness-server --listen 127.0.0.1:18600 &
./bin/agent-runner --listen 127.0.0.1:18601 --hostname proxy-smoke --roots /tmp &
./bin/agent-runner --listen 127.0.0.1:18602 --hostname target-smoke --roots /tmp &

# Register proxy
./bin/harness-cli --server-cid ws:127.0.0.1:18600-* server dial-runner ws:127.0.0.1:18601-*

# Capture proxy's registered CID
PROXY_CID=$(./bin/harness-cli --server-cid ws:127.0.0.1:18600-* ls | awk '/host=proxy-smoke/ {for(i=1;i<=NF;i++) if($i ~ /^id=/) {sub("^id=","",$i); print $i}}')

# Register target via proxy
./bin/harness-cli --server-cid ws:127.0.0.1:18600-* server dial-runner ws:127.0.0.1:18602-* --via "$PROXY_CID"

# Verify
./bin/harness-cli --server-cid ws:127.0.0.1:18600-* ls
```

Expected: both `proxy-smoke` and `target-smoke` listed with Idle status.

- [ ] **Step 3: Use superpowers:finishing-a-development-branch**

---

## Open questions (deferred to plan execution)

1. **Reply channel for EstablishRelayResponse**: spec says use a new
   `RunnerMessageType.establish_relay_response`. Confirm pattern matches
   `RunnerHelloResponse`'s flow (Task 1 / Task 2 implementer).
2. **POC outcomes might require spec revision**: if Task 0 reveals the
   protocol mechanics don't work as designed, STOP and revise spec before
   continuing.
3. **`expectedRelays` lifetime**: today's design has `Take()` removing the
   entry once consumed. If server's slot_id dial never arrives (server
   crashes mid-handshake), the entry sits forever. Phase B's pattern is to
   not worry about this — server timeouts (DialTimeout 10s) bound the
   period; if needed, add a TTL sweep later.
