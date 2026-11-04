package cli

import (
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
