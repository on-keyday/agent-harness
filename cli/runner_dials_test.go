package cli

import (
	"crypto/ed25519"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func dialTestRunnerID(b byte) protocol.RunnerID {
	var id protocol.RunnerID
	for i := range id.Id {
		id.Id[i] = b
	}
	return id
}

func TestFormatRunnerDialListPrintsEveryField(t *testing.T) {
	key := ed25519.PublicKey(make([]byte, 32))
	key[0] = 0xab
	at := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	rows := []RunnerDialRow{
		{ID: 0x3fa9c012, Label: "aws", Key: key, Candidates: []string{"ws:a.example:8540-*", "udp:a.example:8541-*"},
			State: protocol.RunnerDialState_Connected, Runner: dialTestRunnerID(0x11), Answered: 1,
			AddedAt: at, AddedBy: "operator"},
		{ID: 0x00000007, Key: key, Candidates: []string{"ws:b.example:8540-*"},
			State: protocol.RunnerDialState_Waiting, Answered: -1, NextAttempt: at.Add(time.Minute),
			LastError: "key mismatch: 00112233", AddedAt: at, AddedBy: "task:00112233445566778899aabbccddeeff"},
		{ID: 0x00000008, Key: key, Candidates: []string{"ws:c.example:8540-*"},
			State: protocol.RunnerDialState_Duplicate, Answered: -1, DuplicateOf: 0x3fa9c012, AddedAt: at, AddedBy: "operator"},
	}
	out := FormatRunnerDialList(rows)
	for _, want := range []string{
		"3fa9c012 state=connected label=aws key=ab00000000000000 runner=11111111 answered=udp:a.example:8541-* by=operator added=2026-11-04T10:00:00Z",
		"  candidates: ws:a.example:8540-*, udp:a.example:8541-*",
		"00000007 state=waiting label= key=ab00000000000000 runner=- next=2026-11-04T10:01:00Z err=\"key mismatch: 00112233\" by=task:00112233445566778899aabbccddeeff",
		"00000008 state=duplicate label= key=ab00000000000000 runner=- duplicate_of=3fa9c012",
		"records=3",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if got := FormatRunnerDialList(nil); got != "records=0\n" {
		t.Errorf("empty list: %q, want the count even at zero", got)
	}
}

func TestFormatRunnerDialAddAndRemove(t *testing.T) {
	if s, ok := FormatRunnerDialAdd(protocol.RunnerDialStatus_Ok, 0x3fa9c012, []string{"ws:a:1-*"}); !ok || s != "dial 3fa9c012 added: ws:a:1-*" {
		t.Errorf("ok add: %q %v", s, ok)
	}
	for st, want := range map[protocol.RunnerDialStatus]string{
		protocol.RunnerDialStatus_InvalidCandidates:  "runner dial add: invalid candidates",
		protocol.RunnerDialStatus_KeyNotFound:        "runner dial add: no enrolled key has that prefix",
		protocol.RunnerDialStatus_AmbiguousKey:       "runner dial add: more than one enrolled key has that prefix",
		protocol.RunnerDialStatus_DuplicateCandidate: "runner dial add: a candidate is already another record's",
		protocol.RunnerDialStatus_StoreFailed:        "runner dial add: the server could not write runner-dials.json; nothing was added",
	} {
		if s, ok := FormatRunnerDialAdd(st, 0, nil); ok || !strings.HasPrefix(s, want) {
			t.Errorf("%v: %q %v, want prefix %q", st, s, ok, want)
		}
	}
	if s, ok := FormatRunnerDialRemove(protocol.RunnerDialStatus_Ok, 7); !ok || !strings.HasPrefix(s, "dial 00000007 removed") {
		t.Errorf("remove: %q", s)
	}
	if s, ok := FormatRunnerDialRemove(protocol.RunnerDialStatus_NotFound, 7); ok || s != "runner dial rm: no dial record 00000007" {
		t.Errorf("remove not found: %q", s)
	}
	if s, ok := FormatRunnerDialRemove(protocol.RunnerDialStatus_StoreFailed, 7); ok || s != "runner dial rm: the server could not write runner-dials.json; record 00000007 stays" {
		t.Errorf("remove store failed: %q", s)
	}
}

func TestFormatRunnerKickEveryStatus(t *testing.T) {
	res := RunnerKickResult{Runner: RunnerKeyUser{ID: dialTestRunnerID(0x22), Hostname: "aws-a"},
		Tasks: []protocol.TaskID{{Id: [16]byte{0xaa}}}, RemovedDials: []uint32{0x3fa9c012}}
	cases := []struct {
		st     protocol.RunnerKickStatus
		ok     bool
		substr string
	}{
		{protocol.RunnerKickStatus_Left, true, "kicked 22222222 aws-a: it left; ended 1 task(s); removed dial record(s) 3fa9c012"},
		{protocol.RunnerKickStatus_ClosedWithoutLeaving, true, "the runner did not leave (too old to know the request?); a dial-mode runner reconnects"},
		{protocol.RunnerKickStatus_InUse, false, "refused: 22222222 aws-a has 1 live task(s) (pass --force to end them)"},
		{protocol.RunnerKickStatus_NotFound, false, "runner kick: no registered runner has that id prefix"},
		{protocol.RunnerKickStatus_AmbiguousPrefix, false, "runner kick: more than one runner has that id prefix"},
	}
	for _, c := range cases {
		s, ok := FormatRunnerKick(c.st, res)
		if ok != c.ok || !strings.Contains(s, c.substr) {
			t.Errorf("%v: %q ok=%v, want %q ok=%v", c.st, s, ok, c.substr, c.ok)
		}
	}
	if s, _ := FormatRunnerKick(protocol.RunnerKickStatus_InUse, res); !strings.Contains(s, "  aa000000000000000000000000000000") {
		t.Errorf("in_use must list the task ids: %q", s)
	}
}

func TestRunnerLinkStr(t *testing.T) {
	var r protocol.RunnerInfo
	cases := []struct {
		link protocol.RunnerLink
		rec  uint32
		via  protocol.RunnerID
		want string
	}{
		{protocol.RunnerLink_DialedIn, 0, protocol.RunnerID{}, "in"},
		{protocol.RunnerLink_DialedOut, 0, protocol.RunnerID{}, "out"},
		{protocol.RunnerLink_DialRecord, 0x3fa9c012, protocol.RunnerID{}, "dial:3fa9c012"},
		{protocol.RunnerLink_Via, 0, dialTestRunnerID(0x5c), "via:5c5c5c5c"},
	}
	for _, c := range cases {
		r.Link, r.LinkRecord, r.LinkVia = c.link, c.rec, c.via
		if got := RunnerLinkStr(r); got != c.want {
			t.Errorf("%v: %q, want %q", c.link, got, c.want)
		}
	}
}

func TestDialIDRoundTrip(t *testing.T) {
	if s := FormatDialID(0x3fa9c012); s != "3fa9c012" {
		t.Fatalf("format: %q", s)
	}
	if id, err := ParseDialID("3FA9C012"); err != nil || id != 0x3fa9c012 {
		t.Fatalf("parse: %x %v", id, err)
	}
	for _, bad := range []string{"", "3fa9c01", "3fa9c0123", "zzzzzzzz"} {
		if _, err := ParseDialID(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestFormatRunnerUnenrollListsRemovedDials(t *testing.T) {
	key := ed25519.PublicKey(make([]byte, 32))
	s, _ := FormatRunnerUnenroll(protocol.RunnerKeyStatus_Ok, RunnerUnenrollResult{Key: key, RemovedDials: []uint32{7, 8}})
	if !strings.Contains(s, "removed 2 dial record(s): 00000007 00000008") {
		t.Fatalf("%q", s)
	}
	s, _ = FormatRunnerUnenroll(protocol.RunnerKeyStatus_Ok, RunnerUnenrollResult{Key: key})
	if !strings.Contains(s, "removed 0 dial record(s)") {
		t.Fatalf("zero must be printed: %q", s)
	}
}
