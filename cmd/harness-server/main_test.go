package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The operator secret is required by default: handed nothing, the server
// generates one under --data-dir instead of falling back to the connect PSK,
// and reads that same file back on the next start.
func TestResolveOperatorPSK_GeneratesUnderDataDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data") // absent: the resolver must create it
	got, err := resolveOperatorPSK("", "", dir, false)
	if err != nil {
		t.Fatalf("resolveOperatorPSK: %v", err)
	}
	if len(got) != 64 {
		t.Fatalf("generated secret is %d bytes, want 64 hex chars", len(got))
	}
	path := filepath.Join(dir, operatorPSKFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("generated file: %v", err)
	}
	if strings.TrimSpace(string(data)) != string(got) {
		t.Fatalf("file %q holds %q, resolver returned %q", path, data, got)
	}
	again, err := resolveOperatorPSK("", "", dir, false)
	if err != nil || string(again) != string(got) {
		t.Fatalf("second resolve = %q, %v; want the persisted %q", again, err, got)
	}
}

func TestResolveOperatorPSK_PermitNoneYieldsNothing(t *testing.T) {
	dir := t.TempDir()
	got, err := resolveOperatorPSK("", "", dir, true)
	if err != nil || got != nil {
		t.Fatalf("permitNone: got %q, %v; want nil, nil", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, operatorPSKFileName)); !os.IsNotExist(err) {
		t.Fatalf("permitNone must not generate a file; stat err = %v", err)
	}
}

func TestResolveOperatorPSK_NoDataDirIsAnError(t *testing.T) {
	if got, err := resolveOperatorPSK("", "", "", false); err == nil {
		t.Fatalf("no value, no file, no data-dir: want an error, got %q", got)
	}
}

func TestResolveOperatorPSK_ExplicitValueWins(t *testing.T) {
	dir := t.TempDir()
	for _, permit := range []bool{false, true} {
		got, err := resolveOperatorPSK("secret", "", dir, permit)
		if err != nil || string(got) != "secret" {
			t.Fatalf("permit=%v: got %q, %v; want the explicit value", permit, got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, operatorPSKFileName)); !os.IsNotExist(err) {
		t.Fatalf("an explicit value must not generate a file; stat err = %v", err)
	}
}

func TestResolveOperatorPSK_NamedFileIsNotRedirectedToDataDir(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "elsewhere", "op.psk")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := resolveOperatorPSK("", file, dir, false)
	if err != nil || len(got) == 0 {
		t.Fatalf("named file: got %q, %v", got, err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("named file was not generated: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, operatorPSKFileName)); !os.IsNotExist(err) {
		t.Fatalf("the data-dir default must be untouched when a file is named; stat err = %v", err)
	}
}
