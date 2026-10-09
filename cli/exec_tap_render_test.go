package cli

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func execDataRec(ch protocol.ExecTapChannel, off uint64, cut uint32, payload []byte) *protocol.ExecTapRecord {
	d := protocol.ExecTapData{Channel: ch, StreamOffset: off, TruncatedBytes: cut}
	d.SetData(payload)
	rec := &protocol.ExecTapRecord{Kind: protocol.ExecTapRecordKind_Data, UnixMs: 1756000000000}
	rec.SetData(d)
	return rec
}

func TestRenderExecTapHexHeaderAndBody(t *testing.T) {
	lines := RenderExecTapRecord(4, execDataRec(protocol.ExecTapChannel_Stdin, 0x20, 0, []byte(`{"id":1}`)), TapHex)
	if len(lines) < 2 {
		t.Fatalf("want header + body, got %v", lines)
	}
	if !strings.HasPrefix(lines[0], "stdin ") || !strings.Contains(lines[0], "8B") {
		t.Fatalf("header: %q", lines[0])
	}
	if !strings.Contains(lines[1], "00000020") || !strings.Contains(lines[1], `|{"id":1}|`) {
		t.Fatalf("body must be xxd with the wire offset: %q", lines[1])
	}
}

func TestRenderExecTapGapEofAndEnd(t *testing.T) {
	gap := &protocol.ExecTapRecord{Kind: protocol.ExecTapRecordKind_Gap, UnixMs: 1756000000000}
	gap.SetGap(protocol.ExecTapGap{Channel: protocol.ExecTapChannel_Stderr, DroppedBytes: 3 << 20})
	if l := RenderExecTapRecord(4, gap, TapHex)[0]; !strings.HasPrefix(l, "stderr") || !strings.Contains(l, "gap") || !strings.Contains(l, "3.0MB missed") {
		t.Fatalf("gap: %q", l)
	}
	eof := &protocol.ExecTapRecord{Kind: protocol.ExecTapRecordKind_Eof, UnixMs: 1756000000000}
	eof.SetEof(protocol.ExecTapEof{Channel: protocol.ExecTapChannel_Stdin})
	if l := RenderExecTapRecord(4, eof, TapHex)[0]; !strings.HasPrefix(l, "stdin") || !strings.Contains(l, "eof") {
		t.Fatalf("eof: %q", l)
	}
	end := &protocol.ExecTapRecord{Kind: protocol.ExecTapRecordKind_ExecEnded, UnixMs: 1756000000000}
	end.SetExecEnded(protocol.ExecTapExecEnded{Kind: protocol.ExecEventKind_Exited, ExitCode: 0})
	if l := RenderExecTapRecord(4, end, TapHex)[0]; l != "-- exec #4 ended: exited 0 --" {
		t.Fatalf("end: %q", l)
	}
	killed := &protocol.ExecTapRecord{Kind: protocol.ExecTapRecordKind_ExecEnded, UnixMs: 1756000000000}
	killed.SetExecEnded(protocol.ExecTapExecEnded{Kind: protocol.ExecEventKind_Killed, ExitCode: -1})
	if l := RenderExecTapRecord(4, killed, TapHex)[0]; l != "-- exec #4 ended: killed --" {
		t.Fatalf("killed: %q", l)
	}
}

// Review Focus 4: binary payload in every mode.
func TestRenderExecTapBinaryPayload(t *testing.T) {
	payload := []byte{0x00, 'o', 'k', 0xff, 0xc3}
	rec := execDataRec(protocol.ExecTapChannel_Stdout, 0, 0, payload)

	text := RenderExecTapRecord(4, rec, TapText)
	if text[1] != "  .ok.." {
		t.Fatalf("--text body: %q", text[1])
	}
	raw := RenderExecTapRecord(4, rec, TapRaw)
	if len(raw) != 1 || raw[0] != string(payload) {
		t.Fatalf("--raw must be the bytes exactly: %q", raw)
	}
	var obj struct {
		Kind string `json:"kind"`
		Chan string `json:"chan"`
		Data string `json:"data"`
		Len  int    `json:"len"`
	}
	if err := json.Unmarshal([]byte(RenderExecTapRecord(4, rec, TapJSON)[0]), &obj); err != nil {
		t.Fatal(err)
	}
	got, _ := base64.StdEncoding.DecodeString(obj.Data)
	if obj.Kind != "data" || obj.Chan != "stdout" || obj.Len != 5 || string(got) != string(payload) {
		t.Fatalf("--json: %+v", obj)
	}
}

func TestRenderExecTapRawDropsNonData(t *testing.T) {
	eof := &protocol.ExecTapRecord{Kind: protocol.ExecTapRecordKind_Eof, UnixMs: 1}
	eof.SetEof(protocol.ExecTapEof{Channel: protocol.ExecTapChannel_Stdin})
	if got := RenderExecTapRecord(4, eof, TapRaw); len(got) != 0 {
		t.Fatalf("--raw writes payload only, got %q", got)
	}
}

func TestParseExecTapFilter(t *testing.T) {
	for in, want := range map[string]protocol.ExecTapFilter{
		"": protocol.ExecTapFilter_All, "all": protocol.ExecTapFilter_All,
		"stdin": protocol.ExecTapFilter_Stdin, "stdout": protocol.ExecTapFilter_Stdout, "stderr": protocol.ExecTapFilter_Stderr,
	} {
		if got, err := ParseExecTapFilter(in); err != nil || got != want {
			t.Fatalf("%q -> %v %v", in, got, err)
		}
	}
	if _, err := ParseExecTapFilter("sideways"); err == nil {
		t.Fatal("an unknown channel must be refused, not widened to all")
	}
}
