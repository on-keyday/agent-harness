package cli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// ParseExecTapFilter maps --chan onto the wire enum. An unknown value is an
// error rather than a silent "all": a typo that widens what you read is worse
// than a refusal.
func ParseExecTapFilter(ch string) (protocol.ExecTapFilter, error) {
	switch ch {
	case "all", "":
		return protocol.ExecTapFilter_All, nil
	case "stdin":
		return protocol.ExecTapFilter_Stdin, nil
	case "stdout":
		return protocol.ExecTapFilter_Stdout, nil
	case "stderr":
		return protocol.ExecTapFilter_Stderr, nil
	}
	return 0, fmt.Errorf("exec tap: bad --chan %q (want stdin, stdout, stderr or all)", ch)
}

func execTapChanName(c protocol.ExecTapChannel) string {
	switch c {
	case protocol.ExecTapChannel_Stdout:
		return "stdout"
	case protocol.ExecTapChannel_Stderr:
		return "stderr"
	}
	return "stdin"
}

// execTapHeader is the fixed-column line every exec record gets:
//
//	stdin         12:34:56.789  64B
//	stderr gap    12:34:57.100  3.2MB missed
//
// Column 1 is the channel, 2 the kind (blank for data), 3 the wall clock, 4
// whatever that kind measures.
func execTapHeader(ch, kind, ts, detail string) string {
	return strings.TrimRight(fmt.Sprintf("%-6s %-5s  %-12s  %s", ch, kind, ts, detail), " ")
}

func execEndedText(e *protocol.ExecTapExecEnded) string {
	switch e.Kind {
	case protocol.ExecEventKind_Exited:
		return fmt.Sprintf("exited %d", e.ExitCode)
	case protocol.ExecEventKind_Killed:
		return "killed"
	case protocol.ExecEventKind_Failed:
		return "failed"
	}
	return strings.ToLower(e.Kind.String())
}

// RenderExecTapRecord turns one record into the lines to print. The server
// formats nothing; this runs in the client — natively, or compiled to wasm in
// the browser — so the three surfaces show the same text. The body helpers
// (hexDumpLines, printableASCII, FormatByteCount, tapTimestamp) are forward
// tap's, shared rather than copied.
func RenderExecTapRecord(execID uint64, rec *protocol.ExecTapRecord, mode TapRenderMode) []string {
	if rec == nil {
		return nil
	}
	if mode == TapJSON {
		if s := execTapRecordJSON(rec); s != "" {
			return []string{s}
		}
		return nil
	}
	ts := tapTimestamp(rec.UnixMs)
	switch rec.Kind {
	case protocol.ExecTapRecordKind_Data:
		d := rec.Data()
		if d == nil {
			return nil
		}
		if mode == TapRaw {
			return []string{string(d.Data)}
		}
		detail := FormatByteCount(uint64(len(d.Data)))
		if d.TruncatedBytes > 0 {
			detail += fmt.Sprintf("  (truncated, %s cut)", FormatByteCount(uint64(d.TruncatedBytes)))
		}
		out := []string{execTapHeader(execTapChanName(d.Channel), "", ts, detail)}
		if mode == TapText {
			return append(out, "  "+printableASCII(d.Data))
		}
		return append(out, hexDumpLines(d.Data, d.StreamOffset)...)
	case protocol.ExecTapRecordKind_Gap:
		g := rec.Gap()
		if mode == TapRaw || g == nil {
			return nil
		}
		return []string{execTapHeader(execTapChanName(g.Channel), "gap", ts, FormatByteCount(g.DroppedBytes)+" missed")}
	case protocol.ExecTapRecordKind_Eof:
		f := rec.Eof()
		if mode == TapRaw || f == nil {
			return nil
		}
		return []string{execTapHeader(execTapChanName(f.Channel), "eof", ts, "")}
	case protocol.ExecTapRecordKind_ExecEnded:
		e := rec.ExecEnded()
		if mode == TapRaw || e == nil {
			return nil
		}
		return []string{fmt.Sprintf("-- exec #%d ended: %s --", execID, execEndedText(e))}
	}
	return nil
}

// execTapRecordJSONLine is the JSON shape of one record. A struct for a stable
// field order; no omitempty on counts, because a zero is a measurement.
type execTapRecordJSONLine struct {
	Kind           string  `json:"kind"`
	UnixMs         uint64  `json:"unix_ms"`
	Chan           string  `json:"chan,omitempty"`
	Offset         *uint64 `json:"offset,omitempty"`
	Len            *int    `json:"len,omitempty"`
	TruncatedBytes *uint32 `json:"truncated_bytes,omitempty"`
	Data           *string `json:"data,omitempty"`
	DroppedBytes   *uint64 `json:"dropped_bytes,omitempty"`
	Result         string  `json:"result,omitempty"`
	ExitCode       *int32  `json:"exit_code,omitempty"`
}

func execTapRecordJSON(rec *protocol.ExecTapRecord) string {
	line := execTapRecordJSONLine{UnixMs: rec.UnixMs}
	switch rec.Kind {
	case protocol.ExecTapRecordKind_Data:
		d := rec.Data()
		if d == nil {
			return ""
		}
		n, off, cut := len(d.Data), d.StreamOffset, d.TruncatedBytes
		data := base64.StdEncoding.EncodeToString(d.Data)
		line.Kind, line.Chan = "data", execTapChanName(d.Channel)
		line.Offset, line.Len, line.TruncatedBytes, line.Data = &off, &n, &cut, &data
	case protocol.ExecTapRecordKind_Gap:
		g := rec.Gap()
		if g == nil {
			return ""
		}
		dropped := g.DroppedBytes
		line.Kind, line.Chan, line.DroppedBytes = "gap", execTapChanName(g.Channel), &dropped
	case protocol.ExecTapRecordKind_Eof:
		f := rec.Eof()
		if f == nil {
			return ""
		}
		line.Kind, line.Chan = "eof", execTapChanName(f.Channel)
	case protocol.ExecTapRecordKind_ExecEnded:
		e := rec.ExecEnded()
		if e == nil {
			return ""
		}
		code := e.ExitCode
		line.Kind, line.Result, line.ExitCode = "exec_ended", strings.ToLower(e.Kind.String()), &code
	default:
		return ""
	}
	b, _ := json.Marshal(line)
	return string(b)
}
