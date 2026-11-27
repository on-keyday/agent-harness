package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// G on a picker entry asks for the git diff of THAT entry, joined onto the
// directory being browsed — the path git is given is worktree-relative, not
// the bare name on the row.
func TestPickerGitKeyNamesTheEntryPath(t *testing.T) {
	m := NewFilePicker()
	m.open = true
	m.taskID = "cafe1234deadbeef"
	m.curDir = "pkg"
	m.entries = []cli.FileEntryView{{Name: "inner", IsDir: true}, {Name: "a.go"}}
	for i, want := range []string{"pkg/inner", "pkg/a.go"} {
		m.cursor = i
		_, cmd := m.Update(keyRune('G'))
		if cmd == nil {
			t.Fatalf("G on %q raised nothing", want)
		}
		got, ok := cmd().(FileGitRequestMsg)
		if !ok || got.TaskID != m.taskID || got.Path != want {
			t.Fatalf("G on row %d = %#v, want path %q", i, cmd(), want)
		}
	}
}

func TestPickerGitKeyWithNothingSelected(t *testing.T) {
	m := NewFilePicker()
	m.open = true
	m.taskID = "cafe1234deadbeef"
	m, cmd := m.Update(keyRune('G'))
	if cmd != nil {
		t.Fatalf("G on an empty listing raised %#v", cmd())
	}
	if m.msg == "" {
		t.Fatal("G on an empty listing must say why nothing happened")
	}
}

func newNarrowedGitApp() *App {
	a := &App{gitModal: NewGitModal(), cmdresult: NewCmdResult(), width: 100, height: 30}
	a.gitModal.Open("cafe1234deadbeef")
	return a
}

// An empty narrowed diff names its path (a bare "(no difference)" reads as a
// clean worktree) and asks for that path's status, which is where an
// untracked file shows up. The worktree row's summary is the WHOLE worktree's
// and must not be replaced by the one path's count.
func TestNarrowedEmptyDiffFallsBackToThePathsStatus(t *testing.T) {
	a := newNarrowedGitApp()
	id := a.gitModal.TaskID()
	a.gitModal.SetStatus(&cli.GitResult{Entries: []cli.GitStatusEntry{{XY: " M", Path: "x"}, {XY: " M", Path: "y"}}})

	_, cmd := a.Update(GitResultMsg{TaskID: id, Kind: protocol.GitQueryKind_Diff, Path: "new.txt", Result: &cli.GitResult{}})
	if cmd == nil {
		t.Fatal("an empty narrowed diff issued no status query")
	}
	if out := a.gitModal.View(); !strings.Contains(out, "no difference in new.txt") {
		t.Fatalf("empty narrowed diff does not name its path:\n%s", out)
	}

	a.Update(GitResultMsg{TaskID: id, Kind: protocol.GitQueryKind_Status, Path: "new.txt",
		Result: &cli.GitResult{Entries: []cli.GitStatusEntry{{XY: "??", Path: "new.txt"}}}})
	out := a.gitModal.View()
	if !strings.Contains(out, "?? new.txt") {
		t.Fatalf("the path's status is not in the content:\n%s", out)
	}
	if !strings.Contains(out, "2 changed") {
		t.Fatalf("the narrowed status replaced the whole-worktree summary:\n%s", out)
	}
}

// A narrowed status that arrives after the operator moved to other content
// must not paint over it.
func TestLateNarrowedStatusLeavesNewContentAlone(t *testing.T) {
	a := newNarrowedGitApp()
	id := a.gitModal.TaskID()
	a.Update(GitResultMsg{TaskID: id, Kind: protocol.GitQueryKind_Diff, Path: "new.txt", Result: &cli.GitResult{}})
	a.gitModal.SetContent(&cli.GitResult{Text: "diff --git a/z b/z\n+moved on\n"})

	a.Update(GitResultMsg{TaskID: id, Kind: protocol.GitQueryKind_Status, Path: "new.txt",
		Result: &cli.GitResult{Entries: []cli.GitStatusEntry{{XY: "??", Path: "new.txt"}}}})
	if out := a.gitModal.View(); !strings.Contains(out, "+moved on") || strings.Contains(out, "?? new.txt") {
		t.Fatalf("a late narrowed status overwrote newer content:\n%s", out)
	}
}

// A narrowed diff that has content renders as any diff does.
func TestNarrowedDiffWithContentRendersIt(t *testing.T) {
	a := newNarrowedGitApp()
	_, cmd := a.Update(GitResultMsg{TaskID: a.gitModal.TaskID(), Kind: protocol.GitQueryKind_Diff, Path: "x",
		Result: &cli.GitResult{Text: "diff --git a/x b/x\n+two\n"}})
	if cmd != nil {
		t.Fatal("a non-empty narrowed diff issued a follow-up query")
	}
	if out := a.gitModal.View(); !strings.Contains(out, "+two") {
		t.Fatalf("narrowed diff body missing:\n%s", out)
	}
}

// The picker's key line wraps on an 80-column terminal. Its rows have to come
// out of the entry budget, or the box grows past the screen and its top edge
// (with the title) is pushed off.
func TestPickerFitsAnEightyByTwentyFourTerminal(t *testing.T) {
	m := NewFilePicker()
	m.open = true
	m.taskID = "cafe1234deadbeef"
	m.taskShort = "cafe1234dead"
	m.entries = []cli.FileEntryView{{Name: "a.go"}}
	for _, sz := range [][2]int{{80, 24}, {100, 30}, {200, 45}} {
		m.SetSize(sz[0], sz[1])
		if h := lipgloss.Height(m.View()); h > sz[1] {
			t.Errorf("%dx%d: picker is %d rows, taller than the terminal", sz[0], sz[1], h)
		}
	}
}
