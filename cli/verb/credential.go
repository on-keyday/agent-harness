package verb

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// credentialMaxTTLDays is the server's bound on a credential's lifetime
// (server.CredentialMaxTTL, spec D7), restated only to word the refusal of a
// ttl that cannot be a lifetime at all. The server alone decides whether a
// well-formed ttl is too long: `91d` parses here and is refused there.
const credentialMaxTTLDays = 90

// ParseCredentialTTL reads --ttl: a Go duration, or the same with a `d` suffix
// for days (`30d`, `1d12h`, `0.5d`). A credential always expires, so zero and
// negative are refused here with the bound named; whether the value exceeds
// the server's bound is the server's answer.
func ParseCredentialTTL(s string) (time.Duration, error) {
	bad := func(why string) (time.Duration, error) {
		return 0, fmt.Errorf("--ttl %q: %s (a credential expires: a positive duration of at most %dd, such as 30d, 12h or 1d12h)", s, why, credentialMaxTTLDays)
	}
	if strings.TrimSpace(s) != s || s == "" {
		return bad("not a duration")
	}
	d, err := parseDaysDuration(s)
	if err != nil {
		return bad("not a duration")
	}
	if d <= 0 {
		return bad("must be positive")
	}
	return d, nil
}

// parseDaysDuration is time.ParseDuration with `d` (24h) added as a unit.
// Days are split off the front: Go's own units are exact, and a day here is
// 24 hours with no calendar meaning.
func parseDaysDuration(s string) (time.Duration, error) {
	neg := strings.HasPrefix(s, "-")
	body := strings.TrimLeft(s, "+-")
	i := strings.IndexByte(body, 'd')
	if i < 0 {
		return time.ParseDuration(s)
	}
	num := body[:i]
	if num == "" || strings.ContainsAny(num, "eE+-") {
		return 0, fmt.Errorf("bad days")
	}
	days, err := strconv.ParseFloat(num, 64)
	if err != nil || days != days || days > 1e6 {
		return 0, fmt.Errorf("bad days")
	}
	d := time.Duration(days * float64(24*time.Hour))
	if rest := body[i+1:]; rest != "" {
		r, err := time.ParseDuration(rest)
		if err != nil || r < 0 || strings.HasPrefix(rest, "-") || strings.HasPrefix(rest, "+") {
			return 0, fmt.Errorf("bad remainder")
		}
		d += r
	}
	if neg {
		d = -d
	}
	return d, nil
}

// credentialAuthorityNames are the spawn grammar's authority flags, taken
// from spawnFlags as the same rows (never copies): a credential's caps and
// scope are written with the words `session new` uses.
var credentialAuthorityNames = []string{"caps", "scope", "scope-for", "delegate-caps", "delegate-scope", "spawn-depth", "max-descendants"}

func credentialAuthorityFlags() []Flag {
	var out []Flag
	all := spawnFlags(spawnSessionNew)
	for _, n := range credentialAuthorityNames {
		for _, f := range all {
			if f.Name == n {
				out = append(out, f)
			}
		}
	}
	return out
}

// credentialIssueFlags are `credential new` / `credential rekey`'s flags:
// the expiry, where the seed goes (or the public key to register), and the
// authority.
func credentialIssueFlags(isNew bool) []Flag {
	own := []Flag{
		{Name: "ttl", Type: FlagString, Default: "", Field: "TTL", Required: true,
			Help: "how long the credential lives, from now on the server's clock: a Go duration or days (`30d`); required, at most 90d"},
	}
	if isNew {
		own = append(own, Flag{Name: "name", Type: FlagString, Default: "", Field: "Name",
			Help: "a label shown in `credential ls` and `ls` (at most 64 bytes)"})
	}
	own = append(own,
		Flag{Name: "out", Type: FlagString, Default: "", Field: "Out",
			Help: "generate the key pair here and write the secret to this file (mode 0600; an existing file is never overwritten) instead of printing it"},
		Flag{Name: "pubkey", Type: FlagString, Default: "", Field: "Pubkey",
			Help: "register this Ed25519 public key (64 hex, from `credential keygen` on the host that will use it) instead of generating one; no secret is produced"},
	)
	return append(own, credentialAuthorityFlags()...)
}

func credentialIssueValidate(verbName string) func(Bound) error {
	return func(b Bound) error {
		if _, err := ParseCredentialTTL(b.Str("ttl")); err != nil {
			return fmt.Errorf("%s: %w", verbName, err)
		}
		if pk := b.Str("pubkey"); pk != "" {
			if raw, err := hex.DecodeString(pk); err != nil || len(raw) != 32 {
				return fmt.Errorf("%s: --pubkey: want 64 hex characters (an Ed25519 public key; `credential keygen` prints one)", verbName)
			}
		}
		if len(b.Args) > 0 {
			if raw, err := hex.DecodeString(b.Args[0]); err != nil || len(raw) != 16 {
				return fmt.Errorf("%s: credential id: want 32 hex characters", verbName)
			}
		}
		if n := b.Str("name"); len(n) > 64 {
			return fmt.Errorf("%s: --name is %d bytes, at most 64", verbName, len(n))
		}
		return nil
	}
}

// credentialSurfaceReason is why every credential verb is CLI-only for now.
const credentialSurfaceReason = "TUI and WebUI are added with their handlers in the surfaces task"
