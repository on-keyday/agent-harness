package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/exec/frame"
)

func rEntry(ts int64, typ frame.FrameType, payload string) protocol.RecordEntry {
	e := protocol.RecordEntry{TsNs: uint64(ts), FrameType: uint8(typ)}
	e.SetPayload([]byte(payload))
	return e
}

func sizeEntry(ts int64, cols, rows uint16) protocol.RecordEntry {
	ctrl := frame.Control{Type: frame.ControlType_TerminalWindowSize}
	ctrl.SetTerminalWindowSize(frame.TerminalWindowSize{Rows: rows, Columns: cols})
	e := protocol.RecordEntry{TsNs: uint64(ts), FrameType: uint8(frame.FrameType_Control)}
	e.SetPayload(ctrl.MustAppend(nil))
	return e
}

func TestRenderRawConcatenatesPayloads(t *testing.T) {
	es := []protocol.RecordEntry{
		sizeEntry(1, 80, 24),
		rEntry(2, frame.FrameType_Synth, "[head]"),
		rEntry(3, frame.FrameType_Stdout, "out"),
		rEntry(4, frame.FrameType_Stderr, "err"),
	}
	if got := string(RenderRaw(es, true)); got != "[head]outerr" {
		t.Fatalf("with synth: %q", got)
	}
	if got := string(RenderRaw(es, false)); got != "outerr" {
		t.Fatalf("without synth: %q", got)
	}
}

func castLines(t *testing.T, b []byte) []string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	for i, l := range lines {
		var v any
		if err := json.Unmarshal([]byte(l), &v); err != nil {
			t.Fatalf("line %d is not JSON: %q: %v", i, l, err)
		}
	}
	return lines
}

func TestAsciicastHeaderAndEvents(t *testing.T) {
	es := []protocol.RecordEntry{
		rEntry(1_000_000_000, frame.FrameType_Stdout, "a"),
		sizeEntry(1_500_000_000, 120, 40),
		rEntry(3_000_000_000, frame.FrameType_Stdout, "b"),
	}
	b, err := RenderAsciicast(es)
	if err != nil {
		t.Fatal(err)
	}
	lines := castLines(t, b)
	var hdr map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &hdr)
	if hdr["version"] != float64(2) || hdr["width"] != float64(120) || hdr["height"] != float64(40) || hdr["timestamp"] != float64(1) {
		t.Fatalf("header %v, want version 2, the first size 120x40, start 1", hdr)
	}
	want := []string{`[0,"o","a"]`, `[0.5,"r","120x40"]`, `[2,"o","b"]`}
	if strings.Join(lines[1:], "|") != strings.Join(want, "|") {
		t.Fatalf("events %v, want %v", lines[1:], want)
	}
}

func TestAsciicastDefaultsTo80x24WithNoSize(t *testing.T) {
	b, _ := RenderAsciicast([]protocol.RecordEntry{rEntry(1, frame.FrameType_Stdout, "x")})
	var hdr map[string]any
	_ = json.Unmarshal([]byte(castLines(t, b)[0]), &hdr)
	if hdr["width"] != float64(80) || hdr["height"] != float64(24) {
		t.Fatalf("header %v, want 80x24", hdr)
	}
}

// Review Focus 4: a character split across two frames stays one character.
func TestAsciicastCarriesSplitUTF8(t *testing.T) {
	a := "あ" // e3 81 82
	b, _ := RenderAsciicast([]protocol.RecordEntry{
		rEntry(1, frame.FrameType_Stdout, a[:1]),
		rEntry(2, frame.FrameType_Stdout, a[1:]),
	})
	lines := castLines(t, b)
	if strings.Contains(string(b), `�`) || strings.Contains(string(b), "�") {
		t.Fatalf("a split character became U+FFFD: %s", b)
	}
	var ev []any
	_ = json.Unmarshal([]byte(lines[len(lines)-1]), &ev)
	if ev[2] != "あ" {
		t.Fatalf("last event %v, want the whole character", ev)
	}
}

func TestAsciicastReplacesInvalidByte(t *testing.T) {
	b, _ := RenderAsciicast([]protocol.RecordEntry{rEntry(1, frame.FrameType_Stdout, "a\xffb")})
	lines := castLines(t, b)
	var ev []any
	_ = json.Unmarshal([]byte(lines[1]), &ev)
	if ev[2] != "a�b" {
		t.Fatalf("event %v, want the invalid byte replaced", ev)
	}
}

func TestAsciicastTimeNeverGoesBack(t *testing.T) {
	b, _ := RenderAsciicast([]protocol.RecordEntry{
		rEntry(5_000_000_000, frame.FrameType_Stdout, "a"),
		rEntry(4_000_000_000, frame.FrameType_Stdout, "b"), // a server clock step back
	})
	lines := castLines(t, b)
	var e1, e2 []any
	_ = json.Unmarshal([]byte(lines[1]), &e1)
	_ = json.Unmarshal([]byte(lines[2]), &e2)
	if e2[0].(float64) < e1[0].(float64) {
		t.Fatalf("time went back: %v then %v", e1, e2)
	}
}

func TestParseRecordRef(t *testing.T) {
	id := strings.Repeat("ab", 16)
	if task, run, err := ParseRecordRef(id + "#3"); err != nil || task != id || run != 3 {
		t.Fatalf("#3: %q %d %v", task, run, err)
	}
	if task, run, err := ParseRecordRef(id); err != nil || task != id || run != 0 {
		t.Fatalf("bare: %q %d %v", task, run, err)
	}
	for _, bad := range []string{id + "#0", id + "#x", "zz#1", ""} {
		if _, _, err := ParseRecordRef(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestSessionNewRecordFlagsSetExt(t *testing.T) {
	on := buildOpenInteractiveRequest("/r", SessionOpts{RecordSet: true, Record: true})
	if ext := on.Ext(); !on.ExtPresent() || ext == nil || !ext.RecordSet() || !ext.Record() {
		t.Fatalf("--record: ext %+v present %v", ext, on.ExtPresent())
	}
	off := buildOpenInteractiveRequest("/r", SessionOpts{RecordSet: true, Record: false})
	if ext := off.Ext(); ext == nil || !ext.RecordSet() || ext.Record() {
		t.Fatalf("--no-record: ext %+v", ext)
	}
	if none := buildOpenInteractiveRequest("/r", SessionOpts{}); none.ExtPresent() {
		t.Fatal("an open that says nothing put the ext byte on the wire")
	}
}
