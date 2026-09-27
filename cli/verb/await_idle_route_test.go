package verb

import (
	"reflect"
	"strings"
	"testing"
)

// session await-idle is the first depth-2 verb that also has children, so the
// longest-first path match is what sends `ls` to the child and a task id to
// the parent. Checked on both grammars that parse a typed line here.
func TestSessionAwaitIdleChildrenRoute(t *testing.T) {
	const id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, parse := range []struct {
		name string
		fn   func([]string, map[string]string) (Action, bool, error)
	}{{"cli", ParseCLICommand}, {"tui", ParseTUICommand}} {
		for _, tc := range []struct {
			line string
			sub  string
		}{
			{"session await-idle ls", "await-idle-ls"},
			{"session await-idle ls --task " + id, "await-idle-ls"},
			{"session await-idle kill 3 4", "await-idle-kill"},
			{"session await-idle " + id, "await-idle"},
		} {
			act, handled, err := parse.fn(strings.Fields(tc.line), nil)
			if err != nil || !handled {
				t.Fatalf("%s %q: handled=%v err=%v", parse.name, tc.line, handled, err)
			}
			sa, ok := act.(SessionAction)
			if !ok || sa.Sub != tc.sub {
				t.Fatalf("%s %q: got %#v, want SessionAction Sub=%q", parse.name, tc.line, act, tc.sub)
			}
			if tc.sub == "await-idle-kill" && !reflect.DeepEqual(sa.WatcherIDs, []uint64{3, 4}) {
				t.Fatalf("%s %q: WatcherIDs = %v", parse.name, tc.line, sa.WatcherIDs)
			}
			if tc.sub == "await-idle" && sa.TaskID != id {
				t.Fatalf("%s %q: TaskID = %q", parse.name, tc.line, sa.TaskID)
			}
		}
	}
	// `kill` with no id is a mistyped line, as `exec kill` is.
	if _, _, err := ParseCLICommand(strings.Fields("session await-idle kill"), nil); err == nil {
		t.Fatal("bare `session await-idle kill` parsed")
	}
}
