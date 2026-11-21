package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/objtrsf/objproto"
)

// dispatchCredential runs one `credential ...` line through the generated CLI
// dispatcher, the way main does, with a connection function that fails the
// test if a verb tries to dial (these lines are refused or served locally).
func dispatchCredential(t *testing.T, line ...string) (err error, handled bool) {
	t.Helper()
	h := cliVerbs{ctx: context.Background(), cid: func() objproto.ConnectionID {
		t.Fatalf("`%s` dialed the server", strings.Join(line, " "))
		return objproto.ConnectionID{}
	}}
	err, handled, perr := verb.DispatchCLILine[error](h, line, nil)
	if perr != nil {
		return perr, handled
	}
	return err, handled
}

// The operator's argv, flags after the positional (pitfall 13): a verb whose
// parser stops at the first non-flag token never reads --ttl.
func TestCredentialVerbsReadFlagsAfterTheirPositional(t *testing.T) {
	const id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	err, handled := dispatchCredential(t, "credential", "rekey", id, "--ttl", "0", "--out", filepath.Join(t.TempDir(), "x"))
	if !handled {
		t.Fatalf("`credential rekey` is not a CLI verb")
	}
	if err == nil || !strings.Contains(err.Error(), "--ttl") || !strings.Contains(err.Error(), "90d") {
		t.Fatalf("`credential rekey <id> --ttl 0` did not reach the ttl check (a flag after the positional is unread?): %v", err)
	}
	err, _ = dispatchCredential(t, "credential", "new", "--ttl", "1d", "--out", "a", "--pubkey", strings.Repeat("0a", 32))
	if err == nil || !strings.Contains(err.Error(), "--out") {
		t.Fatalf("--out with --pubkey was not refused: %v", err)
	}
	err, _ = dispatchCredential(t, "credential", "new", "--out", "a")
	if err == nil || !strings.Contains(err.Error(), "ttl") {
		t.Fatalf("`credential new` without --ttl was not refused: %v", err)
	}
}

// keygen is served on this host and dials nothing.
func TestCredentialKeygenThroughTheDispatcher(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seed")
	err, handled := dispatchCredential(t, "credential", "keygen", "--out", path)
	if !handled || err != nil {
		t.Fatalf("keygen: handled=%v err=%v", handled, err)
	}
	fi, serr := os.Stat(path)
	if serr != nil {
		t.Fatal(serr)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("seed file mode = %v, want 0600", fi.Mode().Perm())
	}
}

// Every credential verb is declared, CLI-reachable and printed by usage.
func TestCredentialVerbsAreInTheCLI(t *testing.T) {
	for _, p := range []string{"credential keygen", "credential new", "credential rekey", "credential ls", "credential revoke"} {
		sp, ok := verb.Lookup(strings.Fields(p)...)
		if !ok || !sp.CmdlineSurfaces.Has(verb.CLI) {
			t.Errorf("%s is not a CLI verb", p)
		}
		if !strings.Contains(usageText(), p[len("credential "):]) {
			t.Errorf("usage does not mention %s", p)
		}
	}
	if !strings.Contains(usageText(), "--credential-file") {
		t.Errorf("usage does not describe --credential-file")
	}
}
