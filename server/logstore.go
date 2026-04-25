package server

import (
	"os"
	"path/filepath"
	"sync"
)

// LogStore appends raw bytes to per-task log files under dir. Files are kept open
// for the lifetime of the LogStore; call Close when shutting down.
type LogStore struct {
	mu    sync.Mutex
	dir   string
	files map[string]*os.File
}

func NewLogStore(dir string) (*LogStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &LogStore{dir: dir, files: map[string]*os.File{}}, nil
}

// Append writes data to <dir>/<taskID>.log, opening lazily on first append.
// Returns any I/O error.
func (l *LogStore) Append(taskID string, data []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, ok := l.files[taskID]
	if !ok {
		var err error
		f, err = os.OpenFile(filepath.Join(l.dir, taskID+".log"),
			os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		l.files[taskID] = f
	}
	_, err := f.Write(data)
	return err
}

// Close closes all open log files. After Close, Append will reopen on demand.
func (l *LogStore) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, f := range l.files {
		f.Close()
	}
	l.files = map[string]*os.File{}
}
