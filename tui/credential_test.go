package tui

import (
	"context"
	"encoding/hex"
	"io"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// credFixTask is a credential task the way the server lists it.
func credFixTask(b byte, name string, status protocol.TaskStatus) protocol.TaskInfo {
	var t protocol.TaskInfo
	for i := range t.Id.Id {
		t.Id.Id[i] = b
	}
	t.Kind = protocol.TaskKind_Credential
	t.Status = status
	t.CreatedAt = uint64(time.Date(2026, 11, 22, 0, 0, int(b), 0, time.UTC).UnixNano())
	t.StartedAt = t.CreatedAt // the server stamps it at issue
	t.Run = 1
	t.CredentialExpiresAt = uint64(time.Date(2026, 12, 22, 6, 40, 0, 0, time.UTC).UnixNano())
	t.Capabilities = protocol.Capability_ScheduleRun
	t.Scope = protocol.TaskScope{Base: protocol.ScopeBase_Global}
	if name != "" {
		t.SetName([]byte(name))
	}
	return t
}

func credHex(b byte) string {
	t := credFixTask(b, "", protocol.TaskStatus_Active)
	return hex.EncodeToString(t.Id.Id[:])
}

// credApp is an App whose snapshot holds two credentials and one ordinary
// task, with the credentials table focused.
func credApp(w, h int) *App {
	a := oneTableApp(w, h)
	used := credFixTask(0xc1, "nightly", protocol.TaskStatus_Active)
	used.CredentialLastUsedAt = uint64(time.Date(2026, 11, 22, 7, 0, 0, 0, time.UTC).UnixNano())
	var plain protocol.TaskInfo
	plain.Id.Id[0] = 0x0a
	plain.Kind = protocol.TaskKind_Oneshot
	plain.Status = protocol.TaskStatus_Running
	a.Update(SnapshotMsg{Tasks: []protocol.TaskInfo{
		used,
		credFixTask(0xc2, "", protocol.TaskStatus_Cancelled),
		plain,
	}})
	a.setFocus(focusCredentials)
	return a
}

type credCall struct {
	act any
	c   *cli.Client
}

// credSeam records every credential verb the App runs and the client it was
// handed, which must be the long-lived a.client (pitfall 3).
func credSeam(a *App) *[]credCall {
	var calls []credCall
	a.client = &cli.Client{}
	a.credExec = func(_ context.Context, c *cli.Client, act any, out io.Writer) error {
		calls = append(calls, credCall{act: act, c: c})
		io.WriteString(out, "done")
		return nil
	}
	return &calls
}

func cmdresultText(a *App) string { return strings.Join(a.cmdresult.lines, "\n") }

func typeCmdline(a *App, line string) tea.Cmd {
	a.setFocus(focusCmdline)
	a.cmdline.SetValue(line)
	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	return cmd
}

// --- display ---

func TestCredentialDetailShowsKindExpiryAndLastUse(t *testing.T) {
	tk := credFixTask(0xc1, "nightly", protocol.TaskStatus_Active)
	d := formatTaskDetail(tk, nil)
	for _, want := range []string{
		"kind:          credential\n",
		"status:        Active\n",
		"name:          nightly\n",
		"expires:       " + cli.CredentialExpiryLabel(&tk) + "\n",
		"last used:     unknown\n", // zero is a reading: not seen since the server started
	} {
		if !strings.Contains(d, want) {
			t.Errorf("detail lacks %q:\n%s", want, d)
		}
	}
	// No process, so none of the lines about one (the subject is absent, not zero).
	for _, absent := range []string{"skills:", "run number:", "repo:", "execs:", "assigned to:", "lifecycle:"} {
		if strings.Contains(d, absent) {
			t.Errorf("a credential's detail carries %q:\n%s", absent, d)
		}
	}
	// An ordinary task has no credential lines.
	var plain protocol.TaskInfo
	plain.Kind = protocol.TaskKind_Oneshot
	if d := formatTaskDetail(plain, nil); strings.Contains(d, "expires:") || strings.Contains(d, "last used:") {
		t.Errorf("an ordinary task's detail carries credential lines:\n%s", d)
	}
}

func TestTasksTableShowsCredentialActiveAndAbout(t *testing.T) {
	a := credApp(200, 50)
	a.setFocus(focusTasks)
	var row []string
	for i, id := range a.tasks.rowIDs {
		if id == credHex(0xc1) {
			row = a.tasks.table.Rows()[i]
		}
	}
	if row == nil {
		t.Fatalf("the credential is not in the tasks table: %v", a.tasks.rowIDs)
	}
	if row[0] != "Active" {
		t.Errorf("status cell = %q, want Active", row[0])
	}
	about := row[len(row)-1]
	tk := credFixTask(0xc1, "nightly", protocol.TaskStatus_Active)
	if want := "credential nightly expires " + cli.CredentialExpiryLabel(&tk); about != want {
		t.Errorf("About = %q, want %q", about, want)
	}
}

// --- the table ---

func credColTitles(m *CredentialsModel) []string {
	var out []string
	for _, c := range m.table.Columns() {
		out = append(out, c.Title)
	}
	return out
}

func TestCredentialsTableListsOnlyCredentials(t *testing.T) {
	a := credApp(200, 50)
	m := &a.credentials
	if len(m.rows) != 2 || len(m.table.Rows()) != 2 {
		t.Fatalf("%d rows / %d table rows, want the two credentials", len(m.rows), len(m.table.Rows()))
	}
	col := map[string]int{}
	for i, c := range m.table.Columns() {
		col[c.Title] = i
	}
	for _, want := range []string{"ID", "Status", "Name", "Expires", "Last used", "Caps", "Scope"} {
		if _, ok := col[want]; !ok {
			t.Fatalf("no %s column at 200 wide: %v", want, credColTitles(m))
		}
	}
	byID := map[string][]string{}
	for i, r := range m.rows {
		byID[hex.EncodeToString(r.Id.Id[:])] = m.table.Rows()[i]
	}
	live, ended := byID[credHex(0xc1)], byID[credHex(0xc2)]
	if live == nil || ended == nil {
		t.Fatalf("rows by id: %v", byID)
	}
	if live[col["Name"]] != "nightly" || live[col["Status"]] != "Active" || live[col["Last used"]] == "unknown" {
		t.Errorf("live row = %q", live)
	}
	// Nothing hidden because of what it is (checklist 31): an unnamed one says
	// so, a never-seen one reads unknown, an ended one says Cancel.
	if ended[col["Name"]] != "-" || ended[col["Last used"]] != "unknown" || ended[col["Status"]] != "Cancel" {
		t.Errorf("ended row = %q", ended)
	}
	if live[col["Caps"]] != "schedule_run" || live[col["Scope"]] != "global" {
		t.Errorf("authority cells = %q / %q", live[col["Caps"]], live[col["Scope"]])
	}
}

// Checklist 34: the column SET depends on the width, and the rows are rebuilt
// with it through SetRows' decision, in both directions.
func TestCredentialsColumnsAndRowsAgreeAcrossWidths(t *testing.T) {
	eastAsian(t)
	a := credApp(200, 50)
	for _, w := range []int{80, credScopeMinWidth - 1, credScopeMinWidth, 200, 90, credScopeMinWidth + 3, 80} {
		a.width = w
		a.layout()
		m := &a.credentials
		hasScope := false
		for _, c := range m.table.Columns() {
			hasScope = hasScope || c.Title == "Scope"
		}
		if hasScope != (w-2 >= credScopeMinWidth) {
			t.Errorf("w=%d: Scope column present=%v", w, hasScope)
		}
		for i, r := range m.table.Rows() {
			if len(r) != len(m.table.Columns()) {
				t.Fatalf("w=%d row %d has %d cells, %d columns (%v)", w, i, len(r), len(m.table.Columns()), credColTitles(m))
			}
		}
		if got := tableRenderWidth(m.table.Columns()); got > w-2 {
			t.Errorf("w=%d: columns render %d wide in a %d panel", w, got, w-2)
		}
		v := a.View()
		for i, l := range strings.Split(v, "\n") {
			if n := lipgloss.Width(l); n > w {
				t.Errorf("w=%d: line %d is %d wide", w, i, n)
			}
		}
	}
}

// --- keys ---

func TestCredentialRevokeKeyConfirms(t *testing.T) {
	a := credApp(120, 40)
	calls := credSeam(a)
	id := a.credentials.SelectedID()
	if id == "" {
		t.Fatal("no credential selected")
	}
	if cmd := pressRune(a, 'x'); cmd != nil {
		t.Fatal("x must only ask")
	}
	if a.credRevokeID != id || !strings.Contains(cmdresultText(a), "credential revoke "+id[:8]) {
		t.Fatalf("x did not ask about %s: %q", id[:8], cmdresultText(a))
	}
	if v := a.View(); !strings.Contains(v, "revoke it?") {
		t.Errorf("the question is not on screen:\n%s", v)
	}
	// n (or anything but y) keeps it.
	if cmd := pressRune(a, 'n'); cmd != nil || len(*calls) != 0 {
		t.Fatalf("n revoked: %v", *calls)
	}
	if a.credRevokeID != "" || !strings.Contains(cmdresultText(a), "nothing revoked") {
		t.Fatalf("n did not report keeping it: %q", cmdresultText(a))
	}
	// x then y revokes the selected row, through the long-lived client.
	pressRune(a, 'x')
	runCmd(a, pressRune(a, 'y'))
	if len(*calls) != 1 {
		t.Fatalf("y ran %d calls", len(*calls))
	}
	got, ok := (*calls)[0].act.(verb.CredentialRevokeAction)
	if !ok || got.ID != id {
		t.Fatalf("revoke call = %#v, want CredentialRevokeAction{ID: %s}", (*calls)[0].act, id)
	}
	if (*calls)[0].c != a.client {
		t.Errorf("revoke went through %p, not the long-lived client %p", (*calls)[0].c, a.client)
	}
}

func TestCredentialRekeyKeyPrefillsTheCommandLine(t *testing.T) {
	a := credApp(120, 40)
	id := a.credentials.SelectedID()
	pressRune(a, 'r')
	if a.focus != focusCmdline {
		t.Fatalf("r left focus on %d, want the command line", a.focus)
	}
	if want := "credential rekey " + id + " --ttl "; a.cmdline.Value() != want {
		t.Fatalf("cmdline = %q, want %q", a.cmdline.Value(), want)
	}
}

func TestCredentialNewKeyPrefillsTheCommandLine(t *testing.T) {
	a := credApp(120, 40)
	pressRune(a, 'N')
	if a.focus != focusCmdline || a.cmdline.Value() != "credential new --ttl " {
		t.Fatalf("N: focus %d, cmdline %q", a.focus, a.cmdline.Value())
	}
}

func TestCredentialDetailKey(t *testing.T) {
	a := credApp(120, 40)
	pressRune(a, 'd')
	if !a.detail.IsOpen() || !strings.Contains(a.detail.View(), "credential") {
		t.Fatalf("d did not open the credential's detail")
	}
}

func TestArrowsReachTheCredentialsTable(t *testing.T) {
	a := oneTableApp(120, 40)
	a.setFocus(focusSchedules)
	a.Update(tea.KeyMsg{Type: tea.KeyRight})
	if a.focus != focusCredentials {
		t.Fatalf("right from schedules = %d, want credentials", a.focus)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyRight})
	if a.focus != focusRunners {
		t.Fatalf("right from credentials = %d, want runners (wraps)", a.focus)
	}
}

// --- command line ---

func TestCredentialRevokeCmdlineUsesTheLongLivedClient(t *testing.T) {
	a := credApp(120, 40)
	calls := credSeam(a)
	id := credHex(0xc1)
	runCmd(a, typeCmdline(a, "credential revoke "+id[:10]))
	if len(*calls) != 1 {
		t.Fatalf("%d calls; results: %q", len(*calls), cmdresultText(a))
	}
	got, ok := (*calls)[0].act.(verb.CredentialRevokeAction)
	if !ok || got.ID != id {
		t.Fatalf("call = %#v, want the prefix resolved to %s", (*calls)[0].act, id)
	}
	if (*calls)[0].c != a.client {
		t.Errorf("cmdline revoke went through %p, not a.client %p", (*calls)[0].c, a.client)
	}
}

func TestCredentialIssueCmdlineReachesTheVerb(t *testing.T) {
	a := credApp(120, 40)
	calls := credSeam(a)
	pk := strings.Repeat("0a", 32)
	runCmd(a, typeCmdline(a, "credential new --ttl 7d --name ci --caps schedule_run --pubkey "+pk))
	runCmd(a, typeCmdline(a, "credential rekey "+credHex(0xc1)+" --ttl 1d"))
	runCmd(a, typeCmdline(a, "credential ls"))
	if len(*calls) != 3 {
		t.Fatalf("%d calls; results: %q", len(*calls), cmdresultText(a))
	}
	issue, ok := (*calls)[0].act.(verb.CredentialIssueAction)
	if !ok || issue.Sub != "new" || issue.TTL != "7d" || issue.Name != "ci" || issue.Pubkey != pk || !issue.CapsPresent {
		t.Errorf("new = %#v", (*calls)[0].act)
	}
	rk, ok := (*calls)[1].act.(verb.CredentialIssueAction)
	if !ok || rk.Sub != verb.SubRekey || rk.ID != credHex(0xc1) {
		t.Errorf("rekey = %#v", (*calls)[1].act)
	}
	if _, ok := (*calls)[2].act.(verb.CredentialLsAction); !ok {
		t.Errorf("ls = %#v", (*calls)[2].act)
	}
	for i, c := range *calls {
		if c.c != a.client {
			t.Errorf("call %d did not use a.client", i)
		}
	}
}
