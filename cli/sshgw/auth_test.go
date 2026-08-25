//go:build !js

package sshgw

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestLoadOrCreateHostKey_CreatesThenReuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssh_host_ed25519_key")

	first, err := LoadOrCreateHostKey(path)
	if err != nil {
		t.Fatalf("first LoadOrCreateHostKey: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("host key mode = %o, want 600", got)
	}

	// A regenerated host key makes every subsequent ssh refuse to connect with
	// a host-key-changed warning, so stability across runs is the property.
	second, err := LoadOrCreateHostKey(path)
	if err != nil {
		t.Fatalf("second LoadOrCreateHostKey: %v", err)
	}
	if string(first.PublicKey().Marshal()) != string(second.PublicKey().Marshal()) {
		t.Error("host key changed between runs; known_hosts would break")
	}
}

func TestLoadOrCreateHostKey_UnreadableIsAnErrorNotARegeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hk")
	if err := os.WriteFile(path, []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateHostKey(path); err == nil {
		t.Error("want an error for an unparseable host key — silently replacing it would break every known_hosts entry")
	}
}

func TestLoadOrCreateHostKey_CreatesMissingDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "hk")
	if _, err := LoadOrCreateHostKey(path); err != nil {
		t.Fatalf("LoadOrCreateHostKey into a missing dir: %v", err)
	}
}

func TestLoadAuthorizedKeys(t *testing.T) {
	line := string(ssh.MarshalAuthorizedKey(testPublicKey(t)))

	path := filepath.Join(t.TempDir(), "keys")
	content := "# a comment\n\n" + `no-pty,command="x" ` + line + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	keys, err := LoadAuthorizedKeys(path)
	if err != nil {
		t.Fatalf("LoadAuthorizedKeys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("got %d keys, want 1 (comments and blank lines are skipped; options are not keys)", len(keys))
	}
}

func TestLoadAuthorizedKeys_MultipleKeys(t *testing.T) {
	a := ssh.MarshalAuthorizedKey(testPublicKey(t))
	b := ssh.MarshalAuthorizedKey(testPublicKey(t))
	path := filepath.Join(t.TempDir(), "keys")
	if err := os.WriteFile(path, append(a, b...), 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := LoadAuthorizedKeys(path)
	if err != nil {
		t.Fatalf("LoadAuthorizedKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Errorf("got %d keys, want 2", len(keys))
	}
}

func TestLoadAuthorizedKeys_MissingFileIsAnError(t *testing.T) {
	if _, err := LoadAuthorizedKeys(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("want an error for a missing file, not an empty allow-list")
	}
}

func TestLoadAuthorizedKeys_KeylessFileIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys")
	if err := os.WriteFile(path, []byte("# nothing but a comment\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAuthorizedKeys(path); err == nil {
		t.Error("want an error for a file with no usable key")
	}
}

func TestIsLoopbackBind(t *testing.T) {
	// The unspecified forms are NOT loopback: they accept from every
	// interface. Reading them as local is the bind-addr/dial-addr confusion.
	yes := []string{"127.0.0.1:2222", "127.0.0.53:2222", "[::1]:2222", "localhost:2222"}
	no := []string{"0.0.0.0:2222", ":2222", "192.168.1.10:2222", "[::]:2222", "example.test:2222", "garbage"}
	for _, a := range yes {
		if !IsLoopbackBind(a) {
			t.Errorf("IsLoopbackBind(%q) = false, want true", a)
		}
	}
	for _, a := range no {
		if IsLoopbackBind(a) {
			t.Errorf("IsLoopbackBind(%q) = true, want false", a)
		}
	}
}

func TestBuildServerConfig_LoopbackWithoutKeysServesUnauthenticated(t *testing.T) {
	cfg, err := BuildServerConfig(testHostKey(t), nil, "127.0.0.1:2222")
	if err != nil {
		t.Fatalf("loopback with no keys must start: %v", err)
	}
	if !cfg.NoClientAuth {
		t.Error("loopback with no keys should serve without auth")
	}
}

func TestBuildServerConfig_NonLoopbackWithoutKeysIsRefused(t *testing.T) {
	if _, err := BuildServerConfig(testHostKey(t), nil, "0.0.0.0:2222"); err == nil {
		t.Error("a non-loopback bind with no keys must be refused at startup, not served open")
	}
}

func TestBuildServerConfig_KeysGateEveryConnection(t *testing.T) {
	allowed := testPublicKey(t)
	cfg, err := BuildServerConfig(testHostKey(t), []ssh.PublicKey{allowed}, "0.0.0.0:2222")
	if err != nil {
		t.Fatalf("non-loopback with keys must start: %v", err)
	}
	if cfg.NoClientAuth {
		t.Error("a configuration with keys must not also accept unauthenticated clients")
	}
	if cfg.PublicKeyCallback == nil {
		t.Fatal("want a PublicKeyCallback when keys are configured")
	}
	if _, err := cfg.PublicKeyCallback(nil, allowed); err != nil {
		t.Errorf("configured key rejected: %v", err)
	}
	if _, err := cfg.PublicKeyCallback(nil, testPublicKey(t)); err == nil {
		t.Error("an unconfigured key was accepted")
	}
}

// Keys on a loopback bind are honoured too: passing them is a request to
// authenticate, not merely a thing that becomes mandatory off loopback.
func TestBuildServerConfig_KeysOnLoopbackStillGate(t *testing.T) {
	cfg, err := BuildServerConfig(testHostKey(t), []ssh.PublicKey{testPublicKey(t)}, "127.0.0.1:2222")
	if err != nil {
		t.Fatalf("BuildServerConfig: %v", err)
	}
	if cfg.NoClientAuth {
		t.Error("keys were supplied on a loopback bind and then ignored")
	}
}

func testHostKey(t *testing.T) ssh.Signer {
	t.Helper()
	signer, err := LoadOrCreateHostKey(filepath.Join(t.TempDir(), "hk"))
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func testPublicKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return sshPub
}
