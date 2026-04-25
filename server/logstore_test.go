package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLogStoreAppend(t *testing.T) {
	dir := t.TempDir()
	ls, err := NewLogStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := ls.Append("abc", []byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if err := ls.Append("abc", []byte("world\n")); err != nil {
		t.Fatal(err)
	}
	if err := ls.Append("xyz", []byte("solo\n")); err != nil {
		t.Fatal(err)
	}
	ls.Close()

	abc, _ := os.ReadFile(filepath.Join(dir, "abc.log"))
	if string(abc) != "hello\nworld\n" {
		t.Fatalf("abc=%q", abc)
	}
	xyz, _ := os.ReadFile(filepath.Join(dir, "xyz.log"))
	if string(xyz) != "solo\n" {
		t.Fatalf("xyz=%q", xyz)
	}
}

func TestLogStoreReopenAfterClose(t *testing.T) {
	dir := t.TempDir()
	ls, _ := NewLogStore(dir)
	ls.Append("abc", []byte("first\n"))
	ls.Close()
	// Re-append after Close — should reopen
	if err := ls.Append("abc", []byte("second\n")); err != nil {
		t.Fatal(err)
	}
	ls.Close()
	got, _ := os.ReadFile(filepath.Join(dir, "abc.log"))
	if string(got) != "first\nsecond\n" {
		t.Fatalf("got=%q", got)
	}
}
