package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func keyRune(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

func newTestGitModal() GitModal {
	m := NewGitModal()
	m.Open("cafe1234deadbeef")
	m.SetSize(100, 30)
	m.SetLog(&cli.GitResult{
		Kind: protocol.GitQueryKind_Log,
		Commits: []cli.GitCommit{
			{SHA: "aaaaaaaaaaaa", Author: "claude", When: time.Unix(1700000000, 0), Subject: "second"},
			{SHA: "bbbbbbbbbbbb", Author: "claude", When: time.Unix(1699999000, 0), Subject: "first"},
		},
	})
	return m
}

// The two pseudo rows are client-side only; they must sit above the commits so
// the uncommitted state — the case the operator opens this for — is selected
// by default.
func TestGitModalPseudoRowsComeFirst(t *testing.T) {
	m := newTestGitModal()
	if got := m.RowCount(); got != 4 {
		t.Fatalf("RowCount = %d, want 2 pseudo + 2 commits", got)
	}
	if row := m.SelectedRow(); row.Kind != gitRowWorktree {
		t.Fatalf("initial selection = %v, want the worktree row", row.Kind)
	}
}

// The rows exist before any log arrives: a task with no commits still has an
// uncommitted state worth reading.
func TestGitModalHasPseudoRowsBeforeLog(t *testing.T) {
	m := NewGitModal()
	m.Open("cafe1234")
	if m.RowCount() != 2 {
		t.Fatalf("RowCount = %d, want the two pseudo rows", m.RowCount())
	}
}

func TestGitModalBaseDefaultsToHEAD(t *testing.T) {
	m := newTestGitModal()
	if m.BaseRev() != "HEAD" {
		t.Fatalf("BaseRev = %q, want HEAD", m.BaseRev())
	}
}

// `b` on a commit is the whole flexible-baseline feature: it must move the
// baseline without changing which row is selected.
func TestGitModalSetBaseFromCommit(t *testing.T) {
	m := newTestGitModal()
	for i := 0; i < 2; i++ {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	if m.SelectedRow().Kind != gitRowCommit {
		t.Fatalf("expected a commit row, got %v", m.SelectedRow().Kind)
	}
	before := m.SelectedIndex()
	m, _ = m.Update(keyRune('b'))
	if m.BaseRev() != "aaaaaaaaaaaa" {
		t.Fatalf("BaseRev = %q, want the selected commit", m.BaseRev())
	}
	if m.SelectedIndex() != before {
		t.Fatal("setting the baseline must not move the selection")
	}
}

func TestGitModalSetBaseOnPseudoRowIsIgnored(t *testing.T) {
	m := newTestGitModal()
	m, _ = m.Update(keyRune('b'))
	if m.BaseRev() != "HEAD" {
		t.Fatalf("BaseRev = %q; a pseudo row is not a commit-ish", m.BaseRev())
	}
}

// Opening on a different task must not inherit the previous task's baseline.
func TestGitModalOpenResetsBase(t *testing.T) {
	m := newTestGitModal()
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m, _ = m.Update(keyRune('b'))
	if m.BaseRev() == "HEAD" {
		t.Fatal("fixture did not move the baseline; the test proves nothing")
	}
	m.Open("0000111122223333")
	if m.BaseRev() != "HEAD" {
		t.Fatalf("BaseRev = %q after reopening on another task", m.BaseRev())
	}
	if m.TaskID() != "0000111122223333" {
		t.Fatalf("TaskID = %q", m.TaskID())
	}
}

func TestGitModalDownStopsAtLastRow(t *testing.T) {
	m := newTestGitModal()
	for i := 0; i < 20; i++ {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	if got := m.SelectedIndex(); got != m.RowCount()-1 {
		t.Fatalf("cursor = %d, want %d", got, m.RowCount()-1)
	}
}

func TestGitModalRendersDiffText(t *testing.T) {
	m := newTestGitModal()
	m.SetContent(&cli.GitResult{Kind: protocol.GitQueryKind_Diff, Text: "diff --git a/x b/x\n+added\n"})
	if out := m.View(); !strings.Contains(out, "+added") {
		t.Fatalf("view missing the diff body:\n%s", out)
	}
}

func TestGitModalEmptyDiffSaysSo(t *testing.T) {
	m := newTestGitModal()
	m.SetContent(&cli.GitResult{Kind: protocol.GitQueryKind_Diff, Text: ""})
	if out := m.View(); !strings.Contains(out, "no difference") {
		t.Fatalf("an empty diff must say so rather than render blank:\n%s", out)
	}
}

func TestGitModalAnnouncesTruncation(t *testing.T) {
	m := newTestGitModal()
	m.SetContent(&cli.GitResult{Kind: protocol.GitQueryKind_Diff, Text: "x\n", Truncated: true})
	if !strings.Contains(m.View(), "truncated") {
		t.Fatalf("truncation must be visible:\n%s", m.View())
	}
}

func TestGitModalAnnouncesLogTruncation(t *testing.T) {
	m := NewGitModal()
	m.Open("cafe1234")
	m.SetSize(100, 30)
	m.SetLog(&cli.GitResult{
		Kind:      protocol.GitQueryKind_Log,
		Commits:   []cli.GitCommit{{SHA: "aaa", Subject: "s"}},
		Truncated: true,
	})
	if !strings.Contains(m.View(), "truncated") {
		t.Fatalf("a cut commit list must say so:\n%s", m.View())
	}
}

// A failing query shows git's own words inside the modal rather than tearing
// it down or pushing the error to a pane the operator is not looking at.
func TestGitModalShowsGitStderr(t *testing.T) {
	m := newTestGitModal()
	m.SetError("fatal: bad revision 'zzz'")
	if !strings.Contains(m.View(), "bad revision 'zzz'") {
		t.Fatalf("view missing git's message:\n%s", m.View())
	}
}

func TestGitModalStatusSummaryCountsUntrackedSeparately(t *testing.T) {
	m := newTestGitModal()
	m.SetStatus(&cli.GitResult{
		Kind: protocol.GitQueryKind_Status,
		Entries: []cli.GitStatusEntry{
			{XY: " M", Path: "a.go"},
			{XY: "M ", Path: "b.go"},
			{XY: "??", Path: "c.txt"},
		},
	})
	out := m.View()
	if !strings.Contains(out, "2 changed, 1 untracked") {
		t.Fatalf("summary missing from:\n%s", out)
	}
}

func TestGitModalStatusSummaryClean(t *testing.T) {
	m := newTestGitModal()
	m.SetStatus(&cli.GitResult{Kind: protocol.GitQueryKind_Status})
	if !strings.Contains(m.View(), "clean") {
		t.Fatalf("a clean worktree should say so:\n%s", m.View())
	}
}

// Untracked files appear in no diff, so the status listing is the only place
// they are visible.
func TestGitModalStatusContentListsUntracked(t *testing.T) {
	m := newTestGitModal()
	m.SetStatusContent(&cli.GitResult{
		Kind:    protocol.GitQueryKind_Status,
		Entries: []cli.GitStatusEntry{{XY: "??", Path: "scratch.txt"}},
	})
	if !strings.Contains(m.View(), "scratch.txt") {
		t.Fatalf("untracked file missing from:\n%s", m.View())
	}
}

func TestGitModalNextFileJump(t *testing.T) {
	m := newTestGitModal()
	m.SetContent(&cli.GitResult{
		Kind: protocol.GitQueryKind_Diff,
		Text: "diff --git a/one b/one\n+a\ndiff --git a/two b/two\n+b\n",
	})
	start := m.ContentOffset()
	m, _ = m.Update(keyRune('n'))
	if m.ContentOffset() <= start {
		t.Fatalf("n did not advance the viewport (%d -> %d)", start, m.ContentOffset())
	}
	m, _ = m.Update(keyRune('N'))
	if m.ContentOffset() != start {
		t.Fatalf("N did not return to the first file header (%d, want %d)", m.ContentOffset(), start)
	}
}

func TestGitModalQueryForRow(t *testing.T) {
	m := newTestGitModal()

	kind, target, rev := m.GitQueryForRow(gitRow{Kind: gitRowWorktree})
	if kind != protocol.GitQueryKind_Diff || target != protocol.GitDiffTarget_Worktree || rev != "HEAD" {
		t.Fatalf("worktree row -> %v %v %q", kind, target, rev)
	}

	kind, target, _ = m.GitQueryForRow(gitRow{Kind: gitRowIndex})
	if kind != protocol.GitQueryKind_Diff || target != protocol.GitDiffTarget_Index {
		t.Fatalf("index row -> %v %v", kind, target)
	}

	kind, _, rev = m.GitQueryForRow(gitRow{Kind: gitRowCommit, Commit: cli.GitCommit{SHA: "abc"}})
	if kind != protocol.GitQueryKind_Show || rev != "abc" {
		t.Fatalf("commit row -> %v %q", kind, rev)
	}
}

// After b, the worktree row must query against the chosen commit — that is the
// only thing that makes the baseline flexible.
func TestGitModalQueryForRowUsesTheChosenBase(t *testing.T) {
	m := newTestGitModal()
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m, _ = m.Update(keyRune('b'))
	_, _, rev := m.GitQueryForRow(gitRow{Kind: gitRowWorktree})
	if rev != "aaaaaaaaaaaa" {
		t.Fatalf("worktree row still queries against %q", rev)
	}
}

func TestGitModalClosedIgnoresKeys(t *testing.T) {
	m := NewGitModal()
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if m2.SelectedIndex() != 0 {
		t.Fatal("a closed modal must not consume navigation")
	}
}
