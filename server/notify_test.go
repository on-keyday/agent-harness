package server

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/trsf"
)

// syncLogBuf is a goroutine-safe buffer for capturing async slog output.
type syncLogBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *syncLogBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncLogBuf) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func TestCapBuf_CapsAndDropsOverflow(t *testing.T) {
	c := &capBuf{cap: 8}
	c.Write([]byte("hello "))    // 6 bytes
	c.Write([]byte("world!!!!")) // only 2 more bytes retained, rest dropped
	if got := c.String(); got != "hello wo" {
		t.Fatalf("capBuf = %q, want %q", got, "hello wo")
	}
}

func TestRunNotifyHook_LogsHookOutputOnFailure(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "hook.sh")
	// Exit nonzero and print a diagnostic to stderr — what a real hook (e.g. the
	// discord example) does when delivery fails.
	body := "#!/bin/sh\necho 'discord notify: POST failed: BOOM' >&2\nexit 7\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	logbuf := &syncLogBuf{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logbuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)

	if got := runNotifyHook(script, notifyHookPayload{Text: "x"}); got != protocol.NotifyStatus_Accepted {
		t.Fatalf("status = %v, want accepted (failures are logged, not returned)", got)
	}

	// The reap+log happens in a background goroutine — poll for it.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(logbuf.String(), "BOOM") {
			return // the hook's stderr reached the server log
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("hook stderr ('BOOM') never appeared in the server log:\n%s", logbuf.String())
}

func TestRunNotifyHook_NoHook(t *testing.T) {
	if got := runNotifyHook("", notifyHookPayload{Text: "x"}); got != protocol.NotifyStatus_NoHook {
		t.Fatalf("status = %v, want no_hook", got)
	}
}

func TestRunNotifyHook_SpawnFailed(t *testing.T) {
	if got := runNotifyHook("/nonexistent/notify-hook-xyz", notifyHookPayload{Text: "x"}); got != protocol.NotifyStatus_SpawnFailed {
		t.Fatalf("status = %v, want spawn_failed", got)
	}
}

func TestRunNotifyHook_Accepted_DeliversPayload(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "out.txt")
	script := filepath.Join(dir, "hook.sh")
	body := "#!/bin/sh\ncat > " + outFile + "\necho \"LEVEL=$HARNESS_NOTIFY_LEVEL\" >> " + outFile + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	status := runNotifyHook(script, notifyHookPayload{Level: "warn", Text: "hello", Origin: "external"})
	if status != protocol.NotifyStatus_Accepted {
		t.Fatalf("status = %v, want accepted", status)
	}
	var data []byte
	for i := 0; i < 100; i++ {
		if b, err := os.ReadFile(outFile); err == nil && len(b) > 0 {
			data = b
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	s := string(data)
	if !strings.Contains(s, `"text":"hello"`) || !strings.Contains(s, "LEVEL=warn") {
		t.Fatalf("hook did not receive payload/env, got: %q", s)
	}
}

func TestNotifyRing_AppendEvicts(t *testing.T) {
	r := newNotifyRing(3)
	for i := 0; i < 5; i++ {
		r.append(protocol.NotifyEvent{Ts: uint64(i)})
	}
	snap := r.snapshot()
	if len(snap) != 3 {
		t.Fatalf("ring len = %d, want 3", len(snap))
	}
	if snap[0].Ts != 2 || snap[2].Ts != 4 {
		t.Fatalf("ring kept wrong entries: first=%d last=%d", snap[0].Ts, snap[2].Ts)
	}
}

// captureConn is a minimal ConnHandle that records the last SendMessage.
type captureConn struct{ last []byte }

func (c *captureConn) ConnectionID() objproto.ConnectionID { return objproto.ConnectionID{} }
func (c *captureConn) SendMessage(b []byte) (int, uint64, error) {
	c.last = append([]byte(nil), b...)
	return len(b), 0, nil
}
func (c *captureConn) CreateSendStream() trsf.SendStream                   { return nil }
func (c *captureConn) CreateBidirectionalStream() trsf.BidirectionalStream { return nil }
func (c *captureConn) GetReceiveStream(id trsf.StreamID) trsf.ReceiveStream { return nil }
func (c *captureConn) GetBidirectionalStream(id trsf.StreamID) trsf.BidirectionalStream {
	return nil
}

func TestHandleNotify_NoHook_RunsLiveLeg(t *testing.T) {
	var captured *protocol.NotifyEvent
	h := &TaskHandler{
		OnNotify: func(ev protocol.NotifyEvent) { captured = &ev },
	}

	nr := protocol.NotifyRequest{Level: protocol.NotifyLevel_Warn, Origin: protocol.NotifyOrigin_External}
	nr.TextLen = uint16(len("hi"))
	nr.Text = []byte("hi")
	req := protocol.TaskControlRequest{Kind: protocol.TaskControlKind_Notify, RequestId: 7}
	req.SetNotify(nr)

	conn := &captureConn{}
	h.handleNotify(conn, &req)

	if captured == nil {
		t.Fatal("OnNotify (live leg) was not called")
	}
	if string(captured.Text) != "hi" || captured.Level != protocol.NotifyLevel_Warn {
		t.Fatalf("event wrong: text=%q level=%v", captured.Text, captured.Level)
	}
	var resp protocol.TaskControlResponse
	if err := resp.DecodeExact(conn.last[1:]); err != nil { // strip AppKind byte
		t.Fatalf("decode response: %v", err)
	}
	if resp.Kind != protocol.TaskControlKind_Notify || resp.RequestId != 7 {
		t.Fatalf("response kind/id wrong: %v/%d", resp.Kind, resp.RequestId)
	}
	if out := resp.Notify(); out == nil || out.Status != protocol.NotifyStatus_NoHook {
		t.Fatalf("status = %v, want no_hook", out)
	}
}

func TestHandleNotify_WorkerOrigin_PopulatesEvent(t *testing.T) {
	var captured *protocol.NotifyEvent
	h := &TaskHandler{
		OnNotify: func(ev protocol.NotifyEvent) { captured = &ev },
	}

	nr := protocol.NotifyRequest{Level: protocol.NotifyLevel_Info, Origin: protocol.NotifyOrigin_Worker}
	nr.SetWorker(protocol.WorkerInfo{
		TaskIdLen:   uint16(len("task42")),
		TaskId:      []byte("task42"),
		RunnerIdLen: uint16(len("ws:host:1-2")),
		RunnerId:    []byte("ws:host:1-2"),
		RepoLen:     uint16(len("/repo")),
		Repo:        []byte("/repo"),
		HostnameLen: uint16(len("gmkhost")),
		Hostname:    []byte("gmkhost"),
	})
	nr.TextLen = uint16(len("done"))
	nr.Text = []byte("done")
	req := protocol.TaskControlRequest{Kind: protocol.TaskControlKind_Notify, RequestId: 9}
	req.SetNotify(nr)

	conn := &captureConn{}
	h.handleNotify(conn, &req)

	if captured == nil {
		t.Fatal("OnNotify was not called")
	}
	if captured.Origin != protocol.NotifyOrigin_Worker {
		t.Fatalf("event origin = %v, want worker", captured.Origin)
	}
	w := captured.Worker()
	if w == nil {
		t.Fatal("captured event Worker() is nil for worker origin")
	}
	if string(w.TaskId) != "task42" || string(w.Hostname) != "gmkhost" || string(w.RunnerId) != "ws:host:1-2" {
		t.Fatalf("worker fields not propagated: task_id=%q hostname=%q runner_id=%q", w.TaskId, w.Hostname, w.RunnerId)
	}
}
