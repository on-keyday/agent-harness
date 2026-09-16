package integration

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/server"
	"github.com/on-keyday/objtrsf/objproto"
)

// TestUDPPortForwardE2E carries datagrams the whole way — client socket →
// server relay → runner socket → target — and back.
//
// It asserts the three things that make a udp forward different from the tcp
// one beside it:
//
//   - a round trip happens at all, with no accept and no per-connection open;
//   - two SOURCE PORTS are independent flows, whose replies come back to the
//     right one. That is the property the flow table exists for, and the way it
//     fails is by working for one source and mixing up two;
//   - the listing says it is udp and reports the datagram counters, because a
//     tunnel whose drops are invisible is one nobody can debug.
func TestUDPPortForwardE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}
	clearAgentEnv(t)

	repo := initRepo(t)
	fakeClaude, err := filepath.Abs("../testdata/fake-claude-slow.sh")
	if err != nil {
		t.Fatal(err)
	}

	addr := "127.0.0.1:18561"
	peerCID, err := objproto.ParseConnectionID("ws:"+addr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse server cid: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	s := server.New(server.Config{Addr: addr, DataDir: t.TempDir()})
	serverDone := make(chan error, 1)
	go func() { serverDone <- s.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)

	runnerDone := make(chan error, 1)
	go func() {
		runnerDone <- runner.Run(ctx, runner.Config{
			RunnerID:         runner.NewRunnerID(),
			ServerCandidates: runner.CandidatesOf(peerCID),
			AllowedRoots:     []string{repo},
			Profiles:         singleAgentProfile(fakeClaude),
		})
	}()
	time.Sleep(500 * time.Millisecond)

	taskID, err := cli.Submit(ctx, peerCID, repo, "udp-pf-test")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	worktree := filepath.Join(repo, ".harness-worktrees", taskID)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(worktree); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("worktree did not appear: %v", err)
	}

	// The target: a UDP echo that answers each datagram from the socket it
	// arrived on, which is what makes the reply routable back to its flow.
	echo, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	defer echo.Close()
	echoPort := echo.LocalAddr().(*net.UDPAddr).Port
	go func() {
		buf := make([]byte, 64*1024)
		for {
			n, from, rerr := echo.ReadFromUDP(buf)
			if rerr != nil {
				return
			}
			_, _ = echo.WriteToUDP(append([]byte("echo:"), buf[:n]...), from)
		}
	}()

	// A free local port for the forward's own socket.
	probe, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	localPort := probe.LocalAddr().(*net.UDPAddr).Port
	probe.Close()

	c, err := cli.Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	spec, err := cli.ParseForwardSpec(net.JoinHostPort("127.0.0.1",
		strconv.Itoa(localPort)) + ":127.0.0.1:" + strconv.Itoa(echoPort) + "/udp")
	if err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	if spec.Protocol != protocol.ForwardProtocol_Udp {
		t.Fatalf("spec protocol = %v, want udp", spec.Protocol)
	}

	fwdCtx, fwdCancel := context.WithCancel(ctx)
	fwdDone := make(chan error, 1)
	go func() { fwdDone <- cli.RunForward(fwdCtx, c, taskID, []cli.ForwardSpec{spec}, nil, nil) }()

	target := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: localPort}

	// --- (1) a round trip, with no accept anywhere in it ---
	reply := roundTripUDP(t, target, []byte("hello"), 20*time.Second)
	if string(reply) != "echo:hello" {
		t.Fatalf("reply = %q, want %q", reply, "echo:hello")
	}

	// --- (2) two source ports are independent flows ---
	// Each socket gets its own flow id, so each reply must come back to the
	// socket that asked. Sent from two sockets held open at once, because the
	// failure this catches is a table that maps both to one entry.
	a, err := net.DialUDP("udp", nil, target)
	if err != nil {
		t.Fatalf("flow a dial: %v", err)
	}
	defer a.Close()
	b, err := net.DialUDP("udp", nil, target)
	if err != nil {
		t.Fatalf("flow b dial: %v", err)
	}
	defer b.Close()

	if _, err := a.Write([]byte("from-a")); err != nil {
		t.Fatalf("flow a write: %v", err)
	}
	if _, err := b.Write([]byte("from-b")); err != nil {
		t.Fatalf("flow b write: %v", err)
	}
	if got := readUDP(t, a, 15*time.Second); string(got) != "echo:from-a" {
		t.Errorf("flow a got %q, want %q — replies are crossing between flows", got, "echo:from-a")
	}
	if got := readUDP(t, b, 15*time.Second); string(got) != "echo:from-b" {
		t.Errorf("flow b got %q, want %q — replies are crossing between flows", got, "echo:from-b")
	}

	// --- (3) the listing says udp, and carries the datagram counters ---
	fwds, err := c.PortForwardListWith(ctx, taskID)
	if err != nil {
		t.Fatalf("forward ls: %v", err)
	}
	if len(fwds) != 1 {
		t.Fatalf("forward ls returned %d rows, want 1", len(fwds))
	}
	row := &fwds[0]
	if row.Protocol != protocol.ForwardProtocol_Udp {
		t.Errorf("row protocol = %v, want udp", row.Protocol)
	}
	if row.Route != protocol.DataPlaneRoute_Splice {
		t.Errorf("row route = %v, want splice", row.Route)
	}
	if row.BytesToTarget == 0 || row.BytesFromTarget == 0 {
		t.Errorf("traffic counters are zero (to=%d from=%d) after a completed round trip",
			row.BytesToTarget, row.BytesFromTarget)
	}
	line := cli.PortForwardTrafficLine(row)
	for _, want := range []string{"mtu=", "oversize=0", "congested=0", "queued=0"} {
		if !strings.Contains(line, want) {
			t.Errorf("traffic line %q is missing %q", line, want)
		}
	}

	fwdCancel()
	select {
	case <-fwdDone:
	case <-time.After(10 * time.Second):
		t.Error("RunForward did not return within 10s of cancellation")
	}
	cancel()
	<-serverDone
	<-runnerDone
}

// roundTripUDP sends one datagram and returns the answer, retrying until the
// forward's registration has reached the runner. The retry is the point: a
// forward is usable only once the standing instruction has crossed, and there
// is no accept whose success would say when.
func roundTripUDP(t *testing.T, target *net.UDPAddr, payload []byte, within time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		conn, err := net.DialUDP("udp", nil, target)
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		_, _ = conn.Write(payload)
		_ = conn.SetReadDeadline(time.Now().Add(700 * time.Millisecond))
		buf := make([]byte, 64*1024)
		n, rerr := conn.Read(buf)
		conn.Close()
		if rerr == nil && n > 0 {
			return buf[:n]
		}
	}
	t.Fatalf("no reply through the udp forward within %v", within)
	return nil
}

func readUDP(t *testing.T, conn *net.UDPConn, within time.Duration) []byte {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(within))
	buf := make([]byte, 64*1024)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return buf[:n]
}
