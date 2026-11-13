//go:build !windows

package runner

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	agentexec "github.com/on-keyday/objtrsf/exec"
)

// A cancel reaches a child the way every other end of a session does (pillar A
// spec, F5 and A-D16): through a signal it can act on, not a bare SIGKILL. An
// agent that is SIGKILLed cannot end the tool processes it started in their own
// sessions; one that gets SIGHUP or SIGTERM does. What the agent deliberately
// detached into a session of its own is not the harness's to stop: these tests
// pin that it survives.

// waitFile polls for path to exist with content, and returns that content.
func waitFile(t *testing.T, path string, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			return strings.TrimSpace(string(b))
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s did not appear within %v", filepath.Base(path), within)
	return ""
}

func pidAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// readPID reads a pid file and arranges for that process to be killed when the
// test ends, so a survivor the test asserted on is never left behind.
func readPID(t *testing.T, path string) int {
	t.Helper()
	pid, err := strconv.Atoi(waitFile(t, path, 10*time.Second))
	if err != nil {
		t.Fatalf("pid file %s: %v", path, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return pid
}

func needSetsid(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("no setsid(1) on this host")
	}
}

// waitGone polls until pid has exited (or is a zombie no one reaps, which
// Kill(pid, 0) still reports alive — so it also accepts a zombie state).
func waitGone(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return true
		}
		if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
			if f := strings.Fields(string(b)); len(f) > 2 && f[2] == "Z" {
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// The PTY path: ending the relay's input runs objtrsf's SIGHUP → SIGTERM →
// SIGKILL ladder, so the child is HUNG UP rather than killed outright, and a
// process it detached into its own session lives on.
func TestEndingTheRelayInputHangsUpThePTYChild(t *testing.T) {
	needSetsid(t)
	dir := t.TempDir()
	mark := filepath.Join(dir, "mark")
	detached := filepath.Join(dir, "detached.pid")
	ready := filepath.Join(dir, "ready")
	script := filepath.Join(dir, "agent.sh")
	body := "#!/bin/sh\n" +
		"trap 'echo hup > " + mark + "; exit 0' HUP\n" +
		"setsid sleep 300 < /dev/null > /dev/null 2>&1 &\n" +
		"echo $! > " + detached + "\n" +
		"echo ok > " + ready + "\n" +
		"while :; do sleep 0.1; done\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	far, cl := newStreamPair()
	go func() { _, _ = io.Copy(io.Discard, cl.out) }()
	relay := newSessionRelay(far, func() bool { return false })

	done := make(chan error, 1)
	go func() {
		done <- agentexec.ExecuteCommandWithOption(context.Background(), relay, slog.New(slog.NewTextHandler(io.Discard, nil)),
			"/bin/sh", []string{script}, dir, true, nil, agentexec.ExecuteOption{})
	}()
	waitFile(t, ready, 10*time.Second)
	keep := readPID(t, detached)

	relay.endInput()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the session did not end after its input was ended")
	}
	if got := waitFile(t, mark, time.Second); got != "hup" {
		t.Fatalf("mark = %q, want the child to have been hung up", got)
	}
	if !pidAlive(keep) {
		t.Fatal("a process the child detached into its own session was killed")
	}
}

// The registry's cancel of an interactive session ends its input first and
// cancels its context only after a grace, as a backstop.
func TestCancelTaskEndsAPTYSessionsInputBeforeItsContext(t *testing.T) {
	old := ptyCancelGrace
	ptyCancelGrace = 300 * time.Millisecond
	t.Cleanup(func() { ptyCancelGrace = old })

	far, _ := newStreamPair()
	relay := newSessionRelay(far, func() bool { return false })
	cancelled := make(chan struct{})
	var reg TaskRegistry
	reg.put("t1", &taskEntry{cancel: func() { close(cancelled) }, relay: relay})

	if !reg.cancelTask("t1") {
		t.Fatal("cancelTask reported the task unknown")
	}
	readErr := make(chan error, 1)
	go func() { _, err := relay.Read(make([]byte, 8)); readErr <- err }()
	select {
	case err := <-readErr:
		if err != io.EOF {
			t.Fatalf("Read after cancel = %v; want EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Read after cancel still blocks: the input was not ended")
	}
	select {
	case <-cancelled:
		t.Fatal("the context was cancelled before the grace: that is the bare SIGKILL again")
	case <-time.After(100 * time.Millisecond):
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the backstop never cancelled the context")
	}
}

// A task with no relay (a oneshot or a stream session) is cancelled as before.
func TestCancelTaskWithoutARelayCancelsAtOnce(t *testing.T) {
	cancelled := make(chan struct{})
	var reg TaskRegistry
	reg.put("t2", &taskEntry{cancel: func() { close(cancelled) }})
	reg.cancelTask("t2")
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("a task without a relay was not cancelled at once")
	}
}

// The stream path: cancelling the task TERMs the adapter's process group, so
// the agent and what it started in that group stop, and the adapter passes a
// SIGTERM on rather than SIGKILLing the agent; what the agent detached into
// its own session lives on.
func TestStreamTaskCancelStopsTheAgentGroupButNotWhatItDetached(t *testing.T) {
	needSetsid(t)
	dir := t.TempDir()
	mark := filepath.Join(dir, "mark")
	grouped := filepath.Join(dir, "grouped.pid")
	detached := filepath.Join(dir, "detached.pid")
	agent := fakeStreamAgent(t,
		"trap 'echo term > "+mark+"; exit 0' TERM\n"+
			"sleep 300 < /dev/null > /dev/null 2>&1 &\n"+
			"echo $! > "+grouped+"\n"+
			"setsid sleep 300 < /dev/null > /dev/null 2>&1 &\n"+
			"echo $! > "+detached+"\n"+
			"while :; do sleep 0.1; done\n")
	stream, cl := newStreamPair()
	go func() {
		for {
			if _, err := cl.nextStdout(); err != nil {
				return
			}
		}
	}()
	task := &StreamTask{AdapterPath: testAdapter(t), AgentArgv: []string{agent}, Dir: dir}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- task.Run(ctx, stream) }()

	inGroup := readPID(t, grouped)
	keep := readPID(t, detached)
	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the cancel")
	}
	if got := waitFile(t, mark, 3*time.Second); got != "term" {
		t.Fatalf("mark = %q, want the agent to have been sent SIGTERM", got)
	}
	if !waitGone(inGroup, 5*time.Second) {
		t.Fatal("a process in the agent's group outlived the cancel")
	}
	if !pidAlive(keep) {
		t.Fatal("a process the agent detached into its own session was killed")
	}
}
