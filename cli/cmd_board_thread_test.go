//go:build !js

package cli_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// threadTask builds a TaskID whose first byte is the given value, so a test
// can name senders the way the rows display them (id8 = "41000000" for 'a').
func threadTask(id byte) protocol.TaskID {
	var t protocol.TaskID
	t.Id[0] = id
	return t
}

// threadTaskHex renders threadTask's id the way BoardMessage exposes it: the
// full 32-hex string FromTaskHex carries.
func threadTaskHex(id byte) string {
	t := threadTask(id)
	return hex.EncodeToString(t.Id[:])
}

// seedThreadChain publishes a chain that spans two topics: A speaks on its
// own inbound topic (chat.41000000), B replies — the reply lands on B's
// inbound topic (chat.42000000) — and A answers again. With thirdParty, C
// also speaks alone on chat.43000000: a separate chain with no reply links,
// which is the chain a --task filter must be able to exclude.
func seedThreadChain(t *testing.T, srv *operatorE2E, thirdParty bool) (s1, s2, s3 uint64) {
	t.Helper()
	var err error
	s1, _, err = srv.Board().Send("chat.41000000", []byte("root from A"),
		protocol.RunnerID{}, threadTask('a'), "h", "claude", 0)
	if err != nil {
		t.Fatal(err)
	}
	s2, _, err = srv.Board().Send("chat.42000000", []byte("reply from B"),
		protocol.RunnerID{}, threadTask('b'), "h", "claude", s1)
	if err != nil {
		t.Fatal(err)
	}
	s3, _, err = srv.Board().Send("chat.41000000", []byte("reply2 from A"),
		protocol.RunnerID{}, threadTask('a'), "h", "claude", s2)
	if err != nil {
		t.Fatal(err)
	}
	if thirdParty {
		if _, _, err := srv.Board().Send("chat.43000000", []byte("c talks to himself"),
			protocol.RunnerID{}, threadTask('c'), "h", "claude", 0); err != nil {
			t.Fatal(err)
		}
	}
	return s1, s2, s3
}

// runBoardThread runs `board thread <args...>` through the real parse +
// dispatch path (RunBoardSubcmd), so the tests cross the same boundary the
// operator's argv does — pitfall 13: the client method alone is one layer
// below the defect.
func runBoardThread(t *testing.T, peerCID objproto.ConnectionID, out *bytes.Buffer, args ...string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return cli.RunBoardSubcmd(ctx, peerCID, "thread", args, out)
}

// The chain view exists to JOIN sides of a dialogue that live on different
// topics. This is the test for the case it exists for: one chain, three
// messages, two topics, rendered joined in seq order.
func TestBoardThread_JoinsAcrossTopics(t *testing.T) {
	srv, peerCID := startOperatorServerE2E(t)
	s2, _, _ := seedThreadChain(t, srv, false)

	var out bytes.Buffer
	if err := runBoardThread(t, peerCID, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"root from A", "reply from B", "reply2 from A",
		"topic=chat.41000000", "topic=chat.42000000",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("thread output missing %q:\n%s", want, got)
		}
	}
	// The window line is what makes an empty view legible; it must be there.
	if !strings.Contains(got, cli.ThreadWindowOperator) {
		t.Errorf("window statement missing:\n%s", got)
	}
	// Order: the three bodies must appear in seq order.
	i1 := strings.Index(got, "root from A")
	i2 := strings.Index(got, "reply from B")
	i3 := strings.Index(got, "reply2 from A")
	if !(0 <= i1 && i1 < i2 && i2 < i3) {
		t.Errorf("rows not in seq order (root %d, reply %d, reply2 %d):\n%s", i1, i2, i3, got)
	}
	// re= on the replies names the parent.
	if !strings.Contains(got, " re="+strconv.FormatUint(s2, 10)) {
		t.Errorf("reply row missing re=%d:\n%s", s2, got)
	}
}

// One named task keeps the whole chain it participated in — including the
// other side's messages, which is the point of the view.
func TestBoardThread_TaskFilterKeepsWholeChain(t *testing.T) {
	srv, peerCID := startOperatorServerE2E(t)
	seedThreadChain(t, srv, true) // C's separate chain present

	var out bytes.Buffer
	if err := runBoardThread(t, peerCID, &out, "--task", threadTaskHex('a')); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"root from A", "reply from B", "reply2 from A"} {
		if !strings.Contains(got, want) {
			t.Errorf("A's chain incomplete, missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "c talks to himself") {
		t.Errorf("an unrelated chain leaked through the --task filter:\n%s", got)
	}
}

// Two ids: the pair's exchange, NOT a private one — anything either had with
// a third party stays visible. C's solo chain involves neither, so it is the
// one that must stay out.
func TestBoardThread_TaskFilterUnionsIds(t *testing.T) {
	srv, peerCID := startOperatorServerE2E(t)
	seedThreadChain(t, srv, true)

	var out bytes.Buffer
	if err := runBoardThread(t, peerCID, &out, "--task", threadTaskHex('a'), "--task", threadTaskHex('b')); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"root from A", "reply from B", "reply2 from A"} {
		if !strings.Contains(got, want) {
			t.Errorf("pair exchange incomplete, missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "c talks to himself") {
		t.Errorf("C's solo chain leaked into the A+B view:\n%s", got)
	}
}

// A --task naming a task that matches NOTHING is an empty result, never a
// silent fall-back to unfiltered — the gap the agent face's test caught in
// SelectThreads; pinned here so the operator face cannot regress either.
func TestBoardThread_TaskFilterMatchingNothingIsEmpty(t *testing.T) {
	srv, peerCID := startOperatorServerE2E(t)
	seedThreadChain(t, srv, true)

	var out bytes.Buffer
	if err := runBoardThread(t, peerCID, &out, "--task", strings.Repeat("f", 32)); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Contains(got, "root from A") || strings.Contains(got, "c talks to himself") {
		t.Errorf("a non-matching --task printed the unfiltered board:\n%s", got)
	}
	if !strings.Contains(got, cli.ThreadWindowOperator) {
		t.Errorf("empty result lost the window line:\n%s", got)
	}
}

// A --seq naming a message outside the visible set is an ERROR that names
// the seq — an empty result and a bad argument must not look the same.
func TestBoardThread_SeqOutsideVisibleSetIsError(t *testing.T) {
	srv, peerCID := startOperatorServerE2E(t)
	seedThreadChain(t, srv, false)

	var out bytes.Buffer
	err := runBoardThread(t, peerCID, &out, "--seq", "999999")
	if err == nil {
		t.Fatalf("want an error naming the seq, got none; output:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "999999") {
		t.Errorf("error does not name the seq: %v", err)
	}
}

// --seq selects the WHOLE chain containing it, and the filtered view still
// excludes unrelated chains.
func TestBoardThread_SeqSelectsWholeChain(t *testing.T) {
	srv, peerCID := startOperatorServerE2E(t)
	s1, _, _ := seedThreadChain(t, srv, true)

	var out bytes.Buffer
	if err := runBoardThread(t, peerCID, &out, "--seq", strconv.FormatUint(s1, 10)); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"root from A", "reply from B", "reply2 from A"} {
		if !strings.Contains(got, want) {
			t.Errorf("chain incomplete for --seq %d, missing %q:\n%s", s1, want, got)
		}
	}
	if strings.Contains(got, "c talks to himself") {
		t.Errorf("--seq pulled in an unrelated chain:\n%s", got)
	}
}

// --headers-only suppresses bodies; rows stay.
func TestBoardThread_HeadersOnly(t *testing.T) {
	srv, peerCID := startOperatorServerE2E(t)
	seedThreadChain(t, srv, false)

	var out bytes.Buffer
	if err := runBoardThread(t, peerCID, &out, "--headers-only"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "root from A") {
		t.Errorf("--headers-only still printed a body:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "topic=chat.41000000") {
		t.Errorf("--headers-only dropped the rows themselves:\n%s", out.String())
	}
}

// --json emits one record per row with the chain placement, and the seq
// linkage matches what was published.
func TestBoardThread_JSON(t *testing.T) {
	srv, peerCID := startOperatorServerE2E(t)
	s1, _, _ := seedThreadChain(t, srv, false)

	var out bytes.Buffer
	if err := runBoardThread(t, peerCID, &out, "--json"); err != nil {
		t.Fatal(err)
	}
	var recs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("bad JSON line %q: %v", line, err)
		}
		recs = append(recs, rec)
	}
	if len(recs) != 3 {
		t.Fatalf("got %d JSON records, want 3:\n%s", len(recs), out.String())
	}
	if recs[0]["seq"].(float64) != float64(s1) || recs[1]["in_reply_to"].(float64) != float64(s1) {
		t.Errorf("seq linkage wrong: %v / %v", recs[0]["seq"], recs[1]["in_reply_to"])
	}
	if recs[1]["depth"].(float64) != 1 || recs[1]["orphan"].(bool) {
		t.Errorf("chain placement wrong on row 1: %v", recs[1])
	}
	if _, ok := recs[0]["payload_b64"]; !ok {
		t.Errorf("payload_b64 missing:\n%v", recs[0])
	}
}

// An orphan is marked on the row, not hidden — and shown at root.
func TestBoardThread_OrphanMarkedNotHidden(t *testing.T) {
	srv, peerCID := startOperatorServerE2E(t)
	if _, _, err := srv.Board().Send("chat.41000000", []byte("orphan child"),
		protocol.RunnerID{}, threadTask('a'), "h", "claude", 123456789); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runBoardThread(t, peerCID, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ORPHAN") {
		t.Errorf("orphan not marked:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "orphan child") {
		t.Errorf("orphan row hidden — a chain view re-orders, never filters:\n%s", out.String())
	}
}

// Bodies through the chain view on a redirected writer are the published
// bytes — the same gate board read went through in Task 3.
func TestBoardThread_RedirectedBodyIsByteExact(t *testing.T) {
	srv, peerCID := startOperatorServerE2E(t)
	body := []byte("x\x1b[2J\r\u009b\xff\xfe binary")
	if _, _, err := srv.Board().Send("chat.41000000", body,
		protocol.RunnerID{}, threadTask('a'), "h", "claude", 0); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runBoardThread(t, peerCID, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), string(body)) {
		t.Errorf("redirected body altered:\n%s", out.String())
	}
}
