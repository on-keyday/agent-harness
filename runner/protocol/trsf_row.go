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
// itself, and only the caller has them.
//
// InternalState.SentPackets is deliberately not carried: it is an unbounded
// slice, and no reading of a stalled transfer needs per-packet detail.
func TrsfRowFrom(st *trsf.InternalState) TrsfConnState {
	return TrsfConnState{
		Mtu:            uint32(st.CurrentMTU),
		Cwnd:           uint32(st.CongestionWindow),
		BytesInFlight:  uint32(st.BytesInFlight),
		SrttUs:         uint64(st.SmoothedRTT.Microseconds()),
		RttvarUs:       uint64(st.RTTVariance.Microseconds()),
		SendQueue:      uint32(st.SendQueueLength),
		RecvQueue:      uint32(st.ReceiveQueueLength),
		SendStreams:    uint32(st.ActiveSendStreams),
		RecvStreams:    uint32(st.ActiveReceiveStreams),
		LoopIterations: st.LoopIterations,
		LossEvents:     uint64(st.Loss.Events),
		LossPackets:    uint64(st.Loss.Packets),
		LossSpurious:   uint64(st.Loss.Spurious),
		BlockedNs:      st.BlockedNs,
		Blocks:         st.Blocks,
		WakeTimer:      st.WakeTimer,
		WakeSend:       st.WakeSend,
		ArmedPacer:     st.ArmedPacer,
		SendPushApp:    st.SendPushApp,
		SendPushAck:    st.SendPushACK,
		SendPushSelf:   st.SendPushSelf,
		SendPushCwnd:   st.SendPushCwnd,
		SendPushLoss:   st.SendPushLoss,
		SendPushOther:  st.SendPushOther,
	}
}
