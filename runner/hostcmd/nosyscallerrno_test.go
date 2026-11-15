package hostcmd_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// On Windows, Go's POSIX errno names (syscall.EMSGSIZE, ECONNRESET, ENOTEMPTY,
// ...) are synthetic values that no socket or file call ever returns: Winsock
// answers with WSAExxx and the file APIs with ERROR_xxx. So
// errors.Is(err, syscall.EMSGSIZE) compiles everywhere, passes every Linux
// test, and is always false on a Windows runner. It bit twice: objtrsf's
// PLPMTUD too-big guard (every UDP connect from Windows dropped) and file
// delete on a non-empty directory (IoError instead of NotEmpty).
//
// The fix both times was a predicate split by OS (a _windows.go / _other.go
// pair, or a //go:build line). This fails on a syscall.Exxx in a file that has
// no such split. A deliberate use spells `//nolint:syscallerrno` with a reason
// on the same line.
var syscallErrno = regexp.MustCompile(`\bsyscall\.E[A-Z0-9]+\b`)

var osSplitName = regexp.MustCompile(`_(windows|linux|darwin|freebsd|openbsd|netbsd|unix|js|wasm|plan9|solaris|other|posix)\.go$`)

func TestNoPosixErrnoOutsideAnOSSplit(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || osSplitName.MatchString(path) {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src := string(b)
		if strings.Contains(src, "\n//go:build ") || strings.HasPrefix(src, "//go:build ") {
			return nil
		}
		for i, line := range strings.Split(src, "\n") {
			if m := syscallErrno.FindString(line); m != "" && !strings.Contains(line, "//nolint:syscallerrno") {
				t.Errorf("%s:%d uses %s in a file built for every OS; on Windows that value is "+
					"never returned (Winsock says WSAExxx). Split the check by OS "+
					"(_windows.go / _other.go), or mark the line //nolint:syscallerrno with a reason",
					path, i+1, m)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
