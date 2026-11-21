//go:build !js

package cli_test

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// IssueCredential is the issue/re-key core the WebUI's wasm bridge calls (the
// browser has no file to write, so it cannot use CredentialIssueWith). Without
// --pubkey it generates the pair on the issuing side and hands back the one
// secret, `<32 hex id>:<64 hex seed>`, which authenticates as the new task;
// with --pubkey it hands back no secret at all.
func TestIssueCredentialHandsOverOneWorkingSecret(t *testing.T) {
	s, cid := startCredentialServer(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	op, err := credentialDial(ctx, cid)
	if err != nil {
		t.Fatal(err)
	}
	defer op.Close()
	_ = s

	caps := protocol.Capability_ScheduleRun
	h, err := cli.IssueCredential(ctx, op, verb.CredentialIssueAction{Sub: verb.SubNew, TTL: "1d", Name: "web", Caps: &caps, CapsPresent: true})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if h.Cred == nil {
		t.Fatal("the generate path handed back no credential")
	}
	secret := h.Cred.Secret()
	if !regexp.MustCompile(`^[0-9a-f]{32}:[0-9a-f]{64}$`).MatchString(secret) {
		t.Fatalf("the secret (%d bytes, not echoed) is not <32 hex>:<64 hex>", len(secret))
	}
	if h.Cred.TaskID != h.TaskID {
		t.Fatalf("the secret names %x, the issue answered %x", h.Cred.TaskID.Id, h.TaskID.Id)
	}
	line := cli.CredentialIssuedLine(h.CredentialIssued)
	if want := "credential " + hex.EncodeToString(h.TaskID.Id[:])[:8] + ": issued, expires "; !strings.HasPrefix(line, want) || strings.Contains(line, hex.EncodeToString(h.Cred.Key.Seed())) {
		t.Fatalf("issued line = %q", line)
	}

	t.Setenv(cli.CredentialEnv, secret)
	if r := whoami(t, cid); r.PrincipalTaskId != h.TaskID || r.Capabilities != caps {
		t.Fatalf("the handed-over secret is not the credential: principal %x caps %v", r.PrincipalTaskId.Id, cli.CapsLabel(r.Capabilities))
	}
	t.Setenv(cli.CredentialEnv, "")

	// --pubkey: the holder's key is registered and nothing secret comes back.
	pub, _, _ := ed25519.GenerateKey(nil)
	h2, err := cli.IssueCredential(ctx, op, verb.CredentialIssueAction{Sub: verb.SubNew, TTL: "1d", Pubkey: hex.EncodeToString(pub)})
	if err != nil {
		t.Fatalf("pubkey: %v", err)
	}
	if h2.Cred != nil {
		t.Fatal("the pubkey path handed back a credential it cannot have")
	}
	hint := cli.CredentialBindHint(hex.EncodeToString(h2.TaskID.Id[:]))
	if !strings.Contains(hint, "credential bind "+hex.EncodeToString(h2.TaskID.Id[:])+" --file ") {
		t.Fatalf("bind hint = %q", hint)
	}

	// re-key through the same core, generate path: a new working secret, same id.
	h3, err := cli.IssueCredential(ctx, op, verb.CredentialIssueAction{Sub: verb.SubRekey, ID: hex.EncodeToString(h.TaskID.Id[:]), TTL: "2h"})
	if err != nil {
		t.Fatalf("rekey: %v", err)
	}
	if h3.Cred == nil || h3.TaskID != h.TaskID || h3.Cred.Secret() == secret {
		t.Fatalf("rekey handover: id %x, new secret %v", h3.TaskID.Id, h3.Cred != nil)
	}
}

func TestCredentialTimeCell(t *testing.T) {
	if got := cli.CredentialTimeCell(0); got != "unknown" {
		t.Errorf("zero = %q, want unknown", got)
	}
	at := time.Date(2026, 12, 22, 6, 40, 0, 0, time.Local)
	if got := cli.CredentialTimeCell(uint64(at.UnixNano())); got != "2026-12-22 06:40" {
		t.Errorf("cell = %q", got)
	}
}

// A credential has no process and so no hooks: no lifecycle badge (`lc:-`
// would read "hooks that never fired"), on any surface that asks
// LifecycleShown.
func TestCredentialHasNoLifecycle(t *testing.T) {
	var tk protocol.TaskInfo
	tk.Kind = protocol.TaskKind_Credential
	tk.Status = protocol.TaskStatus_Active
	if cli.LifecycleShown(&tk) || cli.LifecycleBadge(&tk) != "" || cli.LifecycleStr(&tk) != "" {
		t.Fatalf("a live credential renders a lifecycle: badge %q str %q", cli.LifecycleBadge(&tk), cli.LifecycleStr(&tk))
	}
	tk.Kind = protocol.TaskKind_Interactive
	tk.Status = protocol.TaskStatus_Running
	if cli.LifecycleBadge(&tk) != "lc:-" {
		t.Fatalf("a live session that never reported lost its lc:- (%q)", cli.LifecycleBadge(&tk))
	}
}
