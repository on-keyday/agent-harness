# Multi-Agent-Profile Runner Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** One runner process advertises multiple named agent profiles (claude/codex/…); a session selects `(mode, profile, runner)` per open, so Claude and Codex no longer need separate runner processes.

**Architecture:** Agent identity moves from the runner *process* to the *session*. The runner holds an ordered set of profiles (bin + argv templates) and execs the one the server resolved; the server advertises profile names, filters/expands candidates by them, and threads the resolved name to the runner. `resume_task_id` (worktree/branch/conversation identity) is decoupled from the per-open `(mode, profile, runner)`; both mode (Kind) and profile become per-open, latest-recorded.

**Tech Stack:** Go; brgen `.bgn` schema (regen via `make protoregen`); wasm (WebUI); bubbletea (TUI); Python (`scripts/runner.py`).

**Spec:** `docs/superpowers/specs/2026-07-15-multi-agent-profile-runner-design.md` (read it — §3 wire, §4/§4a dispatch+picker, §4b profile-on-resume, §4c mode-on-resume, §5 runner exec, §6 UIs, Wiring inventory).

## Global Constraints

- **Read `.claude/skills/implementation-pitfalls/SKILL.md` before dispatching any implementer/reviewer subagent** (project mandate in CLAUDE.md).
- **Wire changes:** edit `runner/protocol/message.bgn`, then regenerate with `make protoregen ARGS='runner/protocol/message.bgn'`. NEVER hand-edit generated `runner/protocol/message.go`.
- **native + wasm parity:** `cli/open_interactive_native.go` and `cli/open_interactive_wasm.go` are separate build-tag impls; every funnel change touches BOTH or WebUI silently diverges. Same for any `cli/*_native.go` / `*_wasm.go` pair.
- **Empty profile = the runner's default (first) profile**, EXCEPT on the interactive+unpinned+no-`--agent` path, where the `(runner,profile)` picker enumerates all combos (spec §4a). oneshot `submit` and pinned `r`/`R` always resolve empty → default (never profile-ambiguous).
- **Verify targets:** `make check` / `make wasm-check` / `go vet ./...` / `go test ./...`; `make build` after landing. Do NOT rely on `./...` alone where the Makefile uses explicit package patterns.
- **Solo dogfood, both ends rebuilt together:** no cross-version wire compatibility shim required.
- **RunnerSelector string cids** already used by the picker (`cli/selector.go`); reuse, do not reinvent.
- `RunnerCandidate` and the `OpenInteractiveResponse` candidates block ALREADY exist in the schema (the AmbiguousRunner picker landed) — this plan *extends* them, it does not introduce them.

---

## File Structure

- `runner/protocol/message.bgn` (+ regen `message.go`) — wire fields.
- `runner/agent_profile.go` (new) — profile model, `--agent-profiles` parse, resolution.
- `cmd/agent-runner/main.go` — `--agent-profiles` flag, build default+extra profiles.
- `runner/connect.go` — advertise profile names in `RunnerHello`.
- `runner/session.go` — hold profile set, resolve incoming `agent_profile` → bin+argv.
- `server/registry.go` — `RunnerEntry.AgentProfiles`.
- `server/taskstore.go` — `TaskEntry.AgentProfile`, `Create`/`Resume` take profile + `newKind`, WAL + replay.
- `server/task_handler.go` — profile filter (submit), combo picker + resume resolution (interactive), Kind guard relaxation.
- `server/dispatch.go` — set `AssignTaskBody.agent_profile` from the task.
- `cli/submit.go`, `cli/open_interactive_native.go`, `cli/open_interactive_wasm.go`, `cli/x11.go` — funnel `agentProfile` param.
- `cmd/harness-cli/main.go`, `cmd/harness-cli/session.go` — `--agent` flag.
- `tui/client.go`, `tui/interactive.go` — `Do*` helpers + picker rows carry profile.
- `cmd/harness-webui-wasm/main.go`, `webui/static/main.js` — agent dropdown + `agent_profiles` marshalling.
- `scripts/runner.py` — `--agents claude,codex` preset.

---

## Task 1: Schema — profile advertisement + per-session agent_profile

**Files:**
- Modify: `runner/protocol/message.bgn`
- Regenerate: `runner/protocol/message.go` (via make, not by hand)
- Test: `runner/protocol/agent_profile_wire_test.go` (new)

**Interfaces:**
- Produces (generated Go): `protocol.AgentProfileName{}` with `SetName([]byte)`/`Name`; `RunnerHello.AgentProfiles []AgentProfileName` + `SetAgentProfiles`; `RunnerInfo.AgentProfiles`; `SubmitRequest.AgentProfile []byte` + `SetAgentProfile`; `OpenInteractiveRequest.AgentProfile`; `AssignTaskBody.AgentProfile`; `OpenExecRunnerRequest.AgentProfile`; `TaskInfo.AgentProfile`; `RunnerCandidate.Profile` + `SetProfile`; `SubmitStatus_ProfileUnavailable`.

- [ ] **Step 1: Add the AgentProfileName format and extend RunnerHello.** In `runner/protocol/message.bgn`, add before `RunnerHello`:

```
format AgentProfileName:
    name_len :u8
    name :[name_len]u8
```

Append to `RunnerHello` (after `skills_injected`/`reserved`):

```
    agent_profiles_len :u8
    agent_profiles :[agent_profiles_len]AgentProfileName
```

- [ ] **Step 2: Extend RunnerInfo, the two client requests, the two runner requests, TaskInfo, RunnerCandidate, SubmitStatus.**
  - `RunnerInfo`: append `agent_profiles_len :u8` + `agent_profiles :[agent_profiles_len]AgentProfileName`.
  - `SubmitRequest`: append `agent_profile_len :u8` + `agent_profile :[agent_profile_len]u8`.
  - `OpenInteractiveRequest`: append the same two lines.
  - `AssignTaskBody`: append the same two lines.
  - `OpenExecRunnerRequest`: append the same two lines (after existing conditional x11 block — place the fixed fields BEFORE the `if x11_enabled` block to keep the conditional last; match sibling ordering).
  - `TaskInfo`: append the same two lines.
  - `RunnerCandidate`: append `profile_len :u16` + `profile :[profile_len]u8`.
  - `SubmitStatus` enum: add `profile_unavailable = "profile_unavailable"`.

- [ ] **Step 3: Regenerate.**

Run: `make protoregen ARGS='runner/protocol/message.bgn'`
Expected: `runner/protocol/message.go` updated, `go build ./runner/protocol/` clean.

- [ ] **Step 4: Write the failing round-trip test.** In `runner/protocol/agent_profile_wire_test.go`, mirror existing wire tests in this package:

```go
func TestRunnerHelloAgentProfilesRoundTrip(t *testing.T) {
    var h protocol.RunnerHello
    var p1, p2 protocol.AgentProfileName
    p1.SetName([]byte("claude"))
    p2.SetName([]byte("codex"))
    h.SetAgentProfiles([]protocol.AgentProfileName{p1, p2})
    buf, err := h.Append(nil)
    if err != nil { t.Fatal(err) }
    var got protocol.RunnerHello
    if _, err := got.Read(buf); err != nil { t.Fatal(err) }
    if len(got.AgentProfiles) != 2 || string(got.AgentProfiles[1].Name) != "codex" {
        t.Fatalf("profiles round-trip: got %+v", got.AgentProfiles)
    }
}

func TestSubmitRequestAgentProfileRoundTrip(t *testing.T) {
    var s protocol.SubmitRequest
    s.SetAgentProfile([]byte("codex"))
    buf, err := s.Append(nil)
    if err != nil { t.Fatal(err) }
    var got protocol.SubmitRequest
    if _, err := got.Read(buf); err != nil { t.Fatal(err) }
    if string(got.AgentProfile) != "codex" { t.Fatalf("got %q", got.AgentProfile) }
}
```

(Confirm the exact encode/decode method names — `Append`/`Read` vs `Encode`/`Decode` — against an existing `runner/protocol/*_test.go`; use whatever the package already uses.)

- [ ] **Step 5: Run — verify fail then pass.**

Run: `go test ./runner/protocol/ -run AgentProfile -v`
Expected: compiles after regen; tests PASS.

- [ ] **Step 6: Commit.**

```bash
git add runner/protocol/message.bgn runner/protocol/message.go runner/protocol/agent_profile_wire_test.go
git commit -m "feat(proto): agent-profile advertisement + per-session agent_profile fields"
```

---

## Task 2: Runner profile model + `--agent-profiles` parsing

**Files:**
- Create: `runner/agent_profile.go`
- Modify: `cmd/agent-runner/main.go` (add `--agent-profiles` flag; assemble profile set)
- Test: `runner/agent_profile_test.go`

**Interfaces:**
- Consumes: existing `runner.ValidateOneshotArgvTemplate` / `ValidateResumeInteractiveArgvTemplate` (`runner/agent_command.go`).
- Produces: `type AgentProfile struct { Name, Bin string; AgentArgs, OneshotArgv, ResumeOneshotArgv, ResumeInteractiveArgv []string }`; `type ProfileSet struct { ... }` with `func NewProfileSet(defaultP AgentProfile, extra []AgentProfile) (ProfileSet, error)` (dup-name error), `func (ProfileSet) Resolve(name string) (AgentProfile, error)` (empty → default/first, unknown → error), `func (ProfileSet) Names() []string`; `func ParseAgentProfilesJSON(s string) ([]AgentProfile, error)`.

- [ ] **Step 1: Write failing tests.** `runner/agent_profile_test.go`:

```go
func TestProfileSetResolve(t *testing.T) {
    def := AgentProfile{Name: "claude", Bin: "claude"}
    ps, err := NewProfileSet(def, []AgentProfile{{Name: "codex", Bin: "codex"}})
    if err != nil { t.Fatal(err) }
    if p, _ := ps.Resolve(""); p.Name != "claude" { t.Fatalf("empty→default got %q", p.Name) }
    if p, _ := ps.Resolve("codex"); p.Bin != "codex" { t.Fatalf("got %q", p.Bin) }
    if _, err := ps.Resolve("gemini"); err == nil { t.Fatal("unknown must error") }
}

func TestProfileSetDupName(t *testing.T) {
    _, err := NewProfileSet(AgentProfile{Name: "claude"}, []AgentProfile{{Name: "claude"}})
    if err == nil { t.Fatal("dup name must error") }
}

func TestParseAgentProfilesJSON(t *testing.T) {
    ps, err := ParseAgentProfilesJSON(`[{"name":"codex","bin":"codex","oneshotArgv":["exec","{args}","{prompt}"],"resumeOneshotArgv":["exec","resume","--last","{args}","{prompt}"]}]`)
    if err != nil { t.Fatal(err) }
    if len(ps) != 1 || ps[0].Name != "codex" { t.Fatalf("got %+v", ps) }
}
```

- [ ] **Step 2: Run — verify fail.** `go test ./runner/ -run Profile -v` → FAIL (undefined).

- [ ] **Step 3: Implement `runner/agent_profile.go`.** Define the structs; `ParseAgentProfilesJSON` via `encoding/json` (struct tags `name,bin,agentArgs,oneshotArgv,resumeOneshotArgv,resumeInteractiveArgv`); `NewProfileSet` prepends default, checks unique `Name` (error listing the dup), validates each profile's argv via the existing `Validate*ArgvTemplate`; `Resolve("")` returns index 0, else linear match, else `fmt.Errorf("unknown agent profile %q (have %v)", name, ps.Names())`.

- [ ] **Step 4: Add the flag in `cmd/agent-runner/main.go`.** Add `AgentProfilesJSON string` to `mainConfig`; bind `fs.StringVar(&c.AgentProfilesJSON, "agent-profiles", c.AgentProfilesJSON, "JSON array of extra agent profiles: [{name,bin,oneshotArgv,resumeOneshotArgv,resumeInteractiveArgv,agentArgs}]")`. In the runner build path, construct the default `AgentProfile` from the existing `ClaudeBin`/`ClaudeArgs`/`AgentOneshotArgv`/… fields (name = basename of `ClaudeBin`), parse `AgentProfilesJSON`, and `NewProfileSet(default, extra)` — surfacing the error to stderr + non-zero exit (mirror the existing `parseAgentArgsFlag` error handling around `main.go:199-214`).

- [ ] **Step 5: Run — verify pass.** `go test ./runner/ -run Profile -v` → PASS. Also `go build ./cmd/agent-runner/`.

- [ ] **Step 6: Commit.**

```bash
git add runner/agent_profile.go runner/agent_profile_test.go cmd/agent-runner/main.go
git commit -m "feat(runner): agent profile set + --agent-profiles parsing"
```

---

## Task 3: Runner advertises profiles + execs the resolved profile

**Files:**
- Modify: `runner/connect.go` (fill `RunnerHello.agent_profiles`)
- Modify: `runner/session.go` (hold `ProfileSet`; resolve incoming `agent_profile` → bin+argv)
- Test: `runner/session_test.go` (extend)

**Interfaces:**
- Consumes: `ProfileSet` (Task 2); `AssignTaskBody.AgentProfile` / `OpenExecRunnerRequest.AgentProfile` (Task 1).
- Produces: `SessionConfig` carries a `Profiles ProfileSet` (replacing the single `ClaudeBin`+argv fields) and resolves per task.

- [ ] **Step 1: Advertise in `runner/connect.go`.** Where `hh.SetAgentBin(...)` is called (~`connect.go:322`), also build `[]protocol.AgentProfileName` from `cfg.Profiles.Names()` and `hh.SetAgentProfiles(...)`. Keep `agent_bin` = default profile basename. Thread the `ProfileSet` from `mainConfig` into the `connect` config struct alongside the existing `ClaudeBin`.

- [ ] **Step 2: Write failing test in `runner/session_test.go`.** Assert that a session config with two profiles execs the codex bin/argv when the assign carries `agent_profile="codex"`, and the default when empty:

```go
func TestSessionResolvesProfile(t *testing.T) {
    ps, _ := NewProfileSet(
        AgentProfile{Name: "claude", Bin: "claude", OneshotArgv: []string{"{args}", "-p", "{prompt}"}},
        []AgentProfile{{Name: "codex", Bin: "codex", OneshotArgv: []string{"exec", "{args}", "{prompt}"}}},
    )
    bin, argv, err := resolveExec(ps, "codex", /*oneshot*/ true, false, nil, "do it")
    if err != nil { t.Fatal(err) }
    if bin != "codex" || argv[0] != "exec" { t.Fatalf("got bin=%q argv=%v", bin, argv) }
    bin, _, _ = resolveExec(ps, "", true, false, nil, "do it")
    if bin != "claude" { t.Fatalf("empty→default got %q", bin) }
}
```

- [ ] **Step 3: Run — verify fail.** `go test ./runner/ -run ResolvesProfile -v` → FAIL.

- [ ] **Step 4: Implement.** In `runner/session.go`: replace the single `ClaudeBin`/`OneshotArgvTemplate`/`ResumeOneshotArgvTemplate`/`ResumeInteractiveArgvTemplate` `SessionConfig` fields with `Profiles ProfileSet`. Add helper `resolveExec(ps ProfileSet, profile string, oneshot, resume bool, extra []string, prompt string) (bin string, argv []string, err error)` that calls `ps.Resolve(profile)` then feeds the profile's bin + templates into the existing `buildOneshotArgs`/`buildInteractiveArgs`. At the exec site (~`session.go:656-661`), read the incoming `agent_profile` (plumb it from `AssignTaskBody`/`OpenExecRunnerRequest` into the session run params) and use `resolveExec`. On `Resolve` error, fail the task with the error (no silent default).

- [ ] **Step 5: Run — verify pass + package build.** `go test ./runner/ -run 'ResolvesProfile|Profile' -v` → PASS; `go build ./runner/ ./cmd/agent-runner/`.

- [ ] **Step 6: Commit.**

```bash
git add runner/connect.go runner/session.go runner/session_test.go
git commit -m "feat(runner): advertise profiles in Hello; exec the resolved profile"
```

---

## Task 4: Server — store profiles, submit-path filter + resolution

**Files:**
- Modify: `server/registry.go` (`RunnerEntry.AgentProfiles` from Hello)
- Modify: `server/taskstore.go` (`TaskEntry.AgentProfile`; `Create` takes profile; WAL)
- Modify: `server/task_handler.go` (`handleSubmit` + `handleSubmitResume` profile filter/resolution)
- Test: `server/task_handler_test.go`, `server/taskstore_test.go`

**Interfaces:**
- Consumes: `RunnerHello.AgentProfiles`, `SubmitRequest.AgentProfile`, `SubmitStatus_ProfileUnavailable` (Task 1).
- Produces: `RunnerEntry.AgentProfiles []string`; `TaskEntry.AgentProfile string`; `TaskStore.Create(..., agentProfile string)`; helper `func filterByProfile(cands []RunnerEntry, profile string) []RunnerEntry`; `func (e RunnerEntry) HasProfile(name string) bool`; `func (e RunnerEntry) DefaultProfile() string`.

- [ ] **Step 1: Registry stores profiles.** In `server/registry.go`, add `AgentProfiles []string` to `RunnerEntry`; populate from `RunnerHello.AgentProfiles` where `AgentBin` is set (`server/runner_handler.go:103` area). Add `HasProfile`/`DefaultProfile` (default = first, or the `AgentBin` fallback if the list is empty for a legacy runner).

- [ ] **Step 2: Failing test — submit filter.** `server/task_handler_test.go`:

```go
func TestSubmitProfileUnavailable(t *testing.T) {
    h := newTestHandler(t) // one runner advertising ["claude"]
    req := &protocol.SubmitRequest{}
    req.SetRepoPath([]byte("/repo")); req.SetPrompt([]byte("x")); req.SetAgentProfile([]byte("codex"))
    resp := h.handleSubmit(req, protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All)
    if resp.Status != protocol.SubmitStatus_ProfileUnavailable {
        t.Fatalf("got %v", resp.Status)
    }
}

func TestSubmitEmptyProfileUsesDefault(t *testing.T) {
    h := newTestHandler(t) // runner advertising ["claude","codex"]
    req := &protocol.SubmitRequest{}
    req.SetRepoPath([]byte("/repo")); req.SetPrompt([]byte("x")) // no agent_profile
    resp := h.handleSubmit(req, protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All)
    if resp.Status != protocol.SubmitStatus_Ok { t.Fatalf("got %v", resp.Status) }
    // TaskEntry.AgentProfile == "claude" (default/first)
}
```

(Follow the existing `newTestHandler`/registry-seeding helpers in `server/*_test.go`; add profile advertisement to the seed.)

- [ ] **Step 3: Run — verify fail.** `go test ./server/ -run 'SubmitProfile|SubmitEmptyProfile' -v` → FAIL.

- [ ] **Step 4: Implement submit filter/resolution.** In `handleSubmit` (`server/task_handler.go:513`), after the `len(cands)==0` cases and BEFORE the `len(cands)>1` switch, insert: if `profile := string(req.AgentProfile)` is non-empty, `cands = filterByProfile(cands, profile)`; if the result is empty (and the pre-filter set was non-empty) return `SubmitResponse{Status: SubmitStatus_ProfileUnavailable}` with an `error_msg` listing the advertised names. Keep the existing `>1` AmbiguousRunner branch (now over the filtered set). After `bound := cands[0]`, compute `resolved := profile; if resolved == "" { resolved = bound.DefaultProfile() }` and pass `resolved` to `Tasks.Create`. Apply the **same** insertion to `handleSubmitResume` (`:557`), but there `resolved` defaults to the resumed task's `AgentProfile` when `profile==""` (read via `PeekRepo`/`Get`).

- [ ] **Step 5: Taskstore — carry the profile.** In `server/taskstore.go`, add `AgentProfile string` to `TaskEntry`; add an `agentProfile` param to `Create` (set field + include in the `task_created` WAL write via a new `WALEvent.AgentProfile` json field, `omitempty`) and apply it in `ReplayEvents` `task_created`. Add `taskstore_test.go` assertions that Create stores it and replay restores it.

- [ ] **Step 6: Run — verify pass.** `go test ./server/ -run 'Submit|Taskstore|Profile' -v` → PASS.

- [ ] **Step 7: Commit.**

```bash
git add server/registry.go server/taskstore.go server/task_handler.go server/*_test.go
git commit -m "feat(server): store agent profiles; submit-path profile filter + resolution"
```

---

## Task 5: Server — thread the resolved profile to the runner (assign)

**Files:**
- Modify: `server/dispatch.go` (`AssignTaskBody.agent_profile` from the task)
- Test: `server/dispatch_test.go`

**Interfaces:**
- Consumes: `TaskEntry.AgentProfile` (Task 4); `AssignTaskBody.AgentProfile` (Task 1).
- Produces: assign messages carry the task's profile.

- [ ] **Step 1: Failing test.** In `server/dispatch_test.go`, assert `buildAssignMsg`/`AssignTaskBody` for a task whose `AgentProfile=="codex"` encodes `agent_profile="codex"`. (Locate the `AssignTaskBody{...}` construction near `server/dispatch.go:116`.)

- [ ] **Step 2: Run — verify fail.**

- [ ] **Step 3: Implement.** At the `AssignTaskBody{...}` build site, set `body.SetAgentProfile([]byte(task.AgentProfile))`. Ensure the `TaskEntry` (with `AgentProfile`) is in scope there (thread from the scheduler’s task lookup if needed).

- [ ] **Step 4: Run — verify pass.** `go test ./server/ -run Assign -v` → PASS.

- [ ] **Step 5: Commit.**

```bash
git add server/dispatch.go server/dispatch_test.go
git commit -m "feat(server): carry resolved agent_profile in AssignTaskBody"
```

---

## Task 6: Server — interactive (runner×profile) picker + resume resolution

**Files:**
- Modify: `server/task_handler.go` (`handleOpenInteractive`: combo expansion, candidate profile, resume default, thread to OpenExec)
- Test: `server/task_handler_test.go`

**Interfaces:**
- Consumes: `RunnerCandidate.Profile`, `OpenInteractiveRequest.AgentProfile`, `OpenExecRunnerRequest.AgentProfile` (Task 1); `RunnerEntry.AgentProfiles`/`HasProfile`/`DefaultProfile` (Task 4).
- Produces: interactive dispatch that (a) filters by `--agent` when given, (b) when unpinned + no `--agent`, enumerates `(runner, profile)` combos into candidates, (c) resolves resume default from the task's `AgentProfile`, (d) sets `OpenExecRunnerRequest.agent_profile`.

- [ ] **Step 1: Failing test — combo picker.** In `server/task_handler_test.go`:

```go
func TestOpenInteractiveMultiProfileBecomesCombos(t *testing.T) {
    h := newTestHandler(t) // ONE runner advertising ["claude","codex"], Any selector, no agent_profile
    req := &protocol.OpenInteractiveRequest{}
    req.SetRepoPath([]byte("/repo")) // Selector defaults to Any
    resp := h.handleOpenInteractive(nil, req, protocol.ClientKind_Tui, protocol.TaskID{}, protocol.Capability_All)
    if resp.Status != protocol.OpenInteractiveStatus_AmbiguousRunner { t.Fatalf("got %v", resp.Status) }
    if len(resp.Candidates) != 2 { t.Fatalf("want 2 combos, got %d", len(resp.Candidates)) }
    got := map[string]bool{string(resp.Candidates[0].Profile): true, string(resp.Candidates[1].Profile): true}
    if !got["claude"] || !got["codex"] { t.Fatalf("combos missing a profile: %v", got) }
}

func TestOpenInteractiveAgentFilterPicksProfile(t *testing.T) {
    h := newTestHandler(t) // ONE runner ["claude","codex"]
    req := &protocol.OpenInteractiveRequest{}
    req.SetRepoPath([]byte("/repo")); req.SetAgentProfile([]byte("codex"))
    // nil tuiConn only exercises error/candidate branches; assert NOT ambiguous (1 combo → proceeds)
    resp := h.handleOpenInteractive(nil, req, protocol.ClientKind_Tui, protocol.TaskID{}, protocol.Capability_All)
    if resp.Status == protocol.OpenInteractiveStatus_AmbiguousRunner { t.Fatal("filtered to codex should not be ambiguous") }
}
```

- [ ] **Step 2: Run — verify fail.**

- [ ] **Step 3: Implement combo expansion.** In `handleOpenInteractive` (`server/task_handler.go:647`): after computing `cands`, build a `combos []struct{ Entry RunnerEntry; Profile string }`:
  - `profile := string(req.AgentProfile)`; on **resume** with empty profile, default `profile` to the resumed task's `AgentProfile` (from `PeekRepo`/`Get`).
  - If `profile != ""`: one combo per candidate that `HasProfile(profile)` (Profile=profile). If none → `NoRunnerForRepo` (or a profile-specific error) — keep parity with submit’s `profile_unavailable` semantics but via the interactive status set.
  - If `profile == ""` **and** selector is `Any` (unpinned): expand each candidate × each advertised profile (Profile = that name).
  - If `profile == ""` **and** pinned: one combo per candidate using its `DefaultProfile()`.
  Then switch on `len(combos)`: `>1` → build `RunnerCandidate` per combo, additionally `rc.SetProfile([]byte(combo.Profile))` (keep existing cid/hostname/matchedRoot/tasks), return AmbiguousRunner. `==1` → `runner := combos[0].Entry`, `resolved := combos[0].Profile`.

- [ ] **Step 4: Thread to OpenExec + task.** On the Ok path, pass `resolved` into `Tasks.Create(..., resolved)` (fresh) or record it on resume (Task 7 adds `Resume` profile param), and set `OpenExecRunnerRequest`’s `agent_profile` where that request is built (search `OpenExecRunnerRequest{` in this handler / dispatch).

- [ ] **Step 5: Run — verify pass.** `go test ./server/ -run OpenInteractive -v` → PASS.

- [ ] **Step 6: Commit.**

```bash
git add server/task_handler.go server/task_handler_test.go
git commit -m "feat(server): interactive (runner,profile) candidate picker + resume profile resolution"
```

---

## Task 7: Server — mode (Kind) switchable on resume

**Files:**
- Modify: `server/taskstore.go` (`Resume` takes `newKind`, `agentProfile`; sets fields; WAL Kind; replay applies Kind)
- Modify: `server/task_handler.go` (relax the two Kind guards)
- Test: `server/taskstore_test.go`

**Interfaces:**
- Consumes: nothing new on the wire.
- Produces: `TaskStore.Resume(..., newKind protocol.TaskKind, agentProfile string)`; resume no longer rejected on Kind mismatch.

- [ ] **Step 1: Failing test — Kind flips + replays.** `server/taskstore_test.go`:

```go
func TestResumeSwitchesKindAndReplays(t *testing.T) {
    s := newTestStore(t)
    id := s.Create("/repo", "", protocol.TaskKind_Interactive, protocol.ClientKind_Tui, protocol.TaskID{}, "", anySel(), nil, protocol.Capability_All, "claude")
    s.Finish(id, 0, nil)
    if _, err := s.Resume(id, "prompt", nil, anySel(), "", protocol.ClientKind_Cli, false, protocol.Capability_All, protocol.TaskKind_Oneshot, "codex"); err != nil { t.Fatal(err) }
    if e, _ := s.Get(id); e.Kind != protocol.TaskKind_Oneshot || e.AgentProfile != "codex" {
        t.Fatalf("got kind=%v profile=%q", e.Kind, e.AgentProfile)
    }
    // replay: rebuild from WAL and assert Kind==Oneshot
    s2 := newTestStore(t); s2.ReplayEvents(s.dumpWALForTest())
    if e, _ := s2.Get(id); e.Kind != protocol.TaskKind_Oneshot { t.Fatalf("replay kind=%v", e.Kind) }
}
```

- [ ] **Step 2: Run — verify fail.**

- [ ] **Step 3: Implement store change.** In `Resume` (`server/taskstore.go:223`): add params `newKind protocol.TaskKind, agentProfile string`; set `e.Kind = newKind` and `e.AgentProfile = agentProfile`; add `Kind: uint8(newKind)` and `AgentProfile: agentProfile` to the `task_resumed` `WALEvent` write. In `ReplayEvents` `task_resumed` case, **unconditionally** add `t.Kind = protocol.TaskKind(ev.Kind)` and `t.AgentProfile = ev.AgentProfile`. (Add `AgentProfile string` json field to `WALEvent` in `server/wal.go` if not already added in Task 4.)

- [ ] **Step 4: Relax handler guards.** In `handleSubmitResume` (`server/task_handler.go:554`) change `if !ok || kind != protocol.TaskKind_Oneshot` → `if !ok` (existence only). In `handleOpenInteractive` resume (`:639`) change `if !ok || kind != protocol.TaskKind_Interactive` → `if !ok`. Update the two `Resume(...)` callsites to pass the invoked mode (`TaskKind_Oneshot` from submit, `TaskKind_Interactive` from open) and the resolved profile.

- [ ] **Step 5: Run — verify pass.** `go test ./server/ -run 'Resume|Kind' -v` → PASS.

- [ ] **Step 6: Commit.**

```bash
git add server/taskstore.go server/wal.go server/task_handler.go server/taskstore_test.go
git commit -m "feat(server): mode (Kind) + profile switchable on resume; relax Kind guards"
```

---

## Task 8: cli.Client funnels — thread agentProfile (native + wasm)

**Files:**
- Modify: `cli/submit.go`, `cli/open_interactive_native.go`, `cli/open_interactive_wasm.go`, `cli/x11.go`
- Test: `cli/submit_test.go` (+ a wasm-tagged test if the package has them)

**Interfaces:**
- Consumes: `SubmitRequest.AgentProfile` / `OpenInteractiveRequest.AgentProfile` (Task 1).
- Produces: funnels gain a trailing `agentProfile string` param and set the wire field. Signatures:
  - `SubmitWithSelectorArgsAndCaps(ctx, repo, prompt, sel, extraArgs, resumeTaskID, caps, resumeCapsOverride, resumeConversation, agentProfile string)`
  - `OpenInteractiveWithSelectorArgsAndCaps(ctx, repo, sel, extraArgs, resumeTaskID, caps, resumeCapsOverride, resumeConversation, agentProfile string)` (+ underlying `openInteractive`)
  - `OpenInteractiveX11(...) / RunInteractiveX11(...)` gain the same trailing param.

- [ ] **Step 1: Failing test.** In `cli/submit_test.go`, if the package exposes request-building, assert the built `SubmitRequest.AgentProfile == "codex"` when passed. Otherwise add a thin test that calls the funnel against a fake conn and inspects the encoded request. (Match existing `cli/*_test.go` style; if none isolate the request build into a tested helper.)

- [ ] **Step 2: Run — verify fail.**

- [ ] **Step 3: Implement — set the wire field.** In `cli/submit.go` where `sub := protocol.SubmitRequest{}` is built (`:52`), add `sub.SetAgentProfile([]byte(agentProfile))`. In BOTH `cli/open_interactive_native.go` (`:128`) and `cli/open_interactive_wasm.go` (`:128`) where `oi := protocol.OpenInteractiveRequest{}` is built, add `oi.SetAgentProfile([]byte(agentProfile))`. Add the `agentProfile` param to the funnels + all delegating wrappers (`Submit`, `SubmitWithSelector*`, `Interactive*`, `OpenInteractive*`, X11) passing `""` from the thin convenience wrappers. Keep native/wasm signatures identical.

- [ ] **Step 4: Run — verify pass + wasm build.** `go test ./cli/ -run Submit -v` → PASS; `GOOS=js GOARCH=wasm go build ./cli/` (or `make wasm-check`).

- [ ] **Step 5: Commit.**

```bash
git add cli/submit.go cli/open_interactive_native.go cli/open_interactive_wasm.go cli/x11.go cli/*_test.go
git commit -m "feat(cli): thread agentProfile through session funnels (native+wasm)"
```

---

## Task 9: CLI `--agent` flag

**Files:**
- Modify: `cmd/harness-cli/main.go` (`submit`, `interactive` commands), `cmd/harness-cli/session.go` (open-detachable / x11 / interactive)
- Test: `cmd/harness-cli/main_test.go` (flag parse)

**Interfaces:**
- Consumes: the Task 8 funnels.
- Produces: `--agent <name>` on submit + session subcommands, forwarded as the funnel's `agentProfile`.

- [ ] **Step 1: Failing test.** Assert `--agent codex` parses to a string var and is passed. (Mirror existing flag tests, e.g. `cmd/harness-cli/*_test.go`.)

- [ ] **Step 2: Run — verify fail.**

- [ ] **Step 3: Implement.** Add `agent := fs.String("agent", "", "agent profile name (empty = runner default)")` to the relevant flag sets in `main.go` and `session.go`; pass `*agent` as the trailing arg to `SubmitWithSelectorArgsAndCaps` (`main.go:124`), `InteractiveWithSelectorArgsAndCaps` (`main.go:321`, `session.go:246`), `OpenInteractiveWithSelectorArgsAndCaps` (`session.go:228`), `RunInteractiveX11` (`session.go:238`).

- [ ] **Step 4: Run — verify pass + build.** `go test ./cmd/harness-cli/ -v`; `go build ./cmd/harness-cli/`.

- [ ] **Step 5: Commit.**

```bash
git add cmd/harness-cli/main.go cmd/harness-cli/session.go cmd/harness-cli/*_test.go
git commit -m "feat(cli): --agent flag on submit + session commands"
```

---

## Task 10: TUI agent selection (compose + picker rows)

**Files:**
- Modify: `tui/client.go` (`DoSubmit`/`DoSubmitWithOpts`), `tui/interactive.go` (`DoOpenDetachableSession`/`DoResumeSession` + callsites), and the picker/compose models
- Test: `tui/*_test.go` (pure-function mapping test)

**Interfaces:**
- Consumes: Task 8 funnels; `RunnerCandidate.Profile` (Task 1); `RunnerInfo.AgentProfiles` (Task 1/4).
- Produces: `Do*` helpers gain a trailing `agentProfile string`; the ambiguous-resume picker rows render + select profile; the compose/new-session flow offers an agent choice.

- [ ] **Step 1: Failing test.** Extend the existing picker/`resumeSelectorOpts`-style test to assert a chosen `(runner, profile)` candidate maps to `SelectorOpts{Runner: cid}` **and** `agentProfile == candidate.Profile`.

- [ ] **Step 2: Run — verify fail.**

- [ ] **Step 3: Implement.** Add `agentProfile` param to `DoSubmitWithOpts` (`tui/client.go:64`), `DoOpenDetachableSession` (`tui/interactive.go:73`), `DoResumeSession` (`:104`); forward to the funnels at each callsite (`tui/interactive.go:79,111,117,143,181,224`, `tui/client.go:73`). Extend the AmbiguousRunner picker (from the picker spec) so each row shows `host · agent · matched_root · cid` (read `candidate.Profile`) and selection carries the profile into the re-issued command. Add an agent picker to the compose/new-session flow populated from the target runner's `RunnerInfo.AgentProfiles` (default = first / task's profile on resume). Reuse the long-lived `*cli.Client` (do not open a new one).

- [ ] **Step 4: Run — verify pass + build.** `go test ./tui/ -v`; `go build ./tui/ ./cmd/harness-tui/`.

- [ ] **Step 5: Commit.**

```bash
git add tui/client.go tui/interactive.go tui/*.go tui/*_test.go
git commit -m "feat(tui): agent selection in compose + (runner,profile) resume picker"
```

---

## Task 11: WebUI agent selection (wasm bridge + JS)

**Files:**
- Modify: `cmd/harness-webui-wasm/main.go` (`harnessSubmit`, `harnessStartInteractive`, runner list mapping)
- Modify: `webui/static/main.js` (new-session form + resume modal dropdown)
- Verify: Playwright (per `project_playwright_webui_visual_check`)

**Interfaces:**
- Consumes: Task 8 funnels; `RunnerInfo.AgentProfiles`; `RunnerCandidate.Profile`.
- Produces: `harness.submit` / `harness.startInteractive` accept an `agent` arg; the runner list exposes `agentProfiles`.

- [ ] **Step 1: Expose profiles to JS.** In the runner list mapping (`cmd/harness-webui-wasm/main.go:452` area), add `"agentProfiles": stringsToJS(r.AgentProfiles)` alongside `agentBin`.

- [ ] **Step 2: Read the agent arg in the bridge.** In `harnessSubmit` (`main.go:350`) and `harnessStartInteractive` (`main.go:862`), read an `agent` field from the JS args object and pass it as the trailing `agentProfile` to the funnels.

- [ ] **Step 3: JS UI.** In `webui/static/main.js`, add an agent `<select>` to the new-session form (options from the selected runner's `agentProfiles`) and to the resume modal (default = task's profile); pass `agent` in the `harness.submit` / `harness.startInteractive` calls. Match the dark/mobile theme (per `feedback_webui_dark_theme_and_mobile`).

- [ ] **Step 4: Build + verify.** `make wasm-check` (or `GOOS=js GOARCH=wasm go build ./cmd/harness-webui-wasm/`). Then, per the verify skill, drive it in Playwright: create a session choosing `codex`, confirm the codex process launches; reload → resume modal shows the agent dropdown. (WebUI hot-reloads — no server restart; `feedback_webui_hot_reload_no_server_restart`.)

- [ ] **Step 5: Commit.**

```bash
git add cmd/harness-webui-wasm/main.go webui/static/main.js
git commit -m "feat(webui): agent dropdown in new-session + resume; expose agentProfiles"
```

---

## Task 12: `runner.py` `--agents` preset

**Files:**
- Modify: `scripts/runner.py` (add `--agents claude,codex` → `--agent-profiles` JSON)
- Test: `scripts/` unit test if present, else a `--dry-run`/print assertion

**Interfaces:**
- Consumes: `--agent-profiles` flag (Task 2).
- Produces: `runner.sh up --agents claude,codex` expands known agents to the profiles JSON, keeping `runner.sh` a pure pass-through.

- [ ] **Step 1: Define the known-agent presets.** In `scripts/runner.py`, add a dict mapping `claude`/`codex`/`gemini` → their `{bin, oneshotArgv, resumeOneshotArgv, resumeInteractiveArgv}` (claude = the existing defaults; codex/gemini = their documented non-interactive/resume invocations). Add `--agents` (comma list); when present, the FIRST becomes the default profile (its bin/argv map to `--agent-bin`/`--agent-*-argv`) and the REST are serialized into `--agent-profiles` JSON.

- [ ] **Step 2: Failing test / dry-run check.** Assert `runner.py up --agents claude,codex --dry-run` (or the print path) emits `--agent-bin claude` + an `--agent-profiles` JSON containing `codex`.

- [ ] **Step 3: Implement + run.** Wire the expansion; run the dry-run check → PASS.

- [ ] **Step 4: Commit.**

```bash
git add scripts/runner.py
git commit -m "feat(scripts): runner.py --agents preset expands to --agent-profiles"
```

---

## Final integration + verify

- [ ] `make check && make wasm-check && go vet ./... && go test ./...` all green.
- [ ] **Real drive (verify skill):** `scripts/runner.sh up --agents claude,codex` (single runner). From CLI: `harness-cli submit --agent codex …` → assert codex ran; `harness-cli session new --agent codex …`. From TUI: `u`/`U` on a finished task → picker lists `{claude, codex}` rows → choose codex. From WebUI: new-session dropdown + resume modal (Playwright). Reopen a finished oneshot worktree as an interactive session under the other agent → worktree state carried over (§4c).
- [ ] `make build` in the main checkout after landing (per `feedback_build_after_landing`).

## Self-Review notes (author)

- Spec coverage: §3 wire → T1; §2 runner cfg → T2; §5 exec + Hello → T3; §4 submit filter → T4/T5; §4a picker + §4b resume profile → T6; §4c mode → T7; funnels/§Wiring → T8; CLI/TUI/WebUI (§6) → T9/T10/T11; runner.py preset (§2) → T12.
- native+wasm parity enforced in T1 (both open_interactive impls) and T8.
- `WALEvent.AgentProfile` introduced in T4, reused in T7 — single definition, no duplication.
