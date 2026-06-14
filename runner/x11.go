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
