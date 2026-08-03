package hostcmd_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The harness shells out to git and xauth from a dozen places. On Windows each
// bare exec.Command pops a console window on the operator's desktop for the
// life of the process, which with git_query is constant blinking.
//
// hostcmd.Command sets CREATE_NO_WINDOW, but only for the sites that use it —
// and a site that does not is invisible to everyone not watching a Windows
// desktop. This walks the source instead of trusting a grep at review time,
// because "one call site was missed" is a failure this project has shipped
// more than once.
//
// A deliberate exception is spelled `//nolint:hostcmd` on the same line, with a
// reason: runner/process.go runs the agent in a PTY the operator attaches to,
// and cli/file_edit.go launches the user's own $EDITOR.
var bareSpawn = regexp.MustCompile(`exec\.Command(Context)?\((\s*\w+,)?\s*"(git|xauth)"`)

func TestNoBareExecForHostHelpers(t *testing.T) {
	root := ".."
	for _, pkg := range []string{".", "../../cli", "../../cmd", "../../server"} {
		dir := filepath.Join(root, pkg)
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			for i, line := range strings.Split(string(b), "\n") {
				if !bareSpawn.MatchString(line) {
					continue
				}
				if strings.Contains(line, "//nolint:hostcmd") {
					continue
				}
				t.Errorf("%s:%d starts a host helper with os/exec: %s\n"+
					"  use hostcmd.Command / hostcmd.CommandContext, or mark the line //nolint:hostcmd with a reason",
					path, i+1, strings.TrimSpace(line))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
}
