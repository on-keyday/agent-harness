package verb_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestVerbImportsNothingFromCli is the load-bearing constraint of the whole
// design (spec D23). cli/cmd_board.go parses the `board` family INSIDE package
// cli, so cli must be free to import cli/verb. The moment cli/verb imports cli
// back, that becomes a cycle and the board migration is blocked -- discovered
// at Phase 3, after five phases were built on the wrong direction.
//
// Checked here rather than left to the compiler because the compiler only
// complains once the cycle actually closes, which is phases away.
func TestVerbImportsNothingFromCli(t *testing.T) {
	const forbidden = "github.com/on-keyday/agent-harness/cli"

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read .: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, filepath.Clean(e.Name()), nil, parser.ImportsOnly)
		if perr != nil {
			t.Fatalf("parse %s: %v", e.Name(), perr)
		}
		for _, imp := range f.Imports {
			path, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil {
				continue
			}
			if path == forbidden {
				t.Errorf("%s:%d: cli/verb imports %q.\n"+
					"cli/verb is BELOW cli: package cli parses the board family itself "+
					"(cli/cmd_board.go), so cli must import cli/verb and not the reverse. "+
					"If a helper is needed here, move the helper down rather than importing up.",
					e.Name(), fset.Position(imp.Pos()).Line, path)
			}
		}
	}
}
