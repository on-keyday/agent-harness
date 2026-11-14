package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
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

// TestPWDReporterHelper is the spawned child of TestOpenExecSetsPWDToCWD.
// It writes the PWD it INHERITED to $HARNESS_PWD_OUT and exits before the
// normal test machinery runs. It is deliberately not a shell script: bash
// recomputes PWD from getcwd() at startup, so a bash stand-in reports the
// right answer whether or not the runner set PWD at all — i.e. it would pass
// with the fix reverted. The same substitution invalidated an earlier sandbox
// PID-1 experiment; see scripts/sandbox/README.md.
func TestPWDReporterHelper(t *testing.T) {
	if os.Getenv("HARNESS_PWD_HELPER") != "1" {
		t.Skip("helper process; driven by TestOpenExecSetsPWDToCWD")
	}
	if err := os.WriteFile(os.Getenv("HARNESS_PWD_OUT"), []byte(os.Getenv("PWD")), 0o644); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

// TestOpenExecSetsPWDToCWD pins the invariant that a spawned agent's PWD
// names the directory it was actually started in. Setting the working
// directory performs a chdir but leaves the inherited PWD untouched, and an
// agent that trusts PWD over getcwd() then reports the RUNNER's directory for
// every task it ever runs — which for opencode is the key `run --continue`
// resolves against, so one task would resume another's conversation. See
// AgentCwdEnv.
func TestOpenExecSetsPWDToCWD(t *testing.T) {
	taskDir := t.TempDir()

	// A PWD that is real but wrong, standing in for the runner's own
	// inherited value. The child must report taskDir, not this.
	stalePWD := t.TempDir()
	t.Setenv("PWD", stalePWD)
	// The agent inherits the runner's environment, which is how these two
	// reach the child.
	out := filepath.Join(t.TempDir(), "pwd")
	t.Setenv("HARNESS_PWD_HELPER", "1")
	t.Setenv("HARNESS_PWD_OUT", out)

	ps, err := NewProfileSet(AgentProfile{
		Name: "default",
		Bin:  os.Args[0],
		// The prompt and args land after "--" so the child's flag parser
		// ignores them; without that it rejects the template's own tokens
		// before any test body runs.
		OneshotArgv: []string{"-test.run=^TestPWDReporterHelper$", "--", "{args}", "{prompt}"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ms := &mockSender{}
	s := &Session{AllowedRoots: []string{taskDir}, Profiles: ps, Sender: ms, Now: time.Now, NoWorktree: true}
	rig := newOpenRig(s)
	s.handleOpenExec(context.Background(), rig.oneshot(t, protocol.TaskID{Id: [16]byte{0x9D}}, taskDir, "ignored"))

	if tf := lastFinished(t, ms); tf.ExitCode != 0 {
		t.Fatalf("exit = %d (%q), want 0", tf.ExitCode, tf.ErrorMessage)
	}

	// t.TempDir can hand back a symlinked path (/var -> /private/var on
	// macOS); compare against what the child could actually observe.
	wantResolved, err := filepath.EvalSymlinks(taskDir)
	if err != nil {
		t.Fatal(err)
	}
	got := readOut(t, out)
	if got != taskDir && got != wantResolved {
		t.Errorf("child inherited PWD = %q, want the task dir %q (stale runner PWD was %q)",
			got, taskDir, stalePWD)
	}
}
