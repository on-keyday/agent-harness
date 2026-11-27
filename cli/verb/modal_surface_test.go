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
		if _, gap := screenGaps[strings.Join(v.Path, " ")]; gap || v.LineCommand {
			continue
		}
		if len(v.ModalSurfaces) == 0 && strings.TrimSpace(v.NoModalSurface) == "" {
			t.Errorf("%v has no verdict about non-cmdline surfaces.\n"+
				"  Look for a TUI action or a WebUI element. `grep -rl DoThing tui/*.go`:\n"+
				"  dispatch.go only means the cmdline, which CmdlineSurfaces already covers.\n"+
				"  Then add a ModalSurfaces row. NoModalSurface is accepted only for a verb typable on\n"+
				"  no UI's command line (TestNoModalSurfaceOnlyForVerbsTypableNowhere).",
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

// screenGaps is every (verb, surface) typable on that surface's command line
// with no screen there yet: the shrinking list. An entry leaves when its
// screen is declared in ModalSurfaces, and TestTypableOnAUIMeansAScreenThere
// fails until it does, so the list cannot keep a verb that has one. A NEW verb
// cannot be added here to pass: being typable on a UI proves the UI can run
// it, and the answer is a screen.
var screenGaps = map[string]Surface{
	"action edit":              TUI,
	"action ls":                TUI,
	"action new":               TUI,
	"action rm":                TUI,
	"caps":                     TUI | WebUI,
	"caps reach":               TUI,
	"caps set-defaults":        TUI | WebUI,
	"caps what-if":             TUI,
	"credential new":           TUI,
	"exec":                     TUI,
	"exec tap":                 WebUI,
	"file delete":              WebUI,
	"file ls":                  WebUI,
	"file mkdir":               WebUI,
	"file new":                 WebUI,
	"file pull":                WebUI,
	"file push":                WebUI,
	"forward tap":              WebUI,
	"ls":                       WebUI,
	"prune":                    TUI | WebUI,
	"record get":               TUI,
	"record off":               TUI,
	"record on":                TUI,
	"record rm":                TUI,
	"restore":                  TUI | WebUI,
	"runner dial add":          TUI | WebUI,
	"runner dial ls":           TUI | WebUI,
	"runner dial rm":           TUI | WebUI,
	"runner enroll":            TUI | WebUI,
	"runner keys":              TUI | WebUI,
	"runner kick":              TUI | WebUI,
	"runner unenroll":          TUI | WebUI,
	"schedule edit":            TUI | WebUI,
	"schedule new":             TUI,
	"server dial-runner":       TUI | WebUI,
	"session kill":             TUI,
	"session ls":               TUI,
	"session rename":           TUI | WebUI,
	"session stream answer":    TUI,
	"session stream approve":   TUI,
	"session stream finish":    TUI,
	"session stream interrupt": TUI,
	"session stream turn":      TUI | WebUI,
	"ssh-gateway status":       TUI,
	"ssh-gateway stop":         TUI,
	"transcript get":           TUI,
	"workspace detach":         TUI,
	"workspace rm":             TUI,
	"workspace save":           TUI,
	"workspace show":           TUI,
}

// Typing a verb on a UI's command line proves that UI can run it, so
// "functionally impossible there" cannot be claimed for it: it has a
// ModalSurfaces row for that UI, unless it is a command of the line itself
// (LineCommand). A reason in free text is not accepted — no test can tell a
// reason from a sentence saying only that no screen exists yet.
func TestTypableOnAUIMeansAScreenThere(t *testing.T) {
	seen := map[string]bool{}
	for _, v := range Verbs {
		path := strings.Join(v.Path, " ")
		var lacking Surface
		for _, s := range []Surface{TUI, WebUI} {
			if v.CmdlineSurfaces&s == 0 || v.LineCommand {
				continue
			}
			has := false
			for _, ms := range v.ModalSurfaces {
				has = has || ms.Surface == s
			}
			if !has {
				lacking |= s
			}
		}
		listed, inList := screenGaps[path]
		seen[path] = inList
		switch {
		case lacking != 0 && listed&lacking != lacking:
			t.Errorf("%q is typable on %v's command line with no screen there.\n"+
				"  Build one and add {Surface: %v, At: ...} to ModalSurfaces. If it is a command of\n"+
				"  the line itself (quit, help), set LineCommand: true.", path, surfaceList(lacking&^listed), surfaceList(lacking&^listed))
		case inList && listed&^lacking != 0:
			t.Errorf("screenGaps lists %q for %v, which it no longer lacks: remove it from the list", path, surfaceList(listed&^lacking))
		}
	}
	for path := range screenGaps {
		if _, ok := seen[path]; !ok {
			t.Errorf("screenGaps lists %q, which is not a verb", path)
		}
	}
}

// A command of the line has nothing to put on a screen and nothing to explain.
func TestLineCommandHasNoScreenAndNoReason(t *testing.T) {
	for _, v := range Verbs {
		if v.LineCommand && (len(v.ModalSurfaces) > 0 || v.NoModalSurface != "") {
			t.Errorf("%v is a LineCommand and also declares a screen or a reason", v.Path)
		}
	}
}

// NoModalSurface is a note for verbs typable on no UI's command line. On a
// verb typable somewhere it would be the free-text excuse the rule replaces.
func TestNoModalSurfaceOnlyForVerbsTypableNowhere(t *testing.T) {
	for _, v := range Verbs {
		if v.NoModalSurface != "" && v.CmdlineSurfaces&(TUI|WebUI) != 0 {
			t.Errorf("%v is typable on %v's command line, so it needs a screen (or screenGaps), not a NoModalSurface reason",
				v.Path, surfaceList(v.CmdlineSurfaces&(TUI|WebUI)))
		}
	}
}

// surfaceList names the UI surfaces in s, for messages.
func surfaceList(s Surface) string {
	var out []string
	if s&TUI != 0 {
		out = append(out, "TUI")
	}
	if s&WebUI != 0 {
		out = append(out, "WebUI")
	}
	return strings.Join(out, "|")
}
