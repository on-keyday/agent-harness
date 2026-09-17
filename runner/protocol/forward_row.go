package protocol

// Counter reads one value out of a forward row.
//
// The second return separates "this row does not report that key" from "it
// reports zero", which is the whole reason the counters are a list: a tcp
// forward omits the datagram group entirely, and a listing that did not ask for
// the per-endpoint drops omits those. A reader that could not tell absent from
// zero would render either omission as a measurement.
func (p *PortForwardInfo) Counter(k ForwardCounterKey) (uint64, bool) {
	for i := range p.Counters {
		if p.Counters[i].Key == k {
			return p.Counters[i].Value, true
		}
	}
	return 0, false
}

// SetCounter appends or replaces one value, so a producer filling a row in
// stages cannot end up emitting the same key twice.
func (p *PortForwardInfo) SetCounter(k ForwardCounterKey, v uint64) {
	for i := range p.Counters {
		if p.Counters[i].Key == k {
			p.Counters[i].Value = v
			return
		}
	}
	p.SetCounters(append(p.Counters, ForwardCounter{Key: k, Value: v}))
}

// ForwardDropKeys names the three causes for one hop, in the order every
// surface renders them: oversize, congestion, queue.
//
// A function rather than three constants at each call site, because the hops
// differ only by which triple they use and a surface that hard-coded one hop's
// keys is how the next hop gets forgotten.
func ForwardDropKeys(hop ForwardHop) [3]ForwardCounterKey {
	switch hop {
	case ForwardHopClient:
		return [3]ForwardCounterKey{
			ForwardCounterKey_ClientDroppedOversize,
			ForwardCounterKey_ClientDroppedCongestion,
			ForwardCounterKey_ClientDroppedQueue,
		}
	case ForwardHopRunner:
		return [3]ForwardCounterKey{
			ForwardCounterKey_RunnerDroppedOversize,
			ForwardCounterKey_RunnerDroppedCongestion,
			ForwardCounterKey_RunnerDroppedQueue,
		}
	default:
		return [3]ForwardCounterKey{
			ForwardCounterKey_RelayDroppedOversize,
			ForwardCounterKey_RelayDroppedCongestion,
			ForwardCounterKey_RelayDroppedQueue,
		}
	}
}

// ForwardHop is which of the three parties a drop happened at. Not on the wire
// -- the wire spells the hop into the key -- but the thing a producer and a
// renderer both loop over.
type ForwardHop uint8

const (
	ForwardHopRelay ForwardHop = iota
	ForwardHopClient
	ForwardHopRunner
)

// String names the hop as an operator reads it.
func (h ForwardHop) String() string {
	switch h {
	case ForwardHopClient:
		return "client"
	case ForwardHopRunner:
		return "runner"
	default:
		return "relay"
	}
}

// SetForwardDropCounters writes one hop's triple onto a row.
func (p *PortForwardInfo) SetForwardDropCounters(hop ForwardHop, b ForwardDropsBody) {
	k := ForwardDropKeys(hop)
	p.SetCounter(k[0], b.DroppedOversize)
	p.SetCounter(k[1], b.DroppedCongestion)
	p.SetCounter(k[2], b.DroppedQueue)
}
