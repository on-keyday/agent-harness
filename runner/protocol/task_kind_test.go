package protocol

import "testing"

// Adding a TaskKind is the moment every "is this a session" comparison in the
// repo becomes a decision. These pin the two answers apart so a third kind
// cannot quietly inherit whichever one it was written next to.
func TestSessionAndPTYKindsAreNotTheSameQuestion(t *testing.T) {
	cases := []struct {
		kind         TaskKind
		session, pty bool
	}{
		{TaskKind_Oneshot, false, false},
		{TaskKind_Interactive, true, true},
		{TaskKind_Stream, true, false}, // a session, but there is no terminal
	}
	for _, c := range cases {
		if got := IsSessionKind(c.kind); got != c.session {
			t.Errorf("IsSessionKind(%v) = %v, want %v", c.kind, got, c.session)
		}
		if got := IsPTYKind(c.kind); got != c.pty {
			t.Errorf("IsPTYKind(%v) = %v, want %v", c.kind, got, c.pty)
		}
	}
	// The distinction has to actually distinguish, or both call sites could
	// use either and nothing would notice.
	if IsSessionKind(TaskKind_Stream) == IsPTYKind(TaskKind_Stream) {
		t.Error("the stream kind answers both questions the same way; then the " +
			"two predicates are one predicate and the grid will paint NDJSON")
	}
}

// An unknown kind from a newer peer must not read as a session: the enum
// decodes any byte, so a value this build does not know arrives as itself.
func TestUnknownKindIsNeitherSessionNorPTY(t *testing.T) {
	future := TaskKind(200)
	if IsSessionKind(future) || IsPTYKind(future) {
		t.Errorf("TaskKind(200) answered session=%v pty=%v; an unknown kind must "+
			"be treated as neither rather than defaulting into one",
			IsSessionKind(future), IsPTYKind(future))
	}
}
