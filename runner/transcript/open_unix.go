//go:build !windows

package transcript

import "syscall"

// noFollow makes the last path component's symlink an error.
const noFollow = syscall.O_NOFOLLOW
