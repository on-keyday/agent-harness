package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// Which bits a request needs, and which bit's scope binds its target, are
// decided in package authority (docs/superpowers/specs/
// 2026-11-22-request-cap-need-design.md). A capability constant written in a
// server handler is a second place that decision lives, and the two drift.
//
// So this test refuses ANY reference to a protocol.Capability_* constant in the
// server's non-test sources -- not only one passed straight to hasCap or
// inScope, because the server also writes `need := protocol.Capability_X` and
// checks it three lines later. A use that is not a requirement (the operator's
// all-bits, the empty set, the board's own reach source) is allowed by name
// below, with the reason.
var capabilityLiteralAllowed = map[string]string{
	// The empty set asks inScope for the VISIBILITY set, not an action bit.
	"await_idle_handler.go:handleAwaitIdle:Capability_None":         "visibility query (inScope with no bit)",
	"await_progress_handler.go:handleAwaitProgress:Capability_None": "visibility query (inScope with no bit)",
	"capabilities.go:scopeSetWith:Capability_None":                  "the no-bit case is the visibility branch",
	"capabilities.go:visibleToCaller:Capability_None":               "visibility query (scopeSet with no bit)",

	// What a caller HOLDS, not what a request needs.
	"capabilities.go:callerCaps:Capability_All":       "the operator holds every bit",
	"capabilities.go:callerCaps:Capability_None":      "a principal whose record holds nothing holds no bit",
	"capabilities.go:agentCallerCaps:Capability_None": "a board caller with no live principal holds no bit",
	"taskstore_reach.go:CapsOf:Capability_None":       "an unknown task holds no bit",
	"task_handler.go:Handle:Capability_All":           "whoami: the operator's delegation bound is every bit",
	"task_handler.go:Handle:Capability_None":          "whoami: a task's delegation bound starts empty before its record is read",

	// The bits a credential task can never be granted: a property of the
	// credential kind (credential-tasks D13), not of any request.
	"credential_store.go:package-level:Capability_CredentialIssue": "CredentialMaskedCaps",
	"credential_store.go:package-level:Capability_ScheduleEdit":    "CredentialMaskedCaps",

	// A WAL record written before the visibility rank existed: board_observe
	// then implied global visibility, and replay keeps that meaning.
	"scope.go:scopeFromWAL:Capability_BoardObserve": "WAL compatibility: pre-vis-rank board_observe meant global visibility",

	// The board's reach source: which chat topics board_peer's scope opens.
	// Reach is agentboard's decision, outside RequestNeed (spec §2).
	"taskstore_reach.go:PeerCovers:Capability_BoardPeer": "board reach source",
	"taskstore_reach.go:PeerGlobal:Capability_BoardPeer": "board reach source",
}

func TestNoCapabilityLiteralOutsideAuthority(t *testing.T) {
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
		protocolName := ""
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == "github.com/on-keyday/agent-harness/runner/protocol" {
				protocolName = "protocol"
				if imp.Name != nil {
					protocolName = imp.Name.Name
				}
			}
		}
		if protocolName == "" {
			continue
		}
		// Each reference is attributed to its enclosing function (or to the
		// package-level declaration it sits in).
		for _, decl := range f.Decls {
			owner := "package-level"
			if fd, ok := decl.(*ast.FuncDecl); ok {
				owner = fd.Name.Name
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || !strings.HasPrefix(sel.Sel.Name, "Capability_") {
					return true
				}
				if x, ok := sel.X.(*ast.Ident); !ok || x.Name != protocolName {
					return true
				}
				key := name + ":" + owner + ":" + sel.Sel.Name
				seen[key] = true
				if _, allowed := capabilityLiteralAllowed[key]; !allowed {
					found = append(found, key+" (line "+itoa(fset.Position(sel.Pos()).Line)+")")
				}
				return true
			})
		}
	}
	sort.Strings(found)
	for _, f := range found {
		t.Errorf("capability constant outside authority: %s -- take the bit from authority "+
			"(RequestNeed, ScopeBit, CentralCap or a helper there), or, if it is not a "+
			"requirement, allow it in capabilityLiteralAllowed with the reason", f)
	}
	for key, reason := range capabilityLiteralAllowed {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("%s is allowed without a reason", key)
		}
		if !seen[key] {
			t.Errorf("%s is allowed but no longer occurs: remove the entry", key)
		}
	}
}
