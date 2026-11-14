package runner

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TestSpawnedAgentGetsOneServerCIDNotTheCandidateList pins the property that
// keeps --server-cid's candidate list off every agent.
//
// The runner's OWN environment can hold the list: scripts/runner-autostart.py
// reads $HARNESS_SERVER_CID when the flag is absent, and an operator who
// exports the list there passes it to the runner process. A spawned agent must
// still see exactly one address — the one this runner actually connected on —
// for two independent reasons: harness-cli's --server-cid takes a single CID,
// and the podman wrapper splits HARNESS_SERVER_CID into ip/proto/port to build
// its harness-server firewall carve-out (scripts/sandbox/agent-in-podman.sh).
// A list reaching either one fails, and the sandbox fails CLOSED — the bridged
// harness-cli simply stops reaching the server.
//
// The guard is the ordering in agentexec's `append(os.Environ(), extraEnv...)`
// (objtrsf/exec), with handleOpenExec's BuildAgentEnv as the extra, plus
// os/exec's dedup-keeps-last. Nothing else asserts it, and a future spawn
// path that skips BuildAgentEnv would inherit the list silently.
func TestSpawnedAgentGetsOneServerCIDNotTheCandidateList(t *testing.T) {
	t.Setenv("HARNESS_SERVER_CID", "ws:10.0.0.1:8539-1,ws:10.0.0.2:8539-2")

	repo := t.TempDir()
	out := filepath.Join(t.TempDir(), "out")
	script := writeFakeClaude(t, `echo "CID=[$HARNESS_SERVER_CID]" > '`+out+`'`)

	live := mustParseCID(t, "ws:127.0.0.1:8539-31062")
	ms := &mockSender{}
	s := &Session{
		AllowedRoots: []string{repo},
		Profiles:     singleProfile(t, script),
		Sender:       ms,
		Now:          time.Now,
		NoWorktree:   true,
		ServerCID:    live,
	}
	rig := newOpenRig(s)
	s.handleOpenExec(context.Background(), rig.oneshot(t, protocol.TaskID{Id: [16]byte{0x5C}}, repo, "hi"))

	if tf := lastFinished(t, ms); tf.ExitCode != 0 {
		t.Fatalf("run: exit=%d %q", tf.ExitCode, tf.ErrorMessage)
	}
	got := readOut(t, out)
	want := "CID=[" + live.String() + "]"
	if !strings.Contains(got, want) {
		t.Fatalf("agent saw the wrong HARNESS_SERVER_CID.\nwant substring: %s\ngot: %s", want, got)
	}
	if strings.Contains(got, ",") {
		t.Fatalf("agent received a candidate LIST; it must receive one address: %s", got)
	}
}
