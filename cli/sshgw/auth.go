//go:build !js

package sshgw

import (
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/on-keyday/agent-harness/cli/workspace"
	"golang.org/x/crypto/ssh"
)

// LoadOrCreateHostKey reads the ed25519 host key at path, generating and
// persisting one on first run — the same "the file is the origin" shape the
// server's --psk-file uses.
//
// Stability is the point. A host key regenerated per run makes every subsequent
// ssh print a host-key-changed warning and refuse to connect, so this never
// overwrites an existing file, and an unparseable one is an error rather than a
// quiet replacement: the operator can move it aside deliberately, but nothing
// here decides that for them.
func LoadOrCreateHostKey(path string) (ssh.Signer, error) {
	pemBytes, err := os.ReadFile(path)
	switch {
	case err == nil:
		signer, perr := ssh.ParsePrivateKey(pemBytes)
		if perr != nil {
			return nil, fmt.Errorf("ssh-gateway: host key %s is unreadable (move it aside to regenerate; every known_hosts entry for this gateway will then need updating): %w", path, perr)
		}
		return signer, nil
	case !os.IsNotExist(err):
		return nil, fmt.Errorf("ssh-gateway: host key %s: %w", path, err)
	}

	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, fmt.Errorf("ssh-gateway: generate host key: %w", err)
	}
	// MarshalPrivateKey takes the VALUE type while ParseRawPrivateKey hands back
	// a *ed25519.PrivateKey. ssh.ParsePrivateKey above hides that asymmetry;
	// do not reintroduce it here by taking an address.
	block, err := ssh.MarshalPrivateKey(priv, "harness ssh-gateway")
	if err != nil {
		return nil, fmt.Errorf("ssh-gateway: marshal host key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("ssh-gateway: host key dir: %w", err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return nil, fmt.Errorf("ssh-gateway: write host key %s: %w", path, err)
	}
	return ssh.NewSignerFromKey(priv)
}

// LoadAuthorizedKeys parses an OpenSSH authorized_keys file.
//
// A missing or key-less file is an error. The caller passed a path, which is a
// request to authenticate; returning an empty list would either lock everyone
// out silently or — worse, given how BuildServerConfig reads an empty list —
// be taken as "authentication was never configured".
func LoadAuthorizedKeys(path string) ([]ssh.PublicKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ssh-gateway: authorized keys %s: %w", path, err)
	}
	var keys []ssh.PublicKey
	rest := raw
	for len(rest) > 0 {
		key, _, _, remainder, perr := ssh.ParseAuthorizedKey(rest)
		if perr != nil {
			// ParseAuthorizedKey skips comments and blank lines itself and
			// errors only once nothing parseable remains.
			break
		}
		keys = append(keys, key)
		rest = remainder
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("ssh-gateway: authorized keys %s contains no usable key", path)
	}
	return keys, nil
}

// IsLoopbackBind reports whether addr binds only to this machine.
//
// An empty or unspecified host — "", ":2222", "0.0.0.0", "::" — is NOT
// loopback: those accept from every interface. Reading them as local is the
// bind-addr/dial-addr confusion, and here it would be the difference between
// an unauthenticated listener on this machine and one on the network.
//
// A name other than "localhost" is not resolved and is treated as non-loopback:
// resolution can change under the process, and the safe answer for a name we
// are not sure about is the one that demands keys.
func IsLoopbackBind(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// BuildServerConfig assembles the ssh server configuration and enforces the
// coupling between where the gateway listens and whether it authenticates.
//
// On loopback, with no keys supplied, there is no authentication. Public keys
// would buy nothing there: an agent the runner starts runs as the operator's
// UID and can read the operator's own private key, so it would authenticate as
// the operator anyway, and a sandboxed agent is confined to the harness
// server's single port and cannot reach this listener at all.
//
// Off loopback that reasoning inverts — a different-UID reader on another
// machine is exactly the adversary — so keys become mandatory. Enforced here
// rather than documented, and as a refusal to start: quietly serving
// 0.0.0.0 with no authentication is the widest possible reading of a mistyped
// flag.
//
// Supplying keys on a loopback bind is honoured, not ignored: passing a path is
// a request to authenticate wherever it is bound.
func BuildServerConfig(hostKey ssh.Signer, authorized []ssh.PublicKey, listenAddr string) (*ssh.ServerConfig, error) {
	cfg := &ssh.ServerConfig{}
	if len(authorized) == 0 {
		if !IsLoopbackBind(listenAddr) {
			return nil, fmt.Errorf("ssh-gateway: --listen %s is not loopback, so --authorized-keys is required (an open listener there would hand operator authority to anything that can reach the port)", listenAddr)
		}
		cfg.NoClientAuth = true
	} else {
		cfg.PublicKeyCallback = func(_ ssh.ConnMetadata, offered ssh.PublicKey) (*ssh.Permissions, error) {
			want := offered.Marshal()
			for _, k := range authorized {
				if subtle.ConstantTimeCompare(k.Marshal(), want) == 1 {
					return &ssh.Permissions{}, nil
				}
			}
			return nil, fmt.Errorf("ssh-gateway: public key not in the authorized list")
		}
	}
	cfg.AddHostKey(hostKey)
	return cfg, nil
}

// DefaultHostKeyPath returns where a gateway keeps its host key when the
// operator names none: beside the workspace config, this project's one
// existing per-repo client-state location. flagPath is the caller's --config
// value, "" when it was not given.
//
// The config location is resolved from the same three sources the config
// loader uses, rather than from the path that loader reports finding: it
// reports none at all when the default .harness/config does not exist, which
// is the ordinary case and exactly the one where a key still needs a home.
// LoadOrCreateHostKey creates the directory.
func DefaultHostKeyPath(flagPath string) string {
	p := flagPath
	if p == "" {
		p = os.Getenv("HARNESS_CONFIG")
	}
	if p == "" {
		p = filepath.FromSlash(workspace.DefaultPath)
	}
	return filepath.Join(filepath.Dir(p), "ssh_host_ed25519_key")
}
