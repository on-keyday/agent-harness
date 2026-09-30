package streamagent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Every test here builds a SYNTHETIC transcript under a scratch config dir.
// None reads the real home directory: a transcript holds a real conversation,
// and a test that found one would both leak it and depend on it.

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// writeTranscript places lines as <cfg>/projects/<encoded dir>/<name>.jsonl.
func writeTranscript(t *testing.T, cfg, dir, name string, lines ...string) string {
	t.Helper()
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(cfg, "projects", encodeProjectDirName(abs))
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, name+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func userRow(uuid, parent, text string) string {
	p := "null"
	if parent != "" {
		p = fmt.Sprintf("%q", parent)
	}
	return fmt.Sprintf(`{"type":"user","uuid":%q,"parentUuid":%s,"message":{"role":"user","content":%q}}`, uuid, p, text)
}

func assistantRow(uuid, parent, text string) string {
	return fmt.Sprintf(`{"type":"assistant","uuid":%q,"parentUuid":%q,"message":{"content":[{"type":"text","text":%q}]}}`, uuid, parent, text)
}

func texts(evs []Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, string(e.Kind)+":"+e.Text)
	}
	return out
}

func TestEncodeProjectDirName(t *testing.T) {
	// The documented rule, and the shape observed on this host: "/" and "."
	// both become "-", so "/x/.harness" gives "-x--harness".
	for in, want := range map[string]string{
		"/Users/me/proj":               "-Users-me-proj",
		"/home/u/repo/.harness-wt/abc": "-home-u-repo--harness-wt-abc",
		`C:\workspace\agent-harness`:   "C--workspace-agent-harness",
	} {
		if got := encodeProjectDirName(in); got != want {
			t.Errorf("encode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFindTranscriptPicksTheNewestInTheProjectDir(t *testing.T) {
	cfg, dir := t.TempDir(), t.TempDir()
	old := writeTranscript(t, cfg, dir, "old", userRow("u1", "", "a"))
	newer := writeTranscript(t, cfg, dir, "new", userRow("u1", "", "b"))
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	got, err := findClaudeTranscript(envOf(map[string]string{"CLAUDE_CONFIG_DIR": cfg}), dir)
	if err != nil || got != newer {
		t.Fatalf("got %q, %v; want %q", got, err, newer)
	}
}

func TestFindTranscriptHonoursTheProjectDirNameOverride(t *testing.T) {
	cfg, dir := t.TempDir(), t.TempDir()
	proj := filepath.Join(cfg, "projects", "chosen")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(proj, "s.jsonl")
	if err := os.WriteFile(want, []byte(userRow("u1", "", "x")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := findClaudeTranscript(envOf(map[string]string{
		"CLAUDE_CONFIG_DIR": cfg, "CLAUDE_CODE_PROJECT_DIR_NAME": "chosen"}), dir)
	if err != nil || got != want {
		t.Fatalf("got %q, %v; want %q", got, err, want)
	}
}

// A name past 200 bytes is truncated by claude with an undocumented hash
// appended, so it is matched on its first 200 bytes.
func TestFindTranscriptMatchesALongNameByPrefix(t *testing.T) {
	cfg := t.TempDir()
	dir := filepath.Join(t.TempDir(), strings.Repeat("d", 230))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(dir)
	enc := encodeProjectDirName(abs)
	proj := filepath.Join(cfg, "projects", enc[:projectDirNameLimit]+"-h4sh")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(proj, "s.jsonl")
	if err := os.WriteFile(want, []byte(userRow("u1", "", "x")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := findClaudeTranscript(envOf(map[string]string{"CLAUDE_CONFIG_DIR": cfg}), dir)
	if err != nil || got != want {
		t.Fatalf("got %q, %v; want %q", got, err, want)
	}
}

// The chain from the last conversation row decides what is replayed: a
// rewound branch is dropped, as are meta rows, sidechain rows and bookkeeping.
func TestReadTranscriptFollowsTheChainAndDropsWhatIsNotConversation(t *testing.T) {
	cfg, dir := t.TempDir(), t.TempDir()
	path := writeTranscript(t, cfg, dir, "s",
		userRow("u1", "", "first question"),
		`{"type":"attachment","uuid":"x1","parentUuid":"u1","attachment":{"type":"hook"}}`,
		assistantRow("a1", "x1", "first answer"),
		// A branch the operator rewound away from: its parent is on the
		// chain, but nothing on the chain descends from it.
		userRow("u2-abandoned", "a1", "abandoned question"),
		assistantRow("a2-abandoned", "u2-abandoned", "abandoned answer"),
		`{"type":"user","uuid":"m1","parentUuid":"a1","isMeta":true,"message":{"role":"user","content":"injected meta"}}`,
		`{"type":"assistant","uuid":"sc1","parentUuid":"m1","isSidechain":true,"message":{"content":[{"type":"text","text":"sidechain"}]}}`,
		userRow("u2", "m1", "second question"),
		`{"type":"mode","mode":"default","sessionId":"s"}`,
		`{"type":"system","subtype":"turn_duration","uuid":"t1","parentUuid":"u2","durationMs":5}`,
		assistantRow("a3", "t1", "second answer"),
		`{"type":"last-prompt","lastPrompt":"x","leafUuid":"a3"}`,
		`not json at all`,
	)
	tr, err := readClaudeTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(texts(tr.Events), " | ")
	want := "user_text:first question | text:first answer | user_text:second question | text:second answer"
	if got != want {
		t.Fatalf("replayed:\n  got  %s\n  want %s", got, want)
	}
	for _, e := range tr.Events {
		if !e.Replay {
			t.Fatalf("an event was not marked Replay: %+v", e)
		}
	}
	if tr.SessionID != "s" || tr.Total != 4 {
		t.Fatalf("SessionID=%q Total=%d", tr.SessionID, tr.Total)
	}
}

// The bound keeps the END of the conversation, and it caps each field.
func TestReadTranscriptKeepsTheTailWithinTheBounds(t *testing.T) {
	cfg, dir := t.TempDir(), t.TempDir()
	var lines []string
	parent := ""
	for i := 0; i < replayMaxEvents+50; i++ {
		u := fmt.Sprintf("u%d", i)
		lines = append(lines, userRow(u, parent, fmt.Sprintf("q%d", i)))
		parent = u
	}
	big := strings.Repeat("é", replayTextCap) // 2 bytes a rune: over the cap
	lines = append(lines, assistantRow("last", parent, big))
	tr, err := readClaudeTranscript(writeTranscript(t, cfg, dir, "s", lines...))
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Events) != replayMaxEvents || tr.Total != replayMaxEvents+51 {
		t.Fatalf("kept %d of %d", len(tr.Events), tr.Total)
	}
	last := tr.Events[len(tr.Events)-1]
	if last.Kind != EventText || len(last.Text) > replayTextCap+len("…") || !strings.HasSuffix(last.Text, "…") {
		t.Fatalf("the newest event was not kept, or its text was not capped: len=%d", len(last.Text))
	}
	if first := tr.Events[0]; first.Text != fmt.Sprintf("q%d", 51) {
		t.Fatalf("the oldest kept event is %q, want the tail to start at q51", first.Text)
	}
}

func TestResumeReplaysBeforeTheAgentSpeaks(t *testing.T) {
	cfg, dir := t.TempDir(), t.TempDir()
	writeTranscript(t, cfg, dir, "sess-1",
		userRow("u1", "", "earlier question"),
		assistantRow("a1", "u1", "earlier answer"))
	argv := filepath.Join(t.TempDir(), "argv")
	agent := fakeAgent(t, argv,
		`printf '%s\n' '{"type":"system","subtype":"init","session_id":"sess-1"}'`+"\n"+
			`printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"live"}]}}'`+"\n"+
			"cat > /dev/null\n")
	msgs, _ := runAdapter(t, agent, ClaudeOpts{Dir: dir, ResumeConversation: true,
		Getenv: envOf(map[string]string{"CLAUDE_CONFIG_DIR": cfg})}, "")

	var seq []string
	for _, m := range msgs {
		switch {
		case m.Kind == KindHello:
			seq = append(seq, "hello")
		case m.Kind == KindEvent && m.Event.Replay:
			seq = append(seq, "replay:"+m.Event.Text)
		case m.Kind == KindEvent && m.Event.Kind == EventText:
			seq = append(seq, "live:"+m.Event.Text)
		case m.Kind == KindEvent && m.Event.Kind == EventError:
			t.Errorf("unexpected warning: %s", m.Event.Text)
		}
	}
	got := strings.Join(seq, " | ")
	want := "hello | replay:── previous conversation: last 2 of 2 events ── | replay:earlier question | replay:earlier answer | replay:── resumed ── | live:live"
	if got != want {
		t.Fatalf("order:\n  got  %s\n  want %s", got, want)
	}
}

func TestResumeWarnsWhenTheAgentResumedAnotherSession(t *testing.T) {
	cfg, dir := t.TempDir(), t.TempDir()
	writeTranscript(t, cfg, dir, "sess-shown", userRow("u1", "", "q"))
	agent := fakeAgent(t, filepath.Join(t.TempDir(), "argv"),
		`printf '%s\n' '{"type":"system","subtype":"init","session_id":"sess-other"}'`+"\n"+
			"cat > /dev/null\n")
	msgs, _ := runAdapter(t, agent, ClaudeOpts{Dir: dir, ResumeConversation: true,
		Getenv: envOf(map[string]string{"CLAUDE_CONFIG_DIR": cfg})}, "")
	var warned bool
	for _, m := range msgs {
		if m.Kind == KindEvent && m.Event.Kind == EventError && m.Event.Warning &&
			strings.Contains(m.Event.Text, "sess-shown") && strings.Contains(m.Event.Text, "sess-other") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("no mismatch warning in %+v", msgs)
	}
}

// No transcript is one warning and a resume that carries on — never fatal.
func TestResumeWithNoTranscriptWarnsOnceAndContinues(t *testing.T) {
	cfg, dir := t.TempDir(), t.TempDir()
	agent := fakeAgent(t, filepath.Join(t.TempDir(), "argv"),
		`printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"live"}]}}'`+"\n"+
			"cat > /dev/null\n")
	msgs, _ := runAdapter(t, agent, ClaudeOpts{Dir: dir, ResumeConversation: true,
		Getenv: envOf(map[string]string{"CLAUDE_CONFIG_DIR": cfg})}, "")
	var warnings, live int
	for _, m := range msgs {
		if m.Kind != KindEvent {
			continue
		}
		if m.Event.Kind == EventError && m.Event.Replay {
			warnings++
		}
		if m.Event.Kind == EventText && !m.Event.Replay {
			live++
		}
	}
	if warnings != 1 || live != 1 {
		t.Fatalf("warnings=%d live=%d, want 1 and 1", warnings, live)
	}
}

// A fresh (non-resume) run touches no transcript at all.
func TestAFreshRunReplaysNothing(t *testing.T) {
	cfg, dir := t.TempDir(), t.TempDir()
	writeTranscript(t, cfg, dir, "s", userRow("u1", "", "should not appear"))
	agent := fakeAgent(t, filepath.Join(t.TempDir(), "argv"), "cat > /dev/null\n")
	msgs, _ := runAdapter(t, agent, ClaudeOpts{Dir: dir,
		Getenv: envOf(map[string]string{"CLAUDE_CONFIG_DIR": cfg})}, "")
	for _, m := range msgs {
		if m.Kind == KindEvent && m.Event.Replay {
			t.Fatalf("a fresh run replayed: %+v", m.Event)
		}
	}
}

func TestRenderTextMarksAReplayedEvent(t *testing.T) {
	line, ok := RenderText(Msg{Kind: KindEvent, Event: &Event{Kind: EventUserText, Text: "q", Replay: true}})
	if !ok || line != ReplayPrefix+"you ▶ q" {
		t.Fatalf("got %q", line)
	}
}
