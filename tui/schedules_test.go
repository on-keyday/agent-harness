package tui

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// schedHex is a schedule id's 32 hex characters.
func schedHex(b byte) string {
	id := schedFixID(b)
	return hex.EncodeToString(id.Id[:])
}

func schedFixID(b byte) protocol.ScheduleID {
	var id protocol.ScheduleID
	for i := range id.Id {
		id.Id[i] = b
	}
	return id
}

// schedFixRow is a schedule row the way the server answers `schedule ls`.
func schedFixRow(b byte, state protocol.ScheduleTaskState) protocol.ScheduleRow {
	r := protocol.ScheduleRow{
		Id:        schedFixID(b),
		TaskState: state,
		NextAt:    uint64(time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC).UnixNano()),
	}
	r.SetCron([]byte("0 9 * * 1-5"))
	r.SetTz([]byte("Asia/Tokyo"))
	r.SetAgent([]byte("claude"))
	r.SetRepo([]byte("/home/someone/workspace/an-org-security/sisakulint-long-name"))
	r.SetPromptHead([]byte("triage new issues"))
	return r
}

func schedFixRows() []protocol.ScheduleRow {
	pending := schedFixRow(0xa1, protocol.ScheduleTaskState_PendingCreate)

	bound := schedFixRow(0xb2, protocol.ScheduleTaskState_Bound)
	bound.TaskId.Id[0], bound.TaskId.Id[1], bound.TaskId.Id[2], bound.TaskId.Id[3] = 0xc3, 0xc3, 0xc3, 0xc3
	bound.TaskStatus = protocol.TaskStatus_Running
	bound.SetLastPresent(true)
	bound.SetLast(protocol.ScheduleHistoryEntry{Outcome: protocol.ScheduleOutcome_Fired,
		DueAt: uint64(time.Now().Add(-3 * time.Minute).UnixNano()), At: uint64(time.Now().Add(-3 * time.Minute).UnixNano())})

	none := schedFixRow(0xd4, protocol.ScheduleTaskState_Unbound)
	none.NextAt = 0
	none.SetPromptHidden(true)
	return []protocol.ScheduleRow{pending, bound, none}
}

func schedApp(w, h int) *App {
	a := oneTableApp(w, h)
	a.Update(ScheduleListMsg{Rows: schedFixRows()})
	return a
}

func schedColTitles(m *SchedulesModel) []string {
	var out []string
	for _, c := range m.table.Columns() {
		out = append(out, c.Title)
	}
	return out
}

// --- focus cycle and strip ---

// Tab visits the shown table, notify, cmdresult and cmdline: the three tables
// are ONE stop, and ←/→ switch between them while a table has focus.
func TestTabTreatsTheTablesAsOneStop(t *testing.T) {
	a := oneTableApp(120, 40)
	a.setFocus(focusRunners)
	want := []focus{focusNotify, focusCmdresult, focusCmdline, focusRunners}
	var got []focus
	for range want {
		a.Update(tea.KeyMsg{Type: tea.KeyTab})
		got = append(got, a.focus)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tab cycle = %v, want %v", got, want)
	}
	a.setFocus(focusSchedules)
	a.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	if a.focus != focusCmdline {
		t.Fatalf("shift-tab from a table = %d, want cmdline", a.focus)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyTab})
	if a.focus != focusSchedules {
		t.Fatalf("tab back onto the tables = %d, want the one last shown (schedules)", a.focus)
	}
}

func TestArrowsSwitchTables(t *testing.T) {
	a := oneTableApp(120, 40)
	a.setFocus(focusRunners)
	want := []focus{focusTasks, focusSchedules, focusCredentials, focusRunners}
	for _, w := range want {
		a.Update(tea.KeyMsg{Type: tea.KeyRight})
		if a.focus != w {
			t.Fatalf("right = %d, want %d", a.focus, w)
		}
	}
	a.Update(tea.KeyMsg{Type: tea.KeyLeft})
	if a.focus != focusCredentials {
		t.Fatalf("left from runners = %d, want credentials (wraps)", a.focus)
	}
	// Off the tables the arrows are the pane's own (the command line's cursor).
	a.setFocus(focusCmdline)
	a.Update(tea.KeyMsg{Type: tea.KeyRight})
	if a.focus != focusCmdline {
		t.Fatalf("right on the command line moved focus to %d", a.focus)
	}
}

// tabBar parses the strip's tab bar (before " | "): the table names in the
// order listed, which one is bracketed (shown, focus on it), and the keys a
// name carries ("Tasks:Tab*2", shown while focus is off the tables).
type tabBarEntry struct {
	name      string
	bracketed bool
	keys      string
}

func tabBar(t *testing.T, s string) []tabBarEntry {
	t.Helper()
	var out []tabBarEntry
	for _, part := range strings.Fields(strings.SplitN(s, " | ", 2)[0]) {
		e := tabBarEntry{}
		if strings.HasPrefix(part, "[") && strings.HasSuffix(part, "]") {
			e.bracketed = true
			part = strings.Trim(part, "[]")
		}
		e.name, e.keys, _ = strings.Cut(part, ":")
		out = append(out, e)
	}
	return out
}

// pressKeys presses what a strip entry's keys say: "Tab" / "S-Tab", with an
// optional "*N" repeat count.
func pressKeys(t *testing.T, a *App, keys string) {
	t.Helper()
	n := 1
	if k, cnt, has := strings.Cut(keys, "*"); has {
		keys = k
		var err error
		if n, err = strconv.Atoi(cnt); err != nil {
			t.Fatalf("unreadable repeat count in %q", keys)
		}
	}
	switch keys {
	case "Tab":
		pressKey(a, tea.KeyTab, n)
	case "S-Tab":
		pressKey(a, tea.KeyShiftTab, n)
	default:
		t.Fatalf("unreadable keys %q", keys)
	}
}

func pressKey(a *App, k tea.KeyType, times int) {
	for i := 0; i < times; i++ {
		a.Update(tea.KeyMsg{Type: k})
	}
}

var tableFocusByName = map[string]focus{"Runners": focusRunners, "Tasks": focusTasks, "Schedules": focusSchedules, "Credentials": focusCredentials}

// The tab bar lists every table, by its full name, and brackets exactly the
// shown one; nothing else on it carries a key.
func TestTabBarListsEveryTableAndBracketsTheShownOne(t *testing.T) {
	for _, shown := range tableOrder {
		a := oneTableApp(120, 40)
		a.setFocus(shown)
		bar := tabBar(t, tableStrip(a.shownTable(), a.focus, nil))
		if len(bar) != len(tableOrder) {
			t.Fatalf("shown %d: the bar lists %d tables, want %d: %+v", shown, len(bar), len(tableOrder), bar)
		}
		for i, e := range bar {
			f, ok := tableFocusByName[e.name]
			if !ok {
				t.Fatalf("shown %d: %q is not a table's full name", shown, e.name)
			}
			if f != tableOrder[i] {
				t.Errorf("shown %d: entry %d is %s, want table %d", shown, i, e.name, tableOrder[i])
			}
			if e.bracketed != (f == shown) {
				t.Errorf("shown %d: %s bracketed=%v", shown, e.name, e.bracketed)
			}
			if e.keys != "" {
				t.Errorf("shown %d: %s carries keys %q with focus on a table", shown, e.name, e.keys)
			}
		}
	}
}

// ←/→ move along the order the bar lists, wrapping at either end: pressing
// one moves the bracket to the neighbouring name.
func TestTabBarArrowsMoveAlongTheListedOrder(t *testing.T) {
	for _, from := range tableOrder {
		for _, k := range []struct {
			key  tea.KeyType
			step int
		}{{tea.KeyRight, 1}, {tea.KeyLeft, -1}} {
			a := oneTableApp(120, 40)
			a.setFocus(from)
			bar := tabBar(t, tableStrip(a.shownTable(), a.focus, nil))
			i := 0
			for j, e := range bar {
				if e.bracketed {
					i = j
				}
			}
			want := tableFocusByName[bar[(i+k.step+len(bar))%len(bar)].name]
			pressKey(a, k.key, 1)
			if a.focus != want {
				t.Errorf("from %d, %v lands on %d; the bar lists %d next", from, k.key, a.focus, want)
			}
			after := tabBar(t, tableStrip(a.shownTable(), a.focus, nil))
			for _, e := range after {
				if e.bracketed != (tableFocusByName[e.name] == want) {
					t.Errorf("after %v from %d the bracket is not on the table shown: %+v", k.key, from, after)
				}
			}
		}
	}
}

// Off the tables the one hint left: the shown table carries the keys that go
// back onto it from where focus IS, and pressing them lands there. An earlier
// strip said "Tab switches" and was wrong.
func TestTabBarFocusOffHintReturnsFocus(t *testing.T) {
	for _, shown := range tableOrder {
		for _, off := range []focus{focusNotify, focusCmdresult, focusCmdline} {
			a := oneTableApp(120, 40)
			a.setFocus(shown)
			a.setFocus(off) // the table stays shown
			strip := tableStrip(a.shownTable(), a.focus, nil)
			var keys string
			for _, e := range tabBar(t, strip) {
				if e.bracketed {
					t.Errorf("from %d: %s bracketed with focus off the tables: %q", off, e.name, strip)
				}
				if e.keys != "" && tableFocusByName[e.name] != shown {
					t.Errorf("from %d: %s, not the shown table, carries keys: %q", off, e.name, strip)
				}
				if tableFocusByName[e.name] == shown {
					keys = e.keys
				}
			}
			if keys == "" {
				t.Fatalf("from %d the shown table carries no keys back: %q", off, strip)
			}
			pressKeys(t, a, keys)
			if a.focus != shown {
				t.Errorf("from %d, strip %q: its keys land on %d, want %d", off, strip, a.focus, shown)
			}
		}
	}
}

// One decision at every width (checklist item 34): the bar is the same text at
// 80 and 200 columns, and with a realistic fleet summary (two-digit counts, an
// offline bucket) it fits 80 cells for every focus under both East Asian width
// readings, every name in full and the summary whole.
func TestTabBarFitsEightyColumnsAndIsTheSameAtEveryWidth(t *testing.T) {
	var fleet []protocol.RunnerInfo
	for i := 0; i < 12; i++ {
		st := []protocol.RunnerStatus{protocol.RunnerStatus_Idle, protocol.RunnerStatus_Busy, protocol.RunnerStatus_Offline}[i%3]
		fleet = append(fleet, protocol.RunnerInfo{Status: st})
	}
	const summary = "runners 12: 4 idle 4 busy 4 offline"
	for _, f := range []focus{focusRunners, focusTasks, focusSchedules, focusCredentials, focusNotify, focusCmdresult, focusCmdline} {
		for _, shown := range tableOrder {
			if isTableFocus(f) && f != shown {
				continue
			}
			strip := tableStrip(shown, f, fleet)
			for name, cond := range map[string]*runewidth.Condition{"narrow": {EastAsianWidth: false}, "wide": {EastAsianWidth: true}} {
				if w := cond.StringWidth(strip); w > 80 {
					t.Errorf("focus %d shown %d: strip is %d cells under %s width (> 80): %q", f, shown, w, name, strip)
				}
			}
			if !strings.Contains(strip, summary) || !strings.Contains(strip, "Credentials") {
				t.Errorf("focus %d shown %d: a name or the summary is cut: %q", f, shown, strip)
			}
		}
	}
	barLine := func(width int) string {
		a := oneTableApp(width, 40)
		a.Update(SnapshotMsg{Runners: fleet})
		a.setFocus(focusSchedules)
		a.setFocus(focusCmdline)
		for _, ln := range strings.Split(a.View(), "\n") {
			if strings.Contains(ln, summary) {
				return strings.TrimRight(stripANSI(ln), " ")
			}
		}
		t.Fatalf("no strip line at width %d", width)
		return ""
	}
	if narrow, wide := barLine(80), barLine(200); narrow != wide {
		t.Errorf("the bar differs between 80 and 200 columns:\n 80: %q\n200: %q", narrow, wide)
	}
}

// --- table ---

func TestSchedulesTableRows(t *testing.T) {
	a := schedApp(200, 50)
	m := &a.schedules
	rows := m.table.Rows()
	if len(rows) != 3 || len(rows) != len(m.rows) {
		t.Fatalf("%d table rows, %d rows", len(rows), len(m.rows))
	}
	for i, r := range rows {
		if len(r) != len(m.table.Columns()) {
			t.Fatalf("row %d has %d cells, %d columns (%v)", i, len(r), len(m.table.Columns()), schedColTitles(m))
		}
	}
	col := map[string]int{}
	for i, c := range m.table.Columns() {
		col[c.Title] = i
	}
	for _, want := range []string{"ID", "Cron", "Next", "Last", "Task", "Agent", "Repo", "About"} {
		if _, ok := col[want]; !ok {
			t.Fatalf("no %s column at 200 wide: %v", want, schedColTitles(m))
		}
	}
	now := time.Now()
	for i, r := range m.rows {
		row := rows[i]
		if want := hex.EncodeToString(r.Id.Id[:4]); row[col["ID"]] != want {
			t.Errorf("row %d ID = %q, want %q", i, row[col["ID"]], want)
		}
		if row[col["Cron"]] != "0 9 * * 1-5" {
			t.Errorf("row %d Cron = %q", i, row[col["Cron"]])
		}
		if row[col["Next"]] != cli.ScheduleNextCell(r) {
			t.Errorf("row %d Next = %q, want %q", i, row[col["Next"]], cli.ScheduleNextCell(r))
		}
		if row[col["Last"]] != cli.ScheduleLastCell(r, now) && i != 1 {
			t.Errorf("row %d Last = %q", i, row[col["Last"]])
		}
		if row[col["Task"]] != cli.ScheduleTaskCell(r) {
			t.Errorf("row %d Task = %q, want %q", i, row[col["Task"]], cli.ScheduleTaskCell(r))
		}
		if row[col["About"]] != cli.ScheduleAboutCell(r) {
			t.Errorf("row %d About = %q", i, row[col["About"]])
		}
	}
	// Values that mean "nothing" are still printed (checklist 31).
	if got := rows[0][col["Last"]]; got != "none" {
		t.Errorf("never-fired Last = %q, want none", got)
	}
	if got := rows[0][col["Task"]]; got != "created-at-first-firing" {
		t.Errorf("pending Task = %q", got)
	}
	if got := rows[1][col["Task"]]; got != "c3c3c3c3:running" {
		t.Errorf("bound Task = %q", got)
	}
	if got := rows[1][col["Last"]]; !strings.HasPrefix(got, "fired ") {
		t.Errorf("fired Last = %q", got)
	}
	if got := rows[2][col["Next"]]; got != "none" {
		t.Errorf("no-next Next = %q, want none", got)
	}
	if got := rows[2][col["Task"]]; got != "no-task" {
		t.Errorf("unbound Task = %q", got)
	}
	if got := rows[2][col["About"]]; got != "(hidden)" {
		t.Errorf("hidden-prompt About = %q", got)
	}
	if got := m.SelectedID(); got != schedHex(0xa1) {
		t.Errorf("SelectedID = %q", got)
	}
}

// The column SET depends on width and its rows are rebuilt with it, at every
// width, in both directions; repo cells are cut to the fitted column.
func TestSchedulesColumnsAndRowsAgreeAcrossWidths(t *testing.T) {
	eastAsian(t)
	a := schedApp(200, 50)
	for _, w := range []int{80, schedAboutMinWidth - 1, schedAboutMinWidth, schedWideMinWidth - 1, schedWideMinWidth, 200, 90, schedWideMinWidth + 20, 80} {
		a.width = w
		a.layout()
		m := &a.schedules
		for i, r := range m.table.Rows() {
			if len(r) != len(m.table.Columns()) {
				t.Fatalf("w=%d row %d has %d cells, %d columns (%v)", w, i, len(r), len(m.table.Columns()), schedColTitles(m))
			}
		}
		for _, want := range []string{"ID", "Cron", "Next", "Last", "Task"} {
			found := false
			for _, c := range m.table.Columns() {
				found = found || c.Title == want
			}
			if !found {
				t.Errorf("w=%d: no %s column (%v)", w, want, schedColTitles(m))
			}
		}
		// Pinned columns keep their declared width at every tier.
		for _, c := range m.table.Columns() {
			if (c.Title == "ID" && c.Width != 8) || (c.Title == "Cron" && c.Width != 15) || (c.Title == "Next" && c.Width != 20) {
				t.Errorf("w=%d: %s was squeezed to %d", w, c.Title, c.Width)
			}
		}
		for i, c := range m.table.Columns() {
			if c.Title != "Repo" {
				continue
			}
			cell := m.table.Rows()[0][i]
			if runewidth.StringWidth(cell) > c.Width || !strings.HasSuffix(cell, "-long-name") {
				t.Errorf("w=%d: Repo cell %q (%d) in a %d column", w, cell, runewidth.StringWidth(cell), c.Width)
			}
		}
		_ = a.View()
	}
}

func TestSchedulesViewFitsTerminal(t *testing.T) {
	eastAsian(t)
	for _, sz := range [][2]int{{80, 24}, {200, 50}} {
		a := schedApp(sz[0], sz[1])
		a.setFocus(focusSchedules)
		v := a.View()
		lines := strings.Split(v, "\n")
		if len(lines) != sz[1] {
			t.Errorf("%v: view is %d rows", sz, len(lines))
		}
		for i, l := range lines {
			if n := lipgloss.Width(l); n > sz[0] {
				t.Errorf("%v: line %d is %d wide: %q", sz, i, n, l)
			}
		}
		if !strings.Contains(v, "Cron") || strings.Contains(v, "Roots") || !strings.Contains(v, "created-at-first-firing") && sz[0] >= 200 {
			t.Errorf("%v: schedules table not shown:\n%s", sz, v)
		}
	}
}

// --- keys ---

type schedExecCall struct {
	sub, id string
}

func schedKeyApp(t *testing.T) (*App, *[]schedExecCall) {
	t.Helper()
	a := schedApp(120, 40)
	a.setFocus(focusSchedules)
	var calls []schedExecCall
	a.schedExec = func(_ context.Context, act verb.ScheduleIDAction, out io.Writer) error {
		calls = append(calls, schedExecCall{act.Sub, act.ID})
		io.WriteString(out, "schedule "+act.Sub+" "+act.ID+": done")
		return nil
	}
	return a, &calls
}

func pressRune(a *App, r rune) tea.Cmd {
	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	return cmd
}

func runCmd(a *App, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	a.Update(cmd())
}

func TestScheduleRunNowKey(t *testing.T) {
	a, calls := schedKeyApp(t)
	a.schedules.table.MoveDown(1) // the bound schedule
	cmd := pressRune(a, rune(mainKeys.ScheduleRunNow[0]))
	if cmd == nil {
		t.Fatal("the run-now key returned no command")
	}
	runCmd(a, cmd)
	want := []schedExecCall{{verb.SubRun, schedHex(0xb2)}}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("calls = %v, want %v", *calls, want)
	}
	if !strings.Contains(strings.Join(a.cmdresult.lines, "\n"), "schedule run "+schedHex(0xb2)+": done") {
		t.Errorf("the result must name the target; cmdresult:\n%s", strings.Join(a.cmdresult.lines, "\n"))
	}
}

// The run-now and delete keys do nothing outside the schedules table.
func TestScheduleKeysAreScopedToSchedules(t *testing.T) {
	a, calls := schedKeyApp(t)
	a.setFocus(focusTasks)
	pressRune(a, rune(mainKeys.ScheduleRunNow[0]))
	pressRune(a, rune(mainKeys.ScheduleDelete[0]))
	if len(*calls) != 0 || a.schedDeleteID != "" {
		t.Errorf("fired from the tasks table: %v / %q", *calls, a.schedDeleteID)
	}
}

func TestScheduleDeleteConfirms(t *testing.T) {
	id := schedHex(0xa1)
	t.Run("x alone deletes nothing, y does", func(t *testing.T) {
		a, calls := schedKeyApp(t)
		if cmd := pressRune(a, rune(mainKeys.ScheduleDelete[0])); cmd != nil {
			t.Fatal("x must only ask")
		}
		if len(*calls) != 0 {
			t.Fatalf("deleted before the confirm: %v", *calls)
		}
		if !strings.Contains(a.View(), "y") || !strings.Contains(a.View(), id[:8]) {
			t.Errorf("the prompt naming the target must be on screen:\n%s", a.View())
		}
		runCmd(a, pressRune(a, 'y'))
		want := []schedExecCall{{verb.SubRm, id}}
		if !reflect.DeepEqual(*calls, want) {
			t.Fatalf("calls = %v, want %v", *calls, want)
		}
		if a.schedDeleteID != "" {
			t.Error("the confirm must be spent")
		}
	})
	t.Run("any other key cancels", func(t *testing.T) {
		for _, k := range []rune{'n', 'x', 'q'} {
			a, calls := schedKeyApp(t)
			pressRune(a, rune(mainKeys.ScheduleDelete[0]))
			cmd := pressRune(a, k)
			if cmd != nil && k == 'q' {
				t.Error("q must cancel, not quit, while the confirm is up")
			}
			runCmd(a, cmd)
			if len(*calls) != 0 || a.schedDeleteID != "" {
				t.Errorf("key %q: calls=%v armed=%q", k, *calls, a.schedDeleteID)
			}
			if !strings.Contains(strings.Join(a.cmdresult.lines, "\n"), "cancelled") || !strings.Contains(strings.Join(a.cmdresult.lines, "\n"), id[:8]) {
				t.Errorf("key %q: the cancel must say what was kept; cmdresult:\n%s", k, strings.Join(a.cmdresult.lines, "\n"))
			}
		}
	})
}

func TestScheduleDetailKeyAndPopup(t *testing.T) {
	a := schedApp(120, 40)
	a.setFocus(focusSchedules)
	a.client = &cli.Client{} // non-nil is enough; the returned cmd is never run
	if cmd := pressRune(a, rune(mainKeys.Detail[0])); cmd == nil {
		t.Fatal("d on a schedule must fetch its detail")
	}
	d := protocol.ScheduleDetail{Row: schedFixRows()[0]}
	d.SetPrompt([]byte("triage new issues"))
	a.Update(ScheduleShowMsg{ID: hex.EncodeToString(d.Row.Id.Id[:]), Detail: d})
	if !a.detail.IsOpen() {
		t.Fatal("the detail popup did not open")
	}
	body := a.detail.body
	for _, want := range []string{"id: " + hex.EncodeToString(d.Row.Id.Id[:]), "zone: Asia/Tokyo", "task: created at first firing", "waits: n=0", "history: none yet"} {
		if !strings.Contains(body, want) {
			t.Errorf("popup lacks %q:\n%s", want, body)
		}
	}
}

func TestScheduleShowErrorGoesToCmdresult(t *testing.T) {
	a := schedApp(120, 40)
	a.Update(ScheduleShowMsg{ID: "ab", Err: context.DeadlineExceeded})
	if a.detail.IsOpen() || !strings.Contains(strings.Join(a.cmdresult.lines, "\n"), "schedule show ab") {
		t.Errorf("open=%v cmdresult:\n%s", a.detail.IsOpen(), strings.Join(a.cmdresult.lines, "\n"))
	}
}

// --- refresh ---

func TestListFillsSchedulesAndAScheduleVerbRefreshes(t *testing.T) {
	a := oneTableApp(120, 40)
	a.Update(ScheduleListMsg{Rows: schedFixRows()})
	if got := len(a.schedules.table.Rows()); got != 3 {
		t.Fatalf("%d rows after the list", got)
	}
	// A failed list keeps what was there and says why, once.
	a.Update(ScheduleListMsg{Err: context.DeadlineExceeded})
	a.Update(ScheduleListMsg{Err: context.DeadlineExceeded})
	if got := len(a.schedules.table.Rows()); got != 3 {
		t.Errorf("a failed list dropped the rows: %d", got)
	}
	if n := strings.Count(strings.Join(a.cmdresult.lines, "\n"), "schedules:"); n != 1 {
		t.Errorf("the failure was reported %d times:\n%s", n, strings.Join(a.cmdresult.lines, "\n"))
	}
	a.client = &cli.Client{}
	if _, cmd := a.Update(RecordResultMsg{Verb: "schedule new", Text: "ok"}); cmd == nil {
		t.Error("a schedule verb's result must refresh the snapshot (and with it the table)")
	}
	if _, cmd := a.Update(RecordResultMsg{Verb: "record ls", Text: "ok"}); cmd != nil {
		t.Error("other record verbs must not")
	}
}

// --- task binding ---

func schedTaskEvent(id byte, kind protocol.StatusEventKind, sched *protocol.ScheduleID) TaskEventMsg {
	ev := protocol.TaskStatusEvent{Kind: kind, TaskStatus: protocol.TaskStatus_Succeeded}
	ev.TaskId.Id[0] = id
	if sched != nil {
		ev.SetSchedulePresent(true)
		ev.SetScheduleId(*sched)
	}
	return TaskEventMsg{Event: ev}
}

func TestTaskDetailNamesSchedule(t *testing.T) {
	tk := liveTask(7, 0, 0)
	if strings.Contains(formatTaskDetail(tk, nil), "schedule:") {
		t.Fatal("an unbound task must not name a schedule")
	}
	sid := schedFixID(0xee)
	tk.SetSchedulePresent(true)
	tk.SetScheduleId(sid)
	got := formatTaskDetail(tk, nil)
	if want := "schedule:      " + hex.EncodeToString(sid.Id[:]); !strings.Contains(got, want) {
		t.Errorf("task detail lacks %q:\n%s", want, got)
	}
}

// Every task event carries the binding, a binding-change event included, so
// the popup follows without waiting for the next list.
func TestTaskEventsApplyTheScheduleBinding(t *testing.T) {
	a := oneTableApp(120, 40)
	tk := liveTask(9, 0, 0)
	a.Update(SnapshotMsg{Tasks: []protocol.TaskInfo{tk}})
	sid := schedFixID(0xee)

	a.Update(schedTaskEvent(9, protocol.StatusEventKind_TaskSchedule, &sid))
	got := a.tasksByID[FormatTaskID(tk.Id)]
	if !got.SchedulePresent() || got.ScheduleId() == nil || *got.ScheduleId() != sid {
		t.Fatalf("binding not applied: present=%v id=%v", got.SchedulePresent(), got.ScheduleId())
	}
	if !strings.Contains(formatTaskDetail(*a.tasks.SelectedTask(), nil), hex.EncodeToString(sid.Id[:])) {
		t.Errorf("the table's row model did not take the binding")
	}
	// Another kind of event keeps it; an unbinding event clears it.
	a.Update(schedTaskEvent(9, protocol.StatusEventKind_TaskEnded, &sid))
	if g := a.tasksByID[FormatTaskID(tk.Id)]; !g.SchedulePresent() {
		t.Error("an ordinary event dropped the binding")
	}
	a.Update(schedTaskEvent(9, protocol.StatusEventKind_TaskSchedule, nil))
	if g := a.tasksByID[FormatTaskID(tk.Id)]; g.SchedulePresent() {
		t.Error("an event with no binding must clear it")
	}
	// A task first seen through an event takes it as well.
	a.Update(schedTaskEvent(10, protocol.StatusEventKind_TaskSchedule, &sid))
	var newID protocol.TaskID
	newID.Id[0] = 10
	if g := a.tasksByID[FormatTaskID(newID)]; !g.SchedulePresent() {
		t.Error("a stubbed row did not take the binding")
	}
}

func TestMainKeysDoNotCollideForSchedules(t *testing.T) {
	// n and x: distinct from every other main key (TestMainKeysAreUnique), and
	// in the schedules pane each is dispatched by its schedule row alone. x
	// has a second row, in the credentials scope only (revoke):
	// TestSameKeyRowsHaveDisjointScopes keeps the two apart.
	for _, k := range []string{mainKeys.ScheduleRunNow, mainKeys.ScheduleDelete} {
		for _, b := range mainKeyBindings {
			for _, bk := range b.Keys {
				if bk == k && b.Scope != scopeSchedules && b.Scope != scopeCredentials {
					t.Errorf("key %q also bound with scope %v", k, b.Scope)
				}
			}
		}
	}
}

// --- fix round 1 ---

// schedListSeam installs a fake list that counts its calls and answers rows.
func schedListSeam(a *App, rows func() []protocol.ScheduleRow) *int {
	n := 0
	a.schedList = func(context.Context) ([]protocol.ScheduleRow, error) {
		n++
		return rows(), nil
	}
	return &n
}

// drain runs cmd and feeds every message it yields back, following batches and
// the commands those messages return.
func drain(a *App, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	switch m := cmd().(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range m {
			drain(a, c)
		}
	default:
		_, next := a.Update(m)
		drain(a, next)
	}
}

// boundSchedApp has one schedule bound to a live task (id 0x44), and the task
// in the snapshot.
func boundSchedApp(t *testing.T) (*App, protocol.TaskInfo, protocol.ScheduleRow) {
	t.Helper()
	a := oneTableApp(200, 50)
	tk := liveTask(0x44, 0, 0)
	row := schedFixRow(0xb2, protocol.ScheduleTaskState_Bound)
	row.TaskId = tk.Id
	row.TaskStatus = protocol.TaskStatus_Running
	a.Update(SnapshotMsg{Tasks: []protocol.TaskInfo{tk}})
	a.Update(ScheduleListMsg{Rows: []protocol.ScheduleRow{row}})
	return a, tk, row
}

func schedTaskCell(a *App) string {
	for i, c := range a.schedules.table.Columns() {
		if c.Title == "Task" {
			return a.schedules.table.Rows()[0][i]
		}
	}
	return ""
}

// A later firing resumes a task that is already bound: its events change the
// task row and, on the schedule's side, next / last / task status. The table
// asks for a fresh list when an event names a binding (or is the binding
// changing), and its Task cell follows the task's live status before that list
// lands.
func TestScheduleTableFollowsBoundTaskEvents(t *testing.T) {
	a, tk, row := boundSchedApp(t)
	calls := schedListSeam(a, func() []protocol.ScheduleRow { return []protocol.ScheduleRow{row} })
	sid := row.Id
	for _, st := range []protocol.TaskStatus{protocol.TaskStatus_Queued, protocol.TaskStatus_Running, protocol.TaskStatus_Succeeded} {
		before := *calls
		ev := protocol.TaskStatusEvent{Kind: protocol.StatusEventKind_TaskStarted, TaskStatus: st, TaskId: tk.Id}
		ev.SetSchedulePresent(true)
		ev.SetScheduleId(sid)
		_, cmd := a.Update(TaskEventMsg{Event: ev})
		if cmd == nil {
			t.Fatalf("status %v: an event of a bound task requested no list refresh", st)
		}
		want := row
		want.TaskStatus = st
		if got := schedTaskCell(a); got != cli.ScheduleTaskCell(want) {
			t.Errorf("status %v: Task cell %q, want %q (live, before the list answers)", st, got, cli.ScheduleTaskCell(want))
		}
		drain(a, cmd)
		if *calls != before+1 {
			t.Errorf("status %v: %d list calls, want %d", st, *calls, before+1)
		}
	}
	// A binding-change event with no binding (an unbind) refreshes too; an
	// ordinary event of an unbound task does not.
	_, cmd := a.Update(TaskEventMsg{Event: protocol.TaskStatusEvent{Kind: protocol.StatusEventKind_TaskSchedule, TaskStatus: protocol.TaskStatus_Succeeded, TaskId: tk.Id}})
	if cmd == nil {
		t.Error("an unbind (task_schedule, no binding) requested no refresh")
	}
	drain(a, cmd)
	_, cmd = a.Update(TaskEventMsg{Event: protocol.TaskStatusEvent{Kind: protocol.StatusEventKind_TaskEnded, TaskStatus: protocol.TaskStatus_Succeeded, TaskId: tk.Id}})
	if cmd != nil {
		t.Error("an event of an unbound task must not list schedules")
	}
}

// A burst of events asks for at most two lists: one in flight, and one more for
// everything that arrived meanwhile.
func TestScheduleListRefreshIsCoalesced(t *testing.T) {
	a, tk, row := boundSchedApp(t)
	calls := schedListSeam(a, func() []protocol.ScheduleRow { return []protocol.ScheduleRow{row} })
	var cmds []tea.Cmd
	for i := 0; i < 10; i++ {
		ev := protocol.TaskStatusEvent{Kind: protocol.StatusEventKind_TaskStarted, TaskStatus: protocol.TaskStatus_Running, TaskId: tk.Id}
		ev.SetSchedulePresent(true)
		ev.SetScheduleId(row.Id)
		if _, cmd := a.Update(TaskEventMsg{Event: ev}); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	if len(cmds) != 1 {
		t.Fatalf("a burst of 10 issued %d list commands, want 1 in flight", len(cmds))
	}
	drain(a, cmds[0]) // answers, then the one coalesced follow-up
	if *calls != 2 {
		t.Errorf("a burst of 10 made %d list calls, want 2", *calls)
	}
}

// A list that never answers must not hold the snapshot: it is its own command.
func TestHangingScheduleListDoesNotDelayTheSnapshot(t *testing.T) {
	a := oneTableApp(120, 40)
	block := make(chan struct{})
	defer close(block)
	a.schedList = func(ctx context.Context) ([]protocol.ScheduleRow, error) {
		select {
		case <-block:
		case <-ctx.Done():
		}
		return nil, ctx.Err()
	}
	_, lcmd := a.Update(ScheduleListMsg{Rows: nil})
	_ = lcmd
	listCmd := a.requestScheduleList()
	if listCmd == nil {
		t.Fatal("no list command")
	}
	go listCmd() // hangs
	done := make(chan struct{})
	go func() {
		a.Update(SnapshotMsg{Tasks: []protocol.TaskInfo{liveTask(1, 0, 0)}})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the snapshot waited on the schedule list")
	}
	if len(a.tasks.Rows()) != 1 {
		t.Error("the snapshot was not applied")
	}
	src, _ := os.ReadFile("client.go")
	i := strings.Index(string(src), "func RefreshSnapshot")
	if body := string(src)[i : i+900]; strings.Contains(body, "ScheduleList") {
		t.Error("RefreshSnapshot must not fetch schedules inside its own budget")
	}
}

func TestResolveSchedulePrefix(t *testing.T) {
	a := schedApp(120, 40)
	full := schedHex(0xa1)
	if got, e := a.resolveSchedulePrefix("a1a1"); got != full || e != "" {
		t.Errorf("unique prefix: %q %q", got, e)
	}
	if got, e := a.resolveSchedulePrefix(strings.ToUpper(full[:6])); got != full || e != "" {
		t.Errorf("upper-case prefix: %q %q", got, e)
	}
	if got, e := a.resolveSchedulePrefix("zz"); got != "" || !strings.Contains(e, "no schedule matches") {
		t.Errorf("none: %q %q", got, e)
	}
	// Two rows sharing a prefix.
	rows := schedFixRows()
	rows[1].Id = rows[0].Id
	rows[1].Id.Id[15] = 0x01
	a.Update(ScheduleListMsg{Rows: rows})
	if got, e := a.resolveSchedulePrefix("a1a1"); got != "" || !strings.Contains(e, "ambiguous") {
		t.Errorf("ambiguous: %q %q", got, e)
	}
	// A full id the table does not list yet (just created) goes to the server.
	other := schedHex(0x77)
	if got, e := a.resolveSchedulePrefix(other); got != other || e != "" {
		t.Errorf("full unknown id: %q %q", got, e)
	}
}

func TestTypedScheduleVerbsResolveThePrefix(t *testing.T) {
	a, calls := schedKeyApp(t)
	h := tuiVerbs{a}
	cmd := h.ScheduleRun(verb.ScheduleIDAction{Sub: verb.SubRun, ID: "b2b2"})
	runCmd(a, cmd)
	if want := []schedExecCall{{verb.SubRun, schedHex(0xb2)}}; !reflect.DeepEqual(*calls, want) {
		t.Fatalf("calls %+v, want %+v", *calls, want)
	}
	// none / ambiguous: nothing is sent, the error is in cmdresult.
	if cmd := h.ScheduleRm(verb.ScheduleIDAction{Sub: verb.SubRm, ID: "zz"}); cmd != nil {
		t.Error("an unresolved id must send nothing")
	}
	if !strings.Contains(strings.Join(a.cmdresult.lines, "\n"), "no schedule matches zz") {
		t.Errorf("cmdresult:\n%s", strings.Join(a.cmdresult.lines, "\n"))
	}
	if cmd := h.ScheduleShow(verb.ScheduleShowAction{ID: "zz"}); cmd != nil {
		t.Error("show with an unresolved id must send nothing")
	}
}

// Errors from n / x and the typed verbs name the target and keep the server's
// status words, not "schedule run failed: schedule run: ...".
func TestScheduleErrorsNameTheTarget(t *testing.T) {
	a, _ := schedKeyApp(t)
	a.schedExec = func(_ context.Context, act verb.ScheduleIDAction, _ io.Writer) error {
		return errors.New("schedule " + act.Sub + ": no_such_schedule: gone")
	}
	runCmd(a, pressRune(a, rune(mainKeys.ScheduleRunNow[0])))
	got := strings.Join(a.cmdresult.lines, "\n")
	if !strings.Contains(got, "schedule run a1a1a1a1: no_such_schedule: gone") || strings.Contains(got, "failed:") {
		t.Errorf("cmdresult:\n%s", got)
	}
}

func TestScheduleLastAgeMovesOnTheTick(t *testing.T) {
	a := schedApp(200, 50)
	a.setFocus(focusSchedules)
	lastCell := func() string {
		for i, c := range a.schedules.table.Columns() {
			if c.Title == "Last" {
				return a.schedules.table.Rows()[1][i]
			}
		}
		return ""
	}
	before := lastCell()
	a.schedules.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	a.Update(actAgeTickMsg{})
	if after := lastCell(); after == before {
		t.Errorf("Last still %q after the tick", after)
	}
}

// The delete asks about the id captured at x: a list that reorders the rows
// before y must not move the target.
func TestScheduleDeleteKeepsTheCapturedID(t *testing.T) {
	a, calls := schedKeyApp(t)
	captured := a.schedules.SelectedID()
	pressRune(a, rune(mainKeys.ScheduleDelete[0]))
	rows := schedFixRows()
	rows[0], rows[2] = rows[2], rows[0]
	a.Update(ScheduleListMsg{Rows: rows})
	if a.schedules.SelectedID() == captured {
		t.Fatal("the reorder did not move the cursor's row; the test proves nothing")
	}
	runCmd(a, pressRune(a, 'y'))
	if want := []schedExecCall{{verb.SubRm, captured}}; !reflect.DeepEqual(*calls, want) {
		t.Errorf("calls = %v, want %v", *calls, want)
	}
}

// The history lines are ~200 cells; the popup must wrap them so the outcome
// and a long skipped_refused message are readable at 80 columns.
func TestScheduleDetailPopupWrapsLongHistoryAt80(t *testing.T) {
	a := schedApp(80, 24)
	d := protocol.ScheduleDetail{Row: schedFixRows()[0]}
	d.SetPrompt([]byte("triage"))
	e := protocol.ScheduleHistoryEntry{Outcome: protocol.ScheduleOutcome_SkippedRefused, DueAt: 1, At: 1}
	const msg = "spawn limit reached because the parent already holds forty descendants and refuses more today"
	e.SetMessage([]byte(msg))
	d.SetHistory([]protocol.ScheduleHistoryEntry{e})
	a.Update(ScheduleShowMsg{ID: hex.EncodeToString(d.Row.Id.Id[:]), Detail: d})
	a.detail.SetSize(80, 24)
	shown := a.detail.vp.View()
	a.detail.vp.GotoBottom()
	shown = a.detail.vp.View()
	content := a.detail.content()
	for _, ln := range strings.Split(content, "\n") {
		if w := runewidth.StringWidth(ln); w > a.detail.vp.Width {
			t.Errorf("line %d cells > width %d: %q", w, a.detail.vp.Width, ln)
		}
	}
	flat := strings.Join(strings.Fields(content), " ")
	for _, want := range []string{"skipped_refused", "end=-", "msg=\"" + msg + "\""} {
		if !strings.Contains(flat, want) {
			t.Errorf("popup content lacks %q:\n%s", want, content)
		}
	}
	if !strings.Contains(strings.Join(strings.Fields(shown), " "), "forty descendants and refuses more today") {
		t.Errorf("the message tail is not in the scrolled-to-bottom view:\n%s", shown)
	}
}
