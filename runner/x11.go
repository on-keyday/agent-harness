package runner

import (
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// writeXauthFile creates a per-task XAUTHORITY file and registers the client's
// MIT-MAGIC-COOKIE-1 for display 127.0.0.1:<display> so an X client started in
// the session authenticates to the forwarded X server. Returns the file path.
// Requires xauth on the runner PATH.
func writeXauthFile(taskIDHex string, display int, cookie []byte) (string, error) {
	if len(cookie) == 0 {
		return "", fmt.Errorf("x11: empty cookie")
	}
	// os.CreateTemp uses O_EXCL + a random suffix and mode 0600, so a
	// pre-existing or attacker-planted path cannot be clobbered or
	// symlink-followed.
	f, err := os.CreateTemp("", "harness-xauth-"+taskIDHex+"-*")
	if err != nil {
		return "", fmt.Errorf("x11: create xauth file: %w", err)
	}
	path := f.Name()
	_ = f.Close()
	displayName := fmt.Sprintf("127.0.0.1:%d", display)
	// Feed the cookie via stdin (xauth batch mode) so the secret never appears
	// in argv / /proc/<pid>/cmdline.
	cmd := exec.Command("xauth", "-f", path)
	cmd.Stdin = strings.NewReader(fmt.Sprintf("add %s MIT-MAGIC-COOKIE-1 %s\n", displayName, hex.EncodeToString(cookie)))
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
