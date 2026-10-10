package protocol

import (
	"strings"
	"testing"
)

// TERM becomes one environment variable on the runner. The alphabet keeps it
// one: no space, no control byte, nothing a shell or a terminfo lookup would
// read as something else.
func TestValidateExecTerm(t *testing.T) {
	for _, ok := range []string{"", "xterm-256color", "screen.xterm", "vt100"} {
		if err := ValidateExecTerm(ok); err != nil {
			t.Errorf("ValidateExecTerm(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"xterm 256", "xterm\n", "x\x00", "\x7f", strings.Repeat("x", 256)} {
		if err := ValidateExecTerm(bad); err == nil {
			t.Errorf("ValidateExecTerm(%q) = nil, want a refusal", bad)
		}
	}
}

// pty is declared AFTER sshd_parent. A flag declared earlier shifts every bit
// behind it, and an old peer then reads one flag as another — a clean decode
// that means something else.
func TestExecRunRequestPtyDoesNotDisturbTheOlderFlags(t *testing.T) {
	var body ExecRunRequest
	body.SetPty(true)
	if body.StdinEnabled() || body.ShellLine() || body.SshdParent() {
		t.Fatalf("setting pty moved an older flag: stdin=%v shell=%v sshd=%v",
			body.StdinEnabled(), body.ShellLine(), body.SshdParent())
	}
	body.SetStdinEnabled(true)
	body.SetShellLine(true)
	body.SetSshdParent(true)
	if !body.Pty() {
		t.Fatal("setting the older flags cleared pty")
	}
}

func TestExecRunBodyRoundTripsTerm(t *testing.T) {
	in := ExecRunBody{}
	in.SetTerm([]byte("xterm-256color"))
	b, err := in.EncodeCopy(nil)
	if err != nil {
		t.Fatal(err)
	}
	var out ExecRunBody
	if err := out.DecodeExactCopy(b); err != nil {
		t.Fatal(err)
	}
	if string(out.Term) != "xterm-256color" {
		t.Fatalf("round trip: term=%q", out.Term)
	}
}

func TestExecRunInfoCarriesPty(t *testing.T) {
	var in ExecRunInfo
	in.SetPty(true)
	b, err := in.Append(nil)
	if err != nil {
		t.Fatal(err)
	}
	var out ExecRunInfo
	if err := out.DecodeExactCopy(b); err != nil {
		t.Fatal(err)
	}
	if !out.Pty() {
		t.Fatal("ExecRunInfo.pty did not survive a round trip")
	}
}
