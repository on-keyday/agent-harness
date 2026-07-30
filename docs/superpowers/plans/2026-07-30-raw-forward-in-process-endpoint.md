# Raw forward with an in-process client endpoint Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a client hold a port forward's client-side endpoint inside its own process instead of on a bound socket, so the WebUI can open forwards at all and `harness-cli` gains an `ssh -W`-style stdio mode.

**Architecture:** `cli.Client.OpenPortForward` already returns a bidi stream and touches no socket, and a `direction=local` registration already never contacts the runner — so the data path needs no change. A new `client_endpoint :ClientEndpointKind` field on two client↔server formats declares that the client end is in-process (so the registry stops reporting a bind address that does not exist), one server-side branch rejects the not-yet-implemented `remote × in_process` combination, and one shared client helper (`OpenRawForward`) is consumed by three surfaces: `harness-cli forward -W`, a WebUI raw-connect pane, and a TUI modal.

**Tech Stack:** Go; brgen `.bgn` schema regenerated via `make protoregen`; trsf bidi streams; `syscall/js` bindings for wasm; vanilla JS/CSS for the WebUI; bubbletea for the TUI.

Spec: `docs/superpowers/specs/2026-07-30-raw-forward-in-process-endpoint-design.md`.

## Global Constraints

- **Raw bytes only.** No HTTP request builder, no response formatting, no TLS (spec decision 8).
- **`direction` is not extended.** `PortForwardDirection` and every runner-facing message stay byte-identical; the only wire changes are two client↔server formats (spec decision 1).
- **`bind_addr` / `bind_port` are sent empty / 0** for `local × in_process`. No placeholder address is invented (spec decision 2).
- **`direction=remote` × `client_endpoint=in_process` is rejected server-side** with `internal_error` (spec decision 3).
- **Every raw connection registers**, after its data stream is established; registration failure closes the data stream (spec decisions 4, 5).
- **`DIR` column keeps `-L` / `-R`**; the endpoint kind appears in the `SPEC` column as `(in-process)` (spec decision 6).
- **WebUI output view is a `<pre>`, never the shared xterm** — the shared xterm assumes VT-interpretable text and a single writer.
- **WebUI received-byte cap: 256 KiB ring.**
- **WebUI palette `#1e1e1e` / `#d4d4d4`, and the ≤600px layout, from the first cut.**
- **Build hygiene:** compile-check with `go build ./...` or `go build -o /dev/null ./cmd/<x>`. Never bare `go build ./cmd/<x>/` — it drops an executable in the worktree root.
- **`AppendData` stores its slice by reference** and copies asynchronously (`trsf/send_stream.go`). Any buffer handed to it must be a copy the caller never touches again.

---

## File Structure

| File | Responsibility |
| --- | --- |
| `runner/protocol/message.bgn` | Schema source: new `ClientEndpointKind` enum + one field on `RegisterPortForwardRequest` and `PortForwardInfo`. |
| `runner/protocol/message.go` | Generated. Never hand-edited. |
| `runner/protocol/port_forward_test.go` | Round-trip tests for the new field. |
| `server/port_forward_registry.go` | `portForward` gains `clientEndpoint`. |
| `server/port_forward.go` | `handleRegisterPortForward` stores the field and rejects `remote × in_process`. |
| `server/port_forward_list.go` | `portForwardInfo` copies the field into `PortForwardInfo`. |
| `cli/port_forward_list.go` | `PortForwardSpecString` renders `(in-process)`. |
| `cli/port_forward.go` | `RegisterPortForward` gains the endpoint argument; `serveLocalForwardControl` generalised with an end callback. |
| `cli/forward_endpoint.go` | **NEW.** `RawConn` + `OpenRawForward` + control watcher. Platform-independent; the one place the three surfaces share. |
| `cli/forward_stdio.go` | **NEW** (`//go:build !js`). `RunStdioForward`: splice a `RawConn` to stdin/stdout. |
| `cli/raw_forward_wasm.go` | **NEW** (`//go:build js`). Keyed pane slots + generation guard + JS-hook pump. |
| `cmd/harness-cli/main.go` | `forward -W host:port`. |
| `cmd/harness-webui-wasm/main.go` | `harness.rawOpen` / `rawSend` / `rawClose` bindings. |
| `webui/index.html` | Raw-connect pane markup inside the existing `#connections` section. |
| `webui/static/main.js` | Pane state, hooks, text/hex rendering, send box. |
| `webui/static/style.css` | Pane styling, dark palette, ≤600px. |
| `tui/rawforward.go` | **NEW.** `RawConnectModal` + `DoStartRawForward` + messages. |
| `tui/app.go` | `t` key opens the modal; message handling. |
| `integration/port_forward_test.go` | E2E: raw connect round-trip, listed with `(in-process)`, killable. |

---

## Task 1: Protocol — `ClientEndpointKind`

**Files:**
- Modify: `runner/protocol/message.bgn` (new enum near line 1192; one field on the format at line 1236 and the format at line 1261)
- Regenerate: `runner/protocol/message.go` (via `make protoregen`)
- Test: `runner/protocol/port_forward_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `protocol.ClientEndpointKind` with members `protocol.ClientEndpointKind_OsSocket` (zero value) and `protocol.ClientEndpointKind_InProcess`; field `ClientEndpoint` on `protocol.RegisterPortForwardRequest` and `protocol.PortForwardInfo`.

- [ ] **Step 1: Write the failing tests**

Append to `runner/protocol/port_forward_test.go`:

```go
// TestRegisterPortForwardRequest_InProcessRoundTrip covers the endpoint-kind
// field. A local in-process registration has no client-side bind address, so
// the bind pair round-trips as empty/0 rather than carrying a placeholder.
func TestRegisterPortForwardRequest_InProcessRoundTrip(t *testing.T) {
	req := RegisterPortForwardRequest{
		TaskId:         TaskID{Id: [16]byte{0x11}},
		Direction:      PortForwardDirection_Local,
		TargetPort:     3000,
		ClientEndpoint: ClientEndpointKind_InProcess,
	}
	req.SetTargetHost([]byte("127.0.0.1"))
	enc, err := req.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	got := &RegisterPortForwardRequest{}
	if _, err := got.Decode(enc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ClientEndpoint != ClientEndpointKind_InProcess {
		t.Fatalf("ClientEndpoint = %v, want InProcess", got.ClientEndpoint)
	}
	if got.BindPort != 0 || len(got.BindAddr) != 0 {
		t.Fatalf("bind pair must stay empty for an in-process endpoint: addr=%q port=%d", got.BindAddr, got.BindPort)
	}
	if string(got.TargetHost) != "127.0.0.1" || got.TargetPort != 3000 {
		t.Fatalf("target round-trip mismatch: %q:%d", got.TargetHost, got.TargetPort)
	}
}

// TestRegisterPortForwardRequest_OsSocketIsZeroValue pins the enum order: a
// struct built without naming the field must mean "the client end is a real
// socket", which is what every existing -L / -R call site means.
func TestRegisterPortForwardRequest_OsSocketIsZeroValue(t *testing.T) {
	req := RegisterPortForwardRequest{Direction: PortForwardDirection_Local, BindPort: 18080}
	req.SetBindAddr([]byte("127.0.0.1"))
	if req.ClientEndpoint != ClientEndpointKind_OsSocket {
		t.Fatalf("zero value = %v, want OsSocket", req.ClientEndpoint)
	}
}

// TestPortForwardInfo_InProcessRoundTrip covers the list-result side, which is
// what makes an in-process forward distinguishable in `forward ls`.
func TestPortForwardInfo_InProcessRoundTrip(t *testing.T) {
	info := PortForwardInfo{
		ForwardId:      7,
		Direction:      PortForwardDirection_Local,
		TaskId:         TaskID{Id: [16]byte{0x22}},
		TargetPort:     6379,
		OriginKind:     ClientKind_Webui,
		ClientEndpoint: ClientEndpointKind_InProcess,
	}
	info.SetTargetHost([]byte("127.0.0.1"))
	info.SetOriginCid([]byte("ws:127.0.0.1:1-2"))
	enc, err := info.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	got := &PortForwardInfo{}
	if _, err := got.Decode(enc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ClientEndpoint != ClientEndpointKind_InProcess {
		t.Fatalf("ClientEndpoint = %v, want InProcess", got.ClientEndpoint)
	}
	if got.ForwardId != 7 || got.TargetPort != 6379 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./runner/protocol/ -run 'PortForward.*InProcess|OsSocketIsZeroValue' -v`
Expected: compile failure — `undefined: ClientEndpointKind_InProcess`, `unknown field ClientEndpoint`.

- [ ] **Step 3: Edit the schema**

In `runner/protocol/message.bgn`, immediately after the `PortForwardDirection` enum (line ~1192-1195), add:

```
enum ClientEndpointKind:
    :u8
    os_socket   # client-side endpoint is an OS socket: the listener for -L, the dial for -R.
    in_process  # client-side endpoint lives inside the client process (stdio for -W, a UI
                # pane in TUI/WebUI). The address pair describing the client side is therefore
                # empty: bind_addr / bind_port for a local forward, target_host / target_port
                # for a remote one. Only local x in_process is defined today; the server
                # rejects remote x in_process.
```

Append one field to `RegisterPortForwardRequest` (line ~1236), after `target_port`:

```
    client_endpoint :ClientEndpointKind
```

Append the same field to `PortForwardInfo` (line ~1261), after `origin_cid`:

```
    client_endpoint :ClientEndpointKind
```

Do **not** touch `PortForwardDirection`, `RunnerOpenPortForwardRequest`, or any other format.

- [ ] **Step 4: Regenerate**

Run: `make protoregen ARGS='runner/protocol/message.bgn'`
Expected: `runner/protocol/message.go` updated. Confirm with `git diff --stat runner/protocol/` that only `message.bgn` and `message.go` changed.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./runner/protocol/ -v -run PortForward`
Expected: PASS, including the pre-existing port-forward round-trip tests.

- [ ] **Step 6: Confirm the wire skew is recoverable**

Run: `scripts/wire-skew-check.sh`
Expected: PASS. This builds NEW-runner × OLD-server and asserts the skew is *recoverable* (rejection → retry → self-heal), which is the guard that exists because a `.bgn` change once killed all 12 runner slots. A setup error exits 2 — that is not a pass; fix it before continuing.

- [ ] **Step 7: Commit**

```bash
git add runner/protocol/message.bgn runner/protocol/message.go runner/protocol/port_forward_test.go
git commit -m "feat(protocol): ClientEndpointKind on port-forward registration and list info"
```

---

## Task 2: Server — store the field, reject `remote × in_process`, render `(in-process)`

**Files:**
- Modify: `server/port_forward_registry.go:16-29` (struct field)
- Modify: `server/port_forward.go:75-118` (`handleRegisterPortForward`)
- Modify: `server/port_forward_list.go:90-108` (`portForwardInfo`)
- Modify: `cli/port_forward_list.go:112-118` (`PortForwardSpecString`)
- Test: `server/port_forward_test.go`, `cli/port_forward_list_test.go`

**Interfaces:**
- Consumes: `protocol.ClientEndpointKind` from Task 1.
- Produces: a registry entry carrying `clientEndpoint`; `PortForwardSpecString` output `(in-process) -> host:port` for in-process locals.

- [ ] **Step 1: Write the failing server tests**

Append to `server/port_forward_test.go`:

```go
// TestHandleRegisterPortForward_LocalInProcess registers a forward whose client
// end is the client process itself: no bind address exists, the runner is not
// contacted, and the stored entry remembers the endpoint kind so the listing can
// avoid reporting a bind address that never existed.
func TestHandleRegisterPortForward_LocalInProcess(t *testing.T) {
	h := &TaskHandler{Tasks: NewTaskStore(), Registry: NewRegistry()}
	idHex := addRunningTask(t, h, 0x44, "runner-1")
	raw, err := hex.DecodeString(idHex)
	if err != nil {
		t.Fatal(err)
	}
	var rawID [16]byte
	copy(rawID[:], raw)

	runnerConn := &fakeConn{}
	h.Registry.Add(&RunnerEntry{ID: "runner-1", Conn: runnerConn})

	ctrl := newRecordingBidiStream(881)
	defer ctrl.CloseBoth()
	clientConn := &fakeConn{nextBidi: ctrl}
	req := &protocol.RegisterPortForwardRequest{
		TaskId:         protocol.TaskID{Id: rawID},
		Direction:      protocol.PortForwardDirection_Local,
		TargetPort:     3000,
		ClientEndpoint: protocol.ClientEndpointKind_InProcess,
	}
	req.SetTargetHost([]byte("127.0.0.1"))

	resp := h.handleRegisterPortForward(clientConn, req, clientConn.ConnectionID().String())
	if resp.Status != protocol.OpenPortForwardStatus_Ok {
		t.Fatalf("status = %v, want Ok", resp.Status)
	}
	pf, ok := h.pforwards().get(resp.ForwardId)
	if !ok {
		t.Fatal("registration not stored")
	}
	if pf.clientEndpoint != protocol.ClientEndpointKind_InProcess {
		t.Fatalf("stored endpoint = %v, want InProcess", pf.clientEndpoint)
	}
	if sent := runnerConn.Sent(); len(sent) != 0 {
		t.Fatalf("an in-process local registration must not contact the runner; got %d messages", len(sent))
	}
	info := portForwardInfo(pf)
	if info.ClientEndpoint != protocol.ClientEndpointKind_InProcess {
		t.Fatalf("listing lost the endpoint kind: %v", info.ClientEndpoint)
	}
}

// TestHandleRegisterPortForward_RemoteInProcessRejected pins the unimplemented
// combination shut. A runner-side listener whose accepted connections are
// answered by an in-browser handler is a separate design; letting it register
// would half-work (the listener binds, nothing answers).
func TestHandleRegisterPortForward_RemoteInProcessRejected(t *testing.T) {
	h := &TaskHandler{Tasks: NewTaskStore(), Registry: NewRegistry()}
	idHex := addRunningTask(t, h, 0x45, "runner-1")
	raw, err := hex.DecodeString(idHex)
	if err != nil {
		t.Fatal(err)
	}
	var rawID [16]byte
	copy(rawID[:], raw)

	runnerConn := &fakeConn{}
	h.Registry.Add(&RunnerEntry{ID: "runner-1", Conn: runnerConn})
	clientConn := &fakeConn{nextBidi: newRecordingBidiStream(882)}
	req := &protocol.RegisterPortForwardRequest{
		TaskId:         protocol.TaskID{Id: rawID},
		Direction:      protocol.PortForwardDirection_Remote,
		BindPort:       18099,
		TargetPort:     3000,
		ClientEndpoint: protocol.ClientEndpointKind_InProcess,
	}
	req.SetBindAddr([]byte("127.0.0.1"))
	req.SetTargetHost([]byte("127.0.0.1"))

	resp := h.handleRegisterPortForward(clientConn, req, clientConn.ConnectionID().String())
	if resp.Status != protocol.OpenPortForwardStatus_InternalError {
		t.Fatalf("status = %v, want InternalError", resp.Status)
	}
	if resp.ForwardId != 0 {
		t.Fatalf("rejected registration must not get an id, got %d", resp.ForwardId)
	}
	if sent := runnerConn.Sent(); len(sent) != 0 {
		t.Fatalf("rejected registration must not ask the runner to bind; got %d messages", len(sent))
	}
}
```

- [ ] **Step 2: Write the failing display test**

Append to `cli/port_forward_list_test.go`:

```go
func TestPortForwardSpecString_InProcess(t *testing.T) {
	fi := &protocol.PortForwardInfo{
		Direction:      protocol.PortForwardDirection_Local,
		TargetPort:     6379,
		ClientEndpoint: protocol.ClientEndpointKind_InProcess,
	}
	fi.SetTargetHost([]byte("127.0.0.1"))
	if got, want := PortForwardSpecString(fi), "(in-process) -> 127.0.0.1:6379"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// The DIR column still reports the direction, not the endpoint kind.
	if got := PortForwardDirFlag(fi.Direction); got != "-L" {
		t.Fatalf("dir flag = %q, want -L", got)
	}
}
```

- [ ] **Step 3: Run both to verify they fail**

Run: `go test ./server/ -run 'RegisterPortForward_(LocalInProcess|RemoteInProcessRejected)' && go test ./cli/ -run TestPortForwardSpecString_InProcess`
Expected: compile failure (`pf.clientEndpoint` undefined) then assertion failures.

- [ ] **Step 4: Add the struct field**

In `server/port_forward_registry.go`, inside `type portForward struct`, after `targetPort uint16`:

```go
	// clientEndpoint records whether the client side of this forward is a real
	// socket or lives inside the client process. It changes nothing about the
	// byte path — it exists so the listing does not report a bind address that
	// was never bound.
	clientEndpoint protocol.ClientEndpointKind
```

- [ ] **Step 5: Store it and reject the unsupported combination**

In `server/port_forward.go`, inside `handleRegisterPortForward`, add to the `pf := &portForward{...}` literal:

```go
		clientEndpoint: req.ClientEndpoint,
```

Then, immediately before the existing `if req.Direction == protocol.PortForwardDirection_Remote {` branch, insert:

```go
	// A runner-side listener whose accepted connections are answered by an
	// in-process handler on the client is a separate design (the browser as a
	// service endpoint). Refuse it rather than letting it half-work: the
	// runner's listener would bind and nothing would ever answer.
	if req.Direction == protocol.PortForwardDirection_Remote &&
		req.ClientEndpoint == protocol.ClientEndpointKind_InProcess {
		slog.Warn("port_forward: remote x in_process registration refused (unimplemented combination)",
			"task_id", taskIDHex)
		return errResp(protocol.OpenPortForwardStatus_InternalError)
	}
```

- [ ] **Step 6: Carry it into the listing**

In `server/port_forward_list.go`, inside `portForwardInfo`, alongside the existing `info.OriginKind = pf.clientKind` assignment:

```go
	info.ClientEndpoint = pf.clientEndpoint
```

- [ ] **Step 7: Render it**

Replace `PortForwardSpecString` in `cli/port_forward_list.go`:

```go
// PortForwardSpecString renders the forward's endpoints as one column. An
// in-process client endpoint has no address to show on the client side, so it
// says so instead of printing the empty bind pair as ":0".
func PortForwardSpecString(fi *protocol.PortForwardInfo) string {
	listen := fmt.Sprintf("%s:%d", fi.BindAddr, fi.BindPort)
	switch {
	case fi.Direction == protocol.PortForwardDirection_Remote:
		listen = "runner:" + listen
	case fi.ClientEndpoint == protocol.ClientEndpointKind_InProcess:
		listen = "(in-process)"
	}
	return fmt.Sprintf("%s -> %s:%d", listen, fi.TargetHost, fi.TargetPort)
}
```

- [ ] **Step 8: Run the tests to verify they pass**

Run: `go test ./server/ ./cli/ -run 'PortForward'`
Expected: PASS, including pre-existing tests (`TestHandleRegisterPortForward_LocalRegisters` still expects a bind address in its spec string).

- [ ] **Step 9: Commit**

```bash
git add server/port_forward_registry.go server/port_forward.go server/port_forward_list.go server/port_forward_test.go cli/port_forward_list.go cli/port_forward_list_test.go
git commit -m "feat(server): record the client endpoint kind and refuse remote x in_process"
```

---

## Task 3: Shared client layer — `RawConn` / `OpenRawForward`

**Files:**
- Modify: `cli/port_forward.go:366` (`RegisterPortForward` signature), `:210` and `:425` (call sites), `:244` (`serveLocalForwardControl` → `serveForwardControl`)
- Create: `cli/forward_endpoint.go`
- Test: `cli/forward_endpoint_test.go`

**Interfaces:**
- Consumes: `protocol.ClientEndpointKind` (Task 1); the server behaviour from Task 2.
- Produces:
  - `func (c *Client) RegisterPortForward(ctx context.Context, taskIDHex string, dir protocol.PortForwardDirection, bindAddr string, bindPort int, targetHost string, targetPort int, endpoint protocol.ClientEndpointKind) (trsf.BidirectionalStream, uint64, error)`
  - `type RawConn struct { ... }` with `func (r *RawConn) Send(b []byte) error`, `func (r *RawConn) Recv(ctx context.Context) ([]byte, bool, error)`, `func (r *RawConn) Close() error`, `func (r *RawConn) ForwardID() uint64`
  - `func OpenRawForward(ctx context.Context, c *Client, taskIDHex, host string, port int, logf func(string)) (*RawConn, error)`

- [ ] **Step 1: Write the failing tests**

Create `cli/forward_endpoint_test.go`:

```go
package cli

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// fakeBidiStream is an in-memory trsf.BidirectionalStream: reads come from
// whatever feed() pushed, writes are recorded, and CloseBoth is observable.
// Enough to exercise the control-stream state machine without a server.
type fakeBidiStream struct {
	id      trsf.StreamID
	mu      sync.Mutex
	written []byte
	closed  bool
	recv    chan []byte
	done    chan struct{}
}

func newFakeBidiStream(id trsf.StreamID) *fakeBidiStream {
	return &fakeBidiStream{id: id, recv: make(chan []byte, 8), done: make(chan struct{})}
}

func (s *fakeBidiStream) feed(b []byte) { s.recv <- b }

func (s *fakeBidiStream) eof() {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
	default:
		close(s.done)
	}
}

func (s *fakeBidiStream) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *fakeBidiStream) Written() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.written...)
}

func (s *fakeBidiStream) ID() trsf.StreamID { return s.id }

func (s *fakeBidiStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.written = append(s.written, p...)
	return len(p), nil
}

func (s *fakeBidiStream) WriteContext(_ context.Context, p []byte) (int, error) { return s.Write(p) }

func (s *fakeBidiStream) Close() error { return s.CloseBoth() }

func (s *fakeBidiStream) CloseBoth() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.eof()
	return nil
}

func (s *fakeBidiStream) HasSendData() bool { return false }
func (s *fakeBidiStream) Completed() bool   { return false }

func (s *fakeBidiStream) AppendData(_ bool, payloads ...[]byte) error {
	for _, p := range payloads {
		if _, err := s.Write(p); err != nil {
			return err
		}
	}
	return nil
}

func (s *fakeBidiStream) AppendDataContext(_ context.Context, eof bool, payloads ...[]byte) error {
	return s.AppendData(eof, payloads...)
}

func (s *fakeBidiStream) Read(p []byte) (int, error) {
	data, eof, err := s.ReadDirect(uint64(len(p)))
	if err != nil {
		return 0, err
	}
	if eof && len(data) == 0 {
		return 0, io.EOF
	}
	return copy(p, data), nil
}

func (s *fakeBidiStream) ReadContext(ctx context.Context, p []byte) (int, error) {
	data, eof, err := s.ReadDirectContext(ctx, uint64(len(p)))
	if err != nil {
		return 0, err
	}
	if eof && len(data) == 0 {
		return 0, io.EOF
	}
	return copy(p, data), nil
}

func (s *fakeBidiStream) ReadDirect(maxN uint64) ([]byte, bool, error) {
	return s.ReadDirectContext(context.Background(), maxN)
}

func (s *fakeBidiStream) ReadDirectContext(ctx context.Context, _ uint64) ([]byte, bool, error) {
	select {
	case b := <-s.recv:
		return b, false, nil
	case <-s.done:
		return nil, true, nil
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
}

func (s *fakeBidiStream) HasRecvData() bool { return len(s.recv) > 0 }
func (s *fakeBidiStream) EOF() bool         { return s.isClosed() }
func (s *fakeBidiStream) Cancel()           {}

func closedEventBytes(t *testing.T, reason protocol.PortForwardCloseReason) []byte {
	t.Helper()
	var ev protocol.PortForwardEvent
	ev.Kind = protocol.PortForwardEventKind_Closed
	ev.SetClosed(protocol.PortForwardClosed{Reason: reason})
	b, err := ev.Append(nil)
	if err != nil {
		t.Fatalf("encode closed event: %v", err)
	}
	return b
}

// TestRawConn_ControlClosedClosesData is what makes `forward kill` reach a raw
// connection: the server pushes a Closed record on the control stream, and the
// data stream — the pane's or the -W process's actual bytes — must go down with
// it rather than sit there relaying through a forward the server forgot.
func TestRawConn_ControlClosedClosesData(t *testing.T) {
	ctrl := newFakeBidiStream(11)
	data := newFakeBidiStream(12)
	rc := &RawConn{data: data, ctrl: ctrl, forwardID: 7}

	done := make(chan struct{})
	go func() {
		rc.watchControl(context.Background(), func(string) {})
		close(done)
	}()
	ctrl.feed(closedEventBytes(t, protocol.PortForwardCloseReason_Killed))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchControl did not return after a Closed record")
	}
	if !data.isClosed() {
		t.Fatal("data stream must be closed when the control stream reports Closed")
	}
	if !ctrl.isClosed() {
		t.Fatal("control stream must be closed on the way out")
	}
}

// TestRawConn_ControlEOFClosesData covers the other end of the same rule: the
// server connection dying is indistinguishable to us from a kill as far as the
// data stream's fate is concerned.
func TestRawConn_ControlEOFClosesData(t *testing.T) {
	ctrl := newFakeBidiStream(21)
	data := newFakeBidiStream(22)
	rc := &RawConn{data: data, ctrl: ctrl, forwardID: 8}

	var lines []string
	done := make(chan struct{})
	go func() {
		rc.watchControl(context.Background(), func(s string) { lines = append(lines, s) })
		close(done)
	}()
	ctrl.eof()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchControl did not return on control EOF")
	}
	if !data.isClosed() {
		t.Fatal("data stream must be closed when the control stream EOFs")
	}
	if len(lines) == 0 {
		t.Fatal("an unexpected control EOF must be reported, not swallowed")
	}
}

// TestRawConn_SendCopiesCallerBuffer pins the AppendData contract: it stores the
// slice by reference and copies asynchronously, so a caller reusing its buffer
// (every pump does) must not be able to corrupt bytes already handed over.
func TestRawConn_SendCopiesCallerBuffer(t *testing.T) {
	data := newFakeBidiStream(31)
	rc := &RawConn{data: data, ctrl: newFakeBidiStream(32)}
	buf := []byte("ping")
	if err := rc.Send(buf); err != nil {
		t.Fatalf("send: %v", err)
	}
	copy(buf, "XXXX")
	if got := string(data.Written()); got != "ping" {
		t.Fatalf("Send must copy its argument; stream saw %q", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./cli/ -run TestRawConn -v`
Expected: compile failure — `undefined: RawConn`.

- [ ] **Step 3: Thread the endpoint kind through `RegisterPortForward`**

In `cli/port_forward.go`, change the signature at line 366 and the request construction:

```go
func (c *Client) RegisterPortForward(ctx context.Context, taskIDHex string, dir protocol.PortForwardDirection,
	bindAddr string, bindPort int, targetHost string, targetPort int,
	endpoint protocol.ClientEndpointKind) (trsf.BidirectionalStream, uint64, error) {
```

and inside it:

```go
	body := protocol.RegisterPortForwardRequest{TaskId: tid, Direction: dir,
		BindPort: uint16(bindPort), TargetPort: uint16(targetPort), ClientEndpoint: endpoint}
```

Update the two existing call sites — both hold real sockets:

- line ~210 (inside `RunForward`, after the listener bound): append `protocol.ClientEndpointKind_OsSocket` to the argument list.
- line ~425 (`OpenRemoteForward`): append `protocol.ClientEndpointKind_OsSocket`.

- [ ] **Step 4: Generalise the control-stream loop**

In `cli/port_forward.go`, rename `serveLocalForwardControl` to `serveForwardControl` and give it an end callback so the raw path can reuse it instead of copying the event loop:

```go
// serveForwardControl reads a registration's control stream until the forward
// ends: a Closed record (someone ran `forward kill`) or EOF (server gone).
// onEnd, when non-nil, runs exactly once on the way out — the raw endpoint uses
// it to close the data stream it owns. Callers whose teardown is driven by
// something else (RunForward's listener) pass nil.
func (c *Client) serveForwardControl(ctx context.Context, ctrl trsf.BidirectionalStream, logf func(string), onEnd func()) {
	defer ctrl.CloseBoth()
	if onEnd != nil {
		defer onEnd()
	}
	// ... existing loop body unchanged ...
}
```

Update its single existing caller in `RunForward` to pass `nil`.

- [ ] **Step 5: Write `cli/forward_endpoint.go`**

```go
package cli

import (
	"context"
	"fmt"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// RawConn is one port forward whose client-side endpoint is this process rather
// than a socket. The runner still dials host:port exactly as it does for -L; the
// difference is that nothing local is bound, so a browser (which cannot listen)
// and a stdio filter (which has nothing to listen for) can both hold this end.
//
// It owns two streams: data carries the bytes, ctrl is the registration handle
// whose EOF deregisters the forward server-side and whose Closed record is how a
// `forward kill` from another surface reaches us.
type RawConn struct {
	data      trsf.BidirectionalStream
	ctrl      trsf.BidirectionalStream
	forwardID uint64
}

// OpenRawForward opens the data stream, registers the forward so it is listable
// and killable, and starts watching the control stream. Registration failure
// closes the data stream: a forward that is running but absent from `forward ls`
// is exactly the state the registry exists to prevent.
func OpenRawForward(ctx context.Context, c *Client, taskIDHex, host string, port int, logf func(string)) (*RawConn, error) {
	if logf == nil {
		logf = func(string) {}
	}
	data, err := c.OpenPortForward(ctx, taskIDHex, host, port)
	if err != nil {
		return nil, err
	}
	ctrl, fid, err := c.RegisterPortForward(ctx, taskIDHex, protocol.PortForwardDirection_Local,
		"", 0, host, port, protocol.ClientEndpointKind_InProcess)
	if err != nil {
		_ = data.CloseBoth()
		return nil, fmt.Errorf("raw forward: register: %w", err)
	}
	rc := &RawConn{data: data, ctrl: ctrl, forwardID: fid}
	go rc.watchControl(ctx, logf)
	return rc, nil
}

// ForwardID is the server-assigned registration id, as shown by `forward ls`.
func (r *RawConn) ForwardID() uint64 { return r.forwardID }

// Send writes bytes to the far end. The buffer is copied: AppendData keeps the
// slice by reference and copies asynchronously, so a caller reusing its read
// buffer (every pump does) would otherwise corrupt bytes already handed over.
func (r *RawConn) Send(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	chunk := make([]byte, len(b))
	copy(chunk, b)
	return r.data.AppendData(false, chunk)
}

// Recv returns the next chunk from the far end. The bool reports EOF, matching
// trsf's ReadDirectContext shape so consumers need no second idiom.
func (r *RawConn) Recv(ctx context.Context) ([]byte, bool, error) {
	return r.data.ReadDirectContext(ctx, 64*1024)
}

// Close tears down both streams. Closing ctrl is what deregisters the forward,
// so a closed pane leaves no entry behind in `forward ls`.
func (r *RawConn) Close() error {
	_ = r.data.CloseBoth()
	return r.ctrl.CloseBoth()
}

// watchControl ends the connection when the registration ends.
func (r *RawConn) watchControl(ctx context.Context, logf func(string)) {
	var c Client // zero value: serveForwardControl uses no Client state
	c.serveForwardControl(ctx, r.ctrl, logf, func() { _ = r.data.CloseBoth() })
}
```

If `serveForwardControl` turns out to touch `*Client` state, make it a package-level function `serveForwardControl(ctx, ctrl, logf, onEnd)` and update both callers rather than constructing a zero `Client`.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./cli/ -run TestRawConn -v`
Expected: PASS.

- [ ] **Step 7: Verify nothing else broke**

Run: `go build ./... && go test ./cli/ ./server/ ./tui/ ./cmd/...`
Expected: PASS. The signature change is caught at compile time everywhere.

- [ ] **Step 8: Commit**

```bash
git add cli/port_forward.go cli/forward_endpoint.go cli/forward_endpoint_test.go
git commit -m "feat(cli): RawConn — a port forward whose client endpoint is this process"
```

---

## Task 4: `harness-cli forward -W host:port`

**Files:**
- Create: `cli/forward_stdio.go` (`//go:build !js`)
- Modify: `cli/port_forward.go` (add `ParseStdioForwardSpec`)
- Modify: `cmd/harness-cli/main.go:471-545`
- Test: `cli/port_forward_test.go`, `cmd/harness-cli/forward_test.go`

**Interfaces:**
- Consumes: `OpenRawForward`, `RawConn` (Task 3).
- Produces:
  - `func ParseStdioForwardSpec(s string) (host string, port int, err error)`
  - `func RunStdioForward(ctx context.Context, c *Client, taskIDHex, host string, port int, logf func(string)) error`

- [ ] **Step 1: Write the failing parser test**

Append to `cli/port_forward_test.go`:

```go
func TestParseStdioForwardSpec(t *testing.T) {
	host, port, err := ParseStdioForwardSpec("127.0.0.1:6379")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if host != "127.0.0.1" || port != 6379 {
		t.Fatalf("got %s:%d", host, port)
	}
	if h, p, err := ParseStdioForwardSpec("localhost:3000"); err != nil || h != "localhost" || p != 3000 {
		t.Fatalf("hostname form: %s:%d err=%v", h, p, err)
	}
	for _, bad := range []string{"", "nope", "127.0.0.1", ":3000", "127.0.0.1:", "127.0.0.1:0", "127.0.0.1:70000", "a:b:c"} {
		if _, _, err := ParseStdioForwardSpec(bad); err == nil {
			t.Fatalf("expected error on %q", bad)
		}
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./cli/ -run TestParseStdioForwardSpec`
Expected: compile failure — `undefined: ParseStdioForwardSpec`.

- [ ] **Step 3: Implement the parser**

Append to `cli/port_forward.go`:

```go
// ParseStdioForwardSpec parses "host:port" for -W: the runner dials host:port
// and this process's stdin/stdout is the other end. IPv6 literal hosts are
// unsupported, matching ParseForwardSpec / ParseRemoteForwardSpec.
func ParseStdioForwardSpec(s string) (string, int, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return "", 0, fmt.Errorf("forward: bad -W spec %q (want host:port)", s)
	}
	host := parts[0]
	if host == "" {
		return "", 0, fmt.Errorf("forward: empty host in %q", s)
	}
	port, err := strconv.Atoi(parts[1])
	if err != nil || port <= 0 || port > 65535 {
		return "", 0, fmt.Errorf("forward: bad port in %q", s)
	}
	return host, port, nil
}
```

- [ ] **Step 4: Run it to verify it passes**

Run: `go test ./cli/ -run TestParseStdioForwardSpec`
Expected: PASS.

- [ ] **Step 5: Implement the stdio splice**

Create `cli/forward_stdio.go`:

```go
//go:build !js

package cli

import (
	"context"
	"os"
	"sync"
)

// RunStdioForward opens a raw forward to host:port and splices it to this
// process's stdin/stdout — the harness equivalent of `ssh -W`. It returns when
// either side ends: EOF on stdin, the far end closing, or the forward being
// killed from another surface (which closes the data stream via RawConn's
// control watcher).
//
// Teardown is either-side-wins, matching spliceConnStream: a half-closed peer
// must not leave the reverse direction blocked forever.
func RunStdioForward(ctx context.Context, c *Client, taskIDHex, host string, port int, logf func(string)) error {
	rc, err := OpenRawForward(ctx, c, taskIDHex, host, port, logf)
	if err != nil {
		return err
	}
	var once sync.Once
	teardown := func() { once.Do(func() { _ = rc.Close() }) }
	defer teardown()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // stdin -> forward
		defer wg.Done()
		defer teardown()
		buf := make([]byte, 32*1024)
		for {
			n, rerr := os.Stdin.Read(buf)
			if n > 0 {
				// Send copies, so reusing buf is safe.
				if serr := rc.Send(buf[:n]); serr != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()
	go func() { // forward -> stdout
		defer wg.Done()
		defer teardown()
		for {
			data, eof, rerr := rc.Recv(ctx)
			if len(data) > 0 {
				if _, werr := os.Stdout.Write(data); werr != nil {
					return
				}
			}
			if eof || rerr != nil {
				return
			}
		}
	}()
	wg.Wait()
	return nil
}
```

- [ ] **Step 6: Wire the flag**

In `cmd/harness-cli/main.go`, in the `case "forward":` block:

Add to the usage banner at the top of the case:

```go
			fmt.Fprintln(os.Stderr, "       harness-cli forward <task-id> -W host:port")
```

After the `-R` flag registration (line ~522):

```go
		// -W mirrors ssh -W: no local listener, this process's stdin/stdout is
		// the forward's client endpoint. ssh makes -W exclusive with -L/-R
		// (it implies ClearAllForwardings) for the same reason we do: -W owns
		// the foreground and exits with its peer, while -L/-R are long-lived
		// listeners. One invocation, one lifetime.
		wspec := fs.String("W", "", "raw stdio forward host:port (mutually exclusive with -L / -R)")
```

Replace the "no specs" guard with:

```go
		fs.Parse(args[1:])
		if *wspec != "" && (len(specs) > 0 || len(rspecs) > 0) {
			fmt.Fprintln(os.Stderr, "forward: -W cannot be combined with -L / -R")
			os.Exit(2)
		}
		if len(specs) == 0 && len(rspecs) == 0 && *wspec == "" {
			fmt.Fprintln(os.Stderr, "usage: harness-cli forward <task-id> [-L [bind:]localport:remotehost:remoteport] [-R [bind:]runnerport:dialhost:dialport] [-W host:port] ...")
			os.Exit(2)
		}
		var wHost string
		var wPort int
		if *wspec != "" {
			h, p, werr := cli.ParseStdioForwardSpec(*wspec)
			if werr != nil {
				die(werr)
			}
			wHost, wPort = h, p
		}
```

After the client is dialled and `fctx` / `logf` are set up, before the existing `-R`/`-L` block, add the `-W` short-circuit:

```go
		if *wspec != "" {
			// stdout is the forward's payload channel, so status lines must go
			// to stderr (logf already does) and nothing may print to stdout.
			if err := cli.RunStdioForward(fctx, c, taskID, wHost, wPort, logf); err != nil {
				die(err)
			}
			return
		}
```

- [ ] **Step 7: Extend the routing test**

Append to `cmd/harness-cli/forward_test.go` a case asserting `forward <task> -W 127.0.0.1:3000` parses and that `-W` with `-L` is rejected. Follow the existing `TestForwardSubcommandRouting` structure in that file (read it first — it drives the same argument-routing helper rather than spawning a process).

- [ ] **Step 8: Verify**

Run: `go test ./cli/ ./cmd/harness-cli/ && go build ./... && go vet ./cmd/harness-cli/`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add cli/forward_stdio.go cli/port_forward.go cli/port_forward_test.go cmd/harness-cli/main.go cmd/harness-cli/forward_test.go
git commit -m "feat(cli): forward -W — ssh -W style stdio port forward"
```

---

## Task 5: wasm bindings

**Files:**
- Create: `cli/raw_forward_wasm.go` (`//go:build js`)
- Modify: `cmd/harness-webui-wasm/main.go` (binding registration near line 109)

**Interfaces:**
- Consumes: `OpenRawForward`, `RawConn` (Task 3).
- Produces (Go): `func OpenRawPane(ctx context.Context, c *Client, taskIDHex, host string, port int) (string, error)`, `func SendRawPane(key string, data []byte) error`, `func CloseRawPane(key string)`.
- Produces (JS): `harness.rawOpen(taskIDHex, host, port) -> Promise<key>`, `harness.rawSend(key, Uint8Array) -> Promise<void>`, `harness.rawClose(key) -> Promise<void>`; hooks the page must define: `window.harness_rawData(key, Uint8Array)`, `window.harness_rawClosed(key, reason)`.

- [ ] **Step 1: Write `cli/raw_forward_wasm.go`**

```go
//go:build js

package cli

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"syscall/js"
)

// rawSlot is one pane's raw connection plus a generation guard. Same shape and
// same reason as previewSlots in preview_wasm.go: the open is asynchronous, so a
// pane closed while OpenRawForward is still in flight must discard the
// connection instead of installing an orphan (stop-wins).
type rawSlot struct {
	conn *RawConn
	gen  uint64
}

var (
	rawMu    sync.Mutex
	rawSlots = map[string]*rawSlot{}
	rawGen   atomic.Uint64 // monotonic across ALL panes; every open/close reserves one
	rawKeySeq atomic.Uint64
)

// OpenRawPane opens a raw forward for a new pane and starts pumping its bytes to
// the JS hooks. The returned key identifies the pane in every later call.
func OpenRawPane(ctx context.Context, c *Client, taskIDHex, host string, port int) (string, error) {
	key := fmt.Sprintf("raw%d", rawKeySeq.Add(1))

	rawMu.Lock()
	gen := rawGen.Add(1)
	rawSlots[key] = &rawSlot{gen: gen}
	rawMu.Unlock()

	rc, err := OpenRawForward(ctx, c, taskIDHex, host, port, func(line string) {
		rawCall(key, gen, "harness_rawClosed", line)
	})
	if err != nil {
		rawMu.Lock()
		if slot := rawSlots[key]; slot != nil && slot.gen == gen {
			delete(rawSlots, key)
		}
		rawMu.Unlock()
		return "", err
	}

	rawMu.Lock()
	slot := rawSlots[key]
	if slot == nil || slot.gen != gen {
		// Superseded (pane closed) while opening: discard rather than install.
		rawMu.Unlock()
		_ = rc.Close()
		return "", errors.New("rawOpen: pane closed while connecting")
	}
	slot.conn = rc
	rawMu.Unlock()

	go rawPump(key, rc, gen)
	return key, nil
}

// SendRawPane writes bytes to the pane's connection.
func SendRawPane(key string, data []byte) error {
	rawMu.Lock()
	slot := rawSlots[key]
	var rc *RawConn
	if slot != nil {
		rc = slot.conn
	}
	rawMu.Unlock()
	if rc == nil {
		return errors.New("rawSend: no such pane")
	}
	return rc.Send(data)
}

// CloseRawPane closes the pane's connection, deregistering the forward. The
// generation bump silences the pump's remaining callbacks, so JS sees no
// harness_rawClosed for a close it initiated itself. Idempotent.
func CloseRawPane(key string) {
	rawMu.Lock()
	slot := rawSlots[key]
	delete(rawSlots, key)
	rawGen.Add(1)
	rawMu.Unlock()
	if slot != nil && slot.conn != nil {
		_ = slot.conn.Close()
	}
}

// rawPump forwards received bytes to the page until the connection ends.
func rawPump(key string, rc *RawConn, gen uint64) {
	for {
		data, eof, err := rc.Recv(context.Background())
		if len(data) > 0 {
			arr := js.Global().Get("Uint8Array").New(len(data))
			js.CopyBytesToJS(arr, data)
			if !rawCall(key, gen, "harness_rawData", arr) {
				return
			}
		}
		if eof || err != nil {
			rawCall(key, gen, "harness_rawClosed", "connection closed")
			rawMu.Lock()
			if slot := rawSlots[key]; slot != nil && slot.gen == gen {
				delete(rawSlots, key)
			}
			rawMu.Unlock()
			return
		}
	}
}

// rawCall invokes the named JS hook with key as its first argument, iff gen is
// still the pane's current generation; returns false when superseded so the pump
// exits silently. A missing hook (non-WebUI wasm host) is a no-op.
func rawCall(key string, gen uint64, hook string, args ...any) bool {
	rawMu.Lock()
	slot := rawSlots[key]
	stale := slot == nil || slot.gen != gen
	rawMu.Unlock()
	if stale {
		return false
	}
	fn := js.Global().Get(hook)
	if fn.Type() != js.TypeFunction {
		return true
	}
	all := append([]any{key}, args...)
	fn.Invoke(all...)
	return true
}
```

- [ ] **Step 2: Add the JS bindings**

In `cmd/harness-webui-wasm/main.go`, register three more entries in the same map that holds `"forwardKill"` (line ~109):

```go
		"rawOpen":            js.FuncOf(harnessRawOpen),
		"rawSend":            js.FuncOf(harnessRawSend),
		"rawClose":           js.FuncOf(harnessRawClose),
```

And add the functions, following `harnessForwardKill`'s Promise-executor shape verbatim:

```go
// harnessRawOpen opens a port forward whose client-side endpoint is this page: no
// local listener exists (a browser cannot bind one), the runner dials host:port,
// and bytes arrive via the harness_rawData hook keyed by the returned pane key.
//
//	harness.rawOpen(taskIDHex, host, port) -> Promise<key>
func harnessRawOpen(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			if len(args) < 3 {
				rejectErr(reject, errors.New("rawOpen: want (taskIDHex, host, port)"))
				return
			}
			key, err := cli.OpenRawPane(rootCtx, c, args[0].String(), args[1].String(), args[2].Int())
			if err != nil {
				rejectErr(reject, err)
				return
			}
			resolve.Invoke(js.ValueOf(key))
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessRawSend writes bytes to a pane's connection.
//
//	harness.rawSend(key, Uint8Array) -> Promise<void>
func harnessRawSend(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			if len(args) < 2 {
				rejectErr(reject, errors.New("rawSend: want (key, Uint8Array)"))
				return
			}
			val := args[1]
			data := make([]byte, val.Get("length").Int())
			js.CopyBytesToGo(data, val)
			if err := cli.SendRawPane(args[0].String(), data); err != nil {
				rejectErr(reject, err)
				return
			}
			resolve.Invoke(js.Undefined())
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessRawClose closes a pane's connection, which deregisters the forward.
//
//	harness.rawClose(key) -> Promise<void>
func harnessRawClose(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			if len(args) < 1 {
				rejectErr(reject, errors.New("rawClose: missing pane key"))
				return
			}
			cli.CloseRawPane(args[0].String())
			resolve.Invoke(js.Undefined())
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}
```

- [ ] **Step 3: Verify the wasm build**

Run: `make wasm-check`
Expected: PASS. Also run `go build ./...` — `cli/raw_forward_wasm.go` must not affect the native build.

- [ ] **Step 4: Commit**

```bash
git add cli/raw_forward_wasm.go cmd/harness-webui-wasm/main.go
git commit -m "feat(wasm): rawOpen/rawSend/rawClose bindings for browser-held forwards"
```

---

## Task 6: WebUI raw-connect pane

**Files:**
- Modify: `webui/index.html` (inside the `#connections` section, after `#forward-list`)
- Modify: `webui/static/main.js` (pane state, hooks, rendering, task select; and the stale comment above `renderForwardList`)
- Modify: `webui/static/style.css`

**Interfaces:**
- Consumes: `harness.rawOpen` / `rawSend` / `rawClose` and the `harness_rawData` / `harness_rawClosed` hooks (Task 5); `PortForwardSpecString`'s `(in-process)` output (Task 2) shows up in the existing `#forward-list` with no change.
- Produces: no JS API other consumers depend on.

- [ ] **Step 1: Add the markup**

In `webui/index.html`, inside `<section id="connections" ...>`, after `<div id="forward-list"></div>`:

```html
      <h2>Raw connect</h2>
      <div id="raw-connect" class="raw-connect">
        <div class="raw-form">
          <label>Task: <select id="raw-task-select"><option value="">(select task)</option></select></label>
          <input id="raw-host-input" type="text" value="127.0.0.1" size="16" placeholder="host">
          <input id="raw-port-input" type="number" min="1" max="65535" placeholder="port">
          <button id="raw-connect-btn" type="button">Connect</button>
        </div>
        <div id="raw-tabs" class="raw-tabs" role="tablist"></div>
        <div class="raw-view-row">
          <button type="button" id="raw-view-text" class="task-chip is-active">text</button>
          <button type="button" id="raw-view-hex" class="task-chip">hex</button>
          <span id="raw-counters" class="raw-counters"></span>
        </div>
        <pre id="raw-output" class="raw-output" aria-label="Raw connection output"></pre>
        <div class="raw-send-row">
          <input id="raw-send-input" type="text" size="48" placeholder="bytes to send">
          <select id="raw-newline">
            <option value="crlf">CRLF</option>
            <option value="lf">LF</option>
            <option value="none">none</option>
          </select>
          <label class="raw-hex-toggle"><input type="checkbox" id="raw-hex-input"> hex入力</label>
          <button id="raw-send-btn" type="button" disabled>Send</button>
          <button id="raw-close-btn" type="button" disabled>Close</button>
        </div>
      </div>
```

- [ ] **Step 2: Add the pane state and hooks**

In `webui/static/main.js`, near the other `#connections`-section helpers, add:

```js
// --- Raw connect pane -------------------------------------------------------
// One entry per open connection, keyed by the pane key wasm returned. A
// connection's bytes are kept as a chunk list capped at RAW_RING_BYTES: the
// output view is a debugging window, not a transcript, and an unbounded buffer
// on a chatty port would grow until the tab died.
const RAW_RING_BYTES = 256 * 1024;
const rawPanes = new Map(); // key -> {key, task, host, port, chunks, bytes, sent, open, note}
let rawActiveKey = null;
let rawViewMode = "text"; // "text" | "hex"

function rawPane(key) { return rawPanes.get(key) || null; }

function rawAppend(key, bytes) {
  const p = rawPane(key);
  if (!p) return;
  p.chunks.push(bytes);
  p.bytes += bytes.length;
  let total = p.chunks.reduce((n, c) => n + c.length, 0);
  while (total > RAW_RING_BYTES && p.chunks.length > 1) {
    total -= p.chunks.shift().length;
  }
  if (key === rawActiveKey) renderRawOutput();
}

function rawBytesOf(p) {
  const total = p.chunks.reduce((n, c) => n + c.length, 0);
  const out = new Uint8Array(total);
  let off = 0;
  for (const c of p.chunks) { out.set(c, off); off += c.length; }
  return out;
}

// Text view keeps newlines and tabs and replaces every other control byte with
// "." — the output is arbitrary bytes, and letting them through would let a
// remote service drive the page's rendering.
function rawRenderText(bytes) {
  const decoded = new TextDecoder("utf-8", { fatal: false }).decode(bytes);
  return decoded.replace(/[ --]/g, ".");
}

function rawRenderHex(bytes) {
  const lines = [];
  for (let i = 0; i < bytes.length; i += 16) {
    const slice = bytes.subarray(i, i + 16);
    const hex = Array.from(slice, (b) => b.toString(16).padStart(2, "0")).join(" ").padEnd(47, " ");
    const ascii = Array.from(slice, (b) => (b >= 0x20 && b < 0x7f ? String.fromCharCode(b) : ".")).join("");
    lines.push(`${i.toString(16).padStart(8, "0")}  ${hex}  ${ascii}`);
  }
  return lines.join("\n");
}

function renderRawOutput() {
  const out = document.getElementById("raw-output");
  const counters = document.getElementById("raw-counters");
  if (!out) return;
  const p = rawActiveKey ? rawPane(rawActiveKey) : null;
  if (!p) {
    out.textContent = "";
    if (counters) counters.textContent = "";
    return;
  }
  const bytes = rawBytesOf(p);
  out.textContent = rawViewMode === "hex" ? rawRenderHex(bytes) : rawRenderText(bytes);
  out.scrollTop = out.scrollHeight;
  if (counters) {
    counters.textContent = `${p.open ? "● open" : "○ closed"}  in ${p.bytes}B / out ${p.sent}B` +
      (p.note ? `  ${p.note}` : "");
  }
  const sendBtn = document.getElementById("raw-send-btn");
  const closeBtn = document.getElementById("raw-close-btn");
  if (sendBtn) sendBtn.disabled = !p.open;
  if (closeBtn) closeBtn.disabled = !p.open;
}

function renderRawTabs() {
  const host = document.getElementById("raw-tabs");
  if (!host) return;
  host.textContent = "";
  for (const p of rawPanes.values()) {
    const tab = document.createElement("button");
    tab.type = "button";
    tab.className = "raw-tab" + (p.key === rawActiveKey ? " is-active" : "") + (p.open ? "" : " is-closed");
    tab.textContent = `${p.host}:${p.port}`;
    tab.addEventListener("click", () => { rawActiveKey = p.key; renderRawTabs(); renderRawOutput(); });
    const drop = document.createElement("span");
    drop.className = "raw-tab-x";
    drop.textContent = "×";
    drop.addEventListener("click", async (ev) => {
      ev.stopPropagation();
      if (p.open) { try { await window.harness.rawClose(p.key); } catch (err) { appendCmdOutput(`rawClose: ${err.message}`); } }
      rawPanes.delete(p.key);
      if (rawActiveKey === p.key) rawActiveKey = rawPanes.size ? [...rawPanes.keys()][0] : null;
      renderRawTabs();
      renderRawOutput();
      refreshSnapshot();
    });
    tab.appendChild(drop);
    host.appendChild(tab);
  }
}

// Hooks invoked from wasm (see cli/raw_forward_wasm.go). Both are keyed by pane.
window.harness_rawData = (key, arr) => rawAppend(key, new Uint8Array(arr));
window.harness_rawClosed = (key, reason) => {
  const p = rawPane(key);
  if (!p) return;
  p.open = false;
  p.note = reason || "closed";
  renderRawTabs();
  renderRawOutput();
  refreshSnapshot(); // the registration is gone; the forward list should agree
};
```

- [ ] **Step 3: Wire the controls**

Still in `webui/static/main.js`, inside the same closure that owns `refreshSnapshot` / `appendCmdOutput`:

```js
  const rawTaskSelect = document.getElementById("raw-task-select");
  const rawConnectBtn = document.getElementById("raw-connect-btn");
  const rawSendInput  = document.getElementById("raw-send-input");
  const rawSendBtn    = document.getElementById("raw-send-btn");
  const rawCloseBtn   = document.getElementById("raw-close-btn");
  const rawHexInput   = document.getElementById("raw-hex-input");

  // renderRawTaskSelect mirrors renderFileTaskSelect: only Running/Detached
  // tasks can hold a forward, so only those are offered.
  function renderRawTaskSelect(tasks) {
    if (!rawTaskSelect) return;
    const prev = rawTaskSelect.value;
    rawTaskSelect.textContent = "";
    const none = document.createElement("option");
    none.value = "";
    none.textContent = "(select task)";
    rawTaskSelect.appendChild(none);
    for (const t of tasks || []) {
      if (t.status !== "running" && t.status !== "detached") continue;
      const opt = document.createElement("option");
      opt.value = t.id;
      opt.textContent = `${t.id.slice(0, 8)}… ${t.repo || ""}`.trim();
      rawTaskSelect.appendChild(opt);
    }
    rawTaskSelect.value = prev;
  }

  rawConnectBtn?.addEventListener("click", async () => {
    const task = rawTaskSelect.value;
    const host = document.getElementById("raw-host-input").value.trim();
    const port = parseInt(document.getElementById("raw-port-input").value, 10);
    if (!task || !host || !(port > 0 && port < 65536)) {
      appendCmdOutput("raw connect: task, host and port are required");
      return;
    }
    rawConnectBtn.disabled = true;
    try {
      const key = await window.harness.rawOpen(task, host, port);
      rawPanes.set(key, { key, task, host, port, chunks: [], bytes: 0, sent: 0, open: true, note: "" });
      rawActiveKey = key;
      renderRawTabs();
      renderRawOutput();
      refreshSnapshot(); // the new registration should appear in the forward list
    } catch (err) {
      appendCmdOutput(`raw connect error: ${err.message}`);
    } finally {
      rawConnectBtn.disabled = false;
    }
  });

  // hexToBytes accepts "48 65 6c" / "48656c" and rejects anything else, so a
  // typo sends nothing rather than sending garbage.
  function hexToBytes(s) {
    const clean = s.replace(/\s+/g, "");
    if (clean.length === 0 || clean.length % 2 !== 0 || /[^0-9a-fA-F]/.test(clean)) return null;
    const out = new Uint8Array(clean.length / 2);
    for (let i = 0; i < out.length; i++) out[i] = parseInt(clean.substr(i * 2, 2), 16);
    return out;
  }

  async function rawSendCurrent() {
    const p = rawActiveKey ? rawPane(rawActiveKey) : null;
    if (!p || !p.open) return;
    const text = rawSendInput.value;
    let bytes;
    if (rawHexInput.checked) {
      bytes = hexToBytes(text);
      if (!bytes) { appendCmdOutput("raw send: not valid hex"); return; }
    } else {
      const nl = document.getElementById("raw-newline").value;
      const suffix = nl === "crlf" ? "\r\n" : nl === "lf" ? "\n" : "";
      bytes = new TextEncoder().encode(text + suffix);
    }
    try {
      await window.harness.rawSend(p.key, bytes);
      p.sent += bytes.length;
      rawSendInput.value = "";
      renderRawOutput();
    } catch (err) {
      appendCmdOutput(`raw send error: ${err.message}`);
    }
  }

  rawSendBtn?.addEventListener("click", rawSendCurrent);
  rawSendInput?.addEventListener("keydown", (ev) => { if (ev.key === "Enter") rawSendCurrent(); });
  rawCloseBtn?.addEventListener("click", async () => {
    const p = rawActiveKey ? rawPane(rawActiveKey) : null;
    if (!p || !p.open) return;
    try { await window.harness.rawClose(p.key); } catch (err) { appendCmdOutput(`rawClose: ${err.message}`); }
    p.open = false;
    p.note = "closed locally";
    renderRawTabs();
    renderRawOutput();
    refreshSnapshot();
  });
  document.getElementById("raw-view-text")?.addEventListener("click", () => {
    rawViewMode = "text";
    document.getElementById("raw-view-text").classList.add("is-active");
    document.getElementById("raw-view-hex").classList.remove("is-active");
    renderRawOutput();
  });
  document.getElementById("raw-view-hex")?.addEventListener("click", () => {
    rawViewMode = "hex";
    document.getElementById("raw-view-hex").classList.add("is-active");
    document.getElementById("raw-view-text").classList.remove("is-active");
    renderRawOutput();
  });
```

Add `renderRawTaskSelect(snap.tasks);` next to the existing `renderFileTaskSelect(snap.tasks);` call in the snapshot handler (line ~459).

- [ ] **Step 4: Fix the now-false comment**

Above `renderForwardList` (line ~470) replace:

```
  // ... Starting a -L forward from the browser is out of
  // scope (a browser cannot bind a local listener) — list + kill is the
  // whole surface here.
```

with:

```
  // ... A browser still cannot bind a local listener, so there is no -L here;
  // the raw-connect pane below opens forwards whose client endpoint is this
  // page instead, and they appear in this list as `(in-process)`.
```

- [ ] **Step 5: Style it**

Append to `webui/static/style.css`, matching the existing dark palette and the ≤600px rules already in the file:

```css
.raw-connect { margin-top: 12px; }
.raw-form, .raw-send-row, .raw-view-row { display: flex; flex-wrap: wrap; gap: 6px; align-items: center; margin: 6px 0; }
.raw-tabs { display: flex; flex-wrap: wrap; gap: 4px; }
.raw-tab { background: #2a2a2a; color: #d4d4d4; border: 1px solid #3a3a3a; border-radius: 4px; padding: 2px 8px; cursor: pointer; }
.raw-tab.is-active { border-color: #6a9955; }
.raw-tab.is-closed { color: #808080; }
.raw-tab-x { margin-left: 6px; color: #808080; }
.raw-output {
  background: #1e1e1e; color: #d4d4d4; border: 1px solid #3a3a3a; border-radius: 4px;
  margin: 0; padding: 6px; max-height: 320px; overflow: auto;
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 12px;
  white-space: pre; tab-size: 8;
}
.raw-counters { color: #808080; font-size: 12px; }
@media (max-width: 600px) {
  .raw-form input, .raw-send-row input { flex: 1 1 100%; min-width: 0; }
  .raw-output { max-height: 220px; font-size: 11px; }
}
```

- [ ] **Step 6: Build and verify in a real browser**

Run: `make build` (rebuilds `webui/static/main.wasm`; WebUI/wasm changes hot-reload, so no server restart).

Then, against a dummy harness (see Task 8 step 1 for standing one up) with an echo listener on the runner host:

```bash
# on the runner host, in another shell
python3 -c "
import socketserver
class E(socketserver.BaseRequestHandler):
    def handle(self):
        while True:
            d=self.request.recv(4096)
            if not d: return
            self.request.sendall(d)
socketserver.TCPServer(('127.0.0.1',18410),E).serve_forever()"
```

- [ ] **Step 7: Playwright checks**

Drive the WebUI with the Playwright MCP tools at desktop width and at 390px, and confirm each:

1. The task select lists the running task; connecting to `127.0.0.1:18410` adds a tab and shows `● open`.
2. Typing `ping` into the send box and pressing Enter — **real keystrokes, not a DOM value assignment** — makes `ping` appear in the output view. Rendering is not the assertion; the input path is.
3. The `hex` toggle re-renders the same bytes as `00000000  70 69 6e 67 …  ping`.
4. The new forward appears in `#forward-list` with spec `(in-process) -> 127.0.0.1:18410` and dir `-L`.
5. `forward kill <id>` from `harness-cli` flips the tab to `○ closed` and the row disappears on the next poll.
6. Closing the tab with `×` removes the row from `#forward-list` on the next poll.
7. At 390px the pane's controls wrap and the page does not scroll horizontally.

- [ ] **Step 8: Commit**

```bash
git add webui/index.html webui/static/main.js webui/static/style.css
git commit -m "feat(webui): raw-connect pane — open forwards with the page as the endpoint"
```

---

## Task 7: TUI raw connect (`t`)

**Files:**
- Create: `tui/rawforward.go`
- Modify: `tui/app.go` (model field, `t` key, message handling, view)
- Test: `tui/rawforward_test.go`

**Interfaces:**
- Consumes: `OpenRawForward` / `RawConn` (Task 3); the app's long-lived `*cli.Client` in `a.client`.
- Produces: `type RawConnectModal`, `func DoStartRawForward(c *cli.Client, taskID, host string, port int, program *tea.Program) tea.Cmd`, messages `RawForwardOpenedMsg`, `RawForwardDataMsg`, `RawForwardClosedMsg`.

- [ ] **Step 1: Write the failing modal test**

Create `tui/rawforward_test.go`:

```go
package tui

import "testing"

func TestRawConnectModal_OpenCloseAndSpec(t *testing.T) {
	var m RawConnectModal
	if m.IsOpen() {
		t.Fatal("zero modal must be closed")
	}
	m.Open("4a1f0000000000000000000000000000")
	if !m.IsOpen() || m.TaskID() == "" {
		t.Fatal("Open must record the task and open")
	}
	m.SetSpec("127.0.0.1:6379")
	host, port, err := m.Target()
	if err != nil || host != "127.0.0.1" || port != 6379 {
		t.Fatalf("Target() = %s:%d err=%v", host, port, err)
	}
	m.SetSpec("garbage")
	if _, _, err := m.Target(); err == nil {
		t.Fatal("Target() must reject a spec that is not host:port")
	}
	m.Close()
	if m.IsOpen() {
		t.Fatal("Close must close")
	}
}

func TestRawConnectModal_RingCap(t *testing.T) {
	var m RawConnectModal
	m.Open("4a1f0000000000000000000000000000")
	big := make([]byte, rawTUIRingBytes+1024)
	for i := range big {
		big[i] = 'x'
	}
	m.AppendOutput(big)
	if got := len(m.Output()); got > rawTUIRingBytes {
		t.Fatalf("output ring = %d bytes, want <= %d", got, rawTUIRingBytes)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./tui/ -run TestRawConnectModal`
Expected: compile failure — `undefined: RawConnectModal`.

- [ ] **Step 3: Implement `tui/rawforward.go`**

```go
package tui

import (
	"context"
	"fmt"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/on-keyday/agent-harness/cli"
)

// rawTUIRingBytes caps the modal's output buffer. Same reasoning as the WebUI
// pane's ring: this is a debugging window, not a transcript.
const rawTUIRingBytes = 64 * 1024

// RawConnectModal prompts for a host:port and then shows the bytes coming back
// from a forward whose client endpoint is this TUI process. Mirrors
// PortForwardModal's shape (open/close/input/View) so the app's modal handling
// needs no new idiom.
type RawConnectModal struct {
	open   bool
	taskID string
	input  textinput.Model
	out    []byte
	live   bool
	note   string
}

func NewRawConnectModal() RawConnectModal {
	in := textinput.New()
	in.Placeholder = "host:port"
	in.CharLimit = 128
	return RawConnectModal{input: in}
}

func (m *RawConnectModal) IsOpen() bool   { return m.open }
func (m *RawConnectModal) TaskID() string { return m.taskID }
func (m *RawConnectModal) IsLive() bool   { return m.live }
func (m *RawConnectModal) Output() []byte { return m.out }

func (m *RawConnectModal) Open(taskID string) {
	if m.input.Placeholder == "" {
		*m = NewRawConnectModal()
	}
	m.open = true
	m.taskID = taskID
	m.live = false
	m.note = ""
	m.out = nil
	m.input.SetValue("")
	m.input.Focus()
}

func (m *RawConnectModal) Close() {
	m.open = false
	m.live = false
	m.input.Blur()
}

func (m *RawConnectModal) SetSpec(s string) { m.input.SetValue(s) }
func (m *RawConnectModal) Spec() string     { return m.input.Value() }

// Target parses the entered spec. Reuses the CLI parser so -W and the TUI cannot
// disagree about what a target looks like.
func (m *RawConnectModal) Target() (string, int, error) {
	return cli.ParseStdioForwardSpec(m.input.Value())
}

func (m *RawConnectModal) MarkLive(note string) { m.live = true; m.note = note }
func (m *RawConnectModal) MarkClosed(note string) { m.live = false; m.note = note }

// AppendOutput adds received bytes, trimming the front so the buffer stays
// bounded.
func (m *RawConnectModal) AppendOutput(b []byte) {
	m.out = append(m.out, b...)
	if len(m.out) > rawTUIRingBytes {
		m.out = append([]byte(nil), m.out[len(m.out)-rawTUIRingBytes:]...)
	}
}

func (m *RawConnectModal) Update(msg tea.Msg) (RawConnectModal, tea.Cmd) {
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return *m, cmd
}

func (m *RawConnectModal) View() string {
	head := fmt.Sprintf("raw connect — task %s", pfShortID(m.taskID))
	state := "enter host:port, Enter to connect"
	if m.live {
		state = "connected — type bytes, Enter sends (esc closes)"
	}
	if m.note != "" {
		state = m.note
	}
	body := string(m.out)
	return head + "\n" + state + "\n" + m.input.View() + "\n\n" + body
}

// RawForwardOpenedMsg reports a successful connect plus its registration id.
type RawForwardOpenedMsg struct {
	TaskID    string
	ForwardID uint64
}

// RawForwardDataMsg carries bytes received from the far end.
type RawForwardDataMsg struct{ Data []byte }

// RawForwardClosedMsg reports the connection ending, for any reason.
type RawForwardClosedMsg struct{ Reason string }

// rawSend holds the live connection for the single raw modal. The TUI shows one
// raw connection at a time (unlike the WebUI's tabs), so one slot is enough.
var rawLive *cli.RawConn

// DoStartRawForward opens the connection on the app's existing long-lived client
// — never a fresh Dial, matching every other Do* in this package — and pumps
// received bytes back as messages.
func DoStartRawForward(c *cli.Client, taskID, host string, port int, program *tea.Program) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		rc, err := cli.OpenRawForward(ctx, c, taskID, host, port, func(line string) {
			if program != nil {
				program.Send(RawForwardClosedMsg{Reason: line})
			}
		})
		if err != nil {
			return RawForwardClosedMsg{Reason: "raw connect: " + err.Error()}
		}
		rawLive = rc
		go func() {
			for {
				data, eof, rerr := rc.Recv(ctx)
				if len(data) > 0 && program != nil {
					program.Send(RawForwardDataMsg{Data: append([]byte(nil), data...)})
				}
				if eof || rerr != nil {
					if program != nil {
						program.Send(RawForwardClosedMsg{Reason: "connection closed"})
					}
					return
				}
			}
		}()
		return RawForwardOpenedMsg{TaskID: taskID, ForwardID: rc.ForwardID()}
	}
}

// SendRawLine writes the modal's current input to the live connection.
func SendRawLine(s string) error {
	if rawLive == nil {
		return fmt.Errorf("raw connect: not connected")
	}
	return rawLive.Send([]byte(s + "\r\n"))
}

// StopRawLive closes the live connection, if any.
func StopRawLive() {
	if rawLive != nil {
		_ = rawLive.Close()
		rawLive = nil
	}
}
```

- [ ] **Step 4: Wire it into the app**

In `tui/app.go`:

- add `rawModal RawConnectModal` to the model struct (near `forwardsModal`, line ~76) and `rawModal: NewRawConnectModal(),` to the constructor (line ~190).
- add a key handler beside the forward-start keys. The closest sibling is the `p` / `b` block at `tui/app.go:1208-1219`, which opens `PortForwardModal` for the selected task — copy its guard shape (`a.focus == focusTasks`, `a.tasks.SelectedID()`, a `WarnStyle` line when nothing is selected). Do **not** hang it off `f` / `ForwardsModal`: that modal is the registry listing, whose per-row action is kill.

```go
		if a.focus == focusTasks && !logsEditing && msg.String() == "t" {
			if t := a.selectedTask(); t != nil {
				a.rawModal.Open(t.ID)
			}
			return a, nil
		}
```

- handle the modal's keys before the generic key dispatch, mirroring how `forwardsModal` is handled at line ~784: `esc` closes (and calls `StopRawLive()`), `enter` either connects (`DoStartRawForward(a.client, ...)` when not live) or sends (`SendRawLine`), anything else goes to `a.rawModal.Update(msg)`.
- handle the three messages: `RawForwardOpenedMsg` → `a.rawModal.MarkLive(fmt.Sprintf("connected (fwd %d)", msg.ForwardID))`; `RawForwardDataMsg` → `a.rawModal.AppendOutput(msg.Data)`; `RawForwardClosedMsg` → `a.rawModal.MarkClosed(msg.Reason)`.
- render `a.rawModal.View()` in the same place the other modals are rendered.

Use `a.client` — never `cli.Dial` — matching every other `Do*` in this package.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./tui/ -run TestRawConnectModal -v && go test ./tui/`
Expected: PASS.

- [ ] **Step 6: Verify in a real TUI**

Against the dummy harness from Task 8 with the echo listener running: `bin/harness-tui`, select the running task, press `t`, type `127.0.0.1:18410`, Enter, then type `ping`, Enter, and confirm `ping` comes back in the modal. Confirm `bin/harness-cli forward ls` shows it as `(in-process)`, and that `esc` makes the row disappear.

- [ ] **Step 7: Commit**

```bash
git add tui/rawforward.go tui/rawforward_test.go tui/app.go
git commit -m "feat(tui): raw connect modal (t) over an in-process forward endpoint"
```

---

## Task 8: Integration test, dummy-harness E2E, and the full gate

**Files:**
- Modify: `integration/port_forward_test.go`

**Interfaces:**
- Consumes: everything above.
- Produces: no new API.

- [ ] **Step 1: Write the failing integration test**

Append to `integration/port_forward_test.go`, following the structure of the existing `TestLocalForwardRegisterListKill` (read it first — it stands up the harness, submits a task, and drives `cli` helpers directly):

```go
// TestRawForwardRoundTripListKill covers the whole in-process endpoint path with
// no socket on the client side: bytes reach an echo listener the runner dials,
// the registration is listable as (in-process), and a kill from a second client
// tears the connection down.
func TestRawForwardRoundTripListKill(t *testing.T) {
	// --- echo listener the runner will dial ---
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func() { defer conn.Close(); io.Copy(conn, conn) }()
		}
	}()
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	// --- harness + running task: copy the setup block from
	// TestLocalForwardRegisterListKill verbatim, ending with `taskID` and a
	// connected *cli.Client named `c`. ---

	rc, err := cli.OpenRawForward(context.Background(), c, taskID, "127.0.0.1", port, func(string) {})
	if err != nil {
		t.Fatalf("OpenRawForward: %v", err)
	}
	if err := rc.Send([]byte("ping")); err != nil {
		t.Fatalf("send: %v", err)
	}
	got := make([]byte, 0, 4)
	deadline := time.Now().Add(10 * time.Second)
	for len(got) < 4 && time.Now().Before(deadline) {
		data, eof, rerr := rc.Recv(context.Background())
		got = append(got, data...)
		if eof || rerr != nil {
			break
		}
	}
	if string(got) != "ping" {
		t.Fatalf("echo round-trip = %q, want \"ping\"", got)
	}

	forwards, err := cli.PortForwardList(context.Background(), serverCID, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found *protocol.PortForwardInfo
	for i := range forwards {
		if forwards[i].ForwardId == rc.ForwardID() {
			found = &forwards[i]
		}
	}
	if found == nil {
		t.Fatalf("raw forward %d absent from the listing (%d entries)", rc.ForwardID(), len(forwards))
	}
	if found.ClientEndpoint != protocol.ClientEndpointKind_InProcess {
		t.Fatalf("listed endpoint = %v, want InProcess", found.ClientEndpoint)
	}
	if spec := cli.PortForwardSpecString(found); !strings.HasPrefix(spec, "(in-process) -> ") {
		t.Fatalf("spec = %q, want an (in-process) prefix", spec)
	}

	if err := cli.KillPortForward(context.Background(), serverCID, rc.ForwardID()); err != nil {
		t.Fatalf("kill: %v", err)
	}
	// The kill must reach the data stream, not just the registry.
	killDeadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(killDeadline) {
		if _, eof, rerr := rc.Recv(context.Background()); eof || rerr != nil {
			return
		}
	}
	t.Fatal("data stream still live 10s after the forward was killed")
}
```

- [ ] **Step 2: Run it to verify it fails, then passes**

Run: `go test -tags integration ./integration/ -run TestRawForwardRoundTripListKill -count=1 -v`
Expected: FAIL before Tasks 1-3 land; PASS now.

- [ ] **Step 3: Dummy-harness E2E with real binaries**

Unit and integration tests do not exercise the built binaries. Stand up a throwaway instance and drive `-W`:

```bash
make build
scripts/dummy-harness.sh up --agent fake --detach --name rawfwd
eval "$(scripts/dummy-harness.sh env --name rawfwd)"
# submit an interactive session so a task is Running, then:
printf 'ping' | bin/harness-cli forward <task-id> -W 127.0.0.1:18410   # echo listener from Task 6 step 6
```

Expected: `ping` on stdout. Then confirm:

- `bin/harness-cli forward ls` shows the forward as `(in-process) -> 127.0.0.1:18410` **while it is running** (use a second shell; the `-W` process holds the foreground).
- `bin/harness-cli forward kill <id>` from the second shell makes the `-W` process exit.
- `bin/harness-cli forward <task> -W 127.0.0.1:18410 -L 18500:127.0.0.1:18410` exits 2 with the mutual-exclusion message.
- Teardown: `scripts/dummy-harness.sh down --name rawfwd`.

- [ ] **Step 4: Run the full gate**

Run:

```bash
make check
make wasm-check
make vet
make test
go test -tags integration ./integration/... -count=1 -timeout 600s
scripts/wire-skew-check.sh
```

Expected: all PASS. `make check` / `wasm-check` use explicit package patterns rather than `./...`, which is why they are run instead of a single `go build ./...`.

- [ ] **Step 5: Commit**

```bash
git add integration/port_forward_test.go
git commit -m "test(integration): raw forward round-trip, listing and kill"
```

---

## Rollout note (not a task step)

Both changed formats are client↔server; no runner-facing message changed, so the runner fleet does not need the server-first-then-runners restart dance that `.bgn` changes normally force. Update the server and the client binaries together.

---

## Self-Review

**Spec coverage:**

| Spec section | Task |
| --- | --- |
| Schema delta (enum + 2 fields) | 1 |
| Server: store field, reject `remote × in_process`, `portForwardInfo` | 2 |
| Display `(in-process)`, `DIR` unchanged (decision 6) | 2 |
| Client shared layer (`RawConn`, `OpenRawForward`, register-after-open, control watcher) | 3 |
| CLI `-W` + `ProxyCommand` composition | 4 |
| WebUI wasm slots + generation guard + bindings | 5 |
| WebUI pane: `<pre>` not xterm, text/hex, newline selector, 256 KiB ring, dark + 390px | 6 |
| TUI entry point | 7 |
| Failure-mode table (kill, client vanishes, registration failure, `remote × in_process`) | 2, 3, 8 |
| Verification (protocol, server, client, integration, dummy-harness, Playwright, make gates) | 1-8 |
| Rollout | Rollout note |
| Out of scope items | absent by construction |

**Deviation from the spec, resolved here:** the spec's CLI section said `-W` "may be combined" with `-L`/`-R`. Task 4 makes them mutually exclusive instead, because `-W` owns the foreground and exits with its peer while `-L`/`-R` are long-lived listeners — and because `ssh -W` itself implies `ClearAllForwardings`. The spec must be updated to match before this plan is executed.

**Placeholder scan:** no TBD/TODO; every code step carries the actual code. Three steps deliberately point at an existing sibling to copy rather than inlining it (`cmd/harness-cli/forward_test.go`'s routing harness in Task 4 step 7, the integration setup block in Task 8 step 1, and the modal-key wiring in Task 7 step 4) — in each case the sibling is named with a file and a symbol, and inlining it would mean guessing at helpers not read during planning.

**Type consistency:** `OpenRawForward(ctx, c, taskIDHex, host, port, logf) (*RawConn, error)`, `RawConn.Send/Recv/Close/ForwardID`, `ParseStdioForwardSpec(s) (string, int, error)`, `OpenRawPane/SendRawPane/CloseRawPane`, and the JS hook names `harness_rawData` / `harness_rawClosed` are used identically in Tasks 3-7. `rawTUIRingBytes` (TUI) and `RAW_RING_BYTES` (JS) are deliberately different constants in different languages with different caps (64 KiB vs 256 KiB), matching each surface's viewport.
