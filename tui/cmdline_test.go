package tui

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
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

func TestParseFileLs(t *testing.T) {
	got, err := ParseCommand(`file ls deadbeef0011 src/`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(FileLsAction)
	if a.TaskID != "deadbeef0011" || a.RelPath != "src/" {
		t.Errorf("got %+v", a)
	}
}

func TestParseFileLsRootDefault(t *testing.T) {
	got, err := ParseCommand(`file ls deadbeef`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(FileLsAction)
	if a.TaskID != "deadbeef" || a.RelPath != "" {
		t.Errorf("got %+v", a)
	}
}

func TestParseFilePush(t *testing.T) {
	got, err := ParseCommand(`file push -r -f deadbeef ./local-dir rel/dir`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(FilePushAction)
	if a.TaskID != "deadbeef" || a.LocalSrc != "./local-dir" || a.RemoteDst != "rel/dir" {
		t.Errorf("paths: %+v", a)
	}
	if !a.Recursive || !a.Force {
		t.Errorf("flags: %+v", a)
	}
}

func TestParseFilePullSingle(t *testing.T) {
	got, err := ParseCommand(`file pull deadbeef rel/file.txt ./local.txt`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(FilePullAction)
	if a.Recursive || a.Force {
		t.Errorf("expected non-recursive non-force, got %+v", a)
	}
	if a.RemoteSrc != "rel/file.txt" || a.LocalDst != "./local.txt" {
		t.Errorf("paths: %+v", a)
	}
}

func TestParseFileDeleteRecursive(t *testing.T) {
	got, err := ParseCommand(`file delete -r -f deadbeef rel/dir`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(FileDeleteAction)
	if !a.Recursive || !a.Force {
		t.Errorf("flags: %+v", a)
	}
	if a.TaskID != "deadbeef" || a.RelPath != "rel/dir" {
		t.Errorf("paths: %+v", a)
	}
}

func TestParseFileDeleteSingle(t *testing.T) {
	got, err := ParseCommand(`file delete deadbeef rel/file.txt`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(FileDeleteAction)
	if a.Recursive || a.Force {
		t.Errorf("expected non-recursive, got %+v", a)
	}
}

func TestParseServerDialRunner(t *testing.T) {
	got, err := ParseCommand(`server dial-runner ws:192.168.3.10:8540-*`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a, ok := got.(ServerDialRunnerAction)
	if !ok {
		t.Fatalf("expected ServerDialRunnerAction, got %T", got)
	}
	if a.RunnerCID != "ws:192.168.3.10:8540-*" {
		t.Errorf("RunnerCID: got %q", a.RunnerCID)
	}
}

func TestParseServerUsageErrors(t *testing.T) {
	cases := []string{
		`server`,                            // missing sub-verb
		`server unknown`,                    // unknown sub-verb
		`server dial-runner`,                // missing CID
		`server dial-runner one two-extra`,  // too many positionals
	}
	for _, in := range cases {
		if _, err := ParseCommand(in, "/cwd"); err == nil {
			t.Errorf("input %q: expected error", in)
		}
	}
}

func TestParseFileUsageErrors(t *testing.T) {
	cases := []string{
		`file`,                             // no sub-verb
		`file unknown`,                     // unknown sub-verb
		`file ls`,                          // missing task id
		`file push deadbeef onlyone`,       // missing remote
		`file pull deadbeef onlyone`,       // missing local
		`file delete deadbeef`,             // missing rel
		`file ls deadbeef sub extra-trailing`, // too many positionals
	}
	for _, in := range cases {
		if _, err := ParseCommand(in, "/cwd"); err == nil {
			t.Errorf("input %q: expected error", in)
		}
	}
}

func TestParseServerDialRunnerWithVia(t *testing.T) {
	got, err := ParseCommand(`server dial-runner ws:192.168.3.10:8540-* --via ws:192.168.3.14:52036-51357`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a, ok := got.(ServerDialRunnerAction)
	if !ok {
		t.Fatalf("expected ServerDialRunnerAction, got %T", got)
	}
	if a.RunnerCID != "ws:192.168.3.10:8540-*" {
		t.Errorf("RunnerCID: got %q", a.RunnerCID)
	}
	if a.Via != "ws:192.168.3.14:52036-51357" {
		t.Errorf("Via: got %q", a.Via)
	}
}

func TestParseServerDialRunnerWithoutVia(t *testing.T) {
	got, err := ParseCommand(`server dial-runner ws:192.168.3.10:8540-*`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a, ok := got.(ServerDialRunnerAction)
	if !ok {
		t.Fatalf("expected ServerDialRunnerAction, got %T", got)
	}
	if a.Via != "" {
		t.Errorf("Via should be empty, got %q", a.Via)
	}
}

func TestParseNotifySimple(t *testing.T) {
	// "notify hello" — no explicit level; Level is empty (defaults to info at dispatch),
	// title = "hello", text = "".
	got, err := ParseCommand(`notify hello`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a, ok := got.(NotifyAction)
	if !ok {
		t.Fatalf("got %T, want NotifyAction", got)
	}
	if a.Level != "" {
		t.Errorf("Level=%q, want empty (defaulted)", a.Level)
	}
	if a.Title != "hello" {
		t.Errorf("Title=%q, want hello", a.Title)
	}
	if a.Text != "" {
		t.Errorf("Text=%q, want empty", a.Text)
	}
}

func TestParseNotifyWarnWithText(t *testing.T) {
	// "notify warn foo bar" — explicit warn level, title = "foo", text = "bar".
	got, err := ParseCommand(`notify warn foo bar`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a, ok := got.(NotifyAction)
	if !ok {
		t.Fatalf("got %T, want NotifyAction", got)
	}
	if a.Level != "warn" {
		t.Errorf("Level=%q, want warn", a.Level)
	}
	if a.Title != "foo" {
		t.Errorf("Title=%q, want foo", a.Title)
	}
	if a.Text != "bar" {
		t.Errorf("Text=%q, want bar", a.Text)
	}
}

func TestParseNotifyEmpty(t *testing.T) {
	// "notify" with no arguments must return an error.
	_, err := ParseCommand(`notify`, "/cwd")
	if err == nil {
		t.Fatal("expected error on empty notify")
	}
}

func TestParseNotifyLevelOnlyNoTitle(t *testing.T) {
	// "notify error" — "error" is consumed as the level, leaving no title → error.
	_, err := ParseCommand(`notify error`, "/cwd")
	if err == nil {
		t.Fatal("expected error: level consumed but no title provided")
	}
}

func TestParseCapsCommand(t *testing.T) {
	act, err := ParseCommand("caps spawn,file_read", "repo")
	if err != nil {
		t.Fatal(err)
	}
	ca, ok := act.(CapsAction)
	if !ok {
		t.Fatalf("got %T, want CapsAction", act)
	}
	if ca.Show {
		t.Fatal("with args, Show should be false")
	}
	if ca.Caps != (protocol.Capability_Spawn | protocol.Capability_FileRead) {
		t.Fatalf("caps = %#x", ca.Caps)
	}
	// no args → Show
	act, _ = ParseCommand("caps", "repo")
	if ca, _ := act.(CapsAction); !ca.Show {
		t.Fatal("no args → Show=true")
	}
	// bad name → error
	if _, err := ParseCommand("caps bogus", "repo"); err == nil {
		t.Fatal("expected error for unknown cap")
	}
}
