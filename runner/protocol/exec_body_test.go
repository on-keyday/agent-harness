package protocol

import (
	"strings"
	"testing"
)

// The envelopes no longer grow with the command: with every fixed field at its
// largest they stay inside trsf's floor (1170 bytes), the smallest budget any
// UDP path gets. The argv lives in the BODY, so the envelope cannot carry it.
func TestExecEnvelopesStaySmallForALongCommand(t *testing.T) {
	var c ExecRunRequest
	c.SetPty(true)
	c.PayloadStreamId = 1 << 40
	cb, err := c.Append(nil)
	if err != nil {
		t.Fatal(err)
	}
	var r RunnerExecRunRequest
	r.SetPty(true)
	r.ExecId, r.StreamId, r.BodyStreamId = 1<<40, 1<<40, 1<<40
	rb, err := r.Append(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cb) > 1170 || len(rb) > 1170 {
		t.Fatalf("envelopes are %d / %d bytes, want <= 1170", len(cb), len(rb))
	}
}

func TestExecRunBodyRoundTripsALongArgvAndTerm(t *testing.T) {
	var b ExecRunBody
	var one ExecArg
	one.SetArg([]byte(strings.Repeat("x", 4096)))
	b.Argv.Argv = []ExecArg{one}
	b.Argv.ArgvLen = 1
	b.SetTerm([]byte("xterm-256color"))
	raw, err := b.EncodeCopy(nil)
	if err != nil {
		t.Fatal(err)
	}
	var out ExecRunBody
	if err := out.DecodeExactCopy(raw); err != nil {
		t.Fatal(err)
	}
	if len(out.Argv.Argv[0].Arg) != 4096 || string(out.Term) != "xterm-256color" {
		t.Fatalf("round trip lost data: argv0=%d term=%q", len(out.Argv.Argv[0].Arg), out.Term)
	}
}
