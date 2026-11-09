package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The spawn limits are checked inside CreateWithinLimits, under the store's
// lock. TaskStore.Create stays unchecked for the tests and fixtures that build
// trees directly, so a production call to it would create tasks past every
// limit and nothing else would notice.
func TestOnlyCreateWithinLimitsInProduction(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	call := regexp.MustCompile(`(?:\.Tasks|\.tasks)\.Create\(`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if call.MatchString(line) {
				t.Errorf("%s:%d calls TaskStore.Create; production creates through CreateWithinLimits", f, i+1)
			}
		}
	}
}
