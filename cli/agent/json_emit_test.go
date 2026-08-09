package agent

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/on-keyday/agent-harness/agentboard"
)

func TestEmitMessageLine_InReplyToAlwaysPresent(t *testing.T) {
	var rid agentboard.RunnerID
	rid.SetTransport([]byte("ws"))
	rid.SetIpAddr([]byte{1, 2, 3, 4})
	var tid agentboard.TaskID

	for _, tc := range []struct {
		name string
		in   uint64
	}{{"not a reply", 0}, {"reply", 42}} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			emitMessageLine(&buf, 7, "t", []byte("hi"), rid, tid, "h", "claude", tc.in)
			var rec map[string]any
			if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
				t.Fatal(err)
			}
			v, ok := rec["in_reply_to"]
			if !ok {
				t.Fatal("in_reply_to absent; it must be emitted unconditionally")
			}
			if uint64(v.(float64)) != tc.in {
				t.Errorf("in_reply_to = %v, want %d", v, tc.in)
			}
		})
	}
}
