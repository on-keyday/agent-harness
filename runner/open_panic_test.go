package runner

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// A panic while opening a session fails that task and closes its stream; it
// does not take the runner and every other task down with it. The oneshot
// path that A3 retired had this; --oneshot runs on handleOpenExec, so the
// guard lives there now.
func TestOpenExecPanicFailsOnlyThatTask(t *testing.T) {
	ms := &mockSender{}
	const streamID trsf.StreamID = 100
	stream := &noopBidiStream{streamID: streamID}
	lookup := &fakeBidiLookup{streams: map[trsf.StreamID]trsf.BidirectionalStream{streamID: stream}}
	s := &Session{Profiles: singleProfile(t, "/bin/true"), Sender: ms, Streams: lookup, Now: time.Now,
		testHookOpenExec: func() { panic("boom") }}
	oer := &protocol.OpenExecRunnerRequest{StreamId: uint64(streamID)}
	oer.TaskId.Id[0] = 0xDD
	oer.SetRepoPath([]byte(t.TempDir()))

	s.handleOpenExec(context.Background(), oer) // must return, not panic

	if len(ms.sent) == 0 {
		t.Fatal("nothing sent")
	}
	tf := decodeRunnerMsg(t, ms.sent[len(ms.sent)-1]).TaskFinished()
	if tf == nil || tf.ExitCode != -1 || !bytes.HasPrefix(tf.ErrorMessage, []byte("runner_panic")) {
		t.Fatalf("TaskFinished = %+v", tf)
	}
	if !stream.closed.Load() {
		t.Fatal("the exec stream was left open")
	}
}
