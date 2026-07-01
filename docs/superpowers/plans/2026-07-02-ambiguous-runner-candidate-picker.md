# AmbiguousRunner Candidate Picker Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** On an `AmbiguousRunner` result when opening an interactive session (fresh or resume), carry the candidate runners back to the client and let the user pick one instead of failing.

**Architecture:** The server already computes candidates (`Registry.Candidates`) but discards them on the ambiguous branch. We extend the `OpenInteractiveResponse` wire schema with a conditional candidates block (present only when `status == ambiguous_runner`), populate it server-side, surface it through a typed `cli.AmbiguousRunnerError`, and consume it in three UIs: a TUI picker popup, a WebUI modal, and a CLI candidate print-out. Selection re-issues the same request pinned to the chosen runner's ConnectionID string (`SelectorOpts{Runner: cid}`).

**Tech Stack:** Go; brgen `.bgn` schema codegen (`make protoregen`); bubbletea TUI; Go/wasm + vanilla JS WebUI.

Design spec: `docs/superpowers/specs/2026-07-02-ambiguous-runner-candidate-picker-design.md`.

## Global Constraints

- **Scope: `OpenInteractive` path only** (fresh interactive + interactive resume). Do **not** touch the Submit/oneshot path or its `ErrorMsg` string.
- **Picker triggers only when ≥2 candidates.** Exactly one candidate auto-selects (unchanged); zero → `NoRunnerForRepo`/`PinnedNotFound` (unchanged).
- **Candidate id on the wire is the ConnectionID string** (`RunnerEntry.ID`), not a typed `RunnerID` — it feeds `SelectorOpts{Runner: cid}` directly and avoids the zero-value `RunnerID` encoder panic.
- **Never hand-edit `runner/protocol/message.go`.** Regenerate via `make protoregen ARGS='runner/protocol/message.bgn'` (brgen-kit cache already present; runs offline, deterministic — verified as a clean no-op on the unmodified schema).
- **Worktree path discipline:** all edits/builds happen in this task worktree. `$HARNESS_REPO_PATH` routes to the parent checkout — do not anchor edits there.
- **Every commit ends with these trailers** (per repo convention):
  ```
  Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01Q5qYx5pmjt8QuA5KKNDR4b
  ```
- **Verification gates before landing:** `go build ./...`, `go vet ./cli/... ./tui/... ./server/...`, `GOOS=js GOARCH=wasm go build ./cli/... ./cmd/harness-webui-wasm/`, `go test ./...`, then `make build`. WebUI runtime behavior is verified with Playwright (no Go unit test reaches the JS modal).
- **Builds on landed commit `98a8286`** (resume ladder: last-runner pin → Any). This plan appends the final rung: Any → picker.

## File Structure

- `runner/protocol/message.bgn` — **modify**: add `RunnerCandidate` format + conditional candidates block in `OpenInteractiveResponse`. Regenerates `runner/protocol/message.go`.
- `runner/protocol/candidate_roundtrip_test.go` — **create**: encode/decode round-trip for the conditional candidates block.
- `server/task_handler.go` — **modify**: populate candidates on the `handleOpenInteractive` ambiguous branch; add a `matchedRoot` helper.
- `server/task_handler_test.go` — **modify**: extend `TestHandleOpenInteractiveAmbiguous` to assert candidates.
- `cli/interactive_errors.go` — **create** (no build tag): `RunnerCandidate` struct, `AmbiguousRunnerError` type, `candidatesFromResponse` mapper.
- `cli/interactive_errors_test.go` — **create**: `candidatesFromResponse` mapping test.
- `cli/open_interactive_native.go` — **modify**: return `*AmbiguousRunnerError` on the ambiguous status.
- `cli/open_interactive_wasm.go` — **modify**: same, in the wasm switch.
- `tui/runnerpicker.go` — **create**: `RunnerPickerModel` (mirrors `ForwardPicker`).
- `tui/runnerpicker_test.go` — **create**: `Pick` + candidate→selector pure tests.
- `tui/app.go` — **modify**: `runnerPicker` + `pendingInteractive` state, stash-on-dispatch, error→picker, key routing, overlay render.
- `cmd/harness-cli/session.go` — **modify**: print candidates + exit on `*AmbiguousRunnerError`; add `formatAmbiguousCandidates`.
- `cmd/harness-cli/session_ambiguous_test.go` — **create**: `formatAmbiguousCandidates` test.
- `cmd/harness-webui-wasm/main.go` — **modify**: read `runner` cid field; reject with structured `code`+`candidates` on ambiguous.
- `webui/index.html` — **modify**: add `<dialog id="runner-picker-modal">`.
- `webui/static/main.js` — **modify**: on `e.code === "ambiguous_runner"`, show modal; retry pinned to chosen cid.

---

### Task 1: Schema — RunnerCandidate + conditional candidates block

**Files:**
- Modify: `runner/protocol/message.bgn` (`OpenInteractiveResponse`, ~line 674)
- Regenerate: `runner/protocol/message.go`
- Test: `runner/protocol/candidate_roundtrip_test.go` (create)

**Interfaces:**
- Produces (generated Go): `protocol.RunnerCandidate{ CidLen uint16; Cid []uint8; HostnameLen uint16; Hostname []uint8; MatchedRootLen uint16; MatchedRoot []uint8; ActiveTasks uint16; MaxTasks uint16 }` with setters `SetCid/SetHostname/SetMatchedRoot`. `protocol.OpenInteractiveResponse` gains `CandidatesLen uint16; Candidates []RunnerCandidate` (encoded only when `Status == OpenInteractiveStatus_AmbiguousRunner`).

> **Note on TDD order:** the Go types come from codegen, so the type cannot exist before the schema edit. The cycle here is: edit schema → regenerate → write the round-trip test → run. This is the one task where "test first" is inverted; every later task is normal TDD.

- [ ] **Step 1: Add the `RunnerCandidate` format** immediately before `format OpenInteractiveResponse:` in `runner/protocol/message.bgn`:

```
# One candidate runner returned when an OpenInteractive request is ambiguous
# (>=2 runners tie on longest-prefix roots score under an Any selector). `cid`
# is ConnectionID.String() — the exact string the --runner selector accepts, so
# a client pins the retry with SelectorOpts{Runner: cid}. Carrying the string
# (not a typed RunnerID) also sidesteps the zero-value RunnerID encoder panic.
format RunnerCandidate:
    cid_len          :u16
    cid              :[cid_len]u8
    hostname_len     :u16
    hostname         :[hostname_len]u8
    matched_root_len :u16
    matched_root     :[matched_root_len]u8
    active_tasks     :u16
    max_tasks        :u16
```

- [ ] **Step 2: Add the conditional candidates block** to `format OpenInteractiveResponse:` — after the `stream_id :u64` field (and its comment block), append:

```
    # Populated ONLY when status == ambiguous_runner: the runners that tied for
    # this repo. The client shows a picker and re-issues pinned to a chosen cid.
    if status == OpenInteractiveStatus.ambiguous_runner:
        candidates_len :u16
        candidates     :[candidates_len]RunnerCandidate
```

- [ ] **Step 3: Regenerate the Go**

Run: `cd <this-worktree> && make protoregen ARGS='runner/protocol/message.bgn'`
Expected: `==> Done. Regenerated: runner/protocol/message.bgn`, and `git diff --stat runner/protocol/message.go` shows additions for `RunnerCandidate` + the new fields.

- [ ] **Step 4: Write the round-trip test** at `runner/protocol/candidate_roundtrip_test.go`:

```go
package protocol

import "testing"

func TestOpenInteractiveResponseCandidatesRoundTrip(t *testing.T) {
	var c RunnerCandidate
	c.SetCid([]byte("ws:192.168.3.14:34184-30218"))
	c.SetHostname([]byte("gmkhost-codex"))
	c.SetMatchedRoot([]byte("/home/x/repo"))
	c.ActiveTasks = 2
	c.MaxTasks = 8

	resp := OpenInteractiveResponse{Status: OpenInteractiveStatus_AmbiguousRunner}
	resp.CandidatesLen = 1
	resp.Candidates = []RunnerCandidate{c}

	raw, err := resp.Encode(nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got OpenInteractiveResponse
	if _, err := got.Decode(raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Status != OpenInteractiveStatus_AmbiguousRunner || len(got.Candidates) != 1 {
		t.Fatalf("status=%v candidates=%d", got.Status, len(got.Candidates))
	}
	if string(got.Candidates[0].Cid) != "ws:192.168.3.14:34184-30218" ||
		string(got.Candidates[0].Hostname) != "gmkhost-codex" ||
		got.Candidates[0].ActiveTasks != 2 || got.Candidates[0].MaxTasks != 8 {
		t.Fatalf("candidate mismatch: %+v", got.Candidates[0])
	}

	// Non-ambiguous status must NOT carry candidate bytes (conditional block absent).
	ok := OpenInteractiveResponse{Status: OpenInteractiveStatus_Ok}
	okRaw, err := ok.Encode(nil)
	if err != nil {
		t.Fatalf("encode ok: %v", err)
	}
	var gotOk OpenInteractiveResponse
	if _, err := gotOk.Decode(okRaw); err != nil {
		t.Fatalf("decode ok: %v", err)
	}
	if len(gotOk.Candidates) != 0 {
		t.Fatalf("ok response carried %d candidates, want 0", len(gotOk.Candidates))
	}
}
```

> If the generated encode/decode method names differ from `Encode(nil)`/`Decode(raw)`, match the signatures used elsewhere in `message.go` (grep an existing `func (x *SubmitResponse) Encode`); adjust the two call sites only.

- [ ] **Step 5: Run the test**

Run: `go test ./runner/protocol/ -run TestOpenInteractiveResponseCandidatesRoundTrip -v`
Expected: PASS.

- [ ] **Step 6: Compile-check the whole tree** (codegen can ripple)

Run: `go build ./...`
Expected: no output (success).

- [ ] **Step 7: Commit**

```bash
git add runner/protocol/message.bgn runner/protocol/message.go runner/protocol/candidate_roundtrip_test.go
git commit -m "feat(protocol): OpenInteractiveResponse carries runner candidates on ambiguous_runner

<trailers>"
```

---

### Task 2: Server — populate candidates on the ambiguous branch

**Files:**
- Modify: `server/task_handler.go` (`handleOpenInteractive`, ambiguous branch ~line 642; add helper)
- Test: `server/task_handler_test.go` (extend `TestHandleOpenInteractiveAmbiguous`, ~line 439)

**Interfaces:**
- Consumes: `protocol.RunnerCandidate`, `OpenInteractiveResponse` (Task 1); `RunnerEntry{ID, Hostname, AllowedRoots, ActiveTasks, MaxTasks}`; `protocol.MatchLen`.
- Produces: `handleOpenInteractive` returns an `OpenInteractiveResponse{Status: AmbiguousRunner}` with candidates set for ≥2 candidates.

> **Codegen shape (from Task 1):** because `candidates` lives in a conditional block, the generated Go exposes it as **variant accessor methods**, NOT plain struct fields: setter `resp.SetCandidates(list)` (sets the slice, its len, and activates the ambiguous variant) and getter `resp.Candidates()` (returns `[]RunnerCandidate`). Assigning `resp.Candidates = ...`/`resp.CandidatesLen = ...` directly does NOT compile / fails encode with "invalid union type for encoding". `RunnerCandidate`'s own byte fields use setters `SetCid/SetHostname/SetMatchedRoot`; `ActiveTasks`/`MaxTasks` are plain `uint16` fields. Confirm the exact `SetCandidates` signature in `runner/protocol/message.go` before use (it may return a bool length-check like `SetRepoPath`).

- [ ] **Step 1: Extend the failing test.** Replace the body of `TestHandleOpenInteractiveAmbiguous` in `server/task_handler_test.go` with:

```go
func TestHandleOpenInteractiveAmbiguous(t *testing.T) {
	h := newTestHandler(t)
	now := time.Now()
	h.Registry.Add(&RunnerEntry{ID: "ws:10.0.0.1:1-1", Hostname: "h1", AllowedRoots: []string{"/shared"}, MaxTasks: 8, ActiveTasks: map[string]struct{}{"t": {}}, ConnectedAt: now, LastSeen: now, Conn: stubConn{}})
	h.Registry.Add(&RunnerEntry{ID: "ws:10.0.0.2:1-1", Hostname: "h2", AllowedRoots: []string{"/shared"}, MaxTasks: 8, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now, Conn: stubConn{}})

	req := &protocol.OpenInteractiveRequest{}
	req.SetRepoPath([]byte("/shared/repo"))

	resp := h.handleOpenInteractive(nil, req, protocol.ClientKind_Unspecified, protocol.TaskID{}, protocol.Capability_All)
	if resp.Status != protocol.OpenInteractiveStatus_AmbiguousRunner {
		t.Fatalf("status=%v want AmbiguousRunner", resp.Status)
	}
	if len(resp.Candidates()) != 2 {
		t.Fatalf("candidates=%d want 2", len(resp.Candidates()))
	}
	byCid := map[string]protocol.RunnerCandidate{}
	for _, c := range resp.Candidates() {
		byCid[string(c.Cid)] = c
	}
	a, ok := byCid["ws:10.0.0.1:1-1"]
	if !ok {
		t.Fatalf("missing candidate h1; got %v", byCid)
	}
	if string(a.Hostname) != "h1" || string(a.MatchedRoot) != "/shared" || a.ActiveTasks != 1 || a.MaxTasks != 8 {
		t.Fatalf("h1 candidate mismatch: %+v", a)
	}
}
```

- [ ] **Step 2: Run it — expect FAIL**

Run: `go test ./server/ -run TestHandleOpenInteractiveAmbiguous -v`
Expected: FAIL (`candidates=0 want 2`).

- [ ] **Step 3: Add the `matchedRoot` helper** near `bestRootScore` in `server/task_handler.go` (or at the bottom of the file):

```go
// matchedRoot returns the first AllowedRoots entry whose MatchLen(root, repo)
// equals the runner's best score — the display reason this runner is a
// candidate. Display metadata only; does not affect selection.
func matchedRoot(roots []string, repo string) string {
	best, bestRoot := 0, ""
	for _, root := range roots {
		if s := protocol.MatchLen(root, repo); s > best {
			best, bestRoot = s, root
		}
	}
	return bestRoot
}
```

- [ ] **Step 4: Populate candidates on the ambiguous branch.** In `handleOpenInteractive`, replace:

```go
	case len(cands) > 1:
		return errResp(protocol.OpenInteractiveStatus_AmbiguousRunner)
```

with:

```go
	case len(cands) > 1:
		slog.Error("handleOpenInteractive: ambiguous", "repo", repo, "candidates", len(cands))
		resp := protocol.OpenInteractiveResponse{Status: protocol.OpenInteractiveStatus_AmbiguousRunner}
		list := make([]protocol.RunnerCandidate, 0, len(cands))
		for _, c := range cands {
			var rc protocol.RunnerCandidate
			rc.SetCid([]byte(c.ID))
			rc.SetHostname([]byte(c.Hostname))
			rc.SetMatchedRoot([]byte(matchedRoot(c.AllowedRoots, repo)))
			rc.ActiveTasks = uint16(len(c.ActiveTasks))
			rc.MaxTasks = uint16(c.MaxTasks)
			list = append(list, rc)
		}
		resp.SetCandidates(list) // sets slice+len AND activates the ambiguous variant; direct field assignment fails encode
		return resp
```

- [ ] **Step 5: Run the test — expect PASS**

Run: `go test ./server/ -run TestHandleOpenInteractiveAmbiguous -v`
Expected: PASS.

- [ ] **Step 6: Full server package test** (guard against regressions; note `TestHandleOpenPortForward_RemoteRegisters` is a known flake — re-run in isolation if it fails)

Run: `go test ./server/`
Expected: `ok`.

- [ ] **Step 7: Commit**

```bash
git add server/task_handler.go server/task_handler_test.go
git commit -m "feat(server): return runner candidates on interactive ambiguous_runner

<trailers>"
```

---

### Task 3: cli — typed AmbiguousRunnerError carrying candidates

**Files:**
- Create: `cli/interactive_errors.go` (no build tag)
- Create: `cli/interactive_errors_test.go`
- Modify: `cli/open_interactive_native.go` (`openInteractive`, ~line 103)
- Modify: `cli/open_interactive_wasm.go` (status switch, ~line 162)

**Interfaces:**
- Consumes: `protocol.OpenInteractiveResponse`, `protocol.RunnerCandidate` (Task 1).
- Produces: `cli.RunnerCandidate{ Cid, Hostname, MatchedRoot string; ActiveTasks, MaxTasks int }`; `cli.AmbiguousRunnerError{ Candidates []RunnerCandidate }` (implements `error`); `cli.candidatesFromResponse(*protocol.OpenInteractiveResponse) []RunnerCandidate`. Both `openInteractive` (native) and `InteractiveWithSelectorArgsAndCaps` (wasm) return `*AmbiguousRunnerError` for the ambiguous status.

- [ ] **Step 1: Write the failing test** at `cli/interactive_errors_test.go`:

```go
package cli

import (
	"errors"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestCandidatesFromResponse(t *testing.T) {
	var c protocol.RunnerCandidate
	c.SetCid([]byte("ws:10.0.0.2:1-1"))
	c.SetHostname([]byte("gmkhost-codex"))
	c.SetMatchedRoot([]byte("/home/x/repo"))
	c.ActiveTasks = 3
	c.MaxTasks = 8
	resp := &protocol.OpenInteractiveResponse{Status: protocol.OpenInteractiveStatus_AmbiguousRunner}
	resp.SetCandidates([]protocol.RunnerCandidate{c})

	got := candidatesFromResponse(resp)
	if len(got) != 1 {
		t.Fatalf("len=%d want 1", len(got))
	}
	if got[0].Cid != "ws:10.0.0.2:1-1" || got[0].Hostname != "gmkhost-codex" ||
		got[0].MatchedRoot != "/home/x/repo" || got[0].ActiveTasks != 3 || got[0].MaxTasks != 8 {
		t.Fatalf("mapping mismatch: %+v", got[0])
	}
}

func TestAmbiguousRunnerErrorIsAs(t *testing.T) {
	err := error(&AmbiguousRunnerError{Candidates: []RunnerCandidate{{Cid: "ws:1", Hostname: "h"}}})
	var are *AmbiguousRunnerError
	if !errors.As(err, &are) {
		t.Fatal("errors.As failed")
	}
	if len(are.Candidates) != 1 || are.Candidates[0].Cid != "ws:1" {
		t.Fatalf("candidates lost: %+v", are.Candidates)
	}
	if err.Error() == "" {
		t.Fatal("empty Error() string")
	}
}
```

- [ ] **Step 2: Run it — expect FAIL** (undefined symbols)

Run: `go test ./cli/ -run 'TestCandidatesFromResponse|TestAmbiguousRunnerErrorIsAs' -v`
Expected: FAIL (`undefined: candidatesFromResponse` / `AmbiguousRunnerError`).

- [ ] **Step 3: Create `cli/interactive_errors.go`** (no build tag — shared by native + wasm):

```go
package cli

import (
	"fmt"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// RunnerCandidate is one runner that tied for a repo when an interactive open
// was ambiguous. Cid is a ConnectionID string — pass it verbatim as
// SelectorOpts{Runner: Cid} to pin a retry.
type RunnerCandidate struct {
	Cid         string
	Hostname    string
	MatchedRoot string
	ActiveTasks int
	MaxTasks    int
}

// AmbiguousRunnerError is returned when opening/resuming an interactive session
// matched >=2 runners under an Any selector. Callers (TUI/CLI/WebUI) use
// errors.As to pull out Candidates and let the user pick one.
type AmbiguousRunnerError struct {
	Candidates []RunnerCandidate
}

func (e *AmbiguousRunnerError) Error() string {
	return fmt.Sprintf("ambiguous_runner: %d runners match; pick one (or pin with --runner/--host/--ip)", len(e.Candidates))
}

// candidatesFromResponse maps the wire candidates into the client-facing slice.
// NOTE (from Task 2): the generated getter Candidates() returns a POINTER
// (*[]protocol.RunnerCandidate) — nil unless Status == ambiguous_runner — so
// deref with a nil guard.
func candidatesFromResponse(oir *protocol.OpenInteractiveResponse) []RunnerCandidate {
	cands := oir.Candidates()
	if cands == nil {
		return nil
	}
	out := make([]RunnerCandidate, 0, len(*cands))
	for _, c := range *cands {
		out = append(out, RunnerCandidate{
			Cid:         string(c.Cid),
			Hostname:    string(c.Hostname),
			MatchedRoot: string(c.MatchedRoot),
			ActiveTasks: int(c.ActiveTasks),
			MaxTasks:    int(c.MaxTasks),
		})
	}
	return out
}
```

- [ ] **Step 4: Run the test — expect PASS**

Run: `go test ./cli/ -run 'TestCandidatesFromResponse|TestAmbiguousRunnerErrorIsAs' -v`
Expected: PASS.

- [ ] **Step 5: Native — return the typed error.** In `cli/open_interactive_native.go` `openInteractive`, replace:

```go
	if err := openInteractiveStatusError(repoPath, oir.Status); err != nil {
		return nil, "", err
	}
```

with:

```go
	if oir.Status == protocol.OpenInteractiveStatus_AmbiguousRunner {
		return nil, "", &AmbiguousRunnerError{Candidates: candidatesFromResponse(oir)}
	}
	if err := openInteractiveStatusError(repoPath, oir.Status); err != nil {
		return nil, "", err
	}
```

(Leave the `AmbiguousRunner` case in `openInteractiveStatusError` as a defensive fallback.)

- [ ] **Step 6: Wasm — return the typed error.** In `cli/open_interactive_wasm.go`, in the status `switch` of `InteractiveWithSelectorArgsAndCaps`, replace:

```go
	case protocol.OpenInteractiveStatus_AmbiguousRunner:
		return "", fmt.Errorf("ambiguous_runner: multiple runners match; pin one with host")
```

with:

```go
	case protocol.OpenInteractiveStatus_AmbiguousRunner:
		return "", &AmbiguousRunnerError{Candidates: candidatesFromResponse(oiResp)}
```

- [ ] **Step 7: Build both targets**

Run: `go build ./... && GOOS=js GOARCH=wasm go build ./cli/...`
Expected: no output (both succeed).

- [ ] **Step 8: Commit**

```bash
git add cli/interactive_errors.go cli/interactive_errors_test.go cli/open_interactive_native.go cli/open_interactive_wasm.go
git commit -m "feat(cli): typed AmbiguousRunnerError carrying runner candidates

<trailers>"
```

---

### Task 4: TUI — runner picker popup + wire into S / r / R

**Files:**
- Create: `tui/runnerpicker.go`
- Create: `tui/runnerpicker_test.go`
- Modify: `tui/app.go` (App struct; `S` handler ~743; `r`/`R` handler ~793-806; `InteractiveReadyMsg` handler ~423; key routing ~638; overlay render ~1023)

**Interfaces:**
- Consumes: `cli.RunnerCandidate`, `cli.AmbiguousRunnerError` (Task 3); `cli.SelectorOpts`; existing `DoResumeSession`, `DoOpenDetachableSession`.
- Produces: `RunnerPickerModel{ Open([]cli.RunnerCandidate); Close(); IsOpen() bool; Pick(key string) *cli.RunnerCandidate; View() string }`; `App.pendingInteractive` state.

- [ ] **Step 1: Write the failing test** at `tui/runnerpicker_test.go`:

```go
package tui

import (
	"testing"

	"github.com/on-keyday/agent-harness/cli"
)

func TestRunnerPickerPick(t *testing.T) {
	var p RunnerPickerModel
	cands := []cli.RunnerCandidate{
		{Cid: "ws:10.0.0.1:1-1", Hostname: "h1", ActiveTasks: 1, MaxTasks: 8},
		{Cid: "ws:10.0.0.2:1-1", Hostname: "h2", ActiveTasks: 0, MaxTasks: 8},
	}
	p.Open(cands)
	if !p.IsOpen() {
		t.Fatal("want open")
	}
	if got := p.Pick("2"); got == nil || got.Cid != "ws:10.0.0.2:1-1" {
		t.Fatalf("Pick(2)=%v", got)
	}
	if got := p.Pick("3"); got != nil {
		t.Fatalf("Pick(3)=%v want nil (out of range)", got)
	}
	if got := p.Pick("x"); got != nil {
		t.Fatalf("Pick(x)=%v want nil (non-digit)", got)
	}
	p.Close()
	if p.IsOpen() {
		t.Fatal("want closed")
	}
}
```

- [ ] **Step 2: Run it — expect FAIL** (undefined `RunnerPickerModel`)

Run: `go test ./tui/ -run TestRunnerPickerPick -v`
Expected: FAIL.

- [ ] **Step 3: Create `tui/runnerpicker.go`** (mirrors `ForwardPicker`):

```go
package tui

import (
	"fmt"

	"github.com/charmbracelet/lipgloss"
	"github.com/on-keyday/agent-harness/cli"
)

// RunnerPickerModel is a digit-select popup shown when an interactive open
// returns AmbiguousRunner: it lists the candidate runners so the user picks one
// with a single keypress. Mirrors ForwardPicker.
type RunnerPickerModel struct {
	open       bool
	candidates []cli.RunnerCandidate
}

func (p *RunnerPickerModel) IsOpen() bool { return p.open }

func (p *RunnerPickerModel) Open(cands []cli.RunnerCandidate) {
	p.open = true
	p.candidates = cands
}

func (p *RunnerPickerModel) Close() { p.open = false; p.candidates = nil }

// Pick maps a digit key ("1".."9") to a candidate, or nil if out of range /
// not a digit.
func (p *RunnerPickerModel) Pick(key string) *cli.RunnerCandidate {
	if len(key) != 1 || key[0] < '1' || key[0] > '9' {
		return nil
	}
	idx := int(key[0] - '1')
	if idx >= len(p.candidates) {
		return nil
	}
	return &p.candidates[idx]
}

func (p *RunnerPickerModel) View() string {
	if !p.open {
		return ""
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorFocused).
		Padding(1, 2)
	body := "Ambiguous runner — pick one:\n\n"
	for i, c := range p.candidates {
		body += fmt.Sprintf("%d) %-16s [%d/%d]  %s  %s\n",
			i+1, c.Hostname, c.ActiveTasks, c.MaxTasks, c.MatchedRoot, c.Cid)
	}
	body += "\n" + FooterStyle.Render("press number to pick · Esc to cancel")
	return box.Render(body)
}
```

- [ ] **Step 4: Run the test — expect PASS**

Run: `go test ./tui/ -run TestRunnerPickerPick -v`
Expected: PASS.

- [ ] **Step 5: Add App state.** In `tui/app.go`, add to the `App` struct (near `forwardPicker`):

```go
	// runnerPicker shows candidate runners when an interactive open returns
	// AmbiguousRunner; a pick re-issues the request pinned to the chosen cid.
	runnerPicker RunnerPickerModel
	// pendingInteractive holds the params of the in-flight interactive open so
	// the picker can re-issue it pinned to a chosen runner.
	pendingInteractive pendingInteractive
```

and define the struct near the top of `app.go` (after the `App` type):

```go
// pendingInteractive captures what an interactive open needs so a runner-picker
// selection can re-issue it. repo is "" for resume (server reuses the task's
// repo); resumeTaskID is "" for a fresh session.
type pendingInteractive struct {
	repo         string
	resumeTaskID string
	extraArgs    []string
	caps         protocol.Capability
	capsOverride bool
}
```

- [ ] **Step 6: Stash params before dispatching.** In the `r`/`R` handler, change the `actionResume` case (around line 798-804) to stash first:

```go
			case actionResume:
				a.pendingInteractive = pendingInteractive{
					repo: "", resumeTaskID: a.tasks.SelectedID(),
					extraArgs: act.ResumeArgs, caps: a.sessionCaps, capsOverride: a.applyCapsOnResume,
				}
				return a, DoResumeSession(a.client, t.AssignedTo, act.ResumeArgs, a.tasks.SelectedID(), a.sessionCaps, a.applyCapsOnResume)
```

And in the `S` handler (line ~743):

```go
		if a.focus != focusCmdline && !logsEditing && msg.String() == "S" {
			a.pendingInteractive = pendingInteractive{
				repo: a.defaultRepo, resumeTaskID: "", extraArgs: nil,
				caps: a.sessionCaps, capsOverride: false,
			}
			return a, DoOpenDetachableSession(a.client, a.defaultRepo, cli.SelectorOpts{}, nil, "", a.sessionCaps, false)
		}
```

- [ ] **Step 7: Open the picker on AmbiguousRunnerError.** In the `case InteractiveReadyMsg:` handler, change the error branch (line ~424-426) to:

```go
		if msg.Err != nil {
			var are *cli.AmbiguousRunnerError
			if errors.As(msg.Err, &are) {
				a.runnerPicker.Open(are.Candidates)
				return a, nil
			}
			a.cmdresult.Append(ErrorStyle.Render("open interactive failed: " + msg.Err.Error()))
			return a, nil
		}
```

Add `"errors"` to the `app.go` import block if not present.

- [ ] **Step 8: Route keys to the picker.** Add near the other popup interceptors (e.g. just before the `forwardPicker.IsOpen()` block ~line 638):

```go
		// Runner picker intercepts keys when open (digit picks, Esc cancels).
		if a.runnerPicker.IsOpen() {
			if msg.Type == tea.KeyEsc {
				a.runnerPicker.Close()
				a.cmdresult.Append(WarnStyle.Render("runner pick cancelled"))
				return a, nil
			}
			if c := a.runnerPicker.Pick(msg.String()); c != nil {
				p := a.pendingInteractive
				a.runnerPicker.Close()
				a.cmdresult.Append(OKStyle.Render("pinned runner: ") + c.Hostname + "  " + c.Cid)
				return a, DoOpenDetachableSession(a.client, p.repo, cli.SelectorOpts{Runner: c.Cid}, p.extraArgs, p.resumeTaskID, p.caps, p.capsOverride)
			}
			return a, nil
		}
```

- [ ] **Step 9: Render the picker overlay.** Near the `forwardPicker` render (line ~1023-1024), add:

```go
	if a.runnerPicker.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.runnerPicker.View())
	}
```

- [ ] **Step 10: Build + full tui test**

Run: `go build ./... && go test ./tui/`
Expected: `ok`.

- [ ] **Step 11: Commit**

```bash
git add tui/runnerpicker.go tui/runnerpicker_test.go tui/app.go
git commit -m "feat(tui): runner-picker popup on ambiguous interactive open (S / r / R)

<trailers>"
```

---

### Task 5: CLI (non-TUI) — print candidates + exit on ambiguous

**Files:**
- Modify: `cmd/harness-cli/session.go` (`runSessionNew`, the three interactive call sites ~200-218)
- Create: `cmd/harness-cli/session_ambiguous_test.go`

**Interfaces:**
- Consumes: `cli.AmbiguousRunnerError`, `cli.RunnerCandidate` (Task 3).
- Produces: `formatAmbiguousCandidates([]cli.RunnerCandidate) string`; on `*AmbiguousRunnerError`, `runSessionNew` prints it and exits non-interactively.

- [ ] **Step 1: Write the failing test** at `cmd/harness-cli/session_ambiguous_test.go`:

```go
package main

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli"
)

func TestFormatAmbiguousCandidates(t *testing.T) {
	out := formatAmbiguousCandidates([]cli.RunnerCandidate{
		{Cid: "ws:10.0.0.1:1-1", Hostname: "gmkhost", MatchedRoot: "/repo", ActiveTasks: 1, MaxTasks: 8},
		{Cid: "ws:10.0.0.2:1-1", Hostname: "gmkhost-codex", MatchedRoot: "/repo", ActiveTasks: 0, MaxTasks: 8},
	})
	for _, want := range []string{"ws:10.0.0.1:1-1", "gmkhost-codex", "--runner", "/repo"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}
```

- [ ] **Step 2: Run it — expect FAIL** (undefined `formatAmbiguousCandidates`)

Run: `go test ./cmd/harness-cli/ -run TestFormatAmbiguousCandidates -v`
Expected: FAIL.

- [ ] **Step 3: Add the formatter + error handling** to `cmd/harness-cli/session.go`. Add the helper (near the top-level funcs):

```go
// formatAmbiguousCandidates renders the candidate runners for the non-TUI CLI:
// a table plus a hint to re-run pinned with --runner <cid>.
func formatAmbiguousCandidates(cands []cli.RunnerCandidate) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ambiguous runner: %d runners match this repo; re-run pinned with --runner <cid>:\n", len(cands))
	for _, c := range cands {
		fmt.Fprintf(&b, "  %-18s [%d/%d]  %s  --runner %s\n", c.Hostname, c.ActiveTasks, c.MaxTasks, c.MatchedRoot, c.Cid)
	}
	return b.String()
}

// exitOnAmbiguous prints the candidate table and exits non-zero when err is an
// AmbiguousRunnerError; otherwise returns err unchanged.
func exitOnAmbiguous(err error) error {
	var are *cli.AmbiguousRunnerError
	if errors.As(err, &are) {
		fmt.Fprint(os.Stderr, formatAmbiguousCandidates(are.Candidates))
		os.Exit(3)
	}
	return err
}
```

Ensure `errors` and `strings` are imported in `session.go`.

- [ ] **Step 4: Wrap the three interactive call sites** in `runSessionNew` so their returned error passes through `exitOnAmbiguous`. For the detach branch:

```go
	if detach {
		stream, taskIDHex, err := c.OpenInteractiveWithSelectorArgsAndCaps(ctx, repoVal, sel, []string(extraArgs), *resume, true, caps, resumeCapsOverride)
		if err != nil {
			return exitOnAmbiguous(err)
		}
		_ = stream.Close()
		fmt.Println(taskIDHex)
		return nil
	}
```

For the x11 branch: `return exitOnAmbiguous(err)` in its `if err != nil`. For the final interactive branch (line ~218):

```go
	id, err := c.InteractiveWithSelectorArgsAndCaps(ctx, repoVal, sel, []string(extraArgs), *resume, true, caps, resumeCapsOverride)
	if err != nil {
		return exitOnAmbiguous(err)
	}
```

- [ ] **Step 5: Run the test — expect PASS**

Run: `go test ./cmd/harness-cli/ -run TestFormatAmbiguousCandidates -v`
Expected: PASS.

- [ ] **Step 6: Build**

Run: `go build ./...`
Expected: no output.

- [ ] **Step 7: Commit**

```bash
git add cmd/harness-cli/session.go cmd/harness-cli/session_ambiguous_test.go
git commit -m "feat(cli): session new prints runner candidates on ambiguous_runner

<trailers>"
```

---

### Task 6: WebUI — structured reject + modal picker + cid-pinned retry

**Files:**
- Modify: `cmd/harness-webui-wasm/main.go` (`harnessStartInteractive` ~769-831; `rejectErr` ~101)
- Modify: `webui/index.html` (add dialog near `file-preview-modal` ~166)
- Modify: `webui/static/main.js` (`openInteractive` catch ~1706; `composeRequest`/retry)

**Interfaces:**
- Consumes: `cli.AmbiguousRunnerError` (Task 3); wasm `SelectorOpts{Runner}` path (extend `harnessStartInteractive`).
- Produces: `startInteractive` Promise rejects with a JS `Error` whose `.code === "ambiguous_runner"` and `.candidates` is an array of `{cid, hostname, matchedRoot, activeTasks, maxTasks}`; retry accepts a `runner` cid field.

> **Note:** wasm/JS glue has no Go unit test that reaches the modal; verification for this task is `wasm-check` compile + a Playwright walkthrough (Step 7). This is the accepted approach for WebUI in this repo.

- [ ] **Step 1: Extend `harnessStartInteractive` to accept a `runner` cid.** In `cmd/harness-webui-wasm/main.go`, where it currently builds the selector from `host` (~line 807), read `runner` too and prefer it:

```go
	runnerCid := args[0].Get("runner").String() // "" when absent (Get returns "undefined"→handle)
	if runnerCid == "undefined" {
		runnerCid = ""
	}
	host := args[0].Get("host").String()
	if host == "undefined" {
		host = ""
	}
	sel, err := cli.BuildSelector(cli.SelectorOpts{Runner: runnerCid, Host: host})
	if err != nil {
		rejectErr(reject, fmt.Errorf("selector: %w", err))
		return
	}
```

(Replace the existing `cli.BuildSelector(cli.SelectorOpts{Host: host})` call. `SelectorOpts.ValidateSelector` inside `BuildSelector` already rejects both-set; the JS retry sends only `runner`.)

- [ ] **Step 2: Reject with structured candidates on ambiguous.** Where `harnessStartInteractive` handles the `InteractiveWithSelectorArgsAndCaps` error (~line 820-822), branch on the typed error:

```go
	taskID, err := c.InteractiveWithSelectorArgsAndCaps(rootCtx, repo, sel, extraArgs, resumeTaskID, detachable, caps, resumeCapsOverride)
	if err != nil {
		var are *cli.AmbiguousRunnerError
		if errors.As(err, &are) {
			cands := make([]any, 0, len(are.Candidates))
			for _, cc := range are.Candidates {
				cands = append(cands, map[string]any{
					"cid": cc.Cid, "hostname": cc.Hostname, "matchedRoot": cc.MatchedRoot,
					"activeTasks": cc.ActiveTasks, "maxTasks": cc.MaxTasks,
				})
			}
			jsErr := js.Global().Get("Error").New("ambiguous_runner")
			jsErr.Set("code", "ambiguous_runner")
			jsErr.Set("candidates", js.ValueOf(cands))
			reject.Invoke(jsErr)
			return
		}
		rejectErr(reject, fmt.Errorf("interactive: %w", err))
		return
	}
```

Ensure `errors` is imported in `main.go`.

- [ ] **Step 3: wasm-check compile**

Run: `GOOS=js GOARCH=wasm go build ./cli/... ./cmd/harness-webui-wasm/`
Expected: no output.

- [ ] **Step 4: Add the modal** to `webui/index.html`, mirroring `file-preview-modal` (body-level, near line 166):

```html
<dialog id="runner-picker-modal">
  <form method="dialog" style="min-width:320px">
    <h3 style="margin-top:0">Ambiguous runner — pick one</h3>
    <div id="runner-picker-list"></div>
    <menu style="display:flex;justify-content:flex-end;gap:8px;margin-top:12px">
      <button value="cancel">Cancel</button>
    </menu>
  </form>
</dialog>
```

- [ ] **Step 5: Show the modal + retry pinned.** In `webui/static/main.js`, update the `openInteractive` catch (~1706-1708) to route ambiguous errors to the picker:

```js
    } catch (e) {
      attachedTask.textContent = "";
      if (e && e.code === "ambiguous_runner" && Array.isArray(e.candidates)) {
        pickRunnerAndRetry(e.candidates, { ...req, detachable });
        return;
      }
      alert(`startInteractive: ${e.message}`);
    }
```

Add the helper (near the other modal helpers):

```js
function pickRunnerAndRetry(candidates, baseReq) {
  const modal = document.getElementById("runner-picker-modal");
  const list = document.getElementById("runner-picker-list");
  list.innerHTML = "";
  candidates.forEach((c) => {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.style.cssText = "display:block;width:100%;text-align:left;margin:4px 0;padding:6px";
    btn.textContent = `${c.hostname}  [${c.activeTasks}/${c.maxTasks}]  ${c.matchedRoot}  ${c.cid}`;
    btn.onclick = async () => {
      modal.close();
      try {
        // Pin by cid (host can itself be ambiguous). Clear host so the selector is unambiguous.
        const taskID = await window.harness.startInteractive({
          ...baseReq, host: "", runner: c.cid,
          caps: spawnCaps, resumeCapsOverride: baseReq.resumeTaskId ? applyCapsOnResume : false,
        });
        onInteractiveOpened(taskID); // mirror the success path used by openInteractive
      } catch (e2) {
        alert(`startInteractive: ${e2.message}`);
      }
    };
    list.appendChild(btn);
  });
  modal.showModal();
}
```

> Match `onInteractiveOpened(taskID)` to whatever the existing success path in `openInteractive` does with `taskID` (e.g. set `attachedTask`, open the xterm). If that logic is inline rather than a named function, factor it into one and call it from both places — do not duplicate.

- [ ] **Step 6: wasm rebuild for the browser** (hot-reload; no server restart per repo norm)

Run: `make webui-build`
Expected: builds `webui/static/main.wasm` + refreshes `wasm_exec.js`.

- [ ] **Step 7: Playwright verification.** With two runners registered on the same repo roots (e.g. the live `gmkhost` + `gmkhost-codex` on `remote-agent-harness`), open the WebUI, start an interactive session with host = "(any)", and confirm: (a) the runner-picker modal appears listing both candidates with `[active/max]` + cid; (b) clicking one opens the session on that runner; (c) `session ls` / tasks show the task bound to the chosen runner. Screenshot desktop + 390px width (dark theme). Capture before/after.

- [ ] **Step 8: Commit**

```bash
git add cmd/harness-webui-wasm/main.go webui/index.html webui/static/main.js webui/static/main.wasm webui/static/wasm_exec.js
git commit -m "feat(webui): runner-picker modal on ambiguous interactive open

<trailers>"
```

---

### Task 7: Full verification

**Files:** none (verification only).

- [ ] **Step 1: vet**

Run: `go vet ./...`
Expected: no output.

- [ ] **Step 2: wasm-check**

Run: `GOOS=js GOARCH=wasm go build ./cli/... ./cmd/harness-webui-wasm/`
Expected: no output.

- [ ] **Step 3: full test suite**

Run: `go test ./...`
Expected: all `ok`. If `server` fails on `TestHandleOpenPortForward_RemoteRegisters`, re-run `go test ./server/ -run TestHandleOpenPortForward_RemoteRegisters -count=3` — it is a known flake; a 3/3 pass in isolation clears it.

- [ ] **Step 4: build binaries**

Run: `make build`
Expected: emits `bin/agent-runner`, `bin/harness-cli`, `bin/harness-server`, `bin/harness-tui` + wasm.

- [ ] **Step 5: TUI smoke (live).** Resume a terminal interactive task on `remote-agent-harness` from the TUI with `r` while both `gmkhost` and `gmkhost-codex` runners are up; confirm the picker appears, selection pins, and the session attaches. (The resume ladder from 98a8286 first tries the last-runner pin; force the picker by resuming a task whose last runner has since been differentiated, or by using `S` for a fresh Any open.)

**Landing:** after all tasks pass, land via the `landing-to-main` skill (Mode A: FF-push the task branch to `origin/main`, advance local main, `make build` in the main checkout). Do not land partial work.

## Self-Review

**Spec coverage:** ① schema conditional block → Task 1. ② server populate → Task 2. ③ typed client error → Task 3. ④ TUI/WebUI/CLI surfaces → Tasks 4/6/5. ⑤ resume ladder (Any→picker) → Task 4 Steps 6-8 (builds on 98a8286). ⑥ tests → each task's TDD steps + Task 7. All spec sections mapped.

**Placeholder scan:** every code step has concrete code; `<trailers>` is defined once in Global Constraints; the one `<this-worktree>` reference is a path the executor substitutes. Two honest deviations are called out explicitly: Task 1 codegen-before-test, and Task 6 wasm/Playwright-instead-of-unit-test.

**Type consistency:** `cli.RunnerCandidate` fields (`Cid/Hostname/MatchedRoot/ActiveTasks/MaxTasks`) are used identically in Tasks 3/4/5/6. `SelectorOpts{Runner: cid}` is the single re-issue mechanism across TUI (Task 4 Step 8), CLI hint (Task 5), and WebUI (Task 6 Step 5). `candidatesFromResponse` defined in Task 3 is consumed by native + wasm in the same task. `DoOpenDetachableSession` reused for both resume and fresh re-issue — signature matches app.go:1212's existing call.
