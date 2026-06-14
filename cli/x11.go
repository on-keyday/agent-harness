//go:build !js

package cli

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	agentexec "github.com/on-keyday/agent-harness/exec"
	"github.com/on-keyday/agent-harness/runner/protocol"
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

// localX11Cookie shells out to `xauth list <display>` and returns the cookie
// bytes for the display number encoded in DISPLAY. Requires xauth on PATH.
func localX11Cookie(display string) ([]byte, error) {
	n, err := x11DisplayNumber(display)
	if err != nil {
		return nil, err
	}
	out, err := exec.Command("xauth", "list", display).Output()
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
func (c *Client) OpenInteractiveX11(ctx context.Context, repo string, sel protocol.RunnerSelector, extraArgs []string, resumeTaskID string, displayN int) (*agentexec.CommandExecutionStream, string, RemoteForwardSpec, string, error) {
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
	stream, taskIDHex, err := c.openInteractive(ctx, repo, sel, extraArgs, resumeTaskID, true /*detachable*/, &X11Request{Display: displayN, Cookie: cookie})
	if err != nil {
		return nil, taskIDHex, RemoteForwardSpec{}, warn, err
	}
	sp := RemoteForwardSpec{BindAddr: "127.0.0.1", RunnerPort: 6000 + displayN, DialNetwork: network, DialHost: host, DialPort: port}
	return stream, taskIDHex, sp, warn, nil
}

// RunInteractiveX11 opens an interactive session with X11 forwarding enabled,
// runs an -R remote forward (runner 127.0.0.1:6000+N -> client's local X
// server) in the background for its lifetime, and drives the PTY in the
// foreground. displayN is the client-chosen display number. Requires xauth on
// the client and a running, authorized local X server (via $DISPLAY).
func (c *Client) RunInteractiveX11(ctx context.Context, repo string, sel protocol.RunnerSelector, extraArgs []string, resumeTaskID string, displayN int) (string, error) {
	stream, taskIDHex, sp, warn, err := c.OpenInteractiveX11(ctx, repo, sel, extraArgs, resumeTaskID, displayN)
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
		logf := func(s string) { fmt.Fprintln(os.Stderr, "x11: "+s) }
		if err := RunRemoteForward(fctx, c, taskIDHex, []RemoteForwardSpec{sp}, logf); err != nil {
			fmt.Fprintln(os.Stderr, "x11 forward: "+err.Error())
		}
	}()
	fmt.Fprintf(os.Stderr, "harness-cli: X11 session %s (remote DISPLAY=127.0.0.1:%d -> local %s; Ctrl+] detach, Ctrl+D/exit ends)\n", taskIDHex, displayN, os.Getenv("DISPLAY"))
	if err := stream.RemoteShell(); err != nil {
		return taskIDHex, err
	}
	return taskIDHex, nil
}
