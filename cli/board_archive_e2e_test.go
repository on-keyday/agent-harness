//go:build !js

package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// archiveE2E is an operator server with a store attached, and run drives the
// argv parser the binary uses.
func archiveE2E(t *testing.T) (*operatorE2E, func(sub string, args ...string) (string, error)) {
	t.Helper()
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
	t.Cleanup(cancel)
	return srv, func(sub string, args ...string) (string, error) {
		t.Helper()
		var out bytes.Buffer
		err := cli.RunBoardSubcmd(ctx, peerCID, sub, args, &out)
		return out.String(), err
	}
}

func sendSeqs(t *testing.T, srv *operatorE2E, topic string, n int) []uint64 {
	t.Helper()
	var seqs []uint64
	for i := 0; i < n; i++ {
		s, _, err := srv.Board().Send(topic, []byte("m"+strconv.Itoa(i)), protocol.RunnerID{}, protocol.TaskID{}, "h", "", 0)
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, s)
	}
	return seqs
}

var rowHeader = regexp.MustCompile(`(?m)^#(\d+) `)

func TestBoardFlushCLI(t *testing.T) {
	srv, run := archiveE2E(t)
	if _, err := run("retain", "keep", "--ttl", "forever"); err != nil {
		t.Fatal(err)
	}
	seqs := sendSeqs(t, srv, "keep", 20)
	out, err := run("flush", "keep")
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Status  string `json:"status"`
		Topic   string `json:"topic"`
		Through uint64 `json:"through"`
		Dropped int    `json:"dropped"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.Status != "ok" || res.Through != seqs[19] || res.Dropped != 20 {
		t.Fatalf("flush output %q (%v)", out, err)
	}
	topics, err := run("topics")
	if err != nil {
		t.Fatal(err)
	}
	if want := "flushed≤" + strconv.FormatUint(seqs[19], 10); !strings.Contains(topics, want) {
		t.Errorf("topics output lacks %q:\n%s", want, topics)
	}
	if out, _ := run("read", "keep"); rowHeader.MatchString(out) {
		t.Errorf("ring read after flush printed rows:\n%s", out)
	}
	out, err = run("read", "keep", "--limit", "64")
	if err != nil {
		t.Fatal(err)
	}
	if rows := rowHeader.FindAllStringSubmatch(out, -1); len(rows) != 20 {
		t.Errorf("paged read after flush = %d rows, want 20", len(rows))
	}
	if _, err := run("flush", "plain-missing"); err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Errorf("flush of a missing topic: %v", err)
	}
	sendSeqs(t, srv, "plain", 1)
	if _, err := run("flush", "plain"); err == nil || !strings.Contains(err.Error(), "not_retained") {
		t.Errorf("flush of an unretained topic: %v", err)
	}
}

func TestBoardArchiveCLI(t *testing.T) {
	srv, run := archiveE2E(t)
	seqs := sendSeqs(t, srv, "grp", 5)
	out, err := run("archive", "grp")
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Status  string `json:"status"`
		Archive string `json:"archive"`
		Through uint64 `json:"through"`
		Count   int    `json:"count"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.Status != "ok" || len(res.Archive) != 12 || res.Through != seqs[4] || res.Count != 5 {
		t.Fatalf("archive output %q (%v)", out, err)
	}
	id := res.Archive

	list, err := run("archives")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^` + id + ` topic=grp through=\d+ msgs=5 by=operator disk=\d+ at=\S+$`).MatchString(list) {
		t.Errorf("archives output:\n%s", list)
	}
	if list, _ := run("archives", "--topic", "other"); strings.Contains(list, id) {
		t.Errorf("archives --topic other listed %s", id)
	}

	out, err = run("read", "--archive", id)
	if err != nil {
		t.Fatal(err)
	}
	if rows := rowHeader.FindAllStringSubmatch(out, -1); len(rows) != 5 || rows[0][1] != strconv.FormatUint(seqs[4], 10) {
		t.Errorf("read --archive = %v", rows)
	}
	if _, err := run("read", "grp", "--archive", id); err == nil {
		t.Error("read with both a topic and --archive was accepted")
	}
	// --before alone pages an archive: its page size defaults to 64 (spec
	// §5.1). On a topic it still needs --limit, which selects paging there.
	out, err = run("read", "--archive", id, "--before", strconv.FormatUint(seqs[2], 10))
	if err != nil {
		t.Fatalf("read --archive --before without --limit: %v", err)
	}
	if rows := rowHeader.FindAllStringSubmatch(out, -1); len(rows) != 2 {
		t.Errorf("read --archive --before seq#2 = %d rows, want 2", len(rows))
	}
	if _, err := run("read", "grp", "--before", "5"); err == nil {
		t.Error("read <topic> --before without --limit was accepted")
	}

	out, err = run("purge", "--archive", id, "--seq", strconv.FormatUint(seqs[0], 10))
	if err != nil || !strings.Contains(out, `"purged":1`) {
		t.Fatalf("purge --archive --seq: %q %v", out, err)
	}
	out, err = run("purge", "--seq", strconv.FormatUint(seqs[1], 10), "--archive", id)
	if err != nil || !strings.Contains(out, `"purged":1`) {
		t.Fatalf("purge --seq --archive: %q %v", out, err)
	}
	out, err = run("purge", "--archive", id)
	if err != nil || !strings.Contains(out, `"purged":3`) {
		t.Fatalf("purge --archive (whole): %q %v", out, err)
	}
	if list, _ := run("archives"); strings.Contains(list, id) {
		t.Errorf("deleted archive still listed:\n%s", list)
	}
	if msgs, ok := srv.Board().ListRetained("grp"); !ok || len(msgs) != 5 {
		t.Errorf("archive operations changed the topic: %d %v", len(msgs), ok)
	}
}
