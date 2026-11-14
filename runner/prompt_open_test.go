package runner

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/exec/frame"
	"github.com/on-keyday/objtrsf/trsf"
)

// promptRecv serves one body then EOF.
type promptRecv struct {
	trsf.ReceiveStream
	data   []byte
	served bool
}

func (r *promptRecv) ReadDirect(uint64) ([]byte, bool, error) {
	if r.served {
		return nil, true, nil
	}
	r.served = true
	return r.data, false, nil
}

func promptBodyBytes(p string) []byte {
	b := protocol.PromptBody{}
	b.SetPrompt([]byte(p))
	return b.MustAppend(nil)
}

// A prompt sent to a profile whose interactive template has no {prompt} ends
// the task with prompt_unsupported, the stream closed (A-D3).
func TestOpenExecWithPromptOnProfileWithoutTokenFinishesPromptUnsupported(t *testing.T) {
	ms := &mockSender{}
	const streamID, promptID trsf.StreamID = 99, 7
	stream := &noopBidiStream{streamID: streamID}
	lookup := &fakeBidiLookup{
		streams: map[trsf.StreamID]trsf.BidirectionalStream{streamID: stream},
		recv:    map[trsf.StreamID]trsf.ReceiveStream{promptID: &promptRecv{data: promptBodyBytes("triage")}},
	}
	s := &Session{Profiles: singleProfile(t, "/bin/true"), Sender: ms, Streams: lookup, Now: time.Now}
	var tid [16]byte
	tid[0] = 0xAB
	oer := &protocol.OpenExecRunnerRequest{TaskId: protocol.TaskID{Id: tid}, StreamId: uint64(streamID)}
	oer.SetRepoPath([]byte(t.TempDir()))
	oer.SetPromptPresent(true)
	oer.SetPromptStreamId(uint64(promptID))

	s.handleOpenExec(context.Background(), oer)

	if !stream.closed.Load() {
		t.Fatal("the exec stream was not closed")
	}
	last := decodeRunnerMsg(t, ms.sent[len(ms.sent)-1])
	tf := last.TaskFinished()
	if tf == nil || tf.ExitCode != -1 || !bytes.HasPrefix(tf.ErrorMessage, []byte("prompt_unsupported:")) {
		t.Fatalf("TaskFinished = %+v", tf)
	}
	// The client attached to the stream is told why, ahead of the close.
	if reason := endFrameSent(t, stream); reason != string(tf.ErrorMessage) {
		t.Fatalf("End frame reason %q, want the task's %q", reason, tf.ErrorMessage)
	}
}

// endFrameSent decodes what the runner wrote to stream and returns the End
// frame's reason, failing when there is none or it came after the close.
func endFrameSent(t *testing.T, stream *noopBidiStream) string {
	t.Helper()
	sent, late := stream.sentBytes()
	if late {
		t.Fatal("a frame was written after the stream was closed")
	}
	r := bytes.NewReader(sent)
	for r.Len() > 0 {
		f := &frame.Frame{}
		if err := f.Read(r); err != nil {
			t.Fatalf("decode sent frames: %v", err)
		}
		if ctrl := f.Control(); ctrl != nil && ctrl.Type == frame.ControlType_End {
			if e := ctrl.End(); e != nil {
				if e.ExitCode != -1 {
					t.Fatalf("End exit %d, want -1 (no process ran)", e.ExitCode)
				}
				return string(e.Reason)
			}
		}
	}
	t.Fatal("no End frame was sent before the close")
	return ""
}

func TestWaitForPromptBodyRefusesOverCap(t *testing.T) {
	lookup := &fakeBidiLookup{recv: map[trsf.StreamID]trsf.ReceiveStream{
		1: &promptRecv{data: promptBodyBytes(strings.Repeat("a", protocol.PromptMax+1))},
		2: &promptRecv{data: promptBodyBytes("ok")},
	}}
	if _, err := waitForPromptBody(context.Background(), lookup, 1); err == nil {
		t.Fatal("an over-cap prompt was accepted")
	}
	if p, err := waitForPromptBody(context.Background(), lookup, 2); err != nil || p != "ok" {
		t.Fatalf("prompt %q, %v", p, err)
	}
}

// The prompt stream is read before any gate can refuse the open, so a refused
// open does not leave it buffered (runner review M1).
func TestPromptReadBeforeRepoGate(t *testing.T) {
	ms := &mockSender{}
	const streamID, promptID trsf.StreamID = 99, 7
	stream := &noopBidiStream{streamID: streamID}
	pr := &promptRecv{data: promptBodyBytes("triage")}
	lookup := &fakeBidiLookup{
		streams: map[trsf.StreamID]trsf.BidirectionalStream{streamID: stream},
		recv:    map[trsf.StreamID]trsf.ReceiveStream{promptID: pr},
	}
	s := &Session{AllowedRoots: []string{"/allowed"}, Profiles: singleProfile(t, "/bin/true"), Sender: ms, Streams: lookup, Now: time.Now}
	oer := &protocol.OpenExecRunnerRequest{TaskId: protocol.TaskID{Id: [16]byte{0xCD}}, StreamId: uint64(streamID)}
	oer.SetRepoPath([]byte("/disallowed"))
	oer.SetPromptPresent(true)
	oer.SetPromptStreamId(uint64(promptID))
	s.handleOpenExec(context.Background(), oer)
	if !pr.served {
		t.Fatal("a refused open left its prompt stream unread")
	}
}
