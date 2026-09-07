package verb

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A declaration that names an entry point is only worth more than a comment if
// something checks the entry point is still there. This walks every
// ModalSurfaces row and resolves its At to a DEFINITION: a Go type or func of
// that name, a JS function of that name, an element with that id. A substring
// match used to be enough, and it let `forward ls` point at PortForwardModal —
// the prompt that OPENS a forward — while the list-and-kill pane it meant,
// ForwardsModal, sat in the same file; both spellings were "in the file".
//
// What this canNOT do is detect an UNDECLARED modal: nothing here can tell that
// a new TUI panel exists and went unrecorded. The TUI half of that direction is
// tui/surface_coverage_test.go, which walks the App's overlays and asks this
// table about each one.
func TestModalSurfaceEntryPointsExist(t *testing.T) {
	for _, v := range Verbs {
		for _, ms := range v.ModalSurfaces {
			path, name, kind := splitEntryPoint(t, v.Path, ms.At)
			b, err := os.ReadFile("../../" + path)
			if err != nil {
				t.Errorf("%v: ModalSurfaces At %q: %v", v.Path, ms.At, err)
				continue
			}
			if !definesEntryPoint(string(b), path, name, kind) {
				t.Errorf("%v: ModalSurfaces At %q: no %s %q is defined in %s —\n"+
					"  the surface was renamed or removed, the entry point was mistyped, "+
					"or it names something merely mentioned there rather than defined there",
					v.Path, ms.At, kind, name, path)
			}
			if ms.Surface == 0 {
				t.Errorf("%v: ModalSurfaces %q names no surface", v.Path, ms.At)
			}
		}
	}
}

// splitEntryPoint accepts "file.go:Symbol", "file.js:Symbol" and
// "page.html#element-id". The spellings exist because the surfaces are found in
// different ways: a symbol by its definition, a page element by its id
// attribute.
func splitEntryPoint(t *testing.T, verbPath []string, at string) (path, name, kind string) {
	t.Helper()
	if i := strings.LastIndex(at, "#"); i > 0 {
		return at[:i], at[i+1:], "element id"
	}
	if i := strings.LastIndex(at, ":"); i > 0 {
		return at[:i], at[i+1:], "symbol"
	}
	t.Errorf("%v: ModalSurfaces At %q has no ':Symbol' or '#element-id'", verbPath, at)
	return at, at, "?"
}

// definesEntryPoint reports whether src DEFINES name: not mentions, defines. Go
// is a type / func / var / const declaration (a method with any receiver
// counts, so a key handler on *App is nameable); JS is a function declaration,
// a const/let/var binding, or a method shorthand; HTML is an id attribute.
func definesEntryPoint(src, path, name, kind string) bool {
	q := regexp.QuoteMeta(name)
	var re *regexp.Regexp
	switch {
	case kind == "element id":
		re = regexp.MustCompile(fmt.Sprintf(`\bid=%q`, name))
	case filepath.Ext(path) == ".go":
		re = regexp.MustCompile(`(?m)^(?:func|type|var|const)\s+(?:\([^)]*\)\s*)?` + q + `\b`)
	case filepath.Ext(path) == ".js" || filepath.Ext(path) == ".mjs":
		re = regexp.MustCompile(`(?m)(?:^|[\s;{])(?:(?:async\s+)?function\s+` + q + `\s*\(|(?:const|let|var)\s+` + q + `\b|` + q + `\s*\([^)]*\)\s*\{)`)
	default:
		re = regexp.MustCompile(`\b` + q + `\b`)
	}
	return re.MatchString(src)
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

// definesEntryPoint must tell a definition from a mention, or the entry-point
// check is back to being a substring search. Each case pairs the thing a row
// may legitimately name with the shape that fooled the old check.
func TestDefinesEntryPointIsADefinitionNotAMention(t *testing.T) {
	goSrc := "package x\n\ntype ForwardsModal struct{}\n\nfunc (a *App) onCancel(m K) (C, bool) { return a.forwardsModal.Update(m) }\n\nfunc DoCancel() {}\n"
	jsSrc := "  function buildTaskSheet(sheet, t) {\n    const doResume = async (a) => {};\n    kill.addEventListener(\"click\", renderExecList);\n  }\n"
	html := `<section id="compose"><button id="reattach">Reattach</button></section>`
	cases := []struct {
		src, path, name, kind string
		want                  bool
	}{
		{goSrc, "tui/portforward.go", "ForwardsModal", "symbol", true},
		{goSrc, "tui/actions.go", "onCancel", "symbol", true}, // a method: receiver allowed
		{goSrc, "tui/client.go", "DoCancel", "symbol", true},
		{goSrc, "tui/portforward.go", "PortForwardModal", "symbol", false}, // absent entirely
		{goSrc, "tui/portforward.go", "forwardsModal", "symbol", false},    // a field use is a mention
		{goSrc, "tui/portforward.go", "Update", "symbol", false},           // a call is a mention
		{jsSrc, "webui/static/main.js", "buildTaskSheet", "symbol", true},
		{jsSrc, "webui/static/main.js", "doResume", "symbol", true},
		{jsSrc, "webui/static/main.js", "renderExecList", "symbol", false}, // passed as a value: a mention
		{html, "webui/index.html", "compose", "element id", true},
		{html, "webui/index.html", "reattach", "element id", true},
		{html, "webui/index.html", "reattach-quick", "element id", false},
		{html, "webui/index.html", "attach", "element id", false}, // a substring of an id is not that id
	}
	for _, c := range cases {
		if got := definesEntryPoint(c.src, c.path, c.name, c.kind); got != c.want {
			t.Errorf("definesEntryPoint(%s %q in %s) = %v, want %v", c.kind, c.name, c.path, got, c.want)
		}
	}
}
