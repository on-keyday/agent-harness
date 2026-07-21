package server

// vtGroundScan is an incremental, allocation-free scanner that tracks whether a
// terminal output byte stream is at a VT "ground state" boundary — i.e. NOT in
// the middle of an escape, CSI, or string (OSC/DCS/SOS/PM/APC) sequence. State
// persists across scan() calls so a sequence split across frames is recognised
// at the boundary.
//
// It exists so capReplayTail can crop the replay ring only at boundaries where a
// fresh client emulator would resync cleanly. Cropping at a mid-sequence
// boundary makes the emulator render the sequence's TAIL as printable text — the
// classic symptom being a window-title OSC (ESC]0;…BEL) whose head was evicted
// with the dropped frames, leaving its trailing characters bleeding into the
// pane (see TestCapReplayTail_NoOSCLeak).
//
// Unlike modeTracker (which only recognises CSI private-mode set/reset), this
// scanner must recognise string sequences and their BEL / ST terminators,
// because those are exactly the long sequences a tail crop cuts through.
type vtGroundScan struct {
	st gsState
}

type gsState int

const (
	gsGround   gsState = iota // between sequences — safe to crop here
	gsEsc                     // saw ESC
	gsEscInt                  // saw ESC + intermediate byte(s) (0x20-0x2f)
	gsCSI                     // inside a CSI sequence, awaiting its final byte
	gsStr                     // inside an OSC/DCS/SOS/PM/APC string
	gsStrEsc                  // inside a string, saw ESC (awaiting '\' for ST)
)

// ground reports whether the scanner is at a sequence boundary.
func (s *vtGroundScan) ground() bool { return s.st == gsGround }

// scan advances over one chunk of output bytes.
func (s *vtGroundScan) scan(b []byte) {
	for _, c := range b {
		// CAN / SUB abort any in-progress sequence, returning to ground.
		if c == 0x18 || c == 0x1a {
			s.st = gsGround
			continue
		}
		switch s.st {
		case gsGround:
			if c == 0x1b {
				s.st = gsEsc
			}
		case gsEsc:
			switch {
			case c == '[':
				s.st = gsCSI
			case c == ']' || c == 'P' || c == 'X' || c == '^' || c == '_':
				s.st = gsStr // OSC / DCS / SOS / PM / APC
			case c == 0x1b:
				// ESC ESC: stay, awaiting the intro byte.
			case c >= 0x20 && c <= 0x2f:
				s.st = gsEscInt // intermediate byte(s) follow
			default:
				s.st = gsGround // 2-byte escape (e.g. ESC c, ESC 7) complete
			}
		case gsEscInt:
			switch {
			case c >= 0x20 && c <= 0x2f:
				// more intermediates
			case c == 0x1b:
				s.st = gsEsc
			default:
				s.st = gsGround // final byte consumed
			}
		case gsCSI:
			switch {
			case c >= 0x40 && c <= 0x7e:
				s.st = gsGround // final byte
			case c == 0x1b:
				s.st = gsEsc // resync on a fresh ESC
			default:
				// parameter / intermediate byte — stay in CSI
			}
		case gsStr:
			switch c {
			case 0x07: // BEL terminates OSC/string
				s.st = gsGround
			case 0x9c: // 8-bit ST
				s.st = gsGround
			case 0x1b:
				s.st = gsStrEsc
			default:
				// string content — stay
			}
		case gsStrEsc:
			switch c {
			case '\\': // ESC '\' = 7-bit ST, terminates the string
				s.st = gsGround
			case 0x1b:
				// ESC ESC inside a string — stay, awaiting '\'
			default:
				s.st = gsStr // ESC was part of the string data
			}
		}
	}
}
