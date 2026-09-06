package protocol

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/objtrsf/trsf"
)

// Every numeric field the transport reports must reach the wire record. The
// check is "no zero survives" against an InternalState whose every field is
// distinct and non-zero: a field added to InternalState and forgotten here
// leaves a zero, which on the operator's screen is indistinguishable from a
// connection that really is idle.
func TestTrsfRowFromCarriesEveryCounter(t *testing.T) {
	st := &trsf.InternalState{
		ActiveSendStreams:    1,
		ActiveReceiveStreams: 2,
		CurrentMTU:           1400,
		SendQueueLength:      3,
		ReceiveQueueLength:   4,
		BytesInFlight:        5,
		CongestionWindow:     6,
		SmoothedRTT:          7 * time.Millisecond,
		RTTVariance:          8 * time.Millisecond,
		LoopIterations:       9,
		Loss:                 trsf.LossStats{Events: 10, Packets: 11, Spurious: 12},
		BlockedNs:            13,
		Blocks:               14,
		WakeTimer:            15,
		WakeSend:             16,
		ArmedPacer:           17,
		SendPushApp:          18,
		SendPushACK:          19,
		SendPushSelf:         20,
		SendPushCwnd:         21,
		SendPushLoss:         22,
		SendPushOther:        23,
	}
	row := TrsfRowFrom(st)
	for name, got := range map[string]uint64{
		"Mtu": uint64(row.Mtu), "Cwnd": uint64(row.Cwnd),
		"BytesInFlight": uint64(row.BytesInFlight),
		"SrttUs":        row.SrttUs, "RttvarUs": row.RttvarUs,
		"SendQueue": uint64(row.SendQueue), "RecvQueue": uint64(row.RecvQueue),
		"SendStreams": uint64(row.SendStreams), "RecvStreams": uint64(row.RecvStreams),
		"LoopIterations": row.LoopIterations,
		"LossEvents":     row.LossEvents, "LossPackets": row.LossPackets,
		"LossSpurious": row.LossSpurious,
		"BlockedNs":    row.BlockedNs, "Blocks": row.Blocks,
		"WakeTimer": row.WakeTimer, "WakeSend": row.WakeSend,
		"ArmedPacer":  row.ArmedPacer,
		"SendPushApp": row.SendPushApp, "SendPushAck": row.SendPushAck,
		"SendPushSelf": row.SendPushSelf, "SendPushCwnd": row.SendPushCwnd,
		"SendPushLoss": row.SendPushLoss, "SendPushOther": row.SendPushOther,
	} {
		if got == 0 {
			t.Errorf("%s is zero: TrsfRowFrom does not carry it", name)
		}
	}
}

// The projection stays in ONE place. Before this, `server` and `runner` each
// built the row by hand from the same InternalState — so a field could land on
// one answerer and not the other, and the reader would see zeros from a runner
// with no way to tell that from an idle loop.
//
// A grep rather than a type check because that is the shape of the mistake:
// nothing stops a second literal compiling.
func TestNoHandWrittenTrsfRowsRemain(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	for _, pkg := range []string{"server", "runner", "cli", "tui"} {
		dir := filepath.Join(root, pkg)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatalf("read %s: %v", e.Name(), err)
			}
			for i, line := range strings.Split(string(b), "\n") {
				// A map or slice TYPE naming the struct is fine; a composite
				// literal that BUILDS one is the duplicate this guards.
				if !strings.Contains(line, "TrsfConnState{") {
					continue
				}
				if strings.Contains(line, "map[") || strings.Contains(line, "[]protocol.TrsfConnState{") {
					continue
				}
				t.Errorf("%s/%s:%d builds a TrsfConnState by hand: %s\n"+
					"use protocol.TrsfRowFrom so a new counter reaches every answerer",
					pkg, e.Name(), i+1, strings.TrimSpace(line))
			}
		}
	}
}
