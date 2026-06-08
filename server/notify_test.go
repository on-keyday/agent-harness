package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/trsf"
)

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
