package peer_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// An objproto.Endpoint is process-scoped: it owns the socket and the map of
// every connection on it, it has no Close, and Dial takes one as a parameter.
// A process that dials again must dial on the SAME one.
//
// That rule was written down once, lost when a refactor moved construction into
// the dial path, and then un-written: `d75ba5a1`'s review found the doc no
// longer matched the code and updated the DOC. Two weeks later `--persist` put
// that dial path inside a retry loop, and every reconnect started building an
// endpoint — four goroutines and (on udp) a bound socket per attempt, measured
// 2026-09-11 on a runner reconnecting in a loop.
//
// A comment cannot survive that. This test can: it fixes WHICH functions may
// construct an endpoint, so a new call site has to be argued for here rather
// than added quietly. Nothing about how often each one is called is checkable
// statically — what is checkable is that the set does not grow.
func TestEndpointConstructionSitesAreFixed(t *testing.T) {
	// callee → the functions allowed to call it, with the reason each one is
	// allowed to make an endpoint at all.
	allowed := map[string]map[string]string{
		"BuildClientEndpoint": {
			"NewProcessEndpoint": "the one a long-lived client keeps; callers hold it across reconnects",
			"DialViaProxy":       "the Phase B proxy leg dials a runner's listen address, possibly another transport; built once and shared by its collision retries",
		},
		"buildRunnerEndpoint": {
			"NewDialEndpoint": "the one a dial-mode runner keeps; agent-runner builds it above PersistLoop",
		},
		"WebSocketEndpoint": {
			"BuildClientEndpoint": "the client-side constructor itself",
			"buildRunnerEndpoint": "the runner-side constructor itself",
			"buildListenEndpoint": "listen mode: the server dials IN, so this endpoint is the listener",
		},
		"UDPEndpoint": {
			"BuildClientEndpoint": "the client-side constructor itself",
			"buildRunnerEndpoint": "the runner-side constructor itself",
			"buildListenEndpoint": "listen mode: the server dials IN, so this endpoint is the listener",
		},
		"UDPWebsocketDualStackEndpoint": {
			"buildRunnerEndpoint": "one endpoint carrying both legs when --server-cid spans ws and udp",
			"buildListenEndpoint": "listen mode's own dualstack case",
		},
	}

	root := repoRoot(t)
	type site struct{ callee, caller, where string }
	var bad []site

	for _, dir := range []string{"cli", "runner", "peer", "cmd", "tui"} {
		walkGoFiles(t, filepath.Join(root, dir), func(path string, file *ast.File, fset *token.FileSet) {
			// Tests may build endpoints freely: they are one process per run,
			// and pinning their call sites would make this file a chore rather
			// than a guard.
			if strings.HasSuffix(path, "_test.go") {
				return
			}
			var enclosing string
			ast.Inspect(file, func(n ast.Node) bool {
				switch v := n.(type) {
				case *ast.FuncDecl:
					enclosing = v.Name.Name
				case *ast.CallExpr:
					name := calleeName(v.Fun)
					callers, watched := allowed[name]
					if !watched {
						return true
					}
					if _, ok := callers[enclosing]; !ok {
						rel, _ := filepath.Rel(root, path)
						bad = append(bad, site{name, enclosing, rel + ":" +
							strings.TrimPrefix(fset.Position(v.Pos()).String(), path+":")})
					}
				}
				return true
			})
		})
	}

	if len(bad) > 0 {
		sort.Slice(bad, func(i, j int) bool { return bad[i].where < bad[j].where })
		var b strings.Builder
		b.WriteString("an objproto.Endpoint is built somewhere new:\n")
		for _, s := range bad {
			b.WriteString("  " + s.where + ": " + s.caller + " calls " + s.callee + "\n")
		}
		b.WriteString("\nAn Endpoint is process-scoped — one socket bundling every connection\n" +
			"made on it, no Close, and peer.Dial takes it as a parameter. If this\n" +
			"call runs once per PROCESS, add it to the allowlist above with the\n" +
			"reason. If it can run once per DIAL, it is the defect this test exists\n" +
			"for: hold one endpoint and dial on it (cli.DialWith / runner.ConnectWith).")
		t.Fatal(b.String())
	}
}

// calleeName reduces `pkg.Fn(...)` and `Fn(...)` to `Fn`.
func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

func walkGoFiles(t *testing.T, dir string, fn func(string, *ast.File, *token.FileSet)) {
	t.Helper()
	fset := token.NewFileSet()
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		// ParseComments is not needed; positions are.
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		fn(path, f, fset)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// This file lives in peer/, so the root is one up. Resolved rather than
	// assumed so a `go test ./...` from anywhere finds the same tree.
	root := filepath.Dir(wd)
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root not found from %s: %v", wd, err)
	}
	return root
}
