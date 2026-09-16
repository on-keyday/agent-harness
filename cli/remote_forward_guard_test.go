package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ServeRemoteForwardControl is only HALF of serving a -R.
//
// It reads the control stream, which is everything a tcp remote forward needs
// and not everything a udp one does: a udp -R also needs its dialer registry
// live, because nothing announces a flow in advance. A caller that reaches for
// the control loop directly therefore registers, binds a listener on the
// runner, and carries nothing — and it fails silently, with a forward that
// lists fine and moves no bytes.
//
// That is not hypothetical. The TUI's DoStartRemoteForward called the control
// loop directly, which was correct while tcp was the only protocol and became
// a hole the moment udp existed. ServeRemoteForward is the whole obligation in
// one function; this test is what keeps the next caller from stepping around
// it, since the compiler cannot tell the two apart.
func TestServeRemoteForwardControlHasNoOutsideCallers(t *testing.T) {
	root := ".."
	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // unreadable entries are not this test's business
		}
		if info.IsDir() {
			base := info.Name()
			if base == ".git" || base == "node_modules" || base == ".harness-worktrees" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// The definition and its own two branches live here, by construction.
		if filepath.Base(path) == "port_forward.go" && strings.Contains(path, "cli") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		if strings.Contains(string(b), "ServeRemoteForwardControl(") {
			offenders = append(offenders, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(offenders) > 0 {
		t.Errorf("these call ServeRemoteForwardControl directly, which serves only the tcp half "+
			"of a -R — a udp one would bind on the runner and carry nothing. Call "+
			"(*Client).ServeRemoteForward instead:\n  %s", strings.Join(offenders, "\n  "))
	}
}
