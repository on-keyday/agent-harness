package server

import (
	"log/slog"
	"sync"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// spliceBidiCounted pumps bytes between the two streams of one port-forward
// connection, counting them against pf and offering them to pf's taps on the
// way past.
//
// Teardown is either-side-wins, as spliceBidi's was and for the same reason: a
// TCP forward is not a guaranteed both-EOF request/response, so a half-closed
// or RST peer must not leave the reverse relay blocked forever.
//
// Direction is decided HERE, once, by which stream is the source — a→b is
// toward the target for both -L and -R, because the server always holds the
// initiator's stream as `a`. Deciding it per relay call would be the obvious
// place to invert it.
func spliceBidiCounted(a, b trsf.BidirectionalStream, taskIDHex string, pf *portForward, connSeq uint64) {
	var wg sync.WaitGroup
	var once sync.Once
	teardown := func() {
		once.Do(func() {
			_ = a.CloseBoth()
			_ = b.CloseBoth()
		})
	}
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer teardown()
		relayBytesCounted(a, b, pf, connSeq, protocol.ForwardTapDirection_ToTarget)
	}()
	go func() {
		defer wg.Done()
		defer teardown()
		relayBytesCounted(b, a, pf, connSeq, protocol.ForwardTapDirection_FromTarget)
	}()
	wg.Wait()

	// Read the connection's own halves before releasing them, so the record a
	// tap receives reports what THIS connection carried rather than the
	// forward's running totals.
	to, from := pf.connBytes(connSeq)
	pf.tapConnClose(connSeq, to, from)
	pf.closeConn(connSeq)
	slog.Info("port_forward: splice ended", "task_id", taskIDHex, "conn_seq", connSeq,
		"to_target", to, "from_target", from)
}

// relayBytesCounted is relayBytes with the two observers attached. It must stay
// as cheap as relayBytes: noteBytes is two atomic stores, and observe's tap
// fan-out is a non-blocking send per tap. Neither may block, because this
// goroutine IS the forward.
func relayBytesCounted(src, dst trsf.BidirectionalStream, pf *portForward, connSeq uint64, dir protocol.ForwardTapDirection) {
	for {
		data, eof, err := src.ReadDirect(64 * 1024)
		if err != nil {
			return
		}
		if len(data) > 0 {
			pf.noteBytes(dir, len(data))
			pf.observe(connSeq, dir, data)
			if werr := dst.AppendData(eof, data); werr != nil {
				return
			}
		} else if eof {
			_ = dst.AppendData(true)
		}
		if eof {
			return
		}
	}
}
