package tui

import (
	"strings"
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

func TestParseSubmitResumeConversation(t *testing.T) {
	got, err := ParseCommand(`submit --resume abc123 --resume-conversation "do work"`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(SubmitAction)
	if a.ResumeTaskID != "abc123" {
		t.Errorf("ResumeTaskID=%q want abc123", a.ResumeTaskID)
	}
	if !a.ResumeConversation {
		t.Fatal("ResumeConversation=false want true")
	}
	if a.Prompt != "do work" {
		t.Errorf("Prompt=%q want do work", a.Prompt)
	}
}

func TestParseSubmitWithAgent(t *testing.T) {
	got, err := ParseCommand(`submit --agent codex "do work"`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(SubmitAction)
	if a.AgentProfile != "codex" {
		t.Errorf("AgentProfile=%q want codex", a.AgentProfile)
	}
}

func TestParseSubmitDefaultAgentEmpty(t *testing.T) {
	got, err := ParseCommand(`submit hello`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(SubmitAction)
	if a.AgentProfile != "" {
		t.Errorf("AgentProfile=%q want empty (runner default)", a.AgentProfile)
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

func TestParseInteractiveResumeConversation(t *testing.T) {
	got, err := ParseCommand(`interactive --resume abc123 --resume-conversation`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(InteractiveAction)
	if a.ResumeTaskID != "abc123" {
		t.Errorf("ResumeTaskID=%q want abc123", a.ResumeTaskID)
	}
	if !a.ResumeConversation {
		t.Fatal("ResumeConversation=false want true")
	}
}

func TestParseInteractiveWithAgent(t *testing.T) {
	got, err := ParseCommand(`interactive --agent codex`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(InteractiveAction)
	if a.AgentProfile != "codex" {
		t.Errorf("AgentProfile=%q want codex", a.AgentProfile)
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

func TestParsePruneByID(t *testing.T) {
	id1 := "0123456789abcdef0123456789abcdef"
	id2 := "fedcba9876543210fedcba9876543210"
	got, err := ParseCommand(`prune --force `+id1+` `+id2, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(PruneAction)
	if !a.Force {
		t.Errorf("Force=false, want true")
	}
	if len(a.TaskIDs) != 2 || a.TaskIDs[0] != id1 || a.TaskIDs[1] != id2 {
		t.Errorf("TaskIDs=%v, want [%s %s]", a.TaskIDs, id1, id2)
	}
	// --before must still carry its default even in id mode (dispatch ignores it).
	if a.Before != 7*24*time.Hour {
		t.Errorf("Before=%v, want 168h", a.Before)
	}
}

func TestParsePruneShortForceFlag(t *testing.T) {
	got, err := ParseCommand(`prune -f deadbeefdeadbeefdeadbeefdeadbeef`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(PruneAction)
	if !a.Force || len(a.TaskIDs) != 1 {
		t.Errorf("got Force=%v TaskIDs=%v, want Force=true, 1 id", a.Force, a.TaskIDs)
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

func TestParseSessionNewWithAgent(t *testing.T) {
	got, err := ParseCommand(`session new --agent codex`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(SessionNewAction)
	if a.AgentProfile != "codex" {
		t.Errorf("AgentProfile=%q want codex", a.AgentProfile)
	}
}

func TestParseSessionNewResumeConversation(t *testing.T) {
	got, err := ParseCommand(`session new --resume abc123 --resume-conversation`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(SessionNewAction)
	if a.ResumeTaskID != "abc123" {
		t.Errorf("ResumeTaskID=%q want abc123", a.ResumeTaskID)
	}
	if !a.ResumeConversation {
		t.Fatal("ResumeConversation=false want true")
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
		`server`,                           // missing sub-verb
		`server unknown`,                   // unknown sub-verb
		`server dial-runner`,               // missing CID
		`server dial-runner one two-extra`, // too many positionals
	}
	for _, in := range cases {
		if _, err := ParseCommand(in, "/cwd"); err == nil {
			t.Errorf("input %q: expected error", in)
		}
	}
}

func TestParseFileUsageErrors(t *testing.T) {
	cases := []string{
		`file`,                                // no sub-verb
		`file unknown`,                        // unknown sub-verb
		`file ls`,                             // missing task id
		`file push deadbeef onlyone`,          // missing remote
		`file pull deadbeef onlyone`,          // missing local
		`file delete deadbeef`,                // missing rel
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

func TestParseCapsOnResumeRemoved(t *testing.T) {
	// The toggle is gone; the parser must say so rather than fall through to
	// ParseCaps and report "--on-resume" as an unknown capability name.
	_, err := ParseCommand("caps --on-resume on", "r")
	if err == nil {
		t.Fatal("caps --on-resume should be rejected")
	}
	if !strings.Contains(err.Error(), "--caps") {
		t.Errorf("error should point at the replacement flag, got: %v", err)
	}

	// Plain `caps` still shows the current default.
	act, err := ParseCommand("caps", "r")
	if err != nil {
		t.Fatalf("caps plain: unexpected error: %v", err)
	}
	ca, ok := act.(CapsAction)
	if !ok {
		t.Fatalf("got %T, want CapsAction", act)
	}
	if !ca.Show {
		t.Fatal("caps plain: Show should be true")
	}
}

// TestParseSpawnCapsFlag covers the per-invocation --caps on the three spawn
// commands: absent leaves Caps nil (fall back to the session default), and an
// explicit "none" must survive as a real value rather than read as absence.
func TestParseSpawnCapsFlag(t *testing.T) {
	act, err := ParseCommand("submit hello", "r")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if c := act.(SubmitAction).Caps; c != nil {
		t.Errorf("submit without --caps: Caps = %v, want nil", *c)
	}

	act, err = ParseCommand("submit --caps all,-spawn hello", "r")
	if err != nil {
		t.Fatalf("submit --caps: %v", err)
	}
	c := act.(SubmitAction).Caps
	if c == nil {
		t.Fatal("submit --caps: Caps is nil")
	}
	if *c&protocol.Capability_Spawn != 0 {
		t.Errorf("submit --caps all,-spawn still grants spawn: %#x", *c)
	}

	act, err = ParseCommand("session new --caps none", "r")
	if err != nil {
		t.Fatalf("session new --caps none: %v", err)
	}
	sc := act.(SessionNewAction).Caps
	if sc == nil {
		t.Fatal("session new --caps none: Caps is nil — none must be distinguishable from unset")
	}
	if *sc != protocol.Capability_None {
		t.Errorf("session new --caps none = %#x, want %#x", *sc, protocol.Capability_None)
	}

	act, err = ParseCommand("interactive --caps exec_attach", "r")
	if err != nil {
		t.Fatalf("interactive --caps: %v", err)
	}
	ic := act.(InteractiveAction).Caps
	if ic == nil || *ic != protocol.Capability_ExecAttach {
		t.Errorf("interactive --caps exec_attach = %v", ic)
	}

	// A bad mask must fail the whole command, not spawn with a default.
	if _, err := ParseCommand("submit --caps bogus hello", "r"); err == nil {
		t.Error("expected an error for an unknown capability name")
	}
}

// TestParseRefresh verifies `refresh` and its `sync` alias parse to a
// RefreshAction (force full snapshot re-sync).
func TestParseRefresh(t *testing.T) {
	for _, in := range []string{"refresh", "sync"} {
		act, err := ParseCommand(in, "")
		if err != nil {
			t.Fatalf("ParseCommand(%q): %v", in, err)
		}
		if _, ok := act.(RefreshAction); !ok {
			t.Errorf("ParseCommand(%q) = %T, want RefreshAction", in, act)
		}
	}
}

// TestParseSessionAwaitIdle covers the session await-idle sub-verb: default
// reply sink, --notify / --topic routing, mutual exclusion, and id required.
func TestParseSessionAwaitIdle(t *testing.T) {
	act, err := ParseCommand("session await-idle abc123", "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ai, ok := act.(SessionAwaitIdleAction)
	if !ok {
		t.Fatalf("got %T, want SessionAwaitIdleAction", act)
	}
	if ai.IDPrefix != "abc123" || ai.Notify || ai.Topic != "" || ai.ThresholdMs != 0 {
		t.Errorf("unexpected defaults: %+v", ai)
	}

	act, err = ParseCommand("session await-idle --notify --threshold-ms 5000 abc123", "")
	if err != nil {
		t.Fatalf("parse notify: %v", err)
	}
	ai = act.(SessionAwaitIdleAction)
	if !ai.Notify || ai.ThresholdMs != 5000 {
		t.Errorf("notify/threshold not parsed: %+v", ai)
	}

	act, err = ParseCommand("session await-idle --topic chat.me abc123", "")
	if err != nil {
		t.Fatalf("parse topic: %v", err)
	}
	if ai = act.(SessionAwaitIdleAction); ai.Topic != "chat.me" {
		t.Errorf("topic not parsed: %+v", ai)
	}

	if _, err = ParseCommand("session await-idle --notify --topic t abc", ""); err == nil {
		t.Error("want error for --notify with --topic")
	}
	if _, err = ParseCommand("session await-idle", ""); err == nil {
		t.Error("want error for missing task id")
	}
}

func TestParseFilePushParents(t *testing.T) {
	got, err := ParseCommand(`file push -p deadbeef ./f rel/dir/f`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(FilePushAction)
	if !a.Parents || a.Force || a.Recursive {
		t.Errorf("flags = %+v want Parents only", a)
	}
}

func TestParseFileMkdir(t *testing.T) {
	got, err := ParseCommand(`file mkdir -p deadbeef rel/new/dir`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(FileMkdirAction)
	if a.TaskID != "deadbeef" || a.RelPath != "rel/new/dir" || !a.Parents {
		t.Errorf("parsed = %+v", a)
	}
	if _, err := ParseCommand(`file mkdir deadbeef`, "/cwd"); err == nil {
		t.Error("missing rel arg accepted")
	}
}

func TestParseForward(t *testing.T) {
	act, err := ParseCommand("forward kill 12", "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	kill, ok := act.(ForwardKillAction)
	if !ok || kill.ForwardID != 12 {
		t.Fatalf("got %#v, want ForwardKillAction{12}", act)
	}
	if _, err := ParseCommand("forward kill", ""); err == nil {
		t.Error("forward kill with no id should be a usage error")
	}
	if _, err := ParseCommand("forward ls", ""); err != nil {
		t.Errorf("forward ls: %v", err)
	}
	if _, err := ParseCommand("forward kill notanumber", ""); err == nil {
		t.Error("forward kill with a non-numeric id should be a usage error")
	}
	if _, err := ParseCommand("forward bogus", ""); err == nil {
		t.Error("forward with an unknown sub-verb should error")
	}
	if _, err := ParseCommand("forward", ""); err == nil {
		t.Error("forward with no sub-verb should error")
	}
}

func TestParseFileEdit(t *testing.T) {
	act, err := parseFile([]string{"edit", "abc123", "notes.txt"})
	if err != nil {
		t.Fatalf("parseFile: %v", err)
	}
	e, ok := act.(FileEditAction)
	if !ok {
		t.Fatalf("got %T, want FileEditAction", act)
	}
	if e.TaskID != "abc123" || e.RelPath != "notes.txt" {
		t.Errorf("act=%+v, want abc123 / notes.txt", e)
	}
}

func TestParseFileEditWrongArity(t *testing.T) {
	if _, err := parseFile([]string{"edit", "abc123"}); err == nil {
		t.Error("parseFile accepted `file edit` with one argument")
	}
}

func TestParseFileNew(t *testing.T) {
	act, err := parseFile([]string{"new", "abc123", "sub/notes.txt"})
	if err != nil {
		t.Fatalf("parseFile: %v", err)
	}
	n, ok := act.(FileNewAction)
	if !ok {
		t.Fatalf("got %T, want FileNewAction", act)
	}
	if n.RelPath != "sub/notes.txt" {
		t.Errorf("act=%+v, want sub/notes.txt", n)
	}
}

func parseGitCmd(t *testing.T, line string) GitAction {
	t.Helper()
	got, err := ParseCommand(line, "/cwd")
	if err != nil {
		t.Fatalf("ParseCommand(%q): %v", line, err)
	}
	a, ok := got.(GitAction)
	if !ok {
		t.Fatalf("ParseCommand(%q) returned %T", line, got)
	}
	return a
}

func TestParseGitDiffNoRevs(t *testing.T) {
	a := parseGitCmd(t, "git cafe1234 diff")
	if a.TaskID != "cafe1234" || a.Sub != "diff" {
		t.Fatalf("action = %+v", a)
	}
	if a.BaseRev != "" || a.TargetRev != "" {
		t.Fatalf("no positionals means no revisions, got %+v", a)
	}
}

func TestParseGitDiffOneRev(t *testing.T) {
	a := parseGitCmd(t, "git cafe1234 diff HEAD~3")
	if a.BaseRev != "HEAD~3" || a.TargetRev != "" {
		t.Fatalf("action = %+v", a)
	}
}

func TestParseGitDiffTwoRevs(t *testing.T) {
	a := parseGitCmd(t, "git cafe1234 diff aaa bbb")
	if a.BaseRev != "aaa" || a.TargetRev != "bbb" {
		t.Fatalf("action = %+v", a)
	}
}

func TestParseGitDiffStaged(t *testing.T) {
	a := parseGitCmd(t, "git cafe1234 diff --staged HEAD")
	if !a.Staged || a.BaseRev != "HEAD" {
		t.Fatalf("action = %+v", a)
	}
}

// The flag must work on either side of the positionals, matching the CLI —
// Go's flag package stops at the first non-flag token without the permuted
// parse, and would drop it silently.
func TestParseGitDiffStagedAfterPositional(t *testing.T) {
	a := parseGitCmd(t, "git cafe1234 diff HEAD --staged")
	if !a.Staged || a.BaseRev != "HEAD" {
		t.Fatalf("action = %+v", a)
	}
}

func TestParseGitDiffStagedWithTwoRevsRejected(t *testing.T) {
	if _, err := ParseCommand("git cafe1234 diff --staged aaa bbb", "/cwd"); err == nil {
		t.Fatal("--staged already names the right-hand side; a second revision must be refused")
	}
}

func TestParseGitPathspec(t *testing.T) {
	a := parseGitCmd(t, "git cafe1234 diff HEAD -- tui/app.go")
	if a.Path != "tui/app.go" || a.BaseRev != "HEAD" {
		t.Fatalf("action = %+v", a)
	}
}

func TestParseGitPathspecWithFlagAfterIt(t *testing.T) {
	a := parseGitCmd(t, "git cafe1234 diff --staged -- tui/app.go")
	if a.Path != "tui/app.go" || !a.Staged {
		t.Fatalf("action = %+v", a)
	}
}

func TestParseGitLogMax(t *testing.T) {
	a := parseGitCmd(t, "git cafe1234 log --max 20")
	if a.Sub != "log" || a.Max != 20 {
		t.Fatalf("action = %+v", a)
	}
}

func TestParseGitShowRev(t *testing.T) {
	a := parseGitCmd(t, "git cafe1234 show abc123")
	if a.Sub != "show" || a.BaseRev != "abc123" {
		t.Fatalf("action = %+v", a)
	}
}

func TestParseGitStatus(t *testing.T) {
	a := parseGitCmd(t, "git cafe1234 status")
	if a.Sub != "status" {
		t.Fatalf("action = %+v", a)
	}
}

func TestParseGitStatusRejectsRev(t *testing.T) {
	if _, err := ParseCommand("git cafe1234 status HEAD", "/cwd"); err == nil {
		t.Fatal("git status takes no revision")
	}
}

func TestParseGitUnknownSub(t *testing.T) {
	if _, err := ParseCommand("git cafe1234 rebase", "/cwd"); err == nil {
		t.Fatal("unknown sub-verb must be rejected")
	}
}

func TestParseGitMissingTaskID(t *testing.T) {
	if _, err := ParseCommand("git", "/cwd"); err == nil {
		t.Fatal("missing task id must be rejected")
	}
}

func TestParseGitMissingSubVerb(t *testing.T) {
	if _, err := ParseCommand("git cafe1234", "/cwd"); err == nil {
		t.Fatal("missing sub-verb must be rejected")
	}
}

func TestParseGitSubrepoFlag(t *testing.T) {
	a := parseGitCmd(t, "git cafe1234 diff --subrepo pkg/inner HEAD")
	if a.Subrepo != "pkg/inner" || a.BaseRev != "HEAD" {
		t.Fatalf("action = %+v", a)
	}
}

// --subrepo chooses the repository and -- chooses the paths within it; both at
// once must survive.
func TestParseGitSubrepoAndPathspecTogether(t *testing.T) {
	a := parseGitCmd(t, "git cafe1234 diff --subrepo pkg/inner -- src/x.go")
	if a.Subrepo != "pkg/inner" || a.Path != "src/x.go" {
		t.Fatalf("action = %+v", a)
	}
}

func TestParseGitSubmoduleFlag(t *testing.T) {
	a := parseGitCmd(t, "git cafe1234 diff --submodule")
	if !a.Submodule {
		t.Fatalf("action = %+v", a)
	}
	b := parseGitCmd(t, "git cafe1234 diff")
	if b.Submodule {
		t.Fatal("--submodule must be off by default")
	}
}

func TestParseGitSubreposVerb(t *testing.T) {
	a := parseGitCmd(t, "git cafe1234 subrepos")
	if a.Sub != "subrepos" {
		t.Fatalf("action = %+v", a)
	}
}

func TestParseGitSubreposWithSubrepo(t *testing.T) {
	a := parseGitCmd(t, "git cafe1234 subrepos --subrepo pkg/inner")
	if a.Sub != "subrepos" || a.Subrepo != "pkg/inner" {
		t.Fatalf("action = %+v", a)
	}
}

func TestParseGitSubreposRejectsRev(t *testing.T) {
	if _, err := ParseCommand("git cafe1234 subrepos HEAD", "/cwd"); err == nil {
		t.Fatal("git subrepos takes no revision")
	}
}

func TestParseGitLogSubrepo(t *testing.T) {
	a := parseGitCmd(t, "git cafe1234 log --subrepo pkg/inner --max 5")
	if a.Subrepo != "pkg/inner" || a.Max != 5 {
		t.Fatalf("action = %+v", a)
	}
}

func TestParseGitFile(t *testing.T) {
	a := parseGitCmd(t, "git cafe1234 file tui/app.go")
	if a.Sub != "file" || a.Path != "tui/app.go" {
		t.Fatalf("action = %+v", a)
	}
}

// A path lifted straight out of a diff header arrives after --; both spellings
// have to work or the copy-paste route breaks.
func TestParseGitFileAfterSeparator(t *testing.T) {
	a := parseGitCmd(t, "git cafe1234 file -- tui/app.go")
	if a.Path != "tui/app.go" {
		t.Fatalf("action = %+v", a)
	}
}

func TestParseGitFileSides(t *testing.T) {
	if a := parseGitCmd(t, "git cafe1234 file --staged x.go"); !a.Staged {
		t.Fatalf("action = %+v", a)
	}
	a := parseGitCmd(t, "git cafe1234 file --rev abc123 x.go")
	if a.TargetRev != "abc123" {
		t.Fatalf("action = %+v", a)
	}
}

func TestParseGitFileNeedsAPath(t *testing.T) {
	if _, err := ParseCommand("git cafe1234 file", "/cwd"); err == nil {
		t.Fatal("a path is required")
	}
}

func TestParseGitFileRejectsTwoPaths(t *testing.T) {
	if _, err := ParseCommand("git cafe1234 file a.go -- b.go", "/cwd"); err == nil {
		t.Fatal("a path given twice must be refused, not silently one of them")
	}
}

func TestParseGitFileWithSubrepo(t *testing.T) {
	a := parseGitCmd(t, "git cafe1234 file --subrepo pkg/inner i.txt")
	if a.Subrepo != "pkg/inner" || a.Path != "i.txt" {
		t.Fatalf("action = %+v", a)
	}
}

func TestParseFilePullRange(t *testing.T) {
	got, err := ParseCommand(`file pull -o 10 -n 20 deadbeef rel/file.txt ./local.txt`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	act, ok := got.(FilePullAction)
	if !ok {
		t.Fatalf("got %T", got)
	}
	if act.Offset != 10 || act.Length != 20 {
		t.Fatalf("offset=%d length=%d, want 10/20", act.Offset, act.Length)
	}
}

// A directory pull is a generated tar; its byte offsets mean nothing stable,
// so the combination is refused at parse time rather than sent.
func TestParseFilePullRangeWithRecursiveIsRejected(t *testing.T) {
	if _, err := ParseCommand(`file pull -r -o 10 deadbeef rel ./local`, "/cwd"); err == nil {
		t.Fatal("want an error for -r with -o")
	}
}
