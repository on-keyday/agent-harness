package server

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestDatagramTapRecordsFate(t *testing.T) {
	pf := newCounterForward()
	tap, recs := newTestTap(t, pf, protocol.ForwardTapFilter_Both, 0)
	defer pf.removeTap(tap)

	payload := bytes.Repeat([]byte{0xAB}, 512)
	pf.observeDatagram(3, protocol.ForwardTapDirection_ToTarget, protocol.ForwardTapDatagramFate_Relayed, payload)
	pf.observeDatagram(3, protocol.ForwardTapDirection_ToTarget, protocol.ForwardTapDatagramFate_DroppedOversize, []byte("x"))
	got := drain(t, recs, 2)

	d := got[0].Datagram()
	if got[0].Kind != protocol.ForwardTapRecordKind_Datagram || d == nil {
		t.Fatalf("record 0 kind %v", got[0].Kind)
	}
	if d.FlowId != 3 || d.Direction != protocol.ForwardTapDirection_ToTarget || d.Seq != 1 ||
		d.Fate != protocol.ForwardTapDatagramFate_Relayed || !bytes.Equal(d.Data, payload) {
		t.Fatalf("record 0 = %+v", d)
	}
	payload[0] = 0 // the tap must hold its own copy
	if d.Data[0] != 0xAB {
		t.Fatal("the tap kept the caller's buffer")
	}
	if d2 := got[1].Datagram(); d2.Seq != 2 || d2.Fate != protocol.ForwardTapDatagramFate_DroppedOversize {
		t.Fatalf("record 1 = %+v", d2)
	}
}

func TestDatagramTapLateOpenSeesRealSeq(t *testing.T) {
	pf := newCounterForward()
	for i := 0; i < 5; i++ {
		pf.observeDatagram(3, protocol.ForwardTapDirection_ToTarget, protocol.ForwardTapDatagramFate_Relayed, []byte("p"))
	}
	tap, recs := newTestTap(t, pf, protocol.ForwardTapFilter_Both, 0)
	defer pf.removeTap(tap)
	pf.observeDatagram(3, protocol.ForwardTapDirection_ToTarget, protocol.ForwardTapDatagramFate_Relayed, []byte("p"))
	pf.observeDatagram(3, protocol.ForwardTapDirection_FromTarget, protocol.ForwardTapDatagramFate_Relayed, []byte("q"))
	got := drain(t, recs, 2)
	if s := got[0].Datagram().Seq; s != 6 {
		t.Fatalf("first seq a late tap sees = %d, want 6", s)
	}
	if s := got[1].Datagram().Seq; s != 1 {
		t.Fatalf("the other direction's first seq = %d, want 1", s)
	}
}

func TestDatagramTapGapIsDatagramGap(t *testing.T) {
	pf := newCounterForward()
	sink := &chanSink{out: make(chan *protocol.ForwardTapRecord, 1)}
	tap := newForwardTap(sink, protocol.ForwardTapFilter_Both, 0)
	pf.addTap(tap)
	defer pf.removeTap(tap)
	// No run(): nothing drains, so the queue overflows.
	for i := 0; i < forwardTapQueueDepth+10; i++ {
		pf.observeDatagram(3, protocol.ForwardTapDirection_ToTarget, protocol.ForwardTapDatagramFate_Relayed, []byte("0123456789"))
	}
	if tap.missedBytes() == 0 {
		t.Fatal("overflow was not counted")
	}
	sink.out = make(chan *protocol.ForwardTapRecord, 1024)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tap.run(ctx)
	// Resent until it gets through: the queue is still full the moment run
	// starts, and an "after" pushed then is missed like the rest.
	resend := time.NewTicker(10 * time.Millisecond)
	defer resend.Stop()

	var sawGap bool
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-resend.C:
			pf.observeDatagram(3, protocol.ForwardTapDirection_ToTarget, protocol.ForwardTapDatagramFate_Relayed, []byte("after"))
		case rec := <-sink.out:
			switch rec.Kind {
			case protocol.ForwardTapRecordKind_Gap:
				t.Fatal("a datagram flow's missed bytes came out as a stream gap")
			case protocol.ForwardTapRecordKind_DatagramGap:
				g := rec.DatagramGap()
				if g.FlowId != 3 || g.Direction != protocol.ForwardTapDirection_ToTarget || g.DroppedBytes == 0 {
					t.Fatalf("datagram_gap = %+v", g)
				}
				sawGap = true
			case protocol.ForwardTapRecordKind_Datagram:
				if string(rec.Datagram().Data) == "after" {
					if !sawGap {
						t.Fatal("no datagram_gap before the next datagram")
					}
					return
				}
			}
		case <-deadline:
			t.Fatal("never saw the datagram after the gap")
		}
	}
}

func TestDatagramTapFilterAndTruncation(t *testing.T) {
	pf := newCounterForward()
	tap, recs := newTestTap(t, pf, protocol.ForwardTapFilter_FromTarget, 16)
	defer pf.removeTap(tap)
	pf.observeDatagram(1, protocol.ForwardTapDirection_ToTarget, protocol.ForwardTapDatagramFate_Relayed, []byte("dropped by filter"))
	pf.observeDatagram(1, protocol.ForwardTapDirection_FromTarget, protocol.ForwardTapDatagramFate_Relayed, bytes.Repeat([]byte("z"), 100))
	d := drain(t, recs, 1)[0].Datagram()
	if d.Direction != protocol.ForwardTapDirection_FromTarget || len(d.Data) != 16 || d.TruncatedBytes != 84 {
		t.Fatalf("datagram = dir %v, %d bytes, %d cut", d.Direction, len(d.Data), d.TruncatedBytes)
	}
}

func TestDatagramSeqPrunedAfterIdle(t *testing.T) {
	pf := newCounterForward()
	t0 := time.Unix(1000, 0)
	pf.observeDatagramAt(t0, 3, protocol.ForwardTapDirection_ToTarget, protocol.ForwardTapDatagramFate_Relayed, []byte("a"))
	pf.pruneDatagramSeqs(t0.Add(59 * time.Second))
	tap, recs := newTestTap(t, pf, protocol.ForwardTapFilter_Both, 0)
	defer pf.removeTap(tap)
	pf.observeDatagramAt(t0.Add(59*time.Second), 3, protocol.ForwardTapDirection_ToTarget, protocol.ForwardTapDatagramFate_Relayed, []byte("b"))
	if s := drain(t, recs, 1)[0].Datagram().Seq; s != 2 {
		t.Fatalf("seq after 59s = %d, want 2 (still the same flow)", s)
	}
	pf.pruneDatagramSeqs(t0.Add(59*time.Second + datagramSeqIdle))
	pf.observeDatagramAt(t0.Add(200*time.Second), 3, protocol.ForwardTapDirection_ToTarget, protocol.ForwardTapDatagramFate_Relayed, []byte("c"))
	if s := drain(t, recs, 1)[0].Datagram().Seq; s != 1 {
		t.Fatalf("seq after an idle prune = %d, want 1", s)
	}
}

func TestPortForwardInfoIPRow(t *testing.T) {
	cfg := testLinkConfig(t, "10.200.0.1/30", "10.200.0.2")
	ip := portForwardInfo(&portForward{direction: protocol.PortForwardDirection_Link,
		protocolKind: protocol.ForwardProtocol_Ip, link: &cfg})
	if _, ok := ip.Counter(protocol.ForwardCounterKey_MaxDatagramSize); !ok {
		t.Fatal("an ip row has no max_datagram_size")
	}
	for _, k := range protocol.ForwardDropKeys(protocol.ForwardHopRelay) {
		if v, ok := ip.Counter(k); !ok || v != 0 {
			t.Fatalf("relay drop key %v = %d, %v; want present and 0", k, v, ok)
		}
	}
	if ip.Link() == nil {
		t.Fatal("an ip row carries no link")
	}
	tcp := portForwardInfo(&portForward{direction: protocol.PortForwardDirection_Local})
	if _, ok := tcp.Counter(protocol.ForwardCounterKey_MaxDatagramSize); ok {
		t.Fatal("a tcp row carries the datagram group")
	}
	if tcp.Link() != nil {
		t.Fatal("a tcp row carries a link")
	}
}

func TestRelayForwardDatagramIPForward(t *testing.T) {
	h, taskHex, runnerConn := stateFixture(t)
	client := &fakeConn{id: tcid("client-1")}
	cfg := testLinkConfig(t, "10.200.0.1/30", "10.200.0.2")
	pf := &portForward{direction: protocol.PortForwardDirection_Link, protocolKind: protocol.ForwardProtocol_Ip,
		taskIDHex: taskHex, runnerID: testRunnerID("runner-1"), runnerCID: runnerConn.ConnectionID().String(),
		clientCxn: client, clientCID: client.ConnectionID().String(), link: &cfg}
	id := h.pforwards().add(pf)
	tap, recs := newTestTap(t, pf, protocol.ForwardTapFilter_Both, 0)
	defer pf.removeTap(tap)
	s := &Server{taskHandler: h, registry: h.Registry, cfg: Config{Logger: slog.Default()}}

	dg := protocol.ForwardDatagram{ForwardId: id, FlowId: 0, Payload: []byte("an ip packet")}
	raw := dg.MustAppend([]byte{byte(appwire.AppKind_ForwardDatagram)})
	s.relayForwardDatagram(streamingConn{}, client.ConnectionID().String(), &dg, raw)

	sent := runnerConn.Datagrams()
	if len(sent) != 1 || !bytes.Equal(sent[0], raw) {
		t.Fatalf("runner received %d datagrams, want the one, byte for byte", len(sent))
	}
	d := drain(t, recs, 1)[0].Datagram()
	if d.Fate != protocol.ForwardTapDatagramFate_Relayed || d.Direction != protocol.ForwardTapDirection_ToTarget {
		t.Fatalf("tap saw %+v", d)
	}

	// A third connection is refused, and the tap is not offered what never
	// belonged to this forward.
	s.relayForwardDatagram(streamingConn{}, tcid("intruder").String(), &dg, raw)
	if n := len(runnerConn.Datagrams()) + len(client.Datagrams()); n != 1 {
		t.Fatalf("an intruder's datagram was relayed (%d sent in total)", n)
	}
	select {
	case rec := <-recs:
		t.Fatalf("the tap was offered an intruder's datagram: %+v", rec)
	case <-time.After(50 * time.Millisecond):
	}
}
