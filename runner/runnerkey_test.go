package runner

import (
	"bytes"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func TestLoadOrCreateRunnerKeyCreatesThenReuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harness", "runner-key")
	first, err := LoadOrCreateRunnerKey(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateRunnerKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Public().(ed25519.PublicKey), second.Public().(ed25519.PublicKey)) {
		t.Fatal("the second load made a new key instead of reading the file")
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("key file mode = %v, want 0600", st.Mode().Perm())
		}
	}
}

func TestLoadOrCreateRunnerKeyRefusesCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner-key")
	if err := os.WriteFile(path, []byte("zz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateRunnerKey(path); err == nil {
		t.Fatal("a corrupt key file loaded; it must be an error")
	}
	got, _ := os.ReadFile(path)
	if string(got) != "zz\n" {
		t.Fatalf("the corrupt file was rewritten to %q; it must be left for the operator", got)
	}
}

func TestRotateRunnerKeyKeepsPrev(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner-key")
	old, err := LoadOrCreateRunnerKey(path)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := RotateRunnerKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(old.Public().(ed25519.PublicKey), rotated.Public().(ed25519.PublicKey)) {
		t.Fatal("rotation kept the same key")
	}
	prev, err := LoadOrCreateRunnerKey(path + ".prev")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(prev.Public().(ed25519.PublicKey), old.Public().(ed25519.PublicKey)) {
		t.Fatal(".prev does not hold the old key")
	}
	again, err := LoadOrCreateRunnerKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again.Public().(ed25519.PublicKey), rotated.Public().(ed25519.PublicKey)) {
		t.Fatal("the key file does not hold the rotated key")
	}
}

// A systemd user unit and an agent's shell can disagree about XDG_CONFIG_HOME;
// the key they resolve must not.
func TestDefaultRunnerKeyPathIgnoresXDG(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	got, err := DefaultRunnerKeyPath()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".config", "harness", "runner-key")
	if got != want {
		t.Fatalf("DefaultRunnerKeyPath() = %q, want %q", got, want)
	}
}

// Every slot on a machine reads one key file, and a fleet restart starts them
// together the first time it exists. Each must come up with the same key; a
// loser of the create race must never read a half-written file.
func TestLoadOrCreateRunnerKeyConcurrentFirstStart(t *testing.T) {
	for round := 0; round < 20; round++ {
		path := filepath.Join(t.TempDir(), "runner-key")
		const n = 16
		keys := make([]ed25519.PrivateKey, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				keys[i], errs[i] = LoadOrCreateRunnerKey(path)
			}(i)
		}
		close(start)
		wg.Wait()
		for i := 0; i < n; i++ {
			if errs[i] != nil {
				t.Fatalf("round %d: slot %d failed: %v", round, i, errs[i])
			}
			if !bytes.Equal(keys[i].Public().(ed25519.PublicKey), keys[0].Public().(ed25519.PublicKey)) {
				t.Fatalf("round %d: slot %d got a different key", round, i)
			}
		}
	}
}
