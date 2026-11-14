//go:build unix

package transcript

import "syscall"

// noFollow makes the last path component's symlink an error.
const noFollow = syscall.O_NOFOLLOW
