package runner

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/on-keyday/agent-harness/cli"
)

// DefaultRunnerKeyPath is <home>/.config/harness/runner-key on every OS —
// beside the other per-user files this repo keeps there (server-key.pub,
// sandbox-claude-token). It deliberately does not follow XDG_CONFIG_HOME
// (os.UserConfigDir): a runner started by a systemd user unit and an agent's
// shell running --print-pubkey can disagree about that variable, and then the
// key that was enrolled is not the key the runner presents.
func DefaultRunnerKeyPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("runner key: no home directory: %w", err)
	}
	return filepath.Join(home, ".config", "harness", "runner-key"), nil
}

// LoadOrCreateRunnerKey reads the runner's Ed25519 key (a hex seed, the
// server-key format) and creates it when the file is absent. A file that does
// not parse is an error and is left as it is: regenerating it would orphan
// the enrollment of the key it held.
func LoadOrCreateRunnerKey(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		key, perr := cli.ParseServerKeySeed(string(data))
		if perr != nil {
			return nil, fmt.Errorf("runner key %s does not parse (move it aside to make a new key, which then needs enrolling): %w", path, perr)
		}
		return key, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("runner key %s: %w", path, err)
	}
	return writeNewRunnerKey(path)
}

// RotateRunnerKey moves the current key file to <path>.prev (replacing an
// older .prev) and writes a new key. A runner already running keeps the key it
// read at startup; the new one takes effect at its next start, once enrolled.
func RotateRunnerKey(path string) (ed25519.PrivateKey, error) {
	if _, err := LoadOrCreateRunnerKey(path); err != nil {
		return nil, err
	}
	if err := os.Rename(path, path+".prev"); err != nil {
		return nil, fmt.Errorf("runner key: keep the old key as %s.prev: %w", path, err)
	}
	return writeNewRunnerKey(path)
}

func writeNewRunnerKey(path string) (ed25519.PrivateKey, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("runner key: %w", err)
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, fmt.Errorf("runner key: %w", err)
	}
	// Written whole to a temp file, then linked into place. Every slot of a
	// machine reads this one file, and a fleet restart starts them together:
	// creating the path directly let a slot that lost the race read it before
	// the winner had written it and exit on "does not parse". A link either
	// places a complete file or fails because one is already there.
	tmp, err := os.CreateTemp(dir, ".runner-key-*")
	if err != nil {
		return nil, fmt.Errorf("runner key: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		tmp.Close()
		return nil, fmt.Errorf("runner key: %w", err)
	}
	if _, err := tmp.WriteString(hex.EncodeToString(seed) + "\n"); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("runner key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("runner key: %w", err)
	}
	if err := os.Link(tmpName, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return LoadOrCreateRunnerKey(path) // another slot won; its file is whole
		}
		return nil, fmt.Errorf("runner key: %w", err)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}
