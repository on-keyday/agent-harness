package verb_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestGeneratedFileIsCurrent regenerates into a temp dir and diffs against the
// committed actions_gen.go.
//
// A stale generated file is the failure mode a generator introduces in place
// of the one it removes: the declaration and the Action agree by construction
// only while the output is current, and editing table.go does not by itself
// force a regeneration. This is what makes `go generate` non-optional -- the
// same shape as this repo committing `make protoregen`'s output.
func TestGeneratedFileIsCurrent(t *testing.T) {
	committed, err := os.ReadFile("actions_gen.go")
	if err != nil {
		t.Fatalf("actions_gen.go is missing -- run `go generate ./cli/verb`: %v", err)
	}
	fresh := filepath.Join(t.TempDir(), "actions_gen.go")
	if out, err := exec.Command("go", "run", "./gen", "-o", fresh).CombinedOutput(); err != nil {
		t.Fatalf("go run ./gen: %v\n%s", err, out)
	}
	got, err := os.ReadFile(fresh)
	if err != nil {
		t.Fatalf("read regenerated: %v", err)
	}
	if string(got) != string(committed) {
		t.Errorf("actions_gen.go is stale: table.go has changed since it was generated.\n" +
			"Run `go generate ./cli/verb` and commit the result. The struct fields and the\n" +
			"assignments that fill them come from one source only while that output is current.")
	}
}
