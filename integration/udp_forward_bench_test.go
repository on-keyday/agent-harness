//go:build integration

package integration

// Throughput and loss for the udp forward's splice route, next to the two
// numbers that say what those figures mean: a raw loopback socket (the ceiling
// the same payload reaches with no harness in the path) and a tcp forward
// through the SAME three-party machinery (what this splice does with a stream).
//
// Benchmarks rather than tests so `make test-integration` does not run them —
// it passes no -bench. Run them with:
//
//	go test -tags integration ./integration -run '^$' -bench UDPForward -benchtime 1x -v
//
// The reported metrics are per case:
//
//	offered_MB/s   what the sender's socket accepted
//	goodput_MB/s   what arrived at the target
//	loss_%         1 - delivered/offered, counted in DATAGRAMS
//	srv_drop       the server row's oversize+congestion+queue total
//
// loss_% and srv_drop are reported side by side on purpose. They are not the
// same quantity and the gap between them is itself a result: the row's counters
// are incremented in server/forward_datagram.go only, so a datagram refused by
// the CLIENT's own SendDatagram never reaches the server and cannot appear
// there.

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/server"
	"github.com/on-keyday/objtrsf/objproto"
)

// benchPayload is a datagram body that fits the initial MTU with the 13 bytes
// of ForwardDatagram framing on top. The design doc puts the usable payload at
// ~1157 B at DefaultInitialMTU, so 1100 crosses without waiting for PLPMTUD and
// the measurement is not a measurement of how fast the MTU grows.
const benchPayload = 1100

// benchDuration is how long each offered-rate case runs. Long enough that the
// slow-start ramp is not the whole sample, short enough that a five-case ladder
// does not turn into a thermal experiment on this box.
const benchDuration = 5 * time.Second

// forwardBenchFixture is one server + one runner + one task + one client,
// stood up once and shared by every case. Setup costs seconds and measures
// nothing, so paying it per case would make the ladder mostly setup.
type forwardBenchFixture struct {
	ctx      context.Context
	cancel   context.CancelFunc
	client   *cli.Client
	taskID   string
	srvDone  chan error
	runDone  chan error
	stopOnce func()
}

func newForwardBenchFixture(b *testing.B, addr string) *forwardBenchFixture {
	return newForwardBenchFixtureOn(b, "ws", addr)
}

// newForwardBenchFixtureOn picks the transport the three parties speak.
//
// The scheme is a parameter because it decides the datagram SIZE, and so what
// any of these benchmarks can observe. On ws the carrier is a stream and
// MaxDatagramSize is StreamMTU (16 KB), fixed for the connection's life — a
// run there can never see PLPMTUD move and can never produce an oversize drop.
// Only udp has a path MTU to discover.
func newForwardBenchFixtureOn(b *testing.B, scheme, addr string) *forwardBenchFixture {
	b.Helper()
	clearAgentEnvB(b)

	repo := initRepoB(b)
	fakeClaude, err := filepath.Abs("../testdata/fake-claude-slow.sh")
	if err != nil {
		b.Fatal(err)
	}
	peerCID, err := objproto.ParseConnectionID(scheme+":"+addr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		b.Fatalf("parse server cid: %v", err)
	}

	cfg := server.Config{DataDir: b.TempDir()}
	if scheme == "udp" {
		cfg.UDPAddr = addr
	} else {
		cfg.Addr = addr
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := server.New(cfg)
	srvDone := make(chan error, 1)
	go func() { srvDone <- s.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)

	runDone := make(chan error, 1)
	go func() {
		runDone <- runner.Run(ctx, runner.Config{
			RunnerID:         runner.NewRunnerID(),
			ServerCandidates: runner.CandidatesOf(peerCID),
			AllowedRoots:     []string{repo},
			Profiles:         singleAgentProfile(fakeClaude),
		})
	}()
	time.Sleep(500 * time.Millisecond)

	taskID, err := cli.Submit(ctx, peerCID, repo, "udp-forward-bench")
	if err != nil {
		cancel()
		b.Fatalf("submit: %v", err)
	}
	worktree := filepath.Join(repo, ".harness-worktrees", taskID)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, serr := os.Stat(worktree); serr == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, serr := os.Stat(worktree); serr != nil {
		cancel()
		b.Fatalf("worktree did not appear: %v", serr)
	}

	c, err := cli.Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		cancel()
		b.Fatalf("dial: %v", err)
	}

	return &forwardBenchFixture{
		ctx: ctx, cancel: cancel, client: c, taskID: taskID,
		srvDone: srvDone, runDone: runDone,
		stopOnce: func() {
			c.Close()
			cancel()
			<-srvDone
			<-runDone
		},
	}
}

// udpSink counts what arrives. It is the TARGET of the forward, so its totals
// are goodput by definition — everything the tunnel actually delivered.
type udpSink struct {
	conn  *net.UDPConn
	port  int
	pkts  atomic.Int64
	bytes atomic.Int64
}

func newUDPSink(tb testing.TB) *udpSink {
	tb.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		tb.Fatalf("sink listen: %v", err)
	}
	// 8 MB, so the sink's own socket buffer is not what the experiment
	// measures. The trap this avoids is the one recorded for the transport's
	// own receive path: a socket-buffer overflow is invisible to tc and reads
	// as loss somewhere else.
	_ = conn.SetReadBuffer(8 << 20)
	s := &udpSink{conn: conn, port: conn.LocalAddr().(*net.UDPAddr).Port}
	go func() {
		buf := make([]byte, 64*1024)
		for {
			n, rerr := conn.Read(buf)
			if rerr != nil {
				return
			}
			s.pkts.Add(1)
			s.bytes.Add(int64(n))
		}
	}()
	return s
}

func (s *udpSink) close() { _ = s.conn.Close() }

// blastUDP offers datagrams at rateBytes/s (0 = as fast as the socket takes
// them) for d, and returns how many the sender's socket accepted.
//
// Paced in 2 ms slices rather than one sleep per datagram: a per-datagram sleep
// on this kernel cannot resolve the interval at these rates, so the pacing
// would silently become "as fast as possible" and every rung would read the
// same.
func blastUDP(tb testing.TB, target *net.UDPAddr, rateBytes int, d time.Duration) (pkts, bytes int64) {
	p := blastUDPSize(tb, target, benchPayload, rateBytes, d)
	return p, p * benchPayload
}

// blastUDPSize is blastUDP with the datagram size as a parameter.
func blastUDPSize(tb testing.TB, target *net.UDPAddr, size, rateBytes int, d time.Duration) (pkts int64) {
	tb.Helper()
	conn, err := net.DialUDP("udp", nil, target)
	if err != nil {
		tb.Fatalf("blast dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetWriteBuffer(8 << 20)

	payload := make([]byte, size)
	const slice = 2 * time.Millisecond
	perSlice := 1 << 30
	if rateBytes > 0 {
		perSlice = int(float64(rateBytes) * slice.Seconds() / float64(size))
		if perSlice < 1 {
			perSlice = 1
		}
	}

	end := time.Now().Add(d)
	next := time.Now()
	for time.Now().Before(end) {
		for i := 0; i < perSlice; i++ {
			if _, werr := conn.Write(payload); werr != nil {
				// ENOBUFS on an unpaced blast is the local socket refusing, not
				// the tunnel losing. Counting it as offered would charge the
				// tunnel for a datagram that never left this process.
				break
			}
			pkts++
			if rateBytes == 0 && time.Now().After(end) {
				break
			}
		}
		if rateBytes > 0 {
			next = next.Add(slice)
			if sleep := time.Until(next); sleep > 0 {
				time.Sleep(sleep)
			}
		}
	}
	return pkts
}

// serverDatagramCounters sums the SERVER's own trsf datagram counters across
// its connections, which is what attributes a missing datagram to a leg.
//
// `datagrams_received` here is the number that actually crossed the
// client→server leg, so offered − received is the loss on the client's own
// leg — the one hop no operator-visible counter covers, because
// `harness-cli trsf` can target the server or a runner and the client running
// the forward is neither.
func serverDatagramCounters(b *testing.B, fx *forwardBenchFixture) map[protocol.TrsfCounterKey]uint64 {
	b.Helper()
	out := map[protocol.TrsfCounterKey]uint64{}
	conns, _, err := fx.client.TrsfStateOn(fx.ctx, cli.TrsfPeer{Target: protocol.TrsfTarget_Server})
	if err != nil {
		b.Logf("trsf state: %v", err)
		return out
	}
	for i := range conns {
		for _, k := range []protocol.TrsfCounterKey{
			protocol.TrsfCounterKey_DatagramsReceived,
			protocol.TrsfCounterKey_DatagramsSent,
			protocol.TrsfCounterKey_DatagramsLost,
			protocol.TrsfCounterKey_DatagramsDroppedSendQueue,
			protocol.TrsfCounterKey_DatagramsDroppedRecvQueue,
			protocol.TrsfCounterKey_DatagramsDroppedCongestion,
			protocol.TrsfCounterKey_DatagramsDroppedOversize,
			protocol.TrsfCounterKey_UnroutedTransportKind,
		} {
			out[k] += conns[i].CounterOr(k, 0)
		}
	}
	return out
}

func datagramCounterDelta(before, after map[protocol.TrsfCounterKey]uint64) string {
	s := ""
	for _, k := range []protocol.TrsfCounterKey{
		protocol.TrsfCounterKey_DatagramsReceived,
		protocol.TrsfCounterKey_DatagramsSent,
		protocol.TrsfCounterKey_DatagramsLost,
		protocol.TrsfCounterKey_DatagramsDroppedSendQueue,
		protocol.TrsfCounterKey_DatagramsDroppedRecvQueue,
		protocol.TrsfCounterKey_DatagramsDroppedCongestion,
		protocol.TrsfCounterKey_DatagramsDroppedOversize,
		protocol.TrsfCounterKey_UnroutedTransportKind,
	} {
		s += fmt.Sprintf("%s=%d ", k, after[k]-before[k])
	}
	return s
}

// readForwardMTU is the row's max_datagram_size. The server stores it on EVERY
// relayed send from the FAR leg's MaxDatagramSize, so on an asymmetric pair —
// the ws client and udp runner the relay's own comment names — this reports the
// runner leg and says nothing about the client's.
func readForwardMTU(b *testing.B, fx *forwardBenchFixture) uint16 {
	b.Helper()
	rows, err := fx.client.PortForwardListWith(fx.ctx, cli.ForwardListQuery{Task: fx.taskID})
	if err != nil {
		return 0
	}
	for i := range rows {
		if mtu, ok := rows[i].Counter(protocol.ForwardCounterKey_MaxDatagramSize); ok {
			return uint16(mtu)
		}
	}
	return 0
}

func freeUDPPortB(b *testing.B) string {
	b.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		b.Fatalf("freeUDPPort: %v", err)
	}
	addr := conn.LocalAddr().String()
	conn.Close()
	return addr
}

// forwardTrafficLine is the row exactly as `forward ls` renders it, so the
// mtu= the tunnel is actually running at is in the log beside the numbers it
// explains rather than being inferred from the design doc.
func forwardTrafficLine(b *testing.B, fx *forwardBenchFixture) string {
	b.Helper()
	rows, err := fx.client.PortForwardListWith(fx.ctx, cli.ForwardListQuery{Task: fx.taskID})
	if err != nil {
		return "forward ls: " + err.Error()
	}
	for i := range rows {
		if rows[i].Protocol == protocol.ForwardProtocol_Udp {
			return cli.PortForwardTrafficLine(&rows[i])
		}
	}
	return "(no udp row)"
}

// BenchmarkUDPForwardSplice walks an offered-rate ladder through one long-lived
// udp -L registration on the splice route.
//
// One registration for the whole ladder, deliberately: a forward is long-lived,
// and the design doc's §6a argument turns on a warm connection. Re-registering
// per rung would price a cold window instead.
func BenchmarkUDPForwardSplice(b *testing.B) {
	fx := newForwardBenchFixture(b, "127.0.0.1:18571")
	defer fx.stopOnce()

	sink := newUDPSink(b)
	defer sink.close()

	probe, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		b.Fatalf("probe listen: %v", err)
	}
	localPort := probe.LocalAddr().(*net.UDPAddr).Port
	probe.Close()

	spec, err := cli.ParseForwardSpec("127.0.0.1:" + strconv.Itoa(localPort) +
		":127.0.0.1:" + strconv.Itoa(sink.port) + "/udp")
	if err != nil {
		b.Fatalf("parse spec: %v", err)
	}
	fwdCtx, fwdCancel := context.WithCancel(fx.ctx)
	defer fwdCancel()
	fwdDone := make(chan error, 1)
	go func() {
		fwdDone <- cli.RunForward(fwdCtx, fx.client, fx.taskID, []cli.ForwardSpec{spec}, nil, nil)
	}()

	target := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: localPort}
	waitForwardUp(b, target, sink)

	for _, rate := range []struct {
		name string
		rate int
	}{
		{"offered_1MBps", 1 << 20},
		{"offered_2MBps", 2 << 20},
		{"offered_4MBps", 4 << 20},
		{"offered_8MBps", 8 << 20},
		{"unpaced", 0},
	} {
		b.Run(rate.name, func(b *testing.B) {
			for b.Loop() {
				before := readForwardDrops(b, fx)
				tBefore := serverDatagramCounters(b, fx)
				p0, y0 := sink.pkts.Load(), sink.bytes.Load()

				start := time.Now()
				offP, offB := blastUDP(b, target, rate.rate, benchDuration)
				elapsed := time.Since(start)

				// Let what is in flight land before reading the totals. Without
				// it the tail of every run is counted as loss.
				time.Sleep(1500 * time.Millisecond)
				gotP, gotB := sink.pkts.Load()-p0, sink.bytes.Load()-y0
				after := readForwardDrops(b, fx)
				tAfter := serverDatagramCounters(b, fx)

				b.ReportMetric(float64(offB)/elapsed.Seconds()/(1<<20), "offered_MB/s")
				b.ReportMetric(float64(gotB)/elapsed.Seconds()/(1<<20), "goodput_MB/s")
				lost := offP - gotP
				if lost < 0 {
					lost = 0
				}
				b.ReportMetric(100*float64(lost)/float64(max64(offP, 1)), "loss_%")
				b.ReportMetric(float64(after.total()-before.total()), "srv_drop")
				b.ReportMetric(float64(offP)/elapsed.Seconds(), "offered_pps")
				b.Logf("offered %d dg / delivered %d dg / forward row: %s / server trsf: %s",
					offP, gotP, after.sub(before), datagramCounterDelta(tBefore, tAfter))
				// The CLIENT's own connection, which is the one refusing. Read
				// directly because nothing carries it: trsf_state can target
				// the server or a runner, and the client is neither.
				b.Logf("client trsf: %s", clientDatagramState(fx))
			}
		})
	}
	fwdCancel()
	select {
	case <-fwdDone:
	case <-time.After(10 * time.Second):
	}
}

// BenchmarkUDPForwardSpliceSize sweeps the PAYLOAD at one fixed offering,
// which is what separates a per-datagram cost from a per-byte one.
//
// It is the question the MB/s ladder cannot answer. A stream on this same
// connection packs StreamMTU (16 KB on ws) into one packet, so comparing its
// MB/s against a tunnel carrying 1100-byte datagrams charges the tunnel for a
// payload size the APPLICATION chose. If delivered datagrams/s is flat across
// this sweep, the ceiling is a packet rate, and the MB/s figure is only
// whatever that rate multiplied by the application's datagram size comes to.
func BenchmarkUDPForwardSpliceSize(b *testing.B) {
	fx := newForwardBenchFixture(b, "127.0.0.1:18575")
	defer fx.stopOnce()

	sink := newUDPSink(b)
	defer sink.close()

	probe, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		b.Fatalf("probe listen: %v", err)
	}
	localPort := probe.LocalAddr().(*net.UDPAddr).Port
	probe.Close()

	spec, err := cli.ParseForwardSpec("127.0.0.1:" + strconv.Itoa(localPort) +
		":127.0.0.1:" + strconv.Itoa(sink.port) + "/udp")
	if err != nil {
		b.Fatalf("parse spec: %v", err)
	}
	fwdCtx, fwdCancel := context.WithCancel(fx.ctx)
	defer fwdCancel()
	go func() {
		_ = cli.RunForward(fwdCtx, fx.client, fx.taskID, []cli.ForwardSpec{spec}, nil, nil)
	}()

	target := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: localPort}
	waitForwardUp(b, target, sink)

	// 4000 datagrams/s offered at every size, so the offering is a constant in
	// DATAGRAMS and the only thing changing is how many bytes each carries.
	const offeredDgPerSec = 4000
	for _, size := range []int{64, 256, 1100, 4096, 8192} {
		b.Run(fmt.Sprintf("payload_%dB", size), func(b *testing.B) {
			for b.Loop() {
				before := readForwardDrops(b, fx)
				p0 := sink.pkts.Load()
				start := time.Now()
				offP := blastUDPSize(b, target, size, offeredDgPerSec*size, benchDuration)
				elapsed := time.Since(start)
				time.Sleep(1500 * time.Millisecond)
				gotP := sink.pkts.Load() - p0
				after := readForwardDrops(b, fx)

				b.ReportMetric(float64(offP)/elapsed.Seconds(), "offered_dg/s")
				b.ReportMetric(float64(gotP)/elapsed.Seconds(), "goodput_dg/s")
				b.ReportMetric(float64(gotP*int64(size))/elapsed.Seconds()/(1<<20), "goodput_MB/s")
				b.ReportMetric(float64(after.total()-before.total()), "srv_drop")
				b.Logf("size=%d offered %d / delivered %d / server %s / row: %s",
					size, offP, gotP, after.sub(before), forwardTrafficLine(b, fx))
			}
		})
	}
}

// BenchmarkUDPForwardMTU follows max_datagram_size over the life of a forward
// on a UDP carrier, which is the only transport where the number can move.
//
// Two things are being asked. First, what the tunnel's usable payload actually
// is at each moment — the design doc computes ~1157 B at DefaultInitialMTU
// growing toward ~1400, and a computed figure is not a measurement. Second,
// whether the growth is fast enough to matter: §6d notes that a 1200-byte QUIC
// Initial cannot pass until the outer path has grown, so the interesting
// quantity is how long a forward spends below that threshold and whether
// anything but time is required to leave it.
//
// The offered load is held well under the ~500 dg/s the rate ladder shows this
// path carries losslessly, so what is measured is the MTU and not the queue.
func BenchmarkUDPForwardMTU(b *testing.B) {
	fx := newForwardBenchFixtureOn(b, "udp", freeUDPPortB(b))
	defer fx.stopOnce()

	sink := newUDPSink(b)
	defer sink.close()

	probe, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		b.Fatalf("probe listen: %v", err)
	}
	localPort := probe.LocalAddr().(*net.UDPAddr).Port
	probe.Close()

	spec, err := cli.ParseForwardSpec("127.0.0.1:" + strconv.Itoa(localPort) +
		":127.0.0.1:" + strconv.Itoa(sink.port) + "/udp")
	if err != nil {
		b.Fatalf("parse spec: %v", err)
	}
	fwdCtx, fwdCancel := context.WithCancel(fx.ctx)
	defer fwdCancel()
	go func() {
		_ = cli.RunForward(fwdCtx, fx.client, fx.taskID, []cli.ForwardSpec{spec}, nil, nil)
	}()

	target := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: localPort}
	waitForwardUp(b, target, sink)

	for b.Loop() {
		// A steady trickle, so the carrier keeps sending and PLPMTUD has
		// traffic to probe alongside. An idle connection would measure how
		// fast the MTU grows with nothing happening, which is not the question.
		stop := make(chan struct{})
		go func() {
			conn, derr := net.DialUDP("udp", nil, target)
			if derr != nil {
				return
			}
			defer conn.Close()
			payload := make([]byte, 200)
			t := time.NewTicker(5 * time.Millisecond) // 200 dg/s
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					_, _ = conn.Write(payload)
				}
			}
		}()

		start := time.Now()
		var traj []string
		last := uint16(0)
		deadline := time.Now().Add(45 * time.Second)
		for time.Now().Before(deadline) {
			mtu := readForwardMTU(b, fx)
			if mtu != last {
				traj = append(traj, fmt.Sprintf("%.1fs:%d", time.Since(start).Seconds(), mtu))
				last = mtu
			}
			time.Sleep(250 * time.Millisecond)
		}
		close(stop)

		b.ReportMetric(float64(last), "final_mtu_B")
		b.Logf("max_datagram_size trajectory: %v", traj)
		b.Logf("final row: %s", forwardTrafficLine(b, fx))

		// What the number is FOR: a payload one byte over it must be refused,
		// and it must SHOW. The client drops this one before trsf ever sees it,
		// so the server cannot count it and the row read oversize=0 while the
		// client logged every one -- which is the hole the drop report closes.
		//
		// The wait is the report's own cadence: it rides the client's existing
		// 30 s reap ticker rather than a timer of its own.
		before := readForwardDrops(b, fx)
		oversize := make([]byte, int(last)+64)
		conn, derr := net.DialUDP("udp", nil, target)
		if derr == nil {
			_, _ = conn.Write(oversize)
			conn.Close()
		}
		reportBy := time.Now().Add(40 * time.Second)
		var after forwardDrops
		for time.Now().Before(reportBy) {
			time.Sleep(2 * time.Second)
			after = readForwardDrops(b, fx)
			if after.total() > before.total() {
				break
			}
		}
		b.Logf("after one %d-byte payload against mtu=%d: %s", len(oversize), last, after.sub(before))
		if after.oversize <= before.oversize {
			b.Errorf("the row still reports oversize=%d after a payload the client refused: "+
				"the drop report did not reach it", after.oversize)
		}
	}
}

// BenchmarkUDPForwardRawLoopback is the ceiling: the same payload, the same
// sink, no harness between them. Without it a tunnel number has nothing to be
// a fraction of.
func BenchmarkUDPForwardRawLoopback(b *testing.B) {
	sink := newUDPSink(b)
	defer sink.close()
	target := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: sink.port}
	for b.Loop() {
		p0, y0 := sink.pkts.Load(), sink.bytes.Load()
		start := time.Now()
		offP, offB := blastUDP(b, target, 0, benchDuration)
		elapsed := time.Since(start)
		time.Sleep(500 * time.Millisecond)
		gotP, gotB := sink.pkts.Load()-p0, sink.bytes.Load()-y0
		b.ReportMetric(float64(offB)/elapsed.Seconds()/(1<<20), "offered_MB/s")
		b.ReportMetric(float64(gotB)/elapsed.Seconds()/(1<<20), "goodput_MB/s")
		b.ReportMetric(100*float64(offP-gotP)/float64(max64(offP, 1)), "loss_%")
		b.ReportMetric(float64(offP)/elapsed.Seconds(), "offered_pps")
	}
}

// BenchmarkUDPForwardTCPReference pushes bytes through a TCP forward on the
// same three-party splice. It is the reference for "what this machinery does
// with a stream", which is the only fair thing to compare a tunnel against —
// the raw-loopback number above prices the kernel, not the harness.
func BenchmarkUDPForwardTCPReference(b *testing.B) {
	fx := newForwardBenchFixture(b, "127.0.0.1:18573")
	defer fx.stopOnce()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("tcp sink listen: %v", err)
	}
	defer ln.Close()
	var got atomic.Int64
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 64*1024)
				for {
					n, rerr := c.Read(buf)
					got.Add(int64(n))
					if rerr != nil {
						return
					}
				}
			}(conn)
		}
	}()
	sinkPort := ln.Addr().(*net.TCPAddr).Port

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("probe listen: %v", err)
	}
	localPort := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	spec, err := cli.ParseForwardSpec("127.0.0.1:" + strconv.Itoa(localPort) +
		":127.0.0.1:" + strconv.Itoa(sinkPort))
	if err != nil {
		b.Fatalf("parse spec: %v", err)
	}
	if spec.Protocol != protocol.ForwardProtocol_Tcp {
		b.Fatalf("spec protocol = %v, want tcp", spec.Protocol)
	}
	fwdCtx, fwdCancel := context.WithCancel(fx.ctx)
	defer fwdCancel()
	go func() {
		_ = cli.RunForward(fwdCtx, fx.client, fx.taskID, []cli.ForwardSpec{spec}, nil, nil)
	}()

	addr := "127.0.0.1:" + strconv.Itoa(localPort)
	var conn net.Conn
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		c, derr := net.DialTimeout("tcp", addr, time.Second)
		if derr == nil {
			conn = c
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if conn == nil {
		b.Fatal("tcp forward never accepted a connection")
	}
	defer conn.Close()

	payload := make([]byte, 64*1024)
	for b.Loop() {
		g0 := got.Load()
		start := time.Now()
		end := start.Add(benchDuration)
		var sent int64
		for time.Now().Before(end) {
			n, werr := conn.Write(payload)
			sent += int64(n)
			if werr != nil {
				b.Fatalf("tcp write: %v", werr)
			}
		}
		elapsed := time.Since(start)
		time.Sleep(1500 * time.Millisecond)
		b.ReportMetric(float64(sent)/elapsed.Seconds()/(1<<20), "offered_MB/s")
		b.ReportMetric(float64(got.Load()-g0)/elapsed.Seconds()/(1<<20), "goodput_MB/s")
	}
}

// --- helpers ------------------------------------------------------------

type forwardDrops struct{ oversize, congestion, queue uint64 }

func (d forwardDrops) total() uint64 { return d.oversize + d.congestion + d.queue }
func (d forwardDrops) sub(o forwardDrops) string {
	return fmt.Sprintf("oversize=%d congestion=%d queue=%d",
		d.oversize-o.oversize, d.congestion-o.congestion, d.queue-o.queue)
}

func readForwardDrops(b *testing.B, fx *forwardBenchFixture) forwardDrops {
	b.Helper()
	rows, err := fx.client.PortForwardListWith(fx.ctx, cli.ForwardListQuery{Task: fx.taskID})
	if err != nil {
		b.Fatalf("forward ls: %v", err)
	}
	// The RELAY's own three, which is what this row reports without --drops.
	k := protocol.ForwardDropKeys(protocol.ForwardHopRelay)
	for i := range rows {
		if over, ok := rows[i].Counter(k[0]); ok {
			cong, _ := rows[i].Counter(k[1])
			queue, _ := rows[i].Counter(k[2])
			return forwardDrops{over, cong, queue}
		}
	}
	return forwardDrops{}
}

// waitForwardUp blocks until a datagram makes it through, because a forward is
// usable only once the standing registration has reached the runner and there
// is no accept whose success would say when.
func waitForwardUp(b *testing.B, target *net.UDPAddr, sink *udpSink) {
	b.Helper()
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		before := sink.pkts.Load()
		conn, err := net.DialUDP("udp", nil, target)
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		_, _ = conn.Write([]byte("up?"))
		conn.Close()
		time.Sleep(300 * time.Millisecond)
		if sink.pkts.Load() > before {
			// One more, so the measured run does not pay the first-datagram
			// flow-table creation on both ends.
			time.Sleep(500 * time.Millisecond)
			return
		}
	}
	b.Fatal("no datagram reached the target within 25s — the forward never came up")
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// initRepoB and clearAgentEnvB mirror initRepo / clearAgentEnv, which take a
// *testing.T. They are duplicated rather than adapted because the two helpers
// take the concrete type, and widening THEIR signatures to testing.TB would be
// a change to files this benchmark has no business touching.
func clearAgentEnvB(b *testing.B) {
	b.Helper()
	for _, k := range []string{
		"HARNESS_RUNNER_ID", "HARNESS_TASK_ID", "HARNESS_AUTH_TICKET",
		"HARNESS_SERVER_CID", "HARNESS_WS_PATH", "HARNESS_PROXY_VIA_RUNNER",
	} {
		b.Setenv(k, "")
	}
}

func initRepoB(b *testing.B) string {
	b.Helper()
	dir := b.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			b.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x\n"), 0o644); err != nil {
		b.Fatal(err)
	}
	run("add", "README")
	run("commit", "-m", "init")
	return dir
}

// clientDatagramState reads the client's own trsf counters. In this benchmark
// the client is in-process, which is the only reason they are reachable at all
// -- `harness-cli trsf` can ask the server or a runner and the client is
// neither, and a datagram dropped INSIDE the run loop (a closed window at drain
// time) never reaches the caller as an error either.
func clientDatagramState(fx *forwardBenchFixture) string {
	st := fx.client.Transport().GetInternalState()
	return fmt.Sprintf("sent=%d cong_drop=%d queue_drop=%d oversize_drop=%d lost=%d cwnd=%d inflight=%d",
		st.DatagramsSent, st.DatagramsDroppedCongestion, st.DatagramsDroppedSendQueue,
		st.DatagramsDroppedOversize, st.DatagramsLost, st.CongestionWindow, st.BytesInFlight)
}
