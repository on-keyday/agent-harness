//go:build !js

package main

import (
	"flag"
	"testing"

	"github.com/on-keyday/agent-harness/cli"
)

// TestForwardSubcommandRouting: "ls" and "kill" are not hex, so they can
// never collide with a task id.
func TestForwardSubcommandRouting(t *testing.T) {
	for _, sub := range []string{"ls", "kill", "tap"} {
		if isTaskIDLike(sub) {
			t.Errorf("%q must not parse as a task id", sub)
		}
	}
	for _, id := range []string{"deadbeef", "0123456789abcdef0123456789abcdef"} {
		if !isTaskIDLike(id) {
			t.Errorf("%q should parse as a task id", id)
		}
	}
}

// TestForwardWFlagRouting drives the same flag.FlagSet shape the "forward"
// case builds (see main.go's `case "forward":`), without spawning a process:
// -W alone must parse into a usable host:port, and -W combined with -L must
// be flagged as a conflict by forwardWConflictsWithLR — the exact check
// main.go runs right after fs.Parse to reject a `-W` + `-L`/`-R` invocation
// with exit code 2.
func TestForwardWFlagRouting(t *testing.T) {
	newForwardFlagSet := func() (*flag.FlagSet, *repeatableStrings, *repeatableStrings, *string) {
		fs := flag.NewFlagSet("forward", flag.ContinueOnError)
		var specs repeatableStrings
		var rspecs repeatableStrings
		fs.Var(&specs, "L", "")
		fs.Var(&rspecs, "R", "")
		wspec := fs.String("W", "", "")
		return fs, &specs, &rspecs, wspec
	}

	// -W alone: parses, and does not conflict with -L/-R.
	fs, specs, rspecs, wspec := newForwardFlagSet()
	if err := fs.Parse([]string{"-W", "127.0.0.1:3000"}); err != nil {
		t.Fatalf("parse -W alone: %v", err)
	}
	if forwardWConflictsWithLR(*wspec, len(*specs), len(*rspecs)) {
		t.Fatalf("-W alone must not be reported as conflicting")
	}
	host, port, err := cli.ParseStdioForwardSpec(*wspec)
	if err != nil || host != "127.0.0.1" || port != 3000 {
		t.Fatalf("ParseStdioForwardSpec(%q) = %q, %d, err=%v", *wspec, host, port, err)
	}

	// -W combined with -L must be rejected.
	fs2, specs2, rspecs2, wspec2 := newForwardFlagSet()
	if err := fs2.Parse([]string{"-L", "3000:127.0.0.1:3000", "-W", "127.0.0.1:3000"}); err != nil {
		t.Fatalf("parse -L + -W: %v", err)
	}
	if !forwardWConflictsWithLR(*wspec2, len(*specs2), len(*rspecs2)) {
		t.Fatalf("-W combined with -L must be reported as conflicting")
	}

	// -W combined with -R must also be rejected.
	fs3, specs3, rspecs3, wspec3 := newForwardFlagSet()
	if err := fs3.Parse([]string{"-R", "3000:127.0.0.1:3000", "-W", "127.0.0.1:3000"}); err != nil {
		t.Fatalf("parse -R + -W: %v", err)
	}
	if !forwardWConflictsWithLR(*wspec3, len(*specs3), len(*rspecs3)) {
		t.Fatalf("-W combined with -R must be reported as conflicting")
	}
}

// "tap" joins "ls" and "kill" as a sub-verb, so it must not parse as a task id
// either — the routing above dispatches on exactly that test.
func TestForwardTapSubcommandRouting(t *testing.T) {
	if isTaskIDLike("tap") {
		t.Error(`"tap" must not parse as a task id`)
	}
}

func TestTapModeRejectsTwoAtOnce(t *testing.T) {
	if _, err := tapMode(true, false, true, false); err == nil {
		t.Fatal("--hex with --raw must be refused, not silently ranked")
	}
	m, err := tapMode(false, false, false, false)
	if err != nil || m != cli.TapHex {
		t.Fatalf("no flag = hex, got %v %v", m, err)
	}
	m, err = tapMode(false, false, false, true)
	if err != nil || m != cli.TapJSON {
		t.Fatalf("--json, got %v %v", m, err)
	}
}
