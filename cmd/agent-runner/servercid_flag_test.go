package main

import (
	"flag"
	"strings"
	"testing"
)

// TestServerCIDFlagTakesCandidateList verifies --server-cid accepts an ordered,
// comma-separated candidate list. This is the runner-only half of the flag:
// harness-cli still takes exactly one, because only the runner has to cross a
// change of network with no human present.
func TestServerCIDFlagTakesCandidateList(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg := newMainConfig()
	cfg.bindFlags(fs)

	if err := fs.Parse([]string{
		"--server-cid", "ws:127.0.0.1:8539-*,ws:harness.example.ts.net:8539-*",
		"--roots", "/tmp",
	}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	cands, err := cfg.serverCandidates()
	if err != nil {
		t.Fatalf("serverCandidates: %v", err)
	}
	if cands.Len() != 2 {
		t.Fatalf("len = %d, want 2 (%v)", cands.Len(), cands.All())
	}
	if cands.All()[0] != "ws:127.0.0.1:8539-*" {
		t.Errorf("first candidate = %q; the list is ordered and the LAN entry was first",
			cands.All()[0])
	}
}

// A stray comma must fail at startup, where the operator is still looking,
// rather than at the first dial.
func TestServerCIDFlagRejectsEmptyCandidate(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg := newMainConfig()
	cfg.bindFlags(fs)

	if err := fs.Parse([]string{
		"--server-cid", "ws:127.0.0.1:8539-*,,ws:127.0.0.1:8540-*",
		"--roots", "/tmp",
	}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	err := cfg.validate()
	if err == nil || !strings.Contains(err.Error(), "server-cid") {
		t.Fatalf("want a --server-cid error, got %v", err)
	}
}

// Listen mode has no candidates at all; validate must not demand any.
func TestServerCIDCandidatesNotRequiredInListenMode(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg := newMainConfig()
	cfg.bindFlags(fs)

	if err := fs.Parse([]string{
		"--server-cid", "",
		"--listen", "0.0.0.0:8540",
		"--roots", "/tmp",
	}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}
