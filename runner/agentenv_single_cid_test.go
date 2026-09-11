package runner

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
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
// The guard is the ordering in Process.Run's `append(os.Environ(), extraEnv...)`
// plus os/exec's dedup-keeps-last. Nothing else asserts it, and a future spawn
// path that skips BuildAgentEnv would inherit the list silently.
func TestSpawnedAgentGetsOneServerCIDNotTheCandidateList(t *testing.T) {
	t.Setenv("HARNESS_SERVER_CID", "ws:10.0.0.1:8539-1,ws:10.0.0.2:8539-2")

	repo := initRepo(t)
	wm := &WorktreeManager{Repo: repo}
	dir, err := wm.Create("single-cid")
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	script := writeFakeClaude(t, `echo "CID=[$HARNESS_SERVER_CID]"`)

	live := mustParseCID(t, "ws:127.0.0.1:8539-31062")
	p := &Process{
		ClaudeBin: script,
		CWD:       dir,
		Timeout:   10 * time.Second,
		Env:       BuildAgentEnv(AgentEnvSpec{ServerCID: live}),
	}

	var mu sync.Mutex
	var out strings.Builder
	exit, err := p.Run(context.Background(), "hi", func(data []byte) {
		mu.Lock()
		out.Write(data)
		mu.Unlock()
	})
	if err != nil || exit != 0 {
		t.Fatalf("run: exit=%d err=%v", exit, err)
	}

	mu.Lock()
	got := out.String()
	mu.Unlock()

	want := "CID=[" + live.String() + "]"
	if !strings.Contains(got, want) {
		t.Fatalf("agent saw the wrong HARNESS_SERVER_CID.\nwant substring: %s\ngot: %s", want, got)
	}
	if strings.Contains(got, ",") {
		t.Fatalf("agent received a candidate LIST; it must receive one address: %s", got)
	}
}
