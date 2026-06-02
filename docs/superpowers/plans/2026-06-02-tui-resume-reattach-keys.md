# TUI one-key reattach / resume — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `r`/`R` keys to the TUI tasks panel — `r` reattaches a Detached session or resumes a finished task with `--continue`; `R` resumes fresh — and strip the now-redundant reattach branch from `i`.

**Architecture:** A pure helper `resumeReattachAction(task, withContinue)` decides the intent (reattach / resume+args / none); the `app.go` key handler wires `r`/`R` to it and dispatches the existing `DoAttachSession` / `DoOpenDetachableSession` commands. `i` is simplified to always open a new interactive. No protocol/server change.

**Tech Stack:** Go; bubbletea TUI.

**Spec:** `docs/superpowers/specs/2026-06-02-tui-resume-reattach-keys-design.md`

---

## Execution context (all tasks)

- **Work in the worktree**: edit/build/commit under
  `/home/kforfk/workspace/remote-agent-harness/.harness-worktrees/0f0d4dd6b7d3b64354cf4ff249b87403/`.
  The parent-repo absolute path routes to the parent checkout (Pitfall 8).
- **Push to origin/main**: commit locally, then cherry-pick onto `origin/main` in an isolated `git worktree add --detach /tmp/wt-push-main origin/main`, verify `git merge-base --is-ancestor origin/main $TIP`, `git push origin $TIP:main`, `git worktree remove`. Never force-push.
- **Deploy note**: the TUI binary (`bin/harness-tui`) is what the human runs; this reaches the user after a `make build`. No runner/server restart needed for the TUI itself.

---

## File structure

| File | Responsibility | Change |
|------|----------------|--------|
| `tui/taskaction.go` | **new** | pure `resumeReattachAction` + `taskAction`/`taskActionKind` types |
| `tui/taskaction_test.go` | **new** | table test for the helper |
| `tui/app.go` | key handler + hint | simplify `i` (drop reattach), add `r`/`R` handler, update hint line |

---

## Task 1: `resumeReattachAction` pure helper + tests

**Files:**
- Create: `tui/taskaction.go`
- Create: `tui/taskaction_test.go`

- [ ] **Step 1: Write the failing test** — create `tui/taskaction_test.go`:

```go
package tui

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestResumeReattachAction(t *testing.T) {
	detached := &protocol.TaskInfo{Status: protocol.TaskStatus_Detached}
	detached.SetDetachable(true)
	running := &protocol.TaskInfo{Status: protocol.TaskStatus_Running}

	if got := resumeReattachAction(nil, true); got.Kind != actionNone {
		t.Errorf("nil: want actionNone, got %v", got.Kind)
	}
	for _, wc := range []bool{true, false} {
		if got := resumeReattachAction(detached, wc); got.Kind != actionReattach {
			t.Errorf("detached wc=%v: want actionReattach, got %v", wc, got.Kind)
		}
	}
	for _, st := range []protocol.TaskStatus{
		protocol.TaskStatus_Succeeded, protocol.TaskStatus_Failed, protocol.TaskStatus_Cancelled,
	} {
		task := &protocol.TaskInfo{Status: st}
		if got := resumeReattachAction(task, true); got.Kind != actionResume ||
			len(got.ResumeArgs) != 1 || got.ResumeArgs[0] != "--continue" {
			t.Errorf("status=%v r: want resume [--continue], got %v %v", st, got.Kind, got.ResumeArgs)
		}
		if got := resumeReattachAction(task, false); got.Kind != actionResume || got.ResumeArgs != nil {
			t.Errorf("status=%v R: want resume nil, got %v %v", st, got.Kind, got.ResumeArgs)
		}
	}
	if got := resumeReattachAction(running, true); got.Kind != actionNone {
		t.Errorf("running: want actionNone, got %v", got.Kind)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./tui/ -run TestResumeReattachAction`
Expected: FAIL — `undefined: resumeReattachAction` / `actionNone` etc.

- [ ] **Step 3: Create `tui/taskaction.go`**

```go
package tui

import "github.com/on-keyday/agent-harness/runner/protocol"

// taskActionKind is the intent decided by resumeReattachAction.
type taskActionKind int

const (
	actionNone taskActionKind = iota
	actionReattach
	actionResume
)

// taskAction is what the r/R keys should do for the selected task.
type taskAction struct {
	Kind       taskActionKind
	ResumeArgs []string // claude args for actionResume (["--continue"] or nil)
	Hint       string   // shown for actionNone
}

// resumeReattachAction decides what r (withContinue=true) / R (withContinue=false)
// do for the selected task: reattach a live Detached+Detachable session, resume a
// finished task into a new detachable session (with or without --continue), or
// nothing (with a hint) for anything else.
func resumeReattachAction(t *protocol.TaskInfo, withContinue bool) taskAction {
	if t == nil {
		return taskAction{Kind: actionNone, Hint: "no task selected"}
	}
	if t.Status == protocol.TaskStatus_Detached && t.Detachable() {
		return taskAction{Kind: actionReattach}
	}
	switch t.Status {
	case protocol.TaskStatus_Succeeded, protocol.TaskStatus_Failed, protocol.TaskStatus_Cancelled:
		var args []string
		if withContinue {
			args = []string{"--continue"}
		}
		return taskAction{Kind: actionResume, ResumeArgs: args}
	}
	return taskAction{Kind: actionNone,
		Hint: "r/R: pick a detached session (reattach) or a finished task (resume)"}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./tui/ -run TestResumeReattachAction`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tui/taskaction.go tui/taskaction_test.go
git commit -m "feat(tui): resumeReattachAction helper deciding r/R intent"
```

---

## Task 2: Simplify `i` (drop the reattach branch)

**Files:**
- Modify: `tui/app.go` (the `i` handler, lines 458-466)

- [ ] **Step 1: Replace the `i` handler**

Find:
```go
		if a.focus != focusCmdline && !logsEditing && msg.String() == "i" {
			if a.focus == focusTasks {
				if t := a.tasks.SelectedTask(); t != nil &&
					t.Status == protocol.TaskStatus_Detached && t.Detachable() {
					return a, DoAttachSession(a.client, a.tasks.SelectedID())
				}
			}
			return a, DoOpenInteractive(a.client, a.defaultRepo)
		}
```

Replace with:
```go
		// `i` opens a new (non-detachable) interactive PTY in the default repo.
		// Reattach lives on `r` now (see below), so `i` no longer special-cases a
		// selected Detached task.
		if a.focus != focusCmdline && !logsEditing && msg.String() == "i" {
			return a, DoOpenInteractive(a.client, a.defaultRepo)
		}
```

- [ ] **Step 2: Build (no behavior test here; covered by Task 4 manual)**

Run: `go build ./tui/`
Expected: success. (If `protocol` becomes unused in app.go, the build will say so — but `protocol` is still used elsewhere in app.go, e.g. the `c`/detail paths, so it stays imported.)

- [ ] **Step 3: Commit**

```bash
git add tui/app.go
git commit -m "feat(tui): i always opens a new interactive (reattach moved to r)"
```

---

## Task 3: Wire `r`/`R` keys + update the hint line

**Files:**
- Modify: `tui/app.go` (insert handler after the `c` block ~line 516; hint line 645)

- [ ] **Step 1: Add the `r`/`R` handler** after the `c` handler block (the `if a.focus == focusTasks && msg.String() == "c" { ... }` ending around line 516), before the cmdline-submit block:

```go
		// `r` / `R` re-enter the selected session: reattach a live Detached
		// session, or resume a finished task into a new detachable session.
		// r resumes with --continue (keep claude's memory); R resumes fresh.
		if a.focus == focusTasks && (msg.String() == "r" || msg.String() == "R") {
			act := resumeReattachAction(a.tasks.SelectedTask(), msg.String() == "r")
			switch act.Kind {
			case actionReattach:
				return a, DoAttachSession(a.client, a.tasks.SelectedID())
			case actionResume:
				// repo is irrelevant on resume — the server reuses the task's
				// RepoPath and worktree branch.
				return a, DoOpenDetachableSession(a.client, "", cli.SelectorOpts{}, act.ResumeArgs, a.tasks.SelectedID())
			case actionNone:
				a.cmdresult.Append(WarnStyle.Render(act.Hint))
				return a, nil
			}
		}
```

- [ ] **Step 2: Update the hint line** (`tui/app.go:645`)

Find:
```go
		hint = "tab focus · ←/→ scroll · / filter · s submit · S session · i interactive · F file picker · d detail · c cancel · q quit"
```

Replace with:
```go
		hint = "tab focus · ←/→ scroll · / filter · s submit · S session · i interactive · r reattach/resume · R resume-fresh · F file picker · d detail · c cancel · q quit"
```

- [ ] **Step 3: Build**

Run: `go build ./...`
Expected: success.

- [ ] **Step 4: Commit**

```bash
git add tui/app.go
git commit -m "feat(tui): r reattach/resume(+continue), R resume-fresh on selected task"
```

---

## Task 4: Integration build + verification

- [ ] **Step 1: Full build + tui tests**

Run: `go build ./... && go test ./tui/`
Expected: all pass (incl. `TestResumeReattachAction` and the existing tui tests).

- [ ] **Step 2: Manual verification (after `make build`, in the TUI)**

- Select a **finished** task (Succeeded/Failed/Cancelled), press `r` → a new detachable session resumes that task's worktree **with `--continue`** (claude continues its prior conversation). Press `R` on it → resumes **without `--continue`** (fresh claude).
- Select a **Detached** detachable session, press `r` (or `R`) → **reattach**.
- Press `r` on a Running/Queued task → a hint line appears, nothing else.
- Press `i` (no selection or any task) → opens a **new** interactive; it no longer reattaches a Detached task (that's `r` now).
- The footer hint shows `r reattach/resume · R resume-fresh`.

- [ ] **Step 3: Final fixups if any, else done.**

---

## Self-review (spec coverage)

- **§3.0 simplify `i` (drop reattach)** → Task 2.
- **§3.1 `r`/`R` semantics (reattach / resume±continue / none+hint)** → Task 1 (helper) + Task 3 (wiring).
- **§4 pure helper `resumeReattachAction`** → Task 1 (exact types/signature: `taskActionKind`, `taskAction{Kind,ResumeArgs,Hint}`, `actionNone/actionReattach/actionResume`).
- **§5 handler wiring (repo="" resume, DoAttachSession/DoOpenDetachableSession)** → Task 3.
- **§6 hint line** → Task 3 Step 2.
- **§7 tests** → Task 1 test (nil / detached / 3 terminal statuses ×continue / running).
- **§8 scope-out (S, non-detachable removal, Running takeover)** → not implemented (by omission).
- **No placeholders:** every code step is concrete; TUI key-handler behavior (hard to unit-test in bubbletea) is covered by the tested pure helper + Task 4 manual steps.
- **Type/name consistency:** `resumeReattachAction`, `taskAction`, `actionNone/Reattach/Resume`, `ResumeArgs`, `DoAttachSession`, `DoOpenDetachableSession`, `cli.SelectorOpts{}` used consistently across tasks.
