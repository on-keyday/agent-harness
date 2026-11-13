package cli

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/exec/frame"
)

// A PTY record is `session snapshot --raw` kept for a whole run (pillar A
// spec, A-D27): the server sends its entries, and what is read out of them is
// rendered here, so every client renders a record the same way.

// isOutput reports whether an entry carries bytes the terminal showed: the
// PTY's own (Stdout, Stderr) or, with synth, the server's (Synth: a segment's
// head, a ring seed's modes).
func isOutput(e protocol.RecordEntry, synth bool) bool {
	switch frame.FrameType(e.FrameType) {
	case frame.FrameType_Stdout, frame.FrameType_Stderr:
		return true
	case frame.FrameType_Synth:
		return synth
	}
	return false
}

// RenderRaw is the record as `session snapshot --raw` prints a screen: the
// output payloads in order, Synth included unless withSynth is false.
func RenderRaw(entries []protocol.RecordEntry, withSynth bool) []byte {
	var b bytes.Buffer
	for _, e := range entries {
		if isOutput(e, withSynth) {
			b.Write(e.Payload)
		}
	}
	return b.Bytes()
}

// recordSize decodes a size entry (the session's TerminalWindowSize control
// frame) with objtrsf's own decoder.
func recordSize(e protocol.RecordEntry) (cols, rows int, ok bool) {
	if frame.FrameType(e.FrameType) != frame.FrameType_Control {
		return 0, 0, false
	}
	var ctrl frame.Control
	if err := ctrl.DecodeExact(e.Payload); err != nil || ctrl.Type != frame.ControlType_TerminalWindowSize {
		return 0, 0, false
	}
	ws := ctrl.TerminalWindowSize()
	if ws == nil {
		return 0, 0, false
	}
	return int(ws.Columns), int(ws.Rows), true
}

// RenderAsciicast renders a record as asciicast v2, the format `asciinema
// play` and its web player read: a header line, then one JSON array per event
// — "o" for output (Synth included: it is what the screen showed), "r" for a
// size change. Times are seconds from the first entry and never go back (a
// server clock step back would otherwise reverse playback). A UTF-8 character
// split across two frames is carried into the next event rather than broken
// in two; a byte invalid on its own becomes U+FFFD, as asciinema does.
func RenderAsciicast(entries []protocol.RecordEntry) ([]byte, error) {
	width, height := 80, 24
	for _, e := range entries {
		if c, r, ok := recordSize(e); ok && c > 0 && r > 0 {
			width, height = c, r
			break
		}
	}
	var start int64
	if len(entries) > 0 {
		start = int64(entries[0].TsNs)
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(map[string]any{
		"version": 2, "width": width, "height": height, "timestamp": start / 1e9,
	}); err != nil {
		return nil, err
	}
	var last float64
	var carry []byte
	event := func(ts uint64, code, data string) error {
		t := math.Round(float64(int64(ts)-start)/1e9*1e6) / 1e6
		if t < last {
			t = last
		}
		last = t
		var js bytes.Buffer
		je := json.NewEncoder(&js)
		je.SetEscapeHTML(false) // terminal output, not HTML: keep < > & as they are
		if err := je.Encode(data); err != nil {
			return err
		}
		out.WriteString("[" + strconv.FormatFloat(t, 'f', -1, 64) + `,"` + code + `",`)
		out.Write(bytes.TrimSuffix(js.Bytes(), []byte("\n")))
		out.WriteString("]\n")
		return nil
	}
	for _, e := range entries {
		if c, r, ok := recordSize(e); ok {
			if err := event(e.TsNs, "r", fmt.Sprintf("%dx%d", c, r)); err != nil {
				return nil, err
			}
			continue
		}
		if !isOutput(e, true) {
			continue
		}
		data := append(carry, e.Payload...)
		carry = nil
		if cut := incompleteTail(data); cut < len(data) {
			carry = append([]byte(nil), data[cut:]...)
			data = data[:cut]
		}
		if len(data) == 0 {
			continue
		}
		if err := event(e.TsNs, "o", strings.ToValidUTF8(string(data), "�")); err != nil {
			return nil, err
		}
	}
	if len(carry) > 0 && len(entries) > 0 {
		if err := event(entries[len(entries)-1].TsNs, "o", strings.ToValidUTF8(string(carry), "�")); err != nil {
			return nil, err
		}
	}
	return out.Bytes(), nil
}

// incompleteTail returns where a trailing, not yet complete UTF-8 sequence
// starts (len(b) when there is none): the bytes from there may be completed
// by the next frame.
func incompleteTail(b []byte) int {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax+1; i-- {
		if !utf8.RuneStart(b[i]) {
			continue
		}
		if !utf8.FullRune(b[i:]) {
			return i
		}
		return len(b)
	}
	return len(b)
}

// ParseRecordRef reads `<task-id>` (the latest run, 0) or `<task-id>#<n>`.
func ParseRecordRef(s string) (taskIDHex string, run uint32, err error) {
	id, n, hasRun := strings.Cut(s, "#")
	if raw, err := hex.DecodeString(id); err != nil || len(raw) != 16 {
		return "", 0, fmt.Errorf("record %q: want <task-id> or <task-id>#<run> with a 32-hex task id", s)
	}
	if !hasRun {
		return id, 0, nil
	}
	v, err := strconv.ParseUint(n, 10, 32)
	if err != nil || v == 0 {
		return "", 0, fmt.Errorf("record %q: the run after # is a number from 1", s)
	}
	return id, uint32(v), nil
}

func frameTypeIsSynth(e protocol.RecordEntry) bool {
	return frame.FrameType(e.FrameType) == frame.FrameType_Synth
}
