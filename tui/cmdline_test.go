package tui

import (
	"github.com/on-keyday/agent-harness/cli/verb"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestParseSubmitWithRepo(t *testing.T) {
	got, err := ParseCommand(`submit --repo /foo "long prompt with spaces"`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a, ok := got.(verb.SpawnAction)
	if !ok {
		t.Fatalf("got %T, want verb.SpawnAction", got)
	}
	if a.Repo != "/foo" {
		t.Errorf("Repo=%q", a.Repo)
	}
	if a.Task != "long prompt with spaces" {
		t.Errorf("Prompt=%q", a.Task)
	}
}

func TestParseSubmitDefaultRepo(t *testing.T) {
	got, err := ParseCommand(`submit hello`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(verb.SpawnAction)
	if a.Repo != "/cwd" {
		t.Errorf("Repo=%q, want /cwd", a.Repo)
	}
	if a.Task != "hello" {
		t.Errorf("Prompt=%q", a.Task)
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
	a := got.(verb.SpawnAction)
	if a.Task != "do work" {
		t.Errorf("Prompt=%q", a.Task)
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
	a := got.(verb.SpawnAction)
	if a.ResumeTaskID != "abc123" {
		t.Errorf("ResumeTaskID=%q want abc123", a.ResumeTaskID)
	}
	if !a.ResumeConversation {
		t.Fatal("ResumeConversation=false want true")
	}
	if a.Task != "do work" {
		t.Errorf("Prompt=%q want do work", a.Task)
	}
}

func TestParseSubmitWithAgent(t *testing.T) {
	got, err := ParseCommand(`submit --agent codex "do work"`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(verb.SpawnAction)
	if a.Agent != "codex" {
		t.Errorf("AgentProfile=%q want codex", a.Agent)
	}
}

func TestParseSubmitDefaultAgentEmpty(t *testing.T) {
	got, err := ParseCommand(`submit hello`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(verb.SpawnAction)
	if a.Agent != "" {
		t.Errorf("AgentProfile=%q want empty (runner default)", a.Agent)
	}
}

func TestParseInteractiveWithClaudeArgs(t *testing.T) {
	got, err := ParseCommand(`interactive --repo /r --claude-arg --add-dir --claude-arg /other`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(verb.SpawnAction)
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
	a := got.(verb.SpawnAction)
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
	a := got.(verb.SpawnAction)
	if a.Agent != "codex" {
		t.Errorf("AgentProfile=%q want codex", a.Agent)
	}
}

func TestParseCancel(t *testing.T) {
	got, err := ParseCommand(`cancel ab12cd`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(verb.CancelAction)
	if a.TaskID != "ab12cd" {
		t.Errorf("IDPrefix=%q", a.TaskID)
	}
}

func TestParseCancelMissingID(t *testing.T) {
	_, err := ParseCommand(`cancel`, "/cwd")
	if err == nil {
		t.Fatal("expected error")
	}
}

// The default is still 7 days -- what changed is that the sweep has to be
// ASKED for. A bare `prune` forgot every terminal task older than it, and the
// server deletes each TaskEntry and its log, after which `submit --resume
// <id>` answers resume_not_found.
func TestParsePruneRefusesTheBareSweep(t *testing.T) {
	if _, err := ParseCommand(`prune`, "/cwd"); err == nil {
		t.Fatal("a bare `prune` parsed; the widest form must be typed out")
	}
	got, err := ParseCommand(`prune --before 168h`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(verb.PruneAction)
	if a.Before != 7*24*time.Hour {
		t.Errorf("Before=%v, want 168h", a.Before)
	}
}

func TestParsePruneFlags(t *testing.T) {
	got, err := ParseCommand(`prune --before=1h`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(verb.PruneAction)
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
	a := got.(verb.PruneAction)
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
	a := got.(verb.PruneAction)
	if !a.Force || len(a.TaskIDs) != 1 {
		t.Errorf("got Force=%v TaskIDs=%v, want Force=true, 1 id", a.Force, a.TaskIDs)
	}
}

func TestParseClear(t *testing.T) {
	got, err := ParseCommand(`clear`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	if sa, ok := got.(verb.ScreenAction); !ok || sa.Sub != "clear" {
		t.Fatalf("got %#v", got)
	}
}

func TestParseQuit(t *testing.T) {
	for _, in := range []string{"quit", "exit"} {
		got, err := ParseCommand(in, "/cwd")
		if err != nil {
			t.Fatal(err)
		}
		// `exit` is its own declared path fixing the same Const, so both
		// spellings arrive as one action -- the alias is in the table, not in
		// a second type or a second case.
		if sa, ok := got.(verb.ScreenAction); !ok || sa.Sub != "quit" {
			t.Fatalf("input %q got %#v", in, got)
		}
	}
}

func TestParseHelp(t *testing.T) {
	got, err := ParseCommand(`help`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	if sa, ok := got.(verb.ScreenAction); !ok || sa.Sub != "help" {
		t.Fatalf("got %#v", got)
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
	a := got.(verb.SpawnAction)
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
	a := got.(verb.SpawnAction)
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
	a := got.(verb.SpawnAction)
	if a.Agent != "codex" {
		t.Errorf("AgentProfile=%q want codex", a.Agent)
	}
}

func TestParseSessionNewResumeConversation(t *testing.T) {
	got, err := ParseCommand(`session new --resume abc123 --resume-conversation`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(verb.SpawnAction)
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
	a := got.(verb.SpawnAction)
	if a.Runner != hex32 {
		t.Errorf("Runner=%q want %s", a.Runner, hex32)
	}
}

func TestParseSessionNewWithIP(t *testing.T) {
	got, err := ParseCommand(`session new --ip 192.168.1.10`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(verb.SpawnAction)
	if a.IP != "192.168.1.10" {
		t.Errorf("IP=%q want 192.168.1.10", a.IP)
	}
}

func TestParseSessionNewDetachAndHost(t *testing.T) {
	got, err := ParseCommand(`session new --detach --host gmkhost-pdf2md`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(verb.SpawnAction)
	if !a.Detach {
		t.Errorf("Detach=false want true")
	}
	if a.Host != "gmkhost-pdf2md" {
		t.Errorf("Host=%q", a.Host)
	}
}

func TestParseSessionNewStream(t *testing.T) {
	got, err := ParseCommand(`session new --stream -d`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(verb.SpawnAction)
	if !a.Stream || !a.Detach {
		t.Errorf("Stream=%v Detach=%v want true/true", a.Stream, a.Detach)
	}
}

// --stream without -d must refuse: the TUI's non-detach path hands the
// terminal to the session, and an event-stream session has no terminal. And
// --stream with --x11 is a terminal-concept conflict. Both are errors, not
// silent drops — a typed option either takes effect or errors.
func TestParseSessionNewStreamRequiresDetach(t *testing.T) {
	for _, in := range []string{
		`session new --stream`,
		`session new --stream --x11 -d`,
	} {
		if _, err := ParseCommand(in, "/cwd"); err == nil {
			t.Errorf("input %q: expected an error", in)
		}
	}
}

func TestParseSessionStreamAttach(t *testing.T) {
	got, err := ParseCommand(`session stream attach deadbeef`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(verb.SessionAction)
	if a.Sub != "stream-attach" || a.TaskID != "deadbeef" {
		t.Errorf("got %#v, want stream-attach deadbeef", a)
	}
}

// A specified-but-unbuilt stream verb must say THAT, not "unknown": the
// namespace is one-to-one with the protocol's inbound kinds, and a verb that
// exists in the design but not the build is a different fact from a typo.
func TestParseSessionStreamUnbuiltVerbNamesItself(t *testing.T) {
	// turn/approve/interrupt/finish shipped; requests and snapshot are what is
	// left, and this asserts the DISTINCTION rather than any one verb's state.
	_, err := ParseCommand(`session stream requests deadbeef`, "/cwd")
	if err == nil || !strings.Contains(err.Error(), "not built") {
		t.Errorf("err=%v want a not-built-yet error", err)
	}
	_, err = ParseCommand(`session stream frobnicate x`, "/cwd")
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("err=%v want unknown-verb", err)
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
	a := got.(verb.FileLsAction)
	if a.TaskID != "deadbeef0011" || a.RelPath != "src/" {
		t.Errorf("got %+v", a)
	}
}

func TestParseFileLsRootDefault(t *testing.T) {
	got, err := ParseCommand(`file ls deadbeef`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(verb.FileLsAction)
	if a.TaskID != "deadbeef" || a.RelPath != "" {
		t.Errorf("got %+v", a)
	}
}

func TestParseFilePush(t *testing.T) {
	got, err := ParseCommand(`file push -r -f deadbeef ./local-dir rel/dir`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(verb.FilePushAction)
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
	a := got.(verb.FilePullAction)
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
	a := got.(verb.FileDeleteAction)
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
	a := got.(verb.FileDeleteAction)
	if a.Recursive || a.Force {
		t.Errorf("expected non-recursive, got %+v", a)
	}
}

func TestParseServerDialRunner(t *testing.T) {
	got, err := ParseCommand(`server dial-runner ws:192.168.3.10:8540-*`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a, ok := got.(verb.ServerDialRunnerAction)
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
	a, ok := got.(verb.ServerDialRunnerAction)
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
	a, ok := got.(verb.ServerDialRunnerAction)
	if !ok {
		t.Fatalf("expected ServerDialRunnerAction, got %T", got)
	}
	if a.Via != "" {
		t.Errorf("Via should be empty, got %q", a.Via)
	}
}

// notify takes the CLI's flags here now. It was a positional grammar --
// `notify [info|warn|error] <title> [text...]` -- which meant the declaration's
// own example, `notify --level warn --title T body`, parsed on this surface as
// a notification TITLED "--level" (D21).
func TestParseNotifyUsesTheDeclaredFlags(t *testing.T) {
	got, err := ParseCommand(`notify --level warn --title build the tree is red`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a, ok := got.(verb.NotifyAction)
	if !ok {
		t.Fatalf("got %T, want verb.NotifyAction", got)
	}
	if a.Level != "warn" || a.Title != "build" || a.Text != "the tree is red" {
		t.Fatalf("got %#v", a)
	}
}

// The body is required, and --level takes the declared vocabulary only.
func TestParseNotifyRejects(t *testing.T) {
	for _, line := range []string{
		`notify`,
		`notify --level bogus x`,
	} {
		if _, err := ParseCommand(line, "/cwd"); err == nil {
			t.Errorf("%q parsed, want an error", line)
		}
	}
}

// The old positional form is gone, and it must not parse as something ELSE:
// `notify warn foo` used to mean level=warn, and silently becoming a body of
// "warn foo" would be the worse failure.
func TestParseNotifyOldPositionalFormIsPlainText(t *testing.T) {
	got, err := ParseCommand(`notify warn foo bar`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(verb.NotifyAction)
	if a.Level != "info" || a.Text != "warn foo bar" {
		t.Fatalf("got %#v, want the whole tail as text at the default level", a)
	}
}

// The session's spawn defaults. `caps <mask>` was a grammar this surface
// alone had -- the mask joined out of the raw tokens -- and is now `caps
// set-defaults --caps <mask>`, the same flags `caps set` takes minus the id.
func TestParseCapsCommand(t *testing.T) {
	act, err := ParseCommand("caps set-defaults --caps spawn,file_read", "repo")
	if err != nil {
		t.Fatal(err)
	}
	ca, ok := act.(verb.SetDefaultsAction)
	if !ok {
		t.Fatalf("got %T, want verb.SetDefaultsAction", act)
	}
	if ca.Caps == nil || *ca.Caps != (protocol.Capability_Spawn|protocol.Capability_FileRead) {
		t.Fatalf("caps = %#v", ca.Caps)
	}
	// No flags: nothing to change, so the three stay nil and the handler
	// opens the picker instead of writing a default nobody named.
	act, err = ParseCommand("caps set-defaults", "repo")
	if err != nil {
		t.Fatal(err)
	}
	if ca, _ := act.(verb.SetDefaultsAction); ca.Caps != nil || ca.Scope != nil || ca.Overrides != nil {
		t.Fatalf("bare `caps set-defaults` = %#v, want all-nil (show)", ca)
	}
	// bad name → error
	if _, err := ParseCommand("caps set-defaults --caps bogus", "repo"); err == nil {
		t.Fatal("expected error for unknown cap")
	}
	// Bare `caps` is the CATALOG now -- every capability with the sentence
	// saying what it gates -- reachable from all three surfaces rather than
	// the CLI alone.
	act, err = ParseCommand("caps", "repo")
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := act.(verb.CatalogAction); !ok || c.Sub != "caps" {
		t.Fatalf("bare `caps` = %#v, want CatalogAction{Sub: caps}", act)
	}
}

// The `--on-resume` toggle is gone: a resumed task keeps its persisted caps
// unless the resuming command names --caps. Nothing declares the flag, on any
// of the three verbs it could be typed against, so every one of them refuses
// it -- which is the property, rather than a hand-written hint that has to be
// kept in step with the removal.
func TestParseCapsOnResumeRemoved(t *testing.T) {
	for _, line := range []string{
		"caps --on-resume on",
		"caps set-defaults --on-resume on",
		"scope --on-resume on",
	} {
		if _, err := ParseCommand(line, "r"); err == nil {
			t.Errorf("%q parsed; --on-resume should be refused", line)
		}
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
	if c := act.(verb.SpawnAction).Caps; c != nil {
		t.Errorf("submit without --caps: Caps = %v, want nil", *c)
	}

	act, err = ParseCommand("submit --caps all,-spawn hello", "r")
	if err != nil {
		t.Fatalf("submit --caps: %v", err)
	}
	c := act.(verb.SpawnAction).Caps
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
	sc := act.(verb.SpawnAction).Caps
	if sc == nil {
		t.Fatal("session new --caps none: Caps is nil — none must be distinguishable from unset")
	}
	if *sc != protocol.Capability_None {
		t.Errorf("session new --caps none = %#x, want %#x", *sc, protocol.Capability_None)
	}

	act, err = ParseCommand("interactive --caps exec_control", "r")
	if err != nil {
		t.Fatalf("interactive --caps: %v", err)
	}
	ic := act.(verb.SpawnAction).Caps
	if ic == nil || *ic != protocol.Capability_ExecControl {
		t.Errorf("interactive --caps exec_control = %v", ic)
	}

	// A bad mask must fail the whole command, not spawn with a default.
	if _, err := ParseCommand("submit --caps bogus hello", "r"); err == nil {
		t.Error("expected an error for an unknown capability name")
	}
}

// TestParseSpawnScopeFlag: --scope on the cmdline spawn commands, the
// target-set half of --caps, with the same unset-vs-zero distinction.
func TestParseSpawnScopeFlag(t *testing.T) {
	act, err := ParseCommand("submit hello", "r")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if s := act.(verb.SpawnAction).Scope; s != nil {
		t.Errorf("submit without --scope: Scope = %v, want nil", *s)
	}

	act, err = ParseCommand("submit --scope none hello", "r")
	if err != nil {
		t.Fatalf("submit --scope none: %v", err)
	}
	s := act.(verb.SpawnAction).Scope
	if s == nil || s.Base != protocol.ScopeBase_None {
		t.Fatalf("submit --scope none = %v, want base none", s)
	}

	act, err = ParseCommand("interactive --scope global", "r")
	if err != nil {
		t.Fatalf("interactive --scope: %v", err)
	}
	is := act.(verb.SpawnAction).Scope
	if is == nil || is.Base != protocol.ScopeBase_Global {
		t.Errorf("interactive --scope global = %v", is)
	}

	act, err = ParseCommand("session new --scope subtree", "r")
	if err != nil {
		t.Fatalf("session new --scope subtree: %v", err)
	}
	ss := act.(verb.SpawnAction).Scope
	if ss == nil || ss.Base != protocol.ScopeBase_Subtree {
		t.Fatal("session new --scope subtree: explicit subtree must be distinguishable from unset")
	}

	if _, err := ParseCommand("submit --scope bogus hello", "r"); err == nil {
		t.Error("expected an error for an unknown scope form")
	}
}

// TestParseScopeOnResumeStandsAlone: --scope on a resume re-grants the scope
// through its own presence bit (scope_present), with or without --caps.
func TestParseScopeOnResumeStandsAlone(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	act, err := ParseCommand("submit --resume "+id+" --scope none hello", "r")
	if err != nil {
		t.Fatalf("lone --scope on resume must parse: %v", err)
	}
	sa := act.(verb.SpawnAction)
	if sa.Scope == nil || sa.Scope.Base != protocol.ScopeBase_None {
		t.Fatalf("Scope = %v, want explicit none", sa.Scope)
	}
	if sa.Caps != nil {
		t.Fatal("Caps must stay nil (kept) when only --scope is given")
	}
}

// TestParseRefresh verifies `refresh` and its `sync` alias parse to a
// ScreenAction{Sub: "refresh"} (force full snapshot re-sync).
func TestParseRefresh(t *testing.T) {
	for _, in := range []string{"refresh", "sync"} {
		act, err := ParseCommand(in, "")
		if err != nil {
			t.Fatalf("ParseCommand(%q): %v", in, err)
		}
		if sa, ok := act.(verb.ScreenAction); !ok || sa.Sub != "refresh" {
			t.Errorf("ParseCommand(%q) = %#v, want ScreenAction{Sub: refresh}", in, act)
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
	ai, ok := act.(verb.SessionAction)
	if !ok {
		t.Fatalf("got %T, want SessionAwaitIdleAction", act)
	}
	if ai.TaskID != "abc123" || ai.Notify || ai.Topic != "" || ai.ThresholdMs != 0 {
		t.Errorf("unexpected defaults: %+v", ai)
	}

	act, err = ParseCommand("session await-idle --notify --threshold-ms 5000 abc123", "")
	if err != nil {
		t.Fatalf("parse notify: %v", err)
	}
	ai = act.(verb.SessionAction)
	if !ai.Notify || ai.ThresholdMs != 5000 {
		t.Errorf("notify/threshold not parsed: %+v", ai)
	}

	act, err = ParseCommand("session await-idle --topic chat.me abc123", "")
	if err != nil {
		t.Fatalf("parse topic: %v", err)
	}
	if ai = act.(verb.SessionAction); ai.Topic != "chat.me" {
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
	a := got.(verb.FilePushAction)
	if !a.Parents || a.Force || a.Recursive {
		t.Errorf("flags = %+v want Parents only", a)
	}
}

func TestParseFileMkdir(t *testing.T) {
	got, err := ParseCommand(`file mkdir -p deadbeef rel/new/dir`, "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	a := got.(verb.FileMkdirAction)
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
	kill, ok := act.(verb.ForwardKillAction)
	if !ok || len(kill.ForwardIDs) != 1 || kill.ForwardIDs[0] != 12 {
		t.Fatalf("got %#v, want verb.ForwardKillAction{12}", act)
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
	e, ok := act.(verb.FileEditAction)
	if !ok {
		t.Fatalf("got %T, want verb.FileEditAction", act)
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
	n, ok := act.(verb.FileNewAction)
	if !ok {
		t.Fatalf("got %T, want verb.FileNewAction", act)
	}
	if n.RelPath != "sub/notes.txt" {
		t.Errorf("act=%+v, want sub/notes.txt", n)
	}
}

func parseGitCmd(t *testing.T, line string) verb.GitAction {
	t.Helper()
	got, err := ParseCommand(line, "/cwd")
	if err != nil {
		t.Fatalf("ParseCommand(%q): %v", line, err)
	}
	a, ok := got.(verb.GitAction)
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
	act, ok := got.(verb.FilePullAction)
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

// scope is the target-set companion to caps: same show/set shape, and the
// session default reaches every spawn through App.authority().
func TestParseScopeCommand(t *testing.T) {
	id := strings.Repeat("ab", 16)
	act, err := ParseCommand("scope", "/cwd")
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	// `scope` is a declared path of the same verb as `caps set-defaults`,
	// the way `exit` is `quit`: one Action, one handler method.
	if sa, ok := act.(verb.SetDefaultsAction); !ok || sa.Scope != nil {
		t.Fatalf("bare `scope` = %#v, want SetDefaultsAction with nothing set", act)
	}
	act, err = ParseCommand("scope --scope none+ids:"+id, "/cwd")
	if err != nil {
		t.Fatalf("scope none+ids: %v", err)
	}
	sa, ok := act.(verb.SetDefaultsAction)
	if !ok || sa.Scope == nil || sa.Scope.Base != protocol.ScopeBase_None || sa.Scope.IdsLen != 1 {
		t.Fatalf("scope none+ids = %#v", act)
	}
	if _, err := ParseCommand("scope --scope bogus", "/cwd"); err == nil {
		t.Fatal("an unknown scope base parsed")
	}
}

func TestParseCapsSetCommand(t *testing.T) {
	id := strings.Repeat("cd", 16)
	act, err := ParseCommand("caps set "+id+" --caps spawn --scope global --cascade", "/cwd")
	if err != nil {
		t.Fatalf("caps set: %v", err)
	}
	sc, ok := act.(verb.SetCapsAction)
	if !ok {
		t.Fatalf("act = %#v, want verb.SetCapsAction", act)
	}
	if sc.TaskID != id || sc.Caps == nil || *sc.Caps != protocol.Capability_Spawn {
		t.Fatalf("caps set = %#v", sc)
	}
	if sc.Scope == nil || sc.Scope.Base != protocol.ScopeBase_Global || !sc.Cascade || sc.KeepConns {
		t.Fatalf("caps set flags = %#v", sc)
	}

	// Either half alone is fine; neither is not — a call that changes nothing
	// is a typo, not a no-op worth sending.
	if _, err := ParseCommand("caps set "+id+" --scope none", "/cwd"); err != nil {
		t.Fatalf("scope-only caps set: %v", err)
	}
	if _, err := ParseCommand("caps set "+id, "/cwd"); err == nil {
		t.Fatal("caps set with neither --caps nor --scope was accepted")
	}
	if _, err := ParseCommand("caps set --caps spawn", "/cwd"); err == nil {
		t.Fatal("caps set without a task id was accepted")
	}
}

func TestParseCapsSetParentCommand(t *testing.T) {
	id := strings.Repeat("ab", 16)
	pid := strings.Repeat("cd", 16)

	act, err := ParseCommand("caps set-parent "+id+" --parent "+pid, "/cwd")
	if err != nil {
		t.Fatalf("set-parent --parent: %v", err)
	}
	sp, ok := act.(verb.SetParentAction)
	if !ok || sp.TaskID != id || sp.ParentID != pid || sp.None || sp.Swap {
		t.Fatalf("set-parent --parent = %#v", act)
	}

	act, err = ParseCommand("caps set-parent "+id+" --none", "/cwd")
	if err != nil {
		t.Fatalf("set-parent --none: %v", err)
	}
	sp, ok = act.(verb.SetParentAction)
	if !ok || sp.TaskID != id || !sp.None || sp.Swap || sp.ParentID != "" {
		t.Fatalf("set-parent --none = %#v", act)
	}

	act, err = ParseCommand("caps set-parent --swap "+id, "/cwd")
	if err != nil {
		t.Fatalf("set-parent --swap (flag first): %v", err)
	}
	sp, ok = act.(verb.SetParentAction)
	if !ok || sp.TaskID != id || !sp.Swap || sp.None {
		t.Fatalf("set-parent --swap = %#v", act)
	}

	// Exactly one of the three forms; zero or two is an error.
	if _, err := ParseCommand("caps set-parent "+id, "/cwd"); err == nil {
		t.Fatal("set-parent with no form parsed")
	}
	if _, err := ParseCommand("caps set-parent "+id+" --none --swap", "/cwd"); err == nil {
		t.Fatal("set-parent with two forms parsed")
	}
	if _, err := ParseCommand("caps set-parent --parent "+pid, "/cwd"); err == nil {
		t.Fatal("set-parent without a target parsed")
	}
}

func TestParseGrid_Modes(t *testing.T) {
	for _, tc := range []struct {
		line       string
		wantMode   cli.GridScopeMode
		wantAnchor string
		wantIDs    []string
	}{
		{"grid", cli.GridAll, "", nil},
		{"grid ab12 cd34", cli.GridIds, "", []string{"ab12", "cd34"}},
		{"grid --under ab12", cli.GridSubtree, "ab12", nil},
		{"grid --under ab12 --descendants", cli.GridDescendants, "ab12", nil},
		// Flag order is free here (unlike send/exec, whose text is free-form).
		{"grid --descendants --under ab12", cli.GridDescendants, "ab12", nil},
	} {
		got, err := ParseCommand(tc.line, "/cwd")
		if err != nil {
			t.Errorf("%q: %v", tc.line, err)
			continue
		}
		a, ok := got.(verb.GridAction)
		if !ok {
			t.Errorf("%q: got %T, want GridAction", tc.line, got)
			continue
		}
		if a.Mode != tc.wantMode || a.Anchor != tc.wantAnchor || strings.Join(a.IDs, ",") != strings.Join(tc.wantIDs, ",") {
			t.Errorf("%q: got mode=%q anchor=%q ids=%v, want mode=%q anchor=%q ids=%v",
				tc.line, a.Mode, a.Anchor, a.IDs, tc.wantMode, tc.wantAnchor, tc.wantIDs)
		}
	}
}

func TestParseGrid_RejectsContradictions(t *testing.T) {
	// Each of these has no single reading, so it must be refused rather than
	// resolved by precedence — a grid that quietly showed the other half of
	// what was asked for is worse than one that did nothing.
	for _, line := range []string{
		"grid --descendants",     // nothing to take the descendants OF
		"grid --under ab12 cd34", // one subtree, plus stray ids
		"grid --under",           // missing value
		"grid --nope",            // unknown flag
	} {
		if _, err := ParseCommand(line, "/cwd"); err == nil {
			t.Errorf("%q: want an error, got none", line)
		}
	}
}

// The CLI registers -d as a shorthand for --detach (cmd/harness-cli/session.go);
// typing the form learned there into the TUI answered "flag provided but not
// defined: -d". Both spellings must produce the same action.
func TestSessionNewDetachShorthand(t *testing.T) {
	for _, spelling := range []string{"-d", "--detach"} {
		got, err := ParseCommand("session new "+spelling+" --agent bash", "/cwd")
		if err != nil {
			t.Fatalf("%s: %v", spelling, err)
		}
		v, ok := got.(verb.SpawnAction)
		if !ok {
			t.Fatalf("%s: action = %T, want verb.SpawnAction", spelling, got)
		}
		if !v.Detach {
			t.Errorf("%s: Detach = false", spelling)
		}
		if v.Agent != "bash" {
			t.Errorf("%s: AgentProfile = %q, want bash", spelling, v.Agent)
		}
	}
}

// The four stream write verbs parse from the TUI command line, so an operator
// can drive a stream task without opening the chat view.
func TestParseSessionStreamWriteVerbs(t *testing.T) {
	got, err := ParseCommand(`session stream turn deadbeef hello there`, "/cwd")
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	turn, ok := got.(verb.SessionAction)
	if !ok || turn.Sub != "stream-turn" || turn.TaskID != "deadbeef" {
		t.Fatalf("turn: got %#v", got)
	}
	if turn.Text != "hello there" {
		t.Errorf("turn text = %q, want the words joined", turn.Text)
	}

	// --deny / --message, the CLI's spelling. The bare-word verdict this
	// surface used carried a reason -- "whitespace-split with no flag parser"
	// -- that was the WebUI's, copied here, where a FlagSet has always run.
	got, err = ParseCommand(`session stream approve deadbeef req-ab-1 --deny --message "use the Makefile"`, "/cwd")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	ap := got.(verb.SessionAction)
	if ap.RequestID != "req-ab-1" || !ap.Deny || ap.Message != "use the Makefile" {
		t.Fatalf("approve: got %#v", ap)
	}

	for _, sub := range []string{"interrupt", "finish"} {
		got, err := ParseCommand("session stream "+sub+" deadbeef", "/cwd")
		if err != nil {
			t.Fatalf("%s: %v", sub, err)
		}
		if w := got.(verb.SessionAction); w.Sub != "stream-"+sub || w.TaskID != "deadbeef" {
			t.Errorf("%s: got %#v", sub, w)
		}
	}
}

// A verdict is required, only one of them, and an allow carries no message --
// the message IS the deny reason, and it reaches the agent verbatim.
func TestParseSessionStreamApproveRejectsBadShapes(t *testing.T) {
	for _, line := range []string{
		`session stream approve deadbeef req-ab-1`,
		`session stream approve deadbeef req-ab-1 --allow --deny`,
		`session stream approve deadbeef req-ab-1 --allow --message "because I said so"`,
	} {
		if _, err := ParseCommand(line, "/cwd"); err == nil {
			t.Errorf("%q parsed, want an error", line)
		}
	}
}

// `diag` toggles the grid's per-pane overlay, and an explicit on/off is NOT the
// same request: a toggle would turn it off for an operator who typed `diag on`
// because someone else already had it on. The empty/non-empty Arg is what keeps
// those apart.
func TestParseDiag(t *testing.T) {
	act, err := ParseCommand("diag", "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	d, ok := act.(verb.ScreenAction)
	if !ok || d.Sub != "diag" {
		t.Fatalf("got %#v, want ScreenAction{Sub: diag}", act)
	}
	if d.Arg != "" {
		t.Errorf("bare `diag` carries Arg=%q, want empty (toggle)", d.Arg)
	}

	for _, tc := range []struct {
		in   string
		want string
	}{{"diag on", "on"}, {"diag off", "off"}} {
		act, err := ParseCommand(tc.in, "")
		if err != nil {
			t.Fatalf("parse %q: %v", tc.in, err)
		}
		d, ok := act.(verb.ScreenAction)
		if !ok || d.Sub != "diag" {
			t.Fatalf("%q: got %#v, want ScreenAction{Sub: diag}", tc.in, act)
		}
		if d.Arg != tc.want {
			t.Errorf("%q: Arg=%q, want %q", tc.in, d.Arg, tc.want)
		}
	}
}

// An unrecognised argument must name what is accepted rather than silently
// toggling — the same "take effect or error" rule the rest of the cmdline holds.
func TestParseDiagRejectsAnUnknownArgument(t *testing.T) {
	if _, err := ParseCommand("diag maybe", ""); err == nil {
		t.Fatal("`diag maybe` parsed; want an error naming on/off")
	}
}

// detach is the verb that takes the workspace back OFF. Its parsing carries
// two refusals worth pinning: it takes no name, and --stop belongs to it alone.
func TestParseWorkspaceDetach(t *testing.T) {
	act, err := ParseCommand("workspace detach", "/cwd")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	w, ok := act.(verb.WorkspaceAction)
	if !ok || w.Sub != "detach" || w.Stop {
		t.Fatalf("action = %+v, want detach with Stop false", act)
	}

	act, err = ParseCommand("workspace detach --stop", "/cwd")
	if err != nil {
		t.Fatalf("parse --stop: %v", err)
	}
	if w := act.(verb.WorkspaceAction); !w.Stop {
		t.Error("--stop did not reach the action")
	}

	// A name would read as "detach that one instead of mine", and there is only
	// ever one installed workspace for it to mean.
	if _, err := ParseCommand("workspace detach other", "/cwd"); err == nil {
		t.Error("workspace detach <name> = nil error, want a refusal")
	}
	// A typed flag either takes effect or errors; silently ignoring --stop on
	// apply would be the shape the checklist's item 33 exists for.
	if _, err := ParseCommand("workspace apply --stop", "/cwd"); err == nil {
		t.Error("workspace apply --stop = nil error, want a refusal")
	}
}

func TestCmdlineParsesForwardTap(t *testing.T) {
	act, err := ParseCommand("forward tap 7 --dir to-target --max-bytes 64", "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ta, ok := act.(verb.ForwardTapAction)
	if !ok {
		t.Fatalf("action type %T", act)
	}
	if ta.ForwardID != 7 || ta.Dir != "to-target" || ta.MaxRecordBytes != 64 {
		t.Fatalf("parsed %+v", ta)
	}
}

// The cmdline accepts exactly what harness-cli accepts, because both go
// through cli.ParseTapFilter. A direction this surface invented would be a
// silent divergence.
func TestCmdlineForwardTapRejectsBadDir(t *testing.T) {
	if _, err := ParseCommand("forward tap 7 --dir inbound", ""); err == nil {
		t.Fatal("a bad --dir must be refused here too")
	}
	if _, err := ParseCommand("forward tap 7 --nope", ""); err == nil {
		t.Fatal("an unknown option must be refused, not ignored")
	}
	if _, err := ParseCommand("forward tap notanumber", ""); err == nil {
		t.Fatal("a non-numeric forward id must be refused")
	}
}

// TestPruneFlagAfterIDsIsRead pins the one behaviour the declaration changes
// on this surface. The old parsePrune had no arity check and used stdlib
// Parse, so `prune <id> --force` put "--force" into TaskIDs and left Force
// false; cli/prune.go then rejected it as a bad task id, making it a confusing
// error rather than a destructive one. Permuting is what the CLI always did,
// and the TUI now matches.
func TestPruneFlagAfterIDsIsRead(t *testing.T) {
	const id = "deadbeefdeadbeefdeadbeefdeadbeef"
	got, err := ParseCommand("prune "+id+" --force", "/cwd")
	if err != nil {
		t.Fatalf("ParseCommand: %v", err)
	}
	act, ok := got.(verb.PruneAction)
	if !ok {
		t.Fatalf("got %T, want verb.PruneAction", got)
	}
	if !act.Force {
		t.Error("--force written after the id was dropped")
	}
	if len(act.TaskIDs) != 1 || act.TaskIDs[0] != id {
		t.Errorf("TaskIDs = %q, want [%s]", act.TaskIDs, id)
	}
}
