package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// runCLIScrubbed runs the binary with EVERY HARNESS_* variable removed.
//
// Both halves matter and both were learned the hard way. HARNESS_AUTH_TICKET
// suppresses config loading entirely (an in-task agent must never pick up an
// operator's workspace) and these tests run inside a task. HARNESS_SERVER_CID
// sits ABOVE the workspace in the resolution order, so leaving it set means a
// test asserting "the workspace supplied the server-cid" instead watches the
// inherited live server being dialled — which is correct behaviour and a
// useless test.
func runCLIScrubbed(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"run", "."}, args...)...)
	cmd.Env = []string{}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "HARNESS_") {
			continue
		}
		cmd.Env = append(cmd.Env, kv)
	}
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// `workspace` with no subcommand prints usage rather than dialling.
func TestCLI_WorkspaceUsage(t *testing.T) {
	s := runCLIScrubbed(t, "--server-cid=ws:127.0.0.1:19998-1", "workspace")
	if !strings.Contains(s, "workspace save") {
		t.Errorf("usage not printed: %s", s)
	}
	if strings.Contains(s, "127.0.0.1:19998") {
		t.Errorf("dialled before printing usage: %s", s)
	}
}

// A bad task id is rejected before any connection is attempted.
func TestCLI_WorkspaceSaveRejectsBadTaskID(t *testing.T) {
	s := runCLIScrubbed(t, "--server-cid=ws:127.0.0.1:19997-1",
		"workspace", "save", "default", "--task", "nope")
	if !strings.Contains(s, "32-hex task id") {
		t.Errorf("error does not name the task id problem: %s", s)
	}
}

// An unknown --workspace name is an error, not a silent fall-through to the
// built-in server-cid default.
func TestCLI_UnknownWorkspaceNameFails(t *testing.T) {
	cfg := t.TempDir() + "/config"
	if err := os.WriteFile(cfg, []byte("[workspace default]\nrepo = /x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := runCLIScrubbed(t, "--config", cfg, "--workspace", "nosuch", "ls")
	if !strings.Contains(s, "no workspace named") {
		t.Errorf("unknown workspace was not reported: %s", s)
	}
}

// The workspace supplies server-cid when neither the flag nor the env does:
// the dial must reach the address the FILE names.
func TestCLI_WorkspaceSuppliesServerCID(t *testing.T) {
	cfg := t.TempDir() + "/config"
	if err := os.WriteFile(cfg, []byte(
		"[workspace default]\nserver-cid = ws:127.0.0.1:19996-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := runCLIScrubbed(t, "--config", cfg, "--workspace", "default", "ls")
	if !strings.Contains(s, "127.0.0.1:19996") {
		t.Errorf("did not dial the workspace's server-cid: %s", s)
	}
}

// The flag still beats the workspace.
func TestCLI_ServerCIDFlagBeatsWorkspace(t *testing.T) {
	cfg := t.TempDir() + "/config"
	if err := os.WriteFile(cfg, []byte(
		"[workspace default]\nserver-cid = ws:127.0.0.1:19996-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := runCLIScrubbed(t, "--config", cfg, "--workspace", "default",
		"--server-cid=ws:127.0.0.1:19995-1", "ls")
	if !strings.Contains(s, "127.0.0.1:19995") {
		t.Errorf("the flag did not beat the workspace: %s", s)
	}
}

// Inside a task (HARNESS_AUTH_TICKET set) the config is not read at all, so an
// operator's workspace cannot leak into an agent through a prefix-forwarded
// HARNESS_CONFIG.
func TestCLI_ConfigIgnoredInsideATask(t *testing.T) {
	cfg := t.TempDir() + "/config"
	if err := os.WriteFile(cfg, []byte("[profile broken]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "run", ".", "--config", cfg, "workspace", "ls")
	cmd.Env = append(os.Environ(), "HARNESS_AUTH_TICKET=00112233445566778899aabbccddeeff")
	out, _ := cmd.CombinedOutput()
	if strings.Contains(string(out), "unknown section header") {
		t.Errorf("the config was parsed inside a task: %s", out)
	}
}
