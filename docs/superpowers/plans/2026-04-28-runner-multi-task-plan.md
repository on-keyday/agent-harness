# Multi-task per runner & multi-repo allowed roots — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Enable one `agent-runner` process to serve multiple repos (under declared `allowed_roots`) and run up to `--max-tasks N` concurrent tasks, with explicit pin selectors (`--runner`/`--host`/`--ip`), strict-by-default ambiguity rejection, end-to-end functional cancel, per-task panic isolation, and Failed-on-disconnect cleanup of orphaned tasks.

**Architecture:** Wire format breaks in place (per individual-dogfood policy). Schema changes are regenerated via `make protoregen`. Server registry tracks `ActiveTasks` set + `MaxTasks` per runner; `Candidates(repo, sel)` filters by directory-boundary prefix + selector match (capacity-agnostic); dispatcher applies capacity check via atomic `BindTask`. Runner spawns each task as a goroutine with per-task `context.CancelFunc` registered in a tasks map; `defer recover()` per task isolates panics. `WorktreeManager` per repo is cached on the session and serializes `git worktree` operations on the same repo.

**Tech Stack:** Go (server/runner/CLI/TUI), `runner/protocol/*.bgn` regenerated via `scripts/protoregen.sh`, WAL persistence for tasks in `server/wal.go`, standard `testing` package for unit/integration tests.

**Spec reference:** `docs/superpowers/specs/2026-04-28-runner-multi-task-design.md`

---

## File structure

### Create
- `runner/protocol/pathmatch.go` — `IsUnderRoot(root, repo)` directory-boundary predicate; imported by both server and runner so they cannot disagree on prefix semantics.
- `runner/protocol/pathmatch_test.go`

### Modify (server)
- `runner/protocol/message.bgn` — schema changes per spec §"Wire protocol changes". Regenerated to `runner/protocol/message.go` via `make protoregen`.
- `server/registry.go` — `RunnerEntry` field overhaul, `BindTask`, `UnbindTask`, `Candidates`, `OnRemove` signature widening.
- `server/registry_test.go`
- `server/taskstore.go` — `TaskEntry` adds `Selector`/`BoundRunnerID`, `MarkFailed` method, WAL Selector serialization.
- `server/taskstore_test.go`
- `server/wal.go` — `task_queued` event payload includes `Selector`; new `task_failed` event reason field reused for `runner_disconnected`.
- `server/wal_test.go`
- `server/scheduler.go` — replace `OldestIdleForRepo` consumer with `Candidates` + `BindTask`.
- `server/scheduler_test.go`
- `server/dispatch.go` — `tryDispatch` with bind/send/rollback; subscribe to `OnCancel` for forwarding; on-disconnect cleanup for stranded tasks.
- `server/dispatch_test.go`
- `server/task_handler.go` — Submit synchronous error codes; `OpenInteractive` synchronous error codes; `error_msg` payload.
- `server/task_handler_test.go`
- `server/runner_handler.go` — TaskFinished triggers `UnbindTask`.
- `server/runner_handler_test.go`
- `server/server.go` — `OnRemove` callback marks stranded tasks `Failed` before publishing `RunnerOffline`.
- `server/server_test.go`

### Modify (runner)
- `runner/connect.go` — replace single `RepoPath` with `AllowedRoots`/`MaxTasks`/`Hostname`; implement `CancelTask` handler.
- `runner/connect_test.go`
- `runner/session.go` — Session refactor (`tasks` map, `wms` map, `getWorktreeManager`, `repoAllowed`), per-task `context.CancelFunc`, `defer recover()`.
- `runner/session_test.go`
- `runner/worktree.go` — `sync.Mutex` field for per-repo serialization.
- `runner/worktree_test.go`
- `cmd/agent-runner/main.go` — replace `--repo` with `--roots`, add `--max-tasks`.

### Modify (clients)
- `cli/submit.go` — accept selector + repo; surface SubmitResponse error.
- `cli/interactive.go` — accept selector.
- `cli/ls.go` — display refactor (roots / tasks / host columns).
- `cmd/harness-cli/main.go` — `--runner`/`--host`/`--ip` flags with mutual-exclusion validation.
- `tui/...` — runner pane columns, submit dialog selector, interactive Candidates lookup.
- `cmd/harness-webui-wasm/main.go` + `webui/static/...` — runner display, submit selector.

### Add (integration)
- `integration/multi_task_test.go` — new file, all the multi-task / multi-repo / pin / cancel / disconnect cases.

---

## Phase 0: Shared path-match helper

This is a tiny prerequisite so server and runner cannot disagree on prefix semantics (spec §"Prefix-match semantics").

### Task 0.1: `IsUnderRoot` helper

**Files:**
- Create: `runner/protocol/pathmatch.go`
- Test: `runner/protocol/pathmatch_test.go`

- [ ] **Step 1: Write the failing test**

```go
// runner/protocol/pathmatch_test.go
package protocol

import "testing"

func TestIsUnderRoot(t *testing.T) {
	cases := []struct {
		name string
		root string
		repo string
		want bool
	}{
		{"exact match", "/home/kforfk/workspace", "/home/kforfk/workspace", true},
		{"child", "/home/kforfk/workspace", "/home/kforfk/workspace/foo", true},
		{"deep child", "/home/kforfk/workspace", "/home/kforfk/workspace/foo/bar", true},
		{"sibling lookalike", "/home/kforfk/workspace", "/home/kforfk/workspace-evil", false},
		{"unrelated", "/home/kforfk/workspace", "/etc/passwd", false},
		{"trailing slash root", "/home/kforfk/workspace/", "/home/kforfk/workspace/foo", true},
		{"trailing slash repo", "/home/kforfk/workspace", "/home/kforfk/workspace/foo/", true},
		{"relative repo refused", "/home/kforfk/workspace", "workspace/foo", false},
		{"root parent", "/home/kforfk/workspace", "/home/kforfk", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsUnderRoot(tc.root, tc.repo)
			if got != tc.want {
				t.Fatalf("IsUnderRoot(%q,%q)=%v want %v", tc.root, tc.repo, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./runner/protocol/ -run TestIsUnderRoot -v`
Expected: FAIL with `undefined: IsUnderRoot`

- [ ] **Step 3: Write minimal implementation**

```go
// runner/protocol/pathmatch.go
package protocol

import (
	"path/filepath"
	"strings"
)

// IsUnderRoot reports whether repo is the same path as root or is contained
// within it, treating directory boundaries correctly. Both arguments are
// filepath.Clean'd and require absolute paths; callers that pass relative
// paths get false.
//
// This is the single source of truth for the allowed_roots prefix predicate
// shared by server (Registry.Candidates) and runner (Session.repoAllowed).
// Server and runner MUST use this same function so they cannot disagree on
// what "is in allowed_roots" means.
func IsUnderRoot(root, repo string) bool {
	if !filepath.IsAbs(root) || !filepath.IsAbs(repo) {
		return false
	}
	r := filepath.Clean(root)
	p := filepath.Clean(repo)
	rel, err := filepath.Rel(r, p)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(rel)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./runner/protocol/ -run TestIsUnderRoot -v`
Expected: PASS, all 9 cases.

- [ ] **Step 5: Commit**

```bash
git add runner/protocol/pathmatch.go runner/protocol/pathmatch_test.go
git commit -m "runner/protocol: add IsUnderRoot directory-boundary prefix helper"
```

---

## Phase 1: Wire schema lock-in

Single task per the user feedback memory "schema/spec in ONE place — no follow-ups". All wire changes from spec §"Wire protocol changes" land here.

### Task 1.1: Update `message.bgn` and regenerate `message.go`

**Files:**
- Modify: `runner/protocol/message.bgn`
- Modify: `runner/protocol/message.go` (regenerated; do not hand-edit)

- [ ] **Step 1: Apply the schema diff in `message.bgn`**

Replace the existing `RunnerHello` and adjacent formats with the spec §"Wire protocol changes" canonical text. The end state of `message.bgn` should contain the formats below in addition to the existing unchanged ones (`TaskID`, etc.). Replace `repo_path` in `RunnerHello`/`RunnerInfo`/`SubmitResponse` etc. as shown.

Add at the top (after `enum RunnerRequestType`):

```bgn
format AllowedRoot:
    path_len :u16
    path :[path_len]u8
```

Replace `RunnerHello`:

```bgn
format RunnerHello:
    version :u8
    hostname_len :u8
    hostname :[hostname_len]u8
    max_tasks :u16
    allowed_roots_len :u8
    allowed_roots :[allowed_roots_len]AllowedRoot
```

Replace `AssignTask`:

```bgn
format AssignTask:
    task_id :TaskID
    repo_path_len :u16
    repo_path :[repo_path_len]u8
    prompt :[..]u8
```

Replace `OpenExecRunnerRequest`:

```bgn
format OpenExecRunnerRequest:
    task_id :TaskID
    stream_id :u64
    repo_path_len :u16
    repo_path :[repo_path_len]u8
```

Add `ActiveTaskRef`, `Hostname`, `IPAddr`, `RunnerSelectorKind`, `RunnerSelector`, `SubmitStatus` (replacing the existing simple `SubmitResponse`), and update `OpenInteractiveStatus`. Add `--ip` constraint per `RunnerID.IpAddrLen` pattern. Replace existing `RunnerInfo`, `SubmitRequest`, `SubmitResponse`, `OpenInteractiveRequest`, `OpenInteractiveStatus`. Use the exact text from spec §"Wire protocol changes" → "New / changed formats".

- [ ] **Step 2: Regenerate**

Run: `make protoregen ARGS=runner/protocol/message.bgn`
Expected: `==> Done. Regenerated: runner/protocol/message.bgn`. The script writes `runner/protocol/message.go` and `gofmt`s it.

If the regen fails with a feature error (e.g. tagged-union match limitation), fall back to hand-writing the smallest equivalent that compiles, but **never** silently work around a missing brgen feature — file an issue noting which feature was required (per project memory, brgen-author dogfood). Likely concerns this run: nested formats inside `match` (used in `RunnerSelector`) and explicit `..` (no-payload) variants. If either fails, simplify the union to a parallel-fields-with-`kind` discriminator (each variant's payload becomes a fixed-size optional sub-format) and add a code comment pointing to the brgen issue.

- [ ] **Step 3: Compile-check**

Run: `make check`
Expected: build errors throughout server/runner/cli — those packages still reference `e.RepoPath`, `RunnerHello.SetRepoPath`, etc. This is **expected** at this stage; the rest of the plan fixes the call sites. Note the failures and proceed; do not commit yet.

- [ ] **Step 4: Commit just the .bgn + regen**

```bash
git add runner/protocol/message.bgn runner/protocol/message.go
git commit -m "proto: multi-task schema (allowed_roots, hostname, max_tasks, selector)"
```

The repo will be in a non-building state after this commit. The subsequent phases bring it back to green.

---

## Phase 2: Server registry refactor

### Task 2.1: `RunnerEntry` field overhaul

**Files:**
- Modify: `server/registry.go`
- Modify: `server/registry_test.go`

- [ ] **Step 1: Update existing test fixtures (compile-only step)**

In `server/registry_test.go`, every `&RunnerEntry{ ... RepoPath: "/x", Status: protocol.RunnerStatus_Idle, CurrentTask: "..." ... }` literal becomes:

```go
&RunnerEntry{
    ID:           "A",
    Hostname:     "hostA",
    AllowedRoots: []string{"/x"},
    MaxTasks:     1,
    ActiveTasks:  map[string]struct{}{},
    ConnectedAt:  now,
    LastSeen:     now,
}
```

Don't worry about `Status` field anymore (it's a method now). Update each `RunnerEntry` literal in `registry_test.go` and any other `_test.go` that constructs one. Keep test names and assertions for `Add`/`Remove`/`Get`/`List` semantics — those still apply.

- [ ] **Step 2: Edit `RunnerEntry` and re-add helper methods**

```go
// server/registry.go (replace the existing RunnerEntry type and add Status method)
type RunnerEntry struct {
	ID           string
	Hostname     string
	AllowedRoots []string
	MaxTasks     int
	ActiveTasks  map[string]struct{}
	ConnectedAt  time.Time
	LastSeen     time.Time
	Conn         ConnHandle
}

// Status derives the wire-visible status from connection + slot occupancy.
// Offline = no Conn; Busy = at capacity; Idle = capacity remains.
func (e *RunnerEntry) Status() protocol.RunnerStatus {
	if e.Conn == nil {
		return protocol.RunnerStatus_Offline
	}
	if len(e.ActiveTasks) >= e.MaxTasks {
		return protocol.RunnerStatus_Busy
	}
	return protocol.RunnerStatus_Idle
}
```

- [ ] **Step 3: Run package compile + unchanged tests**

Run: `go test ./server/ -run "TestRegistryAddFindRemove|TestRegistryList" -v`
Expected: PASS. Add/Remove/Get/List still work with the new field set.

- [ ] **Step 4: Commit**

```bash
git add server/registry.go server/registry_test.go
git commit -m "server/registry: RunnerEntry fields for multi-task + multi-repo"
```

### Task 2.2: `BindTask` / `UnbindTask` atomic capacity ops

**Files:**
- Modify: `server/registry.go`
- Modify: `server/registry_test.go`

- [ ] **Step 1: Write the failing tests**

```go
// server/registry_test.go (append)
func TestRegistryBindTaskAtCapacity(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	r.Add(&RunnerEntry{
		ID: "A", Hostname: "h", AllowedRoots: []string{"/x"}, MaxTasks: 1,
		ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now,
	})
	if !r.BindTask("A", "t1") {
		t.Fatal("expected first BindTask to succeed")
	}
	if r.BindTask("A", "t2") {
		t.Fatal("expected second BindTask to fail at capacity")
	}
	r.UnbindTask("A", "t1")
	if !r.BindTask("A", "t2") {
		t.Fatal("expected BindTask to succeed after UnbindTask")
	}
}

func TestRegistryUnbindTaskIdempotent(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	r.Add(&RunnerEntry{
		ID: "A", Hostname: "h", AllowedRoots: []string{"/x"}, MaxTasks: 2,
		ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now,
	})
	r.UnbindTask("A", "absent") // double-release safe
	r.BindTask("A", "t1")
	r.UnbindTask("A", "t1")
	r.UnbindTask("A", "t1") // idempotent on already-unbound
}

func TestRegistryBindTaskRace(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	r.Add(&RunnerEntry{
		ID: "A", Hostname: "h", AllowedRoots: []string{"/x"}, MaxTasks: 4,
		ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now,
	})
	const N = 64
	results := make(chan bool, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results <- r.BindTask("A", fmt.Sprintf("t%d", i))
		}(i)
	}
	wg.Wait()
	close(results)
	successes := 0
	for ok := range results {
		if ok {
			successes++
		}
	}
	if successes != 4 {
		t.Fatalf("expected exactly 4 successful binds (MaxTasks), got %d", successes)
	}
}
```

The race test requires `import "fmt"` and `"sync"` — add them if not already present.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./server/ -run "TestRegistryBindTask|TestRegistryUnbindTask" -v -race`
Expected: FAIL with `r.BindTask undefined`.

- [ ] **Step 3: Implement BindTask / UnbindTask**

```go
// server/registry.go (append, after SetLastSeen)

// BindTask atomically reserves a task slot on the runner. Returns false if
// the runner is unknown or already at capacity. Caller (dispatcher) must
// call UnbindTask on send failure to roll back the reservation.
func (r *Registry) BindTask(id, taskID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.runners[id]
	if !ok {
		return false
	}
	if len(e.ActiveTasks) >= e.MaxTasks {
		return false
	}
	if e.ActiveTasks == nil {
		e.ActiveTasks = make(map[string]struct{})
	}
	e.ActiveTasks[taskID] = struct{}{}
	e.LastSeen = time.Now()
	return true
}

// UnbindTask releases a previously-reserved slot. Idempotent: no error if the
// runner is unknown or did not hold the task. This makes it safe to call
// from both the dispatcher's rollback path and the runner_handler's
// TaskFinished path even if they race.
func (r *Registry) UnbindTask(id, taskID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.runners[id]
	if !ok {
		return
	}
	delete(e.ActiveTasks, taskID)
	e.LastSeen = time.Now()
}
```

Also delete the now-superseded `SetStatus` and `SetIdleIfBoundTo` methods. Their callers will be migrated in later phases — at this point the package will not compile cleanly until those updates land. That is expected.

- [ ] **Step 4: Run race-aware tests to verify pass**

Run: `go test ./server/ -run "TestRegistryBindTask|TestRegistryUnbindTask" -v -race`
Expected: PASS, including 4-slot race test exiting clean under `-race`.

- [ ] **Step 5: Commit**

```bash
git add server/registry.go server/registry_test.go
git commit -m "server/registry: BindTask/UnbindTask with atomic capacity check"
```

### Task 2.3: `Candidates` matcher

**Files:**
- Modify: `server/registry.go`
- Modify: `server/registry_test.go`

- [ ] **Step 1: Write the failing tests**

```go
// server/registry_test.go (append)
import (
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestRegistryCandidatesPrefixMatch(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	r.Add(&RunnerEntry{ID: "A", Hostname: "gmkhost", AllowedRoots: []string{"/home/kforfk/workspace"}, MaxTasks: 1, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now})
	r.Add(&RunnerEntry{ID: "B", Hostname: "raspi",   AllowedRoots: []string{"/home/pi/workspace"},     MaxTasks: 1, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now})

	cs := r.Candidates("/home/kforfk/workspace/repo1", protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_None})
	if len(cs) != 1 || cs[0].ID != "A" {
		t.Fatalf("expected only A, got %v", cs)
	}
	cs = r.Candidates("/home/pi/workspace/foo", protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_None})
	if len(cs) != 1 || cs[0].ID != "B" {
		t.Fatalf("expected only B, got %v", cs)
	}
	cs = r.Candidates("/etc/passwd", protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_None})
	if len(cs) != 0 {
		t.Fatalf("expected no candidates, got %v", cs)
	}
}

func TestRegistryCandidatesAmbiguous(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	r.Add(&RunnerEntry{ID: "A", Hostname: "h1", AllowedRoots: []string{"/shared"}, MaxTasks: 1, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now})
	r.Add(&RunnerEntry{ID: "B", Hostname: "h2", AllowedRoots: []string{"/shared"}, MaxTasks: 1, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now})
	cs := r.Candidates("/shared/foo", protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_None})
	if len(cs) != 2 {
		t.Fatalf("expected 2 candidates (ambiguous), got %d", len(cs))
	}
}

func TestRegistryCandidatesCapacityAgnostic(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	r.Add(&RunnerEntry{ID: "A", Hostname: "h1", AllowedRoots: []string{"/x"}, MaxTasks: 1,
		ActiveTasks: map[string]struct{}{"existing": {}}, ConnectedAt: now, LastSeen: now})
	cs := r.Candidates("/x/repo", protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_None})
	if len(cs) != 1 {
		t.Fatalf("Candidates must include at-capacity runners (caller filters), got %d", len(cs))
	}
}

func TestRegistryCandidatesSelectorByHostname(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	r.Add(&RunnerEntry{ID: "A", Hostname: "gmkhost", AllowedRoots: []string{"/x"}, MaxTasks: 1, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now})
	r.Add(&RunnerEntry{ID: "B", Hostname: "raspi",   AllowedRoots: []string{"/x"}, MaxTasks: 1, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now})

	sel := protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_ByHostname}
	sel.SetHostname(protocol.Hostname{}) // helper; populate hostname bytes via SetHostname/equivalent setter
	// (use whatever setter the regenerated message.go exposes for Hostname.SetHostname)
	cs := r.Candidates("/x/repo", sel)
	// pseudo-assertion: after wiring the actual setter, cs must have len(1) and cs[0].ID == "A"
	_ = cs
}
```

The `RunnerSelector` field setters depend on the regenerated `message.go`. Use whatever setter pattern the regen produces (typically `sel.SetByHostname(protocol.Hostname{...})` or a sub-struct field assignment); adapt the test code to match the regenerated API.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./server/ -run TestRegistryCandidates -v`
Expected: FAIL with `r.Candidates undefined`.

- [ ] **Step 3: Implement Candidates**

```go
// server/registry.go (append)

// Candidates returns runner snapshots whose allowed_roots contain repo
// (directory-boundary-aware via protocol.IsUnderRoot) and which match the
// selector (or any runner if Kind == None). The returned slice is
// capacity-agnostic: at-capacity runners are still listed so callers can
// detect ambiguity even when matching runners are all busy.
//
// The result is sorted by ConnectedAt asc then ID asc for deterministic
// behavior in tests and dispatch.
func (r *Registry) Candidates(repo string, sel protocol.RunnerSelector) []RunnerEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var out []RunnerEntry
	for _, e := range r.runners {
		if !rootsContain(e.AllowedRoots, repo) {
			continue
		}
		if !selectorMatches(sel, e) {
			continue
		}
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ConnectedAt.Equal(out[j].ConnectedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].ConnectedAt.Before(out[j].ConnectedAt)
	})
	return out
}

func rootsContain(roots []string, repo string) bool {
	for _, root := range roots {
		if protocol.IsUnderRoot(root, repo) {
			return true
		}
	}
	return false
}

func selectorMatches(sel protocol.RunnerSelector, e *RunnerEntry) bool {
	switch sel.Kind {
	case protocol.RunnerSelectorKind_None:
		return true
	case protocol.RunnerSelectorKind_ByRunnerId:
		// Compare ConnectionID strings — e.ID is already RunnerID.String().
		want := sel.RunnerId() // accessor name from regen; adapt if different
		return want != nil && want.String() == e.ID
	case protocol.RunnerSelectorKind_ByHostname:
		h := sel.Hostname() // accessor for the Hostname sub-format
		return h != nil && string(h.Hostname) == e.Hostname
	case protocol.RunnerSelectorKind_ByIp:
		ip := sel.Ip() // accessor for IPAddr sub-format
		return ip != nil && runnerIDIPMatches(e.ID, ip.IpAddr)
	}
	return false
}

// runnerIDIPMatches extracts the IP bytes from a ConnectionID-encoded ID
// string and compares to want. Implementation depends on objproto's
// ConnectionID format — use the existing parser:
func runnerIDIPMatches(id string, want []byte) bool {
	cid, err := objproto.ParseConnectionID(id, 0)
	if err != nil {
		return false
	}
	return bytes.Equal(cid.IPAddr(), want)
}
```

`OldestIdleForRepo` is now obsolete; mark it with a deprecation comment for removal in a later task once schedulers/dispatchers stop calling it. Don't delete yet — it lets Phase 5 land before we touch all call sites.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./server/ -run TestRegistryCandidates -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add server/registry.go server/registry_test.go
git commit -m "server/registry: Candidates with prefix/selector filter, capacity-agnostic"
```

### Task 2.4: Widen `OnRemove` signature to pass snapshot

**Files:**
- Modify: `server/registry.go`
- Modify: `server/registry_test.go`
- Modify: `server/server.go` (single call site update)

- [ ] **Step 1: Write the failing test**

```go
// server/registry_test.go (append)
func TestRegistryOnRemovePassesSnapshot(t *testing.T) {
	r := NewRegistry()
	var got RunnerEntry
	r.OnRemove = func(id string, snap RunnerEntry) {
		got = snap
	}
	now := time.Now()
	r.Add(&RunnerEntry{
		ID: "A", Hostname: "h", AllowedRoots: []string{"/x"}, MaxTasks: 2,
		ActiveTasks: map[string]struct{}{"t1": {}, "t2": {}}, ConnectedAt: now, LastSeen: now,
	})
	r.Remove("A")
	if got.ID != "A" || len(got.ActiveTasks) != 2 {
		t.Fatalf("snapshot lost ActiveTasks: %+v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Expected: FAIL — current `OnRemove` is `func(id string)`; assignment will not compile.

- [ ] **Step 3: Widen the signature**

In `server/registry.go`, change:

```go
OnRemove func(id string)
```

to:

```go
OnRemove func(id string, snapshot RunnerEntry)
```

And update `Remove` to capture and pass the snapshot:

```go
func (r *Registry) Remove(id string) {
	r.mu.Lock()
	e, existed := r.runners[id]
	var snap RunnerEntry
	if existed {
		snap = *e
	}
	delete(r.runners, id)
	onRemove := r.OnRemove
	r.mu.Unlock()
	if existed && onRemove != nil {
		onRemove(id, snap)
	}
}
```

Update the lone caller in `server/server.go` (`s.registry.OnRemove = func(id string) { ... }`) to:

```go
s.registry.OnRemove = func(id string, snapshot RunnerEntry) {
    publishRunnerEvent(id, protocol.StatusEventKind_RunnerOffline, protocol.RunnerStatus_Offline)
}
```

(The MarkFailed wiring is added in Phase 5.)

- [ ] **Step 4: Run tests**

Run: `go test ./server/ -run TestRegistryOnRemove -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add server/registry.go server/registry_test.go server/server.go
git commit -m "server/registry: widen OnRemove to (id, snapshot) for stranded-task cleanup"
```

---

## Phase 3: Server taskstore — Selector + BoundRunnerID + MarkFailed + WAL

### Task 3.1: `TaskEntry` adds `Selector` and `BoundRunnerID`; WAL records both

**Files:**
- Modify: `server/taskstore.go`
- Modify: `server/wal.go`
- Modify: `server/taskstore_test.go`
- Modify: `server/wal_test.go`

- [ ] **Step 1: Write the failing test (taskstore round-trip)**

```go
// server/taskstore_test.go (append)
func TestTaskStoreAddCarriesSelectorAndBoundRunner(t *testing.T) {
	ts := NewTaskStore(nil)
	sel := protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_ByHostname}
	sel.SetHostname(mustHostname(t, "gmkhost"))
	taskID := ts.Add(SubmitInput{
		RepoPath: "/x/repo",
		Prompt:   []byte("hello"),
	}, "runner-A", sel)
	got, ok := ts.Get(taskID)
	if !ok {
		t.Fatal("Get failed")
	}
	if got.BoundRunnerID != "runner-A" {
		t.Fatalf("BoundRunnerID=%q want runner-A", got.BoundRunnerID)
	}
	if got.Selector.Kind != protocol.RunnerSelectorKind_ByHostname {
		t.Fatalf("Selector.Kind=%v want ByHostname", got.Selector.Kind)
	}
}

// helper
func mustHostname(t *testing.T, s string) protocol.Hostname {
	t.Helper()
	var h protocol.Hostname
	h.SetHostname([]byte(s))
	return h
}
```

`SubmitInput` is a new small wrapper — declare it in `taskstore.go` if there isn't already one (most projects have a `SubmitRequest`-like Go struct that decouples wire from store). Adapt to whatever signature `Add` already has — the goal is ONLY to ensure the `BoundRunnerID` and `Selector` fields are stored.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./server/ -run TestTaskStoreAddCarriesSelectorAndBoundRunner -v`
Expected: FAIL — `Add` does not accept these new arguments.

- [ ] **Step 3: Edit `TaskEntry` and `Add` signature**

In `server/taskstore.go`, add to `TaskEntry`:

```go
type TaskEntry struct {
	// ... existing fields ...
	Selector      protocol.RunnerSelector
	BoundRunnerID string
}
```

Change the `Add` method signature to take `boundRunnerID string, selector protocol.RunnerSelector` as the last two arguments and assign into the new fields. Update every existing caller in `server/task_handler.go` (and any test) to pass empty defaults for now; later phases populate them with real values.

- [ ] **Step 4: WAL serialization — write the failing test**

```go
// server/wal_test.go (append)
func TestWALReplayRestoresSelectorAndBoundRunner(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "events.log")
	wal, err := OpenWAL(walPath)
	if err != nil {
		t.Fatalf("OpenWAL: %v", err)
	}
	sel := protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_ByHostname}
	sel.SetHostname(mustHostname(t, "gmkhost"))
	if err := wal.Write(WALEvent{
		Type:          "task_queued",
		TaskID:        "abc",
		Ts:            time.Now().UnixNano(),
		RepoPath:      "/x/repo",
		BoundRunnerID: "runner-A",
		Selector:      sel,
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	wal.Close()

	events, err := ReadWAL(walPath)
	if err != nil {
		t.Fatalf("ReadWAL: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	e := events[0]
	if e.BoundRunnerID != "runner-A" || e.Selector.Kind != protocol.RunnerSelectorKind_ByHostname {
		t.Fatalf("WAL replay lost fields: %+v", e)
	}
}
```

- [ ] **Step 5: Run WAL test to verify fail**

Run: `go test ./server/ -run TestWALReplay -v`
Expected: FAIL — `WALEvent` lacks the new fields.

- [ ] **Step 6: Add WAL fields**

In `server/wal.go`, append fields to `WALEvent`:

```go
type WALEvent struct {
	// ... existing fields ...
	BoundRunnerID string                  `json:"bound_runner_id,omitempty"`
	Selector      protocol.RunnerSelector `json:"selector,omitempty"`
}
```

JSON serialization: `RunnerSelector` is a wire-format generated struct, so MarshalJSON / UnmarshalJSON probably won't produce nice output. Implement custom `MarshalJSON` / `UnmarshalJSON` on `WALEvent` that wraps the selector as `{kind, runner_id?, hostname?, ip?}` for legibility AND replay correctness. The selector is small; aim for the JSON shape:

```json
{"kind":"by_hostname","hostname":"gmkhost"}
```

If JSON marshalling becomes unwieldy, fall back to encoding the selector as base64'd wire bytes — same data, less readable, but mechanical. Either works; pick whichever is cheaper to implement and document the choice in the source comment.

- [ ] **Step 7: Run all wal/taskstore tests**

Run: `go test ./server/ -run "TestWAL|TestTaskStore" -v`
Expected: PASS, including the new round-trip test.

- [ ] **Step 8: Commit**

```bash
git add server/taskstore.go server/wal.go server/taskstore_test.go server/wal_test.go server/task_handler.go
git commit -m "server/taskstore: persist Selector and BoundRunnerID in TaskEntry + WAL"
```

### Task 3.2: `MarkFailed` method on TaskStore (used for runner_disconnected)

**Files:**
- Modify: `server/taskstore.go`
- Modify: `server/taskstore_test.go`

- [ ] **Step 1: Write the failing test**

```go
// server/taskstore_test.go (append)
func TestTaskStoreMarkFailedTransitions(t *testing.T) {
	ts := NewTaskStore(nil)
	id := ts.Add(SubmitInput{RepoPath: "/x", Prompt: []byte("p")}, "", protocol.RunnerSelector{})
	ts.MarkRunning(id, "/x/.harness-worktrees/abc") // mark Running first
	ts.MarkFailed(id, "runner_disconnected")
	got, _ := ts.Get(id)
	if got.Status != protocol.TaskStatus_Failed {
		t.Fatalf("status=%v want Failed", got.Status)
	}
	if string(got.DiffInfo) != "runner_disconnected" {
		t.Fatalf("diff=%q want runner_disconnected", got.DiffInfo)
	}
}

func TestTaskStoreMarkFailedIdempotentOnTerminal(t *testing.T) {
	ts := NewTaskStore(nil)
	id := ts.Add(SubmitInput{RepoPath: "/x", Prompt: []byte("p")}, "", protocol.RunnerSelector{})
	ts.MarkSucceeded(id, 0, nil) // already terminal
	ts.MarkFailed(id, "runner_disconnected")
	got, _ := ts.Get(id)
	if got.Status != protocol.TaskStatus_Succeeded {
		t.Fatalf("MarkFailed should be no-op on terminal state, got %v", got.Status)
	}
}
```

`MarkRunning` / `MarkSucceeded` may be named differently in the codebase — adapt the test to whatever existing methods produce those states.

- [ ] **Step 2: Run test to verify fail**

Run: `go test ./server/ -run TestTaskStoreMarkFailed -v`
Expected: FAIL — method missing.

- [ ] **Step 3: Implement**

```go
// server/taskstore.go (append, near Cancel)
func (s *TaskStore) MarkFailed(id, reason string) {
	now := time.Now()
	s.mu.Lock()
	e, ok := s.tasks[id]
	if !ok {
		s.mu.Unlock()
		return
	}
	switch e.Status {
	case protocol.TaskStatus_Succeeded, protocol.TaskStatus_Failed, protocol.TaskStatus_Cancelled:
		s.mu.Unlock()
		return
	}
	e.Status = protocol.TaskStatus_Failed
	e.EndedAt = &now
	e.ExitCode = -1
	e.DiffInfo = []byte(reason)
	if s.wal != nil {
		if err := s.wal.Write(WALEvent{Type: "task_failed", TaskID: id, Ts: now.UnixNano(), Reason: reason}); err != nil {
			slog.Error("WAL write failed", "op", "task_failed", "task_id", id, "err", err)
		}
	}
	onChange := s.OnChange // any existing callback hook
	s.mu.Unlock()
	if onChange != nil {
		onChange(id)
	}
}
```

Add a `Reason` field to `WALEvent` if not present (used for `task_failed` events).

- [ ] **Step 4: Run tests**

Run: `go test ./server/ -run TestTaskStoreMarkFailed -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add server/taskstore.go server/taskstore_test.go server/wal.go
git commit -m "server/taskstore: MarkFailed with idempotent terminal-state guard"
```

---

## Phase 4: Server submit handler — synchronous error codes

### Task 4.1: Submit synchronous error codes

**Files:**
- Modify: `server/task_handler.go`
- Modify: `server/task_handler_test.go`

- [ ] **Step 1: Write the failing test**

```go
// server/task_handler_test.go (append)
func TestHandleSubmitNoRunnerForRepo(t *testing.T) {
	srv := newTestServer(t) // fixture: empty registry
	resp := srv.handleSubmit(&protocol.SubmitRequest{
		RepoPath: []byte("/x/repo"),
		Selector: protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_None},
		Prompt:   []byte("p"),
	})
	if resp.Status != protocol.SubmitStatus_NoRunnerForRepo {
		t.Fatalf("status=%v want NoRunnerForRepo", resp.Status)
	}
}

func TestHandleSubmitAmbiguousRunner(t *testing.T) {
	srv := newTestServer(t)
	now := time.Now()
	srv.registry.Add(&RunnerEntry{ID: "A", Hostname: "h1", AllowedRoots: []string{"/shared"}, MaxTasks: 1, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now, Conn: stubConn{}})
	srv.registry.Add(&RunnerEntry{ID: "B", Hostname: "h2", AllowedRoots: []string{"/shared"}, MaxTasks: 1, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now, Conn: stubConn{}})

	resp := srv.handleSubmit(&protocol.SubmitRequest{
		RepoPath: []byte("/shared/repo"),
		Selector: protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_None},
		Prompt:   []byte("p"),
	})
	if resp.Status != protocol.SubmitStatus_AmbiguousRunner {
		t.Fatalf("status=%v want AmbiguousRunner", resp.Status)
	}
	if !bytes.Contains(resp.ErrorMsg, []byte("h1")) || !bytes.Contains(resp.ErrorMsg, []byte("h2")) {
		t.Fatalf("error_msg lacks hostnames: %q", resp.ErrorMsg)
	}
}

func TestHandleSubmitPinnedNotFound(t *testing.T) {
	srv := newTestServer(t)
	now := time.Now()
	srv.registry.Add(&RunnerEntry{ID: "A", Hostname: "gmkhost", AllowedRoots: []string{"/x"}, MaxTasks: 1, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now, Conn: stubConn{}})

	sel := protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_ByHostname}
	sel.SetHostname(mustHostname(t, "raspi")) // hostname not present
	resp := srv.handleSubmit(&protocol.SubmitRequest{
		RepoPath: []byte("/x/repo"),
		Selector: sel,
		Prompt:   []byte("p"),
	})
	if resp.Status != protocol.SubmitStatus_PinnedNotFound {
		t.Fatalf("status=%v want PinnedNotFound", resp.Status)
	}
}

func TestHandleSubmitOK(t *testing.T) {
	srv := newTestServer(t)
	now := time.Now()
	srv.registry.Add(&RunnerEntry{ID: "A", Hostname: "h", AllowedRoots: []string{"/x"}, MaxTasks: 1, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now, Conn: stubConn{}})

	resp := srv.handleSubmit(&protocol.SubmitRequest{
		RepoPath: []byte("/x/repo"),
		Selector: protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_None},
		Prompt:   []byte("p"),
	})
	if resp.Status != protocol.SubmitStatus_Ok {
		t.Fatalf("status=%v want Ok", resp.Status)
	}
	if zero := (protocol.TaskID{}); resp.TaskId == zero {
		t.Fatalf("expected task_id populated")
	}
	got, _ := srv.tasks.Get(hex.EncodeToString(resp.TaskId.Id[:]))
	if got.BoundRunnerID != "A" {
		t.Fatalf("BoundRunnerID=%q want A", got.BoundRunnerID)
	}
}
```

`newTestServer` is a helper that returns a `*Server` with empty registry/taskstore wired (use the existing test fixtures pattern from `server/fakes_test.go`). `stubConn` is a `ConnHandle` mock that no-ops `SendMessage`.

- [ ] **Step 2: Run tests to verify fail**

Run: `go test ./server/ -run TestHandleSubmit -v`
Expected: FAIL — current submit handler doesn't return synchronous status codes / doesn't accept the new fields.

- [ ] **Step 3: Implement**

In `server/task_handler.go`, replace the existing submit branch with:

```go
case protocol.TaskControlKind_Submit:
    sr := req.Submit()
    if sr == nil {
        slog.Error("TaskHandler: Submit variant is nil")
        return
    }
    submitResp := h.Server.handleSubmit(sr)
    resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_Submit, RequestId: req.RequestId}
    resp.SetSubmit(submitResp)
    out := resp.MustAppend([]byte{byte(wire.ApplicationPayloadKind_TaskControl)})
    conn.SendMessage(out) //nolint:errcheck
```

And add the `handleSubmit` method to the server:

```go
// server/task_handler.go (or server/server.go — pick whichever already hosts logic; keep handler thin)
func (s *Server) handleSubmit(req *protocol.SubmitRequest) protocol.SubmitResponse {
	repo := filepath.Clean(string(req.RepoPath))
	cands := s.registry.Candidates(repo, req.Selector)
	switch {
	case len(cands) == 0 && req.Selector.Kind != protocol.RunnerSelectorKind_None:
		return protocol.SubmitResponse{Status: protocol.SubmitStatus_PinnedNotFound}
	case len(cands) == 0:
		return protocol.SubmitResponse{Status: protocol.SubmitStatus_NoRunnerForRepo}
	case len(cands) > 1:
		var hostnames []string
		for _, c := range cands {
			hostnames = append(hostnames, c.Hostname)
		}
		msg := []byte("matches: " + strings.Join(hostnames, ", "))
		return protocol.SubmitResponse{
			Status:   protocol.SubmitStatus_AmbiguousRunner,
			ErrorMsg: msg,
		}
	}
	bound := cands[0]
	taskIDHex := s.tasks.Add(SubmitInput{RepoPath: repo, Prompt: req.Prompt}, bound.ID, req.Selector)
	s.dispatcher.Wake()
	var tid protocol.TaskID
	raw, _ := hex.DecodeString(taskIDHex)
	copy(tid.Id[:], raw)
	return protocol.SubmitResponse{Status: protocol.SubmitStatus_Ok, TaskId: tid}
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./server/ -run TestHandleSubmit -v`
Expected: PASS, all 4 cases.

- [ ] **Step 5: Commit**

```bash
git add server/task_handler.go server/task_handler_test.go
git commit -m "server: handleSubmit returns synchronous SubmitStatus error codes"
```

### Task 4.2: OpenInteractive synchronous errors

**Files:**
- Modify: `server/task_handler.go`
- Modify: `server/task_handler_test.go`

Mirror Task 4.1 for the `OpenInteractiveRequest` path. Same five status cases (`ok`, `no_runner_for_repo`, `runner_busy`, `ambiguous_runner`, `pinned_not_found`); the difference is **no Queued state** — capacity-busy short-circuits instead of waiting.

- [ ] **Step 1: Write the failing tests**

```go
// server/task_handler_test.go (append)
func TestHandleOpenInteractiveBusy(t *testing.T) {
	srv := newTestServer(t)
	now := time.Now()
	srv.registry.Add(&RunnerEntry{ID: "A", Hostname: "h", AllowedRoots: []string{"/x"}, MaxTasks: 1,
		ActiveTasks: map[string]struct{}{"existing": {}}, ConnectedAt: now, LastSeen: now, Conn: stubConn{}})
	resp := srv.handleOpenInteractive(&protocol.OpenInteractiveRequest{
		RepoPath: []byte("/x/repo"),
		Selector: protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_None},
	})
	if resp.Status != protocol.OpenInteractiveStatus_RunnerBusy {
		t.Fatalf("status=%v want RunnerBusy", resp.Status)
	}
}

func TestHandleOpenInteractiveNoRunnerForRepo(t *testing.T) {
	srv := newTestServer(t) // empty registry
	resp := srv.handleOpenInteractive(&protocol.OpenInteractiveRequest{
		RepoPath: []byte("/x/repo"),
		Selector: protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_None},
	})
	if resp.Status != protocol.OpenInteractiveStatus_NoRunnerForRepo {
		t.Fatalf("status=%v want NoRunnerForRepo", resp.Status)
	}
}

func TestHandleOpenInteractiveAmbiguous(t *testing.T) {
	srv := newTestServer(t)
	now := time.Now()
	srv.registry.Add(&RunnerEntry{ID: "A", Hostname: "h1", AllowedRoots: []string{"/shared"}, MaxTasks: 1, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now, Conn: stubConn{}})
	srv.registry.Add(&RunnerEntry{ID: "B", Hostname: "h2", AllowedRoots: []string{"/shared"}, MaxTasks: 1, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now, Conn: stubConn{}})
	resp := srv.handleOpenInteractive(&protocol.OpenInteractiveRequest{
		RepoPath: []byte("/shared/repo"),
		Selector: protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_None},
	})
	if resp.Status != protocol.OpenInteractiveStatus_AmbiguousRunner {
		t.Fatalf("status=%v want AmbiguousRunner", resp.Status)
	}
}

func TestHandleOpenInteractivePinnedNotFound(t *testing.T) {
	srv := newTestServer(t)
	now := time.Now()
	srv.registry.Add(&RunnerEntry{ID: "A", Hostname: "gmkhost", AllowedRoots: []string{"/x"}, MaxTasks: 1, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now, Conn: stubConn{}})
	sel := protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_ByHostname}
	sel.SetHostname(mustHostname(t, "raspi"))
	resp := srv.handleOpenInteractive(&protocol.OpenInteractiveRequest{
		RepoPath: []byte("/x/repo"),
		Selector: sel,
	})
	if resp.Status != protocol.OpenInteractiveStatus_PinnedNotFound {
		t.Fatalf("status=%v want PinnedNotFound", resp.Status)
	}
}
```

- [ ] **Step 2: Run tests to verify fail**

Run: `go test ./server/ -run TestHandleOpenInteractive -v`
Expected: FAIL — handler not yet returning new statuses.

- [ ] **Step 3: Implement `handleOpenInteractive`**

In `server/task_handler.go`, replace the `OpenInteractive` case body with a delegation similar to Task 4.1's submit:

```go
case protocol.TaskControlKind_OpenInteractive:
    oir := req.OpenInteractive()
    if oir == nil { return }
    oresp := h.Server.handleOpenInteractive(oir)
    resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_OpenInteractive, RequestId: req.RequestId}
    resp.SetOpenInteractive(oresp)
    out := resp.MustAppend([]byte{byte(wire.ApplicationPayloadKind_TaskControl)})
    conn.SendMessage(out) //nolint:errcheck
```

```go
// server/task_handler.go (or server/server.go, mirror handleSubmit's location)
func (s *Server) handleOpenInteractive(req *protocol.OpenInteractiveRequest) protocol.OpenInteractiveResponse {
	repo := filepath.Clean(string(req.RepoPath))
	cands := s.registry.Candidates(repo, req.Selector)
	switch {
	case len(cands) == 0 && req.Selector.Kind != protocol.RunnerSelectorKind_None:
		return protocol.OpenInteractiveResponse{Status: protocol.OpenInteractiveStatus_PinnedNotFound}
	case len(cands) == 0:
		return protocol.OpenInteractiveResponse{Status: protocol.OpenInteractiveStatus_NoRunnerForRepo}
	case len(cands) > 1:
		return protocol.OpenInteractiveResponse{Status: protocol.OpenInteractiveStatus_AmbiguousRunner}
	}
	bound := cands[0]
	// Capacity gate — interactive cannot queue, fail fast if full.
	if len(bound.ActiveTasks) >= bound.MaxTasks {
		return protocol.OpenInteractiveResponse{Status: protocol.OpenInteractiveStatus_RunnerBusy}
	}
	// Existing splice logic: open bidi stream toward runner, allocate task, BindTask, send OpenExec.
	// (Reuse whatever helper handleOpenInteractive previously called; adapt arg list to take `bound` and `repo`.)
	taskID, streamID, err := s.openInteractiveTask(bound, repo)
	if err != nil {
		return protocol.OpenInteractiveResponse{Status: protocol.OpenInteractiveStatus_InternalError}
	}
	return protocol.OpenInteractiveResponse{Status: protocol.OpenInteractiveStatus_Ok, TaskId: taskID, StreamId: streamID}
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./server/ -run TestHandleOpenInteractive -v`
Expected: PASS, all 4 cases.

- [ ] **Step 5: Commit**

```bash
git add server/task_handler.go server/task_handler_test.go server/server.go
git commit -m "server: handleOpenInteractive returns synchronous status codes"
```

---

## Phase 5: Server dispatcher — capacity, cancel forward, disconnect cleanup

### Task 5.1: `tryDispatch` with bind/send/rollback

**Files:**
- Modify: `server/dispatch.go`
- Modify: `server/dispatch_test.go`

- [ ] **Step 1: Write the failing test**

```go
// server/dispatch_test.go (append)
func TestTryDispatchSuccess(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	conn := &captureConn{}
	r.Add(&RunnerEntry{ID: "A", Hostname: "h", AllowedRoots: []string{"/x"}, MaxTasks: 1,
		ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now, Conn: conn})

	d := NewDispatcher(r, nil /*tasks*/)
	task := &TaskEntry{ID: "t1", RepoPath: "/x/repo", Status: protocol.TaskStatus_Queued, Selector: protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_None}}
	if !d.tryDispatch(task) {
		t.Fatal("tryDispatch should succeed")
	}
	if task.BoundRunnerID != "A" {
		t.Fatalf("BoundRunnerID=%q want A", task.BoundRunnerID)
	}
	entry, _ := r.Get("A")
	if _, ok := entry.ActiveTasks["t1"]; !ok {
		t.Fatal("expected task bound on registry")
	}
	if len(conn.sentAssign) != 1 {
		t.Fatalf("expected 1 AssignTask, got %d", len(conn.sentAssign))
	}
}

func TestTryDispatchSendFailureRollsBack(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	conn := &captureConn{sendErr: errors.New("conn dropped")}
	r.Add(&RunnerEntry{ID: "A", Hostname: "h", AllowedRoots: []string{"/x"}, MaxTasks: 1,
		ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now, Conn: conn})

	d := NewDispatcher(r, nil)
	task := &TaskEntry{ID: "t1", RepoPath: "/x/repo", Status: protocol.TaskStatus_Queued, Selector: protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_None}}
	if d.tryDispatch(task) {
		t.Fatal("tryDispatch should return false on send error")
	}
	if task.BoundRunnerID != "" {
		t.Fatalf("BoundRunnerID=%q want empty after rollback", task.BoundRunnerID)
	}
	entry, _ := r.Get("A")
	if len(entry.ActiveTasks) != 0 {
		t.Fatal("expected slot released on rollback")
	}
}

func TestTryDispatchAmbiguousWaits(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	r.Add(&RunnerEntry{ID: "A", Hostname: "h1", AllowedRoots: []string{"/shared"}, MaxTasks: 1, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now, Conn: &captureConn{}})
	r.Add(&RunnerEntry{ID: "B", Hostname: "h2", AllowedRoots: []string{"/shared"}, MaxTasks: 1, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now, Conn: &captureConn{}})

	d := NewDispatcher(r, nil)
	task := &TaskEntry{ID: "t1", RepoPath: "/shared/repo", Status: protocol.TaskStatus_Queued, Selector: protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_None}}
	if d.tryDispatch(task) {
		t.Fatal("tryDispatch must not pick when ambiguous (re-eval at runtime)")
	}
}
```

`captureConn` is a `ConnHandle` test double:

```go
type captureConn struct {
	sentAssign [][]byte
	sentCancel [][]byte
	sendErr    error
}
func (c *captureConn) SendMessage(b []byte) (uint64, uint64, error) {
	if c.sendErr != nil { return 0, 0, c.sendErr }
	// classify by ApplicationPayloadKind in b[0] if needed
	c.sentAssign = append(c.sentAssign, append([]byte{}, b...))
	return uint64(len(b)), 0, nil
}
// implement other ConnHandle methods as no-ops
```

- [ ] **Step 2: Run tests to verify fail**

Run: `go test ./server/ -run TestTryDispatch -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

In `server/dispatch.go`, replace existing dispatch logic with:

```go
func (d *Dispatcher) tryDispatch(task *TaskEntry) bool {
	cands := d.registry.Candidates(task.RepoPath, task.Selector)
	if len(cands) != 1 {
		return false // ambiguous or empty → wait
	}
	runner := cands[0]
	if !d.registry.BindTask(runner.ID, task.ID) {
		return false // at capacity → wait
	}
	task.BoundRunnerID = runner.ID

	msg := buildAssignTask(task)
	if _, _, err := runner.Conn.SendMessage(msg); err != nil {
		d.registry.UnbindTask(runner.ID, task.ID)
		task.BoundRunnerID = ""
		d.logger.Warn("dispatch send failed; rolled back", "task_id", task.ID, "runner", runner.ID, "err", err)
		return false
	}
	return true
}
```

`buildAssignTask` constructs the `RunnerRequest{kind: AssignTask, ...}` with `task.RepoPath` populated (per spec §"Wire protocol changes" addendum to `AssignTask`).

- [ ] **Step 4: Run tests**

Run: `go test ./server/ -run TestTryDispatch -v -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add server/dispatch.go server/dispatch_test.go
git commit -m "server/dispatch: tryDispatch with capacity bind + send-fail rollback"
```

### Task 5.2: Cancel forwarding via OnCancel

**Files:**
- Modify: `server/dispatch.go`
- Modify: `server/server.go` (wire OnCancel)
- Modify: `server/dispatch_test.go`

- [ ] **Step 1: Write the failing test**

```go
// server/dispatch_test.go (append)
func TestDispatcherOnCancelForwardsToRunner(t *testing.T) {
	r := NewRegistry()
	ts := NewTaskStore(nil)
	now := time.Now()
	conn := &captureConn{}
	r.Add(&RunnerEntry{ID: "A", Hostname: "h", AllowedRoots: []string{"/x"}, MaxTasks: 1,
		ActiveTasks: map[string]struct{}{"t1": {}}, ConnectedAt: now, LastSeen: now, Conn: conn})

	id := ts.Add(SubmitInput{RepoPath: "/x/repo", Prompt: []byte("p")}, "A", protocol.RunnerSelector{})
	d := NewDispatcher(r, ts)
	d.OnCancel(id) // emulates taskstore.OnCancel callback

	if len(conn.sentCancel) != 1 {
		t.Fatalf("expected 1 CancelTask, got %d", len(conn.sentCancel))
	}
}

func TestDispatcherOnCancelSkipsQueuedTask(t *testing.T) {
	r := NewRegistry()
	ts := NewTaskStore(nil)
	id := ts.Add(SubmitInput{RepoPath: "/x/repo", Prompt: []byte("p")}, "" /* not yet bound */, protocol.RunnerSelector{})
	d := NewDispatcher(r, ts)
	d.OnCancel(id) // no-op; nothing to forward
}
```

- [ ] **Step 2-4: Implement**

```go
func (d *Dispatcher) OnCancel(taskID string) {
	task, ok := d.tasks.Get(taskID)
	if !ok || task.BoundRunnerID == "" {
		return
	}
	runner, ok := d.registry.Get(task.BoundRunnerID)
	if !ok {
		return
	}
	msg := buildCancelTask(taskID)
	if _, _, err := runner.Conn.SendMessage(msg); err != nil {
		d.logger.Warn("cancel forward failed", "task_id", taskID, "err", err)
		// Capacity is intentionally NOT released here; the eventual
		// TaskFinished from the runner will release it via the standard path.
	}
}
```

In `server/server.go`, wire it during construction:

```go
s.tasks.OnCancel = func(id string) {
	publishTaskEvent(id, protocol.StatusEventKind_TaskEnded, protocol.TaskStatus_Cancelled, 0)
	s.dispatcher.OnCancel(id) // NEW
}
```

- [ ] **Step 5: Commit**

```bash
git commit -m "server/dispatch: forward Cancel to bound runner via taskstore.OnCancel"
```

### Task 5.3: OnRemove cleanup of stranded tasks

**Files:**
- Modify: `server/server.go`
- Modify: `server/server_test.go`

- [ ] **Step 1: Write the failing test**

```go
// server/server_test.go (append)
func TestOnRemoveMarksStrandedTasksFailed(t *testing.T) {
	srv := newTestServer(t)
	now := time.Now()
	srv.registry.Add(&RunnerEntry{
		ID: "A", Hostname: "h", AllowedRoots: []string{"/x"}, MaxTasks: 4,
		ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastSeen: now, Conn: stubConn{},
	})
	id1 := srv.tasks.Add(SubmitInput{RepoPath: "/x/r1", Prompt: []byte("p")}, "A", protocol.RunnerSelector{})
	id2 := srv.tasks.Add(SubmitInput{RepoPath: "/x/r2", Prompt: []byte("p")}, "A", protocol.RunnerSelector{})
	srv.tasks.MarkRunning(id1, "/x/.harness-worktrees/...")
	srv.tasks.MarkRunning(id2, "/x/.harness-worktrees/...")
	srv.registry.BindTask("A", id1)
	srv.registry.BindTask("A", id2)

	srv.registry.Remove("A") // triggers OnRemove

	for _, id := range []string{id1, id2} {
		got, _ := srv.tasks.Get(id)
		if got.Status != protocol.TaskStatus_Failed {
			t.Fatalf("%s status=%v want Failed", id, got.Status)
		}
		if string(got.DiffInfo) != "runner_disconnected" {
			t.Fatalf("%s diff=%q want runner_disconnected", id, got.DiffInfo)
		}
	}
}
```

- [ ] **Step 2: Run test to verify fail**

Expected: FAIL — OnRemove only publishes the event today.

- [ ] **Step 3: Implement**

In `server/server.go`, expand the `OnRemove` callback:

```go
s.registry.OnRemove = func(id string, snapshot RunnerEntry) {
	for taskID := range snapshot.ActiveTasks {
		s.tasks.MarkFailed(taskID, "runner_disconnected")
	}
	publishRunnerEvent(id, protocol.StatusEventKind_RunnerOffline, protocol.RunnerStatus_Offline)
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./server/ -run TestOnRemoveMarks -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add server/server.go server/server_test.go
git commit -m "server: mark stranded tasks Failed on runner disconnect"
```

---

## Phase 6: Server runner_handler — capacity release on TaskFinished

### Task 6.1: TaskFinished triggers UnbindTask

**Files:**
- Modify: `server/runner_handler.go`
- Modify: `server/runner_handler_test.go`

- [ ] **Step 1: Write the failing test**

```go
// server/runner_handler_test.go (append)
func TestRunnerHandlerTaskFinishedReleasesCapacity(t *testing.T) {
	r := NewRegistry()
	ts := NewTaskStore(nil)
	now := time.Now()
	r.Add(&RunnerEntry{ID: "A", Hostname: "h", AllowedRoots: []string{"/x"}, MaxTasks: 1,
		ActiveTasks: map[string]struct{}{"t1": {}}, ConnectedAt: now, LastSeen: now, Conn: stubConn{}})
	id := ts.Add(SubmitInput{RepoPath: "/x/repo", Prompt: []byte("p")}, "A", protocol.RunnerSelector{})
	ts.MarkRunning(id, "/x/.harness-worktrees/abc")

	h := &RunnerHandler{Registry: r, Tasks: ts}
	finished := protocol.RunnerMessage{Kind: protocol.RunnerMessageType_TaskFinished}
	finished.SetTaskFinished(protocol.TaskFinished{TaskId: mustParseTaskID(t, id), ExitCode: 0})
	h.HandleMessage("A", &finished) // method name adjusts to actual receiver shape

	entry, _ := r.Get("A")
	if _, still := entry.ActiveTasks[id]; still {
		t.Fatal("expected slot released on TaskFinished")
	}
}
```

`mustParseTaskID` is a small helper that hex-decodes back into a `protocol.TaskID`.

- [ ] **Step 2: Run to verify fail**

Expected: FAIL — capacity not released.

- [ ] **Step 3: Implement**

In `server/runner_handler.go`'s TaskFinished branch, after the existing `Tasks.MarkSucceeded/MarkFailed` (and equivalents), add:

```go
h.Registry.UnbindTask(runnerID, taskIDHex)
```

`runnerID` is the connection id of the runner that sent the message — usually already in scope as `conn.ConnectionID().String()`.

- [ ] **Step 4-5: Run tests + commit**

```bash
git add server/runner_handler.go server/runner_handler_test.go
git commit -m "server/runner_handler: release capacity on TaskFinished"
```

---

## Phase 7: Runner-side multi-task

### Task 7.1: `WorktreeManager` mutex per repo

**Files:**
- Modify: `runner/worktree.go`
- Modify: `runner/worktree_test.go`

- [ ] **Step 1: Write the failing test**

```go
// runner/worktree_test.go (append)
func TestWorktreeManagerSerializesSameRepo(t *testing.T) {
	dir := t.TempDir()
	if err := exec.Command("git", "init", dir).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if err := exec.Command("git", "-C", dir, "commit", "--allow-empty", "-m", "init").Run(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	wm := &WorktreeManager{Repo: dir}
	const N = 8
	var wg sync.WaitGroup
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("task%d", i)
			if _, err := wm.Create(id); err != nil {
				errs <- err
				return
			}
			if err := wm.Remove(id); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Create/Remove failed: %v", err)
	}
}
```

(Requires `git` on PATH; the existing test fixtures already use `git` so this is fine.)

- [ ] **Step 2: Run to verify fail (or flake)**

Run: `go test ./runner/ -run TestWorktreeManagerSerializesSameRepo -v -race -count=10`
Expected: FAIL or flake on at least one iteration without the mutex (`git worktree add` racing on the same repo's `index.lock`).

- [ ] **Step 3: Add the mutex**

```go
// runner/worktree.go (modify)
type WorktreeManager struct {
	Repo string
	mu   sync.Mutex
}

func (wm *WorktreeManager) Create(taskID string) (string, error) {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	// ... existing body ...
}

func (wm *WorktreeManager) Remove(taskID string) error {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	// ... existing body ...
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./runner/ -run TestWorktreeManager -v -race -count=10`
Expected: PASS, all 10 iterations.

- [ ] **Step 5: Commit**

```bash
git add runner/worktree.go runner/worktree_test.go
git commit -m "runner/worktree: per-repo Mutex serializes concurrent git worktree ops"
```

### Task 7.2: `Session` refactor — tasks/wms maps + helpers

**Files:**
- Modify: `runner/session.go`
- Modify: `runner/session_test.go`

- [ ] **Step 1: Write the failing tests**

```go
// runner/session_test.go (append)
func TestSessionGetWorktreeManagerCachesPerRepo(t *testing.T) {
	s := &Session{
		AllowedRoots: []string{"/a", "/b"},
		MaxTasks:     2,
	}
	s.initMaps() // no-op constructor helper if added
	wm1 := s.getWorktreeManager("/a/r1")
	wm2 := s.getWorktreeManager("/a/r1")
	wm3 := s.getWorktreeManager("/b/r2")
	if wm1 != wm2 {
		t.Fatal("expected same WM for same repo")
	}
	if wm1 == wm3 {
		t.Fatal("expected different WM for different repos")
	}
}

func TestSessionRepoAllowedDelegatesToProtocol(t *testing.T) {
	s := &Session{AllowedRoots: []string{"/home/kforfk/workspace"}}
	if !s.repoAllowed("/home/kforfk/workspace/foo") {
		t.Fatal("expected /home/kforfk/workspace/foo allowed")
	}
	if s.repoAllowed("/etc/passwd") {
		t.Fatal("/etc/passwd must not be allowed")
	}
	if s.repoAllowed("/home/kforfk/workspace-evil") {
		t.Fatal("sibling lookalike must not be allowed")
	}
}
```

- [ ] **Step 2-4: Implement, test, iterate**

```go
// runner/session.go (modify)
type Session struct {
	AllowedRoots    []string
	ClaudeBin       string
	ExtraClaudeArgs []string
	MaxTasks        int
	Timeout         time.Duration
	Sender          Sender
	Streams         peer.BidirectionalStreamLookup
	Logger          *slog.Logger
	Now             func() time.Time

	mu    sync.Mutex
	tasks map[string]*taskHandle

	wmsMu sync.Mutex
	wms   map[string]*WorktreeManager
}

type taskHandle struct {
	cancel context.CancelFunc
	repo   string
}

func (s *Session) initMaps() {
	s.mu.Lock()
	if s.tasks == nil {
		s.tasks = make(map[string]*taskHandle)
	}
	s.mu.Unlock()
	s.wmsMu.Lock()
	if s.wms == nil {
		s.wms = make(map[string]*WorktreeManager)
	}
	s.wmsMu.Unlock()
}

func (s *Session) getWorktreeManager(repo string) *WorktreeManager {
	s.wmsMu.Lock()
	defer s.wmsMu.Unlock()
	if s.wms == nil {
		s.wms = make(map[string]*WorktreeManager)
	}
	if wm, ok := s.wms[repo]; ok {
		return wm
	}
	wm := &WorktreeManager{Repo: repo}
	s.wms[repo] = wm
	return wm
}

func (s *Session) repoAllowed(repo string) bool {
	for _, r := range s.AllowedRoots {
		if protocol.IsUnderRoot(r, repo) {
			return true
		}
	}
	return false
}
```

Remove the old `RepoPath string` and `wm *WorktreeManager` fields.

- [ ] **Step 5: Commit**

```bash
git add runner/session.go runner/session_test.go
git commit -m "runner/session: tasks/wms maps with per-repo WM caching"
```

### Task 7.3: `handleAssign` per-task ctx + panic recovery + repo gate

**Files:**
- Modify: `runner/session.go`
- Modify: `runner/session_test.go`

- [ ] **Step 1: Write the failing test for panic recovery**

```go
// runner/session_test.go (append)
func TestHandleAssignPanicSendsTaskFinished(t *testing.T) {
	ms := &mockSender{}
	s := &Session{
		AllowedRoots: []string{t.TempDir()}, // any abs dir
		MaxTasks:     2,
		Sender:       ms,
		Logger:       slog.Default(),
		Now:          time.Now,
	}
	s.initMaps()

	// Inject a panic via fake worktree manager that panics on Create.
	// (Easiest is to override getWorktreeManager via a test hook OR
	// replace ClaudeBin with a path that triggers a known panic in
	// post-creation step. Use whichever the test fixture supports.)
	s.testHookHandleAssign = func() { panic("test-panic") }

	req := &protocol.AssignTask{
		TaskId:   protocol.TaskID{Id: [16]byte{0xab}},
		RepoPath: []byte(s.AllowedRoots[0] + "/repo"),
		Prompt:   []byte("p"),
	}
	s.handleAssign(context.Background(), req)

	// Expect TaskAccepted + TaskFinished{ExitCode:-1, "panic: ..."} in ms.sent.
	if len(ms.sent) < 2 {
		t.Fatalf("expected TaskAccepted+TaskFinished, got %d msgs", len(ms.sent))
	}
	last := decodeRunnerMessage(t, ms.sent[len(ms.sent)-1])
	if last.Kind != protocol.RunnerMessageType_TaskFinished {
		t.Fatalf("last msg kind=%v want TaskFinished", last.Kind)
	}
	tf := last.TaskFinished()
	if tf.ExitCode != -1 || !bytes.Contains(tf.DiffInfo, []byte("panic: test-panic")) {
		t.Fatalf("unexpected TaskFinished: code=%d diff=%q", tf.ExitCode, tf.DiffInfo)
	}
}
```

`testHookHandleAssign` is a guarded test seam — declare it as a private field on `Session` and invoke it at the top of `handleAssign` if non-nil. This avoids exposing a complex injection path.

- [ ] **Step 2-4: Implement panic recovery + per-task ctx**

```go
// runner/session.go (replace handleAssign body)
func (s *Session) handleAssign(parentCtx context.Context, req *protocol.AssignTask) {
	s.initMaps()
	taskIDHex := hex.EncodeToString(req.TaskId.Id[:])
	repo := filepath.Clean(string(req.RepoPath))

	// Repo gate (defense-in-depth; server already filtered)
	if !s.repoAllowed(repo) {
		s.sendTaskAccepted(req.TaskId)
		s.sendTaskFinished(req.TaskId, -1, "repo not in allowed_roots: "+repo)
		return
	}

	taskCtx, cancel := context.WithCancel(parentCtx)
	s.mu.Lock()
	s.tasks[taskIDHex] = &taskHandle{cancel: cancel, repo: repo}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.tasks, taskIDHex)
		s.mu.Unlock()
		cancel()
	}()

	defer func() {
		if r := recover(); r != nil {
			s.logger().Error("task panic",
				"task_id", taskIDHex, "panic", r, "stack", string(debug.Stack()))
			s.sendTaskFinished(req.TaskId, -1, fmt.Sprintf("panic: %v", r))
		}
	}()

	if s.testHookHandleAssign != nil {
		s.testHookHandleAssign() // declared in test build only via _test.go file? Use a tag-guarded field instead. See note below.
	}

	// 1. TaskAccepted
	s.sendTaskAccepted(req.TaskId)

	// 2. Worktree
	wm := s.getWorktreeManager(repo)
	dir, err := wm.Create(taskIDHex)
	if err != nil {
		s.sendTaskFinished(req.TaskId, -1, "worktree_error: "+err.Error())
		return
	}

	// 3. TaskStarted
	s.sendTaskStarted(req.TaskId, dir)

	// 4. Process
	proc := &Process{
		ClaudeBin: s.ClaudeBin,
		CWD:       dir,
		Timeout:   s.Timeout,
		ExtraArgs: s.ExtraClaudeArgs,
	}
	logSink := func(data []byte) { _ = s.Sender.Publish(topics.TaskLog(taskIDHex), data) }
	exit, runErr := proc.Run(taskCtx, string(req.Prompt), logSink)

	// 5. TaskFinished
	if runErr != nil {
		s.sendTaskFinished(req.TaskId, -1, "process_error: "+runErr.Error())
	} else {
		s.sendTaskFinished(req.TaskId, int32(exit), "")
	}

	// 6. Cleanup
	if err := wm.Remove(taskIDHex); err != nil {
		s.logger().Warn("worktree remove failed", "task_id", taskIDHex, "err", err)
	}
}
```

Add small helper methods `sendTaskAccepted`, `sendTaskStarted`, `sendTaskFinished` that wrap the existing 4-line `m := &protocol.RunnerMessage{...}` blocks (DRY-up the existing inline code).

For the test seam: keep `testHookHandleAssign func()` as a normal (unexported) field — it's nil in production. This is simpler than build-tag plumbing and the field stays private to the package.

`handleOpenExec` gets the parallel treatment — register/deregister in `tasks` map, panic-recover, repo gate, per-task ctx — but uses `agentexec.ExecuteCommand` instead of `Process.Run`.

- [ ] **Step 5: Commit**

```bash
git add runner/session.go runner/session_test.go
git commit -m "runner/session: per-task ctx + panic recovery + repo gate in handleAssign/handleOpenExec"
```

### Task 7.4: `CancelTask` handler in `connect.go`

**Files:**
- Modify: `runner/connect.go`
- Modify: `runner/connect_test.go`

- [ ] **Step 1: Write the failing test**

```go
// runner/connect_test.go (append)
func TestRunnerHandlesCancelTaskCallsCancelFunc(t *testing.T) {
	s := &Session{}
	s.initMaps()
	called := false
	s.tasks["aabbccdd00112233445566778899aabb"] = &taskHandle{
		cancel: func() { called = true },
		repo:   "/x",
	}

	// Build a CancelTask wire message.
	req := &protocol.RunnerRequest{Kind: protocol.RunnerRequestType_CancelTask}
	var tid protocol.TaskID
	raw, _ := hex.DecodeString("aabbccdd00112233445566778899aabb")
	copy(tid.Id[:], raw)
	req.SetCancelTask(protocol.CancelTask{TaskId: tid})

	dispatchRunnerRequest(s, req, slog.Default()) // helper extracted from connect.go's OnControl

	if !called {
		t.Fatal("expected cancel func invoked")
	}
}
```

- [ ] **Step 2-4: Extract dispatch + implement Cancel**

Refactor `runner/connect.go`'s `OnControl` body into a package-private function so it can be tested without a real Conn:

```go
func dispatchRunnerRequest(s *Session, req *protocol.RunnerRequest, log *slog.Logger) {
	switch req.Kind {
	case protocol.RunnerRequestType_AssignTask:
		// ... unchanged
	case protocol.RunnerRequestType_CancelTask:
		ct := req.CancelTask()
		if ct == nil { return }
		taskIDHex := hex.EncodeToString(ct.TaskId.Id[:])
		s.mu.Lock()
		h, ok := s.tasks[taskIDHex]
		s.mu.Unlock()
		if !ok {
			log.Info("cancel for unknown task", "task_id", taskIDHex)
			return
		}
		h.cancel()
	case protocol.RunnerRequestType_OpenExec:
		// ... unchanged
	}
}
```

Update the existing `pc.SetOnControl(...)` callback to call `dispatchRunnerRequest(session, req, cfg.Logger)`.

- [ ] **Step 5: Commit**

```bash
git add runner/connect.go runner/connect_test.go
git commit -m "runner/connect: implement CancelTask handler via per-task cancelFunc"
```

### Task 7.5: Update `runner.Run` Hello payload

**Files:**
- Modify: `runner/connect.go`
- Modify: `runner/connect_test.go` (already covers Hello via existing harness)

- [ ] **Step 1: Replace `RepoPath`-only Config with `AllowedRoots`/`MaxTasks`/`Hostname`**

```go
// runner/connect.go
type Config struct {
	ServerCID       objproto.ConnectionID
	AllowedRoots    []string  // absolute, filepath.Clean'd
	MaxTasks        int       // >=1
	Hostname        string    // os.Hostname() at startup
	ClaudeBin       string
	ExtraClaudeArgs []string
	Logger          *slog.Logger
}

// In Run, after Dial + sender wiring:
session := &Session{
	AllowedRoots:    cfg.AllowedRoots,
	MaxTasks:        cfg.MaxTasks,
	ClaudeBin:       cfg.ClaudeBin,
	ExtraClaudeArgs: cfg.ExtraClaudeArgs,
	Sender:          sender,
	Streams:         pc.Transport(),
	Logger:          cfg.Logger,
	Now:             time.Now,
}
session.initMaps()

// Send the new Hello.
hello := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_Hello}
h := protocol.RunnerHello{Version: 1, MaxTasks: uint16(cfg.MaxTasks)}
h.SetHostname([]byte(cfg.Hostname))
roots := make([]protocol.AllowedRoot, 0, len(cfg.AllowedRoots))
for _, r := range cfg.AllowedRoots {
	var ar protocol.AllowedRoot
	ar.SetPath([]byte(r))
	roots = append(roots, ar)
}
h.SetAllowedRoots(roots)
hello.SetHello(h)
helloBytes := hello.MustAppend([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)})
if err := sender.Send(helloBytes); err != nil {
	return fmt.Errorf("send Hello: %w", err)
}
```

- [ ] **Step 2-4: Adjust connect_test.go to send the new Hello + update server-side `runner_handler.go` Hello receiver to populate the new RunnerEntry fields**

In `server/runner_handler.go`, change the Hello case:

```go
case protocol.RunnerMessageType_Hello:
	hello := msg.Hello()
	if hello == nil { return }
	if hello.MaxTasks < 1 {
		// reject — close the connection with a log; no formal response message
		log.Warn("rejecting Hello with MaxTasks<1", "id", connID)
		return
	}
	roots := make([]string, 0, len(hello.AllowedRoots))
	for _, ar := range hello.AllowedRoots {
		roots = append(roots, filepath.Clean(string(ar.Path)))
	}
	h.Registry.Add(&RunnerEntry{
		ID:           connID,
		Hostname:     string(hello.Hostname),
		AllowedRoots: roots,
		MaxTasks:     int(hello.MaxTasks),
		ActiveTasks:  map[string]struct{}{},
		ConnectedAt:  time.Now(),
		LastSeen:     time.Now(),
		Conn:         conn,
	})
```

- [ ] **Step 5: Commit**

```bash
git add runner/connect.go server/runner_handler.go server/runner_handler_test.go runner/connect_test.go
git commit -m "runner: send new Hello (allowed_roots, hostname, max_tasks); server consumes"
```

---

## Phase 8: `cmd/agent-runner` flags

### Task 8.1: Replace `--repo` with `--roots`, add `--max-tasks`

**Files:**
- Modify: `cmd/agent-runner/main.go`

- [ ] **Step 1: Update flags + Hostname acquisition**

```go
// cmd/agent-runner/main.go (replace flag block + main body)
var (
	serverCID  = flag.String("server-cid", "ws:127.0.0.1:8539-*", "...")
	rootsCSV   = flag.String("roots", ".", "comma-separated absolute paths the runner is allowed to serve")
	maxTasks   = flag.Int("max-tasks", 1, "maximum concurrent tasks (>=1)")
	claudeBin  = flag.String("claude-bin", "claude", "path to the claude binary")
	claudeArgs = flag.String("claude-args", "", "extra args before -p (whitespace-separated)")
	wsPath     = flag.String("ws-path", "/ws", "WebSocket URL path")
)

func main() {
	flag.Parse()
	cli.WebSocketPath = *wsPath
	if *maxTasks < 1 {
		slog.Error("--max-tasks must be >=1", "got", *maxTasks)
		os.Exit(1)
	}
	roots := strings.Split(*rootsCSV, ",")
	abs := make([]string, 0, len(roots))
	for _, r := range roots {
		r = strings.TrimSpace(r)
		if r == "" { continue }
		ar, err := filepath.Abs(r)
		if err != nil {
			slog.Error("--roots abs", "root", r, "err", err); os.Exit(1)
		}
		abs = append(abs, filepath.Clean(ar))
	}
	if len(abs) == 0 {
		slog.Error("--roots must contain at least one path"); os.Exit(1)
	}
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	peerCID, err := objproto.ParseConnectionID(*serverCID,
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		slog.Error("server-cid", "err", err); os.Exit(1)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := runner.Run(ctx, runner.Config{
		ServerCID:       peerCID,
		AllowedRoots:    abs,
		MaxTasks:        *maxTasks,
		Hostname:        hostname,
		ClaudeBin:       *claudeBin,
		ExtraClaudeArgs: strings.Fields(*claudeArgs),
		Logger:          slog.Default(),
	}); err != nil {
		slog.Error("runner exit", "err", err); os.Exit(1)
	}
}
```

- [ ] **Step 2: `make build` to verify compilation**

Run: `make build`
Expected: PASS for `bin/agent-runner` (server side may still fail until Phase 9). Note any remaining failures and address in subsequent tasks.

- [ ] **Step 3: Commit**

```bash
git add cmd/agent-runner/main.go
git commit -m "cmd/agent-runner: --roots CSV + --max-tasks; emit hostname"
```

---

## Phase 9: Client (`cli/`, `cmd/harness-cli`)

### Task 9.1: `cli/submit.go` selector encoding

**Files:**
- Modify: `cli/submit.go`
- Modify: `cli/submit_test.go` (or create)

- [ ] **Step 1: Write the failing test**

```go
// cli/submit_test.go (new or append)
func TestSubmitEncodesSelectorByHostname(t *testing.T) {
	// Use a Client backed by a fake objproto Connection that captures wire bytes.
	cli := newFakeClient(t)
	sel := SelectorOpts{Host: "gmkhost"}
	if _, err := cli.Submit(context.Background(), "/x/repo", "prompt", sel); err != nil {
		t.Fatal(err)
	}
	req := decodeLastSubmitRequest(t, cli)
	if req.Selector.Kind != protocol.RunnerSelectorKind_ByHostname {
		t.Fatalf("kind=%v want ByHostname", req.Selector.Kind)
	}
	if string(req.Selector.Hostname().Hostname) != "gmkhost" {
		t.Fatal("hostname mismatch")
	}
}

func TestSubmitRejectsMutuallyExclusiveSelectors(t *testing.T) {
	if err := ValidateSelector(SelectorOpts{Host: "a", IP: "1.2.3.4"}); err == nil {
		t.Fatal("expected error for both Host and IP")
	}
}
```

- [ ] **Step 2-4: Implement**

```go
// cli/submit.go (replace Submit + add Selector helpers)
type SelectorOpts struct {
	RunnerID string // ConnectionID string
	Host     string
	IP       string
}

func ValidateSelector(s SelectorOpts) error {
	count := 0
	if s.RunnerID != "" { count++ }
	if s.Host != ""     { count++ }
	if s.IP != ""       { count++ }
	if count > 1 {
		return fmt.Errorf("selector: at most one of --runner / --host / --ip")
	}
	return nil
}

func buildSelector(s SelectorOpts) (protocol.RunnerSelector, error) {
	var sel protocol.RunnerSelector
	switch {
	case s.RunnerID != "":
		sel.Kind = protocol.RunnerSelectorKind_ByRunnerId
		cid, err := objproto.ParseConnectionID(s.RunnerID, 0)
		if err != nil { return sel, err }
		sel.SetRunnerId(toProtocolRunnerID(cid))
	case s.Host != "":
		sel.Kind = protocol.RunnerSelectorKind_ByHostname
		var h protocol.Hostname
		h.SetHostname([]byte(s.Host))
		sel.SetHostname(h)
	case s.IP != "":
		sel.Kind = protocol.RunnerSelectorKind_ByIp
		ip := net.ParseIP(s.IP)
		if ip == nil { return sel, fmt.Errorf("invalid IP %q", s.IP) }
		var addr protocol.IPAddr
		if v4 := ip.To4(); v4 != nil {
			addr.SetIpAddr(v4)
		} else {
			addr.SetIpAddr(ip.To16())
		}
		sel.SetIp(addr)
	default:
		sel.Kind = protocol.RunnerSelectorKind_None
	}
	return sel, nil
}

func (c *Client) Submit(ctx context.Context, repo, prompt string, sel SelectorOpts) (string, error) {
	if err := ValidateSelector(sel); err != nil { return "", err }
	s, err := buildSelector(sel)
	if err != nil { return "", err }
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_Submit}
	sr := protocol.SubmitRequest{Selector: s, Prompt: []byte(prompt)}
	sr.SetRepoPath([]byte(repo))
	req.SetSubmit(sr)
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil { return "", err }
	if resp.Kind != protocol.TaskControlKind_Submit {
		return "", fmt.Errorf("unexpected response kind: %v", resp.Kind)
	}
	sresp := resp.Submit()
	if sresp == nil { return "", fmt.Errorf("submit response missing") }
	if sresp.Status != protocol.SubmitStatus_Ok {
		return "", fmt.Errorf("submit %s: %s", sresp.Status, sresp.ErrorMsg)
	}
	return hex.EncodeToString(sresp.TaskId.Id[:]), nil
}
```

`toProtocolRunnerID` constructs a `protocol.RunnerID` from an `objproto.ConnectionID`, taking care of the IpAddrLen invariant (per project memory). If the ConnectionID has no IP (purely random), reject — selector by_runner_id requires a fully-qualified ID. If you can't parse out an IP, return an error message that says so.

- [ ] **Step 5: Commit**

```bash
git add cli/submit.go cli/submit_test.go
git commit -m "cli/submit: encode RunnerSelector; surface SubmitResponse error"
```

### Task 9.2: `cli/interactive.go` selector

Mirror Task 9.1 for `cli/interactive.go`. Same `SelectorOpts` and `ValidateSelector` (move them to a shared file `cli/selector.go` so both submit and interactive import them — DRY). Decode `OpenInteractiveResponse.Status` and surface `runner_busy` / `ambiguous_runner` / `pinned_not_found`.

```bash
git commit -m "cli/interactive: encode RunnerSelector; share SelectorOpts with submit"
```

### Task 9.3: `cli/ls.go` display refactor

**Files:**
- Modify: `cli/ls.go`
- Modify: `cli/ls_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestLsRendersHostRootsTasks(t *testing.T) {
	rs := []protocol.RunnerInfo{
		{Hostname: []byte("gmkhost"), Status: protocol.RunnerStatus_Idle, MaxTasks: 4,
			ActiveTasks: nil, AllowedRoots: []protocol.AllowedRoot{{Path: []byte("/x")}, {Path: []byte("/y")}}},
		{Hostname: []byte("raspi"), Status: protocol.RunnerStatus_Busy, MaxTasks: 1,
			ActiveTasks: []protocol.ActiveTaskRef{{}}, AllowedRoots: []protocol.AllowedRoot{{Path: []byte("/p")}}},
	}
	out := RenderRunners(rs)
	if !strings.Contains(out, "host=gmkhost") || !strings.Contains(out, "tasks=0/4") {
		t.Fatalf("missing host or tasks column: %q", out)
	}
	if !strings.Contains(out, "roots=/x,/y") {
		t.Fatalf("missing roots: %q", out)
	}
	if !strings.Contains(out, "Busy") {
		t.Fatalf("missing Busy: %q", out)
	}
}
```

- [ ] **Step 2-5: Implement, commit**

```go
func RenderRunners(rs []protocol.RunnerInfo) string {
	var b strings.Builder
	b.WriteString("RUNNERS\n")
	for _, r := range rs {
		roots := make([]string, 0, len(r.AllowedRoots))
		for _, ar := range r.AllowedRoots {
			roots = append(roots, string(ar.Path))
		}
		fmt.Fprintf(&b, "  %-7s host=%s  tasks=%d/%d  roots=%s\n",
			statusName(r.Status), r.Hostname, len(r.ActiveTasks), r.MaxTasks,
			strings.Join(roots, ","))
	}
	return b.String()
}
```

```bash
git commit -m "cli/ls: render host/tasks/roots columns"
```

### Task 9.4: `cmd/harness-cli/main.go` selector flags + mutex

**Files:**
- Modify: `cmd/harness-cli/main.go`

- [ ] **Step 1: Add flags to submit / interactive subcommands**

```go
// In the submit subcommand parse block
var selRunner, selHost, selIP string
fs.StringVar(&selRunner, "runner", "", "pin to a specific runner ConnectionID")
fs.StringVar(&selHost,   "host",   "", "pin to a runner with this hostname")
fs.StringVar(&selIP,     "ip",     "", "pin to a runner with this IP address")
// after fs.Parse:
opts := cli.SelectorOpts{RunnerID: selRunner, Host: selHost, IP: selIP}
if err := cli.ValidateSelector(opts); err != nil {
	fmt.Fprintln(os.Stderr, err); os.Exit(2)
}
// pass opts to cli.Submit / cli.Interactive
```

- [ ] **Step 2: Build verification**

Run: `make build && bin/harness-cli submit --help`
Expected: see new `--runner` / `--host` / `--ip` flags.

- [ ] **Step 3: Commit**

```bash
git add cmd/harness-cli/main.go
git commit -m "cmd/harness-cli: --runner / --host / --ip selector flags"
```

---

## Phase 10: TUI + WebUI surface

### Task 10.1: TUI runner pane columns + submit selector + interactive Candidates

**Files:**
- Modify: `tui/runner_pane.go` (or wherever `repo` is rendered today — locate via `grep -rn "repo=" tui/`)
- Modify: `tui/submit_dialog.go`
- Modify: `tui/interactive_attach.go`

This is the largest UI-side block. Approach:

- [ ] **Step 1: Run the existing TUI smoke tests (if any) to capture the green baseline**

Run: `go test ./tui/...`
Expected: PASS at the current state.

- [ ] **Step 2: Replace `repo=...` rendering with the new columns**

For each rendering point that currently uses `runner.RepoPath`, change to render `host=<hostname>  tasks=<n>/<m>  roots=<csv>` matching `cli/ls.go`. The `runner.repo` index lookups need to map to `AllowedRoots` lookups; the existing 1:1 repo→runner code paths become "find candidates whose roots cover this repo, fail if ambiguous".

- [ ] **Step 3: Submit dialog adds optional pin dropdown**

Add a "Runner pin" field to the submit form:
- "(any)" → SelectorOpts{} (none)
- "<hostname>" → SelectorOpts{Host: ...}

Pass through to `cli.Submit`. On `ambiguous_runner` response, re-open the submit dialog with the pin dropdown defaulted to the first ambiguous match and a status banner explaining why.

- [ ] **Step 4: Interactive attach**

Replace single-runner-for-repo lookup with `cli.OpenInteractive`. Status responses other than `ok` should display in the TUI's status bar with the relevant hostname suggestion (server's `error_msg` already contains "matches: A, B").

- [ ] **Step 5: Run + manually verify**

Run: `make build && bin/harness-server --listen :8539 &` plus `bin/agent-runner --roots <repo> --max-tasks 2 &` plus `bin/harness-tui --server-cid ws:127.0.0.1:8539-1`. Submit two tasks, verify both run concurrently and the runner pane shows `tasks=2/2`. Test pin dropdown.

- [ ] **Step 6: Commit**

```bash
git add tui/
git commit -m "tui: multi-task display, submit pin selector, interactive Candidates"
```

### Task 10.2: WebUI runner display + selector

**Files:**
- Modify: `cmd/harness-webui-wasm/main.go`
- Modify: `webui/static/style.css` (if column changes need styling)
- Modify: `webui/static/app.js` (or whichever JS the WASM wraps)

- [ ] **Step 1: Update the runner select dropdown to show host + tasks/max**

```html
<option value="A">gmkhost — 1/4 — /x,/y</option>
```

- [ ] **Step 2: Submit form gains optional pin field**

Same SelectorOpts model serialized via the WASM bridge.

- [ ] **Step 3: Build + smoke-test in browser**

Run: `make webui-build && bin/harness-server --listen :8539` and open `http://localhost:8539/`. Submit a task, verify the new fields render.

- [ ] **Step 4: Commit**

```bash
git commit -m "webui: render host/tasks/roots; submit pin selector"
```

---

## Phase 11: Integration tests

### Task 11.1: Multi-task concurrent execution

**Files:**
- Create: `integration/multi_task_test.go`

- [ ] **Step 1: Write the test**

```go
// integration/multi_task_test.go
package integration

import (
	"context"
	"strings"
	"testing"
	"time"
	// ...
)

func TestIntegrationTwoTasksConcurrent(t *testing.T) {
	srv, cleanup := startServer(t) // existing fixture
	defer cleanup()
	r := startRunner(t, srv, runnerOpts{MaxTasks: 2, Roots: []string{tempRepo(t)}})
	defer r.Close()

	c := dialClient(t, srv)
	defer c.Close()

	id1 := mustSubmit(t, c, r.RootsCSV(), "echo one")
	id2 := mustSubmit(t, c, r.RootsCSV(), "echo two")

	waitTaskTerminal(t, c, id1, 30*time.Second)
	waitTaskTerminal(t, c, id2, 30*time.Second)

	// Expect both Succeeded
	t1, _ := c.GetTask(context.Background(), id1)
	t2, _ := c.GetTask(context.Background(), id2)
	if t1.Status != protocol.TaskStatus_Succeeded || t2.Status != protocol.TaskStatus_Succeeded {
		t.Fatalf("expected both Succeeded; got %v %v", t1.Status, t2.Status)
	}
}
```

`startServer`, `startRunner`, `dialClient`, etc. follow `integration/e2e_test.go`'s existing patterns. `tempRepo` initializes an empty `git init` directory so worktree creation works. `claude-bin` should point to a stub (the existing `testdata/fake-claude.sh`?) so we don't actually invoke real claude in CI.

- [ ] **Step 2: Run, iterate**

Run: `go test ./integration/ -run TestIntegrationTwoTasksConcurrent -v`
Expected: PASS, completes within a few seconds.

- [ ] **Step 3: Commit**

```bash
git add integration/multi_task_test.go
git commit -m "integration: two tasks run concurrently on one runner"
```

### Task 11.2: Capacity queues then auto-dispatches

**Files:**
- Modify: `integration/multi_task_test.go`

- [ ] **Step 1: Write the test**

```go
func TestIntegrationCapacityQueueing(t *testing.T) {
	srv, cleanup := startServer(t)
	defer cleanup()
	r := startRunner(t, srv, runnerOpts{MaxTasks: 1, Roots: []string{tempRepo(t)}})
	defer r.Close()
	c := dialClient(t, srv)
	defer c.Close()

	id1 := mustSubmit(t, c, r.RootsCSV(), "sleep 2; echo one")
	id2 := mustSubmit(t, c, r.RootsCSV(), "echo two")

	// id2 must be Queued while id1 is Running
	require.Eventually(t, func() bool {
		t1, _ := c.GetTask(context.Background(), id1)
		t2, _ := c.GetTask(context.Background(), id2)
		return t1.Status == protocol.TaskStatus_Running && t2.Status == protocol.TaskStatus_Queued
	}, 5*time.Second, 100*time.Millisecond)

	waitTaskTerminal(t, c, id1, 30*time.Second)
	waitTaskTerminal(t, c, id2, 30*time.Second)

	t2, _ := c.GetTask(context.Background(), id2)
	if t2.Status != protocol.TaskStatus_Succeeded {
		t.Fatalf("id2 should auto-dispatch and succeed; got %v", t2.Status)
	}
}
```

- [ ] **Step 2: Run + commit**

```bash
git commit -m "integration: capacity queue + auto-dispatch on slot release"
```

### Task 11.3: Ambiguous + pin success + pin not found

- [ ] **Step 1: Write the tests**

```go
// integration/multi_task_test.go (append)
func TestIntegrationAmbiguousRunner(t *testing.T) {
	srv, cleanup := startServer(t)
	defer cleanup()
	repo := tempRepo(t)
	r1 := startRunner(t, srv, runnerOpts{MaxTasks: 1, Roots: []string{repo}, Hostname: "h1"})
	defer r1.Close()
	r2 := startRunner(t, srv, runnerOpts{MaxTasks: 1, Roots: []string{repo}, Hostname: "h2"})
	defer r2.Close()
	c := dialClient(t, srv)
	defer c.Close()

	_, err := c.Submit(context.Background(), repo, "echo", cli.SelectorOpts{}) // no pin
	if err == nil {
		t.Fatal("expected ambiguous_runner error")
	}
	if !strings.Contains(err.Error(), "ambiguous_runner") {
		t.Fatalf("err=%v want ambiguous_runner", err)
	}
}

func TestIntegrationPinByHostnameSuccess(t *testing.T) {
	srv, cleanup := startServer(t)
	defer cleanup()
	repo := tempRepo(t)
	r1 := startRunner(t, srv, runnerOpts{MaxTasks: 1, Roots: []string{repo}, Hostname: "gmkhost"})
	defer r1.Close()
	r2 := startRunner(t, srv, runnerOpts{MaxTasks: 1, Roots: []string{repo}, Hostname: "raspi"})
	defer r2.Close()
	c := dialClient(t, srv)
	defer c.Close()

	id, err := c.Submit(context.Background(), repo, "echo", cli.SelectorOpts{Host: "raspi"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	waitTaskTerminal(t, c, id, 30*time.Second)
	ti, _ := c.GetTask(context.Background(), id)
	if !strings.Contains(string(ti.AssignedTo.String()), r2.RunnerID()) {
		t.Fatalf("expected task on raspi runner, got AssignedTo=%v", ti.AssignedTo)
	}
}

func TestIntegrationPinNotFound(t *testing.T) {
	srv, cleanup := startServer(t)
	defer cleanup()
	repo := tempRepo(t)
	r := startRunner(t, srv, runnerOpts{MaxTasks: 1, Roots: []string{repo}, Hostname: "gmkhost"})
	defer r.Close()
	c := dialClient(t, srv)
	defer c.Close()

	_, err := c.Submit(context.Background(), repo, "echo", cli.SelectorOpts{Host: "nowhere"})
	if err == nil || !strings.Contains(err.Error(), "pinned_not_found") {
		t.Fatalf("expected pinned_not_found, got %v", err)
	}
}
```

- [ ] **Step 2: Run + commit**

Run: `go test ./integration/ -run "TestIntegrationAmbiguousRunner|TestIntegrationPinByHostnameSuccess|TestIntegrationPinNotFound" -v`

```bash
git commit -m "integration: ambiguous detection + pin success + pin not found"
```

### Task 11.4: Cancel mid-execution

- [ ] **Step 1: Write the test**

```go
func TestIntegrationCancelKillsClaude(t *testing.T) {
	srv, cleanup := startServer(t)
	defer cleanup()
	r := startRunner(t, srv, runnerOpts{
		MaxTasks: 1,
		Roots:    []string{tempRepo(t)},
		// fake-claude.sh sleeps for the value of $1; we'll prompt with "60"
	})
	defer r.Close()
	c := dialClient(t, srv)
	defer c.Close()

	id := mustSubmit(t, c, r.RootsCSV(), "60") // fake claude sleeps 60s
	require.Eventually(t, func() bool {
		ti, _ := c.GetTask(context.Background(), id)
		return ti.Status == protocol.TaskStatus_Running
	}, 5*time.Second, 100*time.Millisecond)

	if err := c.Cancel(context.Background(), id); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// Within ~10s (5s SIGTERM grace + slack) the runner-side process must die
	// and TaskFinished must come back; the state remains Cancelled (idempotent).
	require.Eventually(t, func() bool {
		ti, _ := c.GetTask(context.Background(), id)
		return ti.Status == protocol.TaskStatus_Cancelled && ti.EndedAt != 0
	}, 15*time.Second, 200*time.Millisecond)

	// Capacity must be released even though TaskFinished was suppressed by terminal-state idempotency.
	require.Eventually(t, func() bool {
		runners := c.MustList(context.Background()).Runners
		for _, ru := range runners {
			if ru.Hostname == r.Hostname() {
				return len(ru.ActiveTasks) == 0
			}
		}
		return false
	}, 5*time.Second, 100*time.Millisecond)
}
```

- [ ] **Step 2: Run + commit**

```bash
git commit -m "integration: cancel of Running task kills claude and releases capacity"
```

### Task 11.5: Panic isolation

- [ ] **Step 1: Write the test**

This one cannot use the production `agent-runner` binary because the panic seam (`testHookHandleAssign`) is a private package field. The integration test instead drives `runner.Session` directly with the harness already in place at `runner/session_test.go` style — i.e. it lives in `runner/` not `integration/`.

```go
// runner/session_test.go (append)
func TestSessionPanicIsolatesSiblingTask(t *testing.T) {
	repo := initGitRepo(t) // existing fixture from session_test.go style
	ms := &mockSender{}
	s := &Session{
		AllowedRoots: []string{repo},
		MaxTasks:     2,
		ClaudeBin:    fakeClaudePath(t), // existing fixture: prints "ok" and exits 0
		Sender:       ms,
		Logger:       slog.Default(),
		Now:          time.Now,
	}
	s.initMaps()

	// Arm panic for the FIRST task only.
	armed := false
	s.testHookHandleAssign = func() {
		if !armed {
			armed = true
			panic("test-panic")
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.handleAssign(context.Background(), &protocol.AssignTask{
			TaskId:   protocol.TaskID{Id: [16]byte{0x01}},
			RepoPath: []byte(repo + "/repo"), Prompt: []byte("a"),
		})
	}()
	go func() {
		defer wg.Done()
		// Small delay so the first goroutine arms the seam first.
		time.Sleep(50 * time.Millisecond)
		s.handleAssign(context.Background(), &protocol.AssignTask{
			TaskId:   protocol.TaskID{Id: [16]byte{0x02}},
			RepoPath: []byte(repo + "/repo"), Prompt: []byte("b"),
		})
	}()
	wg.Wait()

	// Inspect ms.sent: TaskFinished for 0x01 must have ExitCode -1 + "panic:" in DiffInfo;
	// TaskFinished for 0x02 must have ExitCode 0 (success).
	finishedByID := collectTaskFinished(t, ms.sent)
	if got := finishedByID[[16]byte{0x01}]; got.ExitCode != -1 || !bytes.Contains(got.DiffInfo, []byte("panic:")) {
		t.Fatalf("task 0x01 expected panic-failed, got code=%d diff=%q", got.ExitCode, got.DiffInfo)
	}
	if got := finishedByID[[16]byte{0x02}]; got.ExitCode != 0 {
		t.Fatalf("task 0x02 expected Succeeded; got code=%d", got.ExitCode)
	}
}
```

`collectTaskFinished` decodes `ms.sent` filtering `RunnerMessageType_TaskFinished` and returns a map keyed by `TaskID.Id`.

- [ ] **Step 2: Run + commit**

Run: `go test ./runner/ -run TestSessionPanicIsolatesSiblingTask -v -race`

```bash
git commit -m "runner: per-task panic isolation; siblings unaffected"
```

### Task 11.6: Runner disconnect with N stranded tasks

```go
func TestIntegrationDisconnectMarksTasksFailed(t *testing.T) {
	srv, cleanup := startServer(t)
	defer cleanup()
	r := startRunner(t, srv, runnerOpts{MaxTasks: 4, Roots: []string{tempRepo(t)}})
	c := dialClient(t, srv)
	defer c.Close()

	ids := []string{
		mustSubmit(t, c, r.RootsCSV(), "sleep 60"),
		mustSubmit(t, c, r.RootsCSV(), "sleep 60"),
		mustSubmit(t, c, r.RootsCSV(), "sleep 60"),
	}
	require.Eventually(t, func() bool {
		for _, id := range ids {
			ti, _ := c.GetTask(context.Background(), id)
			if ti.Status != protocol.TaskStatus_Running { return false }
		}
		return true
	}, 5*time.Second, 100*time.Millisecond)

	r.Close() // disconnect

	for _, id := range ids {
		waitTaskTerminal(t, c, id, 5*time.Second)
		ti, _ := c.GetTask(context.Background(), id)
		if ti.Status != protocol.TaskStatus_Failed {
			t.Fatalf("%s status=%v want Failed", id, ti.Status)
		}
		if !strings.Contains(string(ti.DiffInfo), "runner_disconnected") {
			t.Fatalf("%s diff=%q want runner_disconnected", id, ti.DiffInfo)
		}
	}
}
```

```bash
git commit -m "integration: runner disconnect marks all stranded tasks Failed"
```

---

## Phase 12: Cleanup

### Task 12.1: Remove dead `OldestIdleForRepo`

After Phases 5+ all callers use `Candidates`. Delete `OldestIdleForRepo` and any deprecation shim from `server/registry.go` and its tests.

```bash
git commit -m "server/registry: remove obsolete OldestIdleForRepo"
```

### Task 12.2: Run the full battery and document

- [ ] `make check`
- [ ] `make test`
- [ ] `go test ./... -race`
- [ ] `go test ./integration/ -v`

Manual smoke (single physical machine): start server, start one runner with `--max-tasks 2 --roots <abs-path>`, submit two tasks via `harness-cli`, verify both complete; cancel one mid-flight; kill the runner mid-task and verify `harness-cli ls` shows the abandoned task as `Failed` with reason `runner_disconnected`.

```bash
git commit -m "ship: multi-task per runner & multi-repo allowed roots" --allow-empty
```

(Empty commit as a marker — optional; skip if already obvious from PR title.)

---

## Risk recap

1. **brgen `match` with sub-format payload**: Phase 1 may hit unimplemented codegen. Fallback recipe documented inline (Task 1.1 Step 2).
2. **Cross-OS git worktree** on Windows: Task 7.1's mutex covers in-process races; verify on a real Windows host before declaring done. Add a manual smoke step in Task 12.2.
3. **WAL Selector format**: JSON vs base64 — pick whichever is easier; document the choice in `server/wal.go` source comment.
4. **`RunnerID.IpAddrLen`** invariant: `cli.buildSelector` for `--runner` must error if the parsed ConnectionID has no IP — see Task 9.1 note.
5. **TUI/WebUI scope creep**: Phase 10 is the largest UI block; if it grows beyond ~3-day estimates, split TUI and WebUI into separate phases per the user's "appropriately-scoped" preference.
