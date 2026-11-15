package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli/verb"
)

func scheduleForm(t *testing.T) BgFormModel {
	t.Helper()
	f := NewBgForm()
	f.OpenSchedule("/r")
	f.prompt.SetValue("triage")
	f.fields[bgFieldCron].SetValue("@daily")
	f.fields[bgFieldAgent].SetValue("claude")
	return f
}

func identityResolve(s string) (string, error) { return s, nil }

func hasPair(args []string, k, v string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == k && args[i+1] == v {
			return true
		}
	}
	return false
}

// `N` with the Schedules table focused opens the schedule form; in the Tasks
// table it is still the background form.
func TestNOpensScheduleFormInSchedulesAndBackgroundInTasks(t *testing.T) {
	a := New(Config{})
	a.width, a.height = 160, 40
	a.defaultRepo = "/r"
	a.setFocus(focusSchedules)
	a = press(t, a, runes("N"))
	if !a.bgform.IsOpen() || !a.bgform.IsSchedule() || !strings.Contains(a.bgform.View(), "new schedule") {
		t.Fatalf("N in Schedules: open=%v schedule=%v view:\n%s", a.bgform.IsOpen(), a.bgform.IsSchedule(), a.bgform.View())
	}
	a.bgform.Close()
	a.setFocus(focusTasks)
	a = press(t, a, runes("N"))
	if !a.bgform.IsOpen() || a.bgform.IsSchedule() || !strings.Contains(a.bgform.View(), "New background session") {
		t.Fatalf("N in Tasks: schedule=%v view:\n%s", a.bgform.IsSchedule(), a.bgform.View())
	}
}

func TestScheduleArgsNewTask(t *testing.T) {
	f := scheduleForm(t)
	f.fields[bgFieldZone].SetValue("Asia/Tokyo")
	f.fields[bgFieldOneshot].SetValue("yes")
	args, err := f.ScheduleArgs(identityResolve)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range [][2]string{{"--cron", "@daily"}, {"--repo", "/r"}, {"--agent", "claude"}, {"--tz", "Asia/Tokyo"}, {"--prompt", "triage"}, {"--deadline", "1h"}} {
		if !hasPair(args, p[0], p[1]) {
			t.Errorf("args %q lack %s %s", args, p[0], p[1])
		}
	}
	if !strings.Contains(strings.Join(args, " "), "--oneshot") {
		t.Errorf("args %q lack --oneshot", args)
	}
	if _, err := verb.ParseCmdScheduleNew(verb.TUI, args, nil); err != nil {
		t.Errorf("the shared parser refuses the form's args %q: %v", args, err)
	}
}

func TestScheduleArgsBoundTask(t *testing.T) {
	f := scheduleForm(t)
	id := strings.Repeat("ab", 16)
	f.fields[bgFieldTarget].SetValue("ab")
	args, err := f.ScheduleArgs(func(string) (string, error) { return id, nil })
	if err != nil {
		t.Fatal(err)
	}
	if !hasPair(args, "--task", id) || strings.Contains(strings.Join(args, " "), "--repo") {
		t.Errorf("args %q: want --task %s and no --repo", args, id)
	}
	if _, err := verb.ParseCmdScheduleNew(verb.TUI, args, nil); err != nil {
		t.Errorf("parse %q: %v", args, err)
	}
}

func TestScheduleArgsNeedsACron(t *testing.T) {
	f := scheduleForm(t)
	f.fields[bgFieldCron].SetValue("")
	if _, err := f.ScheduleArgs(identityResolve); err == nil || !strings.Contains(err.Error(), "cron") {
		t.Errorf("err = %v, want one naming cron", err)
	}
}

// Submitting the schedule form checks it as a schedule: an empty cron is the
// error, not the background form's prompt rule, and the form stays open.
func TestScheduleFormSubmitChecksTheSchedule(t *testing.T) {
	a := New(Config{})
	a.width, a.height = 160, 40
	a.defaultRepo = "/r"
	a.setFocus(focusSchedules)
	a = press(t, a, runes("N"), tea.KeyMsg{Type: tea.KeyCtrlJ})
	if !a.bgform.IsOpen() || !strings.Contains(a.bgform.View(), "cron: a schedule needs one") {
		t.Fatalf("open=%v view:\n%s", a.bgform.IsOpen(), a.bgform.View())
	}
}
