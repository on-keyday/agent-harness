//go:build !windows

package runner

import (
	"errors"
	"syscall"
)

// isDirNotEmpty reports whether err is the OS saying a directory could not be
// removed because something is still in it.
//
// Split by OS because `errors.Is(err, syscall.ENOTEMPTY)` alone is false on
// Windows and there is nothing to see: it compiles, it passes here, and the
// only symptom is an operator on the other platform being told the wrong
// reason. Go defines the POSIX errno NAMES on Windows in a synthetic
// APPLICATION_ERROR block, while the filesystem actually returns
// ERROR_DIR_NOT_EMPTY (145) — and syscall.Errno.Is maps that to os.ErrExist,
// never to syscall.ENOTEMPTY. Same shape as objtrsf's isMessageTooBig, which
// exists because EMSGSIZE bit exactly this way.
func isDirNotEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY)
}
