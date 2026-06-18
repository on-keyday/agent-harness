# Unified Connection Identity via ClientHello (P1b) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `ClientHello` the single connection-identity handshake — agents present `runner_id/task_id/auth_ticket` as a `kind==agent` conditional field, validated by the existing agentboard `Registry` — and delete the now-redundant `AgentBridgeHello`.

**Architecture:** One `peer.Conn` already carries both wire app-kinds (`task_control`=0x41, `agent_message`=0x44). We move identity assertion to `ClientHello` (0x41), reuse `Registry.Validate` + `Board.Attach` + `ConnState` unchanged, and have `handleClientHello` populate the per-connID `agentConn` that the agentboard handlers already read. The agentboard's own hello message family is removed.

**Tech Stack:** Go; brgen schema (`.bgn` → generated `message.go`/`agentboard.go` via `make protoregen`); objtrsf peer/trsf transport.

**Spec:** `docs/superpowers/specs/2026-06-18-task-control-principal-identity-design.md`

---

## Spec Problem-statement coverage (read before starting)

The spec's Problem statement has two bullets. This plan must address BOTH:
1. Task-control path is anonymous (no verifiable identity) → Tasks 1, 3, 5.
2. Two separate asymmetric hellos exist needlessly → Tasks 1, 3, 4 (consolidation; `AgentBridgeHello` deleted).

Non-goal (do NOT implement): per-resource gating, task lineage. P1 records/validates identity only.

## Hard preconditions (apply to every task)

- **Worktree routing trap:** This session's cwd is a harness worktree. `git`/`Edit`/`Write` with absolute paths under `/home/kforfk/workspace/remote-agent-harness/<rel>` route to the PARENT repo, not this worktree. Operate with paths relative to this worktree root (cwd), and verify `git rev-parse --abbrev-ref HEAD` shows `harness/0f0d4dd6...` before committing. (See implementation-pitfalls Pitfall 8.)
- **Build hygiene:** compile-check with `go build ./...` (writes no binary) or `make check`. NEVER bare `go build ./cmd/<x>/` (drops a binary into the worktree). `go test ./...` self-cleans.
- **Schema is source of truth:** every wire byte must be in the `.bgn`. Do not carry bytes by convention.

## File structure

| File | Responsibility | Tasks |
|---|---|---|
| `runner/protocol/message.bgn` | `ClientKind.agent`, `AgentInfo`, `ClientHello` conditional, `ClientHelloStatus` variants | 1 |
| `agentboard/agentboard.bgn` | delete `AgentBridgeHello`/`*Response`/`AgentMessageKind.hello(_response)` | 1 |
| `runner/protocol/message.go`, `agentboard/agentboard.go` | regenerated (never hand-edit) | 1 |
| `server/idconv.go` (new) | `protocol.RunnerID/TaskID` → `agentboard.RunnerID/TaskID` converters | 2 |
| `server/agent_handler.go` | remove `agentHandleHello` + Hello case; add `establishAgentIdentity` hook impl | 3 |
| `server/task_handler.go` | `handleClientHello` validates + attaches via hook; `OnAgentHello` field | 3 |
| `server/server.go` | wire `OnAgentHello` into `TaskHandler` | 3 |
| `cli/agent/conn.go` | `ConnectAgent` sends `ClientHello{agent}`, waits `ClientHelloResponse`; drop client converters | 4 |
| `cli/hello.go` | agent-aware `SayHello` variant | 5 |
| `cmd/harness-cli/main.go` | task-control commands use the agent-aware hello | 5 |
| `agentboard/e2e_test.go` | update hello helper to `ClientHello` | 6 |
| `server/taskstore.go`, `server/wal.go` | `ResumedByKind` + `CreatorTaskID` fields + WAL persist/replay; `Resume`/`Create` record them | 7 |
| `cli/list.go` | render `resumed_by=` + `by=` in `ls` | 7 |
| `server/task_handler.go` | `principals` map at ClientHello + `lookupPrincipal` → creator plumbing | 7 |

---

## Task 1: Schema — ClientHello carries identity; delete AgentBridgeHello

**Files:**
- Modify: `runner/protocol/message.bgn` (around `:206-221`)
- Modify: `agentboard/agentboard.bgn` (`:15-49`, `:159-165`)
- Regenerate: `runner/protocol/message.go`, `agentboard/agentboard.go`

- [ ] **Step 1: Edit `runner/protocol/message.bgn`** — replace the `ClientKind`/`ClientHello`/`ClientHelloStatus` block (`:206-221`) with:

```
# ClientKind tags the kind of caller that opened a TaskControl session.
# `agent` is an in-task harness-cli that proves its identity via agent_info;
# operator surfaces use cli/tui/webui and carry no agent_info.
enum ClientKind:
    :u8
    unspecified
    cli
    tui
    webui
    agent

# AgentInfo is the per-task agent credential, mirroring the (now-removed)
# agentboard AgentBridgeHello triple. Validated server-side via the agentboard
# Registry. hostname is length-prefixed (0 len = absent).
format AgentInfo:
    runner_id :RunnerID
    task_id :TaskID
    auth_ticket :[16]u8
    hostname_len :u8
    hostname :[hostname_len]u8

format ClientHello:
    kind :ClientKind
    if kind == ClientKind.agent:
        agent_info :AgentInfo

enum ClientHelloStatus:
    :u8
    ok = "ok"
    bad_ticket
    unknown_task
    runner_mismatch

format ClientHelloResponse:
    status :ClientHelloStatus
```

- [ ] **Step 1b: Edit `runner/protocol/message.bgn` `format TaskInfo`** (`:381`) — add `resumed_by_kind` immediately after `origin_kind` (`:385`):

```
    origin_kind :ClientKind   # which kind of client FIRST created this task.
                              # Set at Create; sticky across resume.
    resumed_by_kind :ClientKind  # kind of the connection that performed the
                              # LATEST resume; Unspecified until first resumed.
                              # origin_kind stays the original creator.
    creator_task_id :TaskID   # task id of the AGENT principal that created this
                              # task (kind=agent connection). All-zero for
                              # operator-created tasks. Set at Create; unchanged
                              # on resume. Single parent link (lineage emerges by
                              # chasing it).
```

(All schema edits live in this one task — see the "don't split schema" rule.)

- [ ] **Step 2: Edit `agentboard/agentboard.bgn`** — remove the agent hello surface:
  - In `enum AgentMessageKind` (`:15`), delete the `hello` and `hello_response` members.
  - In `format AgentMessage`'s `match kind:` (`:162-163`), delete the `hello` and `hello_response` arms.
  - Delete `format AgentBridgeHello` (`:34-39`) and `format AgentBridgeHelloResponse` (`:48-49`).
  - KEEP `enum HelloStatus` (`:41-46`) — it remains the return type of `Registry.Validate`.

- [ ] **Step 3: Regenerate Go from both schemas**

Run: `make protoregen ARGS='runner/protocol/message.bgn agentboard/agentboard.bgn'`
Expected: completes without error; `git status` shows `runner/protocol/message.go` and `agentboard/agentboard.go` modified. (First run downloads ~20 MB brgen-kit, ~10 s.)

- [ ] **Step 4: Confirm the generated API shape** (these are consumed by later tasks)

Run: `grep -n "func (c \*ClientHello) AgentInfo\|func (.*ClientHello) SetAgentInfo\|type AgentInfo struct\|ClientKind_Agent\|ClientHelloStatus_BadTicket" runner/protocol/message.go`
Expected: `ClientHello.AgentInfo() *AgentInfo` accessor (nil when kind!=agent, mirroring `OpenExecRunnerRequest.X11()`), a `SetAgentInfo(AgentInfo) bool` setter (mirroring `SetX11`), `ClientKind_Agent`, `ClientHelloStatus_BadTicket/_UnknownTask/_RunnerMismatch` constants.

- [ ] **Step 5: Confirm the deletions broke the expected call sites only**

Run: `go build ./... 2>&1 | head -40`
Expected: compile errors ONLY in `server/agent_handler.go` (uses `AgentMessageKind_Hello`, `AgentBridgeHello`, `agentHandleHello`), `cli/agent/conn.go` (builds `AgentBridgeHello`), and possibly `agentboard/e2e_test.go`. These are fixed in Tasks 3, 4, 6. No other package should reference the deleted symbols.

- [ ] **Step 6: Commit** (generated code + schema together)

```bash
git add runner/protocol/message.bgn agentboard/agentboard.bgn runner/protocol/message.go agentboard/agentboard.go
git commit -m "feat(schema): ClientHello carries agent identity; remove AgentBridgeHello"
```

---

## Task 2: Server-side protocol→agentboard ID converters

The client used to convert IDs before sending `AgentBridgeHello`. Now the wire carries `protocol.RunnerID/TaskID` in `ClientHello`, so the SERVER converts. `agentboard` must NOT import `protocol` (it deliberately defines its own IDs), so the converters live in `server`.

**Files:**
- Create: `server/idconv.go`
- Test: `server/idconv_test.go`

- [ ] **Step 1: Write the failing test** — `server/idconv_test.go`:

```go
package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestBoardRunnerIDFromProto(t *testing.T) {
	var p protocol.RunnerID
	p.SetTransport([]byte("ws"))
	p.SetIpAddr([]byte{127, 0, 0, 1})
	p.Port = 8539
	p.UniqueNumber = 42

	got := boardRunnerIDFromProto(p)
	if string(got.Transport) != "ws" || len(got.IpAddr) != 4 || got.Port != 8539 || got.UniqueNumber != 42 {
		t.Fatalf("runner id round-trip mismatch: %+v", got)
	}
}

func TestBoardTaskIDFromProto(t *testing.T) {
	var p protocol.TaskID
	p.Id = [16]byte{1, 2, 3}
	got := boardTaskIDFromProto(p)
	if got.Id != p.Id {
		t.Fatalf("task id mismatch: %x != %x", got.Id, p.Id)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./server/ -run 'TestBoard.*FromProto' -v`
Expected: FAIL — `boardRunnerIDFromProto`/`boardTaskIDFromProto` undefined.

- [ ] **Step 3: Implement** — `server/idconv.go`:

```go
package server

import (
	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// boardRunnerIDFromProto converts a wire protocol.RunnerID (as carried in
// ClientHello.AgentInfo) to the agentboard.RunnerID the Registry/Board key on.
// Field-for-field copy; the two are structurally identical but distinct Go
// types (agentboard does not import protocol). Mirrors the former client-side
// protoToBoardRunnerID in cli/agent/conn.go.
func boardRunnerIDFromProto(p protocol.RunnerID) agentboard.RunnerID {
	var out agentboard.RunnerID
	out.SetTransport(p.Transport)
	out.SetIpAddr(p.IpAddr)
	out.Port = p.Port
	out.UniqueNumber = p.UniqueNumber
	return out
}

func boardTaskIDFromProto(p protocol.TaskID) agentboard.TaskID {
	var out agentboard.TaskID
	out.Id = p.Id
	return out
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./server/ -run 'TestBoard.*FromProto' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add server/idconv.go server/idconv_test.go
git commit -m "feat(server): protocol->agentboard ID converters for ClientHello identity"
```

---

## Task 3: Server — establish identity at ClientHello; remove agentboard hello path

**Files:**
- Modify: `server/agent_handler.go` (`:59-114` — remove Hello case + `agentHandleHello`; add `establishAgentIdentity`)
- Modify: `server/task_handler.go` (`:265-288` `handleClientHello`; add `OnAgentHello` field to the `TaskHandler` struct near `:100-111`)
- Modify: `server/server.go` (where `TaskHandler` is constructed — wire `OnAgentHello`)

- [ ] **Step 1: In `server/agent_handler.go`, delete the Hello case and `agentHandleHello`.**
  - In `handleAgentMessage` (`:69-86`), remove the line `case agentboard.AgentMessageKind_Hello: s.agentHandleHello(conn, ac, msg.Hello())`.
  - Delete the whole `func (s *Server) agentHandleHello(...)` (`:98-114`).

- [ ] **Step 2: Add the identity-establishment method** in `server/agent_handler.go` (it produces the per-connID `agentConn` that every other agent handler reads via `ac.helloed`/`ac.state`):

```go
// establishAgentIdentity validates an agent's credential (from ClientHello) and,
// on success, attaches the per-connID agentConn used by every agentboard
// handler (ac.helloed gate + ac.state.Identity()). Returns the agentboard
// HelloStatus. Reuses Registry.Validate + Board.Attach unchanged — this is the
// single place agent identity is established, for both task-control ops and
// agentboard messaging on the same connection.
func (s *Server) establishAgentIdentity(conn ConnHandle, info *protocol.AgentInfo) agentboard.HelloStatus {
	if s.Board == nil {
		return agentboard.HelloStatusOk // attribution-only degrade (test wiring)
	}
	rid := boardRunnerIDFromProto(info.RunnerId)
	tid := boardTaskIDFromProto(info.TaskId)
	status := s.Board.Registry().Validate(rid, tid, info.AuthTicket)
	if status == agentboard.HelloStatusOk {
		ac := s.getOrCreateAgentConn(conn)
		ac.helloed = true
		ac.state = s.Board.Attach(rid, tid, string(info.Hostname))
	}
	return status
}
```

Add `"github.com/on-keyday/agent-harness/runner/protocol"` to the imports if not present.

- [ ] **Step 3: Add `OnAgentHello` to the `TaskHandler` struct** in `server/task_handler.go` (near the `clientKinds` field, `:109-110`):

```go
	// OnAgentHello validates+establishes an agent principal for a ClientHello
	// with kind==agent. Implemented by Server (Validate + Board.Attach). nil =>
	// identity not established (e.g. minimal test wiring); ClientHello falls
	// back to attribution-only with status Ok.
	OnAgentHello func(conn ConnHandle, info *protocol.AgentInfo) protocol.ClientHelloStatus
```

Add the `protocol` import if needed (it is already imported in this file).

- [ ] **Step 4: Rewrite `handleClientHello`** in `server/task_handler.go` (`:265-288`):

```go
	case protocol.TaskControlKind_ClientHello:
		hello := req.ClientHello()
		if hello == nil {
			slog.Error("TaskHandler: ClientHello variant is nil")
			return
		}
		cid := conn.ConnectionID().String()
		slog.Info("client hello", "kind", hello.Kind.String(), "cid", cid)

		status := protocol.ClientHelloStatus_Ok
		if hello.Kind == protocol.ClientKind_Agent {
			if info := hello.AgentInfo(); info != nil && h.OnAgentHello != nil {
				status = h.OnAgentHello(conn, info)
			}
		}

		// Record this connection's kind for task-origin attribution
		// (Submit / OpenInteractive look it up). Only record on success so a
		// rejected agent does not get attributed as kind=agent.
		if status == protocol.ClientHelloStatus_Ok {
			h.clientKindsMu.Lock()
			if h.clientKinds == nil {
				h.clientKinds = make(map[string]protocol.ClientKind)
			}
			h.clientKinds[cid] = hello.Kind
			h.clientKindsMu.Unlock()
		}

		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_ClientHello, RequestId: req.RequestId}
		resp.SetClientHello(protocol.ClientHelloResponse{Status: status})
		out := resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck
```

- [ ] **Step 5: Wire `OnAgentHello` where `TaskHandler` is constructed** in `server/server.go`.

First find the construction site:
Run: `grep -n "TaskHandler{" server/server.go`

Then add to that struct literal a field that maps the agentboard status to the protocol status:

```go
		OnAgentHello: func(conn ConnHandle, info *protocol.AgentInfo) protocol.ClientHelloStatus {
			return clientHelloStatusFromBoard(s.establishAgentIdentity(conn, info))
		},
```

And add the mapping helper in `server/agent_handler.go` (HelloStatus and ClientHelloStatus share the same member order: ok/bad_ticket/unknown_task/runner_mismatch):

```go
func clientHelloStatusFromBoard(s agentboard.HelloStatus) protocol.ClientHelloStatus {
	switch s {
	case agentboard.HelloStatusBadTicket:
		return protocol.ClientHelloStatus_BadTicket
	case agentboard.HelloStatusUnknownTask:
		return protocol.ClientHelloStatus_UnknownTask
	case agentboard.HelloStatusRunnerMismatch:
		return protocol.ClientHelloStatus_RunnerMismatch
	default:
		return protocol.ClientHelloStatus_Ok
	}
}
```

(If `s` is a `*Server` is not in scope at the construction literal, capture it from the enclosing function — confirm the variable name via the grep in Step 5.)

- [ ] **Step 6: Write a server test** — `server/agent_handler_clienthello_test.go`. Construct a `TaskHandler` with `OnAgentHello` wired to a real `Server`+`Board`, register a ticket, and assert the four outcomes. Use the existing test helpers in `server/*_test.go` for Board/Registry setup (grep `Board.Registry().Register` / `agentboard.New` in `server/*_test.go` and mirror).

```go
package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestClientHelloAgentIdentity(t *testing.T) {
	s := newTestServerWithBoard(t) // helper: see existing server tests; wires s.Board
	var ticket [16]byte
	ticket[0] = 0xAB
	rid := /* agentboard.RunnerID for the test */ testBoardRunnerID()
	tid := testBoardTaskID(0x11)
	s.Board.Registry().Register(rid, tid, ticket)

	mkInfo := func(tk [16]byte) *protocol.AgentInfo {
		info := &protocol.AgentInfo{AuthTicket: tk}
		info.RunnerId = protoRunnerIDFor(rid) // inverse of boardRunnerIDFromProto for the test
		info.TaskId = protocol.TaskID{Id: tid.Id}
		return info
	}

	if got := clientHelloStatusFromBoard(s.establishAgentIdentity(fakeConn(t), mkInfo(ticket))); got != protocol.ClientHelloStatus_Ok {
		t.Fatalf("valid ticket => %v, want Ok", got)
	}
	var bad [16]byte
	if got := clientHelloStatusFromBoard(s.establishAgentIdentity(fakeConn(t), mkInfo(bad))); got != protocol.ClientHelloStatus_BadTicket {
		t.Fatalf("wrong ticket => %v, want BadTicket", got)
	}
}
```

Adapt the helper names (`newTestServerWithBoard`, `fakeConn`, `testBoardRunnerID`, etc.) to whatever the existing `server` test suite already provides — grep first: `grep -rn "func newTest\|ConnHandle\b.*struct\|fakeConn\|func.*Board" server/*_test.go`. Do NOT invent a new mock if one exists.

- [ ] **Step 7: Run tests**

Run: `go test ./server/ -run 'TestClientHello|TestBoard.*FromProto' -v`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add server/agent_handler.go server/task_handler.go server/server.go server/agent_handler_clienthello_test.go
git commit -m "feat(server): establish agent identity at ClientHello; drop agentboard hello"
```

---

## Task 4: Client — agentboard ConnectAgent over ClientHello

**Files:**
- Modify: `cli/agent/conn.go` (`:126-140` remove converters; `:179-258` rework hello)

- [ ] **Step 1: Remove the now-unused client converters** `protoToBoardRunnerID`/`protoToBoardTaskID` (`cli/agent/conn.go:126-140`) — the client now sends `protocol` types directly.

- [ ] **Step 2: Replace the PSK→hello→wait block** (`:185-257`). Keep PSK exactly as-is. Change the control handler to watch for a `TaskControl` `ClientHelloResponse` instead of an `AgentMessage` `HelloResponse`, and send a `ClientHello` instead of `AgentBridgeHello`:

```go
	psk := cli.GetPSK()
	pskRespCh := make(chan appwire.PskAuthStatus, 1)
	helloRespCh := make(chan protocol.ClientHelloStatus, 1)

	pc.SetOnControl(func(kind appwire.AppKind, payload []byte) {
		switch kind {
		case appwire.AppKind_PskAuth:
			if len(payload) > 0 {
				select {
				case pskRespCh <- appwire.PskAuthStatus(payload[0]):
				default:
				}
			}
		case appwire.AppKind_TaskControl:
			var resp protocol.TaskControlResponse
			if err := resp.DecodeExact(payload); err != nil {
				return
			}
			if resp.Kind == protocol.TaskControlKind_ClientHello {
				if r := resp.ClientHello(); r != nil {
					select {
					case helloRespCh <- r.Status:
					default:
					}
				}
			}
		}
	})
	pc.Start(ctx)
```

Then PSK (unchanged, `:213-229`), then send the ClientHello:

```go
	hostname := cliopts.ResolveString(f.Hostname, "HARNESS_HOSTNAME")
	info := protocol.AgentInfo{RunnerId: rid, TaskId: tid, AuthTicket: ticket}
	info.SetHostname([]byte(hostname)) // 0-len when empty is fine
	hello := protocol.ClientHello{Kind: protocol.ClientKind_Agent}
	hello.SetAgentInfo(info) // discriminator (Kind) already set first
	req := protocol.TaskControlRequest{Kind: protocol.TaskControlKind_ClientHello}
	req.SetClientHello(hello)
	data := req.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
	if _, _, err := pc.Connection().SendMessage(data); err != nil {
		pc.Close()
		return nil, fmt.Errorf("send hello: %w", err)
	}

	select {
	case status := <-helloRespCh:
		if status != protocol.ClientHelloStatus_Ok {
			pc.Close()
			return nil, fmt.Errorf("hello rejected: %v", status)
		}
	case <-ctx.Done():
		pc.Close()
		return nil, ctx.Err()
	}
	return &Conn{pc: pc, taskID: tid, runnerID: rid}, nil
```

Confirm exact generated method names against `runner/protocol/message.go` (Task 1 Step 4): `SetAgentInfo`, `SetHostname`, `req.SetClientHello`, `resp.ClientHello()`. Remove the now-unused `agentboard`/hello imports if they become unused (let `go build` tell you).

- [ ] **Step 3: Compile-check**

Run: `go build ./cli/... && go vet ./cli/agent/`
Expected: builds clean. (No test here — exercised by the E2E in Task 6.)

- [ ] **Step 4: Commit**

```bash
git add cli/agent/conn.go
git commit -m "feat(cli/agent): agentboard ConnectAgent authenticates via ClientHello"
```

---

## Task 5: Client — agent-aware task-control hello

**Files:**
- Modify: `cli/hello.go`
- Modify: `cmd/harness-cli/main.go` (`:117,257,276,397` — the `SayHello(ClientKind_Cli)` sites)

- [ ] **Step 1: Add an agent-aware hello** in `cli/hello.go`. It sends `kind=agent` with `agent_info` when the env triple is present, else falls back to the supplied operator kind:

```go
// SayHelloAuto sends a ClientHello. When the in-task agent env (HARNESS_RUNNER_ID
// / HARNESS_TASK_ID / HARNESS_AUTH_TICKET) is present it announces kind=agent
// with the credential so the server can attribute and verify the principal;
// otherwise it announces the given operator kind (cli/tui/webui). Reuses the
// same resolution as the agentboard client (cli/cliopts).
func (c *Client) SayHelloAuto(ctx context.Context, operatorKind protocol.ClientKind) error {
	hello := protocol.ClientHello{Kind: operatorKind}
	if rid, err := cliopts.ResolveRunnerID(""); err == nil {
		if tid, err := cliopts.ResolveTaskID(""); err == nil {
			if ticket, err := cliopts.ResolveAuthTicket(); err == nil {
				info := protocol.AgentInfo{RunnerId: rid, TaskId: tid, AuthTicket: ticket}
				info.SetHostname([]byte(cliopts.ResolveString("", "HARNESS_HOSTNAME")))
				hello.Kind = protocol.ClientKind_Agent
				hello.SetAgentInfo(info)
			}
		}
	}
	return c.sendClientHello(ctx, hello)
}
```

Refactor the existing `SayHello` to share a `sendClientHello(ctx, protocol.ClientHello) error` (the round-trip + status check currently at `cli/hello.go:24-39`), so both entry points reuse it. Add `cli/cliopts` to imports.

- [ ] **Step 2: Point the existing task-control hello sites at `SayHelloAuto`** in `cmd/harness-cli/main.go` — replace `c.SayHello(ctx, protocol.ClientKind_Cli)` at the `submit` (`:117`), `interactive` (`:257`), `file` (`:276`), and `forward` (`:397`) sites with `c.SayHelloAuto(ctx, protocol.ClientKind_Cli)`. An operator CLI (no env ticket) still sends `cli`; an in-task agent sends `agent`.

- [ ] **Step 3: Compile-check**

Run: `go build ./... && go vet ./cli/ ./cmd/harness-cli/`
Expected: clean.

- [ ] **Step 4: Commit**

```bash
git add cli/hello.go cmd/harness-cli/main.go
git commit -m "feat(cli): task-control commands send agent identity when in-task"
```

> NOTE (decided, not deferred): `cancel`/`logs`/`prune`/`session attach` go through `cli.Cancel`/`cli.Logs`/etc. which dial without any hello today. Adding identity there requires threading a hello into each package function. For P1 — which gates nothing — their lack of attribution is harmless (they only name a task id), and the agentboard + task-creating paths (submit/interactive/file/forward) already carry identity. Adding hello to the no-hello commands is explicitly out of THIS plan's scope; revisit when enforcement (P-gating) lands and those commands must present a capability.

---

## Task 6: Update tests for the unified hello

**Files:**
- Modify: `agentboard/e2e_test.go` (`:113-205` hello helper)

- [ ] **Step 1: Update the e2e hello helper** (`agentboard/e2e_test.go:181-205`) to send a `ClientHello{kind=agent, agent_info}` (app-kind `TaskControl`) and await `ClientHelloResponse`, instead of `AgentMessage{Hello}`. This mirrors the production change in Task 4. The helper builds `protocol.AgentInfo` from the test's runner/task ids + ticket; map the awaited `protocol.ClientHelloStatus` back to the helper's existing `agentboard.HelloStatus` return (or change the helper's return type to `protocol.ClientHelloStatus` and update its 3 call sites at `:246,253,400`).

- [ ] **Step 2: Run the agentboard + server suites**

Run: `go test ./agentboard/ ./server/ -v 2>&1 | tail -40`
Expected: PASS. The `bad_ticket` test (`:380-401`) still asserts rejection — now via `ClientHelloStatus_BadTicket`.

- [ ] **Step 3: Commit**

```bash
git add agentboard/e2e_test.go
git commit -m "test(agentboard): e2e hello uses ClientHello identity path"
```

---

## Task 7: Attribution fields — `resumed_by_kind` + `creator_task_id`

Two attribution fields, sharing the same files (store/WAL/handler/render), done as
one task to avoid editing those files twice. Wire fields were added in Task 1
(Step 1b). Part A = `resumed_by_kind` (the `ClientKind` of the latest resumer;
`origin_kind` stays the original creator). Part B = `creator_task_id` (the agent
principal task id that created the task; zero for operator-created).

**Files:**
- Modify: `server/taskstore.go` (`:26` fields, `:131/:146` Create + WAL, `:200` Resume, `:560-570` replay)
- Modify: `server/wal.go` (`:39` WAL struct)
- Modify: `server/task_handler.go` (`:265` ClientHello principal map; `:335/:365` Create plumbing; `:1003` TaskInfo conv; `:342/:384` thread origin into resume; `:408`, `:503` Resume calls)
- Modify: `cli/list.go` (`:123` render both)
- Test: `server/taskstore_test.go`

### Part A — `resumed_by_kind`

- [ ] **Step 1: Write the failing test** — append to `server/taskstore_test.go`:

```go
func TestResumeRecordsResumedByKind(t *testing.T) {
	s := NewTaskStore(t.TempDir()) // match the existing constructor in this file
	id := s.Create("/repo", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, "runner1", protocol.RunnerSelector{}, nil)
	// drive to terminal so Resume is allowed
	s.Assign(id, "runner1", "/wt")
	s.Finish(id, 0, "") // match the existing terminal-transition helper name

	if _, err := s.Resume(id, "p2", nil, protocol.RunnerSelector{}, "runner1", protocol.ClientKind_Agent); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	got, _ := s.Get(id)
	if got.OriginKind != protocol.ClientKind_Cli {
		t.Fatalf("origin_kind should stay cli, got %v", got.OriginKind)
	}
	if got.ResumedByKind != protocol.ClientKind_Agent {
		t.Fatalf("resumed_by_kind should be agent, got %v", got.ResumedByKind)
	}
}
```

Adapt `NewTaskStore`/`Finish` to the exact constructor + terminal-transition helpers already used in `server/taskstore_test.go` (grep first: `grep -n "func NewTaskStore\|func (s \*TaskStore) Finish\|Succeed\|MarkTerminal" server/taskstore*.go`).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./server/ -run TestResumeRecordsResumedByKind -v`
Expected: FAIL — `Resume` takes 6 args / `ResumedByKind` undefined.

- [ ] **Step 3: Add the field** to `TaskEntry` in `server/taskstore.go` (after `OriginKind`, `:26`):

```go
	// ResumedByKind records the ClientKind of the connection that performed the
	// most recent Resume. Unspecified until the task is first resumed.
	// OriginKind stays the original creator's kind.
	ResumedByKind protocol.ClientKind
```

- [ ] **Step 4: Add the WAL field** in `server/wal.go` (after `OriginKind`, `:39`):

```go
	ResumedByKind uint8 `json:"resumed_by_kind,omitempty"`
```

- [ ] **Step 5: Change `Resume` signature + body** in `server/taskstore.go` (`:200`):
  - Signature: add a trailing `resumerKind protocol.ClientKind` param.
  - In the body where the entry is reset, set `entry.ResumedByKind = resumerKind`.
  - Where Resume writes its WAL event, set `ResumedByKind: uint8(resumerKind)` on that event (find the resume WAL append in this function; mirror how Create writes `OriginKind` at `:146`).
  - In the WAL **replay** resume branch (`:614`), set `existing.ResumedByKind = protocol.ClientKind(ev.ResumedByKind)`. In the create-replay branch (`:570`), add `ResumedByKind: protocol.ClientKind(ev.ResumedByKind)` (legacy entries → 0 = Unspecified).

- [ ] **Step 6: Thread the resumer kind through the handlers** in `server/task_handler.go`:
  - `handleSubmit` (`:341-342`): change `return h.handleSubmitResume(req)` → `return h.handleSubmitResume(req, origin)` and update the func signature `handleSubmitResume(req *protocol.SubmitRequest, origin protocol.ClientKind)` (`:384`).
  - In `handleSubmitResume`, the `Tasks.Resume(...)` call (`:408`) gains a trailing `origin` arg.
  - In `handleOpenInteractive`, the resume `Tasks.Resume(...)` call (`:503`) gains a trailing `origin` arg (origin is already a param here).

- [ ] **Step 7: Copy the field into the wire TaskInfo** in `server/task_handler.go` (`:999-1003`), alongside `OriginKind: t.OriginKind`:

```go
		ResumedByKind: t.ResumedByKind,
```

- [ ] **Step 8: Render in `ls`** in `cli/list.go` (`:123-128`). Add a `resumed_by` segment that is blank when Unspecified, reusing the `originStr` helper (`:152`):

```go
		resumedBy := ""
		if t.ResumedByKind != protocol.ClientKind_Unspecified {
			resumedBy = "  resumed_by=" + originStr(t.ResumedByKind)
		}
```

Then insert `%s` for `resumedBy` into the `Fprintf` format string + args at the appropriate position (after the `from=%s%s` group).

- [ ] **Step 9: Run the test + store suite**

Run: `go test ./server/ -run 'TestResume|TestResumeRecordsResumedByKind' -v && go build ./...`
Expected: PASS + clean build.

- [ ] **Step 10: Commit Part A**

```bash
git add server/taskstore.go server/wal.go server/task_handler.go cli/list.go server/taskstore_test.go
git commit -m "feat(attribution): record resumed_by_kind; origin_kind stays original creator"
```

### Part B — `creator_task_id`

- [ ] **Step 11: Write the failing test** — append to `server/taskstore_test.go`:

```go
func TestCreateRecordsCreatorTaskID(t *testing.T) {
	s := NewTaskStore(t.TempDir()) // match the constructor used in this file
	var creator protocol.TaskID
	creator.Id = [16]byte{0xAA, 0xBB}

	id := s.Create("/repo", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent, creator, "runner1", protocol.RunnerSelector{}, nil)
	got, _ := s.Get(id)
	if got.CreatorTaskID.Id != creator.Id {
		t.Fatalf("creator_task_id = %x, want %x", got.CreatorTaskID.Id, creator.Id)
	}

	// operator create => zero creator
	var zero protocol.TaskID
	id2 := s.Create("/repo", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, zero, "runner1", protocol.RunnerSelector{}, nil)
	got2, _ := s.Get(id2)
	if got2.CreatorTaskID.Id != ([16]byte{}) {
		t.Fatalf("operator creator should be zero, got %x", got2.CreatorTaskID.Id)
	}

	// resume must NOT change creator
	s.Assign(id, "runner1", "/wt")
	s.Finish(id, 0, "") // match the terminal-transition helper used in Part A
	if _, err := s.Resume(id, "p2", nil, protocol.RunnerSelector{}, "runner1", protocol.ClientKind_Agent); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	got3, _ := s.Get(id)
	if got3.CreatorTaskID.Id != creator.Id {
		t.Fatalf("resume changed creator: %x", got3.CreatorTaskID.Id)
	}
}
```

- [ ] **Step 12: Run test to verify it fails**

Run: `go test ./server/ -run TestCreateRecordsCreatorTaskID -v`
Expected: FAIL — `Create` arity / `CreatorTaskID` undefined.

- [ ] **Step 13: Store layer** in `server/taskstore.go`:
  - Add field to `TaskEntry` (after `ResumedByKind`):
    ```go
    // CreatorTaskID is the task id of the agent principal that created this
    // task (kind=agent connection). All-zero for operator-created tasks. Set
    // at Create; never changed on resume. Single parent link.
    CreatorTaskID protocol.TaskID
    ```
  - `Create` (`:123`): add a `creatorTaskID protocol.TaskID` param (after `origin`); set `CreatorTaskID: creatorTaskID` in the `TaskEntry` literal (`:131`) and the WAL event (`:146`) as `CreatorTaskID: hex.EncodeToString(creatorTaskID.Id[:])` (only meaningful when non-zero; the omitempty hex string is fine for zero — it encodes 32 zeros, so guard: write "" when zero to keep WAL clean). Use:
    ```go
    creatorHex := ""
    if creatorTaskID.Id != ([16]byte{}) {
        creatorHex = hex.EncodeToString(creatorTaskID.Id[:])
    }
    ```
    and set `CreatorTaskID: creatorHex` on the WAL event.
  - Replay (`:565-570`): in the create-replay branch set `CreatorTaskID: taskIDFromHexLenient(ev.CreatorTaskID)` where the helper decodes the hex into a `protocol.TaskID` (empty/invalid → zero). Add that small helper next to the existing hex handling in this file.

- [ ] **Step 14: WAL struct** in `server/wal.go` (after `ResumedByKind`):

```go
	CreatorTaskID string `json:"creator_task_id,omitempty"`
```

- [ ] **Step 15: Run the store test**

Run: `go test ./server/ -run TestCreateRecordsCreatorTaskID -v`
Expected: PASS (after also updating the other `Create` call sites in the next step so the package compiles).

- [ ] **Step 16: Record the principal at ClientHello + plumb into Create** in `server/task_handler.go`:
  - Add a principals map to `TaskHandler` (near `clientKinds`, `:109`):
    ```go
    // principals maps connID → the agent principal's task id, recorded on a
    // successful kind=agent ClientHello. Used to stamp creator_task_id on
    // tasks the agent creates. Absent => operator (zero creator).
    principals map[string]protocol.TaskID
    ```
  - In `handleClientHello` (rewritten in Task 3), after a successful `kind=agent` hello, record it:
    ```go
    if status == protocol.ClientHelloStatus_Ok && hello.Kind == protocol.ClientKind_Agent {
        if info := hello.AgentInfo(); info != nil {
            h.clientKindsMu.Lock()
            if h.principals == nil {
                h.principals = make(map[string]protocol.TaskID)
            }
            h.principals[cid] = info.TaskId
            h.clientKindsMu.Unlock()
        }
    }
    ```
  - Add a lookup (near `lookupClientKind`, `:115`):
    ```go
    func (h *TaskHandler) lookupPrincipal(connID string) protocol.TaskID {
        h.clientKindsMu.Lock()
        defer h.clientKindsMu.Unlock()
        return h.principals[connID]
    }
    ```
  - At the `Submit` dispatch (`:137-138`) and `OpenInteractive` dispatch (`:210-211`), resolve the creator and pass it through:
    ```go
    creator := h.lookupPrincipal(conn.ConnectionID().String())
    ```
    Add a `creator protocol.TaskID` param to `handleSubmit` / `handleOpenInteractive` and pass `creator` to their `h.Tasks.Create(...)` calls (`:365`, `:517`). (Resume branches do NOT set creator — it stays from the original Create.)

- [ ] **Step 17: Wire field** in `server/task_handler.go` TaskInfo conversion (`:1003`, alongside `OriginKind`/`ResumedByKind`):

```go
		CreatorTaskID: t.CreatorTaskID,
```

- [ ] **Step 18: Render in `ls`** in `cli/list.go` (`:123`), next to the `resumed_by` segment from Part A:

```go
		createdBy := ""
		if t.CreatorTaskID.Id != ([16]byte{}) {
			createdBy = "  by=" + hex.EncodeToString(t.CreatorTaskID.Id[:])[:8]
		}
```

Insert `%s` for `createdBy` into the `Fprintf` format + args (after the `resumed_by` segment). Ensure `encoding/hex` is imported in `cli/list.go`.

- [ ] **Step 19: Build + run all server/cli tests**

Run: `go test ./server/ ./cli/... && go build ./...`
Expected: PASS + clean build.

- [ ] **Step 20: Commit Part B**

```bash
git add server/taskstore.go server/wal.go server/task_handler.go cli/list.go server/taskstore_test.go
git commit -m "feat(attribution): record creator_task_id (agent principal that created the task)"
```

---

## Task 8: Full verification

- [ ] **Step 1: Compile-check everything**

Run: `make check`
Expected: webui-build + `go build ./...` succeed (this also catches TUI/WebUI, which call `SayHello`/identity paths — confirm they still build; if a TUI/WebUI hello site needs `SayHelloAuto`, it would surface here as a behavior question, but operator surfaces intentionally keep operator kinds).

- [ ] **Step 2: wasm-check**

Run: `make wasm-check`
Expected: `GOOS=js GOARCH=wasm go build ./cli/... ./cmd/harness-webui-wasm/` succeeds.

- [ ] **Step 3: vet + full test**

Run: `make vet && make test`
Expected: both pass. (`make test` = `go test ./...`.)

- [ ] **Step 4: Confirm no dangling references to deleted symbols**

Run: `grep -rn "AgentBridgeHello\|AgentMessageKind_Hello\|agentHandleHello\|protoToBoardRunnerID" --include=*.go . | grep -v _gen`
Expected: no matches (all removed).

- [ ] **Step 5: Manual E2E (attribution + resume + rejection)** — with a server + runner running (per the project's restart-all flow): submit a task from inside a runner-spawned task and from an operator CLI; confirm `harness-cli ls` shows origin `agent` vs `cli` respectively. Then `submit --resume <cli-created-id>` from inside an agent task and confirm `ls` shows `from=cli  resumed_by=agent` (origin sticky, resumer recorded), and that an agent-submitted task shows `by=<agent-short-id>` (creator link) while an operator-submitted task shows no `by=`. Confirm an agentboard `harness-cli agent send` still works end-to-end. (Coordinate the server restart + runner rebuild per the spec's rollout: server and all hello-speaking clients must run the new build together.)

- [ ] **Step 6: Final commit (if any verification fixups were needed)**

```bash
git add -A
git commit -m "chore(auth): verification fixups for unified ClientHello identity"
```

---

## Self-review checklist (completed by plan author)

- **Spec coverage:** schema (T1), task-control identity (T1/T3/T5), agentboard consolidation + AgentBridgeHello deletion (T1/T3/T4), validation/rejection statuses (T1/T3/T6), attribution agent-vs-operator (T3 origin via clientKinds), resume attribution `resumed_by_kind` (T1 schema + T7 Part A), creator link `creator_task_id` (T1 schema + T7 Part B; principal recorded at the T3 ClientHello), Board==nil degrade (T3), rollout/verify (T8). The "no-hello commands" item is explicitly scoped OUT with rationale (T5 note) — matches spec non-goal posture.
- **Placeholder scan:** test helper names in T3/T6 are marked "adapt to existing suite, grep first" with the exact grep — this is a real instruction, not a TBD, because inventing a mock when one exists violates implementation-pitfalls; the generated-API names (T4/T5) are confirmed against the X11 precedent and re-checked in T1 Step 4.
- **Type consistency:** `boardRunnerIDFromProto`/`boardTaskIDFromProto` (T2) used in T3; `establishAgentIdentity` (T3) returns `agentboard.HelloStatus`, mapped by `clientHelloStatusFromBoard` (T3); `OnAgentHello` returns `protocol.ClientHelloStatus`; `SayHelloAuto`/`sendClientHello` (T5) names consistent across steps.
