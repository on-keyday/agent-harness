//go:build !js

package cli

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/on-keyday/agent-harness/runner/hostcmd"
	agentexec "github.com/on-keyday/objtrsf/exec"
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

// x11DisplayNumber extracts the display number N from a DISPLAY value,
// stripping an optional "unix" prefix and a trailing ".screen".
func x11DisplayNumber(display string) (int, error) {
	if display == "" {
		return 0, fmt.Errorf("x11: DISPLAY is empty")
	}
	d := strings.TrimPrefix(display, "unix")
	colon := strings.LastIndex(d, ":")
	if colon < 0 {
		return 0, fmt.Errorf("x11: malformed DISPLAY %q", display)
	}
	numPart := d[colon+1:]
	if dot := strings.IndexByte(numPart, '.'); dot >= 0 {
		numPart = numPart[:dot]
	}
	n, err := strconv.Atoi(numPart)
	if err != nil {
		return 0, fmt.Errorf("x11: bad display number in %q", display)
	}
	return n, nil
}

// localXServerDialSpec parses a client-side DISPLAY value into a dial target
// for the REAL local X server. "[unix]:N" → unix socket /tmp/.X11-unix/XN;
// "host:N" → TCP host:(6000+N).
func localXServerDialSpec(display string) (network, host string, port int, err error) {
	n, err := x11DisplayNumber(display)
	if err != nil {
		return "", "", 0, err
	}
	d := strings.TrimPrefix(display, "unix")
	colon := strings.LastIndex(d, ":")
	hostPart := d[:colon]
	if hostPart == "" {
		return "unix", fmt.Sprintf("/tmp/.X11-unix/X%d", n), 0, nil
	}
	return "tcp", hostPart, 6000 + n, nil
}

// CheckX11Display reports why display could not name a local X server an X11
// forward can dial on this OS, or nil. It reads the form only; see
// ProbeLocalX11Display for whether anything answers there.
func CheckX11Display(display string) error {
	_, err := localX11DisplayFor(display, runtime.GOOS)
	return err
}

// x11ProbeMax is the last display number ProbeLocalX11Display tries when it
// has to find the X server (Windows, DISPLAY unset): TCP 6000 to 6000+x11ProbeMax.
const x11ProbeMax = 9

// ProbeLocalX11Display finds the X server an X11 forward would dial, before a
// caller does something costly for one (the TUI restarts a live session with
// it). With $DISPLAY set it dials what the value names and fails when nothing
// answers. With $DISPLAY empty on Windows, where no X server sets it, it dials
// localhost:6000 to 6000+x11ProbeMax and returns the first that answers with
// found=true; the caller must say which one it is using. Empty elsewhere is
// an error: there it means there is no X. dial is net.Dial with a short
// timeout in production; a connect is the whole test.
func ProbeLocalX11Display(dial func(network, address string) error) (display string, found bool, err error) {
	return probeX11DisplayFor(os.Getenv("DISPLAY"), runtime.GOOS, dial)
}

func probeX11DisplayFor(display, goos string, dial func(network, address string) error) (string, bool, error) {
	if display == "" && goos == "windows" {
		for n := 0; n <= x11ProbeMax; n++ {
			if dial("tcp", net.JoinHostPort("localhost", strconv.Itoa(6000+n))) == nil {
				return fmt.Sprintf("localhost:%d", n), true, nil
			}
		}
		return "", false, fmt.Errorf("x11: DISPLAY is empty and no X server answered on localhost:6000-%d; start one (XLaunch) or set it with `display localhost:N`", 6000+x11ProbeMax)
	}
	if _, err := localX11DisplayFor(display, goos); err != nil {
		return "", false, err
	}
	network, host, port, _ := localXServerDialSpec(display)
	addr := host
	if network == "tcp" {
		addr = net.JoinHostPort(host, strconv.Itoa(port))
	}
	if err := dial(network, addr); err != nil {
		return "", false, fmt.Errorf("x11: nothing answered at DISPLAY=%s (%s %s): is the X server running? %w", display, network, addr, err)
	}
	return display, false, nil
}

// localX11DisplayFor checks display's form for goos. A
// host-less DISPLAY means the fixed unix socket path /tmp/.X11-unix/XN
// (localXServerDialSpec). Windows has AF_UNIX, but on Windows that path is
// \tmp\.X11-unix\XN on the current drive, where an X server there would not
// be listening; the Windows X servers this was written against (VcXsrv,
// Xming) are reached over TCP 6000+N. So on Windows the value must name a
// host. Any X server reachable that way works; none is special-cased.
func localX11DisplayFor(display, goos string) (string, error) {
	network, _, _, err := localXServerDialSpec(display)
	if err != nil {
		return "", err
	}
	if network == "unix" && goos == "windows" {
		n, _ := x11DisplayNumber(display)
		return "", fmt.Errorf("x11: DISPLAY=%s means the unix socket path /tmp/.X11-unix/X%d, which an X server on Windows is not reached at; use the TCP form, DISPLAY=localhost:%d", display, n, n)
	}
	return display, nil
}

// localX11Cookie shells out to `xauth list <display>` and returns the cookie
// bytes for the display number encoded in DISPLAY. Requires xauth on PATH.
func localX11Cookie(display string) ([]byte, error) {
	n, err := x11DisplayNumber(display)
	if err != nil {
		return nil, err
	}
	out, err := hostcmd.Command("xauth", "list", display).Output()
	if err != nil {
		return nil, fmt.Errorf("x11: `xauth list %s` failed (is xauth installed and the X server authorized?): %w", display, err)
	}
	return parseXauthCookie(string(out), n)
}

// X11Request carries the client-chosen display number and the local X
// server's cookie into OpenInteractiveRequest. This is the cli-side input
// type; the wire type is protocol.X11Forward.
type X11Request struct {
	Display int    // N; app sees DISPLAY=127.0.0.1:N on the runner
	Cookie  []byte // MIT-MAGIC-COOKIE-1 value of the client's local X server
}

// OpenInteractiveX11 opens an X11-forwarded interactive session and returns the
// PTY stream, task id, the -R RemoteForwardSpec the caller must run (via
// RunRemoteForward) for the session's lifetime, and a non-empty warn string
// when forwarding WITHOUT authentication (no cookie). It does NOT print: the
// caller surfaces warn (CLI → stderr; TUI → status line, since stderr would
// corrupt the alt-screen).
// resumeCapsOverride, when true, instructs the server to apply caps as an
// override on resume (re-grant) rather than inheriting the original task's
// capability mask. Has no effect on new tasks (non-resume).
// agentProfile, when non-empty, selects a named agent profile (e.g. "codex")
// for the spawned task instead of the runner's default. "" means default.
func (c *Client) OpenInteractiveX11(ctx context.Context, repo string, opts SessionOpts, displayN int) (*agentexec.CommandExecutionStream, string, RemoteForwardSpec, string, error) {
	display := os.Getenv("DISPLAY")
	network, host, port, err := localXServerDialSpec(display)
	if err != nil {
		return nil, "", RemoteForwardSpec{}, "", err
	}
	var warn string
	cookie, err := localX11Cookie(display)
	if err != nil {
		warn = fmt.Sprintf("no cookie for %s (%v); forwarding WITHOUT authentication — your X server must accept unauthenticated connections", display, err)
		cookie = nil
	}
	stream, taskIDHex, err := c.openInteractive(ctx, repo, opts, &X11Request{Display: displayN, Cookie: cookie})
	if err != nil {
		return nil, taskIDHex, RemoteForwardSpec{}, warn, err
	}
	sp := RemoteForwardSpec{BindAddr: "127.0.0.1", RunnerPort: 6000 + displayN, DialNetwork: network, DialHost: host, DialPort: port}
	return stream, taskIDHex, sp, warn, nil
}

// rawSafeStderr returns a line writer that terminates with CR+LF instead of a
// bare LF. Anything printed from the X11 forward goroutine can land while the
// foreground RemoteShell holds the tty in raw mode, where ONLCR is off and a
// lone LF moves down a row without returning to column 0 — successive lines
// then staircase across the screen. CR+LF is correct in cooked mode too (the
// extra CR is idempotent), so the same writer serves both.
func rawSafeStderr(prefix string) func(string) {
	return func(s string) { fmt.Fprint(os.Stderr, prefix+s+"\r\n") }
}

// RunInteractiveX11 opens an interactive session with X11 forwarding enabled,
// runs an -R remote forward (runner 127.0.0.1:6000+N -> client's local X
// server) in the background for its lifetime, and drives the PTY in the
// foreground. displayN is the client-chosen display number. Requires xauth on
// the client and a running, authorized local X server (via $DISPLAY).
// resumeCapsOverride is forwarded to OpenInteractiveX11 unchanged.
// agentProfile is forwarded to OpenInteractiveX11 unchanged.
func (c *Client) RunInteractiveX11(ctx context.Context, repo string, opts SessionOpts, displayN int) (string, error) {
	stream, taskIDHex, sp, warn, err := c.OpenInteractiveX11(ctx, repo, opts, displayN)
	if err != nil {
		return taskIDHex, err
	}
	defer stream.Close()
	if warn != "" {
		fmt.Fprintln(os.Stderr, "x11: "+warn)
	}
	fctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		if err := RunRemoteForward(fctx, c, taskIDHex, []RemoteForwardSpec{sp}, rawSafeStderr("x11: ")); err != nil {
			rawSafeStderr("x11 forward: ")(err.Error())
		}
		// The forward can stop BEFORE the session: `forward kill` on this
		// registration makes RunRemoteForward return mid-session. So this runs
		// while RemoteShell still holds the tty in raw mode — hence CR+LF.
		rawSafeStderr("x11: ")("forward stopped")
	}()
	fmt.Fprintf(os.Stderr, "harness-cli: X11 session %s (remote DISPLAY=127.0.0.1:%d -> local %s; Ctrl+] detach, Ctrl+D/exit ends)\n", taskIDHex, displayN, os.Getenv("DISPLAY"))
	// RemoteShell returns with the local terminal already back in cooked mode
	// and its screen and input modes reset — it does that on the way out
	// whether it is returning cleanly or not (objtrsf exec.WriteTerminalReset).
	return taskIDHex, finishAttach(stream, stream.RemoteShell(), os.Stderr)
}
