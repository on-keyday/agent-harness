package main

import (
	"flag"
	"strings"
	"testing"
)

// TestListenFlagMutualExclusion verifies that providing both --server-cid
// (other than its default) and --listen returns an error from validate.
func TestListenFlagMutualExclusion(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg := newMainConfig()
	cfg.bindFlags(fs)

	if err := fs.Parse([]string{
		"--server-cid", "ws:127.0.0.1:8539-*",
		"--listen", "0.0.0.0:8540",
		"--roots", "/tmp",
	}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	cfg.serverCIDExplicit = true // simulate user-set (not default)
	err := cfg.validate()
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutually-exclusive error, got %v", err)
	}
}

// TestListenFlagRequiresOneOf verifies that providing neither --server-cid
// (when cleared) nor --listen/--udp-listen returns an error.
func TestListenFlagRequiresOneOf(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg := newMainConfig()
	cfg.bindFlags(fs)

	if err := fs.Parse([]string{
		"--server-cid", "",
		"--roots", "/tmp",
	}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	err := cfg.validate()
	if err == nil {
		t.Fatalf("expected error when neither --server-cid nor --listen provided")
	}
}

// TestListenOnlyMode verifies --listen alone (no --server-cid) is valid.
func TestListenOnlyMode(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg := newMainConfig()
	cfg.bindFlags(fs)

	if err := fs.Parse([]string{
		"--server-cid", "",
		"--listen", "0.0.0.0:8540",
		"--roots", "/tmp",
	}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !cfg.isListenMode() {
		t.Errorf("expected isListenMode() true with --listen set")
	}
}

// TestServerCIDOnlyMode verifies the legacy --server-cid path stays valid.
func TestServerCIDOnlyMode(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg := newMainConfig()
	cfg.bindFlags(fs)

	if err := fs.Parse([]string{
		"--server-cid", "ws:127.0.0.1:8539-*",
		"--roots", "/tmp",
	}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if cfg.isListenMode() {
		t.Errorf("expected isListenMode() false with only --server-cid set")
	}
}
