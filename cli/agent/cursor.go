package agent

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func cursorPath(taskIDHex string) (string, error) {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".cache")
	}
	dir := filepath.Join(base, "harness")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, "agent-cursor-"+taskIDHex), nil
}

// LoadCursor returns 0 when no cursor file yet.
func LoadCursor(taskIDHex string) (uint64, error) {
	p, err := cursorPath(taskIDHex)
	if err != nil {
		return 0, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, err
	}
	return v, nil
}

// SaveCursor writes the cursor as decimal to the per-task cursor file.
func SaveCursor(taskIDHex string, cursor uint64) error {
	p, err := cursorPath(taskIDHex)
	if err != nil {
		return err
	}
	return os.WriteFile(p, []byte(strconv.FormatUint(cursor, 10)), 0o644)
}
