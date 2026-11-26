package verb

import (
	"strings"
	"testing"
)

func bindHex(b byte) string { return strings.Repeat(string("0123456789abcdef"[b%16]), 32) }

func TestParseBindFlag(t *testing.T) {
	var ids []string
	for i := 0; i < 17; i++ {
		ids = append(ids, strings.Repeat("0", 30)+string("0123456789abcdef"[i/16])+string("0123456789abcdef"[i%16]))
	}
	got, err := ParseBindFlag(strings.Join(ids[:16], ","))
	if err != nil || len(got) != 16 {
		t.Fatalf("16 ids: %d, %v", len(got), err)
	}
	if _, err := ParseBindFlag(strings.Join(ids, ",")); err == nil || !strings.Contains(err.Error(), "16") {
		t.Errorf("17 ids: err=%v, want a limit error", err)
	}
	if _, err := ParseBindFlag("abcd"); err == nil {
		t.Error("a short id must be refused")
	}
	if _, err := ParseBindFlag(strings.Repeat("z", 32)); err == nil {
		t.Error("a non-hex id must be refused")
	}
	got, err = ParseBindFlag(bindHex(1) + "," + bindHex(1))
	if err != nil || len(got) != 1 {
		t.Errorf("a repeated id: %d ids, %v; want it deduped", len(got), err)
	}
	// --bind given with nothing in it is refused, not read as unbound: a
	// script's unset variable must not enroll the widest key.
	for _, empty := range []string{"", ",", " , "} {
		if _, err := ParseBindFlag(empty); err == nil {
			t.Errorf("--bind %q: accepted, want refused", empty)
		}
	}
}

func TestParseTaskIDListSorts(t *testing.T) {
	got, err := ParseTaskIDList(bindHex(2) + "," + bindHex(1))
	if err != nil || len(got) != 2 || got[0].Id[0] != 0x11 {
		t.Fatalf("ParseTaskIDList = %v, %v; want sorted", got, err)
	}
}
