//go:build !js

package main

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TestParseCapsFlag verifies the three behaviours of parseCaps:
//  1. Empty input → Capability_All (inherit-all default).
//  2. Comma-separated valid names → correct OR mask.
//  3. Unknown name → error (not a silent no-op or panic).
func TestParseCapsFlag(t *testing.T) {
	// Empty → All
	got, err := parseCaps("")
	if err != nil {
		t.Fatalf("parseCaps(\"\") error: %v", err)
	}
	if got != protocol.Capability_All {
		t.Fatalf("parseCaps(\"\") = %#x, want %#x (Capability_All)", got, protocol.Capability_All)
	}

	// Whitespace-only → All
	got, err = parseCaps("   ")
	if err != nil {
		t.Fatalf("parseCaps(whitespace) error: %v", err)
	}
	if got != protocol.Capability_All {
		t.Fatalf("parseCaps(whitespace) = %#x, want %#x (Capability_All)", got, protocol.Capability_All)
	}

	// "spawn,file_read" → OR of the two bits
	got, err = parseCaps("spawn,file_read")
	if err != nil {
		t.Fatalf("parseCaps(\"spawn,file_read\") error: %v", err)
	}
	want := protocol.Capability_Spawn | protocol.Capability_FileRead
	if got != want {
		t.Fatalf("parseCaps(\"spawn,file_read\") = %#x, want %#x", got, want)
	}

	// Single cap
	got, err = parseCaps("cancel")
	if err != nil {
		t.Fatalf("parseCaps(\"cancel\") error: %v", err)
	}
	if got != protocol.Capability_Cancel {
		t.Fatalf("parseCaps(\"cancel\") = %#x, want %#x", got, protocol.Capability_Cancel)
	}

	// "all" → Capability_All
	got, err = parseCaps("all")
	if err != nil {
		t.Fatalf("parseCaps(\"all\") error: %v", err)
	}
	if got != protocol.Capability_All {
		t.Fatalf("parseCaps(\"all\") = %#x, want %#x", got, protocol.Capability_All)
	}

	// Unknown → error
	if _, err := parseCaps("bogus"); err == nil {
		t.Fatal("parseCaps(\"bogus\"): expected error, got nil")
	}

	// Unknown mixed with valid → error
	if _, err := parseCaps("spawn,bogus"); err == nil {
		t.Fatal("parseCaps(\"spawn,bogus\"): expected error, got nil")
	}
}
