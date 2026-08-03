package tui

import (
	"fmt"
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

func withSubrepos(m GitModal, paths ...string) GitModal {
	m.SetSubrepos(&cli.GitResult{Kind: protocol.GitQueryKind_Subrepos, Subrepos: paths})
	return m
}

// [REPO] rows sit BELOW the commits: they are navigation, and the uncommitted
// state must stay the default selection.
func TestGitModalSubrepoRowsComeLast(t *testing.T) {
	m := withSubrepos(newTestGitModal(), "pkg/inner", "vendor/lib")
	if got := m.RowCount(); got != 6 {
		t.Fatalf("RowCount = %d, want 2 pseudo + 2 commits + 2 repos", got)
	}
	if m.SelectedRow().Kind != gitRowWorktree {
		t.Fatalf("selection moved to %v", m.SelectedRow().Kind)
	}
	last := m.rows[len(m.rows)-1]
	if last.Kind != gitRowSubrepo || last.Subrepo != "vendor/lib" {
		t.Fatalf("last row = %+v", last)
	}
}

func TestGitModalEnterSubrepoResetsEverythingTheOldRepoOwned(t *testing.T) {
	m := withSubrepos(newTestGitModal(), "pkg/inner")
	// Move the baseline off its default so the reset is observable.
	for i := 0; i < 2; i++ {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	m, _ = m.Update(keyRune('b'))
	if m.BaseRev() == "HEAD" {
		t.Fatal("fixture did not move the baseline; the test proves nothing")
	}

	m.EnterSubrepo("pkg/inner")
	if m.Subrepo() != "pkg/inner" {
		t.Fatalf("Subrepo = %q", m.Subrepo())
	}
	if m.BaseRev() != "HEAD" {
		t.Fatalf("BaseRev = %q — that commit belongs to the repository we left", m.BaseRev())
	}
	if m.RowCount() != 2 {
		t.Fatalf("RowCount = %d — the old repository's commits survived the move", m.RowCount())
	}
	if m.SelectedIndex() != 0 {
		t.Fatalf("cursor = %d", m.SelectedIndex())
	}
}

func TestGitModalEnterSubrepoNests(t *testing.T) {
	m := newTestGitModal()
	m.EnterSubrepo("pkg/inner")
	// The runner reports paths relative to the CURRENT root, so a second entry
	// has to append rather than replace.
	m.EnterSubrepo("deeper")
	if m.Subrepo() != "pkg/inner/deeper" {
		t.Fatalf("Subrepo = %q", m.Subrepo())
	}
}

func TestGitModalLeaveSubrepo(t *testing.T) {
	m := newTestGitModal()
	m.EnterSubrepo("pkg/inner")
	m.EnterSubrepo("deeper")

	if !m.LeaveSubrepo() || m.Subrepo() != "pkg/inner" {
		t.Fatalf("one level up = %q", m.Subrepo())
	}
	if !m.LeaveSubrepo() || m.Subrepo() != "" {
		t.Fatalf("two levels up = %q", m.Subrepo())
	}
	if m.LeaveSubrepo() {
		t.Fatal("at the worktree there is nowhere further up; must report false")
	}
}

// Every query the modal issues has to carry the current root, or it silently
// answers about the outer repository.
func TestGitModalQueryCarriesRootAndSubmodule(t *testing.T) {
	m := newTestGitModal()
	if q := m.Query(); q.Subrepo != "" || q.SubmoduleDiff {
		t.Fatalf("fresh query = %+v", q)
	}
	m.EnterSubrepo("pkg/inner")
	m.ToggleSubmodule()
	q := m.Query()
	if q.Subrepo != "pkg/inner" || !q.SubmoduleDiff {
		t.Fatalf("query = %+v", q)
	}
}

func TestGitModalToggleSubmodule(t *testing.T) {
	m := newTestGitModal()
	if m.SubmoduleDiff() {
		t.Fatal("off by default")
	}
	if !m.ToggleSubmodule() || !m.SubmoduleDiff() {
		t.Fatal("toggle on failed")
	}
	if m.ToggleSubmodule() || m.SubmoduleDiff() {
		t.Fatal("toggle off failed")
	}
}

func TestGitModalHeaderNamesTheRoot(t *testing.T) {
	m := withSubrepos(newTestGitModal(), "pkg/inner")
	if !strings.Contains(m.View(), "(root)") {
		t.Fatalf("header should name the worktree as the root:\n%s", m.View())
	}
	if !strings.Contains(m.View(), "[REPO]") {
		t.Fatalf("nested repos must be visible in the picker:\n%s", m.View())
	}
	m.EnterSubrepo("pkg/inner")
	if !strings.Contains(m.View(), "pkg/inner") {
		t.Fatalf("header should name the current subrepo:\n%s", m.View())
	}
}

func TestGitModalOpenResetsTheRoot(t *testing.T) {
	m := newTestGitModal()
	m.EnterSubrepo("pkg/inner")
	m.ToggleSubmodule()
	m.Open("0000111122223333")
	if m.Subrepo() != "" || m.SubmoduleDiff() {
		t.Fatalf("a new task inherited subrepo=%q submodule=%v", m.Subrepo(), m.SubmoduleDiff())
	}
}

// b on a [REPO] row must not set the baseline: a directory is not a commit-ish.
func TestGitModalSetBaseOnSubrepoRowIsIgnored(t *testing.T) {
	m := withSubrepos(newTestGitModal(), "pkg/inner")
	for i := 0; i < 4; i++ {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	if m.SelectedRow().Kind != gitRowSubrepo {
		t.Fatalf("expected a subrepo row, got %v", m.SelectedRow().Kind)
	}
	m, _ = m.Update(keyRune('b'))
	if m.BaseRev() != "HEAD" {
		t.Fatalf("BaseRev = %q", m.BaseRev())
	}
}

func manyCommits(n int) []cli.GitCommit {
	out := make([]cli.GitCommit, n)
	for i := range out {
		out[i] = cli.GitCommit{
			SHA:     fmt.Sprintf("%040x", i),
			Author:  "claude",
			When:    time.Unix(1700000000+int64(i), 0),
			Subject: fmt.Sprintf("commit %d", i),
		}
	}
	return out
}

func viewLines(m GitModal) int { return strings.Count(m.View(), "\n") + 1 }

// A long history must not push the modal past the terminal. SetSize reserved a
// capped number of picker lines while View rendered every row, so a hundred
// commits made the box ~90 lines too tall and the top scrolled away.
func TestGitModalViewFitsTheTerminal(t *testing.T) {
	for _, h := range []int{10, 24, 40, 60} {
		m := NewGitModal()
		m.Open("cafe1234")
		m.SetSize(120, h)
		m.SetLog(&cli.GitResult{Kind: protocol.GitQueryKind_Log, Commits: manyCommits(200)})
		m.SetSubrepos(&cli.GitResult{Kind: protocol.GitQueryKind_Subrepos, Subrepos: []string{"pkg/inner", "sub"}})
		m.SetContent(&cli.GitResult{Kind: protocol.GitQueryKind_Diff, Text: strings.Repeat("+line\n", 500)})
		if got := viewLines(m); got > h {
			t.Errorf("h=%d rendered %d lines", h, got)
		}
	}
}

// Truncation notes and an error message are the other things that can push the
// box over; they are inside the same budget.
func TestGitModalViewFitsWithEveryNoteShowing(t *testing.T) {
	const h = 24
	m := NewGitModal()
	m.Open("cafe1234")
	m.SetSize(120, h)
	m.SetLog(&cli.GitResult{Kind: protocol.GitQueryKind_Log, Commits: manyCommits(300), Truncated: true})
	m.SetSubrepos(&cli.GitResult{Kind: protocol.GitQueryKind_Subrepos, Subrepos: []string{"a", "b", "c"}})
	m.SetContent(&cli.GitResult{Kind: protocol.GitQueryKind_Diff, Text: strings.Repeat("+x\n", 400), Truncated: true})
	if got := viewLines(m); got > h {
		t.Fatalf("rendered %d lines, terminal is %d", got, h)
	}
}

// Scrolling past the window has to bring the cursor's row with it, or the
// selection becomes invisible.
func TestGitModalWindowFollowsTheCursor(t *testing.T) {
	m := NewGitModal()
	m.Open("cafe1234")
	m.SetSize(120, 24)
	m.SetLog(&cli.GitResult{Kind: protocol.GitQueryKind_Log, Commits: manyCommits(100)})

	for i := 0; i < 60; i++ {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	start, end := m.listWindow()
	if m.SelectedIndex() < start || m.SelectedIndex() >= end {
		t.Fatalf("cursor %d outside the rendered window [%d,%d)", m.SelectedIndex(), start, end)
	}
	if !strings.Contains(m.View(), "> ") {
		t.Fatal("the cursor marker is not on screen")
	}
}

// Hidden rows are counted, not silently dropped.
func TestGitModalWindowReportsHiddenRows(t *testing.T) {
	m := NewGitModal()
	m.Open("cafe1234")
	m.SetSize(120, 24)
	m.SetLog(&cli.GitResult{Kind: protocol.GitQueryKind_Log, Commits: manyCommits(100)})
	for i := 0; i < 50; i++ {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	out := m.View()
	if !strings.Contains(out, "more") {
		t.Fatalf("102 rows in a 12-line picker and no count of what is hidden:\n%s", out)
	}
}

// A short list is rendered whole, with no marker to explain away.
func TestGitModalShortListHasNoMarkers(t *testing.T) {
	m := newTestGitModal()
	m.SetSize(120, 40)
	if strings.Contains(m.View(), "more") {
		t.Fatalf("four rows fit; nothing should be reported hidden:\n%s", m.View())
	}
}
