# Client workspace config — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A named workspace in `.harness/config` restores the connection values, the resumed task, its port forwards and the grid — on client start, on reconnect, and on `workspace apply`.

**Architecture:** A new leaf package `cli/workspace` owns the file: parse, validate, render, and locate. It delegates every value grammar it does not own (`cli.ParseForwardSpec`, `cli.ParseRemoteForwardSpec`, and a `cli.ParseGridArgs` extracted from the TUI in Task 3). The TUI gains one apply routine, reached from three paths that all funnel through the same code. Nothing on the wire changes.

**Tech Stack:** Go 1.25.7, standard library only for the file format (`strings`, `bufio`), `github.com/google/shlex` (already a direct dependency) for splitting `forward` and `grid` values.

**Spec:** `docs/superpowers/specs/2026-08-24-workspace-startup-config-design.md`

## Global Constraints

- **No new go.mod dependency.** The file format is line-oriented and hand-parsed for this reason. Do not add a TOML or YAML library.
- **No `.bgn`, server, runner, or WAL change.** If a task seems to need one, stop and report — the spec's premise is that this is client-local.
- **The config is not read when `HARNESS_AUTH_TICKET` is set.** In-task agents must never pick up an operator's workspace.
- **One grammar, one parser.** A `forward` value goes to `cli.ParseForwardSpec` / `cli.ParseRemoteForwardSpec`; a `grid` value goes to `cli.ParseGridArgs`. Never re-implement either.
- **Import direction:** `cli/workspace` imports `cli`. `cli` must never import `cli/workspace`. `cli/cliopts` imports neither.
- **Build hygiene:** compile-check with `go build ./...` or `go vet ./...`. NEVER bare `go build ./cmd/<x>/` — it drops a binary in the worktree root. The working tree must be as clean after your checks as before.
- **Verification runs through make targets**, not ad-hoc `go build`: `make vet`, `make test`, `make check`.
- **Naming:** the operator-facing word is **workspace**. Never *profile* — that already names an agent preset in this repository.

---

### Task 1: The file grammar — parse and render

**Files:**
- Create: `cli/workspace/config.go`
- Test: `cli/workspace/config_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `workspace.Parse(io.Reader) (*File, error)`, `(*File).Render() []byte`, `(*File).Workspace(name string) (*Workspace, bool)`, `(*File).Names() []string`, and the types `File`, `Workspace`, `Task`, `Resume`, `Runner` with the constants `ResumeNo`/`ResumeContinue`/`ResumeFresh` and `RunnerAssigned`/`RunnerAny`.

- [ ] **Step 1: Write the failing round-trip and error tests**

```go
package workspace

import (
	"strings"
	"testing"
)

const sample = `# my harness setup

[workspace default]
server-cid = ws:example.invalid:8539-*
repo       = /abs/path/to/repo
grid       = --under 3f2a9c00000000000000000000000001

[workspace default task 3f2a9c00000000000000000000000001]
resume  = continue
runner  = assigned
forward = -L 3000:127.0.0.1:3000
forward = -R 8080:127.0.0.1:8080

# a second one, for the phone
[workspace phone]
server-cid = ws:example.invalid:8539-*
`

func TestParseRenderRoundTrip(t *testing.T) {
	f, err := Parse(strings.NewReader(sample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := string(f.Render()); got != sample {
		t.Errorf("Render is not byte-identical to input:\n--- got ---\n%s\n--- want ---\n%s", got, sample)
	}
}

func TestParseValues(t *testing.T) {
	f, err := Parse(strings.NewReader(sample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	ws, ok := f.Workspace("default")
	if !ok {
		t.Fatal(`Workspace("default") not found`)
	}
	if ws.Repo != "/abs/path/to/repo" {
		t.Errorf("Repo = %q", ws.Repo)
	}
	if ws.Grid != "--under 3f2a9c00000000000000000000000001" || !ws.GridSet {
		t.Errorf("Grid = %q set=%v", ws.Grid, ws.GridSet)
	}
	if len(ws.Tasks) != 1 {
		t.Fatalf("len(Tasks) = %d, want 1", len(ws.Tasks))
	}
	tk := ws.Tasks[0]
	if tk.Resume != ResumeContinue || tk.Runner != RunnerAssigned {
		t.Errorf("resume=%q runner=%q", tk.Resume, tk.Runner)
	}
	if len(tk.Forwards) != 2 || tk.Forwards[0] != "-L 3000:127.0.0.1:3000" {
		t.Errorf("Forwards = %q", tk.Forwards)
	}
	if names := f.Names(); len(names) != 2 || names[0] != "default" || names[1] != "phone" {
		t.Errorf("Names() = %q, want [default phone] in file order", names)
	}
}

func TestParseDefaults(t *testing.T) {
	f, err := Parse(strings.NewReader(
		"[workspace w]\n[workspace w task 3f2a9c00000000000000000000000001]\nforward = -L 1:127.0.0.1:1\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	ws, _ := f.Workspace("w")
	if ws.Tasks[0].Resume != ResumeNo {
		t.Errorf("resume default = %q, want %q", ws.Tasks[0].Resume, ResumeNo)
	}
	if ws.Tasks[0].Runner != RunnerAssigned {
		t.Errorf("runner default = %q, want %q", ws.Tasks[0].Runner, RunnerAssigned)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"unknown key":       "[workspace w]\nfowardd = -L 1:127.0.0.1:1\n",
		"unknown header":    "[profile w]\n",
		"bad resume":        "[workspace w]\n[workspace w task 3f2a9c00000000000000000000000001]\nresume = maybe\n",
		"bad runner":        "[workspace w]\n[workspace w task 3f2a9c00000000000000000000000001]\nrunner = whoever\n",
		"short task id":     "[workspace w]\n[workspace w task 3f2a9c]\n",
		"task before ws":    "[workspace w task 3f2a9c00000000000000000000000001]\n",
		"duplicate ws":      "[workspace w]\n[workspace w]\n",
		"key before header": "repo = /x\n",
		"no equals":         "[workspace w]\nrepo\n",
	}
	for name, src := range cases {
		if _, err := Parse(strings.NewReader(src)); err == nil {
			t.Errorf("%s: Parse succeeded, want an error", name)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./cli/workspace/ -run 'TestParse' -v`
Expected: FAIL — the package does not exist yet (`no Go files` / undefined identifiers).

- [ ] **Step 3: Write `cli/workspace/config.go`**

The `File` keeps every input line verbatim plus, per workspace, the half-open line span `[start, end)` it occupies. That span is what Task 4 replaces, and holding the raw lines is what makes the round-trip byte-identical.

```go
// Package workspace reads and writes .harness/config: named client-side
// workspaces describing which task to bring back, which forwards to establish
// and which grid to open when a client starts, reconnects, or is asked to
// re-apply.
//
// This package owns the FILE, not the value grammars inside it. A forward value
// is handed to cli.ParseForwardSpec / cli.ParseRemoteForwardSpec and a grid
// value to cli.ParseGridArgs, so the config can never accept a spelling the
// command line rejects.
package workspace

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// Resume says what an apply does with a task found in a terminal state.
type Resume string

const (
	ResumeNo       Resume = "no"       // leave it alone (default)
	ResumeContinue Resume = "continue" // resume keeping the agent's conversation (r/u)
	ResumeFresh    Resume = "fresh"    // resume without it (R/U)
)

// Runner says which runner may take a resumed task.
type Runner string

const (
	RunnerAssigned Runner = "assigned" // the one it last ran on (r/R); default
	RunnerAny      Runner = "any"      // any runner (u/U)
)

// Task is one [workspace <name> task <id>] block.
type Task struct {
	ID       string // 32 lowercase hex
	Resume   Resume
	Runner   Runner
	Forwards []string // verbatim "-L …" / "-R …", in file order
}

// Workspace is one [workspace <name>] block plus its task blocks.
type Workspace struct {
	Name      string
	ServerCID string
	WSPath    string
	Repo      string
	// Grid is the `grid` command's argument string, verbatim. GridSet
	// separates an absent key (do not touch the grid) from `grid =` with an
	// empty value (open the unnarrowed grid) — the empty string is a
	// meaningful value here, so presence needs its own bit.
	Grid    string
	GridSet bool
	Tasks   []Task

	start, end int // half-open line span in File.lines
}

// File is a parsed .harness/config with every source line retained.
type File struct {
	lines []string
	ws    []*Workspace
}

// Parse reads a config. Every unrecognised header, key or enum value is an
// error: a typo that silently established nothing is the failure this rejects.
func Parse(r io.Reader) (*File, error) {
	f := &File{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var cur *Workspace
	var curTask *Task // points into cur.Tasks
	lineNo := 0
	for sc.Scan() {
		raw := sc.Text()
		f.lines = append(f.lines, raw)
		lineNo++
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return nil, fmt.Errorf("%s:%d: unterminated section header %q", configName, lineNo, line)
			}
			name, taskID, err := parseHeader(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"), lineNo)
			if err != nil {
				return nil, err
			}
			if taskID == "" {
				if _, dup := f.Workspace(name); dup {
					return nil, fmt.Errorf("%s:%d: workspace %q declared twice", configName, lineNo, name)
				}
				if cur != nil {
					cur.end = lineNo - 1
				}
				cur = &Workspace{Name: name, start: lineNo - 1, end: lineNo}
				f.ws = append(f.ws, cur)
				curTask = nil
				continue
			}
			if cur == nil || cur.Name != name {
				return nil, fmt.Errorf("%s:%d: task block names workspace %q, which is not the open one", configName, lineNo, name)
			}
			cur.Tasks = append(cur.Tasks, Task{ID: taskID, Resume: ResumeNo, Runner: RunnerAssigned})
			curTask = &cur.Tasks[len(cur.Tasks)-1]
			continue
		}
		if cur == nil {
			return nil, fmt.Errorf("%s:%d: %q appears before any [workspace …] header", configName, lineNo, line)
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: %q is not `key = value`", configName, lineNo, line)
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		if err := assign(cur, curTask, key, val, lineNo); err != nil {
			return nil, err
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if cur != nil {
		cur.end = lineNo
	}
	for _, w := range f.ws {
		w.end = min(w.end, len(f.lines))
	}
	return f, nil
}

const configName = ".harness/config"

// parseHeader accepts "workspace <name>" and "workspace <name> task <32hex>".
func parseHeader(body string, lineNo int) (name, taskID string, err error) {
	tok := strings.Fields(body)
	switch {
	case len(tok) == 2 && tok[0] == "workspace":
		return tok[1], "", nil
	case len(tok) == 4 && tok[0] == "workspace" && tok[2] == "task":
		id := strings.ToLower(tok[3])
		if !isHex32(id) {
			return "", "", fmt.Errorf("%s:%d: %q is not a 32-hex task id", configName, lineNo, tok[3])
		}
		return tok[1], id, nil
	}
	return "", "", fmt.Errorf("%s:%d: unknown section header [%s] (want [workspace <name>] or [workspace <name> task <32-hex>])", configName, lineNo, body)
}

func isHex32(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// assign writes one key into the workspace or the open task block. Which keys
// are legal depends on which block is open, so an operator who puts `forward`
// under [workspace w] is told where it belongs rather than silently ignored.
func assign(ws *Workspace, tk *Task, key, val string, lineNo int) error {
	if tk != nil {
		switch key {
		case "resume":
			switch Resume(val) {
			case ResumeNo, ResumeContinue, ResumeFresh:
				tk.Resume = Resume(val)
				return nil
			}
			return fmt.Errorf("%s:%d: resume = %q (want no, continue or fresh)", configName, lineNo, val)
		case "runner":
			switch Runner(val) {
			case RunnerAssigned, RunnerAny:
				tk.Runner = Runner(val)
				return nil
			}
			return fmt.Errorf("%s:%d: runner = %q (want assigned or any)", configName, lineNo, val)
		case "forward":
			tk.Forwards = append(tk.Forwards, val)
			return nil
		}
		return fmt.Errorf("%s:%d: unknown key %q in a task block (want resume, runner or forward)", configName, lineNo, key)
	}
	switch key {
	case "server-cid":
		ws.ServerCID = val
	case "ws-path":
		ws.WSPath = val
	case "repo":
		ws.Repo = val
	case "grid":
		ws.Grid, ws.GridSet = val, true
	default:
		return fmt.Errorf("%s:%d: unknown key %q in a workspace block (want server-cid, ws-path, repo or grid)", configName, lineNo, key)
	}
	return nil
}

// Workspace returns the named workspace.
func (f *File) Workspace(name string) (*Workspace, bool) {
	if f == nil {
		return nil, false
	}
	for _, w := range f.ws {
		if w.Name == name {
			return w, true
		}
	}
	return nil, false
}

// Names lists the workspaces in file order.
func (f *File) Names() []string {
	if f == nil {
		return nil
	}
	out := make([]string, 0, len(f.ws))
	for _, w := range f.ws {
		out = append(out, w.Name)
	}
	return out
}

// Render returns the file's bytes. Lines this package never modified come back
// exactly as they were read, comments included.
func (f *File) Render() []byte {
	if f == nil || len(f.lines) == 0 {
		return nil
	}
	return []byte(strings.Join(f.lines, "\n") + "\n")
}
```

Note on `Render`: `Parse` drops the trailing newline of the last line (bufio.Scanner strips it) and `Render` adds exactly one back. A source file that does not end in a newline will therefore gain one; that is intended and the round-trip test's fixture ends in a newline.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./cli/workspace/ -v`
Expected: PASS for `TestParseRenderRoundTrip`, `TestParseValues`, `TestParseDefaults`, `TestParseErrors`.

- [ ] **Step 5: Commit**

```bash
git add cli/workspace/config.go cli/workspace/config_test.go
git commit -m "feat(workspace): line-preserving parser for .harness/config"
```

---

### Task 2: Locating the file, and refusing to read it inside an agent

**Files:**
- Create: `cli/workspace/locate.go`
- Test: `cli/workspace/locate_test.go`

**Interfaces:**
- Consumes: `workspace.Parse` from Task 1.
- Produces: `workspace.Load(flagPath string) (*File, string, error)` and its testable core `workspace.LoadFrom(flagPath, envPath, defaultPath string, inAgent bool) (*File, string, error)`. Both return `(nil, "", nil)` when no config applies.

- [ ] **Step 1: Write the failing test**

```go
package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCfg(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "config")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadFrom(t *testing.T) {
	dir := t.TempDir()
	good := writeCfg(t, dir, "[workspace default]\nrepo = /x\n")

	t.Run("flag wins over env and default", func(t *testing.T) {
		other := filepath.Join(t.TempDir(), "other")
		if err := os.WriteFile(other, []byte("[workspace fromenv]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		f, path, err := LoadFrom(good, other, other, false)
		if err != nil || f == nil {
			t.Fatalf("LoadFrom: f=%v err=%v", f, err)
		}
		if path != good {
			t.Errorf("path = %q, want the --config value %q", path, good)
		}
		if _, ok := f.Workspace("default"); !ok {
			t.Error("loaded the wrong file")
		}
	})

	t.Run("explicit missing path is an error", func(t *testing.T) {
		missing := filepath.Join(dir, "nope")
		if _, _, err := LoadFrom(missing, "", "", false); err == nil {
			t.Error("LoadFrom(--config <missing>) succeeded, want an error")
		}
		if _, _, err := LoadFrom("", missing, "", false); err == nil {
			t.Error("LoadFrom(HARNESS_CONFIG=<missing>) succeeded, want an error")
		}
	})

	t.Run("missing default path is silent", func(t *testing.T) {
		f, path, err := LoadFrom("", "", filepath.Join(dir, "absent"), false)
		if err != nil || f != nil || path != "" {
			t.Errorf("LoadFrom(default absent) = %v, %q, %v; want nil, \"\", nil", f, path, err)
		}
	})

	t.Run("in an agent nothing is read", func(t *testing.T) {
		f, path, err := LoadFrom(good, good, good, true)
		if err != nil || f != nil || path != "" {
			t.Errorf("LoadFrom(inAgent) = %v, %q, %v; want nil, \"\", nil", f, path, err)
		}
	})
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./cli/workspace/ -run TestLoadFrom -v`
Expected: FAIL — `undefined: LoadFrom`.

- [ ] **Step 3: Write `cli/workspace/locate.go`**

```go
package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// DefaultPath is the config the clients look for when neither --config nor
// HARNESS_CONFIG names one: .harness/config under the current directory. The
// search does not walk up to parent directories — a client started somewhere
// else names its file explicitly.
const DefaultPath = ".harness/config"

// Load resolves and parses the config for a client process. The returned path
// is where it came from, for a status line. A nil *File with a nil error means
// no workspace applies, which is the ordinary case.
func Load(flagPath string) (*File, string, error) {
	return LoadFrom(flagPath, os.Getenv("HARNESS_CONFIG"), filepath.FromSlash(DefaultPath),
		os.Getenv("HARNESS_AUTH_TICKET") != "")
}

// LoadFrom is Load with its three sources and the agent test injected, so the
// resolution order is testable without touching the process environment or the
// working directory.
//
// inAgent suppresses everything. An in-task agent has HARNESS_AUTH_TICKET set,
// and scripts/sandbox/agent-in-podman.sh forwards environment into the
// container BY PREFIX, so a HARNESS_CONFIG left in a runner's environment
// otherwise reaches every sandboxed agent — pointing at a path that in the
// container either does not exist or is a different operator's file.
func LoadFrom(flagPath, envPath, defaultPath string, inAgent bool) (*File, string, error) {
	if inAgent {
		return nil, "", nil
	}
	switch {
	case flagPath != "":
		f, err := parseFile(flagPath)
		return f, flagPath, err
	case envPath != "":
		f, err := parseFile(envPath)
		return f, envPath, err
	case defaultPath != "":
		f, err := parseFile(defaultPath)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, "", nil // running without a workspace is normal
		}
		return f, defaultPath, err
	}
	return nil, "", nil
}

func parseFile(path string) (*File, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	f, err := Parse(fh)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}
```

`parseFile`'s `os.Open` error is returned unwrapped so the default-path branch can test it with `errors.Is(err, fs.ErrNotExist)`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./cli/workspace/ -v`
Expected: PASS, including Task 1's tests.

- [ ] **Step 5: Commit**

```bash
git add cli/workspace/locate.go cli/workspace/locate_test.go
git commit -m "feat(workspace): resolve --config / HARNESS_CONFIG / .harness/config, never inside an agent"
```

---

### Task 3: One grammar, one parser — extract `ParseGridArgs`, validate values at parse time

The config must reject a `forward` or `grid` value the command line would reject, and it must do so without a second copy of either grammar. `cli.ParseForwardSpec` and `cli.ParseRemoteForwardSpec` already live in `cli`; the grid argument parser does not — it is `parseGrid` inside `tui`, which `cli/workspace` cannot import.

**Files:**
- Create: `cli/gridargs.go`
- Create: `cli/workspace/validate.go`
- Modify: `tui/cmdline.go:1316-1360` (`parseGrid` becomes a wrapper)
- Test: `cli/gridargs_test.go`, `cli/workspace/validate_test.go`

**Interfaces:**
- Consumes: `workspace.Workspace` / `workspace.Task` from Task 1.
- Produces: `cli.ParseGridArgs(args []string) (GridScopeMode, string, []string, error)`; `workspace.ForwardDir` with `ForwardLocal`/`ForwardRemote`; `workspace.ParseForwardValue(value string) (ForwardDir, cli.ForwardSpec, cli.RemoteForwardSpec, error)`; `(*Workspace).Validate() error`.

- [ ] **Step 1: Write the failing tests**

```go
// cli/gridargs_test.go
package cli

import "testing"

func TestParseGridArgs(t *testing.T) {
	const id = "3f2a9c00000000000000000000000001"
	mode, anchor, ids, err := ParseGridArgs(nil)
	if err != nil || mode != GridAll || anchor != "" || len(ids) != 0 {
		t.Errorf("no args = %v %q %v %v; want GridAll", mode, anchor, ids, err)
	}
	mode, anchor, _, err = ParseGridArgs([]string{"--under", id})
	if err != nil || mode != GridSubtree || anchor != id {
		t.Errorf("--under = %v %q %v; want GridSubtree/%s", mode, anchor, err, id)
	}
	mode, _, _, err = ParseGridArgs([]string{"--under", id, "--descendants"})
	if err != nil || mode != GridDescendants {
		t.Errorf("--under --descendants = %v %v; want GridDescendants", mode, err)
	}
	mode, _, ids, err = ParseGridArgs([]string{id})
	if err != nil || mode != GridIds || len(ids) != 1 {
		t.Errorf("bare id = %v %v %v; want GridIds", mode, ids, err)
	}
	if _, _, _, err := ParseGridArgs([]string{"--under"}); err == nil {
		t.Error("--under with no id succeeded, want an error")
	}
	if _, _, _, err := ParseGridArgs([]string{"--under", id, id}); err == nil {
		t.Error("--under plus a bare id succeeded, want an error")
	}
	if _, _, _, err := ParseGridArgs([]string{"--nope"}); err == nil {
		t.Error("unknown flag succeeded, want an error")
	}
}
```

```go
// cli/workspace/validate_test.go
package workspace

import (
	"strings"
	"testing"
)

func TestParseForwardValue(t *testing.T) {
	dir, l, _, err := ParseForwardValue("-L 3000:127.0.0.1:3000")
	if err != nil || dir != ForwardLocal || l.LocalPort != 3000 || l.RemotePort != 3000 {
		t.Errorf("-L = %v %+v %v", dir, l, err)
	}
	dir, _, r, err := ParseForwardValue("-R 8080:127.0.0.1:9090")
	if err != nil || dir != ForwardRemote || r.RunnerPort != 8080 || r.DialPort != 9090 {
		t.Errorf("-R = %v %+v %v", dir, r, err)
	}
	for _, bad := range []string{"-W host:port", "3000:127.0.0.1:3000", "-L", "-L not-a-spec"} {
		if _, _, _, err := ParseForwardValue(bad); err == nil {
			t.Errorf("ParseForwardValue(%q) succeeded, want an error", bad)
		}
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	for name, src := range map[string]string{
		"bad forward": "[workspace w]\n[workspace w task 3f2a9c00000000000000000000000001]\nforward = -L nonsense\n",
		"bad grid":    "[workspace w]\ngrid = --nope\n",
	} {
		f, err := Parse(strings.NewReader(src))
		if err != nil {
			t.Fatalf("%s: Parse: %v", name, err)
		}
		ws, _ := f.Workspace("w")
		if err := ws.Validate(); err == nil {
			t.Errorf("%s: Validate succeeded, want an error", name)
		}
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./cli/ -run TestParseGridArgs -v && go test ./cli/workspace/ -run 'TestParseForwardValue|TestValidate' -v`
Expected: FAIL — `undefined: ParseGridArgs`, `undefined: ParseForwardValue`.

- [ ] **Step 3: Move the grid grammar into `cli` and make the TUI call it**

Create `cli/gridargs.go` with the body currently inside `tui.parseGrid` (`tui/cmdline.go:1316-1360`), returning the three values instead of a `GridAction`:

```go
package cli

import (
	"fmt"
	"strings"
)

// GridArgsUsage is the one-line usage shared by every surface that accepts the
// grid argument grammar.
const GridArgsUsage = "grid: usage: grid [<task-id>...] | grid --under <task-id> [--descendants]"

// ParseGridArgs parses the `grid` command's arguments into the three values
// GridSet consumes. It lives here rather than in the TUI because the workspace
// config accepts the same grammar and must not carry a second copy of it: a
// mirror has no way to fail loudly when the grammar grows.
func ParseGridArgs(args []string) (GridScopeMode, string, []string, error) {
	mode, anchor, descendants := GridAll, "", false
	var ids []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--descendants":
			descendants = true
		case "--under":
			if i+1 >= len(args) {
				return mode, "", nil, fmt.Errorf("grid: --under needs a task id")
			}
			i++
			anchor = args[i]
		default:
			if strings.HasPrefix(args[i], "-") {
				return mode, "", nil, fmt.Errorf("grid: unknown flag %q\n%s", args[i], GridArgsUsage)
			}
			ids = append(ids, args[i])
		}
	}
	switch {
	case anchor != "":
		if len(ids) > 0 {
			return mode, "", nil, fmt.Errorf("grid: --under names one subtree; drop the extra ids\n%s", GridArgsUsage)
		}
		mode = GridSubtree
		if descendants {
			mode = GridDescendants
		}
	case len(ids) > 0:
		if descendants {
			return mode, "", nil, fmt.Errorf("grid: --descendants needs --under\n%s", GridArgsUsage)
		}
		mode = GridIds
	case descendants:
		return mode, "", nil, fmt.Errorf("grid: --descendants needs --under\n%s", GridArgsUsage)
	}
	return mode, anchor, ids, nil
}
```

Before writing this, READ `tui/cmdline.go:1316` to the end of `parseGrid` and carry over every branch it has, including any this skeleton does not show. The tail cases above are the ones the tests pin; if the original rejects or accepts something else, the original wins and the test gets a case.

Then replace the body of `tui.parseGrid` with:

```go
func parseGrid(args []string) (Action, error) {
	mode, anchor, ids, err := cli.ParseGridArgs(args)
	if err != nil {
		return nil, err
	}
	return GridAction{Mode: mode, Anchor: anchor, IDs: ids}, nil
}
```

- [ ] **Step 4: Write `cli/workspace/validate.go`**

```go
package workspace

import (
	"fmt"
	"strings"

	"github.com/google/shlex"
	"github.com/on-keyday/agent-harness/cli"
)

// ForwardDir distinguishes the two forward flags a workspace may carry. -W is
// deliberately absent: it splices one process's stdin/stdout and has no
// meaning in a file that describes long-lived listeners.
type ForwardDir int

const (
	ForwardLocal ForwardDir = iota
	ForwardRemote
)

// ParseForwardValue splits a `forward` value into its flag and its spec, and
// hands the spec to the same parser `harness-cli forward` uses.
func ParseForwardValue(value string) (ForwardDir, cli.ForwardSpec, cli.RemoteForwardSpec, error) {
	flag, rest, ok := strings.Cut(strings.TrimSpace(value), " ")
	rest = strings.TrimSpace(rest)
	if !ok || rest == "" {
		return 0, cli.ForwardSpec{}, cli.RemoteForwardSpec{},
			fmt.Errorf("forward = %q: want `-L [bind:]localport:remotehost:remoteport` or `-R [bind:]runnerport:dialhost:dialport`", value)
	}
	switch flag {
	case "-L":
		sp, err := cli.ParseForwardSpec(rest)
		return ForwardLocal, sp, cli.RemoteForwardSpec{}, err
	case "-R":
		sp, err := cli.ParseRemoteForwardSpec(rest)
		return ForwardRemote, cli.ForwardSpec{}, sp, err
	}
	return 0, cli.ForwardSpec{}, cli.RemoteForwardSpec{},
		fmt.Errorf("forward = %q: only -L and -R are accepted here", value)
}

// GridArgs splits the workspace's grid value the way a shell would, so the
// value is written exactly as it is typed after `grid`.
func (w *Workspace) GridArgs() ([]string, error) {
	if !w.GridSet || strings.TrimSpace(w.Grid) == "" {
		return nil, nil
	}
	return shlex.Split(w.Grid)
}

// Validate checks every value this workspace carries against the parser that
// owns its grammar. Callers run it once after Parse; an apply then works from
// values already known to be well formed.
func (w *Workspace) Validate() error {
	if w.GridSet {
		args, err := w.GridArgs()
		if err != nil {
			return fmt.Errorf("workspace %s: grid: %w", w.Name, err)
		}
		if _, _, _, err := cli.ParseGridArgs(args); err != nil {
			return fmt.Errorf("workspace %s: %w", w.Name, err)
		}
	}
	for _, t := range w.Tasks {
		for _, fw := range t.Forwards {
			if _, _, _, err := ParseForwardValue(fw); err != nil {
				return fmt.Errorf("workspace %s task %s: %w", w.Name, t.ID[:8], err)
			}
		}
	}
	return nil
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./cli/... ./tui/ -run 'Grid|Forward|Validate' -v`
Expected: PASS, and the pre-existing `tui` grid cmdline tests still pass — they exercise the wrapper.

- [ ] **Step 6: Commit**

```bash
git add cli/gridargs.go cli/gridargs_test.go cli/workspace/validate.go cli/workspace/validate_test.go tui/cmdline.go
git commit -m "refactor(cli): own the grid argument grammar so the workspace config can reuse it"
```

---

### Task 4: Writing one workspace back without disturbing the rest of the file

**Files:**
- Create: `cli/workspace/save.go`
- Test: `cli/workspace/save_test.go`

**Interfaces:**
- Consumes: `File`, `Workspace`, `Task` from Task 1.
- Produces: `workspace.New() *File`, `(*File).Set(ws *Workspace)`, `(*File).Save(path string) error`.

- [ ] **Step 1: Write the failing test**

```go
package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetPreservesEveryOtherLine(t *testing.T) {
	src := `# a comment nobody should lose

[workspace default]
repo = /old

[workspace phone]
repo    = /keep/me
ws-path = /ws
`
	f, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	f.Set(&Workspace{
		Name: "default",
		Repo: "/new",
		Tasks: []Task{{
			ID: "3f2a9c00000000000000000000000001", Resume: ResumeContinue,
			Runner: RunnerAssigned, Forwards: []string{"-L 3000:127.0.0.1:3000"},
		}},
	})
	out := string(f.Render())

	if !strings.Contains(out, "# a comment nobody should lose") {
		t.Error("the leading comment was lost")
	}
	if !strings.Contains(out, "repo    = /keep/me") {
		t.Error("the untouched workspace's own spacing was rewritten")
	}
	if strings.Contains(out, "/old") {
		t.Error("the replaced workspace's old value survived")
	}

	reparsed, err := Parse(strings.NewReader(out))
	if err != nil {
		t.Fatalf("the rendered file does not parse: %v", err)
	}
	ws, ok := reparsed.Workspace("default")
	if !ok || ws.Repo != "/new" || len(ws.Tasks) != 1 || ws.Tasks[0].Forwards[0] != "-L 3000:127.0.0.1:3000" {
		t.Errorf("round-trip lost the written workspace: %+v", ws)
	}
	if phone, ok := reparsed.Workspace("phone"); !ok || phone.Repo != "/keep/me" {
		t.Errorf("round-trip lost the other workspace: %+v", phone)
	}
}

func TestSetAppendsWhenAbsent(t *testing.T) {
	f := New()
	f.Set(&Workspace{Name: "default", ServerCID: "ws:example.invalid:8539-*"})
	reparsed, err := Parse(strings.NewReader(string(f.Render())))
	if err != nil {
		t.Fatalf("Parse of a fresh file: %v", err)
	}
	if _, ok := reparsed.Workspace("default"); !ok {
		t.Error("Set on an empty file did not add the workspace")
	}
}

func TestSaveCreatesParentDir(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".harness", "config")
	f := New()
	f.Set(&Workspace{Name: "default"})
	if err := f.Save(p); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("Save did not create %s: %v", p, err)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./cli/workspace/ -run 'TestSet|TestSave' -v`
Expected: FAIL — `undefined: New`, `undefined: Set`.

- [ ] **Step 3: Write `cli/workspace/save.go`**

```go
package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// New returns an empty file, for a first `workspace save`.
func New() *File { return &File{} }

// Set replaces the named workspace's lines with a rendering of ws, leaving
// every other line — including comments and whatever spacing the operator
// chose — byte-identical. An unknown name is appended at the end.
//
// Replacing a line SPAN rather than rewriting the whole file is why this
// format is line-oriented: a structured encoder would have to reproduce the
// operator's own text, and would not.
func (f *File) Set(ws *Workspace) {
	block := renderWorkspace(ws)
	old, exists := f.Workspace(ws.Name)
	if !exists {
		if len(f.lines) > 0 && strings.TrimSpace(f.lines[len(f.lines)-1]) != "" {
			f.lines = append(f.lines, "")
		}
		start := len(f.lines)
		f.lines = append(f.lines, block...)
		clone := *ws
		clone.start, clone.end = start, len(f.lines)
		f.ws = append(f.ws, &clone)
		return
	}
	delta := len(block) - (old.end - old.start)
	tail := append([]string(nil), f.lines[old.end:]...)
	f.lines = append(append(f.lines[:old.start], block...), tail...)
	for _, w := range f.ws {
		if w.start > old.start {
			w.start += delta
			w.end += delta
		}
	}
	clone := *ws
	clone.start, clone.end = old.start, old.start+len(block)
	*old = clone
}

// renderWorkspace emits the block for one workspace: its own keys, then one
// task block per task. Keys whose value is empty are omitted — an empty
// `repo =` line would parse back as an explicit empty repo, which is not the
// same as not having written one.
func renderWorkspace(ws *Workspace) []string {
	out := []string{fmt.Sprintf("[workspace %s]", ws.Name)}
	for _, kv := range [][2]string{
		{"server-cid", ws.ServerCID},
		{"ws-path", ws.WSPath},
		{"repo", ws.Repo},
	} {
		if kv[1] != "" {
			out = append(out, fmt.Sprintf("%-10s = %s", kv[0], kv[1]))
		}
	}
	if ws.GridSet {
		out = append(out, fmt.Sprintf("%-10s = %s", "grid", ws.Grid))
	}
	for _, t := range ws.Tasks {
		out = append(out, "", fmt.Sprintf("[workspace %s task %s]", ws.Name, t.ID))
		resume := t.Resume
		if resume == "" {
			resume = ResumeNo
		}
		runner := t.Runner
		if runner == "" {
			runner = RunnerAssigned
		}
		out = append(out,
			fmt.Sprintf("%-8s = %s", "resume", resume),
			fmt.Sprintf("%-8s = %s", "runner", runner))
		for _, fw := range t.Forwards {
			out = append(out, fmt.Sprintf("%-8s = %s", "forward", fw))
		}
	}
	return out
}

// Save writes the file, creating the parent directory. 0o600: a workspace
// carries a server address and repository paths, not secrets, but it is the
// operator's own file and there is no reason for it to be group-readable.
func (f *File) Save(path string) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return os.WriteFile(path, f.Render(), 0o600)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./cli/workspace/ -v`
Expected: PASS, all of Tasks 1–4.

- [ ] **Step 5: Commit**

```bash
git add cli/workspace/save.go cli/workspace/save_test.go
git commit -m "feat(workspace): rewrite one workspace's lines and leave the rest byte-identical"
```

---

### Task 5: Resuming a task without taking the operator's screen

`DoResumeSession` (`tui/interactive.go:118`) resumes pinned to `AssignedTo` and retries once with the Any selector when the pin fails with `cli.ErrPinnedNotFound` — the case where the runner restarted with a new RunnerID. It then ATTACHES. `DoStartDetachedSession` (`tui/interactive.go:269`) does not attach but has no such retry. A workspace apply needs both halves, and the retry matters most in the reconnect after a server restart, which is when runners have often restarted too.

**Files:**
- Modify: `tui/interactive.go` (extract the pinned-retry open; add the detached resume)
- Test: `tui/interactive_workspace_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `tui.DoResumeSessionDetached(c *cli.Client, assignedTo protocol.RunnerID, unpinned bool, resumeTaskID string, auth Authority, resumeConversation bool, agentProfile string, size TermSize) tea.Cmd`, returning `SessionStartedMsg`.

- [ ] **Step 1: Read the two existing functions in full**

Read `tui/interactive.go` from `resumeSelectorOpts` (around line 100) through the end of `DoStartDetachedSession`. The extraction in Step 3 must preserve every argument both current callers pass; `DoResumeSession`'s behaviour must not change.

- [ ] **Step 2: Write the failing test**

```go
package tui

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// A workspace with runner = any must not pin, and with runner = assigned must
// pin to the task's own runner. The selector choice is the whole difference
// between the two values, so it is what this pins.
func TestResumeSelectorOptsForWorkspace(t *testing.T) {
	var rid protocol.RunnerID
	rid.SetTransport([]byte("ws"))
	rid.SetIpAddr([]byte{127, 0, 0, 1})
	rid.Port = 8540
	rid.UniqueNumber = 7

	if got := workspaceResumeOpts(rid, false); got.Runner == "" {
		t.Error("assigned: want a pinned selector, got the Any selector")
	}
	if got := workspaceResumeOpts(rid, true); got.Runner != "" {
		t.Errorf("any: want the Any selector, got Runner=%q", got.Runner)
	}
}
```

- [ ] **Step 3: Run it to verify it fails**

Run: `go test ./tui/ -run TestResumeSelectorOptsForWorkspace -v`
Expected: FAIL — `undefined: workspaceResumeOpts`.

- [ ] **Step 4: Implement**

Add to `tui/interactive.go`:

```go
// workspaceResumeOpts picks the selector for a workspace resume: the task's own
// runner for `runner = assigned`, the Any selector for `runner = any`. Same
// split as r/R versus u/U.
func workspaceResumeOpts(assignedTo protocol.RunnerID, unpinned bool) cli.SelectorOpts {
	if unpinned {
		return cli.SelectorOpts{}
	}
	return resumeSelectorOpts(assignedTo)
}

// openResumeWithRetry opens a resumed session with opts, and retries once with
// the Any selector when a pinned attempt reports the runner is gone. Extracted
// from DoResumeSession so the attaching and the detached resume paths cannot
// drift: the retry is the part that is easy to omit and expensive to miss,
// because a runner that restarted has a new RunnerID and the pin then always
// fails.
func openResumeWithRetry(c *cli.Client, opts cli.SelectorOpts, req sessionRequest, auth Authority) (trsf.BidirectionalStream, string, error) {
	sel, err := cli.BuildSelector(opts)
	if err != nil {
		return nil, "", fmt.Errorf("selector: %w", err)
	}
	req.Selector = sel
	stream, taskID, err := c.OpenInteractive(context.Background(), "", auth.opts(req))
	if opts.Runner != "" && errors.Is(err, cli.ErrPinnedNotFound) {
		sel, err = cli.BuildSelector(cli.SelectorOpts{})
		if err != nil {
			return nil, "", fmt.Errorf("selector: %w", err)
		}
		req.Selector = sel
		stream, taskID, err = c.OpenInteractive(context.Background(), "", auth.opts(req))
	}
	return stream, taskID, err
}

// DoResumeSessionDetached resumes a terminal task into a detachable session and
// closes the local stream immediately, so nothing takes over the operator's
// terminal. This is what a workspace apply uses: the screen is restored by the
// grid, not by a handover.
func DoResumeSessionDetached(c *cli.Client, assignedTo protocol.RunnerID, unpinned bool, resumeTaskID string, auth Authority, resumeConversation bool, agentProfile string, size TermSize) tea.Cmd {
	return func() tea.Msg {
		stream, taskID, err := openResumeWithRetry(c, workspaceResumeOpts(assignedTo, unpinned), sessionRequest{
			ResumeTaskID:       resumeTaskID,
			ResumeConversation: resumeConversation,
			AgentProfile:       agentProfile,
			// Both must be non-zero to take effect (see the field comments on
			// sessionRequest). Nobody attaches to this session, so the TUI's own
			// terminal is the only size proxy available — the same reasoning
			// DoStartDetachedSession records.
			InitialRows: size.Rows,
			InitialCols: size.Cols,
		}, auth)
		if err != nil {
			return SessionStartedMsg{Err: err}
		}
		// Nobody attaches to this session. Closing our end here is what makes
		// it detached rather than a handover with no renderer.
		_ = stream.CloseBoth()
		return SessionStartedMsg{TaskID: taskID}
	}
}
```

Then rewrite `DoResumeSession`'s body to call `openResumeWithRetry` with the same `sessionRequest` it builds today, so exactly one copy of the retry exists.

Do NOT hand-build a `cli.SessionOpts` anywhere in this task. `auth.opts(sessionRequest{…})` is the single funnel, and `TestSessionOptsIsBuiltInOnePlace` (`tui/client.go:111`) fails when a second literal appears in this package.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./tui/ -run 'Resume|Interactive' -v`
Expected: PASS, including the pre-existing resume tests.

- [ ] **Step 6: Commit**

```bash
git add tui/interactive.go tui/interactive_workspace_test.go
git commit -m "feat(tui): resume a task detached, keeping the pinned-runner retry"
```

---

### Task 6: Reconciling forwards

**Files:**
- Modify: `tui/portforward.go` (add `FromWorkspace` to `PortForwardSession` and to `PortForwardStartedMsg`)
- Modify: `tui/app.go:1036-1040` (carry the flag into the session it constructs)
- Create: `tui/workspace_reconcile.go`
- Test: `tui/workspace_reconcile_test.go`

**Interfaces:**
- Consumes: `workspace.Task` from Task 1.
- Produces: `tui.planForwards(declared []string, active map[int]*PortForwardSession, taskID string) forwardPlan`, where `forwardPlan` has `Start []string` and `Stop []*PortForwardSession`.

- [ ] **Step 1: Write the failing test**

```go
package tui

import "testing"

func TestPlanForwards(t *testing.T) {
	const id = "3f2a9c00000000000000000000000001"
	const other = "7b1e000000000000000000000000000f"

	active := map[int]*PortForwardSession{
		1: {ID: 1, TaskID: id, Spec: "3000:127.0.0.1:3000", Direction: ForwardLocal, FromWorkspace: true},
		2: {ID: 2, TaskID: id, Spec: "5432:127.0.0.1:5432", Direction: ForwardLocal, FromWorkspace: true},
		3: {ID: 3, TaskID: id, Spec: "9999:127.0.0.1:9999", Direction: ForwardLocal},       // hand-started
		4: {ID: 4, TaskID: other, Spec: "3000:127.0.0.1:3000", Direction: ForwardLocal, FromWorkspace: true},
	}
	declared := []string{
		"-L 3000:127.0.0.1:3000", // already running, workspace-owned → leave alone
		"-R 8080:127.0.0.1:8080", // not running → start
	}

	plan := planForwards(declared, active, id)

	if len(plan.Start) != 1 || plan.Start[0] != "-R 8080:127.0.0.1:8080" {
		t.Errorf("Start = %q, want only the -R", plan.Start)
	}
	if len(plan.Stop) != 1 || plan.Stop[0].ID != 2 {
		var got []int
		for _, s := range plan.Stop {
			got = append(got, s.ID)
		}
		t.Errorf("Stop = %v, want only session 2 (workspace-owned, no longer declared)", got)
	}
}

func TestPlanForwardsOnAnEmptyActiveSetStartsAll(t *testing.T) {
	const id = "3f2a9c00000000000000000000000001"
	declared := []string{"-L 3000:127.0.0.1:3000", "-R 8080:127.0.0.1:8080"}
	plan := planForwards(declared, map[int]*PortForwardSession{}, id)
	if len(plan.Start) != 2 || len(plan.Stop) != 0 {
		t.Errorf("reconnect case: Start=%q Stop=%d, want both started and nothing stopped", plan.Start, len(plan.Stop))
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./tui/ -run TestPlanForwards -v`
Expected: FAIL — `undefined: planForwards` and `FromWorkspace` is not a field.

- [ ] **Step 3: Add the ownership flag**

In `tui/portforward.go`, add to `PortForwardSession`:

```go
	// FromWorkspace marks a forward started by a workspace apply. It is what
	// reconciliation acts on: an apply may stop only forwards it owns, so a
	// forward the operator started by hand is never taken away. Client-local
	// bookkeeping — the server-side registry has no notion of a workspace, and
	// giving it one would mean a wire field.
	FromWorkspace bool
```

Add the same field to `PortForwardStartedMsg`, and in `tui/app.go`'s `case PortForwardStartedMsg:` copy it into the constructed `PortForwardSession`. That construction is the ONE place a `PortForwardSession` is built — verify with `grep -c 'PortForwardSession{' tui/*.go` before and after, and if the count is above one, set the field at every site.

- [ ] **Step 4: Write `tui/workspace_reconcile.go`**

```go
package tui

import "strings"

// forwardPlan is the difference between what a workspace declares and what is
// already running for one task.
type forwardPlan struct {
	Start []string               // declared values with nothing running
	Stop  []*PortForwardSession  // workspace-owned sessions no longer declared
}

// planForwards computes that difference. Reconciling rather than restarting is
// what makes `workspace apply` usable as the recovery from a port conflict: the
// forwards that ARE working must survive the apply that retries the one that is
// not.
//
// Matching is on (direction, spec) because that pair is what a session records:
// PortForwardSession.Spec holds the spec without its flag, and Direction holds
// the flag.
func planForwards(declared []string, active map[int]*PortForwardSession, taskID string) forwardPlan {
	type key struct {
		dir  ForwardDirection
		spec string
	}
	want := make(map[key]bool, len(declared))
	order := make([]string, 0, len(declared))
	for _, value := range declared {
		flag, spec, ok := strings.Cut(strings.TrimSpace(value), " ")
		if !ok {
			continue // Validate rejected this before the apply ran
		}
		dir := ForwardLocal
		if flag == "-R" {
			dir = ForwardRemote
		}
		want[key{dir, strings.TrimSpace(spec)}] = true
		order = append(order, value)
	}

	running := make(map[key]bool, len(active))
	var plan forwardPlan
	for _, s := range active {
		if s.TaskID != taskID || !s.FromWorkspace {
			continue // another task's, or the operator's own — not ours to touch
		}
		k := key{s.Direction, s.Spec}
		if !want[k] {
			plan.Stop = append(plan.Stop, s)
			continue
		}
		running[k] = true
	}
	for _, value := range order {
		flag, spec, _ := strings.Cut(strings.TrimSpace(value), " ")
		dir := ForwardLocal
		if flag == "-R" {
			dir = ForwardRemote
		}
		if !running[key{dir, strings.TrimSpace(spec)}] {
			plan.Start = append(plan.Start, value)
		}
	}
	return plan
}
```

Before writing this, confirm what `PortForwardSession.Spec` actually holds by reading `DoStartPortForward` (`tui/portforward.go:260`): it passes `spec` — the value WITHOUT the `-L`/`-R` flag — into `PortForwardStartedMsg`. If it holds the flag too, drop the `strings.Cut` on the session side and compare whole values.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./tui/ -run TestPlanForwards -v`
Expected: PASS for both.

- [ ] **Step 6: Commit**

```bash
git add tui/portforward.go tui/app.go tui/workspace_reconcile.go tui/workspace_reconcile_test.go
git commit -m "feat(tui): reconcile workspace-owned forwards instead of restarting them"
```

---

### Task 7: The apply routine, on start, on reconnect, and on demand

**Files:**
- Create: `tui/workspace.go`
- Modify: `tui/app.go` — the `SubscribedMsg` handler (around `:610`), the `SnapshotMsg` handler, and the App struct
- Test: `tui/workspace_test.go`

**Interfaces:**
- Consumes: `workspace.Workspace`, `workspace.ParseForwardValue`, `(*Workspace).GridArgs` (Tasks 1–3); `tui.DoResumeSessionDetached` (Task 5); `tui.planForwards` (Task 6).
- Produces: `(*App).SetWorkspace(ws *workspace.Workspace)`, `(*App).applyWorkspace() tea.Cmd`, and the App fields `workspace *workspace.Workspace` and `workspaceArmed bool`.

- [ ] **Step 1: Write the failing test**

```go
package tui

import (
	"testing"

	"github.com/on-keyday/agent-harness/cli/workspace"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// A workspace resume must fire only for a task in a terminal state. A live task
// is left alone, which is why a plain network blip cannot spawn anything: the
// task is still Running when the reconnect's snapshot arrives.
func TestWorkspaceResumeOnlyForTerminalTasks(t *testing.T) {
	cases := []struct {
		status protocol.TaskStatus
		want   bool
	}{
		{protocol.TaskStatus_Running, false},
		{protocol.TaskStatus_Detached, false},
		{protocol.TaskStatus_Queued, false},
		{protocol.TaskStatus_Failed, true},
		{protocol.TaskStatus_Succeeded, true},
		{protocol.TaskStatus_Cancelled, true},
	}
	for _, c := range cases {
		ti := protocol.TaskInfo{Status: c.status, Kind: protocol.TaskKind_Interactive}
		if got := workspaceWantsResume(&ti, workspace.Task{Resume: workspace.ResumeContinue}); got != c.want {
			t.Errorf("status %v: wantsResume = %v, want %v", c.status, got, c.want)
		}
	}
	ti := protocol.TaskInfo{Status: protocol.TaskStatus_Failed, Kind: protocol.TaskKind_Interactive}
	if workspaceWantsResume(&ti, workspace.Task{Resume: workspace.ResumeNo}) {
		t.Error("resume = no still resumed a terminal task")
	}
	if workspaceWantsResume(nil, workspace.Task{Resume: workspace.ResumeContinue}) {
		t.Error("a task absent from the snapshot was resumed")
	}
}
```

Check `tui/taskaction.go`'s `taskSessionAlive` and the `protocol.TaskStatus_*` spellings before writing this; use the constants that exist rather than the ones sketched here.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./tui/ -run TestWorkspaceResumeOnlyForTerminalTasks -v`
Expected: FAIL — `undefined: workspaceWantsResume`.

- [ ] **Step 3: Write `tui/workspace.go`**

```go
package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/workspace"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// SetWorkspace installs the workspace an apply works from. Nil disables every
// apply path.
func (a *App) SetWorkspace(ws *workspace.Workspace) { a.workspace = ws }

// ArmWorkspace asks for one apply on the next snapshot. The snapshot is where
// an apply must run — deciding whether a task needs resuming requires its
// status — and arming is how the three entry points (first join, rejoin, and
// the `workspace apply` command) reach that one place.
func (a *App) ArmWorkspace() { a.workspaceArmed = true }

// workspaceWantsResume reports whether this task, as the snapshot shows it,
// is one the workspace should bring back. A task that is alive is left alone,
// which is what keeps a reconnect after a network blip from spawning anything.
func workspaceWantsResume(t *protocol.TaskInfo, decl workspace.Task) bool {
	if t == nil || decl.Resume == workspace.ResumeNo || decl.Resume == "" {
		return false
	}
	switch t.Status {
	case protocol.TaskStatus_Succeeded, protocol.TaskStatus_Failed, protocol.TaskStatus_Cancelled:
		return true
	}
	return false
}

// applyWorkspace reconciles the live client to the installed workspace. Every
// step reports its own outcome and none aborts the others: a port already bound
// must not cost the operator the resume or the grid.
func (a *App) applyWorkspace() tea.Cmd {
	ws := a.workspace
	if ws == nil || a.client == nil {
		return nil
	}
	var cmds []tea.Cmd
	for _, decl := range ws.Tasks {
		// a.tasksByID is map[string]protocol.TaskInfo — VALUES, not pointers
		// (tui/app.go:71). A missing task is the zero TaskInfo, whose Status is
		// Queued, so "absent" has to be carried separately rather than read off
		// the value.
		var info *protocol.TaskInfo
		if t, ok := a.tasksByID[decl.ID]; ok {
			info = &t
		}

		if workspaceWantsResume(info, decl) {
			var assigned protocol.RunnerID
			if info != nil {
				assigned = info.AssignedTo
			}
			var profile string
			if info != nil {
				profile = string(info.AgentProfile)
			}
			a.cmdresult.Append(fmt.Sprintf("workspace %s: resuming %s (%s, %s)",
				ws.Name, pfShortID(decl.ID), decl.Resume, decl.Runner))
			cmds = append(cmds, DoResumeSessionDetached(a.client, assigned,
				decl.Runner == workspace.RunnerAny, decl.ID, a.authority(),
				decl.Resume == workspace.ResumeContinue, profile,
				TermSize{Rows: uint16(a.height), Cols: uint16(a.width)}))
		} else if info == nil && decl.Resume != workspace.ResumeNo && decl.Resume != "" {
			a.cmdresult.Append(WarnStyle.Render(fmt.Sprintf(
				"workspace %s: task %s is not in the visible task set — not resumed",
				ws.Name, pfShortID(decl.ID))))
		}

		plan := planForwards(decl.Forwards, a.activeForwards, decl.ID)
		for _, s := range plan.Stop {
			a.cmdresult.Append(fmt.Sprintf("workspace %s: stopping %s %s on %s (no longer declared)",
				ws.Name, s.Direction.flag(), s.Spec, pfShortID(decl.ID)))
			if s.Cancel != nil {
				s.Cancel()
			}
		}
		for _, value := range plan.Start {
			dir, _, _, err := workspace.ParseForwardValue(value)
			if err != nil {
				a.cmdresult.Append(ErrorStyle.Render(fmt.Sprintf("workspace %s: %v", ws.Name, err)))
				continue
			}
			spec := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(value), "-L"), "-R"))
			id := a.nextForwardID
			a.nextForwardID++
			// One call per spec: cli.RunForward aborts every spec in a call
			// when any one of them fails to listen, so batching would let a
			// conflict on 3000 take 8080 down with it.
			// NOTE two similarly named constant pairs are in play here:
			// workspace.ForwardLocal/ForwardRemote (workspace.ForwardDir, what
			// ParseForwardValue returns) and tui's own
			// ForwardLocal/ForwardRemote (ForwardDirection, what a session
			// records). They are different types; do not mix them.
			if dir == workspace.ForwardRemote {
				cmds = append(cmds, DoStartRemoteForward(a.client, decl.ID, spec, id, a.program, true))
			} else {
				cmds = append(cmds, DoStartPortForward(a.client, decl.ID, spec, id, a.program, true))
			}
		}
	}

	if ws.GridSet {
		args, err := ws.GridArgs()
		if err == nil {
			var mode cli.GridScopeMode
			var anchor string
			var ids []string
			mode, anchor, ids, err = cli.ParseGridArgs(args)
			if err == nil {
				cmds = append(cmds, a.openGrid(mode, anchor, ids))
			}
		}
		if err != nil {
			a.cmdresult.Append(ErrorStyle.Render(fmt.Sprintf("workspace %s: grid: %v", ws.Name, err)))
		}
	}
	return tea.Batch(cmds...)
}
```

`DoStartPortForward` and `DoStartRemoteForward` gain one trailing `fromWorkspace bool` parameter, which they copy onto the `PortForwardStartedMsg` they send. Extend the existing functions — do not add `…Owned` twins. Find every call site with `grep -rn 'DoStartPortForward(\|DoStartRemoteForward(' tui/` (tests included) and pass `false` at each; a partially wired signature change is the failure mode this repository has hit before.

- [ ] **Step 4: Wire the three entry points in `tui/app.go`**

Add the fields to the App struct:

```go
	// workspace is the installed .harness/config workspace, or nil.
	workspace      *workspace.Workspace
	workspaceArmed bool // an apply is due on the next snapshot
```

In the `SubscribedMsg` handler, in the `msg.Topic == topics.TasksStatus()` branch (which already covers BOTH the first join and a resubscribe), arm before returning `RefreshSnapshot(a.client)`:

```go
			if a.workspace != nil {
				a.ArmWorkspace()
			}
```

In the `SnapshotMsg` handler, after the snapshot has been applied to `a.tasksByID` (the apply reads task statuses out of it, so it must run after, not before):

```go
	if a.workspaceArmed {
		a.workspaceArmed = false
		if cmd := a.applyWorkspace(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
```

Read the existing `SnapshotMsg` case first: match how it accumulates and returns commands rather than assuming a `cmds` slice exists.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./tui/ -v 2>&1 | tail -30`
Expected: PASS. `TestOpenInteractiveSessionMux` in `server` is known to be flaky about 1 run in 4 and is unrelated; the `tui` package should be green.

- [ ] **Step 6: Commit**

```bash
git add tui/workspace.go tui/workspace_test.go tui/app.go tui/portforward.go
git commit -m "feat(tui): apply a workspace on join, rejoin and demand through one routine"
```

---

### Task 8: The `workspace` verb in the TUI command line

**Files:**
- Modify: `tui/cmdline.go` (add the `workspace` case and `WorkspaceAction`)
- Modify: `tui/app.go` (dispatch `WorkspaceAction`)
- Test: `tui/cmdline_workspace_test.go`

**Interfaces:**
- Consumes: `(*App).ArmWorkspace`, `(*App).applyWorkspace` (Task 7); `workspace.File` (Tasks 1, 4).
- Produces: `tui.WorkspaceAction{Sub string; Name string}` with `Sub` in `save`, `apply`, `ls`, `show`.

- [ ] **Step 1: Write the failing test**

```go
package tui

import "testing"

func TestParseWorkspace(t *testing.T) {
	for _, c := range []struct {
		in   string
		sub  string
		name string
	}{
		{"workspace apply", "apply", ""},
		{"workspace apply default", "apply", "default"},
		{"workspace save default", "save", "default"},
		{"workspace ls", "ls", ""},
		{"workspace show default", "show", "default"},
	} {
		act, err := ParseCommand(c.in, "")
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		wa, ok := act.(WorkspaceAction)
		if !ok {
			t.Fatalf("%q: got %T, want WorkspaceAction", c.in, act)
		}
		if wa.Sub != c.sub || wa.Name != c.name {
			t.Errorf("%q: got %+v, want sub=%q name=%q", c.in, wa, c.sub, c.name)
		}
	}
	for _, bad := range []string{"workspace", "workspace nope", "workspace save"} {
		if _, err := ParseCommand(bad, ""); err == nil {
			t.Errorf("%q parsed, want an error", bad)
		}
	}
}
```

`workspace save` with no name is an error on purpose: saving is what writes the file, and defaulting the name would let a slip overwrite the wrong workspace.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./tui/ -run TestParseWorkspace -v`
Expected: FAIL — `unknown command: "workspace"`.

- [ ] **Step 3: Add the verb**

In `tui/cmdline.go`, next to the other action types:

```go
// WorkspaceAction is the `workspace <sub> [name]` family: save the current
// client state into .harness/config, re-apply a workspace, or inspect what the
// file holds.
type WorkspaceAction struct {
	Sub  string // "save" | "apply" | "ls" | "show"
	Name string // "" means the installed workspace, except for save
}

func (WorkspaceAction) isAction() {}
```

In `ParseCommand`'s switch, beside `case "forward":`:

```go
	case "workspace":
		return parseWorkspace(tokens[1:])
```

```go
func parseWorkspace(args []string) (Action, error) {
	const usage = "workspace: usage: workspace save <name> | workspace apply [name] | workspace ls | workspace show [name]"
	if len(args) == 0 {
		return nil, fmt.Errorf("%s", usage)
	}
	sub := args[0]
	var name string
	if len(args) > 1 {
		name = args[1]
	}
	switch sub {
	case "save":
		if name == "" {
			return nil, fmt.Errorf("workspace save: needs a name\n%s", usage)
		}
	case "apply", "ls", "show":
	default:
		return nil, fmt.Errorf("workspace: unknown subcommand %q\n%s", sub, usage)
	}
	if len(args) > 2 {
		return nil, fmt.Errorf("workspace %s: too many arguments\n%s", sub, usage)
	}
	return WorkspaceAction{Sub: sub, Name: name}, nil
}
```

Add a row to the TUI's cmdline help listing so the verb is discoverable — find where `forward` is described (`grep -n '"forward"' tui/cmdline.go` and the help text near it) and follow the same shape.

- [ ] **Step 4: Dispatch it in `tui/app.go`**

In the `runAction` switch (find it with `grep -n 'case ForwardAction' tui/app.go` and add beside it):

```go
	case WorkspaceAction:
		return a.runWorkspaceAction(v)
```

Add to `tui/workspace.go`:

```go
// runWorkspaceAction handles the `workspace` verb. save and apply act on the
// live client; ls and show read the file.
func (a *App) runWorkspaceAction(v WorkspaceAction) tea.Cmd {
	switch v.Sub {
	case "apply":
		if v.Name != "" {
			ws, ok := a.workspaceFile.Workspace(v.Name)
			if !ok {
				a.cmdresult.Append(ErrorStyle.Render("workspace apply: no workspace named " + v.Name))
				return nil
			}
			if err := ws.Validate(); err != nil {
				a.cmdresult.Append(ErrorStyle.Render(err.Error()))
				return nil
			}
			a.SetWorkspace(ws)
		}
		if a.workspace == nil {
			a.cmdresult.Append(WarnStyle.Render("workspace apply: no workspace installed (--workspace <name>)"))
			return nil
		}
		return a.applyWorkspace()
	case "ls":
		names := a.workspaceFile.Names()
		if len(names) == 0 {
			a.cmdresult.Append("workspace ls: no workspaces in " + a.workspacePath)
			return nil
		}
		for _, n := range names {
			mark := "  "
			if a.workspace != nil && a.workspace.Name == n {
				mark = "* "
			}
			a.cmdresult.Append(mark + n)
		}
		return nil
	case "show":
		name := v.Name
		if name == "" && a.workspace != nil {
			name = a.workspace.Name
		}
		ws, ok := a.workspaceFile.Workspace(name)
		if !ok {
			a.cmdresult.Append(ErrorStyle.Render("workspace show: no workspace named " + name))
			return nil
		}
		for _, line := range strings.Split(strings.TrimRight(string(workspace.Block(ws)), "\n"), "\n") {
			a.cmdresult.Append(line)
		}
		return nil
	case "save":
		return a.saveWorkspace(v.Name)
	}
	return nil
}
```

`workspace.Block(ws)` renders one workspace for display. Implement it in `cli/workspace` as a thin wrapper over `renderWorkspace` (the code is in Task 10, Step 4), so what `show` prints and what `save` writes cannot drift. Add `a.workspaceFile *workspace.File` and `a.workspacePath string` to the App struct; Task 9 fills them. Both `(*File).Workspace` and `(*File).Names` tolerate a nil receiver, so a client with no config file needs no extra guard here.

- [ ] **Step 5: Implement `saveWorkspace`**

```go
// saveWorkspace captures the live client state into the named workspace and
// writes the file, replacing only that workspace's lines.
//
// The forwards saved are this client's own (a.activeForwards). `forward ls`
// would also show forwards other clients established, and a workspace
// describes what THIS client sets up. Raw (`t`) forwards are not saved: they
// never join a.activeForwards and bind nothing locally, so no -L/-R spec
// reproduces one.
func (a *App) saveWorkspace(name string) tea.Cmd {
	// The App does not keep its Config: tui.New copies cfg.Server into a.server
	// and cfg.DefaultRepo into a.defaultRepo (tui/app.go:263-264).
	ws := &workspace.Workspace{
		Name:      name,
		ServerCID: a.server,
		Repo:      a.defaultRepo,
	}
	if a.grid.IsOpen() {
		ws.Grid, ws.GridSet = a.grid.ArgsString(), true
	}
	byTask := map[string]*workspace.Task{}
	var order []string
	add := func(id string) *workspace.Task {
		if t, ok := byTask[id]; ok {
			return t
		}
		t := &workspace.Task{ID: id, Resume: workspace.ResumeContinue, Runner: workspace.RunnerAssigned}
		byTask[id] = t
		order = append(order, id)
		return t
	}
	if id := a.logs.TaskID(); id != "" {
		add(id)
	}
	for _, s := range a.activeForwards {
		add(s.TaskID).Forwards = append(add(s.TaskID).Forwards, s.Direction.flag()+" "+s.Spec)
	}
	for _, id := range order {
		ws.Tasks = append(ws.Tasks, *byTask[id])
	}

	f := a.workspaceFile
	if f == nil {
		f = workspace.New()
		a.workspaceFile = f
	}
	f.Set(ws)
	path := a.workspacePath
	if path == "" {
		path = workspace.DefaultPath
		a.workspacePath = path
	}
	if err := f.Save(path); err != nil {
		a.cmdresult.Append(ErrorStyle.Render("workspace save: " + err.Error()))
		return nil
	}
	a.cmdresult.Append(OKStyle.Render(fmt.Sprintf("workspace %s saved to %s: %d task(s), %d forward(s)",
		name, path, len(ws.Tasks), countForwards(ws))))
	a.SetWorkspace(ws)
	return nil
}

func countForwards(ws *workspace.Workspace) int {
	n := 0
	for _, t := range ws.Tasks {
		n += len(t.Forwards)
	}
	return n
}
```

`a.grid.ArgsString()` does not exist yet: add it to `GridModel` in `tui/grid.go`, rendering the model's current mode/anchor/ids back into the `grid` argument grammar (`--under <id>`, `--under <id> --descendants`, a bare id list, or empty for `all`). Read how `GridModel` stores its scope — `scopeLabel()` around `tui/grid.go:446` shows the fields — and render from those. Pin it with a test that `cli.ParseGridArgs(shlex.Split(ArgsString()))` returns the mode and anchor the model holds, for all four modes. A serializer whose comment claims it round-trips, without a test that runs the round trip, is a documented recurring defect in this repository.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./tui/ -v 2>&1 | tail -20`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add tui/cmdline.go tui/cmdline_workspace_test.go tui/app.go tui/workspace.go tui/grid.go
git commit -m "feat(tui): workspace save/apply/ls/show in the command line"
```

---

### Task 9: `harness-tui` flags

**Files:**
- Modify: `cmd/harness-tui/main.go`
- Modify: `tui/app.go` (accept the file and path through `tui.Config`)

**Interfaces:**
- Consumes: `workspace.Load` (Task 2), `(*Workspace).Validate` (Task 3), `(*App).SetWorkspace` (Task 7).
- Produces: the `--config` and `--workspace` flags; `tui.Config` gains `WorkspaceFile *workspace.File`, `WorkspacePath string`, `WorkspaceName string`.

- [ ] **Step 1: Add the flags and load the file**

In `cmd/harness-tui/main.go`, beside the existing flag vars:

```go
	configPath = flag.String("config", "", "workspace config file (env: HARNESS_CONFIG; default ./.harness/config)")
	wsName     = flag.String("workspace", "", "workspace to apply on start and on every reconnect (see `workspace ls`)")
```

After `flag.Parse()` and before building the App:

```go
	wsFile, wsPath, err := workspace.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(2)
	}
```

A parse error exits here, before bubbletea takes the alt screen — an error written after that would be painted over.

- [ ] **Step 2: Resolve the connection values through the workspace**

```go
	var ws *workspace.Workspace
	if wsFile != nil && *wsName != "" {
		w, ok := wsFile.Workspace(*wsName)
		if !ok {
			fmt.Fprintf(os.Stderr, "config: %s has no workspace named %q\n", wsPath, *wsName)
			os.Exit(2)
		}
		if err := w.Validate(); err != nil {
			fmt.Fprintln(os.Stderr, "config:", err)
			os.Exit(2)
		}
		ws = w
	}

	wsServerCID, wsWSPath, wsRepo := "", "", ""
	if ws != nil {
		wsServerCID, wsWSPath, wsRepo = ws.ServerCID, ws.WSPath, ws.Repo
	}
	cli.WebSocketPath = cliopts.ResolveStringWith(*wsPathFlag, "HARNESS_WS_PATH", wsWSPath)
	if cli.WebSocketPath == "" {
		cli.WebSocketPath = "/ws"
	}
```

`harness-tui`'s existing `--ws-path` has the default `"/ws"` baked into `flag.String`, which would make the flag always non-empty and shadow the workspace value. Change its default to `""` and apply `/ws` only after resolution, as sketched above. Do the same for `--server-cid`, whose current default is `"ws:127.0.0.1:8539-*"`: resolve first, then fall back to that literal.

Add to `cli/cliopts/cliopts.go`:

```go
// ResolveStringWith is ResolveString with a third tier: a value from the
// workspace config, consulted only when neither the flag nor the environment
// supplied one. cliopts takes the value rather than the file so it does not
// import the workspace package — the resolution order lives here, the file
// format lives there.
func ResolveStringWith(flagVal, envName, fileVal string) string {
	if v := ResolveString(flagVal, envName); v != "" {
		return v
	}
	return fileVal
}
```

- [ ] **Step 3: Hand the workspace to the App**

Extend `tui.Config` with `WorkspaceFile`, `WorkspacePath`, `WorkspaceName`, populate them in `tui.New`, and in `tui.New` install the workspace with `SetWorkspace` when the name resolves. Read `tui.New` and the `Config` struct first; follow the field-copying pattern already there.

- [ ] **Step 4: Verify**

Run: `make vet && go test ./tui/... ./cli/... -count=1`
Expected: PASS, and `git status --short` shows no stray binary.

Then check the flag by hand against a dummy harness (see the `dummy-harness` skill): start one, write a `.harness/config` with a `[workspace default]` holding only `server-cid`, and confirm `bin/harness-tui --workspace default` connects with no `--server-cid` on the command line.

- [ ] **Step 5: Commit**

```bash
git add cmd/harness-tui/main.go tui/app.go cli/cliopts/cliopts.go
git commit -m "feat(tui): --config and --workspace, applied on start and every reconnect"
```

---

### Task 10: `harness-cli` flags and the `workspace` subcommand

**Files:**
- Modify: `cmd/harness-cli/main.go` (global flags, `workspace` case, usage)
- Create: `cmd/harness-cli/workspace.go`
- Test: `cmd/harness-cli/workspace_test.go`

**Interfaces:**
- Consumes: `workspace.Load`, `(*File).Set`, `(*File).Save`, `Block` (Tasks 1–4); `cli.PortForwardList` (existing).
- Produces: `harness-cli workspace save <name> --task <id>`, `workspace ls`, `workspace show [name]`.

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"strings"
	"testing"
)

// `workspace` with no subcommand must print usage and exit 2 without dialling —
// the same shape as `forward` with no arguments.
func TestCLI_WorkspaceUsage(t *testing.T) {
	out, code := runCLI(t, "--server-cid=ws:127.0.0.1:19998-1", "workspace")
	if code == 0 {
		t.Errorf("exit code = 0, want non-zero")
	}
	if !strings.Contains(out, "workspace save") {
		t.Errorf("usage not printed: %s", out)
	}
}

// A bad task id must be rejected before any connection is attempted.
func TestCLI_WorkspaceSaveRejectsBadTaskID(t *testing.T) {
	out, code := runCLI(t, "--server-cid=ws:127.0.0.1:19997-1", "workspace", "save", "default", "--task", "nope")
	if code == 0 {
		t.Errorf("exit code = 0, want non-zero")
	}
	if !strings.Contains(out, "task") {
		t.Errorf("error does not mention the task id: %s", out)
	}
}
```

`runCLI` is the existing helper in `cmd/harness-cli/main_test.go` (see `TestCLI_ForwardLsRoutes`); read it and match its signature rather than writing a new one.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./cmd/harness-cli/ -run TestCLI_Workspace -v`
Expected: FAIL — `workspace` is an unknown subcommand.

- [ ] **Step 3: Add the global flags**

Beside `serverCID` and `wsPath` in `main()`:

```go
	configPath := flag.String("config", "", "workspace config file (env: HARNESS_CONFIG; default ./.harness/config)")
	wsName := flag.String("workspace", "", "workspace whose server-cid / ws-path / repo to use")
```

After `flag.Parse()`, load it and fold the three values into the existing resolution. `parseCID` currently substitutes `ws:127.0.0.1:8539-*` when both flag and env are empty; the workspace value goes between those:

```go
	wsFile, _, err := workspace.Load(*configPath)
	if err != nil {
		die(fmt.Errorf("config: %w", err))
	}
	var wsServerCID, wsWSPath, wsRepo string
	if wsFile != nil && *wsName != "" {
		w, ok := wsFile.Workspace(*wsName)
		if !ok {
			die(fmt.Errorf("config: no workspace named %q", *wsName))
		}
		wsServerCID, wsWSPath, wsRepo = w.ServerCID, w.WSPath, w.Repo
	}
```

Then `resolvedWS := cliopts.ResolveStringWith(*wsPath, "HARNESS_WS_PATH", wsWSPath)`, and inside `parseCID` prefer `wsServerCID` over the built-in literal. `wsRepo` joins the existing `HARNESS_REPO_PATH` fallback at `cmd/harness-cli/main.go:334` and `cmd/harness-cli/session.go:633` — wire BOTH; a repo default wired in one of the two is the surface-skew failure this repository has hit before.

- [ ] **Step 4: Write `cmd/harness-cli/workspace.go`**

```go
package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"os"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/workspace"
	"github.com/on-keyday/objtrsf/objproto"
)

func workspaceUsage() {
	fmt.Fprintln(os.Stderr, "usage: harness-cli workspace save <name> --task <32-hex> [--resume no|continue|fresh] [--runner assigned|any]")
	fmt.Fprintln(os.Stderr, "       harness-cli workspace ls")
	fmt.Fprintln(os.Stderr, "       harness-cli workspace show [<name>]")
}

// runWorkspace implements the `workspace` family. save reads the task's
// forwards from the server-side registry, which is the only source a
// short-lived CLI process has: it holds no forwards of its own. That registry
// also carries forwards other clients established, so a CLI save can capture
// more than the TUI's would — see the spec's "Writing a workspace". Filtering
// by origin_cid is not the fix: this process owns none of them.
func runWorkspace(ctx context.Context, args []string, cid objproto.ConnectionID, cfgPath, serverCIDStr string) error {
	if len(args) == 0 {
		workspaceUsage()
		os.Exit(2)
	}
	f, path, err := workspace.Load(cfgPath)
	if err != nil {
		return err
	}
	if path == "" {
		path = workspace.DefaultPath
	}
	switch args[0] {
	case "ls":
		if f == nil || len(f.Names()) == 0 {
			fmt.Printf("no workspaces in %s\n", path)
			return nil
		}
		for _, n := range f.Names() {
			fmt.Println(n)
		}
		return nil

	case "show":
		name := ""
		if len(args) > 1 {
			name = args[1]
		}
		if f == nil {
			return fmt.Errorf("no config at %s", path)
		}
		if name == "" {
			for _, n := range f.Names() {
				os.Stdout.Write(workspace.Block(mustWorkspace(f, n)))
			}
			return nil
		}
		ws, ok := f.Workspace(name)
		if !ok {
			return fmt.Errorf("no workspace named %q in %s", name, path)
		}
		os.Stdout.Write(workspace.Block(ws))
		return nil

	case "save":
		if len(args) < 2 {
			workspaceUsage()
			os.Exit(2)
		}
		name := args[1]
		fs := flag.NewFlagSet("workspace save", flag.ExitOnError)
		taskID := fs.String("task", "", "task id (32 hex) whose forwards to capture")
		resume := fs.String("resume", string(workspace.ResumeContinue), "no | continue | fresh")
		runner := fs.String("runner", string(workspace.RunnerAssigned), "assigned | any")
		repo := fs.String("repo", "", "repo identifier to record")
		fs.Parse(args[2:])
		if _, err := hex.DecodeString(*taskID); err != nil || len(*taskID) != 32 {
			return fmt.Errorf("workspace save: --task must be a 32-hex task id, got %q", *taskID)
		}
		forwards, err := cli.PortForwardList(ctx, cid, *taskID)
		if err != nil {
			return err
		}
		tk := workspace.Task{ID: *taskID, Resume: workspace.Resume(*resume), Runner: workspace.Runner(*runner)}
		for i := range forwards {
			tk.Forwards = append(tk.Forwards,
				cli.PortForwardDirFlag(forwards[i].Direction)+" "+cli.PortForwardSpecString(&forwards[i]))
		}
		if f == nil {
			f = workspace.New()
		}
		ws := &workspace.Workspace{Name: name, ServerCID: serverCIDStr, Repo: *repo, Tasks: []workspace.Task{tk}}
		if err := ws.Validate(); err != nil {
			return err
		}
		f.Set(ws)
		if err := f.Save(path); err != nil {
			return err
		}
		fmt.Printf("workspace %s saved to %s: 1 task, %d forward(s)\n", name, path, len(tk.Forwards))
		return nil
	}
	workspaceUsage()
	os.Exit(2)
	return nil
}

func mustWorkspace(f *workspace.File, name string) *workspace.Workspace {
	ws, _ := f.Workspace(name)
	return ws
}
```

`cli.PortForwardSpecString` (`cli/port_forward_list.go:113`) renders a registered forward's endpoints; read it and confirm the string it produces is what `cli.ParseForwardSpec` / `ParseRemoteForwardSpec` accept. If it is not — for instance if it renders `a -> b` rather than a colon spec — write the spec from the `PortForwardInfo` fields directly and add a test that the result round-trips through the matching parser. Do not ship an unvalidated string into a config file.

Add `workspace.Block`:

```go
// Block renders one workspace exactly as Set would write it, so `workspace
// show` and the file cannot disagree.
func Block(ws *Workspace) []byte {
	return []byte(strings.Join(renderWorkspace(ws), "\n") + "\n")
}
```

Wire the case in `main()` beside `case "forward":`, and add the two usage lines to the binary's overall usage near `cmd/harness-cli/main.go:1051`.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./cmd/harness-cli/ -v 2>&1 | tail -20`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/harness-cli/workspace.go cmd/harness-cli/workspace_test.go cmd/harness-cli/main.go cmd/harness-cli/session.go
git commit -m "feat(cli): harness-cli workspace save/ls/show and --config/--workspace"
```

---

### Task 11: gitignore, docs, and the checklist firing log

**Files:**
- Modify: `.gitignore`
- Modify: `README.md`
- Modify: `.claude/skills/surface-parity-checklist/firing-log.md`

- [ ] **Step 1: Ignore the config**

Add to `.gitignore`, beside `harness-data/`:

```
# Client-side workspace config: which task to bring back, which forwards to
# establish, which grid to open. Holds a LAN server-cid and local repo paths,
# so it is machine-local like harness-data/ and bin/.run/.
.harness/
```

Confirm this does not shadow `.harness-worktrees/`: run `git check-ignore -v .harness-worktrees` and expect the existing `.harness-worktrees/` rule, not the new one.

- [ ] **Step 2: Document it in the README**

Add a "Workspace config" section covering: the file's location and resolution order, the grammar with the full example from the spec, the key tables, the three apply paths, that an apply reconciles rather than restarts, and that the config is ignored inside an agent. Add `workspace` to the TUI cmdline verb list and to the `harness-cli` subcommand summary — `README.md` documents both, and a verb missing from either list is the documentation skew this repository has hit before.

- [ ] **Step 3: Record the checklist walk**

Append an entry to `.claude/skills/surface-parity-checklist/firing-log.md` in the format the existing entries use (read the file first — entries are commit-anchored and end with a `missed:` line). The walk that produced this plan came back:

- `done`: 1, 2, 3, 9, 10, 24, 25, 27, 28a, 29, 30, 31, 32, 33, 35, 37
- `omitted`: 4 (no keybinding — re-applying is rare and the command line reaches it), 6, 7, 8 (WebUI: no file in a browser, and `-L` has no browser equivalent), 36 (agent skills unchanged: agents must not read an operator's workspace)
- S5 fired and was the walk's find: `HARNESS_*` is forwarded into the podman sandbox by prefix, so `HARNESS_CONFIG` would reach every sandboxed agent. Answered by refusing to read any config when `HARNESS_AUTH_TICKET` is set.

Update the standing tallies table in the same edit, and add a row for S5 if none exists.

- [ ] **Step 4: Full verification**

Run: `make vet && make test && make check`
Expected: all pass. `TestOpenInteractiveSessionMux` in `server` is flaky about 1 run in 4 and pre-dates this work — re-run before attributing a failure there to this change.

Then `git status --short` must show nothing but the intended files: no stray `harness-tui`, `harness-cli` or `*.test` binary.

- [ ] **Step 5: Commit**

```bash
git add .gitignore README.md .claude/skills/surface-parity-checklist/firing-log.md
git commit -m "docs: workspace config in the README, gitignore .harness/, record the parity walk"
```

---

## Verification before calling this done

- `make check` and `make test` green.
- Against a dummy harness (`dummy-harness` skill), with a real `.harness/config`:
  1. `bin/harness-tui --workspace default` connects with no `--server-cid` on the command line.
  2. A declared `-L` forward is listed by `harness-cli forward ls` after start.
  3. Restarting the server and letting the TUI reconnect brings the task back and re-establishes the forward, with no keystroke.
  4. Occupying the declared local port with another process, then starting the TUI, produces one failure line and leaves the resume and the grid working.
  5. Freeing the port and running `workspace apply` starts only the missing forward — the other one's `forward ls` id is unchanged.
- `HARNESS_AUTH_TICKET=<32 hex> bin/harness-cli workspace ls` reports no config even with one present in the working directory.
