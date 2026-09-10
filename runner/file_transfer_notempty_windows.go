package runner

import (
	"errors"
	"syscall"
)

// isDirNotEmpty is the Windows arm. See the !windows file for why this is
// split at all.
//
// ERROR_DIR_NOT_EMPTY is what the filesystem returns; syscall.ENOTEMPTY is
// kept in the test because a future Go could start mapping the two, and this
// should keep working rather than start matching twice.
func isDirNotEmpty(err error) bool {
	return errors.Is(err, syscall.ERROR_DIR_NOT_EMPTY) || errors.Is(err, syscall.ENOTEMPTY)
}
