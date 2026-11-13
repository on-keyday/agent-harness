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
