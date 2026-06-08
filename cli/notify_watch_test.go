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
	rest := drainNotifyEvents(buf, &out, &mu)

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
