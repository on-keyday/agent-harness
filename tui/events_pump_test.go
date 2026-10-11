package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
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

func execEvent(t *testing.T, line string) []byte {
	t.Helper()
	var info protocol.ExecRunInfo
	info.Argv = protocol.ExecArgv{ArgvLen: 1, Argv: []protocol.ExecArg{{ArgLen: uint16(len(line)), Arg: []byte(line)}}}
	ev := protocol.ExecStatusEvent{Kind: protocol.StatusEventKind_ExecStarted, Ts: 1, Info: info}
	return ev.MustAppend(nil)
}

func execMsg(payload []byte) tea.Msg {
	ev, err := DecodeExecStatus(payload)
	if err != nil {
		return nil
	}
	return ExecStatusMsg{Event: ev}
}

// The live failure: an ExecStatusEvent with a 3 KiB argv split across reads
// reached the TUI as two halves, and neither decoded.
func TestPumpEvents_LongExecEventSplitAcrossReads(t *testing.T) {
	line := "echo " + strings.Repeat("a", 3000)
	var sink frameSink
	if err := pubsub.WriteFrame(&sink, execEvent(t, line)); err != nil {
		t.Fatal(err)
	}
	mid := len(sink.wire) / 2
	st := &chunkStream{chunks: [][]byte{sink.wire[:mid], sink.wire[mid:]}}

	var got []tea.Msg
	_ = PumpEvents(context.Background(), st, "execs.status", execMsg, func(m tea.Msg) { got = append(got, m) })

	if len(got) != 1 {
		t.Fatalf("got %d messages, want 1", len(got))
	}
	ev := got[0].(ExecStatusMsg).Event
	if a := ev.Info.Argv.Argv; len(a) != 1 || string(a[0].Arg) != line {
		t.Fatalf("argv did not survive the split")
	}
}

// Two events in one read: the second was dropped without a word.
func TestPumpEvents_TwoEventsInOneRead(t *testing.T) {
	var sink frameSink
	for _, l := range []string{"one", "two"} {
		if err := pubsub.WriteFrame(&sink, execEvent(t, l)); err != nil {
			t.Fatal(err)
		}
	}
	var got []tea.Msg
	_ = PumpEvents(context.Background(), &chunkStream{chunks: [][]byte{sink.wire}}, "execs.status", execMsg,
		func(m tea.Msg) { got = append(got, m) })
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2", len(got))
	}
}
