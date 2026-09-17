package protocol

import (
	"errors"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/objtrsf/trsf"
)

// capture drives ReportTo and hands back the bytes it tried to send. It also
// clears the rate limit first, so a test states the behaviour it is about
// rather than the floor between reports.
func capture(c *ForwardDropCounters, forwardID uint64) ([]byte, bool) {
	c.mu.Lock()
	c.lastSent = time.Time{}
	c.mu.Unlock()
	var got []byte
	_ = c.ReportTo(forwardID, func(b []byte) error { got = b; return nil })
	return got, got != nil
}

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
		b, ok := capture(&c, 7)
		if !ok {
			if tc.over+tc.cong+tc.queu != 0 {
				t.Errorf("%v produced no report although it should count", tc.err)
			}
			continue
		}
		var got ForwardDropReport
		if err := got.DecodeExact(b[1:]); err != nil {
			t.Fatalf("%v: decode: %v", tc.err, err)
		}
		if got.DroppedOversize != tc.over || got.DroppedCongestion != tc.cong || got.DroppedQueue != tc.queu {
			t.Errorf("%v -> oversize=%d congestion=%d queue=%d, want %d/%d/%d",
				tc.err, got.DroppedOversize, got.DroppedCongestion, got.DroppedQueue,
				tc.over, tc.cong, tc.queu)
		}
	}
}

func TestReportCarriesTheKindByteAndTheForwardID(t *testing.T) {
	var c ForwardDropCounters
	c.NoteOversize()
	b, ok := capture(&c, 0xDEAD)
	if !ok {
		t.Fatal("no report after a drop")
	}
	if appwire.AppKind(b[0]) != appwire.AppKind_ForwardDropReport {
		t.Fatalf("leading byte = 0x%02X, want the drop-report kind", b[0])
	}
	var got ForwardDropReport
	if err := got.DecodeExact(b[1:]); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ForwardId != 0xDEAD {
		t.Errorf("ForwardId = %d, want 0xDEAD: a report that does not name its forward cannot be filed", got.ForwardId)
	}
}

// Silent when nothing moved, so an idle forward sends nothing at all.
func TestReportIsSilentUntilSomethingChanges(t *testing.T) {
	var c ForwardDropCounters
	if _, ok := capture(&c, 1); ok {
		t.Error("a forward that has dropped nothing still sent a report")
	}
	c.NoteOversize()
	if _, ok := capture(&c, 1); !ok {
		t.Fatal("no report after the first drop")
	}
	if _, ok := capture(&c, 1); ok {
		t.Error("the same totals were reported twice")
	}
	c.NoteSendError(trsf.ErrCongestionBlocked)
	if _, ok := capture(&c, 1); !ok {
		t.Error("a new drop did not produce a report")
	}
}

// The totals are cumulative, which is what makes losing a report harmless --
// and this rides the same unreliable frame as the data it counts, so reports
// WILL be lost.
func TestTotalsAreCumulativeSoALostReportSelfHeals(t *testing.T) {
	var c ForwardDropCounters
	c.NoteOversize()
	if _, ok := capture(&c, 1); !ok {
		t.Fatal("no first report")
	}
	// Pretend that one never arrived. Two more drops happen.
	c.NoteOversize()
	c.NoteOversize()
	b, ok := capture(&c, 1)
	if !ok {
		t.Fatal("no second report")
	}
	var got ForwardDropReport
	if err := got.DecodeExact(b[1:]); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.DroppedOversize != 3 {
		t.Errorf("DroppedOversize = %d, want 3: a delta would have lost the first drop with the report that carried it",
			got.DroppedOversize)
	}
}

// A refused send must NOT be recorded as sent, or the totals it carried are
// lost for good: the next call compares against numbers that never crossed,
// finds nothing changed, and stays silent forever. "Cumulative, so a lost
// report self-heals" only holds if the lost one is still pending.
func TestARefusedReportIsRetried(t *testing.T) {
	var c ForwardDropCounters
	c.NoteOversize()

	refused := errors.New("no")
	if err := c.ReportTo(1, func([]byte) error { return refused }); err != refused {
		t.Fatalf("ReportTo returned %v, want the send error", err)
	}

	c.mu.Lock()
	c.lastSent = time.Time{}
	c.mu.Unlock()
	var got []byte
	if err := c.ReportTo(1, func(b []byte) error { got = b; return nil }); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got == nil {
		t.Fatal("the refused report was never retried: its counts are lost")
	}
}

// A drop storm must not answer with a report per dropped datagram, on the same
// path that is dropping them.
func TestReportsAreRateLimited(t *testing.T) {
	var c ForwardDropCounters
	sends := 0
	for i := 0; i < 100; i++ {
		c.NoteOversize()
		_ = c.ReportTo(1, func([]byte) error { sends++; return nil })
	}
	if sends != 1 {
		t.Errorf("%d reports for 100 drops in a burst, want 1: the floor between reports is not holding", sends)
	}
}
