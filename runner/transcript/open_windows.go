//go:build windows

package transcript

// noFollow: Windows has no O_NOFOLLOW; os.Root keeps the open inside the
// store, and openRegular's Lstat/SameFile check refuses a swapped file.
const noFollow = 0
