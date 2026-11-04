package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner"
)

// resolveRunnerKeyPath: --runner-key-file, then HARNESS_RUNNER_KEY_FILE, then
// <home>/.config/harness/runner-key.
func resolveRunnerKeyPath(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if v := os.Getenv(cli.RunnerKeyFileEnv); v != "" {
		return v, nil
	}
	return runner.DefaultRunnerKeyPath()
}

// runKeyAction carries out --print-pubkey / --rotate-key, which run before
// any other flag is validated: an agent preparing an enrollment has no
// --server-cid to give. stdout gets the public key alone, so a script can
// take it; stderr names the file. handled=false means neither was asked for.
func runKeyAction(path string, printPubkey, rotate bool, stdout, stderr io.Writer) (handled bool, err error) {
	var key ed25519.PrivateKey
	switch {
	case rotate:
		key, err = runner.RotateRunnerKey(path)
	case printPubkey:
		key, err = runner.LoadOrCreateRunnerKey(path)
	default:
		return false, nil
	}
	if err != nil {
		return true, err
	}
	fmt.Fprintln(stdout, hex.EncodeToString(key.Public().(ed25519.PublicKey)))
	if rotate {
		fmt.Fprintf(stderr, "runner key: %s (the previous key is %s.prev; enroll the new key, restart the runners that use this file, then unenroll the old one)\n", path, path)
	} else {
		fmt.Fprintf(stderr, "runner key: %s\n", path)
	}
	return true, nil
}

// listenNeedsServerKey refuses listen mode without the server's key
// (docs/superpowers/specs/2026-11-04-persistent-reverse-dial-and-runner-kick-design.md
// D10): a listening runner accepts handshakes from anyone who reaches its port,
// and without a key nothing checks that the one answering is the server — the
// first dialer to say ok could assign it tasks.
func listenNeedsServerKey(listenMode bool, serverPubKey ed25519.PublicKey) error {
	if listenMode && serverPubKey == nil {
		return fmt.Errorf("--listen/--udp-listen needs the server's key (--server-pubkey or --server-pubkey-file): without it any dialer can act as the server")
	}
	return nil
}
