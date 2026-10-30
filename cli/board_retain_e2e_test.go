//go:build !js

package cli_test

import (
	"bytes"
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Driven through RunBoardSubcmd — the argv parser the binary uses — so a flag
// the parser drops shows up here, not first on a live board.
func TestBoardRetainCLI(t *testing.T) {
	srv, peerCID := startOperatorServerE2E(t)
	store, err := agentboard.OpenFileStore(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := srv.Board().AttachStore(store); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	run := func(sub string, args ...string) (string, error) {
		t.Helper()
		var out bytes.Buffer
		err := cli.RunBoardSubcmd(ctx, peerCID, sub, args, &out)
		return out.String(), err
	}
	topicLine := func(name string) string {
		t.Helper()
		out, err := run("topics")
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, name+"  ") {
				return l
			}
		}
		t.Fatalf("no topics line for %s in:\n%s", name, out)
		return ""
	}

	if out, err := run("retain", "free.x", "--ttl", "2h"); err != nil || !strings.Contains(out, `"status":"ok"`) {
		t.Fatalf("retain --ttl 2h: %q, %v", out, err)
	}
	if l := topicLine("free.x"); !regexp.MustCompile(`retain=2h0m0s by=operator disk=\d+`).MatchString(l) {
		t.Errorf("topics line = %q, want retain=2h0m0s by=operator disk=<n>", l)
	}
	if out, err := run("retain", "free.x", "--ttl", "forever"); err != nil || !strings.Contains(out, `"retain":"forever"`) {
		t.Fatalf("retain --ttl forever: %q, %v", out, err)
	}
	if out, err := run("retain", "free.x", "--clear"); err != nil || !strings.Contains(out, `"status":"ok"`) {
		t.Fatalf("retain --clear: %q, %v", out, err)
	}
	if l := topicLine("free.x"); strings.Contains(l, "retain=") {
		t.Errorf("topics line after --clear still shows retention: %q", l)
	}
	if _, err := run("retain", "chat.0123abcd", "--ttl", "1h"); err == nil || !strings.Contains(err.Error(), "not_retainable") {
		t.Errorf("retain on a chat topic: err = %v, want not_retainable", err)
	}
	if _, err := run("retain", "x"); err == nil {
		t.Error("retain with neither --ttl nor --clear was accepted")
	}
	if _, err := run("retain", "x", "--ttl", "1h", "--clear"); err == nil {
		t.Error("retain with both --ttl and --clear was accepted")
	}
	if _, err := run("retain", "x", "--ttl", "soon"); err == nil {
		t.Error("retain --ttl soon was accepted")
	}

	// Paging reaches past the 64-entry ring on a retained topic.
	if _, err := run("retain", "paged", "--ttl", "forever"); err != nil {
		t.Fatal(err)
	}
	var seqs []uint64
	for i := 0; i < 100; i++ {
		s, _, err := srv.Board().Send("paged", []byte("m"+strconv.Itoa(i)), protocol.RunnerID{}, protocol.TaskID{}, "h", "", 0)
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, s)
	}
	header := regexp.MustCompile(`(?m)^#(\d+) `)
	out, err := run("read", "paged", "--limit", "10")
	if err != nil {
		t.Fatal(err)
	}
	rows := header.FindAllStringSubmatch(out, -1)
	if len(rows) != 10 || rows[0][1] != strconv.FormatUint(seqs[99], 10) {
		t.Fatalf("read --limit 10 = %d rows starting %v, want 10 from seq %d", len(rows), rows, seqs[99])
	}
	out, err = run("read", "paged", "--before", strconv.FormatUint(seqs[10], 10), "--limit", "100")
	if err != nil {
		t.Fatal(err)
	}
	if rows := header.FindAllStringSubmatch(out, -1); len(rows) != 10 {
		t.Fatalf("read --before seq#10 = %d rows, want the 10 older ones (past the ring)", len(rows))
	}
}
