# SSH gateway Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a plain `ssh` client reach a harness interactive session, so `~/.ssh/config` aliases, tmux, mosh and scripted ssh drivers work against tasks.

**Architecture:** A listener inside an ordinary harness client process (the `harness-cli ssh-gateway` verb, or the TUI) speaks the SSH protocol with `golang.org/x/crypto/ssh`. The ssh user name names the task and the attach mode; each accepted session channel is spliced to an `AttachSession` stream through the pump exported from `objtrsf/exec`. Nothing changes in `harness-server`, `runner/`, or any `.bgn` schema.

**Tech Stack:** Go 1.25.7, `golang.org/x/crypto/ssh` (already in `go.sum` at v0.52.0, promoted indirect → direct), `github.com/on-keyday/objtrsf/exec`, existing `cli.Client` / `protocol.AttachMode`.

**Spec:** `docs/superpowers/specs/2026-08-25-ssh-gateway-design.md` — read the **Problem** section verbatim before starting, and report which of P1–P4 your diff addresses.

## Global Constraints

- **Where the work lands.** objtrsf tasks: `/home/kforfk/workspace/objtrsf` on branch `main`. Harness tasks: `/home/kforfk/workspace/remote-agent-harness/.harness-worktrees/70fbad4a6eb6f1e992be8a669f1bcefd` on branch `harness/70fbad4a6eb6f1e992be8a669f1bcefd`. Confirm with `git rev-parse --abbrev-ref HEAD` before the first commit of every task. A bare absolute path under `/home/kforfk/workspace/remote-agent-harness/` routes to the PARENT checkout, not this worktree.
- **Task 1 must be landed and pushed before Task 2 starts.** Everything after Task 2 depends on the bumped `go.mod`. No `replace` directive at any point.
- Every file in `cli/sshgw` starts with `//go:build !js`. Package `cli` is compiled for `js/wasm`; `make wasm-check` is what catches a miss.
- No `.bgn` change, no `server/` change, no `runner/` change (D9).
- The gateway never dials: `Run` takes an already-connected `*cli.Client` (`feedback_reuse_long_lived_client`).
- Default listen address: `127.0.0.1:2222`.
- User-name forms: bare 32-hex = **cowrite**, `<32hex>.control` = control, `<32hex>.view` = view (D11).
- Verify with `make check`, `make wasm-check`, `make vet`, `make test` — not ad-hoc `go build ./...`.
- **Never** run bare `go build ./cmd/<x>` — it drops a binary in the worktree root. Use `go build -o /dev/null ./cmd/<x>`.
- `pty-req` and `window-change` carry **columns before rows**; `SetTerminalWindowSize` takes **rows first**. Swapping them renders a session at a transposed size that still looks plausible.

---

### Task 1: Export the pump and the terminal resets from objtrsf

**Repo:** `/home/kforfk/workspace/objtrsf` (branch `main`)

**Files:**
- Create: `exec/terminal_modes.go`
- Modify: `exec/exec_shell.go` (the `defer fmt.Fprint(os.Stdout, …)` literal, and `func (w *CommandExecutionStream) pumpTerminalIO`)
- Test: `exec/terminal_modes_test.go` (create), `exec/exec_shell_test.go` (modify: 8 call sites)

**Interfaces:**
- Consumes: nothing.
- Produces: `exec.ScreenModeReset` (const string), `exec.InputModeReset` (const string), `exec.WriteTerminalReset(w io.Writer)`, `(*CommandExecutionStream).PumpTerminalIO(in io.Reader, out io.Writer) error`.

- [ ] **Step 1: Write the failing test**

Create `exec/terminal_modes_test.go`:

```go
//go:build !js

package exec

import (
	"bytes"
	"strings"
	"testing"
)

// The reset is written to a terminal the caller may not own (an ssh channel,
// not os.Stdout), so what matters is that ONE call emits both groups. A caller
// that writes only one leaves either a stranded alternate screen or a terminal
// still sending mouse reports.
func TestWriteTerminalReset_EmitsBothGroups(t *testing.T) {
	var buf bytes.Buffer
	WriteTerminalReset(&buf)
	got := buf.String()

	for _, want := range []string{
		"\x1b[?1049l", // screen group: leave the alternate screen
		"\x1b[r",      // screen group: reset the scroll region (DECSTBM)
		"\x1b[?6l",    // screen group: reset origin mode (DECOM)
		"\x1b[?1006l", // input group: SGR mouse reporting off
		"\x1b[?2004l", // input group: bracketed paste off
		"\x1b[?2031l", // input group: colour-scheme notifications off
	} {
		if !strings.Contains(got, want) {
			t.Errorf("WriteTerminalReset output is missing %q", want)
		}
	}
}

func TestWriteTerminalReset_IsExactlyTheTwoConsts(t *testing.T) {
	var buf bytes.Buffer
	WriteTerminalReset(&buf)
	if want := ScreenModeReset + InputModeReset; buf.String() != want {
		t.Errorf("WriteTerminalReset wrote %q, want ScreenModeReset+InputModeReset", buf.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/kforfk/workspace/objtrsf && go test ./exec/ -run TestWriteTerminalReset -v`
Expected: FAIL — `undefined: WriteTerminalReset`, `undefined: ScreenModeReset`, `undefined: InputModeReset`.

- [ ] **Step 3: Create the consts and the writer**

Create `exec/terminal_modes.go`. `ScreenModeReset` is the literal currently inside `RemoteShell`'s `defer` in `exec/exec_shell.go`, moved verbatim. `InputModeReset` and its doc comment are moved verbatim from the harness's `cli/terminal_input_modes.go` (that file is deleted in Task 2). Copy both comment blocks across — they carry the incidents that produced each list, and neither is reconstructible from the bytes.

```go
//go:build !js

package exec

import "io"

// ScreenModeReset puts the SCREEN back: it undoes what a full-screen app
// (htop, vim, less) turned on and never got to tear down, because a detach
// leaves the app running instead of ending it.
//
// [Move the full explanation from RemoteShell's defer in exec_shell.go here —
// the Win32 Input Mode / modifyOtherKeys negotiation, the alternate-screen and
// hidden-cursor cases, the bubbletea tea.Exec re-enter interaction, and the
// DECSTBM/DECOM paragraph. Do not paraphrase it.]
const ScreenModeReset = "" +
	"\x1b[?9001l" + // Win32 Input Mode off
	"\x1b[>4;0m" + // modifyOtherKeys off
	"\x1b[?1049l" + // leave the alternate screen
	"\x1b[?25h" + // show the cursor (DECTCEM)
	"\x1b[?1000l" + // mouse: button press/release
	"\x1b[?1002l" + // mouse: button-event tracking
	"\x1b[?1003l" + // mouse: any-event tracking
	"\x1b[?1006l" + // mouse: SGR coordinates
	"\x1b[?2004l" + // bracketed paste
	"\x1b[r" + // DECSTBM: scroll region back to the full window
	"\x1b[?6l" + // DECOM: origin mode
	"\x1b[0m" // SGR reset

// InputModeReset stops the local terminal from SENDING things.
//
// [Move the full doc comment from the harness's cli/terminal_input_modes.go
// verbatim, including the reproduced `35;65;36M` incident and the paragraph
// explaining why screen-affecting modes are deliberately NOT in THIS list.
// That paragraph stays accurate: it describes this const, not the pair.]
const InputModeReset = "" +
	"\x1b[?1l" + // DECCKM: cursor keys back to normal encoding
	"\x1b[?9l" + // X10 mouse reporting
	"\x1b[?66l" + // DECNKM: numeric keypad
	"\x1b[?1000l" +
	"\x1b[?1001l" +
	"\x1b[?1002l" +
	"\x1b[?1003l" +
	"\x1b[?1004l" + // focus in/out reporting
	"\x1b[?1005l" +
	"\x1b[?1006l" +
	"\x1b[?1015l" +
	"\x1b[?1016l" +
	"\x1b[?2004l" +
	"\x1b[?2031l" // colour-scheme change notifications

// WriteTerminalReset writes the full reset — both groups — to w.
//
// It exists so that "what a full reset is" has one definition. RemoteShell
// passes os.Stdout; a front end that is not a local tty (the harness ssh
// gateway) passes the channel that reaches its client's terminal. A third
// group added here later reaches both without anyone remembering to.
//
// The two groups overlap (the mouse modes and bracketed paste are in each).
// Turning off a mode that is already off is a no-op, so the overlap needs no
// reconciling and the order within it does not matter.
//
// Errors are ignored on purpose: this runs on the way out, the terminal may
// already be gone, and there is nothing a caller could do about it that would
// not be worse than a terminal it no longer owns.
func WriteTerminalReset(w io.Writer) {
	_, _ = io.WriteString(w, ScreenModeReset)
	_, _ = io.WriteString(w, InputModeReset)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /home/kforfk/workspace/objtrsf && go test ./exec/ -run TestWriteTerminalReset -v`
Expected: PASS (both tests).

- [ ] **Step 5: Point RemoteShell at the new symbols**

In `exec/exec_shell.go`, replace the deferred literal with a call, keeping the LIFO position — it must still fire before `term.Restore`, which is what puts the escape out while stdout is still in raw mode:

```go
	defer WriteTerminalReset(os.Stdout)
```

Delete the now-duplicated literal. Leave the explanatory comment block ONLY in `terminal_modes.go`; at this call site keep one line saying the reset fires before `term.Restore` and why.

- [ ] **Step 6: Export the pump**

Rename `func (w *CommandExecutionStream) pumpTerminalIO(in io.Reader, out io.Writer) error` to `PumpTerminalIO`, and update its doc comment to say it is callable with any ends, not just a tty:

```go
// PumpTerminalIO splices in→runner and runner→out until either direction ends,
// intercepting the detach key on the way in. RemoteShell passes os.Stdin /
// os.Stdout; a front end that is not a local tty passes its own ends (the
// harness ssh gateway passes an ssh channel), and tests pass theirs.
//
// The caller owns everything tty-specific — raw mode, the SIGWINCH forwarder,
// and writing WriteTerminalReset on the way out. This function assumes none of
// it.
//
// Both directions must be able to end it: an input pump that dies alone leaves
// the caller blocked on the output copy with nothing reading the terminal, and
// raw mode means neither Ctrl+C nor the detach key can get out of that.
func (w *CommandExecutionStream) PumpTerminalIO(in io.Reader, out io.Writer) error {
```

Update the one production call site (the tail of `RemoteShell`) and the 8 call sites in `exec/exec_shell_test.go`:

```bash
cd /home/kforfk/workspace/objtrsf
grep -rn "pumpTerminalIO" exec/
```

Expected after the edit: every hit is `PumpTerminalIO`, except prose inside comments that names the function — update those too.

- [ ] **Step 7: Run the full objtrsf suite**

Run: `cd /home/kforfk/workspace/objtrsf && go build ./... && go test ./exec/ -v`
Expected: PASS, including `TestPumpTerminalIO_RemoteEndEndsSession`, `TestPumpTerminalIO_DetachEndsWhenPeerCloses`, `TestPumpTerminalIO_SwallowIsBounded` and the rest — this task is a rename plus an extraction, so a behaviour change here is a bug in the task.

Then: `cd /home/kforfk/workspace/objtrsf && go test ./...`
Expected: PASS.

- [ ] **Step 8: Commit and push**

```bash
cd /home/kforfk/workspace/objtrsf
git add exec/terminal_modes.go exec/terminal_modes_test.go exec/exec_shell.go exec/exec_shell_test.go
git commit -m "feat(exec): export the terminal pump and the mode resets

A front end that is not a local tty — the harness ssh gateway — needs
the same input pump and the same reset escapes RemoteShell uses, and
could reach neither. pumpTerminalIO already took io.Reader/io.Writer, so
exporting it is a rename; the reset literal becomes two named consts
plus WriteTerminalReset, which is where the composition now lives.

InputModeReset moves here from the harness, where it sat one repository
away from the overlapping half and was only ever written together with
it."
git push origin main
git rev-parse HEAD    # record this SHA — Task 2 needs it
```

Expected: fast-forward push (objtrsf landing policy is local-trunk FF; see `landing-policy-objtrsf`). If the push is rejected, rebase onto `origin/main` and re-run the suite before pushing again — never force.

---

### Task 2: Bump objtrsf and delete the harness's copy of the input reset

**Repo:** the harness worktree (see Global Constraints)

**Files:**
- Modify: `go.mod`, `go.sum`
- Delete: `cli/terminal_input_modes.go`
- Modify: `cli/attach_native.go:74`, `cli/open_interactive_native.go:145`, `cli/x11.go:174`, `tui/interactive.go:357`

**Interfaces:**
- Consumes: `exec.WriteTerminalReset` from Task 1 (via `RemoteShell`'s defer — no harness call site calls it directly yet).
- Produces: a `go.mod` on the new objtrsf. `cli.InputModeReset` and `cli.RestoreLocalInputModes` no longer exist.

- [ ] **Step 1: Bump the dependency**

```bash
cd /home/kforfk/workspace/remote-agent-harness/.harness-worktrees/70fbad4a6eb6f1e992be8a669f1bcefd
go get github.com/on-keyday/objtrsf@<SHA from Task 1 Step 8>
grep objtrsf go.mod
```

Expected: the pseudo-version's trailing hex matches the first 12 chars of that SHA.

- [ ] **Step 2: Run the build to see the four call sites break**

Run: `go build ./... 2>&1 | head -20`
Expected: still PASSES — nothing is broken yet, because `RestoreLocalInputModes` still exists locally. This step exists to confirm the bump itself is clean before anything is deleted.

- [ ] **Step 3: Delete the harness copy and its call sites**

```bash
git rm cli/terminal_input_modes.go
```

Then delete these four lines, each of which sits immediately after a `RemoteShell()` call that now writes both groups itself:

- `cli/attach_native.go:74` — `RestoreLocalInputModes(os.Stdout)`
- `cli/open_interactive_native.go:145` — `RestoreLocalInputModes(os.Stdout)`
- `cli/x11.go:174` — `RestoreLocalInputModes(os.Stdout)`
- `tui/interactive.go:357` — `cli.RestoreLocalInputModes(os.Stdout)`

Keep each site's surrounding comment, but rewrite it to say the reset now happens inside `RemoteShell`. At `cli/attach_native.go` the comment currently explains why the call is placed before the error check; that reasoning moved into objtrsf with the defer, so the comment shrinks to one line rather than being deleted outright.

Remove any `"os"` import that becomes unused (the compiler will name the file).

- [ ] **Step 4: Verify nothing else referenced them**

Run: `grep -rn "InputModeReset\|RestoreLocalInputModes" --include='*.go' .`
Expected: no output. (Before this task the complete set was those four call sites plus the definition file — no test, no wasm path, referenced either.)

- [ ] **Step 5: Build and test every surface**

```bash
make check
make wasm-check
make vet
make test
```
Expected: all PASS. `wasm-check` matters here: `cli` is compiled for `js/wasm` and this task removes a symbol from it.

- [ ] **Step 6: Commit**

```bash
git add -A
git commit -m "refactor: take the input-mode reset from objtrsf

Both reset groups now live in objtrsf/exec and RemoteShell writes them
together, so the four call sites that wrote the input half by hand no
longer need to. They were the complete set of references."
```

---

### Task 3: ssh user name → task id and attach mode

**Files:**
- Create: `cli/sshgw/user.go`
- Test: `cli/sshgw/user_test.go`

**Interfaces:**
- Consumes: `protocol.AttachMode`.
- Produces: `sshgw.ParseUserName(name string) (taskIDHex string, mode protocol.AttachMode, err error)`. Returns lowercase 32-hex and one of `AttachMode_Cowrite` / `AttachMode_Control` / `AttachMode_View`.

- [ ] **Step 1: Write the failing test**

Create `cli/sshgw/user_test.go`:

```go
//go:build !js

package sshgw

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

const validID = "0123456789abcdef0123456789abcdef"

func TestParseUserName_Accepted(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want protocol.AttachMode
	}{
		{"bare is cowrite", validID, protocol.AttachMode_Cowrite},
		{"control suffix", validID + ".control", protocol.AttachMode_Control},
		{"view suffix", validID + ".view", protocol.AttachMode_View},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, mode, err := ParseUserName(tc.in)
			if err != nil {
				t.Fatalf("ParseUserName(%q) error: %v", tc.in, err)
			}
			if id != validID {
				t.Errorf("task id = %q, want %q", id, validID)
			}
			if mode != tc.want {
				t.Errorf("mode = %v, want %v", mode, tc.want)
			}
		})
	}
}

func TestParseUserName_Rejected(t *testing.T) {
	// Uppercase is rejected rather than lowered: task ids are printed lowercase
	// everywhere, so an uppercase name is more likely a typo than a request.
	for _, in := range []string{
		"",
		"root",
		validID[:31],
		validID + "0",
		"0123456789ABCDEF0123456789ABCDEF",
		validID + ".cowrite",
		validID + ".Control",
		validID + ".control.view",
		"prefix-" + validID,
		"01234567-89ab-cdef-0123-456789abcdef",
	} {
		if _, _, err := ParseUserName(in); err == nil {
			t.Errorf("ParseUserName(%q) = nil error, want a rejection", in)
		}
	}
}

// The rejection text is what an ssh client prints, so it must name the forms.
func TestParseUserName_ErrorNamesTheForms(t *testing.T) {
	_, _, err := ParseUserName("root")
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{".control", ".view"} {
		if !contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (len(sub) == 0 || indexOf(s, sub) >= 0) }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cli/sshgw/ -run TestParseUserName -v`
Expected: FAIL — the package does not exist yet.

- [ ] **Step 3: Write the implementation**

Create `cli/sshgw/user.go`:

```go
//go:build !js

// Package sshgw serves SSH connections that land in harness interactive
// sessions. See docs/superpowers/specs/2026-08-25-ssh-gateway-design.md.
package sshgw

import (
	"fmt"
	"strings"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

const taskIDHexLen = 32

// ParseUserName maps an ssh user name to the task it names and the attach mode
// it asks for.
//
//	<32-hex>            cowrite — types, takes no seat
//	<32-hex>.control    control — takes the seat, owns the PTY size
//	<32-hex>.view       view    — reads only
//
// The bare form is cowrite on purpose: a control attach is a takeover
// server-side, so a bare `ssh <id>@host` would silently detach whatever the
// operator had attached in the TUI. Arriving somewhere should not evict you
// from it, and the takeover stays available spelled out.
func ParseUserName(name string) (string, protocol.AttachMode, error) {
	id, suffix, hasSuffix := strings.Cut(name, ".")
	if !isTaskIDHex(id) {
		return "", 0, fmt.Errorf("ssh user name %q is not a task: use <32-hex-task-id>[.control|.view]@host (lowercase hex)", name)
	}
	mode := protocol.AttachMode_Cowrite
	if hasSuffix {
		switch suffix {
		case "control":
			mode = protocol.AttachMode_Control
		case "view":
			mode = protocol.AttachMode_View
		default:
			return "", 0, fmt.Errorf("ssh user name %q: unknown mode %q (want .control or .view, or no suffix for cowrite)", name, suffix)
		}
	}
	return id, mode, nil
}

func isTaskIDHex(s string) bool {
	if len(s) != taskIDHexLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cli/sshgw/ -run TestParseUserName -v`
Expected: PASS. Note `strings.Cut` splits at the FIRST dot, so `<id>.control.view` leaves suffix `control.view`, which the switch rejects.

- [ ] **Step 5: Commit**

```bash
git add cli/sshgw/user.go cli/sshgw/user_test.go
git commit -m "feat(sshgw): map an ssh user name to a task and attach mode"
```

---

### Task 4: Host key, authorized keys, and the bind/auth coupling

**Files:**
- Create: `cli/sshgw/auth.go`
- Test: `cli/sshgw/auth_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `sshgw.LoadOrCreateHostKey(path string) (ssh.Signer, error)`
  - `sshgw.LoadAuthorizedKeys(path string) ([]ssh.PublicKey, error)`
  - `sshgw.IsLoopbackBind(addr string) bool`
  - `sshgw.BuildServerConfig(hostKey ssh.Signer, authorized []ssh.PublicKey, listenAddr string) (*ssh.ServerConfig, error)`

- [ ] **Step 1: Write the failing test**

Create `cli/sshgw/auth_test.go`:

```go
//go:build !js

package sshgw

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestLoadOrCreateHostKey_CreatesThenReuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssh_host_ed25519_key")

	first, err := LoadOrCreateHostKey(path)
	if err != nil {
		t.Fatalf("first LoadOrCreateHostKey: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("host key mode = %o, want 600", got)
	}

	// A regenerated host key makes every subsequent ssh refuse to connect with
	// a host-key-changed warning, so stability is the property under test.
	second, err := LoadOrCreateHostKey(path)
	if err != nil {
		t.Fatalf("second LoadOrCreateHostKey: %v", err)
	}
	if string(first.PublicKey().Marshal()) != string(second.PublicKey().Marshal()) {
		t.Error("host key changed between runs; known_hosts would break")
	}
}

func TestLoadAuthorizedKeys(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	line := string(ssh.MarshalAuthorizedKey(sshPub))

	path := filepath.Join(t.TempDir(), "keys")
	content := "# a comment\n\n" + `no-pty,command="x" ` + line + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	keys, err := LoadAuthorizedKeys(path)
	if err != nil {
		t.Fatalf("LoadAuthorizedKeys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("got %d keys, want 1 (comments and blank lines are skipped, options are not keys)", len(keys))
	}
	if string(keys[0].Marshal()) != string(sshPub.Marshal()) {
		t.Error("parsed key does not match the one written")
	}
}

func TestLoadAuthorizedKeys_MissingFileIsAnError(t *testing.T) {
	if _, err := LoadAuthorizedKeys(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("want an error for a missing file, not an empty allow-list")
	}
}

func TestIsLoopbackBind(t *testing.T) {
	yes := []string{"127.0.0.1:2222", "127.0.0.53:2222", "[::1]:2222", "localhost:2222"}
	no := []string{"0.0.0.0:2222", ":2222", "192.168.1.10:2222", "[::]:2222"}
	for _, a := range yes {
		if !IsLoopbackBind(a) {
			t.Errorf("IsLoopbackBind(%q) = false, want true", a)
		}
	}
	for _, a := range no {
		if IsLoopbackBind(a) {
			t.Errorf("IsLoopbackBind(%q) = true, want false", a)
		}
	}
}

func TestBuildServerConfig_BindAuthCoupling(t *testing.T) {
	key := testHostKey(t)

	cfg, err := BuildServerConfig(key, nil, "127.0.0.1:2222")
	if err != nil {
		t.Fatalf("loopback with no keys must start: %v", err)
	}
	if !cfg.NoClientAuth {
		t.Error("loopback with no keys should serve without auth")
	}

	if _, err := BuildServerConfig(key, nil, "0.0.0.0:2222"); err == nil {
		t.Error("a non-loopback bind with no keys must be refused at startup")
	}

	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err = BuildServerConfig(key, []ssh.PublicKey{sshPub}, "0.0.0.0:2222")
	if err != nil {
		t.Fatalf("non-loopback with keys must start: %v", err)
	}
	if cfg.NoClientAuth {
		t.Error("a configuration with keys must not also accept unauthenticated clients")
	}
	if cfg.PublicKeyCallback == nil {
		t.Fatal("want a PublicKeyCallback when keys are configured")
	}
	if _, err := cfg.PublicKeyCallback(nil, sshPub); err != nil {
		t.Errorf("configured key rejected: %v", err)
	}
	otherPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	otherSSH, err := ssh.NewPublicKey(otherPub)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.PublicKeyCallback(nil, otherSSH); err == nil {
		t.Error("an unconfigured key was accepted")
	}
}

func testHostKey(t *testing.T) ssh.Signer {
	t.Helper()
	signer, err := LoadOrCreateHostKey(filepath.Join(t.TempDir(), "hk"))
	if err != nil {
		t.Fatal(err)
	}
	return signer
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cli/sshgw/ -run 'TestLoad|TestIsLoopback|TestBuildServerConfig' -v`
Expected: FAIL — undefined symbols.

- [ ] **Step 3: Write the implementation**

Create `cli/sshgw/auth.go`:

```go
//go:build !js

package sshgw

import (
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
)

// LoadOrCreateHostKey reads the ed25519 host key at path, generating and
// persisting one on first run — the same "the file is the origin" shape the
// server's --psk-file uses.
//
// Stability is the point. A host key regenerated per run makes every
// subsequent ssh print a host-key-changed warning and refuse to connect, so
// this never overwrites an existing file, and a parse failure is an error
// rather than a silent regeneration.
func LoadOrCreateHostKey(path string) (ssh.Signer, error) {
	pemBytes, err := os.ReadFile(path)
	switch {
	case err == nil:
		signer, perr := ssh.ParsePrivateKey(pemBytes)
		if perr != nil {
			return nil, fmt.Errorf("ssh-gateway: host key %s is unreadable (move it aside to regenerate): %w", path, perr)
		}
		return signer, nil
	case !os.IsNotExist(err):
		return nil, fmt.Errorf("ssh-gateway: host key %s: %w", path, err)
	}

	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, fmt.Errorf("ssh-gateway: generate host key: %w", err)
	}
	// MarshalPrivateKey takes the VALUE type; ParseRawPrivateKey hands back a
	// *ed25519.PrivateKey. ssh.ParsePrivateKey above hides that asymmetry, so
	// do not reintroduce it here by taking an address.
	block, err := ssh.MarshalPrivateKey(priv, "harness ssh-gateway")
	if err != nil {
		return nil, fmt.Errorf("ssh-gateway: marshal host key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("ssh-gateway: host key dir: %w", err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return nil, fmt.Errorf("ssh-gateway: write host key %s: %w", path, err)
	}
	return ssh.NewSignerFromKey(priv)
}

// LoadAuthorizedKeys parses an OpenSSH authorized_keys file. A missing file is
// an error: the caller asked for key authentication, and returning an empty
// list would turn that request into "accept nobody" or, worse, be read as
// "authentication not configured".
func LoadAuthorizedKeys(path string) ([]ssh.PublicKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ssh-gateway: authorized keys %s: %w", path, err)
	}
	var keys []ssh.PublicKey
	rest := raw
	for len(rest) > 0 {
		key, _, _, remainder, perr := ssh.ParseAuthorizedKey(rest)
		if perr != nil {
			// ParseAuthorizedKey consumes comments and blank lines itself and
			// only errors when nothing parseable remains.
			break
		}
		keys = append(keys, key)
		rest = remainder
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("ssh-gateway: authorized keys %s contains no usable key", path)
	}
	return keys, nil
}

// IsLoopbackBind reports whether addr binds only to this machine.
//
// An empty or unspecified host ("" / ":2222" / "0.0.0.0" / "::") is NOT
// loopback: those accept from every interface. Treating them as local is the
// bind-addr/dial-addr confusion that has bitten this project before.
func IsLoopbackBind(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// BuildServerConfig assembles the ssh server configuration and enforces the
// coupling between where the gateway listens and whether it authenticates.
//
// On loopback there is no authentication. Public keys would buy nothing there:
// an agent the runner starts runs as the operator's UID and can read the
// operator's own private key, so it would authenticate as the operator anyway,
// and a sandboxed agent cannot reach the listener at all.
//
// Off loopback that reasoning inverts — a different-UID reader on another
// machine is exactly the adversary — so keys become mandatory, enforced here
// rather than documented, and the failure is a refusal to start. Quietly
// serving 0.0.0.0 with no authentication is the widest possible reading of a
// mistyped flag.
func BuildServerConfig(hostKey ssh.Signer, authorized []ssh.PublicKey, listenAddr string) (*ssh.ServerConfig, error) {
	cfg := &ssh.ServerConfig{}
	if len(authorized) == 0 {
		if !IsLoopbackBind(listenAddr) {
			return nil, fmt.Errorf("ssh-gateway: --listen %s is not loopback, so --authorized-keys is required", listenAddr)
		}
		cfg.NoClientAuth = true
	} else {
		cfg.PublicKeyCallback = func(_ ssh.ConnMetadata, offered ssh.PublicKey) (*ssh.Permissions, error) {
			want := offered.Marshal()
			for _, k := range authorized {
				have := k.Marshal()
				if subtle.ConstantTimeCompare(have, want) == 1 {
					return &ssh.Permissions{}, nil
				}
			}
			return nil, fmt.Errorf("ssh-gateway: public key not in the authorized list")
		}
	}
	cfg.AddHostKey(hostKey)
	return cfg, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cli/sshgw/ -v`
Expected: PASS (Task 3's tests too).

- [ ] **Step 5: Commit**

```bash
git add cli/sshgw/auth.go cli/sshgw/auth_test.go
git commit -m "feat(sshgw): host key, authorized keys, and the bind/auth coupling"
```

---

### Task 5: The listener and one session channel

**Files:**
- Create: `cli/sshgw/gateway.go`, `cli/sshgw/session.go`
- Test: `cli/sshgw/gateway_test.go`

**Interfaces:**
- Consumes: `ParseUserName` (Task 3), `LoadOrCreateHostKey` / `LoadAuthorizedKeys` / `BuildServerConfig` (Task 4), `exec.WriteTerminalReset` / `PumpTerminalIO` (Task 1), `cli.Client.AttachSessionWithReplayLimit`.
- Produces:
  - `type Options struct { Listen, HostKeyPath, AuthorizedKeysPath string }`
  - `func Run(ctx context.Context, c *cli.Client, opts Options) error`
  - `func (g *gateway) claimControl(taskID string) bool` / `releaseControl(taskID string)` (unexported, tested directly)

- [ ] **Step 1: Write the failing test for the control-seat map**

Create `cli/sshgw/gateway_test.go`:

```go
//go:build !js

package sshgw

import (
	"sync"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Only the control seat is scarce. A second control session would take the
// seat server-side from whatever holds it — possibly the operator's own TUI —
// so the gateway refuses it instead of superseding. Cowriters and viewers are
// what those modes are for and are never counted.
func TestGateway_ControlSeatIsExclusivePerTask(t *testing.T) {
	g := newGateway()
	const a, b = "aaaa", "bbbb"

	if !g.claim(a, protocol.AttachMode_Control) {
		t.Fatal("first control claim on a free task must succeed")
	}
	if g.claim(a, protocol.AttachMode_Control) {
		t.Error("second control claim on the same task must be refused")
	}
	if !g.claim(b, protocol.AttachMode_Control) {
		t.Error("control on a different task must be unaffected")
	}
	if !g.claim(a, protocol.AttachMode_Cowrite) || !g.claim(a, protocol.AttachMode_Cowrite) {
		t.Error("cowriters must never be refused")
	}
	if !g.claim(a, protocol.AttachMode_View) {
		t.Error("viewers must never be refused")
	}

	g.release(a, protocol.AttachMode_Control)
	if !g.claim(a, protocol.AttachMode_Control) {
		t.Error("the seat must be reusable once released")
	}
}

// Releasing a mode that never claims must not free somebody else's seat.
func TestGateway_ReleasingACowriterKeepsTheSeat(t *testing.T) {
	g := newGateway()
	const id = "aaaa"
	if !g.claim(id, protocol.AttachMode_Control) {
		t.Fatal("claim")
	}
	g.release(id, protocol.AttachMode_Cowrite)
	if g.claim(id, protocol.AttachMode_Control) {
		t.Error("a cowriter's release freed the control seat")
	}
}

func TestGateway_ClaimIsRaceFree(t *testing.T) {
	g := newGateway()
	const id = "aaaa"
	var wins int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if g.claim(id, protocol.AttachMode_Control) {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("%d concurrent claims succeeded, want exactly 1", wins)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cli/sshgw/ -run TestGateway -v`
Expected: FAIL — `undefined: newGateway`.

- [ ] **Step 3: Write gateway.go**

Create `cli/sshgw/gateway.go`:

```go
//go:build !js

package sshgw

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"golang.org/x/crypto/ssh"
)

// Options configures one gateway listener.
type Options struct {
	// Listen is the bind address. Off loopback, AuthorizedKeysPath becomes
	// mandatory (see BuildServerConfig).
	Listen string
	// HostKeyPath is the ed25519 host key; generated on first run.
	HostKeyPath string
	// AuthorizedKeysPath is optional on a loopback bind.
	AuthorizedKeysPath string
}

type gateway struct {
	client *cli.Client

	mu    sync.Mutex
	seats map[string]bool // task id → a control session is live here
}

func newGateway() *gateway { return &gateway{seats: map[string]bool{}} }

// claim reserves what the mode needs. Only control needs anything: it is the
// one attach that evicts another client server-side (SessionMux.Attach is a
// takeover), so a second one through this gateway would take the seat from
// whatever holds it — including the operator's own TUI. Refusing is visible;
// taking is not.
func (g *gateway) claim(taskID string, mode protocol.AttachMode) bool {
	if mode != protocol.AttachMode_Control {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.seats[taskID] {
		return false
	}
	g.seats[taskID] = true
	return true
}

func (g *gateway) release(taskID string, mode protocol.AttachMode) {
	if mode != protocol.AttachMode_Control {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.seats, taskID)
}

// Run serves ssh connections until ctx is cancelled or the listener fails.
//
// c is an already-connected client — the TUI passes its long-lived one, and
// harness-cli passes the one it dialled. The gateway never dials: a listener
// has no short-lived form for a *With split to distinguish.
func Run(ctx context.Context, c *cli.Client, opts Options) error {
	hostKey, err := LoadOrCreateHostKey(opts.HostKeyPath)
	if err != nil {
		return err
	}
	var authorized []ssh.PublicKey
	if opts.AuthorizedKeysPath != "" {
		if authorized, err = LoadAuthorizedKeys(opts.AuthorizedKeysPath); err != nil {
			return err
		}
	}
	cfg, err := BuildServerConfig(hostKey, authorized, opts.Listen)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", opts.Listen)
	if err != nil {
		return fmt.Errorf("ssh-gateway: listen %s: %w", opts.Listen, err)
	}
	defer ln.Close()
	go func() { <-ctx.Done(); _ = ln.Close() }()

	g := newGateway()
	g.client = c
	for {
		nConn, aerr := ln.Accept()
		if aerr != nil {
			if ctx.Err() != nil {
				return nil // cancelled: an ordinary stop, not a failure
			}
			return fmt.Errorf("ssh-gateway: accept: %w", aerr)
		}
		go g.serveConn(ctx, nConn, cfg)
	}
}

func (g *gateway) serveConn(ctx context.Context, nConn net.Conn, cfg *ssh.ServerConfig) {
	sshConn, chans, reqs, err := ssh.NewServerConn(nConn, cfg)
	if err != nil {
		_ = nConn.Close()
		return
	}
	defer sshConn.Close()
	go ssh.DiscardRequests(reqs)

	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			// direct-tcpip lands here: `ssh -L` through the gateway would be a
			// second, drifting path to `harness-cli forward`.
			_ = newCh.Reject(ssh.UnknownChannelType,
				fmt.Sprintf("ssh-gateway serves only session channels (got %q); use `harness-cli forward` for port forwarding", newCh.ChannelType()))
			continue
		}
		go g.serveSession(ctx, sshConn.User(), newCh)
	}
}
```

- [ ] **Step 4: Run the seat tests**

Run: `go test ./cli/sshgw/ -run TestGateway -v`
Expected: PASS. (`serveSession` does not exist yet, so the package will not compile — write Step 5 first if the compiler objects; the test above only exercises `newGateway`/`claim`/`release`.)

- [ ] **Step 5: Write session.go**

Create `cli/sshgw/session.go`:

```go
//go:build !js

package sshgw

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/on-keyday/agent-harness/runner/protocol"
	agentexec "github.com/on-keyday/objtrsf/exec"
	"golang.org/x/crypto/ssh"
)

// ptyDims are the four numbers pty-req and window-change both carry. SSH puts
// COLUMNS first; SetTerminalWindowSize takes ROWS first. Decoding into named
// fields is what keeps the two orders from being silently swapped — a
// transposed size renders a plausible-looking but wrong screen.
type ptyDims struct {
	Cols, Rows, WidthPx, HeightPx uint16
}

// parseWindowChange decodes a window-change payload: four uint32s.
func parseWindowChange(payload []byte) (ptyDims, error) {
	if len(payload) < 16 {
		return ptyDims{}, fmt.Errorf("window-change payload is %d bytes, want 16", len(payload))
	}
	return ptyDims{
		Cols:     uint16(binary.BigEndian.Uint32(payload[0:4])),
		Rows:     uint16(binary.BigEndian.Uint32(payload[4:8])),
		WidthPx:  uint16(binary.BigEndian.Uint32(payload[8:12])),
		HeightPx: uint16(binary.BigEndian.Uint32(payload[12:16])),
	}, nil
}

// parsePtyReq decodes a pty-req payload: a TERM string, then the same four
// uint32s, then encoded modes. TERM is read and discarded — the runner-side
// PTY's TERM is fixed when the session is created, and changing it mid-session
// would change what the already-running agent renders.
func parsePtyReq(payload []byte) (ptyDims, error) {
	if len(payload) < 4 {
		return ptyDims{}, fmt.Errorf("pty-req payload is %d bytes, too short for the TERM length", len(payload))
	}
	termLen := binary.BigEndian.Uint32(payload[0:4])
	rest := payload[4:]
	if uint32(len(rest)) < termLen {
		return ptyDims{}, fmt.Errorf("pty-req TERM length %d exceeds the payload", termLen)
	}
	return parseWindowChange(rest[termLen:])
}

// serveSession runs one ssh session channel against one attach stream.
func (g *gateway) serveSession(ctx context.Context, user string, newCh ssh.NewChannel) {
	taskID, mode, err := ParseUserName(user)
	if err != nil {
		_ = newCh.Reject(ssh.Prohibited, err.Error())
		return
	}
	if !g.claim(taskID, mode) {
		_ = newCh.Reject(ssh.Prohibited,
			fmt.Sprintf("ssh-gateway: task %s already has a control session through this gateway; connect without .control to co-write, or .view to watch", taskID))
		return
	}
	defer g.release(taskID, mode)

	ch, requests, err := newCh.Accept()
	if err != nil {
		return
	}
	defer ch.Close()

	// Wait for pty-req/shell before attaching: the initial size rides pty-req,
	// and attaching first would mean a replay painted at the wrong size.
	var dims ptyDims
	var haveDims bool
	started := false
	for req := range requests {
		switch req.Type {
		case "pty-req":
			d, perr := parsePtyReq(req.Payload)
			if perr != nil {
				fmt.Fprintf(ch.Stderr(), "ssh-gateway: %v\r\n", perr)
				_ = req.Reply(false, nil)
				continue
			}
			dims, haveDims = d, true
			_ = req.Reply(true, nil)
		case "shell":
			_ = req.Reply(true, nil)
			started = true
		case "exec", "subsystem":
			fmt.Fprintf(ch.Stderr(),
				"ssh-gateway: %s is not served here — this gateway attaches to a session's PTY. For files use `harness-cli file push/pull`.\r\n", req.Type)
			_ = req.Reply(false, nil)
		default:
			_ = req.Reply(false, nil)
		}
		if started {
			break
		}
	}
	if !started {
		return
	}

	g.attachAndPump(ctx, ch, requests, taskID, mode, dims, haveDims)
}

func (g *gateway) attachAndPump(ctx context.Context, ch ssh.Channel, requests <-chan *ssh.Request,
	taskID string, mode protocol.AttachMode, dims ptyDims, haveDims bool) {

	stream, replayBytes, kind, err := g.client.AttachSessionWithReplayLimit(ctx, taskID, mode, 0)
	if err != nil {
		fmt.Fprintf(ch.Stderr(), "ssh-gateway: %v\r\n", err)
		sendExit(ch, 1)
		return
	}
	defer stream.Close()

	if kind == protocol.TaskKind_Stream {
		fmt.Fprintf(ch.Stderr(),
			"ssh-gateway: task %s is an event-stream session (structured events, no terminal): use `harness-cli session stream attach %s`\r\n", taskID, taskID)
		sendExit(ch, 1)
		return
	}

	// Both-or-nothing, matching applyInitialWindowSize: a PTY sized 40x0 is not
	// a smaller terminal, it is a broken one.
	if haveDims && dims.Rows != 0 && dims.Cols != 0 {
		if serr := stream.SetTerminalWindowSize(dims.Rows, dims.Cols, dims.WidthPx, dims.HeightPx); serr != nil {
			fmt.Fprintf(ch.Stderr(), "ssh-gateway: send initial size: %v\r\n", serr)
			sendExit(ch, 1)
			return
		}
	} else {
		fmt.Fprintf(ch.Stderr(),
			"ssh-gateway: no usable terminal size from pty-req; the session keeps the size it had (full-screen apps may mis-render)\r\n")
	}
	fmt.Fprintf(ch.Stderr(), "ssh-gateway: attached to %s as %s (replay %d bytes; Ctrl+] detaches)\r\n", taskID, mode, replayBytes)

	// window-change arrives while the pump runs, so it needs its own reader.
	go func() {
		for req := range requests {
			if req.Type != "window-change" {
				_ = req.Reply(false, nil)
				continue
			}
			d, perr := parseWindowChange(req.Payload)
			if perr != nil || d.Rows == 0 || d.Cols == 0 {
				continue
			}
			_ = stream.SetTerminalWindowSize(d.Rows, d.Cols, d.WidthPx, d.HeightPx)
		}
	}()

	go func() { _, _ = io.Copy(ch.Stderr(), stream.Stderr()) }()

	// The pump owns detach-key interception and the half-close that the server
	// reads as a detach. Reset the client's terminal before the channel closes:
	// this is one of the two endings the gateway is present for. A client that
	// disconnects on its own (~., a closed window, a dropped link) is gone
	// before this line, and nothing can be delivered to it.
	err = stream.PumpTerminalIO(ch, ch)
	agentexec.WriteTerminalReset(ch)
	if err != nil {
		fmt.Fprintf(ch.Stderr(), "ssh-gateway: session ended: %v\r\n", err)
		sendExit(ch, 1)
		return
	}
	sendExit(ch, 0)
}

func sendExit(ch ssh.Channel, code uint32) {
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
}
```

- [ ] **Step 6: Add payload-decode tests**

Append to `cli/sshgw/gateway_test.go`:

```go
func TestParsePtyReq(t *testing.T) {
	// "xterm-256color", 120 cols, 40 rows, 960x480 px, empty modes.
	term := "xterm-256color"
	payload := make([]byte, 0, 4+len(term)+20)
	payload = appendU32(payload, uint32(len(term)))
	payload = append(payload, term...)
	payload = appendU32(payload, 120)
	payload = appendU32(payload, 40)
	payload = appendU32(payload, 960)
	payload = appendU32(payload, 480)
	payload = appendU32(payload, 0)

	d, err := parsePtyReq(payload)
	if err != nil {
		t.Fatalf("parsePtyReq: %v", err)
	}
	// The whole point of the named fields: SSH sends columns first.
	if d.Cols != 120 || d.Rows != 40 || d.WidthPx != 960 || d.HeightPx != 480 {
		t.Errorf("got %+v, want {Cols:120 Rows:40 WidthPx:960 HeightPx:480}", d)
	}
}

func TestParsePtyReq_Short(t *testing.T) {
	if _, err := parsePtyReq([]byte{0, 0, 0, 9}); err == nil {
		t.Error("want an error when the TERM length exceeds the payload")
	}
	if _, err := parsePtyReq(nil); err == nil {
		t.Error("want an error for an empty payload")
	}
}

func TestParseWindowChange_ZeroIsDecodedNotRejected(t *testing.T) {
	// A zero size decodes fine; it is the CALLER that declines to send it on.
	payload := make([]byte, 0, 16)
	payload = appendU32(payload, 0)
	payload = appendU32(payload, 0)
	payload = appendU32(payload, 0)
	payload = appendU32(payload, 0)
	d, err := parseWindowChange(payload)
	if err != nil {
		t.Fatalf("parseWindowChange: %v", err)
	}
	if d.Rows != 0 || d.Cols != 0 {
		t.Errorf("got %+v, want zeroes", d)
	}
}

func appendU32(b []byte, v uint32) []byte {
	return append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}
```

- [ ] **Step 7: Run the package suite**

Run: `go test ./cli/sshgw/ -v`
Expected: PASS.

Then: `make wasm-check`
Expected: PASS — this is the check that catches a `//go:build !js` missing from any of the four new files.

- [ ] **Step 8: Commit**

```bash
git add cli/sshgw/
git commit -m "feat(sshgw): serve ssh session channels against attach streams"
```

---

### Task 6: End-to-end test through a real server and runner

**Files:**
- Create: `integration/ssh_gateway_test.go`
- Test: itself

**Interfaces:**
- Consumes: everything from Tasks 3–5.
- Produces: nothing consumed by later tasks.

- [ ] **Step 1: Write the test**

Model the boot sequence on `integration/port_forward_test.go` — `initRepo(t)`, `clearAgentEnv(t)`, `server.New(server.Config{Addr: …, DataDir: t.TempDir()})`, a runner pointed at `../testdata/fake-claude-slow.sh`, then a `session new` so there is a live detachable PTY. Reuse those helpers rather than writing new ones; read that file first and copy its shape exactly.

Create `integration/ssh_gateway_test.go` with these cases, each asserting against observable state rather than against a frame having been written:

```go
//go:build !js

package integration

// TestSSHGatewayE2E boots server + runner + one interactive session, starts a
// gateway on a free loopback port, and drives it with golang.org/x/crypto/ssh
// AS A CLIENT — so the suite does not depend on an `ssh` binary being present.
//
// Sub-tests, in order:
//
//   attach            bare user name → pty-req → shell → the replay burst
//                     arrives on the channel
//   resize_free_seat  window-change from the bare (cowrite) session with no
//                     controller attached CHANGES the size the session reports
//   resize_held_seat  the same window-change while a .control session is live
//                     does NOT change it — the seat rule. Asserting only that
//                     the frame was written would pass either way, which is
//                     the whole reason this case is here
//   second_cowrite    a second bare connection to the same task is ACCEPTED
//   second_control    a second .control connection is REFUSED, and the
//                     rejection text names the task
//   detach_key        writing 0x1d ends the ssh session, the task stays alive
//                     and is re-attachable, and the last bytes on the channel
//                     are the terminal reset
//   wrong_user        an unparseable user name is rejected at channel open,
//                     not at authentication
//   exec_refused      an exec request is refused with a message on stderr
func TestSSHGatewayE2E(t *testing.T) {
	// ... build per the notes above
}
```

Write each sub-test in full. For `resize_free_seat` / `resize_held_seat`, read the size back with `cli.SessionSnapshot` (it reports the size the server replays) rather than inventing a new query.

- [ ] **Step 2: Run it**

Run: `go test ./integration/ -run TestSSHGatewayE2E -v`
Expected: PASS. If `resize_held_seat` fails by observing a size change, the bug is real and is in the test's assumption about which client holds the seat — not in the server; `applyObserverWinSize` honours an observer's size only while the seat is empty.

- [ ] **Step 3: Run the whole integration suite**

Run: `make test-integration`
Expected: PASS. Note `server/`'s `TestOpenInteractiveSessionMux` is flaky at roughly 1 run in 4 and is pre-existing — re-run before attributing it to this diff.

- [ ] **Step 4: Commit**

```bash
git add integration/ssh_gateway_test.go
git commit -m "test(sshgw): end-to-end ssh gateway against a live server and runner"
```

---

### Task 7: The `harness-cli ssh-gateway` verb

**Files:**
- Modify: `cmd/harness-cli/main.go` (verb dispatch near `case "forward":` at :604, usage text near :1095)
- Test: manual, per the steps below

**Interfaces:**
- Consumes: `sshgw.Run`, `sshgw.Options`.
- Produces: the `ssh-gateway` verb.

- [ ] **Step 1: Add the verb**

In the dispatch switch, after `case "forward":`, add:

```go
	case "ssh-gateway":
		fs := flag.NewFlagSet("ssh-gateway", flag.ExitOnError)
		listen := fs.String("listen", "127.0.0.1:2222", "ssh listen host:port (loopback unless --authorized-keys is given)")
		hostKey := fs.String("host-key", "", "ssh host key path (default: alongside the workspace config; generated on first run)")
		authKeys := fs.String("authorized-keys", "", "OpenSSH authorized_keys file; optional on a loopback bind, required otherwise")
		if err := fs.Parse(args); err != nil {
			die(err)
		}
		c, err := cli.Dial(ctx, parseCID(), protocol.ClientKind_Cli)
		if err != nil {
			die(err)
		}
		defer c.Close()
		fmt.Fprintf(os.Stderr, "harness-cli: ssh gateway on %s — `ssh -p %s <32-hex-task-id>@%s`; Ctrl-C to stop\n",
			*listen, portOf(*listen), hostOf(*listen))
		if err := sshgw.Run(ctx, c, sshgw.Options{
			Listen:             *listen,
			HostKeyPath:        defaultHostKeyPath(*hostKey, *configPath),
			AuthorizedKeysPath: *authKeys,
		}); err != nil {
			die(err)
		}
		return
```

`defaultHostKeyPath` resolves an empty `--host-key` to `filepath.Join(filepath.Dir(resolved config path), "ssh_host_ed25519_key")`, using the same resolution `workspace.Load` already performs at `main.go:66` — take the path it returns rather than re-deriving it. `hostOf` / `portOf` are two-line helpers for the hint line; if `net.SplitHostPort` fails, print the raw address instead of guessing.

- [ ] **Step 2: Add the usage text**

In `usage()`, immediately after the `forward kill` block (around :1094), add lines in the same style:

```go
	fmt.Fprintln(os.Stderr, "  ssh-gateway [--listen 127.0.0.1:2222] [--host-key PATH] [--authorized-keys PATH]")
	fmt.Fprintln(os.Stderr, "                                      serve ssh; `ssh -p 2222 <32-hex-task-id>@127.0.0.1` attaches to that task")
	fmt.Fprintln(os.Stderr, "                                      user name picks the mode: bare = cowrite, .control takes the seat, .view watches")
	fmt.Fprintln(os.Stderr, "                                      Ctrl+] detaches (ssh's own ~. disconnects instead, and leaves the terminal unreset)")
	fmt.Fprintln(os.Stderr, "                                      no ssh auth on a loopback bind; --authorized-keys is required off loopback")
	fmt.Fprintln(os.Stderr, "                                      foreground; Ctrl-C stops it and every session it serves")
```

- [ ] **Step 3: Build and check the help**

```bash
go build -o /dev/null ./cmd/harness-cli
make vet
./bin/harness-cli 2>&1 | grep -A 5 ssh-gateway   # after `make build`
```
Expected: compiles; the usage block lists the verb. (`go build -o /dev/null` — a bare `go build ./cmd/harness-cli` would drop a binary in the worktree root.)

- [ ] **Step 4: Smoke-test against a dummy harness**

```bash
scripts/dummy-harness.sh up --detach --agent fake --name SSHGW
# eval the env it prints, then:
./bin/harness-cli session new --repo <repo>      # note the task id
./bin/harness-cli ssh-gateway --listen 127.0.0.1:2222 &
ssh -p 2222 -o StrictHostKeyChecking=no <task-id>@127.0.0.1
# expect: the session's screen; type; Ctrl+] returns you to your shell
scripts/dummy-harness.sh down
```
Expected: the screen appears, keystrokes reach the session, `Ctrl+]` detaches and the task survives (`harness-cli ls` still shows it). Follow the `dummy-harness` skill for the environment traps.

- [ ] **Step 5: Commit**

```bash
git add cmd/harness-cli/main.go
git commit -m "feat(cli): add the ssh-gateway verb"
```

---

### Task 8: The TUI command

**Files:**
- Modify: `tui/cmdline.go` (action types near :258, `isAction()` near :382, dispatch switch near :424, a new `parseSSHGateway`), `tui/app.go` (state near :137, action dispatch near :3111)
- Create: `tui/sshgateway.go`
- Test: `tui/sshgateway_test.go`

**Interfaces:**
- Consumes: `sshgw.Run`, `sshgw.Options`, `a.client`.
- Produces: `SSHGatewayAction{Sub string, Listen string}`, `DoStartSSHGateway`, `DoStopSSHGateway`.

- [ ] **Step 1: Write the failing parse test**

Create `tui/sshgateway_test.go`:

```go
package tui

import "testing"

func TestParseSSHGateway(t *testing.T) {
	cases := []struct {
		in       []string
		wantSub  string
		wantAddr string
		wantErr  bool
	}{
		{nil, "status", "", false},
		{[]string{"start"}, "start", "127.0.0.1:2222", false},
		{[]string{"start", "127.0.0.1:2300"}, "start", "127.0.0.1:2300", false},
		{[]string{"stop"}, "stop", "", false},
		{[]string{"bogus"}, "", "", true},
		{[]string{"start", "a", "b"}, "", "", true},
	}
	for _, tc := range cases {
		act, err := parseSSHGateway(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseSSHGateway(%v) = nil error, want one", tc.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("parseSSHGateway(%v): %v", tc.in, err)
		}
		g, ok := act.(SSHGatewayAction)
		if !ok {
			t.Fatalf("parseSSHGateway(%v) returned %T", tc.in, act)
		}
		if g.Sub != tc.wantSub || g.Listen != tc.wantAddr {
			t.Errorf("parseSSHGateway(%v) = %+v, want sub=%q listen=%q", tc.in, g, tc.wantSub, tc.wantAddr)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./tui/ -run TestParseSSHGateway -v`
Expected: FAIL — `undefined: parseSSHGateway`.

- [ ] **Step 3: Add the action, the parser and the dispatch entry**

In `tui/cmdline.go`, alongside `ForwardLsAction` (:258):

```go
// SSHGatewayAction starts, stops or reports the ssh gateway this TUI hosts.
// Sub is "start", "stop" or "status".
type SSHGatewayAction struct {
	Sub    string
	Listen string
}
```

Add `func (SSHGatewayAction) isAction() {}` next to the others (:382), add the parser next to `parseForward` (:1069):

```go
// parseSSHGateway handles `ssh-gateway [start [addr] | stop]`. Unlike a
// forward, there is no modal: a gateway takes one optional address, where a
// forward spec is four fields with no default.
func parseSSHGateway(args []string) (Action, error) {
	const usage = "ssh-gateway: usage: ssh-gateway [start [bind:port] | stop]"
	if len(args) == 0 {
		return SSHGatewayAction{Sub: "status"}, nil
	}
	switch args[0] {
	case "start":
		addr := "127.0.0.1:2222"
		switch len(args) {
		case 1:
		case 2:
			addr = args[1]
		default:
			return nil, fmt.Errorf("%s", usage)
		}
		return SSHGatewayAction{Sub: "start", Listen: addr}, nil
	case "stop":
		if len(args) != 1 {
			return nil, fmt.Errorf("%s", usage)
		}
		return SSHGatewayAction{Sub: "stop"}, nil
	default:
		return nil, fmt.Errorf("ssh-gateway: unknown sub-verb %q (want start | stop)", args[0])
	}
}
```

and the dispatch entry next to `case "forward":` (:424):

```go
	case "ssh-gateway":
		return parseSSHGateway(tokens[1:])
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./tui/ -run TestParseSSHGateway -v`
Expected: PASS.

- [ ] **Step 5: Hold the listener in App state and wire the action**

In `tui/app.go`, next to the port-forward state (:137):

```go
	// ssh gateway: at most one per TUI. It is NOT in activeForwards — that map
	// is keyed per forward session and drives the task-scoped P/B stop keys and
	// workspace capture, none of which apply to a listener that serves every
	// task.
	sshGateway *SSHGatewaySession
```

Create `tui/sshgateway.go` holding `SSHGatewaySession{Listen string; Cancel context.CancelFunc}`, `DoStartSSHGateway(c *cli.Client, listen, hostKeyPath string, program *tea.Program) tea.Cmd` and `DoStopSSHGateway`. Follow `tui/portforward.go`'s shape exactly: the `Do*` takes `a.client` (never dials), starts the listener in a goroutine, and reports back through `program.Send` with `SSHGatewayStartedMsg` / `SSHGatewayStoppedMsg` / `SSHGatewayStatusMsg`, which `Update` folds into state. Read `tui/portforward.go` before writing this — matching that file's message flow is the requirement, not just compiling.

In the action switch (`tui/app.go:3111` area):

```go
	case SSHGatewayAction:
		if a.client == nil {
			a.cmdresult.Append(ErrorStyle.Render("ssh-gateway: not connected to server"))
			return a, nil
		}
		return a, a.runSSHGatewayAction(v)
```

`runSSHGatewayAction` reports through `a.cmdresult.Append` the way the forward commands do (`tui/app.go:535`): on start, the address plus the `ssh -p <port> <32-hex-task-id>@<host>` line the operator will paste into `~/.ssh/config`; on start when one is already running, an error naming the existing address; on stop with none running, an error saying so; on status, the address or "not running".

- [ ] **Step 6: Add a state test**

Append to `tui/sshgateway_test.go` a test in the shape of `tui/portforward_test.go`'s start/stop test: after a start message the App holds a session, after a stop message the entry is gone, and a second start while one is live is refused with a message rather than replacing it silently.

- [ ] **Step 7: Verify**

```bash
go test ./tui/ -v
make check
make vet
```
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add tui/
git commit -m "feat(tui): host the ssh gateway from the command line"
```

---

### Task 9: Documentation and the surface-parity walk

**Files:**
- Modify: `README.md` (the `harness-cli` verb list around :76–90, and the TUI section)
- Modify: `docs/superpowers/specs/2026-08-25-ssh-gateway-design.md` (only if the walk finds a cell the spec got wrong)

**Interfaces:**
- Consumes: the finished feature.
- Produces: documentation.

- [ ] **Step 1: Walk the surface-parity checklist**

Invoke the `surface-parity-checklist` skill and walk it item by item, writing a verdict per number. Do not summarize it. The spec's **Operator surface matrix** is the design-level answer and is what each verdict is checked against; a disagreement is a finding, not a formality.

- [ ] **Step 2: Update the README**

Add `ssh-gateway` to the `harness-cli` bullet list next to **Port forwarding**, with the three user-name forms and the loopback/auth rule. Add one line to the TUI section for the `ssh-gateway` command. Note the `~.` limitation where an operator will meet it: ssh's own disconnect leaves the terminal unreset, and `reset` fixes it.

- [ ] **Step 3: Verify the docs build nothing and commit**

```bash
git add README.md docs/
git commit -m "docs: document the ssh gateway across CLI and TUI"
```

---

### Task 10: Live verification, including the reset negative control

**Files:** none — this task produces observations, recorded in the spec.

- [ ] **Step 1: Bring up a dummy harness with a real agent session**

Per the `dummy-harness` skill, two-instance topology. Start a session running something full-screen (`htop`) for the screen-group runs.

- [ ] **Step 2: Confirm the ordinary path with a real client**

From a real terminal, and again from inside tmux:

```bash
ssh -p 2222 <task-id>@127.0.0.1
```
Check: the screen appears; **typed keystrokes reach the agent** (rendering is not proof that input works); resizing the terminal reflows the agent; `Ctrl+]` returns you to your shell with the task still listed by `harness-cli ls` and re-attachable from the TUI.

- [ ] **Step 3: Run the reset negative control**

Three runs, each detaching with `Ctrl+]` — never with `~.`, because that path writes nothing at all and would look broken either way:

| Run | Suppress | Scenario | Prediction |
| --- | --- | --- | --- |
| 1 | `exec.InputModeReset` | attach to a session whose app had mouse tracking on, detach, move the mouse at the local shell prompt | breaks — the server replays every tracked mode on attach |
| 2 | `exec.ScreenModeReset` | attach while `htop` runs, detach mid-run, use the local shell | genuinely open |
| 3 | both | either scenario | breaks |

Suppress by editing `WriteTerminalReset` in the local objtrsf checkout for the duration of the run; restore it afterwards. A run whose suppression leaves the terminal usable means that group is not carrying its weight in this path — drop it from `WriteTerminalReset`'s gateway use and record the observation.

- [ ] **Step 4: Confirm the documented limitation is real**

Once, disconnect with `~.` out of `htop` and check what the terminal is left in. This confirms § Detach's third row is observed behaviour rather than an assumption about what OpenSSH does on the way out.

- [ ] **Step 5: Record the findings in the spec**

Append a short "Observed" subsection to the spec's § Testing with what each run actually did. Per `feedback_verify_before_memory_writes`, no memory file is written until the symptom has been seen to change with the fix applied.

```bash
git add docs/superpowers/specs/2026-08-25-ssh-gateway-design.md
git commit -m "docs: record what the ssh gateway's reset negative control showed"
```

---

## Self-Review

**Spec coverage.** Problem P1–P4 are addressed by Tasks 5–8 together (a listener reachable by any ssh client, ssh-config-shaped user names, foreground CLI verb for scripts, an ordinary ssh login for mosh). D1 → Task 5's `Run` signature. D2 → Tasks 7 and 8. D3/D11 → Task 3. D4 → Tasks 7 and 8 (both die with their process; no daemon). D5 → Task 4. D6 → Task 5's seat map. D7 → Task 5 uses `PumpTerminalIO`, which owns the interception. D8 → Task 5's `WriteTerminalReset` call plus Task 10's negative control. D9 → no task touches `.bgn`, `server/` or `runner/`. D10 → Tasks 1 and 2.

**Gap accepted deliberately:** the spec's Errors section lists a rejection message for `direct-tcpip`; Task 5 rejects every non-session channel type with one message that names the type, which covers it without a per-type list.

**Type consistency.** `ParseUserName` returns `(string, protocol.AttachMode, error)` and is called that way in Task 5. `claim`/`release` take `(taskID string, mode protocol.AttachMode)` in both the test and the implementation. `Options` field names (`Listen`, `HostKeyPath`, `AuthorizedKeysPath`) match between Tasks 5, 7 and 8. `WriteTerminalReset(w io.Writer)` matches its Task 1 definition and its Task 5 call. `PumpTerminalIO(in, out)` matches Task 1's rename and Task 5's `stream.PumpTerminalIO(ch, ch)`.
