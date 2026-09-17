package protocol

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
)

type fakeSource struct {
	rows  []TrsfConnState
	drops map[uint64]ForwardDropsBody
}

func (f fakeSource) TrsfStates() []TrsfConnState { return f.rows }

func (f fakeSource) ForwardDrops(id uint64) (ForwardDropsBody, bool) {
	b, ok := f.drops[id]
	return b, ok
}

// answer runs one request through AnswerTelemetry and decodes what came back,
// checking on the way that the reply is framed as its own AppKind. A peer that
// answered on the wrong kind would be routed to the control seam and the asker
// would wait out its timeout.
func answer(t *testing.T, req []byte, src TelemetrySource) TelemetryResponse {
	t.Helper()
	var sent []byte
	if err := AnswerTelemetry(req, src, func(b []byte) error { sent = b; return nil }); err != nil {
		t.Fatalf("AnswerTelemetry: %v", err)
	}
	if len(sent) == 0 {
		t.Fatal("nothing was sent: the asker learns nothing until its timeout")
	}
	if appwire.AppKind(sent[0]) != appwire.AppKind_Telemetry {
		t.Fatalf("leading byte = 0x%02X, want the telemetry kind", sent[0])
	}
	var resp TelemetryResponse
	if err := resp.DecodeExact(sent[1:]); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

func mustRequest(t *testing.T, req TelemetryRequest) []byte {
	t.Helper()
	b, err := req.Append(nil)
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	return b
}

// The rows come back, and with the ANSWERER's clock on them: every counter on a
// row is read as a rate, and the interval has to be measured where the counters
// advanced rather than a round trip away.
func TestTrsfStateAnswerCarriesTheRowsAndTheAnswerersStamp(t *testing.T) {
	var row TrsfConnState
	row.Role = ConnRole_Cli
	row.SetCid([]byte("ws:127.0.0.1:1-2"))
	before := time.Now().UnixNano()

	resp := answer(t, mustRequest(t, TelemetryRequest{Kind: TelemetryKind_TrsfState, RequestId: 42}),
		fakeSource{rows: []TrsfConnState{row}})

	if resp.Status != TelemetryStatus_Ok {
		t.Fatalf("status = %v, want ok", resp.Status)
	}
	if resp.RequestId != 42 {
		t.Errorf("RequestId = %d, want 42: an answer nobody can correlate is a timeout", resp.RequestId)
	}
	body := resp.TrsfState()
	if body == nil {
		t.Fatal("an ok answer carried no body")
	}
	if body.Count != 1 || len(body.Conns) != 1 {
		t.Fatalf("Count=%d rows=%d, want 1", body.Count, len(body.Conns))
	}
	if got := string(body.Conns[0].Cid); got != "ws:127.0.0.1:1-2" {
		t.Errorf("cid = %q, want the row's own", got)
	}
	if int64(body.SampledUnixNs) < before {
		t.Errorf("SampledUnixNs = %d, want a stamp taken at answer time (>= %d)", body.SampledUnixNs, before)
	}
}

func TestForwardDropsAnswerCarriesTheNamedForward(t *testing.T) {
	req := TelemetryRequest{Kind: TelemetryKind_ForwardDrops, RequestId: 7}
	req.SetForwardDrops(ForwardDropsQuery{ForwardId: 9})
	src := fakeSource{drops: map[uint64]ForwardDropsBody{
		9: {ForwardId: 9, DroppedOversize: 1, DroppedCongestion: 2, DroppedQueue: 3},
	}}

	resp := answer(t, mustRequest(t, req), src)

	if resp.Status != TelemetryStatus_Ok {
		t.Fatalf("status = %v, want ok", resp.Status)
	}
	body := resp.ForwardDrops()
	if body == nil {
		t.Fatal("an ok answer carried no body")
	}
	if body.ForwardId != 9 || body.DroppedOversize != 1 || body.DroppedCongestion != 2 || body.DroppedQueue != 3 {
		t.Errorf("body = %+v, want the source's counts for forward 9", *body)
	}
}

// "I hold no such forward" is an ANSWER. Staying silent also ends the exchange,
// but only after the asker's full timeout, and it cannot then be told apart
// from a peer that has gone away.
func TestAnUnknownForwardIsAnsweredRatherThanIgnored(t *testing.T) {
	req := TelemetryRequest{Kind: TelemetryKind_ForwardDrops, RequestId: 5}
	req.SetForwardDrops(ForwardDropsQuery{ForwardId: 404})

	resp := answer(t, mustRequest(t, req), fakeSource{})

	if resp.Status != TelemetryStatus_UnknownForward {
		t.Fatalf("status = %v, want unknown_forward", resp.Status)
	}
	if resp.RequestId != 5 {
		t.Errorf("RequestId = %d, want 5", resp.RequestId)
	}
	// The body rides along because the schema gives every known kind one; what
	// makes it safe is that it is zero and names the forward that was not
	// found. A caller that reads it as counts has skipped the status, which is
	// why askTelemetry turns a non-ok status into an error for them.
	body := resp.ForwardDrops()
	if body == nil {
		t.Fatal("a known kind lost its arm")
	}
	if body.ForwardId != 404 {
		t.Errorf("ForwardId = %d, want the 404 that was asked about", body.ForwardId)
	}
	if body.DroppedOversize|body.DroppedCongestion|body.DroppedQueue != 0 {
		t.Errorf("a refusal carried non-zero counts: %+v", *body)
	}
}

// The skew case the whole status field exists for: a NEWER asker names a kind
// this build has never heard of.
//
// Two things have to hold at once, and the second is why TelemetryRequest has
// no catch-all `error(..)` arm. The request must DECODE far enough to recover
// the request id -- an answerer that cannot decode cannot reply to anything --
// and the response must ENCODE while naming a kind whose body this build cannot
// write, which works only because the body is gated behind status == ok.
func TestAKindThisBuildDoesNotKnowIsRefusedByName(t *testing.T) {
	const unknownKind = 0x7F
	// Hand-built: the generated encoder cannot be asked to write a kind that is
	// not in the enum. kind:u8 then request_id:u32, big-endian.
	req := []byte{unknownKind, 0x00, 0x00, 0x01, 0x23}

	resp := answer(t, req, fakeSource{})

	if resp.Status != TelemetryStatus_Unsupported {
		t.Fatalf("status = %v, want unsupported", resp.Status)
	}
	if resp.RequestId != 0x123 {
		t.Errorf("RequestId = %#x, want 0x123: a refusal nobody can correlate is still a timeout", resp.RequestId)
	}
	if uint8(resp.Kind) != unknownKind {
		t.Errorf("Kind = %d, want the kind that was asked (%d) echoed back", resp.Kind, unknownKind)
	}
}
