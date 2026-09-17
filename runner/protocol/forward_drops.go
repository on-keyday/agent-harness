package protocol

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/objtrsf/trsf"
)

// ForwardDropCounters is one endpoint's tally of the datagrams it did not carry
// for a forward, and the thing that turns that tally into a report.
//
// It lives here rather than in cli and runner because both count the same three
// causes off the same three trsf errors, and the server already maps those same
// errors to the same causes on its own relay. Three copies of that switch is
// three places to disagree about what a drop means.
type ForwardDropCounters struct {
	oversize   atomic.Uint64
	congestion atomic.Uint64
	queue      atomic.Uint64

	// Guards the reporting state below. The drop sites are several goroutines
	// -- a listener loop and one reply reader per flow -- and reporting from
	// the site that noticed is what lets this work without a ticker, so the
	// state it reads has to tolerate them.
	mu       sync.Mutex
	sent     [3]uint64
	lastSent time.Time
}

// ForwardDropReportInterval is the floor between two reports for one forward.
//
// Reporting from the drop site rather than a ticker means no goroutine, no
// timer and no lifetime to manage -- but a drop storm would otherwise send one
// report per dropped datagram, on the same congested path that is dropping
// them. This is the rate limit that makes that safe, and it is a floor rather
// than a period: a single drop on an otherwise quiet forward is reported at
// once.
const ForwardDropReportInterval = 5 * time.Second

// NoteOversize records a payload refused before trsf ever saw it, because it
// did not fit this leg's MaxDatagramSize.
//
// This is the one the server cannot infer. Its own oversize counter fires when
// the FAR leg cannot take a datagram it already received; a payload too large
// for the near leg is dropped here and never arrives, which is why a row could
// read oversize=0 while the sender logged every one.
func (c *ForwardDropCounters) NoteOversize() { c.oversize.Add(1) }

// NoteSendError records the outcome of a SendDatagram. A nil error is not a
// drop and is counted nowhere; the caller passes it anyway so the call site
// reads as "account for this send" rather than as a branch.
func (c *ForwardDropCounters) NoteSendError(err error) {
	switch {
	case err == nil:
	case err == trsf.ErrCongestionBlocked:
		c.congestion.Add(1)
	case err == trsf.ErrDatagramTooLarge:
		c.oversize.Add(1)
	default:
		// Includes ErrDatagramQueueFull, and anything later added to that
		// family: a send this process refused for its own reasons rather than
		// the window's. The operator reading `queued` is being told to look at
		// host load, which is the right place for every member of it.
		c.queue.Add(1)
	}
}

// ReportTo sends the current totals through send, and records them as sent
// ONLY if that succeeded. Returns the error send gave, or nil when there was
// nothing to report or the rate limit swallowed it.
//
// The success check is load-bearing and its absence was a real bug: marking the
// totals sent before the send happened meant a refused report was lost for
// good, because the next tick compared against totals that never crossed and
// found nothing changed. "Cumulative, so a lost report self-heals" is only true
// if a lost one is still pending.
func (c *ForwardDropCounters) ReportTo(forwardID uint64, send func([]byte) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := [3]uint64{c.oversize.Load(), c.congestion.Load(), c.queue.Load()}
	if now == c.sent {
		return nil
	}
	if at := time.Now(); at.Sub(c.lastSent) < ForwardDropReportInterval {
		return nil // rate limited; the totals are cumulative, so nothing is lost
	} else {
		c.lastSent = at
	}
	r := ForwardDropReport{
		ForwardId:         forwardID,
		DroppedOversize:   now[0],
		DroppedCongestion: now[1],
		DroppedQueue:      now[2],
	}
	b, err := r.Append([]byte{byte(appwire.AppKind_ForwardDropReport)})
	if err != nil {
		return err
	}
	if err := send(b); err != nil {
		return err
	}
	c.sent = now
	return nil
}
