package cli

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func fanoutFixture() []ThreadRow {
	return []ThreadRow{
		{Topic: "chat.aaaaaaaa", Msg: BoardMessage{Seq: 1}},
		{Topic: "chat.aaaaaaaa", Msg: BoardMessage{Seq: 2, Retracted: true}},
		{Topic: "chat.bbbbbbbb", Msg: BoardMessage{Seq: 3}},
		{Topic: "chat.bbbbbbbb", Msg: BoardMessage{Seq: 4, Retracted: true}},
	}
}

// TestFanoutRetractSkipsAlreadyWithdrawn is the regression this helper exists
// for. The server collapses already-withdrawn into not_found so a reply cannot
// probe a topic, so a naive fan-out reports a half-withdrawn thread as
// "retracted N / not-found N" -- success rendered as failure. Measured on the
// exchange that prompted this feature: 11 of 22 messages were already
// auto-retracted, because a reply withdraws the message it answers.
//
// The client has just read every row and holds Retracted per message, so it
// partitions locally and asks the server nothing extra.
func TestFanoutRetractSkipsAlreadyWithdrawn(t *testing.T) {
	var calls []uint64
	res := fanoutRows(fanoutFixture(), "hdr", "retracted", true,
		func(topic string, seq uint64) (bool, error) {
			calls = append(calls, seq)
			return true, nil
		})

	if len(calls) != 2 || calls[0] != 1 || calls[1] != 3 {
		t.Fatalf("calls = %v, want [1 3]: a withdrawn row must not be asked about", calls)
	}
	want := map[string]int{"retracted": 2, "already-withdrawn": 2, "not-found": 0, "failed": 0, "skipped": 0}
	got := res.Counts()
	for k, v := range want {
		if got[k] != v {
			t.Errorf("Counts()[%q] = %d, want %d (all: %v)", k, got[k], v, got)
		}
	}
}

// TestFanoutPurgeReachesAlreadyWithdrawn is the OPPOSITE rule, and the one the
// two-stage workflow rests on: retract moves a message from topic.ring to
// topic.retracted, removeSeq scans both, so purge must still be asked about it.
// Skipping withdrawn rows here -- the rule that is correct for retract -- would
// make stage 2 unable to clear what stage 1 withdrew.
func TestFanoutPurgeReachesAlreadyWithdrawn(t *testing.T) {
	var calls []uint64
	res := fanoutRows(fanoutFixture(), "hdr", "purged", false,
		func(topic string, seq uint64) (bool, error) {
			calls = append(calls, seq)
			return true, nil
		})

	if len(calls) != 4 {
		t.Fatalf("calls = %v, want all four seqs: purge reaches withdrawn messages", calls)
	}
	got := res.Counts()
	if got["purged"] != 4 {
		t.Errorf("Counts()[\"purged\"] = %d, want 4 (all: %v)", got["purged"], got)
	}
	if _, present := got["already-withdrawn"]; present {
		t.Errorf("purge reported an already-withdrawn category; it has none: %v", got)
	}
}

// TestFanoutSummaryPrintsZeros: a zero is a measurement. Eliding it makes
// "nothing failed" indistinguishable from "failures were not counted". The
// category set is fixed per op rather than derived from the outcomes seen,
// because deriving it means a run where everything was already withdrawn
// prints no "retracted 0" at all.
func TestFanoutSummaryPrintsZeros(t *testing.T) {
	allWithdrawn := []ThreadRow{
		{Topic: "t", Msg: BoardMessage{Seq: 1, Retracted: true}},
	}
	s := fanoutRows(allWithdrawn, "hdr", "retracted", true,
		func(string, uint64) (bool, error) { return true, nil }).Summary()
	for _, want := range []string{"retracted 0", "already-withdrawn 1", "not-found 0", "failed 0", "skipped 0"} {
		if !strings.Contains(s, want) {
			t.Errorf("Summary() = %q, missing %q", s, want)
		}
	}
}

// TestFanoutStopsOnErrorAndSkipsTheRest: a capability denial on the first call
// is a denial on every call, so the helper must not hammer the server with the
// remaining rows. The untried rows are "skipped", which is neither "we tried
// and it was gone" nor "it failed".
func TestFanoutStopsOnErrorAndSkipsTheRest(t *testing.T) {
	rows := []ThreadRow{
		{Topic: "t", Msg: BoardMessage{Seq: 1}},
		{Topic: "t", Msg: BoardMessage{Seq: 2}},
		{Topic: "t", Msg: BoardMessage{Seq: 3}},
	}
	boom := errors.New("permission denied: BoardRetract requires capability purge")
	n := 0
	res := fanoutRows(rows, "hdr", "retracted", true,
		func(string, uint64) (bool, error) {
			n++
			if n == 2 {
				return false, boom
			}
			return true, nil
		})
	if n != 2 {
		t.Fatalf("issued %d calls, want 2: the run must stop at the first error", n)
	}
	if !errors.Is(res.Err, boom) {
		t.Errorf("Err = %v, want the stopping error", res.Err)
	}
	got := res.Counts()
	if got["retracted"] != 1 || got["failed"] != 1 || got["skipped"] != 1 {
		t.Errorf("Counts() = %v, want retracted 1 / failed 1 / skipped 1", got)
	}
}

// TestFanoutNotFoundContinues: not-found is an outcome, not a fault.
func TestFanoutNotFoundContinues(t *testing.T) {
	rows := []ThreadRow{
		{Topic: "t", Msg: BoardMessage{Seq: 1}},
		{Topic: "t", Msg: BoardMessage{Seq: 2}},
	}
	n := 0
	res := fanoutRows(rows, "hdr", "purged", false,
		func(string, uint64) (bool, error) {
			n++
			return false, nil
		})
	if n != 2 {
		t.Fatalf("issued %d calls, want 2: not-found must not stop the run", n)
	}
	if res.Err != nil {
		t.Errorf("Err = %v, want nil", res.Err)
	}
	if got := res.Counts(); got["not-found"] != 2 {
		t.Errorf("Counts() = %v, want not-found 2", got)
	}
}

// TestFanoutUsesTheRowsOwnTopic: a thread spans topics by construction, so the
// destination cannot come from one argument.
func TestFanoutUsesTheRowsOwnTopic(t *testing.T) {
	rows := []ThreadRow{
		{Topic: "chat.aaaaaaaa", Msg: BoardMessage{Seq: 1}},
		{Topic: "chat.bbbbbbbb", Msg: BoardMessage{Seq: 2}},
	}
	seen := map[uint64]string{}
	fanoutRows(rows, "hdr", "retracted", true, func(topic string, seq uint64) (bool, error) {
		seen[seq] = topic
		return true, nil
	})
	if seen[1] != "chat.aaaaaaaa" || seen[2] != "chat.bbbbbbbb" {
		t.Fatalf("topics = %v, want each row's own", seen)
	}
}

// TestFanoutRowsCarryEveryMessage: the JSON form must account for every row,
// including the ones never called for, or a reader cannot reconcile it against
// what `board thread` showed.
func TestFanoutRowsCarryEveryMessage(t *testing.T) {
	res := fanoutRows(fanoutFixture(), "hdr", "retracted", true,
		func(string, uint64) (bool, error) { return true, nil })
	if len(res.Rows) != 4 {
		t.Fatalf("Rows = %d, want 4", len(res.Rows))
	}
	for _, r := range res.Rows {
		if r.Outcome == "" {
			t.Errorf("seq %d has no outcome", r.Seq)
		}
		if r.Topic == "" {
			t.Errorf("seq %d has no topic", r.Seq)
		}
	}
}

// TestThreadDestructionCallSitesArePinned pins which files call the per-seq
// destructive CLIENT methods, and fails when a new one joins them.
//
// The failure class it guards is the one implementation-pitfalls Pitfall 3
// records: a surface reaching past the shared helper and growing its own loop,
// which then drifts from the partitioning above -- most likely by asking the
// server about an already-withdrawn message and reporting the collapsed
// not-found as a failure. Counting the call sites is cheap; discovering the
// drift by symptom is not.
//
// It matches on the call SHAPE (`…, topic, seq)`) rather than the bare method
// name, because the name alone also hits the generated verb dispatch and the
// wire response accessors -- noise that would make the expected set a list of
// unrelated files, which is how a guard trains its readers to skip it.
//
// A new entry is not automatically wrong. It has to be a DELIBERATE entry with
// a reason, which is the point: this makes the addition visible, not illegal.
func TestThreadDestructionCallSitesArePinned(t *testing.T) {
	expected := map[string]string{
		"cli/board.go":                   "the package-level wrappers over the client methods",
		"cli/threadfanout.go":            "the shared fan-out -- the only thread-scoped caller",
		"tui/board.go":                   "w / X on ONE message inside the board modal, per-seq",
		"cmd/harness-webui-wasm/main.go": "harness.boardRetract / boardPurge, per-seq",
	}

	found := map[string]bool{}
	for _, root := range []string{".", "../tui", "../cmd/harness-webui-wasm"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			for _, line := range strings.Split(string(b), "\n") {
				if !strings.Contains(line, "topic, seq)") {
					continue
				}
				if !strings.Contains(line, ".BoardRetract(") && !strings.Contains(line, ".BoardPurge(") {
					continue
				}
				rel := filepath.ToSlash(filepath.Clean(path))
				rel = strings.TrimPrefix(rel, "../")
				if !strings.Contains(rel, "/") {
					rel = "cli/" + rel
				}
				found[rel] = true
				break
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	var unexpected, missing []string
	for f := range found {
		if _, ok := expected[f]; !ok {
			unexpected = append(unexpected, f)
		}
	}
	for f := range expected {
		if !found[f] {
			missing = append(missing, f)
		}
	}
	sort.Strings(unexpected)
	sort.Strings(missing)

	if len(unexpected) > 0 {
		t.Errorf("new per-seq destructive call site(s): %v\n"+
			"A thread-scoped caller must go through FanoutThread, which partitions\n"+
			"already-withdrawn rows locally. If this is a deliberate per-seq caller,\n"+
			"add it to `expected` with its reason.", unexpected)
	}
	if len(missing) > 0 {
		t.Errorf("expected call site(s) gone: %v -- drop them from `expected` if that is intended", missing)
	}
}
