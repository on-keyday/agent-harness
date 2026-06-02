# Agent-aware injection (cross-tool instructions + skills) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Inject cross-tool agent config — `AGENTS.md`/`GEMINI.md` pointers + skills under both `.claude/skills/` and `.agents/skills/` — alongside the existing `.claude/` injection, so non-claude agents (codex/gemini) also get the harness-cli skill and instructions.

**Architecture:** Extend `runner.WriteAgentSkills` to (1) materialize the embedded skills into both `.claude/skills/` and `.agents/skills/`, and (2) write the minimal pointer to `CLAUDE.md`/`AGENTS.md`/`GEMINI.md` (each only-if-absent, protecting project files). Update the pointer text + `HarnessInjectedPaths` + the SKILL.md docs. No protocol change, no `agent_bin` branching.

**Tech Stack:** Go; `//go:embed` via the `runner/agentskills` package.

**Spec:** `docs/superpowers/specs/2026-06-02-agent-aware-injection-design.md`

---

## Execution context (all tasks)

- **Work in the worktree**: edit/build/commit under
  `/home/kforfk/workspace/remote-agent-harness/.harness-worktrees/0f0d4dd6b7d3b64354cf4ff249b87403/`.
  Editing via the parent-repo absolute path routes to the parent checkout (Pitfall 8).
- **Push to origin/main**: after committing locally, cherry-pick the new commit(s) onto `origin/main` in an isolated `git worktree add --detach /tmp/wt-push-main origin/main`, verify `git merge-base --is-ancestor origin/main $TIP`, `git push origin $TIP:main`, `git worktree remove`. Never force-push.
- **Keep `HarnessInjectedPaths` in sync with the writers** — `runner/agentinjected.go`'s own comment warns that a new injected file not listed there makes worktrees look "dirty" and stops cleanup. Task 3 covers this.
- **Deploy note (post-merge)**: this changes runner injection, so it reaches new task worktrees only after the runner is rebuilt+restarted (`scripts/build_and_restart_all.py` on the runner host). No server/protocol change, so server need not be rebuilt for this alone.

---

## File structure

| File | Responsibility | Change |
|------|----------------|--------|
| `runner/agentskill.go` | injection writer | extend `WriteAgentSkills`: both skill dirs + 3 pointers; update `claudeMdMinimal` text + helpers |
| `runner/agentinjected.go` | injected-path list | add `AGENTS.md`, `GEMINI.md`, `.agents/skills/` |
| `runner/agentskill_test.go` | tests | add cases for `.agents/skills/`, AGENTS.md/GEMINI.md write + preserve, pointer content |
| `runner/agentinjected_test.go` | **new** | assert the list covers the new injections |
| `runner/agentskills/harness-cli/SKILL.md` | the skill text | add harness-injected doc note; update peer-identity `+skills` caveat |

---

## Task 1: Update the injected pointer text (`claudeMdMinimal`)

**Files:**
- Modify: `runner/agentskill.go` (`claudeMdMinimal` const + its comment, lines 12-22)
- Test: `runner/agentskill_test.go` (new test)

- [ ] **Step 1: Write the failing test** — append to `runner/agentskill_test.go`:

```go
func TestClaudeMdMinimalContent(t *testing.T) {
	if !strings.Contains(claudeMdMinimal, "harness-cli skill harness-cli") {
		t.Error("pointer should route any agent to `harness-cli skill harness-cli`")
	}
	if !strings.Contains(claudeMdMinimal, ".agents/skills/harness-cli/SKILL.md") {
		t.Error("pointer should mention the .agents/skills location too")
	}
	if !strings.Contains(claudeMdMinimal, "do not commit") {
		t.Error("pointer should tell agents not to commit harness-injected files")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./runner/ -run TestClaudeMdMinimalContent`
Expected: FAIL — the current pointer mentions neither `harness-cli skill harness-cli` nor `.agents/skills` nor "do not commit".

- [ ] **Step 3: Replace the const + comment** in `runner/agentskill.go`:

```go
// claudeMdMinimal is written to <worktree>/{CLAUDE,AGENTS,GEMINI}.md only when
// that file does not already exist. It tells a cold-started agent (claude,
// codex, gemini, …) that harness-cli + the bundled skill are available, how to
// read the skill in any agent, and that harness-injected files are not its work.
const claudeMdMinimal = `This task runs inside a harness-managed worktree.

- ` + "`harness-cli`" + ` is on PATH; ` + "`HARNESS_*`" + ` env vars are pre-set by the runner.
- Read the harness-cli skill for agent-to-agent messaging on the agentboard:
  run ` + "`harness-cli skill harness-cli`" + ` (works in any agent), or open
  ` + "`.claude/skills/harness-cli/SKILL.md`" + ` / ` + "`.agents/skills/harness-cli/SKILL.md`" + `.
- Reserved well-known topic for the initial handshake: ` + "`harness.hello`" + `.

Harness-injected files in this worktree are NOT your work — do not commit them
as your own: this file (CLAUDE.md/AGENTS.md/GEMINI.md), ` + "`.claude/`" + `, and
` + "`.agents/skills/`" + `. If you intentionally add project-specific content to
one of them, that addition IS legitimate work and may be committed.
`
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./runner/ -run TestClaudeMdMinimalContent`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add runner/agentskill.go runner/agentskill_test.go
git commit -m "feat(runner): pointer routes any agent to harness-cli skill + don't-commit note"
```

---

## Task 2: Inject to both skill dirs + all three pointers

**Files:**
- Modify: `runner/agentskill.go` (`WriteAgentSkills` body + two helpers, lines 24-65)
- Test: `runner/agentskill_test.go` (new tests)

- [ ] **Step 1: Write the failing tests** — append to `runner/agentskill_test.go`:

```go
func TestWriteAgentSkills_WritesAgentsSkillsLocation(t *testing.T) {
	dir := t.TempDir()
	if err := WriteAgentSkills(dir); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		filepath.Join(dir, ".claude", "skills", "harness-cli", "SKILL.md"),
		filepath.Join(dir, ".agents", "skills", "harness-cli", "SKILL.md"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected skill at %s: %v", p, err)
		}
	}
}

func TestWriteAgentSkills_WritesAgentsAndGeminiPointers(t *testing.T) {
	dir := t.TempDir()
	if err := WriteAgentSkills(dir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"CLAUDE.md", "AGENTS.md", "GEMINI.md"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s not written: %v", name, err)
		}
		if !strings.Contains(string(data), "harness-cli") {
			t.Errorf("%s should mention harness-cli", name)
		}
	}
}

func TestWriteAgentSkills_PreservesExistingAgentsMd(t *testing.T) {
	dir := t.TempDir()
	original := []byte("# project AGENTS.md\nproject rules\n")
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteAgentSkills(dir); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if string(got) != string(original) {
		t.Errorf("existing AGENTS.md was modified:\nwant %q\ngot  %q", original, got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./runner/ -run 'WritesAgentsSkillsLocation|WritesAgentsAndGeminiPointers|PreservesExistingAgentsMd'`
Expected: FAIL — `.agents/skills/...` missing; `AGENTS.md`/`GEMINI.md` not written.

- [ ] **Step 3: Replace `WriteAgentSkills` (and add helpers)** in `runner/agentskill.go`:

```go
// WriteAgentSkills materialises the bundled skills into both the Claude
// (.claude/skills) and cross-tool (.agents/skills) locations, and writes a
// minimal instruction pointer to CLAUDE.md/AGENTS.md/GEMINI.md when each is
// absent. Skill files are always overwritten so runner upgrades ship updated
// guidance; pointer files are never overwritten — a project may provide its own.
func WriteAgentSkills(worktreeDir string) error {
	for _, root := range []string{
		filepath.Join(worktreeDir, ".claude", "skills"),
		filepath.Join(worktreeDir, ".agents", "skills"),
	} {
		if err := materializeSkills(root); err != nil {
			return err
		}
	}
	for _, name := range []string{"CLAUDE.md", "AGENTS.md", "GEMINI.md"} {
		if err := writePointerIfAbsent(filepath.Join(worktreeDir, name)); err != nil {
			return err
		}
	}
	return nil
}

// materializeSkills copies the embedded skill tree into destRoot, overwriting
// existing files.
func materializeSkills(destRoot string) error {
	return fs.WalkDir(agentskills.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		dst := filepath.Join(destRoot, filepath.FromSlash(p))
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
}

// writePointerIfAbsent writes claudeMdMinimal to path only when no file exists
// there, leaving a project's own pointer file untouched.
func writePointerIfAbsent(path string) error {
	if _, statErr := os.Stat(path); statErr == nil {
		return nil
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	return os.WriteFile(path, []byte(claudeMdMinimal), 0o644)
}
```

- [ ] **Step 4: Run tests to verify they pass (and existing tests still pass)**

Run: `go test ./runner/ -run 'WriteAgentSkills|ClaudeMdMinimal'`
Expected: PASS — including the pre-existing `TestWriteAgentSkills_PreservesExistingClaudeMd` (CLAUDE.md still only-if-absent via `writePointerIfAbsent`).

- [ ] **Step 5: Commit**

```bash
git add runner/agentskill.go runner/agentskill_test.go
git commit -m "feat(runner): inject skills to .agents/skills and pointers to AGENTS.md/GEMINI.md"
```

---

## Task 3: Add new paths to `HarnessInjectedPaths`

**Files:**
- Modify: `runner/agentinjected.go` (the `HarnessInjectedPaths` slice)
- Test: `runner/agentinjected_test.go` (new)

- [ ] **Step 1: Write the failing test** — create `runner/agentinjected_test.go`:

```go
package runner

import "testing"

func TestHarnessInjectedPathsCoverNewInjections(t *testing.T) {
	has := func(p string) bool {
		for _, x := range HarnessInjectedPaths {
			if x == p {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"AGENTS.md", "GEMINI.md", ".agents/skills/"} {
		if !has(want) {
			t.Errorf("HarnessInjectedPaths missing %q (writer/list out of sync)", want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./runner/ -run TestHarnessInjectedPathsCoverNewInjections`
Expected: FAIL — `AGENTS.md`, `GEMINI.md`, `.agents/skills/` not yet listed.

- [ ] **Step 3: Update the slice** in `runner/agentinjected.go`:

```go
var HarnessInjectedPaths = []string{
	"CLAUDE.md",
	"AGENTS.md",
	"GEMINI.md",
	".claude/settings.json",
	".claude/skills/",
	".agents/skills/",
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./runner/ -run TestHarnessInjectedPathsCoverNewInjections`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add runner/agentinjected.go runner/agentinjected_test.go
git commit -m "feat(runner): list AGENTS.md/GEMINI.md/.agents/skills as harness-injected"
```

---

## Task 4: SKILL.md — harness-injected note + cross-tool `+skills` caveat

**Files:**
- Modify: `runner/agentskills/harness-cli/SKILL.md`

- [ ] **Step 1: Add a "harness-injected files" note**

Open `runner/agentskills/harness-cli/SKILL.md`. Immediately before the final `## Trust model` section, insert:

```markdown
## Harness-injected files — don't commit them

The runner injects these into your worktree; they are NOT your work: the pointer
files (`CLAUDE.md` / `AGENTS.md` / `GEMINI.md`), `.claude/` (settings + skills),
and `.agents/skills/`. Don't commit them as your own. If you deliberately add
project-specific content to one of them, that addition is legitimate work and
may be committed.

```

- [ ] **Step 2: Update the `+skills` caveat to cross-tool**

In the "Peers may not be claude" section, find:

```
`ls` now shows each task's runner identity: an `agent=<bin>` column (the agent
binary basename — `claude` / `gemini` / `codex` / `bash` …), with `+skills` when
the runner did its harness injection (settings hook + this skill). **That
injection is currently claude-specific** — it writes `.claude/`. So:
```

Replace with:

```
`ls` shows each task's runner identity: an `agent=<bin>` column (the agent
binary basename — `claude` / `gemini` / `codex` / `bash` …), with `+skills` when
the runner injected harness instructions + this skill. Injection is now
cross-tool — `AGENTS.md`/`GEMINI.md`/`CLAUDE.md` pointers plus the skill under
both `.claude/skills/` and `.agents/skills/` — so `+skills` means a skill-aware
peer regardless of agent. The one claude-only piece is the **auto-inbox hook**
(`.claude/settings.json`); a non-claude `+skills` peer still has the skill but
must poll `harness-cli agent inbox` itself. So:
```

(If the exact wording differs, grep `agent=<bin>` in the file and adapt — keep the meaning: `+skills` is now cross-tool; auto-inbox remains claude-only.)

- [ ] **Step 3: Verify the skill tests still pass**

Run: `go test ./runner/ -run Skill`
Expected: PASS (the existing skill-content tests assert frontmatter/handshake/JSON — unaffected).

- [ ] **Step 4: Commit**

```bash
git add runner/agentskills/harness-cli/SKILL.md
git commit -m "docs(skill): harness-injected note + cross-tool +skills caveat"
```

---

## Task 5: Integration build + verification

- [ ] **Step 1: Full build + runner tests**

Run: `go build ./... && go test ./runner/...`
Expected: all pass.

- [ ] **Step 2: Injection check via the unit tests (no live agent needed)**

Run: `go test ./runner/ -run WriteAgentSkills -v`
Expected: PASS — the Task 2 tests are the authoritative assertions that
`.claude/skills/`, `.agents/skills/`, `CLAUDE.md`, `AGENTS.md`, `GEMINI.md` are
written and that existing pointer files are preserved.

- [ ] **Step 3: Live verification after deploy (you have gemini + claude)**

After the runner is rebuilt+restarted and a new task worktree is created:
- `ls -a <worktree>` shows `CLAUDE.md`, `AGENTS.md`, `GEMINI.md`, `.claude/skills/harness-cli/SKILL.md`, `.agents/skills/harness-cli/SKILL.md`.
- A **gemini CLI** run in that worktree picks up `AGENTS.md`/`GEMINI.md`.
- A **claude** run is unchanged (CLAUDE.md + `.claude/`).
- `harness-cli skill harness-cli` prints the skill in any agent.
- (codex `.agents/skills` is best-effort; not verifiable without an account.)

- [ ] **Step 4: Final fixups if any, else done.**

---

## Self-review (spec coverage)

- **§3.1 three pointers, only-if-absent, new content** → Task 1 (content) + Task 2 (writePointerIfAbsent ×3).
- **§3.2 skills to both .claude/skills and .agents/skills** → Task 2 (materializeSkills ×2).
- **§3.3 .claude/settings.json unchanged** → not touched (WriteAgentSettings out of scope; still called as before).
- **§4 existing-file protection** → Task 2 tests (PreservesExistingClaudeMd existing + PreservesExistingAgentsMd new); only-if-absent for all 3.
- **§5 HarnessInjectedPaths** → Task 3.
- **§6 doc note (pointer + SKILL.md)** → Task 1 (pointer) + Task 4 Step 1 (SKILL.md).
- **§7 peer-identity +skills caveat** → Task 4 Step 2.
- **§8 verification (gemini/claude/unit; codex out)** → Task 5.
- **§9 scope-out (hooks, git mechanism, agent_bin branching)** → not implemented (by omission; union injection, no branching).
- **No placeholders:** every code step is concrete; Task 5 Step 2's manual check defers to the Task 2 unit tests (the authoritative assertions), not a TODO.
- **Type/name consistency:** `materializeSkills`, `writePointerIfAbsent`, `claudeMdMinimal`, `HarnessInjectedPaths` used consistently across tasks.
