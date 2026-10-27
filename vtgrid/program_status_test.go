package vtgrid

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestParseProgramStatus(t *testing.T) {
	ok := func(body string, want ProgramStatus) {
		t.Helper()
		got, query, valid := ParseProgramStatus(body)
		if !valid || query || got != want {
			t.Errorf("ParseProgramStatus(%q) = %+v query=%v ok=%v, want %+v", body, got, query, valid, want)
		}
	}
	bad := func(body string) {
		t.Helper()
		if _, query, valid := ParseProgramStatus(body); valid || query {
			t.Errorf("ParseProgramStatus(%q) accepted, want it ignored", body)
		}
	}
	ok("state=blocked:kind=question:msg=cT8=", ProgramStatus{State: "blocked", Kind: "question", Msg: "q?"})
	ok("state=working", ProgramStatus{State: "working"})
	// kind only travels with blocked; padding is optional.
	ok("state=working:kind=question:msg=cT8", ProgramStatus{State: "working", Msg: "q?"})
	// clear removes the root record: the zero status, accepted.
	ok("state=clear", ProgramStatus{})
	// Whitespace around keys and values is removed; an unknown kind is absent.
	ok(" state = done : kind=nope", ProgramStatus{State: "done"})

	if _, query, valid := ParseProgramStatus("?"); !query || valid {
		t.Errorf("the probe was not recognised as a query")
	}
	bad("state=working:id=a/b") // a child record: only the root is modelled
	bad("state=bogus")
	bad("kind=question")                                                            // no state
	bad("state=blocked:msg=" + base64.StdEncoding.EncodeToString([]byte("a\x1bb"))) // control char
	bad("state=blocked:msg=A")                                                      // in the value charset, but not decodable base64
	// A byte outside the value charset skips that pair only; the rest stands.
	ok("state=blocked:msg=***", ProgramStatus{State: "blocked"})
	bad("state=working:msg=" + strings.Repeat("A", 2733)) // over the encoded cap
	bad("state=working:" + strings.Repeat("x", 4100))     // over the sequence cap
}

func TestTerminal_ProgramStatusAcrossWrites(t *testing.T) {
	term := New(80, 24)
	term.Write([]byte("\x1b]7501;state=wor"))
	term.Write([]byte("king\x07"))
	if got := term.ProgramStatus(); got.State != "working" {
		t.Fatalf("after a split report: %+v, want working", got)
	}
	term.Write([]byte("\x1b]7501;state=bogus\x07"))
	if got := term.ProgramStatus(); got.State != "working" {
		t.Fatalf("an invalid report changed the status: %+v", got)
	}
	term.Write([]byte("\x1bc")) // RIS removes every record
	if got := term.ProgramStatus(); got != (ProgramStatus{}) {
		t.Fatalf("after RIS: %+v, want none", got)
	}
}

// The repaint carries the program status for the reason it carries the title:
// a program reports a state once, on a change, so a terminal rebuilt from the
// repaint alone — the next server after a held restart, a client whose ring no
// longer holds the report — would otherwise show none until the next change.
// Reconstructing through a FRESH Terminal is the point.
func TestRepaintCarriesProgramStatus(t *testing.T) {
	for _, want := range []ProgramStatus{
		{State: "working"},
		{State: "blocked", Kind: "question", Msg: "Tea or coffee? 紅茶"},
	} {
		src := New(80, 24)
		msg := ""
		if want.Msg != "" {
			msg = ":msg=" + base64.StdEncoding.EncodeToString([]byte(want.Msg))
		}
		src.Write([]byte("\x1b]7501;state=" + want.State + ":kind=" + want.Kind + msg + ":app=claude-code\x1b\\screen"))
		if got := src.ProgramStatus(); got != want {
			t.Fatalf("precondition: %+v, want %+v", got, want)
		}
		dst := New(80, 24)
		dst.Write(src.Repaint())
		if got := dst.ProgramStatus(); got != want {
			t.Errorf("after repaint: %+v, want %+v", got, want)
		}
	}
}

// No record, no OSC: "never reported" and "cleared" read the same here, and
// leaving an observer's status alone is the safer reading, as for the title.
func TestRepaintOmitsAbsentProgramStatus(t *testing.T) {
	src := New(80, 24)
	src.Write([]byte("\x1b]7501;state=working\x07\x1b]7501;state=clear\x07plain"))
	if rp := src.Repaint(); strings.Contains(string(rp), "7501") {
		t.Errorf("repaint without a status carries 7501: %q", rp)
	}
}

func TestTerminal_ProbeCounted(t *testing.T) {
	term := New(80, 24)
	term.Write([]byte("\x1b]7501;?\x1b\\x\x1b]7501;?\x07"))
	if n := term.TakeProgramStatusQueries(); n != 2 {
		t.Fatalf("queries = %d, want 2", n)
	}
	if n := term.TakeProgramStatusQueries(); n != 0 {
		t.Fatalf("queries after take = %d, want 0", n)
	}
	if got := term.ProgramStatus(); got != (ProgramStatus{}) {
		t.Fatalf("a probe set a status: %+v", got)
	}
}
