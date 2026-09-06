package runner

import (
	"context"
	"testing"
	"time"

	"github.com/on-keyday/objtrsf/trsf"
)

// drainProbe is a stream whose receive half hands out chunks and then, if
// endsWithEOF, reports EOF. It records whether CloseBoth ran after that EOF was
// consumed, which is the whole invariant here: trsf sends a cancel for a
// receive half closed while it has not reported EOF, and a recvStream reports
// EOF only once a read has consumed through the EOF chunk.
//
// The embedded interface is nil on purpose. closeServedStream may call only the
// three methods below; anything else is a nil panic naming the new call rather
// than a silent zero value.
type drainProbe struct {
	trsf.BidirectionalStream

	chunk          []byte // handed out on each read until the budget or ctx ends
	endsWithEOF    bool
	blockUntilDone bool // a client that neither sends nor half-closes

	reads          int
	eof            bool
	closed         bool
	closedAfterEOF bool
}

func (p *drainProbe) EOF() bool { return p.eof }

func (p *drainProbe) ReadDirectContext(ctx context.Context, maxN uint64) ([]byte, bool, error) {
	if p.blockUntilDone {
		<-ctx.Done()
		return nil, false, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	p.reads++
	if p.endsWithEOF {
		p.eof = true
		return nil, true, nil
	}
	c := p.chunk
	if uint64(len(c)) > maxN {
		c = c[:maxN]
	}
	return c, false, nil
}

func (p *drainProbe) CloseBoth() error {
	p.closed = true
	p.closedAfterEOF = p.eof
	return nil
}

// The reason the whole helper exists: every direction except push and dir_push
// writes without ever reading, so without this the close cancels a stream that
// ran to completion and the peer logs "received cancel for unknown stream".
func TestClosingAServedStreamConsumesTheClientsEOF(t *testing.T) {
	p := &drainProbe{endsWithEOF: true}
	closeServedStream(context.Background(), p)

	if p.reads == 0 {
		t.Fatalf("the close never read, so the peer is still told the transfer was abandoned")
	}
	if !p.eof {
		t.Fatalf("the receive half never reported EOF")
	}
	if !p.closed {
		t.Fatalf("the stream was not closed")
	}
	if !p.closedAfterEOF {
		t.Fatalf("closed before the EOF was consumed, which is exactly what emits the cancel")
	}
}

// A stream already at EOF -- push and dir_push, which read the body themselves
// -- must not cost a read.
func TestClosingSkipsAStreamAlreadyAtEOF(t *testing.T) {
	p := &drainProbe{eof: true}
	closeServedStream(context.Background(), p)

	if p.reads != 0 {
		t.Fatalf("read %d times a stream that had already reported EOF", p.reads)
	}
	if !p.closed || !p.closedAfterEOF {
		t.Fatalf("closed=%v closedAfterEOF=%v", p.closed, p.closedAfterEOF)
	}
}

// A client that really did stop early still has the rest of a file coming, and
// draining that would mean reading bytes nobody wants. The budget gives up and
// lets the cancel happen -- there the cancel is the correct signal.
func TestDrainGivesUpAtTheByteBudget(t *testing.T) {
	const chunk = 4096
	p := &drainProbe{chunk: make([]byte, chunk)}
	closeServedStream(context.Background(), p)

	if want := recvEOFDrainBudget / chunk; p.reads != want {
		t.Fatalf("read %d chunks, want the budget's %d", p.reads, want)
	}
	if p.closedAfterEOF {
		t.Fatalf("claimed EOF on a stream that never sent one")
	}
	if !p.closed {
		t.Fatalf("giving up must still close the stream")
	}
}

// A client that neither sends nor half-closes must not hold the serving
// goroutine open. The parent deadline is shorter than recvEOFDrainTimeout, so
// this asserts the drain honours whichever bound fires first.
func TestDrainGivesUpWhenTheContextDoes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	p := &drainProbe{blockUntilDone: true}
	done := make(chan struct{})
	go func() {
		defer close(done)
		closeServedStream(ctx, p)
	}()

	select {
	case <-done:
	case <-time.After(recvEOFDrainTimeout):
		t.Fatal("the drain outlived the context it was given")
	}
	if !p.closed {
		t.Fatalf("a timed-out drain must still close the stream")
	}
}
