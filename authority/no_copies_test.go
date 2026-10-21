package authority

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The names scope semantics used to be written under outside this package.
// The server resolved them in server/, and the clients restated them in cli/ —
// "mirrors the server's Scope.ForCap" — so a change to how a scope reads had
// to be made twice, and the client half was easy to miss.
var formerCopies = []string{
	"scopeForCap", "reachOfDescendant", "actionSet", "visibleSetOf", "descendantsOf",
	"clampScopeUnder", "isNarrowing", "minScopeBase", "scopeBaseRank", "validateScope",
	"exercisedBits", "attachModeScopeCap",
}

// TestNoSecondScopeImplementation fails when a function under one of those
// names is declared again anywhere that enforces or displays authority.
func TestNoSecondScopeImplementation(t *testing.T) {
	decl := regexp.MustCompile(`(?m)^func (?:\([^)]*\) )?(` + strings.Join(formerCopies, "|") + `)\(`)
	for _, dir := range []string{"../server", "../cli", "../tui", "../cmd"} {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range decl.FindAllStringSubmatch(string(src), -1) {
				t.Errorf("%s declares %s: scope semantics live in package authority; call it instead of restating it", path, m[1])
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
}
