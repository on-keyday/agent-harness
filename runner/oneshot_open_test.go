package runner

import (
	"bytes"
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// A3 (A-D29): `session new --oneshot` runs the profile's oneshot argv in the
// PTY; the session ends with the process, its exit code the task's.

// heldStream is a noopBidiStream whose reads wait, as a live client's do,
// instead of ending at once: an ended input is a hang-up, which would stop
// the agent before it exits on its own. The wait is released after a bound
// so a test that never sees the exit still ends.
type heldStream struct {
	*noopBidiStream
	release chan struct{}
	once    sync.Once
	// input, if any, is read before the wait: what a client typed.
	inMu  sync.Mutex
	input []byte
}

func (h *heldStream) wait() { <-h.release }

// takeInput hands out the pending input, at most len(p) bytes of it.
func (h *heldStream) takeInput(p []byte) int {
	h.inMu.Lock()
	defer h.inMu.Unlock()
	n := copy(p, h.input)
	h.input = h.input[n:]
	return n
}

// CloseBoth cancels the reads, as trsf's does.
func (h *heldStream) CloseBoth() error {
	h.once.Do(func() { close(h.release) })
	return h.noopBidiStream.CloseBoth()
}
func (h *heldStream) Cancel() { h.once.Do(func() { close(h.release) }) }
func (h *heldStream) Read(p []byte) (int, error) {
	if n := h.takeInput(p); n > 0 {
		return n, nil
	}
	h.wait()
	return h.noopBidiStream.Read(p)
}
func (h *heldStream) ReadContext(ctx context.Context, p []byte) (int, error) {
	if n := h.takeInput(p); n > 0 {
		return n, nil
	}
	h.wait()
	return h.noopBidiStream.ReadContext(ctx, p)
}
func (h *heldStream) ReadDirect(n uint64) ([]byte, bool, error) {
	h.wait()
	return h.noopBidiStream.ReadDirect(n)
}
func (h *heldStream) ReadDirectContext(ctx context.Context, n uint64) ([]byte, bool, error) {
	select {
	case <-h.release:
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
	return h.noopBidiStream.ReadDirectContext(ctx, n)
}

type oneshotRun struct {
	ms     *mockSender
	stream *noopBidiStream
}

// openOneshot runs handleOpenExec with the oneshot bit for a /bin/sh profile
// whose oneshot template writes its {prompt} argument to out and exits 3, and
// whose resume template writes "resumed" instead.
func openOneshot(t *testing.T, prompt string, resume, eventStream bool, out string) oneshotRun {
	t.Helper()
	return openExec(t, AgentProfile{
		Name:              "default",
		Bin:               "/bin/sh",
		OneshotArgv:       []string{"-c", `printf %s "$1" > ` + out + `; exit 3`, "sh", "{prompt}"},
		ResumeOneshotArgv: []string{"-c", `printf resumed > ` + out + `; exit 4`, "sh", "{prompt}"},
	}, true, prompt, resume, eventStream, nil)
}

// openExec runs handleOpenExec for profile p with the given oneshot bit. A
// non-nil running is called once the child's stdin is wired (the task's
// entry is started), with the session and the task's hex id, while the child
// still runs; handleOpenExec returns once the child exits.
func openExec(t *testing.T, p AgentProfile, oneshot bool, prompt string, resume, eventStream bool, running func(s *Session, taskIDHex string)) oneshotRun {
	t.Helper()
	return openExecOpts(t, p, execOpts{oneshot: oneshot, prompt: prompt, resume: resume, eventStream: eventStream}, running)
}

// execOpts is what openExecOpts puts on the OpenExec request, plus input: bytes
// the server's stream delivers first (frames, already encoded), before its
// reads wait as a live client's do.
type execOpts struct {
	oneshot, resume, eventStream, noPTY bool
	prompt                              string
	input                               []byte
}

func openExecOpts(t *testing.T, p AgentProfile, o execOpts, running func(s *Session, taskIDHex string)) oneshotRun {
	t.Helper()
	oneshot, prompt, resume, eventStream := o.oneshot, o.prompt, o.resume, o.eventStream
	repo := t.TempDir()
	ps, err := NewProfileSet(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	ms := &mockSender{}
	const streamID, promptID trsf.StreamID = 100, 7
	stream := &noopBidiStream{streamID: streamID}
	held := &heldStream{noopBidiStream: stream, release: make(chan struct{}), input: o.input}
	release := time.AfterFunc(10*time.Second, func() { held.once.Do(func() { close(held.release) }) })
	t.Cleanup(func() { release.Stop(); held.once.Do(func() { close(held.release) }) })
	lookup := &fakeBidiLookup{streams: map[trsf.StreamID]trsf.BidirectionalStream{streamID: held}}
	oer := &protocol.OpenExecRunnerRequest{StreamId: uint64(streamID)}
	oer.TaskId.Id[0] = 0xC3
	oer.SetRepoPath([]byte(repo))
	oer.SetOneshot(oneshot)
	oer.SetResumeConversation(resume)
	oer.SetEventStream(eventStream)
	oer.SetNoPty(o.noPTY)
	if prompt != "" {
		lookup.recv = map[trsf.StreamID]trsf.ReceiveStream{promptID: &promptRecv{data: promptBodyBytes(prompt)}}
		oer.SetPromptPresent(true)
		oer.SetPromptStreamId(uint64(promptID))
	}
	s := &Session{AllowedRoots: []string{repo}, Profiles: ps, Sender: ms, Streams: lookup, Now: time.Now, NoWorktree: true}
	if running == nil {
		s.handleOpenExec(context.Background(), oer)
		return oneshotRun{ms: ms, stream: stream}
	}
	done := make(chan struct{})
	go func() { defer close(done); s.handleOpenExec(context.Background(), oer) }()
	taskIDHex := hex.EncodeToString(oer.TaskId.Id[:])
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		e, ok := s.reg.get(taskIDHex)
		started := ok && e != nil && e.started.Load()
		s.mu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the child's stdin was never wired")
		}
		time.Sleep(10 * time.Millisecond)
	}
	running(s, taskIDHex)
	<-done
	return oneshotRun{ms: ms, stream: stream}
}

// gatedSh is a /bin/sh argv that waits for the file gate to exist and exits.
func gatedSh(gate string) []string {
	return []string{"-c", `while [ ! -e "` + gate + `" ]; do sleep 0.02; done`, "sh", "{prompt}"}
}

// A --oneshot child runs in a PTY, where git, systemctl and journalctl start
// a pager and wait for a key that never comes. Its environment names cat as
// the pager; an interactive session's environment is left as the user's.
func TestOneshotOpenDisablesPagers(t *testing.T) {
	for _, k := range []string{"PAGER", "GIT_PAGER", "SYSTEMD_PAGER"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	report := `printf '%s|%s|%s' "${PAGER-unset}" "${GIT_PAGER-unset}" "${SYSTEMD_PAGER-unset}" > `
	for _, tc := range []struct {
		name    string
		oneshot bool
		want    string
	}{
		{"oneshot", true, "cat|cat|cat"},
		{"interactive", false, "unset|unset|unset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "out")
			argv := []string{"-c", report + out, "sh", "{prompt}"}
			openExec(t, AgentProfile{Name: "default", Bin: "/bin/sh", OneshotArgv: argv, InteractiveArgv: argv},
				tc.oneshot, "p", false, false, nil)
			if b, err := os.ReadFile(out); err != nil || string(b) != tc.want {
				t.Fatalf("PAGER|GIT_PAGER|SYSTEMD_PAGER = %q (err %v), want %q", b, err, tc.want)
			}
		})
	}
}

// An agentboard wake types a marker and Enter into the agent's terminal. A
// --oneshot child is claude -p or a command, not a prompt waiting for input,
// so it gets no wake; an interactive session does.
func TestOneshotOpenInstallsNoWake(t *testing.T) {
	for _, tc := range []struct {
		name     string
		oneshot  bool
		wantWake bool
	}{
		{"oneshot", true, false},
		{"interactive", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := filepath.Join(t.TempDir(), "gate")
			argv := gatedSh(gate)
			var hasWake bool
			openExec(t, AgentProfile{Name: "default", Bin: "/bin/sh", OneshotArgv: argv, InteractiveArgv: argv},
				tc.oneshot, "p", false, false, func(s *Session, id string) {
					s.mu.Lock()
					e, _ := s.reg.get(id)
					hasWake = e.wake != nil
					s.mu.Unlock()
					if err := os.WriteFile(gate, nil, 0o600); err != nil {
						t.Error(err)
					}
				})
			if hasWake != tc.wantWake {
				t.Fatalf("wake installed = %v, want %v", hasWake, tc.wantWake)
			}
		})
	}
}

func (r oneshotRun) finished(t *testing.T) *protocol.TaskFinished {
	t.Helper()
	if len(r.ms.sent) == 0 {
		t.Fatal("nothing sent")
	}
	tf := decodeRunnerMsg(t, r.ms.sent[len(r.ms.sent)-1]).TaskFinished()
	if tf == nil {
		t.Fatal("last message is not TaskFinished")
	}
	return tf
}

// Review Focus 3 and 4: the prompt — newlines, quotes, a leading dash, 8 KB —
// is the template's single {prompt} argument, and the process's exit code is
// the task's.
func TestOneshotOpenUsesOneshotTemplate(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	prompt := "-not a flag\nsays \"hi\" and 'bye'\n" + strings.Repeat("x", 8<<10)
	start := time.Now()
	r := openOneshot(t, prompt, false, false, out)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the session outlived its process by %v: it must end when the agent exits", d)
	}
	if tf := r.finished(t); tf.ExitCode != 3 {
		t.Fatalf("exit %d (%q), want 3", tf.ExitCode, tf.ErrorMessage)
	}
	if b, err := os.ReadFile(out); err != nil || string(b) != prompt {
		t.Fatalf("the agent saw %d bytes (err %v), want the prompt as one argument", len(b), err)
	}
}

func TestOneshotResumeUsesResumeOneshotTemplate(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	r := openOneshot(t, "go on", true, false, out)
	if tf := r.finished(t); tf.ExitCode != 4 {
		t.Fatalf("exit %d (%q), want 4", tf.ExitCode, tf.ErrorMessage)
	}
	if b, _ := os.ReadFile(out); string(b) != "resumed" {
		t.Fatalf("resume ran %q", b)
	}
}

// A oneshot with no prompt has nothing to run: refused before any process.
func TestOneshotOpenNeedsPrompt(t *testing.T) {
	r := openOneshot(t, "", false, false, filepath.Join(t.TempDir(), "out"))
	if tf := r.finished(t); tf.ExitCode != -1 || !bytes.HasPrefix(tf.ErrorMessage, []byte("prompt_required:")) {
		t.Fatalf("TaskFinished = %d %q", tf.ExitCode, tf.ErrorMessage)
	}
	if !r.stream.closed.Load() {
		t.Fatal("the exec stream was not closed")
	}
}

// --oneshot is a PTY session; an event-stream open cannot carry it.
func TestOneshotOnEventStreamRefused(t *testing.T) {
	r := openOneshot(t, "x", false, true, filepath.Join(t.TempDir(), "out"))
	if tf := r.finished(t); tf.ExitCode != -1 || !bytes.Contains(tf.ErrorMessage, []byte("oneshot")) {
		t.Fatalf("TaskFinished = %d %q", tf.ExitCode, tf.ErrorMessage)
	}
}

func TestBuildRunnerHelloAdvertisesOneshotSessions(t *testing.T) {
	if hh := buildRunnerHello(Config{MaxTasks: 1}); !hh.OneshotSessions() {
		t.Fatal("hello does not advertise oneshot_sessions")
	}
}
