package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli"
)

// --print-pubkey needs no other flag (no --server-cid, no --roots) and prints
// the same key every time: stdout carries the key alone so a script can take
// it, stderr names the file it came from.
func TestPrintPubkeyIsStableAndNeedsNoOtherFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner-key")
	var out1, err1, out2, err2 bytes.Buffer
	if handled, err := runKeyAction(path, true, false, &out1, &err1); !handled || err != nil {
		t.Fatalf("first --print-pubkey: handled=%v err=%v", handled, err)
	}
	if handled, err := runKeyAction(path, true, false, &out2, &err2); !handled || err != nil {
		t.Fatalf("second --print-pubkey: handled=%v err=%v", handled, err)
	}
	key := strings.TrimSpace(out1.String())
	if len(key) != 64 || key != strings.TrimSpace(out2.String()) {
		t.Fatalf("stdout = %q then %q, want the same 64-hex key twice", out1.String(), out2.String())
	}
	if !strings.Contains(err1.String(), path) {
		t.Fatalf("stderr %q does not name the key file %q", err1.String(), path)
	}
}

func TestRotateKeyPrintsTheNewKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner-key")
	var before, rotated, after, sink bytes.Buffer
	runKeyAction(path, true, false, &before, &sink)
	if handled, err := runKeyAction(path, false, true, &rotated, &sink); !handled || err != nil {
		t.Fatalf("--rotate-key: handled=%v err=%v", handled, err)
	}
	runKeyAction(path, true, false, &after, &sink)
	if rotated.String() == before.String() || rotated.String() != after.String() {
		t.Fatalf("before %q, rotated %q, after %q: rotation must print the key the file now holds", before.String(), rotated.String(), after.String())
	}
}

func TestNoKeyActionFallsThrough(t *testing.T) {
	var out, errb bytes.Buffer
	if handled, err := runKeyAction(filepath.Join(t.TempDir(), "k"), false, false, &out, &errb); handled || err != nil {
		t.Fatalf("no action requested: handled=%v err=%v, want false, nil", handled, err)
	}
}

func TestResolveRunnerKeyPathOrder(t *testing.T) {
	t.Setenv(cli.RunnerKeyFileEnv, "/from/env")
	if got, _ := resolveRunnerKeyPath("/from/flag"); got != "/from/flag" {
		t.Errorf("flag given: %q, want the flag", got)
	}
	if got, _ := resolveRunnerKeyPath(""); got != "/from/env" {
		t.Errorf("env only: %q, want the env", got)
	}
	os.Unsetenv(cli.RunnerKeyFileEnv)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if got, _ := resolveRunnerKeyPath(""); got != filepath.Join(home, ".config", "harness", "runner-key") {
		t.Errorf("neither: %q, want the default under HOME", got)
	}
}

// Agents have no use for the key's path, and the runner hands them its env.
func TestScrubDropsTheRunnerKeyFileName(t *testing.T) {
	t.Setenv(cli.RunnerKeyFileEnv, "/somewhere/runner-key")
	scrubOperatorSecret()
	if v, ok := os.LookupEnv(cli.RunnerKeyFileEnv); ok {
		t.Errorf("%s survived the scrub with %q", cli.RunnerKeyFileEnv, v)
	}
}
