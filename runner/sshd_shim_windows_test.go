//go:build windows

package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestStageSSHDShim exercises the real staging path on Windows: it stages the
// shim, then confirms the staged image is an absolute-pathed sshd.exe that
// behaves as cmd.exe (exit code, stdout/stderr separation) and that concurrent
// staging is safe. This is the only guard against a staging regression on
// Windows, which neither half of this work builds by default.
//
// It SKIPS rather than fails when System32\cmd.exe cannot be resolved or read:
// that is an environment gap (a locked-down or unusual host), not a defect in
// stageSSHDShim, and a skip keeps the signal — a real failure — unambiguous.
func TestStageSSHDShim(t *testing.T) {
	src, err := systemCmdExe()
	if err != nil {
		t.Skipf("cannot resolve System32 cmd.exe: %v", err)
	}
	if f, err := os.Open(src); err != nil {
		t.Skipf("System32 cmd.exe unreadable (%s): %v", src, err)
	} else {
		f.Close()
	}

	p, err := stageSSHDShim()
	if err != nil {
		t.Fatalf("stageSSHDShim: %v", err)
	}
	t.Logf("staged: %s", p)

	if !filepath.IsAbs(p) {
		t.Fatalf("path not absolute: %s", p)
	}
	if got := strings.ToLower(filepath.Base(p)); got != "sshd.exe" {
		t.Fatalf("basename = %q, want sshd.exe", got)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("staged file missing: %v", err)
	}

	// Exit code passthrough: `sshd.exe /c "exit 7"` must surface exit 7, proving
	// the shim is cmd.exe under a different name, not a wrapper that eats codes.
	err = exec.Command(p, "/c", "exit 7").Run()
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 7 {
		t.Fatalf("exit code passthrough: got %v, want exit status 7", err)
	}

	// stdout and stderr stay separated, as cmd.exe would.
	cmd := exec.Command(p, "/c", "echo OUTLINE& echo ERRLINE 1>&2")
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.TrimSpace(out.String()) != "OUTLINE" {
		t.Fatalf("stdout = %q, want OUTLINE", out.String())
	}
	if strings.TrimSpace(errb.String()) != "ERRLINE" {
		t.Fatalf("stderr = %q, want ERRLINE", errb.String())
	}

	// Concurrent staging: every caller sees the same path and no error. Guards
	// the in-process mutex + re-check and the atomic replace against races.
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			got, err := stageSSHDShim()
			if err != nil {
				t.Errorf("concurrent stageSSHDShim: %v", err)
				return
			}
			if got != p {
				t.Errorf("concurrent stageSSHDShim: path = %q, want %q", got, p)
			}
		})
	}
	wg.Wait()
}
