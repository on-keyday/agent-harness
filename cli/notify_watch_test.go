package cli

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/pubsub"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

type chunkStream struct{ chunks [][]byte }

func (s *chunkStream) ReadDirect(uint64) ([]byte, bool, error) {
	if len(s.chunks) == 0 {
		return nil, true, nil
	}
	c := s.chunks[0]
	s.chunks = s.chunks[1:]
	return c, len(s.chunks) == 0, nil
}

type frameSink struct{ wire []byte }

func (f *frameSink) AppendData(_ bool, data ...[]byte) error {
	for _, d := range data {
		f.wire = append(f.wire, d...)
	}
	return nil
}

// A payload that does not decode is skipped; the events around it still arrive.
func TestReadFrames_BadPayloadDoesNotDisturbTheNext(t *testing.T) {
	e1 := (&protocol.NotifyEvent{Ts: 1, Level: protocol.NotifyLevel_Info, Origin: protocol.NotifyOrigin_External, TextLen: 2, Text: []byte("hi")}).MustAppend(nil)
	e2 := (&protocol.NotifyEvent{Ts: 2, Level: protocol.NotifyLevel_Warn, Origin: protocol.NotifyOrigin_External, TextLen: 1, Text: []byte("x")}).MustAppend(nil)
	var sink frameSink
	for _, m := range [][]byte{e1, {0xff}, e2} {
		if err := pubsub.WriteFrame(&sink, m); err != nil {
			t.Fatal(err)
		}
	}
	var lines []string
	err := readFrames(&chunkStream{chunks: [][]byte{sink.wire[:5], sink.wire[5:]}}, "notifications", func(p []byte) {
		ev := &protocol.NotifyEvent{}
		if _, err := ev.Decode(p); err != nil {
			return
		}
		lines = append(lines, notifyEventJSONLine(ev))
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || !strings.Contains(lines[0], `"level":"info"`) || !strings.Contains(lines[1], `"level":"warn"`) {
		t.Fatalf("lines = %q, want the info event then the warn event", lines)
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
