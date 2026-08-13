# Task-scoped capabilities Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give every capability grant a target-task scope, enforce it on every
request that names a task, and let the operator re-grant caps + scope on a live
task without restarting it.

**Architecture:** A `TaskScope` (base `subtree`/`none`/`global` plus an explicit
id list, unioned with self) is stored on each task beside its `Capability`
bitmask. One server-side `authorize(connID, wantCap, targetHex)` becomes the
single gate for every `TaskControl` kind carrying a task id; `visibleToCaller`
becomes the same resolution widened by `info_global`. A new operator-only
`set_caps` kind writes caps + scope into the task store, which every RPC
already re-reads, so the change is live.

**Tech Stack:** Go, brgen `.bgn` schemas (`make protoregen`), trsf/objproto
transport, existing `server/` + `cli/` + `tui/` + `webui/` layers.

**Spec:** `docs/superpowers/specs/2026-08-13-task-scoped-caps-design.md` —
read it before Task 1. Its §1–§4 Problem sections define the acceptance bar,
not just the Design sections.

## Global Constraints

- **Schema lives in one task.** Every `.bgn` change is Task 1. No later task
  adds a wire field.
- **`subtree = 0`** is the zero value of `ScopeBase`; clamping ranks by
  permissiveness (`none < subtree < global`) via `minScopeBase`, never by the
  numeric value. An unrecognised byte ranks as `none`.
- **Out-of-scope answers "no such task"**, never `permission_denied` — except
  `kill_port_forward`, which keeps its existing two-step visible-then-cap shape.
- **`set_caps` is gated on `lookupPrincipal(cid) == zero`**, never on a
  capability bit.
- **Three surfaces.** Every operator-visible knob lands in CLI *and* TUI *and*
  WebUI in the same task-group (pitfall 9). Do not ship one surface.
- **TUI/WebUI use the `*With(client)` helper variants** against their existing
  long-lived `*cli.Client`, never a fresh `Dial` (pitfall 3).
- **Verify with make targets**, not ad-hoc `go build ./...`:
  `make check`, `make wasm-check`, `make vet`, `make test`.
- **`scripts/wire-skew-check.sh` must pass before landing** (pitfall 10).
- No compat shims, no deprecation periods — individual dogfood scope.

---

### Task 1: Schema — every wire change, in one place

**Files:**
- Modify: `runner/protocol/message.bgn`
- Regenerate: `runner/protocol/message.go` (via `make protoregen`)
- Test: `runner/protocol/scope_wire_test.go` (create)

**Interfaces produced (every later task depends on these names):**

```go
protocol.ScopeBase_Subtree // 0
protocol.ScopeBase_None    // 1
protocol.ScopeBase_Global  // 2
protocol.TaskScope{ Base protocol.ScopeBase; IdsLen uint16; Ids []protocol.TaskID }
protocol.SubmitRequest.Scope            protocol.TaskScope
protocol.OpenInteractiveRequest.Scope   protocol.TaskScope
protocol.TaskInfo.Scope                 protocol.TaskScope
protocol.WhoAmIResponse.Scope           protocol.TaskScope
protocol.SubmitStatus_ScopeNotPermitted
protocol.OpenInteractiveStatus_ScopeNotPermitted
protocol.CancelResult_Ok / protocol.CancelResult_NoSuchTask
protocol.CancelStatus{ Status protocol.CancelResult }
protocol.TaskControlKind_SetCaps
protocol.SetCapsRequest{ TaskId; Caps; Scope; SetCaps; SetScope; Cascade; KeepConns uint8-bit fields }
protocol.SetCapsResponse{ Status; AffectedLen; Affected []TaskID; ConnsClosed uint32 }
protocol.SetCapsStatus_Ok / _NotFound / _NotOperator / _InternalError
```

- [ ] **Step 1: Add `ScopeBase` and `TaskScope` above `enum Capability`**

```
# ScopeBase is the base target set a TaskScope starts from, before its explicit
# id list is unioned in. Self is always in the effective set regardless.
enum ScopeBase:
    :u8
    # subtree is 0 deliberately: a client that sends no scope, a WAL record
    # written before scopes existed (omitempty), and a zero-valued Go struct all
    # read as the default instead of as the strictest setting.
    subtree = 0, "subtree"
    none    = 1, "none"
    global  = 2, "global"

# TaskScope bounds WHICH tasks a capability may be pointed at. The effective
# set is {self} ∪ baseSet(base) ∪ ids. Clamping at spawn ranks bases by
# permissiveness (none < subtree < global), not by numeric value.
format TaskScope:
    base    :ScopeBase
    ids_len :u16
    ids     :[ids_len]TaskID
```

- [ ] **Step 2: Append `scope :TaskScope` to the four carrier formats**

`SubmitRequest`: after `agent_profile`.
`OpenInteractiveRequest`: after `agent_profile`, BEFORE the
`if x11_enabled == 1:` block.
`TaskInfo`: after `agent_profile`.
`WhoAmIResponse`: after `capabilities`.

Each gets the comment:

```
    # scope bounds which tasks this task's capabilities may target; see TaskScope.
    scope :TaskScope
```

- [ ] **Step 3: Add the two new status values and the Cancel enum**

Append to `enum SubmitStatus` and `enum OpenInteractiveStatus`:

```
    # requested scope is not a subset of the spawner's effective scope
    scope_not_permitted = "scope_not_permitted"
```

Replace `format CancelStatus: status :u8` with:

```
enum CancelResult:
    :u8
    ok           = 0, "ok"
    no_such_task = 1, "no_such_task"

format CancelStatus:
    status :CancelResult
```

`ok = 0` keeps the success byte identical to every reply written today.

- [ ] **Step 4: Add the `set_caps` kind and its three formats**

Append `set_caps` to `enum TaskControlKind` (last, so existing ordinals are
stable) with the comment `# operator-only: rewrite a live task's caps + scope`.

```
enum SetCapsStatus:
    :u8
    ok             = "ok"
    not_found      = "not_found"
    not_operator   = "not_operator"
    internal_error = "internal_error"

# SetCapsRequest rewrites a live task's authority. set_caps / set_scope are
# presence bits, not conveniences: caps = 0 is Capability.none and
# scope{subtree,[]} is a real scope, so neither field has a spare value meaning
# "leave it alone".
format SetCapsRequest:
    task_id    :TaskID
    caps       :Capability
    scope      :TaskScope
    set_caps   :u1
    set_scope  :u1
    cascade    :u1
    keep_conns :u1
    reserved   :u4

format SetCapsResponse:
    status       :SetCapsStatus
    affected_len :u16
    affected     :[affected_len]TaskID
    conns_closed :u32
```

Add both union arms:

```
        TaskControlKind.set_caps => set_caps :SetCapsRequest      # in TaskControlRequest
        TaskControlKind.set_caps => set_caps :SetCapsResponse     # in TaskControlResponse
```

- [ ] **Step 5: Regenerate and compile**

```bash
make protoregen
make check
```
Expected: `runner/protocol/message.go` regenerates; `make check` fails in
`server/` and `cli/` on `CancelStatus{Status: 0}` — that untyped 0 no longer
assigns to `CancelResult`. Fix those call sites to `protocol.CancelResult_Ok`
(`grep -rn 'CancelStatus{' --include=*.go .`) and re-run until green.

- [ ] **Step 6: Write the wire test**

Create `runner/protocol/scope_wire_test.go`, modelled on
`file_transfer_wire_test.go`:

```go
package protocol

import "testing"

// A pre-scope SubmitRequest payload is one TaskScope short. The new decoder
// must reject it rather than reading whatever follows as a scope.
func TestSubmitRequestRejectsPreScopePayload(t *testing.T) {
	full := SubmitRequest{}
	full.SetRepoPath([]byte("/r"))
	full.SetPrompt([]byte("p"))
	buf, err := full.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	// TaskScope encodes as base(1) + ids_len(2) with no ids.
	old := buf[:len(buf)-3]
	var got SubmitRequest
	if err := got.DecodeExact(old); err == nil {
		t.Fatal("pre-scope payload decoded; want a short-buffer error")
	}
}

// The reverse direction: an old decoder sees trailing bytes. Simulated by
// decoding a new payload with three bytes appended, which is the same
// "DecodeExact has leftovers" failure an old peer would hit.
func TestSubmitRequestRejectsTrailingBytes(t *testing.T) {
	var req SubmitRequest
	req.SetRepoPath([]byte("/r"))
	req.SetPrompt([]byte("p"))
	buf, err := req.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	var got SubmitRequest
	if err := got.DecodeExact(append(buf, 0, 0, 0)); err == nil {
		t.Fatal("trailing bytes accepted; want an exact-length error")
	}
}

// Enum value additions are decode-safe: no is_defined check, String falls back.
func TestUnknownSubmitStatusDecodes(t *testing.T) {
	if got := SubmitStatus(200).String(); got != "SubmitStatus(200)" {
		t.Fatalf("String() = %q, want the numeric fallback", got)
	}
}

// CancelStatus{ok} must still be a single zero byte.
func TestCancelStatusOkIsOneZeroByte(t *testing.T) {
	buf, err := CancelStatus{Status: CancelResult_Ok}.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if len(buf) != 1 || buf[0] != 0 {
		t.Fatalf("encoded %v, want [0]", buf)
	}
}

// The default scope is subtree, and it is the zero value.
func TestZeroScopeIsSubtree(t *testing.T) {
	if (TaskScope{}).Base != ScopeBase_Subtree {
		t.Fatal("zero TaskScope is not subtree")
	}
}
```

- [ ] **Step 7: Run the wire test**

```bash
go test ./runner/protocol/ -run 'Scope|Cancel|SubmitRequestRejects|UnknownSubmitStatus' -v
```
Expected: PASS. If `Append` has a different receiver form for `CancelStatus`,
match the sibling in `file_transfer_wire_test.go` rather than guessing.

- [ ] **Step 8: Commit**

```bash
git add runner/protocol/
git commit -m "feat(protocol): TaskScope, set_caps, and a typed CancelStatus"
```

---

### Task 2: Store the scope — `server.Scope`, TaskStore, WAL

**Files:**
- Create: `server/scope.go`
- Modify: `server/taskstore.go` (TaskEntry, Create, Resume, new SetCaps)
- Modify: `server/wal.go` (WALEvent + JSON round-trip + replay)
- Test: `server/scope_test.go` (create), `server/taskstore_test.go` (extend)

**Interfaces:**
- Consumes: `protocol.TaskScope`, `protocol.ScopeBase_*` from Task 1.
- Produces:

```go
type Scope struct {
    Base protocol.ScopeBase
    IDs  []string // task-id hex, sorted + deduped
}
func scopeFromWire(w protocol.TaskScope) Scope
func (s Scope) toWire() protocol.TaskScope
func (s Scope) String() string           // "subtree", "none", "ids:ab..,cd..", "subtree+ids:ab.."
func scopeBaseRank(b protocol.ScopeBase) int
func minScopeBase(a, b protocol.ScopeBase) protocol.ScopeBase
// TaskEntry.Scope Scope
func (s *TaskStore) Create(..., caps protocol.Capability, scope Scope, agentProfile string) string
func (s *TaskStore) Resume(..., capsOverride bool, newCaps protocol.Capability,
                           scopeOverride bool, newScope Scope, ...) (TaskEntry, error)
func (s *TaskStore) SetCaps(id string, setCaps bool, caps protocol.Capability,
                            setScope bool, scope Scope) (TaskEntry, bool)
```

- [ ] **Step 1: Write the failing test**

`server/scope_test.go`:

```go
func TestMinScopeBaseRanksByPermissiveness(t *testing.T) {
	cases := []struct{ a, b, want protocol.ScopeBase }{
		{protocol.ScopeBase_Subtree, protocol.ScopeBase_Global, protocol.ScopeBase_Subtree},
		{protocol.ScopeBase_Global, protocol.ScopeBase_Subtree, protocol.ScopeBase_Subtree},
		{protocol.ScopeBase_None, protocol.ScopeBase_Subtree, protocol.ScopeBase_None},
		{protocol.ScopeBase_Global, protocol.ScopeBase_Global, protocol.ScopeBase_Global},
		// An unrecognised byte from a newer peer must rank as none.
		{protocol.ScopeBase(9), protocol.ScopeBase_Global, protocol.ScopeBase(9)},
	}
	for _, c := range cases {
		if got := minScopeBase(c.a, c.b); got != c.want {
			t.Errorf("minScopeBase(%v,%v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestScopeWireRoundTrip(t *testing.T) {
	in := Scope{Base: protocol.ScopeBase_None, IDs: []string{"bb", "aa", "aa"}}
	got := scopeFromWire(in.toWire())
	// Round-trip normalises: sorted, deduped.
	if got.Base != protocol.ScopeBase_None || len(got.IDs) != 2 ||
		got.IDs[0] != "aa" || got.IDs[1] != "bb" {
		t.Fatalf("round trip = %+v", got)
	}
}

func TestScopeString(t *testing.T) {
	for _, c := range []struct {
		in   Scope
		want string
	}{
		{Scope{Base: protocol.ScopeBase_Subtree}, "subtree"},
		{Scope{Base: protocol.ScopeBase_None}, "none"},
		{Scope{Base: protocol.ScopeBase_Global}, "global"},
		{Scope{Base: protocol.ScopeBase_None, IDs: []string{"aa"}}, "ids:aa"},
		{Scope{Base: protocol.ScopeBase_Subtree, IDs: []string{"aa"}}, "subtree+ids:aa"},
	} {
		if got := c.in.String(); got != c.want {
			t.Errorf("String() = %q, want %q", got, c.want)
		}
	}
}
```

Note the id hexes are short here on purpose — `Scope` stores whatever hex the
caller normalised, and `scopeFromWire` pads/truncates nothing. Real ids are 32
hex chars; `toWire` must left-pad-decode via `hex.DecodeString` into the
`[16]byte`, so make the test ids valid hex of even length.

- [ ] **Step 2: Run it to see it fail**

```bash
go test ./server/ -run 'TestMinScopeBase|TestScopeWire|TestScopeString' -v
```
Expected: FAIL to compile — `minScopeBase`, `Scope`, `scopeFromWire` undefined.

- [ ] **Step 3: Write `server/scope.go`**

`Scope`, `scopeFromWire`, `toWire`, `String`, `scopeBaseRank`, `minScopeBase`.
`scopeFromWire` sorts + dedupes the hex ids. `toWire` skips ids that are not
32-char hex rather than panicking (an id arriving malformed is a client bug,
not a server crash).

- [ ] **Step 4: Run the tests**

```bash
go test ./server/ -run 'TestMinScopeBase|TestScopeWire|TestScopeString' -v
```
Expected: PASS.

- [ ] **Step 5: Thread `Scope` through TaskEntry, Create, Resume**

Add `Scope Scope` to `TaskEntry` with a comment pointing at `server/scope.go`.
Add the parameter to `Create` (after `caps`) and the `scopeOverride bool,
newScope Scope` pair to `Resume` (after the caps pair). Update every call site:

```bash
grep -rn '\.Tasks\.Create(\|\.tasks\.Create(\|\.Resume(' --include=*.go . | grep -v '_test.go'
```
Fix the tests the same way — `go test ./server/` names them all.

- [ ] **Step 6: Add `TaskStore.SetCaps`**

```go
// SetCaps rewrites a live task's authority. Unlike Resume it has no
// terminal-state precondition: the point is to change a RUNNING task. Returns
// the post-change entry and whether the task existed.
func (s *TaskStore) SetCaps(id string, setCaps bool, caps protocol.Capability,
	setScope bool, scope Scope) (TaskEntry, bool)
```
It takes `s.mu`, applies whichever halves are set, writes one
`task_caps_changed` WAL event carrying both, and returns a copy.

- [ ] **Step 7: Extend the WAL**

`WALEvent` gains `ScopeBase uint8 \`json:"scope_base,omitempty"\`` and
`ScopeIDs []string \`json:"scope_ids,omitempty"\`` on the same struct that
already carries `Capabilities`. Write them from `task_created` and
`task_caps_changed`; read them in both replay cases. A legacy record has no
keys, so `ScopeBase` is 0 → `subtree` → today's behaviour, with no special case.

- [ ] **Step 8: Add the WAL round-trip test**

Extend `server/taskstore_test.go` beside `TestTaskCapsChangedWALRoundTrip`:

```go
func TestSetCapsWALCarriesScope(t *testing.T) {
	// 1. Marshal WALEvent{Type: "task_caps_changed", TaskID: id,
	//    Capabilities: uint32(Capability_Spawn), ScopeBase: uint8(ScopeBase_None),
	//    ScopeIDs: []string{peerHex}} and unmarshal it back: all three fields
	//    survive. Mirror TestTaskCapsChangedWALRoundTrip's marshalling helper.
	// 2. Replay that event onto a store holding `id` and assert
	//    entry.Capabilities == Capability_Spawn, entry.Scope.Base == None,
	//    entry.Scope.IDs == []string{peerHex}.
	// 3. Replay a LEGACY event — same Type and TaskID, Capabilities set, no
	//    scope keys at all — and assert entry.Scope.Base == ScopeBase_Subtree.
	//    This is the upgrade path: an omitempty-absent base must read as the
	//    default, not as the strictest setting.
}
```
Build the store over a temp-dir WAL exactly as
`TestTaskCapsChangedWALRoundTrip` does at `server/taskstore_test.go:1008`, and
reuse its replay entry point rather than reaching into `s.tasks` directly.

- [ ] **Step 9: Full package test + commit**

```bash
go test ./server/ && make check
git add server/ && git commit -m "feat(server): store a TaskScope beside each task's caps"
```

---

### Task 3: One resolution function — `scopeSet`, `authorize`, `visibleToCaller`

**Files:**
- Modify: `server/capabilities.go`
- Test: `server/capabilities_test.go` (extend)

**Interfaces:**
- Consumes: `Scope`, `TaskEntry.Scope` (Task 2).
- Produces:

```go
func (h *TaskHandler) scopeSet(connID string) (all bool, allowed map[string]bool)
func (h *TaskHandler) authorize(connID string, want protocol.Capability, targetHex string) bool
func (h *TaskHandler) childIndex() map[string][]string // creatorHex -> childHex, used by scopeSet and cascade
```
`visibleToCaller` and `listVisibleToCaller` keep their exact signatures.

- [ ] **Step 1: Write the failing tests**

In `server/capabilities_test.go`, beside the existing subtree test at line ~541:

```go
// info_global widens what may be SEEN and must not widen what may be DONE.
func TestInfoGlobalDoesNotWidenAuthorize(t *testing.T) {
	// B holds cancel + info_global, scope subtree. D is an unrelated task.
	// visibleToCaller(B) => all=true. authorize(B, cancel, D) => false.
}

func TestScopeSetHonoursBaseAndIDs(t *testing.T) {
	// base=none        => {self}
	// base=none+ids:D  => {self, D}
	// base=subtree     => {self, C}
	// base=global      => all=true
}

func TestAuthorizeRequiresBothCapAndScope(t *testing.T) {
	// cap present, target out of scope   => false
	// cap absent, target in scope        => false
	// both                               => true
	// operator (zero principal)          => true regardless
}

// Spec §2a: the LIST-only parent hop is justified by the creator relationship,
// not by the target set, so it survives the narrowest base — and it must never
// become an authorize target.
func TestParentHopSurvivesNoneBaseAndIsNotActionable(t *testing.T) {
	// child C of parent P, C.scope = {base: none}, C.caps = all
	// listVisibleToCaller(C)  => parentHex == P   (redacted row still listed)
	// scopeSet(C).allowed     => {C} only, P absent
	// authorize(C, cancel, P) => false
}

// An explicit ids:<parent> grant is the one way the parent becomes actionable,
// and then it is a full row rather than a redacted hop.
func TestParentInIDsIsActionableAndUnredacted(t *testing.T) {
	// C.scope = {base: none, ids: [P]}
	// authorize(C, cancel, P) => true
	// listVisibleToCaller(C)  => parentHex == "" (P is already in allowed)
}
```
Fill each body using the fixture style already in that file (it builds a
`TaskHandler` with a fake task store and registers principals directly).

- [ ] **Step 2: Run to see them fail**

```bash
go test ./server/ -run 'TestInfoGlobalDoesNotWiden|TestScopeSet|TestAuthorizeRequires' -v
```
Expected: FAIL to compile — `scopeSet`, `authorize` undefined.

- [ ] **Step 3: Implement**

Move the existing BFS out of `visibleToCaller` into `childIndex` +
`scopeSet`. `scopeSet` returns `all=true` for a zero principal or
`ScopeBase_Global`, and otherwise seeds `allowed` with the caller's own hex,
adds the scope ids, and BFSes only when the base is `subtree`. `authorize` is
`hasCap && (all || allowed[target])`. `visibleToCaller` becomes the
info_global wrapper. `listVisibleToCaller` keeps its body but now calls the
wrapper.

- [ ] **Step 4: Run the tests**

```bash
go test ./server/ -run 'Cap|Scope|Authorize|Visible' -v
```
Expected: PASS, including every pre-existing capability test.

- [ ] **Step 5: Commit**

```bash
git add server/capabilities.go server/capabilities_test.go
git commit -m "feat(server): scopeSet and authorize, with info_global as visibility only"
```

---

### Task 4: Wire `authorize` into every target-taking kind

**Files:**
- Modify: `server/task_handler.go` (Cancel, OpenFileTransfer, ListFiles,
  GitQuery, AttachSession, OpenPortForward, RegisterPortForward,
  KillPortForward, and the decode-failure log)
- Modify: `server/await_idle_handler.go`
- Modify: `server/file_transfer.go` (signatures gain `connID`)
- Test: `server/scope_enforcement_test.go` (create),
  `server/scope_completeness_test.go` (create)

**Interfaces:**
- Consumes: `authorize` (Task 3).
- Produces: nothing new; this task changes behaviour only.

- [ ] **Step 1: Write the failing enforcement test**

`server/scope_enforcement_test.go` — one subtest per row of spec §3, each
asserting the *specific* status, not a generic denial:

```go
func TestOutOfScopeTargetsAnswerNotFound(t *testing.T) {
	// caller B: caps=all, scope=subtree. target D: unrelated task.
	// cancel              => CancelResult_NoSuchTask
	// attach_session      => AttachSessionStatus_NotFound
	// await_idle          => AwaitIdleStatus_NotFound
	// open_file_transfer  => OpenFileTransferStatus_NoSuchTask
	// list_files          => ListFilesStatus_NoSuchTask
	// git_query           => GitQueryStatus_NoSuchTask
	// open_port_forward   => OpenPortForwardStatus_NoSuchTask
	// register_port_fwd   => OpenPortForwardStatus_NoSuchTask
	// and NONE of them answers TaskControlKind_PermissionDenied
}
```

- [ ] **Step 2: Run to see it fail**

```bash
go test ./server/ -run TestOutOfScopeTargets -v
```
Expected: FAIL — every subtest currently succeeds against the foreign task.

- [ ] **Step 3: Thread `connID` into the handlers that lack it**

`handleOpenFileTransfer`, `handleListFiles`, `handleGitQuery`,
`handleAttachSession` gain a `connID string` parameter and call `authorize`
before touching the target. `handleAwaitIdle` already has `conn`. The
direction-dependent file cap stays where it is — pass the resolved `need` into
`authorize` instead of calling `hasCap` separately, so there is exactly one
gate per request.

- [ ] **Step 4: Gate Cancel and the two port-forward opens**

Cancel returns `CancelStatus{Status: protocol.CancelResult_NoSuchTask}` when
`authorize` fails, and `CancelResult_Ok` otherwise. `KillPortForward` keeps its
existing visible-first shape; only the inner `hasCap` becomes `authorize`.

- [ ] **Step 5: Improve the decode-failure log**

In `Handle`, when `req.DecodeExact` fails, log kind + request id + payload
length before returning. Both are at fixed offsets ahead of the union arm:

```go
if err := req.DecodeExact(payload); err != nil {
	kind, reqID := "?", uint32(0)
	if len(payload) >= 5 {
		kind = protocol.TaskControlKind(payload[0]).String()
		reqID = binary.BigEndian.Uint32(payload[1:5])
	}
	slog.Error("TaskHandler: failed to decode TaskControlRequest",
		"error", err, "kind", kind, "request_id", reqID, "payload_len", len(payload))
	return
}
```

- [ ] **Step 6: Run the enforcement test**

```bash
go test ./server/ -run TestOutOfScopeTargets -v
```
Expected: PASS.

- [ ] **Step 7: Write the completeness test**

`server/scope_completeness_test.go`, modelled on
`server/mapper_completeness_test.go`: a literal table of every
`TaskControlKind` classified as `targetGated` / `infoScoped` / `noTarget`,
asserting the table covers every value the enum defines. A new kind is
unclassified until someone adds a row, and the row forces the author to decide.

```go
var kindTargetClass = map[protocol.TaskControlKind]string{ /* every kind */ }

func TestEveryTaskControlKindIsClassified(t *testing.T) {
	for k := protocol.TaskControlKind(0); k <= protocol.TaskControlKind_SetCaps; k++ {
		if k.String() == fmt.Sprintf("TaskControlKind(%d)", k) {
			continue // gap in the enum, not a real kind
		}
		if _, ok := kindTargetClass[k]; !ok {
			t.Errorf("kind %v unclassified: add it to kindTargetClass and gate it if it names a task", k)
		}
	}
}
```

- [ ] **Step 8: Run everything and commit**

```bash
go test ./server/ && make check && make vet
git add server/ && git commit -m "feat(server): gate every task-targeting kind on the caller's scope"
```

---

### Task 5: Spawn-time scope attenuation

**Files:**
- Modify: `server/task_handler.go` (`handleSubmit`, `handleSubmitResume`,
  `handleOpenInteractive`)
- Test: `server/scope_attenuation_test.go` (create)

**Interfaces:**
- Consumes: `minScopeBase`, `scopeSet`, `Scope` (Tasks 2–3).
- Produces:

```go
// attenuateScope clamps a requested scope to the creator's effective one.
// Returns the offending id hex when a requested id is outside it.
func (h *TaskHandler) attenuateScope(creatorCID string, req Scope) (Scope, string, bool)
```

- [ ] **Step 1: Write the failing test**

```go
func TestAttenuateScopeClampsBase(t *testing.T)      // subtree parent + global request => subtree
func TestAttenuateScopeRejectsForeignID(t *testing.T) // id outside the parent's set => ok=false, offender returned
func TestAttenuateScopeAllowsSelfID(t *testing.T)     // parent may grant a child ids:<parent>
func TestSubmitRejectsUnpermittedScope(t *testing.T)  // SubmitStatus_ScopeNotPermitted + error_msg names the id
```

- [ ] **Step 2: Run to see it fail**

```bash
go test ./server/ -run TestAttenuateScope -v
```
Expected: FAIL to compile.

- [ ] **Step 3: Implement `attenuateScope` and wire the three spawn paths**

Both resume paths ALSO gate `resume_task_id` through `authorize(cid,
Capability_Spawn, resumeHex)` and answer `resume_not_found` when it fails —
that is spec §1 problem 2 and it is easy to skip because the resume code reads
as a lookup rather than as an operation on someone else's task.

`handleOpenInteractive` returns `OpenInteractiveStatus_ScopeNotPermitted` with
no message (that response has no `error_msg` field); the CLI supplies the
detail.

- [ ] **Step 4: Run the tests**

```bash
go test ./server/ -run 'TestAttenuateScope|TestSubmitRejects' -v
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add server/ && git commit -m "feat(server): clamp a spawned task's scope to its creator's"
```

---

### Task 6: `set_caps` — operator-only live re-grant

**Files:**
- Create: `server/set_caps_handler.go`
- Modify: `server/task_handler.go` (dispatch case + the new hook field)
- Modify: `server/server.go` (wire `DropConnsForPrincipal`)
- Test: `server/set_caps_test.go` (create)

**Interfaces:**
- Consumes: `TaskStore.SetCaps`, `childIndex`, `Scope` (Tasks 2–3).
- Produces:

```go
// On TaskHandler:
DropConnsForPrincipal func(taskIDHex string) int
func (h *TaskHandler) handleSetCaps(conn ConnHandle, requestID uint32, cid string, req *protocol.SetCapsRequest)
// On Server:
func (s *Server) dropConnsForPrincipal(taskIDHex string) int
```

- [ ] **Step 1: Write the failing tests**

```go
func TestSetCapsRejectsNonOperator(t *testing.T)  // agent principal => SetCapsStatus_NotOperator
func TestSetCapsIsLive(t *testing.T)              // callerCaps(target) reflects the change immediately
func TestSetCapsCascadeClampsDescendants(t *testing.T) // two-level subtree, caps AND'd, base min'd, ids filtered
func TestSetCapsNarrowingDropsConns(t *testing.T) // hook called for each affected task; not called on a pure widening
func TestSetCapsKeepConnsSuppressesDrop(t *testing.T)
```

- [ ] **Step 2: Run to see them fail**

```bash
go test ./server/ -run TestSetCaps -v
```
Expected: FAIL to compile.

- [ ] **Step 3: Implement the handler**

Gate first: `h.lookupPrincipal(cid).Id != [16]byte{}` → `not_operator`, before
any store lookup. Then `Tasks.SetCaps`; then, when `cascade`, BFS `childIndex`
and clamp each descendant (`caps &= newCaps`, `base = minScopeBase`, ids
filtered to the target's new effective set) through the same `Tasks.SetCaps`.
Compute `narrowed` as "any cap bit removed OR the base ranked down OR the id
set shrank" across the affected set; when `narrowed && !keepConns`, call
`DropConnsForPrincipal` for each affected id and sum the results into
`conns_closed`.

- [ ] **Step 4: Wire the dispatch case and the hook**

`case protocol.TaskControlKind_SetCaps:` in `Handle`. It is NOT added to
`requiredCap` — the gate is identity, not a bit, and a `requiredCap` entry
would let anyone holding that bit through.

In `server.go`, beside the other TaskHandler hooks:

```go
s.taskHandler.DropConnsForPrincipal = s.dropConnsForPrincipal
```

```go
// dropConnsForPrincipal closes every live connection whose principal is
// taskIDHex, tearing down attaches, file transfers and port forwards in one
// step. Returns how many were closed.
func (s *Server) dropConnsForPrincipal(taskIDHex string) int {
	var victims []streamingConn
	s.activeConnsMu.Lock()
	for cid, sc := range s.activeConns {
		p := s.taskHandler.lookupPrincipal(cid.String())
		if p.Id != ([16]byte{}) && hex.EncodeToString(p.Id[:]) == taskIDHex {
			victims = append(victims, sc)
		}
	}
	s.activeConnsMu.Unlock()
	for _, sc := range victims {
		_ = sc.Close() // objproto.Connection.Close, embedded in streamingConn
	}
	return len(victims)
}
```
Collect under the lock, close outside it — `Close` can re-enter connection
teardown that takes `activeConnsMu`.

- [ ] **Step 5: Run the tests**

```bash
go test ./server/ -run TestSetCaps -v && go test ./server/
```
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add server/ && git commit -m "feat(server): operator-only set_caps, live, with cascade and conn drop"
```

---

### Task 7: Scope-filter `prune`

**Files:**
- Modify: `server/server.go` (`PruneFn` closure), `server/task_handler.go`
  (pass the caller through)
- Test: `server/prune_scope_test.go` (create)

**Interfaces:**
- Consumes: `scopeSet` (Task 3).
- Produces: `PruneFn` gains a leading `allowed map[string]bool` (nil = every
  task, i.e. an operator or a `global` base).

- [ ] **Step 1: Write the failing test**

```go
func TestPruneByIDsSkipsOutOfScope(t *testing.T) {
	// confined caller prunes [own-terminal, foreign-terminal]
	// => removed == 1, skipped_missing == 1, foreign task still in the store
}
func TestPruneSweepIsScopedToCaller(t *testing.T) {
	// bare before_ts sweep from a confined caller leaves the operator's
	// terminal tasks in place
}
```

- [ ] **Step 2: Run to see it fail**

```bash
go test ./server/ -run TestPrune -v
```
Expected: FAIL — both prune forms currently reach every task.

- [ ] **Step 3: Implement**

`PruneFn func(allowed map[string]bool, req *protocol.PruneTasksRequest) (...)`.
The dispatch case computes `all, allowed := h.scopeSet(cid)` and passes
`nil` when `all`. `PruneByIDs` filters out-of-scope ids into
`skippedMissing` — deliberately the same bucket as a nonexistent id, so the
count does not distinguish "not yours" from "not there".
`PruneTerminal` gains the same filter.

- [ ] **Step 4: Run the tests + commit**

```bash
go test ./server/ && git add server/
git commit -m "feat(server): prune only reaches the caller's own scope"
```

---

### Task 8: CLI — `--scope`, `caps set`, and scope in every listing

**Files:**
- Create: `cli/scope.go`, `cli/scope_test.go`, `cli/set_caps.go`
- Modify: `cli/session_opts.go` (SessionOpts.Scope), `cli/submit.go`,
  `cli/open_interactive_native.go`, `cli/open_interactive_wasm.go`,
  `cli/list.go`, `cli/whoami.go`, `cli/caps.go`
- Modify: `cmd/harness-cli/main.go` (flags + `caps set` subcommand + usage),
  `cmd/harness-cli/session.go`

**Interfaces:**
- Consumes: Task 1 wire types.
- Produces:

```go
func cli.ParseScope(s string) (protocol.TaskScope, error)
func cli.ScopeLabel(s protocol.TaskScope) string
func cli.SetCaps(ctx, peerCID string, opts SetCapsOpts) (SetCapsResult, error)
func cli.SetCapsWith(ctx, c *Client, opts SetCapsOpts) (SetCapsResult, error)
type SetCapsOpts struct {
	TaskID              string
	Caps                *protocol.Capability // nil = keep
	Scope               *protocol.TaskScope  // nil = keep
	Cascade, KeepConns  bool
}
type SetCapsResult struct {
	Status      protocol.SetCapsStatus
	Affected    []string
	ConnsClosed uint32
}
// SessionOpts.Scope protocol.TaskScope   // zero value = subtree
```

Both `SetCaps` and `SetCapsWith` exist and `SetCaps` is the thin dial+close
wrapper around `SetCapsWith` — TUI and WebUI must call the `With` form
(pitfall 3).

- [ ] **Step 1: Write the failing parser test**

`cli/scope_test.go`:

```go
func TestParseScope(t *testing.T) {
	id := strings.Repeat("ab", 16)
	for _, c := range []struct {
		in      string
		base    protocol.ScopeBase
		ids     int
		wantErr bool
	}{
		{"", protocol.ScopeBase_Subtree, 0, false},
		{"subtree", protocol.ScopeBase_Subtree, 0, false},
		{"none", protocol.ScopeBase_None, 0, false},
		{"global", protocol.ScopeBase_Global, 0, false},
		{"ids:" + id, protocol.ScopeBase_None, 1, false},
		{"none+ids:" + id, protocol.ScopeBase_None, 1, false},
		{"subtree+ids:" + id + "," + id, protocol.ScopeBase_Subtree, 1, false}, // deduped
		{"global+ids:" + id, 0, 0, true},   // ids under global are meaningless
		{"ids:", 0, 0, true},                // empty list
		{"ids:zz", 0, 0, true},              // not hex
		{"bogus", 0, 0, true},
	} { /* assert */ }
}

func TestScopeLabelRoundTrips(t *testing.T) {
	// ScopeLabel(ParseScope(x)) == x for every canonical form above
}
```
`global+ids:` is an error rather than a silent no-op: a scope the user wrote
and the server ignores is the invisible-clamping failure this design rejects
elsewhere.

- [ ] **Step 2: Run to see it fail**

```bash
go test ./cli/ -run TestParseScope -v
```
Expected: FAIL to compile.

- [ ] **Step 3: Implement `cli/scope.go`**

`ParseScope` accepts `""|subtree|none|global|[base+]ids:<hex>[,<hex>]`,
validates each id as 32 hex chars, sorts + dedupes. `ScopeLabel` is the
inverse and returns `"subtree"` for the zero value.

- [ ] **Step 4: Run the parser tests**

```bash
go test ./cli/ -run 'TestParseScope|TestScopeLabel' -v
```
Expected: PASS.

- [ ] **Step 5: Add `SessionOpts.Scope` and carry it on both spawn paths**

`buildSubmitRequest` and the interactive builder set `req.Scope = opts.Scope`.
Check both `open_interactive_native.go` and `open_interactive_wasm.go` — they
are separate builders and the wasm one is the surface that silently skews
(memory: native+wasm parity trap).

- [ ] **Step 6: Add `cli/set_caps.go`**

`SetCapsWith` builds the request, round-trips, maps the response.
`SetCaps` = `Dial` + `defer Close` + `SetCapsWith`.

- [ ] **Step 7: Add the flags and the subcommand**

`--scope` on `submit`, `interactive`, `session new` with the help text:

```
--scope: which tasks this task's caps may target — subtree (default) | none |
         global | [subtree+]ids:<task-id>[,<task-id>]
```

`harness-cli caps set <task-id> [--caps NAMES] [--scope SPEC] [--cascade] [--keep-conns]`.
Reject a call with neither `--caps` nor `--scope` client-side, using
`capsExplicitlySet(fs)` and its new `scopeExplicitlySet` twin — the same
"was the flag actually typed" check `--resume --caps` already needs. On
`scope_not_permitted` from either spawn path, print the ids that were
requested so the interactive path (no `error_msg` field) is still diagnosable.

- [ ] **Step 8: Show the scope wherever caps are shown**

`ls --json`, `session ls`, `whoami` (all three gain a `scope` field);
human `ls` renders `scope=<label>` beside `caps=` only when the label is not
`"subtree"`. `harness-cli caps` gains a SCOPE section documenting the grammar,
and `--json` gains a sibling `scopes` array.

- [ ] **Step 9: Verify and commit**

```bash
go test ./cli/ && make check && make wasm-check
git add cli/ cmd/harness-cli/
git commit -m "feat(cli): --scope on every spawn, caps set, and scope in listings"
```

---

### Task 9: TUI

**Files:**
- Modify: `tui/app.go` (`sessionScope` beside `sessionCaps`), `tui/cmdline.go`
  (`caps` command gains a scope argument; new `setcaps` command),
  `tui/client.go` (call `cli.SetCapsWith`), `tui/interactive.go` and every
  spawn site that reads `sessionCaps`
- Test: `tui/cmdline_test.go` (extend)

**Interfaces:**
- Consumes: `cli.ParseScope`, `cli.ScopeLabel`, `cli.SetCapsWith`,
  `SessionOpts.Scope`.

- [ ] **Step 1: Find every spawn site**

```bash
grep -n 'sessionCaps' tui/*.go
```
The per-spawn caps spec (`2026-08-10-tui-per-spawn-caps-design.md`) counted
thirteen. Every one that sets `Caps` must also set `Scope`, or the TUI silently
spawns with the default while the CLI honours the flag.

- [ ] **Step 2: Write the failing cmdline test**

```go
func TestCapsCommandAcceptsScope(t *testing.T) {
	// `caps spawn,file_read scope=none` sets both fields and echoes both
}
func TestSetCapsCommandCallsClient(t *testing.T) {
	// `setcaps <id> --scope global` reaches the stub client with the right opts
}
```

- [ ] **Step 3: Run to see it fail**

```bash
go test ./tui/ -run 'TestCapsCommand|TestSetCapsCommand' -v
```

- [ ] **Step 4: Implement, using the long-lived client**

`tui/client.go` calls `cli.SetCapsWith(ctx, a.client, ...)`. Confirm the
sibling pattern first:

```bash
grep -n 'cli\.[A-Za-z]*With(' tui/*.go
```

- [ ] **Step 5: Run and commit**

```bash
go test ./tui/ && make check
git add tui/ && git commit -m "feat(tui): per-spawn scope and a set-caps command"
```

---

### Task 10: WebUI

**Files:**
- Modify: `cmd/harness-webui-wasm/main.go` (export the scope + set-caps
  bridges), `webui/static/main.js` (spawn dialog input, task-table column,
  task-detail re-grant action)

**Interfaces:**
- Consumes: the same `cli` helpers via the wasm bridge; must use
  `currentClient()`, not a fresh dial.

- [ ] **Step 1: Read the sibling bridge first**

```bash
grep -n 'caps' cmd/harness-webui-wasm/main.go webui/static/main.js
```
Copy the shape of the existing caps bridge exactly — argument marshalling and
error propagation both differ from the native path.

- [ ] **Step 2: Add the scope input to the spawn dialog**

Beside the existing caps input, same label/`<input>` structure. Dark theme
(`#1e1e1e` / `#d4d4d4`) and a layout that still works at 390px wide.

- [ ] **Step 3: Add the scope column and the re-grant action**

Column beside caps in the task table; a re-grant control on the task detail
view, unconditionally visible (a WebUI connection is an operator connection by
construction — spec §7).

- [ ] **Step 4: Build and verify visually**

```bash
make wasm-check && make webui-build
```
Then drive the live WebUI with Playwright at both 390px and desktop widths,
and keep the screenshots (they are deliverables, not scratch files).

- [ ] **Step 5: Commit**

```bash
git add cmd/harness-webui-wasm/ webui/
git commit -m "feat(webui): scope on spawn, in the task table, and a re-grant action"
```

---

### Task 11: Agent skills

**Files:**
- Modify: `runner/agentskills/supervising-workers/SKILL.md` (the `go:embed`
  source of truth), then mirror byte-for-byte to
  `.claude/skills/supervising-workers/SKILL.md` and
  `.agents/skills/supervising-workers/SKILL.md`

- [ ] **Step 1: Rewrite the `--caps` section**

Add scope to the attenuation rules: omitted means `subtree`, never
inherit-the-creator; `global` is the explicit opt-out; `ids:` narrows.

- [ ] **Step 2: Rewrite the prune conventions**

The "prune only what you spawned / don't run the bare sweep on a shared
server" paragraph now describes an enforced rule. Keep the advice to pipe ids
rather than hand-typing them — that is about id hygiene, not authority.

- [ ] **Step 3: Mirror and verify the copies match**

```bash
for d in .claude .agents; do
  cp runner/agentskills/supervising-workers/SKILL.md $d/skills/supervising-workers/SKILL.md
done
diff -q runner/agentskills/supervising-workers/SKILL.md .claude/skills/supervising-workers/SKILL.md
diff -q runner/agentskills/supervising-workers/SKILL.md .agents/skills/supervising-workers/SKILL.md
```

- [ ] **Step 4: Commit**

```bash
git add runner/agentskills/ .claude/skills/ .agents/skills/
git commit -m "docs(skills): scope is enforced, and prune is no longer a convention"
```

---

### Task 12: End-to-end verification

**Files:**
- Create: `integration/scope_e2e_test.go`

- [ ] **Step 1: Run the wire-skew guard**

```bash
scripts/wire-skew-check.sh
```
Expected: PASS. This change touches `.bgn`, so it is not a no-op run. It
asserts NEW runner × OLD server degrades recoverably; since no `Runner*`
format changed, the handshake should be unaffected. If it goes red, stop —
that means a shared format changed that this plan did not account for.

- [ ] **Step 2: Write the integration test**

`integration/scope_e2e_test.go` (build tag `integration`, matching its
siblings):

```go
// A subtree-scoped child cannot reach a sibling, an ids-scoped one can reach
// exactly its named target, and neither shows the other in ls.
func TestScopeConfinesAcrossTasks(t *testing.T)

// caps set takes effect on the target's very next RPC, with no restart.
func TestSetCapsIsLiveWithoutRestart(t *testing.T)
```

- [ ] **Step 3: Run it**

```bash
make test-integration
```
Expected: PASS.

- [ ] **Step 4: Full local gate**

```bash
make check && make wasm-check && make vet && make test
```
Expected: all green.

- [ ] **Step 5: Dummy-harness manual pass**

```bash
scripts/dummy-harness.sh up --detach --agent fake --name SCOPE
```
Then, per spec §Testing:
- a `--scope subtree` child cannot cancel / attach / file-pull a sibling and
  does not list it;
- a `--scope ids:<sibling>` child can do exactly those three and nothing else;
- `caps set <child> --caps none` from the operator changes the child's next
  `ls` with no restart;
- the same call while the child holds an attach drops the attach and leaves the
  child's own session alive;
- **resume across a server restart on the new build** — spawn interactive,
  restart the server so the task lands in Failed/`server_restart`, resume with
  the rebuilt client. This is the gate from spec §8 step 2.

```bash
scripts/dummy-harness.sh down
```

- [ ] **Step 6: Commit**

```bash
git add integration/
git commit -m "test(integration): scope confinement and live set_caps end to end"
```

---

## Landing

Follow the `landing-to-main` skill (Mode A local-trunk, FF-push, never
cherry-pick), then `make build` in the main checkout. Before restarting
anything, re-read spec §8: **build every client binary and the wasm first,
prove resume works against the new server on a dummy harness, and only then
restart the live server** — the server restart forces every running task to
Failed, and the recovery path is resume, which travels on the two formats this
change modifies.
