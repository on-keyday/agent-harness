package protocol

import (
	"sync/atomic"

	"github.com/on-keyday/objtrsf/trsf"
)

// ForwardDropCounters is one endpoint's tally of the datagrams it did not carry
// for a forward.
//
// It lives here rather than in cli and runner because both count the same three
// causes off the same three trsf errors, and the server already maps those same
// errors to the same causes on its own relay. Three copies of that switch is
// three places to disagree about what a drop means.
type ForwardDropCounters struct {
	oversize   atomic.Uint64
	congestion atomic.Uint64
	queue      atomic.Uint64
}

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

// Snapshot answers a forward_drops question with the running totals.
//
// These used to be PUSHED: the drop site sent a report on the datagram frame,
// rate limited to one per five seconds, keeping the last-sent totals here so a
// refused report could be retried rather than lost for good. All of that state
// belonged to the direction rather than to the counters — a pull has nothing in
// flight, so there is nothing to retry, nothing to rate limit, and the numbers
// no longer compete for the queue they are reporting on.
func (c *ForwardDropCounters) Snapshot(forwardID uint64) ForwardDropsBody {
	return ForwardDropsBody{
		ForwardId:         forwardID,
		DroppedOversize:   c.oversize.Load(),
		DroppedCongestion: c.congestion.Load(),
		DroppedQueue:      c.queue.Load(),
	}
}
