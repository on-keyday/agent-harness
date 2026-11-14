package runner

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/trsf"
)

// singleProfile builds a minimal ProfileSet with one profile ("default") at
// the given bin path and no argv templates (buildOneshotArgs/
// buildInteractiveArgs fall back to their built-in defaults). This is the
// test-local equivalent of the old Session.AgentBin single-binary field,
// now that Session carries a full ProfileSet.
func singleProfile(t *testing.T, bin string) ProfileSet {
	t.Helper()
	ps, err := NewProfileSet(AgentProfile{Name: "default", Bin: bin}, nil)
	if err != nil {
		t.Fatalf("singleProfile(%q): %v", bin, err)
	}
	return ps
}

type mockSender struct {
	mu        sync.Mutex
	sent      [][]byte
	datagrams [][]byte
}

func (m *mockSender) Send(data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, append([]byte{}, data...))
	return nil
}
func (m *mockSender) ID() objproto.ConnectionID { return objproto.ConnectionID{} }

// TestSessionResolvesProfile verifies that resolveExec picks the named
// profile's bin + argv (the codex case), and falls back to the default
// profile (index 0) when the requested profile name is empty.
func TestSessionResolvesProfile(t *testing.T) {
	ps, _ := NewProfileSet(
		AgentProfile{Name: "claude", Bin: "claude", OneshotArgv: []string{"{args}", "-p", "{prompt}"}},
		[]AgentProfile{{Name: "codex", Bin: "codex", OneshotArgv: []string{"exec", "{args}", "{prompt}"}}},
	)
	bin, argv, err := resolveExec(ps, "codex" /*oneshot*/, true, false, nil, "do it")
	if err != nil {
		t.Fatal(err)
	}
	if bin != "codex" || argv[0] != "exec" {
		t.Fatalf("got bin=%q argv=%v", bin, argv)
	}
	bin, _, _ = resolveExec(ps, "", true, false, nil, "do it")
	if bin != "claude" {
		t.Fatalf("empty→default got %q", bin)
	}
}

// fakeClaudePath returns the absolute path to testdata/fake-claude.sh from the runner package directory.
func fakeClaudePath(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("../testdata/fake-claude.sh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("fake-claude.sh missing: %v", err)
	}
	return abs
}

// writeFakeClaude creates a temporary executable script with the given body and returns its path.
func writeFakeClaude(t *testing.T, body string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "fake-claude.sh")
	fullBody := "#!/usr/bin/env bash\nset -e\n" + body + "\nexit 0\n"
	if err := os.WriteFile(script, []byte(fullBody), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// openRig serves handleOpenExec its streams: per task, a held exec stream (see
// heldStream: its reads wait, as a live client's do, so the agent runs to its
// own exit instead of being hung up on) and the prompt stream of a oneshot
// open. Register every task before running any, so the lookup is not written
// while an open reads it.
type openRig struct {
	lookup *fakeBidiLookup
	next   trsf.StreamID
}

func newOpenRig(s *Session) *openRig {
	r := &openRig{lookup: &fakeBidiLookup{
		streams: map[trsf.StreamID]trsf.BidirectionalStream{},
		recv:    map[trsf.StreamID]trsf.ReceiveStream{},
	}, next: 200}
	s.Streams = r.lookup
	return r
}

// oneshot registers a `session new --oneshot` open of prompt in repo: the
// profile's oneshot argv in the PTY, ended by the agent's own exit.
func (r *openRig) oneshot(t *testing.T, tid protocol.TaskID, repo, prompt string) *protocol.OpenExecRunnerRequest {
	t.Helper()
	streamID, promptID := r.next, r.next+1
	r.next += 2
	held := &heldStream{noopBidiStream: &noopBidiStream{streamID: streamID}, release: make(chan struct{})}
	release := time.AfterFunc(10*time.Second, func() { held.once.Do(func() { close(held.release) }) })
	t.Cleanup(func() { release.Stop(); held.once.Do(func() { close(held.release) }) })
	r.lookup.streams[streamID] = held
	r.lookup.recv[promptID] = &promptRecv{data: promptBodyBytes(prompt)}
	oer := &protocol.OpenExecRunnerRequest{TaskId: tid, StreamId: uint64(streamID)}
	oer.SetRepoPath([]byte(repo))
	oer.SetOneshot(true)
	oer.SetPromptPresent(true)
	oer.SetPromptStreamId(uint64(promptID))
	return oer
}

// lastFinished returns the last message sent, which must be a TaskFinished.
func lastFinished(t *testing.T, ms *mockSender) *protocol.TaskFinished {
	t.Helper()
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if len(ms.sent) == 0 {
		t.Fatal("nothing sent")
	}
	m := decodeRunnerMsg(t, ms.sent[len(ms.sent)-1])
	tf := m.TaskFinished()
	if m.Kind != protocol.RunnerMessageType_TaskFinished || tf == nil {
		t.Fatalf("last message kind %v, want TaskFinished", m.Kind)
	}
	return tf
}

// readOut reads what an agent wrote to out. The agents in these tests report
// through a file rather than their terminal output so the assertion does not
// depend on how the PTY's bytes are framed on the exec stream.
func readOut(t *testing.T, out string) string {
	t.Helper()
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("the agent wrote nothing to %s: %v", out, err)
	}
	return string(b)
}

func TestOneshotOpenSuccessSequence(t *testing.T) {
	repo := initRepo(t)
	ms := &mockSender{}
	s := &Session{
		AllowedRoots: []string{repo},
		Profiles:     singleProfile(t, fakeClaudePath(t)),
		Sender:       ms,
		Now:          time.Now,
	}
	rig := newOpenRig(s)
	s.handleOpenExec(context.Background(), rig.oneshot(t, protocol.TaskID{Id: [16]byte{0xAB}}, repo, "hello"))

	// TaskAccepted, TaskStarted, TaskFinished with exit_code=0.
	if len(ms.sent) < 3 {
		t.Fatalf("expected ≥3 messages, got %d", len(ms.sent))
	}
	if k := decodeRunnerMsg(t, ms.sent[0]).Kind; k != protocol.RunnerMessageType_TaskAccepted {
		t.Fatalf("first msg kind: %v", k)
	}
	if tf := lastFinished(t, ms); tf.ExitCode != 0 {
		t.Fatalf("finished: exit %d %q", tf.ExitCode, tf.ErrorMessage)
	}
	foundStarted := false
	for _, m := range ms.sent[1 : len(ms.sent)-1] {
		if decodeRunnerMsg(t, m).Kind == protocol.RunnerMessageType_TaskStarted {
			foundStarted = true
			break
		}
	}
	if !foundStarted {
		t.Fatalf("missing TaskStarted in: %v", ms.sent)
	}
}

func TestOpenExecWorktreeFailureReportsFinished(t *testing.T) {
	// A non-existent repo path; WorktreeManager.Create will fail.
	ms := &mockSender{}
	s := &Session{
		AllowedRoots: []string{"/no/such/repo"},
		Profiles:     singleProfile(t, "/bin/true"),
		Sender:       ms,
		Now:          time.Now,
	}
	rig := newOpenRig(s)
	s.handleOpenExec(context.Background(), rig.oneshot(t, protocol.TaskID{}, "/no/such/repo", "x"))

	// TaskAccepted, then TaskFinished with error info.
	if len(ms.sent) < 2 {
		t.Fatalf("expected ≥2 messages, got %d", len(ms.sent))
	}
	tf := lastFinished(t, ms)
	if tf.ExitCode == 0 {
		t.Fatalf("expected non-zero exit on worktree failure, got %+v", tf)
	}
	if !bytes.Contains(tf.ErrorMessage, []byte("worktree_error")) {
		t.Fatalf("expected 'worktree_error' in ErrorMessage, got %q", tf.ErrorMessage)
	}
}

// TestOpenExecPassesEnvToAgent verifies that HARNESS_* env vars built from
// session fields and request data are visible to the spawned agent.
func TestOpenExecPassesEnvToAgent(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	fake := writeFakeClaude(t, `echo "TICKET=$HARNESS_AUTH_TICKET" > '`+out+`'
echo "TASK=$HARNESS_TASK_ID" >> '`+out+`'`)

	repo := initRepo(t)
	ms := &mockSender{}
	sess := &Session{
		AllowedRoots: []string{repo},
		Profiles:     singleProfile(t, fake),
		Sender:       ms,
		ServerCID:    mustParseCID(t, "ws:127.0.0.1:8539-1"),
		Hostname:     "test-host",
		WSPath:       "/ws",
		Now:          time.Now,
	}
	var ticket [16]byte
	ticket[0] = 0xCD
	ticket[15] = 0xEF
	rig := newOpenRig(sess)
	oer := rig.oneshot(t, protocol.TaskID{Id: [16]byte{0xAB}}, repo, "env-test")
	oer.AuthTicket = ticket
	sess.handleOpenExec(context.Background(), oer)

	if tf := lastFinished(t, ms); tf.ExitCode != 0 {
		t.Fatalf("finished: exit %d %q", tf.ExitCode, tf.ErrorMessage)
	}
	output := readOut(t, out)
	// ticket[0]=0xCD, ticket[15]=0xEF, all others zero → 32-char hex
	expectedTicket := "TICKET=cd" + strings.Repeat("00", 14) + "ef"
	// taskID[0]=0xAB, rest zero
	expectedTask := "TASK=ab" + strings.Repeat("00", 15)
	if !strings.Contains(output, expectedTicket) {
		t.Errorf("auth ticket env not visible to the agent; output=%q want substring %q", output, expectedTicket)
	}
	if !strings.Contains(output, expectedTask) {
		t.Errorf("task id env not visible to the agent; output=%q want substring %q", output, expectedTask)
	}
}

// TestOpenExecOmitsEmptyHostname verifies that when Session.Hostname is empty
// the spawned agent gets no HARNESS_HOSTNAME at all — not an empty one. This
// tests the session-level flow, complementing the unit-level BuildAgentEnv
// test.
func TestOpenExecOmitsEmptyHostname(t *testing.T) {
	// The agent inherits the runner's environment, and a test run inside a
	// harness task has HARNESS_HOSTNAME there; take it out for this test so
	// what the agent sees is only what the session added.
	t.Setenv("HARNESS_HOSTNAME", "")
	os.Unsetenv("HARNESS_HOSTNAME")

	out := filepath.Join(t.TempDir(), "out")
	fake := writeFakeClaude(t, `echo "HOSTNAME=${HARNESS_HOSTNAME-<unset>}" > '`+out+`'`)

	repo := initRepo(t)
	ms := &mockSender{}
	sess := &Session{
		AllowedRoots: []string{repo},
		Profiles:     singleProfile(t, fake),
		Sender:       ms,
		ServerCID:    mustParseCID(t, "ws:127.0.0.1:8539-1"),
		Hostname:     "", // Empty hostname — should not be passed to the agent
		WSPath:       "/ws",
		Now:          time.Now,
	}
	rig := newOpenRig(sess)
	sess.handleOpenExec(context.Background(), rig.oneshot(t, protocol.TaskID{Id: [16]byte{0xEE}}, repo, "test-empty-hostname"))

	if tf := lastFinished(t, ms); tf.ExitCode != 0 {
		t.Fatalf("finished: exit %d %q", tf.ExitCode, tf.ErrorMessage)
	}
	if got := strings.TrimSpace(readOut(t, out)); got != "HOSTNAME=<unset>" {
		t.Errorf("empty hostname should not be passed; agent saw %q", got)
	}
}

// TestOpenExecWritesSettingsAndPropagatesEnv verifies the full open chain in
// worktree mode: (1) .claude/settings.json is written into the worktree before
// the agent starts, (2) it contains the UserPromptSubmit hook entry, (3)
// HARNESS_AUTH_TICKET is visible to the agent.
func TestOpenExecWritesSettingsAndPropagatesEnv(t *testing.T) {
	outDir := t.TempDir()
	// set -e: a missing settings.json fails the cat, and the task with it.
	fake := writeFakeClaude(t, `cat .claude/settings.json > '`+filepath.Join(outDir, "settings")+`'
echo "TICKET=$HARNESS_AUTH_TICKET" > '`+filepath.Join(outDir, "ticket")+`'`)

	repo := initRepo(t)
	ms := &mockSender{}
	sess := &Session{
		AllowedRoots: []string{repo},
		Profiles:     singleProfile(t, fake),
		Sender:       ms,
		ServerCID:    mustParseCID(t, "ws:127.0.0.1:8539-1"),
		Hostname:     "test-host",
		WSPath:       "/ws",
		Now:          time.Now,
	}
	var ticket [16]byte
	ticket[0] = 0xFE
	ticket[15] = 0xED
	rig := newOpenRig(sess)
	oer := rig.oneshot(t, protocol.TaskID{Id: [16]byte{0xAB}}, repo, "settings-smoke")
	oer.AuthTicket = ticket
	sess.handleOpenExec(context.Background(), oer)

	// (1) settings.json was there for the agent to read.
	if tf := lastFinished(t, ms); tf.ExitCode != 0 {
		t.Fatalf("settings.json not written to the worktree: exit %d %q", tf.ExitCode, tf.ErrorMessage)
	}
	// (2) settings.json contains UserPromptSubmit.
	if settings := readOut(t, filepath.Join(outDir, "settings")); !strings.Contains(settings, "UserPromptSubmit") {
		t.Errorf("settings.json missing UserPromptSubmit hook; content=%q", settings)
	}
	// (3) HARNESS_AUTH_TICKET reaches the agent.
	expectedLine := "TICKET=" + hex.EncodeToString(ticket[:])
	if got := readOut(t, filepath.Join(outDir, "ticket")); !strings.Contains(got, expectedLine) {
		t.Errorf("auth ticket env not propagated to the agent; output=%q want substring %q", got, expectedLine)
	}
}

// TestSessionGetWorktreeManagerCachesPerRepo verifies that getWorktreeManager
// returns the same *WorktreeManager for the same repo path across calls.
func TestSessionGetWorktreeManagerCachesPerRepo(t *testing.T) {
	s := &Session{}
	wm1 := s.getWorktreeManager("/repo/a")
	wm2 := s.getWorktreeManager("/repo/a")
	if wm1 != wm2 {
		t.Errorf("expected same WorktreeManager instance for same repo, got different pointers")
	}

	wm3 := s.getWorktreeManager("/repo/b")
	if wm3 == wm1 {
		t.Errorf("expected different WorktreeManager instance for different repo")
	}
}

// TestSessionRepoAllowedDelegatesToProtocol verifies that repoAllowed uses
// protocol.RootMatches — the same exact rule as the server side (G9): a repo
// below a root is refused.
func TestSessionRepoAllowedDelegatesToProtocol(t *testing.T) {
	s := &Session{AllowedRoots: []string{"/home/user/repos"}}

	cases := []struct {
		repo    string
		allowed bool
	}{
		{"/home/user/repos", true},
		{"/home/user/repos/", true},
		{"/home/user/repos/project", false},
		{"/home/user/other", false},
		{"/home/user", false},
		{"relative/path", false},
	}

	for _, tc := range cases {
		got := s.repoAllowed(tc.repo)
		if got != tc.allowed {
			t.Errorf("repoAllowed(%q) = %v, want %v", tc.repo, got, tc.allowed)
		}
	}
}

// collectTaskFinished decodes all sent messages from a mockSender and returns a
// map keyed by TaskID.Id for every TaskFinished message found.
func collectTaskFinished(t *testing.T, sent [][]byte) map[[16]byte]protocol.TaskFinished {
	t.Helper()
	result := make(map[[16]byte]protocol.TaskFinished)
	for _, raw := range sent {
		msg := decodeRunnerMsg(t, raw)
		if msg.Kind != protocol.RunnerMessageType_TaskFinished {
			continue
		}
		tf := msg.TaskFinished()
		if tf == nil {
			continue
		}
		result[tf.TaskId.Id] = *tf
	}
	return result
}

// decodeRunnerMsg parses the wire-prefixed RunnerControl payload from a Sender.Send call.
func decodeRunnerMsg(t *testing.T, raw []byte) *protocol.RunnerMessage {
	t.Helper()
	if len(raw) == 0 || raw[0] != byte(appwire.AppKind_RunnerControl) {
		t.Fatalf("expected RunnerControl prefix byte, got %v", raw)
	}
	msg := &protocol.RunnerMessage{}
	if _, err := msg.Decode(raw[1:]); err != nil {
		t.Fatalf("decode runner msg: %v", err)
	}
	return msg
}

// fakeBidiLookup is a minimal peer.BidirectionalStreamLookup implementation
// that returns pre-registered streams for the requested StreamID.
type fakeBidiLookup struct {
	streams map[trsf.StreamID]trsf.BidirectionalStream
	recv    map[trsf.StreamID]trsf.ReceiveStream
}

func (l *fakeBidiLookup) GetBidirectionalStream(id trsf.StreamID) trsf.BidirectionalStream {
	return l.streams[id]
}

func (l *fakeBidiLookup) GetReceiveStream(id trsf.StreamID) trsf.ReceiveStream {
	return l.recv[id]
}

// noopBidiStream is a minimal trsf.BidirectionalStream stub. Reads return EOF
// immediately and CloseBoth flips a flag so tests can assert teardown.
type noopBidiStream struct {
	streamID trsf.StreamID
	closed   atomic.Bool
	// sent is what AppendData carried, and lateSend whether any of it came
	// after CloseBoth — a frame written then never reaches the peer.
	mu       sync.Mutex
	sent     []byte
	lateSend bool
}

func (s *noopBidiStream) sentBytes() ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.sent...), s.lateSend
}

func (s *noopBidiStream) ID() trsf.StreamID { return s.streamID }
func (s *noopBidiStream) Write(p []byte) (int, error) {
	_ = s.AppendData(false, p)
	return len(p), nil
}
func (s *noopBidiStream) Close() error { return nil }
func (s *noopBidiStream) WriteContext(_ context.Context, p []byte) (int, error) {
	return len(p), nil
}
func (s *noopBidiStream) HasSendData() bool { return false }
func (s *noopBidiStream) Completed() bool   { return true }
func (s *noopBidiStream) AppendData(_ bool, bs ...[]byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range bs {
		s.sent = append(s.sent, b...)
	}
	if s.closed.Load() {
		s.lateSend = true
	}
	return nil
}
func (s *noopBidiStream) AppendDataContext(_ context.Context, _ bool, _ ...[]byte) error {
	return nil
}
func (s *noopBidiStream) Read([]byte) (int, error)                             { return 0, io.EOF }
func (s *noopBidiStream) ReadContext(_ context.Context, _ []byte) (int, error) { return 0, io.EOF }
func (s *noopBidiStream) ReadDirect(_ uint64) ([]byte, bool, error)            { return nil, true, nil }
func (s *noopBidiStream) ReadDirectContext(_ context.Context, _ uint64) ([]byte, bool, error) {
	return nil, true, nil
}
func (s *noopBidiStream) HasRecvData() bool { return false }
func (s *noopBidiStream) EOF() bool         { return true }
func (s *noopBidiStream) Cancel()           {}
func (s *noopBidiStream) CloseBoth() error  { s.closed.Store(true); return nil }

// TestHandleOpenExecGateFailureClosesStream verifies that when the AllowedRoots
// gate rejects an OpenExec request, the runner closes the server-allocated
// bidi stream before returning. Without this the server-side splice goroutine
// would block on ReadDirect forever and the TUI behind it would hang.
func TestHandleOpenExecGateFailureClosesStream(t *testing.T) {
	ms := &mockSender{}
	const streamID trsf.StreamID = 99
	stream := &noopBidiStream{streamID: streamID}
	lookup := &fakeBidiLookup{streams: map[trsf.StreamID]trsf.BidirectionalStream{streamID: stream}}

	s := &Session{
		AllowedRoots: []string{"/allowed"},
		Profiles:     singleProfile(t, "/bin/true"),
		Sender:       ms,
		Streams:      lookup,
		Now:          time.Now,
	}

	var taskIDBytes [16]byte
	taskIDBytes[0] = 0xDE
	oer := &protocol.OpenExecRunnerRequest{
		TaskId:   protocol.TaskID{Id: taskIDBytes},
		StreamId: uint64(streamID),
	}
	oer.SetRepoPath([]byte("/disallowed/repo"))

	s.handleOpenExec(context.Background(), oer)

	if !stream.closed.Load() {
		t.Fatal("stream.CloseBoth was not called on gate failure — server splice would hang")
	}

	// Should have sent TaskAccepted then TaskFinished with exit=-1.
	if len(ms.sent) < 2 {
		t.Fatalf("expected ≥2 messages, got %d", len(ms.sent))
	}
	last := decodeRunnerMsg(t, ms.sent[len(ms.sent)-1])
	if last.Kind != protocol.RunnerMessageType_TaskFinished {
		t.Fatalf("last kind=%v want TaskFinished", last.Kind)
	}
	tf := last.TaskFinished()
	if tf == nil || tf.ExitCode != -1 {
		t.Fatalf("TaskFinished=%+v want exit=-1", tf)
	}
	if !bytes.Contains(tf.ErrorMessage, []byte("repo_not_allowed")) {
		t.Fatalf("ErrorMessage=%q want repo_not_allowed reason", tf.ErrorMessage)
	}
	if reason := endFrameSent(t, stream); reason != string(tf.ErrorMessage) {
		t.Fatalf("End frame reason %q, want the task's %q", reason, tf.ErrorMessage)
	}
}

// TestOneshotOpen_NoWorktree_RunsInRepo verifies that with Session.NoWorktree
// the agent runs with cwd=repoPath, to its own exit, without Codex hooks, and
// that the repo dir survives the cleanup. The repoPath is a non-git tempdir,
// which would otherwise fail the worktree create step.
// TestHandleOpenExec_NoWorktree_NoGitDir pins the rest of the no-worktree
// contract (TaskStarted.WorktreeDir == repoPath, no .harness-worktrees/, no
// .claude/).
func TestOneshotOpen_NoWorktree_RunsInRepo(t *testing.T) {
	repo := t.TempDir()
	out := filepath.Join(t.TempDir(), "out")
	fake := writeFakeClaude(t, `pwd > '`+out+`'
touch harness_nw_sentinel.txt`)

	ms := &mockSender{}
	s := &Session{
		AllowedRoots: []string{repo},
		Profiles:     singleProfile(t, fake),
		Sender:       ms,
		Now:          time.Now,
		NoWorktree:   true,
	}
	rig := newOpenRig(s)
	s.handleOpenExec(context.Background(), rig.oneshot(t, protocol.TaskID{Id: [16]byte{0xBB}}, repo, "nw-no-git"))

	// (1) Last message is TaskFinished, ExitCode 0.
	if tf := lastFinished(t, ms); tf.ExitCode != 0 {
		t.Fatalf("finished: exit %d %q", tf.ExitCode, tf.ErrorMessage)
	}

	// (2) The agent ran with cwd=repoPath: it reported repo as its working
	// directory, and the sentinel it touched is in the repo directory.
	wantResolved, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(readOut(t, out)); got != repo && got != wantResolved {
		t.Fatalf("agent cwd = %q, want %q", got, repo)
	}
	if _, err := os.Stat(filepath.Join(repo, "harness_nw_sentinel.txt")); err != nil {
		t.Fatalf("sentinel file not created in repo (agent cwd was not repoPath): %v", err)
	}

	// (3) Codex settings follow the no-worktree opt-out.
	if _, err := os.Stat(filepath.Join(repo, ".codex", "hooks.json")); !os.IsNotExist(err) {
		t.Fatalf("Codex hooks injected in no-worktree mode: %v", err)
	}

	// (4) repo dir itself survives the run.
	if _, err := os.Stat(repo); err != nil {
		t.Fatalf("repo dir disappeared: %v", err)
	}
}

// TestOneshotOpen_NoWorktree_GitDir_HEADUntouched verifies that running a task
// in NoWorktree mode against a real git repo does not modify the user's HEAD,
// does not create the harness/<id> branch, and does not create a worktree dir.
func TestOneshotOpen_NoWorktree_GitDir_HEADUntouched(t *testing.T) {
	repo := initRepo(t) // git repo on branch "main"
	headBefore := readHEADRef(t, repo)

	fake := writeFakeClaude(t, `echo nw-git-test`)
	ms := &mockSender{}
	s := &Session{
		AllowedRoots: []string{repo},
		Profiles:     singleProfile(t, fake),
		Sender:       ms,
		Now:          time.Now,
		NoWorktree:   true,
	}
	var taskIDBytes [16]byte
	taskIDBytes[0] = 0xCD
	rig := newOpenRig(s)
	s.handleOpenExec(context.Background(), rig.oneshot(t, protocol.TaskID{Id: taskIDBytes}, repo, "nw-git"))

	// The agent ran, so what follows is not vacuous.
	if tf := lastFinished(t, ms); tf.ExitCode != 0 {
		t.Fatalf("finished: exit %d %q", tf.ExitCode, tf.ErrorMessage)
	}

	// HEAD ref unchanged.
	if got := readHEADRef(t, repo); got != headBefore {
		t.Errorf("HEAD changed: before=%q after=%q", headBefore, got)
	}

	// No harness/<id> branch.
	taskHex := hex.EncodeToString(taskIDBytes[:])
	if branchExists(t, repo, "harness/"+taskHex) {
		t.Errorf("branch harness/%s should not exist in NoWorktree mode", taskHex)
	}

	// No worktree dir.
	if _, err := os.Stat(filepath.Join(repo, ".harness-worktrees", taskHex)); !os.IsNotExist(err) {
		t.Errorf(".harness-worktrees/%s should not exist; stat err=%v", taskHex, err)
	}
}

// readHEADRef returns the current symbolic HEAD value of repo (e.g. "refs/heads/main").
func readHEADRef(t *testing.T, repo string) string {
	t.Helper()
	cmd := exec.Command("git", "symbolic-ref", "HEAD")
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("symbolic-ref HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// branchExists reports whether refs/heads/<name> is a valid ref in repo.
func branchExists(t *testing.T, repo, name string) bool {
	t.Helper()
	cmd := exec.Command("git", "show-ref", "--verify", "--quiet", "refs/heads/"+name)
	cmd.Dir = repo
	return cmd.Run() == nil
}

// TestOneshotOpen_NoWorktree_ConcurrentTasks verifies that two tasks opened
// concurrently on the same Session in NoWorktree mode both reach TaskFinished
// without serializing on each other (no shared worktree mutex).
func TestOneshotOpen_NoWorktree_ConcurrentTasks(t *testing.T) {
	repo := t.TempDir()
	fake := writeFakeClaude(t, `sleep 0.1; echo done`)
	ms := &mockSender{}
	s := &Session{
		AllowedRoots: []string{repo},
		Profiles:     singleProfile(t, fake),
		Sender:       ms,
		Now:          time.Now,
		NoWorktree:   true,
	}
	rig := newOpenRig(s)
	oers := []*protocol.OpenExecRunnerRequest{
		rig.oneshot(t, protocol.TaskID{Id: [16]byte{1}}, repo, "nw-concurrent"),
		rig.oneshot(t, protocol.TaskID{Id: [16]byte{2}}, repo, "nw-concurrent"),
	}

	var wg sync.WaitGroup
	for _, oer := range oers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handleOpenExec(context.Background(), oer)
		}()
	}
	wg.Wait()

	ms.mu.Lock()
	sentCopy := append([][]byte{}, ms.sent...)
	ms.mu.Unlock()
	finished := collectTaskFinished(t, sentCopy)

	for _, id := range []byte{1, 2} {
		key := [16]byte{id}
		tf, ok := finished[key]
		if !ok {
			t.Errorf("no TaskFinished for task %d", id)
			continue
		}
		if tf.ExitCode != 0 {
			t.Errorf("task %d: ExitCode=%d (want 0); err=%q", id, tf.ExitCode, tf.ErrorMessage)
		}
	}
}

// TestOneshotOpen_NoWorktree_Resume verifies that running the same task_id
// twice in NoWorktree mode does not break (no worktree state to reuse, no
// branch to re-attach — both runs simply use cwd=repoPath).
func TestOneshotOpen_NoWorktree_Resume(t *testing.T) {
	repo := t.TempDir()
	fake := writeFakeClaude(t, `echo run`)
	ms := &mockSender{}
	s := &Session{
		AllowedRoots: []string{repo},
		Profiles:     singleProfile(t, fake),
		Sender:       ms,
		Now:          time.Now,
		NoWorktree:   true,
	}
	taskID := protocol.TaskID{Id: [16]byte{0x99}}
	rig := newOpenRig(s)
	runs := []*protocol.OpenExecRunnerRequest{
		rig.oneshot(t, taskID, repo, "nw-resume"),
		rig.oneshot(t, taskID, repo, "nw-resume"),
	}
	for _, oer := range runs {
		s.handleOpenExec(context.Background(), oer)
	}

	ms.mu.Lock()
	sentCopy := append([][]byte{}, ms.sent...)
	ms.mu.Unlock()

	// Count TaskFinished messages for this task — expect exactly 2, both ExitCode 0.
	var finishedCount, okCount int
	for _, raw := range sentCopy {
		m := decodeRunnerMsg(t, raw)
		if m.Kind != protocol.RunnerMessageType_TaskFinished {
			continue
		}
		tf := m.TaskFinished()
		if tf == nil || tf.TaskId.Id != taskID.Id {
			continue
		}
		finishedCount++
		if tf.ExitCode == 0 {
			okCount++
		}
	}
	if finishedCount != 2 || okCount != 2 {
		t.Fatalf("expected 2 successful TaskFinished, got finished=%d ok=%d", finishedCount, okCount)
	}
}

// TestHandleOpenExec_NoWorktree_NoGitDir is the no-worktree check for a plain
// interactive open (TestOneshotOpen_NoWorktree_RunsInRepo runs an agent to its
// exit). Uses /bin/true as AgentBin and the existing
// noopBidiStream so the exec terminates immediately; we only verify the
// runner-side wiring (no worktree create error, WorktreeDir == repoPath, no
// .harness-worktrees/, no .claude/).
func TestHandleOpenExec_NoWorktree_NoGitDir(t *testing.T) {
	repo := t.TempDir() // non-git on purpose
	ms := &mockSender{}
	const streamID trsf.StreamID = 100
	stream := &noopBidiStream{streamID: streamID}
	lookup := &fakeBidiLookup{streams: map[trsf.StreamID]trsf.BidirectionalStream{streamID: stream}}

	s := &Session{
		AllowedRoots: []string{repo},
		Profiles:     singleProfile(t, "/bin/true"),
		Sender:       ms,
		Streams:      lookup,
		Now:          time.Now,
		NoWorktree:   true,
	}
	var taskIDBytes [16]byte
	taskIDBytes[0] = 0xBE
	oer := &protocol.OpenExecRunnerRequest{
		TaskId:   protocol.TaskID{Id: taskIDBytes},
		StreamId: uint64(streamID),
	}
	oer.SetRepoPath([]byte(repo))

	s.handleOpenExec(context.Background(), oer)

	// (1) Last message is TaskFinished (success or non-zero — both mean "we got past worktree create").
	if len(ms.sent) < 3 {
		t.Fatalf("expected ≥3 messages, got %d", len(ms.sent))
	}
	last := decodeRunnerMsg(t, ms.sent[len(ms.sent)-1])
	if last.Kind != protocol.RunnerMessageType_TaskFinished {
		t.Fatalf("last msg kind: %v", last.Kind)
	}
	tf := last.TaskFinished()
	if tf == nil {
		t.Fatal("TaskFinished missing payload")
	}
	if bytes.Contains(tf.ErrorMessage, []byte("worktree_error")) {
		t.Errorf("got worktree_error in NoWorktree mode: %q", tf.ErrorMessage)
	}

	// (2) TaskStarted.WorktreeDir == repoPath.
	var startedDir string
	for _, raw := range ms.sent {
		m := decodeRunnerMsg(t, raw)
		if m.Kind == protocol.RunnerMessageType_TaskStarted {
			ts := m.TaskStarted()
			if ts != nil {
				startedDir = string(ts.WorktreeDir)
			}
		}
	}
	if startedDir != repo {
		t.Errorf("TaskStarted.WorktreeDir: got %q, want %q", startedDir, repo)
	}

	// (3) No worktree dir created.
	if _, err := os.Stat(filepath.Join(repo, ".harness-worktrees")); !os.IsNotExist(err) {
		t.Errorf(".harness-worktrees should not exist; stat err=%v", err)
	}

	// (4) No .claude/ injection.
	if _, err := os.Stat(filepath.Join(repo, ".claude")); !os.IsNotExist(err) {
		t.Errorf(".claude/ should not exist in NoWorktree without force-inject; stat err=%v", err)
	}
}

// TestOneshotOpen_NoWorktree_ForceInject verifies that with both
// NoWorktree=true and ForceInjectHarnessSettings=true, the runner writes
// .claude/settings.json (with harness-cli hooks) and .claude/skills/
// directly into repoPath. Cleanup is still skipped — the injected files
// persist past task end.
func TestOneshotOpen_NoWorktree_ForceInject(t *testing.T) {
	repo := t.TempDir()
	fake := writeFakeClaude(t, `echo force-inject`)
	ms := &mockSender{}
	s := &Session{
		AllowedRoots:               []string{repo},
		Profiles:                   singleProfile(t, fake),
		Sender:                     ms,
		Now:                        time.Now,
		NoWorktree:                 true,
		ForceInjectHarnessSettings: true,
	}
	rig := newOpenRig(s)
	s.handleOpenExec(context.Background(), rig.oneshot(t, protocol.TaskID{Id: [16]byte{0xEE}}, repo, "nw-force-inject"))

	// (1) settings.json exists in repoPath.
	settingsPath := filepath.Join(repo, ".claude", "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("expected settings.json at %s, got err=%v", settingsPath, err)
	}
	// settings.json carries the harness-cli command prefix (any matching hook is fine).
	if !strings.Contains(string(data), "harness-cli ") {
		t.Errorf("settings.json missing harness-cli hook commands; content=%s", data)
	}

	// (2) skills dir exists with at least one entry.
	skillsDir := filepath.Join(repo, ".claude", "skills")
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		t.Fatalf("expected skills dir at %s, got err=%v", skillsDir, err)
	}
	if len(entries) == 0 {
		t.Errorf("skills dir is empty; expected at least one harness skill")
	}

	// (3) No worktree dir despite force-inject.
	if _, err := os.Stat(filepath.Join(repo, ".harness-worktrees")); !os.IsNotExist(err) {
		t.Errorf(".harness-worktrees should not exist in NoWorktree mode; stat err=%v", err)
	}
}

// The datagram half of Sender. Recorded rather than dropped: a test that wants
// to assert what a udp forward sent back has nowhere else to look, and a
// silent stub would make "it sent nothing" and "it sent into a void"
// indistinguishable. The size answers a realistic value rather than 0, so a
// caller comparing a payload against it is not told everything is oversized.
func (m *mockSender) SendDatagram(b []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.datagrams = append(m.datagrams, append([]byte(nil), b...))
	return nil
}

func (m *mockSender) MaxDatagramSize() int { return 1169 }
