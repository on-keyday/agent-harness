package runner

import (
	"bytes"
	"context"
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
}

func (h *heldStream) wait() { <-h.release }

// CloseBoth cancels the reads, as trsf's does.
func (h *heldStream) CloseBoth() error {
	h.once.Do(func() { close(h.release) })
	return h.noopBidiStream.CloseBoth()
}
func (h *heldStream) Cancel()                    { h.once.Do(func() { close(h.release) }) }
func (h *heldStream) Read(p []byte) (int, error) { h.wait(); return h.noopBidiStream.Read(p) }
func (h *heldStream) ReadContext(ctx context.Context, p []byte) (int, error) {
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
	repo := t.TempDir()
	ps, err := NewProfileSet(AgentProfile{
		Name:              "default",
		Bin:               "/bin/sh",
		OneshotArgv:       []string{"-c", `printf %s "$1" > ` + out + `; exit 3`, "sh", "{prompt}"},
		ResumeOneshotArgv: []string{"-c", `printf resumed > ` + out + `; exit 4`, "sh", "{prompt}"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ms := &mockSender{}
	const streamID, promptID trsf.StreamID = 100, 7
	stream := &noopBidiStream{streamID: streamID}
	held := &heldStream{noopBidiStream: stream, release: make(chan struct{})}
	release := time.AfterFunc(10*time.Second, func() { held.once.Do(func() { close(held.release) }) })
	t.Cleanup(func() { release.Stop(); held.once.Do(func() { close(held.release) }) })
	lookup := &fakeBidiLookup{streams: map[trsf.StreamID]trsf.BidirectionalStream{streamID: held}}
	oer := &protocol.OpenExecRunnerRequest{StreamId: uint64(streamID)}
	oer.TaskId.Id[0] = 0xC3
	oer.SetRepoPath([]byte(repo))
	oer.SetOneshot(true)
	oer.SetResumeConversation(resume)
	oer.SetEventStream(eventStream)
	if prompt != "" {
		lookup.recv = map[trsf.StreamID]trsf.ReceiveStream{promptID: &promptRecv{data: promptBodyBytes(prompt)}}
		oer.SetPromptPresent(true)
		oer.SetPromptStreamId(uint64(promptID))
	}
	s := &Session{AllowedRoots: []string{repo}, Profiles: ps, Sender: ms, Streams: lookup, Now: time.Now, NoWorktree: true}
	s.handleOpenExec(context.Background(), oer)
	return oneshotRun{ms: ms, stream: stream}
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
