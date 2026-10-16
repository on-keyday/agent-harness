package agentboard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"testing"
)

// reachGated lists every Board method that reaches a task's subscription set,
// with whether it must ask the reach predicate (the reach spec's §5 table). A
// method that starts calling taskState.matches or snapshotPatterns fails this
// test until it is added here with a decision — the next reader is the gap
// otherwise.
var reachGated = map[string]bool{
	"Send": true, "Wake": true, "Subscribes": true, "Inbox": true,
	"InboxAdvance": true, "ListSubscribers": true,
	"anyTaskMatchesLocked": false, // cleanup: the raw set can only keep more
	"Revoke":               false, // cleanup loop over snapshotPatterns
	"ListSubscriptions":    false, // reports the set itself
}

func TestReachCallSitesAreListed(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "board.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := func(body *ast.BlockStmt, names ...string) bool {
		hit := false
		ast.Inspect(body, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				for _, name := range names {
					if sel.Sel.Name == name {
						hit = true
					}
				}
			}
			return true
		})
		return hit
	}
	var found []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Body == nil || !calls(fn.Body, "matches", "snapshotPatterns") {
			continue
		}
		found = append(found, fn.Name.Name)
		gated, listed := reachGated[fn.Name.Name]
		if !listed {
			t.Errorf("Board.%s reaches the subscription set and is not in reachGated: decide whether it asks the reach predicate", fn.Name.Name)
			continue
		}
		if gated && !calls(fn.Body, "readableFor", "readableTopic", "readableTargets", "readablePatterns", "Readable") {
			t.Errorf("Board.%s is gated but never asks the reach predicate", fn.Name.Name)
		}
	}
	sort.Strings(found)
	t.Logf("subscription-set readers: %v", found)
}
