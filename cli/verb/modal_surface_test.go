package verb

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// A declaration that names an entry point is only worth more than a comment if
// something checks the entry point is still there. This walks every
// ModalSurfaces row and resolves its At.
//
// What this canNOT do is detect an UNDECLARED modal: nothing here can tell that
// a new TUI panel exists and went unrecorded. It catches the other direction —
// a widget renamed or deleted while the table still points at it — which is the
// failure a stale map produces after the map is finally written.
func TestModalSurfaceEntryPointsExist(t *testing.T) {
	for _, v := range Verbs {
		for _, ms := range v.ModalSurfaces {
			path, needle, kind := splitEntryPoint(t, v.Path, ms.At)
			b, err := os.ReadFile("../../" + path)
			if err != nil {
				t.Errorf("%v: ModalSurfaces At %q: %v", v.Path, ms.At, err)
				continue
			}
			if !strings.Contains(string(b), needle) {
				t.Errorf("%v: ModalSurfaces At %q: %s %q is not in %s any more —\n"+
					"  the surface was renamed or removed, or the entry point was mistyped",
					v.Path, ms.At, kind, needle, path)
			}
			if ms.Surface == 0 {
				t.Errorf("%v: ModalSurfaces %q names no surface", v.Path, ms.At)
			}
		}
	}
}

// splitEntryPoint accepts "file.go:Symbol" and "page.html#element-id". The two
// spellings exist because the two surfaces are found in different ways: a Go
// symbol by name, a page element by its id attribute.
func splitEntryPoint(t *testing.T, verbPath []string, at string) (path, needle, kind string) {
	t.Helper()
	if i := strings.LastIndex(at, "#"); i > 0 {
		return at[:i], fmt.Sprintf("id=%q", at[i+1:]), "element id"
	}
	if i := strings.LastIndex(at, ":"); i > 0 {
		return at[:i], at[i+1:], "symbol"
	}
	t.Errorf("%v: ModalSurfaces At %q has no ':Symbol' or '#element-id'", verbPath, at)
	return at, at, "?"
}

// The two axes answer different questions and a verb may be on either, both, or
// neither. What must never happen is a verb reachable NOWHERE, which is a row
// nobody can use.
func TestEveryVerbIsReachableSomehow(t *testing.T) {
	for _, v := range Verbs {
		if v.CmdlineSurfaces == 0 && len(v.ModalSurfaces) == 0 {
			t.Errorf("%v is on no cmdline and has no modal surface: nothing can reach it", v.Path)
		}
	}
}

// Every verb must carry a VERDICT about its non-cmdline surfaces: either at
// least one ModalSurfaces row, or a NoModalSurface reason saying it was looked
// for and is not there.
//
// This is the completeness claim the other test cannot make. Nothing can detect
// an UNDECLARED modal, so "the table is complete" is unprovable — but "somebody
// has looked at every row" is checkable, and it is what makes a blank row
// meaningful. Before this, an empty ModalSurfaces meant either "surveyed, none"
// or "nobody has been here", and a reader could not tell which. That ambiguity
// is the whole reason `conns` was called CLI-only while two of its surfaces sat
// in the tree.
func TestEveryVerbHasASurfaceVerdict(t *testing.T) {
	for _, v := range Verbs {
		if len(v.ModalSurfaces) == 0 && strings.TrimSpace(v.NoModalSurface) == "" {
			t.Errorf("%v has no verdict about non-cmdline surfaces.\n"+
				"  Look for a TUI action or a WebUI element. `grep -rl DoThing tui/*.go`:\n"+
				"  dispatch.go only means the cmdline, which CmdlineSurfaces already covers.\n"+
				"  Then either add a ModalSurfaces row, or set NoModalSurface to what you found.",
				v.Path)
		}
	}
}

// The two answers are exclusive. A verb that declares a surface AND says there
// is none has been edited twice and read once.
func TestNoVerbBothDeclaresAndDeniesASurface(t *testing.T) {
	for _, v := range Verbs {
		if len(v.ModalSurfaces) > 0 && strings.TrimSpace(v.NoModalSurface) != "" {
			t.Errorf("%v declares %d modal surface(s) and also says there are none: %q",
				v.Path, len(v.ModalSurfaces), v.NoModalSurface)
		}
	}
}
