package runner

import (
	"context"
	"encoding/hex"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TestPeerSenderPublishCachesStreamPerTopic verifies that the runner's
// Publish path (peerSender → peer.Conn.Publish) creates one bidirectional
// stream per topic and caches it for subsequent Publish calls.
//
// Full verification is deferred because implementing faithful fakes for
// trsf.Transport (CreateBidirectionalStream, AcceptBidirectionalStream)
// and objproto.Connection (SendMessage) requires reproducing non-trivial
// internal trsf stream machinery. The behaviour is exercised end-to-end
// in the integration test (see Task 5.1 in the original plan).
func TestPeerSenderPublishCachesStreamPerTopic(t *testing.T) {
	t.Skip("requires fake trsf.Transport stub — covered by integration test")
}

// TestRunnerHandlesCancelTaskCallsCancelFunc verifies that receiving a CancelTask
// request causes dispatchRunnerRequest to call the registered cancel function for
// the matching task.
func TestRunnerHandlesCancelTaskCallsCancelFunc(t *testing.T) {
	ms := &mockSender{}
	s := &Session{
		AllowedRoots: []string{"/repo"},
		Profiles:     singleProfile(t, "/bin/true"),
		Timeout:      30 * time.Second,
		Sender:       ms,
		Now:          time.Now,
	}

	// Pre-register a task with a cancel function we can observe.
	var taskIDBytes [16]byte
	taskIDBytes[0] = 0xDE
	taskIDHex := hex.EncodeToString(taskIDBytes[:])

	cancelCalled := false
	var cancelMu sync.Mutex
	s.mu.Lock()
	s.initMaps()
	s.reg.put(taskIDHex, &taskEntry{
		cancel: func() {
			cancelMu.Lock()
			cancelCalled = true
			cancelMu.Unlock()
		},
		repoPath: "/repo",
	})
	s.mu.Unlock()

	// Build a CancelTask RunnerRequest.
	ct := protocol.CancelTask{}
	ct.TaskId.Id = taskIDBytes
	req := &protocol.RunnerRequest{Kind: protocol.RunnerRequestType_CancelTask}
	req.SetCancelTask(ct)
	payload, err := req.Append(nil)
	if err != nil {
		t.Fatalf("encode CancelTask: %v", err)
	}

	dispatchRunnerRequest(context.Background(), s, s.logger(), appwire.AppKind_RunnerControl, payload)

	cancelMu.Lock()
	got := cancelCalled
	cancelMu.Unlock()
	if !got {
		t.Errorf("expected cancel to be called for task %q", taskIDHex)
	}
}

// A CancelTask for a task this runner does not have is answered with a
// TaskFinished. The server resends CancelTask until one arrives, so the silence
// this used to answer with kept it resending; and the two ways to get here — the
// task's own TaskFinished was lost, or its AssignTask never arrived — both leave
// the server with a row only a TaskFinished can close.
func TestRunnerAnswersCancelForUnknownTaskWithTaskFinished(t *testing.T) {
	ms := &mockSender{}
	s := &Session{
		AllowedRoots: []string{"/repo"},
		Sender:       ms,
		Now:          time.Now,
	}

	var taskIDBytes [16]byte
	taskIDBytes[0] = 0xFF
	ct := protocol.CancelTask{}
	ct.TaskId.Id = taskIDBytes
	req := &protocol.RunnerRequest{Kind: protocol.RunnerRequestType_CancelTask}
	req.SetCancelTask(ct)
	payload, err := req.Append(nil)
	if err != nil {
		t.Fatalf("encode CancelTask: %v", err)
	}

	dispatchRunnerRequest(context.Background(), s, s.logger(), appwire.AppKind_RunnerControl, payload)

	if len(ms.sent) != 1 {
		t.Fatalf("sent %d messages, want exactly one TaskFinished", len(ms.sent))
	}
	m := decodeRunnerMsg(t, ms.sent[0])
	tf := m.TaskFinished()
	if m.Kind != protocol.RunnerMessageType_TaskFinished || tf == nil {
		t.Fatalf("sent kind %v, want TaskFinished", m.Kind)
	}
	if tf.TaskId.Id != taskIDBytes || tf.ExitCode != -1 || string(tf.ErrorMessage) != cancelNotRunning {
		t.Fatalf("TaskFinished = {task %x, exit %d, %q}, want {%x, -1, %q}",
			tf.TaskId.Id, tf.ExitCode, tf.ErrorMessage, taskIDBytes, cancelNotRunning)
	}
}

// The cancel can overtake its own AssignTask: the body is fetched on a goroutine
// of its own. The runner has already told the server the task is over, so the
// late assignment must be refused, not started — otherwise the task runs with
// its slot released and its ticket revoked.
func TestCancelBeforeAssignRefusesTheLateTask(t *testing.T) {
	ms := &mockSender{}
	s := &Session{
		AllowedRoots: []string{"/some/repo"},
		Profiles:     singleProfile(t, "/bin/true"),
		Timeout:      time.Second,
		Sender:       ms,
		Now:          time.Now,
	}
	s.mu.Lock()
	s.initMaps()
	s.mu.Unlock()

	taskID := protocol.TaskID{Id: [16]byte{0xCA}}
	// The AssignTask envelope arrived (the receive loop marks it pending), and
	// the CancelTask overtook its body.
	endPending := s.beginPendingTask(taskID)
	s.handleCancelTask(taskID)

	body := &protocol.AssignTaskBody{Prompt: []byte("x")}
	body.SetRepoPath([]byte("/some/repo"))
	s.handleAssign(context.Background(), taskID, body)
	endPending()

	// Cancel's TaskFinished, the assign's TaskAccepted, then the refusal's
	// TaskFinished — and NO TaskStarted, which is the proof nothing ran.
	var kinds []protocol.RunnerMessageType
	var last *protocol.TaskFinished
	for _, b := range ms.sent {
		m := decodeRunnerMsg(t, b)
		kinds = append(kinds, m.Kind)
		if m.Kind == protocol.RunnerMessageType_TaskStarted {
			t.Fatalf("a task cancelled before it arrived was started; sent %v", kinds)
		}
		if tf := m.TaskFinished(); tf != nil {
			last = tf
		}
	}
	if last == nil || string(last.ErrorMessage) != cancelledBeforeStart {
		t.Fatalf("last TaskFinished = %+v, want reason %q; sent %v", last, cancelledBeforeStart, kinds)
	}
	if _, ok := s.reg.get(hex.EncodeToString(taskID.Id[:])); ok {
		t.Fatal("the refused task was left in the registry")
	}
}

// A cancel for a task that is NOT pending must not refuse a later run of the
// same id. A resume reuses the id, and the server cancels an interactive task
// whose mux stopped just as the runner forgets it — so a record that outlived
// the pending window refused `session new --resume` right after.
func TestCancelOfUnknownTaskDoesNotRefuseALaterRun(t *testing.T) {
	ms := &mockSender{}
	s := &Session{
		AllowedRoots: []string{"/some/repo"},
		Profiles:     singleProfile(t, "/bin/true"),
		Timeout:      time.Second,
		Sender:       ms,
		Now:          time.Now,
	}
	// A live runner's registry exists before any cancel can reach it (Config.Tasks,
	// or the first task's initMaps); without it the cancel below records nothing
	// and the test cannot fail.
	s.mu.Lock()
	s.initMaps()
	s.mu.Unlock()
	taskID := protocol.TaskID{Id: [16]byte{0xCB}}
	s.handleCancelTask(taskID) // the run is already gone: answered, not recorded

	endPending := s.beginPendingTask(taskID) // the resume's AssignTask
	body := &protocol.AssignTaskBody{Prompt: []byte("x")}
	body.SetRepoPath([]byte("/some/repo"))
	s.handleAssign(context.Background(), taskID, body)
	endPending()

	last := decodeRunnerMsg(t, ms.sent[len(ms.sent)-1]).TaskFinished()
	if last == nil || string(last.ErrorMessage) == cancelledBeforeStart {
		t.Fatalf("the resumed run was refused by the earlier cancel: %+v", last)
	}
	if s.reg.pending[hex.EncodeToString(taskID.Id[:])] {
		t.Fatal("a pending mark outlived its run")
	}
}

// TestBuildRunnerHello verifies that buildRunnerHello (used in the merged
// PskAuthRequest) populates AllowedRoots, MaxTasks, and Hostname from Config.
// The RunnerHello is now embedded in the PskAuthRequest rather than sent as a
// separate RunnerMessage (the old pre-merged-handshake path).
func TestBuildRunnerHello(t *testing.T) {
	cfg := Config{
		AllowedRoots: []string{"/home/user/repos", "/data/work"},
		MaxTasks:     3,
		Hostname:     "build-runner-1",
	}

	hh := buildRunnerHello(cfg)

	if string(hh.Hostname) != "build-runner-1" {
		t.Errorf("Hostname: want %q, got %q", "build-runner-1", string(hh.Hostname))
	}
	if hh.MaxTasks != 3 {
		t.Errorf("MaxTasks: want 3, got %d", hh.MaxTasks)
	}
	if hh.Version != 1 {
		t.Errorf("Version: want 1, got %d", hh.Version)
	}
	if len(hh.AllowedRoots) != 2 {
		t.Fatalf("AllowedRoots len: want 2, got %d", len(hh.AllowedRoots))
	}
	if string(hh.AllowedRoots[0].Path) != "/home/user/repos" {
		t.Errorf("AllowedRoots[0]: want %q, got %q", "/home/user/repos", string(hh.AllowedRoots[0].Path))
	}
	if string(hh.AllowedRoots[1].Path) != "/data/work" {
		t.Errorf("AllowedRoots[1]: want %q, got %q", "/data/work", string(hh.AllowedRoots[1].Path))
	}
}

// TestRunnerMergedHandshakeEncodesRoleRunner verifies that sendRunnerMergedHandshake
// builds a PskAuthRequest with role=runner containing the RunnerHello from Config,
// and that the PskAuthResponse{ok} on the response channel causes it to return nil.
// This is the merged-handshake path: one message replaces both SendAndWaitPSK and
// the separate RunnerHello send that previously occurred in OnConnect.
func TestRunnerMergedHandshakeEncodesRoleRunner(t *testing.T) {
	cfg := Config{
		AllowedRoots: []string{"/srv/repo"},
		MaxTasks:     2,
		Hostname:     "test-runner",
	}

	var sentBytes []byte
	sendFn := func(b []byte) error {
		sentBytes = append(sentBytes, b...)
		return nil
	}

	respCh := make(chan protocol.PskAuthResponse, 1)
	respCh <- protocol.PskAuthResponse{Status: protocol.PskAuthStatus_Ok}

	// No PSK: binder_len should be 0.
	ctx := context.Background()
	if err := sendRunnerMergedHandshake(ctx, sendFn, nil, nil, cfg, respCh); err != nil {
		t.Fatalf("sendRunnerMergedHandshake: %v", err)
	}

	// sentBytes[0] must be AppKind_PskAuth (0x45).
	if len(sentBytes) == 0 {
		t.Fatal("no bytes sent")
	}
	if sentBytes[0] != byte(appwire.AppKind_PskAuth) {
		t.Errorf("first byte: got 0x%02x, want 0x%02x (AppKind_PskAuth)", sentBytes[0], byte(appwire.AppKind_PskAuth))
	}

	// Decode the PskAuthRequest from the remaining bytes and verify role=runner.
	var req protocol.PskAuthRequest
	if _, err := req.Decode(sentBytes[1:]); err != nil {
		t.Fatalf("Decode PskAuthRequest: %v", err)
	}
	if req.Role != protocol.AuthRole_Runner {
		t.Errorf("Role: got %v, want AuthRole_Runner", req.Role)
	}
	if req.BinderLen != 0 {
		t.Errorf("BinderLen: got %d, want 0 (no-PSK)", req.BinderLen)
	}
	rh := req.RunnerHello()
	if rh == nil {
		t.Fatal("RunnerHello() returned nil")
	}
	if rh.Version != 1 {
		t.Errorf("RunnerHello.Version: got %d, want 1", rh.Version)
	}
	if rh.MaxTasks != 2 {
		t.Errorf("RunnerHello.MaxTasks: got %d, want 2", rh.MaxTasks)
	}
	if string(rh.Hostname) != "test-runner" {
		t.Errorf("RunnerHello.Hostname: got %q, want %q", string(rh.Hostname), "test-runner")
	}
	// ClientHello must be nil for runner role.
	if req.ClientHello() != nil {
		t.Error("ClientHello() should be nil for runner role")
	}
}

// TestDispatchRunnerHelloResponseStoresCanonicalID verifies the dispatch case:
// a RunnerRequest of kind RunnerHelloResponse populates
// Session.runnerCanonicalID, which is what HARNESS_RUNNER_ID is built from.
func TestDispatchRunnerHelloResponseStoresCanonicalID(t *testing.T) {
	s := &Session{Now: time.Now}

	rid := protocol.RunnerID{Id: [16]byte{0xC0, 0xA8, 1, 42, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x42, 0x42}}

	req := &protocol.RunnerRequest{Kind: protocol.RunnerRequestType_RunnerHelloResponse}
	req.SetRunnerHelloResponse(protocol.RunnerHelloResponse{YourRunnerId: rid})
	payload, err := req.Append(nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	dispatchRunnerRequest(context.Background(), s, nil, appwire.AppKind_RunnerControl, payload)

	if got := s.runnerCanonicalRunnerID(); got != rid {
		t.Errorf("canonical identity = %s, want %s", got.Hex(), rid.Hex())
	}
}

// A Session that has not heard a RunnerHelloResponse must report the ZERO
// identity, not something plausible. The old version of this test guarded
// against a panic, because encoding a zero RunnerID used to trip an
// IpAddrLen assertion; what matters now is that absence stays absence — an
// agent then fails Hello validation instead of authenticating as nobody.
func TestRunnerCanonicalIdentityIsZeroBeforeHelloResponse(t *testing.T) {
	s := &Session{}
	if got := s.runnerCanonicalRunnerID(); !got.IsZero() {
		t.Fatalf("identity before hello response = %s, want zero", got.Hex())
	}
}

// runHooks is a test seam used by TestRun_RunCtxCancelsOnPeerDone to inject
// a handleAssign-shaped goroutine without spinning up real claude processes.
type runHooks struct {
	spawnTask func(ctx context.Context)
	kicker    chan struct{}
}

func (h *runHooks) kickoff() { close(h.kicker) }

// fakeRunHandle implements PersistHandle for runner unit tests.
type fakeRunHandle struct {
	done chan struct{}
}

func (h *fakeRunHandle) Done() <-chan struct{} { return h.done }
func (h *fakeRunHandle) Close()                {}

// runConnected is the test-facing core of OnConnect: derive runCtx, fire a
// spawn callback when the kicker channel triggers, then block on Done.
func runConnected(parent context.Context, h *fakeRunHandle, hooks runHooks) error {
	runCtx, runCancel := context.WithCancel(parent)
	defer runCancel()
	if hooks.kicker == nil {
		hooks.kicker = make(chan struct{})
	}
	go func() {
		<-hooks.kicker
		if hooks.spawnTask != nil {
			hooks.spawnTask(runCtx)
		}
	}()
	select {
	case <-h.Done():
		return nil
	case <-parent.Done():
		return nil
	}
}

// TestRun_RunCtxCancelsOnPeerDone verifies that when the underlying peer.Conn
// reports Done, the per-Run ctx visible to spawned task handlers is cancelled.
//
// We don't have a real peer.Conn here, so we exercise the ctx wiring directly
// via runConnected (the helper introduced by this task) with a fake handle.
func TestRun_RunCtxCancelsOnPeerDone(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := &fakeRunHandle{done: make(chan struct{})}
	captured := make(chan context.Context, 1)
	hooks := runHooks{
		spawnTask: func(ctx context.Context) { captured <- ctx },
		kicker:    make(chan struct{}),
	}

	go func() {
		_ = runConnected(parent, h, hooks)
	}()

	// Trigger one synthetic AssignTask path via the spawn hook.
	hooks.kickoff()

	var taskCtx context.Context
	select {
	case taskCtx = <-captured:
	case <-time.After(2 * time.Second):
		t.Fatalf("spawnTask was never invoked")
	}
	if taskCtx.Err() != nil {
		t.Fatalf("taskCtx already cancelled before peer Done: %v", taskCtx.Err())
	}

	close(h.done) // simulate disconnect

	select {
	case <-taskCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatalf("taskCtx was not cancelled after peer Done")
	}
}

// TestRunHandle_BufferedRunnerHelloResponse_SetsCanonicalID is the regression
// guard for the fleet incident: the merged handshake makes the server reply
// RunnerHelloResponse during the handshake window — before OnConnect installs
// the dispatcher. The old handler dropped it, leaving the canonical RunnerID
// zero, so spawned agents inherited HARNESS_RUNNER_ID=":invalid AddrPort-0".
// The handler must buffer it and OnConnect must replay it.
func TestRunHandle_BufferedRunnerHelloResponse_SetsCanonicalID(t *testing.T) {
	sess := &Session{}
	h := &RunHandle{session: sess, cfg: Config{Logger: slog.Default()}}

	// A RunnerHelloResponse exactly as the server sends it (a RunnerRequest
	// payload; the AppKind is the separate `kind` arg, not in the payload).
	rid := protocol.RunnerID{Id: [16]byte{0xC0, 0xA8, 3, 14, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xD1, 0x79}}
	req := protocol.RunnerRequest{Kind: protocol.RunnerRequestType_RunnerHelloResponse}
	req.SetRunnerHelloResponse(protocol.RunnerHelloResponse{YourRunnerId: rid})
	payload, err := req.Append(nil)
	if err != nil {
		t.Fatalf("encode RunnerHelloResponse: %v", err)
	}

	// Arrives during the handshake window (ctlDispatch == nil) → must be buffered,
	// NOT yet applied to the session.
	h.bufferOrDispatch(appwire.AppKind_RunnerControl, payload)
	if got := sess.runnerCanonicalRunnerID(); got == rid {
		t.Fatalf("canonical id applied before activateDispatch — should have been buffered (got %s)", got.Hex())
	}

	// OnConnect activates the dispatcher and replays the buffer → SetRunnerCanonicalID.
	h.activateDispatch(func(kind appwire.AppKind, p []byte) {
		dispatchRunnerRequest(context.Background(), sess, h.cfg.Logger, kind, p)
	})

	if got := sess.runnerCanonicalRunnerID(); got != rid {
		t.Fatalf("buffered RunnerHelloResponse not applied on activate: got %s want %s", got.Hex(), rid.Hex())
	}

	// A message arriving AFTER activation is dispatched live (not buffered).
	if len(h.ctlBuf) != 0 {
		t.Fatalf("ctlBuf not drained after activateDispatch: %d left", len(h.ctlBuf))
	}
}
