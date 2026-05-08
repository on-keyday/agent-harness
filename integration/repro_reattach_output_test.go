//go:build integration

package integration

import (
	"bytes"
	"context"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func fakeClaudeBurstThenTickPath(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("../testdata/fake-claude-burst-then-tick.sh")
	if err != nil {
		t.Fatalf("resolve fake-claude-burst-then-tick.sh: %v", err)
	}
	return abs
}

// drainBuf is a thread-safe sink for stdout bytes; tests poll Snapshot() until
// the awaited substring appears.
type drainBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (d *drainBuf) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.buf.Write(p)
}

func (d *drainBuf) Snapshot() []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]byte{}, d.buf.Bytes()...)
}

// TestSessionReattach_PostReattachOutput reproduces the user-reported bug:
// after detach (0 attached) -> reattach, runner output is supposed to keep
// flowing to the new client, but reportedly does not.
func TestSessionReattach_PostReattachOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("fake-claude scripts require bash — skipping on Windows")
	}

	serverCID := startServer(t)
	repo := tempRepo(t)

	startRunner(t, serverCID, runnerOpts{
		MaxTasks:  1,
		Roots:     []string{repo},
		ClaudeBin: fakeClaudeBurstThenTickPath(t), // 1.5 MiB burst → tick mode
	})

	// Client 1: open detachable session.
	c1 := dialClient(t, serverCID)
	sel := protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any}
	stream1, taskIDHex, err := c1.OpenInteractiveWithSelectorAndArgs(
		context.Background(), repo, sel, nil, "", true,
	)
	if err != nil {
		t.Fatalf("OpenInteractive: %v", err)
	}
	t.Logf("opened detachable session, task=%s", taskIDHex[:12])

	// Drain stdout so the runner's send window doesn't fill.
	d1 := &drainBuf{}
	drain1Done := make(chan struct{})
	go func() {
		defer close(drain1Done)
		buf := make([]byte, 4096)
		for {
			n, rerr := stream1.Stdout().Read(buf)
			if n > 0 {
				d1.Write(buf[:n])
			}
			if rerr != nil {
				return
			}
		}
	}()

	// Wait until at least a couple ticks have arrived.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if bytes.Contains(d1.Snapshot(), []byte("tick 1")) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !bytes.Contains(d1.Snapshot(), []byte("tick 1")) {
		t.Fatalf("never saw 'tick 1' on first attach; got %q", d1.Snapshot())
	}
	preDetachLast := d1.Snapshot()
	t.Logf("first attach saw %d bytes", len(preDetachLast))

	// Detach.
	stream1.Close()
	<-drain1Done

	eventually(t, func() bool {
		ti := getTask(t, c1, taskIDHex)
		return ti.Status == protocol.TaskStatus_Detached
	}, 5*time.Second, 100*time.Millisecond, "task to reach Detached")
	t.Logf("detached; reattaching with fresh client")

	// Wait some real time so the ticker emits more output AFTER detach
	// and before reattach. This stresses the "0 attached -> reattach" path.
	time.Sleep(1 * time.Second)

	// Reattach using the SAME client (mirrors TUI/WebUI: one cli.Client lives
	// across attach/detach/reattach, only the bidi stream is recreated).
	stream2, replayBytes, err := c1.AttachSession(context.Background(), taskIDHex)
	if err != nil {
		t.Fatalf("AttachSession: %v", err)
	}
	t.Logf("reattached, replayBytes=%d", replayBytes)

	d2 := &drainBuf{}
	drain2Done := make(chan struct{})
	go func() {
		defer close(drain2Done)
		buf := make([]byte, 4096)
		for {
			n, rerr := stream2.Stdout().Read(buf)
			if n > 0 {
				d2.Write(buf[:n])
			}
			if rerr != nil {
				return
			}
		}
	}()

	// THE BUG: after reattach, NEW ticks (emitted live, post-reattach) must
	// reach stream2. Wait for replay to settle (~500ms) then sample, then
	// wait 3s and resample — live forwarding must produce additional bytes.
	time.Sleep(500 * time.Millisecond)
	postReplay := d2.Snapshot()
	t.Logf("post-replay snapshot: %d bytes", len(postReplay))

	time.Sleep(3 * time.Second)
	postLive := d2.Snapshot()
	t.Logf("post-live (3s later): %d bytes", len(postLive))

	delta := len(postLive) - len(postReplay)
	if delta == 0 {
		t.Errorf("post-reattach LIVE output did not flow: postReplay=%d postLive=%d (delta=0)", len(postReplay), len(postLive))
		tail := postLive
		if len(tail) > 600 {
			tail = tail[len(tail)-600:]
		}
		t.Logf("tail of stream2 stdout: %q", tail)
	} else {
		t.Logf("live forwarding delta=%d bytes — OK", delta)
	}

	// Bonus: tick numbers should have advanced. Find the highest tick N seen.
	maxTick := -1
	for i := 0; i+8 <= len(postLive); i++ {
		if postLive[i] == 't' && bytes.HasPrefix(postLive[i:], []byte("tick ")) {
			j := i + 5
			n := 0
			for j < len(postLive) && postLive[j] >= '0' && postLive[j] <= '9' {
				n = n*10 + int(postLive[j]-'0')
				j++
			}
			if j > i+5 && n > maxTick {
				maxTick = n
			}
		}
	}
	t.Logf("highest tick observed on stream2: %d", maxTick)

	stream2.Close()
	<-drain2Done

	// Cycle 2: detach again, reattach again. The user reports the bug after
	// reaching "0 attached" — exercise that path more than once.
	eventually(t, func() bool {
		ti := getTask(t, c1, taskIDHex)
		return ti.Status == protocol.TaskStatus_Detached
	}, 5*time.Second, 100*time.Millisecond, "task to reach Detached after second detach")

	time.Sleep(500 * time.Millisecond)

	stream3, _, err := c1.AttachSession(context.Background(), taskIDHex)
	if err != nil {
		t.Fatalf("third AttachSession: %v", err)
	}
	d3 := &drainBuf{}
	drain3Done := make(chan struct{})
	go func() {
		defer close(drain3Done)
		buf := make([]byte, 4096)
		for {
			n, rerr := stream3.Stdout().Read(buf)
			if n > 0 {
				d3.Write(buf[:n])
			}
			if rerr != nil {
				return
			}
		}
	}()
	time.Sleep(500 * time.Millisecond)
	postReplay3 := d3.Snapshot()
	time.Sleep(3 * time.Second)
	postLive3 := d3.Snapshot()
	delta3 := len(postLive3) - len(postReplay3)
	t.Logf("cycle 2: postReplay3=%d postLive3=%d delta=%d", len(postReplay3), len(postLive3), delta3)
	if delta3 == 0 {
		t.Errorf("cycle 2: post-reattach LIVE output did not flow (delta=0)")
	}
	stream3.Close()
	<-drain3Done
}
