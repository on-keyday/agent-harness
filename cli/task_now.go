package cli

import (
	"strings"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/vtgrid"
)

// promptCellBytes caps the prompt text a now-text carries; a display clips it
// further to its own width.
const promptCellBytes = 140

// TaskNowText is what a task says it is doing now: its own progress line when
// it has one, else its program's window title marked "(title)" — a plain-text
// mark, because a table truncates cells by width and an ANSI style inside one
// can be cut mid-sequence — else the prompt cell. The TUI's About column and
// the overview both print it. It was named Doing until the operator objected:
// a title is generated from the first prompt and drifts from the work, so the
// header claimed something two of the three sources do not say (progress spec,
// D31).
//
// The line carries its age, which is how the reader judges a stale one (D4).
// Every branch strips control characters HERE: a table hands a cell's bytes to
// the terminal as they are, and a task event can be forged by any subscriber,
// so the server's own rules are not a substitute. The prompt branch used to
// map only \n \r \t, so an agent-written prompt reached the terminal with its
// ESC and C1 bytes.
func TaskNowText(t protocol.TaskInfo) string {
	if t.ProgressAt != 0 && len(t.Progress) > 0 {
		p := string(t.Progress)
		return vtgrid.SanitizeTitle(p, len(p)) + " (" + AgeStr(t.ProgressAgeMs) + ")"
	}
	if t.LiveTerminal() && len(t.Title) > 0 {
		ti := string(t.Title)
		return "(title) " + vtgrid.SanitizeTitle(ti, len(ti))
	}
	return promptCell(t)
}

// promptCell is the prompt-column string for a task. An interactive session
// shows "<interactive>" because its prompt is intentionally empty. The Kind
// field is the authoritative source — TaskStatusEvent carries it from the very
// first event, so a freshly-stubbed row knows its kind without needing the
// next List snapshot to disambiguate.
func promptCell(t protocol.TaskInfo) string {
	switch t.Kind {
	case protocol.TaskKind_Oneshot:
		if !t.NoPty() {
			return "(oneshot+pty) " + promptText(t.Prompt)
		}
		return "(oneshot) " + promptText(t.Prompt)
	case protocol.TaskKind_Interactive:
		if t.NoPty() {
			return "<interactive+pipe>"
		}
		return "<interactive>"
	case protocol.TaskKind_Stream:
		// Distinct from <interactive>: same "no prompt" reason, different
		// thing on the other end, and the row is where an operator first
		// notices which one they are looking at.
		return "<stream>"
	case protocol.TaskKind_Credential:
		return credentialAboutText(t)
	}
	return promptText(t.Prompt)
}

// promptText folds line breaks and tabs to spaces, drops every other control
// character, and cuts on a rune boundary.
func promptText(p []byte) string {
	folded := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, string(p))
	return vtgrid.SanitizeTitle(folded, promptCellBytes)
}
