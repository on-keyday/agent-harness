package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The derivation these rows come from moved to cli.TrsfSampler so the TUI and
// the WebUI could share it. These are the golden bytes the CLI printed BEFORE
// that move — captured from the old code, not written from the new one — so the
// move is pinned as a refactor rather than described as one.

func trsfConn(cid string, role protocol.ConnRole, kv map[protocol.TrsfCounterKey]uint64) protocol.TrsfConnState {
	var s protocol.TrsfConnState
	s.SetCid([]uint8(cid))
	s.Role = role
	c := make([]protocol.TrsfCounter, 0, len(kv))
	for k, v := range kv {
		c = append(c, protocol.TrsfCounter{Key: k, Value: v})
	}
	s.SetCounters(c)
	return s
}

// runnerConn is the busy connection in the fixtures: loss is a counter that
// moves between the two readings, and the parks are all send-channel wakes
// pushed by the application.
func runnerConn(loss, loop, blockedNs, blocks, wakeSend, pushApp uint64) protocol.TrsfConnState {
	s := trsfConn("udp:10.0.0.3:41233-a1", protocol.ConnRole_Runner, map[protocol.TrsfCounterKey]uint64{
		protocol.TrsfCounterKey_Cwnd: 131072, protocol.TrsfCounterKey_BytesInFlight: 64240,
		protocol.TrsfCounterKey_SrttUs: 18000, protocol.TrsfCounterKey_MinRttUs: 14000,
		protocol.TrsfCounterKey_LossEvents: loss, protocol.TrsfCounterKey_LossSpurious: 1,
		protocol.TrsfCounterKey_LoopIterations: loop, protocol.TrsfCounterKey_BlockedNs: blockedNs,
		protocol.TrsfCounterKey_Blocks: blocks, protocol.TrsfCounterKey_WakeTimer: 0,
		protocol.TrsfCounterKey_WakeSend: wakeSend, protocol.TrsfCounterKey_ArmedPacer: 0,
		protocol.TrsfCounterKey_SendPushApp: pushApp,
	})
	s.PrincipalTask.Id = [16]uint8{0x8b, 0xd0, 0xe4, 0x12}
	return s
}

// idleConn reports no min_rtt (no ACK has arrived) and no principal task.
func idleConn() protocol.TrsfConnState {
	return trsfConn("udp:10.0.0.9:55010-7c", protocol.ConnRole_Cli, map[protocol.TrsfCounterKey]uint64{
		protocol.TrsfCounterKey_Cwnd: 14600, protocol.TrsfCounterKey_SrttUs: 2000,
	})
}

const (
	trsfHeaderLine = "CID                                ROLE    TASK          CWND  INFLIGHT      SRTT    QUEUE    LOSS+   SPUR+   LOOP+  BLOCK% WAIT       \n"
	trsfIdleLine   = "udp:10.0.0.9:55010-7c              Cli     -            14600         0       2ms        -        -       -       -       - -          \n"
)

// TestWriteTrsfTableOneShot: with nothing to compare against, every rate column
// is "-" and the value columns still render.
func TestWriteTrsfTableOneShot(t *testing.T) {
	var s cli.TrsfSampler
	var buf bytes.Buffer
	if err := writeTrsfTable(&buf, s.Observe([]protocol.TrsfConnState{
		runnerConn(3, 400, 0, 0, 0, 0), idleConn(),
	}, 0)); err != nil {
		t.Fatal(err)
	}
	want := trsfHeaderLine +
		"udp:10.0.0.3:41233-a1              Runner  8bd0e412    131072     64240      18ms      4ms        -       -       -       - -          \n" +
		trsfIdleLine + "\n"
	if buf.String() != want {
		t.Errorf("one-shot table changed by the move:\n--- got ---\n%s\n--- want ---\n%s", buf.String(), want)
	}
}

// TestWriteTrsfTableWatch: the second reading fills the delta columns. BLOCK%
// is 870ms of parking over the answerer's own 1s interval.
func TestWriteTrsfTableWatch(t *testing.T) {
	const t0 = int64(1_000_000_000)
	var s cli.TrsfSampler
	s.Observe([]protocol.TrsfConnState{runnerConn(3, 400, 0, 0, 0, 0), idleConn()}, t0)

	var buf bytes.Buffer
	if err := writeTrsfTable(&buf, s.Observe([]protocol.TrsfConnState{
		runnerConn(5, 812, uint64(870*time.Millisecond), 10, 10, 9), idleConn(),
	}, t0+int64(time.Second))); err != nil {
		t.Fatal(err)
	}
	want := trsfHeaderLine +
		"udp:10.0.0.3:41233-a1              Runner  8bd0e412    131072     64240      18ms      4ms        2       0     412     87% send/app   \n" +
		trsfIdleLine + "\n"
	if buf.String() != want {
		t.Errorf("watch table changed by the move:\n--- got ---\n%s\n--- want ---\n%s", buf.String(), want)
	}
}

// TestWriteTrsfTableEmpty: no connections is a sentence, not a bare header. A
// confined caller sees no runner connections at all, so this is a normal
// answer rather than a failure.
func TestWriteTrsfTableEmpty(t *testing.T) {
	var s cli.TrsfSampler
	var buf bytes.Buffer
	if err := writeTrsfTable(&buf, s.Observe(nil, 0)); err != nil {
		t.Fatal(err)
	}
	want := trsfHeaderLine + "(no connections visible to you)\n\n"
	if buf.String() != want {
		t.Errorf("empty table:\n--- got ---\n%s\n--- want ---\n%s", buf.String(), want)
	}
}

// TestWriteTrsfJSON: one object per connection, keys sorted by encoding/json,
// and no delta key on a first reading.
func TestWriteTrsfJSON(t *testing.T) {
	var s cli.TrsfSampler
	var buf bytes.Buffer
	if err := writeTrsfJSON(&buf, s.ObserveJSON([]protocol.TrsfConnState{idleConn()}, 0)); err != nil {
		t.Fatal(err)
	}
	want := `{"cid":"udp:10.0.0.9:55010-7c","cwnd":14600,"role":"Cli","srtt_us":2000}` + "\n"
	if buf.String() != want {
		t.Errorf("json line changed by the move:\n got: %s\nwant: %s", buf.String(), want)
	}
}
