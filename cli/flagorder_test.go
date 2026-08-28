package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// flagsMustPrecedePositionals lists the verbs whose flags REALLY must come
// before their positional, keyed by the name their FlagSet was constructed
// with. Each one takes free-form trailing text, where a word beginning with '-'
// is indistinguishable from a flag, so the peel in ParsePermuted would eat it.
//
// Everything NOT listed here must parse through ParsePermuted. See the test
// below for why this is a test rather than a note in a skill.
var flagsMustPrecedePositionals = map[string]string{
	"session send":        "trailing words are the literal text to type into the PTY",
	"session exec":        "trailing words are the command line to run",
	"session stream turn": "trailing words are the user turn's text",
	"notify":              "trailing words are the notification body",
}

// TestVerbsParseFlagsInEitherOrder is the guard for a defect class that shipped
// twice and was caught by neither review nor any test: a verb whose own usage
// line prints a flag AFTER a positional, parsed with stdlib flag, which stops at
// the first non-flag token and leaves that flag unread.
//
// What it cost, measured on a live board on 2026-08-28: `board purge <topic>
// --seq N` — the exact string the help text printed — left --seq at its zero
// value, which is the WHOLE-TOPIC form. Two messages, one named, both
// destroyed. `board read --in-reply-to` and the new `board retract --seq` had
// the same shape; so did `server dial-runner --via` and `agent read`/`agent
// retract`'s --server-cid.
//
// Why a test and not a review habit: every one of those sites was reviewed, and
// the feature that introduced the third one had full unit + in-process E2E
// coverage. Neither could see it, because the tests called the client method
// directly and the review read the flag definition rather than the parse. The
// only thing that reaches it is executing the documented command line — or
// asserting the shape, which is what this does.
func TestVerbsParseFlagsInEitherOrder(t *testing.T) {
	// Relative to this package: the CLI's verb parsers live in three places.
	dirs := []string{".", "agent", filepath.Join("..", "cmd", "harness-cli")}

	type site struct {
		name string
		file string
		line int
	}
	var offenders []site

	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			// Scoped to the BLOCK that builds the FlagSet, not to the enclosing
			// function: harness-cli's main() is one switch holding a dozen verb
			// parsers, and a function-level scan would have to exempt all of it.
			ast.Inspect(f, func(n ast.Node) bool {
				var body []ast.Stmt
				switch b := n.(type) {
				case *ast.BlockStmt:
					body = b.List
				case *ast.CaseClause:
					body = b.Body
				default:
					return true
				}
				for _, stmt := range body {
					name, varName, pos, ok := flagSetDecl(stmt)
					if !ok {
						continue
					}
					defines, stdParse, readsPos := scanFlagSetUse(body, varName)
					if defines && stdParse && readsPos {
						if _, allowed := flagsMustPrecedePositionals[name]; !allowed {
							offenders = append(offenders, site{name, path, fset.Position(pos).Line})
						}
					}
				}
				return true
			})
		}
	}

	for _, o := range offenders {
		t.Errorf("%s:%d: verb %q defines flags, reads positionals, and parses with the stdlib flag package.\n"+
			"Go's flag stops at the first non-flag token, so a flag written AFTER the positional — which is how "+
			"most of these verbs' own usage lines print it — is silently dropped, taking whatever default it has "+
			"(for `board purge --seq` that default meant the whole topic).\n"+
			"Fix: parse with cli.ParsePermuted(fs, args). If the verb takes free-form trailing text and genuinely "+
			"cannot permute, add it to flagsMustPrecedePositionals with the reason.",
			o.file, o.line, o.name)
	}
}

// flagSetDecl reports whether stmt is `x := flag.NewFlagSet("name", …)`, and
// returns the verb name and the variable it was bound to.
func flagSetDecl(stmt ast.Stmt) (name, varName string, pos token.Pos, ok bool) {
	as, isAssign := stmt.(*ast.AssignStmt)
	if !isAssign || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
		return "", "", 0, false
	}
	call, isCall := as.Rhs[0].(*ast.CallExpr)
	if !isCall || len(call.Args) == 0 {
		return "", "", 0, false
	}
	sel, isSel := call.Fun.(*ast.SelectorExpr)
	if !isSel || sel.Sel.Name != "NewFlagSet" {
		return "", "", 0, false
	}
	pkg, isIdent := sel.X.(*ast.Ident)
	if !isIdent || pkg.Name != "flag" {
		return "", "", 0, false
	}
	lit, isLit := call.Args[0].(*ast.BasicLit)
	if !isLit || lit.Kind != token.STRING {
		return "", "", 0, false
	}
	unquoted, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", "", 0, false
	}
	lhs, isIdent := as.Lhs[0].(*ast.Ident)
	if !isIdent {
		return "", "", 0, false
	}
	return unquoted, lhs.Name, call.Pos(), true
}

// scanFlagSetUse reports how the FlagSet bound to varName is used within body:
// whether any flag is DEFINED on it, whether it is parsed with the stdlib
// Parse, and whether positionals are read back off it. All three together are
// the silent-drop shape; any two of them are harmless.
func scanFlagSetUse(body []ast.Stmt, varName string) (defines, stdParse, readsPos bool) {
	for _, stmt := range body {
		ast.Inspect(stmt, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			recv, ok := sel.X.(*ast.Ident)
			if !ok || recv.Name != varName {
				return true
			}
			switch sel.Sel.Name {
			case "Bool", "String", "Uint", "Uint64", "Int", "Int64", "Float64", "Duration", "Var", "Func":
				defines = true
			case "Parse":
				stdParse = true
			case "Arg", "Args", "NArg":
				readsPos = true
			}
			return true
		})
	}
	return defines, stdParse, readsPos
}
