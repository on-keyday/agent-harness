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
