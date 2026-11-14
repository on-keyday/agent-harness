//go:build !unix

package transcript

// noFollow: Windows (and js/wasm, which only links this package through
// streamagent) has no O_NOFOLLOW; os.Root keeps the open inside the
// store, and openRegular's Lstat/SameFile check refuses a swapped file.
const noFollow = 0
