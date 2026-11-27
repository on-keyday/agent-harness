package transcript

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func ids(ss []Session) []string {
	var out []string
	for _, s := range ss {
		out = append(out, s.ID)
	}
	sort.Strings(out)
	return out
}

func names(fs []File) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Name)
	}
	sort.Strings(out)
	return out
}

// claude: a session is listed when the cwd it records is the task's
// directory, not merely because its file sits under the encoded name — a
// truncated name can be shared by another directory (B-F2, B-D3).
func TestClaudeSessionsMatchRecordedCwd(t *testing.T) {
	root := t.TempDir()
	dir := "/w/repo/.harness-worktrees/aaaa"
	other := "/w/repo/.harness-worktrees/aaab"
	proj := filepath.Join(root, "projects", ClaudeProjectDirName(dir))
	line := func(cwd, id string) string {
		return `{"type":"user","cwd":"` + cwd + `","sessionId":"` + id + `","timestamp":"2026-11-14T01:02:03.000Z"}` + "\n"
	}
	write(t, filepath.Join(proj, "s1.jsonl"), `{"type":"summary"}`+"\n"+line(dir, "s1"))
	write(t, filepath.Join(proj, "s2.jsonl"), line(dir, "s2"))
	write(t, filepath.Join(proj, "s3.jsonl"), line(other, "s3")) // same folder, another directory
	st, err := New("claude", env(map[string]string{"CLAUDE_CONFIG_DIR": root}), "")
	if err != nil {
		t.Fatal(err)
	}
	ss, err := st.Sessions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(ss); strings.Join(got, ",") != "s1,s2" {
		t.Fatalf("sessions %v, want s1,s2", got)
	}
	for _, s := range ss {
		if s.Agent != "claude" || s.Size == 0 || !s.SizeKnown || s.Created.IsZero() || s.Updated.IsZero() {
			t.Fatalf("row %+v lacks agent, size or times", s)
		}
	}
	if want := time.Date(2026, 11, 14, 1, 2, 3, 0, time.UTC); !ss[0].Created.Equal(want) && !ss[1].Created.Equal(want) {
		t.Fatalf("created %v / %v, want the first record's timestamp %v", ss[0].Created, ss[1].Created, want)
	}
}

// A long worktree path is truncated by claude; the listing still matches on
// the cwd inside, so another task under the same truncated prefix is not
// listed (B-F2).
func TestClaudeTruncatedNameIsCheckedInside(t *testing.T) {
	root := t.TempDir()
	long := "/" + strings.Repeat("x", 220) + "/.harness-worktrees/aaaa"
	longOther := "/" + strings.Repeat("x", 220) + "/.harness-worktrees/bbbb"
	enc := ClaudeProjectDirName(long)
	write(t, filepath.Join(root, "projects", enc[:ClaudeProjectDirNameLimit]+"-h1", "mine.jsonl"), `{"cwd":"`+long+`"}`+"\n")
	write(t, filepath.Join(root, "projects", enc[:ClaudeProjectDirNameLimit]+"-h2", "theirs.jsonl"), `{"cwd":"`+longOther+`"}`+"\n")
	st, _ := New("claude", env(map[string]string{"CLAUDE_CONFIG_DIR": root}), "")
	ss, err := st.Sessions(long)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(ss); strings.Join(got, ",") != "mine" {
		t.Fatalf("sessions %v, want only mine", got)
	}
}

// A claude session is its main file plus the subagent and tool-result files
// kept beside it (B-F1a).
func TestClaudeFilesIncludeSubagentsAndToolResults(t *testing.T) {
	root := t.TempDir()
	dir := "/w/r/.harness-worktrees/cccc"
	proj := filepath.Join(root, "projects", ClaudeProjectDirName(dir))
	write(t, filepath.Join(proj, "s1.jsonl"), `{"cwd":"`+dir+`"}`+"\n")
	write(t, filepath.Join(proj, "s1", "subagents", "a.jsonl"), "{}\n")
	write(t, filepath.Join(proj, "s1", "tool-results", "t.txt"), "out")
	st, _ := New("claude", env(map[string]string{"CLAUDE_CONFIG_DIR": root}), "")
	fs, err := st.Files(dir, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(fs), ","); got != "s1.jsonl,s1/subagents/a.jsonl,s1/tool-results/t.txt" {
		t.Fatalf("files %s", got)
	}
	for _, f := range fs {
		if !f.SizeKnown || f.Size == 0 {
			t.Fatalf("file %+v has no size", f)
		}
	}
}

// codex stores by date; the session_meta line's payload.cwd decides.
func TestCodexSessionsFilterByMetaCwd(t *testing.T) {
	root := t.TempDir()
	dir := "/w/r/.harness-worktrees/dddd"
	meta := func(id, cwd string) string {
		return `{"timestamp":"2026-11-14T00:00:00Z","type":"session_meta","payload":{"id":"` + id + `","cwd":"` + cwd + `"}}` + "\n"
	}
	write(t, filepath.Join(root, "sessions", "2026", "11", "14", "rollout-2026-11-14T00-00-00-c1.jsonl"), meta("c1", dir)+`{"type":"x"}`+"\n")
	write(t, filepath.Join(root, "sessions", "2026", "11", "13", "rollout-2026-11-13T00-00-00-c2.jsonl"), meta("c2", "/elsewhere"))
	st, err := New("codex", env(map[string]string{"CODEX_HOME": root}), "")
	if err != nil {
		t.Fatal(err)
	}
	ss, err := st.Sessions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(ss); strings.Join(got, ",") != "c1" {
		t.Fatalf("sessions %v, want c1", got)
	}
	fs, err := st.Files(dir, "c1")
	if err != nil || len(fs) != 1 || fs[0].Name != "rollout-2026-11-14T00-00-00-c1.jsonl" {
		t.Fatalf("files %+v err %v", fs, err)
	}
}

// pi: the first line is the session header, and its cwd decides.
func TestPiSessionsFilterByHeaderCwd(t *testing.T) {
	root := t.TempDir()
	dir := "/w/r/.harness-worktrees/eeee"
	hdr := func(id, cwd string) string {
		return `{"type":"session","version":3,"id":"` + id + `","timestamp":"2026-11-14T00:00:00.000Z","cwd":"` + cwd + `"}` + "\n"
	}
	write(t, filepath.Join(root, "--w-r-.harness-worktrees-eeee--", "2026-11-14_p1.jsonl"), hdr("p1", dir))
	write(t, filepath.Join(root, "--elsewhere--", "2026-11-14_p2.jsonl"), hdr("p2", "/elsewhere"))
	st, err := New("pi", env(map[string]string{"PI_CODING_AGENT_SESSION_DIR": root}), "")
	if err != nil {
		t.Fatal(err)
	}
	ss, err := st.Sessions(dir)
	if err != nil || strings.Join(ids(ss), ",") != "p1" {
		t.Fatalf("sessions %v err %v, want p1", ids(ss), err)
	}
}

// pi's session directory: PI_CODING_AGENT_SESSION_DIR, else
// $PI_CODING_AGENT_DIR/sessions (B-F8).
func TestPiSessionDirFromAgentDir(t *testing.T) {
	root := t.TempDir()
	dir := "/w/r/.harness-worktrees/ffff"
	write(t, filepath.Join(root, "sessions", "--x--", "a_p9.jsonl"), `{"type":"session","id":"p9","cwd":"`+dir+`"}`+"\n")
	st, _ := New("pi", env(map[string]string{"PI_CODING_AGENT_DIR": root}), "")
	ss, err := st.Sessions(dir)
	if err != nil || strings.Join(ids(ss), ",") != "p9" {
		t.Fatalf("sessions %v err %v, want p9", ids(ss), err)
	}
}

// Review Focus 1 and 2: only a name the session's listing produced is
// opened, and only for a session recorded in the directory asked about.
func TestOpenRefusesNamesNotListed(t *testing.T) {
	root := t.TempDir()
	dir := "/w/r/.harness-worktrees/gggg"
	proj := filepath.Join(root, "projects", ClaudeProjectDirName(dir))
	write(t, filepath.Join(proj, "s1.jsonl"), `{"cwd":"`+dir+`"}`+"\n")
	write(t, filepath.Join(proj, "s2.jsonl"), `{"cwd":"/elsewhere"}`+"\n")
	write(t, filepath.Join(root, "secret.txt"), "no")
	st, _ := New("claude", env(map[string]string{"CLAUDE_CONFIG_DIR": root}), "")
	for _, name := range []string{"../../secret.txt", filepath.Join(root, "secret.txt"), "s2.jsonl", "s1/../s2.jsonl", ""} {
		if _, err := st.Open(dir, "s1", name); !errors.Is(err, ErrNoFile) {
			t.Fatalf("Open(%q) err %v, want ErrNoFile", name, err)
		}
	}
	if _, err := st.Files(dir, "s2"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("Files of a session recorded elsewhere: err %v, want ErrNoSession", err)
	}
	if _, err := st.Open(dir, "s2", "s2.jsonl"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("Open of a session recorded elsewhere: err %v, want ErrNoSession", err)
	}
	rc, err := st.Open(dir, "s1", "s1.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if !strings.Contains(string(b), dir) {
		t.Fatalf("read %q", b)
	}
}

// Review Focus 3: a file still being written is read to its end at the time
// of reading, without error.
func TestOpenWhileWritten(t *testing.T) {
	root := t.TempDir()
	dir := "/w/r/.harness-worktrees/hhhh"
	proj := filepath.Join(root, "projects", ClaudeProjectDirName(dir))
	path := filepath.Join(proj, "s1.jsonl")
	first := `{"cwd":"` + dir + `"}` + "\n"
	write(t, path, first)
	st, _ := New("claude", env(map[string]string{"CLAUDE_CONFIG_DIR": root}), "")
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		defer f.Close()
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = f.WriteString(`{"type":"x"}` + "\n")
			}
		}
	}()
	rc, err := st.Open(dir, "s1", "s1.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(rc)
	rc.Close()
	close(stop)
	<-done
	if err != nil || !strings.HasPrefix(string(b), first) {
		t.Fatalf("read err %v, %d bytes", err, len(b))
	}
}

func TestSamePath(t *testing.T) {
	for _, c := range []struct {
		a, b    string
		windows bool
		want    bool
	}{
		{"/a/b", "/a/b/", false, true},
		{"/a/b", "/a/./b", false, true},
		{"/a/B", "/a/b", false, false},
		{`C:\A\b`, "c:/a/b", true, true},
		{`C:\a\b`, `C:\a\c`, true, false},
	} {
		if got := samePath(c.a, c.b, c.windows); got != c.want {
			t.Fatalf("samePath(%q, %q, windows=%v) = %v, want %v", c.a, c.b, c.windows, got, c.want)
		}
	}
}

func TestNewRefusesUnknownKind(t *testing.T) {
	if _, err := New("bash", env(nil), ""); err == nil {
		t.Fatal("a kind with no store was accepted")
	}
}
