//go:build unix

package transcript

import "syscall"

// openFlags: O_NONBLOCK so a FIFO swapped in after the Lstat opens at once
// instead of waiting for a writer; openRegular's Stat then refuses it. It
// has no effect on reading a regular file.
const openFlags = syscall.O_NONBLOCK
