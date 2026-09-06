package protocol

import "github.com/on-keyday/objtrsf/trsf"

// TrsfRowFrom projects one connection's transport state onto the wire record.
//
// ONE projection, not one per answerer. The server answers for its own
// connections and a runner for its own, and each used to build this struct by
// hand from the same `trsf.InternalState`. Two hand-written copies of one
// mapping is how a field arrives on one end and not the other — and the
// failure is silent in the worst way, because the reader sees zeros from the
// runner and real numbers from the server with nothing to distinguish that
// from a runner whose loop genuinely is idle.
//
// Role, PrincipalTask and Cid are deliberately NOT set here. They are the
// answerer's knowledge about the connection, not the transport's account of
// itself, and only the caller has them — which is the same identity/measurement
// line the row itself is built on.
//
// InternalState.SentPackets is deliberately not carried: it is an unbounded
// slice, and no reading of a stalled transfer needs per-packet detail.
func TrsfRowFrom(st *trsf.InternalState) TrsfConnState {
	c := make([]TrsfCounter, 0, 29)
	add := func(k TrsfCounterKey, v uint64) {
		c = append(c, TrsfCounter{Key: k, Value: v})
	}
	add(TrsfCounterKey_Mtu, uint64(st.CurrentMTU))
	add(TrsfCounterKey_Cwnd, uint64(st.CongestionWindow))
	add(TrsfCounterKey_BytesInFlight, uint64(st.BytesInFlight))
	add(TrsfCounterKey_SrttUs, uint64(st.SmoothedRTT.Microseconds()))
	add(TrsfCounterKey_RttvarUs, uint64(st.RTTVariance.Microseconds()))
	// Omitted rather than zeroed before the first ACK: the keyed list can say
	// "not measured", which no fixed field could, and a min_rtt of 0 would read
	// as a zero-latency path.
	if st.MinRTT > 0 {
		add(TrsfCounterKey_MinRttUs, uint64(st.MinRTT.Microseconds()))
	}
	add(TrsfCounterKey_SendQueue, uint64(st.SendQueueLength))
	add(TrsfCounterKey_RecvQueue, uint64(st.ReceiveQueueLength))
	add(TrsfCounterKey_SendActionCount, uint64(st.SendActionCount))
	add(TrsfCounterKey_UpdateWindowCount, uint64(st.UpdateWindowCount))
	add(TrsfCounterKey_CancelStreamCount, uint64(st.CancelStreamCount))
	add(TrsfCounterKey_SendStreams, uint64(st.ActiveSendStreams))
	add(TrsfCounterKey_RecvStreams, uint64(st.ActiveReceiveStreams))
	add(TrsfCounterKey_LoopIterations, st.LoopIterations)
	add(TrsfCounterKey_LossEvents, uint64(st.Loss.Events))
	add(TrsfCounterKey_LossPackets, uint64(st.Loss.Packets))
	add(TrsfCounterKey_LossSpurious, uint64(st.Loss.Spurious))
	add(TrsfCounterKey_BlockedNs, st.BlockedNs)
	add(TrsfCounterKey_Blocks, st.Blocks)
	add(TrsfCounterKey_WakeTimer, st.WakeTimer)
	add(TrsfCounterKey_WakeSend, st.WakeSend)
	add(TrsfCounterKey_ArmedPacer, st.ArmedPacer)
	add(TrsfCounterKey_SendPushApp, st.SendPushApp)
	add(TrsfCounterKey_SendPushAck, st.SendPushACK)
	add(TrsfCounterKey_SendPushSelf, st.SendPushSelf)
	add(TrsfCounterKey_SendPushCwnd, st.SendPushCwnd)
	add(TrsfCounterKey_SendPushLoss, st.SendPushLoss)
	add(TrsfCounterKey_SendPushOther, st.SendPushOther)

	row := TrsfConnState{CounterCount: uint16(len(c))}
	row.SetCounters(c)
	return row
}

// Counter reads one value out of a row.
//
// The second return separates "this answerer does not report that key" from
// "it reports zero", which is the whole reason the row is a list: a runner
// older than a counter omits it, and a reader that cannot tell absent from zero
// would render the omission as a measurement.
func (t *TrsfConnState) Counter(k TrsfCounterKey) (uint64, bool) {
	for i := range t.Counters {
		if t.Counters[i].Key == k {
			return t.Counters[i].Value, true
		}
	}
	return 0, false
}

// CounterOr is Counter for callers that have a sensible default and do not want
// the two-value form. Absent reads as the default, so use Counter wherever the
// difference is the point.
func (t *TrsfConnState) CounterOr(k TrsfCounterKey, def uint64) uint64 {
	if v, ok := t.Counter(k); ok {
		return v
	}
	return def
}
