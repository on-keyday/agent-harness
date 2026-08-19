package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentCwdEnv(t *testing.T) {
	if got := AgentCwdEnv(""); got != nil {
		t.Errorf("AgentCwdEnv(\"\") = %v, want nil — a caller that sets no working directory must not claim one", got)
	}
	got := AgentCwdEnv("/tmp/work tree")
	want := []string{"PWD=/tmp/work tree"}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("AgentCwdEnv = %v, want %v", got, want)
	}
}

// TestPWDReporterHelper is the spawned child of TestProcess_RunSetsPWDToCWD.
// It prints the PWD it INHERITED and exits before the normal test machinery
// runs. It is deliberately not a shell script: bash recomputes PWD from
// getcwd() at startup, so a bash stand-in reports the right answer whether or
// not the runner set PWD at all — i.e. it would pass with the fix reverted.
// The same substitution invalidated an earlier sandbox PID-1 experiment; see
// scripts/sandbox/README.md.
func TestPWDReporterHelper(t *testing.T) {
	if os.Getenv("HARNESS_PWD_HELPER") != "1" {
		t.Skip("helper process; driven by TestProcess_RunSetsPWDToCWD")
	}
	fmt.Printf("INHERITED_PWD=%s\n", os.Getenv("PWD"))
	os.Exit(0)
}

// TestProcess_RunSetsPWDToCWD pins the invariant that a spawned agent's PWD
// names the directory it was actually started in. Setting cmd.Dir performs a
// chdir but leaves the inherited PWD untouched, and an agent that trusts PWD
// over getcwd() then reports the RUNNER's directory for every task it ever
// runs — which for opencode is the key `run --continue` resolves against, so
// one task would resume another's conversation. See AgentCwdEnv.
func TestProcess_RunSetsPWDToCWD(t *testing.T) {
	taskDir := t.TempDir()

	// A PWD that is real but wrong, standing in for the runner's own
	// inherited value. The child must report taskDir, not this.
	stalePWD := t.TempDir()
	t.Setenv("PWD", stalePWD)

	p := &Process{
		ClaudeBin: os.Args[0],
		CWD:       taskDir,
		Env:       []string{"HARNESS_PWD_HELPER=1"},
		// The prompt and args land after "--" so the child's flag parser
		// ignores them; without that it rejects the template's own tokens
		// before any test body runs.
		OneshotArgvTemplate: []string{
			"-test.run=^TestPWDReporterHelper$", "--", "{args}", "{prompt}",
		},
	}

	var out []byte
	code, err := p.Run(context.Background(), "ignored", func(data []byte) { out = append(out, data...) })
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit = %d, want 0; output = %q", code, out)
	}

	// t.TempDir can hand back a symlinked path (/var -> /private/var on
	// macOS); compare against what the child could actually observe.
	wantResolved, err := filepath.EvalSymlinks(taskDir)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(strings.TrimPrefix(firstLineWithPrefix(string(out), "INHERITED_PWD="), "INHERITED_PWD="))
	if got != taskDir && got != wantResolved {
		t.Errorf("child inherited PWD = %q, want the task dir %q (stale runner PWD was %q); output = %q",
			got, taskDir, stalePWD, out)
	}
}

// firstLineWithPrefix returns the first line of s carrying prefix, with the
// log sink's own "[out]"/"[err]" stream prefix already stripped.
func firstLineWithPrefix(s, prefix string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimPrefix(strings.TrimPrefix(line, "[out]"), "[err]")
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}
