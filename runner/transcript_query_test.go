package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/runner/transcript"
	"github.com/on-keyday/objtrsf/trsf"
)

// transcriptFixture is a runner with a claude and a codex profile whose
// stores live under temp dirs, and a repo it serves.
type transcriptFixture struct {
	s          *Session
	repo, task string
	claude     string // claude config dir
	codex      string // CODEX_HOME
}

func newTranscriptFixture(t *testing.T) *transcriptFixture {
	t.Helper()
	f := &transcriptFixture{repo: t.TempDir(), claude: t.TempDir(), codex: t.TempDir()}
	f.task = strings.Repeat("ab", 16)
	ps, err := NewProfileSet(AgentProfile{Name: "claude", Bin: "claude", TranscriptStore: "claude"},
		[]AgentProfile{
			{Name: "codex", Bin: "codex", TranscriptStore: "codex"},
			{Name: "sandbox-claude", Bin: "x", TranscriptStore: "claude"}, // same store: asked once
			{Name: "bash", Bin: "bash"},
		})
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{"CLAUDE_CONFIG_DIR": f.claude, "CODEX_HOME": f.codex}
	f.s = &Session{Profiles: ps, AllowedRoots: []string{f.repo}, transcriptEnv: func(k string) string { return vars[k] }}
	return f
}

func (f *transcriptFixture) worktree() string {
	return filepath.Join(f.repo, ".harness-worktrees", f.task)
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *transcriptFixture) claudeSession(t *testing.T, dir, id, extra string) {
	t.Helper()
	p := filepath.Join(f.claude, "projects", transcript.ClaudeProjectDirName(dir), id+".jsonl")
	writeFile(t, p, `{"cwd":"`+dir+`","timestamp":"2026-11-14T00:00:00Z"}`+"\n"+extra)
}

func (f *transcriptFixture) codexSession(t *testing.T, dir, id string) {
	t.Helper()
	p := filepath.Join(f.codex, "sessions", "2026", "11", "14", "rollout-2026-11-14T00-00-00-"+id+".jsonl")
	writeFile(t, p, `{"type":"session_meta","timestamp":"2026-11-14T00:00:00Z","payload":{"id":"`+id+`","cwd":"`+dir+`"}}`+"\n")
}

func (f *transcriptFixture) req(op protocol.TranscriptOp, session, file string, includeShared bool) *protocol.RunnerTranscriptRequest {
	r := &protocol.RunnerTranscriptRequest{Op: op}
	b, _ := hex.DecodeString(f.task)
	copy(r.TaskId.Id[:], b)
	r.SetRepoPath([]byte(f.repo))
	r.SetSession([]byte(session))
	r.SetFile([]byte(file))
	r.SetIncludeShared(includeShared)
	return r
}

// answer runs the request and returns the decoded header and the raw rest.
func (f *transcriptFixture) answer(t *testing.T, r *protocol.RunnerTranscriptRequest) (protocol.TranscriptResult, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if err := f.s.writeTranscriptAnswer(r, &buf); err != nil {
		t.Fatal(err)
	}
	rd := bytes.NewReader(buf.Bytes())
	var res protocol.TranscriptResult
	if err := res.Read(rd); err != nil {
		t.Fatalf("decode header: %v", err)
	}
	rest, _ := io.ReadAll(rd)
	return res, rest
}

func sessionIDs(res protocol.TranscriptResult) []string {
	var out []string
	if b := res.Sessions(); b != nil {
		for _, s := range b.Sessions {
			out = append(out, string(s.Agent)+":"+string(s.Id))
		}
	}
	sort.Strings(out)
	return out
}

// Every agent the runner knows is asked, each store once (B-D1).
func TestTranscriptSessionsAcrossAgents(t *testing.T) {
	f := newTranscriptFixture(t)
	f.claudeSession(t, f.worktree(), "s1", "")
	f.codexSession(t, f.worktree(), "c1")
	f.claudeSession(t, "/elsewhere", "s9", "")
	res, _ := f.answer(t, f.req(protocol.TranscriptOp_Sessions, "", "", false))
	if res.Status != protocol.TranscriptStatus_Ok {
		t.Fatalf("status %v %q", res.Status, res.Message)
	}
	if got := strings.Join(sessionIDs(res), ","); got != "claude:s1,codex:c1" {
		t.Fatalf("sessions %s", got)
	}
}

// Review Focus 1: an id that exists, but in another task's directory, is not
// served.
func TestTranscriptForeignSessionIsNoSuchSession(t *testing.T) {
	f := newTranscriptFixture(t)
	f.claudeSession(t, f.worktree(), "s1", "")
	f.claudeSession(t, filepath.Join(f.repo, ".harness-worktrees", strings.Repeat("cd", 16)), "theirs", "")
	for _, op := range []protocol.TranscriptOp{protocol.TranscriptOp_Files, protocol.TranscriptOp_Get} {
		res, rest := f.answer(t, f.req(op, "theirs", "", false))
		if res.Status != protocol.TranscriptStatus_NoSuchSession || len(rest) != 0 {
			t.Fatalf("op %v: status %v, %d bytes; want no_such_session and nothing", op, res.Status, len(rest))
		}
	}
}

// A --no-worktree task's directory is the repo, shared with others: its
// sessions are listed only to a caller the server says may read them
// (include_shared), and marked shared (B-D2).
func TestTranscriptSharedHiddenWithoutIncludeShared(t *testing.T) {
	f := newTranscriptFixture(t)
	f.s.NoWorktree = true
	f.claudeSession(t, f.repo, "s1", "")
	res, _ := f.answer(t, f.req(protocol.TranscriptOp_Sessions, "", "", false))
	if res.Status != protocol.TranscriptStatus_Ok || len(sessionIDs(res)) != 0 || len(res.Message) == 0 {
		t.Fatalf("without include_shared: status %v rows %v msg %q; want ok, none, and a message saying why", res.Status, sessionIDs(res), res.Message)
	}
	if res, _ := f.answer(t, f.req(protocol.TranscriptOp_Get, "s1", "s1.jsonl", false)); res.Status != protocol.TranscriptStatus_NoSuchSession {
		t.Fatalf("get without include_shared: %v", res.Status)
	}
	res, _ = f.answer(t, f.req(protocol.TranscriptOp_Sessions, "", "", true))
	rows := res.Sessions()
	if rows == nil || len(rows.Sessions) != 1 || !rows.Sessions[0].Shared() {
		t.Fatalf("with include_shared: %+v, want s1 marked shared", rows)
	}
}

// A whole-session get is a tar of exactly the files the listing names.
func TestTranscriptWholeSessionIsTar(t *testing.T) {
	f := newTranscriptFixture(t)
	f.claudeSession(t, f.worktree(), "s1", "{}\n")
	writeFile(t, filepath.Join(f.claude, "projects", transcript.ClaudeProjectDirName(f.worktree()), "s1", "subagents", "a.jsonl"), "sub")
	files, _ := f.answer(t, f.req(protocol.TranscriptOp_Files, "s1", "", false))
	var listed []string
	for _, fl := range files.Files().Files {
		listed = append(listed, string(fl.Name))
	}
	res, rest := f.answer(t, f.req(protocol.TranscriptOp_Get, "s1", "", false))
	if res.Status != protocol.TranscriptStatus_Ok {
		t.Fatalf("status %v %q", res.Status, res.Message)
	}
	if g := res.Get(); g == nil || g.Size != uint64(len(rest)) {
		t.Fatalf("get header %+v, %d bytes followed: the size must count exactly what follows", g, len(rest))
	}
	tr := tar.NewReader(bytes.NewReader(rest))
	var inTar []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		inTar = append(inTar, h.Name)
	}
	sort.Strings(listed)
	sort.Strings(inTar)
	if strings.Join(inTar, ",") != strings.Join(listed, ",") || len(inTar) != 2 {
		t.Fatalf("tar %v, listing %v", inTar, listed)
	}
}

func TestTranscriptOneFile(t *testing.T) {
	f := newTranscriptFixture(t)
	f.claudeSession(t, f.worktree(), "s1", "LINE2\n")
	res, rest := f.answer(t, f.req(protocol.TranscriptOp_Get, "s1", "s1.jsonl", false))
	if res.Status != protocol.TranscriptStatus_Ok || !strings.Contains(string(rest), "LINE2") {
		t.Fatalf("status %v rest %q", res.Status, rest)
	}
	if g := res.Get(); g == nil || g.Size != uint64(len(rest)) {
		t.Fatalf("get header %+v, %d bytes followed", g, len(rest))
	}
	if res, _ := f.answer(t, f.req(protocol.TranscriptOp_Get, "s1", "../x", false)); res.Status != protocol.TranscriptStatus_NoSuchFile {
		t.Fatalf("an unlisted name: %v", res.Status)
	}
}

// A repo outside the runner's roots is refused before any store is read, as
// git queries re-validate it.
func TestTranscriptRepoOutsideRootsRefused(t *testing.T) {
	f := newTranscriptFixture(t)
	r := f.req(protocol.TranscriptOp_Sessions, "", "", false)
	r.SetRepoPath([]byte("/not/a/root"))
	if res, _ := f.answer(t, r); res.Status != protocol.TranscriptStatus_StoreError {
		t.Fatalf("status %v", res.Status)
	}
}

// Bounds (plan Global Constraints): an over-long session id or file name is
// refused, not looked up.
func TestTranscriptBounds(t *testing.T) {
	f := newTranscriptFixture(t)
	// The bound answers before any lookup, and says so: a lookup would give
	// the same status for an id that matches nothing.
	if res, _ := f.answer(t, f.req(protocol.TranscriptOp_Files, strings.Repeat("s", 129), "", false)); res.Status != protocol.TranscriptStatus_NoSuchSession || !strings.Contains(string(res.Message), "too long") {
		t.Fatalf("long session: %v %q", res.Status, res.Message)
	}
	f.claudeSession(t, f.worktree(), "s1", "")
	if res, _ := f.answer(t, f.req(protocol.TranscriptOp_Get, "s1", strings.Repeat("f", 513), false)); res.Status != protocol.TranscriptStatus_NoSuchFile || !strings.Contains(string(res.Message), "too long") {
		t.Fatalf("long file: %v %q", res.Status, res.Message)
	}
}

// Review Focus 4: a large transcript is streamed, never held whole.
func TestTranscriptStreamsLargeFile(t *testing.T) {
	f := newTranscriptFixture(t)
	f.claudeSession(t, f.worktree(), "big", "")
	p := filepath.Join(f.claude, "projects", transcript.ClaudeProjectDirName(f.worktree()), "big.jsonl")
	fh, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	const size = 100 << 20
	if err := fh.Truncate(size); err != nil {
		t.Fatal(err)
	}
	fh.Close()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	var n countWriter
	if err := f.s.writeTranscriptAnswer(f.req(protocol.TranscriptOp_Get, "big", "big.jsonl", false), &n); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if n < size {
		t.Fatalf("wrote %d bytes, want at least %d", n, size)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 8<<20 {
		t.Fatalf("allocated %d bytes to stream %d: the file was held, not streamed", grew, size)
	}
}

type countWriter int64

func (c *countWriter) Write(p []byte) (int, error) { *c += countWriter(len(p)); return len(p), nil }

// The request arrives on a stream the server allocated: the answer is written
// there and the stream closed.
func TestHandleTranscriptWritesToTheStream(t *testing.T) {
	f := newTranscriptFixture(t)
	f.claudeSession(t, f.worktree(), "s1", "")
	const sid trsf.StreamID = 41
	stream := &noopBidiStream{streamID: sid}
	f.s.Streams = &fakeBidiLookup{streams: map[trsf.StreamID]trsf.BidirectionalStream{sid: stream}}
	r := f.req(protocol.TranscriptOp_Sessions, "", "", false)
	r.StreamId = uint64(sid)
	f.s.handleTranscript(context.Background(), r)
	sent, _ := stream.sentBytes()
	var res protocol.TranscriptResult
	if err := res.Read(bytes.NewReader(sent)); err != nil || len(sessionIDs(res)) != 1 || !stream.closed.Load() {
		t.Fatalf("err %v rows %v closed %v", err, sessionIDs(res), stream.closed.Load())
	}
}

func TestProfileTranscriptStoreFromJSON(t *testing.T) {
	ps, err := ParseAgentProfilesJSON(`[{"name":"codex","bin":"codex","transcriptStore":"codex"}]`)
	if err != nil || len(ps) != 1 || ps[0].TranscriptStore != "codex" {
		t.Fatalf("parsed %+v err %v", ps, err)
	}
	set, _ := NewProfileSet(AgentProfile{Name: "d", Bin: "x", TranscriptStore: "nope"}, ps)
	if bad := set.UnrecognisedTranscriptStores(); len(bad) != 1 || !strings.Contains(bad[0], "nope") {
		t.Fatalf("unrecognised %v", bad)
	}
}

// A file that grows while it is read (a live session) is sent as it was when
// opened: the header's size and the bytes that follow agree (Review Focus 3
// and 5).
func TestTranscriptGetOfAGrowingFileMatchesItsSize(t *testing.T) {
	f := newTranscriptFixture(t)
	f.claudeSession(t, f.worktree(), "live", "")
	p := filepath.Join(f.claude, "projects", transcript.ClaudeProjectDirName(f.worktree()), "live.jsonl")
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		fh, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
		defer fh.Close()
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = fh.WriteString("{\"type\":\"x\"}\n")
			}
		}
	}()
	res, rest := f.answer(t, f.req(protocol.TranscriptOp_Get, "live", "live.jsonl", false))
	close(stop)
	<-done
	if res.Status != protocol.TranscriptStatus_Ok || res.Get() == nil || res.Get().Size != uint64(len(rest)) {
		t.Fatalf("status %v header %+v, %d bytes followed", res.Status, res.Get(), len(rest))
	}
}
