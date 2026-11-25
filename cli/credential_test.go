//go:build !js

package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/connauth"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// A credential is `<32 hex task id>:<64 hex seed>`. The seed is a secret, so
// no error here may repeat what was parsed.

func testCredential(t *testing.T) connauth.Credential {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	var tid protocol.TaskID
	for i := range tid.Id {
		tid.Id[i] = byte(0xa0 + i)
	}
	return connauth.Credential{TaskID: tid, Key: priv}
}

func TestParseCredentialRoundTrip(t *testing.T) {
	c := testCredential(t)
	s := c.Secret()
	if len(s) != 32+1+64 || s[32] != ':' {
		t.Fatalf("String() = %d bytes %q, want <32 hex>:<64 hex>", len(s), s[:8])
	}
	got, err := connauth.ParseCredential(s)
	if err != nil {
		t.Fatal(err)
	}
	if got.TaskID != c.TaskID || !bytes.Equal(got.Key, c.Key) {
		t.Fatalf("round trip changed the credential")
	}
	if got2, err := connauth.ParseCredential("  " + s + "\n"); err != nil || got2.TaskID != c.TaskID {
		t.Fatalf("surrounding whitespace (a file's newline) must be tolerated: %v", err)
	}
}

func TestParseCredentialRejects(t *testing.T) {
	good := testCredential(t).Secret()
	id, seed := good[:32], good[33:]
	for name, in := range map[string]string{
		"empty":         "",
		"no colon":      id + seed,
		"short id":      id[:30] + ":" + seed,
		"long id":       id + "00:" + seed,
		"short seed":    id + ":" + seed[:62],
		"long seed":     id + ":" + seed + "00",
		"non-hex id":    "zz" + id[2:] + ":" + seed,
		"non-hex seed":  id + ":" + "zz" + seed[2:],
		"two colons":    id + ":" + seed[:32] + ":" + seed[32:],
		"seed only":     seed,
		"just a colon":  ":",
		"id and colon":  id + ":",
		"colon and key": ":" + seed,
	} {
		_, err := connauth.ParseCredential(in)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), seed) || (len(in) > 8 && strings.Contains(err.Error(), in[len(in)-8:])) {
			t.Errorf("%s: the error repeats the input: %v", name, err)
		}
	}
}

func clearCredentialEnv(t *testing.T) {
	t.Helper()
	t.Setenv(connauth.CredentialFileEnv, "")
	t.Setenv(connauth.CredentialEnv, "")
}

func writeCredentialFile(t *testing.T, dir, name string, c connauth.Credential, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(c.Secret()+"\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadCredentialOrder(t *testing.T) {
	clearCredentialEnv(t)
	dir := t.TempDir()
	flagC, fileC, valueC := testCredential(t), testCredential(t), testCredential(t)
	flagC.TaskID.Id[0], fileC.TaskID.Id[0], valueC.TaskID.Id[0] = 1, 2, 3
	flagPath := writeCredentialFile(t, dir, "flag", flagC, 0o600)
	filePath := writeCredentialFile(t, dir, "env", fileC, 0o600)

	if c, err := connauth.LoadCredential(""); err != nil || c != nil {
		t.Fatalf("no source: got (%v, %v), want (nil, nil)", c, err)
	}

	t.Setenv(connauth.CredentialEnv, valueC.Secret())
	if c, err := connauth.LoadCredential(""); err != nil || c == nil || c.TaskID != valueC.TaskID {
		t.Fatalf("value env alone: got (%v, %v)", c, err)
	}
	t.Setenv(connauth.CredentialFileEnv, filePath)
	if c, err := connauth.LoadCredential(""); err != nil || c == nil || c.TaskID != fileC.TaskID {
		t.Fatalf("file env must beat value env: got (%v, %v)", c, err)
	}
	if c, err := connauth.LoadCredential(flagPath); err != nil || c == nil || c.TaskID != flagC.TaskID {
		t.Fatalf("flag must beat both: got (%v, %v)", c, err)
	}
	// First match wins: a named file that cannot be read is an error, never a
	// quiet fall through to the next source.
	if _, err := connauth.LoadCredential(filepath.Join(dir, "missing")); err == nil {
		t.Fatalf("a missing --credential-file fell through to another source")
	}
}

func TestLoadCredentialRefusesGroupReadableFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits")
	}
	clearCredentialEnv(t)
	c := testCredential(t)
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o660, 0o666} {
		p := writeCredentialFile(t, t.TempDir(), "cred", c, mode)
		_, err := connauth.LoadCredential(p)
		if err == nil {
			t.Errorf("mode %04o accepted", mode)
			continue
		}
		if !strings.Contains(err.Error(), "mode") || !strings.Contains(err.Error(), "chmod 600") {
			t.Errorf("mode %04o: error does not name the mode and the fix: %v", mode, err)
		}
		if strings.Contains(err.Error(), c.Secret()[33:]) {
			t.Errorf("the error contains the seed")
		}
	}
	if _, err := connauth.LoadCredential(writeCredentialFile(t, t.TempDir(), "ok", c, 0o600)); err != nil {
		t.Fatalf("0600 refused: %v", err)
	}
	if _, err := connauth.LoadCredential(writeCredentialFile(t, t.TempDir(), "ro", c, 0o400)); err != nil {
		t.Fatalf("0400 refused: %v", err)
	}
}

// A credential the server refused for being revoked or expired will not
// become valid by trying again: PersistLoop must stop, not retry forever.
func TestRevokedIsFatal(t *testing.T) {
	for _, st := range []protocol.PskAuthStatus{protocol.PskAuthStatus_Revoked, protocol.PskAuthStatus_Expired, protocol.PskAuthStatus_BadTicket} {
		if connauth.NewPskRejectedError(st).Retryable() {
			t.Errorf("%v is retryable", st)
		}
	}
	// What the script's operator reads still wraps the rejection, so
	// PersistLoop classifies it by the same Retryable().
	err := credentialRefusalError(testCredential(t), connauth.NewPskRejectedError(protocol.PskAuthStatus_Revoked))
	var rej *connauth.PskRejectedError
	if !errors.As(err, &rej) || rej.Retryable() {
		t.Errorf("the worded refusal lost the fatal rejection: %v", err)
	}
	if !strings.Contains(err.Error(), "revoked") || !strings.Contains(err.Error(), "credential rekey") {
		t.Errorf("the revoked refusal does not say revoked and what to do: %v", err)
	}
}

// D3a: an agent identity must not print a seed into its own transcript.
func TestAgentIdentityRefusesPrintedSeed(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("HARNESS_RUNNER_ID", strings.Repeat("ab", 16))
	t.Setenv("HARNESS_TASK_ID", strings.Repeat("cd", 16))
	t.Setenv("HARNESS_AUTH_TICKET", strings.Repeat("ef", 16))

	const want = "an agent identity may not print a private key: use --out FILE or --pubkey HEX"
	var out bytes.Buffer
	err := CredentialIssueWith(context.Background(), nil, verb.CredentialIssueAction{Sub: verb.SubNew, TTL: "30d"}, &out)
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
	if out.Len() != 0 {
		t.Fatalf("printed %q before refusing", out.String())
	}
	if err := checkSeedDestination(verb.CredentialIssueAction{Out: "f"}); err != nil {
		t.Errorf("--out refused: %v", err)
	}
	if err := checkSeedDestination(verb.CredentialIssueAction{Pubkey: strings.Repeat("00", 32)}); err != nil {
		t.Errorf("--pubkey refused: %v", err)
	}
	// An operator may print.
	for _, k := range []string{"HARNESS_RUNNER_ID", "HARNESS_TASK_ID", "HARNESS_AUTH_TICKET"} {
		t.Setenv(k, "")
	}
	if err := checkSeedDestination(verb.CredentialIssueAction{}); err != nil {
		t.Errorf("operator refused: %v", err)
	}
}

// A Credential formatted by accident (a log line, an error) never shows its seed.
func TestCredentialFormattingIsRedacted(t *testing.T) {
	c := testCredential(t)
	seed := hex.EncodeToString(c.Key.Seed())
	got := fmt.Sprintf("%v %s %+v %#v %q", c, c, c, c, c)
	if strings.Contains(got, seed) || strings.Contains(got, c.Secret()) {
		t.Fatalf("formatting a Credential printed the seed: %s", got)
	}
	if !strings.Contains(got, "credential a0a1a2a3") {
		t.Fatalf("redacted form should name the task: %s", got)
	}
	if !strings.Contains(c.Secret(), seed) {
		t.Fatalf("Secret() lost the seed")
	}
}

// D3 way (2) end to end on one host: keygen writes the seed, bind turns it
// into a credential file LoadCredential accepts.
func TestKeygenBindLoad(t *testing.T) {
	clearCredentialEnv(t)
	path := filepath.Join(t.TempDir(), "seed")
	var out bytes.Buffer
	if err := CredentialKeygenWith(verb.CredentialKeygenAction{Out: path}, &out); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("ab", 16)
	if _, err := connauth.LoadCredential(path); err == nil {
		t.Fatalf("a bare seed file loaded as a credential")
	}
	out.Reset()
	if err := CredentialBindWith(verb.CredentialBindAction{ID: id, File: path}, &out); err != nil {
		t.Fatal(err)
	}
	c, err := connauth.LoadCredential(path)
	if err != nil || c == nil || hex.EncodeToString(c.TaskID.Id[:]) != id {
		t.Fatalf("bound file does not load: %v", err)
	}
	if strings.Contains(out.String(), hex.EncodeToString(c.Key.Seed())) {
		t.Fatalf("bind printed the seed")
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v, want 0600", fi.Mode().Perm())
		}
	}
	if ents, _ := os.ReadDir(filepath.Dir(path)); len(ents) != 1 {
		t.Fatalf("a temp file was left behind: %v", ents)
	}
	// Already bound: refused, file untouched.
	before, _ := os.ReadFile(path)
	if err := CredentialBindWith(verb.CredentialBindAction{ID: strings.Repeat("cd", 16), File: path}, &out); err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Fatalf("rebinding: %v", err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatalf("a refused bind changed the file")
	}
}

func TestBindRefusals(t *testing.T) {
	dir := t.TempDir()
	id := strings.Repeat("ab", 16)
	seedFile := func(name, body string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		os.Chmod(p, mode)
		return p
	}
	good := strings.Repeat("0a", 32) + "\n"
	if runtime.GOOS != "windows" {
		p := seedFile("open", good, 0o644)
		if err := CredentialBindWith(verb.CredentialBindAction{ID: id, File: p}, io.Discard); err == nil || !strings.Contains(err.Error(), "chmod 600") {
			t.Errorf("group-readable seed file: %v", err)
		}
		if b, _ := os.ReadFile(p); string(b) != good {
			t.Errorf("a refused bind changed the file")
		}
	}
	for name, body := range map[string]string{"short": "0a0a\n", "nothex": strings.Repeat("zz", 32) + "\n", "empty": ""} {
		err := CredentialBindWith(verb.CredentialBindAction{ID: id, File: seedFile(name, body, 0o600)}, io.Discard)
		if err == nil || strings.Contains(err.Error(), "zz") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := CredentialBindWith(verb.CredentialBindAction{ID: "xyz", File: seedFile("ok", good, 0o600)}, io.Discard); err == nil {
		t.Errorf("bad task id accepted")
	}
	if err := CredentialBindWith(verb.CredentialBindAction{ID: id, File: filepath.Join(dir, "missing")}, io.Discard); err == nil {
		t.Errorf("missing file accepted")
	}
}
