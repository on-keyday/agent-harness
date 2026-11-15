package cli

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// --- fixtures ---

var schedNow = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

func nsOf(t time.Time) uint64 { return uint64(t.UnixNano()) }

func schedID(b byte) protocol.ScheduleID {
	var id protocol.ScheduleID
	for i := range id.Id {
		id.Id[i] = b
	}
	return id
}

func schedTaskID(b byte) protocol.TaskID {
	var id protocol.TaskID
	for i := range id.Id {
		id.Id[i] = b
	}
	return id
}

func schedRow(state protocol.ScheduleTaskState) protocol.ScheduleRow {
	r := protocol.ScheduleRow{
		Id:        schedID(0xab),
		TaskState: state,
		NextAt:    nsOf(time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)), // 09:00 JST
	}
	r.SetCron([]byte("0 9 * * 1-5"))
	r.SetTz([]byte("Asia/Tokyo"))
	r.SetAgent([]byte("claude"))
	r.SetRepo([]byte("/r"))
	r.SetPromptHead([]byte("triage new issues"))
	return r
}

func histEntry(o protocol.ScheduleOutcome, at time.Time) protocol.ScheduleHistoryEntry {
	e := protocol.ScheduleHistoryEntry{Outcome: o, DueAt: nsOf(at), At: nsOf(at)}
	return e
}

// --- ScheduleRowLine ---

// Every task state reads as its own words, and no field is elided at zero:
// a schedule that never fired says last=none, not nothing (checklist 31).
func TestScheduleRowLine(t *testing.T) {
	t.Run("pending_create", func(t *testing.T) {
		got := ScheduleRowLine(schedRow(protocol.ScheduleTaskState_PendingCreate), schedNow)
		for _, want := range []string{
			strings.Repeat("ab", 16), // the full id: it is the argument of show/edit/run/rm
			"0 9 * * 1-5", "Asia/Tokyo",
			"next=2026-11-16 09:00 JST", // in the schedule's own zone
			"last=none",
			"task=created-at-first-firing",
			"claude", "/r", `prompt="triage new issues"`,
		} {
			if !strings.Contains(got, want) {
				t.Errorf("row %q lacks %q", got, want)
			}
		}
	})
	t.Run("unbound", func(t *testing.T) {
		got := ScheduleRowLine(schedRow(protocol.ScheduleTaskState_Unbound), schedNow)
		if !strings.Contains(got, "task=no-task") {
			t.Errorf("row %q lacks task=no-task", got)
		}
	})
	t.Run("bound names the task and its status", func(t *testing.T) {
		r := schedRow(protocol.ScheduleTaskState_Bound)
		r.TaskId = schedTaskID(0xcd)
		r.TaskStatus = protocol.TaskStatus_Running
		got := ScheduleRowLine(r, schedNow)
		if !strings.Contains(got, "task=cdcdcdcd:running") {
			t.Errorf("row %q lacks task=cdcdcdcd:running", got)
		}
	})
	t.Run("last outcome and age", func(t *testing.T) {
		r := schedRow(protocol.ScheduleTaskState_Bound)
		r.SetLastPresent(true)
		r.SetLast(histEntry(protocol.ScheduleOutcome_SkippedBusy, schedNow.Add(-90*time.Minute)))
		got := ScheduleRowLine(r, schedNow)
		if !strings.Contains(got, "last=skipped_busy 1h ago") {
			t.Errorf("row %q lacks last=skipped_busy 1h ago", got)
		}
	})
	t.Run("a hidden prompt is not an empty one", func(t *testing.T) {
		r := schedRow(protocol.ScheduleTaskState_Bound)
		r.SetPromptHidden(true)
		r.SetPromptHead(nil)
		got := ScheduleRowLine(r, schedNow)
		if !strings.Contains(got, "prompt=(hidden)") || strings.Contains(got, `prompt=""`) {
			t.Errorf("row %q: want prompt=(hidden)", got)
		}
	})
	t.Run("no next firing says so", func(t *testing.T) {
		r := schedRow(protocol.ScheduleTaskState_Unbound)
		r.NextAt = 0
		if got := ScheduleRowLine(r, schedNow); !strings.Contains(got, "next=none") {
			t.Errorf("row %q lacks next=none", got)
		}
	})
	t.Run("a prompt with a newline stays one line", func(t *testing.T) {
		r := schedRow(protocol.ScheduleTaskState_Bound)
		r.SetPromptHead([]byte("a\nb"))
		if got := ScheduleRowLine(r, schedNow); strings.Contains(got, "\n") {
			t.Errorf("row %q spans lines", got)
		}
	})
}

// --- WaitStats ---

// S-D10 / S-D11: only fired and run_now entries that have both queued_at and
// started_at count; a resumed_by entry never does, even with both set.
func TestWaitStatsExcludesResumedBy(t *testing.T) {
	at := schedNow
	mk := func(o protocol.ScheduleOutcome, wait time.Duration, queued bool) protocol.ScheduleHistoryEntry {
		e := histEntry(o, at)
		if queued {
			e.QueuedAt = nsOf(at)
		}
		e.StartedAt = nsOf(at.Add(wait))
		return e
	}
	h := []protocol.ScheduleHistoryEntry{
		mk(protocol.ScheduleOutcome_Fired, 10*time.Second, true),
		mk(protocol.ScheduleOutcome_RunNow, 30*time.Second, true),
		mk(protocol.ScheduleOutcome_ResumedBy, 99*time.Hour, true), // never counts
		mk(protocol.ScheduleOutcome_Fired, 5*time.Minute, false),   // no queued_at
		histEntry(protocol.ScheduleOutcome_Fired, at),              // never started
		mk(protocol.ScheduleOutcome_SkippedBusy, time.Hour, true),
	}
	n, median, max := WaitStats(h)
	if n != 2 || median != 20*time.Second || max != 30*time.Second {
		t.Fatalf("n=%d median=%v max=%v, want 2 / 20s (mean of the middle two) / 30s", n, median, max)
	}
	if n, m, x := WaitStats(nil); n != 0 || m != 0 || x != 0 {
		t.Fatalf("empty history: %d %v %v", n, m, x)
	}
	odd := []protocol.ScheduleHistoryEntry{
		mk(protocol.ScheduleOutcome_Fired, 1*time.Second, true),
		mk(protocol.ScheduleOutcome_Fired, 9*time.Second, true),
		mk(protocol.ScheduleOutcome_Fired, 3*time.Second, true),
	}
	if n, m, x := WaitStats(odd); n != 3 || m != 3*time.Second || x != 9*time.Second {
		t.Fatalf("odd: %d %v %v", n, m, x)
	}
}

// --- ScheduleDetailText ---

func schedDetail() protocol.ScheduleDetail {
	d := protocol.ScheduleDetail{Row: schedRow(protocol.ScheduleTaskState_Bound)}
	d.Row.TaskId = schedTaskID(0xcd)
	d.Row.TaskStatus = protocol.TaskStatus_Succeeded
	d.SetPrompt([]byte("triage new issues"))
	d.Conditions = protocol.BackgroundConditions{DeadlineMs: 30 * 60 * 1000, TurnEnd: protocol.TurnEndSignal_Any, QuietMs: 5 * 60 * 1000, BlockedMs: 5 * 60 * 1000}
	d.Caps = protocol.Capability_Spawn
	d.Creator = schedTaskID(0x11)
	d.CreatedAt = nsOf(schedNow.Add(-48 * time.Hour))
	d.EditedAt = nsOf(schedNow.Add(-time.Hour))
	d.EditedByKind = protocol.ClientKind_Agent
	d.EditedBy = schedTaskID(0x22)
	d.SetRecordSet(true)
	d.SetPtySet(true)
	queued := schedNow.Add(-24 * time.Hour)
	fired := histEntry(protocol.ScheduleOutcome_Fired, queued)
	fired.QueuedAt = nsOf(queued)
	fired.StartedAt = nsOf(queued.Add(42 * time.Second))
	fired.EndedAt = nsOf(queued.Add(20 * time.Minute))
	fired.EndReason = protocol.EndReason_Deadline
	resumed := histEntry(protocol.ScheduleOutcome_ResumedBy, schedNow.Add(-2*time.Hour))
	resumed.ByKind = protocol.ClientKind_Cli
	unbound := histEntry(protocol.ScheduleOutcome_SkippedRefused, schedNow.Add(-3*time.Hour))
	unbound.SetMessage([]byte("spawn limit reached"))
	d.SetHistory([]protocol.ScheduleHistoryEntry{fired, resumed, unbound})
	return d
}

func TestScheduleDetailTextShowsEverythingItHas(t *testing.T) {
	got := ScheduleDetailText(schedDetail(), schedNow)
	for _, want := range []string{
		// the zone with every time, and the absolute instant beside it
		"next: 2026-11-16 09:00 JST (2026-11-16T00:00:00Z)",
		"zone: Asia/Tokyo",
		"creator: " + strings.Repeat("11", 16),
		"edited: 2026-11-15 20:00 JST (2026-11-15T11:00:00Z) by agent " + strings.Repeat("22", 16),
		"deadline=30m0s", "turn-end=any", "quiet=5m0s", "blocked=5m0s",
		"queue-timeout=none",
		"caps=spawn", "scope=subtree", "spawn-depth=", "max-descendants=",
		"waits: n=1 median=42s max=42s",
		// history: a field that exists is never dropped; a time not set is -
		"fired", "queued=2026-11-14 21:00:00 JST", "started=2026-11-14 21:00:42 JST", "end=deadline",
		"resumed_by", "by=cli",
		"skipped_refused", "queued=-", "ended=-", "end=-", "spawn limit reached",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("detail lacks %q:\n%s", want, got)
		}
	}
}

func TestScheduleDetailTextOperatorCreatorAndNeverEdited(t *testing.T) {
	d := schedDetail()
	d.Creator = protocol.TaskID{}
	d.EditedAt, d.EditedByKind, d.EditedBy = 0, protocol.ClientKind_Unspecified, protocol.TaskID{}
	d.SetHistory(nil)
	got := ScheduleDetailText(d, schedNow)
	for _, want := range []string{"creator: operator", "edited: never", "waits: n=0", "history: none yet"} {
		if !strings.Contains(got, want) {
			t.Errorf("detail lacks %q:\n%s", want, got)
		}
	}
}

func TestScheduleDetailTextHiddenPrompt(t *testing.T) {
	d := schedDetail()
	d.Row.SetPromptHidden(true)
	d.SetPrompt(nil)
	got := ScheduleDetailText(d, schedNow)
	if !strings.Contains(got, "prompt: (hidden") {
		t.Errorf("detail does not say the prompt is hidden:\n%s", got)
	}
}

func TestScheduleDetailTextUnloadableZoneStillShowsTheInstant(t *testing.T) {
	d := schedDetail()
	d.Row.SetTz([]byte("Not/AZone"))
	got := ScheduleDetailText(d, schedNow)
	if !strings.Contains(got, "2026-11-16T00:00:00Z") || !strings.Contains(got, "Not/AZone") {
		t.Errorf("detail lost the instant or the zone name:\n%s", got)
	}
}

// --- statuses ---

// Every ScheduleStatus the wire has reads as a human message naming the status
// and carries the server's message; ok is no error. A status appended to the
// .bgn without a message here fails this test.
func TestEveryScheduleStatusHasAMessage(t *testing.T) {
	if err := scheduleStatusError("new", protocol.ScheduleStatus_Ok, nil); err != nil {
		t.Fatalf("ok is an error: %v", err)
	}
	n := 0
	for st := protocol.ScheduleStatus(1); !strings.HasPrefix(st.String(), "ScheduleStatus("); st++ {
		n++
		err := scheduleStatusError("new", st, []byte("the server said so"))
		if err == nil {
			t.Fatalf("%v: no error", st)
		}
		if !strings.Contains(err.Error(), EnumWireName(st.String())) || !strings.Contains(err.Error(), "the server said so") {
			t.Errorf("%v: %q names neither the status nor the server's message", st, err)
		}
		// No message from the server still reads: the status carries a gloss.
		bare := scheduleStatusError("new", st, nil)
		if bare == nil || len(bare.Error()) <= len("schedule new: ")+len(EnumWireName(st.String())) {
			t.Errorf("%v: no gloss when the server sent no message: %v", st, bare)
		}
	}
	if n < 21 {
		t.Fatalf("only %d statuses walked", n)
	}
	err := scheduleStatusError("run", protocol.ScheduleStatus_NotReady, nil)
	if !strings.Contains(err.Error(), "still starting") {
		t.Errorf("run on not_ready must say the server is still starting: %v", err)
	}
}

// --- the spec builder ---

func parseScheduleLine(t *testing.T, line string) verb.ScheduleSpecAction {
	t.Helper()
	args := strings.Fields(line)
	sub, rest := args[0], args[1:]
	var (
		a   verb.ScheduleSpecAction
		err error
	)
	switch sub {
	case "new":
		a, err = verb.ParseCmdScheduleNew(verb.CLI, rest, nil)
	case "edit":
		a, err = verb.ParseCmdScheduleEdit(verb.CLI, rest, nil)
	}
	if err != nil {
		t.Fatalf("%q: %v", line, err)
	}
	return a
}

const schedHexID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func specBytes(t *testing.T, s protocol.ScheduleSpec) []byte {
	t.Helper()
	b, err := s.EncodeCopy(nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// An edit naming only --cron sends only that presence bit (checklist 25, 33):
// nothing else in the spec moves, so an edit cannot overwrite what it did not
// name — not the size, not the conditions, not the authority.
func TestScheduleEditOnlyPresent(t *testing.T) {
	o, err := ScheduleSpecOptsFromAction(parseScheduleLine(t, "edit "+schedHexID+" --cron @weekly"))
	if err != nil {
		t.Fatal(err)
	}
	got := specBytes(t, buildScheduleSpec(o, 0))
	var want protocol.ScheduleSpec
	want.SetCronPresent(true)
	want.SetCron([]byte("@weekly"))
	if !bytes.Equal(got, specBytes(t, want)) {
		t.Fatalf("spec %x, want only cron_present: %x", got, specBytes(t, want))
	}
}

// A bool the operator wrote as =false is a presence, not an absence.
func TestScheduleEditBoolsNeedPresence(t *testing.T) {
	o, err := ScheduleSpecOptsFromAction(parseScheduleLine(t, "edit "+schedHexID+" --oneshot=false --resume-conversation=false --no-record --pty"))
	if err != nil {
		t.Fatal(err)
	}
	var want protocol.ScheduleSpec
	want.SetOneshotSet(true)
	want.SetOneshot(false)
	want.SetResumeConversationSet(true)
	want.SetResumeConversation(false)
	want.SetRecordSet(true)
	want.SetRecord(false)
	want.SetPtySet(true)
	want.SetPty(true)
	if got := specBytes(t, buildScheduleSpec(o, 0)); !bytes.Equal(got, specBytes(t, want)) {
		t.Fatalf("spec %x, want %x", got, specBytes(t, want))
	}
}

// The wire has ONE size block: naming one of --rows / --cols alone would
// overwrite the other with its default, so the edit is refused at the parse.
func TestScheduleEditRowsColsGoTogether(t *testing.T) {
	_, err := verb.ParseCmdScheduleEdit(verb.CLI, []string{schedHexID, "--rows", "50"}, nil)
	if err == nil {
		t.Fatal("an edit naming --rows alone was accepted")
	}
	a, err := verb.ParseCmdScheduleEdit(verb.CLI, []string{schedHexID, "--rows", "40", "--cols", "120"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	o, err := ScheduleSpecOptsFromAction(a)
	if err != nil {
		t.Fatal(err)
	}
	s := buildScheduleSpec(o, 0)
	if !s.SizePresent() || *s.InitialRows() != 40 || *s.InitialCols() != 120 {
		t.Fatalf("an explicit 40x120 (the defaults) must still be sent on an edit: %+v", s)
	}
}

// A create carries the defaults a background open does (40x120), the authority
// the operator named, the conditions, the task or repo, and the prompt's stream.
func TestScheduleNewSpec(t *testing.T) {
	a := parseScheduleLine(t, "new --cron @daily --tz Asia/Tokyo --repo /r --agent claude --prompt triage --deadline 30m --caps spawn --scope-for cancel=none --turn-end quiet --oneshot --no-pty --record --agent-arg=--x --runner "+strings.Repeat("ab", 16))
	o, err := ScheduleSpecOptsFromAction(a)
	if err != nil {
		t.Fatal(err)
	}
	s := buildScheduleSpec(o, 77)
	if !s.CronPresent() || string(*s.Cron()) != "@daily" || !s.TzPresent() || string(*s.Tz()) != "Asia/Tokyo" {
		t.Errorf("time: %+v", s)
	}
	if !s.RepoPresent() || string(*s.RepoPath()) != "/r" || s.TaskPresent() {
		t.Errorf("repo/task: %+v", s)
	}
	if !s.AgentPresent() || string(*s.AgentProfile()) != "claude" {
		t.Errorf("agent")
	}
	if !s.PromptPresent() || *s.PromptStreamId() != 77 {
		t.Errorf("the prompt rides stream 77")
	}
	if !s.RequestedCapsPresent() || *s.RequestedCaps() != protocol.Capability_Spawn {
		t.Errorf("caps")
	}
	if !s.ScopePresent() || s.Overrides() == nil || len(*s.Overrides()) != 1 {
		t.Errorf("--scope-for must reach the spec with the scope presence bit")
	}
	if !s.DeadlinePresent() || *s.DeadlineMs() != 30*60*1000 || !s.TurnEndPresent() || *s.TurnEnd() != protocol.TurnEndSignal_Quiet {
		t.Errorf("conditions: deadline/turn_end")
	}
	if s.QuietPresent() || s.BlockedPresent() || s.QueueTimeoutPresent() {
		t.Errorf("unnamed conditions are present")
	}
	if !s.OneshotSet() || !s.Oneshot() || !s.PtySet() || s.Pty() || !s.RecordSet() || !s.Record() {
		t.Errorf("run flags")
	}
	if !s.SizePresent() || *s.InitialRows() != 40 || *s.InitialCols() != 120 {
		t.Errorf("a create sends the background open's 40x120")
	}
	if !s.ExtraArgsPresent() || len(s.ExtraArgs().AsStrings()) != 1 || !s.SelectorPresent() {
		t.Errorf("extra args / selector")
	}
	// What was not named is not present (a create too: the server defaults it).
	if s.ResumeConversationSet() || s.DelegateCapsPresent() || s.DelegateScopePresent() || s.SpawnDepthPresent() || s.MaxDescendantsPresent() {
		t.Errorf("unnamed fields are present: %+v", s)
	}
}

func TestScheduleNewWithTask(t *testing.T) {
	o, err := ScheduleSpecOptsFromAction(parseScheduleLine(t, "new --cron @hourly --task "+schedHexID+" --prompt p --deadline 1m"))
	if err != nil {
		t.Fatal(err)
	}
	s := buildScheduleSpec(o, 1)
	if !s.TaskPresent() || s.TaskId().Id != [16]byte{0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa} || s.RepoPresent() {
		t.Errorf("task/repo: %+v", s)
	}
}

// --repo with --task, and neither on a create, are refused at the parse, and
// HARNESS_REPO_PATH (set in every runner-spawned task) does not fill --repo in.
func TestScheduleRepoVersusTask(t *testing.T) {
	t.Setenv("HARNESS_REPO_PATH", "/from/env")
	if _, err := verb.ParseCmdScheduleNew(verb.CLI, []string{"--cron", "@daily", "--repo", "/r", "--task", schedHexID}, nil); err == nil {
		t.Error("--repo with --task accepted")
	}
	if _, err := verb.ParseCmdScheduleNew(verb.CLI, []string{"--cron", "@daily"}, nil); err == nil {
		t.Error("neither --repo nor --task accepted on a create")
	}
	a, err := verb.ParseCmdScheduleNew(verb.CLI, []string{"--cron", "@daily", "--task", schedHexID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.Repo != "" {
		t.Errorf("HARNESS_REPO_PATH leaked into a --task create: %q", a.Repo)
	}
}

// --- ONE builder (checklist 28a) ---

// Every ScheduleSpecOpts literal outside tests lives in the builder, and every
// protocol.ScheduleSpec literal in buildScheduleSpec, so a field added to the
// spec cannot be set in one surface and missed in another (the --scope-for
// incident: seven hand-written SessionOpts literals, six of them stale).
func TestScheduleSpecOptsHasOneBuilder(t *testing.T) {
	root := ".."
	var bad []string
	for _, dir := range []string{"cli", "tui", "cmd", "webui", "runner/agentskills"} {
		_ = filepath.Walk(filepath.Join(root, dir), func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if perr != nil {
				t.Errorf("parse %s: %v", path, perr)
				return nil
			}
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				ast.Inspect(fn, func(n ast.Node) bool {
					lit, ok := n.(*ast.CompositeLit)
					if !ok {
						return true
					}
					name := ""
					switch ty := lit.Type.(type) {
					case *ast.Ident:
						name = ty.Name
					case *ast.SelectorExpr:
						name = ty.Sel.Name
					}
					switch {
					case name == "ScheduleSpecOpts" && fn.Name.Name != "ScheduleSpecOptsFromAction":
						bad = append(bad, path+":"+fn.Name.Name+" builds a ScheduleSpecOpts")
					case name == "ScheduleSpec" && fn.Name.Name != "buildScheduleSpec":
						bad = append(bad, path+":"+fn.Name.Name+" builds a protocol.ScheduleSpec")
					}
					return true
				})
			}
			return nil
		})
	}
	if len(bad) > 0 {
		t.Fatalf("schedule specs are built in one place (ScheduleSpecOptsFromAction -> buildScheduleSpec):\n%s", strings.Join(bad, "\n"))
	}
}

// The request a client sends stays inside one datagram's decode: a built
// create round-trips through the wire format (the size guard itself is the
// send path's CheckControlMessage, which this adds nothing to).
func TestScheduleCreateRequestRoundTrips(t *testing.T) {
	o, err := ScheduleSpecOptsFromAction(parseScheduleLine(t, "new --cron @daily --repo /r --agent claude --prompt triage --deadline 30m"))
	if err != nil {
		t.Fatal(err)
	}
	req := scheduleCreateRequest(o, 5)
	if req.Header.Kind != protocol.TaskControlKind_ScheduleCreate {
		t.Fatalf("kind %v", req.Header.Kind)
	}
	var back protocol.TaskControlRequest
	if err := back.DecodeExactCopy(req.MustAppend(nil)); err != nil {
		t.Fatal(err)
	}
	if back.ScheduleCreate() == nil || *back.ScheduleCreate().Spec.PromptStreamId() != 5 {
		t.Fatalf("decoded: %+v", back.ScheduleCreate())
	}
}

// --- ls ---

// A bound task's row names its schedule, and a run a schedule started reads
// from=schedule (checklist 11, 12, 31).
func TestLsShowsSchedule(t *testing.T) {
	lr := &protocol.ListResultBody{}
	var bound protocol.TaskInfo
	bound.Id.Id = [16]byte{1}
	bound.Status = protocol.TaskStatus_Running
	bound.OriginKind = protocol.ClientKind_Schedule
	bound.SetSchedulePresent(true)
	bound.SetScheduleId(schedID(0xab))
	var plain protocol.TaskInfo
	plain.Id.Id = [16]byte{2}
	plain.Status = protocol.TaskStatus_Running
	plain.OriginKind = protocol.ClientKind_Cli
	lr.Tasks = []protocol.TaskInfo{bound, plain}

	var text bytes.Buffer
	renderList(lr, &text)
	var boundLine, plainLine string
	for _, l := range strings.Split(text.String(), "\n") {
		switch {
		case strings.Contains(l, strings.Repeat("00", 15)) && strings.HasPrefix(strings.TrimSpace(l), "01"):
			boundLine = l
		case strings.HasPrefix(strings.TrimSpace(l), "02"):
			plainLine = l
		}
	}
	if !strings.Contains(boundLine, "schedule=abababab") || !strings.Contains(boundLine, "from=schedule") {
		t.Errorf("bound row %q lacks schedule=abababab / from=schedule", boundLine)
	}
	if strings.Contains(plainLine, "schedule=") {
		t.Errorf("an unbound row names a schedule: %q", plainLine)
	}

	var js bytes.Buffer
	renderListJSON(lr, &js)
	var doc struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal(js.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if got := doc.Tasks[0]["schedule_id"]; got != strings.Repeat("ab", 16) {
		t.Errorf("bound schedule_id = %v, want the full id", got)
	}
	if _, has := doc.Tasks[1]["schedule_id"]; has {
		t.Errorf("an unbound task carries schedule_id: %v", doc.Tasks[1]["schedule_id"])
	}
	if doc.Tasks[0]["from"] != "schedule" {
		t.Errorf("from = %v", doc.Tasks[0]["from"])
	}
}

// --- JSON forms ---

func TestScheduleJSONKeepsEveryField(t *testing.T) {
	var buf bytes.Buffer
	if err := writeScheduleListJSON(&buf, []protocol.ScheduleRow{schedRow(protocol.ScheduleTaskState_PendingCreate)}, schedNow); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Schedules []map[string]any `json:"schedules"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("%v: %s", err, buf.String())
	}
	r := doc.Schedules[0]
	for _, k := range []string{"id", "cron", "tz", "task_state", "task_id", "task_status", "next_at", "next_local", "last", "agent", "repo", "prompt_head", "prompt_hidden"} {
		if _, ok := r[k]; !ok {
			t.Errorf("row JSON lacks %q: %v", k, r)
		}
	}
	if r["task_state"] != "pending_create" || r["last"] != nil {
		t.Errorf("task_state %v last %v", r["task_state"], r["last"])
	}

	buf.Reset()
	if err := writeScheduleDetailJSON(&buf, schedDetail(), schedNow); err != nil {
		t.Fatal(err)
	}
	var one map[string]any
	if err := json.Unmarshal(buf.Bytes(), &one); err != nil {
		t.Fatalf("%v: %s", err, buf.String())
	}
	for _, k := range []string{"id", "caps", "scope", "delegate_caps", "delegate_scope", "spawn_depth", "max_descendants", "creator", "created_at", "edited_at", "edited_by_kind", "edited_by", "conditions", "history", "wait_stats", "prompt", "extra_args", "selector", "resume_conversation", "oneshot", "record", "pty"} {
		if _, ok := one[k]; !ok {
			t.Errorf("detail JSON lacks %q: %s", k, buf.String())
		}
	}
	ws := one["wait_stats"].(map[string]any)
	if ws["n"] != float64(1) || ws["median_ms"] != float64(42000) || ws["max_ms"] != float64(42000) {
		t.Errorf("wait_stats %v", ws)
	}
}

// A watch line for a task bound to a schedule names it (the carry from Task 4:
// surfaces apply schedule_present / schedule_id from task events, including the
// task_schedule kind that announces a binding change).
func TestWatchLineNamesTheSchedule(t *testing.T) {
	ev := &protocol.TaskStatusEvent{Kind: protocol.StatusEventKind_TaskSchedule}
	ev.TaskId.Id = [16]byte{1, 2, 3, 4, 5, 6}
	if got := taskEventLine(ev); strings.Contains(got, "schedule=") {
		t.Errorf("an unbound event names a schedule: %q", got)
	}
	ev.SetSchedulePresent(true)
	ev.SetScheduleId(schedID(0xab))
	if got := taskEventLine(ev); !strings.Contains(got, "schedule=abababab") || !strings.Contains(got, "kind=TaskSchedule") {
		t.Errorf("bound event line %q", got)
	}
}

// --- R17: conditions are per-field presence, a present zero is an answer ---

func editSpec(t *testing.T, args string) protocol.ScheduleSpec {
	t.Helper()
	o, err := ScheduleSpecOptsFromAction(parseScheduleLine(t, "edit "+schedHexID+" "+args))
	if err != nil {
		t.Fatal(err)
	}
	return buildScheduleSpec(o, 0)
}

// The probe cases of the review: `--queue-timeout 0` used to be dropped (a zero
// duration read as "not named"), and with --deadline it kept the stored value.
func TestScheduleEditExplicitZeroConditions(t *testing.T) {
	var want protocol.ScheduleSpec
	want.SetQueueTimeoutPresent(true)
	want.SetQueueTimeoutMs(0)
	if got := editSpec(t, "--queue-timeout 0"); !bytes.Equal(specBytes(t, got), specBytes(t, want)) {
		t.Fatalf("--queue-timeout 0: %x, want only queue_timeout_present=0: %x", specBytes(t, got), specBytes(t, want))
	}
	want = protocol.ScheduleSpec{}
	want.SetDeadlinePresent(true)
	want.SetDeadlineMs(3600000)
	want.SetQueueTimeoutPresent(true)
	want.SetQueueTimeoutMs(0)
	if got := editSpec(t, "--deadline 1h --queue-timeout 0"); !bytes.Equal(specBytes(t, got), specBytes(t, want)) {
		t.Fatalf("--deadline 1h --queue-timeout 0: %x, want %x", specBytes(t, got), specBytes(t, want))
	}
	want = protocol.ScheduleSpec{}
	want.SetQuietPresent(true)
	want.SetQuietMs(0)
	want.SetBlockedPresent(true)
	want.SetBlockedMs(0)
	if got := editSpec(t, "--quiet 0 --blocked 0"); !bytes.Equal(specBytes(t, got), specBytes(t, want)) {
		t.Fatalf("--quiet 0 --blocked 0: %x, want %x", specBytes(t, got), specBytes(t, want))
	}
	// Naming only --quiet names only quiet.
	want = protocol.ScheduleSpec{}
	want.SetQuietPresent(true)
	want.SetQuietMs(180000)
	if got := editSpec(t, "--quiet 3m"); !bytes.Equal(specBytes(t, got), specBytes(t, want)) {
		t.Fatalf("--quiet 3m: %x, want %x", specBytes(t, got), specBytes(t, want))
	}
}

func TestScheduleEditTurnEndDefault(t *testing.T) {
	for _, arg := range []string{"--turn-end default", "--turn-end="} {
		var want protocol.ScheduleSpec
		want.SetTurnEndPresent(true)
		want.SetTurnEnd(protocol.TurnEndSignal_Unset)
		if got := editSpec(t, arg); !bytes.Equal(specBytes(t, got), specBytes(t, want)) {
			t.Errorf("%s: %x, want turn_end present unset: %x", arg, specBytes(t, got), specBytes(t, want))
		}
	}
	if _, err := verb.ParseCmdScheduleEdit(verb.CLI, []string{schedHexID, "--turn-end", "sometimes"}, nil); err == nil {
		t.Error("a bad --turn-end was accepted")
	}
}

// --- R19: edit-only spellings ---

func TestScheduleEditRunnerAnyAndNoAgentArgs(t *testing.T) {
	s := editSpec(t, "--runner any")
	if !s.SelectorPresent() || s.Selector().Kind != protocol.RunnerSelectorKind_Any {
		t.Fatalf("--runner any: %+v", s)
	}
	s = editSpec(t, "--no-agent-args")
	if !s.ExtraArgsPresent() || len(s.ExtraArgs().AsStrings()) != 0 {
		t.Fatalf("--no-agent-args must be present and empty: %+v", s)
	}
	// Not named: absent (the stored list and selector stay).
	s = editSpec(t, "--cron @daily")
	if s.ExtraArgsPresent() || s.SelectorPresent() {
		t.Fatalf("unnamed extra args / selector are present: %+v", s)
	}
	if _, err := verb.ParseCmdScheduleEdit(verb.CLI, []string{schedHexID, "--no-agent-args", "--agent-arg=--x"}, nil); err == nil {
		t.Error("--no-agent-args with --agent-arg accepted")
	}
	if _, err := verb.ParseCmdScheduleNew(verb.CLI, []string{"--cron", "@daily", "--repo", "/r", "--no-agent-args"}, nil); err == nil {
		t.Error("schedule new accepted the edit-only --no-agent-args")
	}
}

// --- R18: one embedded zone source ---

func TestScheduleLocationLoadsWithoutAHostDatabase(t *testing.T) {
	// The package embeds the zone database (time/tzdata), so a name loads on a
	// host with none. On Linux Go tries ZONEINFO and the system directories
	// before the embedded copy, so pointing ZONEINFO at an empty directory
	// cannot by itself prove the embedded one is used: the import is asserted
	// separately below, and this load must succeed either way.
	t.Setenv("ZONEINFO", t.TempDir())
	loc := scheduleLocation("Asia/Tokyo")
	if loc.String() != "Asia/Tokyo" {
		t.Fatalf("Asia/Tokyo loaded as %q (UTC fallback?)", loc)
	}
}

func TestCliEmbedsTheZoneDatabase(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "schedule.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, im := range f.Imports {
		if im.Path.Value == `"time/tzdata"` {
			return
		}
	}
	t.Fatal(`cli/schedule.go no longer imports "time/tzdata": a client host without a zone database would render schedules in UTC`)
}

// --- history instants, JSON spelling of an absent next ---

func TestScheduleHistoryCarriesAbsoluteInstants(t *testing.T) {
	got := ScheduleDetailText(schedDetail(), schedNow)
	for _, want := range []string{
		"queued=2026-11-14 21:00:00 JST (2026-11-14T12:00:00Z)",
		"started=2026-11-14 21:00:42 JST (2026-11-14T12:00:42Z)",
		"ended=2026-11-14 21:20:00 JST (2026-11-14T12:20:00Z)",
		"due=2026-11-14 21:00:00 JST (2026-11-14T12:00:00Z)",
		"  2026-11-14 21:00:00 JST (2026-11-14T12:00:00Z)  fired", // the entry's own time
		"queued=-",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("history lacks %q:\n%s", want, got)
		}
	}
}

func TestScheduleJSONAbsentNextSpelledLikeTheRow(t *testing.T) {
	r := schedRow(protocol.ScheduleTaskState_Unbound)
	r.NextAt = 0
	var buf bytes.Buffer
	if err := writeScheduleListJSON(&buf, []protocol.ScheduleRow{r}, schedNow); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Schedules []map[string]any `json:"schedules"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if got := doc.Schedules[0]["next_local"]; got != "none" {
		t.Errorf("next_local = %v, want \"none\" as in the text row", got)
	}
}

// The WebUI dialog arranges ScheduleDetailParts itself; the text form is the
// parts joined, so the two cannot word the same schedule differently.
func TestScheduleDetailPartsAreTheTextCutUp(t *testing.T) {
	d := schedDetail()
	p := ScheduleDetailParts(d, schedNow)
	var b strings.Builder
	for _, l := range p.Head {
		b.WriteString(l + "\n")
	}
	b.WriteString(p.Waits + "\n" + p.HistoryTitle + "\n")
	for _, l := range p.History {
		b.WriteString("  " + l + "\n")
	}
	if got := ScheduleDetailText(d, schedNow); got != b.String() {
		t.Errorf("text != parts joined:\n%s\n---\n%s", got, b.String())
	}
	if len(p.History) != 3 || !strings.HasPrefix(p.HistoryTitle, "history (3, oldest first)") {
		t.Errorf("history title %q, %d entries", p.HistoryTitle, len(p.History))
	}
	if !strings.HasPrefix(p.Waits, "waits: n=1 median=42s") {
		t.Errorf("waits %q", p.Waits)
	}
	empty := ScheduleDetailParts(protocol.ScheduleDetail{Row: schedRow(protocol.ScheduleTaskState_Unbound)}, schedNow)
	if empty.HistoryTitle != "history: none yet" || len(empty.History) != 0 {
		t.Errorf("empty history: %q %v", empty.HistoryTitle, empty.History)
	}
}

// TaskScheduleHex: the whole id when bound, "" when not, gated on existence.
func TestTaskScheduleHex(t *testing.T) {
	var bound protocol.TaskInfo
	bound.SetSchedulePresent(true)
	bound.SetScheduleId(schedID(0xab))
	if got := TaskScheduleHex(&bound); got != strings.Repeat("ab", 16) {
		t.Errorf("bound = %q", got)
	}
	if got := TaskScheduleHex(&protocol.TaskInfo{}); got != "" {
		t.Errorf("unbound = %q", got)
	}
}

// A negative duration must be refused: the later ms() clamp would turn -5m
// into "no queue timeout" / "the default", silently.
func TestScheduleRefusesNegativeDurations(t *testing.T) {
	for _, flag := range []string{"--deadline", "--quiet", "--blocked", "--queue-timeout"} {
		for _, sub := range []string{"new --cron @hourly --task " + schedHexID + " --prompt p", "edit " + schedHexID} {
			line := sub + " " + flag + "=-5m"
			a := parseScheduleLine(t, line)
			if _, err := ScheduleSpecOptsFromAction(a); err == nil || !strings.Contains(err.Error(), flag) {
				t.Errorf("%q: err %v, want a refusal naming %s", line, err, flag)
			}
		}
	}
}

// `edit --rows 50` alone used to be dropped silently. The verb parser refuses
// it; the shared builder refuses it too, for an action built another way.
func TestScheduleEditRefusesRowsWithoutCols(t *testing.T) {
	a := parseScheduleLine(t, "edit "+schedHexID+" --rows 50 --cols 100")
	if o, err := ScheduleSpecOptsFromAction(a); err != nil || !o.SizeSet {
		t.Fatalf("both named: %v %+v", err, o)
	}
	for _, drop := range []string{"cols", "rows"} {
		b := a
		if drop == "cols" {
			b.ColsPresent = false
		} else {
			b.RowsPresent = false
		}
		if _, err := ScheduleSpecOptsFromAction(b); err == nil || !strings.Contains(err.Error(), "--rows") || !strings.Contains(err.Error(), "--cols") {
			t.Errorf("without %s: err %v, want a refusal naming both", drop, err)
		}
	}
}
