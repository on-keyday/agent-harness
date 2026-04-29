package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCursor_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)
	if err := SaveCursor("abc123", 42, 30); err != nil {
		t.Fatal(err)
	}
	cursor, prev, err := LoadCursor("abc123")
	if err != nil {
		t.Fatal(err)
	}
	if cursor != 42 || prev != 30 {
		t.Errorf("loaded = (%d, %d), want (42, 30)", cursor, prev)
	}
}

func TestCursor_LoadMissingReturnsZero(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)
	cursor, prev, err := LoadCursor("nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if cursor != 0 || prev != 0 {
		t.Errorf("missing = (%d, %d), want (0, 0)", cursor, prev)
	}
}

// TestCursor_LegacySingleLineFile verifies backward compatibility with
// pre-prev-cursor files containing only the live cursor.
func TestCursor_LegacySingleLineFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)
	p, err := cursorPath("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p), []byte("99"), 0o644); err != nil {
		t.Fatal(err)
	}
	cursor, prev, err := LoadCursor("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if cursor != 99 || prev != 0 {
		t.Errorf("legacy = (%d, %d), want (99, 0)", cursor, prev)
	}
}
