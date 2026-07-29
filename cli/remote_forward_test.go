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
