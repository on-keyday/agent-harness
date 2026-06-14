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
	colon := strings.LastIndex(d, ":")
	if colon < 0 {
		return "", "", 0, fmt.Errorf("x11: malformed DISPLAY %q", display)
	}
	hostPart := d[:colon]
	numPart := d[colon+1:]
	if dot := strings.IndexByte(numPart, '.'); dot >= 0 {
		numPart = numPart[:dot]
	}
	nval, convErr := strconv.Atoi(numPart)
	if convErr != nil {
		return "", "", 0, fmt.Errorf("x11: bad display number in %q", display)
	}
	if hostPart == "" {
		return "unix", fmt.Sprintf("/tmp/.X11-unix/X%d", nval), 0, nil
	}
	return "tcp", hostPart, 6000 + nval, nil
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
