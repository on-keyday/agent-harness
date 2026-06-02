# Peer agent identity + harness-cli SKILL embed — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Expose what each peer runs (`agent_bin`) and whether harness `.claude` injection happened (`skills_injected`) over the protocol so `ls`/snapshot can show it; and embed the agent SKILL into `harness-cli` so any peer can fetch it.

**Architecture:** Append two fields to `RunnerHello` (runner→server) and `RunnerInfo` (server→client) in the `.bgn` schema; the runner fills them from its config, the server stores+echoes them, and clients render per-task by joining `task.assigned_to → runner`. Separately, move `runner/agentskills` into a tiny shared package both `agent-runner` and a new `harness-cli skill` subcommand import.

**Tech Stack:** Go; `.bgn` schema codegen via `make protoregen` (brgen api server at `localhost:8181`); `go:embed`.

**Spec:** `docs/superpowers/specs/2026-06-02-peer-agent-identity-design.md`

---

## Execution context (all tasks)

- **Work in the worktree**: edit/build/commit under
  `/home/kforfk/workspace/remote-agent-harness/.harness-worktrees/0f0d4dd6b7d3b64354cf4ff249b87403/`.
  Editing via the parent-repo absolute path routes to the parent checkout (Pitfall 8). Run `git` from the worktree cwd.
- **Push to origin/main** (this session's established flow): after committing locally, cherry-pick the new commit onto `origin/main` in an isolated `git worktree add --detach /tmp/wt-push-main origin/main`, verify `git merge-base --is-ancestor origin/main $TIP`, `git push origin $TIP:main`, then `git worktree remove`. Do NOT force-push.
- **Codegen dependency**: Task 1 runs `make protoregen`, which POSTs to a brgen api server at `http://localhost:8181`. If it isn't running, that's a hard blocker — **surface it to the user (they author brgen) rather than hand-editing the generated serialization in `message.go`.** All later tasks depend on Task 1's regenerated accessors.
- **Generated accessor naming** (confirmed from existing code): variable-length field `agent_bin` → struct fields `AgentBin []uint8` / `AgentBinLen uint8`, setter `SetAgentBin([]uint8)`, read via `string(x.AgentBin)`. Bitfield `skills_injected` → getter `SkillsInjected() bool`, setter `SetSkillsInjected(bool)` (mirrors `Detachable()`/`SetDetachable()` at `runner/protocol/message.go:7885-7900`).

---

## File structure

| File | Responsibility | Change |
|------|----------------|--------|
| `runner/protocol/message.bgn` | wire schema (source of truth) | add `agent_bin` + `skills_injected` to `RunnerHello` and `RunnerInfo` |
| `runner/protocol/message.go` | generated | regenerate via `make protoregen` |
| `runner/connect.go` | runner hello build | set the two fields from config; + derivation helpers |
| `server/registry.go` | `RunnerEntry` | add `AgentBin string`, `SkillsInjected bool` |
| `server/runner_handler.go` | hello → registry | read the two fields into `RunnerEntry` |
| `server/task_handler.go` | `toRunnerInfo` | echo the two fields into `RunnerInfo` |
| `cli/list.go` | `ls` text render | per-task `agent=` join + RUNNERS columns; + `agentStr` helper |
| `cmd/harness-webui-wasm/main.go` | wasm snapshot JSON | add `agentBin` / `skillsInjected` to runner map |
| `runner/agentskills/embed.go` | **new** shared embed pkg | `package agentskills`; embed + `Skill(name)` |
| `runner/agentskill.go` | runner injection | use `agentskills.FS` instead of own embed |
| `cmd/harness-cli/main.go` | CLI | `skill [name]` subcommand + usage line |
| `runner/agentskills/harness-cli/SKILL.md` | the skill text | update the "no way to tell" caveat |

---

## Task 1: Schema — add agent_bin + skills_injected to RunnerHello & RunnerInfo, regenerate

**Files:**
- Modify: `runner/protocol/message.bgn` (RunnerHello ~line 50-56; RunnerInfo ~line 330-342)
- Regenerate: `runner/protocol/message.go`

- [ ] **Step 1: Edit `RunnerHello` in `runner/protocol/message.bgn`**

Append the two fields to the end of the `RunnerHello` format (after `allowed_roots :[allowed_roots_len]AllowedRoot`):

```
format RunnerHello:
    version :u8
    hostname_len :u8
    hostname :[hostname_len]u8
    max_tasks :u16
    allowed_roots_len :u8
    allowed_roots :[allowed_roots_len]AllowedRoot
    # basename of the agent binary the runner runs (--claude-bin): "claude" /
    # "gemini" / "codex" / "bash" / ... . Empty = unknown.
    agent_bin_len :u8
    agent_bin :[agent_bin_len]u8
    # whether the runner injects .claude/{settings.json,skills} for its tasks
    # (= the inbox hook AND this skill are present). False = peer follows none
    # of the skill conventions even if it is an agent CLI.
    skills_injected :u1
    reserved :u7
```

- [ ] **Step 2: Edit `RunnerInfo` in `runner/protocol/message.bgn`**

Append the same two fields to the end of `RunnerInfo` (after `last_seen :u64`):

```
format RunnerInfo:
    id :RunnerID
    hostname_len :u8
    hostname :[hostname_len]u8
    status :RunnerStatus
    max_tasks :u16
    allowed_roots_len :u8
    allowed_roots :[allowed_roots_len]AllowedRoot
    active_tasks_len :u16
    active_tasks :[active_tasks_len]ActiveTaskRef
    connected_at :u64  # unix nano
    last_seen :u64
    agent_bin_len :u8
    agent_bin :[agent_bin_len]u8
    skills_injected :u1
    reserved :u7
```

- [ ] **Step 3: Ensure brgen api server is reachable, then regenerate**

Run: `curl -s -o /dev/null -w "%{http_code}\n" http://localhost:8181/api/generate -X POST -d '{}' || true`
Expected: a reachable HTTP code (e.g. `400`/`200`), NOT a connection error. If connection refused, STOP and tell the user the brgen api server must be running for `make protoregen`.

Then: `make protoregen`
Expected: regenerates `runner/protocol/message.go`, exits 0.

- [ ] **Step 4: Verify generated accessors exist**

Run:
```bash
grep -nE 'func \(.*RunnerHello\) SetAgentBin|func \(.*RunnerHello\) SetSkillsInjected|func \(.*RunnerInfo\) SetAgentBin|func \(.*RunnerInfo\) SkillsInjected' runner/protocol/message.go
```
Expected: four matches (setters on RunnerHello/RunnerInfo, getter `SkillsInjected()` on RunnerInfo). Also confirm struct fields `AgentBin []uint8` exist on both. If the generated setter/getter names differ, note them — later tasks must use the actual generated names.

- [ ] **Step 5: Build**

Run: `go build ./...`
Expected: success (no references to the new fields yet, just the regenerated code compiles).

- [ ] **Step 6: Commit**

```bash
git add runner/protocol/message.bgn runner/protocol/message.go
git commit -m "feat(proto): add agent_bin + skills_injected to RunnerHello/RunnerInfo"
```

---

## Task 2: Runner — fill agent_bin + skills_injected in the hello, with tested derivation helpers

**Files:**
- Modify: `runner/connect.go` (hello build ~line 224-228; add helpers)
- Test: `runner/connect_agentid_test.go` (new)

- [ ] **Step 1: Write the failing test** — create `runner/connect_agentid_test.go`:

```go
package runner

import "testing"

func TestAgentBinBase(t *testing.T) {
	cases := map[string]string{
		"claude":          "claude",
		"/usr/bin/gemini": "gemini",
		"./codex":         "codex",
		"bash":            "bash",
		"":                "", // empty stays empty (not ".")
	}
	for in, want := range cases {
		if got := agentBinBase(in); got != want {
			t.Errorf("agentBinBase(%q)=%q want %q", in, got, want)
		}
	}
}

func TestSkillsInjected(t *testing.T) {
	// injected unless no-worktree without force-inject
	if !skillsInjected(false, false) {
		t.Error("worktree mode should inject")
	}
	if skillsInjected(true, false) {
		t.Error("no-worktree without force should NOT inject")
	}
	if !skillsInjected(true, true) {
		t.Error("no-worktree with force should inject")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./runner/ -run 'AgentBinBase|SkillsInjected'`
Expected: FAIL — `undefined: agentBinBase` / `undefined: skillsInjected`.

- [ ] **Step 3: Add the helpers and wire them into the hello build**

In `runner/connect.go`, add (top-level, near other helpers; ensure `path/filepath` is imported):

```go
// agentBinBase is the basename of the agent binary the runner runs, for peer
// identification over the wire. Empty stays empty (callers treat "" as unknown).
func agentBinBase(claudeBin string) string {
	if claudeBin == "" {
		return ""
	}
	return filepath.Base(claudeBin)
}

// skillsInjected reports whether the runner injects .claude/{settings.json,skills}
// for its tasks. Mirrors the guard in runner/session.go (!NoWorktree ||
// ForceInjectHarnessSettings).
func skillsInjected(noWorktree, forceInject bool) bool {
	return !noWorktree || forceInject
}
```

Then in the hello build, immediately before `hh.SetAllowedRoots(roots)` ... `hello.SetHello(hh)` (around `runner/connect.go:227`), set the new fields on `hh`:

```go
	hh.SetAllowedRoots(roots)
	hh.SetAgentBin([]byte(agentBinBase(cfg.ClaudeBin)))
	hh.SetSkillsInjected(skillsInjected(cfg.NoWorktree, cfg.ForceInjectHarnessSettings))
	hello.SetHello(hh)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./runner/ -run 'AgentBinBase|SkillsInjected'`
Expected: PASS.

- [ ] **Step 5: Build**

Run: `go build ./...`
Expected: success.

- [ ] **Step 6: Commit**

```bash
git add runner/connect.go runner/connect_agentid_test.go
git commit -m "feat(runner): report agent_bin + skills_injected in RunnerHello"
```

---

## Task 3: Server — store the two fields on RunnerEntry and echo them in RunnerInfo

**Files:**
- Modify: `server/registry.go` (`RunnerEntry` struct ~line 26-34)
- Modify: `server/runner_handler.go` (hello → entry ~line 82-90)
- Modify: `server/task_handler.go` (`toRunnerInfo` ~line 913-931)
- Test: `server/runnerinfo_agentid_test.go` (new)

- [ ] **Step 1: Add fields to `RunnerEntry`** in `server/registry.go`, after `MaxTasks int`:

```go
	MaxTasks       int                 // from RunnerHello.max_tasks (>=1)
	AgentBin       string              // from RunnerHello.agent_bin (basename of --claude-bin)
	SkillsInjected bool                // from RunnerHello.skills_injected
	ActiveTasks    map[string]struct{} // task_id (hex) set; len() = current load
```

- [ ] **Step 2: Read them when building the entry** in `server/runner_handler.go`, inside the `entry := &RunnerEntry{...}` literal (after `MaxTasks: maxTasks,`):

```go
		entry := &RunnerEntry{
			ID:             runnerID,
			Hostname:       string(hello.Hostname),
			AllowedRoots:   roots,
			MaxTasks:       maxTasks,
			AgentBin:       string(hello.AgentBin),
			SkillsInjected: hello.SkillsInjected(),
			ActiveTasks:    make(map[string]struct{}),
			ConnectedAt:    now,
			LastSeen:       now,
		}
```

- [ ] **Step 3: Write the failing test** — create `server/runnerinfo_agentid_test.go`:

```go
package server

import "testing"

func TestToRunnerInfoCarriesAgentIdentity(t *testing.T) {
	e := RunnerEntry{
		ID:             "ws:127.0.0.1:1-2",
		Hostname:       "h",
		AgentBin:       "gemini",
		SkillsInjected: true,
		ActiveTasks:    map[string]struct{}{},
		Conn:           stubConnHandle("ws:127.0.0.1:1-2"),
	}
	info := toRunnerInfo(e)
	if got := string(info.AgentBin); got != "gemini" {
		t.Errorf("AgentBin=%q want gemini", got)
	}
	if !info.SkillsInjected() {
		t.Error("SkillsInjected=false want true")
	}
}
```

Note: `stubConnHandle` — check `server/*_test.go` for the existing test stub that satisfies `ConnHandle` with a given ConnectionID (grep `ConnHandle` in `server/*_test.go`). Use that helper's actual name; if none exists, the minimal stub is a type with a `ConnectionID() objproto.ConnectionID` method returning a parsed CID. `toRunnerInfo` calls `r.Conn.ConnectionID()`, so `Conn` must be non-nil.

- [ ] **Step 4: Run test to verify it fails**

Run: `go test ./server/ -run TestToRunnerInfoCarriesAgentIdentity`
Expected: FAIL — `info.AgentBin` empty / `SkillsInjected()` false (echo not implemented yet).

- [ ] **Step 5: Echo the fields in `toRunnerInfo`** (`server/task_handler.go`), after `info.SetHostname([]byte(r.Hostname))`:

```go
	info.SetHostname([]byte(r.Hostname))
	info.SetAgentBin([]byte(r.AgentBin))
	info.SetSkillsInjected(r.SkillsInjected)
	info.Id = protocol.ConnIDToRunnerID(r.Conn.ConnectionID())
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./server/ -run TestToRunnerInfoCarriesAgentIdentity`
Expected: PASS.

- [ ] **Step 7: Full server build + existing tests**

Run: `go build ./... && go test ./server/ ./runner/`
Expected: success.

- [ ] **Step 8: Commit**

```bash
git add server/registry.go server/runner_handler.go server/task_handler.go server/runnerinfo_agentid_test.go
git commit -m "feat(server): store + echo agent_bin/skills_injected in RunnerInfo"
```

---

## Task 4: CLI render — show agent identity in `ls` (RUNNERS line + per-task join)

**Files:**
- Modify: `cli/list.go` (RUNNERS loop ~line 82-95; TASKS loop ~line 101-110; add `agentStr` helper)
- Test: `cli/list_agentid_test.go` (new)

- [ ] **Step 1: Write the failing test** — create `cli/list_agentid_test.go`:

```go
package cli

import "testing"

func TestAgentStr(t *testing.T) {
	cases := []struct {
		bin      string
		injected bool
		want     string
	}{
		{"claude", true, "agent=claude+skills"},
		{"gemini", false, "agent=gemini"},
		{"bash", false, "agent=bash"},
		{"", false, "agent=?"},
		{"", true, "agent=?+skills"},
	}
	for _, c := range cases {
		if got := agentStr(c.bin, c.injected); got != c.want {
			t.Errorf("agentStr(%q,%v)=%q want %q", c.bin, c.injected, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cli/ -run TestAgentStr`
Expected: FAIL — `undefined: agentStr`.

- [ ] **Step 3: Add the `agentStr` helper** in `cli/list.go` (near `originStr`):

```go
// agentStr renders a peer's agent descriptor for the ls output: the agent
// binary basename, plus "+skills" when the runner injects the harness skill.
// Empty bin renders as "?".
func agentStr(bin string, injected bool) string {
	if bin == "" {
		bin = "?"
	}
	if injected {
		return "agent=" + bin + "+skills"
	}
	return "agent=" + bin
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cli/ -run TestAgentStr`
Expected: PASS.

- [ ] **Step 5: Use it in the RUNNERS and TASKS render loops**

In the RUNNERS loop (`cli/list.go` ~line 87, the `fmt.Fprintf(out, "  %s  host=%s ...")`), append ` %s` + `agentStr(string(r.AgentBin), r.SkillsInjected())` to the format/args so each runner line ends with the agent descriptor.

In the TASKS loop (`cli/list.go:101-110`), build a runner lookup once before the loop and append the agent descriptor per task:

```go
	// Index runners by ConnID string for per-task agent lookup.
	runnerByID := make(map[string]protocol.RunnerInfo, len(lr.Runners))
	for _, r := range lr.Runners {
		runnerByID[protocol.RunnerIDToConnID(r.Id).String()] = r
	}
	fmt.Fprintln(out, "TASKS")
	if len(lr.Tasks) == 0 {
		fmt.Fprintln(out, "  (none)")
	}
	for _, t := range lr.Tasks {
		agent := ""
		if r, ok := runnerByID[protocol.RunnerIDToConnID(t.AssignedTo).String()]; ok {
			agent = "  " + agentStr(string(r.AgentBin), r.SkillsInjected())
		}
		fmt.Fprintf(out, "  %s  %s  repo=%s  from=%s%s  prompt=%q\n",
			taskIDStr(t.Id.Id[:]),
			taskStatusStr(t.Status),
			string(t.RepoPath),
			originStr(t.OriginKind),
			agent,
			string(t.Prompt),
		)
	}
```

- [ ] **Step 6: Build + cli tests**

Run: `go build ./... && go test ./cli/`
Expected: success.

- [ ] **Step 7: Commit**

```bash
git add cli/list.go cli/list_agentid_test.go
git commit -m "feat(cli): show agent=<bin>[+skills] per task and per runner in ls"
```

---

## Task 5: WebUI — surface agentBin/skillsInjected in the wasm snapshot

**Files:**
- Modify: `cmd/harness-webui-wasm/main.go` (runner map ~line 330-338)

- [ ] **Step 1: Add the two fields to the runner JSON map**

In `harnessSnapshot`, the `runners = append(runners, map[string]any{...})` literal (`cmd/harness-webui-wasm/main.go:330-338`), add two entries:

```go
				runners = append(runners, map[string]any{
					"hostname":       string(r.Hostname),
					"status":         r.Status.String(),
					"tasks":          float64(r.ActiveTasksLen),
					"maxTasks":       float64(r.MaxTasks),
					"roots":          roots,
					"connectedAt":    float64(r.ConnectedAt),
					"lastSeen":       float64(r.LastSeen),
					"agentBin":       string(r.AgentBin),
					"skillsInjected": r.SkillsInjected(),
				})
```

Also update the doc comment above `harnessSnapshot` (the `runners: [{...}]` shape list, ~line 305) to include `agentBin, skillsInjected`.

- [ ] **Step 2: Build the wasm**

Run: `GOOS=js GOARCH=wasm go build -o webui/static/main.wasm ./cmd/harness-webui-wasm/`
Expected: success. (`webui/static/main.wasm` is gitignored — do not add it.)

- [ ] **Step 3: Commit**

```bash
git add cmd/harness-webui-wasm/main.go
git commit -m "feat(webui): expose agentBin/skillsInjected in the wasm snapshot"
```

---

## Task 6: Update the SKILL.md caveat to reference the new `ls` column

**Files:**
- Modify: `runner/agentskills/harness-cli/SKILL.md` (the "Peers may not be claude" section)

- [ ] **Step 1: Replace the "no way to tell" sentence**

Find:
```
And **there is currently no way to tell from `harness-cli` what a peer is.**
`ls` / snapshot expose kind (Oneshot/Interactive), status, repo, and the
*creator's* `from=` ClientKind — but nothing about the spawned binary or whether
skills were injected. So your only practical signal is behavioral: a peer that
completes the `harness.hello` handshake and replies in the format you agreed on
is cooperative; treat anything that stays silent or answers opaquely as not
skill-following (possibly `bash`, or a human-driven PTY).
```

Replace with:
```
`ls` now shows each task's runner identity: an `agent=<bin>` column (the agent
binary basename — `claude` / `gemini` / `codex` / `bash` …), with `+skills` when
the runner injects the harness skill + inbox hook. So `agent=claude+skills` is a
conventional, skill-following peer; `agent=bash` or `agent=claude` (no `+skills`)
is not — it has neither this skill nor the auto-inbox hook. Behavior is still the
final word (does it complete `harness.hello`?), but you no longer have to guess.
```

- [ ] **Step 2: Verify skill tests still pass**

Run: `go test ./runner/ -run Skill`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add runner/agentskills/harness-cli/SKILL.md
git commit -m "docs(skill): peers are now distinguishable via ls agent= column"
```

---

## Task 7: Share `runner/agentskills` as a package; runner uses it

**Files:**
- Create: `runner/agentskills/embed.go`
- Modify: `runner/agentskill.go` (replace own embed + adjust walk root)
- Test: `runner/agentskills/agentskills_test.go` (new)

- [ ] **Step 1: Write the failing test** — create `runner/agentskills/agentskills_test.go`:

```go
package agentskills

import "testing"

func TestSkillHarnessCLI(t *testing.T) {
	b, err := Skill("harness-cli")
	if err != nil {
		t.Fatalf("Skill(harness-cli): %v", err)
	}
	if len(b) == 0 {
		t.Fatal("harness-cli skill is empty")
	}
}

func TestSkillUnknown(t *testing.T) {
	if _, err := Skill("nope"); err == nil {
		t.Fatal("expected error for unknown skill")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./runner/agentskills/`
Expected: FAIL — no Go files / `undefined: Skill`.

- [ ] **Step 3: Create `runner/agentskills/embed.go`**

```go
// Package agentskills embeds the harness agent skill files so both the runner
// (which injects them into task worktrees) and harness-cli (which prints them
// on demand) share one copy. It imports only the standard library.
package agentskills

import "embed"

//go:embed all:harness-cli
var FS embed.FS

// Skill returns the SKILL.md bytes for a named skill (e.g. "harness-cli").
func Skill(name string) ([]byte, error) {
	return FS.ReadFile(name + "/SKILL.md")
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./runner/agentskills/`
Expected: PASS.

- [ ] **Step 5: Repoint the runner injection to the shared FS**

In `runner/agentskill.go`: delete the `//go:embed all:agentskills` line and `var agentSkillsFS embed.FS`; remove the now-unused `"embed"` import; add the module import `"github.com/on-keyday/agent-harness/runner/agentskills"`. In `WriteAgentSkills`, change the walk to use the shared FS with root `"."` (the FS's top entry is now `harness-cli/`):

```go
func WriteAgentSkills(worktreeDir string) error {
	skillsDir := filepath.Join(worktreeDir, ".claude", "skills")

	err := fs.WalkDir(agentskills.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		dst := filepath.Join(skillsDir, filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		data, err := agentskills.FS.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o644)
	})
	if err != nil {
		return err
	}

	claudeMd := filepath.Join(worktreeDir, "CLAUDE.md")
	if _, statErr := os.Stat(claudeMd); statErr == nil {
		return nil
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	return os.WriteFile(claudeMd, []byte(claudeMdMinimal), 0o644)
}
```

(Keep `claudeMdMinimal` and the imports `errors`, `io/fs`, `os`, `path/filepath` in `agentskill.go`.)

- [ ] **Step 6: Run the runner skill test (injection unchanged)**

Run: `go test ./runner/ -run Skill && go build ./...`
Expected: PASS — `runner/agentskill_test.go` still passes (it materializes `.claude/skills/harness-cli/SKILL.md` exactly as before).

- [ ] **Step 7: Commit**

```bash
git add runner/agentskills/embed.go runner/agentskills/agentskills_test.go runner/agentskill.go
git commit -m "refactor(runner): share agentskills via a package for reuse by harness-cli"
```

---

## Task 8: `harness-cli skill [name]` subcommand

**Files:**
- Modify: `cmd/harness-cli/main.go` (add `case "skill"`; usage line)

- [ ] **Step 1: Add the subcommand**

In `cmd/harness-cli/main.go`, add the import `"github.com/on-keyday/agent-harness/runner/agentskills"`, and a new `case` in the top-level subcommand switch (alongside `case "ls":` etc.):

```go
	case "skill":
		name := "harness-cli"
		if len(args) > 0 {
			name = args[0]
		}
		md, err := agentskills.Skill(name)
		if err != nil {
			die(fmt.Errorf("skill %q: %w", name, err))
		}
		os.Stdout.Write(md)
```

- [ ] **Step 2: Add a usage line**

In the `usage()` function (`cmd/harness-cli/main.go`, the `Subcommands:` block), add:

```go
	fmt.Fprintln(os.Stderr, "  skill [NAME]                        print the embedded agent skill (default: harness-cli)")
```

- [ ] **Step 3: Build and smoke-test**

Run:
```bash
go build -o /tmp/harness-cli ./cmd/harness-cli/ && /tmp/harness-cli skill | head -3 && rm -f /tmp/harness-cli
```
Expected: prints the first lines of the harness-cli SKILL.md (the YAML frontmatter `---` / `name:` lines).

- [ ] **Step 4: Commit**

```bash
git add cmd/harness-cli/main.go
git commit -m "feat(cli): harness-cli skill [name] prints the embedded agent skill"
```

---

## Task 9: Integration build, full test, manual verification

- [ ] **Step 1: Full build + tests**

Run: `go build ./... && go test ./runner/... ./server/ ./cli/`
Expected: all pass.

- [ ] **Step 2: Manual end-to-end (optional, needs a temp stack)**

If verifying live: build `bin/harness-server` + `bin/agent-runner`; start a server, then two runners — one default (`--claude-bin claude`), one with `--claude-bin bash` (or `--no-worktree`). Run `harness-cli ls` and confirm runner/task lines show `agent=claude+skills` vs `agent=bash`. Then `harness-cli skill | head` prints the SKILL.md. Tear the stack down afterward (`scripts/runner.sh down --as <tag>`, `scripts/server.sh down --as <tag>`).

- [ ] **Step 3: Final commit if any fixups were needed** (else done).

---

## Self-review (spec coverage)

- **§3 schema (agent_bin + skills_injected on RunnerHello & RunnerInfo, no enum)** → Task 1.
- **§4 runner derivation (basename + !NoWorktree||ForceInject)** → Task 2 (helpers + tests, matches `session.go:398/589`).
- **§5 server store + echo** → Task 3.
- **§6 client render: cli per-task join + RUNNERS line; WebUI snapshot; SKILL.md caveat** → Task 4 (cli), Task 5 (webui), Task 6 (skill caveat).
- **§7 compat: append-only, rebuild together** → covered by execution-context note; no version branching added.
- **§8 tests + protoregen** → Tasks 1-4,7 unit tests; Task 1 protoregen; Task 9 integration.
- **§9 harness-cli SKILL embed (shared pkg + skill subcommand, runner injection unchanged)** → Task 7 (pkg + runner refactor) + Task 8 (subcommand).
- **Type/name consistency:** `AgentBin`/`SetAgentBin`/`SkillsInjected()`/`SetSkillsInjected` used consistently (per Task 1 Step 4 verification); `agentStr`, `agentBinBase`, `skillsInjected`, `agentskills.Skill`, `agentskills.FS` consistent across tasks.
- **No placeholders:** every code step has concrete code; the only deferred detail is the exact generated accessor names, gated by Task 1 Step 4 verification (a real codegen dependency, not a TODO).
