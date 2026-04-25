# harness-tui Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `cmd/harness-tui`, a Bubble Tea-based TUI frontend that wraps existing CLI operations (submit / ls / logs / cancel / prune / watch) into one interactive screen against the running `harness-server`.

**Architecture:** New `cmd/harness-tui` binary plus new `tui/` package. Pubsub event bridge keeps tables and the log viewport live; submit happens via a multi-line popup; other cmdline ops route through `shlex` + stdlib `flag.FlagSet` parsing. No changes to server / runner / cli packages — purely additive.

**Tech Stack:** Go 1.25.7, `github.com/charmbracelet/bubbletea` + `bubbles` + `lipgloss`, `github.com/google/shlex`, existing in-tree `objproto` / `trsf` / `pubsub` / `cli`.

**Spec:** `docs/superpowers/specs/2026-04-25-harness-tui-design.md`. Read it first if you need bigger-picture context.

---

## Reference for implementers

### bgn-generated API conventions (CRITICAL)

`runner/protocol/message.go` is generated from `.bgn` schema. Match-union types (`RunnerMessage`, `TaskControlRequest`, etc.) expose variants via getter / setter methods, NOT struct fields:

```go
// CORRECT
m := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_Hello}
m.SetHello(protocol.RunnerHello{Version: 1})

// WRONG (does not compile)
m := &protocol.RunnerMessage{Kind: ..., Hello: &protocol.RunnerHello{...}}
```

Variable-length byte fields use `SetXxx([]byte)` setters that auto-sync `*Len`. Do NOT manually fill `RepoPathLen` etc.

### bubbletea pattern

The `tea.Program` is created in `main.go`. Goroutines that push events use `program.Send(msg)`. The `Model` does not need a reference to `Program`; we wire the goroutines up in `main.go` after `New()` and before `Run()`.

### Existing CLI helpers to reuse

- `cli.Dial(ctx, addr) (*cli.Client, error)` returns a Client with an open `objproto.Connection`.
- `cli.Submit(ctx, addr, repo, prompt) (taskID string, error)` opens its own short-lived conn.
- `cli.Cancel(ctx, addr, taskIDHex) error` short-lived.
- `cli.PruneTasks(ctx, addr, cutoff) (uint32, error)` short-lived.
- `cli.Prune(ctx, addr, repo, before, out)` does both server prune + local worktree cleanup.
- `cli.Logs(ctx, addr, taskID, out)` long-lived; reuse the helper internals (`pubsub.JoinTopic`, `trsf.NewStreams`, `trsf.AutoPing`) directly because the TUI shares ONE persistent connection across all interactions.
- `topics.TaskLog(id)`, `topics.TasksStatus()`, `topics.RunnersStatus()`.

The TUI should keep ONE long-lived `objproto.Connection` (with `trsf.AutoPing` keep-alive) for the lifetime of the program. cli.Submit / Cancel / PruneTasks open their own connections; for the TUI we keep them, since their round-trip is short enough that opening per-call is acceptable. The persistent connection is needed only for the pubsub subscriptions.

### Generated message field references (look these up if needed)

- `protocol.TaskStatusEvent`: `Kind`, `TaskId.Id [16]byte`, `Ts uint64`, `TaskStatus`, `ExitCode int32`
- `protocol.RunnerStatusEvent`: `Kind`, `RunnerId`, `Ts`, `RunnerStatus`
- `protocol.RunnerInfo`: `Id`, `Status`, `RepoPath`, `RepoPathLen`, `CurrentTask.Id`, `ConnectedAt`, `LastSeen`
- `protocol.TaskInfo`: `Id`, `Status`, `RepoPath`, `WorktreeDir`, `Prompt`, timestamps, `ExitCode`

---

## File structure

### Create

```
cmd/harness-tui/main.go    Entry: flags, server dial, tea.Program startup
tui/
  styles.go                 lipgloss style consts (panel borders, focus colors)
  cmdline.go                Command input parser (shlex + flag.FlagSet)
  cmdline_test.go           Parser tests
  runners.go                Runners table sub-model
  tasks.go                  Tasks table sub-model
  cmdresult.go              Viewport for last command output
  app.go                    Top-level Model: focus rotation, layout, key dispatch
  client.go                 Wraps cli.Submit/Cancel/PruneTasks as tea.Cmd factories;
                            holds the persistent objproto.Connection for pubsub.
  events.go                 pubsub topic subscribe goroutines (program.Send dispatch)
  events_test.go            Decode-helper tests
  logs.go                   Log viewport sub-model + topic JOIN/LEAVE on selection
  popup.go                  Submit multiline popup (textarea overlay)
```

### Modify

- `go.mod`, `go.sum`: add bubbletea / bubbles / lipgloss / shlex
- `README.md`: add a `harness-tui` section

### No changes to (purely additive task)

- `cli/`, `server/`, `runner/`, `pubsub/`, `objproto/`, `trsf/`, `transport/`, `topics/`, `runner/protocol/`

---

## Phase 1: Skeleton + parser

### Task 1: Add deps and `cmd/harness-tui` entry stub

**Files:**
- Create: `cmd/harness-tui/main.go`
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1: Add deps**

```bash
go get github.com/charmbracelet/bubbletea
go get github.com/charmbracelet/bubbles
go get github.com/charmbracelet/lipgloss
go get github.com/google/shlex
```

Verify: `grep -E 'bubbletea|bubbles|lipgloss|shlex' go.mod` shows all four.

- [ ] **Step 2: Write a minimal `cmd/harness-tui/main.go` that just exits**

```go
package main

import (
	"flag"
	"fmt"
	"os"
)

var (
	serverAddr = flag.String("server", "localhost:8539", "harness-server host:port")
	repoFlag   = flag.String("repo", ".", "default repo path for submit popup")
)

func main() {
	flag.Parse()
	fmt.Fprintf(os.Stderr, "harness-tui (skeleton): server=%s repo=%s\n", *serverAddr, *repoFlag)
}
```

(We will replace this with the full Bubble Tea startup in Task 7.)

- [ ] **Step 3: Build**

Run: `go build ./...`
Expected: clean.

- [ ] **Step 4: Commit**

```bash
git add go.mod go.sum cmd/harness-tui/
git commit -m "tui: add deps and harness-tui entry stub" \
  -m "Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: lipgloss style constants

**Files:**
- Create: `tui/styles.go`

- [ ] **Step 1: Write `tui/styles.go`**

```go
package tui

import "github.com/charmbracelet/lipgloss"

// Color palette. Tweak freely once the user has run the binary.
var (
	colorBorder    = lipgloss.Color("241") // gray
	colorFocused   = lipgloss.Color("69")  // accent (focus highlight)
	colorOK        = lipgloss.Color("42")  // green
	colorWarn      = lipgloss.Color("214") // amber
	colorErr       = lipgloss.Color("196") // red
	colorMuted     = lipgloss.Color("245") // dim
)

// PanelStyle is the unfocused panel border.
var PanelStyle = lipgloss.NewStyle().
	Border(lipgloss.RoundedBorder()).
	BorderForeground(colorBorder)

// PanelStyleFocused is the focused panel border.
var PanelStyleFocused = lipgloss.NewStyle().
	Border(lipgloss.RoundedBorder()).
	BorderForeground(colorFocused)

// HeaderStyle styles the top status line.
var HeaderStyle = lipgloss.NewStyle().Bold(true)

// FooterStyle styles the bottom hotkey hint line.
var FooterStyle = lipgloss.NewStyle().Foreground(colorMuted)

// ErrorStyle marks error text in cmdresult.
var ErrorStyle = lipgloss.NewStyle().Foreground(colorErr)

// OKStyle marks success / connected indicators.
var OKStyle = lipgloss.NewStyle().Foreground(colorOK)

// WarnStyle marks warnings.
var WarnStyle = lipgloss.NewStyle().Foreground(colorWarn)
```

- [ ] **Step 2: Build**

Run: `go build ./...`
Expected: clean.

- [ ] **Step 3: Commit**

```bash
git add tui/styles.go
git commit -m "tui: add lipgloss style consts" \
  -m "Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: Command-line parser (TDD)

**Files:**
- Create: `tui/cmdline.go`, `tui/cmdline_test.go`

The parser converts a single user input line into a typed action struct. Other components consume actions, not raw strings.

- [ ] **Step 1: Write the failing tests**

```go
// tui/cmdline_test.go
package tui

import (
	"testing"
	"time"
)

func TestParseSubmitWithRepo(t *testing.T) {
	got, err := ParseCommand(`submit --repo /foo "long prompt with spaces"`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a, ok := got.(SubmitAction)
	if !ok {
		t.Fatalf("got %T, want SubmitAction", got)
	}
	if a.Repo != "/foo" {
		t.Errorf("Repo=%q", a.Repo)
	}
	if a.Prompt != "long prompt with spaces" {
		t.Errorf("Prompt=%q", a.Prompt)
	}
}

func TestParseSubmitDefaultRepo(t *testing.T) {
	got, err := ParseCommand(`submit hello`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(SubmitAction)
	if a.Repo != "/cwd" {
		t.Errorf("Repo=%q, want /cwd", a.Repo)
	}
	if a.Prompt != "hello" {
		t.Errorf("Prompt=%q", a.Prompt)
	}
}

func TestParseSubmitMissingPrompt(t *testing.T) {
	_, err := ParseCommand(`submit`, "/cwd")
	if err == nil {
		t.Fatal("expected error on missing prompt")
	}
}

func TestParseCancel(t *testing.T) {
	got, err := ParseCommand(`cancel ab12cd`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(CancelAction)
	if a.IDPrefix != "ab12cd" {
		t.Errorf("IDPrefix=%q", a.IDPrefix)
	}
}

func TestParseCancelMissingID(t *testing.T) {
	_, err := ParseCommand(`cancel`, "/cwd")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestParsePruneDefault(t *testing.T) {
	got, err := ParseCommand(`prune`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(PruneAction)
	if a.Before != 7*24*time.Hour {
		t.Errorf("Before=%v, want 168h", a.Before)
	}
	if a.Offline {
		t.Error("Offline=true, want false")
	}
}

func TestParsePruneFlags(t *testing.T) {
	got, err := ParseCommand(`prune --before=1h --offline`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(PruneAction)
	if a.Before != time.Hour {
		t.Errorf("Before=%v", a.Before)
	}
	if !a.Offline {
		t.Error("Offline=false")
	}
}

func TestParseClear(t *testing.T) {
	got, err := ParseCommand(`clear`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.(ClearAction); !ok {
		t.Fatalf("got %T", got)
	}
}

func TestParseQuit(t *testing.T) {
	for _, in := range []string{"quit", "exit"} {
		got, err := ParseCommand(in, "/cwd")
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := got.(QuitAction); !ok {
			t.Fatalf("input %q got %T", in, got)
		}
	}
}

func TestParseHelp(t *testing.T) {
	got, err := ParseCommand(`help`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.(HelpAction); !ok {
		t.Fatalf("got %T", got)
	}
}

func TestParseEmpty(t *testing.T) {
	got, err := ParseCommand(``, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("expected nil action on empty input, got %T", got)
	}
}

func TestParseUnknown(t *testing.T) {
	_, err := ParseCommand(`teleport`, "/cwd")
	if err == nil {
		t.Fatal("expected error")
	}
}
```

- [ ] **Step 2: Run tests, verify they fail**

Run: `go test ./tui/... -v -run TestParse`
Expected: compile errors (`ParseCommand`, action types undefined).

- [ ] **Step 3: Implement the parser**

```go
// tui/cmdline.go
package tui

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/shlex"
)

// Action is the typed result of parsing one cmdline input.
// app.go switches on the concrete type.
type Action interface{ isAction() }

type SubmitAction struct {
	Repo   string
	Prompt string
}

type CancelAction struct {
	IDPrefix string
}

type PruneAction struct {
	Before  time.Duration
	Offline bool
}

type ClearAction struct{}
type QuitAction struct{}
type HelpAction struct{}

func (SubmitAction) isAction() {}
func (CancelAction) isAction() {}
func (PruneAction) isAction()  {}
func (ClearAction) isAction()  {}
func (QuitAction) isAction()   {}
func (HelpAction) isAction()   {}

// ParseCommand tokenizes and parses one input line. defaultRepo is used when
// `submit` is invoked without --repo (typically the cwd).
// Returns (nil, nil) for empty / whitespace-only input.
func ParseCommand(input, defaultRepo string) (Action, error) {
	tokens, err := shlex.Split(input)
	if err != nil {
		return nil, fmt.Errorf("shlex: %w", err)
	}
	if len(tokens) == 0 {
		return nil, nil
	}
	switch tokens[0] {
	case "submit":
		return parseSubmit(tokens[1:], defaultRepo)
	case "cancel":
		return parseCancel(tokens[1:])
	case "prune":
		return parsePrune(tokens[1:])
	case "clear":
		return ClearAction{}, nil
	case "quit", "exit":
		return QuitAction{}, nil
	case "help":
		return HelpAction{}, nil
	default:
		return nil, fmt.Errorf("unknown command: %q", tokens[0])
	}
}

func parseSubmit(args []string, defaultRepo string) (Action, error) {
	fs := flag.NewFlagSet("submit", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repo := fs.String("repo", defaultRepo, "")
	if err := fs.Parse(args); err != nil {
		return nil, fmt.Errorf("submit: %w", err)
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return nil, fmt.Errorf("submit: prompt is required")
	}
	return SubmitAction{Repo: *repo, Prompt: strings.Join(rest, " ")}, nil
}

func parseCancel(args []string) (Action, error) {
	fs := flag.NewFlagSet("cancel", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return nil, fmt.Errorf("cancel: %w", err)
	}
	if fs.NArg() == 0 {
		return nil, fmt.Errorf("cancel: task id required")
	}
	return CancelAction{IDPrefix: fs.Arg(0)}, nil
}

func parsePrune(args []string) (Action, error) {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	before := fs.Duration("before", 7*24*time.Hour, "")
	offline := fs.Bool("offline", false, "")
	if err := fs.Parse(args); err != nil {
		return nil, fmt.Errorf("prune: %w", err)
	}
	return PruneAction{Before: *before, Offline: *offline}, nil
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./tui/... -v -run TestParse`
Expected: all tests PASS.

- [ ] **Step 5: Commit**

```bash
git add tui/cmdline.go tui/cmdline_test.go
git commit -m "tui: add cmdline parser (shlex + flag.FlagSet)" \
  -m "Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Phase 2: Static panels

These tasks build each visual panel against fake/static data so we can run the binary and visually verify before plugging in live data.

### Task 4: Runners table sub-model

**Files:**
- Create: `tui/runners.go`

The runners table is informational. Sub-model owns a `bubbles/table.Model`, exposes:
- `New() RunnersModel` — constructor
- `(m RunnersModel) Update(msg) (RunnersModel, tea.Cmd)` — propagates messages to the embedded table when focused
- `(m RunnersModel) View() string` — borders applied externally
- `(m *RunnersModel) SetRows([]protocol.RunnerInfo)` — replaces rows
- `(m *RunnersModel) Focus()` / `Blur()` — focus state

- [ ] **Step 1: Write `tui/runners.go`**

```go
package tui

import (
	"fmt"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

type RunnersModel struct {
	table   table.Model
	focused bool
}

func NewRunners() RunnersModel {
	cols := []table.Column{
		{Title: "Status", Width: 8},
		{Title: "Repo", Width: 40},
		{Title: "Current Task", Width: 14},
	}
	t := table.New(table.WithColumns(cols), table.WithFocused(false))
	return RunnersModel{table: t}
}

func (m *RunnersModel) Focus() {
	m.focused = true
	m.table.Focus()
}

func (m *RunnersModel) Blur() {
	m.focused = false
	m.table.Blur()
}

func (m *RunnersModel) IsFocused() bool { return m.focused }

func (m *RunnersModel) SetSize(w, h int) {
	m.table.SetWidth(w)
	m.table.SetHeight(h)
}

// SetRows updates the runner rows from a snapshot.
func (m *RunnersModel) SetRows(rs []protocol.RunnerInfo) {
	rows := make([]table.Row, 0, len(rs))
	for _, r := range rs {
		rows = append(rows, table.Row{
			runnerStatusStr(r.Status),
			truncateLeft(string(r.RepoPath), 40),
			shortHexNonZero(r.CurrentTask.Id[:]),
		})
	}
	m.table.SetRows(rows)
}

func (m RunnersModel) Update(msg tea.Msg) (RunnersModel, tea.Cmd) {
	if !m.focused {
		return m, nil
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m RunnersModel) View() string {
	return m.table.View()
}

func runnerStatusStr(s protocol.RunnerStatus) string {
	switch s {
	case protocol.RunnerStatus_Idle:
		return "Idle"
	case protocol.RunnerStatus_Busy:
		return "Busy"
	default:
		return "Offline"
	}
}

// truncateLeft keeps the right-most part of s within max chars (left side gets "…").
// Repo paths are most informative on the right (the last directory).
func truncateLeft(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return "…" + s[len(s)-(max-1):]
}

// shortHexNonZero renders the first 12 hex chars of b, or "-" if b is all-zero.
func shortHexNonZero(b []byte) string {
	allZero := true
	for _, v := range b {
		if v != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return "-"
	}
	const tab = "0123456789abcdef"
	out := make([]byte, 0, 12)
	for i := 0; i < 6 && i < len(b); i++ {
		out = append(out, tab[b[i]>>4], tab[b[i]&0xf])
	}
	return string(out)
}

// formatTaskID is a small helper used by tests / debug.
func formatTaskID(b []byte) string { return fmt.Sprintf("%x", b) }
```

- [ ] **Step 2: Build**

Run: `go build ./...`
Expected: clean. (No tests at this stage — visual panel.)

- [ ] **Step 3: Commit**

```bash
git add tui/runners.go
git commit -m "tui: add runners table sub-model" \
  -m "Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: Tasks table sub-model

**Files:**
- Create: `tui/tasks.go`

Mirror of `runners.go` with different columns and a `SelectedID()` accessor.

- [ ] **Step 1: Write `tui/tasks.go`**

```go
package tui

import (
	"encoding/hex"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

type TasksModel struct {
	table   table.Model
	focused bool
	// rowIDs[i] is the full hex task ID for row i; bubbles/table doesn't carry
	// arbitrary metadata so we mirror.
	rowIDs []string
}

func NewTasks() TasksModel {
	cols := []table.Column{
		{Title: "Status", Width: 9},
		{Title: "ID", Width: 12},
		{Title: "Repo", Width: 28},
		{Title: "Prompt", Width: 0}, // resized later via SetSize
	}
	t := table.New(table.WithColumns(cols), table.WithFocused(false))
	return TasksModel{table: t}
}

func (m *TasksModel) Focus() {
	m.focused = true
	m.table.Focus()
}

func (m *TasksModel) Blur() {
	m.focused = false
	m.table.Blur()
}

func (m *TasksModel) IsFocused() bool { return m.focused }

func (m *TasksModel) SetSize(w, h int) {
	m.table.SetWidth(w)
	m.table.SetHeight(h)
	// Stretch the prompt column to fill remaining width.
	cols := m.table.Columns()
	used := 0
	for i := 0; i < len(cols)-1; i++ {
		used += cols[i].Width + 2 // table padding
	}
	if rest := w - used - 4; rest > 0 {
		cols[len(cols)-1].Width = rest
		m.table.SetColumns(cols)
	}
}

func (m *TasksModel) SetRows(ts []protocol.TaskInfo) {
	rows := make([]table.Row, 0, len(ts))
	ids := make([]string, 0, len(ts))
	for _, t := range ts {
		idHex := hex.EncodeToString(t.Id.Id[:])
		rows = append(rows, table.Row{
			taskStatusStr(t.Status),
			idHex[:12],
			truncateLeft(string(t.RepoPath), 28),
			truncatePrompt(string(t.Prompt)),
		})
		ids = append(ids, idHex)
	}
	m.rowIDs = ids
	m.table.SetRows(rows)
}

// SelectedID returns the full 32-char hex ID of the focused row, or "" if empty.
func (m *TasksModel) SelectedID() string {
	if len(m.rowIDs) == 0 {
		return ""
	}
	idx := m.table.Cursor()
	if idx < 0 || idx >= len(m.rowIDs) {
		return ""
	}
	return m.rowIDs[idx]
}

func (m TasksModel) Update(msg tea.Msg) (TasksModel, tea.Cmd) {
	if !m.focused {
		return m, nil
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m TasksModel) View() string {
	return m.table.View()
}

func taskStatusStr(s protocol.TaskStatus) string {
	switch s {
	case protocol.TaskStatus_Queued:
		return "Queued"
	case protocol.TaskStatus_Running:
		return "Running"
	case protocol.TaskStatus_Succeeded:
		return "Done"
	case protocol.TaskStatus_Failed:
		return "Failed"
	case protocol.TaskStatus_Cancelled:
		return "Cancel"
	}
	return "?"
}

// truncatePrompt collapses newlines and clips to ~140 chars (the column SetSize will further clip).
func truncatePrompt(p string) string {
	out := make([]byte, 0, len(p))
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c == '\n' || c == '\r' || c == '\t' {
			out = append(out, ' ')
		} else {
			out = append(out, c)
		}
	}
	if len(out) > 140 {
		out = out[:140]
	}
	return string(out)
}
```

- [ ] **Step 2: Build**

Run: `go build ./...`
Expected: clean.

- [ ] **Step 3: Commit**

```bash
git add tui/tasks.go
git commit -m "tui: add tasks table sub-model with SelectedID()" \
  -m "Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: Cmdresult viewport

**Files:**
- Create: `tui/cmdresult.go`

Holds a `bubbles/viewport` for the last command's output. `Append(string)` adds a line and scrolls to bottom.

- [ ] **Step 1: Write `tui/cmdresult.go`**

```go
package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

type CmdResultModel struct {
	vp    viewport.Model
	lines []string
}

func NewCmdResult() CmdResultModel {
	vp := viewport.New(80, 5)
	vp.SetContent("(no command yet)")
	return CmdResultModel{vp: vp}
}

func (m *CmdResultModel) SetSize(w, h int) {
	m.vp.Width = w
	m.vp.Height = h
}

func (m *CmdResultModel) Append(line string) {
	m.lines = append(m.lines, line)
	if len(m.lines) > 200 {
		m.lines = m.lines[len(m.lines)-200:]
	}
	m.vp.SetContent(strings.Join(m.lines, "\n"))
	m.vp.GotoBottom()
}

func (m *CmdResultModel) Clear() {
	m.lines = nil
	m.vp.SetContent("")
}

// View renders the viewport (the caller adds the panel border).
func (m CmdResultModel) View() string { return m.vp.View() }

// Update lets the viewport handle scroll keys when needed (we don't focus
// cmdresult in v1, so this is rarely exercised — but keep it parity-clean).
func (m CmdResultModel) Update(msg tea.Msg) (CmdResultModel, tea.Cmd) {
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}
```

- [ ] **Step 2: Build & commit**

```bash
go build ./...
git add tui/cmdresult.go
git commit -m "tui: add cmdresult viewport" \
  -m "Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

### Task 7: Top-level app skeleton (no live data yet)

**Files:**
- Create: `tui/app.go`
- Modify: `cmd/harness-tui/main.go`

The top-level model wires runners + tasks + cmdresult + cmdline into a single layout. At the end of this task `go run ./cmd/harness-tui` should bring up the layout with empty tables, a working cmdline (parser hooked up but actions are no-ops), a focus-rotation, and a Quit on `q`/`Ctrl+C`.

- [ ] **Step 1: Write `tui/app.go`**

```go
package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type focus int

const (
	focusRunners focus = iota
	focusTasks
	focusLogs
	focusCmdline
)

// App is the top-level Bubble Tea Model.
type App struct {
	server     string
	defaultRepo string

	runners    RunnersModel
	tasks      TasksModel
	cmdresult  CmdResultModel
	cmdline    textinput.Model

	focus      focus
	width      int
	height     int

	// connected mirrors the persistent connection's status (set by main.go via msgs).
	connected  bool

	// status is a one-line message at the top (e.g., "DISCONNECTED — retrying")
	status     string
}

type Config struct {
	Server      string
	DefaultRepo string
}

func New(cfg Config) App {
	cmd := textinput.New()
	cmd.Prompt = "> "
	cmd.Placeholder = "submit / cancel / prune / clear / help / quit"
	cmd.CharLimit = 1024
	cmd.Width = 60
	return App{
		server:     cfg.Server,
		defaultRepo: cfg.DefaultRepo,
		runners:    NewRunners(),
		tasks:      NewTasks(),
		cmdresult:  NewCmdResult(),
		cmdline:    cmd,
		focus:      focusTasks,
		connected:  false,
		status:     "connecting…",
	}
}

func (a App) Init() tea.Cmd {
	a.tasks.Focus()
	return textinput.Blink
}

func (a App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width = msg.Width
		a.height = msg.Height
		a.layout()
		return a, nil

	case tea.KeyMsg:
		// Quit shortcuts.
		if msg.Type == tea.KeyCtrlC {
			return a, tea.Quit
		}
		// `q` quits when not in the cmdline (cmdline must accept literal 'q').
		if a.focus != focusCmdline && msg.String() == "q" {
			return a, tea.Quit
		}
		// Tab cycles focus.
		switch msg.Type {
		case tea.KeyTab:
			a.cycleFocus(+1)
			return a, nil
		case tea.KeyShiftTab:
			a.cycleFocus(-1)
			return a, nil
		}
		// Cmdline submit.
		if a.focus == focusCmdline && msg.Type == tea.KeyEnter {
			input := a.cmdline.Value()
			a.cmdline.SetValue("")
			act, err := ParseCommand(input, a.defaultRepo)
			if err != nil {
				a.cmdresult.Append(ErrorStyle.Render("error: " + err.Error()))
				return a, nil
			}
			if act == nil {
				return a, nil
			}
			a.cmdresult.Append("> " + input)
			return a.runAction(act)
		}
	}

	// Forward to focused panel.
	var cmd tea.Cmd
	switch a.focus {
	case focusRunners:
		a.runners, cmd = a.runners.Update(msg)
	case focusTasks:
		a.tasks, cmd = a.tasks.Update(msg)
	case focusCmdline:
		a.cmdline, cmd = a.cmdline.Update(msg)
	}
	return a, cmd
}

func (a *App) cycleFocus(delta int) {
	a.runners.Blur()
	a.tasks.Blur()
	a.cmdline.Blur()

	a.focus = focus((int(a.focus) + delta + 4) % 4)

	switch a.focus {
	case focusRunners:
		a.runners.Focus()
	case focusTasks:
		a.tasks.Focus()
	case focusCmdline:
		a.cmdline.Focus()
	}
}

// layout computes per-panel sizes from a.width / a.height. Header 1, runners
// + tasks 10 each, cmdresult 5, cmdline 1, footer 1, plus 4 border rows
// distributed across panels = 22 reserved. Log gets the rest (min 5).
func (a *App) layout() {
	if a.width < 80 || a.height < 24 {
		return
	}
	half := a.width / 2
	a.runners.SetSize(half-2, 10)
	a.tasks.SetSize(a.width-half-2, 10)
	a.cmdresult.SetSize(a.width-2, 5)
	a.cmdline.Width = a.width - 4
}

func (a App) View() string {
	if a.width < 80 || a.height < 24 {
		return "terminal too small (need at least 80x24)"
	}

	connectedTag := ErrorStyle.Render("DISCONNECTED")
	if a.connected {
		connectedTag = OKStyle.Render("CONNECTED")
	}
	header := HeaderStyle.Render(fmt.Sprintf("harness-tui · %s · %s", a.server, connectedTag))

	runnersView := a.runners.View()
	tasksView := a.tasks.View()
	if a.runners.IsFocused() {
		runnersView = PanelStyleFocused.Render(runnersView)
	} else {
		runnersView = PanelStyle.Render(runnersView)
	}
	if a.tasks.IsFocused() {
		tasksView = PanelStyleFocused.Render(tasksView)
	} else {
		tasksView = PanelStyle.Render(tasksView)
	}
	top := lipgloss.JoinHorizontal(lipgloss.Top, runnersView, tasksView)

	// Log placeholder until Task 11 fills this in.
	logView := PanelStyle.
		Width(a.width - 2).
		Height(max(a.height-22, 5)).
		Render("(log will appear here when a task is selected)")

	cmdresultView := PanelStyle.Width(a.width - 2).Render(a.cmdresult.View())
	cmdlineView := a.cmdline.View()
	footer := FooterStyle.Render("tab focus · s submit · c cancel · enter follow · ? help · q quit")

	return strings.Join([]string{
		header,
		top,
		logView,
		cmdresultView,
		cmdlineView,
		footer,
	}, "\n")
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// runAction is the placeholder dispatch — actual implementations land in Task 13.
func (a App) runAction(act Action) (tea.Model, tea.Cmd) {
	switch act.(type) {
	case QuitAction:
		return a, tea.Quit
	case ClearAction:
		a.cmdresult.Clear()
		return a, nil
	case HelpAction:
		a.cmdresult.Append("commands: submit / cancel / prune / clear / help / quit")
		return a, nil
	default:
		a.cmdresult.Append(WarnStyle.Render("(action not yet implemented)"))
		return a, nil
	}
}
```

- [ ] **Step 2: Replace `cmd/harness-tui/main.go` with the bubbletea startup**

```go
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/tui"
)

var (
	serverAddr = flag.String("server", "localhost:8539", "harness-server host:port")
	repoFlag   = flag.String("repo", ".", "default repo path for submit popup")
)

func main() {
	flag.Parse()
	repoAbs, err := filepath.Abs(*repoFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "repo:", err)
		os.Exit(1)
	}
	app := tui.New(tui.Config{
		Server:      *serverAddr,
		DefaultRepo: repoAbs,
	})
	p := tea.NewProgram(app, tea.WithAltScreen(), tea.WithMouseAllMotion())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
```

- [ ] **Step 3: Build and run smoke test**

Run: `go build ./...`
Expected: clean.

Manual smoke (NOT in CI):
```
go run ./cmd/harness-tui
```
Expected:
- Layout renders, two empty tables on top, log placeholder middle, cmdresult below, cmdline at bottom.
- `Tab` cycles focus through runners → tasks → cmdline (skipping logs which is not yet a real model). The focused panel's border becomes the accent color.
- Type `help` then Enter → cmdresult shows the line.
- Type `quit` Enter → exits.
- `Ctrl+C` exits from any focus.

If anything visually broken at this stage, fix before moving on. The plan does not assume teatest.

- [ ] **Step 4: Commit**

```bash
git add tui/app.go cmd/harness-tui/main.go
git commit -m "tui: assemble app skeleton with focus rotation and parser dispatch" \
  -m "Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Phase 3: Live data integration

### Task 8: Persistent client wrapper (`client.go`)

**Files:**
- Create: `tui/client.go`

This task adds a `Client` that holds the long-lived `objproto.Connection` and exposes `tea.Cmd` factories for submit / cancel / prune. The connection is dialed in main.go and passed in.

- [ ] **Step 1: Write `tui/client.go`**

```go
package tui

import (
	"context"
	"crypto/ecdh"
	"fmt"
	"log/slog"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/transport"
	"github.com/on-keyday/agent-harness/trsf"
)

// portFrom returns the port part of "host:port".
func portFrom(addr string) string {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return addr
	}
	return addr[i+1:]
}

// Connect dials the harness-server and returns a connection ready for both
// pubsub subscriptions (via trsf streams) and ad-hoc TaskControl round-trips.
// Caller is responsible for closing both ctx and the returned conn.
func Connect(ctx context.Context, addr string) (objproto.Connection, trsf.Transport, error) {
	sess, err := transport.WebSocketSession(slog.Default(), addr, nil, objproto.SessionModeClient)
	if err != nil {
		return nil, nil, fmt.Errorf("ws session: %w", err)
	}
	cid := objproto.MustParseConnectionID(fmt.Sprintf("ws:127.0.0.1:%s-3333", portFrom(addr)))
	conn, err := objproto.DoECDHHandshake(ctx, sess, cid, ecdh.P521(), objproto.AES128GCM)
	if err != nil {
		return nil, nil, fmt.Errorf("ecdh: %w", err)
	}
	p := trsf.NewStreams(ctx, false, trsf.DefaultInitialMTU, trsf.DefaultMaxMTU, conn, slog.Default())
	go trsf.AutoSend(ctx, p, conn, nil)
	go trsf.AutoReceive(ctx, p, conn, func(*objproto.Message, error) {})
	go trsf.AutoPing(ctx, conn, 30*time.Second)
	return conn, p, nil
}

// --- tea.Cmd factories backed by the short-lived cli.* helpers ---
// Each opens its own connection (cli.Dial) for simplicity; they are short
// round-trips. If profiling shows this is a problem we can move them onto
// the persistent conn.

type SubmitResultMsg struct {
	TaskID string
	Err    error
	Echo   string // "submit /repo \"prompt\""
}

type CancelResultMsg struct {
	IDPrefix string
	Resolved string
	Err      error
}

type PruneResultMsg struct {
	Removed uint32
	Err     error
}

func DoSubmit(addr, repo, prompt string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		echo := fmt.Sprintf("submit --repo %q %q", repo, prompt)
		id, err := cli.Submit(ctx, addr, repo, prompt)
		return SubmitResultMsg{TaskID: id, Err: err, Echo: echo}
	}
}

// DoCancel: the caller resolves the id-prefix to a full id BEFORE invoking
// (lookup happens in app.go using the local tasks snapshot). If resolved == ""
// the action returns an error message without contacting the server.
func DoCancel(addr, idPrefix, resolved string) tea.Cmd {
	return func() tea.Msg {
		if resolved == "" {
			return CancelResultMsg{IDPrefix: idPrefix, Err: fmt.Errorf("no task matching prefix %q", idPrefix)}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := cli.Cancel(ctx, addr, resolved)
		return CancelResultMsg{IDPrefix: idPrefix, Resolved: resolved, Err: err}
	}
}

func DoPruneTasks(addr string, before time.Duration) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cutoff := time.Now().Add(-before)
		removed, err := cli.PruneTasks(ctx, addr, cutoff)
		return PruneResultMsg{Removed: removed, Err: err}
	}
}
```

- [ ] **Step 2: Build & commit**

```bash
go build ./...
git add tui/client.go
git commit -m "tui: add client wrappers and Connect helper" \
  -m "Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

### Task 9: Pubsub event bridge (`events.go`)

**Files:**
- Create: `tui/events.go`, `tui/events_test.go`

This task spawns goroutines that JOIN the `tasks.status` and `runners.status` topics on the persistent connection, accept the topic streams the broker opens, and forward decoded events as `tea.Msg` via `program.Send`.

- [ ] **Step 1: Write the failing decode-helper tests**

```go
// tui/events_test.go
package tui

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestDecodeTaskStatusEvent(t *testing.T) {
	orig := protocol.TaskStatusEvent{
		Kind:       protocol.StatusEventKind_TaskQueued,
		Ts:         1234567,
		TaskStatus: protocol.TaskStatus_Queued,
		ExitCode:   0,
	}
	orig.TaskId.Id[0] = 0xAB
	encoded := orig.MustAppend(nil)

	got, err := DecodeTaskStatus(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != orig.Kind {
		t.Errorf("Kind got=%v want=%v", got.Kind, orig.Kind)
	}
	if got.TaskId.Id[0] != 0xAB {
		t.Errorf("TaskId byte: %x", got.TaskId.Id[0])
	}
}

func TestDecodeRunnerStatusEvent(t *testing.T) {
	orig := protocol.RunnerStatusEvent{
		Kind:         protocol.StatusEventKind_RunnerRegistered,
		Ts:           42,
		RunnerStatus: protocol.RunnerStatus_Idle,
	}
	// RunnerID encoder requires IpAddrLen ∈ {4,16}; populate a placeholder.
	orig.RunnerId.SetTransport([]byte("ws"))
	orig.RunnerId.SetIpAddr([]byte{127, 0, 0, 1})
	encoded := orig.MustAppend(nil)

	got, err := DecodeRunnerStatus(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got.RunnerStatus != orig.RunnerStatus {
		t.Errorf("RunnerStatus got=%v", got.RunnerStatus)
	}
}
```

- [ ] **Step 2: Run, verify FAIL**

Run: `go test ./tui/... -v -run TestDecode`
Expected: FAIL (DecodeTaskStatus / DecodeRunnerStatus undefined).

- [ ] **Step 3: Implement `tui/events.go`**

```go
package tui

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/pubsub"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/topics"
	"github.com/on-keyday/agent-harness/trsf"
)

// Messages dispatched into the tea.Program from the pubsub bridge.
type SnapshotMsg struct {
	Runners []protocol.RunnerInfo
	Tasks   []protocol.TaskInfo
	Err     error
}

type TaskEventMsg struct {
	Event protocol.TaskStatusEvent
}

type RunnerEventMsg struct {
	Event protocol.RunnerStatusEvent
}

type LogChunkMsg struct {
	TaskID string // hex; empty if from the topic header before payload
	Chunk  []byte
}

type ConnectionMsg struct {
	Connected bool
	Err       error
}

// DecodeTaskStatus decodes a TaskStatusEvent payload. Returns the decoded value
// (not a pointer) along with any decode error.
func DecodeTaskStatus(payload []byte) (protocol.TaskStatusEvent, error) {
	var ev protocol.TaskStatusEvent
	if _, err := ev.Decode(payload); err != nil {
		return protocol.TaskStatusEvent{}, fmt.Errorf("decode TaskStatusEvent: %w", err)
	}
	return ev, nil
}

// DecodeRunnerStatus decodes a RunnerStatusEvent payload.
func DecodeRunnerStatus(payload []byte) (protocol.RunnerStatusEvent, error) {
	var ev protocol.RunnerStatusEvent
	if _, err := ev.Decode(payload); err != nil {
		return protocol.RunnerStatusEvent{}, fmt.Errorf("decode RunnerStatusEvent: %w", err)
	}
	return ev, nil
}

// SubscribeTaskStatus issues a JOIN for tasks.status, accepts the resulting
// stream, and forwards each decoded event as TaskEventMsg via program.Send.
// Returns once ctx is cancelled or the stream errors out.
func SubscribeTaskStatus(ctx context.Context, conn objproto.Connection, p trsf.Transport, program *tea.Program) {
	subscribeAndStream(ctx, conn, p, topics.TasksStatus(), program, func(payload []byte) tea.Msg {
		ev, err := DecodeTaskStatus(payload)
		if err != nil {
			slog.Warn("decode task event", "err", err)
			return nil
		}
		return TaskEventMsg{Event: ev}
	})
}

// SubscribeRunnerStatus mirror of SubscribeTaskStatus for runners.status.
func SubscribeRunnerStatus(ctx context.Context, conn objproto.Connection, p trsf.Transport, program *tea.Program) {
	subscribeAndStream(ctx, conn, p, topics.RunnersStatus(), program, func(payload []byte) tea.Msg {
		ev, err := DecodeRunnerStatus(payload)
		if err != nil {
			slog.Warn("decode runner event", "err", err)
			return nil
		}
		return RunnerEventMsg{Event: ev}
	})
}

// SubscribeTaskLog joins task.<taskID>.log and forwards each chunk as
// LogChunkMsg{TaskID: taskID}. Caller is expected to filter on TaskID at the
// consumer side because rapid tab-switching may interleave streams briefly.
func SubscribeTaskLog(ctx context.Context, conn objproto.Connection, p trsf.Transport, program *tea.Program, taskID string) {
	subscribeAndStream(ctx, conn, p, topics.TaskLog(taskID), program, func(payload []byte) tea.Msg {
		chunk := make([]byte, len(payload))
		copy(chunk, payload)
		return LogChunkMsg{TaskID: taskID, Chunk: chunk}
	})
}

// subscribeAndStream sends a JOIN, accepts the next stream, discards the topic
// header, then delivers payload chunks via fn(payload) → program.Send.
func subscribeAndStream(ctx context.Context, conn objproto.Connection, p trsf.Transport, topic string, program *tea.Program, fn func([]byte) tea.Msg) {
	joinBytes := pubsub.JoinTopic("tui", topic)
	if _, _, err := conn.SendMessage(joinBytes); err != nil {
		slog.Warn("JOIN failed", "topic", topic, "err", err)
		return
	}
	st, err := p.AcceptBidirectionalStream(ctx)
	if err != nil {
		slog.Warn("accept stream failed", "topic", topic, "err", err)
		return
	}
	// Topic-header line: byte-by-byte until '\n'.
	for {
		data, eof, err := st.ReadDirect(1)
		if err != nil || eof {
			return
		}
		if len(data) > 0 && data[0] == '\n' {
			break
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		data, eof, err := st.ReadDirect(64 * 1024)
		if err != nil {
			return
		}
		if len(data) > 0 {
			if msg := fn(data); msg != nil {
				program.Send(msg)
			}
		}
		if eof {
			return
		}
	}
}

// FormatTaskID returns the hex string for a TaskID, exposed for app.go.
func FormatTaskID(t protocol.TaskID) string {
	return hex.EncodeToString(t.Id[:])
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./tui/... -v -run TestDecode`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tui/events.go tui/events_test.go
git commit -m "tui: add pubsub event bridge with decode tests" \
  -m "Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

### Task 10: Wire initial snapshot + live events into tables

**Files:**
- Modify: `cmd/harness-tui/main.go`, `tui/app.go`

This task extends `App` to consume `SnapshotMsg`, `TaskEventMsg`, `RunnerEventMsg`, and `ConnectionMsg`, and main.go to dial the server, fetch an initial `cli.List`, spawn the subscriber goroutines, and feed the program.

- [ ] **Step 1: Extend `App` with a local model of runners and tasks**

In `tui/app.go`, add fields and message handlers. Add to `App`:

```go
// in struct App:
allRunners map[string]protocol.RunnerInfo // keyed by hex of RunnerId... but RunnerId isn't a stable hex; use repo+something
allTasks   map[string]protocol.TaskInfo   // keyed by hex(TaskId)
```

Wait — `RunnerStatusEvent` doesn't carry a useful runner ID for keying (we placeholder it server-side). So we cannot patch individual runner rows reliably from events; instead we treat runners.status as a "kick to refresh" trigger and re-fetch via `cli.List`. Tasks DO carry a real TaskID and can be patched per-event.

Replace the simple field plan with:

```go
// in struct App:
tasksByID map[string]protocol.TaskInfo // hex(TaskId) → snapshot
runnersSnapshot []protocol.RunnerInfo // refreshed on demand (initial load + every runner event)
needRunnerRefresh bool
```

The Update handlers:

```go
// in (a App) Update — add cases before the focus-forwarding switch:
case SnapshotMsg:
	if msg.Err != nil {
		a.cmdresult.Append(ErrorStyle.Render("snapshot: " + msg.Err.Error()))
		return a, nil
	}
	a.runnersSnapshot = msg.Runners
	a.runners.SetRows(msg.Runners)
	a.tasksByID = make(map[string]protocol.TaskInfo, len(msg.Tasks))
	for _, t := range msg.Tasks {
		a.tasksByID[FormatTaskID(t.Id)] = t
	}
	a.refreshTasksTable()
	return a, nil

case TaskEventMsg:
	id := FormatTaskID(msg.Event.TaskId)
	cur, ok := a.tasksByID[id]
	if !ok {
		// Brand-new task: minimal entry; the list table will show "(unknown)" prompt
		// until the next snapshot refresh.
		var ti protocol.TaskInfo
		ti.Id = msg.Event.TaskId
		ti.Status = msg.Event.TaskStatus
		ti.CreatedAt = msg.Event.Ts
		a.tasksByID[id] = ti
	} else {
		cur.Status = msg.Event.TaskStatus
		if msg.Event.Kind == protocol.StatusEventKind_TaskEnded {
			cur.ExitCode = msg.Event.ExitCode
			cur.EndedAt = msg.Event.Ts
		}
		a.tasksByID[id] = cur
	}
	a.refreshTasksTable()
	return a, nil

case RunnerEventMsg:
	// We can't reliably key the snapshot's RunnerInfo to this event because
	// the server fills RunnerStatusEvent.RunnerId with a placeholder. Schedule
	// a snapshot refresh.
	return a, RefreshSnapshot(a.server)

case ConnectionMsg:
	a.connected = msg.Connected
	if !msg.Connected && msg.Err != nil {
		a.cmdresult.Append(ErrorStyle.Render("disconnected: " + msg.Err.Error()))
	}
	return a, nil
```

Add helpers in `tui/app.go`:

```go
func (a *App) refreshTasksTable() {
	// Sort by CreatedAt desc and clamp to 100 most-recent so the table doesn't grow without bound.
	all := make([]protocol.TaskInfo, 0, len(a.tasksByID))
	for _, t := range a.tasksByID {
		all = append(all, t)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt > all[j].CreatedAt })
	if len(all) > 100 {
		all = all[:100]
	}
	a.tasks.SetRows(all)
}
```

Add to imports: `"sort"`, `"github.com/on-keyday/agent-harness/runner/protocol"`.

- [ ] **Step 2: Add `RefreshSnapshot` tea.Cmd in `tui/client.go`**

```go
// RefreshSnapshot calls cli.List and dispatches a SnapshotMsg.
func RefreshSnapshot(addr string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c, err := cli.Dial(ctx, addr)
		if err != nil {
			return SnapshotMsg{Err: err}
		}
		defer c.Close()

		// Build a TaskControlRequest{List}, decode response.
		// Cleaner: piggyback on existing cli helper if it exists.
		// (Use the same approach as cli.List which writes to an io.Writer.
		// We re-encode ourselves to get structured TaskInfo / RunnerInfo back.)
		req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_List}
		req.SetList(protocol.ListQuery{})
		resp, err := roundTripList(c, req)
		if err != nil {
			return SnapshotMsg{Err: err}
		}
		lr := resp.List()
		if lr == nil {
			return SnapshotMsg{Err: fmt.Errorf("empty list response")}
		}
		// Copy slices so the caller can hold them without aliasing the response.
		runners := make([]protocol.RunnerInfo, len(lr.Runners))
		copy(runners, lr.Runners)
		tasks := make([]protocol.TaskInfo, len(lr.Tasks))
		copy(tasks, lr.Tasks)
		return SnapshotMsg{Runners: runners, Tasks: tasks}
	}
}

// roundTripList sends a TaskControl request through an existing Client.
// We can't use cli.roundTripTaskControl directly (unexported), so build it inline.
func roundTripList(c *cli.Client, req *protocol.TaskControlRequest) (*protocol.TaskControlResponse, error) {
	conn := c.Conn()
	data := req.MustAppend([]byte{byte(wire.ApplicationPayloadKind_TaskControl)})
	if _, _, err := conn.SendMessage(data); err != nil {
		return nil, err
	}
	msg, err := conn.ReceiveMessage()
	if err != nil {
		return nil, err
	}
	if len(msg.Data) == 0 || wire.ApplicationPayloadKind(msg.Data[0]) != wire.ApplicationPayloadKind_TaskControl {
		return nil, fmt.Errorf("unexpected response kind")
	}
	resp := &protocol.TaskControlResponse{}
	if _, err := resp.Decode(msg.Data[1:]); err != nil {
		return nil, err
	}
	return resp, nil
}
```

Add imports: `"github.com/on-keyday/agent-harness/runner/protocol"` and `"github.com/on-keyday/agent-harness/trsf/wire"`.

(Note: `cli.Client.Conn()` is exported per Task 4.1 of the previous plan; verify with `grep "func (c \*Client) Conn" cli/client.go`. If not exported, expose it.)

- [ ] **Step 3: Replace `cmd/harness-tui/main.go` with the full startup**

```go
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/tui"
)

var (
	serverAddr = flag.String("server", "localhost:8539", "harness-server host:port")
	repoFlag   = flag.String("repo", ".", "default repo path for submit popup")
)

func main() {
	flag.Parse()
	repoAbs, err := filepath.Abs(*repoFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "repo:", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	app := tui.New(tui.Config{
		Server:      *serverAddr,
		DefaultRepo: repoAbs,
	})
	program := tea.NewProgram(app, tea.WithAltScreen())

	// Connect, then spawn pubsub subscribers and an initial snapshot fetch.
	go func() {
		conn, p, err := tui.Connect(ctx, *serverAddr)
		if err != nil {
			program.Send(tui.ConnectionMsg{Connected: false, Err: err})
			return
		}
		program.Send(tui.ConnectionMsg{Connected: true})
		program.Send(tui.RefreshSnapshot(*serverAddr)())
		go tui.SubscribeTaskStatus(ctx, conn, p, program)
		go tui.SubscribeRunnerStatus(ctx, conn, p, program)
		<-ctx.Done()
	}()

	if _, err := program.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// Allow goroutines to drain briefly.
	time.Sleep(50 * time.Millisecond)
}
```

- [ ] **Step 4: Build & smoke test**

Run: `go build ./...`
Expected: clean.

Manual smoke (assumes you have `harness-server` and at least one `agent-runner` running locally):
```
go run ./cmd/harness-tui --server localhost:8539
```
Expected: header shows CONNECTED. Both tables populate within ~1s. Submit a task via `harness-cli submit --task hi` from another shell — within ~1s the task should appear in the tasks table with `Queued`, then `Running`, then `Done`.

If it does not, common diagnoses:
- Pubsub stream order: ensure the JOINs are sequential (already are in main.go).
- decode error: log via slog will appear on stderr (not the TUI screen).

- [ ] **Step 5: Commit**

```bash
git add tui/app.go tui/client.go cmd/harness-tui/main.go
git commit -m "tui: wire initial snapshot and pubsub events into tables" \
  -m "Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

### Task 11: Log viewport (`logs.go`)

**Files:**
- Create: `tui/logs.go`
- Modify: `tui/app.go`, `cmd/harness-tui/main.go`

The log viewport displays bytes from `task.<id>.log` for the currently-selected task. Pressing Enter (or `f`) on the tasks table starts following the highlighted task; switching to a different task LEAVEs the previous one and JOINs the new.

LEAVE is implemented by cancelling the per-stream context. The simplest pattern is to keep a `context.CancelFunc` per active log subscription and cancel it on switch.

- [ ] **Step 1: Write `tui/logs.go`**

```go
package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

type LogsModel struct {
	vp     viewport.Model
	taskID string
	lines  []string
}

func NewLogs() LogsModel {
	vp := viewport.New(80, 10)
	vp.SetContent("(no task selected)")
	return LogsModel{vp: vp}
}

func (m *LogsModel) SetSize(w, h int) {
	m.vp.Width = w
	m.vp.Height = h
}

// Reset clears the viewport and sets the task ID we're following.
// taskID == "" means no task selected.
func (m *LogsModel) Reset(taskID string) {
	m.taskID = taskID
	m.lines = nil
	if taskID == "" {
		m.vp.SetContent("(no task selected)")
	} else {
		m.vp.SetContent("(following " + taskID[:12] + "…)")
	}
}

// TaskID returns which task we're currently following, or "" if none.
func (m *LogsModel) TaskID() string { return m.taskID }

// Append appends a chunk of bytes (already prefixed by the runner with [out]/[err]).
// Chunks may contain partial lines; we keep them as-is.
func (m *LogsModel) Append(chunk []byte) {
	if m.taskID == "" {
		return
	}
	m.lines = append(m.lines, string(chunk))
	if len(m.lines) > 1000 {
		m.lines = m.lines[len(m.lines)-1000:]
	}
	m.vp.SetContent(strings.Join(m.lines, ""))
	m.vp.GotoBottom()
}

func (m LogsModel) Update(msg tea.Msg) (LogsModel, tea.Cmd) {
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

func (m LogsModel) View() string { return m.vp.View() }
```

- [ ] **Step 2: Wire into `App`**

In `tui/app.go`:

1. Add a `logs LogsModel` field; init it in `New`.
2. Add a `logsCancel context.CancelFunc` field; protect `App` struct mutations through pointer receivers where needed.
3. Replace the placeholder log render in `View` with `a.logs.View()` wrapped in a panel border.
4. Add a `LogChunkMsg` handler in `Update` that appends only if `msg.TaskID == a.logs.TaskID()`.
5. On the tasks table receiving an `Enter` keystroke (you must intercept it before forwarding to the table, OR check after):
   - Compute `newID := a.tasks.SelectedID()`.
   - If different from `a.logs.TaskID()`: call `a.followTask(newID)`.

Minimal `followTask` skeleton (the goroutine spawn happens via `tea.Cmd`-returned closure that captures the persistent `conn` / `p`):

```go
// in App we need access to the persistent conn/transport. Add fields:
//   conn objproto.Connection
//   trans trsf.Transport
//   ctx  context.Context  // app-level context, set by main
// and a constructor variant New(cfg, ctx, conn, trans) — OR pass them in via a
// SetTransport(ctx, conn, trans) method called from main.go after Connect.

func (a *App) followTask(taskID string) tea.Cmd {
	if a.logsCancel != nil {
		a.logsCancel()
	}
	a.logs.Reset(taskID)
	if taskID == "" || a.conn == nil {
		return nil
	}
	subCtx, cancel := context.WithCancel(a.ctx)
	a.logsCancel = cancel
	return func() tea.Msg {
		go SubscribeTaskLog(subCtx, a.conn, a.trans, a.program, taskID)
		return nil
	}
}
```

Note we need `a.program *tea.Program` — this is awkward because the Program is owned by main. Workaround: define a small `Sender interface { Send(tea.Msg) }`, accept it in `App`. main.go passes `program`. The events.go Subscribe* functions already accept a `*tea.Program`; either pass the program explicitly or change their signatures to `Sender`. Simpler: pass `program` via a setter:

```go
// in App:
//   program *tea.Program
func (a *App) BindProgram(p *tea.Program) { a.program = p }

// and SubscribeTaskLog already takes *tea.Program; pass a.program.
```

Update `cmd/harness-tui/main.go` to call `app.BindProgram(program)` immediately after `tea.NewProgram` — but App is currently a value, not a pointer. Two options:
- Use `*App` everywhere (Bubble Tea is OK with `*Model` Init/Update/View; just be sure to return the same pointer from Update).
- Keep App as value but stash these on package-globals (bad).

Going with `*App`. Update `New` to return `*App`, update Init/Update/View receivers, update main.go:

```go
app := tui.New(tui.Config{...})  // now *App
program := tea.NewProgram(app, tea.WithAltScreen())
app.BindProgram(program)
app.BindContext(ctx)
// In the Connect goroutine, after success:
app.BindTransport(conn, p)
```

Add `BindContext(ctx)` and `BindTransport(conn, p)` methods on `*App`.

- [ ] **Step 3: Tasks table Enter intercept**

In Update's KeyMsg branch, BEFORE forwarding to the focused panel, intercept Enter when focus is tasks:

```go
if a.focus == focusTasks && msg.Type == tea.KeyEnter {
	id := a.tasks.SelectedID()
	if id != "" {
		return a, a.followTask(id)
	}
	return a, nil
}
```

- [ ] **Step 4: Build & smoke**

Run: `go build ./...`
Manual: with server + runner up, submit a task that sleeps. Tab to tasks table, ↓/↑ to select, Enter — log starts streaming in the middle viewport.

- [ ] **Step 5: Commit**

```bash
git add tui/logs.go tui/app.go cmd/harness-tui/main.go
git commit -m "tui: add log viewport with JOIN/LEAVE on task selection" \
  -m "Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

### Task 12: Submit popup (`popup.go`)

**Files:**
- Create: `tui/popup.go`
- Modify: `tui/app.go`

A modal overlay with a multi-line `bubbles/textarea`. `s` opens it; `Ctrl+Enter` submits via `DoSubmit`; `Esc` cancels.

- [ ] **Step 1: Write `tui/popup.go`**

```go
package tui

import (
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type PopupModel struct {
	repo string
	ta   textarea.Model
	open bool
}

func NewPopup(defaultRepo string) PopupModel {
	ta := textarea.New()
	ta.Placeholder = "Type the prompt for claude. Ctrl+Enter to submit, Esc to cancel."
	ta.SetWidth(60)
	ta.SetHeight(10)
	return PopupModel{repo: defaultRepo, ta: ta}
}

func (m *PopupModel) IsOpen() bool { return m.open }

func (m *PopupModel) Open() {
	m.open = true
	m.ta.Reset()
	m.ta.Focus()
}

func (m *PopupModel) Close() {
	m.open = false
	m.ta.Blur()
}

func (m *PopupModel) Repo() string   { return m.repo }
func (m *PopupModel) Prompt() string { return m.ta.Value() }

func (m *PopupModel) SetRepo(r string) { m.repo = r }

func (m PopupModel) Update(msg tea.Msg) (PopupModel, tea.Cmd) {
	if !m.open {
		return m, nil
	}
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	return m, cmd
}

func (m PopupModel) View() string {
	if !m.open {
		return ""
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorFocused).
		Padding(1, 2)
	header := "New task — repo: " + m.repo
	footer := FooterStyle.Render("Ctrl+Enter: submit  ·  Esc: cancel")
	return box.Render(header + "\n\n" + m.ta.View() + "\n\n" + footer)
}
```

- [ ] **Step 2: Wire popup into App**

In `tui/app.go`:

1. Add `popup PopupModel` field; init in `New`.
2. In `Update`, handle the open/submit/cancel keys BEFORE other dispatch:

```go
case tea.KeyMsg:
	if a.popup.IsOpen() {
		switch msg.Type {
		case tea.KeyEsc:
			a.popup.Close()
			return a, nil
		case tea.KeyCtrlJ: // bubbletea sends Ctrl+J for Ctrl+Enter on most terms
			repo := a.popup.Repo()
			prompt := a.popup.Prompt()
			a.popup.Close()
			if prompt == "" {
				a.cmdresult.Append(WarnStyle.Render("submit cancelled (empty prompt)"))
				return a, nil
			}
			return a, DoSubmit(a.server, repo, prompt)
		}
		var cmd tea.Cmd
		a.popup, cmd = a.popup.Update(msg)
		return a, cmd
	}
	// Existing key dispatch follows...
```

(`Ctrl+Enter` is reported as `KeyCtrlJ` on most terminals; confirm by checking what `msg.String()` prints during smoke. If it's `ctrl+m` or `enter`, adjust. As a fallback also accept `tea.KeyCtrlD` — document the chosen key in the popup view.)

3. Add `s` shortcut in the global keys (NOT when popup or cmdline is focused):

```go
if a.focus != focusCmdline && msg.String() == "s" {
	a.popup.SetRepo(a.defaultRepo)
	a.popup.Open()
	return a, nil
}
```

4. Render the popup overlay in `View`. If `a.popup.IsOpen()`, render the popup centered; otherwise render the normal layout.

```go
view := strings.Join(...) // existing layout
if a.popup.IsOpen() {
	overlay := lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.popup.View())
	return overlay // simplest; loses background but works for v1
}
return view
```

5. Handle `SubmitResultMsg`:

```go
case SubmitResultMsg:
	if msg.Err != nil {
		a.cmdresult.Append(ErrorStyle.Render("submit failed: " + msg.Err.Error()))
		return a, nil
	}
	a.cmdresult.Append(OKStyle.Render("submitted: ") + msg.TaskID[:12])
	return a, nil
```

- [ ] **Step 3: Build & smoke**

Run: `go build ./...`
Manual: press `s`, type a prompt, `Ctrl+Enter`. Verify: popup closes, cmdresult shows "submitted: <id>", tasks table shows new Queued task within ~1s.

- [ ] **Step 4: Commit**

```bash
git add tui/popup.go tui/app.go
git commit -m "tui: add submit multiline popup" \
  -m "Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

### Task 13: Cmdline action dispatch (cancel / prune / clear / quit / help)

**Files:**
- Modify: `tui/app.go`

Replace the placeholder `runAction` with real implementations. Cancel resolves the id-prefix locally against `a.tasksByID`; prune calls server-side prune for tasks (the worktree-side cleanup is intentionally NOT in the TUI v1 — users still run `harness-cli prune` for that).

- [ ] **Step 1: Add an id-prefix resolver helper in `tui/app.go`**

```go
// resolveTaskIDPrefix returns the full hex id matching prefix (case-insensitive).
// Returns "" if zero or multiple matches.
func (a *App) resolveTaskIDPrefix(prefix string) (string, string) {
	prefix = strings.ToLower(prefix)
	var matches []string
	for id := range a.tasksByID {
		if strings.HasPrefix(id, prefix) {
			matches = append(matches, id)
		}
	}
	switch len(matches) {
	case 0:
		return "", "no task matches " + prefix
	case 1:
		return matches[0], ""
	default:
		return "", fmt.Sprintf("ambiguous prefix %q matches %d tasks", prefix, len(matches))
	}
}
```

(Imports: `"strings"`, `"fmt"`.)

- [ ] **Step 2: Replace `runAction`**

```go
func (a *App) runAction(act Action) (tea.Model, tea.Cmd) {
	switch v := act.(type) {
	case QuitAction:
		return a, tea.Quit
	case ClearAction:
		a.cmdresult.Clear()
		return a, nil
	case HelpAction:
		a.cmdresult.Append("commands: submit / cancel <id> / prune [--before=DUR] [--offline] / clear / help / quit")
		return a, nil
	case SubmitAction:
		return a, DoSubmit(a.server, v.Repo, v.Prompt)
	case CancelAction:
		full, errStr := a.resolveTaskIDPrefix(v.IDPrefix)
		if errStr != "" {
			a.cmdresult.Append(ErrorStyle.Render(errStr))
			return a, nil
		}
		return a, DoCancel(a.server, v.IDPrefix, full)
	case PruneAction:
		if v.Offline {
			a.cmdresult.Append(WarnStyle.Render("--offline is a CLI-only flag; use harness-cli prune --offline. Server-side prune skipped."))
			return a, nil
		}
		return a, DoPruneTasks(a.server, v.Before)
	}
	a.cmdresult.Append(WarnStyle.Render(fmt.Sprintf("(unhandled action %T)", act)))
	return a, nil
}
```

- [ ] **Step 3: Add result handlers in Update**

```go
case CancelResultMsg:
	if msg.Err != nil {
		a.cmdresult.Append(ErrorStyle.Render("cancel failed: " + msg.Err.Error()))
		return a, nil
	}
	a.cmdresult.Append(OKStyle.Render("cancelled ") + msg.Resolved[:12])
	return a, nil

case PruneResultMsg:
	if msg.Err != nil {
		a.cmdresult.Append(ErrorStyle.Render("prune failed: " + msg.Err.Error()))
		return a, nil
	}
	a.cmdresult.Append(OKStyle.Render(fmt.Sprintf("pruned %d task(s)", msg.Removed)))
	// Refresh local snapshot since the server forgot some tasks.
	return a, RefreshSnapshot(a.server)
```

- [ ] **Step 4: Add the `c` shortcut for cancel from the tasks table**

In the tasks-focus branch of Update, intercept `c`:

```go
if a.focus == focusTasks && msg.String() == "c" {
	id := a.tasks.SelectedID()
	if id == "" {
		a.cmdresult.Append(WarnStyle.Render("no task selected"))
		return a, nil
	}
	return a, DoCancel(a.server, id, id)
}
```

- [ ] **Step 5: Build & smoke**

Run: `go build ./...`
Manual: submit a long-running task (e.g., a sleep wrapper). Type `cancel <prefix>` in cmdline → row flips to `Cancel`. Or focus tasks, select row, press `c` → same.

- [ ] **Step 6: Commit**

```bash
git add tui/app.go
git commit -m "tui: implement cmdline actions (cancel/prune/clear/help)" \
  -m "Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

### Task 14: Disconnection detection + reconnect ticker

**Files:**
- Modify: `cmd/harness-tui/main.go`, `tui/app.go`

If the persistent connection drops, display DISCONNECTED in the header and retry every 3s. On reconnect: refresh snapshot, re-spawn subscribers.

- [ ] **Step 1: Wrap the connect-and-subscribe routine into a function that monitors readiness**

In `cmd/harness-tui/main.go`:

```go
go func() {
	for {
		if ctx.Err() != nil {
			return
		}
		connCtx, connCancel := context.WithCancel(ctx)
		conn, p, err := tui.Connect(connCtx, *serverAddr)
		if err != nil {
			program.Send(tui.ConnectionMsg{Connected: false, Err: err})
			connCancel()
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
				continue
			}
		}
		app.BindTransport(conn, p)
		program.Send(tui.ConnectionMsg{Connected: true})
		program.Send(tui.RefreshSnapshot(*serverAddr)())
		go tui.SubscribeTaskStatus(connCtx, conn, p, program)
		go tui.SubscribeRunnerStatus(connCtx, conn, p, program)

		// Watch the connection. trsf.AutoReceive returns when the conn closes.
		<-connCtx.Done() // we cancel only on shutdown; for now disconnect detection is via Send failure in the goroutines

		connCancel()
		// brief pause before retrying.
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}()
```

For v1 keep this simple: if the WS conn drops, the SendMessage call in Subscribe* will fail and that goroutine exits. We don't have an explicit OnDisconnect callback yet. The reconnect happens when the user `Ctrl+C`s and restarts — same as `harness-cli watch`.

A more robust v1: add a heartbeat-based liveness check. Simplest: every 5s send a ping via cli.Dial(); if it fails, dispatch `ConnectionMsg{Connected: false}`. Defer if pressed for time — note in spec §9.

For this task, just dispatch the initial ConnectionMsg and let v1 ship without auto-reconnect. Update spec §6 line "retry every 3s" → "manual restart on drop in v1".

Apply to `tui/app.go`: the View already handles `connected = false` correctly.

Update `docs/superpowers/specs/2026-04-25-harness-tui-design.md` to reflect the deferral:

```diff
-Reconnection: if the underlying conn drops, dispatch a `disconnectedMsg`. The header bar shows a "DISCONNECTED" indicator and the TUI retries `cli.Dial` every 3 seconds. Tables and log viewport keep their last known state; events resume on reconnect.
+Reconnection: v1 detects loss only via cmdline-level RPC failures (each round-trip dials its own conn). If the persistent pubsub conn drops silently, events stop arriving but the header keeps showing CONNECTED. Manual restart needed. Auto-reconnect is a v2 item — see open questions.
```

- [ ] **Step 2: Build & smoke**

Run: `go build ./...`
Manual: kill the server while the TUI is running. Submit a task → cmdresult shows error. Restart server, restart TUI.

- [ ] **Step 3: Commit (incl. spec update)**

```bash
git add tui/app.go cmd/harness-tui/main.go docs/superpowers/specs/2026-04-25-harness-tui-design.md
git commit -m "tui: connect / disconnect plumbing; defer auto-reconnect to v2" \
  -m "Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

### Task 15: README update

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Add a `harness-tui` section to README**

Insert before the `## v1 limitations / non-goals` section:

```markdown
## TUI

`cmd/harness-tui` is an interactive terminal frontend (Bubble Tea) that bundles
submit / ls / logs / cancel / prune / watch into a single screen.

```bash
go run ./cmd/harness-tui --server localhost:8539 --repo /abs/path/to/repo
```

Layout:

```
┌── Runners ────────┐ ┌── Tasks ─────────────────────┐
│ Idle  /home/foo  ··│ │ Queued  9d50  prompt...      │
│ Busy  /home/foo  ··│ │ Running abcd  prompt...      │
└────────────────────┘ └──────────────────────────────┘
┌── Log: <selected task> ─────────────────────────────┐
│ [out] hello                                          │
│ [err] ...                                            │
└──────────────────────────────────────────────────────┘
┌── Last command output ──────────────────────────────┐
│ submitted: 9d508...                                  │
└──────────────────────────────────────────────────────┘
> [cmdline]
tab focus · s submit · enter follow · c cancel · ? help · q quit
```

Keys:

| Key | Action |
|---|---|
| `Tab` / `Shift+Tab` | Cycle focus runners → tasks → cmdline |
| `s` | Open the multi-line submit popup (`Ctrl+Enter` to send, `Esc` to cancel) |
| `Enter` (tasks focus) | Follow the selected task's log |
| `c` (tasks focus) | Cancel the selected task |
| `q`, `Ctrl+C` | Quit |

The cmdline accepts `submit / cancel / prune / clear / help / quit`.
```

- [ ] **Step 2: Build & commit**

```bash
go build ./...
git add README.md
git commit -m "docs: README section for harness-tui" \
  -m "Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Self-review checklist

1. **Spec coverage**:
   - §1 Goal: Tasks 1-15 collectively build cmd/harness-tui ✓
   - §3 Architecture: file structure mirrored in §"File structure" above ✓
   - §3.1 Data flow: pubsub bridge implemented in Task 9, wired in Task 10 ✓
   - §4.1 Layout: Tasks 4-7 (panels) + 11 (logs) + 12 (popup); header line wired in Task 7 ✓
   - §4.2 Keybindings: Task 7 (focus + global), Task 12 (`s`/popup keys), Task 13 (`c` cancel from tasks) ✓
   - §4.3 cmdline commands: Task 3 (parser) + Task 13 (dispatch) ✓
   - §5 Real-time updates: Task 9 (events) + Task 10 (initial+wire) + Task 11 (logs) ✓
   - §6 Error handling: covered piecewise in Task 13 (RPC errors → cmdresult) and Task 14 (connection state) ✓
   - §7 Configuration: Task 7 (flags) ✓
   - §10 Testing: parser (Task 3) + decode helpers (Task 9) ✓
   - §11 Implementation order: matches Tasks 1→15 ✓

2. **Placeholders**: none. Each step has the actual code or exact command.

3. **Type consistency**:
   - `ParseCommand` / `Action` / `SubmitAction` / `CancelAction` / `PruneAction` / `ClearAction` / `QuitAction` / `HelpAction` consistent across Tasks 3 and 13.
   - `App` field names (`runners`, `tasks`, `logs`, `cmdresult`, `popup`, `cmdline`, `server`, `defaultRepo`, `connected`, `focus`, `width`, `height`, `tasksByID`, `runnersSnapshot`, `conn`, `trans`, `ctx`, `program`, `logsCancel`) consistent across Tasks 7, 10, 11, 12, 13, 14.
   - `SubmitResultMsg` / `CancelResultMsg` / `PruneResultMsg` / `SnapshotMsg` / `TaskEventMsg` / `RunnerEventMsg` / `ConnectionMsg` / `LogChunkMsg` consistent in Tasks 8, 9, 10, 12, 13.

4. **Open known limitations** (acknowledge in spec, not gaps):
   - Auto-reconnect deferred (Task 14 spec patch)
   - Mouse / scroll-wheel deferred (spec §9)
   - teatest UI snapshot tests deferred (spec §10)

If during implementation a step turns out to be wrong, fix the plan inline before proceeding to the next task.
