package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// DefaultPath is the config the clients look for when neither --config nor
// HARNESS_CONFIG names one: .harness/config under the current directory. The
// search does not walk up to parent directories — a client started somewhere
// else names its file explicitly.
const DefaultPath = ".harness/config"

// Load resolves and parses the config for a client process. The returned path
// is where it came from, for a status line. A nil *File with a nil error means
// no workspace applies, which is the ordinary case.
func Load(flagPath string) (*File, string, error) {
	return LoadFrom(flagPath, os.Getenv("HARNESS_CONFIG"), filepath.FromSlash(DefaultPath),
		os.Getenv("HARNESS_AUTH_TICKET") != "")
}

// LoadFrom is Load with its three sources and the agent test injected, so the
// resolution order is testable without touching the process environment or the
// working directory.
//
// inAgent suppresses everything. An in-task agent has HARNESS_AUTH_TICKET set,
// and scripts/sandbox/agent-in-podman.sh forwards environment into the
// container BY PREFIX, so a HARNESS_CONFIG left in a runner's environment
// otherwise reaches every sandboxed agent — pointing at a path that in the
// container either does not exist or is a different operator's file.
func LoadFrom(flagPath, envPath, defaultPath string, inAgent bool) (*File, string, error) {
	if inAgent {
		return nil, "", nil
	}
	switch {
	case flagPath != "":
		f, err := parseFile(flagPath)
		return f, flagPath, err
	case envPath != "":
		f, err := parseFile(envPath)
		return f, envPath, err
	case defaultPath != "":
		f, err := parseFile(defaultPath)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, "", nil // running without a workspace is normal
		}
		return f, defaultPath, err
	}
	return nil, "", nil
}

// parseFile opens and parses one path. The os.Open error is returned unwrapped
// so LoadFrom's default-path branch can test it with errors.Is(fs.ErrNotExist);
// a file that exists but does not parse is wrapped with its path and reported.
func parseFile(path string) (*File, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	f, err := Parse(fh)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}
