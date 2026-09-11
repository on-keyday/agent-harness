package runner

import (
	"strings"
	"testing"

	"github.com/on-keyday/objtrsf/objproto"
)

func TestParseServerCandidatesKeepsOrderAndTrims(t *testing.T) {
	got, err := ParseServerCandidates(" ws:127.0.0.1:8539-* , udp:127.0.0.1:8540-* ")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"ws:127.0.0.1:8539-*", "udp:127.0.0.1:8540-*"}
	if got.Len() != len(want) {
		t.Fatalf("len = %d, want %d (%v)", got.Len(), len(want), got.All())
	}
	for i, w := range want {
		if got.All()[i] != w {
			t.Errorf("candidate %d = %q, want %q", i, got.All()[i], w)
		}
	}
}

// A single candidate is the shape every existing runner uses, and it must stay
// a list of one rather than a special case.
func TestParseServerCandidatesSingle(t *testing.T) {
	got, err := ParseServerCandidates("ws:127.0.0.1:8539-*")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Len() != 1 || got.All()[0] != "ws:127.0.0.1:8539-*" {
		t.Fatalf("got %v, want one entry", got.All())
	}
}

// An empty entry is a typo (a stray or trailing comma), not "no candidate
// here": silently dropping it would dial a list the operator did not write.
func TestParseServerCandidatesRejectsEmptyEntry(t *testing.T) {
	for _, spec := range []string{"", "   ", ",", "ws:a:1-*,", ",ws:a:1-*", "ws:a:1-*,,udp:b:2-*"} {
		if _, err := ParseServerCandidates(spec); err == nil {
			t.Errorf("ParseServerCandidates(%q): want error, got none", spec)
		}
	}
}

// CandidatesOf is how an already-resolved ConnectionID (tests, embedders)
// enters the list. The whole type rests on ConnectionID.String() being
// re-parsable, which is also what agents rely on: the runner injects
// HARNESS_SERVER_CID as String() and harness-cli parses it back.
func TestCandidatesOfRoundTripsThroughText(t *testing.T) {
	cid, err := objproto.ParseConnectionID("ws:127.0.0.1:8539-12345", objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c := CandidatesOf(cid)
	if c.Len() != 1 {
		t.Fatalf("len = %d, want 1", c.Len())
	}
	back, err := ResolveServerCandidate(c.All()[0])
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if back != cid {
		t.Fatalf("round trip: got %v, want %v", back, cid)
	}
}

// A candidate that cannot be resolved must name ITSELF in the error. With a
// list, "invalid connection ID format" alone does not say which entry.
func TestResolveServerCandidateNamesTheCandidate(t *testing.T) {
	_, err := ResolveServerCandidate("not-a-cid")
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "not-a-cid") {
		t.Fatalf("error %q does not name the candidate", err)
	}
}
