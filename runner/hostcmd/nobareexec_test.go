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
// reason: cli/file_edit.go launches the user's own $EDITOR.
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

// anySpawn is the second half, and it exists because the first half only ever
// looked for git and xauth.
//
// Every process the RUNNER starts on a Windows host pops a console window
// unless the flag is set, whatever the binary is called. Two agent spawns sat
// outside the git/xauth pattern for that whole time -- the oneshot path and the
// stream agent -- and each put a window on the operator's desktop for the life
// of the task. Neither was findable by a check that only knew two program
// names.
//
// Scoped to runner/ on purpose: cli/ legitimately starts the user's $EDITOR,
// and cmd/ is where the operator's own terminal already is.
var anySpawn = regexp.MustCompile(`exec\.Command(Context)?\(`)

func TestRunnerStartsNoProcessWithBareExec(t *testing.T) {
	err := filepath.Walk("..", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") || strings.Contains(path, "hostcmd") {
			return nil // hostcmd itself is where the wrapping happens
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		for i, line := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || !anySpawn.MatchString(line) {
				continue // a comment describing a spawn is not a spawn
			}
			if strings.Contains(line, "//nolint:hostcmd") {
				continue
			}
			t.Errorf("%s:%d starts a process with os/exec: %s\n"+
				"  on a Windows runner this puts a console window on the operator's desktop "+
				"for the life of the process. Use hostcmd.CommandContext, or mark the line "+
				"//nolint:hostcmd with the reason it is meant to be seen.",
				path, i+1, trimmed)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}
