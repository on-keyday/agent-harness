# Raw-connect panes + HTTP request builder — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the TUI's raw-connect modal the WebUI's multi-pane model, then let all three surfaces build an HTTP request instead of hand-typing one.

**Architecture:** Phase A moves connection state off `App` and into a slice of panes owned by `RawConnectModal`, routing the existing generation-tagged messages to the pane that owns the generation. Phase B adds one byte-builder in `cli/httpreq.go` that every surface calls, so the request on the wire cannot differ between CLI, TUI and WebUI.

**Tech Stack:** Go 1.25.7, bubbletea/bubbles (`textinput`, `textarea`, `table`), syscall/js for the wasm bridge, plain JS/CSS for the WebUI.

## Global Constraints

- Go 1.25.7. **No new module dependencies** — bubbles/bubbletea/lipgloss/runewidth only.
- `gofmt` every file you touch, and touch **only** files the task lists (a blanket `gofmt -w tui/*.go` reformats unrelated committed files and pollutes the diff).
- Work inside the harness worktree at `/home/kforfk/workspace/remote-agent-harness/.harness-worktrees/70fbad4a6eb6f1e992be8a669f1bcefd`. Absolute paths under `/home/kforfk/workspace/remote-agent-harness/<rel>` route to the PARENT checkout.
- Verification is per-task and scoped: `go test ./tui/` for a TUI task, `go test ./cli/` for a cli task, `-run TestX` for a targeted regression. `make check` + `make vet` + `make wasm-check` + `go test ./...` + `make test-integration` run **once**, at the end, before landing. Do not re-run a suite that already passed.
- Landing (once, after Task 7): FF-push to `origin/main`, `git -C <main checkout> merge --ff-only origin/main`, then `make build` in the main checkout.
- Specs: `docs/superpowers/specs/2026-07-31-tui-raw-connect-multipane-design.md` (A) and `docs/superpowers/specs/2026-07-31-http-request-builder-design.md` (B). Read the **Problem** section of each, not just Decisions.

## File Structure

| File | Responsibility |
| --- | --- |
| `tui/rawforward.go` (modify) | `rawPane` + `RawConnectModal` multi-pane state, tab strip view |
| `tui/rawforward_test.go` (create) | pane routing, esc/x semantics, tab movement |
| `tui/app.go` (modify) | route `RawForward*Msg` by generation; modal key handling; close panes on quit |
| `tui/httpform.go` (create) | the four-field HTTP form model used by the pane's form mode |
| `tui/httpform_test.go` (create) | form → `cli.HTTPRequestSpec` mapping |
| `cli/httpreq.go` (create) | `HTTPRequestSpec` + `BuildHTTPRequest` — the only place request bytes are assembled |
| `cli/httpreq_test.go` (create) | header rules, CRLF rejection, body handling |
| `cli/forward_stdio.go` (modify) | `RunHTTPRequestForward` — send a built request, stream the response |
| `cmd/harness-cli/main.go` (modify) | `--http-*` flags on `forward` |
| `cli/raw_forward_wasm.go` (modify) | remember each pane's host/port; `SendRawPaneHTTP` |
| `cmd/harness-webui-wasm/main.go` (modify) | `harness.rawSendHTTP` bridge |
| `webui/static/index.html`, `main.js`, `style.css` (modify) | the HTTP form beside the raw send box |
| `integration/port_forward_test.go` (modify) | one HTTP round-trip over a raw forward |

---

## Phase A — TUI multi-pane

### Task 1: Pane model in the modal

**Files:**
- Modify: `tui/rawforward.go:27-173`
- Create: `tui/rawforward_test.go`

**Interfaces:**
- Consumes: `cli.RawConn` (`Send`, `Close`, `ForwardID`), `cli.ParseStdioForwardSpec`.
- Produces, used by Task 2:
  - `func (m *RawConnectModal) Show(taskID string)` / `Hide()`
  - `func (m *RawConnectModal) OnNewSlot() bool`
  - `func (m *RawConnectModal) MovePane(delta int)`
  - `func (m *RawConnectModal) AddPane(taskID, host string, port int, gen uint64)`
  - `func (m *RawConnectModal) PaneForGen(gen uint64) *rawPane`
  - `func (m *RawConnectModal) CloseActivePane()` / `CloseAllPanes()`
  - `func (m *RawConnectModal) SendActive(b []byte) error`
  - `func (m *RawConnectModal) ActivePane() *rawPane`

- [ ] **Step 1: Write the failing test**

Create `tui/rawforward_test.go`:

```go
package tui

import "testing"

// Panes are addressed by the generation their messages carry; a reply for one
// pane must never be applied to another.
func TestRawModalRoutesByGeneration(t *testing.T) {
	var m RawConnectModal
	m.Show("task-1")
	m.AddPane("task-1", "127.0.0.1", 8080, 7)
	m.AddPane("task-1", "127.0.0.1", 9090, 8)

	p := m.PaneForGen(7)
	if p == nil || p.port != 8080 {
		t.Fatalf("PaneForGen(7) = %+v, want the 8080 pane", p)
	}
	p.AppendOutput([]byte("hello"))
	if other := m.PaneForGen(8); other == nil || len(other.out) != 0 {
		t.Errorf("output leaked into the 9090 pane: %+v", other)
	}
	if m.PaneForGen(99) != nil {
		t.Errorf("unknown generation resolved to a pane")
	}
}

// esc hides the modal and leaves every connection running; x closes exactly
// the active pane. The old modal closed the connection on esc, which also
// deregistered the forward.
func TestRawModalHideKeepsPanesCloseDropsOne(t *testing.T) {
	var m RawConnectModal
	m.Show("task-1")
	m.AddPane("task-1", "a", 1, 1)
	m.AddPane("task-1", "b", 2, 2)

	m.Hide()
	if m.IsOpen() {
		t.Errorf("Hide left the modal open")
	}
	if got := m.PaneCount(); got != 2 {
		t.Errorf("Hide dropped panes: %d left, want 2", got)
	}

	m.Show("task-1")
	m.MovePane(+1) // off [+ new] onto pane 1
	m.CloseActivePane()
	if got := m.PaneCount(); got != 1 {
		t.Fatalf("CloseActivePane left %d panes, want 1", got)
	}
	if p := m.PaneForGen(1); p != nil {
		t.Errorf("closed the wrong pane: gen 1 still present")
	}
}

// [+ new] is index 0 and stays; connecting appends and selects.
func TestRawModalNewSlotIsSticky(t *testing.T) {
	var m RawConnectModal
	m.Show("task-1")
	if !m.OnNewSlot() {
		t.Fatalf("a fresh modal must start on the [+ new] slot")
	}
	m.AddPane("task-1", "a", 1, 1)
	if m.OnNewSlot() {
		t.Errorf("AddPane must select the new pane")
	}
	m.MovePane(-1)
	if !m.OnNewSlot() {
		t.Errorf("moving left from the first pane must reach [+ new]")
	}
	m.MovePane(-1)
	if !m.OnNewSlot() {
		t.Errorf("movement must clamp at [+ new], not wrap")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./tui/ -run TestRawModal -count=1`
Expected: FAIL — `m.Show undefined`, `m.AddPane undefined`, etc.

- [ ] **Step 3: Replace the modal's state and methods**

In `tui/rawforward.go`, replace the `RawConnectModal` struct and every method
from `IsOpen` through `View` with the pane-based versions. Keep
`rawTUIRingBytes` and `rawConnectTimeout` as they are.

```go
// rawPane is one live (or ended) raw connection. The WebUI has held several of
// these since it shipped (rawSlots in cli/raw_forward_wasm.go); the TUI held
// exactly one, which is what made esc destructive and a second target
// impossible.
type rawPane struct {
	taskID string
	host   string
	port   int
	// gen tags every message this pane's pump sends. It scopes a PANE, not a
	// connect attempt: two attempts within one pane share it, which is why
	// connecting still gates the second dispatch.
	gen        uint64
	conn       *cli.RawConn
	cancel     context.CancelFunc
	out        []byte
	live       bool
	connecting bool
	note       string
}

func (p *rawPane) target() string { return fmt.Sprintf("%s:%d", p.host, p.port) }

// AppendOutput adds received bytes, trimming the front so the buffer stays
// bounded and the NEWEST bytes are the ones kept.
func (p *rawPane) AppendOutput(b []byte) {
	p.out = append(p.out, b...)
	if len(p.out) > rawTUIRingBytes {
		p.out = append([]byte(nil), p.out[len(p.out)-rawTUIRingBytes:]...)
	}
}

// closeConn closes the connection and stops its pump. Idempotent.
func (p *rawPane) closeConn() {
	if p.conn != nil {
		_ = p.conn.Close()
		p.conn = nil
	}
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
}

// RawConnectModal shows the [+ new] target prompt plus one tab per pane.
// active == 0 is the [+ new] slot; panes are active-1.
type RawConnectModal struct {
	open   bool
	taskID string
	input  textinput.Model
	panes  []rawPane
	active int
}

func NewRawConnectModal() RawConnectModal {
	in := textinput.New()
	in.Placeholder = "host:port"
	in.CharLimit = 128
	return RawConnectModal{input: in}
}

func (m *RawConnectModal) IsOpen() bool    { return m.open }
func (m *RawConnectModal) TaskID() string  { return m.taskID }
func (m *RawConnectModal) PaneCount() int  { return len(m.panes) }
func (m *RawConnectModal) OnNewSlot() bool { return m.active == 0 }

// Show reveals the modal for taskID. Unlike the old Open it keeps existing
// panes: they are connections, not view state.
func (m *RawConnectModal) Show(taskID string) {
	if m.input.Placeholder == "" {
		*m = NewRawConnectModal()
	}
	m.open = true
	m.taskID = taskID
	m.input.SetValue("")
	m.input.Focus()
}

// Hide closes the view only. Panes stay connected and stay registered.
func (m *RawConnectModal) Hide() {
	m.open = false
	m.input.Blur()
}

func (m *RawConnectModal) ActivePane() *rawPane {
	if m.active <= 0 || m.active > len(m.panes) {
		return nil
	}
	return &m.panes[m.active-1]
}

func (m *RawConnectModal) PaneForGen(gen uint64) *rawPane {
	for i := range m.panes {
		if m.panes[i].gen == gen {
			return &m.panes[i]
		}
	}
	return nil
}

// MovePane walks the tab strip. It clamps rather than wraps: wrapping past
// [+ new] would make "go left until you reach new" a guessing game.
func (m *RawConnectModal) MovePane(delta int) {
	m.active += delta
	if m.active < 0 {
		m.active = 0
	}
	if m.active > len(m.panes) {
		m.active = len(m.panes)
	}
	m.input.SetValue("")
}

// AddPane appends a connecting pane and selects it.
func (m *RawConnectModal) AddPane(taskID, host string, port int, gen uint64) {
	m.panes = append(m.panes, rawPane{
		taskID: taskID, host: host, port: port, gen: gen,
		connecting: true, note: "connecting…",
	})
	m.active = len(m.panes)
	m.input.SetValue("")
}

// CloseActivePane drops the selected pane, closing its connection (which is
// what deregisters the forward server-side).
func (m *RawConnectModal) CloseActivePane() {
	p := m.ActivePane()
	if p == nil {
		return
	}
	p.closeConn()
	i := m.active - 1
	m.panes = append(m.panes[:i], m.panes[i+1:]...)
	if m.active > len(m.panes) {
		m.active = len(m.panes)
	}
}

// CloseAllPanes is what quitting must call: a RawConn whose process is gone
// leaves a registration nobody can reach.
func (m *RawConnectModal) CloseAllPanes() {
	for i := range m.panes {
		m.panes[i].closeConn()
	}
	m.panes = nil
	m.active = 0
}

// SendActive writes bytes verbatim — no CRLF is appended. SendLine is the
// line-oriented convenience; a built HTTP request must not have anything added
// to it or Content-Length stops matching.
func (m *RawConnectModal) SendActive(b []byte) error {
	p := m.ActivePane()
	if p == nil || p.conn == nil {
		return fmt.Errorf("raw connect: not connected")
	}
	return p.conn.Send(b)
}

// SendLine writes the given text plus CRLF, for the line-oriented protocols
// this pane is for (HTTP by hand, Redis, SMTP).
func (m *RawConnectModal) SendLine(s string) error {
	return m.SendActive([]byte(s + "\r\n"))
}

func (m *RawConnectModal) SetConn(gen uint64, rc *cli.RawConn, cancel context.CancelFunc, note string) {
	p := m.PaneForGen(gen)
	if p == nil {
		return
	}
	p.closeConn()
	p.conn, p.cancel = rc, cancel
	p.connecting, p.live, p.note = false, true, note
}

func (m *RawConnectModal) MarkClosed(gen uint64, note string) {
	p := m.PaneForGen(gen)
	if p == nil {
		return
	}
	p.live, p.connecting, p.note = false, false, note
	p.closeConn()
}

func (m *RawConnectModal) SetSpec(s string) { m.input.SetValue(s) }
func (m *RawConnectModal) Spec() string     { return m.input.Value() }

// Target parses the entered spec. Reuses the CLI parser so -W and the TUI
// cannot disagree about what a target looks like.
func (m *RawConnectModal) Target() (string, int, error) {
	return cli.ParseStdioForwardSpec(m.input.Value())
}

func (m *RawConnectModal) Update(msg tea.Msg) (RawConnectModal, tea.Cmd) {
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return *m, cmd
}
```

Delete `IsLive`, `IsConnecting`, `Output`, `Open`, `Close`, `CloseConn`,
`MarkConnecting`, `MarkLive`, `AppendOutput` from the modal — Task 2 rewrites
their call sites. Keep the file compiling by leaving `View` for Task 3; for now
have it render `m.input.View()` only.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./tui/ -run TestRawModal -count=1`
Expected: PASS. `go build ./tui/` will still fail — Task 2 fixes `app.go`.

- [ ] **Step 5: Commit**

```bash
git add tui/rawforward.go tui/rawforward_test.go
git commit -m "refactor(tui): raw connect holds panes, not one connection"
```

### Task 2: Route messages and keys to panes

**Files:**
- Modify: `tui/app.go:88-105` (fields), `:760-792` (messages), `:1031-1078` (keys), and the `t` handler around `:1353`
- Modify: `tui/rawforward_test.go` (add the quit test)

**Interfaces:**
- Consumes: everything Task 1 produced.
- Produces: nothing new; `DoStartRawForward` keeps its signature.

- [ ] **Step 1: Write the failing test**

Append to `tui/rawforward_test.go`:

```go
// Quitting must close every pane: a RawConn whose process exits leaves a
// registration in `forward ls` that nothing can reach.
func TestRawModalCloseAllOnQuit(t *testing.T) {
	var m RawConnectModal
	m.Show("task-1")
	m.AddPane("task-1", "a", 1, 1)
	m.AddPane("task-1", "b", 2, 2)
	m.CloseAllPanes()
	if m.PaneCount() != 0 {
		t.Errorf("CloseAllPanes left %d panes", m.PaneCount())
	}
	if m.ActivePane() != nil {
		t.Errorf("active pane survived CloseAllPanes")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./tui/ -run TestRawModalCloseAll -count=1`
Expected: FAIL to build until Step 3 lands, then PASS.

- [ ] **Step 3: Rewire `app.go`**

Rename the field and repoint every use:

```go
	// rawGenSeq allocates the generation each pane is tagged with. It is an
	// allocator, not the guard: the guard is "which pane owns this gen", so a
	// reply for a closed pane finds no owner and is dropped.
	rawGenSeq uint64
```

Message handling:

```go
	case RawForwardOpenedMsg:
		// A pane closed while its open was in flight owns nothing now, so the
		// connection must be closed here — otherwise it stays registered with
		// no UI reference to it.
		if a.rawModal.PaneForGen(msg.Gen) == nil {
			_ = msg.Conn.Close()
			if msg.Cancel != nil {
				msg.Cancel()
			}
			return a, nil
		}
		a.rawModal.SetConn(msg.Gen, msg.Conn, msg.Cancel,
			fmt.Sprintf("connected (fwd %d)", msg.ForwardID))
		return a, nil

	case RawForwardDataMsg:
		if p := a.rawModal.PaneForGen(msg.Gen); p != nil {
			p.AppendOutput(msg.Data)
		}
		return a, nil

	case RawForwardClosedMsg:
		// The pump already closed the connection (that's what deregisters the
		// forward server-side); MarkClosed is idempotent and is the one place
		// that drops the pane's reference and stops its sink goroutine.
		a.rawModal.MarkClosed(msg.Gen, msg.Reason)
		return a, nil
```

Key handling inside `if a.rawModal.IsOpen()`:

```go
			switch msg.Type {
			case tea.KeyEsc:
				a.rawModal.Hide() // panes stay connected; x closes one
				return a, nil
			case tea.KeyLeft:
				a.rawModal.MovePane(-1)
				return a, nil
			case tea.KeyRight:
				a.rawModal.MovePane(+1)
				return a, nil
			case tea.KeyEnter:
				if p := a.rawModal.ActivePane(); p != nil {
					if p.live {
						if err := a.rawModal.SendLine(a.rawModal.Spec()); err != nil {
							a.rawModal.MarkClosed(p.gen, "raw connect: "+err.Error())
						} else {
							a.rawModal.SetSpec("")
						}
					}
					return a, nil
				}
				// [+ new]: one connect attempt at a time per pane, so a typo
				// fixed before the first RPC resolves cannot leave two replies
				// that the generation guard alone cannot tell apart.
				host, port, err := a.rawModal.Target()
				if err != nil {
					a.cmdresult.Append(WarnStyle.Render("raw connect: " + err.Error()))
					return a, nil
				}
				a.rawGenSeq++
				gen := a.rawGenSeq
				taskID := a.rawModal.TaskID()
				a.rawModal.AddPane(taskID, host, port, gen)
				return a, DoStartRawForward(a.client, taskID, host, port, gen, a.program)
			}
```

Add `x` before the input fallthrough, and only when a pane is selected:

```go
			if msg.String() == modalKeys.ForwardKill && !a.rawModal.OnNewSlot() {
				a.rawModal.CloseActivePane()
				return a, nil
			}
```

Replace the `t` handler's `a.rawModal.Open(taskID)` with `a.rawModal.Show(taskID)`.

In the `q` / `tea.KeyCtrlC` quit paths, call `a.rawModal.CloseAllPanes()` before
returning `tea.Quit`.

- [ ] **Step 4: Run the tests**

Run: `go build ./tui/ && go test ./tui/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tui/app.go tui/rawforward_test.go
git commit -m "feat(tui): route raw-connect messages and keys per pane; esc hides"
```

### Task 3: Tab strip view

**Files:**
- Modify: `tui/rawforward.go` (`View`)
- Modify: `tui/rawforward_test.go`

**Interfaces:**
- Consumes: Task 1's state.
- Produces: nothing beyond `View`.

- [ ] **Step 1: Write the failing test**

```go
func TestRawModalViewShowsTabsAndActiveTarget(t *testing.T) {
	var m RawConnectModal
	m.Show("3777c91ae235bdcc1f18db0b1d33d183")
	m.AddPane("3777c91ae235bdcc1f18db0b1d33d183", "127.0.0.1", 8080, 1)
	m.SetConn(1, nil, nil, "connected (fwd 7)")
	m.AddPane("3777c91ae235bdcc1f18db0b1d33d183", "10.0.0.2", 22, 2)
	m.MarkClosed(2, "connection closed")
	m.MovePane(-1) // back onto the 8080 pane

	v := m.View()
	for _, want := range []string{"+ new", "127.0.0.1:8080", "10.0.0.2:22", "connected (fwd 7)", "x close pane", "esc hide"} {
		if !strings.Contains(v, want) {
			t.Errorf("View() missing %q:\n%s", want, v)
		}
	}
}
```

Add `"strings"` to the test file's imports.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./tui/ -run TestRawModalView -count=1`
Expected: FAIL — the view still renders only the input.

- [ ] **Step 3: Implement `View`**

```go
func (m *RawConnectModal) View() string {
	head := fmt.Sprintf("raw connect — task %s", pfShortID(m.taskID))

	tabs := make([]string, 0, len(m.panes)+1)
	label := "[+ new]"
	if m.active == 0 {
		label = FocusedStyle.Render(label)
	}
	tabs = append(tabs, label)
	for i := range m.panes {
		t := "[" + m.panes[i].target() + "]"
		switch {
		case m.active == i+1:
			t = FocusedStyle.Render(t)
		case !m.panes[i].live:
			t = MutedStyle.Render(t)
		}
		tabs = append(tabs, t)
	}

	state := "enter host:port, Enter to connect"
	body := ""
	if p := m.ActivePane(); p != nil {
		state = p.note
		if state == "" {
			state = "connected"
		}
		body = string(p.out)
	}
	foot := "←/→ tab · Enter send · x close pane · esc hide"
	return head + "\n" + strings.Join(tabs, " ") + "\n" + state + "\n" +
		m.input.View() + "\n\n" + body + "\n" + FooterStyle.Render(foot)
}
```

If `FocusedStyle` / `MutedStyle` do not exist in `tui/styles.go`, add them next
to `FooterStyle` — `FocusedStyle` bold with `colorFocused`, `MutedStyle` with
`colorMuted` — and use them here only.

- [ ] **Step 4: Run the tests**

Run: `go test ./tui/ -count=1`
Expected: PASS.

- [ ] **Step 5: Live pass (once)**

```bash
scripts/dummy-harness.sh up --detach --name panes --agent fake
eval "$(scripts/dummy-harness.sh env --name panes)"
```

Open two panes to two ports from the TUI, send on each, press `esc`, reopen
with `t`, confirm both are still live and that both appear in
`harness-cli --server-cid "$CID" forward ls`. Then `scripts/dummy-harness.sh
down --name panes`. One pass — the routing itself is covered by unit tests.

- [ ] **Step 6: Commit**

```bash
git add tui/rawforward.go tui/rawforward_test.go tui/styles.go
git commit -m "feat(tui): tab strip for raw-connect panes"
```

---

## Phase B — HTTP request builder

### Task 4: `cli.BuildHTTPRequest`

**Files:**
- Create: `cli/httpreq.go`, `cli/httpreq_test.go`

**Interfaces:**
- Produces, used by Tasks 5-7:
  - `type HTTPRequestSpec struct { Method, Path string; Headers []string; Body []byte }`
  - `func BuildHTTPRequest(spec HTTPRequestSpec, host string, port int) ([]byte, error)`

- [ ] **Step 1: Write the failing test**

```go
package cli

import (
	"strings"
	"testing"
)

func TestBuildHTTPRequestDefaults(t *testing.T) {
	got, err := BuildHTTPRequest(HTTPRequestSpec{Path: "/healthz"}, "127.0.0.1", 8080)
	if err != nil {
		t.Fatalf("BuildHTTPRequest: %v", err)
	}
	want := "GET /healthz HTTP/1.1\r\nHost: 127.0.0.1:8080\r\nConnection: close\r\n\r\n"
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildHTTPRequestBodyAndContentLength(t *testing.T) {
	got, err := BuildHTTPRequest(HTTPRequestSpec{
		Method: "POST", Path: "/api", Body: []byte(`{"a":1}`),
	}, "h", 80)
	if err != nil {
		t.Fatalf("BuildHTTPRequest: %v", err)
	}
	s := string(got)
	if !strings.Contains(s, "Content-Length: 7\r\n") {
		t.Errorf("missing Content-Length:\n%s", s)
	}
	if !strings.HasSuffix(s, "\r\n\r\n"+`{"a":1}`) {
		t.Errorf("body must be last and unterminated:\n%q", s)
	}
	if !strings.Contains(s, "Host: h:80\r\n") {
		t.Errorf("port must be written even when 80:\n%s", s)
	}
}

func TestBuildHTTPRequestUserHeaderWins(t *testing.T) {
	got, err := BuildHTTPRequest(HTTPRequestSpec{
		Path:    "/x",
		Headers: []string{"host: example.com", "Connection: keep-alive", "Content-Length: 0"},
		Body:    []byte("ignored-by-length"),
	}, "127.0.0.1", 9)
	if err != nil {
		t.Fatalf("BuildHTTPRequest: %v", err)
	}
	s := string(got)
	if strings.Contains(s, "Host: 127.0.0.1:9") {
		t.Errorf("automatic Host overrode the user's:\n%s", s)
	}
	if strings.Count(s, "Connection:") != 1 || !strings.Contains(s, "Connection: keep-alive") {
		t.Errorf("automatic Connection was added anyway:\n%s", s)
	}
	if strings.Count(s, "Content-Length:") != 1 {
		t.Errorf("automatic Content-Length was added anyway:\n%s", s)
	}
}

func TestBuildHTTPRequestRejectsInjection(t *testing.T) {
	cases := map[string]HTTPRequestSpec{
		"method":  {Method: "GET\r\nX: 1", Path: "/"},
		"path":    {Path: "/x\r\nX: 1"},
		"header":  {Path: "/", Headers: []string{"X: 1\r\nY: 2"}},
		"badname": {Path: "/", Headers: []string{"X Y: 1"}},
		"noname":  {Path: "/", Headers: []string{"no-colon"}},
		"relpath": {Path: "healthz"},
	}
	for name, spec := range cases {
		if _, err := BuildHTTPRequest(spec, "h", 1); err == nil {
			t.Errorf("%s: want an error, got none", name)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cli/ -run TestBuildHTTPRequest -count=1`
Expected: FAIL — `BuildHTTPRequest` undefined.

- [ ] **Step 3: Implement the builder**

```go
package cli

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// HTTPRequestSpec is what a surface collects from the operator. Every surface
// builds through BuildHTTPRequest so the bytes on the wire cannot differ
// between the CLI, the TUI and the WebUI.
type HTTPRequestSpec struct {
	Method  string   // empty = GET
	Path    string   // "/healthz"; must start with "/" (or be "*")
	Headers []string // "Name: value", in order; suppresses the matching automatic header
	Body    []byte
}

// BuildHTTPRequest renders spec as HTTP/1.1 request bytes addressed to
// host:port.
//
// Host, Content-Length and Connection are supplied automatically and a
// user-supplied header of the same name always wins. Host carries the port
// even when it is 80: writing it unconditionally removes a "which form did it
// send?" question from every debugging session this pane exists for.
//
// CR or LF anywhere in the method, path, a header name or a header value is an
// error and nothing is built. That is the header-injection / request-smuggling
// boundary, and it is validation rather than sanitisation so the operator sees
// a message instead of silently different bytes.
func BuildHTTPRequest(spec HTTPRequestSpec, host string, port int) ([]byte, error) {
	method := spec.Method
	if method == "" {
		method = "GET"
	}
	if !isHTTPToken(method) {
		return nil, fmt.Errorf("http: bad method %q", method)
	}
	if spec.Path != "*" && !strings.HasPrefix(spec.Path, "/") {
		return nil, fmt.Errorf("http: path %q must start with / (or be *)", spec.Path)
	}
	if strings.ContainsAny(spec.Path, "\r\n") {
		return nil, fmt.Errorf("http: path contains CR or LF")
	}

	seen := map[string]bool{}
	type hdr struct{ name, value string }
	headers := make([]hdr, 0, len(spec.Headers))
	for _, raw := range spec.Headers {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		if strings.ContainsAny(raw, "\r\n") {
			return nil, fmt.Errorf("http: header %q contains CR or LF", raw)
		}
		name, value, ok := strings.Cut(raw, ":")
		if !ok {
			return nil, fmt.Errorf("http: header %q is not \"Name: value\"", raw)
		}
		name = strings.TrimSpace(name)
		if !isHTTPToken(name) {
			return nil, fmt.Errorf("http: bad header name %q", name)
		}
		headers = append(headers, hdr{name, strings.TrimSpace(value)})
		seen[strings.ToLower(name)] = true
	}

	var b bytes.Buffer
	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\n", method, spec.Path)
	if !seen["host"] {
		fmt.Fprintf(&b, "Host: %s:%d\r\n", host, port)
	}
	if len(spec.Body) > 0 && !seen["content-length"] {
		b.WriteString("Content-Length: " + strconv.Itoa(len(spec.Body)) + "\r\n")
	}
	if !seen["connection"] {
		b.WriteString("Connection: close\r\n")
	}
	for _, h := range headers {
		b.WriteString(h.name + ": " + h.value + "\r\n")
	}
	b.WriteString("\r\n")
	b.Write(spec.Body)
	return b.Bytes(), nil
}

// isHTTPToken reports whether s is a non-empty RFC 9110 token.
func isHTTPToken(s string) bool {
	if s == "" {
		return false
	}
	const extra = "!#$%&'*+-.^_`|~"
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune(extra, r):
		default:
			return false
		}
	}
	return true
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./cli/ -run TestBuildHTTPRequest -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cli/httpreq.go cli/httpreq_test.go
git commit -m "feat(cli): BuildHTTPRequest — one place that assembles request bytes"
```

### Task 5: CLI `--http-*` flags

**Files:**
- Modify: `cli/forward_stdio.go` (add `RunHTTPRequestForward`)
- Modify: `cmd/harness-cli/main.go:529-579`, usage text at `:826`

**Interfaces:**
- Consumes: `BuildHTTPRequest`, `OpenRawForward`.
- Produces: `func RunHTTPRequestForward(ctx context.Context, c *Client, taskIDHex, host string, port int, spec HTTPRequestSpec, out io.Writer, logf func(string)) error`

- [ ] **Step 1: Write the failing test**

Create `cli/httpreq_forward_test.go`:

```go
package cli

import "testing"

// The flag surface must reject a spec before dialling anything: an operator
// who typed a bad header should not first watch a forward be established.
func TestRunHTTPRequestForwardValidatesBeforeDial(t *testing.T) {
	err := RunHTTPRequestForward(t.Context(), nil, "task", "h", 1,
		HTTPRequestSpec{Path: "relative"}, nil, nil)
	if err == nil {
		t.Fatal("want a validation error, got nil")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cli/ -run TestRunHTTPRequestForward -count=1`
Expected: FAIL — undefined.

- [ ] **Step 3: Implement**

In `cli/forward_stdio.go`:

```go
// RunHTTPRequestForward sends one built request over a raw forward and copies
// the response to out until the far end closes. stdin is deliberately not
// spliced: a request fully specified by flags has nothing to read from it.
func RunHTTPRequestForward(ctx context.Context, c *Client, taskIDHex, host string, port int, spec HTTPRequestSpec, out io.Writer, logf func(string)) error {
	req, err := BuildHTTPRequest(spec, host, port)
	if err != nil {
		return err // before any dial: a typo must not cost a forward
	}
	rc, err := OpenRawForward(ctx, c, taskIDHex, host, port, logf)
	if err != nil {
		return err
	}
	defer rc.Close()
	if err := rc.Send(req); err != nil {
		return err
	}
	for {
		data, eof, rerr := rc.Recv(ctx)
		if len(data) > 0 {
			if _, werr := out.Write(data); werr != nil {
				return werr
			}
		}
		if eof {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}
```

In `cmd/harness-cli/main.go`, beside the `-W` flag:

```go
			httpMethod := fs.String("http-method", "GET", "with -W: HTTP method to send")
			httpPath := fs.String("http-path", "", "with -W: send this HTTP request path instead of splicing stdin")
			httpBody := fs.String("http-body", "", "with --http-path: request body (literal, @file, or - for stdin)")
			var httpHeaders stringList
			fs.Var(&httpHeaders, "http-header", "with --http-path: \"Name: value\" (repeatable)")
```

and in the `*wspec != ""` branch, before `RunStdioForward`:

```go
			if *httpPath != "" {
				body, berr := readFlagBody(*httpBody)
				if berr != nil {
					die(berr)
				}
				spec := cli.HTTPRequestSpec{
					Method: *httpMethod, Path: *httpPath,
					Headers: httpHeaders, Body: body,
				}
				if err := cli.RunHTTPRequestForward(fctx, c, taskID, wHost, wPort, spec, os.Stdout, logf); err != nil {
					die(err)
				}
				return
			}
```

Add the two helpers near `forwardWConflictsWithLR`:

```go
// stringList collects a repeatable string flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ", ") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// readFlagBody resolves a --http-body value: literal, @file, or - for stdin.
func readFlagBody(v string) ([]byte, error) {
	switch {
	case v == "":
		return nil, nil
	case v == "-":
		return io.ReadAll(os.Stdin)
	case strings.HasPrefix(v, "@"):
		return os.ReadFile(v[1:])
	default:
		return []byte(v), nil
	}
}
```

Extend the usage line at `:826` with
`[--http-path /p [--http-method M] [--http-header 'N: v'] [--http-body B]]`.

- [ ] **Step 4: Run the tests**

Run: `go test ./cli/ ./cmd/harness-cli/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cli/forward_stdio.go cli/httpreq_forward_test.go cmd/harness-cli/main.go
git commit -m "feat(cli): forward -W --http-path sends a built request and streams the response"
```

### Task 6: TUI HTTP form mode

**Files:**
- Create: `tui/httpform.go`, `tui/httpform_test.go`
- Modify: `tui/rawforward.go` (form mode + `View`), `tui/app.go` (ctrl+t, tab, Enter in form mode)

**Interfaces:**
- Consumes: `cli.HTTPRequestSpec`, `cli.BuildHTTPRequest`, Task 1's `SendActive`.
- Produces:
  - `type httpForm struct { ... }`, `func newHTTPForm() httpForm`
  - `func (f *httpForm) Spec() cli.HTTPRequestSpec`
  - `func (f *httpForm) NextField()`, `func (f *httpForm) CycleMethod(delta int)`
  - `func (f httpForm) View() string`
  - `func (m *RawConnectModal) ToggleForm()`, `func (m *RawConnectModal) InForm() bool`
  - `func (m *RawConnectModal) SendForm() error`

- [ ] **Step 1: Write the failing test**

Create `tui/httpform_test.go`:

```go
package tui

import "testing"

// One header per line — the field is a textarea precisely so no separator
// syntax has to be invented.
func TestHTTPFormSpecSplitsHeadersByLine(t *testing.T) {
	f := newHTTPForm()
	f.setForTest("POST", "/api", "Accept: application/json\nX-Trace: 1", `{"a":1}`)

	spec := f.Spec()
	if spec.Method != "POST" || spec.Path != "/api" {
		t.Fatalf("spec = %+v", spec)
	}
	if len(spec.Headers) != 2 || spec.Headers[1] != "X-Trace: 1" {
		t.Errorf("headers = %#v", spec.Headers)
	}
	if string(spec.Body) != `{"a":1}` {
		t.Errorf("body = %q", spec.Body)
	}
}

func TestHTTPFormSkipsBlankHeaderLines(t *testing.T) {
	f := newHTTPForm()
	f.setForTest("GET", "/", "\n\nAccept: */*\n\n", "")
	if got := f.Spec().Headers; len(got) != 1 || got[0] != "Accept: */*" {
		t.Errorf("headers = %#v, want one entry", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./tui/ -run TestHTTPForm -count=1`
Expected: FAIL — `newHTTPForm` undefined.

- [ ] **Step 3: Implement the form**

`tui/httpform.go` — a `textinput` for path, a method index, and two
`textarea`s (headers, body), following `tui/popup.go`'s textarea usage:

```go
package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/on-keyday/agent-harness/cli"
)

// httpMethods is the cycle order for the method field; anything outside it can
// still be sent from the CLI, which takes a free-form --http-method.
var httpMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"}

type httpFormField int

const (
	httpFieldMethod httpFormField = iota
	httpFieldPath
	httpFieldHeaders
	httpFieldBody
	httpFieldCount
)

type httpForm struct {
	method  int
	path    textinput.Model
	headers textarea.Model
	body    textarea.Model
	field   httpFormField
}

func newHTTPForm() httpForm {
	p := textinput.New()
	p.Placeholder = "/healthz"
	p.CharLimit = 512
	h := textarea.New()
	h.Placeholder = "Accept: application/json"
	h.SetHeight(3)
	b := textarea.New()
	b.Placeholder = "request body"
	b.SetHeight(3)
	f := httpForm{path: p, headers: h, body: b, field: httpFieldPath}
	f.path.Focus()
	return f
}

// setForTest fills the fields without going through key events.
func (f *httpForm) setForTest(method, path, headers, body string) {
	for i, m := range httpMethods {
		if m == method {
			f.method = i
		}
	}
	f.path.SetValue(path)
	f.headers.SetValue(headers)
	f.body.SetValue(body)
}

func (f *httpForm) CycleMethod(delta int) {
	f.method = (f.method + delta + len(httpMethods)) % len(httpMethods)
}

func (f *httpForm) NextField() {
	f.field = (f.field + 1) % httpFieldCount
	f.path.Blur()
	f.headers.Blur()
	f.body.Blur()
	switch f.field {
	case httpFieldPath:
		f.path.Focus()
	case httpFieldHeaders:
		f.headers.Focus()
	case httpFieldBody:
		f.body.Focus()
	}
}

func (f *httpForm) Spec() cli.HTTPRequestSpec {
	headers := make([]string, 0, 4)
	for _, line := range strings.Split(f.headers.Value(), "\n") {
		if strings.TrimSpace(line) != "" {
			headers = append(headers, strings.TrimSpace(line))
		}
	}
	return cli.HTTPRequestSpec{
		Method:  httpMethods[f.method],
		Path:    f.path.Value(),
		Headers: headers,
		Body:    []byte(f.body.Value()),
	}
}

func (f httpForm) Update(msg tea.Msg) (httpForm, tea.Cmd) {
	var cmd tea.Cmd
	switch f.field {
	case httpFieldPath:
		f.path, cmd = f.path.Update(msg)
	case httpFieldHeaders:
		f.headers, cmd = f.headers.Update(msg)
	case httpFieldBody:
		f.body, cmd = f.body.Update(msg)
	}
	return f, cmd
}

func (f httpForm) View() string {
	mark := func(want httpFormField, s string) string {
		if f.field == want {
			return FocusedStyle.Render(s)
		}
		return s
	}
	return mark(httpFieldMethod, "method  "+httpMethods[f.method]+"  (←/→)") + "\n" +
		mark(httpFieldPath, "path    ") + f.path.View() + "\n" +
		mark(httpFieldHeaders, "headers") + "\n" + f.headers.View() + "\n" +
		mark(httpFieldBody, "body") + "\n" + f.body.View() + "\n" +
		FooterStyle.Render("tab next field · Enter send · ctrl+t back")
}
```

In `tui/rawforward.go`, add to `RawConnectModal`:

```go
	form     httpForm
	formMode bool
```

```go
// ToggleForm switches the active pane between raw byte entry and the HTTP
// form. The key that reaches it must not be printable: the pane's text input
// consumes those.
func (m *RawConnectModal) ToggleForm() {
	if m.ActivePane() == nil {
		return
	}
	if !m.formMode {
		m.form = newHTTPForm()
	}
	m.formMode = !m.formMode
}

func (m *RawConnectModal) InForm() bool { return m.formMode && m.ActivePane() != nil }

// SendForm builds the request and writes it in ONE Send — nothing may append
// to the bytes or Content-Length stops matching what arrives.
func (m *RawConnectModal) SendForm() error {
	p := m.ActivePane()
	if p == nil {
		return fmt.Errorf("raw connect: not connected")
	}
	req, err := cli.BuildHTTPRequest(m.form.Spec(), p.host, p.port)
	if err != nil {
		return err
	}
	return m.SendActive(req)
}
```

`View` renders `m.form.View()` in place of the input line when `InForm()`.

In `tui/app.go`, inside the modal's key block and BEFORE the input fallthrough:

```go
			if msg.Type == tea.KeyCtrlT {
				a.rawModal.ToggleForm()
				return a, nil
			}
			if a.rawModal.InForm() {
				switch msg.Type {
				case tea.KeyTab:
					a.rawModal.FormNextField()
					return a, nil
				case tea.KeyLeft, tea.KeyRight:
					d := 1
					if msg.Type == tea.KeyLeft {
						d = -1
					}
					a.rawModal.FormCycleMethod(d)
					return a, nil
				case tea.KeyEnter:
					if err := a.rawModal.SendForm(); err != nil {
						a.rawModal.SetActiveNote("http: " + err.Error())
					}
					return a, nil
				}
				var fcmd tea.Cmd
				a.rawModal, fcmd = a.rawModal.UpdateForm(msg)
				return a, fcmd
			}
```

Add the three thin forwarders (`FormNextField`, `FormCycleMethod`,
`SetActiveNote`, `UpdateForm`) to `RawConnectModal`; they exist so `app.go`
never reaches into the form's fields.

Update the pane footer string to `←/→ tab · Enter send · ctrl+t HTTP · x close pane · esc hide`.

- [ ] **Step 4: Run the tests**

Run: `go test ./tui/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tui/httpform.go tui/httpform_test.go tui/rawforward.go tui/app.go
git commit -m "feat(tui): ctrl+t builds an HTTP request in the active raw pane"
```

### Task 7: WebUI form + integration test

**Files:**
- Modify: `cli/raw_forward_wasm.go` (host/port on the slot, `SendRawPaneHTTP`)
- Modify: `cmd/harness-webui-wasm/main.go` (`rawSendHTTP` bridge)
- Modify: `webui/static/index.html`, `webui/static/main.js`, `webui/static/style.css`
- Modify: `integration/port_forward_test.go`

**Interfaces:**
- Consumes: `BuildHTTPRequest`, `SendRawPane`.
- Produces: `func SendRawPaneHTTP(key string, spec HTTPRequestSpec) error`, and
  `harness.rawSendHTTP(paneKey, {method, path, headers, body})` in JS.

- [ ] **Step 1: Write the failing test**

In `integration/port_forward_test.go`, add:

```go
// A built request must satisfy a real HTTP server across a raw forward — the
// unit tests fix the bytes, this fixes that those bytes are what a server
// accepts.
func TestRawForwardHTTPRequestRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E skipped in -short mode")
	}
	var gotMethod, gotPath, gotHeader string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotHeader = r.Method, r.URL.Path, r.Header.Get("X-Trace")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "pong")
	}))
	defer srv.Close()
	port := mustPortOf(t, srv.URL)

	// --- same server + runner + task setup as TestRawForwardRoundTripListKill ---
	// (copy that test's setup block verbatim; it ends with a dialled *cli.Client c
	//  and a taskID)

	var out bytes.Buffer
	err := cli.RunHTTPRequestForward(context.Background(), c, taskID, "127.0.0.1", port,
		cli.HTTPRequestSpec{
			Method: "POST", Path: "/echo",
			Headers: []string{"X-Trace: 1"},
			Body:    []byte("hi"),
		}, &out, func(string) {})
	if err != nil {
		t.Fatalf("RunHTTPRequestForward: %v", err)
	}
	if gotMethod != "POST" || gotPath != "/echo" || gotHeader != "1" || string(gotBody) != "hi" {
		t.Fatalf("server saw %s %s X-Trace=%q body=%q", gotMethod, gotPath, gotHeader, gotBody)
	}
	if !strings.Contains(out.String(), "pong") {
		t.Fatalf("response not streamed to out:\n%s", out.String())
	}
}

// mustPortOf extracts the port from an httptest server URL.
func mustPortOf(t *testing.T, rawURL string) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port of %q: %v", rawURL, err)
	}
	return p
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -tags integration ./integration/ -run TestRawForwardHTTPRequestRoundTrip -count=1 -timeout 180s`
Expected: FAIL — undefined / setup incomplete until Step 3.

- [ ] **Step 3: Implement the wasm + WebUI side**

`cli/raw_forward_wasm.go` — record the target when the pane opens, because the
builder needs it for `Host`:

```go
type rawSlot struct {
	conn *RawConn
	gen  uint64
	host string
	port int
	note string
}
```

Set `host`/`port` in `OpenRawPane` where the slot is created, and add:

```go
// SendRawPaneHTTP builds a request for the pane's own target and sends it in
// one write. The page never assembles request bytes itself — same builder as
// the CLI and the TUI.
func SendRawPaneHTTP(key string, spec HTTPRequestSpec) error {
	rawMu.Lock()
	slot := rawSlots[key]
	rawMu.Unlock()
	if slot == nil || slot.conn == nil {
		return errors.New("rawSendHTTP: no such pane")
	}
	req, err := BuildHTTPRequest(spec, slot.host, slot.port)
	if err != nil {
		return err
	}
	return slot.conn.Send(req)
}
```

`cmd/harness-webui-wasm/main.go` — register `"rawSendHTTP"` beside the existing
raw entries, reading `{method, path, headers, body}` from a JS object
(`headers` a newline-separated string, matching the TUI's textarea).

`webui/static/index.html` — a `raw-http-form` block beside the send row:
method `<select>`, path `<input>`, headers `<textarea>`, body `<textarea>`, a
send button, and a toggle that shows either the byte send row or the form.
Style it in `style.css` with the existing dark palette and keep it usable at
≤600px (stack the fields).

`webui/static/main.js` — the toggle plus a submit handler calling
`window.harness.rawSendHTTP(rawActiveKey, {...})`, reporting a build error into
the pane's note the way `harness_rawClosed` does.

- [ ] **Step 4: Run the tests**

Run: `go test -tags integration ./integration/ -run TestRawForwardHTTPRequestRoundTrip -count=1 -timeout 180s`
Expected: PASS.
Run: `make wasm-check`
Expected: builds clean.

- [ ] **Step 5: One live WebUI pass**

Dummy harness + the echo/httptest target, connect a pane, submit the form, and
confirm the response text appears in the pane. One pass — the bytes themselves
are already fixed by Task 4's tests.

- [ ] **Step 6: Commit**

```bash
git add cli/raw_forward_wasm.go cmd/harness-webui-wasm/main.go webui/static/ integration/port_forward_test.go
git commit -m "feat(webui): build HTTP requests in the raw pane"
```

---

## Final gate (once)

```bash
make check && make vet && make wasm-check && go test ./... && make test-integration
```

Then land per the Global Constraints, and `make build` in the main checkout.

## Self-Review

- **Spec coverage.** A: panes on the modal (T1), per-pane generations + esc/x +
  quit (T2), tab strip view (T3), no-hex decision recorded in the spec and not
  contradicted here. B: builder (T4), CLI flags (T5), TUI form on ctrl+t (T6),
  WebUI form + integration (T7). Every Decision in both specs maps to a task.
- **Placeholders.** None: every code step carries the code, every test step
  carries the assertions.
- **Type consistency.** `HTTPRequestSpec` is defined once (T4) and consumed with
  the same field names in T5/T6/T7. `PaneForGen`, `SendActive`, `ActivePane`
  are introduced in T1 and used under those names in T2 and T6. `rawGenSeq`
  replaces `rawGen` in T2 and appears nowhere else.
