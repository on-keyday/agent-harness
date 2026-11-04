package cli_test

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The dial and kick verbs round-trip over a live server: request encoding, the
// rows-on-a-stream answer, and the status mapping.
func TestRunnerDialAndKickRoundTrip(t *testing.T) {
	_, peerCID := startOperatorServerE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := cli.Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	pub, _, _ := ed25519.GenerateKey(nil)
	if st, err := cli.RunnerEnrollWith(ctx, c, pub, "aws"); err != nil || st != protocol.RunnerKeyStatus_Ok {
		t.Fatalf("enroll: %v %v", st, err)
	}
	// Port 1 on loopback: nothing answers, so the record stays waiting.
	cands := []string{"ws:127.0.0.1:1-*"}
	st, id, err := cli.RunnerDialAddWith(ctx, c, cands, hex.EncodeToString(pub)[:8], "aws-a")
	if err != nil || st != protocol.RunnerDialStatus_Ok || id == 0 {
		t.Fatalf("add: %v id=%d %v", st, id, err)
	}
	rows, err := cli.RunnerDialListWith(ctx, c)
	if err != nil || len(rows) != 1 || rows[0].ID != id || rows[0].Label != "aws-a" || rows[0].Candidates[0] != cands[0] || !rows[0].Key.Equal(pub) {
		t.Fatalf("list: %+v %v", rows, err)
	}
	if st, err := cli.RunnerDialRemoveWith(ctx, c, id); err != nil || st != protocol.RunnerDialStatus_Ok {
		t.Fatalf("remove: %v %v", st, err)
	}
	if rows, _ := cli.RunnerDialListWith(ctx, c); len(rows) != 0 {
		t.Fatalf("after remove: %+v", rows)
	}
	if st, _, err := cli.RunnerKickWith(ctx, c, "ffffffff", false); err != nil || st != protocol.RunnerKickStatus_NotFound {
		t.Fatalf("kick: %v %v", st, err)
	}
}
