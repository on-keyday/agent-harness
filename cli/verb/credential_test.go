package verb

import (
	"strings"
	"testing"
	"time"
)

// DUR is a Go duration, plus a `d` suffix for days. The server bounds it
// (90 days); the client only has to say what was typed.
func TestDurationWithDaySuffix(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"30d":    30 * 24 * time.Hour,
		"90d":    90 * 24 * time.Hour,
		"91d":    91 * 24 * time.Hour, // parses; the server refuses it
		"1d":     24 * time.Hour,
		"12h":    12 * time.Hour,
		"90m":    90 * time.Minute,
		"1d12h":  36 * time.Hour,
		"0.5d":   12 * time.Hour,
		"1h30m":  90 * time.Minute,
		"1500ms": 1500 * time.Millisecond,
	} {
		got, err := ParseCredentialTTL(in)
		if err != nil || got != want {
			t.Errorf("%q: got (%v, %v), want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0", "0d", "0s", "-1h", "-3d", "d", "xd", "30 d", "1w", "NaNd", "9223372036854775807d", "1e3d"} {
		_, err := ParseCredentialTTL(in)
		if err == nil {
			t.Errorf("%q accepted", in)
			continue
		}
		if !strings.Contains(err.Error(), "90d") {
			t.Errorf("%q: the refusal does not name the 90-day bound: %v", in, err)
		}
	}
}

func TestCredentialVerbsParseWithFlagsAfterPositionals(t *testing.T) {
	const id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pub := strings.Repeat("0a", 32)

	a, err := ParseCmdCredentialRekey(CLI, []string{id, "--ttl", "30d", "--pubkey", pub}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != id || a.TTL != "30d" || a.Pubkey != pub || a.Sub != SubRekey {
		t.Fatalf("rekey parsed as %+v", a)
	}
	n, err := ParseCmdCredentialNew(CLI, []string{"--ttl", "7d", "--name", "ci", "--caps", "schedule_run", "--out", "f"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n.Name != "ci" || n.Out != "f" || n.Caps == nil || n.Sub != SubNew {
		t.Fatalf("new parsed as %+v", n)
	}
	r, err := ParseCmdCredentialRevoke(CLI, []string{id}, nil)
	if err != nil || r.ID != id {
		t.Fatalf("revoke: %+v %v", r, err)
	}
	l, err := ParseCmdCredentialLs(CLI, []string{"--json"}, nil)
	if err != nil || !l.JSON {
		t.Fatalf("ls: %+v %v", l, err)
	}
	k, err := ParseCmdCredentialKeygen(CLI, []string{"--out", "seed"}, nil)
	if err != nil || k.Out != "seed" {
		t.Fatalf("keygen: %+v %v", k, err)
	}
}

func TestCredentialVerbRefusals(t *testing.T) {
	const id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pub := strings.Repeat("0a", 32)
	for name, args := range map[string][]string{
		"new without ttl":        {"--out", "f"},
		"new --out and --pubkey": {"--ttl", "1d", "--out", "f", "--pubkey", pub},
		"new bad ttl":            {"--ttl", "soon"},
		"new zero ttl":           {"--ttl", "0"},
		"new short pubkey":       {"--ttl", "1d", "--pubkey", "abcd"},
		"new non-hex pubkey":     {"--ttl", "1d", "--pubkey", strings.Repeat("zz", 32)},
	} {
		if _, err := ParseCmdCredentialNew(CLI, args, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ParseCmdCredentialRekey(CLI, []string{"--ttl", "1d"}, nil); err == nil {
		t.Errorf("rekey without an id accepted")
	}
	if _, err := ParseCmdCredentialRekey(CLI, []string{"nothex", "--ttl", "1d"}, nil); err == nil {
		t.Errorf("rekey with a malformed id accepted")
	}
	if _, err := ParseCmdCredentialRekey(CLI, []string{id}, nil); err == nil {
		t.Errorf("rekey without ttl accepted")
	}
	if _, err := ParseCmdCredentialKeygen(CLI, nil, nil); err == nil {
		t.Errorf("keygen without --out accepted")
	}
}
