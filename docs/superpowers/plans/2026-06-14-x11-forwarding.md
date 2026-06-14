# X11 Forwarding (session-coupled) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.
>
> **Project preconditions (every subagent):** Work in `/home/kforfk/workspace/remote-agent-harness/`, NOT in any `.harness-worktrees/<hash>/`. Verify branch with `git rev-parse --abbrev-ref HEAD` before writing. Read `.claude/skills/implementation-pitfalls/SKILL.md` in full first. Build-check with `go build ./...` / `go vet ./...` (never bare `go build ./cmd/<x>/` — it drops a binary into the worktree).

**Spec:** `docs/superpowers/specs/2026-06-14-x11-forwarding.md` (problem statement + decisions taken + out-of-scope; reviewers must read its Problem statement, per Pitfall 1).

**Goal:** Add `harness-cli session new --x11` so a GUI (X11) program launched inside the interactive session renders on the client's local X server, tunneled over the harness transport.

**Architecture:** Session-coupled (option A). The client extracts its local X server's MIT-MAGIC-COOKIE-1 and a display number `N`, and ships them in `OpenInteractiveRequest`. The runner writes a per-task `XAUTHORITY` file and injects `DISPLAY=127.0.0.1:N` + `XAUTHORITY=<file>` into the spawned shell's env, so any X client started in that shell connects to `127.0.0.1:(6000+N)`. The byte tunnel reuses the EXISTING `-R` remote-forward machinery: the client, right after opening the session, registers a remote forward binding `127.0.0.1:(6000+N)` on the runner and dialing the client's local X server. Runner→app and client→real-X-server are spliced bytewise by code that already exists; the only generic extension is UNIX-socket dial on the client side (Linux X servers default to `-nolisten tcp`).

**Tech Stack:** Go; `.bgn` schema (brgen codegen via `make protoregen`); existing `cli`/`server`/`runner` packages; `xauth` CLI on both client and runner hosts.

**Locked decisions (no implementer choices):**
- **Trusted forwarding only** (SSH `-Y` equivalent): the client's real cookie is copied to the runner. No SECURITY-extension / untrusted cookie translation. Justified by `project_protocol_stack_scope` + `feedback_individual_dogfood` (toy/dogfood scope).
- **Client picks the display number** (`--x11`, default `N=10`; `--x11-display N` to override). Rationale: the `OpenInteractive` response is sent before the runner runs (server fire-and-forwards `OpenExec` then returns immediately at `server/task_handler.go:681`), so the runner cannot report an allocated `N` back synchronously. Client-chosen `N` avoids a response round-trip. Collisions surface as a `-R` BindFailed error (observable, retry with another `N`).
- **Runner listens on TCP `127.0.0.1:(6000+N)`**, sets `DISPLAY=127.0.0.1:N`. This sidesteps UNIX-socket support on the runner side entirely (no `/tmp/.X11-unix` file management on the runner).
- **`--x11` is incompatible with `--detach`** (a detached session has no client process to host the tunnel) and is **only available on `session new`** (interactive). Both are validated as errors.
- **`xauth` required on both ends.** Missing `xauth`, or no parseable cookie for the client's `$DISPLAY`, is a hard error with a clear message. No fallback.
- **WebUI is out of scope** (no port-forward wiring there, no X server in a browser). CLI/TUI only; this plan ships the CLI path.

---

## File Structure

**Schema (one task, one place — `feedback_no_split_schemas`):**
- Modify: `runner/protocol/message.bgn` — add X11 fields to `OpenInteractiveRequest` and `OpenExecRunnerRequest`.
- Regenerate: `runner/protocol/message.go` via `make protoregen`.

**Client (`cli` package):**
- Modify: `cli/port_forward.go` — add `DialNetwork` to `RemoteForwardSpec`; branch `dialAndSplice` on it.
- Create: `cli/x11.go` — local cookie extraction, `$DISPLAY`→dial-target derivation, the `OpenInteractiveX11` request builder, and the `RunInteractiveX11` foreground driver.
- Create: `cli/x11_test.go` — unit tests for the pure helpers.
- Modify: `cli/open_interactive_native.go` — refactor `OpenInteractiveWithSelectorAndArgs` to delegate to a new x11-aware builder (keep one request builder).

**CLI binary:**
- Modify: `cmd/harness-cli/session.go` — `--x11` / `--x11-display` flags + validation + dispatch to `RunInteractiveX11`.

**Server (relay):**
- Modify: `server/task_handler.go` — thread x11 fields from `OpenInteractiveRequest` into `OpenExecRunnerRequest`.

**Runner:**
- Create: `runner/x11.go` — `writeXauthFile` (per-task `XAUTHORITY`) and `cleanupXauthFile`.
- Modify: `runner/agentenv.go` — `AgentEnvSpec` gains `X11Display int` + `X11AuthFile string`; `BuildAgentEnv` emits `DISPLAY`/`XAUTHORITY` when set.
- Modify: `runner/session.go` — in `handleOpenExec`, when `oer.X11Enabled()`, write the xauth file, populate the env spec, defer cleanup.
- Create: `runner/agentenv_test.go` additions (or new test) — assert `DISPLAY`/`XAUTHORITY` emission.

**Docs:**
- Modify: `README.md` (or the forwarding doc) — document `session new --x11`.

---

## Task 1: Schema — X11 fields on both request messages

**Files:**
- Modify: `runner/protocol/message.bgn` — add `format X11Forward` (above line 103); embed it under `if x11_enabled == 1` in `OpenExecRunnerRequest` (103-115) and `OpenInteractiveRequest` (502-512)
- Regenerate: `runner/protocol/message.go`

- [ ] **Step 1a: Define the shared `X11Forward` format**

The display number + cookie are identical in both messages, so factor them into
one named format (DRY; single source for the X11 byte layout). Insert this
ABOVE `format OpenExecRunnerRequest:` (currently line 103):

```
# X11Forward carries the data needed to forward X11 to the client's local X
# server: the display number N (app sees DISPLAY=127.0.0.1:N → TCP 6000+N) and
# the client's MIT-MAGIC-COOKIE-1 value (raw bytes, typically 16). Embedded
# conditionally in OpenInteractiveRequest and OpenExecRunnerRequest under
# `if x11_enabled == 1` so no bytes appear on the wire when X11 is off.
format X11Forward:
    display :u16
    cookie_len :u16
    cookie :[cookie_len]u8
```

- [ ] **Step 1b: Edit `OpenInteractiveRequest`**

Replace lines 502-512 with:

```
format OpenInteractiveRequest:
    repo_path_len :u16
    repo_path :[repo_path_len]u8
    selector :RunnerSelector
    extra_args :ClaudeArgs
    # See SubmitRequest.resume_task_id — same semantics, applied to the
    # interactive path (PTY claude with empty prompt).
    detachable :u1    # 1 = session new (detach on disconnect),
                      # 0 = legacy interactive (kill on disconnect)
    x11_enabled :u1   # 1 = client requested X11 forwarding; gates the
                      # conditional block below. Incompatible with detach (no
                      # client process to host the tunnel).
    reserved :u6
    resume_task_id :TaskID
    # X11 block is on the wire ONLY when x11_enabled == 1 (no invisible bytes
    # when disabled — feedback_no_schema_invisible_bytes).
    if x11_enabled == 1:
        x11 :X11Forward
```

- [ ] **Step 2: Edit `OpenExecRunnerRequest`**

Replace lines 103-115 with:

```
format OpenExecRunnerRequest:
    task_id :TaskID
    auth_ticket :[16]u8
    repo_path_len :u16
    repo_path :[repo_path_len]u8
    stream_id :u64    # bidi stream the server has just created toward this
                      # runner; runner looks it up via
                      # Transport.GetBidirectionalStream(id) and feeds it to
                      # exec.ExecuteCommand for an interactive PTY claude.
    extra_args :ClaudeArgs
    detachable :u1    # 1 = stream EOF → keep claude alive,
                      # 0 = stream EOF → SIGHUP ladder (legacy)
    x11_enabled :u1   # relayed from OpenInteractiveRequest.x11_enabled
    reserved :u6
    if x11_enabled == 1:
        x11 :X11Forward  # relayed verbatim from OpenInteractiveRequest.x11
```

- [ ] **Step 3: Regenerate**

Run: `make protoregen`
Expected: regenerates `runner/protocol/message.go` with no error (first run downloads ~20 MB brgen-kit). 

- [ ] **Step 4: Confirm generated accessors**

Run: `grep -n "X11Enabled\|SetX11Enabled\|func.*X11()\|SetX11\|X11Forward\|SetCookie" runner/protocol/message.go`
Expected, mirroring the existing `Detachable()`/`SetDetachable()` (`:u1` bit) and `Worker()`/`SetWorker()` (embedded format under `if origin == NotifyOrigin.worker`):
- On BOTH `OpenInteractiveRequest` and `OpenExecRunnerRequest`: `X11Enabled() bool`, `SetX11Enabled(bool)`, `SetX11(X11Forward)`, and a getter `X11() *X11Forward` that returns `nil` when `x11_enabled == 0`.
- On `X11Forward`: field `Display uint16`, field `Cookie []uint8`, `SetCookie([]byte)` (and `CookieLen uint16`).

If brgen named them differently, use the actual generated names in all later tasks.

- [ ] **Step 5: Build + commit**

Run: `go build ./... && go vet ./runner/protocol/`
Expected: builds clean.

```bash
git add runner/protocol/message.bgn runner/protocol/message.go
git commit -m "feat(proto): add X11 fields to OpenInteractive/OpenExec requests"
```

---

## Task 2: Client — UNIX-socket dial in remote forward

**Files:**
- Modify: `cli/port_forward.go` (RemoteForwardSpec struct ~210-218; `dialAndSplice` ~378-390; `ParseRemoteForwardSpec` ~244 to set default network)
- Test: `cli/port_forward_test.go`

Linux X servers usually listen only on UNIX `/tmp/.X11-unix/X0` (`-nolisten tcp` default), so the client side must be able to dial a UNIX socket. This is a small generic extension; `-R` callers keep working by defaulting `DialNetwork` to `"tcp"`.

- [ ] **Step 1: Write the failing test**

Add to `cli/port_forward_test.go`:

```go
func TestDialAndSplice_UnixTarget(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "echo.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		io.Copy(c, c) // echo
		c.Close()
	}()

	// dialTarget is the helper extracted in Step 3; it must resolve a unix spec.
	sp := RemoteForwardSpec{DialNetwork: "unix", DialHost: sock}
	conn, err := dialForwardTarget(sp)
	if err != nil {
		t.Fatalf("dial unix: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "ping" {
		t.Fatalf("got %q", buf)
	}
}
```

Add imports `io`, `net`, `path/filepath` to the test file if missing.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cli/ -run TestDialAndSplice_UnixTarget`
Expected: FAIL — `undefined: dialForwardTarget` and `RemoteForwardSpec has no field DialNetwork`.

- [ ] **Step 3: Add the field + dial helper, branch `dialAndSplice`**

In `cli/port_forward.go`, extend the struct (the block ending at lines 216-218):

```go
type RemoteForwardSpec struct {
	BindAddr   string
	RunnerPort int
	DialHost   string
	DialPort   int
	// DialNetwork selects how the client dials the local target: "tcp"
	// (default; DialHost:DialPort) or "unix" (DialHost is the socket path,
	// DialPort ignored). Used by X11 forwarding to reach a UNIX X server.
	DialNetwork string
}
```

Add the helper near `dialAndSplice`:

```go
// dialForwardTarget dials the client-side target described by sp, honoring
// sp.DialNetwork ("unix" → DialHost is a socket path; otherwise TCP).
func dialForwardTarget(sp RemoteForwardSpec) (net.Conn, error) {
	if sp.DialNetwork == "unix" {
		return net.Dial("unix", sp.DialHost)
	}
	return net.Dial("tcp", net.JoinHostPort(sp.DialHost, strconv.Itoa(sp.DialPort)))
}
```

Replace the `net.Dial("tcp", ...)` call inside `dialAndSplice` (line 384) with:

```go
	conn, err := dialForwardTarget(sp)
```

And update the failure log just below (line 386) to be network-aware:

```go
	if err != nil {
		logf(fmt.Sprintf("remote-forward: dial %s/%s failed: %v", sp.DialNetwork, sp.DialHost, err))
		_ = st.CloseBoth()
		return
	}
```

In `ParseRemoteForwardSpec` (the returned struct at line 244), set the default explicitly:

```go
	return RemoteForwardSpec{BindAddr: bind, RunnerPort: rport, DialHost: dhost, DialPort: dport, DialNetwork: "tcp"}, nil
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cli/ -run TestDialAndSplice_UnixTarget`
Expected: PASS.

- [ ] **Step 5: Build + commit**

Run: `go build ./... && go test ./cli/ -run TestParseRemoteForwardSpec`
Expected: builds; existing -R parse test still passes.

```bash
git add cli/port_forward.go cli/port_forward_test.go
git commit -m "feat(cli): support unix-socket dial target in remote forward"
```

---

## Task 3: Client — X11 helpers (cookie extraction + dial-target derivation)

**Files:**
- Create: `cli/x11.go`
- Create/Test: `cli/x11_test.go`

- [ ] **Step 1: Write the failing test**

Create `cli/x11_test.go`:

```go
package cli

import (
	"reflect"
	"testing"
)

func TestParseXauthCookie(t *testing.T) {
	// `xauth list` output lines: "<display>  <proto>  <hex-cookie>"
	out := "myhost/unix:0  MIT-MAGIC-COOKIE-1  0123456789abcdef0123456789abcdef\n" +
		"myhost/unix:1  MIT-MAGIC-COOKIE-1  ffffffffffffffffffffffffffffffff\n"
	cookie, err := parseXauthCookie(out, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef}
	if !reflect.DeepEqual(cookie, want) {
		t.Fatalf("cookie = %x, want %x", cookie, want)
	}
}

func TestParseXauthCookie_NoMatch(t *testing.T) {
	if _, err := parseXauthCookie("otherhost/unix:5  MIT-MAGIC-COOKIE-1  abcd\n", 0); err == nil {
		t.Fatal("expected error for missing display :0")
	}
}

func TestLocalXServerDialSpec(t *testing.T) {
	cases := []struct {
		display     string
		wantNetwork string
		wantHost    string
		wantPort    int
		wantErr     bool
	}{
		{":0", "unix", "/tmp/.X11-unix/X0", 0, false},
		{"unix:0", "unix", "/tmp/.X11-unix/X0", 0, false},
		{":2", "unix", "/tmp/.X11-unix/X2", 0, false},
		{"localhost:0", "tcp", "localhost", 6000, false},
		{"192.168.0.5:1", "tcp", "192.168.0.5", 6001, false},
		{"", "", "", 0, true},
	}
	for _, tc := range cases {
		net, host, port, err := localXServerDialSpec(tc.display)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%q: want err", tc.display)
			}
			continue
		}
		if err != nil || net != tc.wantNetwork || host != tc.wantHost || port != tc.wantPort {
			t.Errorf("%q: got (%q,%q,%d,%v)", tc.display, net, host, port, err)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cli/ -run 'TestParseXauthCookie|TestLocalXServerDialSpec'`
Expected: FAIL — undefined `parseXauthCookie`, `localXServerDialSpec`.

- [ ] **Step 3: Implement `cli/x11.go` pure helpers**

Create `cli/x11.go`:

```go
//go:build !js

package cli

import (
	"encoding/hex"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// parseXauthCookie extracts the MIT-MAGIC-COOKIE-1 hex value for display
// number n from `xauth list` output. Each line is
// "<display>  <proto>  <hex-cookie>"; the display column ends in ":<n>".
func parseXauthCookie(xauthList string, n int) ([]byte, error) {
	suffix := ":" + strconv.Itoa(n)
	for _, line := range strings.Split(xauthList, "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		if !strings.HasSuffix(f[0], suffix) || f[1] != "MIT-MAGIC-COOKIE-1" {
			continue
		}
		b, err := hex.DecodeString(f[2])
		if err != nil {
			return nil, fmt.Errorf("x11: bad cookie hex for display %s: %w", suffix, err)
		}
		return b, nil
	}
	return nil, fmt.Errorf("x11: no MIT-MAGIC-COOKIE-1 entry for display %s in xauth list (is the X server running and authorized?)", suffix)
}

// localXServerDialSpec parses a client-side DISPLAY value into a dial target
// for the REAL local X server. "[unix]:N" → unix socket /tmp/.X11-unix/XN;
// "host:N" → TCP host:(6000+N).
func localXServerDialSpec(display string) (network, host string, port int, err error) {
	if display == "" {
		return "", "", 0, fmt.Errorf("x11: DISPLAY is empty")
	}
	d := strings.TrimPrefix(display, "unix")
	// strip screen suffix ".S" if present: "host:N.S"
	colon := strings.LastIndex(d, ":")
	if colon < 0 {
		return "", "", 0, fmt.Errorf("x11: malformed DISPLAY %q", display)
	}
	hostPart := d[:colon]
	numPart := d[colon+1:]
	if dot := strings.IndexByte(numPart, '.'); dot >= 0 {
		numPart = numPart[:dot]
	}
	n, convErr := strconv.Atoi(numPart)
	if convErr != nil {
		return "", "", 0, fmt.Errorf("x11: bad display number in %q", display)
	}
	if hostPart == "" {
		return "unix", fmt.Sprintf("/tmp/.X11-unix/X%d", n), 0, nil
	}
	return "tcp", hostPart, 6000 + n, nil
}

// localX11Cookie shells out to `xauth list <DISPLAY>` and returns the cookie
// bytes for display number n. Requires xauth on the client PATH.
func localX11Cookie(display string, n int) ([]byte, error) {
	out, err := exec.Command("xauth", "list", display).Output()
	if err != nil {
		return nil, fmt.Errorf("x11: `xauth list %s` failed (is xauth installed and the X server authorized?): %w", display, err)
	}
	return parseXauthCookie(string(out), n)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cli/ -run 'TestParseXauthCookie|TestLocalXServerDialSpec'`
Expected: PASS.

- [ ] **Step 5: Build + commit**

Run: `go build ./...`

```bash
git add cli/x11.go cli/x11_test.go
git commit -m "feat(cli): X11 cookie extraction and DISPLAY dial-target parsing"
```

---

## Task 4: Client — thread X11 fields into the OpenInteractive request

**Files:**
- Modify: `cli/open_interactive_native.go:42-82`

Refactor the existing full-featured method to accept an optional `*X11Request`, keeping ONE request builder (DRY). Existing callers pass `nil`.

- [ ] **Step 1: Add the X11Request type + new builder**

In `cli/x11.go`, add:

```go
// X11Request carries the client-chosen display number and the local X
// server's cookie into OpenInteractiveRequest.
type X11Request struct {
	Display int    // N; app sees DISPLAY=127.0.0.1:N on the runner
	Cookie  []byte // MIT-MAGIC-COOKIE-1 value of the client's local X server
}
```

In `cli/open_interactive_native.go`, rename the body of `OpenInteractiveWithSelectorAndArgs` into a new method and make the old one delegate. Replace lines 42-82 with:

```go
func (c *Client) OpenInteractiveWithSelectorAndArgs(ctx context.Context, repoPath string, sel protocol.RunnerSelector, extraArgs []string, resumeTaskID string, detachable bool) (*agentexec.CommandExecutionStream, string, error) {
	return c.openInteractiveImpl(ctx, repoPath, sel, extraArgs, resumeTaskID, detachable, nil)
}

// openInteractiveImpl is the single OpenInteractive request builder. x11 is
// nil for non-X11 sessions; when set, x11_enabled + display + cookie are sent.
func (c *Client) openInteractiveImpl(ctx context.Context, repoPath string, sel protocol.RunnerSelector, extraArgs []string, resumeTaskID string, detachable bool, x11 *X11Request) (*agentexec.CommandExecutionStream, string, error) {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_OpenInteractive}
	oi := protocol.OpenInteractiveRequest{}
	oi.SetRepoPath([]byte(repoPath))
	oi.Selector = sel
	oi.ExtraArgs = protocol.ClaudeArgsFromStrings(extraArgs)
	if resumeTaskID != "" {
		tid, err := parseTaskIDHex(resumeTaskID)
		if err != nil {
			return nil, "", fmt.Errorf("OpenInteractive: parse resume id: %w", err)
		}
		oi.ResumeTaskId = tid
	}
	if detachable {
		oi.SetDetachable(true)
	}
	if x11 != nil {
		oi.SetX11Enabled(true) // set the discriminator BEFORE the embedded block
		f := protocol.X11Forward{Display: uint16(x11.Display)}
		f.SetCookie(x11.Cookie)
		oi.SetX11(f)
	}
	req.SetOpenInteractive(oi)

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, "", err
	}
	if resp.Kind != protocol.TaskControlKind_OpenInteractive {
		return nil, "", fmt.Errorf("expected OpenInteractive response, got kind=%v", resp.Kind)
	}
	oir := resp.OpenInteractive()
	if oir == nil {
		return nil, "", fmt.Errorf("OpenInteractive response variant missing")
	}
	if err := openInteractiveStatusError(repoPath, oir.Status); err != nil {
		return nil, "", err
	}

	taskIDHex := hex.EncodeToString(oir.TaskId.Id[:])

	st := peer.WaitForBidirectionalStream(ctx, c.Transport(), trsf.StreamID(oir.StreamId))
	if st == nil {
		return nil, taskIDHex, fmt.Errorf("exec stream %d not visible after OpenInteractive", oir.StreamId)
	}
	return agentexec.NewCommandExecutionStream(st), taskIDHex, nil
}
```

(Confirm the generated setters are `SetX11Enabled` / `SetX11Cookie` and field `X11Display`; adjust to Task 1 Step 4's actual names if different.)

- [ ] **Step 2: Build to verify**

Run: `go build ./... && go vet ./cli/`
Expected: builds clean; all existing callers of `OpenInteractiveWithSelectorAndArgs` unaffected.

- [ ] **Step 3: Commit**

```bash
git add cli/open_interactive_native.go cli/x11.go
git commit -m "feat(cli): thread optional X11 request into OpenInteractive builder"
```

---

## Task 5: Client — `RunInteractiveX11` foreground driver

**Files:**
- Modify: `cli/x11.go`

Open the session (x11 fields set), launch the `-R` remote forward in the background (bind `127.0.0.1:(6000+N)` on the runner, dial the local X server), run the PTY in the foreground, and cancel the forward on exit. Mirrors `InteractiveWithSelectorAndArgs` (open_interactive_native.go:128-144) but interleaves the forward.

- [ ] **Step 1: Implement the driver**

Add to `cli/x11.go` (imports: `context`, `fmt`, `os`, and the `protocol` package; add a `//go:build !js` already present):

```go
// RunInteractiveX11 opens an interactive session with X11 forwarding enabled,
// runs an `-R` remote forward (runner 127.0.0.1:6000+N → client's local X
// server) in the background for its lifetime, and drives the PTY in the
// foreground. displayN is the client-chosen display number. Requires xauth on
// the client and a running, authorized local X server (via $DISPLAY).
func (c *Client) RunInteractiveX11(ctx context.Context, repo string, sel protocol.RunnerSelector, extraArgs []string, resumeTaskID string, displayN int) (string, error) {
	display := os.Getenv("DISPLAY")
	network, host, port, err := localXServerDialSpec(display)
	if err != nil {
		return "", err
	}
	cookie, err := localX11Cookie(display) // derives the display number internally
	if err != nil {
		return "", err
	}

	stream, taskIDHex, err := c.openInteractiveImpl(ctx, repo, sel, extraArgs, resumeTaskID, true /*detachable*/, &X11Request{Display: displayN, Cookie: cookie})
	if err != nil {
		return taskIDHex, err
	}
	defer stream.Close()

	// Background -R forward: runner binds 127.0.0.1:(6000+displayN); each X
	// client connection is dialed to the client's local X server.
	fctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sp := RemoteForwardSpec{
		BindAddr:    "127.0.0.1",
		RunnerPort:  6000 + displayN,
		DialNetwork: network,
		DialHost:    host,
		DialPort:    port,
	}
	go func() {
		logf := func(s string) { fmt.Fprintln(os.Stderr, "x11: "+s) }
		if err := RunRemoteForward(fctx, c, taskIDHex, []RemoteForwardSpec{sp}, logf); err != nil {
			fmt.Fprintln(os.Stderr, "x11 forward: "+err.Error())
		}
	}()

	fmt.Fprintf(os.Stderr, "harness-cli: X11 session %s (remote DISPLAY=127.0.0.1:%d -> local %s; Ctrl+] detach, Ctrl+D/exit ends)\n", taskIDHex, displayN, display)
	if err := stream.RemoteShell(); err != nil {
		return taskIDHex, err
	}
	return taskIDHex, nil
}
```

(`localX11Cookie` and `localXServerDialSpec` both derive the display number from
`$DISPLAY` internally via `x11DisplayNumber` — added in Task 3's refactor — so no
separate `displayNumber` helper is needed here.)

(Note: `RunInteractiveX11` always opens detachable=true to match `session new`; `--x11`+`--detach` is rejected at the CLI layer in Task 8, so the session is never actually detached out from under the forward.)

- [ ] **Step 2: Build to verify**

Run: `go build ./... && go vet ./cli/`
Expected: builds clean.

- [ ] **Step 3: Commit**

```bash
git add cli/x11.go
git commit -m "feat(cli): RunInteractiveX11 foreground driver with background -R forward"
```

---

## Task 6: Server — relay X11 fields into OpenExecRunnerRequest

**Files:**
- Modify: `server/task_handler.go:572-582`

- [ ] **Step 1: Thread the fields**

In `handleOpenInteractive`, where `oer` is built (lines 572-581), after the `if req.Detachable() { oer.SetDetachable(true) }` block, add:

```go
	if req.X11Enabled() {
		oer.SetX11Enabled(true) // discriminator first
		if f := req.X11(); f != nil {
			oer.SetX11(*f) // relay the whole X11Forward block verbatim
		}
	}
```

(`req` here is `*protocol.OpenInteractiveRequest`; `oer` is `protocol.OpenExecRunnerRequest`. Confirm field/method names match Task 1 Step 4.)

- [ ] **Step 2: Build to verify**

Run: `go build ./... && go vet ./server/`
Expected: builds clean.

- [ ] **Step 3: Commit**

```bash
git add server/task_handler.go
git commit -m "feat(server): relay X11 fields from OpenInteractive to runner"
```

---

## Task 7: Runner — write XAUTHORITY + inject DISPLAY/XAUTHORITY

**Files:**
- Create: `runner/x11.go`
- Modify: `runner/agentenv.go:18-37` (AgentEnvSpec) and `:39-74` (BuildAgentEnv)
- Modify: `runner/session.go:503-619` (handleOpenExec)
- Test: `runner/agentenv_test.go`

- [ ] **Step 1: Write the failing test for env emission**

Add to `runner/agentenv_test.go` (create if absent, `package runner`):

```go
func TestBuildAgentEnv_X11(t *testing.T) {
	env := BuildAgentEnv(AgentEnvSpec{
		X11Display:  10,
		X11AuthFile: "/tmp/harness-xauth-abc",
	})
	var gotDisplay, gotXauth bool
	for _, e := range env {
		if e == "DISPLAY=127.0.0.1:10" {
			gotDisplay = true
		}
		if e == "XAUTHORITY=/tmp/harness-xauth-abc" {
			gotXauth = true
		}
	}
	if !gotDisplay || !gotXauth {
		t.Fatalf("missing X11 env: display=%v xauth=%v in %v", gotDisplay, gotXauth, env)
	}
}

func TestBuildAgentEnv_NoX11WhenUnset(t *testing.T) {
	for _, e := range BuildAgentEnv(AgentEnvSpec{}) {
		if len(e) >= 8 && e[:8] == "DISPLAY=" {
			t.Fatalf("unexpected DISPLAY when X11 unset: %q", e)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./runner/ -run TestBuildAgentEnv_X11`
Expected: FAIL — `AgentEnvSpec has no field X11Display`.

- [ ] **Step 3: Extend AgentEnvSpec + BuildAgentEnv**

In `runner/agentenv.go`, add to the `AgentEnvSpec` struct:

```go
	// X11Display/X11AuthFile, when X11AuthFile != "", inject
	// DISPLAY=127.0.0.1:<X11Display> and XAUTHORITY=<X11AuthFile> so X clients
	// started in the session reach the forwarded X server. See runner/x11.go.
	X11Display  int
	X11AuthFile string
```

In `BuildAgentEnv`, before `return env`, add:

```go
	if s.X11AuthFile != "" {
		env = append(env,
			fmt.Sprintf("DISPLAY=127.0.0.1:%d", s.X11Display),
			"XAUTHORITY="+s.X11AuthFile,
		)
	}
```

Add `"fmt"` to the imports if not already present.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./runner/ -run TestBuildAgentEnv`
Expected: PASS.

- [ ] **Step 5: Implement `runner/x11.go`**

Create `runner/x11.go`:

```go
package runner

import (
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// writeXauthFile creates a per-task XAUTHORITY file and registers the client's
// MIT-MAGIC-COOKIE-1 for display 127.0.0.1:<display> so an X client started in
// the session authenticates to the forwarded X server. Returns the file path.
// Requires xauth on the runner PATH.
func writeXauthFile(taskIDHex string, display int, cookie []byte) (string, error) {
	if len(cookie) == 0 {
		return "", fmt.Errorf("x11: empty cookie")
	}
	path := filepath.Join(os.TempDir(), "harness-xauth-"+taskIDHex)
	// Create/truncate so a stale file from a reused task id can't leak.
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600); err != nil {
		return "", fmt.Errorf("x11: create xauth file: %w", err)
	} else {
		_ = f.Close()
	}
	displayName := fmt.Sprintf("127.0.0.1:%d", display)
	cmd := exec.Command("xauth", "-f", path, "add", displayName, "MIT-MAGIC-COOKIE-1", hex.EncodeToString(cookie))
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("x11: xauth add failed (is xauth installed?): %v: %s", err, out)
	}
	return path, nil
}

// cleanupXauthFile removes a file created by writeXauthFile. Best-effort.
func cleanupXauthFile(path string) {
	if path != "" {
		_ = os.Remove(path)
	}
}
```

- [ ] **Step 6: Wire into `handleOpenExec`**

In `runner/session.go`, inside `handleOpenExec`, immediately BEFORE the `env := BuildAgentEnv(AgentEnvSpec{...})` call (line 608), add:

```go
	var x11Display int
	var x11AuthFile string
	if oer.X11Enabled() {
		if f := oer.X11(); f != nil {
			x11Display = int(f.Display)
			p, err := writeXauthFile(taskIDHex, x11Display, f.Cookie)
			if err != nil {
				log.Warn("x11 setup failed; continuing without DISPLAY", "task_id", taskIDHex, "err", err)
			} else {
				x11AuthFile = p
				defer cleanupXauthFile(p)
			}
		}
	}
```

Then add the two fields to the `AgentEnvSpec{...}` literal (lines 608-619):

```go
		X11Display:  x11Display,
		X11AuthFile: x11AuthFile,
```

(The `defer` fires when `handleOpenExec` returns — i.e. after the PTY session ends — which is the correct cleanup point. `log` is already bound at line 505.)

- [ ] **Step 7: Build + test**

Run: `go build ./... && go test ./runner/ -run TestBuildAgentEnv`
Expected: builds; tests pass.

- [ ] **Step 8: Commit**

```bash
git add runner/x11.go runner/agentenv.go runner/agentenv_test.go runner/session.go
git commit -m "feat(runner): write per-task XAUTHORITY and inject DISPLAY for X11"
```

---

## Task 8: CLI — `--x11` / `--x11-display` flags

**Files:**
- Modify: `cmd/harness-cli/session.go:45-105`

- [ ] **Step 1: Add flags + validation + dispatch**

In `runSessionNew`, add flag declarations after the `detach` flags (line 56):

```go
	x11 := false
	fs.BoolVar(&x11, "x11", false, "forward X11: inject DISPLAY/XAUTHORITY so GUI apps in the session render on your local X server (requires xauth + a running local X server)")
	x11Display := fs.Int("x11-display", 10, "X11 display number N (runner binds 127.0.0.1:6000+N; default 10)")
```

After `fs.Parse(args)` validation (after line 59), add:

```go
	if x11 && detach {
		return fmt.Errorf("session new: --x11 is incompatible with --detach (a detached session has no client to host the X tunnel)")
	}
	if x11 && (*x11Display < 0 || *x11Display > 99) {
		return fmt.Errorf("session new: --x11-display must be 0..99")
	}
```

Replace the foreground dispatch (lines 99-103) so that `--x11` routes to the new driver:

```go
	if x11 {
		id, err := c.RunInteractiveX11(ctx, repoVal, sel, []string(extraArgs), *resume, *x11Display)
		if err != nil {
			return err
		}
		fmt.Printf("session %s ended\n", id)
		return nil
	}

	id, err := c.InteractiveWithSelectorAndArgs(ctx, repoVal, sel, []string(extraArgs), *resume, true /*detachable*/)
	if err != nil {
		return err
	}
	fmt.Printf("session %s ended\n", id)
	return nil
```

- [ ] **Step 2: Build to verify**

Run: `go build ./... && go vet ./cmd/harness-cli/`
Expected: builds clean.

- [ ] **Step 3: Smoke-check help text**

Run: `go run ./cmd/harness-cli session new -h 2>&1 | grep -E 'x11|x11-display'`
Expected: both flags listed.

- [ ] **Step 4: Commit**

```bash
git add cmd/harness-cli/session.go
git commit -m "feat(cli): session new --x11 / --x11-display flags"
```

---

## Task 9: Docs + manual end-to-end verification

**Files:**
- Modify: `README.md` (forwarding section)

- [ ] **Step 1: Document the feature**

Add to the forwarding/usage section of `README.md`:

```markdown
### X11 forwarding

`harness-cli session new --x11 --repo <path>` injects `DISPLAY`/`XAUTHORITY`
into the session so GUI programs render on your local X server (SSH `-Y`
equivalent; trusted forwarding). Requires `xauth` on both the client and the
runner, a Linux runner (or a runner with X11 client libs), and a running local
X server (Linux with `$DISPLAY`, or Windows/macOS with VcXsrv/XQuartz set as
`$DISPLAY`). Override the display number with `--x11-display N` (default 10).
Not available with `--detach` or for WebUI clients.
```

- [ ] **Step 2: Build the binaries**

Run: `make build`
Expected: builds all binaries clean.

- [ ] **Step 3: Manual E2E (requires a real X server + runner)**

This path cannot be unit-tested (needs a live X server). On a host with `$DISPLAY` set and `xauth`/`xterm` (or `xeyes`) installed, with a Linux runner registered:

1. `harness-cli session new --x11 --repo <repo>` 
2. In the session shell: `echo $DISPLAY` → expect `127.0.0.1:10`; `echo $XAUTHORITY` → expect `/tmp/harness-xauth-<taskid>`.
3. In the session shell: `xeyes` (or `xterm`).
4. Expected: the window appears on your LOCAL screen. Moving the mouse moves the eyes (input round-trips), confirming the bidirectional tunnel.
5. Exit the session; confirm `/tmp/harness-xauth-<taskid>` is removed on the runner.

Record the observed result. (Per `feedback_verify_before_memory_writes`: only after seeing this work end-to-end should any "X11 forwarding works" memory be written.)

- [ ] **Step 4: Commit**

```bash
git add README.md
git commit -m "docs: document session new --x11 forwarding"
```

---

## Self-Review

**Spec coverage:**
- Cookie extraction (client) → Task 3. ✓
- Cookie registration + DISPLAY/XAUTHORITY injection (runner) → Task 7. ✓
- Byte tunnel → reuses existing `-R` (Tasks 2 + 5). ✓
- UNIX-socket dial for `-nolisten tcp` Linux servers → Task 2. ✓
- Schema for every wire byte (`feedback_no_schema_invisible_bytes`) → Task 1, both messages, in one task (`feedback_no_split_schemas`). ✓
- Server relay → Task 6. ✓
- CLI entry + validation (`--x11`+`--detach` rejected; `session new` only) → Task 8. ✓
- Docs + E2E → Task 9. ✓

**Placeholder scan:** No "TBD"/"handle errors appropriately"/"similar to Task N" — each step shows the code. ✓

**Type consistency:** `RemoteForwardSpec.DialNetwork` (Task 2) used in Task 5. `X11Request{Display,Cookie}` (cli-side input type) defined Task 4, used Task 5 — distinct from the wire type `protocol.X11Forward{Display,Cookie}` (Task 1). `AgentEnvSpec.X11Display/X11AuthFile` defined Task 7 Step 3, used Task 7 Step 6. Wire accessors `X11Enabled()/SetX11Enabled(bool)/SetX11(X11Forward)/X11() *X11Forward` + `X11Forward.{Display,Cookie,SetCookie}` used consistently in Tasks 4/6/7 (set discriminator before the embedded block on encode; getter is nil-guarded on decode); every later task carries the "confirm against Task 1 Step 4" caveat. ✓

**Known limitation (documented, not a gap):** two runner processes on one host both using the same display `N` would collide on `127.0.0.1:(6000+N)`; surfaced as a `-R` BindFailed error. Consistent with `project_runner_ambiguous_same_host_roots`. Mitigation if needed later: per-runner display allocation requiring an OpenInteractive response field (deferred — needs a server→client round-trip the current flow lacks).
