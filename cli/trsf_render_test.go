package cli

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// trsfState builds one answerer row. counters is a map because Counter() scans
// by key: what a row carries matters here, the order it carries it in does not.
func trsfState(cid string, role protocol.ConnRole, counters map[protocol.TrsfCounterKey]uint64) protocol.TrsfConnState {
	var s protocol.TrsfConnState
	s.SetCid([]uint8(cid))
	s.Role = role
	c := make([]protocol.TrsfCounter, 0, len(counters))
	for k, v := range counters {
		c = append(c, protocol.TrsfCounter{Key: k, Value: v})
	}
	s.SetCounters(c)
	return s
}

const (
	t0 = int64(1_000_000_000)
	t1 = t0 + int64(time.Second) // the ANSWERER's clock, one second on
)

// TestTrsfFirstReadingHasNoDeltas: a single sample defines no rate. Every
// column that only exists as a change must say so rather than print a zero the
// reader would take for a measurement.
func TestTrsfFirstReadingHasNoDeltas(t *testing.T) {
	var s TrsfSampler
	rows := s.Observe([]protocol.TrsfConnState{
		trsfState("udp:10.0.0.3:41233-a1", protocol.ConnRole_Runner, map[protocol.TrsfCounterKey]uint64{
			protocol.TrsfCounterKey_Cwnd:           131072,
			protocol.TrsfCounterKey_BytesInFlight:  64240,
			protocol.TrsfCounterKey_SrttUs:         18000,
			protocol.TrsfCounterKey_MinRttUs:       14000,
			protocol.TrsfCounterKey_LossEvents:     3,
			protocol.TrsfCounterKey_LoopIterations: 400,
		}),
	}, t0)

	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	r := rows[0]
	// Present-value columns render on the first reading.
	if r.Cwnd != "131072" || r.InFlight != "64240" {
		t.Errorf("cwnd/inflight = %q/%q, want 131072/64240", r.Cwnd, r.InFlight)
	}
	if r.SRTT != "18ms" {
		t.Errorf("srtt = %q, want 18ms", r.SRTT)
	}
	if r.Queue != "4ms" {
		t.Errorf("queue = %q, want 4ms (srtt-minrtt)", r.Queue)
	}
	// Delta columns do not.
	for name, got := range map[string]string{
		"LossD": r.LossD, "SpurD": r.SpurD, "LoopD": r.LoopD,
		"BlockPct": r.BlockPct, "Wait": r.Wait,
	} {
		if got != "-" {
			t.Errorf("%s = %q on a first reading, want %q", name, got, "-")
		}
	}
}

// TestTrsfBlockPctUsesTheAnswerersElapsed: BLOCK% is a share of the interval the
// counters advanced over, and that interval is measured on the answering host.
// Dividing a remote delta by a local elapsed is what printed 135%.
func TestTrsfBlockPctUsesTheAnswerersElapsed(t *testing.T) {
	var s TrsfSampler
	// Every key the second reading has, at zero: TrsfRowFrom adds the push
	// counters unconditionally, so a real previous reading always carries
	// them. Omitting them here would exercise the absent-on-one-side rule
	// (TestTrsfAbsentCounterStaysAbsent owns that) instead of the label.
	first := map[protocol.TrsfCounterKey]uint64{
		protocol.TrsfCounterKey_BlockedNs:   0,
		protocol.TrsfCounterKey_Blocks:      0,
		protocol.TrsfCounterKey_WakeTimer:   0,
		protocol.TrsfCounterKey_WakeSend:    0,
		protocol.TrsfCounterKey_SendPushApp: 0,
		protocol.TrsfCounterKey_SendPushAck: 0,
	}
	second := map[protocol.TrsfCounterKey]uint64{
		protocol.TrsfCounterKey_BlockedNs: uint64(870 * time.Millisecond),
		protocol.TrsfCounterKey_Blocks:    10,
		protocol.TrsfCounterKey_WakeTimer: 0,
		protocol.TrsfCounterKey_WakeSend:  10,
		// send-dominant, and app is the dominant push
		protocol.TrsfCounterKey_SendPushApp: 9,
		protocol.TrsfCounterKey_SendPushAck: 1,
	}
	s.Observe([]protocol.TrsfConnState{trsfState("c1", protocol.ConnRole_Cli, first)}, t0)
	rows := s.Observe([]protocol.TrsfConnState{trsfState("c1", protocol.ConnRole_Cli, second)}, t1)

	if rows[0].BlockPct != "87%" {
		t.Errorf("BLOCK%% = %q, want 87%% (870ms parked over the answerer's 1s)", rows[0].BlockPct)
	}
	if rows[0].Wait != "send/app" {
		t.Errorf("WAIT = %q, want send/app", rows[0].Wait)
	}
}

// TestTrsfZeroDeltaIsNotAbsent: two readings with the same counter mean the
// counter did not move, which is a measurement. Rendering it as "-" throws that
// measurement away and reads as "this end does not report it".
func TestTrsfZeroDeltaIsNotAbsent(t *testing.T) {
	var s TrsfSampler
	same := map[protocol.TrsfCounterKey]uint64{
		protocol.TrsfCounterKey_LossEvents:     7,
		protocol.TrsfCounterKey_LossSpurious:   0,
		protocol.TrsfCounterKey_LoopIterations: 900,
	}
	s.Observe([]protocol.TrsfConnState{trsfState("c1", protocol.ConnRole_Cli, same)}, t0)
	rows := s.Observe([]protocol.TrsfConnState{trsfState("c1", protocol.ConnRole_Cli, same)}, t1)

	for name, got := range map[string]string{
		"LossD": rows[0].LossD, "SpurD": rows[0].SpurD, "LoopD": rows[0].LoopD,
	} {
		if got != "0" {
			t.Errorf("%s = %q, want %q -- a counter that did not move is a measurement", name, got, "0")
		}
	}
}

// TestTrsfAbsentCounterStaysAbsent: an answerer older than a key omits it, and
// a missing key on EITHER side means "this end does not report it".
func TestTrsfAbsentCounterStaysAbsent(t *testing.T) {
	var s TrsfSampler
	with := map[protocol.TrsfCounterKey]uint64{protocol.TrsfCounterKey_LossEvents: 1}
	without := map[protocol.TrsfCounterKey]uint64{}
	s.Observe([]protocol.TrsfConnState{trsfState("c1", protocol.ConnRole_Cli, with)}, t0)
	rows := s.Observe([]protocol.TrsfConnState{trsfState("c1", protocol.ConnRole_Cli, without)}, t1)

	if rows[0].LossD != "-" {
		t.Errorf("LossD = %q, want %q when the second reading omits the key", rows[0].LossD, "-")
	}
}

// TestTrsfWaitLabels walks the label selection. WAIT names what ended the parks
// in the interval, and the send channel is many-to-one so its label carries the
// dominant PUSH reason.
func TestTrsfWaitLabels(t *testing.T) {
	base := map[protocol.TrsfCounterKey]uint64{
		protocol.TrsfCounterKey_Blocks: 0, protocol.TrsfCounterKey_WakeTimer: 0,
		protocol.TrsfCounterKey_WakeSend: 0, protocol.TrsfCounterKey_ArmedPacer: 0,
		protocol.TrsfCounterKey_SendPushApp: 0, protocol.TrsfCounterKey_SendPushAck: 0,
		protocol.TrsfCounterKey_SendPushSelf: 0, protocol.TrsfCounterKey_SendPushCwnd: 0,
		protocol.TrsfCounterKey_SendPushLoss: 0, protocol.TrsfCounterKey_SendPushOther: 0,
	}
	for _, tc := range []struct {
		name string
		next map[protocol.TrsfCounterKey]uint64
		want string
	}{
		{
			// armed_pacer over half the parks: most carried the pacer's deadline.
			name: "timer/pacer",
			next: map[protocol.TrsfCounterKey]uint64{
				protocol.TrsfCounterKey_Blocks: 10, protocol.TrsfCounterKey_WakeTimer: 8,
				protocol.TrsfCounterKey_ArmedPacer: 7,
			},
			want: "timer/pacer",
		},
		{
			// timer-dominant, but the pacer armed almost none of them.
			name: "timer/loss",
			next: map[protocol.TrsfCounterKey]uint64{
				protocol.TrsfCounterKey_Blocks: 10, protocol.TrsfCounterKey_WakeTimer: 8,
				protocol.TrsfCounterKey_ArmedPacer: 1,
			},
			want: "timer/loss",
		},
		{
			name: "send names its dominant push",
			next: map[protocol.TrsfCounterKey]uint64{
				protocol.TrsfCounterKey_Blocks: 10, protocol.TrsfCounterKey_WakeSend: 9,
				protocol.TrsfCounterKey_SendPushCwnd: 6, protocol.TrsfCounterKey_SendPushApp: 2,
			},
			want: "send/cwnd",
		},
		{
			// neither timer nor send: the remainder is an inbound packet.
			name: "peer",
			next: map[protocol.TrsfCounterKey]uint64{protocol.TrsfCounterKey_Blocks: 10},
			want: "peer",
		},
		{
			// No park at all in the interval. A share of nothing has no subject,
			// so this is absent rather than a zero.
			name: "no parks",
			next: map[protocol.TrsfCounterKey]uint64{protocol.TrsfCounterKey_Blocks: 0},
			want: "-",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := map[protocol.TrsfCounterKey]uint64{}
			for k, v := range base {
				next[k] = v
			}
			for k, v := range tc.next {
				next[k] = v
			}
			var s TrsfSampler
			s.Observe([]protocol.TrsfConnState{trsfState("c1", protocol.ConnRole_Cli, base)}, t0)
			rows := s.Observe([]protocol.TrsfConnState{trsfState("c1", protocol.ConnRole_Cli, next)}, t1)
			if rows[0].Wait != tc.want {
				t.Errorf("WAIT = %q, want %q", rows[0].Wait, tc.want)
			}
		})
	}
}

// TestTrsfQueueDelay: srtt-min_rtt is how much of the round trip is a queue
// rather than the path. Not measured is not zero.
func TestTrsfQueueDelay(t *testing.T) {
	for _, tc := range []struct {
		name     string
		counters map[protocol.TrsfCounterKey]uint64
		want     string
	}{
		{
			name:     "no ack has arrived",
			counters: map[protocol.TrsfCounterKey]uint64{protocol.TrsfCounterKey_SrttUs: 18000},
			want:     "-",
		},
		{
			name: "srtt below min",
			counters: map[protocol.TrsfCounterKey]uint64{
				protocol.TrsfCounterKey_SrttUs: 10000, protocol.TrsfCounterKey_MinRttUs: 14000,
			},
			want: "0s",
		},
		{
			name: "queueing delay",
			counters: map[protocol.TrsfCounterKey]uint64{
				protocol.TrsfCounterKey_SrttUs: 18000, protocol.TrsfCounterKey_MinRttUs: 14000,
			},
			want: "4ms",
		},
		{
			// A clock coarser than the path measures a legitimate zero min_rtt;
			// the projection gates on MinRTTValid, so this row HAS the key.
			name: "min_rtt measured as zero",
			counters: map[protocol.TrsfCounterKey]uint64{
				protocol.TrsfCounterKey_SrttUs: 3000, protocol.TrsfCounterKey_MinRttUs: 0,
			},
			want: "3ms",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var s TrsfSampler
			rows := s.Observe([]protocol.TrsfConnState{trsfState("c1", protocol.ConnRole_Cli, tc.counters)}, t0)
			if rows[0].Queue != tc.want {
				t.Errorf("QUEUE = %q, want %q", rows[0].Queue, tc.want)
			}
		})
	}
}

// TestTrsfIdentityColumns: the row carries who the connection is, and a task id
// is shortened the way every other surface shortens one. A zero principal is a
// runner or non-agent conn and prints as absent.
func TestTrsfIdentityColumns(t *testing.T) {
	withTask := trsfState("udp:10.0.0.3:41233-a1", protocol.ConnRole_Agent, nil)
	withTask.PrincipalTask.Id = [16]uint8{0x8b, 0xd0, 0xe4, 0x12, 0xff}
	noTask := trsfState("udp:10.0.0.9:55010-7c", protocol.ConnRole_Runner, nil)

	var s TrsfSampler
	rows := s.Observe([]protocol.TrsfConnState{withTask, noTask}, t0)

	// The wire spelling, not a surface's convention: the CLI table has printed
	// "Agent" since this reading existed, and the TUI lowercases in its own
	// row builder the way it already does for the identity columns.
	if rows[0].CID != "udp:10.0.0.3:41233-a1" || rows[0].Role != "Agent" {
		t.Errorf("cid/role = %q/%q", rows[0].CID, rows[0].Role)
	}
	if rows[0].Task != "8bd0e412" {
		t.Errorf("task = %q, want the 8-hex head", rows[0].Task)
	}
	if rows[1].Task != "-" {
		t.Errorf("task = %q for a zero principal, want %q", rows[1].Task, "-")
	}
}

// TestTrsfJSONCarriesEveryCounter: nothing enumerates the counters, so one
// added to the transport reaches --json with no edit here. Deltas appear only
// when there is a previous reading, so a consumer can tell "no change" from
// "nothing to compare against".
func TestTrsfJSONCarriesEveryCounter(t *testing.T) {
	counters := map[protocol.TrsfCounterKey]uint64{
		protocol.TrsfCounterKey_Cwnd:       131072,
		protocol.TrsfCounterKey_LossEvents: 3,
	}
	var s TrsfSampler
	first := s.ObserveJSON([]protocol.TrsfConnState{trsfState("c1", protocol.ConnRole_Cli, counters)}, t0)
	if _, ok := first[0]["loss_events_delta"]; ok {
		t.Errorf("first reading carries a delta; there is nothing to compare against")
	}
	if first[0]["cwnd"] != uint64(131072) {
		t.Errorf("cwnd = %v, want the raw counter value", first[0]["cwnd"])
	}

	counters[protocol.TrsfCounterKey_LossEvents] = 5
	second := s.ObserveJSON([]protocol.TrsfConnState{trsfState("c1", protocol.ConnRole_Cli, counters)}, t1)
	if second[0]["loss_events_delta"] != uint64(2) {
		t.Errorf("loss_events_delta = %v, want 2", second[0]["loss_events_delta"])
	}
}

// TestTrsfSamplerTracksPerCID: two connections advance independently, and a CID
// that appears for the first time on the second reading has no previous of its
// own even though the sampler has one.
func TestTrsfSamplerTracksPerCID(t *testing.T) {
	c := func(loss uint64) map[protocol.TrsfCounterKey]uint64 {
		return map[protocol.TrsfCounterKey]uint64{protocol.TrsfCounterKey_LossEvents: loss}
	}
	var s TrsfSampler
	s.Observe([]protocol.TrsfConnState{trsfState("c1", protocol.ConnRole_Cli, c(1))}, t0)
	rows := s.Observe([]protocol.TrsfConnState{
		trsfState("c1", protocol.ConnRole_Cli, c(4)),
		trsfState("c2", protocol.ConnRole_Cli, c(9)),
	}, t1)

	if rows[0].LossD != "3" {
		t.Errorf("c1 LossD = %q, want 3", rows[0].LossD)
	}
	if rows[1].LossD != "-" {
		t.Errorf("c2 LossD = %q, want %q -- first sighting of this cid", rows[1].LossD, "-")
	}
}

// TestTrsfRowsAreOrderedByCID: the answerer ranges a Go map, so it returns the
// same connections in a different order every reading. On a table with a
// cursor that is not cosmetic — the row under the selection changes once a
// second, and a key aimed at one lands on another. Found by pressing enter on
// the runner row in the TUI and retargeting nothing.
func TestTrsfRowsAreOrderedByCID(t *testing.T) {
	c := func(cid string) protocol.TrsfConnState {
		return trsfState(cid, protocol.ConnRole_Cli, nil)
	}
	var s TrsfSampler
	first := s.Observe([]protocol.TrsfConnState{c("c"), c("a"), c("b")}, t0)
	second := s.Observe([]protocol.TrsfConnState{c("b"), c("c"), c("a")}, t1)

	want := []string{"a", "b", "c"}
	for i, w := range want {
		if first[i].CID != w {
			t.Errorf("first reading row %d = %q, want %q", i, first[i].CID, w)
		}
		if second[i].CID != w {
			t.Errorf("second reading row %d = %q, want %q -- the answerer's order must not reach the row order", i, second[i].CID, w)
		}
	}

	// The JSON form too: a --watch consumer diffing successive readings should
	// not see its lines permute either.
	var js TrsfSampler
	lines := js.ObserveJSON([]protocol.TrsfConnState{c("c"), c("a"), c("b")}, t0)
	for i, w := range want {
		if lines[i]["cid"] != w {
			t.Errorf("json line %d cid = %v, want %q", i, lines[i]["cid"], w)
		}
	}
}
