package tui

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/vtgrid"
)

// CredentialsModel is the fourth table (credential tasks,
// docs/superpowers/specs/2026-11-21-credential-tasks-design.md): the rows
// `credential ls` prints. They are TaskInfo rows of the snapshot the tasks
// table already holds (cli.CredentialRows, the filter `credential ls` uses),
// so the table follows every snapshot and task event without a list of its
// own. Keys: x revokes after a y/n confirm, r prefills `credential rekey`,
// N prefills `credential new`, d opens the task detail. Issuing and
// re-keying run on the command line: the one-time secret of a generated key
// is printed there, in the results pane, and nowhere else.
type CredentialsModel struct {
	table    table.Model
	baseCols []table.Column
	focused  bool
	// width decides the column SET (credColsFor); the row builder asks the
	// same question (showsScope) so a row always has one cell per column.
	width int
	// rows[i] is the TaskInfo for table row i, for the keys (which act on the
	// full 32-hex id) and the detail popup.
	rows []protocol.TaskInfo
}

// credScopeMinWidth is the PANEL width from which the Scope column joins.
// Below it Caps is the flex column and Scope is in the `d` popup: at 78 cells
// the six fixed columns leave Caps about seven cells, and a seventh column
// would take that away.
const credScopeMinWidth = 120

// credColsFor is the ONE place the column set is decided: NewCredentials,
// SetSize and rebuild all go through it (checklist 34).
func credColsFor(width int) []table.Column {
	cols := []table.Column{
		{Title: "ID", Width: 8},
		{Title: "Status", Width: 7},
		{Title: "Name", Width: 12},
		{Title: "Expires", Width: 16},   // 2006-01-02 15:04
		{Title: "Last used", Width: 16}, // or unknown
		{Title: "Caps", Width: 0},
	}
	if width >= credScopeMinWidth {
		cols = append(cols, table.Column{Title: "Scope", Width: 24})
	}
	return cols
}

func NewCredentials() CredentialsModel {
	cols := credColsFor(0)
	t := table.New(table.WithColumns(cols), table.WithFocused(false))
	return CredentialsModel{table: t, baseCols: cols}
}

func (m *CredentialsModel) Focus() {
	m.focused = true
	m.table.Focus()
}

func (m *CredentialsModel) Blur() {
	m.focused = false
	m.table.Blur()
}

func (m *CredentialsModel) IsFocused() bool { return m.focused }

func (m *CredentialsModel) showsScope() bool { return m.width >= credScopeMinWidth }

// SetSize fits the columns to w; a change of the column SET goes through
// swapColumnsAndRebuild, a width-only change keeps the rows.
func (m *CredentialsModel) SetSize(w, h int) {
	m.table.SetWidth(w)
	m.table.SetHeight(h)
	m.width = w
	m.baseCols = credColsFor(w)
	cols := fitColumns(m.baseCols, w, flexColumn(m.baseCols, "Caps"))
	if len(cols) == len(m.table.Columns()) {
		m.table.SetColumns(cols)
		return
	}
	m.swapColumnsAndRebuild(cols)
}

// swapColumnsAndRebuild is SchedulesModel's: rows emptied before the columns
// change and rebuilt after, the cursor walked back to where it was.
func (m *CredentialsModel) swapColumnsAndRebuild(cols []table.Column) {
	cursor := m.table.Cursor()
	m.table.SetRows(nil)
	m.table.SetColumns(cols)
	m.rebuild()
	if cursor > 0 && cursor < len(m.rows) {
		m.table.SetCursor(0)
		m.table.MoveDown(cursor)
	}
}

// SetRows takes the tasks of a snapshot (in the order the tasks table sorts
// them) and keeps the credentials.
func (m *CredentialsModel) SetRows(tasks []protocol.TaskInfo) {
	m.rows = cli.CredentialRows(tasks)
	m.rebuild()
}

// rebuild is the only place credential cells are produced. Nothing is elided
// for what it is (checklist 31): an unnamed credential reads `-`, a last use
// not seen since the server started reads `unknown`.
func (m *CredentialsModel) rebuild() {
	rows := make([]table.Row, 0, len(m.rows))
	for _, t := range m.rows {
		name := "-"
		if len(t.Name) > 0 {
			n := string(t.Name)
			name = vtgrid.SanitizeTitle(n, len(n))
		}
		row := table.Row{
			hex.EncodeToString(t.Id.Id[:4]),
			taskStatusStr(t.Status),
			name,
			cli.CredentialTimeCell(t.CredentialExpiresAt),
			cli.CredentialTimeCell(t.CredentialLastUsedAt),
			cli.CapsLabel(t.Capabilities),
		}
		if m.showsScope() {
			row = append(row, cli.ScopeLabel(t.Scope))
		}
		rows = append(rows, row)
	}
	setTableRows(&m.table, rows)
}

// SelectedTask is the credential under the cursor, nil when there is none.
func (m *CredentialsModel) SelectedTask() *protocol.TaskInfo {
	idx := m.table.Cursor()
	if idx < 0 || idx >= len(m.rows) {
		return nil
	}
	return &m.rows[idx]
}

// SelectedID is the 32-hex id under the cursor, "" when none.
func (m *CredentialsModel) SelectedID() string {
	if t := m.SelectedTask(); t != nil {
		return hex.EncodeToString(t.Id.Id[:])
	}
	return ""
}

func (m CredentialsModel) Update(msg tea.Msg) (CredentialsModel, tea.Cmd) {
	if !m.focused {
		return m, nil
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m CredentialsModel) View() string { return m.table.View() }

// --- keys and verbs ---

// credentialRecord runs one credential verb through the long-lived client,
// as every Do* here does (pitfall 3): DoRecord hands fn a.client. Its result
// lands in cmdresult as a RecordResultMsg, which also refreshes the snapshot
// the table is built from. credExec, when set, replaces the cli call (a test
// seam: the real one needs a server) and receives the same client.
func (a *App) credentialRecord(verbName string, act any, fn func(ctx context.Context, c *cli.Client, out io.Writer) error) tea.Cmd {
	if a.client == nil {
		a.cmdresult.Append(ErrorStyle.Render(verbName + ": not connected to server"))
		return nil
	}
	exec := a.credExec
	return DoRecord(a.client, verbName, func(ctx context.Context, c *cli.Client, out io.Writer) error {
		if exec != nil {
			return exec(ctx, c, act, out)
		}
		return fn(ctx, c, out)
	})
}

// credentialRevokeCmd is the revoke the x key and the typed verb share, so
// both say what `credential revoke` prints (checklist 29/30).
func (a *App) credentialRevokeCmd(id string) tea.Cmd {
	act := verb.CredentialRevokeAction{ID: id}
	return a.credentialRecord("credential revoke", act, func(ctx context.Context, c *cli.Client, out io.Writer) error {
		return cli.CredentialRevokeWith(ctx, c, id, out)
	})
}

// onCredentialRevoke only ASKS: it arms credRevokeID, and
// inCredRevokeConfirm (an overlay) takes the next key.
func (a *App) onCredentialRevoke(msg tea.KeyMsg) (tea.Cmd, bool) {
	id := a.credentials.SelectedID()
	if id == "" {
		a.cmdresult.Append(WarnStyle.Render("credential revoke: no credential selected"))
		return nil, true
	}
	a.credRevokeID = id
	a.cmdresult.Append(WarnStyle.Render(credRevokePrompt(id)))
	return nil, true
}

func credRevokePrompt(id string) string {
	return fmt.Sprintf("credential revoke %s: revoke it? y revokes (its connections close; credential rekey brings the id back), any other key keeps it", credShort(id))
}

// inCredRevokeConfirm owns the keys while a revoke awaits its answer: y
// revokes, anything else -- q included -- keeps the credential.
func (a *App) inCredRevokeConfirm(msg tea.KeyMsg) tea.Cmd {
	id := a.credRevokeID
	a.credRevokeID = ""
	if msg.String() == "y" || msg.String() == "Y" {
		return a.credentialRevokeCmd(id)
	}
	a.cmdresult.Append(fmt.Sprintf("credential revoke %s: cancelled; nothing revoked", credShort(id)))
	return nil
}

// prefillCmdline puts a verb on the command line for the operator to finish
// and moves focus there. Used where the rest of the line is a decision the
// key cannot make (a ttl, a public key).
func (a *App) prefillCmdline(line string) {
	a.setFocus(focusCmdline)
	a.cmdline.SetValue(line)
	a.cmdline.CursorEnd()
}

// onCredentialRekey is r in the credentials table: `credential rekey <id>
// --ttl ` on the command line, to be completed with a ttl and, optionally,
// --pubkey or --out.
func (a *App) onCredentialRekey(msg tea.KeyMsg) (tea.Cmd, bool) {
	id := a.credentials.SelectedID()
	if id == "" {
		a.cmdresult.Append(WarnStyle.Render("credential rekey: no credential selected"))
		return nil, true
	}
	a.prefillCmdline("credential rekey " + id + " --ttl ")
	return nil, true
}

// onCredentialDetail is d in the credentials table: the task detail popup,
// which carries the credential's name, expiry and last use.
func (a *App) onCredentialDetail() (tea.Cmd, bool) {
	t := a.credentials.SelectedTask()
	if t == nil {
		a.cmdresult.Append(WarnStyle.Render("no credential selected"))
		return nil, true
	}
	a.detail.Open("Credential detail", formatTaskDetail(*t, runnerIndex(a.runnersSnapshot)))
	return nil, true
}

// resolveCredentialPrefix turns a typed id prefix into the full id of a
// credential the table lists. A full id it does not list is passed through
// for the server (and cli's own reach check) to judge.
func (a *App) resolveCredentialPrefix(prefix string) (string, string) {
	full, errStr := a.resolveTaskIDPrefix(prefix)
	if errStr != "" {
		return "", errStr
	}
	if t, ok := a.tasksByID[full]; ok && t.Kind != protocol.TaskKind_Credential {
		return "", fmt.Sprintf("credential revoke: %s is not a credential", credShort(full))
	}
	return full, ""
}

// credShort is the id8 a credential is named by in results, as cli prints it
// (`credential <id8>: revoked`).
func credShort(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
