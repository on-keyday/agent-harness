package protocol

import (
	"encoding/hex"
	"fmt"
)

// RunnerID is 16 opaque bytes naming a runner PROCESS, so the only conversions
// it needs are to and from the hex an operator sees and types. There is no
// RunnerID-to-address conversion any more, by design: the two used to be the
// same value and every place that silently relied on it is a place that has to
// say which one it meant. WHERE a runner is is ConnID, and only the server can
// map one to the other (Registry.GetByIdentity).
//
// Deliberately named Hex rather than String: a bare String() on a wire type
// gets picked up by %v in log lines and format strings, and an identity that
// renders itself implicitly is how the old value ended up standing in for an
// address in the first place.

// Hex renders the identity as 32 lowercase hex characters, the form `ls` prints
// and `--runner` accepts. A zero RunnerID renders as 32 zeros rather than empty,
// so an absent identity is visible in a log line instead of vanishing.
func (r RunnerID) Hex() string {
	return hex.EncodeToString(r.Id[:])
}

// IsZero reports whether this is the zero identity — a runner that predates the
// field, or a value that was never filled in. Callers that must distinguish
// "absent" from "present" check this rather than comparing against a literal.
func (r RunnerID) IsZero() bool {
	return r == RunnerID{}
}

// RunnerIDFromHex parses the 32-hex form. The length check is explicit because
// hex.Decode accepts any even-length input, and a short id would otherwise be
// zero-padded into a DIFFERENT valid-looking identity.
func RunnerIDFromHex(s string) (RunnerID, error) {
	var r RunnerID
	if len(s) != 2*len(r.Id) {
		return r, fmt.Errorf("runner id %q: want %d hex characters, got %d", s, 2*len(r.Id), len(s))
	}
	raw, err := hex.DecodeString(s)
	if err != nil {
		return r, fmt.Errorf("runner id %q: %w", s, err)
	}
	copy(r.Id[:], raw)
	return r, nil
}
