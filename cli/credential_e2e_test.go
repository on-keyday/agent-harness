//go:build !js

package cli_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/cliopts"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/server"
	"github.com/on-keyday/objtrsf/objproto"
)

// startCredentialServer is an in-process server, with an operator PSK when
// psk is non-empty. The process env is scrubbed of every identity source so a
// test names exactly the ones it sets.
func startCredentialServer(t *testing.T, psk string) (*server.Server, objproto.ConnectionID) {
	t.Helper()
	for _, k := range []string{
		"HARNESS_RUNNER_ID", "HARNESS_TASK_ID", "HARNESS_AUTH_TICKET", "HARNESS_OPERATOR_PSK",
		"HARNESS_OPERATOR_PSK_FILE", cli.CredentialEnv, cli.CredentialFileEnv, "HARNESS_PROXY_VIA_RUNNER",
	} {
		t.Setenv(k, "")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	cfg := server.Config{Addr: addr}
	if psk != "" {
		cfg.OperatorPSK = []byte(psk)
		t.Setenv("HARNESS_OPERATOR_PSK", psk)
	}
	s := server.New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Run(ctx) }()
	t.Cleanup(cancel)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 50*time.Millisecond); err == nil {
			c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cid, err := cliopts.ResolveServerCID("ws:" + addr + "-*")
	if err != nil {
		t.Fatal(err)
	}
	return s, cid
}

func newCredential(t *testing.T, s *server.Server, g authority.Grant) cli.Credential {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	var k [32]byte
	copy(k[:], pub)
	id, err := s.Tasks().CreateCredential(protocol.TaskID{}, protocol.ClientKind_Cli, g, k, time.Now().Add(time.Hour), "ci")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := hex.DecodeString(id)
	var tid protocol.TaskID
	copy(tid.Id[:], raw)
	return cli.Credential{TaskID: tid, Key: priv}
}

func whoami(t *testing.T, cid objproto.ConnectionID) protocol.WhoAmIResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	r, err := cli.WhoAmIWith(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Review Focus 3: a shell carrying the operator PSK and a credential (an
// operator trying a script from their own terminal) connects as the
// credential, never silently as the operator.
func TestCredentialBeatsOperatorPSK(t *testing.T) {
	s, cid := startCredentialServer(t, "operator-secret")
	cred := newCredential(t, s, authority.Grant{Caps: protocol.Capability_ScheduleRun})

	// Control: the PSK alone is the operator.
	if r := whoami(t, cid); r.PrincipalTaskId != (protocol.TaskID{}) {
		t.Fatalf("control: the PSK alone connected as task %x, want the operator", r.PrincipalTaskId.Id)
	}

	t.Setenv(cli.CredentialEnv, cred.Secret())
	r := whoami(t, cid)
	if r.PrincipalTaskId != cred.TaskID {
		t.Fatalf("with a credential loaded the principal is %x, want the credential task %x", r.PrincipalTaskId.Id, cred.TaskID.Id)
	}
	if r.PrincipalKind != protocol.ClientKind_Credential {
		t.Fatalf("principal kind = %v, want credential", r.PrincipalKind)
	}
	if r.Capabilities&protocol.Capability_ScheduleEdit != 0 {
		t.Fatalf("a schedule_run credential holds schedule_edit: %v", r.Capabilities)
	}
	if r.Capabilities != protocol.Capability_ScheduleRun {
		t.Fatalf("caps = %v, want schedule_run only", cli.CapsLabel(r.Capabilities))
	}
	var out bytes.Buffer
	if err := cli.WriteWhoAmI(&out, r, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "kind=credential expires=") {
		t.Fatalf("whoami does not say it is a credential with an expiry:\n%s", out.String())
	}
	if strings.Contains(out.String(), cred.Secret()[33:]) {
		t.Fatalf("whoami printed the seed")
	}
	// The expiry is a parseable RFC3339 time.
	i := strings.Index(out.String(), "expires=")
	if _, err := time.Parse(time.RFC3339, strings.Fields(out.String()[i+len("expires="):])[0]); err != nil {
		t.Fatalf("expires= is not RFC3339: %v", err)
	}
}

// Review Focus 4: an agent task's shell that sets HARNESS_CREDENTIAL (the
// task's own id, runner id and ticket are also in its env) connects as the
// credential, not as the agent.
func TestCredentialBeatsAgentEnv(t *testing.T) {
	s, cid := startCredentialServer(t, "")
	cred := newCredential(t, s, authority.Grant{Caps: protocol.Capability_ScheduleRun})
	t.Setenv("HARNESS_RUNNER_ID", strings.Repeat("ab", 16))
	t.Setenv("HARNESS_TASK_ID", strings.Repeat("cd", 16))
	t.Setenv("HARNESS_AUTH_TICKET", strings.Repeat("ef", 16))
	t.Setenv(cli.CredentialEnv, cred.Secret())

	kind, tid := cli.EffectiveClientKind(protocol.ClientKind_Cli)
	if kind != protocol.ClientKind_Credential || tid != cred.TaskID {
		t.Fatalf("EffectiveClientKind = (%v, %x), want the credential", kind, tid.Id)
	}
	r := whoami(t, cid)
	if r.PrincipalTaskId != cred.TaskID || r.PrincipalKind != protocol.ClientKind_Credential {
		t.Fatalf("principal = (%v, %x), want the credential task", r.PrincipalKind, r.PrincipalTaskId.Id)
	}
}

func credentialDial(ctx context.Context, cid objproto.ConnectionID) (*cli.Client, error) {
	return cli.Dial(ctx, cid, protocol.ClientKind_Cli)
}

func readCredentialFile(t *testing.T, path string) cli.Credential {
	t.Helper()
	c, err := cli.LoadCredential(path)
	if err != nil || c == nil {
		t.Fatalf("load %s: %v", path, err)
	}
	return *c
}

// The whole verb family against a server: keygen is local; new / rekey /
// ls / revoke go through the Client; the result lines are the ones the
// usage promises, and a seed reaches only the place it was asked to go.
func TestCredentialVerbsEndToEnd(t *testing.T) {
	s, cid := startCredentialServer(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	op, err := credentialDial(ctx, cid)
	if err != nil {
		t.Fatal(err)
	}
	defer op.Close()
	dir := t.TempDir()

	// --- new --out: the seed goes to the file, never to the output.
	outPath := filepath.Join(dir, "cred")
	var out bytes.Buffer
	caps := protocol.Capability_ScheduleRun | protocol.Capability_ResumeExact
	if err := cli.CredentialIssueWith(ctx, op, verb.CredentialIssueAction{
		Sub: verb.SubNew, TTL: "30d", Name: "nightly", Out: outPath, Caps: &caps, CapsPresent: true,
	}, &out); err != nil {
		t.Fatalf("new --out: %v", err)
	}
	if !strings.Contains(out.String(), ": issued, expires ") || !strings.Contains(out.String(), "secret written to "+outPath) {
		t.Fatalf("output:\n%s", out.String())
	}
	cred := readCredentialFile(t, outPath)
	if strings.Contains(out.String(), hex.EncodeToString(cred.Key.Seed())) {
		t.Fatalf("--out printed the seed:\n%s", out.String())
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(outPath); fi.Mode().Perm() != 0o600 {
			t.Fatalf("credential file mode = %v, want 0600", fi.Mode().Perm())
		}
	}
	if !strings.HasPrefix(out.String(), "credential "+hex.EncodeToString(cred.TaskID.Id[:])[:8]+": issued") {
		t.Fatalf("the first line must be `credential <id8>: issued, expires <RFC3339>`:\n%s", out.String())
	}
	// --out never overwrites an existing secret.
	if err := cli.CredentialIssueWith(ctx, op, verb.CredentialIssueAction{Sub: verb.SubNew, TTL: "1d", Out: outPath}, &bytes.Buffer{}); err == nil {
		t.Fatalf("--out overwrote an existing file")
	}
	if got := readCredentialFile(t, outPath); got.TaskID != cred.TaskID {
		t.Fatalf("a refused --out changed the file")
	}

	// The credential works, with what it was given.
	t.Setenv(cli.CredentialFileEnv, outPath)
	if r := whoami(t, cid); r.PrincipalTaskId != cred.TaskID || r.Capabilities != caps {
		t.Fatalf("whoami as the new credential: task=%x caps=%v", r.PrincipalTaskId.Id, cli.CapsLabel(r.Capabilities))
	}
	t.Setenv(cli.CredentialFileEnv, "")

	// --- new, printed: `secret: <id>:<seed>` once.
	out.Reset()
	if err := cli.CredentialIssueWith(ctx, op, verb.CredentialIssueAction{Sub: verb.SubNew, TTL: "2h"}, &out); err != nil {
		t.Fatalf("new (print): %v", err)
	}
	var printed cli.Credential
	for _, l := range strings.Split(out.String(), "\n") {
		if v, ok := strings.CutPrefix(l, "secret: "); ok {
			if printed, err = cli.ParseCredential(v); err != nil {
				t.Fatalf("the printed secret does not parse: %v", err)
			}
		}
	}
	if printed.Key == nil {
		t.Fatalf("no `secret:` line:\n%s", out.String())
	}

	// --- new --pubkey: the server gets the key, the output has no secret.
	pub, priv, _ := ed25519.GenerateKey(nil)
	out.Reset()
	if err := cli.CredentialIssueWith(ctx, op, verb.CredentialIssueAction{Sub: verb.SubNew, TTL: "1d", Pubkey: hex.EncodeToString(pub)}, &out); err != nil {
		t.Fatalf("new --pubkey: %v", err)
	}
	if strings.Contains(out.String(), "secret") && !strings.Contains(out.String(), "<seed") {
		t.Fatalf("--pubkey output mentions a secret it does not have:\n%s", out.String())
	}
	var pubID string
	for _, l := range strings.Fields(out.String()) {
		if len(l) == 32 {
			if _, err := hex.DecodeString(l); err == nil {
				pubID = l
			}
		}
	}
	if pubID == "" {
		t.Fatalf("--pubkey output does not give the full task id the holder needs:\n%s", out.String())
	}
	raw, _ := hex.DecodeString(pubID)
	var pubTID protocol.TaskID
	copy(pubTID.Id[:], raw)
	t.Setenv(cli.CredentialEnv, cli.Credential{TaskID: pubTID, Key: priv}.Secret())
	if r := whoami(t, cid); r.PrincipalTaskId != pubTID {
		t.Fatalf("the registered key does not authenticate")
	}
	t.Setenv(cli.CredentialEnv, "")

	// --- refusals name their cause.
	for name, c := range map[string]struct {
		a    verb.CredentialIssueAction
		want string
	}{
		"ttl over the bound": {verb.CredentialIssueAction{Sub: verb.SubNew, TTL: "91d", Out: filepath.Join(dir, "x")}, "90"},
		"masked cap": {verb.CredentialIssueAction{Sub: verb.SubNew, TTL: "1d", Out: filepath.Join(dir, "y"),
			Caps: capsPtr(protocol.Capability_ScheduleEdit), CapsPresent: true}, "schedule_edit"},
		"bad pubkey": {verb.CredentialIssueAction{Sub: verb.SubNew, TTL: "1d", Pubkey: strings.Repeat("00", 32)}, "public key"},
	} {
		err := cli.CredentialIssueWith(ctx, op, c.a, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want an error naming %q", name, err, c.want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "x")); err == nil {
		t.Errorf("a refused issue left a credential file behind")
	}

	// --- ls: text and JSON list only credentials.
	s.Tasks().Create("repo", "", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{}, "")
	out.Reset()
	if err := cli.CredentialLsWith(ctx, op, false, &out); err != nil {
		t.Fatal(err)
	}
	idHex := hex.EncodeToString(cred.TaskID.Id[:])
	if !strings.Contains(out.String(), idHex) || !strings.Contains(out.String(), "name=nightly") ||
		!strings.Contains(out.String(), "expires=") || !strings.Contains(out.String(), "last_used=") {
		t.Fatalf("credential ls:\n%s", out.String())
	}
	if n := strings.Count(strings.TrimSpace(out.String()), "\n") + 1; n != 3 {
		t.Fatalf("credential ls listed %d rows, want the 3 credentials:\n%s", n, out.String())
	}
	out.Reset()
	if err := cli.CredentialLsWith(ctx, op, true, &out); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("credential ls --json: %v\n%s", err, out.String())
	}
	if len(rows) != 3 {
		t.Fatalf("credential ls --json: %d rows, want 3", len(rows))
	}
	for _, k := range []string{"id", "name", "status", "caps", "scope", "credential_expires_at", "credential_last_used_at"} {
		if _, ok := rows[0][k]; !ok {
			t.Errorf("credential ls --json row has no %q: %v", k, rows[0])
		}
	}

	// --- rekey: the old key stops, the new one works under the same id.
	newOut := filepath.Join(dir, "cred2")
	out.Reset()
	if err := cli.CredentialIssueWith(ctx, op, verb.CredentialIssueAction{Sub: verb.SubRekey, ID: idHex, TTL: "10d", Out: newOut}, &out); err != nil {
		t.Fatalf("rekey: %v", err)
	}
	if !strings.Contains(out.String(), ": issued, expires ") {
		t.Fatalf("rekey output:\n%s", out.String())
	}
	if got := readCredentialFile(t, newOut); got.TaskID != cred.TaskID {
		t.Fatalf("rekey changed the task id")
	}
	t.Setenv(cli.CredentialFileEnv, outPath) // the old seed
	if _, err := credentialDial(ctx, cid); err == nil {
		t.Fatalf("the replaced key still authenticates")
	}
	t.Setenv(cli.CredentialFileEnv, newOut)
	if r := whoami(t, cid); r.PrincipalTaskId != cred.TaskID {
		t.Fatalf("the re-keyed credential does not authenticate")
	}
	t.Setenv(cli.CredentialFileEnv, "")
	// A rekey names its cause when the target is not a credential / not in reach.
	other := hex.EncodeToString(bytes.Repeat([]byte{0x77}, 16))
	err = cli.CredentialIssueWith(ctx, op, verb.CredentialIssueAction{Sub: verb.SubRekey, ID: other, TTL: "1d", Out: filepath.Join(dir, "z")}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no such credential within your reach") {
		t.Fatalf("rekey of an unknown id: %v", err)
	}

	// --- revoke: ends it; the next handshake is refused `revoked`, fatally.
	out.Reset()
	if err := cli.CredentialRevokeWith(ctx, op, idHex, &out); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if !strings.Contains(out.String(), "revoked") {
		t.Fatalf("revoke output: %q", out.String())
	}
	t.Setenv(cli.CredentialFileEnv, newOut)
	_, err = credentialDial(ctx, cid)
	var fatal *cli.PSKAuthError
	if err == nil || !strings.Contains(err.Error(), "revoked") || !errors.As(err, &fatal) {
		t.Fatalf("dial with a revoked credential: %v (want a fatal PSKAuthError naming revoked)", err)
	}
	t.Setenv(cli.CredentialFileEnv, "")
	// Revoke is a credential verb: it will not cancel another kind of task.
	taskID := s.Tasks().Create("repo", "", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{}, "")
	if err := cli.CredentialRevokeWith(ctx, op, taskID, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "no such credential within your reach") {
		t.Fatalf("revoke of an ordinary task: %v", err)
	}
	if e, _ := s.Tasks().Get(taskID); e.Status == protocol.TaskStatus_Cancelled {
		t.Fatalf("`credential revoke` cancelled an ordinary task")
	}
}

func capsPtr(c protocol.Capability) *protocol.Capability { return &c }

// keygen: the seed goes to a 0600 file, the public key to the output.
func TestCredentialKeygen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seed")
	var out bytes.Buffer
	if err := cli.CredentialKeygenWith(verb.CredentialKeygenAction{Out: path}, &out); err != nil {
		t.Fatal(err)
	}
	pub, err := hex.DecodeString(strings.TrimSpace(out.String()))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		t.Fatalf("keygen printed %q, want the 64-hex public key", out.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(seed) != ed25519.SeedSize {
		t.Fatalf("the seed file holds %d bytes, want a 64-hex seed", len(data))
	}
	if !bytes.Equal(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey), pub) {
		t.Fatalf("the printed public key is not the seed file's")
	}
	if strings.Contains(out.String(), strings.TrimSpace(string(data))) {
		t.Fatalf("keygen printed the seed")
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Fatalf("seed file mode = %v, want 0600", fi.Mode().Perm())
		}
	}
	if err := cli.CredentialKeygenWith(verb.CredentialKeygenAction{Out: path}, &bytes.Buffer{}); err == nil {
		t.Fatalf("keygen overwrote an existing seed file")
	}
}
