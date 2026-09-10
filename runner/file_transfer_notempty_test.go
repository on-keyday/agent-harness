package runner

import (
	"os"
	"path/filepath"
	"testing"
)

// The error comes from a REAL os.Remove on a real non-empty directory, not
// from a syscall.Errno this test built itself. That is the whole point: the
// bug being guarded was that the value the OS actually returns is not the
// constant the code compared against, so a test that constructs the constant
// passes on both platforms while the product is broken on one.
//
// Before the OS split this failed on Windows only: os.Remove returns
// ERROR_DIR_NOT_EMPTY (145) there, syscall.ENOTEMPTY is a synthetic
// APPLICATION_ERROR value, and syscall.Errno.Is maps 145 to os.ErrExist and
// never to ENOTEMPTY. `file delete` on a non-empty directory therefore
// answered IoError instead of NotEmpty — the operation failed either way, but
// the operator was told the wrong reason.
func TestIsDirNotEmptyMatchesWhatTheOSActuallyReturns(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "full")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "child"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := os.Remove(dir)
	if err == nil {
		t.Fatal("removing a non-empty directory succeeded")
	}
	if !isDirNotEmpty(err) {
		t.Errorf("isDirNotEmpty(%#v) = false — the delete path will report a "+
			"generic IoError instead of NotEmpty on this OS", err)
	}
}

// And it must not swallow every removal failure: a missing path is a
// different answer to the caller (NotFound), decided before this predicate.
func TestIsDirNotEmptyIsNotJustAnyRemoveError(t *testing.T) {
	err := os.Remove(filepath.Join(t.TempDir(), "nope"))
	if err == nil {
		t.Fatal("removing a missing path succeeded")
	}
	if isDirNotEmpty(err) {
		t.Errorf("isDirNotEmpty(%#v) = true for a missing path", err)
	}
}
