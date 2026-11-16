package transcript

import (
	"os"
	"path/filepath"
	"runtime"
)

// Spool is a temp file that is gone once closed. It lives under the user
// cache dir, not the temp dir, which can be tmpfs (RAM); off Windows it is
// unlinked at once, so a runner that dies mid-transfer leaves nothing behind.
type Spool struct{ *os.File }

// NewSpool creates an empty Spool.
func NewSpool() (*Spool, error) {
	dir := os.TempDir()
	if c, err := os.UserCacheDir(); err == nil {
		d := filepath.Join(c, "harness-transcripts")
		if os.MkdirAll(d, 0o700) == nil {
			dir = d
		}
	}
	f, err := os.CreateTemp(dir, "spool-*")
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" {
		_ = os.Remove(f.Name())
	}
	return &Spool{f}, nil
}

// Close closes the file and removes it where it still exists (Windows).
func (s *Spool) Close() error {
	err := s.File.Close()
	_ = os.Remove(s.File.Name())
	return err
}
