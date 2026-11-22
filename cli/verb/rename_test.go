package verb

import (
	"strings"
	"testing"
)

func TestSessionRenameParses(t *testing.T) {
	id := strings.Repeat("a", 32)
	a, err := ParseCmdSessionRename(CLI, []string{id, "nightly-ci"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.TaskID != id || a.Name != "nightly-ci" {
		t.Fatalf("action = %+v", a)
	}
	if _, err := ParseCmdSessionRename(CLI, []string{id}, nil); err == nil {
		t.Fatal("a missing name parsed")
	}
	if a, err := ParseCmdSessionRename(CLI, []string{id, ""}, nil); err != nil || a.Name != "" {
		t.Fatalf("an empty name (clear) = %+v, %v", a, err)
	}
}
