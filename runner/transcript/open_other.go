//go:build !unix

package transcript

// openFlags: nothing to add. Windows has no FIFO in the file system (named
// pipes live under \\.\pipe\), and js/wasm only links this package through
// streamagent; openRegular's Lstat/SameFile check still refuses anything but
// the regular file it saw.
const openFlags = 0
