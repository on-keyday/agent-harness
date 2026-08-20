package tui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The authority half of a session request (caps, scope, overrides, and the
// resume presence bit) was hand-written into seven cli.SessionOpts literals in
// this package. Six of them — every interactive and session path — predated
// TaskScope.Overrides and never grew the field, so `session new --scope-for
// spawn=global` parsed, rode SessionNewAction and spawnAuthority, and then
// spawned with the bare scope. Only DoSubmitWithOpts carried it.
//
// The tests below pin the fix rather than the symptom: one builder, and a
// mechanical check that it carries every authority field.

// findSessionOptsLiterals returns "funcName:line" for every cli.SessionOpts
// composite literal in the package's non-test sources.
func findSessionOptsLiterals(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	var found []string
	sawAnyFile := false
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		sawAnyFile = true
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				sel, ok := lit.Type.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "SessionOpts" {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "cli" {
					return true
				}
				found = append(found, fn.Name.Name+":"+fset.Position(lit.Pos()).String())
				return true
			})
		}
	}
	// A parser that silently reads nothing would report "one literal, in opts"
	// forever. Fail loudly instead.
	if !sawAnyFile {
		t.Fatal("scanned no non-test .go files; the guard cannot see the package")
	}
	return found
}

// TestSessionOptsIsBuiltInOnePlace is the mechanism the doc comments were
// standing in for. Adding a field to Authority must not require finding every
// caller again: there is exactly one place that turns an Authority into a
// cli.SessionOpts.
func TestSessionOptsIsBuiltInOnePlace(t *testing.T) {
	found := findSessionOptsLiterals(t)
	if len(found) != 1 {
		t.Fatalf("cli.SessionOpts is built at %d sites, want exactly 1 (Authority.opts):\n  %s\n"+
			"Route the new site through auth.opts(sessionRequest{...}) — a hand-written literal "+
			"silently drops whatever authority field it predates.", len(found), strings.Join(found, "\n  "))
	}
	if !strings.HasPrefix(found[0], "opts:") {
		t.Errorf("the sole cli.SessionOpts literal is in %s, want Authority.opts", found[0])
	}
}

// TestAuthorityOptsCarriesEveryAuthorityField populates every field of
// Authority with a non-zero value and asserts each one arrives in the built
// SessionOpts under the same name. A field added to Authority and forgotten in
// opts fails here, which is the failure this whole file exists for.
func TestAuthorityOptsCarriesEveryAuthorityField(t *testing.T) {
	auth := Authority{
		Caps:  protocol.Capability_Spawn | protocol.Capability_ExecView,
		Scope: protocol.TaskScope{Base: protocol.ScopeBase_None},
		Overrides: []protocol.ScopeOverride{
			{Caps: protocol.Capability_Spawn, Base: protocol.ScopeBase_Global},
		},
		ScopePresent: true,
	}

	// Every field must actually be non-zero, or "it survived" is vacuous for
	// that field.
	av := reflect.ValueOf(auth)
	for i := 0; i < av.NumField(); i++ {
		if av.Field(i).IsZero() {
			t.Fatalf("Authority.%s is left at its zero value; the assertion below "+
				"cannot distinguish carried from dropped", av.Type().Field(i).Name)
		}
	}

	got := reflect.ValueOf(auth.opts(sessionRequest{}))
	for i := 0; i < av.NumField(); i++ {
		name := av.Type().Field(i).Name
		dst := got.FieldByName(name)
		if !dst.IsValid() {
			t.Errorf("cli.SessionOpts has no %s field; Authority and SessionOpts have drifted", name)
			continue
		}
		if !reflect.DeepEqual(dst.Interface(), av.Field(i).Interface()) {
			t.Errorf("SessionOpts.%s = %#v, want %#v (dropped by Authority.opts)",
				name, dst.Interface(), av.Field(i).Interface())
		}
	}
}

// TestAuthorityOptsCarriesTheRequestHalf is the other direction: the
// non-authority fields must survive the same call, or collapsing seven
// literals into one builder would have traded a dropped scope for a dropped
// selector.
func TestAuthorityOptsCarriesTheRequestHalf(t *testing.T) {
	req := sessionRequest{
		ExtraArgs:          []string{"--add-dir", "/other"},
		ResumeTaskID:       "d7df8fe8dc239a515c4f2de02e05ee78",
		ResumeCapsOverride: true,
		ResumeConversation: true,
		AgentProfile:       "codex",
		InitialRows:        40,
		InitialCols:        120,
	}
	got := Authority{}.opts(req)
	if !reflect.DeepEqual(got.ExtraArgs, req.ExtraArgs) {
		t.Errorf("ExtraArgs = %v", got.ExtraArgs)
	}
	if got.ResumeTaskID != req.ResumeTaskID {
		t.Errorf("ResumeTaskID = %q", got.ResumeTaskID)
	}
	// The two resume booleans are the transposition pair sessionRequest's doc
	// comment names; assert them apart rather than together.
	if !got.ResumeCapsOverride {
		t.Error("ResumeCapsOverride was dropped")
	}
	if !got.ResumeConversation {
		t.Error("ResumeConversation was dropped")
	}
	if got.AgentProfile != req.AgentProfile {
		t.Errorf("AgentProfile = %q", got.AgentProfile)
	}
	if got.InitialRows != 40 || got.InitialCols != 120 {
		t.Errorf("PTY size = %dx%d, want 40x120 (rows, cols)", got.InitialRows, got.InitialCols)
	}
}

// TestSessionNewCarriesScopeForIntoTheRequest replays the command that
// surfaced this: the override must survive parse -> action -> spawnAuthority
// -> SessionOpts. --caps was already arriving, which is what made the loss
// look like a scope-parsing bug rather than a dropped field.
func TestSessionNewCarriesScopeForIntoTheRequest(t *testing.T) {
	const resumeID = "d7df8fe8dc239a515c4f2de02e05ee78"
	act, err := ParseCommand(
		`session new --resume `+resumeID+` --caps spawn --scope-for spawn=global`, "/cwd")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	v, ok := act.(SessionNewAction)
	if !ok {
		t.Fatalf("action = %T, want SessionNewAction", act)
	}
	if len(v.Overrides) != 1 {
		t.Fatalf("parsed overrides = %d, want 1", len(v.Overrides))
	}

	var a App
	caps, _ := a.resolveSpawnCaps(v.Caps, v.ResumeTaskID != "")
	auth := a.spawnAuthority(v.Scope, v.Overrides, v.ResumeTaskID, caps)
	opts := auth.opts(sessionRequest{ResumeTaskID: v.ResumeTaskID})

	if opts.Caps != protocol.Capability_Spawn {
		t.Errorf("caps = %v, want spawn", opts.Caps)
	}
	if len(opts.Overrides) != 1 {
		t.Fatalf("request carries %d overrides, want 1 — --scope-for was dropped "+
			"between the command line and the request", len(opts.Overrides))
	}
	ov := opts.Overrides[0]
	if ov.Caps != protocol.Capability_Spawn || ov.Base != protocol.ScopeBase_Global {
		t.Errorf("override = %v/%v, want spawn/global", ov.Caps, ov.Base)
	}
	// Naming --scope-for makes the scope half explicit, so a resume re-grants
	// it instead of keeping the resumed task's persisted scope; without this
	// the override would be accepted and then never written.
	if !opts.ScopePresent {
		t.Error("ScopePresent is false on a resume that named --scope-for; " +
			"the server would keep the task's persisted scope and discard the override")
	}
	// What ls prints must survive a round trip back through the flag parser,
	// which is the only reason the two spellings both exist.
	lbl := cli.OverridesLabel(opts.Overrides)
	if lbl != "spawn:global" {
		t.Fatalf("label = %q, want %q", lbl, "spawn:global")
	}
	if _, back, err := cli.ParseScopeFor(lbl); err != nil {
		t.Errorf("ls label %q does not parse back as --scope-for: %v", lbl, err)
	} else if back.Caps != ov.Caps || back.Base != ov.Base {
		t.Errorf("round trip = %v/%v, want %v/%v", back.Caps, back.Base, ov.Caps, ov.Base)
	}
}
