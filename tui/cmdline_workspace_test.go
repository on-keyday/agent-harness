package tui

import (
	"testing"

	"github.com/on-keyday/agent-harness/cli/verb"
)

func TestParseWorkspace(t *testing.T) {
	for _, c := range []struct {
		in   string
		sub  string
		name string
	}{
		{"workspace apply", "apply", ""},
		{"workspace apply default", "apply", "default"},
		{"workspace save default", "save", "default"},
		{"workspace ls", "ls", ""},
		{"workspace show", "show", ""},
		{"workspace show default", "show", "default"},
	} {
		act, err := ParseCommand(c.in, "")
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		wa, ok := act.(verb.WorkspaceAction)
		if !ok {
			t.Fatalf("%q: got %T, want WorkspaceAction", c.in, act)
		}
		if wa.Sub != c.sub || wa.Name != c.name {
			t.Errorf("%q: got %+v, want sub=%q name=%q", c.in, wa, c.sub, c.name)
		}
	}
	// `workspace save` with no name is refused on purpose: saving writes the
	// file from live client state, and defaulting the name would let a slip
	// overwrite the wrong workspace.
	for _, bad := range []string{"workspace", "workspace nope", "workspace save", "workspace apply a b"} {
		if _, err := ParseCommand(bad, ""); err == nil {
			t.Errorf("%q parsed, want an error", bad)
		}
	}
}

// The grid selection must survive being rendered into a workspace and parsed
// back — a save writes what an operator could have typed after `grid`.
func TestGridArgsStringRoundTripsThroughTheApp(t *testing.T) {
	a := New(Config{})
	a.gridSelMode, a.gridSelAnchor = "subtree", wsTaskA
	act, err := ParseCommand("grid "+a.gridArgsString(), "")
	if err != nil {
		t.Fatalf("grid %q: %v", a.gridArgsString(), err)
	}
	ga, ok := act.(GridAction)
	if !ok {
		t.Fatalf("got %T, want GridAction", act)
	}
	if string(ga.Mode) != "subtree" || ga.Anchor != wsTaskA {
		t.Errorf("round-tripped as %v/%q", ga.Mode, ga.Anchor)
	}
}
