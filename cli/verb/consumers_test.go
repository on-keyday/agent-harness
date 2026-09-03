package verb_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli/verb"
)

// A shared verb path now parses identically on every surface. What it MEANS
// after parsing is still per-surface, because each one executes differently on
// purpose -- stdout and an exit code, a tea.Cmd, a DOM update. That is the gap
// this file measures.
//
// The failure it catches is not a compile error and not a wrong value: it is a
// field the declaration carries and a surface that reaches the verb never
// reads, so an option the operator typed acts on one surface and is dropped on
// another. `--scope-for` did exactly that -- the CLI derived its presence bit
// from --scope alone and discarded a lone --scope-for on resume, while the TUI
// marked the half present for either flag. Both accepted it; one acted on it.
//
// Two things this test is NOT:
//
//   - It does not prove the surfaces do the same THING with a field. It proves
//     each one looks at it. A field read and then misused is beyond a static
//     check; a field never mentioned is not.
//   - It is not a demand for uniformity. Fields are legitimately surface-local,
//     which is what the allowlist is for -- each entry names the field, the
//     surface, and why.
//
// The unit is a VERB PATH, not an action type. Several verbs share one action
// (SessionAction covers attach, ls, kill, await-idle, snapshot, resize and the
// stream sub-verbs), and a per-type walk would demand that the TUI -- which
// reaches only await-idle -- read snapshot's rendering knobs.

// verbConsumer says which files execute one verb path on one surface.
type verbConsumer struct {
	path    string // as VerbSpec.FlagSetName() renders it
	surface verb.Surface
	files   []string
}

// tui/dispatch.go is on every TUI row and cmd/harness-cli/dispatch.go on
// every CLI row: each surface's verb handlers moved there when they became
// verb.TUIDispatch / verb.CLIDispatch methods, and app.go / main.go now only
// route into them. A row naming the old file alone would report every field
// as unread.
//
// verbConsumers lists the executing sites. Enumerated rather than derived:
// "executes this verb" is not something the import graph distinguishes from
// "mentions this type".
var verbConsumers = []verbConsumer{
	{"submit", verb.CLI, []string{"../../cmd/harness-cli/dispatch.go", "../../cmd/harness-cli/main.go"}},
	{"submit", verb.TUI, []string{"../../tui/dispatch.go", "../../tui/app.go"}},
	{"interactive", verb.CLI, []string{"../../cmd/harness-cli/dispatch.go", "../../cmd/harness-cli/main.go"}},
	{"interactive", verb.TUI, []string{"../../tui/dispatch.go", "../../tui/app.go"}},
	// session.go parses and holds the PTY knobs; spawnOpts in main.go turns the
	// shared half into the client's option bag. Both are the CLI's execution of
	// this verb, so both count as consumers.
	{"session new", verb.CLI, []string{"../../cmd/harness-cli/dispatch.go", "../../cmd/harness-cli/session.go", "../../cmd/harness-cli/main.go"}},
	{"session new", verb.TUI, []string{"../../tui/dispatch.go", "../../tui/app.go"}},

	{"prune", verb.CLI, []string{"../../cmd/harness-cli/dispatch.go", "../../cmd/harness-cli/main.go"}},
	{"prune", verb.TUI, []string{"../../tui/dispatch.go", "../../tui/app.go"}},

	{"file push", verb.CLI, []string{"../../cmd/harness-cli/dispatch.go", "../../cmd/harness-cli/main.go"}},
	{"file push", verb.TUI, []string{"../../tui/dispatch.go", "../../tui/app.go"}},
	{"file pull", verb.CLI, []string{"../../cmd/harness-cli/dispatch.go", "../../cmd/harness-cli/main.go"}},
	{"file pull", verb.TUI, []string{"../../tui/dispatch.go", "../../tui/app.go"}},

	{"git diff", verb.CLI, []string{"../../cmd/harness-cli/dispatch.go", "../../cmd/harness-cli/git.go"}},
	{"git diff", verb.TUI, []string{"../../tui/dispatch.go", "../../tui/app.go", "../../tui/gitmodal.go"}},

	{"exec", verb.CLI, []string{"../../cmd/harness-cli/dispatch.go", "../../cmd/harness-cli/exec.go"}},
	{"exec", verb.TUI, []string{"../../tui/dispatch.go", "../../tui/app.go", "../../tui/execrun.go"}},

	{"forward tap", verb.CLI, []string{"../../cmd/harness-cli/dispatch.go", "../../cmd/harness-cli/main.go"}},
	{"forward tap", verb.TUI, []string{"../../tui/dispatch.go", "../../tui/app.go", "../../tui/forwardtap_pump.go"}},

	{"session await-idle", verb.CLI, []string{"../../cmd/harness-cli/dispatch.go", "../../cmd/harness-cli/session.go"}},
	{"session await-idle", verb.TUI, []string{"../../tui/dispatch.go", "../../tui/app.go"}},

	{"workspace save", verb.CLI, []string{"../../cmd/harness-cli/dispatch.go", "../../cmd/harness-cli/workspace.go"}},
	{"workspace save", verb.TUI, []string{"../../tui/dispatch.go", "../../tui/app.go", "../../tui/workspace.go"}},

	// The paths whose drops an audit found, each now watched where it broke.
	// `exec ls` in particular was invisible: the `exec` row above restricts to
	// the RUN sub-verb's fields, so TaskFilter -- which the TUI read under the
	// wrong name and silently listed everything -- was in no row at all.
	{"exec ls", verb.CLI, []string{"../../cmd/harness-cli/dispatch.go", "../../cmd/harness-cli/exec.go"}},
	{"exec ls", verb.TUI, []string{"../../tui/dispatch.go", "../../tui/app.go", "../../tui/execrun.go"}},
	{"exec kill", verb.CLI, []string{"../../cmd/harness-cli/dispatch.go", "../../cmd/harness-cli/exec.go"}},
	{"exec kill", verb.TUI, []string{"../../tui/dispatch.go", "../../tui/app.go", "../../tui/execrun.go"}},
	{"forward kill", verb.CLI, []string{"../../cmd/harness-cli/dispatch.go", "../../cmd/harness-cli/main.go"}},
	{"forward kill", verb.TUI, []string{"../../tui/dispatch.go", "../../tui/app.go"}},
	{"caps set", verb.CLI, []string{"../../cmd/harness-cli/dispatch.go", "../../cmd/harness-cli/main.go"}},
	{"caps set", verb.TUI, []string{"../../tui/dispatch.go", "../../tui/app.go"}},
	{"caps set-parent", verb.CLI, []string{"../../cmd/harness-cli/dispatch.go", "../../cmd/harness-cli/main.go"}},
	{"caps set-parent", verb.TUI, []string{"../../tui/dispatch.go", "../../tui/app.go"}},
	{"notify", verb.CLI, []string{"../../cmd/harness-cli/dispatch.go", "../../cmd/harness-cli/main.go"}},
	{"notify", verb.TUI, []string{"../../tui/dispatch.go", "../../tui/app.go"}},
	{"session attach", verb.CLI, []string{"../../cmd/harness-cli/dispatch.go", "../../cmd/harness-cli/session.go"}},
	{"session attach", verb.TUI, []string{"../../tui/dispatch.go", "../../tui/app.go"}},
	// git's limits were declared for three surfaces and honoured by one: the
	// TUI built its query from the modal's state and the WebUI forwarded
	// neither, so `git <id> log --max 5` showed a default count on both.
	{"git log", verb.CLI, []string{"../../cmd/harness-cli/dispatch.go", "../../cmd/harness-cli/git.go"}},
	{"git log", verb.TUI, []string{"../../tui/dispatch.go", "../../tui/app.go"}},
	{"git show", verb.CLI, []string{"../../cmd/harness-cli/dispatch.go", "../../cmd/harness-cli/git.go"}},
	{"git show", verb.TUI, []string{"../../tui/dispatch.go", "../../tui/app.go"}},
	// `cancel <a> <b>` cancelled the first and exited 0 here, because this
	// was the one declared CLI path that never reached verb.Lookup.
	{"cancel", verb.CLI, []string{"../../cmd/harness-cli/dispatch.go", "../../cmd/harness-cli/main.go"}},
	{"cancel", verb.TUI, []string{"../../tui/dispatch.go", "../../tui/app.go"}},
	// prune-local removes WORKTREES, and its --repo ladder was
	// re-implemented on the VALUE rather than on presence, so `--repo .` was
	// silently replaced by HARNESS_REPO_PATH.
	{"prune-local", verb.CLI, []string{"../../cmd/harness-cli/dispatch.go", "../../cmd/harness-cli/main.go"}},
	{"session stream approve", verb.CLI, []string{"../../cmd/harness-cli/dispatch.go", "../../cmd/harness-cli/session.go"}},
	{"session stream approve", verb.TUI, []string{"../../tui/dispatch.go", "../../tui/app.go", "../../tui/client.go"}},
}

// actionFor names the action type a verb path builds, so the walk knows which
// fields to look for. Only the paths in verbConsumers need an entry.
var actionFor = map[string]any{
	"submit":                 verb.SpawnAction{},
	"interactive":            verb.SpawnAction{},
	"session new":            verb.SpawnAction{},
	"prune":                  verb.PruneAction{},
	"file push":              verb.FilePushAction{},
	"file pull":              verb.FilePullAction{},
	"git diff":               verb.GitAction{},
	"exec":                   verb.ExecRunAction{},
	"forward tap":            verb.ForwardTapAction{},
	"session await-idle":     verb.SessionAction{},
	"workspace save":         verb.WorkspaceAction{},
	"exec ls":                verb.ExecRunAction{},
	"git log":                verb.GitAction{},
	"git show":               verb.GitAction{},
	"cancel":                 verb.CancelAction{},
	"prune-local":            verb.PruneLocalAction{},
	"exec kill":              verb.ExecRunAction{},
	"forward kill":           verb.ForwardKillAction{},
	"caps set":               verb.SetCapsAction{},
	"caps set-parent":        verb.SetParentAction{},
	"notify":                 verb.NotifyAction{},
	"session attach":         verb.SessionAction{},
	"session stream approve": verb.SessionAction{},
}

// surfaceLocal names fields one surface legitimately ignores for one verb,
// with the reason. An entry here is a design decision on the record.
var surfaceLocal = map[string]string{
	// The CLI resolves --repo through its own env/workspace ladder before
	// building the options, so spawnOpts never reads the parsed field back.
	"submit.Repo/CLI":      "resolved through cliopts' env + workspace ladder and passed positionally",
	"interactive.Repo/CLI": "same ladder as submit",
	"submit.Kind/CLI":      "the CLI dispatches on its own switch before parsing, so it knows the verb already",
	"interactive.Kind/CLI": "same switch as submit",
	"session new.Kind/CLI": "same switch as submit",
	"submit.Task/CLI":      "read at the submit call site rather than in spawnOpts",

	// Only session new has a terminal to detach, stream, size or X11-forward.
	// submit is queued and interactive attaches immediately.
	"submit.Detach/CLI":          "a queued submit has no terminal to detach from",
	"submit.Stream/CLI":          "submit has no event-stream form",
	"submit.X11/CLI":             "submit has no client to host an X tunnel",
	"submit.X11Display/CLI":      "with X11",
	"submit.Rows/CLI":            "a queued submit opens no PTY",
	"submit.Cols/CLI":            "with Rows",
	"interactive.Detach/CLI":     "interactive attaches by definition; `session new -d` is the detaching verb",
	"interactive.Stream/CLI":     "interactive is a PTY verb",
	"interactive.X11/CLI":        "reached through session new --x11",
	"interactive.X11Display/CLI": "with X11",
	"interactive.Rows/CLI":       "the attached terminal supplies its own size",
	"interactive.Cols/CLI":       "with Rows",
	"submit.Detach/TUI":          "as CLI",
	"submit.Stream/TUI":          "as CLI",
	"submit.X11/TUI":             "as CLI",
	"submit.X11Display/TUI":      "as CLI",
	"submit.Rows/TUI":            "as CLI",
	"submit.Cols/TUI":            "as CLI",
	"interactive.Detach/TUI":     "as CLI",
	"interactive.Stream/TUI":     "as CLI",
	"interactive.X11/TUI":        "as CLI",
	"interactive.X11Display/TUI": "as CLI",
	"interactive.Rows/TUI":       "as CLI",
	"interactive.Cols/TUI":       "as CLI",
	"session new.Rows/TUI":       "the TUI sizes a new session from its own window, not from flags",
	"session new.Cols/TUI":       "with Rows",

	// The TUI derives the two presence bits from the pointers themselves,
	// because it must also fold in the session defaults the CLI has none of.
	"submit.CapsPresent/TUI":       "derived in resolveSpawnCaps, together with the session default",
	"submit.ScopePresent/TUI":      "derived in spawnAuthority, together with the session default",
	"interactive.CapsPresent/TUI":  "as submit",
	"interactive.ScopePresent/TUI": "as submit",
	"session new.CapsPresent/TUI":  "as submit",
	"session new.ScopePresent/TUI": "as submit",

	"git diff.TaskID/CLI":   "peeled before the shared parse: the id sits between the family word and the sub-verb",
	"git diff.TaskID/TUI":   "same peel as the CLI",
	"git diff.Sub/CLI":      "read as g.Sub in the dispatch switch, which the selector scan sees on the switch expression",
	"git diff.MaxBytes/TUI": "the TUI's git modal renders into a pane it sizes itself; byte caps are the runner's job there",

	"exec.Argv/TUI":       "the TUI pane builds its argv from its own input line",
	"exec.Shell/TUI":      "the TUI exec pane is always a shell line",
	"exec.SshdParent/TUI": "wired on Windows through the CLI; the TUI pane does not offer it",

	"forward tap.Mode/TUI": "the TUI tap panel renders one way; the four render modes are a stdout concern",

	"workspace save.TaskID/TUI": "the TUI picks tasks in its picker rather than naming one on the line",
	"workspace save.Resume/TUI": "written through the TUI's picker",
	"workspace save.Runner/TUI": "written through the TUI's picker",
	"workspace save.Repo/TUI":   "the TUI already knows its repo from the session",
	"workspace save.All/CLI":    "--all skips the TUI's task picker; the CLI has none",

	"file pull.LocalDst/TUI": "the TUI writes through its own file picker",
	"file push.LocalSrc/TUI": "the TUI reads through its own file picker",

	// An empty ParentID IS the detach request on the wire, so a consumer that
	// forwards ParentID has already carried --none. The declaration refuses
	// `--parent ""` at the value, which is the only place the two can be told
	// apart: ExactlyOne is decided on presence, so by the time a consumer
	// runs, the branch has been chosen and no condition on None can narrow it.
	"caps set-parent.None/CLI": "an empty ParentID is the detach request; refused at the flag, not sorted out here",
	"caps set-parent.None/TUI": "an empty ParentID is the detach request; refused at the flag, not sorted out here",

	// --allow and --deny are complements under ExactlyOne, so a consumer
	// branches on ONE of them and the other is that branch's else. Both are
	// declared because both are typed.
	"session stream approve.Deny/CLI":  "the allow branch's else; ExactlyOne makes them complements",
	"session stream approve.Allow/TUI": "the deny branch's else; ExactlyOne makes them complements",
}

func surfaceName(s verb.Surface) string {
	switch s {
	case verb.CLI:
		return "CLI"
	case verb.TUI:
		return "TUI"
	default:
		return "WebUI"
	}
}

// TestEverySurfaceReadsEveryActionField walks each verb path's fields and
// checks that every surface reaching it mentions each one, or says why not.
func TestEverySurfaceReadsEveryActionField(t *testing.T) {
	for _, c := range verbConsumers {
		proto, ok := actionFor[c.path]
		if !ok {
			t.Fatalf("%s: no entry in actionFor", c.path)
		}
		rt := reflect.TypeOf(proto)

		// The fields THIS path fills on THIS surface, taken from the narrowed
		// spec rather than from the action type. Several verbs share an action
		// (SessionAction covers attach, ls, kill, await-idle and the stream
		// sub-verbs), so a per-type walk demands that a surface reaching only
		// await-idle read snapshot's rendering knobs -- and a hand-written
		// restriction list, which is what this used to be, goes stale the
		// other way: it named exec's run fields and so `exec ls --task` sat in
		// no list at all while the TUI dropped it.
		want := declaredFields(t, c.path, c.surface)

		read := map[string]bool{}
		for _, f := range c.files {
			for name := range fieldsMentionedFor(t, f, rt.Name()) {
				read[name] = true
			}
		}
		sn := surfaceName(c.surface)
		for _, name := range want {
			if read[name] {
				continue
			}
			if _, allowed := surfaceLocal[c.path+"."+name+"/"+sn]; allowed {
				continue
			}
			t.Errorf("%s: %s.%s is never mentioned by the %s surface (%s).\n"+
				"The declaration carries this field, so every surface that runs the verb "+
				"decides what to do with it. A field one surface reads and another never "+
				"names is how `--scope-for` reached the wire from the TUI and was dropped "+
				"by the CLI -- both parsed it, one acted on it.\n"+
				"Either read it, or add %q to surfaceLocal with the reason.",
				c.path, rt.Name(), name, sn, strings.Join(c.files, ", "),
				c.path+"."+name+"/"+sn)
		}
	}
}

// declaredFields is every Action field the declaration fills for one verb path
// on one surface: its flags, its positionals, its Const values, its trailing
// text and anything Derived. Surface-narrowed flags and args drop out, which
// is the point -- `exec ls --json` is not declared for the TUI, so the TUI
// owes nothing for it.
func declaredFields(t *testing.T, path string, sf verb.Surface) []string {
	t.Helper()
	sp, ok := verb.Lookup(strings.Fields(path)...)
	if !ok {
		t.Fatalf("%s: not in the verb table", path)
	}
	sp = sp.For(sf)
	var out []string
	add := func(n string) {
		if n == "" {
			return
		}
		for _, e := range out {
			if e == n {
				return
			}
		}
		out = append(out, n)
	}
	for _, f := range sp.Flags {
		add(f.Field)
		add(f.PresenceField)
	}
	for _, a := range sp.Args {
		add(a.Field)
	}
	for _, d := range sp.Derived {
		add(d.Field)
	}
	if sp.Modes != nil {
		add(sp.Modes.Field)
	}
	if sp.Trailing != nil {
		add(sp.Trailing.Field)
		add(sp.Trailing.FieldArgs)
	}
	add(sp.PathspecField)
	// Const values are fixed by the path, not typed by the operator, so a
	// surface that switches on Sub reads it and one that does not need not.
	return out
}

// fieldsMentionedFor returns the field names this file selects off a variable
// holding ONE action type -- `a.Overrides`, `v.ExtraArgs`.
//
// Scoped to the type, which is the whole difficulty. Two earlier versions were
// inert and both looked reasonable:
//
//   - counting every `x.Foo` in the file: `Overrides` and `ExtraArgs` are
//     field names on the client's option structs too, so dropping them from
//     the action left the name present and the test green.
//   - counting selectors on the variables that hold AN action: tui/app.go
//     dispatches ~37 action types from a single `switch v := act.(type)`, so
//     one `v` covers the whole file. Measured: 127 distinct names, including
//     App fields like Width and Reconnecting. Deleting both reads of
//     PruneAction.Force left the suite green, because `Force` survives in the
//     `file push` / `file pull` / `file delete` cases.
//
// So the collection is per type: selectors inside the `case verb.T:` clause
// that binds the switch variable, plus the bodies of functions taking a
// parameter of that type (tui/app.go hands most cases straight to a
// runXxxAction helper), plus assignments from `act.(verb.T)`.
//
// It is still an over-approximation WITHIN that scope: `a.Overrides` counts
// whether or not the value goes anywhere useful. What it rules out is the
// specific failure that motivated it -- a field no code path on this surface
// takes off this action at all.
func fieldsMentionedFor(t *testing.T, path, typeName string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	src, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	// Functions in this package that RETURN the type, so an assignment from
	// one is a scope too: the CLI writes `a := parseSpawn("session-new", …)`,
	// which is neither a type assertion nor a parameter.
	returns := funcsReturning(t, filepath.Dir(path), typeName)

	// Bodies to walk, each with the variable naming the action inside it.
	type scope struct {
		node ast.Node
		recv string
	}
	var scopes []scope
	// Helper functions whose PARAMETER is this action type, and the names of
	// those functions -- a case clause that only calls one of them reads the
	// fields there.
	helpers := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Type.Params == nil || fn.Body == nil {
			return true
		}
		for _, p := range fn.Type.Params.List {
			if !namesType(p.Type, typeName) {
				continue
			}
			helpers[fn.Name.Name] = true
			for _, nm := range p.Names {
				if nm.Name != "_" {
					scopes = append(scopes, scope{fn.Body, nm.Name})
				}
			}
		}
		return true
	})
	ast.Inspect(f, func(n ast.Node) bool {
		switch st := n.(type) {
		case *ast.TypeSwitchStmt:
			bound := ""
			if as, ok := st.Assign.(*ast.AssignStmt); ok && len(as.Lhs) == 1 {
				if id, ok := as.Lhs[0].(*ast.Ident); ok {
					bound = id.Name
				}
			}
			if bound == "" || st.Body == nil {
				return true
			}
			for _, stmt := range st.Body.List {
				cc, ok := stmt.(*ast.CaseClause)
				if !ok {
					continue
				}
				for _, e := range cc.List {
					if namesType(e, typeName) {
						for _, s := range cc.Body {
							scopes = append(scopes, scope{s, bound})
						}
					}
				}
			}
		case *ast.AssignStmt:
			// `x := act.(verb.T)` / `x, ok := act.(verb.T)` / `x := parseX(…)`
			if len(st.Rhs) != 1 {
				return true
			}
			if !namesType(st.Rhs[0], typeName) && !callsReturning(st.Rhs[0], returns) {
				return true
			}
			for _, lhs := range st.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && id.Name != "_" {
					scopes = append(scopes, scope{f, id.Name})
				}
			}
		}
		return true
	})

	// Aliases: `run := act.(verb.T)` then `a := run`. A plain identifier copy
	// is neither an assertion nor a call, so the second name was not a
	// receiver and every field read through it went uncounted --
	// cmd/harness-cli/exec.go does exactly this, and `exec ls --task` read as
	// unconsumed there while the line above it uses it.
	named := map[string]bool{}
	for _, sc := range scopes {
		named[sc.recv] = true
	}
	for again := true; again; {
		again = false
		ast.Inspect(f, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok || len(as.Rhs) != 1 {
				return true
			}
			src, ok := as.Rhs[0].(*ast.Ident)
			if !ok || !named[src.Name] {
				return true
			}
			for _, lhs := range as.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && id.Name != "_" && !named[id.Name] {
					named[id.Name] = true
					scopes = append(scopes, scope{f, id.Name})
					again = true
				}
			}
			return true
		})
	}

	for _, sc := range scopes {
		ast.Inspect(sc.node, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == sc.recv {
				out[sel.Sel.Name] = true
			}
			return true
		})
	}
	return out
}

// funcsReturning names every function in a directory whose single result is
// verb.<typeName>.
func funcsReturning(t *testing.T, dir, typeName string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		f, perr := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if perr != nil {
			continue
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Type.Results == nil {
				continue
			}
			for _, r := range fn.Type.Results.List {
				if namesType(r.Type, typeName) {
					out[fn.Name.Name] = true
				}
			}
		}
	}
	return out
}

// callsReturning reports whether an expression is a call to one of them.
func callsReturning(e ast.Expr, returns map[string]bool) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return returns[fn.Name]
	case *ast.SelectorExpr:
		return returns[fn.Sel.Name]
	}
	return false
}

// namesType reports whether an expression names verb.<typeName> -- as a type,
// a type assertion, or a composite literal.
func namesType(e ast.Expr, typeName string) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		switch t := n.(type) {
		case *ast.SelectorExpr:
			if t.Sel.Name == typeName {
				if id, ok := t.X.(*ast.Ident); ok && id.Name == "verb" {
					found = true
				}
			}
		case *ast.Ident:
			// Inside package tui the surface-local actions are unqualified;
			// none of them share a name with a generated one any more, so an
			// exact match is enough.
			if t.Name == typeName {
				found = true
			}
		}
		return !found
	})
	return found
}

// mentionsVerbAction reports whether an expression names a cli/verb action --
// a type assertion, a call to one of the parse helpers, or a composite literal.
func mentionsVerbAction(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		switch t := n.(type) {
		case *ast.SelectorExpr:
			if id, ok := t.X.(*ast.Ident); ok && id.Name == "verb" && strings.HasSuffix(t.Sel.Name, "Action") {
				found = true
			}
		case *ast.Ident:
			// parseSpawn / parseSession / parseOne / parseViaSpec* all return
			// an action; naming them is enough.
			if strings.HasPrefix(t.Name, "parseSpawn") || strings.HasPrefix(t.Name, "parseSession") ||
				t.Name == "parseOne" || strings.HasPrefix(t.Name, "parseViaSpec") {
				found = true
			}
		}
		return true
	})
	return found
}

// TestConsumerTablesAreLive keeps the two tables from outliving what they
// describe: a stale exemption reads as a considered decision while naming a
// field that no longer exists, and a verb path that has been renamed would
// silently stop being checked at all.
func TestConsumerTablesAreLive(t *testing.T) {
	for _, c := range verbConsumers {
		parts := strings.Fields(c.path)
		sp, ok := verb.Lookup(parts...)
		if !ok {
			t.Errorf("verbConsumers names %q, which is not in the table", c.path)
			continue
		}
		if !sp.Surfaces.Has(c.surface) {
			t.Errorf("%s is listed as consumed by %s, but the declaration does not offer it there",
				c.path, surfaceName(c.surface))
		}
		if _, has := actionFor[c.path]; !has {
			t.Errorf("%s has no actionFor entry", c.path)
		}
	}

	var stale []string
	for key, reason := range surfaceLocal {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("%s: an exemption needs a reason", key)
		}
		pathAndField, sn, _ := strings.Cut(key, "/")
		i := strings.LastIndex(pathAndField, ".")
		if i < 0 {
			stale = append(stale, key+" (malformed)")
			continue
		}
		path, field := pathAndField[:i], pathAndField[i+1:]
		proto, ok := actionFor[path]
		if !ok {
			stale = append(stale, key+" (no such verb path in actionFor)")
			continue
		}
		if _, has := reflect.TypeOf(proto).FieldByName(field); !has {
			stale = append(stale, key+" (no such field)")
			continue
		}
		found := false
		for _, c := range verbConsumers {
			if c.path == path && surfaceName(c.surface) == sn {
				found = true
				break
			}
		}
		if !found {
			stale = append(stale, key+" (that surface does not consume this verb)")
		}
	}
	sort.Strings(stale)
	for _, s := range stale {
		t.Errorf("stale exemption: %s", s)
	}
}
