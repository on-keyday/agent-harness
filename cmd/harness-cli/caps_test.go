//go:build !js

package main

import (
	"flag"
	"testing"
)

// TestCapsExplicitlySet verifies that capsExplicitlySet reports true only when
// "--caps" was actually present on the command line, and false when it was
// omitted (even if the flag is registered with a default).
func TestCapsExplicitlySet(t *testing.T) {
	// Case 1: --caps not provided — must return false.
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	_ = fs.String("caps", "", "")
	_ = fs.String("resume", "", "")
	if err := fs.Parse([]string{"--resume", "abc"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if capsExplicitlySet(fs) {
		t.Fatal("no --caps given → capsExplicitlySet must return false")
	}

	// Case 2: --caps explicitly provided — must return true.
	fs2 := flag.NewFlagSet("t", flag.ContinueOnError)
	_ = fs2.String("caps", "", "")
	if err := fs2.Parse([]string{"--caps", "spawn"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !capsExplicitlySet(fs2) {
		t.Fatal("--caps given → capsExplicitlySet must return true")
	}
}
