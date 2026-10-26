package integration

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/iplink"
	"github.com/on-keyday/agent-harness/runner"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/server"
	"github.com/on-keyday/objtrsf/objproto"
	"golang.org/x/sys/unix"
)

// chanTun stands in for a tun device: Write puts a packet on out, Read takes
// one from in. A pair wired crosswise would loop; these are wired to the test.
type chanTun struct {
	name string
	in   chan []byte
	out  chan []byte
	once sync.Once
	done chan struct{}
}

func newChanTun(name string) *chanTun {
	return &chanTun{name: name, in: make(chan []byte, 16), out: make(chan []byte, 16), done: make(chan struct{})}
}

func (d *chanTun) Read(p []byte) (int, error) {
	select {
	case pkt := <-d.in:
		return copy(p, pkt), nil
	case <-d.done:
		return 0, os.ErrClosed
	}
}
func (d *chanTun) Write(p []byte) (int, error) {
	select {
	case <-d.done:
		return 0, os.ErrClosed
	case d.out <- append([]byte(nil), p...):
		return len(p), nil
	}
}
func (d *chanTun) Close() error { d.once.Do(func() { close(d.done) }); return nil }
func (d *chanTun) Name() string { return d.name }
func (d *chanTun) closed() bool {
	select {
	case <-d.done:
		return true
	default:
		return false
	}
}

// tunFactory hands out a fresh chanTun per open, or fails every open with err.
type tunFactory struct {
	mu   sync.Mutex
	err  error
	devs []*chanTun
}

func (f *tunFactory) open(addr netip.Prefix, mtu int) (iplink.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	d := newChanTun(fmt.Sprintf("fake%d", len(f.devs)))
	f.devs = append(f.devs, d)
	return d, nil
}

func (f *tunFactory) last() *chanTun {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.devs) == 0 {
		return nil
	}
	return f.devs[len(f.devs)-1]
}

func recvPacket(t *testing.T, ch chan []byte, within time.Duration) []byte {
	t.Helper()
	select {
	case p := <-ch:
		return p
	case <-time.After(within):
		t.Fatalf("no packet within %v", within)
		return nil
	}
}

// TestIPLinkE2E carries packets between two devices through a real server and
// runner: registration that waits for the runner's device, the relay, both
// datagram seams, the listing, a tap, a kill, and a setup the runner refuses.
// Only the kernel's tun is faked -- that part is acceptance in netem-lab.
func TestIPLinkE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}
	clearAgentEnv(t)

	repo := initRepo(t)
	fakeClaude, err := filepath.Abs("../testdata/fake-claude-slow.sh")
	if err != nil {
		t.Fatal(err)
	}
	addr := "127.0.0.1:18565"
	peerCID, err := objproto.ParseConnectionID("ws:"+addr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse server cid: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	s := server.New(server.Config{RunnerKeys: testRunnerKeys(), Addr: addr, DataDir: t.TempDir()})
	serverDone := make(chan error, 1)
	go func() { serverDone <- s.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)

	runnerTuns := &tunFactory{}
	runnerDone := make(chan error, 1)
	go func() {
		runnerDone <- runner.Run(ctx, runner.Config{RunnerKey: testRunnerKey,
			RunnerID:         runner.NewRunnerID(),
			ServerCandidates: runner.CandidatesOf(peerCID),
			AllowedRoots:     []string{repo},
			Profiles:         singleAgentProfile(fakeClaude),
			TunOpener:        runnerTuns.open,
		})
	}()
	time.Sleep(500 * time.Millisecond)

	taskID, err := cli.Submit(ctx, peerCID, repo, "ip-link-test")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	worktree := filepath.Join(repo, ".harness-worktrees", taskID)
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if _, err := os.Stat(worktree); err == nil {
			break
		}
	}

	c, err := cli.Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	cfg, err := cli.ParseIPLink("10.200.0.1/30", "10.200.0.2")
	if err != nil {
		t.Fatal(err)
	}
	clientTuns := &tunFactory{}
	fidCh := make(chan uint64, 1)
	linkDone := make(chan error, 1)
	go func() {
		linkDone <- cli.RunIPLink(ctx, c, taskID, cfg, func(string) {}, cli.IPLinkOptions{
			Open:         clientTuns.open,
			OnRegistered: func(id uint64) { fidCh <- id },
		})
	}()
	var fid uint64
	select {
	case fid = <-fidCh:
	case err := <-linkDone:
		t.Fatalf("RunIPLink returned before registering: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("the link never registered")
	}
	clientDev, runnerDev := clientTuns.last(), runnerTuns.last()
	if clientDev == nil || runnerDev == nil {
		t.Fatalf("devices: client %v runner %v", clientDev, runnerDev)
	}

	// A tap opened before any packet, rendered as JSON so the fate is a field.
	var tapMu sync.Mutex
	var tapLines []string
	tapCtx, tapCancel := context.WithCancel(ctx)
	defer tapCancel()
	go func() {
		_ = cli.StreamForwardTap(tapCtx, c, fid, cli.ForwardTapOpts{Mode: cli.TapJSON}, func(lines []string) {
			tapMu.Lock()
			tapLines = append(tapLines, lines...)
			tapMu.Unlock()
		})
	}()
	time.Sleep(300 * time.Millisecond)

	// --- (1) a packet each way, unchanged ---
	clientDev.in <- []byte("echo request")
	if got := recvPacket(t, runnerDev.out, 15*time.Second); string(got) != "echo request" {
		t.Fatalf("runner device got %q", got)
	}
	runnerDev.in <- []byte("echo reply")
	if got := recvPacket(t, clientDev.out, 15*time.Second); string(got) != "echo reply" {
		t.Fatalf("client device got %q", got)
	}

	// --- (2) the listing ---
	fwds, err := c.PortForwardListWith(ctx, cli.ForwardListQuery{Task: taskID})
	if err != nil || len(fwds) != 1 {
		t.Fatalf("forward ls: %d rows, %v", len(fwds), err)
	}
	row := &fwds[0]
	if got := cli.PortForwardSpecString(row); got != "ip 10.200.0.1/30 <-> 10.200.0.2" {
		t.Errorf("spec = %q", got)
	}
	if got := cli.PortForwardDirFlag(row.Direction); got != "--ip" {
		t.Errorf("dir = %q", got)
	}
	if _, ok := row.Counter(protocol.ForwardCounterKey_MaxDatagramSize); !ok {
		t.Error("the link row carries no datagram group")
	}
	if row.BytesToTarget == 0 || row.BytesFromTarget == 0 {
		t.Errorf("traffic counters (to=%d from=%d) after a packet each way", row.BytesToTarget, row.BytesFromTarget)
	}

	// --- (3) the tap saw both, relayed ---
	deadline := time.Now().Add(5 * time.Second)
	for {
		tapMu.Lock()
		var to, from int
		for _, l := range tapLines {
			if strings.Contains(l, `"kind":"datagram"`) && strings.Contains(l, `"fate":"relayed"`) {
				if strings.Contains(l, `"dir":"to_target"`) {
					to++
				} else {
					from++
				}
			}
		}
		tapMu.Unlock()
		if to == 1 && from == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tap saw %d to / %d from relayed datagrams, want 1 / 1: %v", to, from, tapLines)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// --- (4) a kill ends the link at both ends, and RunIPLink says why ---
	if err := c.KillPortForwardWith(ctx, fid); err != nil {
		t.Fatalf("kill: %v", err)
	}
	select {
	case err := <-linkDone:
		var closed *cli.ForwardClosedError
		if !errors.As(err, &closed) || closed.Reason != protocol.PortForwardCloseReason_Killed {
			t.Fatalf("RunIPLink after kill = %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("RunIPLink did not return after the kill")
	}
	for deadline := time.Now().Add(10 * time.Second); !(clientDev.closed() && runnerDev.closed()); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("devices still open after the kill: client %v runner %v", !clientDev.closed(), !runnerDev.closed())
		}
	}

	// --- (5) a runner that cannot open its device refuses, with its reason ---
	runnerTuns.mu.Lock()
	runnerTuns.err = fmt.Errorf("TUNSETIFF: %w", unix.EPERM)
	runnerTuns.mu.Unlock()
	err = cli.RunIPLink(ctx, c, taskID, cfg, func(string) {}, cli.IPLinkOptions{Open: clientTuns.open})
	if err == nil || !strings.Contains(err.Error(), "forward: ip link failed: runner: ") ||
		!strings.Contains(err.Error(), "operation not permitted") {
		t.Fatalf("a refused setup = %v", err)
	}
	if d := clientTuns.last(); !d.closed() {
		t.Fatal("the client's device stayed open after a refused setup")
	}

	cancel()
	<-serverDone
	<-runnerDone
}

// The real device, where the test host allows it: an unprivileged user and
// network namespace holds CAP_NET_ADMIN over itself, which is how netem-lab
// runs. Skipped when unshare is unavailable.
func TestIPLinkRealTunInNamespace(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped in -short mode")
	}
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skip("no unshare")
	}
	if os.Getenv("HARNESS_IPLINK_TUN_CHILD") == "1" {
		dev, err := iplink.OpenTun(netip.MustParsePrefix("10.200.0.1/30"), 1100)
		if err != nil {
			fmt.Println("OPEN-FAILED", err)
			os.Exit(3)
		}
		fmt.Println("OPENED", dev.Name())
		_ = dev.Close()
		os.Exit(0)
	}
	cmd := exec.Command("unshare", "--user", "--map-root-user", "--net", os.Args[0],
		"-test.run=^TestIPLinkRealTunInNamespace$", "-test.v")
	cmd.Env = append(os.Environ(), "HARNESS_IPLINK_TUN_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil && strings.Contains(string(out), "Operation not permitted") && !strings.Contains(string(out), "OPEN-FAILED") {
		t.Skipf("user namespaces unavailable here: %s", out)
	}
	if !strings.Contains(string(out), "OPENED hlink") {
		t.Fatalf("the real tun did not open inside a namespace: %v\n%s", err, out)
	}
	t.Logf("%s", strings.TrimSpace(string(out)))
}
