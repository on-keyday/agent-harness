# Resume capability override Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Let a resumer re-grant a different capability set to a resumed task (same task id), opt-in and persisted, across server/CLI/TUI/WebUI.

**Architecture:** A `resume_caps_override :u1` bit on the resume-capable requests. When set, the server resume path sets the task's caps to `callerCaps(resumer) ∩ requested_caps` and persists it (new `task_caps_changed` WAL event). Plain resume (bit=0) keeps persisted caps. Clients opt in explicitly (CLI `--caps` on resume; TUI/WebUI an "apply caps on resume" toggle, default off).

**Tech Stack:** Go, brgen (`.bgn`), TaskStore/WAL, `cli` builders, Bubble Tea (TUI), Go/wasm + JS (WebUI).

**Spec:** `docs/superpowers/specs/2026-06-20-resume-caps-override-design.md`

## Global Constraints

- **Override is opt-in; plain resume keeps persisted caps.** Never auto-widen on a plain resume.
- **Authority = resumer:** `caps_resumed = callerCaps(resumer) ∩ requested_caps` (operator=All → arbitrary; agent bounded by its own caps). Mirrors the spawn `creator ∩ requested` rule.
- **Persist via a dedicated `task_caps_changed` WAL event** (NOT an `omitempty` field on `task_resumed`) so an override to `Capability_None` (0) is unambiguous on replay.
- **Additive builders:** existing spawn/resume builders are UNCHANGED (the new `resume_caps_override` request field defaults to 0). Override is carried only by NEW `*ResumeWithCaps` builder variants. No churn to existing callers.
- **Generated code not hand-edited** (`message.go`); `.bgn` + `make protoregen`. Build hygiene: `make check`/`wasm-check` + focused `go test`. NEVER bare `go build ./cmd/<x>/`. Commit only intended files (no `git add -A`; ignore pre-existing untracked noise).
- Public repo: no local env in committed content.

---

## File Structure

- `runner/protocol/message.bgn` → `resume_caps_override :u1` on `SubmitRequest` + `OpenInteractiveRequest` (regenerates `message.go`).
- `server/taskstore.go` → `Resume(...)` gains caps-override params; `task_caps_changed` WAL write + `ReplayEvents` case.
- `server/wal.go` → `task_caps_changed` event handling (Capabilities already on WALEvent from the capabilities feature).
- `server/task_handler.go` → resume branches compute `intersectCaps(callerCaps(cid), req.RequestedCaps)` and pass the override.
- `cli/submit.go`, `cli/open_interactive_native.go`, `cli/open_interactive_wasm.go` → new `*ResumeWithCaps` builders (set the override bit).
- `cmd/harness-cli/main.go`, `cmd/harness-cli/session.go` → `--caps` on resume (flag.Visit) → call the override builder.
- `tui/cmdline.go`, `tui/app.go`, `tui/interactive.go` → `applyCapsOnResume` + `caps --on-resume` + resume uses override builder when on.
- `cmd/harness-webui-wasm/main.go`, `webui/index.html`, `webui/static/main.js`, `webui/static/style.css` → `opts.resumeCapsOverride` + checkbox.

---

## Task 1: schema — `resume_caps_override` bit

**Files:** Modify `runner/protocol/message.bgn` (SubmitRequest, OpenInteractiveRequest); regenerate `runner/protocol/message.go`. Test: `runner/protocol/capability_test.go` (extend).

**Interfaces:** Produces `SubmitRequest.ResumeCapsOverride` and `OpenInteractiveRequest.ResumeCapsOverride` (type `uint1`/the generated bit type; 0 default).

- [ ] **Step 1:** In `runner/protocol/message.bgn`, add `resume_caps_override :u1` to `SubmitRequest` and `OpenInteractiveRequest`, adjacent to the existing `u1` bitfields (`detachable`/`x11_enabled`) so it packs with them. Add a comment: "resume only: 1 = set caps to caps_resumer ∩ requested_caps on resume; 0 = keep persisted. Ignored on create."

- [ ] **Step 2:** Regenerate: `make protoregen ARGS='runner/protocol/message.bgn'`. If it fails for environmental reasons, STOP / report BLOCKED.

- [ ] **Step 3:** Extend `runner/protocol/capability_test.go` with a round-trip asserting `ResumeCapsOverride` survives encode/decode on a `SubmitRequest` (set it to 1, encode, decode, assert 1; and a separate value-0 case):

```go
func TestResumeCapsOverrideRoundTrip(t *testing.T) {
	in := SubmitRequest{}
	in.ResumeCapsOverride = 1
	in.RequestedCaps = Capability_Spawn
	b := in.MustAppend(nil)
	var out SubmitRequest
	if err := out.DecodeExact(b); err != nil { t.Fatal(err) }
	if out.ResumeCapsOverride != 1 { t.Fatalf("override = %d, want 1", out.ResumeCapsOverride) }
}
```

- [ ] **Step 4:** `go test ./runner/protocol/ -run TestResumeCapsOverride -v && make check` → PASS.

- [ ] **Step 5:** Commit. `git add runner/protocol/message.bgn runner/protocol/message.go runner/protocol/capability_test.go && git commit -m "feat(protocol): add resume_caps_override bit to spawn requests"`

---

## Task 2: server — apply + persist the override on resume

**Files:** Modify `server/taskstore.go` (`Resume` `:225`, `ReplayEvents` `:583`), `server/wal.go` (task_caps_changed), `server/task_handler.go` (resume branches). Test: `server/capabilities_test.go` (extend), `server/taskstore_test.go`.

**Interfaces:**
- Consumes: `intersectCaps`, `callerCaps` (capabilities feature), `protocol.Capability`, `SubmitRequest.ResumeCapsOverride`.
- Produces: `TaskStore.Resume(id, prompt, extraArgs, selector, boundRunnerID, resumerKind, capsOverride bool, newCaps protocol.Capability)`; WAL event type `"task_caps_changed"`.

- [ ] **Step 1: Write the test** (extend `server/capabilities_test.go`): operator resumes a confined task with override → caps become the requested set; plain resume (override=false) → caps unchanged; override by a limited agent → intersected.

```go
func TestResumeCapsOverride(t *testing.T) {
	h := newTestHandler(t)
	id := h.Tasks.Create("r","t",protocol.TaskKind_Oneshot,protocol.ClientKind_Cli,protocol.TaskID{},"",protocol.RunnerSelector{},nil, protocol.Capability_Spawn)
	// move it to a terminal state so Resume is allowed (mirror existing resume tests' setup)
	markTerminalForTest(t, h, id) // use whatever the existing resume tests use to reach terminal
	// override resume by operator → caps replaced
	if _, err := h.Tasks.Resume(id, "", nil, protocol.RunnerSelector{}, "", protocol.ClientKind_Cli, true, protocol.Capability_FileRead); err != nil {
		t.Fatal(err)
	}
	if e,_ := h.Tasks.Get(id); e.Capabilities != protocol.Capability_FileRead {
		t.Fatalf("override caps = %#x, want FileRead", e.Capabilities)
	}
	// plain resume → unchanged
	markTerminalForTest(t, h, id)
	if _, err := h.Tasks.Resume(id, "", nil, protocol.RunnerSelector{}, "", protocol.ClientKind_Cli, false, protocol.Capability_None); err != nil { t.Fatal(err) }
	if e,_ := h.Tasks.Get(id); e.Capabilities != protocol.Capability_FileRead {
		t.Fatalf("plain resume changed caps to %#x", e.Capabilities)
	}
}
```

(Use the existing resume-test helper to reach a terminal state — find how `resume_test.go` does it; do not invent.)

- [ ] **Step 2:** Run → FAIL (Resume signature mismatch).

- [ ] **Step 3: Extend `TaskStore.Resume`** (`:225`) with trailing `capsOverride bool, newCaps protocol.Capability`. Inside, after the terminal-check/reset, when `capsOverride` set `e.Capabilities = newCaps` and write a WAL event:

```go
	if capsOverride {
		e.Capabilities = newCaps
		if s.wal != nil {
			if err := s.wal.Write(WALEvent{Type: "task_caps_changed", TaskID: id, Capabilities: uint32(newCaps)}); err != nil {
				slog.Error("WAL write failed", "op", "task_caps_changed", "task_id", id, "err", err)
			}
		}
	}
```

(`WALEvent.Capabilities` already exists from the capabilities feature.)

- [ ] **Step 4: Replay** — in `ReplayEvents` (`:583`) add a case:

```go
	case "task_caps_changed":
		if t, ok := s.tasks[ev.TaskID]; ok {
			t.Capabilities = protocol.Capability(ev.Capabilities)
		}
```

(mirror how other event cases mutate `s.tasks[ev.TaskID]`.)

- [ ] **Step 5: Wire the resume branches** in `server/task_handler.go`. In `handleSubmit`'s resume branch (`handleSubmitResume`) and `handleOpenInteractive`'s resume branch, compute and pass the override. The handlers already have the caller `cid` (top-level in `Handle`) and the request. Pass:

```go
	override := req.ResumeCapsOverride == 1
	newCaps := intersectCaps(h.callerCaps(cid), req.RequestedCaps)
	... h.Tasks.Resume(id, prompt, extraArgs, sel, bound, origin, override, newCaps)
```

Update both resume call sites + the `Resume` callers in tests (pass `false, protocol.Capability_None` where override isn't exercised).

- [ ] **Step 6: WAL round-trip test** (extend `server/taskstore_test.go`): a `task_caps_changed` event Marshal/Unmarshal round-trips `Capabilities` (incl. value 0).

- [ ] **Step 7:** `go test ./server/ -run 'TestResumeCapsOverride|TestCaps' -v && make check && go test ./server/` → PASS.

- [ ] **Step 8:** Commit. `git add server/taskstore.go server/wal.go server/task_handler.go server/capabilities_test.go server/taskstore_test.go && git commit -m "feat(server): apply+persist resume capability override (resumer ∩ requested)"`

---

## Task 3: cli override builders + CLI `--caps` on resume

**Files:** Modify `cli/submit.go`, `cli/open_interactive_native.go`, `cli/open_interactive_wasm.go` (new builders); `cmd/harness-cli/main.go`, `cmd/harness-cli/session.go` (flag.Visit → call override builder). Test: `cmd/harness-cli/caps_test.go` or a cli test.

**Interfaces:**
- Produces: `cli.(*Client).SubmitResumeWithCaps(ctx, repo, prompt string, sel, extraArgs, resumeTaskID string, caps protocol.Capability) (string, error)`; `cli.(*Client).InteractiveResumeWithCaps(ctx, repo string, sel, extraArgs, resumeTaskID string, detachable bool, caps protocol.Capability) (string, error)` (native + wasm twins). Each sets `RequestedCaps=caps`, `ResumeCapsOverride=1`, `ResumeTaskId=resumeTaskID`.

- [ ] **Step 1:** Add `SubmitResumeWithCaps` in `cli/submit.go` — same body as `SubmitWithSelectorArgsAndCaps` but additionally `req.ResumeCapsOverride = 1` (and resumeTaskID is required/non-empty). Add `InteractiveResumeWithCaps` in `cli/open_interactive_native.go` AND the `//go:build js` twin `cli/open_interactive_wasm.go` — same as the `*ArgsAndCaps` interactive builder plus `oi.ResumeCapsOverride = 1`. (These are additive; existing builders unchanged → they keep ResumeCapsOverride=0.)

- [ ] **Step 2:** Write CLI test (`cmd/harness-cli/caps_test.go` or sibling): assert that with an explicit `--caps` on a resume invocation the resume override is requested, and without `--caps` it is not. Since the request building is in `cli`, the testable unit is the flag-detection helper: extract/assert `capsExplicitlySet(fs)` via `flag.Visit`:

```go
func TestCapsFlagExplicit(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	caps := fs.String("caps", "", "")
	_ = caps
	fs.Parse([]string{"--resume", "abc"}) // no --caps
	if capsExplicitlySet(fs) { t.Fatal("no --caps → not explicit") }
	fs2 := flag.NewFlagSet("t", flag.ContinueOnError)
	fs2.String("caps", "", "")
	fs2.Parse([]string{"--caps", "spawn"})
	if !capsExplicitlySet(fs2) { t.Fatal("--caps given → explicit") }
}
```

- [ ] **Step 3:** Implement `capsExplicitlySet(fs *flag.FlagSet) bool` (uses `fs.Visit` to check whether `"caps"` was set). In `cmd/harness-cli/main.go` (`submit`, `interactive`) and `session.go` (`session new`), where resume is active (resume id non-empty): if `capsExplicitlySet(fs)` → call the `*ResumeWithCaps` builder with `cli.ParseCaps(*caps)`; else keep the existing plain-resume builder (override stays 0).

- [ ] **Step 4:** `go test ./cmd/harness-cli/ -run TestCapsFlagExplicit -v && make check` → PASS.

- [ ] **Step 5:** Commit. `git add cli/ cmd/harness-cli/ && git commit -m "feat(cli): --caps on resume re-grants via ResumeWithCaps builder"`

---

## Task 4: TUI — `applyCapsOnResume` toggle

**Files:** Modify `tui/cmdline.go` (extend `caps` command with `--on-resume`), `tui/app.go` (`applyCapsOnResume` field + handler), `tui/interactive.go` (resume sites use override builder when on). Test: `tui/cmdline_test.go`.

**Interfaces:** Consumes the `*ResumeWithCaps` builders (Task 3), `App.sessionCaps` (session-default-caps feature). Produces `App.applyCapsOnResume bool` and `caps --on-resume on|off`.

- [ ] **Step 1:** Write test: `ParseCommand("caps --on-resume on", "r")` → a `CapsAction` with an `OnResume *bool` (true); `caps --on-resume off` → false; `caps` (plain) and `caps <names>` unchanged.

```go
func TestParseCapsOnResume(t *testing.T) {
	act, _ := ParseCommand("caps --on-resume on", "r")
	ca := act.(CapsAction)
	if ca.OnResume == nil || !*ca.OnResume { t.Fatal("on-resume on") }
	act, _ = ParseCommand("caps --on-resume off", "r")
	if ca := act.(CapsAction); ca.OnResume == nil || *ca.OnResume { t.Fatal("on-resume off") }
}
```

- [ ] **Step 2:** Run → FAIL.

- [ ] **Step 3:** Extend `CapsAction` with `OnResume *bool` (nil = not an on-resume command). In `ParseCommand`'s `case "caps"`: if first token is `--on-resume`, parse `on`/`off` into `OnResume`; else existing behavior. Add `App.applyCapsOnResume bool` (default false) to the `&App{` literal. In the `CapsAction` Update handler: if `act.OnResume != nil`, set `a.applyCapsOnResume = *act.OnResume` and status `"caps on-resume: on/off"`; the `Show` branch also prints `applyCapsOnResume`.

- [ ] **Step 4:** At TUI resume call sites (the resume paths in `tui/app.go` / `tui/interactive.go` that pass a non-empty resume id — e.g. the `r`/`R` resume action and `DoOpenDetachableSession` with a resume id): when `a.applyCapsOnResume` is true, call the `InteractiveResumeWithCaps` builder with `a.sessionCaps`; else the existing plain-resume builder. Thread `applyCapsOnResume` + `sessionCaps` as params into the helper (do not read globals inside the helper).

- [ ] **Step 5:** `go test ./tui/ -run TestParseCapsOnResume -v && go test ./tui/ && make check` → PASS.

- [ ] **Step 6:** Commit. `git add tui/ && git commit -m "feat(tui): caps --on-resume toggle re-grants session caps on resume"`

---

## Task 5: WebUI — apply-caps-on-resume checkbox

**Files:** Modify `cmd/harness-webui-wasm/main.go` (read `opts.resumeCapsOverride`), `webui/index.html` (checkbox), `webui/static/main.js` (`applyCapsOnResume` state + pass on resume), `webui/static/style.css` (if needed). Test: Playwright.

**Interfaces:** Consumes `*ResumeWithCaps` builders (Task 3); `harness.submit`/`startInteractive` opts gain `resumeCapsOverride: bool`.

- [ ] **Step 1:** In `harnessSubmit` and `harnessStartInteractive` (`cmd/harness-webui-wasm/main.go`), read `opts.Get("resumeCapsOverride")` (bool). When it is true AND a resume id is present, call the `*ResumeWithCaps` builder (with the `caps` already read); otherwise the existing path. (`resumeCapsOverride` absent/false → unchanged behavior.)

- [ ] **Step 2:** `make wasm-check && make check` → PASS.

- [ ] **Step 3:** In `webui/index.html`, add near the cap chips a checkbox `<label><input type="checkbox" id="caps-on-resume"> apply caps on resume</label>`. In `webui/static/main.js`, add `let applyCapsOnResume = false;`, wire the checkbox `change` to set it, and add `resumeCapsOverride: applyCapsOnResume` to the resume-capable `harness.submit`/`harness.startInteractive` opts (the same call sites that already pass `caps: spawnCaps` AND carry a resume id — and also the plain new-spawn calls may pass it harmlessly since the server ignores override on create, but prefer to send it only where a resume id is present). Style the checkbox to match the dark palette; ensure 390px layout unaffected.

- [ ] **Step 4:** `make webui-build && make check`. Playwright (best-effort; defer to server rebuild if not live): checkbox default unchecked → resuming a confined task keeps its caps; checked + chips set → resume re-grants. Verify dark/390px. If the running server serves embedded assets (not live), report the deferral.

- [ ] **Step 5:** Commit. `git add cmd/harness-webui-wasm/main.go webui/index.html webui/static/main.js webui/static/style.css && git commit -m "feat(webui): apply-caps-on-resume checkbox re-grants caps on resume"`

---

## Self-Review

**Spec coverage:** schema bit → T1; server apply+persist (resumer ∩ requested) + task_caps_changed WAL + replay + plain-resume-unchanged → T2; CLI explicit `--caps` on resume → T3; TUI toggle → T4; WebUI checkbox → T5. Opt-in/no-silent-widening enforced (override only set by explicit paths; defaults 0). ✓

**Type consistency:** `ResumeCapsOverride` (request field), `Resume(..., capsOverride bool, newCaps protocol.Capability)`, `task_caps_changed` WAL type, `*ResumeWithCaps` builders, `CapsAction.OnResume *bool`, `App.applyCapsOnResume`, `opts.resumeCapsOverride` — consistent across tasks. Authority is `intersectCaps(callerCaps(cid), RequestedCaps)` everywhere.

**Placeholder scan:** test helpers `markTerminalForTest`/`capsExplicitlySet` are anchored to "use existing resume-test setup" / are defined in-task; no TBD.
