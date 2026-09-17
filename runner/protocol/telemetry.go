package protocol

import (
	"fmt"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
)

// TelemetrySource is the numbers a peer can be asked for.
//
// A runner and a cli client differ only in where those numbers come from — one
// walks a registry of connections it holds, the other holds exactly one — so
// that is the only thing either supplies. Decoding the question, stamping the
// answer and encoding it are identical on both, and live in AnswerTelemetry
// rather than in each; two copies of that is what this replaced.
type TelemetrySource interface {
	// TrsfStates reports every connection this peer holds.
	//
	// No visibility policy is applied here. The SERVER decides which rows a
	// caller may see, because it is the only party that evaluates scope; a
	// second implementation of that rule on the answering side is drift this
	// design has already paid for once.
	TrsfStates() []TrsfConnState

	// ForwardDrops reports what this peer refused to send for one forward, and
	// false when it holds no forward by that id.
	ForwardDrops(forwardID uint64) (ForwardDropsBody, bool)
}

// AnswerTelemetry decodes one request, asks src for the numbers, and hands send
// the encoded reply with its AppKind byte already on the front.
//
// Every outcome the answerer can name is answered, including the ones it cannot
// serve. Staying silent also "works" — the asker times out — but it costs three
// seconds to learn something this side knew immediately, and a timeout cannot
// be told apart from a peer that has gone away.
func AnswerTelemetry(payload []byte, src TelemetrySource, send func([]byte) error) error {
	var req TelemetryRequest
	if _, err := req.Decode(payload); err != nil {
		// The one case with nothing to answer TO: request_id is inside the
		// bytes that would not decode, so any reply would be addressed to a
		// caller picked at random.
		return fmt.Errorf("decode TelemetryRequest: %w", err)
	}
	resp := TelemetryResponse{Kind: req.Kind, RequestId: req.RequestId, Status: TelemetryStatus_Ok}
	switch req.Kind {
	case TelemetryKind_TrsfState:
		rows := src.TrsfStates()
		resp.SetTrsfState(TelemetryTrsfStateBody{
			Count: uint16(len(rows)),
			// Stamped HERE, by the clock the counters advanced against, and
			// passed through by the asker rather than restamped: the delta
			// between two readings is the interval every counter on a row is
			// read as a rate over, and the asker's clock is a round trip away.
			SampledUnixNs: uint64(time.Now().UnixNano()),
			Conns:         rows,
		})
	case TelemetryKind_ForwardDrops:
		q := req.ForwardDrops()
		if q == nil {
			return fmt.Errorf("forward_drops request carries no forward id")
		}
		body, ok := src.ForwardDrops(q.ForwardId)
		if !ok {
			// Refused, but the arm is still filled in. A kind the schema knows
			// always carries its body, so leaving it out is an encode failure
			// rather than an absence -- and the counts stay zero while the
			// forward id says which registration was not found.
			resp.Status = TelemetryStatus_UnknownForward
			body = ForwardDropsBody{ForwardId: q.ForwardId}
		}
		resp.SetForwardDrops(body)
	default:
		// A kind newer than this build. Answerable because no arm matches it on
		// either side: the request decoded without a body, and the response
		// encodes without one, so neither end has to know what the kind meant.
		resp.Status = TelemetryStatus_Unsupported
	}
	b, err := resp.Append([]byte{byte(appwire.AppKind_Telemetry)})
	if err != nil {
		return fmt.Errorf("encode TelemetryResponse: %w", err)
	}
	return send(b)
}
