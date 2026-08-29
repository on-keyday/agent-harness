package cli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TapRenderMode selects how a tap record is written out.
type TapRenderMode int

const (
	// TapHex is the default: a fixed-column header per record, body as xxd.
	TapHex TapRenderMode = iota
	// TapText is the same header with the body as printable text, no offset
	// column. HTTP over a forward is the likeliest use and a hexdump of it is
	// unreadable.
	TapText
	// TapRaw is the payload bytes and nothing else, for piping into a decoder.
	// Non-data records render as nothing.
	TapRaw
	// TapJSON is one object per record, matching what `ls --json` and
	// `forward ls --json` already are.
	TapJSON
)

// ParseTapFilter maps the --dir flag onto the wire enum. An unrecognised value
// is an error rather than a silent "both": a typo that quietly widens what you
// are reading is worse than a refusal.
func ParseTapFilter(dir string) (protocol.ForwardTapFilter, error) {
	switch dir {
	case "both", "":
		return protocol.ForwardTapFilter_Both, nil
	case "to-target":
		return protocol.ForwardTapFilter_ToTarget, nil
	case "from-target":
		return protocol.ForwardTapFilter_FromTarget, nil
	}
	return 0, fmt.Errorf("forward tap: bad --dir %q (want to-target, from-target or both)", dir)
}

// dirArrow renders a direction the way `forward ls` already writes a row —
// "127.0.0.1:8080 -> localhost:3000" — so a right arrow means "toward the
// target" on both surfaces. ASCII, because a Windows console is a first-class
// client here.
func dirArrow(d protocol.ForwardTapDirection) string {
	if d == protocol.ForwardTapDirection_FromTarget {
		return "<-"
	}
	return "->"
}

// tapHeader is the fixed-column line every record gets:
//
//	#3 ->     12:34:56.789  64B
//	#3 open   12:34:56.700  localhost:3000
//	   forward 12:35:02.000 closed (killed)
//
// Column 1 is the connection, 2 the kind, 3 the wall clock, 4 whatever that
// kind measures.
func tapHeader(conn, kind, ts, detail string) string {
	return strings.TrimRight(fmt.Sprintf("%-4s %-7s %-12s %s", conn, kind, ts, detail), " ")
}

func tapTimestamp(unixMs uint64) string {
	return time.UnixMilli(int64(unixMs)).Format("15:04:05.000")
}

// RenderTapRecord turns one record into the lines to print. The SERVER never
// formats anything — it sends records and this runs in the client, natively or
// compiled to wasm in the browser, so all three surfaces show the same text.
func RenderTapRecord(rec *protocol.ForwardTapRecord, mode TapRenderMode) []string {
	if rec == nil {
		return nil
	}
	if mode == TapJSON {
		return []string{tapRecordJSON(rec)}
	}
	ts := tapTimestamp(rec.UnixMs)

	switch rec.Kind {
	case protocol.ForwardTapRecordKind_Data:
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
		out := []string{tapHeader(fmt.Sprintf("#%d", d.ConnSeq), dirArrow(d.Direction), ts, detail)}
		if mode == TapText {
			return append(out, "  "+printableASCII(d.Data))
		}
		return append(out, hexDumpLines(d.Data, d.StreamOffset)...)

	case protocol.ForwardTapRecordKind_Gap:
		if mode == TapRaw {
			return nil
		}
		g := rec.Gap()
		if g == nil {
			return nil
		}
		return []string{tapHeader(fmt.Sprintf("#%d", g.ConnSeq), "gap", ts,
			FormatByteCount(g.DroppedBytes)+" missed "+dirArrow(g.Direction))}

	case protocol.ForwardTapRecordKind_ConnOpen:
		if mode == TapRaw {
			return nil
		}
		o := rec.ConnOpen()
		if o == nil {
			return nil
		}
		return []string{tapHeader(fmt.Sprintf("#%d", o.ConnSeq), "open", ts,
			fmt.Sprintf("%s:%d", o.TargetHost, o.TargetPort))}

	case protocol.ForwardTapRecordKind_ConnClose:
		if mode == TapRaw {
			return nil
		}
		c := rec.ConnClose()
		if c == nil {
			return nil
		}
		return []string{tapHeader(fmt.Sprintf("#%d", c.ConnSeq), "close", ts,
			fmt.Sprintf("->%s <-%s", FormatByteCount(c.BytesToTarget), FormatByteCount(c.BytesFromTarget)))}

	case protocol.ForwardTapRecordKind_ForwardClosed:
		if mode == TapRaw {
			return nil
		}
		f := rec.ForwardClosed()
		if f == nil {
			return nil
		}
		return []string{tapHeader("", "forward", ts, "closed ("+tapCloseReason(f.Reason)+")")}
	}
	return nil
}

func tapCloseReason(r protocol.PortForwardCloseReason) string {
	switch r {
	case protocol.PortForwardCloseReason_Killed:
		return "killed"
	case protocol.PortForwardCloseReason_TaskGone:
		return "task gone"
	}
	return strings.ToLower(r.String())
}

// hexDumpLines renders the payload in xxd layout. The offset column is the
// record's stream_offset, counted by the server from the connection's first
// byte: record boundaries are an artifact of the relay's read size and carry no
// application meaning, so a per-record offset would restart mid-message.
func hexDumpLines(data []byte, offset uint64) []string {
	var out []string
	for i := 0; i < len(data); i += 16 {
		end := i + 16
		if end > len(data) {
			end = len(data)
		}
		chunk := data[i:end]
		var hex strings.Builder
		for j := 0; j < 16; j++ {
			if j == 8 {
				hex.WriteByte(' ')
			}
			if j < len(chunk) {
				fmt.Fprintf(&hex, "%02x ", chunk[j])
			} else {
				hex.WriteString("   ")
			}
		}
		out = append(out, fmt.Sprintf("  %08x  %s |%s|", offset+uint64(i), hex.String(), printableASCII(chunk)))
	}
	return out
}

// printableASCII is the gutter of a hex dump and the whole body of --text.
func printableASCII(data []byte) string {
	var b strings.Builder
	b.Grow(len(data))
	for _, c := range data {
		if c >= 0x20 && c < 0x7f {
			b.WriteByte(c)
		} else {
			b.WriteByte('.')
		}
	}
	return b.String()
}

// tapRecordJSONLine is the JSON shape of one record. A struct, not a map, so
// field order is stable across lines — the same reason portForwardJSON is one.
// omitempty is deliberately absent on the counts: a zero is a measurement.
type tapRecordJSONLine struct {
	Kind            string  `json:"kind"`
	UnixMs          uint64  `json:"unix_ms"`
	Conn            uint64  `json:"conn,omitempty"`
	Dir             string  `json:"dir,omitempty"`
	Offset          *uint64 `json:"offset,omitempty"`
	Len             *int    `json:"len,omitempty"`
	TruncatedBytes  *uint32 `json:"truncated_bytes,omitempty"`
	Data            string  `json:"data,omitempty"`
	DroppedBytes    *uint64 `json:"dropped_bytes,omitempty"`
	Target          string  `json:"target,omitempty"`
	BytesToTarget   *uint64 `json:"bytes_to_target,omitempty"`
	BytesFromTarget *uint64 `json:"bytes_from_target,omitempty"`
	Reason          string  `json:"reason,omitempty"`
}

func tapDirName(d protocol.ForwardTapDirection) string {
	if d == protocol.ForwardTapDirection_FromTarget {
		return "from_target"
	}
	return "to_target"
}

func tapRecordJSON(rec *protocol.ForwardTapRecord) string {
	line := tapRecordJSONLine{UnixMs: rec.UnixMs}
	switch rec.Kind {
	case protocol.ForwardTapRecordKind_Data:
		d := rec.Data()
		if d == nil {
			return ""
		}
		n := len(d.Data)
		off, cut := d.StreamOffset, d.TruncatedBytes
		line.Kind = "data"
		line.Conn = d.ConnSeq
		line.Dir = tapDirName(d.Direction)
		line.Offset = &off
		line.Len = &n
		line.TruncatedBytes = &cut
		line.Data = base64.StdEncoding.EncodeToString(d.Data)
	case protocol.ForwardTapRecordKind_Gap:
		g := rec.Gap()
		if g == nil {
			return ""
		}
		dropped := g.DroppedBytes
		line.Kind = "gap"
		line.Conn = g.ConnSeq
		line.Dir = tapDirName(g.Direction)
		line.DroppedBytes = &dropped
	case protocol.ForwardTapRecordKind_ConnOpen:
		o := rec.ConnOpen()
		if o == nil {
			return ""
		}
		line.Kind = "conn_open"
		line.Conn = o.ConnSeq
		line.Target = fmt.Sprintf("%s:%d", o.TargetHost, o.TargetPort)
	case protocol.ForwardTapRecordKind_ConnClose:
		c := rec.ConnClose()
		if c == nil {
			return ""
		}
		to, from := c.BytesToTarget, c.BytesFromTarget
		line.Kind = "conn_close"
		line.Conn = c.ConnSeq
		line.BytesToTarget = &to
		line.BytesFromTarget = &from
	case protocol.ForwardTapRecordKind_ForwardClosed:
		f := rec.ForwardClosed()
		if f == nil {
			return ""
		}
		line.Kind = "forward_closed"
		line.Reason = tapCloseReason(f.Reason)
	default:
		return ""
	}
	b, _ := json.Marshal(line)
	return string(b)
}
