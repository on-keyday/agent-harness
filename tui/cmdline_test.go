package tui

import (
	"testing"
	"time"
)

func TestParseSubmitWithRepo(t *testing.T) {
	got, err := ParseCommand(`submit --repo /foo "long prompt with spaces"`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a, ok := got.(SubmitAction)
	if !ok {
		t.Fatalf("got %T, want SubmitAction", got)
	}
	if a.Repo != "/foo" {
		t.Errorf("Repo=%q", a.Repo)
	}
	if a.Prompt != "long prompt with spaces" {
		t.Errorf("Prompt=%q", a.Prompt)
	}
}

func TestParseSubmitDefaultRepo(t *testing.T) {
	got, err := ParseCommand(`submit hello`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(SubmitAction)
	if a.Repo != "/cwd" {
		t.Errorf("Repo=%q, want /cwd", a.Repo)
	}
	if a.Prompt != "hello" {
		t.Errorf("Prompt=%q", a.Prompt)
	}
}

func TestParseSubmitMissingPrompt(t *testing.T) {
	_, err := ParseCommand(`submit`, "/cwd")
	if err == nil {
		t.Fatal("expected error on missing prompt")
	}
}

func TestParseCancel(t *testing.T) {
	got, err := ParseCommand(`cancel ab12cd`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(CancelAction)
	if a.IDPrefix != "ab12cd" {
		t.Errorf("IDPrefix=%q", a.IDPrefix)
	}
}

func TestParseCancelMissingID(t *testing.T) {
	_, err := ParseCommand(`cancel`, "/cwd")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestParsePruneDefault(t *testing.T) {
	got, err := ParseCommand(`prune`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(PruneAction)
	if a.Before != 7*24*time.Hour {
		t.Errorf("Before=%v, want 168h", a.Before)
	}
	if a.Offline {
		t.Error("Offline=true, want false")
	}
}

func TestParsePruneFlags(t *testing.T) {
	got, err := ParseCommand(`prune --before=1h --offline`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(PruneAction)
	if a.Before != time.Hour {
		t.Errorf("Before=%v", a.Before)
	}
	if !a.Offline {
		t.Error("Offline=false")
	}
}

func TestParseClear(t *testing.T) {
	got, err := ParseCommand(`clear`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.(ClearAction); !ok {
		t.Fatalf("got %T", got)
	}
}

func TestParseQuit(t *testing.T) {
	for _, in := range []string{"quit", "exit"} {
		got, err := ParseCommand(in, "/cwd")
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := got.(QuitAction); !ok {
			t.Fatalf("input %q got %T", in, got)
		}
	}
}

func TestParseHelp(t *testing.T) {
	got, err := ParseCommand(`help`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.(HelpAction); !ok {
		t.Fatalf("got %T", got)
	}
}

func TestParseEmpty(t *testing.T) {
	got, err := ParseCommand(``, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("expected nil action on empty input, got %T", got)
	}
}

func TestParseUnknown(t *testing.T) {
	_, err := ParseCommand(`teleport`, "/cwd")
	if err == nil {
		t.Fatal("expected error")
	}
}
