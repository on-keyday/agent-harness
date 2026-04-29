//go:build !js

package exec

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/on-keyday/agent-harness/trsf"
)

// eofBidiStream is a minimal trsf.BidirectionalStream stub for tests.
// Its Read side returns io.EOF immediately (via a closed pipe) so handleInput
// exits cleanly. AppendData and CloseBoth are no-ops. All other interface
// methods are stubs that satisfy the compiler.
type eofBidiStream struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func newEOFBidiStream() *eofBidiStream {
	r, w := io.Pipe()
	// Close write end immediately so reads return io.EOF.
	w.Close()
	return &eofBidiStream{r: r, w: w}
}

// SendStream methods.
func (s *eofBidiStream) ID() trsf.StreamID                                                     { return 0 }
func (s *eofBidiStream) Write(p []byte) (int, error)                                           { return len(p), nil }
func (s *eofBidiStream) Close() error                                                          { return nil }
func (s *eofBidiStream) WriteContext(_ context.Context, p []byte) (int, error)                 { return len(p), nil }
func (s *eofBidiStream) HasSendData() bool                                                     { return false }
func (s *eofBidiStream) Completed() bool                                                       { return false }
func (s *eofBidiStream) AppendData(_ bool, _ ...[]byte) error                                  { return nil }
func (s *eofBidiStream) AppendDataContext(_ context.Context, _ bool, _ ...[]byte) error        { return nil }

// ReceiveStream methods.
func (s *eofBidiStream) Read(p []byte) (int, error)                                            { return s.r.Read(p) }
func (s *eofBidiStream) ReadContext(_ context.Context, p []byte) (int, error)                  { return s.r.Read(p) }
func (s *eofBidiStream) ReadDirectContext(_ context.Context, _ uint64) ([]byte, bool, error)   { return nil, true, nil }
func (s *eofBidiStream) ReadDirect(_ uint64) ([]byte, bool, error)                             { return nil, true, nil }
func (s *eofBidiStream) HasRecvData() bool                                                     { return false }
func (s *eofBidiStream) EOF() bool                                                             { return true }
func (s *eofBidiStream) Cancel()                                                               {}

// BidirectionalStream method.
func (s *eofBidiStream) CloseBoth() error { return nil }

// TestExecuteCommandWithOption_OnStdinWriter verifies that ExecuteCommandWithOption
// invokes OnStdinWriter with a write closure bound to the child's stdin pipe.
// It uses /bin/cat (echoes stdin to stdout) as the subprocess, writes one
// payload via the hook, then cancels the context to let the function return.
func TestExecuteCommandWithOption_OnStdinWriter(t *testing.T) {
	stream := newEOFBidiStream()
	captured := make(chan struct{}, 1)
	opt := ExecuteOption{
		OnStdinWriter: func(w func([]byte) (int, error)) {
			n, err := w([]byte("test\n"))
			if err != nil {
				t.Errorf("write err = %v", err)
			}
			if n != 5 {
				t.Errorf("write n = %d, want 5", n)
			}
			captured <- struct{}{}
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-captured
		cancel()
	}()
	logger := slog.Default()
	// Run /bin/cat which echoes stdin to stdout; we only verify that
	// OnStdinWriter is invoked and the returned writer accepts bytes without
	// error. Context cancellation terminates /bin/cat.
	_ = ExecuteCommandWithOption(ctx, stream, logger, "/bin/cat", nil, "", false, nil, opt)
}
