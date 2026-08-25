package tui

import (
	"strings"
	"testing"
)

func TestParseExecRun(t *testing.T) {
	cases := []struct {
		in       []string
		wantSub  string
		wantID   string
		wantCmd  []string
		wantKill uint64
		wantErr  bool
	}{
		{in: []string{"ls"}, wantSub: "ls"},
		{in: []string{"ls", "-task", "abcdef012345"}, wantSub: "ls", wantID: "abcdef012345"},
		{in: []string{"kill", "3"}, wantSub: "kill", wantKill: 3},
		{in: []string{"abcdef012345", "git", "status"}, wantSub: "run", wantID: "abcdef012345", wantCmd: []string{"git", "status"}},
		{in: []string{"abcdef012345", "--", "git", "status"}, wantSub: "run", wantID: "abcdef012345", wantCmd: []string{"git", "status"}},
		// A command whose first word collides with a sub-verb still runs, as
		// long as it is introduced by the task id: `exec <id> ls` is a command.
		{in: []string{"abcdef012345", "ls", "-la"}, wantSub: "run", wantID: "abcdef012345", wantCmd: []string{"ls", "-la"}},
		{in: []string{"abcdef012345"}, wantErr: true},
		{in: nil, wantErr: true},
		{in: []string{"kill", "notanumber"}, wantErr: true},
		{in: []string{"kill"}, wantErr: true},
		{in: []string{"ls", "-task"}, wantErr: true},
	}
	for _, tc := range cases {
		act, err := parseExecRun(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseExecRun(%v) = nil error, want one", tc.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("parseExecRun(%v): %v", tc.in, err)
		}
		a, ok := act.(ExecRunAction)
		if !ok {
			t.Fatalf("parseExecRun(%v) returned %T", tc.in, act)
		}
		if a.Sub != tc.wantSub || a.TaskID != tc.wantID {
			t.Errorf("parseExecRun(%v) = %+v, want sub=%q id=%q", tc.in, a, tc.wantSub, tc.wantID)
		}
		if a.ExecID != tc.wantKill {
			t.Errorf("parseExecRun(%v) exec id = %d, want %d", tc.in, a.ExecID, tc.wantKill)
		}
		if len(a.Argv) != len(tc.wantCmd) {
			t.Fatalf("parseExecRun(%v) argv = %v, want %v", tc.in, a.Argv, tc.wantCmd)
		}
		for i := range tc.wantCmd {
			if a.Argv[i] != tc.wantCmd[i] {
				t.Errorf("parseExecRun(%v) argv[%d] = %q, want %q", tc.in, i, a.Argv[i], tc.wantCmd[i])
			}
		}
	}
}

// The argv reaches the runner verbatim. Re-joining and re-splitting it would
// silently turn one argument containing a space into two, which is the whole
// reason the CLI takes it after a `--` rather than as a string.
func TestParseExecRunKeepsArgvVerbatim(t *testing.T) {
	act, err := parseExecRun([]string{"abcdef012345", "--", "sh", "-c", "echo one two"})
	if err != nil {
		t.Fatalf("parseExecRun: %v", err)
	}
	a := act.(ExecRunAction)
	if len(a.Argv) != 3 || a.Argv[2] != "echo one two" {
		t.Errorf("argv = %q, want the three-word form with its last argument intact", a.Argv)
	}
}

// A command's output is drawn inside a bordered panel, so it must not be able
// to move the cursor out of it. The pane had only trusted producers before this
// verb; sanitizeOutput exists in this package for exactly this and the raw
// forward pane already uses it.
func TestExecOutputLineSanitizes(t *testing.T) {
	cases := []struct{ name, in, wantAbsent string }{
		{"alt screen", "before\x1b[?1049hafter", "\x1b"},
		{"clear screen", "\x1b[2Jgone", "\x1b"},
		{"scroll region", "\x1b[1;5rboom", "\x1b"},
		{"colour", "\x1b[31mred", "\x1b"},
		{"bare CR overwrites the border", "50%\r100%", "\r"},
		{"NUL", "a\x00b", "\x00"},
	}
	for _, tc := range cases {
		for _, stderr := range []bool{false, true} {
			got := execOutputLine(tc.in, stderr)
			// The stderr marker is OURS and carries its own styling, so strip
			// the prefix before looking for escapes in the command's half.
			body := got[strings.Index(got, "| ")+2:]
			if strings.Contains(body, tc.wantAbsent) {
				t.Errorf("%s (stderr=%v): %q still carries %q", tc.name, stderr, body, tc.wantAbsent)
			}
		}
	}
}

// Sanitizing must not eat the content. A tab is layout an operator wants (it is
// what `git status` and every table-printing command emit), and ordinary text
// has to survive verbatim.
func TestExecOutputLineKeepsRealContent(t *testing.T) {
	got := execOutputLine("modified:\tcli/exec_run.go", false)
	if !strings.Contains(got, "modified:\tcli/exec_run.go") {
		t.Errorf("output = %q, want the tab and the text intact", got)
	}
}
