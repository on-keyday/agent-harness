package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// One harness-cli process serves many MCP tool calls at once, with stdin and
// stdout carrying the JSON-RPC stream (spec §7, D16, D18). So a tool's body may
// not end the process, read fd 0, write fd 1 or run outside the call's context:
// it returns an exit-code error, reads h.stdin(), writes h.stdout() and dials
// with h.ctx. This scans every function in the package;
// the ones allowed to touch the process's streams are named below with the
// reason -- the process entry points, and the bodies of verbs that are NOT
// tools (VerbSpec.MCPNotATool), which own the terminal by design.
var processStreamAllowed = map[string]string{
	"main":                       "the process entry point",
	"die":                        "turns a verb's error into the process's exit",
	"helpWidth":                  "asks the terminal's width for -h; never reached by a tool call (help is a parse error there)",
	"forceExitWithStacks":        "second interrupt of a blocking verb",
	"runExecAction":              "exec is not a tool: it splices stdin/stdout to the command",
	"runFileEdit":                "file edit is not a tool: it runs $EDITOR on the terminal",
	"runFileNew":                 "file new is not a tool: it runs $EDITOR on the terminal",
	"editViaExternalEditor":      "file edit / new: the editor gets the terminal",
	"Forward":                    "forward is not a tool: it blocks holding forwards open",
	"readFlagBody":               "forward's --http-body @- reads stdin",
	"runSessionStreamAttachWith": "session stream attach is not a tool: it streams",
	"Lifecycle":                  "lifecycle is not a tool: it is a hook reading stdin",
	"serveMCP":                   "the MCP server itself owns stdin and stdout",
	"stdout":                     "cliVerbs' nil-safe default: a cliVerbs built with no sink is the command line's",
	"stdin":                      "cliVerbs' nil-safe default: a cliVerbs built with no reader is the command line's",
	"baseCtx":                    "cliVerbs' nil-safe default: a cliVerbs built with no context (a test) runs under Background",
}

func TestToolBodiesDoNotTouchProcessStreams(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			fn := fd.Name.Name
			ast.Inspect(fd, func(n ast.Node) bool {
				// A call to die ends the process as surely as os.Exit does.
				if call, ok := n.(*ast.CallExpr); ok {
					if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "die" {
						if _, allowed := processStreamAllowed[fn]; !allowed {
							found = append(found, name+":"+fn+": die(...) (line "+itoaLine(fset.Position(call.Pos()).Line)+")")
						} else {
							seen[fn] = true
						}
					}
				}
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				x, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				// A context of the body's own making escapes the call's: MCP's
				// cancellation and the verb's declared Sends (spec §7, §8.2).
				bad := (x.Name == "os" && (sel.Sel.Name == "Exit" || sel.Sel.Name == "Stdin" || sel.Sel.Name == "Stdout")) ||
					(x.Name == "fmt" && (sel.Sel.Name == "Print" || sel.Sel.Name == "Printf" || sel.Sel.Name == "Println")) ||
					(x.Name == "context" && (sel.Sel.Name == "Background" || sel.Sel.Name == "TODO"))
				if !bad {
					return true
				}
				if _, allowed := processStreamAllowed[fn]; allowed {
					seen[fn] = true
					return true
				}
				found = append(found, name+":"+fn+": "+x.Name+"."+sel.Sel.Name+" (line "+itoaLine(fset.Position(sel.Pos()).Line)+")")
				return true
			})
		}
	}
	sort.Strings(found)
	for _, f := range found {
		t.Errorf("process stream outside the allowed bodies: %s -- write h.stdout(), read h.stdin(), return an exit-code error, use h.ctx", f)
	}
	for fn := range processStreamAllowed {
		if !seen[fn] {
			t.Errorf("%s is allowed but touches no process stream any more: remove the entry", fn)
		}
	}
}

func itoaLine(n int) string {
	s := ""
	for ; n > 0; n /= 10 {
		s = string(rune('0'+n%10)) + s
	}
	return s
}
