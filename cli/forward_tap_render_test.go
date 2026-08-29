package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func tapDataRec(seq uint64, dir protocol.ForwardTapDirection, off uint64, cut uint32, payload string) *protocol.ForwardTapRecord {
	d := protocol.ForwardTapData{ConnSeq: seq, Direction: dir, StreamOffset: off, TruncatedBytes: cut}
	d.SetData([]byte(payload))
	rec := &protocol.ForwardTapRecord{Kind: protocol.ForwardTapRecordKind_Data, UnixMs: 1756000000000}
	rec.SetData(d)
	return rec
}

func tapCloseRec(seq uint64, to, from uint64) *protocol.ForwardTapRecord {
	rec := &protocol.ForwardTapRecord{Kind: protocol.ForwardTapRecordKind_ConnClose, UnixMs: 1756000000000}
	rec.SetConnClose(protocol.ForwardTapConnClose{ConnSeq: seq, BytesToTarget: to, BytesFromTarget: from})
	return rec
}

func TestRenderHexHeaderAndBody(t *testing.T) {
	lines := RenderTapRecord(tapDataRec(3, protocol.ForwardTapDirection_ToTarget, 0, 0, "GET /x"), TapHex)
	if len(lines) < 2 {
		t.Fatalf("want a header and a body line, got %v", lines)
	}
	head := lines[0]
	if !strings.HasPrefix(head, "#3 ") {
		t.Fatalf("header must lead with the connection: %q", head)
	}
	if !strings.Contains(head, "->") {
		t.Fatalf("to_target renders as -> (ASCII, the direction `forward ls` already uses): %q", head)
	}
	if !strings.Contains(head, "6B") {
		t.Fatalf("header must carry the payload size: %q", head)
	}
	if !strings.Contains(lines[1], "00000000") || !strings.Contains(lines[1], "|GET /x") {
		t.Fatalf("body is not xxd layout: %q", lines[1])
	}
}

// The offset column is the record's own stream_offset. A renderer-side counter
// would start at 0 when the tap opened rather than when the connection did.
func TestRenderHexOffsetComesFromTheWire(t *testing.T) {
	lines := RenderTapRecord(tapDataRec(3, protocol.ForwardTapDirection_ToTarget, 0x1000, 0, "ab"), TapHex)
	if !strings.Contains(lines[1], "00001000") {
		t.Fatalf("offset column must be the record's stream_offset: %q", lines[1])
	}
}

func TestRenderHexNamesTheCut(t *testing.T) {
	lines := RenderTapRecord(tapDataRec(3, protocol.ForwardTapDirection_FromTarget, 0, 1331, "HTTP"), TapHex)
	if !strings.Contains(lines[0], "<-") {
		t.Fatalf("from_target renders as <-: %q", lines[0])
	}
	if !strings.Contains(lines[0], "truncated") {
		t.Fatalf("a truncated record must say so: %q", lines[0])
	}
}

func TestRenderGapNamesWhatWasMissed(t *testing.T) {
	rec := &protocol.ForwardTapRecord{Kind: protocol.ForwardTapRecordKind_Gap, UnixMs: 1756000000000}
	rec.SetGap(protocol.ForwardTapGap{ConnSeq: 3, Direction: protocol.ForwardTapDirection_ToTarget, DroppedBytes: 3 << 20})
	line := RenderTapRecord(rec, TapHex)[0]
	if !strings.Contains(line, "gap") || !strings.Contains(line, "missed") || !strings.Contains(line, "3.0MB") {
		t.Fatalf("gap line: %q", line)
	}
}

func TestRenderConnOpenAndCloseBracketTheConnection(t *testing.T) {
	open := &protocol.ForwardTapRecord{Kind: protocol.ForwardTapRecordKind_ConnOpen, UnixMs: 1756000000000}
	o := protocol.ForwardTapConnOpen{ConnSeq: 3, TargetPort: 3000}
	o.SetTargetHost([]byte("localhost"))
	open.SetConnOpen(o)
	if line := RenderTapRecord(open, TapHex)[0]; !strings.Contains(line, "open") || !strings.Contains(line, "localhost:3000") {
		t.Fatalf("conn_open line: %q", line)
	}

	line := RenderTapRecord(tapCloseRec(3, 4198, 1<<20), TapHex)[0]
	if !strings.Contains(line, "close") || !strings.Contains(line, "->4.1kB") || !strings.Contains(line, "<-1.0MB") {
		t.Fatalf("close line must carry that connection's two totals: %q", line)
	}
}

func TestRenderForwardClosedSaysWhy(t *testing.T) {
	rec := &protocol.ForwardTapRecord{Kind: protocol.ForwardTapRecordKind_ForwardClosed, UnixMs: 1756000000000}
	rec.SetForwardClosed(protocol.ForwardTapForwardClosed{Reason: protocol.PortForwardCloseReason_Killed})
	line := RenderTapRecord(rec, TapHex)[0]
	if !strings.Contains(line, "forward") || !strings.Contains(line, "killed") {
		t.Fatalf("forward_closed line must name the reason: %q", line)
	}
}

func TestRenderTextDropsTheOffsetColumn(t *testing.T) {
	lines := RenderTapRecord(tapDataRec(3, protocol.ForwardTapDirection_ToTarget, 0, 0, "GET /x\x00\x01"), TapText)
	if len(lines) < 2 {
		t.Fatalf("want header + body, got %v", lines)
	}
	if strings.Contains(lines[1], "00000000") {
		t.Fatalf("text mode must not print the hex offset column: %q", lines[1])
	}
	if !strings.Contains(lines[1], "GET /x..") {
		t.Fatalf("non-printables render as dots: %q", lines[1])
	}
}

func TestRenderJSONIsOneObjectPerRecord(t *testing.T) {
	lines := RenderTapRecord(tapDataRec(3, protocol.ForwardTapDirection_ToTarget, 4096, 2, "hi"), TapJSON)
	if len(lines) != 1 {
		t.Fatalf("one line per record, got %d", len(lines))
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	for _, k := range []string{"kind", "unix_ms", "conn", "dir", "offset", "len", "truncated_bytes", "data"} {
		if _, ok := got[k]; !ok {
			t.Fatalf("JSON contract missing %q: %v", k, got)
		}
	}
	if got["data"].(string) != "aGk=" {
		t.Fatalf("payload must be base64: %v", got["data"])
	}
}

func TestRenderJSONCloseCarriesTotals(t *testing.T) {
	var got map[string]any
	if err := json.Unmarshal([]byte(RenderTapRecord(tapCloseRec(3, 10, 20), TapJSON)[0]), &got); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if got["bytes_to_target"].(float64) != 10 || got["bytes_from_target"].(float64) != 20 {
		t.Fatalf("conn_close JSON: %v", got)
	}
}

func TestRenderRawIsPayloadOnly(t *testing.T) {
	lines := RenderTapRecord(tapDataRec(3, protocol.ForwardTapDirection_ToTarget, 0, 0, "hi"), TapRaw)
	if len(lines) != 1 || lines[0] != "hi" {
		t.Fatalf("raw must be the payload and nothing else: %v", lines)
	}
	if got := RenderTapRecord(tapCloseRec(3, 1, 2), TapRaw); len(got) != 0 {
		t.Fatalf("raw must emit nothing for a non-data record, got %v", got)
	}
}

func TestParseTapFilterRejectsGarbage(t *testing.T) {
	if _, err := ParseTapFilter("inbound"); err == nil {
		t.Fatal("a bad --dir must error, not silently mean both")
	}
	for in, want := range map[string]protocol.ForwardTapFilter{
		"both":        protocol.ForwardTapFilter_Both,
		"to-target":   protocol.ForwardTapFilter_ToTarget,
		"from-target": protocol.ForwardTapFilter_FromTarget,
	} {
		got, err := ParseTapFilter(in)
		if err != nil || got != want {
			t.Fatalf("ParseTapFilter(%q) = %v, %v", in, got, err)
		}
	}
}

// A tap stream is a concatenation of self-delimiting records: no length prefix,
// because that would be a wire byte the schema does not describe. The reader
// therefore has to decode one and keep the remainder, including across a chunk
// boundary that splits a record.
func TestTapRecordReaderHandlesSplitRecords(t *testing.T) {
	a := tapDataRec(1, protocol.ForwardTapDirection_ToTarget, 0, 0, "hello")
	b := tapDataRec(1, protocol.ForwardTapDirection_FromTarget, 0, 0, "world")
	buf := a.MustEncodeCopy(nil)
	buf = append(buf, b.MustEncodeCopy(nil)...)

	var r tapRecordReader
	// Feed it one byte at a time: the worst split there is.
	var got []*protocol.ForwardTapRecord
	for i := 0; i < len(buf); i++ {
		recs, err := r.push(buf[i : i+1])
		if err != nil {
			t.Fatalf("push at %d: %v", i, err)
		}
		got = append(got, recs...)
	}
	if len(got) != 2 {
		t.Fatalf("decoded %d records, want 2", len(got))
	}
	if string(got[0].Data().Data) != "hello" || string(got[1].Data().Data) != "world" {
		t.Fatalf("payloads: %q %q", got[0].Data().Data, got[1].Data().Data)
	}
}
