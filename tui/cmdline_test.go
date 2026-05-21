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

func TestParseSubmitWithClaudeArgs(t *testing.T) {
	got, err := ParseCommand(`submit --claude-arg --resume --claude-arg deadbeef "do work"`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(SubmitAction)
	if a.Prompt != "do work" {
		t.Errorf("Prompt=%q", a.Prompt)
	}
	want := []string{"--resume", "deadbeef"}
	if len(a.ExtraArgs) != len(want) {
		t.Fatalf("ExtraArgs=%v want %v", a.ExtraArgs, want)
	}
	for i := range want {
		if a.ExtraArgs[i] != want[i] {
			t.Errorf("ExtraArgs[%d]=%q want %q", i, a.ExtraArgs[i], want[i])
		}
	}
}

func TestParseInteractiveWithClaudeArgs(t *testing.T) {
	got, err := ParseCommand(`interactive --repo /r --claude-arg --add-dir --claude-arg /other`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(InteractiveAction)
	if a.Repo != "/r" {
		t.Errorf("Repo=%q", a.Repo)
	}
	want := []string{"--add-dir", "/other"}
	if len(a.ExtraArgs) != len(want) {
		t.Fatalf("ExtraArgs=%v want %v", a.ExtraArgs, want)
	}
	for i := range want {
		if a.ExtraArgs[i] != want[i] {
			t.Errorf("ExtraArgs[%d]=%q want %q", i, a.ExtraArgs[i], want[i])
		}
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
}

func TestParsePruneFlags(t *testing.T) {
	got, err := ParseCommand(`prune --before=1h`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(PruneAction)
	if a.Before != time.Hour {
		t.Errorf("Before=%v", a.Before)
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

func TestParseSessionNewNoFlags(t *testing.T) {
	got, err := ParseCommand(`session new`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(SessionNewAction)
	if a.Repo != "/cwd" {
		t.Errorf("Repo=%q want /cwd", a.Repo)
	}
	if a.Host != "" || a.Runner != "" || a.IP != "" {
		t.Errorf("expected empty selector, got Host=%q Runner=%q IP=%q", a.Host, a.Runner, a.IP)
	}
	if a.Detach {
		t.Errorf("Detach should default to false")
	}
}

func TestParseSessionNewWithHost(t *testing.T) {
	got, err := ParseCommand(`session new --host raspi`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(SessionNewAction)
	if a.Host != "raspi" {
		t.Errorf("Host=%q want raspi", a.Host)
	}
	if a.Runner != "" || a.IP != "" {
		t.Errorf("expected only Host set, got Runner=%q IP=%q", a.Runner, a.IP)
	}
}

func TestParseSessionNewWithRunner(t *testing.T) {
	hex32 := "00112233445566778899aabbccddeeff"
	got, err := ParseCommand(`session new --runner `+hex32, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(SessionNewAction)
	if a.Runner != hex32 {
		t.Errorf("Runner=%q want %s", a.Runner, hex32)
	}
}

func TestParseSessionNewWithIP(t *testing.T) {
	got, err := ParseCommand(`session new --ip 192.168.1.10`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(SessionNewAction)
	if a.IP != "192.168.1.10" {
		t.Errorf("IP=%q want 192.168.1.10", a.IP)
	}
}

func TestParseSessionNewDetachAndHost(t *testing.T) {
	got, err := ParseCommand(`session new --detach --host gmkhost-pdf2md`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(SessionNewAction)
	if !a.Detach {
		t.Errorf("Detach=false want true")
	}
	if a.Host != "gmkhost-pdf2md" {
		t.Errorf("Host=%q", a.Host)
	}
}

func TestParseSessionNewSelectorMutualExclusion(t *testing.T) {
	cases := []string{
		`session new --host A --runner deadbeef`,
		`session new --host A --ip 10.0.0.1`,
		`session new --runner deadbeef --ip 10.0.0.1`,
	}
	for _, in := range cases {
		if _, err := ParseCommand(in, "/cwd"); err == nil {
			t.Errorf("input %q: expected mutual-exclusion error", in)
		}
	}
}
