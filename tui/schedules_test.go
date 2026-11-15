package tui

import (
	"context"
	"encoding/hex"
	"io"
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
	a.Update(SnapshotMsg{Schedules: schedFixRows()})
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

func TestFocusCycleIncludesSchedules(t *testing.T) {
	a := oneTableApp(120, 40)
	a.setFocus(focusRunners)
	want := []focus{focusTasks, focusSchedules, focusNotify, focusCmdresult, focusCmdline, focusRunners}
	var got []focus
	for range want {
		a.Update(tea.KeyMsg{Type: tea.KeyTab})
		got = append(got, a.focus)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tab cycle = %v, want %v", got, want)
	}
}

// stripHint parses the strip's "<Table>:<keys>" entries for the tables that are
// not shown. keys is "Tab", "Shift-Tab", optionally followed by " xN".
func stripHint(t *testing.T, s string, table string) (back bool, n int) {
	t.Helper()
	for _, part := range strings.Split(strings.SplitN(s, " · ", 2)[0], "  ") {
		name, keys, ok := strings.Cut(strings.TrimSpace(part), ":")
		if !ok || strings.Trim(name, "[]") != table {
			continue
		}
		n = 1
		if k, cnt, has := strings.Cut(keys, " x"); has {
			keys = k
			n, _ = strconv.Atoi(cnt)
		}
		switch keys {
		case "Tab":
			return false, n
		case "Shift-Tab":
			return true, n
		}
		t.Fatalf("strip %q: unreadable keys %q for %s", s, keys, table)
	}
	t.Fatalf("strip %q does not name %s", s, table)
	return
}

// The strip names all three tables and, from the table shown, the keys that
// reach the others. Each hint is PRESSED against the real focus cycle: an
// earlier strip said "Tab switches" and was wrong.
func TestStripNamesThreeTables(t *testing.T) {
	tables := map[string]focus{"Runners": focusRunners, "Tasks": focusTasks, "Schedules": focusSchedules}
	for fromName, from := range tables {
		a := oneTableApp(120, 40)
		a.setFocus(from)
		strip := tableStrip(a.shownTable(), a.focus, nil)
		if !strings.Contains(strip, "["+fromName+"]") {
			t.Errorf("from %s: shown table is not bracketed: %q", fromName, strip)
		}
		for toName, to := range tables {
			if toName == fromName {
				continue
			}
			back, n := stripHint(t, strip, toName)
			a.setFocus(from)
			for i := 0; i < n; i++ {
				if back {
					a.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
				} else {
					a.Update(tea.KeyMsg{Type: tea.KeyTab})
				}
			}
			if a.focus != to {
				t.Errorf("from %s the strip says %s is %v x%d (%q); that lands on focus %d, want %d",
					fromName, toName, back, n, strip, a.focus, to)
			}
		}
	}
}

// With focus on notify the shown table is the one last rested on, and the
// hint must be the keys from where focus IS, not from the table.
func TestStripKeysFollowFocusOffTheTable(t *testing.T) {
	a := oneTableApp(120, 40)
	a.setFocus(focusSchedules)
	a.setFocus(focusNotify) // schedules stays shown
	if a.shownTable() != focusSchedules {
		t.Fatalf("shown = %d", a.shownTable())
	}
	strip := tableStrip(a.shownTable(), a.focus, nil)
	back, n := stripHint(t, strip, "Tasks")
	for i := 0; i < n; i++ {
		if back {
			a.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
		} else {
			a.Update(tea.KeyMsg{Type: tea.KeyTab})
		}
	}
	if a.focus != focusTasks {
		t.Errorf("strip %q: lands on %d, want tasks", strip, a.focus)
	}
}

func TestStripFitsEightyColumns(t *testing.T) {
	for _, f := range []focus{focusRunners, focusTasks, focusSchedules, focusNotify} {
		a := oneTableApp(80, 24)
		a.Update(SnapshotMsg{Runners: []protocol.RunnerInfo{{Status: protocol.RunnerStatus_Busy}}})
		a.setFocus(f)
		strip := tableStrip(a.shownTable(), a.focus, a.runnersSnapshot)
		for name, cond := range map[string]*runewidth.Condition{"narrow": {EastAsianWidth: false}, "wide": {EastAsianWidth: true}} {
			if w := cond.StringWidth(strip); w > 80 {
				t.Errorf("focus %d: strip is %d cells under %s width (> 80): %q", f, w, name, strip)
			}
		}
		if !strings.Contains(strip, "runners 1: 0 idle 1 busy") {
			t.Errorf("focus %d: summary missing: %q", f, strip)
		}
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

func TestSnapshotFillsSchedulesAndAScheduleVerbRefreshes(t *testing.T) {
	a := oneTableApp(120, 40)
	a.Update(SnapshotMsg{Schedules: schedFixRows()})
	if got := len(a.schedules.table.Rows()); got != 3 {
		t.Fatalf("%d rows after the snapshot", got)
	}
	// A failed list keeps what was there and says why, once.
	a.Update(SnapshotMsg{SchedulesErr: context.DeadlineExceeded})
	a.Update(SnapshotMsg{SchedulesErr: context.DeadlineExceeded})
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
	// n and x: distinct from every other main key (TestMainKeysAreUnique) and
	// each dispatched ONLY in the schedules scope.
	for _, k := range []string{mainKeys.ScheduleRunNow, mainKeys.ScheduleDelete} {
		for _, b := range mainKeyBindings {
			for _, bk := range b.Keys {
				if bk == k && b.Scope != scopeSchedules {
					t.Errorf("key %q also bound with scope %v", k, b.Scope)
				}
			}
		}
	}
}
