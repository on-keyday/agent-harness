package cli

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TestCapsLabel verifies the three output forms of CapsLabel.
func TestCapsLabel(t *testing.T) {
	if got := CapsLabel(protocol.Capability_All); got != "all" {
		t.Fatalf("all=%q", got)
	}
	if got := CapsLabel(protocol.Capability_None); got != "none" {
		t.Fatalf("none=%q", got)
	}
	if got := CapsLabel(protocol.Capability_Spawn | protocol.Capability_FileRead); got != "spawn,file_read" {
		t.Fatalf("got %q", got)
	}
}

// TestParseCaps verifies the three behaviours of ParseCaps:
//  1. Empty input → Capability_All (inherit-all default).
//  2. Comma-separated valid names → correct OR mask.
//  3. Unknown name → error (not a silent no-op or panic).
func TestParseCaps(t *testing.T) {
	// Empty → All
	got, err := ParseCaps("")
	if err != nil {
		t.Fatalf("ParseCaps(\"\") error: %v", err)
	}
	if got != protocol.Capability_All {
		t.Fatalf("ParseCaps(\"\") = %#x, want %#x (Capability_All)", got, protocol.Capability_All)
	}

	// Whitespace-only → All
	got, err = ParseCaps("   ")
	if err != nil {
		t.Fatalf("ParseCaps(whitespace) error: %v", err)
	}
	if got != protocol.Capability_All {
		t.Fatalf("ParseCaps(whitespace) = %#x, want %#x (Capability_All)", got, protocol.Capability_All)
	}

	// "spawn,file_read" → OR of the two bits
	got, err = ParseCaps("spawn,file_read")
	if err != nil {
		t.Fatalf("ParseCaps(\"spawn,file_read\") error: %v", err)
	}
	want := protocol.Capability_Spawn | protocol.Capability_FileRead
	if got != want {
		t.Fatalf("ParseCaps(\"spawn,file_read\") = %#x, want %#x", got, want)
	}

	// Single cap
	got, err = ParseCaps("cancel")
	if err != nil {
		t.Fatalf("ParseCaps(\"cancel\") error: %v", err)
	}
	if got != protocol.Capability_Cancel {
		t.Fatalf("ParseCaps(\"cancel\") = %#x, want %#x", got, protocol.Capability_Cancel)
	}

	// "all" → Capability_All
	got, err = ParseCaps("all")
	if err != nil {
		t.Fatalf("ParseCaps(\"all\") error: %v", err)
	}
	if got != protocol.Capability_All {
		t.Fatalf("ParseCaps(\"all\") = %#x, want %#x", got, protocol.Capability_All)
	}

	// Unknown → error
	if _, err := ParseCaps("bogus"); err == nil {
		t.Fatal("ParseCaps(\"bogus\"): expected error, got nil")
	}

	// Unknown mixed with valid → error
	if _, err := ParseCaps("spawn,bogus"); err == nil {
		t.Fatal("ParseCaps(\"spawn,bogus\"): expected error, got nil")
	}
}

// TestParseCaps_Subtractive covers the "-name" terms: order independence, the
// requirement for a positive base, and the interaction with an explicit list.
func TestParseCaps_Subtractive(t *testing.T) {
	all, err := ParseCaps("all")
	if err != nil {
		t.Fatalf("ParseCaps(all): %v", err)
	}

	got, err := ParseCaps("all,-spawn")
	if err != nil {
		t.Fatalf("ParseCaps(all,-spawn): %v", err)
	}
	if got&protocol.Capability_Spawn != 0 {
		t.Errorf("all,-spawn still grants spawn: %#x", got)
	}
	if want := all &^ protocol.Capability_Spawn; got != want {
		t.Errorf("all,-spawn = %#x, want %#x", got, want)
	}

	// Order must not matter — the two-pass rule.
	rev, err := ParseCaps("-spawn,all")
	if err != nil {
		t.Fatalf("ParseCaps(-spawn,all): %v", err)
	}
	if rev != got {
		t.Errorf("-spawn,all = %#x, want %#x (same as all,-spawn)", rev, got)
	}

	// Subtracting from an explicit list, not just from all.
	got, err = ParseCaps("spawn,file_read,-spawn")
	if err != nil {
		t.Fatalf("ParseCaps(spawn,file_read,-spawn): %v", err)
	}
	if got != protocol.Capability_FileRead {
		t.Errorf("spawn,file_read,-spawn = %#x, want %#x", got, protocol.Capability_FileRead)
	}

	// A negative with no base is rejected rather than assumed to mean "all".
	if _, err := ParseCaps("-spawn"); err == nil {
		t.Error("expected an error for a negatives-only list")
	}

	// Unknown names still error, prefixed or not.
	if _, err := ParseCaps("all,-nosuchcap"); err == nil {
		t.Error("expected an error for an unknown negated capability")
	}
}
