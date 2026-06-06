package cli

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestParseRemoteForwardSpec(t *testing.T) {
	got, err := ParseRemoteForwardSpec("8080:localhost:3000")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.BindAddr != "127.0.0.1" || got.RunnerPort != 8080 || got.DialHost != "localhost" || got.DialPort != 3000 {
		t.Fatalf("got %+v", got)
	}
	got2, err := ParseRemoteForwardSpec("0.0.0.0:8080:localhost:3000")
	if err != nil || got2.BindAddr != "0.0.0.0" || got2.RunnerPort != 8080 || got2.DialPort != 3000 {
		t.Fatalf("bind form: %+v err=%v", got2, err)
	}
	for _, bad := range []string{"nope", "8080:localhost", "x:localhost:3000", "8080:localhost:y", "8080::3000"} {
		if _, err := ParseRemoteForwardSpec(bad); err == nil {
			t.Fatalf("expected error on %q", bad)
		}
	}
}

func TestParseConnNotifies_Framing(t *testing.T) {
	mk := func(id uint64) []byte {
		n := protocol.RemoteForwardConnNotify{StreamId: id}
		b, err := n.Append(nil)
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		return b
	}

	// Two notifies coalesced into one buffer → both parsed, no remainder.
	two := append(mk(22), mk(33)...)
	ids, rest := parseConnNotifies(append([]byte{}, two...))
	if len(ids) != 2 || ids[0] != 22 || ids[1] != 33 || len(rest) != 0 {
		t.Fatalf("coalesced: ids=%v rest=%d", ids, len(rest))
	}

	// A partial notify → nothing parsed, remainder preserved.
	one := mk(11)
	ids, rest = parseConnNotifies(one[:4])
	if len(ids) != 0 || len(rest) != 4 {
		t.Fatalf("partial: ids=%v rest=%d", ids, len(rest))
	}

	// Partial then completed → parsed once.
	rejoined := append(append([]byte{}, one[:4]...), one[4:]...)
	ids, rest = parseConnNotifies(rejoined)
	if len(ids) != 1 || ids[0] != 11 || len(rest) != 0 {
		t.Fatalf("rejoined: ids=%v rest=%d", ids, len(rest))
	}
}
