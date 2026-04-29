package main_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Tests run `go run .` to pick up code changes without an explicit build
// step. cmd.Env must be seeded from os.Environ() — passing a nil-extended
// slice strips PATH/HOME and breaks the toolchain lookup.
//
// Each test uses a distinct unused localhost port so we can grep slog
// stderr for the dialed host:port. Localhost with no listener gives a
// fast "connection refused" — the websocket layer logs "address=127.0.0.1:NNN"
// before the higher ECDH timeout fires. Reserved-range IPs (198.51.100.x)
// don't work for this purpose because TCP SYN times out *after* ECDH.

// TestCLI_ServerCIDFlagBeatsEnv: explicit --server-cid wins over env.
func TestCLI_ServerCIDFlagBeatsEnv(t *testing.T) {
	cmd := exec.Command("go", "run", ".",
		"--server-cid=ws:127.0.0.1:19991-1", "ls")
	cmd.Env = append(os.Environ(), "HARNESS_SERVER_CID=ws:127.0.0.1:19992-1")
	out, _ := cmd.CombinedOutput()
	s := string(out)
	if !strings.Contains(s, "127.0.0.1:19991") {
		t.Errorf("flag value not used: %s", s)
	}
	if strings.Contains(s, "127.0.0.1:19992") {
		t.Errorf("env value leaked: %s", s)
	}
}

// TestCLI_ServerCIDEnvFallback: with no flag, env is used.
func TestCLI_ServerCIDEnvFallback(t *testing.T) {
	cmd := exec.Command("go", "run", ".", "ls")
	cmd.Env = append(os.Environ(), "HARNESS_SERVER_CID=ws:127.0.0.1:19993-1")
	out, _ := cmd.CombinedOutput()
	s := string(out)
	if strings.Contains(s, "server-cid required") {
		t.Errorf("env fallback not applied: %s", s)
	}
	if !strings.Contains(s, "127.0.0.1:19993") {
		t.Errorf("env value did not reach dial: %s", s)
	}
}

// TestCLI_RepoFlagBeatsEnv: submit must use --repo over HARNESS_REPO_PATH.
// We can't observe the resolved repo path on the wire (server is down), but
// we can confirm the flag was accepted (no "required" error) and the
// program proceeded to dial.
func TestCLI_RepoFlagBeatsEnv(t *testing.T) {
	cmd := exec.Command("go", "run", ".",
		"--server-cid=ws:127.0.0.1:19994-1",
		"submit", "--repo=/tmp/from-flag", "--task=x")
	cmd.Env = append(os.Environ(), "HARNESS_REPO_PATH=/tmp/from-env")
	out, _ := cmd.CombinedOutput()
	s := string(out)
	if strings.Contains(s, "--repo or HARNESS_REPO_PATH required") {
		t.Errorf("flag was not picked up: %s", s)
	}
	if !strings.Contains(s, "127.0.0.1:19994") {
		t.Errorf("did not reach dial step: %s", s)
	}
}

// TestCLI_RepoEnvFallback: with no flag, env supplies repo.
func TestCLI_RepoEnvFallback(t *testing.T) {
	cmd := exec.Command("go", "run", ".",
		"--server-cid=ws:127.0.0.1:19995-1",
		"submit", "--task=x")
	cmd.Env = append(os.Environ(), "HARNESS_REPO_PATH=/tmp/from-env")
	out, _ := cmd.CombinedOutput()
	s := string(out)
	if strings.Contains(s, "--repo or HARNESS_REPO_PATH required") {
		t.Errorf("env fallback not applied: %s", s)
	}
	if !strings.Contains(s, "127.0.0.1:19995") {
		t.Errorf("did not reach dial step: %s", s)
	}
}
