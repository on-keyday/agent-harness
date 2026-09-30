package streamagent

import (
	"fmt"
	"strings"
)

// Tone is what a chat surface should make a line LOOK like, decided once here
// so the TUI (a lipgloss style) and the WebUI (a `c-<tone>` CSS class) cannot
// classify one message two ways. Each surface maps a Tone to its own paint and
// nothing else.
type Tone string

const (
	ToneText  Tone = "text"  // the agent's answer: the one unmuted thing
	ToneYou   Tone = "you"   // a turn the user wrote
	ToneMuted Tone = "muted" // activity and history: recedes
	ToneWarn  Tone = "warn"
	ToneErr   Tone = "err"
	ToneRaw   Tone = "raw" // a line that is not the protocol, shown escaped
)

// Display is how a chat surface shows one adapter→client message: the line to
// append (none when Text is empty), how it looks, and what it does to the
// surface's status line and busy state.
//
// It exists because the TUI chat and the WebUI chat each carried this decision
// by hand, and the two copies had started to disagree about replayed events:
// one left a replayed `thinking` driving the status line. A replayed event
// describes the past, so it never touches status or busy state.
//
// A request is NOT covered: both chats render its payload and choices in a
// surface-specific control, so what they owe it cannot be a line.
type Display struct {
	Text      string
	Tone      Tone
	SetStatus bool   // replace the status line with Status
	Status    string // "" with SetStatus clears it
	Idle      bool   // the agent is no longer running a turn
	// Resolves names a request this message settled. A chat holding that
	// request as pending drops it, however it was answered — from this chat,
	// from another client, or before this chat attached and replayed.
	Resolves string
}

// DisplayOf decides m's Display. ok=false means the message has no display of
// its own here (a request, or a client→adapter kind).
func DisplayOf(m Msg) (d Display, ok bool) {
	switch m.Kind {
	case KindHello:
		if m.Hello == nil {
			return Display{}, false
		}
		return Display{SetStatus: true,
			Status: fmt.Sprintf("attached · %s protocol %d", m.Hello.Vendor, m.Hello.Protocol)}, true
	case KindProgress:
		// The status line only: a heartbeat is not transcript. What completes
		// arrives as its own event and supersedes it.
		if m.Progress == nil {
			return Display{}, false
		}
		return Display{SetStatus: true, Status: ProgressStatus(*m.Progress)}, true
	case KindResolved:
		if m.Resolved == nil {
			return Display{}, false
		}
		text, _ := RenderText(m)
		return Display{Text: text, Tone: ToneMuted, Resolves: m.Resolved.ID}, true
	case KindExit:
		if m.Exit == nil {
			return Display{}, false
		}
		text, tone := RenderExit(*m.Exit)
		return Display{Text: text, Tone: tone, SetStatus: true, Status: "session ended", Idle: true}, true
	case KindEvent:
		if m.Event == nil {
			return Display{}, false
		}
		text, _ := RenderText(m)
		d := Display{Text: text, Tone: EventTone(m.Event)}
		if m.Event.Replay {
			return d, true
		}
		switch m.Event.Kind {
		case EventFinish:
			d.SetStatus, d.Status, d.Idle = true, "", true
		case EventThinking:
			d.SetStatus, d.Status = true, "thinking…"
		case EventToolStart:
			d.SetStatus, d.Status = true, "running "+m.Event.Tool+"…"
		}
		return d, true
	}
	return Display{}, false
}

// EventTone paints an event by what it MEANS, so the agent's answer stays
// primary and the machinery around it recedes. Replayed history recedes
// whatever it says: it is context for the next turn, not the turn in progress.
func EventTone(e *Event) Tone {
	if e == nil || e.Replay {
		return ToneMuted
	}
	switch e.Kind {
	case EventText:
		return ToneText
	case EventUserText:
		return ToneYou
	case EventError:
		if e.Warning {
			return ToneWarn
		}
		return ToneErr
	}
	return ToneMuted // session_start / thinking / tool_start / tool_end / finish / raw
}

// ProgressStatus is the one wording of a progress heartbeat, shared by both
// chats. The count is printed even at zero: "0 tokens" is a measurement, and
// an elided one would read as "not reported".
func ProgressStatus(p Progress) string {
	switch p.Phase {
	case PhaseThinking:
		if p.Chars > 0 {
			return fmt.Sprintf("thinking… %s tokens · %s chars", compactCount(p.Tokens), compactCount(p.Chars))
		}
		return fmt.Sprintf("thinking… %s tokens", compactCount(p.Tokens))
	case PhaseToolInput:
		return fmt.Sprintf("→ %s: writing input… %s chars", p.Tool, compactCount(p.Chars))
	default:
		return fmt.Sprintf("writing… %s chars", compactCount(p.Chars))
	}
}

// compactCount renders n as 340, 1.2k, 12k.
func compactCount(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 10000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%dk", n/1000)
	}
}

// RenderExit is the one wording of an agent's exit, shared by the chats and
// `session stream attach`.
func RenderExit(ex Exit) (string, Tone) {
	if ex.Err != "" {
		return fmt.Sprintf("agent exited: code=%d err=%s", ex.Code, ex.Err), ToneErr
	}
	return fmt.Sprintf("agent exited: code=%d", ex.Code), ToneMuted
}

// QuestionSummary is a question request's one-line form, for the task log and
// the chats' notice line: the first question, how many more, and the id.
func QuestionSummary(r Request) string {
	if len(r.Questions) == 0 {
		return ""
	}
	q := r.Questions[0]
	s := "❓ question: "
	if q.Header != "" {
		s += q.Header + ": "
	}
	s += q.Question
	if n := len(r.Questions) - 1; n > 0 {
		s += fmt.Sprintf(" (+%d more)", n)
	}
	return s + " (" + r.ID + ")"
}

// QuestionComplete reports whether answers settle every question of r, or a
// freeform reply stands in for all of them. It is what BOTH chats gate their
// send on, so neither can send a half-answered question the other would not.
func QuestionComplete(r Request, answers map[string][]string, reply string) bool {
	if strings.TrimSpace(reply) != "" {
		return true
	}
	for _, q := range r.Questions {
		if len(answers[q.Question]) == 0 {
			return false
		}
	}
	return len(r.Questions) > 0
}

// AnswerResponse is the one builder of a question's answer, keyed by question
// text. The adapter also accepts a header as the key, for a typed command line.
func AnswerResponse(id string, answers map[string][]string, reply string) Response {
	return Response{ID: id, Behavior: BehaviorAllow, Answers: answers, Reply: reply}
}
