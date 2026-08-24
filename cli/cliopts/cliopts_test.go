package cliopts

import (
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

func TestResolveServerCID_FlagWinsOverEnv(t *testing.T) {
	t.Setenv("HARNESS_SERVER_CID", "ws:127.0.0.1:1-1")
	cid, err := ResolveServerCID("ws:127.0.0.1:2-2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(cid.String(), "-2") {
		t.Errorf("flag should win, got %s", cid.String())
	}
}

func TestResolveServerCID_FallsBackToEnv(t *testing.T) {
	t.Setenv("HARNESS_SERVER_CID", "ws:127.0.0.1:1-3")
	cid, err := ResolveServerCID("")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(cid.String(), "-3") {
		t.Errorf("env should be used when flag empty, got %s", cid.String())
	}
}

func TestResolveServerCID_ErrorWhenMissing(t *testing.T) {
	os.Unsetenv("HARNESS_SERVER_CID")
	if _, err := ResolveServerCID(""); err == nil {
		t.Error("expected error when both flag and env empty")
	}
}

func TestResolveAuthTicket_EnvOnly(t *testing.T) {
	var want [16]byte
	for i := range want {
		want[i] = byte(i)
	}
	t.Setenv("HARNESS_AUTH_TICKET", hex.EncodeToString(want[:]))
	got, err := ResolveAuthTicket()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("ticket = %x, want %x", got, want)
	}
}

func TestResolveAuthTicket_RejectsBadHex(t *testing.T) {
	t.Setenv("HARNESS_AUTH_TICKET", "not-hex")
	if _, err := ResolveAuthTicket(); err == nil {
		t.Error("expected error on invalid hex")
	}
}

// The workspace tier is consulted only when neither the flag nor the env
// supplied a value. A flag default baked into flag.String would defeat this,
// which is why harness-tui's connection flags default to empty.
func TestResolveStringWith(t *testing.T) {
	t.Setenv("HARNESS_TEST_RSW", "")
	if got := ResolveStringWith("flag", "HARNESS_TEST_RSW", "file"); got != "flag" {
		t.Errorf("flag should win: got %q", got)
	}
	t.Setenv("HARNESS_TEST_RSW", "env")
	if got := ResolveStringWith("", "HARNESS_TEST_RSW", "file"); got != "env" {
		t.Errorf("env should beat the file: got %q", got)
	}
	t.Setenv("HARNESS_TEST_RSW", "")
	if got := ResolveStringWith("", "HARNESS_TEST_RSW", "file"); got != "file" {
		t.Errorf("file should be used when neither flag nor env is set: got %q", got)
	}
	if got := ResolveStringWith("", "HARNESS_TEST_RSW", ""); got != "" {
		t.Errorf("all empty should stay empty: got %q", got)
	}
}
