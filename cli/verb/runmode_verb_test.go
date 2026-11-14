package verb

import (
	"strings"
	"testing"
	"time"
)

func TestSessionNewBackgroundFlagsParse(t *testing.T) {
	act, _, err := ParseCLICommand(strings.Fields("session new --repo /r --background --agent claude --prompt triage --deadline 1h --turn-end lifecycle --quiet 2m --blocked 3m --queue-timeout 4m"), nil)
	if err != nil {
		t.Fatal(err)
	}
	a, ok := act.(SpawnAction)
	if !ok {
		t.Fatalf("action %T", act)
	}
	if !a.Background || a.Prompt != "triage" || a.Deadline != time.Hour || a.TurnEnd != "lifecycle" ||
		a.QuietFor != 2*time.Minute || a.BlockedFor != 3*time.Minute || a.QueueTimeout != 4*time.Minute {
		t.Fatalf("parsed %+v", a)
	}
	if a.Rows != 40 || a.Cols != 120 {
		t.Fatalf("default size %dx%d, want 40x120", a.Rows, a.Cols)
	}
	// --stream with --background needs no -d: background implies it.
	if _, _, err := ParseCLICommand(strings.Fields("session new --repo /r --stream --background --agent claude --prompt p --deadline 1h"), nil); err != nil {
		t.Fatalf("stream + background: %v", err)
	}
	if _, _, err := ParseCLICommand(strings.Fields("session new --repo /r --stream"), nil); err == nil {
		t.Fatal("stream without detach or background accepted")
	}
	if _, _, err := ParseCLICommand(strings.Fields("session new --repo /r --background --foreground"), nil); err == nil {
		t.Fatal("both run modes accepted")
	}
	if _, _, err := ParseCLICommand(strings.Fields("session new --repo /r --background --turn-end sometimes"), nil); err == nil {
		t.Fatal("an undefined --turn-end accepted")
	}
}

func TestSessionModeVerbParses(t *testing.T) {
	id := strings.Repeat("ab", 16)
	act, _, err := ParseCLICommand(strings.Fields("session mode "+id+" background --deadline 30m --turn-end any"), nil)
	if err != nil {
		t.Fatal(err)
	}
	a, ok := act.(SessionAction)
	if !ok || a.Sub != "mode" || a.TaskID != id || a.Mode != "background" || a.Deadline != 30*time.Minute || a.TurnEnd != "any" {
		t.Fatalf("parsed %T %+v", act, act)
	}
	if bg, err := ParseRunModeWord("foreground"); err != nil || bg {
		t.Fatalf("foreground: %v %v", bg, err)
	}
	if _, err := ParseRunModeWord("sideways"); err == nil {
		t.Fatal("an undefined mode word accepted")
	}
}

// A3 (A-D29): --oneshot needs the prompt it runs, and is a PTY session, not an
// event stream; it takes either run mode.
func TestSessionNewOneshotFlag(t *testing.T) {
	act, _, err := ParseCLICommand(strings.Fields("session new --repo /r -d --oneshot --agent claude --prompt triage"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := act.(SpawnAction); !ok || !a.Oneshot {
		t.Fatalf("parsed %+v", act)
	}
	if _, _, err := ParseCLICommand(strings.Fields("session new --repo /r --background --oneshot --agent claude --prompt p --deadline 1h"), nil); err != nil {
		t.Fatalf("background oneshot: %v", err)
	}
	if _, _, err := ParseCLICommand(strings.Fields("session new --repo /r -d --oneshot --agent claude"), nil); err == nil || !strings.Contains(err.Error(), "prompt") {
		t.Fatalf("--oneshot without --prompt: %v", err)
	}
	if _, _, err := ParseCLICommand(strings.Fields("session new --repo /r -d --oneshot --stream --agent claude --prompt p"), nil); err == nil {
		t.Fatal("--oneshot with --stream accepted")
	}
}

// A3 (A-D31): submit is gone from every surface; session new --oneshot
// replaces it.
func TestSubmitVerbIsGone(t *testing.T) {
	// An unknown verb is (nil, false, nil): not handled, not an error.
	if act, handled, err := ParseCLICommand(strings.Fields("submit --repo /r --task hi"), nil); handled || act != nil {
		t.Fatalf("submit still parses: %+v, %v", act, err)
	}
	for _, s := range Verbs {
		if len(s.Path) == 1 && s.Path[0] == "submit" {
			t.Fatal("a submit row is still declared")
		}
	}
}
