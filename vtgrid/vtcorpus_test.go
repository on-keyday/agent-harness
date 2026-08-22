package vtgrid

// Captured PTY byte streams and a measurement of what they exercise.
//
// These exist because any change to how we render a session screen — a
// replay-trimming heuristic, a different emulator, a purpose-built grid model —
// has to be checked against real terminal output rather than hand-written
// escapes. A corpus makes that checkable offline: no runner, no session, no
// live agent.
//
// Provenance (which session, which size, what was running) is in
// testdata/vtcorpus/README.md. Each file is the TAIL of a `session snapshot
// --raw` capture, so it begins mid-sequence exactly as a ring replay does.

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
	"unicode/utf8"
)

// vtCorpus names a stored capture and the PTY size it was produced at. The
// size is not decoration: a grid render is only meaningful at the width the
// app wrapped its output for, and a differential check must feed both
// implementations the same one.
type vtCorpus struct {
	Name       string
	Rows, Cols int
	What       string
}

var vtCorpora = []vtCorpus{
	{"agy-start", 40, 150, "Antigravity's first bytes: a full private-mode reset sweep, Kitty keyboard push/pop (CSI = u), DECSCUSR, and the alt-screen exit that truncates its own replay"},
	{"agy-tui", 40, 150, "Antigravity CLI (Gemini): banner, a permission prompt, and an interrupt"},
	{"altscreen", 40, 150, "shell inside the alternate screen, captured before it exited"},
	{"bash-scroll", 40, 150, "`seq 1 200000` — scrolled short lines, no escapes"},
	{"claude-start", 40, 150, "Claude Code's first bytes, ending on the trust dialog: DECSTBM reset (ESC 7 / ESC [ r / ESC 8), bracketed paste, focus and colour-scheme notification modes"},
	{"claude-tui", 40, 150, "a live Claude Code session answering arithmetic: prompt box, spinner, relative cursor motion"},
	{"codex-start", 40, 150, "the richest startup handshake here: DSR cursor query, OSC 10/11 colour queries, DA1, Kitty keyboard query, and 12 synchronized-output brackets"},
	{"codex-tui", 40, 150, "OpenAI Codex answering arithmetic: boxed panels and a bordered composer"},
	{"conpty-ssh", 36, 173, "bash reached over ssh from Windows cmd.exe — the bytes pass through ConPTY"},
	{"herdr-tui", 36, 173, "the herdr multiplexer repainting a pane that scrolls colored text"},
	{"htop", 40, 150, "htop filtered to root processes, captured while still inside the alternate screen: colour meters, tree view, 0.3s repaint"},
	{"opencode-tui", 40, 150, "opencode driven by keystrokes only (no provider configured): command palette, tab switching, input editing"},
	{"pwsh", 40, 150, "PowerShell 7 as the interactive shell: PSReadLine syntax highlighting keystroke by keystroke, tab-completion cycling, Write-Progress"},
	{"torture", 40, 150, "deliberate coverage: SGR, CJK, DECSTBM, IL/DL/ICH/DCH, tabs, autowrap"},
	{"win-start", 40, 150, "the first 296 bytes a fresh Windows session ever emits: ConPTY's attach preamble, Win32 Input Mode, and the cmd banner"},
	{"win-cmd", 40, 150, "native cmd.exe on Windows: dir/tree/ver, console colour changes and a PowerShell progress bar, all translated to VT by ConPTY"},
	{"vim-split", 40, 150, "vim with a vertical split, scrolled with ^F/^B and j"},
}

// localIdentifiers match the SHAPES that carry an environment into a capture —
// never the specific names of any one.
//
// Spelling out real hostnames and usernames here would publish exactly what the
// rule exists to withhold: in a public repository the deny-list IS the
// disclosure. Shapes avoid that, and they generalise — they catch the next
// machine, the next operator, and the fragments a TUI leaves behind when cursor
// motion splits a path across two writes.
//
// The last entry names fields instead of values because their contents are
// base64: an address inside one survives any text substitution, so a capture
// carrying it has to be re-taken rather than scrubbed.
// ipOctet bounds a dotted-quad component to 0-255. Writing \d{1,3} instead
// looks equivalent and is not: a Windows build number is four dot-separated
// numbers starting with 10, and `10.0.26200.9168` matched the loose form. A
// guard that cries wolf on a version banner is worse than none, because the
// next person weakens it.
const ipOctet = `(?:25[0-5]|2[0-4]\d|[01]?\d?\d)`

var localIdentifiers = []struct {
	What string
	Re   *regexp.Regexp
}{
	{"a private-range IPv4 address", regexp.MustCompile(
		`\b(?:10\.` + ipOctet + `\.` + ipOctet + `|192\.168\.` + ipOctet +
			`|172\.(?:1[6-9]|2\d|3[01])\.` + ipOctet + `)\.` + ipOctet + `\b`)},
	{"a user@host prompt or window title", regexp.MustCompile(`\b[a-z_][a-z0-9_-]{1,31}@[a-z0-9][a-z0-9.-]{1,63}\b`)},
	{"a home-directory path", regexp.MustCompile(`(?:/home/|/Users/|\\Users\\)[A-Za-z0-9_.-]+`)},
	{"a runner connection id", regexp.MustCompile(`\bws:[0-9.]+:\d+-\d+`)},
	{"a field whose value encodes an address", regexp.MustCompile(`HARNESS_AUTH_TICKET|selector_b64|bound_runner_id`)},
}

// loadVTCorpus decompresses one stored capture.
func loadVTCorpus(tb testing.TB, name string) []byte {
	tb.Helper()
	f, err := os.Open(filepath.Join("testdata", "vtcorpus", name+".raw.gz"))
	if err != nil {
		tb.Fatalf("open corpus %s: %v", name, err)
	}
	defer f.Close() //nolint:errcheck // read-only
	zr, err := gzip.NewReader(f)
	if err != nil {
		tb.Fatalf("gunzip %s: %v", name, err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		tb.Fatalf("read %s: %v", name, err)
	}
	return b
}

// vtSeqCounts is what a byte stream actually contains, by sequence kind. CSI
// finals are keyed with their private marker ("?h" is not "h"), because that
// distinction is the whole difference between a mode set and a cursor move.
type vtSeqCounts struct {
	CSI       map[string]int
	Esc       map[string]int
	OSC       map[string]int
	DCS       int
	OtherStr  int // APC / PM / SOS
	C0        map[byte]int
	NonASCII  int
	Printable int
}

// countVTSequences walks a terminal byte stream and tallies what it holds. It
// is a classifier, not an emulator: it recognises sequence *boundaries* and
// their identifying bytes, which is exactly what a coverage claim needs and
// nothing more.
func countVTSequences(b []byte) vtSeqCounts {
	c := vtSeqCounts{
		CSI: map[string]int{}, Esc: map[string]int{},
		OSC: map[string]int{}, C0: map[byte]int{},
	}
	const (
		ground = iota
		esc
		csi
		str
		strEsc
	)
	st := ground
	var params []byte
	var strIntro byte
	for i := 0; i < len(b); {
		ch := b[i]
		switch st {
		case ground:
			switch {
			case ch == 0x1b:
				st, params = esc, params[:0]
			case ch < 0x20 || ch == 0x7f:
				c.C0[ch]++
			default:
				r, size := utf8.DecodeRune(b[i:])
				if r >= 0x80 {
					c.NonASCII++
				}
				c.Printable++
				i += size
				continue
			}
		case esc:
			switch {
			case ch == '[':
				st, params = csi, params[:0]
			case ch == ']' || ch == 'P' || ch == 'X' || ch == '^' || ch == '_':
				st, strIntro, params = str, ch, params[:0]
			case ch == 0x1b:
				// ESC ESC — keep waiting for the intro byte.
			default:
				c.Esc[string(ch)]++
				st = ground
			}
		case csi:
			switch {
			case ch >= 0x40 && ch <= 0x7e:
				key := string(ch)
				if len(params) > 0 && (params[0] == '?' || params[0] == '>' || params[0] == '<' || params[0] == '=') {
					key = string(params[0]) + key
				}
				c.CSI[key]++
				st = ground
			default:
				params = append(params, ch)
			}
		case str:
			switch {
			case ch == 0x07 || ch == 0x9c:
				c.recordString(strIntro, params)
				st = ground
			case ch == 0x1b:
				st = strEsc
			default:
				if len(params) < 32 {
					params = append(params, ch)
				}
			}
		case strEsc:
			if ch == '\\' {
				c.recordString(strIntro, params)
				st = ground
			} else {
				st = str
			}
		}
		i++
	}
	return c
}

// recordString files a completed string sequence. OSC is keyed by its command
// number, which is the part that says what it meant; the rest are counted in
// bulk because we do not act on them.
func (c *vtSeqCounts) recordString(intro byte, params []byte) {
	if intro != ']' {
		if intro == 'P' {
			c.DCS++
		} else {
			c.OtherStr++
		}
		return
	}
	cmd := "?"
	for i, ch := range params {
		if ch == ';' {
			cmd = string(params[:i])
			break
		}
		if ch < '0' || ch > '9' {
			break
		}
		if i == len(params)-1 {
			cmd = string(params)
		}
	}
	c.OSC[cmd]++
}

// TestVTCorpusLoads is the always-on guard: every declared corpus must be
// present and decompress to something non-trivial. A silently-missing corpus
// would turn a future differential test into a test that checks nothing.
func TestVTCorpusLoads(t *testing.T) {
	for _, c := range vtCorpora {
		b := loadVTCorpus(t, c.Name)
		// The floor is low because one corpus legitimately is: a session's
		// startup preamble is a few hundred bytes and complete at that size.
		// Everything else here is a 256 KiB tail.
		if len(b) < 256 {
			t.Errorf("%s: only %d bytes — corpus looks truncated", c.Name, len(b))
		}
		if !bytes.Contains(b, []byte{0x1b}) && c.Name != "bash-scroll" {
			t.Errorf("%s: no ESC anywhere — is this really terminal output?", c.Name)
		}
	}
}

// TestVTCorpusNoLocalIdentifiers is the reason a screen capture can live in a
// public repository at all. A corpus is a photograph of somebody's terminal;
// whatever was on it — prompts, paths, log lines — came along. This fails the
// build rather than trusting that whoever adds the next one remembered.
func TestVTCorpusNoLocalIdentifiers(t *testing.T) {
	for _, c := range vtCorpora {
		b := loadVTCorpus(t, c.Name)
		for _, id := range localIdentifiers {
			if m := id.Re.Find(b); m != nil {
				t.Errorf("%s: holds %s (%q) — re-capture it from neutral content, or "+
					"substitute a SAME-LENGTH placeholder that does not match this shape "+
					"(a shorter one would move every cell after it; an exemption list "+
					"would be a hole)",
					c.Name, id.What, m)
			}
		}
	}
}

// TestVTCorpusCoverage prints what each corpus exercises. It asserts nothing:
// the point is to be able to answer "does anything we have actually emit
// DECSTBM / OSC 8 / DCS" with a measurement instead of a guess, before
// deciding what a renderer must implement.
func TestVTCorpusCoverage(t *testing.T) {
	total := vtSeqCounts{CSI: map[string]int{}, Esc: map[string]int{}, OSC: map[string]int{}, C0: map[byte]int{}}
	for _, c := range vtCorpora {
		n := countVTSequences(loadVTCorpus(t, c.Name))
		t.Logf("%-12s %dx%-4d  printable=%-8d non-ascii=%-6d CSI=%-6d OSC=%-4d DCS=%-3d other-str=%d",
			c.Name, c.Rows, c.Cols, n.Printable, n.NonASCII, sumCounts(n.CSI), sumCounts(n.OSC), n.DCS, n.OtherStr)
		t.Logf("%-12s   CSI: %s", "", topKeys(n.CSI, 14))
		if len(n.OSC) > 0 {
			t.Logf("%-12s   OSC: %s", "", topKeys(n.OSC, 8))
		}
		if len(n.Esc) > 0 {
			t.Logf("%-12s   ESC: %s", "", topKeys(n.Esc, 8))
		}
		mergeCounts(total.CSI, n.CSI)
		mergeCounts(total.OSC, n.OSC)
		mergeCounts(total.Esc, n.Esc)
	}
	t.Logf("")
	t.Logf("union CSI finals: %s", topKeys(total.CSI, 64))
	t.Logf("union OSC cmds  : %s", topKeys(total.OSC, 32))
	t.Logf("union ESC finals: %s", topKeys(total.Esc, 32))
}

func sumCounts(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

func mergeCounts(dst, src map[string]int) {
	for k, v := range src {
		dst[k] += v
	}
}

// topKeys renders the n most frequent entries as "key×count", most frequent
// first — a stable projection so two runs of the same corpus read the same.
func topKeys(m map[string]int, n int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if len(keys) > n {
		keys = keys[:n]
	}
	var out []byte
	for i, k := range keys {
		if i > 0 {
			out = append(out, ' ')
		}
		out = append(out, k...)
		out = append(out, 0xc3, 0x97) // ×
		out = appendInt(out, m[k])
	}
	if len(out) == 0 {
		return "(none)"
	}
	return string(out)
}

func appendInt(b []byte, n int) []byte {
	if n == 0 {
		return append(b, '0')
	}
	var tmp [20]byte
	i := len(tmp)
	for n > 0 {
		i--
		tmp[i] = byte('0' + n%10)
		n /= 10
	}
	return append(b, tmp[i:]...)
}

// TestLocalIdentifierPatterns checks the guard against inputs that must trip it
// and inputs that must not. A privacy guard is code like any other: an
// over-broad pattern is not "safe", it is a false alarm that teaches whoever
// meets it to relax the rule.
func TestLocalIdentifierPatterns(t *testing.T) {
	mustFlag := []string{
		"connect 192.168.3.14 ok",
		"inside 10.1.2.3 subnet",
		"172.16.0.1 and 172.31.255.254",
		"user@somehost:~$ ",
		"cd /home/someone/work",
		"C:\\Users\\someone\\Desktop",
		`"bound_runner_id":"ws:1.2.3.4:5-6"`,
		"HARNESS_AUTH_TICKET=deadbeef",
	}
	mustNotFlag := []string{
		"Microsoft Windows [Version 10.0.26200.9168]", // build number, not an address
		"172.32.0.1 and 172.15.0.1",                   // outside the private 172.16/12 block
		"10.0.300.1",                                  // 300 is not an octet
		"go1.25.7 linux/amd64",
		"C:\\Windows\\System32\\drivers",
		"see https://example.com/a/b",
	}
	flags := func(s string) (string, bool) {
		for _, id := range localIdentifiers {
			if m := id.Re.FindString(s); m != "" {
				return id.What + " " + m, true
			}
		}
		return "", false
	}
	for _, s := range mustFlag {
		if _, ok := flags(s); !ok {
			t.Errorf("guard missed %q", s)
		}
	}
	for _, s := range mustNotFlag {
		if what, ok := flags(s); ok {
			t.Errorf("guard false-positive on %q: matched %s", s, what)
		}
	}
}
