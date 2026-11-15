package verb

import (
	"reflect"
	"sort"
	"testing"
)

func verbByPath(t *testing.T, path ...string) VerbSpec {
	t.Helper()
	for _, v := range Verbs {
		if reflect.DeepEqual(v.Path, path) {
			return v
		}
	}
	t.Fatalf("no verb %v", path)
	return VerbSpec{}
}

func flagsByName(v VerbSpec) map[string]Flag {
	m := map[string]Flag{}
	for _, f := range v.Flags {
		m[f.Name] = f
	}
	return m
}

// TestScheduleFlagsAreSpawnFlags: every run and authority flag of `session new
// --background` is accepted by `schedule new` and `schedule edit` as the SAME
// Flag row (the spec's Verbs section: "the same Flag rows, not copies, so a
// later change to one reaches both verbs"). What a firing cannot carry is
// removed, and the few attributes in which a schedule's copy differs are named
// here, so a new difference has to be argued for in this list.
func TestScheduleFlagsAreSpawnFlags(t *testing.T) {
	spawn := flagsByName(verbByPath(t, "session", "new"))

	// Removed: they name a resume target, a terminal, a foreground run or a
	// stream (R16: --stream, because ScheduleSpec cannot carry it).
	removed := []string{"resume", "exact", "x11", "x11-display", "detach", "stream", "foreground", "background"}
	// Same name, same row except for these attributes (see scheduleFlags).
	differs := map[string]string{
		"repo":                "no HARNESS_REPO_PATH ladder: it would make --task conflict and a bare edit change the repo",
		"resume-conversation": "its help names --resume, which a schedule has not",
		"oneshot":             "PresenceField: an edit must tell --oneshot=false from not naming it",
		"rows":                "PresenceField: an edit must tell an explicit 40 from the default",
		"cols":                "PresenceField: an edit must tell an explicit 120 from the default",
		"deadline":            "PresenceField (R17)",
		"quiet":               "PresenceField (R17): a present 0 is back to the default",
		"blocked":             "PresenceField (R17): a present 0 is back to the default",
		"queue-timeout":       "PresenceField (R17): a present 0 is no queue timeout",
		"turn-end":            "PresenceField (R17) and `default` in the help",
		"runner":              "help: `any` on an edit sets the selector back to any",
	}
	norm := func(f Flag) Flag {
		f.Help, f.Resolve, f.PresenceField = "", nil, ""
		return f
	}

	for _, path := range [][]string{{"schedule", "new"}, {"schedule", "edit"}} {
		v := verbByPath(t, path...)
		got := flagsByName(v)
		for name, sf := range spawn {
			if contains(removed, name) {
				if _, has := got[name]; has {
					t.Errorf("%v takes --%s, which a firing cannot carry", path, name)
				}
				continue
			}
			g, has := got[name]
			if !has {
				t.Errorf("%v does not take --%s, a flag of session new", path, name)
				continue
			}
			if !reflect.DeepEqual(norm(sf), norm(g)) {
				t.Errorf("%v --%s is not session new's row:\n session new %+v\n schedule    %+v", path, name, norm(sf), norm(g))
			}
			if !reflect.DeepEqual(sf, g) {
				if _, ok := differs[name]; !ok {
					t.Errorf("%v --%s differs from session new's row in Help/Resolve/PresenceField and %q is not in the allowed list", path, name, name)
				}
			}
		}
		for name := range differs {
			if reflect.DeepEqual(spawn[name], got[name]) {
				t.Errorf("%v --%s is listed as different but is identical: drop it from the list", path, name)
			}
		}
		// The schedule's own flags.
		for _, own := range []string{"cron", "tz", "task"} {
			if _, has := got[own]; !has {
				t.Errorf("%v lacks --%s", path, own)
			}
		}
		if got["cron"].Required != (path[1] == "new") {
			t.Errorf("%v: --cron Required = %v", path, got["cron"].Required)
		}
		// Nothing a schedule takes is a flag nobody (not session new, not the
		// schedule's own three) declares.
		var extra []string
		own := []string{"cron", "tz", "task"}
		if path[1] == "edit" {
			own = append(own, "no-agent-args") // R19: edit-only
		} else if _, has := got["no-agent-args"]; has {
			t.Errorf("%v takes --no-agent-args, which only an edit has", path)
		}
		for name := range got {
			if _, ok := spawn[name]; !ok && !contains(own, name) {
				extra = append(extra, name)
			}
		}
		sort.Strings(extra)
		if len(extra) > 0 {
			t.Errorf("%v has flags of its own beyond cron/tz/task: %v", path, extra)
		}
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// A schedule has no --stream and no way to name a resume target: the parse
// refuses them rather than ignoring them (checklist 33).
func TestScheduleRefusesWhatAFiringCannotCarry(t *testing.T) {
	for _, flag := range []string{"--stream", "--resume=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "--foreground", "--x11"} {
		if _, err := ParseCmdScheduleNew(CLI, []string{"--cron", "@daily", "--repo", "/r", flag}, nil); err == nil {
			t.Errorf("schedule new accepted %s", flag)
		}
	}
}

// The schedule verbs are declared for the CLI and the TUI only until the WebUI
// dialog lands (ruling R2).
func TestScheduleRowsAreCLIAndTUI(t *testing.T) {
	for _, p := range []string{"new", "edit", "run", "rm", "ls", "show"} {
		v := verbByPath(t, "schedule", p)
		if v.CmdlineSurfaces != CLI|TUI {
			t.Errorf("schedule %s: CmdlineSurfaces = %v, want CLI|TUI", p, v.CmdlineSurfaces)
		}
	}
}
