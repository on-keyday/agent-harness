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

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/topics"
	"github.com/on-keyday/agent-harness/trsf"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

type mockSender struct {
	mu        sync.Mutex
	sent      [][]byte
	publishes []publishedMsg
}

type publishedMsg struct {
	topic string
	data  []byte
}

func (m *mockSender) Send(data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, append([]byte{}, data...))
	return nil
}
func (m *mockSender) ID() objproto.ConnectionID { return objproto.ConnectionID{} }
func (m *mockSender) Publish(topic string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.publishes = append(m.publishes, publishedMsg{topic, append([]byte{}, data...)})
	return nil
}

func TestHandleAssignSuccessSequence(t *testing.T) {
	repo := initRepo(t)
	ms := &mockSender{}
	fakePath, _ := filepath.Abs("../testdata/fake-claude.sh") // relative from runner/
	s := &Session{
		AllowedRoots: []string{repo},
		ClaudeBin:    fakePath,
		Timeout:      5 * time.Second,
		Sender:       ms,
		Now:          time.Now,
	}
	var taskIDBytes [16]byte
	taskIDBytes[0] = 0xAB
	req := &protocol.AssignTask{
		TaskId: protocol.TaskID{Id: taskIDBytes},
		Prompt: []byte("hello"),
	}
	req.SetRepoPath([]byte(repo))
	s.handleAssign(context.Background(), req)

	// Should have sent at least 3 control messages: Accepted, Started, Finished.
	if len(ms.sent) < 3 {
		t.Fatalf("expected ≥3 messages, got %d", len(ms.sent))
	}

	// First should be TaskAccepted.
	accepted := decodeRunnerMsg(t, ms.sent[0])
	if accepted.Kind != protocol.RunnerMessageType_TaskAccepted {
		t.Fatalf("first msg kind: %v", accepted.Kind)
	}

	// Last should be TaskFinished with exit_code=0.
	last := decodeRunnerMsg(t, ms.sent[len(ms.sent)-1])
	if last.Kind != protocol.RunnerMessageType_TaskFinished {
		t.Fatalf("last msg kind: %v", last.Kind)
	}
	tf := last.TaskFinished()
	if tf == nil || tf.ExitCode != 0 {
		t.Fatalf("finished: %+v", tf)
	}

	// Some TaskStarted in between.
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

	// Logs published to the right topic.
	expectedTopic := topics.TaskLog("ab" + strings.Repeat("00", 15))
	if len(ms.publishes) == 0 {
		t.Fatalf("expected log publishes, got none")
	}
	for _, p := range ms.publishes {
		if p.topic != expectedTopic {
			t.Fatalf("unexpected topic %q (want %q)", p.topic, expectedTopic)
		}
	}
	// Some chunk should contain the prompt echo.
	var combined []byte
	for _, p := range ms.publishes {
		combined = append(combined, p.data...)
	}
	if !strings.Contains(string(combined), "hello") {
		t.Fatalf("prompt not echoed in logs: %q", combined)
	}
}

func TestHandleAssignWorktreeFailureReportsFinished(t *testing.T) {
	// Use a non-existent repo path; WorktreeManager.Create will fail.
	ms := &mockSender{}
	s := &Session{
		AllowedRoots: []string{"/no/such/repo"},
		ClaudeBin:    "/bin/true",
		Timeout:      1 * time.Second,
		Sender:       ms,
		Now:          time.Now,
	}
	req := &protocol.AssignTask{TaskId: protocol.TaskID{}, Prompt: []byte("x")}
	req.SetRepoPath([]byte("/no/such/repo"))
	s.handleAssign(context.Background(), req)

	// Should have sent: TaskAccepted, then TaskFinished with error info.
	if len(ms.sent) < 2 {
		t.Fatalf("expected ≥2 messages, got %d", len(ms.sent))
	}
	last := decodeRunnerMsg(t, ms.sent[len(ms.sent)-1])
	if last.Kind != protocol.RunnerMessageType_TaskFinished {
		t.Fatalf("last kind: %v", last.Kind)
	}
	tf := last.TaskFinished()
	if tf == nil || tf.ExitCode == 0 {
		t.Fatalf("expected non-zero exit on worktree failure, got %+v", tf)
	}
	if !bytes.Contains(tf.ErrorMessage, []byte("worktree_error")) {
		t.Fatalf("expected 'worktree_error' in ErrorMessage, got %q", tf.ErrorMessage)
	}
}

// TestHandleAssignPanicSendsTaskFinished verifies that a panic inside handleAssign
// is recovered and reported as a TaskFinished message so the server doesn't wait
// forever. Uses the testHookHandleAssign seam to inject the panic.
func TestHandleAssignPanicSendsTaskFinished(t *testing.T) {
	ms := &mockSender{}
	s := &Session{
		AllowedRoots: []string{"/some/repo"},
		ClaudeBin:    "/bin/true",
		Timeout:      1 * time.Second,
		Sender:       ms,
		Now:          time.Now,
		testHookHandleAssign: func() {
			panic("injected test panic")
		},
	}

	var taskIDBytes [16]byte
	taskIDBytes[0] = 0xCC
	req := &protocol.AssignTask{
		TaskId: protocol.TaskID{Id: taskIDBytes},
		Prompt: []byte("test"),
	}
	req.SetRepoPath([]byte("/some/repo"))

	// handleAssign should not re-panic; it should send Accepted then Finished.
	s.handleAssign(context.Background(), req)

	// Must have sent at least TaskAccepted + TaskFinished.
	if len(ms.sent) < 2 {
		t.Fatalf("expected ≥2 messages (Accepted + Finished), got %d", len(ms.sent))
	}

	first := decodeRunnerMsg(t, ms.sent[0])
	if first.Kind != protocol.RunnerMessageType_TaskAccepted {
		t.Fatalf("first message should be TaskAccepted, got %v", first.Kind)
	}

	last := decodeRunnerMsg(t, ms.sent[len(ms.sent)-1])
	if last.Kind != protocol.RunnerMessageType_TaskFinished {
		t.Fatalf("last message should be TaskFinished, got %v", last.Kind)
	}
	tf := last.TaskFinished()
	if tf == nil || tf.ExitCode == 0 {
		t.Fatalf("expected non-zero exit on panic, got %+v", tf)
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
// protocol.IsUnderRoot semantics — same rules as the server side.
func TestSessionRepoAllowedDelegatesToProtocol(t *testing.T) {
	s := &Session{AllowedRoots: []string{"/home/user/repos"}}

	cases := []struct {
		repo    string
		allowed bool
	}{
		{"/home/user/repos", true},
		{"/home/user/repos/project", true},
		{"/home/user/repos/a/b/c", true},
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

// TestSessionPanicIsolatesSiblingTask verifies that a panic in handleAssign for
// one task is recovered and reported as TaskFinished (with ExitCode -1), while a
// concurrently running sibling task completes normally with ExitCode 0.
//
// The test uses testHookHandleAssign to inject a panic into the FIRST task only.
// A sync.Once ensures exactly one invocation triggers the panic; subsequent calls
// (from the sibling goroutine) return without panicking.
func TestSessionPanicIsolatesSiblingTask(t *testing.T) {
	repo := initRepo(t)
	ms := &mockSender{}
	fakePath, _ := filepath.Abs("../testdata/fake-claude.sh")

	s := &Session{
		AllowedRoots: []string{repo},
		ClaudeBin:    fakePath,
		Timeout:      10 * time.Second,
		Sender:       ms,
		Now:          time.Now,
	}

	// Arm panic for the FIRST invocation only; subsequent invocations are no-ops.
	var once sync.Once
	s.testHookHandleAssign = func() {
		once.Do(func() {
			panic("test-panic-isolation")
		})
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// Task 0x01 — panics.
	go func() {
		defer wg.Done()
		req := &protocol.AssignTask{
			TaskId: protocol.TaskID{Id: [16]byte{0x01}},
			Prompt: []byte("task-a"),
		}
		req.SetRepoPath([]byte(repo))
		s.handleAssign(context.Background(), req)
	}()

	// Task 0x02 — runs normally. Small delay to allow 0x01 to fire the once.Do first.
	go func() {
		defer wg.Done()
		time.Sleep(80 * time.Millisecond)
		req := &protocol.AssignTask{
			TaskId: protocol.TaskID{Id: [16]byte{0x02}},
			Prompt: []byte("task-b"),
		}
		req.SetRepoPath([]byte(repo))
		s.handleAssign(context.Background(), req)
	}()

	wg.Wait()

	ms.mu.Lock()
	sentCopy := append([][]byte{}, ms.sent...)
	ms.mu.Unlock()

	finished := collectTaskFinished(t, sentCopy)

	// Task 0x01 must be panic-reported: ExitCode -1, DiffInfo contains "runner_panic".
	tf01, ok := finished[[16]byte{0x01}]
	if !ok {
		t.Fatal("no TaskFinished for task 0x01")
	}
	if tf01.ExitCode != -1 {
		t.Errorf("task 0x01: want ExitCode -1, got %d", tf01.ExitCode)
	}
	if !bytes.Contains(tf01.ErrorMessage, []byte("runner_panic")) {
		t.Errorf("task 0x01: expected 'runner_panic' in ErrorMessage, got %q", tf01.ErrorMessage)
	}

	// Task 0x02 must have succeeded: ExitCode 0.
	tf02, ok := finished[[16]byte{0x02}]
	if !ok {
		t.Fatal("no TaskFinished for task 0x02")
	}
	if tf02.ExitCode != 0 {
		t.Errorf("task 0x02: want ExitCode 0 (success), got %d", tf02.ExitCode)
	}
}

// TestHandleAssign_PassesEnvToProcess verifies that HARNESS_* env vars built from
// session fields and request data are visible to the spawned claude process.
func TestHandleAssign_PassesEnvToProcess(t *testing.T) {
	// Fake claude: print the two env vars we care about, then exit 0.
	fake := writeFakeClaude(t, `echo "TICKET=$HARNESS_AUTH_TICKET"
echo "TASK=$HARNESS_TASK_ID"`)

	repo := initRepo(t)
	ms := &mockSender{}
	sess := &Session{
		AllowedRoots: []string{repo},
		ClaudeBin:    fake,
		Timeout:      5 * time.Second,
		Sender:       ms,
		ServerCID:    mustParseCID(t, "ws:127.0.0.1:8539-1"),
		Hostname:     "test-host",
		WSPath:       "/ws",
		Logger:       nil, // defaults to slog.Default()
		Now:          time.Now,
	}

	var taskIDBytes [16]byte
	taskIDBytes[0] = 0xAB
	var ticket [16]byte
	ticket[0] = 0xCD
	ticket[15] = 0xEF
	req := &protocol.AssignTask{
		TaskId:     protocol.TaskID{Id: taskIDBytes},
		AuthTicket: ticket,
		Prompt:     []byte("env-test"),
	}
	req.SetRepoPath([]byte(repo))

	sess.handleAssign(context.Background(), req)

	// Collect all published log data.
	ms.mu.Lock()
	var combined []byte
	for _, p := range ms.publishes {
		combined = append(combined, p.data...)
	}
	ms.mu.Unlock()

	output := string(combined)
	// ticket[0]=0xCD, ticket[15]=0xEF, all others zero → 32-char hex
	expectedTicket := "TICKET=cd" + strings.Repeat("00", 14) + "ef"
	// taskID[0]=0xAB, rest zero
	expectedTask := "TASK=ab" + strings.Repeat("00", 15)
	if !strings.Contains(output, expectedTicket) {
		t.Errorf("auth ticket env not visible to claude; output=%q want substring %q", output, expectedTicket)
	}
	if !strings.Contains(output, expectedTask) {
		t.Errorf("task id env not visible to claude; output=%q want substring %q", output, expectedTask)
	}
}

// decodeRunnerMsg parses the wire-prefixed RunnerControl payload from a Sender.Send call.
func decodeRunnerMsg(t *testing.T, raw []byte) *protocol.RunnerMessage {
	t.Helper()
	if len(raw) == 0 || raw[0] != byte(wire.ApplicationPayloadKind_RunnerControl) {
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
}

func (l *fakeBidiLookup) GetBidirectionalStream(id trsf.StreamID) trsf.BidirectionalStream {
	return l.streams[id]
}

// noopBidiStream is a minimal trsf.BidirectionalStream stub. Reads return EOF
// immediately and CloseBoth flips a flag so tests can assert teardown.
type noopBidiStream struct {
	streamID trsf.StreamID
	closed   atomic.Bool
}

func (s *noopBidiStream) ID() trsf.StreamID           { return s.streamID }
func (s *noopBidiStream) Write(p []byte) (int, error) { return len(p), nil }
func (s *noopBidiStream) Close() error                { return nil }
func (s *noopBidiStream) WriteContext(_ context.Context, p []byte) (int, error) {
	return len(p), nil
}
func (s *noopBidiStream) HasSendData() bool                    { return false }
func (s *noopBidiStream) Completed() bool                      { return true }
func (s *noopBidiStream) AppendData(_ bool, _ ...[]byte) error { return nil }
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

// TestHandleAssign_OmitsEmptyHostname verifies that when Session.Hostname is empty,
// handleAssign does NOT pass a HARNESS_HOSTNAME= env var to the spawned claude process.
// This tests the full session-level flow, complementing the unit-level BuildAgentEnv test.
func TestHandleAssign_OmitsEmptyHostname(t *testing.T) {
	// Fake claude: print the HARNESS_HOSTNAME env var (or empty string if unset).
	fake := writeFakeClaude(t, `echo "HOSTNAME=$HARNESS_HOSTNAME"`)

	repo := initRepo(t)
	ms := &mockSender{}
	sess := &Session{
		AllowedRoots: []string{repo},
		ClaudeBin:    fake,
		Timeout:      5 * time.Second,
		Sender:       ms,
		ServerCID:    mustParseCID(t, "ws:127.0.0.1:8539-1"),
		Hostname:     "", // Empty hostname — should not be passed to claude
		WSPath:       "/ws",
		Now:          time.Now,
	}

	var taskIDBytes [16]byte
	taskIDBytes[0] = 0xEE
	req := &protocol.AssignTask{
		TaskId: protocol.TaskID{Id: taskIDBytes},
		Prompt: []byte("test-empty-hostname"),
	}
	req.SetRepoPath([]byte(repo))

	sess.handleAssign(context.Background(), req)

	// Collect all published log data.
	ms.mu.Lock()
	var combined []byte
	for _, p := range ms.publishes {
		combined = append(combined, p.data...)
	}
	ms.mu.Unlock()

	output := string(combined)
	// When HARNESS_HOSTNAME is not set, the echo will print just "HOSTNAME="
	if !strings.Contains(output, "HOSTNAME=") || strings.Contains(output, "HOSTNAME=test") {
		t.Errorf("empty hostname should not be passed; output=%q", output)
	}
}

// TestHandleAssign_WritesSettingsAndPropagatesEnv verifies the full assign chain:
// (1) settings.json is written into the worktree under .claude/,
// (2) the file contains the UserPromptSubmit hook entry,
// (3) HARNESS_AUTH_TICKET is visible to the spawned claude process.
func TestHandleAssign_WritesSettingsAndPropagatesEnv(t *testing.T) {
	// Fake claude: list the settings file, print its first few lines, echo the ticket env.
	fake := writeFakeClaude(t, `ls -la .claude/settings.json
cat .claude/settings.json
echo "TICKET=$HARNESS_AUTH_TICKET"`)

	repo := initRepo(t)
	ms := &mockSender{}
	sess := &Session{
		AllowedRoots: []string{repo},
		ClaudeBin:    fake,
		Timeout:      5 * time.Second,
		Sender:       ms,
		ServerCID:    mustParseCID(t, "ws:127.0.0.1:8539-1"),
		Hostname:     "test-host",
		WSPath:       "/ws",
		Now:          time.Now,
	}

	var taskIDBytes [16]byte
	taskIDBytes[0] = 0xAB
	var ticket [16]byte
	ticket[0] = 0xFE
	ticket[15] = 0xED

	req := &protocol.AssignTask{
		TaskId:     protocol.TaskID{Id: taskIDBytes},
		AuthTicket: ticket,
		Prompt:     []byte("settings-smoke"),
	}
	req.SetRepoPath([]byte(repo))

	sess.handleAssign(context.Background(), req)

	// Collect all published log data.
	ms.mu.Lock()
	var combined []byte
	for _, p := range ms.publishes {
		combined = append(combined, p.data...)
	}
	ms.mu.Unlock()

	output := string(combined)

	// (1) settings.json was written — ls -la output includes the filename.
	if !strings.Contains(output, "settings.json") {
		t.Errorf("settings.json not written to worktree; output=%q", output)
	}

	// (2) settings.json contains UserPromptSubmit — head -5 includes the hook key.
	if !strings.Contains(output, "UserPromptSubmit") {
		t.Errorf("settings.json missing UserPromptSubmit hook; output=%q", output)
	}

	// (3) HARNESS_AUTH_TICKET reaches claude — build expected hex at runtime.
	expectedTicketHex := hex.EncodeToString(ticket[:])
	expectedLine := "TICKET=" + expectedTicketHex
	if !strings.Contains(output, expectedLine) {
		t.Errorf("auth ticket env not propagated to claude; output=%q want substring %q", output, expectedLine)
	}
}

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
		ClaudeBin:    "/bin/true",
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
}

// TestHandleAssign_NoWorktree_NoGitDir verifies that with Session.NoWorktree=true
// the runner executes claude with cwd=repoPath, does not create a git worktree,
// does not inject .claude/settings.json or .claude/skills/, and does not delete
// the repo dir on cleanup. The repoPath in this test is a non-git tempdir, which
// would otherwise fail the worktree create step.
func TestHandleAssign_NoWorktree_NoGitDir(t *testing.T) {
	// repo is a plain tempdir (non-git). In normal (NoWorktree=false) mode,
	// wm.Create would fail because there is no git repo. With NoWorktree=true
	// the worktree-create step is entirely skipped, so the plain dir works.
	repo := t.TempDir()
	// Script echoes cwd and creates a sentinel file to verify the process ran
	// with the correct working directory.
	fake := writeFakeClaude(t, `echo "cwd=$(pwd)"
touch harness_nw_sentinel.txt
echo "done"`)

	ms := &mockSender{}
	s := &Session{
		AllowedRoots: []string{repo},
		ClaudeBin:    fake,
		Timeout:      5 * time.Second,
		Sender:       ms,
		Now:          time.Now,
		NoWorktree:   true,
	}
	var taskIDBytes [16]byte
	taskIDBytes[0] = 0xBB
	req := &protocol.AssignTask{
		TaskId: protocol.TaskID{Id: taskIDBytes},
		Prompt: []byte("nw-no-git"),
	}
	req.SetRepoPath([]byte(repo))
	s.handleAssign(context.Background(), req)

	// (1) Last message is TaskFinished, ExitCode 0.
	if len(ms.sent) < 3 {
		t.Fatalf("expected ≥3 messages, got %d", len(ms.sent))
	}
	last := decodeRunnerMsg(t, ms.sent[len(ms.sent)-1])
	if last.Kind != protocol.RunnerMessageType_TaskFinished {
		t.Fatalf("last msg kind: %v", last.Kind)
	}
	if tf := last.TaskFinished(); tf == nil || tf.ExitCode != 0 {
		t.Fatalf("finished: %+v", tf)
	}

	// (2) TaskStarted carries WorktreeDir == repoPath.
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
		t.Fatalf("TaskStarted.WorktreeDir: got %q, want %q", startedDir, repo)
	}

	// (3) claude ran with cwd=repoPath: echo "cwd=$(pwd)" output contains repo, and
	// touch harness_nw_sentinel.txt was created in the repo directory.
	var combined []byte
	ms.mu.Lock()
	for _, p := range ms.publishes {
		combined = append(combined, p.data...)
	}
	ms.mu.Unlock()
	if !strings.Contains(string(combined), repo) {
		t.Fatalf("expected cwd output to contain %q; got %q", repo, combined)
	}
	if _, err := os.Stat(filepath.Join(repo, "harness_nw_sentinel.txt")); err != nil {
		t.Fatalf("sentinel file not created in repo (claude cwd was not repoPath): %v", err)
	}

	// (4) No worktree dir created.
	if _, err := os.Stat(filepath.Join(repo, ".harness-worktrees")); !os.IsNotExist(err) {
		t.Fatalf(".harness-worktrees should not exist; stat err=%v", err)
	}

	// (5) No .claude/ injection happened.
	if _, err := os.Stat(filepath.Join(repo, ".claude")); !os.IsNotExist(err) {
		t.Fatalf(".claude/ should not exist in no-worktree mode without force-inject; stat err=%v", err)
	}

	// (6) repo dir itself survives the run.
	if _, err := os.Stat(repo); err != nil {
		t.Fatalf("repo dir disappeared: %v", err)
	}
}

// TestHandleAssign_NoWorktree_GitDir_HEADUntouched verifies that running a task
// in NoWorktree mode against a real git repo does not modify the user's HEAD,
// does not create the harness/<id> branch, and does not create a worktree dir.
func TestHandleAssign_NoWorktree_GitDir_HEADUntouched(t *testing.T) {
	repo := initRepo(t) // git repo on branch "main"
	headBefore := readHEADRef(t, repo)

	fake := writeFakeClaude(t, `echo nw-git-test`)
	ms := &mockSender{}
	s := &Session{
		AllowedRoots: []string{repo},
		ClaudeBin:    fake,
		Timeout:      5 * time.Second,
		Sender:       ms,
		Now:          time.Now,
		NoWorktree:   true,
	}
	var taskIDBytes [16]byte
	taskIDBytes[0] = 0xCD
	req := &protocol.AssignTask{
		TaskId: protocol.TaskID{Id: taskIDBytes},
		Prompt: []byte("nw-git"),
	}
	req.SetRepoPath([]byte(repo))
	s.handleAssign(context.Background(), req)

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

// TestHandleAssign_NoWorktree_ConcurrentTasks verifies that two tasks assigned
// concurrently to the same Session in NoWorktree mode both reach TaskFinished
// without serializing on each other (no shared worktree mutex).
func TestHandleAssign_NoWorktree_ConcurrentTasks(t *testing.T) {
	repo := t.TempDir()
	fake := writeFakeClaude(t, `sleep 0.1; echo done`)
	ms := &mockSender{}
	s := &Session{
		AllowedRoots: []string{repo},
		ClaudeBin:    fake,
		Timeout:      5 * time.Second,
		Sender:       ms,
		Now:          time.Now,
		NoWorktree:   true,
	}

	var wg sync.WaitGroup
	wg.Add(2)
	for i := byte(1); i <= 2; i++ {
		i := i
		go func() {
			defer wg.Done()
			req := &protocol.AssignTask{
				TaskId: protocol.TaskID{Id: [16]byte{i}},
				Prompt: []byte("nw-concurrent"),
			}
			req.SetRepoPath([]byte(repo))
			s.handleAssign(context.Background(), req)
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

// TestHandleAssign_NoWorktree_Resume verifies that running the same task_id
// twice in NoWorktree mode does not break (no worktree state to reuse, no
// branch to re-attach — both runs simply use cwd=repoPath).
func TestHandleAssign_NoWorktree_Resume(t *testing.T) {
	repo := t.TempDir()
	fake := writeFakeClaude(t, `echo run`)
	ms := &mockSender{}
	s := &Session{
		AllowedRoots: []string{repo},
		ClaudeBin:    fake,
		Timeout:      5 * time.Second,
		Sender:       ms,
		Now:          time.Now,
		NoWorktree:   true,
	}
	taskID := protocol.TaskID{Id: [16]byte{0x99}}
	for i := 0; i < 2; i++ {
		req := &protocol.AssignTask{
			TaskId: taskID,
			Prompt: []byte("nw-resume"),
		}
		req.SetRepoPath([]byte(repo))
		s.handleAssign(context.Background(), req)
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

// TestHandleOpenExec_NoWorktree_NoGitDir mirrors TestHandleAssign_NoWorktree_NoGitDir
// but for the interactive PTY path. Uses /bin/true as ClaudeBin and the existing
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
		ClaudeBin:    "/bin/true",
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

// TestHandleAssign_NoWorktree_ForceInject verifies that with both
// NoWorktree=true and ForceInjectHarnessSettings=true, the runner writes
// .claude/settings.json (with harness-cli hooks) and .claude/skills/
// directly into repoPath. Cleanup is still skipped — the injected files
// persist past task end.
func TestHandleAssign_NoWorktree_ForceInject(t *testing.T) {
	repo := t.TempDir()
	fake := writeFakeClaude(t, `echo force-inject`)
	ms := &mockSender{}
	s := &Session{
		AllowedRoots:               []string{repo},
		ClaudeBin:                  fake,
		Timeout:                    5 * time.Second,
		Sender:                     ms,
		Now:                        time.Now,
		NoWorktree:                 true,
		ForceInjectHarnessSettings: true,
	}
	var taskIDBytes [16]byte
	taskIDBytes[0] = 0xEE
	req := &protocol.AssignTask{
		TaskId: protocol.TaskID{Id: taskIDBytes},
		Prompt: []byte("nw-force-inject"),
	}
	req.SetRepoPath([]byte(repo))
	s.handleAssign(context.Background(), req)

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
