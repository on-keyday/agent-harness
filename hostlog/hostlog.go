// Package hostlog manages process-level log output and snapshot reads for
// harness-server and agent-runner (pillar C,
// docs/superpowers/specs/2026-11-28-pillar-c-host-logs-design.md).
//
// Two modes (C-D1):
//   - File mode (--log-file <path>): slog writes (append, 0600) to that file
//     instead of stderr; Read serves tail or byte-range snapshots from the file.
//   - Ring mode (--log-file unset): slog writes to os.Stderr mirrored into an
//     in-process 1 MiB circular buffer; Read serves snapshots from the ring.
package hostlog

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

const (
	// DefaultMaxBytes is used when a request leaves max_bytes at 0 (C-D3).
	DefaultMaxBytes = 64 << 10 // 64 KiB
	// MaxBytesCap is the hard ceiling on bytes returned per snapshot (C-D3).
	MaxBytesCap = 4 << 20 // 4 MiB
	// DefaultRingSize is the capacity of the in-process fallback ring buffer (C-D1).
	DefaultRingSize = 1 << 20 // 1 MiB
)

// NormalizeMaxBytes applies the default and upper cap to a request's max_bytes.
func NormalizeMaxBytes(n uint32) int {
	if n == 0 {
		return DefaultMaxBytes
	}
	if int(n) > MaxBytesCap {
		return MaxBytesCap
	}
	return int(n)
}

// Snapshot is one tail or byte-range read from a host log source.
type Snapshot struct {
	Source      protocol.HostLogSource
	TotalSize   uint64
	StartOffset uint64
	Data        []byte
}

// Ring is a thread-safe circular byte buffer that tracks total bytes ever
// written so callers have monotonic byte offsets across wraps.
type Ring struct {
	mu    sync.Mutex
	buf   []byte
	cap   int
	write int    // next write index in [0, cap)
	total uint64 // total bytes written since creation
}

// NewRing allocates a Ring with capacity size bytes (defaults to DefaultRingSize if <= 0).
func NewRing(size int) *Ring {
	if size <= 0 {
		size = DefaultRingSize
	}
	return &Ring{
		buf: make([]byte, size),
		cap: size,
	}
}

// Write appends p to the ring, overwriting the oldest bytes if full.
func (r *Ring) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	origLen := len(p)
	r.total += uint64(origLen)

	// If p is larger than the entire ring, only keep the trailing cap bytes.
	if len(p) >= r.cap {
		copy(r.buf, p[len(p)-r.cap:])
		r.write = 0
		return origLen, nil
	}

	first := r.cap - r.write
	if first >= len(p) {
		copy(r.buf[r.write:], p)
		r.write = (r.write + len(p)) % r.cap
	} else {
		copy(r.buf[r.write:], p[:first])
		rem := len(p) - first
		copy(r.buf[0:], p[first:])
		r.write = rem
	}
	return origLen, nil
}

// ReadSnapshot returns a tail or byte-range snapshot from the ring.
func (r *Ring) ReadSnapshot(mode protocol.HostLogMode, offset uint64, maxBytes uint32) Snapshot {
	limit := NormalizeMaxBytes(maxBytes)

	r.mu.Lock()
	defer r.mu.Unlock()

	total := r.total
	retained := total
	if retained > uint64(r.cap) {
		retained = uint64(r.cap)
	}
	earliest := total - retained

	var start uint64
	switch mode {
	case protocol.HostLogMode_Range:
		start = offset
		if start < earliest {
			start = earliest
		}
		if start >= total {
			return Snapshot{
				Source:      protocol.HostLogSource_Ring,
				TotalSize:   total,
				StartOffset: total,
			}
		}
	default: // HostLogMode_Tail
		if retained <= uint64(limit) {
			start = earliest
		} else {
			start = total - uint64(limit)
		}
	}

	avail := total - start
	n := int(avail)
	if n > limit {
		n = limit
	}
	out := make([]byte, n)
	if n == 0 {
		return Snapshot{
			Source:      protocol.HostLogSource_Ring,
			TotalSize:   total,
			StartOffset: start,
		}
	}

	// Map logical offset `start` into the circular buffer.
	// When total <= cap, logical byte 0 sits at index 0.
	// When total > cap, logical byte `earliest` sits at index `r.write`.
	var ringStart int
	if total <= uint64(r.cap) {
		ringStart = int(start)
	} else {
		ringStart = (r.write + int(start-earliest)) % r.cap
	}
	first := r.cap - ringStart
	if first >= n {
		copy(out, r.buf[ringStart:ringStart+n])
	} else {
		copy(out, r.buf[ringStart:])
		copy(out[first:], r.buf[:n-first])
	}

	return Snapshot{
		Source:      protocol.HostLogSource_Ring,
		TotalSize:   total,
		StartOffset: start,
		Data:        out,
	}
}

// Reader serves process log snapshots from either a file or an in-process Ring.
type Reader struct {
	filePath string
	ring     *Ring
}

// NewFileReader creates a Reader backed by a log file on disk.
func NewFileReader(path string) *Reader {
	return &Reader{filePath: path}
}

// NewRingReader creates a Reader backed by an in-process Ring.
func NewRingReader(ring *Ring) *Reader {
	return &Reader{ring: ring}
}

// Read returns a tail or byte-range snapshot from the configured source.
func (r *Reader) Read(mode protocol.HostLogMode, offset uint64, maxBytes uint32) (Snapshot, error) {
	if r == nil {
		return Snapshot{}, fmt.Errorf("host log reader not configured")
	}
	if r.filePath != "" {
		return readFileSnapshot(r.filePath, mode, offset, maxBytes)
	}
	if r.ring != nil {
		return r.ring.ReadSnapshot(mode, offset, maxBytes), nil
	}
	return Snapshot{}, fmt.Errorf("host log source unavailable")
}

func readFileSnapshot(path string, mode protocol.HostLogMode, offset uint64, maxBytes uint32) (Snapshot, error) {
	limit := NormalizeMaxBytes(maxBytes)
	f, err := os.Open(path)
	if err != nil {
		return Snapshot{}, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return Snapshot{}, err
	}
	total := uint64(fi.Size())

	var start uint64
	switch mode {
	case protocol.HostLogMode_Range:
		if offset >= total {
			return Snapshot{
				Source:      protocol.HostLogSource_File,
				TotalSize:   total,
				StartOffset: total,
			}, nil
		}
		start = offset
	default: // HostLogMode_Tail
		if total <= uint64(limit) {
			start = 0
		} else {
			start = total - uint64(limit)
		}
	}

	toRead := total - start
	if toRead > uint64(limit) {
		toRead = uint64(limit)
	}
	buf := make([]byte, int(toRead))
	if len(buf) > 0 {
		n, err := f.ReadAt(buf, int64(start))
		if err != nil && err != io.EOF {
			return Snapshot{}, err
		}
		buf = buf[:n]
	}
	return Snapshot{
		Source:      protocol.HostLogSource_File,
		TotalSize:   total,
		StartOffset: start,
		Data:        buf,
	}, nil
}

// Setup configures slog.Default() and returns a Reader for serving host logs.
//
// When logFilePath != "", slog writes (append, 0600) to logFilePath instead of
// stderr (C-D1) and the Reader reads from logFilePath.
// When logFilePath == "", slog writes to os.Stderr mirrored into a 1 MiB
// in-process Ring and the Reader reads from that Ring.
func Setup(logFilePath string) (*Reader, func() error, error) {
	if logFilePath != "" {
		if dir := filepath.Dir(logFilePath); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, nil, fmt.Errorf("hostlog: mkdir %s: %w", dir, err)
			}
		}
		f, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, nil, fmt.Errorf("hostlog: open %s: %w", logFilePath, err)
		}
		slog.SetDefault(slog.New(slog.NewTextHandler(f, nil)))
		return NewFileReader(logFilePath), f.Close, nil
	}

	ring := NewRing(DefaultRingSize)
	w := io.MultiWriter(os.Stderr, ring)
	slog.SetDefault(slog.New(slog.NewTextHandler(w, nil)))
	return NewRingReader(ring), func() error { return nil }, nil
}
