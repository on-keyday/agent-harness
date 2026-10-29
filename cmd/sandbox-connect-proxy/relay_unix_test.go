//go:build unix

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeCreds(t *testing.T, path, tok string, exp time.Time) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": tok, "refreshToken": "r", "expiresAt": exp.UnixMilli()}})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeClaude stands in for `claude auth status --json`: it counts its runs and
// rewrites the credential file with a fresh token, as the real CLI does.
func fakeClaude(t *testing.T, dir, creds string) (bin string, runs func() int) {
	t.Helper()
	count := filepath.Join(dir, "runs")
	bin = filepath.Join(dir, "claude")
	exp := time.Now().Add(8 * time.Hour).UnixMilli()
	script := fmt.Sprintf("#!/bin/sh\n[ \"$*\" = \"auth status --json\" ] || exit 9\necho x >>%q\n"+
		"printf '%%s' '{\"claudeAiOauth\":{\"accessToken\":\"fresh\",\"refreshToken\":\"r2\",\"expiresAt\":%d}}' >%q\n",
		count, exp, creds)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, func() int {
		b, _ := os.ReadFile(count)
		return strings.Count(string(b), "x")
	}
}

func newCreds(t *testing.T) (*claudeCreds, func() int) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".credentials.json")
	bin, runs := fakeClaude(t, dir, path)
	return &claudeCreds{path: path, bin: bin, lock: filepath.Join(dir, "lock"),
		refreshBefore: 5 * time.Minute, now: time.Now}, runs
}

func TestRelayUsesAFreshTokenAsIs(t *testing.T) {
	c, runs := newCreds(t)
	writeCreds(t, c.path, "current", time.Now().Add(2*time.Hour))
	if k, err := c.key(); err != nil || k != "current" {
		t.Fatalf("key = %q, %v", k, err)
	}
	if runs() != 0 {
		t.Error("refreshed a token two hours from expiry")
	}
}

func TestRelayHasTheCLIRefreshNearExpiry(t *testing.T) {
	for _, left := range []time.Duration{3 * time.Minute, -time.Minute} {
		t.Run(left.String(), func(t *testing.T) {
			c, runs := newCreds(t)
			writeCreds(t, c.path, "stale", time.Now().Add(left))
			if k, err := c.key(); err != nil || k != "fresh" {
				t.Fatalf("key = %q, %v; want the token the CLI wrote", k, err)
			}
			if runs() != 1 {
				t.Errorf("auth status ran %d times, want 1", runs())
			}
		})
	}
}

// Every request of a busy agent arrives at once around expiry; the CLI must
// still run once, not once per request.
func TestRelayRefreshesOnceUnderConcurrency(t *testing.T) {
	c, runs := newCreds(t)
	writeCreds(t, c.path, "stale", time.Now().Add(time.Minute))
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if k, err := c.key(); err != nil || k != "fresh" {
				t.Errorf("key = %q, %v", k, err)
			}
		}()
	}
	wg.Wait()
	if runs() != 1 {
		t.Errorf("auth status ran %d times, want 1", runs())
	}
}

// A file the host's claude rewrites between requests is read anew: the relay
// holds no token of its own.
func TestRelayRereadsTheFileEachRequest(t *testing.T) {
	c, _ := newCreds(t)
	writeCreds(t, c.path, "one", time.Now().Add(time.Hour))
	k1, _ := c.key()
	writeCreds(t, c.path, "two", time.Now().Add(time.Hour))
	k2, _ := c.key()
	if k1 != "one" || k2 != "two" {
		t.Errorf("got %q then %q", k1, k2)
	}
}

func TestRelayFailedRefreshStillSendsWhatItHas(t *testing.T) {
	c, _ := newCreds(t)
	c.bin = "/nonexistent/claude"
	writeCreds(t, c.path, "expired", time.Now().Add(-time.Minute))
	if k, err := c.key(); err != nil || k != "expired" {
		t.Errorf("key = %q, %v; want the expired token (the upstream's 401 says more than a 502)", k, err)
	}
}

func TestRelayRefusesAFileWithoutAnOAuthToken(t *testing.T) {
	c, _ := newCreds(t)
	if err := os.WriteFile(c.path, []byte(`{"other":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.key(); err == nil || !strings.Contains(err.Error(), "accessToken") {
		t.Errorf("err = %v", err)
	}
}
