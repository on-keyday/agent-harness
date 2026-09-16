package cli

import (
	"testing"
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

// TestParseRemoteForwardSpec_BarePort pins the one-element shorthand as the
// MIRROR of the -L one: same port both ends, loopback on both, so an operator
// who learned `-L 3000` does not have to learn a second rule for `-R 3000`.
// `8080:3000` stays in the bad list above on purpose — two elements is the
// shape of a `-W host:port` target, and letting it mean a port pair here would
// make one string mean two things depending on which flag carried it.
func TestParseRemoteForwardSpec_BarePort(t *testing.T) {
	got, err := ParseRemoteForwardSpec("8080")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.BindAddr != "127.0.0.1" || got.RunnerPort != 8080 ||
		got.DialHost != "127.0.0.1" || got.DialPort != 8080 || got.DialNetwork != "tcp" {
		t.Fatalf("got %+v", got)
	}
	for _, bad := range []string{"0", "70000", "-1", ""} {
		if _, err := ParseRemoteForwardSpec(bad); err == nil {
			t.Fatalf("expected error on %q", bad)
		}
	}
}
