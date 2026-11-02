package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/cli/agent"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// A task holding no capabilities lists and reads the archives of a topic it
// may read; deleting one needs the purge bit; another task's tree topic is
// refused like an unknown one.
func TestAgentCLI_E2E_Archives(t *testing.T) {
	addr := freePortE2E(t)
	board, _ := startServerE2E(t, addr)
	store, err := agentboard.OpenFileStore(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := board.AttachStore(store); err != nil {
		t.Fatal(err)
	}

	rid := mkRidE2E([4]byte{1, 2, 3, 4}, 9710, 71)
	tid := mkTidE2E(0x71)
	var ticket [16]byte
	ticket[0] = 0x71
	e2eRegisterAs(t, board, rid, tid, ticket, protocol.TaskID{}, protocol.Capability_None)

	for _, body := range []string{"one", "two", "three"} {
		if _, _, err := board.Send("grp", []byte(body), protocol.RunnerID{}, protocol.TaskID{}, "h", "", 0); err != nil {
			t.Fatal(err)
		}
	}
	info, err := board.Archive("grp", protocol.TaskID{})
	if err != nil {
		t.Fatal(err)
	}
	tree := "tree.00112233445566778899aabbccddeeff.d"
	if _, _, err := board.Send(tree, []byte("secret"), protocol.RunnerID{}, protocol.TaskID{}, "h", "", 0); err != nil {
		t.Fatal(err)
	}
	closed, err := board.Archive(tree, protocol.TaskID{})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	restore := setAgentEnv(addr, rid, tid, ticket)
	defer restore()

	var out bytes.Buffer
	if err := agent.Archives(ctx, []string{"--topic", "grp"}, &out); err != nil {
		t.Fatal(err)
	}
	var row struct {
		ID    string `json:"id"`
		Topic string `json:"topic"`
		Msgs  int    `json:"msgs"`
		By    string `json:"by"`
	}
	if err := json.Unmarshal(out.Bytes(), &row); err != nil || row.ID != info.ID || row.Msgs != 3 || row.By != "operator" {
		t.Fatalf("archives output %q (%v)", out.String(), err)
	}

	out.Reset()
	if err := agent.Read(ctx, []string{"--archive", info.ID}, &out); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(out.String()), "\n"); len(lines) != 3 {
		t.Errorf("read --archive = %d lines:\n%s", len(lines), out.String())
	}

	if err := agent.Read(ctx, []string{"--archive", closed.ID}, &bytes.Buffer{}); err == nil {
		t.Error("read of another task's tree archive succeeded")
	}
	if err := agent.Archives(ctx, []string{"--topic", tree}, &bytes.Buffer{}); err == nil {
		t.Error("archives of another task's tree topic succeeded")
	}
	if err := agent.Archives(ctx, nil, &bytes.Buffer{}); err == nil {
		t.Error("archives without --topic was accepted")
	}
	err = agent.Purge(ctx, []string{"--archive", info.ID}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "board_purge") {
		t.Errorf("purge --archive without the bit: %v", err)
	}
}
