package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// An empty `restore --list` used to be one sentence for four states, three of
// which are FAILURES that measured nothing. Zero candidates is a measurement
// -- the WAL was read and no prune in it is standing -- and it looked
// identical to: this server keeps no WAL at all, the events.log is not there,
// and the events.log would not parse. The only record of the last two was a
// line in the server's own log, which the operator asking the question cannot
// see.
//
// One test per state, because the bug was that they were indistinguishable:
// a test that only checked "empty list" would have passed on every one of
// them, before and after.

func TestRestorableSaysWhyWhenThereIsNoDataDir(t *testing.T) {
	// A server without --data-dir writes no WAL, so nothing it ever pruned
	// can come back. Reporting that as "no prune is standing" tells the
	// operator to stop looking for an id that was never recorded.
	rows, status := restorableFromPath("", func(string) bool { return true }, nil)
	if len(rows) != 0 {
		t.Fatalf("got %d rows from an unset path", len(rows))
	}
	if status != protocol.RestoreWALStatus_NoDataDir {
		t.Errorf("status = %v, want NoDataDir", status)
	}
}

func TestRestorableSaysWhyWhenTheWALIsMissing(t *testing.T) {
	// The state that motivated the whole enum: ReadWAL opens the file and
	// reports ErrNotExist as an EMPTY LIST rather than an error, so "the
	// server never wrote one" arrived at the operator as "no prunes".
	missing := filepath.Join(t.TempDir(), "events.log")
	rows, status := restorableFromPath(missing, func(string) bool { return true }, nil)
	if len(rows) != 0 {
		t.Fatalf("got %d rows from an absent WAL", len(rows))
	}
	if status != protocol.RestoreWALStatus_Missing {
		t.Errorf("status = %v, want Missing", status)
	}

	// The control: ReadWAL itself still answers the way the status exists to
	// compensate for. If this ever starts erroring, the os.Stat above is
	// redundant rather than load-bearing, and the comment saying so is wrong.
	if events, err := ReadWAL(missing); err != nil || len(events) != 0 {
		t.Errorf("ReadWAL on a missing file = (%d events, %v); "+
			"the Missing status exists because this returns (0, nil)", len(events), err)
	}
}

func TestRestorableSaysWhyWhenTheWALWillNotParse(t *testing.T) {
	// One bad line makes the WHOLE file unreadable, so this says nothing
	// about what was pruned -- which is exactly why it must not be reported
	// as "nothing was".
	path := filepath.Join(t.TempDir(), "events.log")
	if err := os.WriteFile(path, []byte("{\"type\":\"task_created\"}\nnot json at all\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, status := restorableFromPath(path, func(string) bool { return true }, nil)
	if len(rows) != 0 {
		t.Fatalf("got %d rows from an unparseable WAL", len(rows))
	}
	if status != protocol.RestoreWALStatus_Unreadable {
		t.Errorf("status = %v, want Unreadable", status)
	}
}

func TestRestorableReportsOkWhenTheWALWasActuallyRead(t *testing.T) {
	// The measurement. A readable WAL with no standing prune is genuinely
	// "nothing to put back", and it must not be lumped in with the three
	// failures above.
	s, _, dir := walStore(t)
	path := filepath.Join(dir, "events.log")
	id := s.Create("/repo", "kept", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, defaultScope(), "")
	s.MarkFailed(id, "x")

	rows, status := restorableFromPath(path, s.Live, nil)
	if status != protocol.RestoreWALStatus_Ok {
		t.Fatalf("status = %v, want Ok", status)
	}
	if len(rows) != 0 {
		t.Fatalf("nothing was pruned, so nothing is restorable; got %+v", rows)
	}

	// And once something IS pruned, the same call reports it -- so Ok is not
	// a synonym for "empty".
	s.PruneByIDs(nil, []string{id}, false, filepath.Join(dir, "logs"))
	rows, status = restorableFromPath(path, s.Live, nil)
	if status != protocol.RestoreWALStatus_Ok || len(rows) != 1 {
		t.Fatalf("after a prune: status=%v rows=%d, want Ok and 1", status, len(rows))
	}
}

// TestReadWALSurvivesALongPromptLine is the other half of the same failure.
//
// The WAL is one JSON line per event and a task_created carries the whole
// PROMPT. At the old 1 MiB scanner cap a single long-prompt submit made the
// scan fail -- and the failure is not local: it takes the whole file with it,
// so replay loses every task and `restore` reports nothing to put back
// forever, on a server that recorded everything correctly.
func TestReadWALSurvivesALongPromptLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.log")
	long := strings.Repeat("x", 4<<20) // 4 MiB: past the old cap, inside the new one
	rec, err := json.Marshal(WALEvent{Type: "task_created", TaskID: "a", Prompt: long})
	if err != nil {
		t.Fatal(err)
	}
	if werr := os.WriteFile(path, append(rec, '\n'), 0o600); werr != nil {
		t.Fatal(werr)
	}
	events, rerr := ReadWAL(path)
	if rerr != nil {
		t.Fatalf("ReadWAL: %v\nA prompt longer than the scanner's cap must not cost the whole WAL", rerr)
	}
	if len(events) != 1 || len(events[0].Prompt) != len(long) {
		t.Fatalf("got %d events, prompt %d bytes; want 1 and %d",
			len(events), len(events[0].Prompt), len(long))
	}
}

// A parse error names the line. Without it the operator is told "invalid
// character" about a file with a hundred thousand of them.
func TestReadWALErrorNamesTheLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.log")
	good, _ := json.Marshal(WALEvent{Type: "task_created", TaskID: "a"})
	body := append(append(good, '\n'), append(append([]byte{}, good...), '\n')...)
	body = append(body, []byte("{ broken\n")...)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ReadWAL(path)
	if err == nil {
		t.Fatal("a broken line parsed")
	}
	if !strings.Contains(err.Error(), ":3:") {
		t.Errorf("error does not name line 3: %v", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error does not name the file: %v", err)
	}
}
