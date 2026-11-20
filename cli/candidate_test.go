package cli

import (
	"context"
	"strings"
	"testing"
)

func TestResolveCandidateNamesTheCandidate(t *testing.T) {
	_, err := ResolveCandidate("not-a-cid")
	if err == nil || !strings.Contains(err.Error(), "not-a-cid") {
		t.Fatalf("error %v does not name the candidate", err)
	}
}

// ParseCandidate checks the grammar WITHOUT resolving: a host name that does
// not resolve yet is still a valid dial record candidate.
func TestParseCandidateDoesNotResolve(t *testing.T) {
	if err := ParseCandidate("ws:nosuchhost.invalid:8540-*"); err != nil {
		t.Fatalf("an unresolvable host must parse: %v", err)
	}
	// The id follows the LAST '-', so a host name holding one (every EC2
	// public DNS name) is a candidate like any other.
	if err := ParseCandidate("ws:ec2-1-2-3-4.example:8540-*"); err != nil {
		t.Fatalf("a host name with '-' must parse: %v", err)
	}
	if err := ParseCandidate("ws:ec2-1-2-3-4.example:8540"); err == nil {
		t.Fatal("a candidate without an id must not parse")
	}
	if err := ParseCandidate("ws:127.0.0.1:8540-*"); err != nil {
		t.Fatalf("literal address: %v", err)
	}
	if err := ParseCandidate("not-a-cid"); err == nil {
		t.Fatal("garbage must not parse")
	}
}

// ResolveCandidate looks a hyphenated host name up whole; before the id was
// cut at the last '-' it failed on the first label, before any lookup.
func TestResolveCandidateHyphenatedHost(t *testing.T) {
	_, err := ResolveCandidate("ws:no-such-host.invalid:8540-*")
	if err == nil || !strings.Contains(err.Error(), "lookup no-such-host.invalid") {
		t.Fatalf("host not looked up whole: %v", err)
	}
}

func TestCandidateTransport(t *testing.T) {
	if got := CandidateTransport("udp:a.example:1-*"); got != "udp" {
		t.Fatalf("got %q", got)
	}
}

func TestSplitCandidatesRefusesEmptyEntry(t *testing.T) {
	if got, err := SplitCandidates(" ws:a:1-* , udp:a:2-* "); err != nil || len(got) != 2 || got[1] != "udp:a:2-*" {
		t.Fatalf("got %q %v", got, err)
	}
	if _, err := SplitCandidates("ws:a:1-*,"); err == nil {
		t.Fatal("a trailing comma is a typo, not an empty candidate to skip")
	}
}

func TestCanonicalDialRecordCandidate(t *testing.T) {
	for _, c := range []struct{ in, want, errHas string }{
		{"WSS://H:443/WS", "wss://h/WS", ""},
		{"wss://agent-1.example.com:8443/a/ws", "wss://agent-1.example.com:8443/a/ws", ""},
		{"ws:1.2.3.4:80-*", "ws:1.2.3.4:80-*", ""},
		{"wss://h", "", "path"},
		{"wss://h/ws?", "", "query is not supported"},
		{"not-a-cid", "", ""},
	} {
		got, err := CanonicalDialRecordCandidate(c.in)
		if c.want == "" {
			if err == nil {
				t.Errorf("CanonicalDialRecordCandidate(%q) = %q, want an error", c.in, got)
			} else if c.errHas != "" && !strings.Contains(err.Error(), c.errHas) {
				t.Errorf("CanonicalDialRecordCandidate(%q) error %v, want it to name %q", c.in, err, c.errHas)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("CanonicalDialRecordCandidate(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	if tr := CandidateTransport("wss://h/ws"); tr != "wss" {
		t.Errorf("CandidateTransport(URL) = %q, want wss", tr)
	}
	if !IsURLCandidate("wss://h/ws") || IsURLCandidate("ws:h:1-*") {
		t.Error("IsURLCandidate misclassifies")
	}
}

// A refused URL is refused before the request leaves, with the rule named:
// the server answers only invalid_candidates.
func TestRunnerDialAddWithRefusesBadURLLocally(t *testing.T) {
	f := &fakeTaskControlClient{}
	_, _, err := RunnerDialAddWith(context.Background(), f, []string{"wss://h/ws?token=x"}, "abcd1234", "")
	if err == nil || !strings.Contains(err.Error(), "query is not supported") {
		t.Fatalf("err = %v, want one naming the query rule", err)
	}
	if f.lastRequest != nil {
		t.Fatal("the request was sent")
	}
}
