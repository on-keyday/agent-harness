package agent

import (
	"errors"
	"fmt"
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

// LoadCursor returns (cursor, prev, err). cursor is the live ack position
// that hooks advance with --commit; prev is the snapshot of cursor taken
// just before the most recent advance, used as the read position for
// peek-only `inbox --since-last` so the LLM-visible inbox stays consistent
// with what the hook already delivered to its prompt context.
//
// Backward compatible: a legacy single-line file (cursor only) loads as
// (cursor=N, prev=0). A missing file loads as (0, 0).
func LoadCursor(taskIDHex string) (uint64, uint64, error) {
	p, err := cursorPath(taskIDHex)
	if err != nil {
		return 0, 0, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	cursor, err := strconv.ParseUint(strings.TrimSpace(lines[0]), 10, 64)
	if err != nil {
		return 0, 0, err
	}
	var prev uint64
	if len(lines) >= 2 {
		prev, err = strconv.ParseUint(strings.TrimSpace(lines[1]), 10, 64)
		if err != nil {
			return 0, 0, err
		}
	}
	return cursor, prev, nil
}

// SaveCursor writes (cursor, prev) as two decimal lines. Callers that
// advance the cursor should pass the OLD cursor as prev so a subsequent
// peek-only read returns the just-advanced-past batch.
func SaveCursor(taskIDHex string, cursor uint64, prev uint64) error {
	p, err := cursorPath(taskIDHex)
	if err != nil {
		return err
	}
	return os.WriteFile(p, []byte(fmt.Sprintf("%d\n%d\n", cursor, prev)), 0o644)
}
