package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// gitRowKind distinguishes the two client-side pseudo rows from a real commit.
// The pseudo rows are not protocol: they name the two things a commit id
// cannot, the working tree and the index.
type gitRowKind int

const (
	gitRowWorktree gitRowKind = iota
	gitRowIndex
	gitRowCommit
	// gitRowSubrepo is a nested repository. Selecting one re-roots the whole
	// modal into it, so the picker doubles as the place nested repos are
	// discovered — no second pane, and no path to type from memory.
	gitRowSubrepo
)

type gitRow struct {
	Kind    gitRowKind
	Commit  cli.GitCommit // zero unless Kind is gitRowCommit
	Subrepo string        // relative path; set only for gitRowSubrepo
}

// gitDefaultBase is where the baseline starts. HEAD means "everything not yet
// committed", which is what an operator looking at a running agent wants
// first; pressing b on a commit moves it back from there.
const gitDefaultBase = "HEAD"

// GitModal is the two-pane git view: a row picker on top (the working tree,
// the index, then the commits) and a content viewport underneath.
//
// Key dispatch follows the BoardModal convention — the App intercepts the keys
// that need a *cli.Client (Enter, r, s) before calling Update, so this type
// never has to hold one. Update owns only selection, the baseline, and the
// viewport.
type GitModal struct {
	open      bool
	taskID    string
	rows      []gitRow
	cursor    int
	baseRev   string
	content   viewport.Model
	rawLines  []string // uncoloured content lines, for the n/N file jump
	truncated bool
	errMsg    string
	commits   []cli.GitCommit
	statusSum string // "3 changed, 1 untracked", rendered on the worktree row
	logNote   string // set when the commit list itself was truncated

	// subrepoStack is the chain of nested repositories entered so far, each
	// element relative to the one before it. A LEVEL is one entry, not one
	// path segment: the runner reports "pkg/inner" as a single nested repo
	// two directories deep, and going up from it lands at the worktree, not
	// at "pkg" — where there is no repository at all.
	subrepoStack []string
	subrepos     []string // offered as [REPO] rows at the bottom of the picker
	submodule    bool     // --submodule for the diff and show queries

	// height / listCap are the size budget View() renders within; see SetSize.
	height  int
	listCap int

	// fileView names the file whose whole content is in the viewport, empty
	// when the viewport holds a diff. lastContent is the query that produced
	// the diff, kept so opening a file can ask for THE SAME SIDE and so
	// leaving the file view can put the diff back.
	fileView    string
	lastContent cli.GitQuery
}

func NewGitModal() GitModal {
	vp := viewport.New(80, 10)
	vp.SetContent("(select a row and press Enter)")
	return GitModal{baseRev: gitDefaultBase, content: vp}
}

func (m GitModal) IsOpen() bool    { return m.open }
func (m GitModal) TaskID() string  { return m.taskID }
func (m GitModal) BaseRev() string { return m.baseRev }

// SetBaseRev seeds the baseline from outside the modal — the cmdline route,
// where the operator named a revision before the modal existed. Inside the
// modal the baseline moves via the set-base key instead.
func (m *GitModal) SetBaseRev(rev string) {
	if rev != "" {
		m.baseRev = rev
	}
}

// SetRoot seeds the root and the submodule setting from outside the modal —
// the cmdline route, which names them before the modal exists. Inside the
// modal they move via Enter on a [REPO] row and the submodule key.
func (m *GitModal) SetRoot(subrepo string, submodule bool) {
	m.subrepoStack = nil
	if subrepo != "" {
		// One entry, not one per path segment: a path typed on the command
		// line names one repository, so up from it is the worktree.
		m.subrepoStack = []string{subrepo}
	}
	m.submodule = submodule
}

// Open resets everything except the size: a modal opened on a different task
// must not inherit the previous task's baseline or content.
func (m *GitModal) Open(taskID string) {
	m.open = true
	m.taskID = taskID
	m.rows = defaultGitRows()
	m.cursor = 0
	m.baseRev = gitDefaultBase
	m.rawLines = nil
	m.truncated = false
	m.errMsg = ""
	m.statusSum = ""
	m.logNote = ""
	m.subrepoStack = nil
	m.subrepos = nil
	m.submodule = false
	m.fileView = ""
	m.lastContent = cli.GitQuery{}
	m.content.SetContent("(loading)")
	m.content.GotoTop()
}

func (m *GitModal) Close() {
	m.open = false
	m.taskID = ""
}

// gitListCap decides how many picker lines the modal renders, from the
// terminal height alone — NOT from how many rows exist. The row count must not
// enter this: a hundred commits would otherwise reserve a hundred lines, and
// the modal grew taller than the terminal and spilled off the top.
func gitListCap(h int) int {
	cap := h / 3
	if cap < 3 {
		cap = 3
	}
	if cap > 12 {
		cap = 12
	}
	return cap
}

// SetSize splits the height between the picker and the content so that
// View() can never exceed h. The budget is:
//
//	2 border + 1 header + listCap + 1 logNote + content + 1 truncation + 1 footer
//
// so content gets h - listCap - 6. View() renders exactly that many picker
// lines, windowing around the cursor when there are more rows than fit.
func (m *GitModal) SetSize(w, h int) {
	m.height = h
	m.listCap = gitListCap(h)
	ch := h - m.listCap - 6
	if ch < 3 {
		ch = 3
	}
	m.content.Width = w - 4
	m.content.Height = ch
}

// listWindow returns the half-open range of rows View() renders, chosen so the
// cursor is always inside it.
func (m GitModal) listWindow() (start, end int) {
	capRows := m.listCap
	if capRows <= 0 {
		capRows = gitListCap(m.height)
	}
	if len(m.rows) <= capRows {
		return 0, len(m.rows)
	}
	start = m.cursor - capRows/2
	if start < 0 {
		start = 0
	}
	if start+capRows > len(m.rows) {
		start = len(m.rows) - capRows
	}
	return start, start + capRows
}

// defaultGitRows is the two pseudo rows, which exist whether or not a log has
// arrived — the uncommitted state is readable before any commit exists.
func defaultGitRows() []gitRow {
	return []gitRow{{Kind: gitRowWorktree}, {Kind: gitRowIndex}}
}

// SetLog rebuilds the row list from a log result, keeping the two pseudo rows
// on top so the uncommitted state stays the default selection.
func (m *GitModal) SetLog(res *cli.GitResult) {
	m.errMsg = ""
	m.commits = make([]cli.GitCommit, len(res.Commits))
	copy(m.commits, res.Commits)
	m.rebuildRows()
	m.logNote = ""
	if res.Truncated {
		m.logNote = fmt.Sprintf("commit list truncated at %d", len(res.Commits))
	}
}

// SetSubrepos records the nested repositories offered at the bottom of the
// picker. Kept separate from SetLog because the two answers arrive
// independently and either may be refreshed alone.
func (m *GitModal) SetSubrepos(res *cli.GitResult) {
	m.errMsg = ""
	m.subrepos = make([]string, len(res.Subrepos))
	copy(m.subrepos, res.Subrepos)
	m.rebuildRows()
}

// rebuildRows lays the picker out: the two pseudo rows, the commits, then the
// nested repositories. The order is deliberate — the uncommitted state stays
// the default selection, and navigation sits below the content rather than
// above it.
func (m *GitModal) rebuildRows() {
	m.rows = defaultGitRows()
	for _, c := range m.commits {
		m.rows = append(m.rows, gitRow{Kind: gitRowCommit, Commit: c})
	}
	for _, sr := range m.subrepos {
		m.rows = append(m.rows, gitRow{Kind: gitRowSubrepo, Subrepo: sr})
	}
	if m.cursor >= len(m.rows) {
		m.cursor = 0
	}
}

// Subrepo is the nested repository the modal is rooted in, relative to the
// task's worktree. Empty means the worktree itself.
func (m GitModal) Subrepo() string { return strings.Join(m.subrepoStack, "/") }

// SubmoduleDiff reports whether diff and show should inline a submodule's own
// changes.
func (m GitModal) SubmoduleDiff() bool { return m.submodule }

// Query is the GitQuery every caller should start from: it carries the modal's
// current root and submodule setting, so no caller has to remember to thread
// them and none can disagree about them.
func (m GitModal) Query() cli.GitQuery {
	return cli.GitQuery{Subrepo: m.Subrepo(), SubmoduleDiff: m.submodule}
}

// EnterSubrepo re-roots the modal into rel (relative to the CURRENT root, which
// is how the runner reports it) and clears everything that belonged to the old
// repository — its commits, its baseline, its content.
func (m *GitModal) EnterSubrepo(rel string) {
	if rel == "" {
		return
	}
	m.subrepoStack = append(m.subrepoStack, rel)
	m.resetForNewRoot()
}

// LeaveSubrepo goes back up one level, to the parent repository. Reports false
// when already at the task's worktree.
func (m *GitModal) LeaveSubrepo() bool {
	if len(m.subrepoStack) == 0 {
		return false
	}
	m.subrepoStack = m.subrepoStack[:len(m.subrepoStack)-1]
	m.resetForNewRoot()
	return true
}

// ToggleSubmodule flips --submodule and returns the new setting.
func (m *GitModal) ToggleSubmodule() bool {
	m.submodule = !m.submodule
	return m.submodule
}

func (m *GitModal) resetForNewRoot() {
	m.fileView = ""
	m.lastContent = cli.GitQuery{}
	m.commits = nil
	m.subrepos = nil
	m.baseRev = gitDefaultBase
	m.statusSum = ""
	m.logNote = ""
	m.errMsg = ""
	m.rawLines = nil
	m.truncated = false
	m.cursor = 0
	m.rebuildRows()
	m.content.SetContent("(loading)")
	m.content.GotoTop()
}

// SetStatus records the summary shown on the worktree row. It does not touch
// the content pane: status is context for the picker, not the thing being read.
func (m *GitModal) SetStatus(res *cli.GitResult) {
	changed, untracked := 0, 0
	for _, e := range res.Entries {
		if e.XY == "??" {
			untracked++
			continue
		}
		changed++
	}
	switch {
	case changed == 0 && untracked == 0:
		m.statusSum = "clean"
	case untracked == 0:
		m.statusSum = fmt.Sprintf("%d changed", changed)
	default:
		m.statusSum = fmt.Sprintf("%d changed, %d untracked", changed, untracked)
	}
}

// SetStatusContent renders a status listing INTO the content pane. Separate
// from SetStatus because pressing s asks to read the listing, while a
// background refresh only wants the summary.
func (m *GitModal) SetStatusContent(res *cli.GitResult) {
	m.SetStatus(res)
	m.errMsg = ""
	m.truncated = false
	if len(res.Entries) == 0 {
		m.setLines([]string{"(nothing uncommitted)"}, false)
		return
	}
	lines := make([]string, 0, len(res.Entries))
	for _, e := range res.Entries {
		lines = append(lines, fmt.Sprintf("%s %s", e.XY, e.Path))
	}
	m.setLines(lines, false)
}

// RecordContentQuery remembers which query the next diff/show content came
// from, so OpenFileQuery can ask for the same side and LeaveFileView can put
// the diff back.
func (m *GitModal) RecordContentQuery(q cli.GitQuery) { m.lastContent = q }

// SetContent puts diff or show text into the viewport, coloured by line class.
func (m *GitModal) SetContent(res *cli.GitResult) {
	m.errMsg = ""
	m.fileView = ""
	text := strings.TrimSuffix(res.Text, "\n")
	if text == "" {
		m.setLines([]string{"(no difference)"}, res.Truncated)
		return
	}
	m.setLines(strings.Split(text, "\n"), res.Truncated)
}

// SetError replaces the content with git's own words. A failed query leaves
// the modal standing: the operator picked the wrong baseline far more often
// than the view broke, and the fix is one keypress away.
func (m *GitModal) SetError(msg string) {
	m.errMsg = msg
	m.rawLines = nil
	m.truncated = false
	m.content.SetContent(ErrorStyle.Render(msg))
	m.content.GotoTop()
}

func (m *GitModal) setLines(lines []string, truncated bool) {
	m.rawLines = lines
	m.truncated = truncated
	var b strings.Builder
	for i, line := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(styleGitLine(line))
	}
	m.content.SetContent(b.String())
	m.content.GotoTop()
}

func styleGitLine(line string) string {
	switch cli.ClassifyGitLine(line) {
	case cli.GitLineAdd:
		return GitAddStyle.Render(line)
	case cli.GitLineDel:
		return GitDelStyle.Render(line)
	case cli.GitLineHunk:
		return GitHunkStyle.Render(line)
	case cli.GitLineFile:
		return GitFileStyle.Render(line)
	case cli.GitLineMeta:
		return GitMetaStyle.Render(line)
	default:
		return line
	}
}

func (m GitModal) RowCount() int      { return len(m.rows) }
func (m GitModal) SelectedIndex() int { return m.cursor }

func (m GitModal) SelectedRow() gitRow {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return gitRow{}
	}
	return m.rows[m.cursor]
}

func (m GitModal) ContentOffset() int { return m.content.YOffset }

// Update owns selection, the baseline and viewport scrolling. Enter, r and s
// are handled by the App, which holds the client.
func (m GitModal) Update(msg tea.Msg) (GitModal, tea.Cmd) {
	if !m.open {
		return m, nil
	}
	if k, ok := msg.(tea.KeyMsg); ok {
		switch k.Type {
		case tea.KeyUp:
			if m.cursor > 0 {
				m.cursor--
			}
			return m, nil
		case tea.KeyDown:
			if m.cursor < len(m.rows)-1 {
				m.cursor++
			}
			return m, nil
		}
		switch k.String() {
		case modalKeys.GitSetBase:
			// Only a commit is a commit-ish. The pseudo rows name states, not
			// objects, so there is nothing to set the baseline to.
			if row := m.SelectedRow(); row.Kind == gitRowCommit {
				m.baseRev = row.Commit.SHA
			}
			return m, nil
		case modalKeys.GitNextFile:
			m.jumpFile(1)
			return m, nil
		case modalKeys.GitPrevFile:
			m.jumpFile(-1)
			return m, nil
		}
		// GitSubmodule and GitUp change what the NEXT query asks for, so the
		// App handles them: it holds the client that has to re-issue it.
	}
	var cmd tea.Cmd
	m.content, cmd = m.content.Update(msg)
	return m, cmd
}

// jumpFile moves the viewport to the next or previous "diff --git" header, so
// a diff spanning many files stays navigable without a second pane.
func (m *GitModal) jumpFile(dir int) {
	if len(m.rawLines) == 0 {
		return
	}
	i := m.content.YOffset + dir
	for i >= 0 && i < len(m.rawLines) {
		if cli.ClassifyGitLine(m.rawLines[i]) == cli.GitLineFile {
			m.content.YOffset = i
			return
		}
		i += dir
	}
	// No further header: park at the end the operator was heading toward,
	// rather than leaving the keypress silently inert.
	if dir > 0 {
		m.content.GotoBottom()
	} else {
		m.content.GotoTop()
	}
}

func (m GitModal) View() string {
	box := PanelStyleFocused.Padding(0, 1)

	taskShort := m.taskID
	if len(taskShort) > 8 {
		taskShort = taskShort[:8]
	}
	baseShort := m.baseRev
	if len(baseShort) > 12 {
		baseShort = baseShort[:12]
	}
	repoLabel := m.Subrepo()
	if repoLabel == "" {
		repoLabel = "(root)"
	}
	submoduleNote := ""
	if m.submodule {
		submoduleNote = "  +submodule"
	}
	headerText := fmt.Sprintf("git — task %s   base: %s   repo: %s%s",
		taskShort, baseShort, repoLabel, submoduleNote)
	if m.fileView != "" {
		headerText = fmt.Sprintf("git — task %s   repo: %s   file: %s",
			taskShort, repoLabel, m.fileView)
	}
	header := HeaderStyle.Render(headerText)

	start, end := m.listWindow()
	lines := make([]string, 0, end-start+1)
	for i := start; i < end; i++ {
		row := m.rows[i]
		cursor := "  "
		if i == m.cursor {
			cursor = "> "
		}
		switch row.Kind {
		case gitRowWorktree:
			summary := m.statusSum
			if summary == "" {
				summary = "uncommitted"
			}
			lines = append(lines, fmt.Sprintf("%s[WORKTREE]  %s", cursor, summary))
		case gitRowIndex:
			lines = append(lines, fmt.Sprintf("%s[INDEX]     staged", cursor))
		case gitRowSubrepo:
			lines = append(lines, fmt.Sprintf("%s%s  %s", cursor,
				MutedStyle.Render("[REPO]"), row.Subrepo))
		default:
			c := row.Commit
			lines = append(lines, fmt.Sprintf("%s%-8s  %s  %-10s  %s",
				cursor, c.Short(), c.When.Format("01-02 15:04"), truncateRunes(c.Author, 10), c.Subject))
		}
	}
	// The markers replace the edge line rather than adding one, so the picker
	// occupies exactly listCap lines however many rows there are.
	if start > 0 && len(lines) > 0 {
		lines[0] = MutedStyle.Render(fmt.Sprintf("  ↑ %d more", start))
	}
	if end < len(m.rows) && len(lines) > 0 {
		lines[len(lines)-1] = MutedStyle.Render(fmt.Sprintf("  ↓ %d more", len(m.rows)-end))
	}
	if m.logNote != "" {
		lines = append(lines, MutedStyle.Render("  "+m.logNote))
	}
	list := strings.Join(lines, "\n")

	notes := ""
	if m.truncated {
		notes = "  " + WarnStyle.Render("truncated")
	}
	footerText := "↑/↓ select · Enter: show/enter · " +
		modalKeys.GitSetBase + ": base · " +
		modalKeys.GitStatus + ": status · " +
		modalKeys.GitSubmodule + ": submodule · " +
		modalKeys.GitUp + ": up · " +
		modalKeys.GitNextFile + "/" + modalKeys.GitPrevFile + ": jump · " +
		modalKeys.GitOpenFile + ": whole file · r: refresh · Esc: close"
	if m.fileView != "" {
		footerText = "PgUp/PgDn scroll · " + modalKeys.GitOpenFile + ": back to the diff · Esc: close"
	}
	footer := FooterStyle.Render(footerText)

	return box.Render(header + "\n" + list + "\n" + m.content.View() + notes + "\n" + footer)
}

// truncateRunes clips s to at most n runes. Used only for the author column,
// where a long name would push the subject off the row.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// GitQueryForRow says what a row means as a query: the pseudo rows are diffs
// against the current baseline, a commit is a show of that commit. Exported
// shape kept small so the App does not re-derive the mapping.
// A [REPO] row is not content — Enter on one re-roots instead of querying — so
// the App checks the row kind before calling this.
func (m GitModal) GitQueryForRow(row gitRow) (kind protocol.GitQueryKind, target protocol.GitDiffTarget, rev string) {
	switch row.Kind {
	case gitRowWorktree:
		return protocol.GitQueryKind_Diff, protocol.GitDiffTarget_Worktree, m.baseRev
	case gitRowIndex:
		return protocol.GitQueryKind_Diff, protocol.GitDiffTarget_Index, m.baseRev
	default:
		return protocol.GitQueryKind_Show, protocol.GitDiffTarget_Worktree, row.Commit.SHA
	}
}

// FileView names the file whose whole content is showing, or "" for a diff.
func (m GitModal) FileView() string { return m.fileView }

// SetFileContent puts a whole file into the viewport. The path is remembered so
// the header can say which file is showing and the footer can offer the way
// back.
func (m *GitModal) SetFileContent(path string, res *cli.GitResult) {
	m.errMsg = ""
	text := strings.TrimSuffix(res.Text, "\n")
	if text == "" {
		m.setLines([]string{"(empty file)"}, res.Truncated)
	} else {
		m.setLines(strings.Split(text, "\n"), res.Truncated)
	}
	m.fileView = path
}

// OpenFileQuery builds the query for the file the viewport is currently
// scrolled to, or reports ok=false when the content is not a diff or the line
// belongs to no file (a deleted one, or a commit header).
//
// The side is taken from the query that produced the diff, NOT defaulted:
// showing the working tree for a diff of two commits would answer a different
// question than the one on screen.
func (m GitModal) OpenFileQuery() (cli.GitQuery, bool) {
	if m.fileView != "" || len(m.rawLines) == 0 {
		return cli.GitQuery{}, false
	}
	path := cli.DiffFilePathAt(m.rawLines, m.content.YOffset)
	if path == "" {
		return cli.GitQuery{}, false
	}
	q := m.Query()
	q.Path = path
	switch {
	case m.lastContent.Kind == protocol.GitQueryKind_Show:
		// The commit being shown IS the side on screen.
		q.Target = protocol.GitDiffTarget_Rev
		q.TargetRev = m.lastContent.BaseRev
	case m.lastContent.Target == protocol.GitDiffTarget_Rev:
		q.Target = protocol.GitDiffTarget_Rev
		q.TargetRev = m.lastContent.TargetRev
	case m.lastContent.Target == protocol.GitDiffTarget_Index:
		q.Target = protocol.GitDiffTarget_Index
	default:
		q.Target = protocol.GitDiffTarget_Worktree
	}
	return q, true
}

// LeaveFileView reports whether a file was showing; the App re-issues the
// diff query it recorded.
func (m *GitModal) LeaveFileView() bool {
	if m.fileView == "" {
		return false
	}
	m.fileView = ""
	return true
}

// LastContentQuery is the diff query to re-issue when leaving the file view.
func (m GitModal) LastContentQuery() cli.GitQuery { return m.lastContent }
