package protocol

import (
	"errors"
	"testing"

	"github.com/on-keyday/objtrsf/trsf"
)

// Each trsf refusal has to land on the cause whose remedy matches it, because
// the three counters are read as three different instructions to the operator:
// oversize means the application must send smaller, congestion means reduce
// offered load, queue means look at host load.
func TestSendErrorsMapToTheCauseWithTheMatchingRemedy(t *testing.T) {
	for _, tc := range []struct {
		err              error
		over, cong, queu uint64
	}{
		{nil, 0, 0, 0},
		{trsf.ErrCongestionBlocked, 0, 1, 0},
		{trsf.ErrDatagramTooLarge, 1, 0, 0},
		{trsf.ErrDatagramQueueFull, 0, 0, 1},
		{errors.New("something new in that family"), 0, 0, 1},
	} {
		var c ForwardDropCounters
		c.NoteSendError(tc.err)
		got := c.Snapshot(7)
		if got.DroppedOversize != tc.over || got.DroppedCongestion != tc.cong || got.DroppedQueue != tc.queu {
			t.Errorf("%v -> oversize=%d congestion=%d queue=%d, want %d/%d/%d",
				tc.err, got.DroppedOversize, got.DroppedCongestion, got.DroppedQueue,
				tc.over, tc.cong, tc.queu)
		}
	}
}

// A snapshot that does not name its forward cannot be filed against one, and
// the asker holds several.
func TestSnapshotNamesItsForward(t *testing.T) {
	var c ForwardDropCounters
	c.NoteOversize()
	if got := c.Snapshot(0xDEAD); got.ForwardId != 0xDEAD {
		t.Errorf("ForwardId = %d, want 0xDEAD", got.ForwardId)
	}
}

// Cumulative, never reset by a read. Two readers poll independently -- a
// `forward ls --drops` and a --watch can overlap -- so a snapshot that consumed
// the counts would hand each of them a different, wrong answer.
func TestSnapshotDoesNotResetTheTotals(t *testing.T) {
	var c ForwardDropCounters
	c.NoteOversize()
	if got := c.Snapshot(1); got.DroppedOversize != 1 {
		t.Fatalf("first read = %d, want 1", got.DroppedOversize)
	}
	if got := c.Snapshot(1); got.DroppedOversize != 1 {
		t.Errorf("second read = %d, want 1: the read consumed the count", got.DroppedOversize)
	}
	c.NoteOversize()
	c.NoteOversize()
	if got := c.Snapshot(1); got.DroppedOversize != 3 {
		t.Errorf("after two more = %d, want 3", got.DroppedOversize)
	}
}
