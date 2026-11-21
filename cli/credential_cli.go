//go:build !js

package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// The credential verbs for the CLI: keygen and the secret's hand-over touch
// the host's files and stdout, which is why they are here and not in
// credential.go. Each verb has the three forms the schedule family has: the
// With form on a *Client a surface already holds, the Out form that dials.

// checkSeedDestination is D3a: an agent identity may not have a seed printed
// into its own transcript, so --out or --pubkey is required there. Not a
// security boundary (an agent can run `credential keygen` itself; whether it
// may mint a credential at all is the credential_issue grant) — accident
// prevention.
func checkSeedDestination(a verb.CredentialIssueAction) error {
	if a.Out != "" || a.Pubkey != "" {
		return nil
	}
	if kind, _ := EffectiveClientKind(protocol.ClientKind_Cli); kind == protocol.ClientKind_Agent {
		return errors.New("an agent identity may not print a private key: use --out FILE or --pubkey HEX")
	}
	return nil
}

// createSecretFile creates path for a secret: new, mode 0600, never an
// existing file (a seed already there may be the only copy of another
// credential).
func createSecretFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("%s exists; a secret is never overwritten (remove it, or name another file)", path)
		}
		return nil, err
	}
	return f, nil
}

// CredentialKeygenWith is `credential keygen --out FILE`: a fresh key pair on
// the host that will use the credential. The seed goes to the file (hex, one
// line), the public key (64 hex) to out; the seed is never printed.
func CredentialKeygenWith(a verb.CredentialKeygenAction, out io.Writer) error {
	if a.Out == "" {
		return errors.New("credential keygen: --out FILE is required (the seed is written there, never printed)")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	f, err := createSecretFile(a.Out)
	if err != nil {
		return fmt.Errorf("credential keygen: %w", err)
	}
	_, werr := fmt.Fprintf(f, "%s\n", hex.EncodeToString(priv.Seed()))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(a.Out)
		return fmt.Errorf("credential keygen: %w", werr)
	}
	fmt.Fprintln(out, hex.EncodeToString(pub))
	return nil
}

// CredentialIssueWith runs credential new / rekey for the CLI and the TUI.
// The key pair is made here (D3, IssueCredential): the server is sent the
// public key only. With --pubkey the key is the holder's and no secret exists
// on this side; otherwise the seed is handed over once, to --out or to out,
// after the server has accepted it.
func CredentialIssueWith(ctx context.Context, c *Client, a verb.CredentialIssueAction, out io.Writer) error {
	verbName := "new"
	if a.Sub == verb.SubRekey {
		verbName = "rekey"
	}
	if err := checkSeedDestination(a); err != nil {
		return err
	}
	if _, err := credentialTTLMs(a.TTL); err != nil {
		return fmt.Errorf("credential %s: %w", verbName, err)
	}

	// The file is created BEFORE the server is asked, so a path that cannot be
	// written is refused while there is still nothing to lose; its content is
	// written once the id is known.
	var f *os.File
	if a.Out != "" {
		if a.Pubkey != "" {
			return fmt.Errorf("credential %s: --out and --pubkey: one generates the key here and the other registers a key made elsewhere", verbName)
		}
		var err error
		if f, err = createSecretFile(a.Out); err != nil {
			return fmt.Errorf("credential %s: --out: %w", verbName, err)
		}
	}
	h, err := IssueCredential(ctx, c, a)
	if err != nil {
		if f != nil {
			f.Close()
			os.Remove(a.Out)
		}
		return err
	}
	id := hex.EncodeToString(h.TaskID.Id[:])
	fmt.Fprintln(out, CredentialIssuedLine(h.CredentialIssued))
	switch {
	case f != nil:
		_, werr := fmt.Fprintf(f, "%s\n", h.Cred.Secret())
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return fmt.Errorf("credential %s: issued as %s but the secret could not be written to %s: %w (credential rekey %s gives it a new key)", verbName, id, a.Out, werr, id)
		}
		fmt.Fprintf(out, "secret written to %s\n", a.Out)
	case h.Cred != nil:
		fmt.Fprintf(out, "secret: %s\n", h.Cred.Secret())
	default:
		// The key is the holder's: it needs the full id, which the first
		// line shortens.
		fmt.Fprintln(out, CredentialBindHint(id))
	}
	return nil
}

// CredentialBindWith is `credential bind <id> --file FILE`: D3 way (2)'s last
// step. The seed file keygen wrote becomes the credential file
// `<task-id>:<seed>`, written to a temp file beside it (mode 0600), synced and
// renamed over it, so a crash leaves the old seed file or the new credential,
// never half of one. The seed is read and written, never printed, and no error
// repeats it.
func CredentialBindWith(a verb.CredentialBindAction, out io.Writer) error {
	tid, err := parseTaskIDHex(a.ID)
	if err != nil {
		return fmt.Errorf("credential bind: credential id: %w", err)
	}
	f, err := os.Open(a.File)
	if err != nil {
		return fmt.Errorf("credential bind: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("credential bind: %w", err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		f.Close()
		return fmt.Errorf("credential bind: %s has mode %04o, readable by group or other: chmod 600 it", a.File, fi.Mode().Perm())
	}
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	f.Close()
	if err != nil {
		return fmt.Errorf("credential bind: %w", err)
	}
	seed := strings.TrimSpace(string(data))
	if strings.Contains(seed, ":") {
		return fmt.Errorf("credential bind: %s already holds a credential (<task-id>:<seed>); nothing to bind", a.File)
	}
	raw, err := hex.DecodeString(seed)
	if err != nil || len(raw) != ed25519.SeedSize {
		return fmt.Errorf("credential bind: %s is not a seed file (want the 64 hex characters credential keygen wrote)", a.File)
	}
	cred := Credential{TaskID: tid, Key: ed25519.NewKeyFromSeed(raw)}

	tmp, err := os.CreateTemp(filepath.Dir(a.File), ".credential-bind-*")
	if err != nil {
		return fmt.Errorf("credential bind: %w", err)
	}
	tmpName := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("credential bind: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		return fail(err)
	}
	if _, err := fmt.Fprintf(tmp, "%s\n", cred.Secret()); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("credential bind: %w", err)
	}
	if err := os.Rename(tmpName, a.File); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("credential bind: %w", err)
	}
	fmt.Fprintf(out, "credential %s: bound %s; run scripts with HARNESS_CREDENTIAL_FILE=%s\n", hex.EncodeToString(tid.Id[:])[:8], a.File, a.File)
	return nil
}

func credentialDial(ctx context.Context, peerCID objproto.ConnectionID, fn func(*Client) error) error {
	return recordDial(ctx, peerCID, fn)
}

// CredentialKeygenOut, CredentialIssueOut, CredentialLsOut and
// CredentialRevokeOut are the fresh-dial forms for the CLI. keygen is local
// and dials nothing.
func CredentialKeygenOut(a verb.CredentialKeygenAction, out io.Writer) error {
	return CredentialKeygenWith(a, out)
}

func CredentialIssueOut(ctx context.Context, peerCID objproto.ConnectionID, a verb.CredentialIssueAction, out io.Writer) error {
	// Refused before dialing: neither an agent printing a seed nor a
	// malformed ttl is worth a connection.
	if err := checkSeedDestination(a); err != nil {
		return err
	}
	return credentialDial(ctx, peerCID, func(c *Client) error { return CredentialIssueWith(ctx, c, a, out) })
}

func CredentialLsOut(ctx context.Context, peerCID objproto.ConnectionID, asJSON bool, out io.Writer) error {
	return credentialDial(ctx, peerCID, func(c *Client) error { return CredentialLsWith(ctx, c, asJSON, out) })
}

func CredentialRevokeOut(ctx context.Context, peerCID objproto.ConnectionID, id string, out io.Writer) error {
	return credentialDial(ctx, peerCID, func(c *Client) error { return CredentialRevokeWith(ctx, c, id, out) })
}
