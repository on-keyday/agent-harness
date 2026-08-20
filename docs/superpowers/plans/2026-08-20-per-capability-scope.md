# Per-Capability Scope Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give every capability its own target set, promote visibility from a capability bit to an axis of `TaskScope`, and make `self` removable from an action set.

**Architecture:** `TaskScope` grows a visibility rank (`vis_base` + a presence bit), an `exclude_self` flag, and a view-only id list; a sparse list of `ScopeOverride` entries — each a disjoint capability *mask* plus a scope — rides beside it in every format that already carries a scope. `scopeSet` gains a capability argument so the compiler forces every call site to say which verb it is resolving for. `Capability_InfoGlobal` loses its visibility duty to the axis and keeps its agentboard duty under the name `board_observe`.

**Tech Stack:** Go 1.x, `brgen` schema codegen (`.bgn` → `runner/protocol/message.go`), Bubble Tea (TUI), Go/WASM + vanilla JS (WebUI), standard-library `testing`.

**Spec:** `docs/superpowers/specs/2026-08-20-per-capability-scope-design.md` — read it first; every task below argues from a numbered section of it. The base spec it supersedes is `docs/superpowers/specs/2026-08-13-task-scoped-caps-design.md`.

## Global Constraints

- **Every new field's zero value must be the pre-change behaviour.** `base = subtree`, `vis_base_present = 0`, `exclude_self = 0`, empty lists. An all-zeros `TaskScope` is today's default. (spec §1)
- **Migration never clears a capability bit.** Replay may add the authority a record implies; it may remove nothing. (spec §8)
- **Out-of-scope targets answer the kind's own "no such task", never `permission_denied`.** (base spec §3)
- **Rank order is by permissiveness, not numeric value:** `none < subtree < global`; an unrecognised byte ranks as `none` (fail closed). `server/scope.go:scopeBaseRank`.
- **Capability ordinals never change.** `Capability_InfoGlobal` keeps `1024`; only its name changes.
- **Verify with make targets, never bare `go build`:** `make check`, `make wasm-check`, `make vet`, `make test`, `make test-integration`. `go build ./...` hides breaks that the targets catch.
- **Schema is regenerated, never hand-edited:** `make protoregen ARGS='runner/protocol/message.bgn'`. Never edit `runner/protocol/message.go` by hand.
- **`runner/agentskills/` is the `go:embed` source of truth** for skill text; edits there must be mirrored to `.claude/skills/` and `.agents/skills/`.
- **This is a hard wire break in both directions.** Do not restart anything until Task 15. A skewed request produces no response and the client hangs on a deadline-less context.

---

### Task 1: Schema — the three axes and the override list

**Files:**
- Modify: `runner/protocol/message.bgn:1553-1576` (`enum ScopeBase`, `format TaskScope`)
- Modify: `runner/protocol/message.bgn:1488` (`enum Capability` — rename `info_global`)
- Regenerate: `runner/protocol/message.go` (via `make protoregen`)
- Test: `runner/protocol/scope_wire_test.go` (create)

**Interfaces:**
- Produces: `protocol.TaskScope{Base, VisBase, ExcludeSelf, VisBasePresent, VisIdsLen, VisIds, IdsLen, Ids}`, `protocol.ScopeOverride{Caps, Base, ExcludeSelf, IdsLen, Ids}`, `protocol.Capability_BoardObserve` (value 1024). Every later task consumes these names.

- [ ] **Step 1: Write the failing wire test**

Create `runner/protocol/scope_wire_test.go`:

```go
package protocol

import "testing"

// An all-zeros TaskScope must decode to today's default: subtree base,
// visibility following the base, self included, no ids.
func TestTaskScopeZeroValueIsPreChangeDefault(t *testing.T) {
	var s TaskScope
	if s.Base != ScopeBase_Subtree {
		t.Errorf("Base = %v, want subtree", s.Base)
	}
	if s.VisBasePresent() {
		t.Error("VisBasePresent = true, want false (visibility follows base)")
	}
	if s.ExcludeSelf() {
		t.Error("ExcludeSelf = true, want false (self included)")
	}
	if len(s.Ids) != 0 || len(s.VisIds) != 0 {
		t.Errorf("ids = %d, vis_ids = %d, want 0 and 0", len(s.Ids), len(s.VisIds))
	}
}

func TestTaskScopeRoundTripWithOverrideFields(t *testing.T) {
	var s TaskScope
	s.Base = ScopeBase_None
	s.VisBase = ScopeBase_Global
	if !s.SetVisBasePresent(true) || !s.SetExcludeSelf(true) {
		t.Fatal("flag setters rejected a value")
	}
	enc := s.MustEncodeCopy(nil)

	var got TaskScope
	if err := got.DecodeExact(enc); err != nil {
		t.Fatalf("DecodeExact: %v", err)
	}
	if got.Base != ScopeBase_None || got.VisBase != ScopeBase_Global {
		t.Errorf("bases = %v/%v, want none/global", got.Base, got.VisBase)
	}
	if !got.VisBasePresent() || !got.ExcludeSelf() {
		t.Error("flags did not survive the round trip")
	}
}
```

- [ ] **Step 2: Run it to confirm it fails**

Run: `make test 2>&1 | grep -A5 scope_wire`
Expected: compile failure — `s.VisBasePresent undefined`, `ScopeOverride` not declared.

- [ ] **Step 3: Edit the schema**

In `runner/protocol/message.bgn`, replace `format TaskScope` (line 1573) with:

```
format TaskScope:
    base             :ScopeBase
    # Visibility rank. Ignored unless vis_base_present is 1, and MUST be 0
    # when it is 0 — otherwise one authority has two encodings, which compare
    # unequal and render differently in --json. See design §11 rule 5.
    vis_base         :ScopeBase
    # 0 = {self} is in the action set. Phrased negatively so the zero value is
    # the pre-change unconditional self; include_self would have replayed every
    # legacy record into "cannot touch my own worktree".
    exclude_self     :u1
    # 0 = the visibility rank is `base`, which pins the pair to the diagonal
    # and makes every legacy record legal by construction.
    vis_base_present :u1
    reserved         :u6
    vis_ids_len      :u16
    vis_ids          :[vis_ids_len]TaskID
    ids_len          :u16
    ids              :[ids_len]TaskID

# One override: a capability MASK and the scope those bits resolve through.
# Masks in one list are pairwise disjoint, so override(cap) is a lookup and
# never needs a precedence rule. Not a nested TaskScope: an override has no
# visibility of its own, and a nested vis_* would be a field that must be
# validated as always-zero.
format ScopeOverride:
    caps         :Capability
    base         :ScopeBase
    exclude_self :u1
    reserved     :u7
    ids_len      :u16
    ids          :[ids_len]TaskID
```

In `enum Capability` (line 1488), rename the `info_global` entry, keeping its value:

```
    board_observe  = 0x400, "board_observe"
```

Add a comment above it explaining the rename, in the style of the `exec_control` comment already at 1499:

```
    # board_observe keeps 0x400 — it was info_global, which gated task
    # visibility, connection visibility AND the agentboard's enumeration
    # surfaces. The first two moved to TaskScope.vis_base; this bit kept the
    # third. The WAL persists the number, so no record migrates. The name says
    # observe rather than read because the bit is not needed to send, to
    # subscribe, or to read your own inbox — only to observe other agents'
    # topics, messages and subscribers, and an agent that reads "board_read
    # denied" goes debugging a messaging path that is working.
```

Then append `overrides_len :u8` + `overrides :[overrides_len]ScopeOverride` immediately after the `scope` field of each format that carries one: `SubmitRequest`, `OpenInteractiveRequest`, `SetCapsRequest`, `TaskInfo`, `WhoAmIResponse`. Locate them with:

```bash
grep -n 'scope *:TaskScope' runner/protocol/message.bgn
```

- [ ] **Step 4: Regenerate and run the test**

```bash
make protoregen ARGS='runner/protocol/message.bgn'
make test 2>&1 | grep -E 'scope_wire|FAIL|ok  '
```
Expected: PASS. If the generated flag accessor is named differently from `VisBasePresent()`, fix the *test* to match the generated name and note it in the commit — the generator owns that spelling.

- [ ] **Step 5: Confirm the whole tree still compiles**

Run: `make vet`
Expected: failures only in `server/`, `cli/`, `tui/`, `cmd/` where `Capability_InfoGlobal` no longer exists. That list is the work of Tasks 2-11 — capture it:

```bash
make vet 2>&1 | grep InfoGlobal | tee /tmp/infoglobal-sites.txt
```

- [ ] **Step 6: Commit**

```bash
git add runner/protocol/message.bgn runner/protocol/message.go runner/protocol/scope_wire_test.go
git commit -m "feat(protocol): TaskScope gains a visibility axis and override list"
```

---

### Task 2: `server/scope.go` — the value, its rank, and its validation

**Files:**
- Modify: `server/scope.go:23-26` (`type Scope`), `:35` (`defaultScope`), `:137` (`scopeFromWAL`)
- Test: `server/scope_test.go` (exists — extend)

**Interfaces:**
- Consumes: `protocol.TaskScope`, `protocol.ScopeOverride` from Task 1.
- Produces:
  - `type Scope struct { Base protocol.ScopeBase; VisBase protocol.ScopeBase; VisBasePresent bool; ExcludeSelf bool; IDs []string; VisIDs []string; Overrides []ScopeOverride }`
  - `type ScopeOverride struct { Caps protocol.Capability; Base protocol.ScopeBase; ExcludeSelf bool; IDs []string }`
  - `func (s Scope) VisRank() protocol.ScopeBase`
  - `func (s Scope) ForCap(c protocol.Capability) (base protocol.ScopeBase, excludeSelf bool, ids []string)`
  - `func validateScope(s Scope) error` — the seven rejections of design §11
- Later tasks call `VisRank`, `ForCap` and `validateScope` by these exact names.

- [ ] **Step 1: Write the failing tests**

Append to `server/scope_test.go`:

```go
func TestVisRankFollowsBaseWhenAbsent(t *testing.T) {
	s := Scope{Base: protocol.ScopeBase_None}
	if got := s.VisRank(); got != protocol.ScopeBase_None {
		t.Errorf("VisRank = %v, want none (follows base)", got)
	}
	s.VisBasePresent = true
	s.VisBase = protocol.ScopeBase_Global
	if got := s.VisRank(); got != protocol.ScopeBase_Global {
		t.Errorf("VisRank = %v, want global", got)
	}
}

func TestForCapPrefersTheOverride(t *testing.T) {
	s := Scope{
		Base: protocol.ScopeBase_Subtree,
		Overrides: []ScopeOverride{{
			Caps: protocol.Capability_ExecCowrite, Base: protocol.ScopeBase_Subtree, ExcludeSelf: true,
		}},
	}
	if _, ex, _ := s.ForCap(protocol.Capability_ExecCowrite); !ex {
		t.Error("exec_cowrite: ExcludeSelf = false, want true from the override")
	}
	if _, ex, _ := s.ForCap(protocol.Capability_ExecView); ex {
		t.Error("exec_view: ExcludeSelf = true, want false — the override must not leak to another bit")
	}
}

func TestValidateScopeRejections(t *testing.T) {
	id := "00112233445566778899aabbccddeeff"
	cases := []struct {
		name string
		s    Scope
	}{
		{"base outranks visibility", Scope{
			Base: protocol.ScopeBase_Subtree, VisBasePresent: true, VisBase: protocol.ScopeBase_None}},
		{"override outranks visibility", Scope{
			Base: protocol.ScopeBase_None, VisBasePresent: true, VisBase: protocol.ScopeBase_None,
			Overrides: []ScopeOverride{{Caps: protocol.Capability_ExecControl, Base: protocol.ScopeBase_Subtree}}}},
		{"empty mask", Scope{
			Overrides: []ScopeOverride{{Caps: protocol.Capability_None}}}},
		{"masks intersect", Scope{Overrides: []ScopeOverride{
			{Caps: protocol.Capability_ExecView | protocol.Capability_Cancel},
			{Caps: protocol.Capability_Cancel},
		}}},
		{"non-canonical vis_base", Scope{VisBase: protocol.ScopeBase_Global}},
	}
	for _, tc := range cases {
		if err := validateScope(tc.s); err == nil {
			t.Errorf("%s: validateScope = nil, want an error", tc.name)
		}
	}

	ok := Scope{
		Base: protocol.ScopeBase_None, VisBasePresent: true, VisBase: protocol.ScopeBase_Subtree,
		IDs:  []string{id},
		Overrides: []ScopeOverride{{Caps: protocol.Capability_ExecControl, Base: protocol.ScopeBase_Subtree}},
	}
	if err := validateScope(ok); err != nil {
		t.Errorf("legal scope rejected: %v", err)
	}
}
```

- [ ] **Step 2: Run to confirm failure**

Run: `go test ./server/ -run 'TestVisRank|TestForCap|TestValidateScope' -v`
Expected: compile failure — `s.VisRank undefined`.

- [ ] **Step 3: Implement**

In `server/scope.go`, extend the struct and add the three functions:

```go
type ScopeOverride struct {
	Caps        protocol.Capability
	Base        protocol.ScopeBase
	ExcludeSelf bool
	IDs         []string
}

type Scope struct {
	Base           protocol.ScopeBase
	VisBase        protocol.ScopeBase
	VisBasePresent bool
	ExcludeSelf    bool
	IDs            []string
	VisIDs         []string
	Overrides      []ScopeOverride
}

// VisRank is the visibility rank: vis_base when present, the action base
// otherwise. The absent case pins the pair to the diagonal, which is why every
// legacy record and the zero value are legal without a check.
func (s Scope) VisRank() protocol.ScopeBase {
	if s.VisBasePresent {
		return s.VisBase
	}
	return s.Base
}

// ForCap resolves the scope one capability sees. Masks are validated disjoint,
// so at most one override matches and the first hit is the only hit.
func (s Scope) ForCap(c protocol.Capability) (protocol.ScopeBase, bool, []string) {
	for _, o := range s.Overrides {
		if o.Caps&c != 0 {
			return o.Base, o.ExcludeSelf, o.IDs
		}
	}
	return s.Base, s.ExcludeSelf, s.IDs
}

// validateScope is design §11's rejection list. Ids are NOT checked here —
// their bound is the parent's set at grant time (attenuateScope), not the
// task's own base.
func validateScope(s Scope) error {
	vis := s.VisRank()
	if scopeBaseRank(s.Base) > scopeBaseRank(vis) {
		return fmt.Errorf("scope base %s outranks visibility %s", s.Base, vis)
	}
	if !s.VisBasePresent && s.VisBase != protocol.ScopeBase(0) {
		return fmt.Errorf("vis_base set while vis_base_present is 0 (non-canonical)")
	}
	var seen protocol.Capability
	for _, o := range s.Overrides {
		if o.Caps == protocol.Capability_None {
			return fmt.Errorf("override with an empty capability mask")
		}
		if seen&o.Caps != 0 {
			return fmt.Errorf("override masks intersect at %s", CapsLabelForMask(seen&o.Caps))
		}
		seen |= o.Caps
		if scopeBaseRank(o.Base) > scopeBaseRank(vis) {
			return fmt.Errorf("override for %s: base %s outranks visibility %s",
				CapsLabelForMask(o.Caps), o.Base, vis)
		}
	}
	return nil
}
```

`CapsLabelForMask` does not exist yet — add it in this task as a thin wrapper so error text names bits rather than numbers:

```go
// CapsLabelForMask renders a mask for error text. cli.CapsLabel takes a single
// bit; this walks the mask.
func CapsLabelForMask(m protocol.Capability) string {
	var parts []string
	for bit := protocol.Capability(1); bit != 0 && bit <= protocol.Capability_All; bit <<= 1 {
		if m&bit != 0 {
			parts = append(parts, bit.String())
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ",")
}
```

- [ ] **Step 4: Run to confirm pass**

Run: `go test ./server/ -run 'TestVisRank|TestForCap|TestValidateScope' -v`
Expected: PASS, all three.

- [ ] **Step 5: Commit**

```bash
git add server/scope.go server/scope_test.go
git commit -m "feat(server): Scope carries visibility, exclude_self and overrides"
```

---

### Task 3: `scopeSet` takes a capability

**Files:**
- Modify: `server/capabilities.go:152` (`scopeSet`), `:188` (`inScope`), `:199` (`authorize`), `:261` (`visibleToCaller`)
- Modify: every non-test call site — enumerate with the command in Step 1
- Test: `server/capabilities_test.go` (exists — extend)

**Interfaces:**
- Consumes: `Scope.ForCap`, `Scope.VisRank` (Task 2).
- Produces: `func (h *TaskHandler) scopeSet(connID string, want protocol.Capability) (all bool, allowed map[string]bool)` — `want == protocol.Capability_None` resolves the VISIBILITY set. `inScope(connID, want, targetHex)`. `authorize` keeps its signature.

- [ ] **Step 1: Enumerate the call sites you must change**

```bash
grep -rn --include='*.go' 'scopeSet(\|inScope(\|visibleToCaller(' server/ | grep -v '_test.go'
```
Expected: 18 lines. Every one of them must compile after Step 3 — the signature change is the enforcement, so do not add a wrapper that preserves the old arity.

- [ ] **Step 2: Write the failing test**

Append to `server/capabilities_test.go`:

```go
// An override must narrow one bit without touching another, and must never
// narrow what ls shows.
func TestScopeSetPerCapability(t *testing.T) {
	h, parent := newHandlerWithTask(t, Scope{
		Base: protocol.ScopeBase_Subtree,
		Overrides: []ScopeOverride{{
			Caps: protocol.Capability_Cancel, Base: protocol.ScopeBase_None,
		}},
	})
	child := spawnChild(t, h, parent)

	if _, allowed := h.scopeSet(parent.conn, protocol.Capability_ExecView); !allowed[child.hex] {
		t.Error("exec_view lost the child; only cancel was overridden")
	}
	if _, allowed := h.scopeSet(parent.conn, protocol.Capability_Cancel); allowed[child.hex] {
		t.Error("cancel reached the child despite an override of base none")
	}
	if _, allowed := h.visibleToCaller(parent.conn); !allowed[child.hex] {
		t.Error("visibility narrowed by an override; overrides bound actions only")
	}
}
```

`newHandlerWithTask` / `spawnChild` are helpers: check `server/capabilities_test.go` for the existing fixture helpers and reuse them rather than adding new ones — match whatever that file already calls them.

- [ ] **Step 3: Run to confirm failure**

Run: `go test ./server/ -run TestScopeSetPerCapability -v`
Expected: compile failure — too many arguments to `h.scopeSet`.

- [ ] **Step 4: Implement**

Rewrite the four functions in `server/capabilities.go`:

```go
func (h *TaskHandler) scopeSet(connID string, want protocol.Capability) (all bool, allowed map[string]bool) {
	pid := h.lookupPrincipal(connID)
	if pid.Id == ([16]byte{}) {
		return true, nil
	}
	callerHex := hex.EncodeToString(pid.Id[:])

	scope := defaultScope()
	if t, ok := h.Tasks.Get(callerHex); ok {
		scope = t.Scope
	}

	// want == Capability_None is the visibility question: the vis rank, the
	// view-only ids, and every action id (a granted id is a disclosed id).
	if want == protocol.Capability_None {
		if scope.VisRank() == protocol.ScopeBase_Global {
			return true, nil
		}
		allowed = map[string]bool{callerHex: true} // self is always visible
		for _, id := range scope.VisIDs {
			allowed[id] = true
		}
		for _, id := range scope.IDs {
			allowed[id] = true
		}
		for _, o := range scope.Overrides {
			for _, id := range o.IDs {
				allowed[id] = true
			}
		}
		if scope.VisRank() == protocol.ScopeBase_Subtree {
			descendantsOf(h.childIndex(), callerHex, allowed)
		}
		return false, allowed
	}

	base, excludeSelf, ids := scope.ForCap(want)
	if base == protocol.ScopeBase_Global {
		return true, nil
	}
	allowed = map[string]bool{}
	if !excludeSelf {
		allowed[callerHex] = true
	}
	for _, id := range ids {
		allowed[id] = true
	}
	if base == protocol.ScopeBase_Subtree {
		descendantsOf(h.childIndex(), callerHex, allowed)
		if excludeSelf {
			delete(allowed, callerHex)
		}
	}
	return false, allowed
}

func (h *TaskHandler) inScope(connID string, want protocol.Capability, targetHex string) bool {
	all, allowed := h.scopeSet(connID, want)
	return all || allowed[targetHex]
}

func (h *TaskHandler) authorize(connID string, want protocol.Capability, targetHex string) bool {
	if !hasCap(h.callerCaps(connID), want) {
		return false
	}
	return h.inScope(connID, want, targetHex)
}

func (h *TaskHandler) visibleToCaller(connID string) (all bool, allowed map[string]bool) {
	if h.lookupPrincipal(connID).Id == ([16]byte{}) {
		return true, nil
	}
	return h.scopeSet(connID, protocol.Capability_None)
}
```

`descendantsOf` seeds `allowed` with self and walks from it, so `exclude_self` must be applied *after* the walk — deleting before would make the walk skip its own root. That is the same trap the existing `visited`/`allowed` split in `descendantsOf` documents.

Then fix each of the 18 call sites: pass the capability the site is already checking. `visibleToCaller` callers change nothing. Sites that call `inScope` directly gain the same argument.

- [ ] **Step 5: Run the tests**

```bash
go test ./server/ -run TestScopeSetPerCapability -v
make vet
```
Expected: PASS, and vet clean for `server/` except `Capability_InfoGlobal` (Task 6).

- [ ] **Step 6: Commit**

```bash
git add server/capabilities.go server/capabilities_test.go
git commit -m "feat(server): resolve scope per capability"
```

---

### Task 4: Attenuation and validation at every write

**Files:**
- Modify: `server/capabilities.go:225` (`attenuateScope`), `:245` (`callerScopeBase`)
- Test: `server/capabilities_test.go`

**Interfaces:**
- Produces: `func (h *TaskHandler) attenuateScope(creatorCID string, req Scope) (out Scope, offender string, ok bool)` — `offender` now names a capability *or* an id.

- [ ] **Step 1: Write the failing test**

```go
func TestAttenuateScopeClampsPerCapability(t *testing.T) {
	h, parent := newHandlerWithTask(t, Scope{
		Base: protocol.ScopeBase_Subtree,
		Overrides: []ScopeOverride{{Caps: protocol.Capability_Cancel, Base: protocol.ScopeBase_None}},
	})

	// The child asks for global on a bit the parent holds at subtree.
	req := Scope{Base: protocol.ScopeBase_Global}
	out, _, ok := h.attenuateScope(parent.conn, req)
	if !ok || out.Base != protocol.ScopeBase_Subtree {
		t.Errorf("base = %v ok=%v, want clamped to subtree", out.Base, ok)
	}

	// An override wider than the child's own visibility is refused outright.
	bad := Scope{
		Base:      protocol.ScopeBase_None,
		Overrides: []ScopeOverride{{Caps: protocol.Capability_ExecControl, Base: protocol.ScopeBase_Subtree}},
	}
	if _, offender, ok := h.attenuateScope(parent.conn, bad); ok {
		t.Errorf("accepted an override outranking visibility (offender %q)", offender)
	}
}
```

- [ ] **Step 2: Run to confirm failure**

Run: `go test ./server/ -run TestAttenuateScopeClamps -v`
Expected: FAIL — the override is accepted.

- [ ] **Step 3: Implement**

Extend `attenuateScope` to clamp per bit and then validate:

```go
func (h *TaskHandler) attenuateScope(creatorCID string, req Scope) (out Scope, offender string, ok bool) {
	out = Scope{
		Base:           req.Base,
		VisBase:        req.VisBase,
		VisBasePresent: req.VisBasePresent,
		ExcludeSelf:    req.ExcludeSelf,
		IDs:            normalizeScopeIDs(req.IDs),
		VisIDs:         normalizeScopeIDs(req.VisIDs),
		Overrides:      append([]ScopeOverride(nil), req.Overrides...),
	}
	for i := range out.Overrides {
		out.Overrides[i].IDs = normalizeScopeIDs(out.Overrides[i].IDs)
	}

	if err := validateScope(out); err != nil {
		return Scope{}, err.Error(), false
	}

	all, _ := h.scopeSet(creatorCID, protocol.Capability_None)
	if all {
		return out, "", true
	}

	parentBase := h.callerScopeBase(creatorCID)
	out.Base = minScopeBase(out.Base, parentBase)
	if out.VisBasePresent {
		out.VisBase = minScopeBase(out.VisBase, parentBase)
	}
	for i := range out.Overrides {
		out.Overrides[i].Base = minScopeBase(out.Overrides[i].Base, parentBase)
	}

	// ids are bounded by the parent's set FOR THAT CAPABILITY.
	for _, o := range out.Overrides {
		_, pAllowed := h.scopeSet(creatorCID, o.Caps)
		for _, id := range o.IDs {
			if !pAllowed[id] {
				return Scope{}, id, false
			}
		}
	}
	_, pAllowedVis := h.scopeSet(creatorCID, protocol.Capability_None)
	for _, id := range append(append([]string{}, out.IDs...), out.VisIDs...) {
		if !pAllowedVis[id] {
			return Scope{}, id, false
		}
	}

	// Clamping may have lowered the visibility rank below an override.
	if err := validateScope(out); err != nil {
		return Scope{}, err.Error(), false
	}
	return out, "", true
}
```

The second `validateScope` is not redundant: `minScopeBase` can lower `VisBase` under a parent while leaving an override where it was.

- [ ] **Step 4: Run to confirm pass**

Run: `go test ./server/ -run TestAttenuateScope -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add server/capabilities.go server/capabilities_test.go
git commit -m "feat(server): attenuate and validate scope per capability"
```

---

### Task 5: WAL persistence and the additive migration

**Files:**
- Modify: `server/wal.go:24` (`WALEvent`)
- Modify: `server/scope.go:137` (`scopeFromWAL`) and its write-side counterpart at `server/taskstore.go:202` and `:229`
- Modify: `server/taskstore.go:810`, `:888` (replay)
- Test: `server/wal_test.go` (exists — extend)

**Interfaces:**
- Produces: `WALEvent.ScopeVisBase`, `.ScopeVisBasePresent`, `.ScopeExcludeSelf`, `.ScopeVisIDs`, `.ScopeOverrides []WALScopeOverride`; `scopeFromWAL(ev WALEvent) Scope` (signature changes from `(base uint8, ids []string)` — update both call sites).

- [ ] **Step 1: Write the failing test**

```go
// A record written before this change must replay as today's authority: self
// included, visibility following the base — and its capability bits intact.
func TestReplayOfLegacyRecordIsAdditive(t *testing.T) {
	ev := WALEvent{
		Type:         "task_created",
		TaskID:       "aa112233445566778899aabbccddeeff",
		Capabilities: uint32(protocol.Capability_BoardObserve | protocol.Capability_Cancel),
		ScopeBase:    uint8(protocol.ScopeBase_Subtree),
	}
	s := scopeFromWAL(ev)
	if s.ExcludeSelf {
		t.Error("ExcludeSelf = true on a legacy record; self was unconditional")
	}
	if s.VisBasePresent {
		t.Error("VisBasePresent = true on a legacy record; visibility followed the base")
	}
	if got := replayCaps(ev); got != protocol.Capability(ev.Capabilities) {
		t.Errorf("caps = %v, want %v — replay must never clear a bit", got, ev.Capabilities)
	}
}

// The one non-zero migration: the legacy bit implies global visibility, and
// the bit itself is KEPT.
func TestLegacyBoardObserveGrantsGlobalVisibility(t *testing.T) {
	ev := WALEvent{
		Type:         "task_created",
		Capabilities: uint32(protocol.Capability_BoardObserve),
		ScopeBase:    uint8(protocol.ScopeBase_None),
	}
	s := scopeFromWAL(ev)
	if s.VisRank() != protocol.ScopeBase_Global {
		t.Errorf("VisRank = %v, want global", s.VisRank())
	}
	if replayCaps(ev)&protocol.Capability_BoardObserve == 0 {
		t.Error("replay cleared board_observe; migration must be additive")
	}
}
```

`replayCaps` is a new one-line helper in `server/taskstore.go` that both replay sites already do inline — factor it out so the test can assert on it.

- [ ] **Step 2: Run to confirm failure**

Run: `go test ./server/ -run 'TestReplayOfLegacy|TestLegacyBoardObserve' -v`
Expected: compile failure — `scopeFromWAL` takes two args.

- [ ] **Step 3: Implement**

Add to `WALEvent` (keep every existing tag, add these):

```go
	// ScopeVisBase / ScopeVisBasePresent / ScopeExcludeSelf / ScopeVisIDs /
	// ScopeOverrides extend the stored TaskScope. Every one of them replays
	// correctly from its JSON zero value, which is why the two flags are
	// phrased by-presence and negatively — a legacy record has none of these
	// keys and must mean "visibility follows the base, self included".
	ScopeVisBase        uint8              `json:"scope_vis_base,omitempty"`
	ScopeVisBasePresent bool               `json:"scope_vis_base_present,omitempty"`
	ScopeExcludeSelf    bool               `json:"scope_exclude_self,omitempty"`
	ScopeVisIDs         []string           `json:"scope_vis_ids,omitempty"`
	ScopeOverrides      []WALScopeOverride `json:"scope_overrides,omitempty"`
```

```go
// WALScopeOverride is one ScopeOverride in JSON. Caps is the numeric mask so a
// capability rename never invalidates a record — the same reason Kind and
// OriginKind are numbers.
type WALScopeOverride struct {
	Caps        uint32   `json:"caps"`
	Base        uint8    `json:"base,omitempty"`
	ExcludeSelf bool     `json:"exclude_self,omitempty"`
	IDs         []string `json:"ids,omitempty"`
}
```

Rewrite `scopeFromWAL`:

```go
// scopeFromWAL reconstructs a Scope from a record. The migration here is
// ADDITIVE: a legacy record whose caps carry board_observe (formerly
// info_global) gains global visibility, and its capability mask is returned
// untouched by replayCaps. Clearing the bit would silently revoke board
// observation at the moment of a restart, with nothing in the WAL to say why.
func scopeFromWAL(ev WALEvent) Scope {
	s := Scope{
		Base:           protocol.ScopeBase(ev.ScopeBase),
		VisBase:        protocol.ScopeBase(ev.ScopeVisBase),
		VisBasePresent: ev.ScopeVisBasePresent,
		ExcludeSelf:    ev.ScopeExcludeSelf,
		IDs:            normalizeScopeIDs(ev.ScopeIDs),
		VisIDs:         normalizeScopeIDs(ev.ScopeVisIDs),
	}
	for _, o := range ev.ScopeOverrides {
		s.Overrides = append(s.Overrides, ScopeOverride{
			Caps:        protocol.Capability(o.Caps),
			Base:        protocol.ScopeBase(o.Base),
			ExcludeSelf: o.ExcludeSelf,
			IDs:         normalizeScopeIDs(o.IDs),
		})
	}
	if !s.VisBasePresent && protocol.Capability(ev.Capabilities)&protocol.Capability_BoardObserve != 0 {
		s.VisBasePresent = true
		s.VisBase = protocol.ScopeBase_Global
	}
	return s
}

// replayCaps is the capability mask a record restores. It exists as its own
// function so the "migration never clears a bit" rule has something to assert
// against.
func replayCaps(ev WALEvent) protocol.Capability { return protocol.Capability(ev.Capabilities) }
```

Update the write sites (`server/taskstore.go:202`, `:229`) to populate the new fields, and the two replay sites (`:810`, `:888`) to call `scopeFromWAL(ev)`.

- [ ] **Step 4: Run the tests**

Run: `go test ./server/ -run 'TestReplay|TestLegacy|TestWAL' -v`
Expected: PASS, including the pre-existing WAL tests.

- [ ] **Step 5: Commit**

```bash
git add server/wal.go server/scope.go server/taskstore.go server/wal_test.go
git commit -m "feat(server): persist the scope axes; migration is additive"
```

---

### Task 5b: `caps set` cascade and resume carry the new fields

**Files:**
- Modify: `server/task_handler.go` (the `set_caps` handler — find with `grep -n 'SetCapsRequest\|handleSetCaps' server/task_handler.go`)
- Modify: `server/taskstore.go` (`SetCaps`)
- Modify: the two resume paths — `handleSubmitResume` and its interactive twin
- Test: `server/capabilities_test.go`

**Interfaces:**
- Consumes: `validateScope`, `Scope.ForCap` (Task 2); `attenuateScope` (Task 4).
- Produces: no new exported names — the existing `scope_present` bit now governs the whole scope half, overrides included.

Spec §10. Three behaviours, none of which the earlier tasks cover.

- [ ] **Step 1: Write the failing tests**

```go
// scope_present governs the WHOLE scope half. An empty override list under a
// set scope_present is the only way to clear overrides, exactly as an empty
// ids list is the only way to clear ids.
func TestSetCapsClearsOverridesWhenScopePresent(t *testing.T) {
	h, task := newHandlerWithTask(t, Scope{
		Base:      protocol.ScopeBase_Subtree,
		Overrides: []ScopeOverride{{Caps: protocol.Capability_Cancel, Base: protocol.ScopeBase_None}},
	})
	if err := h.Tasks.SetCaps(task.hex, protocol.Capability_All, Scope{Base: protocol.ScopeBase_Subtree}); err != nil {
		t.Fatalf("SetCaps: %v", err)
	}
	got, _ := h.Tasks.Get(task.hex)
	if len(got.Scope.Overrides) != 0 {
		t.Errorf("overrides = %+v, want cleared", got.Scope.Overrides)
	}
}

// Cascade clamps a descendant's override against the target's NEW effective
// scope for that bit, and drops an override for a bit the descendant no
// longer holds.
func TestCascadeClampsOverridesPerBit(t *testing.T) {
	h, parent := newHandlerWithTask(t, Scope{Base: protocol.ScopeBase_Subtree})
	child := spawnChildWithScope(t, h, parent, Scope{
		Base:      protocol.ScopeBase_Subtree,
		Overrides: []ScopeOverride{{Caps: protocol.Capability_Cancel, Base: protocol.ScopeBase_Subtree}},
	})

	// Narrow the parent to none, cascading.
	if err := h.cascadeClamp(parent.hex, protocol.Capability_All, Scope{Base: protocol.ScopeBase_None}); err != nil {
		t.Fatalf("cascadeClamp: %v", err)
	}
	got, _ := h.Tasks.Get(child.hex)
	for _, o := range got.Scope.Overrides {
		if scopeBaseRank(o.Base) > scopeBaseRank(protocol.ScopeBase_None) {
			t.Errorf("override %+v outranks the parent's new scope after cascade", o)
		}
	}
}

// Resume re-grants the scope half iff scope_present, independently of
// resume_caps_override — the base spec's 2026-08-13 amendment, extended to
// overrides for the same reason it covered ids.
func TestResumeRegrantsOverridesOnlyWhenScopePresent(t *testing.T) {
	h, task := newHandlerWithTask(t, Scope{
		Base:      protocol.ScopeBase_Subtree,
		Overrides: []ScopeOverride{{Caps: protocol.Capability_Cancel, Base: protocol.ScopeBase_None}},
	})
	resumeWithoutScopePresent(t, h, task)
	got, _ := h.Tasks.Get(task.hex)
	if len(got.Scope.Overrides) != 1 {
		t.Error("resume without scope_present rewrote the overrides; it must keep them")
	}
}
```

`spawnChildWithScope` / `resumeWithoutScopePresent` are fixtures — check `server/capabilities_test.go` for the existing helpers and extend those rather than adding a parallel set.

- [ ] **Step 2: Run to confirm failure**

Run: `go test ./server/ -run 'TestSetCapsClears|TestCascadeClamps|TestResumeRegrants' -v`
Expected: FAIL — `SetCaps` takes the old `Scope`, `cascadeClamp` undefined.

- [ ] **Step 3: Implement**

- `TaskStore.SetCaps` takes the full `Scope` and writes every field, including `Overrides`, to the store and the `task_caps_changed` WAL event (fields added in Task 5).
- Factor the cascade walk into `func (h *TaskHandler) cascadeClamp(targetHex string, caps protocol.Capability, s Scope) error`. For each descendant, in addition to the two clamps that exist today:

```go
	// Third clamp: an override may not outrank the target's new effective
	// scope for the same bit, and an override for a bit the descendant no
	// longer holds is dropped rather than left to reapply if the bit returns.
	kept := child.Scope.Overrides[:0]
	for _, o := range child.Scope.Overrides {
		o.Caps &= child.Capabilities
		if o.Caps == protocol.Capability_None {
			continue
		}
		newBase, _, _ := s.ForCap(o.Caps)
		o.Base = minScopeBase(o.Base, newBase)
		kept = append(kept, o)
	}
	child.Scope.Overrides = kept
```

- In both resume paths, gate the scope half on `scope_present` alone. The bug the 2026-08-13 amendment fixed was passing the caps override bit twice; do not reintroduce it by reading `resume_caps_override` for the overrides.

- [ ] **Step 4: Run**

Run: `go test ./server/ -v`
Expected: PASS, including the pre-existing `caps set` and resume tests.

- [ ] **Step 5: Commit**

```bash
git add server/task_handler.go server/taskstore.go server/capabilities_test.go
git commit -m "feat(server): caps set and resume carry the override list"
```

---

### Task 6: `info_global` → `board_observe`, and the conn surfaces move to the axis

**Files:**
- Modify: `server/capabilities.go:38-41` (`requiredCap` board rows — comment only, the value is unchanged)
- Modify: `server/task_handler.go:1610` (`ListConns`), `server/server.go:363` (conns_status fanout)
- Modify: `server/agent_handler.go:584` (agent-side `list_topics`)
- Modify: `cli/caps.go:34` (catalogue), `:75` (`CapsLabel` case), `:231` (`ParseCaps` — rename, no alias)
- Test: `server/capabilities_test.go`, `cli/caps_test.go`

**Interfaces:**
- Consumes: `protocol.Capability_BoardObserve` (Task 1), `scopeSet(_, Capability_None)` (Task 3).

- [ ] **Step 1: Write the failing tests**

```go
// The conn surfaces are task visibility, so they follow the axis and read no
// capability bit.
func TestListConnsFollowsVisibilityAxis(t *testing.T) {
	h, task := newHandlerWithTask(t, Scope{
		Base: protocol.ScopeBase_None, VisBasePresent: true, VisBase: protocol.ScopeBase_Global,
	})
	if all, _ := h.visibleToCaller(task.conn); !all {
		t.Error("vis_base global did not widen the conn/task visibility set")
	}
}
```

```go
// cli/caps_test.go — the old name still parses, and grants the same bit.
func TestParseCapsRejectsTheOldName(t *testing.T) {
	if _, err := ParseCaps("info_global"); err == nil {
		t.Error("ParseCaps(info_global) = nil error; the old name is gone, not aliased")
	}
	got, err := ParseCaps("board_observe")
	if err != nil {
		t.Fatalf("ParseCaps(board_observe): %v", err)
	}
	if got != protocol.Capability_BoardObserve {
		t.Errorf("= %v, want board_observe", got)
	}
}
```

- [ ] **Step 2: Run to confirm failure**

Run: `go test ./server/ ./cli/ -run 'TestListConnsFollows|TestParseCapsRejectsTheOldName' -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

- `server/task_handler.go:1610` — delete the `hasInfoGlobal` local and let `ConnListFn` take `isOperator || all` from `h.visibleToCaller(connID)`.
- `server/server.go:363` — drop the `hasCap(..., InfoGlobal)` branch; the `visibleToCaller` call two lines below already answers it.
- `server/agent_handler.go:584` — replace the constant with `protocol.Capability_BoardObserve`; update the comment, which currently says "(visibility scope)" and is now wrong: this gate is board observation, not task visibility.
- `cli/caps.go` — rename the catalogue entry and its description to the spec's text:

```go
	case protocol.Capability_BoardObserve:
		return "list board topics, read a topic's retained messages, and list its subscribers. " +
			"Not required to send, subscribe, or read your own inbox"
```

- `ParseCaps` — **rename only; do not add a deprecated alias.** This project is single-developer dogfood with no external users, and the standing rule is that renames are quality fixes rather than migrations: no shims, no deprecation periods, no back-compat layers for users who do not exist. `--caps info_global` becomes an unknown-capability error, which is the correct and immediate signal.

  The WAL migration in Task 5 is a different thing and stays: it reads records that already exist on disk, so it is data handling, not a compatibility shim for a caller.

- [ ] **Step 4: Run**

```bash
go test ./server/ ./cli/ -v
make vet
```
Expected: PASS; `make vet` now has zero `InfoGlobal` references — check against `/tmp/infoglobal-sites.txt` from Task 1.

- [ ] **Step 5: Commit**

```bash
git add server/ cli/caps.go cli/caps_test.go
git commit -m "feat: info_global becomes board_observe; conn visibility moves to the axis"
```

---

### Task 7: `cli/scope.go` — grammar for the new axes

**Files:**
- Modify: `cli/scope.go:32` (`ScopeGrammar`), `:36` (`ParseScope`), `:106` (`ScopeLabel`)
- Test: `cli/scope_test.go` (exists — extend)

**Interfaces:**
- Produces: grammar `[<vis>/]<act> [+ids:…] [+vis-ids:…]`, the word `descendants` (= `subtree` with `exclude_self`), and `ParseScopeFor(string) (protocol.Capability, protocol.TaskScope, error)` for `--scope-for`.

- [ ] **Step 1: Write the failing tests**

```go
func TestParseScopeVisibilityPair(t *testing.T) {
	s, err := ParseScope("global/subtree")
	if err != nil {
		t.Fatalf("ParseScope: %v", err)
	}
	if s.Base != protocol.ScopeBase_Subtree || s.VisBase != protocol.ScopeBase_Global || !s.VisBasePresent() {
		t.Errorf("= %+v, want act subtree / vis global with the presence bit set", s)
	}
}

func TestParseScopeDescendantsSetsTheFlag(t *testing.T) {
	s, err := ParseScope("descendants")
	if err != nil {
		t.Fatalf("ParseScope: %v", err)
	}
	if s.Base != protocol.ScopeBase_Subtree || !s.ExcludeSelf() {
		t.Errorf("= %+v, want subtree with exclude_self — descendants is a UI word, not a base value", s)
	}
}

func TestScopeLabelRoundTrips(t *testing.T) {
	for _, in := range []string{"subtree", "none", "global", "descendants", "global/none", "ids:" + strings.Repeat("ab", 16)} {
		s, err := ParseScope(in)
		if err != nil {
			t.Fatalf("ParseScope(%q): %v", in, err)
		}
		if got := ScopeLabel(s); got != in {
			t.Errorf("ScopeLabel(ParseScope(%q)) = %q", in, got)
		}
	}
}
```

- [ ] **Step 2: Run to confirm failure**

Run: `go test ./cli/ -run 'TestParseScopeVisibility|TestParseScopeDescendants|TestScopeLabelRoundTrips' -v`
Expected: FAIL — `unknown scope base "global/subtree"`.

- [ ] **Step 3: Implement**

Split on `/` first (`vis/act`, act-only when absent), then reuse the existing base+ids parsing for each half. `descendants` maps to `Base: subtree, ExcludeSelf: true`. Set `VisBasePresent` **only** when the pair form was written, so `ParseScope("none")` stays canonical (design §11 rule 5). Update `ScopeGrammar` to:

```go
const ScopeGrammar = "[<visibility>/]<action> where each is subtree (default) | descendants | none | global, " +
	"plus [+ids:<task-id>[,<task-id>]] and [+vis-ids:<task-id>[,<task-id>]]"
```

Add `ParseScopeFor` for the `--scope-for caps=scope` form, splitting on the first `=`, parsing the left with `ParseCaps` (which already accepts comma lists).

- [ ] **Step 4: Run**

Run: `go test ./cli/ -v`
Expected: PASS, including the pre-existing scope tests — they cover the forms that must not change.

- [ ] **Step 5: Commit**

```bash
git add cli/scope.go cli/scope_test.go
git commit -m "feat(cli): scope grammar carries visibility, descendants and vis-ids"
```

---

### Task 8: CLI flags, rendering and `caps set`

**Files:**
- Modify: `cli/set_caps.go`, and the flag wiring for `submit` / `interactive` / `session new` (find with `grep -rn '"scope"' cmd/harness-cli/`)
- Modify: `cli/caps.go:182` (`CapsLabel`) — no change expected, verify only
- Test: `cli/set_caps_test.go` if present, else `cli/scope_test.go`

**Interfaces:**
- Consumes: `ParseScope`, `ParseScopeFor` (Task 7).
- Produces: `--scope-for` (repeatable) on every verb that takes `--scope`; `scope` in `ls --json` / `whoami` / `session ls` as the fully-resolved capability→scope map.

- [ ] **Step 1: Write the failing test**

```go
func TestScopeForFlagsMergeIntoOverrides(t *testing.T) {
	s, err := ParseScope("subtree")
	if err != nil {
		t.Fatal(err)
	}
	cap, ov, err := ParseScopeFor("exec_cowrite,file_write=descendants")
	if err != nil {
		t.Fatalf("ParseScopeFor: %v", err)
	}
	s = MergeScopeOverride(s, cap, ov)
	if len(s.Overrides) != 1 || s.Overrides[0].Caps != (protocol.Capability_ExecCowrite|protocol.Capability_FileWrite) {
		t.Errorf("overrides = %+v, want one entry covering both bits", s.Overrides)
	}
}
```

- [ ] **Step 2: Run to confirm failure**

Run: `go test ./cli/ -run TestScopeForFlags -v`
Expected: compile failure — `MergeScopeOverride` undefined.

- [ ] **Step 3: Implement**

Add `MergeScopeOverride(s protocol.TaskScope, caps protocol.Capability, ov protocol.TaskScope) protocol.TaskScope` to `cli/scope.go`; it appends one `ScopeOverride` and returns the scope. Reject an intersecting mask client-side with the same message the server uses, so the round trip is not needed to learn about a typo.

Wire a repeatable `--scope-for` `flag.Value` into each verb beside its existing `--scope`. Extend `ScopeLabel` output in `ls`, `whoami` and `session ls`, and emit the resolved map in the `--json` variants.

- [ ] **Step 4: Run**

```bash
go test ./cli/ -v
make check
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cli/ cmd/harness-cli/
git commit -m "feat(cli): --scope-for, and render the resolved per-capability scope"
```

---

### Task 9: TUI

**Files:**
- Modify: `tui/authoritypicker.go` (the caps/scope picker behind `a`), `tui/cmdline.go` (`caps` / `scope` commands), `tui/detail.go` (the `d` popup)
- Test: `tui/authoritypicker_test.go`, `tui/cmdline_test.go`, `tui/app_regrant_test.go`

**Interfaces:**
- Consumes: `cli.ParseScope`, `cli.ParseScopeFor`, `cli.ScopeLabel`.

- [ ] **Step 1: Write the failing test**

```go
func TestAuthorityPickerRendersVisibilityAndOverrides(t *testing.T) {
	s, err := cli.ParseScope("global/subtree")
	if err != nil {
		t.Fatal(err)
	}
	m := newAuthorityPicker(s, protocol.Capability_All)
	out := m.View()
	if !strings.Contains(out, "global/subtree") {
		t.Errorf("picker does not show the visibility pair:\n%s", out)
	}
}
```

Check `tui/authoritypicker_test.go` for the constructor's real name before writing this — match it rather than introducing `newAuthorityPicker` if it is called something else.

- [ ] **Step 2: Run to confirm failure**

Run: `go test ./tui/ -run TestAuthorityPickerRenders -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Add a visibility row and a per-capability section to the picker, collapsed by default so a single-scope grant renders as it does today. Accept `--scope-for` in the `caps` / `scope` cmdline commands.

- [ ] **Step 4: Run**

Run: `go test ./tui/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tui/
git commit -m "feat(tui): visibility pair and per-capability overrides in the picker"
```

---

### Task 10: WebUI

**Files:**
- Modify: `cmd/harness-webui-wasm/main.go`, `webui/static/main.js`, `webui/index.html`, `webui/static/style.css`

**Interfaces:**
- Consumes: the same wire fields; the wasm client already decodes `TaskInfo`.

- [ ] **Step 1: Add the scope inputs**

Extend the spawn dialog and the task sheet's 「🔑 caps/scope 再付与」 action with a visibility field and a collapsed per-capability section. Render the resolved scope in the task rows.

- [ ] **Step 2: Build and check**

```bash
make wasm-check
make webui-build
```
Expected: both clean.

- [ ] **Step 3: Verify in a browser at both widths**

The WebUI is dark-themed and used on phones. Check desktop **and** 390px. The server runs with `--webui-dir`, so assets hot-reload — do not restart it.

- [ ] **Step 4: Commit**

```bash
git add cmd/harness-webui-wasm/ webui/
git commit -m "feat(webui): visibility pair and per-capability overrides"
```

---

### Task 11: Completeness tests

**Files:**
- Modify: `server/scope_completeness_test.go` (exists), `cli/caps_completeness_test.go` (exists)

- [ ] **Step 1: Write the failing test**

```go
// Today's version catches an unwired KIND. The gap this closes is a new BIT
// that silently resolves through the base scope because no authorize call site
// ever passes it.
func TestEveryCapabilityIsPassedToAuthorize(t *testing.T) {
	passed := capabilitiesPassedToAuthorize(t) // parses server/*.go for authorize( calls
	for bit := protocol.Capability(1); bit <= protocol.Capability_All; bit <<= 1 {
		if bit == protocol.Capability_BoardObserve {
			continue // gates kinds via requiredCap, never a target task
		}
		if protocol.Capability_All&bit == 0 {
			continue
		}
		if !passed[bit] {
			t.Errorf("%s is never passed to authorize — it resolves through the base scope silently", bit)
		}
	}
}
```

- [ ] **Step 2: Run, implement the AST walk, run again**

Run: `go test ./server/ -run TestEveryCapabilityIsPassed -v`
Model `capabilitiesPassedToAuthorize` on whatever the existing completeness test already does to enumerate kinds — reuse its parsing approach rather than inventing a second one.

- [ ] **Step 3: Commit**

```bash
git add server/scope_completeness_test.go cli/caps_completeness_test.go
git commit -m "test: a capability never passed to authorize fails the build"
```

---

### Task 12: The §11 matrix as a table test

**Files:**
- Create: `server/scope_matrix_test.go`

- [ ] **Step 1: Write the table**

One case per row of design §11: the nine rank pairs with their verdicts, the two override-rank rows, every "accepted, no effect" row, and the seven rejections. Assert the *resolved set*, not just the error.

```go
func TestScopeMatrix(t *testing.T) {
	cases := []struct {
		name    string
		scope   Scope
		wantErr bool
	}{
		{"none/none", Scope{Base: protocol.ScopeBase_None, VisBasePresent: true, VisBase: protocol.ScopeBase_None}, false},
		{"none/subtree", Scope{Base: protocol.ScopeBase_None, VisBasePresent: true, VisBase: protocol.ScopeBase_Subtree}, false},
		{"none/global", Scope{Base: protocol.ScopeBase_None, VisBasePresent: true, VisBase: protocol.ScopeBase_Global}, false},
		{"subtree/subtree", Scope{Base: protocol.ScopeBase_Subtree}, false},
		{"subtree/global", Scope{Base: protocol.ScopeBase_Subtree, VisBasePresent: true, VisBase: protocol.ScopeBase_Global}, false},
		{"global/global", Scope{Base: protocol.ScopeBase_Global, VisBasePresent: true, VisBase: protocol.ScopeBase_Global}, false},
		{"subtree act, none vis", Scope{Base: protocol.ScopeBase_Subtree, VisBasePresent: true, VisBase: protocol.ScopeBase_None}, true},
		{"global act, none vis", Scope{Base: protocol.ScopeBase_Global, VisBasePresent: true, VisBase: protocol.ScopeBase_None}, true},
		{"global act, subtree vis", Scope{Base: protocol.ScopeBase_Global, VisBasePresent: true, VisBase: protocol.ScopeBase_Subtree}, true},
	}
	for _, tc := range cases {
		err := validateScope(tc.scope)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, wantErr = %v", tc.name, err, tc.wantErr)
		}
	}
}

// The invariant, over randomised grants: no capability may act outside what ls
// shows. This is the assertion that catches the axes being collapsed in either
// direction.
func TestEffectiveIsAlwaysWithinVisible(t *testing.T) {
	rng := rand.New(rand.NewSource(20260820)) // fixed seed: a failure must reproduce
	bases := []protocol.ScopeBase{
		protocol.ScopeBase_None, protocol.ScopeBase_Subtree, protocol.ScopeBase_Global,
	}
	bits := []protocol.Capability{
		protocol.Capability_Cancel, protocol.Capability_ExecView,
		protocol.Capability_ExecCowrite, protocol.Capability_FileRead,
		protocol.Capability_FileWrite,
	}

	for i := 0; i < 200; i++ {
		s := Scope{
			Base:           bases[rng.Intn(len(bases))],
			VisBasePresent: rng.Intn(2) == 1,
			ExcludeSelf:    rng.Intn(2) == 1,
		}
		if s.VisBasePresent {
			s.VisBase = bases[rng.Intn(len(bases))]
		}
		// Disjoint masks: hand each chosen bit to at most one override.
		perm := rng.Perm(len(bits))
		for n := 0; n < rng.Intn(3); n++ {
			s.Overrides = append(s.Overrides, ScopeOverride{
				Caps:        bits[perm[n]],
				Base:        bases[rng.Intn(len(bases))],
				ExcludeSelf: rng.Intn(2) == 1,
			})
		}
		if err := validateScope(s); err != nil {
			continue // illegal by construction; the matrix test covers those
		}

		h, task := newHandlerWithTask(t, s)
		_, visible := h.scopeSet(task.conn, protocol.Capability_None)
		if visible == nil {
			continue // vis rank global: everything is visible, inclusion is trivial
		}
		for _, bit := range bits {
			all, allowed := h.scopeSet(task.conn, bit)
			if all {
				t.Fatalf("case %d: %s resolved to ALL while visibility is bounded — "+
					"an action set escaped the visibility set", i, bit)
			}
			for id := range allowed {
				if !visible[id] {
					t.Fatalf("case %d: %s reaches %s, which visibility does not include\nscope: %+v",
						i, bit, id, s)
				}
			}
		}
	}
}
```

- [ ] **Step 2: Run**

Run: `go test ./server/ -run 'TestScopeMatrix|TestEffectiveIsAlways' -v`
Expected: PASS. The seed is fixed, so a failure names a reproducible case index and prints the scope that produced it.

- [ ] **Step 3: Commit**

```bash
git add server/scope_matrix_test.go
git commit -m "test: the design's legal-value matrix, as a table"
```

---

### Task 13: Wire-skew assertions

**Files:**
- Modify: `runner/protocol/scope_wire_test.go` (Task 1)

- [ ] **Step 1: Assert the skew in both directions**

A hand-built pre-change `SubmitRequest` payload must be rejected by the new decoder with `not enough data`; a new-layout payload must be rejected by a decoder truncated to the old field list with `expect no remaining bytes`. Model these on the existing `runner/protocol/file_transfer_wire_test.go` / `agent_profile_wire_test.go`.

Also assert: a `ScopeOverride` with an empty mask and two intersecting masks are rejected by `validateScope` — at spawn and at `caps set`, not only at decode.

- [ ] **Step 2: Run the project's wire-skew guard**

```bash
scripts/wire-skew-check.sh
```

`OLD_REF` defaults to the merge-base with `origin/main` — what is actually
deployed. The script builds both sides and runs NEW runner × OLD server, and it
asserts the failure is **recoverable** (rejected → retries → self-heals once the
server is upgraded), not that skew works; this project has no compat shims by
design. It exits 2 on a setup error rather than passing.

This is not optional diligence. A `.bgn` change landed against an old server
once killed all 12 runner slots in about a second, and none came back
(`d4f7a5a`). The runner has its **own** handshake in `runner/connect.go`, which
is how the previous fix was missed by a `cli/`-scoped grep.

Run the negative control before trusting a PASS: a guard that cannot fail is
worse than none. The first version of this script passed on everything because a
fresh worktree has no `webui/static/main.wasm` and the old server never started.

- [ ] **Step 3: Run the unit tests and commit**

```bash
go test ./runner/protocol/ -v
git add runner/protocol/scope_wire_test.go
git commit -m "test: assert the wire skew rather than assuming it"
```

---

### Task 14: Integration, on a dummy harness

**Files:**
- Modify: `integration/` (add a scope case beside the existing e2e tests)

- [ ] **Step 1: Bring up a dummy instance**

```bash
scripts/dummy-harness.sh up --detach --agent fake --name scope-e2e
```
Evaluate the env it prints. Tear it down with `scripts/dummy-harness.sh down` when finished.

- [ ] **Step 2: Exercise the three cases the spec names**

- A child with `exec_view` at `subtree` and `exec_cowrite` with `exclude_self`: `session snapshot` on itself works, `session send` to itself answers `no_such_task`, and both work against a child of its own.
- `base=none, vis_base=none` + `override{cancel, ids:X}`: `ls` shows `X`, `cancel X` works, `cancel Y` answers `no_such_task`.
- `vis_ids:[X]`: `X` is listed and every action against it is refused.

- [ ] **Step 3: Run and commit**

```bash
make test-integration
git add integration/
git commit -m "test(integration): per-capability scope end to end"
```

---

### Task 14b: Deploy gate — prove a client can get INTO a task, on new binaries

**This is a hard gate. Without a written PASS here, the deploy does not happen.**

Nothing in Tasks 1-14 proves the thing that matters most after a wire break:
that there is still a way in. Unit tests run in-process; the integration suite
drives a fake agent. Neither exercises a real client opening a real session
against a real server — the path that fails by **hanging**, silently, on a
deadline-less context. The failure guarded against is not "a feature
regressed", it is "the fleet is upgraded and nothing can attach to anything",
with the recovery path itself being the broken one.

**Files:** none — a procedure against a throwaway instance.

- [ ] **Step 1: Load the dummy-harness skill first**

Invoke the `dummy-harness` skill before running anything. It documents the
environment traps that make a dummy instance fail in *misleading* ways; misread
one and this gate reports a false red, or worse a false green.

- [ ] **Step 2: Bring up a dummy instance, both ends on the NEW binaries**

```bash
scripts/dummy-harness.sh up --detach --agent fake --name deploy-gate
```

Evaluate the env it prints. A dummy whose server is stale tests nothing — check
that the server it started came from this branch's build.

- [ ] **Step 3: Prove entry, with real keystrokes**

```bash
harness-cli session new -d --repo <dummy repo> --rows 40 --cols 120
harness-cli session ls
harness-cli session exec <task-id> 'echo ENTRY-PROOF-$$'
harness-cli session snapshot <task-id>
```

The snapshot must contain the echoed nonce. **Rendering is not proof — feed
real input and read the result back**: a session that draws but accepts no
keystrokes looks identical in a screenshot. Match on the nonce, never on cursor
position — Enter echoes as a bare CR in a PTY, so position-based matching lies.

- [ ] **Step 4: Prove entry survives the restart the deploy performs**

```bash
scripts/dummy-harness.sh restart-server --name deploy-gate
harness-cli ls                      # the task should be Failed/server_restart
harness-cli session new --repo <dummy repo> --resume <task-id>
harness-cli session exec <task-id> 'echo RESUME-PROOF-$$'
```

Recovery travels on `SubmitRequest` / `OpenInteractiveRequest` — the two
formats this change breaks — so this step, not Step 3, is the one that would
have caught the historical fleet wipe. If `restart-server` is not a subcommand
of the script, restart it by whatever means the dummy-harness skill documents;
do not skip the step.

- [ ] **Step 5: Prove a second client kind gets in**

Repeat Step 3's entry from the TUI (`bin/harness-tui`, `S` opens a session)
against the same dummy server. One working client is the minimum asked for; a
second is what distinguishes "the wire is fine" from "the CLI happens to work".

- [ ] **Step 6: Tear down and write the result down**

```bash
scripts/dummy-harness.sh down --name deploy-gate
```

Record in the handoff notes: which client kinds were proven, the nonces
observed, and whether resume worked. Task 15's deploy proceeds only on that
written PASS.

---

### Task 15: Documentation and the upgrade rehearsal

**Files:**
- Modify: `README.md` (the **Capabilities and scope** section)
- Modify: `runner/agentskills/supervising-workers/SKILL.md` — **the `go:embed` source of truth** — then mirror to `.claude/skills/supervising-workers/SKILL.md` and `.agents/skills/supervising-workers/SKILL.md`
- Modify: `runner/agentskills/harness-cli/SKILL.md` + mirrors, for `board_observe`

- [ ] **Step 1: Update the docs**

Document the visibility pair, `descendants`, `--scope-for`, `vis-ids`, and the `board_observe` rename with its "not required to send, subscribe, or read your own inbox" boundary.

- [ ] **Step 2: Full verification**

```bash
make check && make wasm-check && make vet && make test && make test-integration
```
Expected: all clean. **Do not restart anything yet.**

- [ ] **Step 3: Rehearse the upgrade, in the spec's order**

This is the gate, not a formality — recovery from the restart travels on the two formats this change breaks, so an un-rebuilt client fails exactly when it is needed, and fails by hanging.

1. `make build` everywhere a client binary lives; rebuild the wasm.
2. **Task 14b's written PASS must already exist.** Do not re-derive it here and do not proceed without it.
3. Restart the real server, then the runners: `scripts/build_and_restart_all.py` (self-last).
4. Resume the sessions the restart failed, with the rebuilt client.
5. Hard-reload every WebUI tab — a cached pre-change wasm is an old client and will hang.

- [ ] **Step 4: Commit**

```bash
git add README.md runner/agentskills/ .claude/skills/ .agents/skills/
git commit -m "docs: per-capability scope, visibility pair, board_observe"
```

---

## Notes for the executor

- **The signature change is the design.** If a call site is awkward to thread a capability through, that awkwardness is information: it usually means the site is resolving a target without knowing which verb it is for. Do not add an arity-preserving wrapper to make it compile.
- **`descendants` is a UI word.** It never appears on the wire. If you find yourself adding a `ScopeBase` value for it, re-read design §5.
- **Two `validateScope` calls in `attenuateScope` are deliberate** (Task 4, Step 3).
- **Never restart the server or runners before Task 15.** The wire break is silent on the client side.
