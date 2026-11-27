package cli

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func formDetail() protocol.ScheduleDetail {
	d := protocol.ScheduleDetail{Kind: protocol.TaskKind_Oneshot, Caps: protocol.Capability_Spawn}
	d.Row.Cron = []byte("0 9 * * *")
	d.Row.Tz = []byte("Asia/Tokyo")
	d.Row.Agent = []byte("claude")
	d.Row.Repo = []byte("/r")
	d.SetPrompt([]byte("triage"))
	d.Conditions = protocol.BackgroundConditions{DeadlineMs: uint64((30 * time.Minute).Milliseconds()), TurnEnd: protocol.TurnEndSignal_Any}
	d.SetRecordSet(true)
	d.SetRecord(true)
	d.SetResumeConversation(true)
	d.ExtraArgs = protocol.AgentArgsFromStrings([]string{"--model", "x"})
	return d
}

// Opening a schedule's form and saving it untouched sends nothing but the id:
// an edit names only what changed.
func TestScheduleEditArgsOfAnUntouchedForm(t *testing.T) {
	f := ScheduleFormOf(formDetail())
	if f.Cron != "0 9 * * *" || f.Kind != "oneshot" || f.Deadline != "30m0s" || f.Record != "on" || !f.ResumeConversation ||
		!reflect.DeepEqual(f.AgentArgs, []string{"--model", "x"}) || f.Prompt != "triage" || f.Caps != "spawn" {
		t.Fatalf("form: %+v", f)
	}
	args, err := ScheduleEditArgs("ab", f, f)
	if err != nil || !reflect.DeepEqual(args, []string{"ab"}) {
		t.Fatalf("untouched: %q %v", args, err)
	}
}

// What is turned off or emptied is said explicitly — omitting it would keep it
// — and every line parses as `schedule edit`.
func TestScheduleEditArgsSaysWhatWasTurnedOff(t *testing.T) {
	id := strings.Repeat("ab", 16)
	was := ScheduleFormOf(formDetail())
	now := was
	now.Kind = "interactive"
	now.Deadline = ""
	now.ResumeConversation = false
	now.AgentArgs = nil
	now.Cron = "@daily"
	args, err := ScheduleEditArgs(id, was, now)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(args, " ")
	for _, want := range []string{"--cron @daily", "--oneshot=false", "--deadline 0", "--resume-conversation=false", "--no-agent-args"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q lacks %q", got, want)
		}
	}
	v, err := verb.ParseCmdScheduleEdit(verb.CLI, args, nil)
	if err != nil {
		t.Fatalf("does not parse: %v", err)
	}
	o, err := ScheduleSpecOptsFromAction(v)
	if err != nil {
		t.Fatal(err)
	}
	if o.Kind == nil || *o.Kind != protocol.TaskKind_Interactive || o.ResumeConversation == nil || *o.ResumeConversation ||
		o.ExtraArgs == nil || len(o.ExtraArgs) != 0 {
		t.Fatalf("edit options: kind=%v conv=%v args=%v", o.Kind, o.ResumeConversation, o.ExtraArgs)
	}
}

// The authority travels whole when any of its scope parts changed: --scope
// replaces the overrides, so naming one --scope-for alone would drop the rest.
func TestScheduleEditArgsSendsTheScopeWhole(t *testing.T) {
	was := ScheduleFormOf(formDetail())
	was.Scope = "subtree"
	was.ScopeFor = []string{"cancel=none"}
	now := was
	now.ScopeFor = []string{"cancel=none", "exec_run=global"}
	args, err := ScheduleEditArgs("ab", was, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(args, " "); !strings.Contains(got, "--scope subtree --scope-for cancel=none --scope-for exec_run=global") {
		t.Fatalf("scope not sent whole: %q", got)
	}
}

// What a schedule cannot hold is refused, not dropped: a stored schedule's
// record and pty are on or off (create resolves the default), so there is no
// default to go back to; a hidden prompt cannot be edited blind.
func TestScheduleEditArgsRefusesWhatItCannotSay(t *testing.T) {
	was := ScheduleFormOf(formDetail())
	now := was
	now.Record = ""
	if _, err := ScheduleEditArgs("ab", was, now); err == nil || !strings.Contains(err.Error(), "record: a schedule holds on or off") {
		t.Fatalf("record back to default: %v", err)
	}
	was.PromptHidden, was.Prompt = true, ""
	now = was
	now.Prompt = "new"
	if _, err := ScheduleEditArgs("ab", was, now); err != nil {
		t.Fatalf("a new prompt over a hidden one is a plain set: %v", err)
	}
}
