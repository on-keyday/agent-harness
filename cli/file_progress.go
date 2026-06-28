package cli

import (
	"io"
	"time"
)

// ProgressFunc reports transfer progress: transferred = bytes moved so far,
// total = the full size when known or 0 when unknown (e.g. a directory tar
// whose size isn't announced upfront). It is called once with (0, total) at
// the start, then at most every ~100ms, then a final time with the complete
// count — so a UI can show 0%→…→100% and end exact. Keep the callback cheap:
// for the WebUI wasm bridge it crosses into JS, and calling it per 32KB chunk
// would starve the single JS event loop (see the trsf busy-spin freeze).
type ProgressFunc func(transferred, total uint64)

// copyWithProgress is io.Copy with throttled progress reporting. With a nil
// onProgress it is exactly io.Copy. The 100ms throttle bounds JS-bridge calls
// regardless of transfer speed; the start and end are always reported so the
// bar appears immediately and settles on the true total.
func copyWithProgress(dst io.Writer, src io.Reader, total uint64, onProgress ProgressFunc) (uint64, error) {
	if onProgress == nil {
		n, err := io.Copy(dst, src)
		return uint64(n), err
	}
	buf := make([]byte, 64*1024)
	var transferred uint64
	var last time.Time
	onProgress(0, total)
	for {
		nr, er := src.Read(buf)
		if nr > 0 {
			nw, ew := dst.Write(buf[:nr])
			transferred += uint64(nw)
			if ew != nil {
				return transferred, ew
			}
			if nw < nr {
				return transferred, io.ErrShortWrite
			}
			if now := time.Now(); now.Sub(last) >= 100*time.Millisecond {
				onProgress(transferred, total)
				last = now
			}
		}
		if er == io.EOF {
			break
		}
		if er != nil {
			return transferred, er
		}
	}
	onProgress(transferred, total)
	return transferred, nil
}
