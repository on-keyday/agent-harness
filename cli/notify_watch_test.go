package cli

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestDrainNotifyEvents(t *testing.T) {
	e1 := (&protocol.NotifyEvent{Ts: 1, Level: protocol.NotifyLevel_Info, Origin: protocol.NotifyOrigin_External, TextLen: 2, Text: []byte("hi")}).MustAppend(nil)
	e2 := (&protocol.NotifyEvent{Ts: 2, Level: protocol.NotifyLevel_Warn, Origin: protocol.NotifyOrigin_External, TextLen: 1, Text: []byte("x")}).MustAppend(nil)
	buf := append(append([]byte{}, e1...), e2...)
	buf = append(buf, e2[:1]...) // a partial third event

	var out bytes.Buffer
	var mu sync.Mutex
	rest := drainNotifyEvents(buf, &out, &mu, notifyEventJSONLine)

	if n := strings.Count(out.String(), "\n"); n != 2 {
		t.Fatalf("emitted %d lines, want 2:\n%s", n, out.String())
	}
	if len(rest) != 1 {
		t.Fatalf("leftover = %d bytes, want 1 (the partial event)", len(rest))
	}
	// each emitted line is JSON with the lowercase level
	if !strings.Contains(out.String(), `"level":"info"`) || !strings.Contains(out.String(), `"level":"warn"`) {
		t.Fatalf("expected lowercase levels in output:\n%s", out.String())
	}
}

func TestNotifyEventTextLine(t *testing.T) {
	ev := protocol.NotifyEvent{Ts: 1717800000, Level: protocol.NotifyLevel_Warn, Origin: protocol.NotifyOrigin_Worker, TextLen: 2, Text: []byte("hi")}
	ev.SetWorker(protocol.WorkerInfo{
		TaskIdLen: uint16(len("0f0d4dd6b7d3b64354cf4ff249b87403")), TaskId: []byte("0f0d4dd6b7d3b64354cf4ff249b87403"),
		HostnameLen: uint16(len("gmkhost")), Hostname: []byte("gmkhost"),
	})
	got := notifyEventTextLine(&ev)
	for _, want := range []string{"[warn]", "hi", "gmkhost", "0f0d4dd6b7d3b64354cf4ff249b87403"} {
		if !strings.Contains(got, want) {
			t.Fatalf("text line missing %q: %q", want, got)
		}
	}
}
