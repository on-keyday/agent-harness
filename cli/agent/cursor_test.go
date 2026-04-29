package agent

import (
	"testing"
)

func TestCursor_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)
	if err := SaveCursor("abc123", 42); err != nil {
		t.Fatal(err)
	}
	got, err := LoadCursor("abc123")
	if err != nil {
		t.Fatal(err)
	}
	if got != 42 {
		t.Errorf("loaded cursor = %d, want 42", got)
	}
}

func TestCursor_LoadMissingReturnsZero(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)
	got, err := LoadCursor("nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Errorf("missing cursor = %d, want 0", got)
	}
}
