package verb

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

// The operator's proposal was blunter than this: 「いっそのこと `case "` って
// 文字列を探し出してあったらエラーにするくらいでもいい」. Taken literally it
// fires on things that have nothing to do with the grammar -- keybindings
// (`case "esc"`, `case "up", "k"`), SSH channel types (`case "pty-req"`,
// `case "shell"`), workspace config keys (`case "resume"`) -- 15 of the 48
// sites a plain word match hits. A guard that cries wolf 15 times gets an
// exemption list, and an exemption list is where the real one hides.
//
// So the rule is structural rather than lexical: a switch over a field whose
// values the DECLARATION fixes may not spell those values by hand. That is
// exactly the regression the proposal is about -- a sub-verb renamed in the
// table while `case "stream-turn"` sits somewhere matching nothing, silently,
// because Go does not mind a case label no value reaches.

// discriminatorFields are the action fields Const assigns. A switch on one of
// them is switching on the table.
var discriminatorFields = map[string]bool{"Sub": true, "Kind": true}

// TestNoHandSpelledDiscriminatorValues walks every non-generated, non-test Go
// file in the repository.
func TestNoHandSpelledDiscriminatorValues(t *testing.T) {
	values := map[string]string{} // value -> the constant that names it
	for _, v := range Verbs {
		for field, val := range v.Const {
			values[val] = ConstName(field, val)
		}
	}

	root := "../.."
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", ".harness-worktrees", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "_test.go") ||
			strings.HasSuffix(path, "actions_gen.go") {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			return nil // not ours to compile; the build says so
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sw, ok := n.(*ast.SwitchStmt)
			if !ok || sw.Tag == nil {
				return true
			}
			sel, ok := sw.Tag.(*ast.SelectorExpr)
			if !ok || !discriminatorFields[sel.Sel.Name] {
				return true
			}
			for _, stmt := range sw.Body.List {
				cc, ok := stmt.(*ast.CaseClause)
				if !ok {
					continue
				}
				for _, e := range cc.List {
					lit, ok := e.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					val, uerr := strconv.Unquote(lit.Value)
					if uerr != nil {
						continue
					}
					name, declared := values[val]
					if !declared {
						continue
					}
					t.Errorf("%s: `case %q` switches on .%s, whose values the declaration fixes.\n"+
						"Write verb.%s instead. A literal here is a second spelling of a table "+
						"entry: rename the value in cli/verb/table.go and this case matches "+
						"nothing, silently -- Go does not mind a case label no value reaches.",
						path, val, sel.Sel.Name, name)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
