package streamagent

import (
	"fmt"

	"github.com/on-keyday/agent-harness/runner/agentlog"
)

// FromAgentlog maps agentlog's Event onto the wire type; ToAgentlog below is
// its inverse, and the round trip is asserted in render_test.go. Kept as an explicit
// switch rather than an int cast so adding a Kind on either side is a compile
// error rather than a silently mislabelled event.
func FromAgentlog(e agentlog.Event) Event {
	out := Event{
		Text: e.Text, Tool: e.Tool, Args: e.Args, Result: e.Result,
		ExitCode: e.ExitCode, IsError: e.IsError, Warning: e.Warning,
	}
	switch e.Kind {
	case agentlog.KindRaw:
		out.Kind = EventRaw
	case agentlog.KindSessionStart:
		out.Kind = EventSessionStart
	case agentlog.KindThinking:
		out.Kind = EventThinking
	case agentlog.KindToolStart:
		out.Kind = EventToolStart
	case agentlog.KindToolEnd:
		out.Kind = EventToolEnd
	case agentlog.KindText:
		out.Kind = EventText
	case agentlog.KindUserText:
		out.Kind = EventUserText
	case agentlog.KindFinish:
		out.Kind = EventFinish
	case agentlog.KindError:
		out.Kind = EventError
	default:
		out.Kind = EventRaw
	}
	if s := e.Stats; s != (agentlog.Stats{}) {
		out.Stats = &Stats{
			DurationMS: s.DurationMS, CostUSD: s.CostUSD,
			InputTokens: s.InputTokens, OutputTokens: s.OutputTokens,
		}
	}
	return out
}

// ToAgentlog is FromAgentlog inverted: a wire Event back into agentlog's, so a
// neutral event renders through the SAME agentlog.Render everywhere — the
// runner's task-log tap and the CLI's `session stream attach` both call it,
// and the oneshot kind's log lines are produced by that Render too, so the
// three surfaces cannot drift into three renderings of one event. The round
// trip (agentlog → neutral → agentlog) is asserted in render_test.go;
// if it were lossy, this kind's rendering would quietly say less.
func (e Event) ToAgentlog() agentlog.Event {
	out := agentlog.Event{
		Text: e.Text, Tool: e.Tool, Args: e.Args, Result: e.Result,
		ExitCode: e.ExitCode, IsError: e.IsError, Warning: e.Warning,
	}
	switch e.Kind {
	case EventSessionStart:
		out.Kind = agentlog.KindSessionStart
	case EventThinking:
		out.Kind = agentlog.KindThinking
	case EventToolStart:
		out.Kind = agentlog.KindToolStart
	case EventToolEnd:
		out.Kind = agentlog.KindToolEnd
	case EventText:
		out.Kind = agentlog.KindText
	case EventUserText:
		out.Kind = agentlog.KindUserText
	case EventFinish:
		out.Kind = agentlog.KindFinish
	case EventError:
		out.Kind = agentlog.KindError
	default:
		out.Kind = agentlog.KindRaw
	}
	if e.Stats != nil {
		out.Stats = agentlog.Stats{
			DurationMS: e.Stats.DurationMS, CostUSD: e.Stats.CostUSD,
			InputTokens: e.Stats.InputTokens, OutputTokens: e.Stats.OutputTokens,
		}
	}
	return out
}

// ReplayPrefix marks a replayed event's display line. webui/static/main.js
// chatRenderEvent writes the same prefix.
const ReplayPrefix = "↺ "

// RenderText is the one-line human rendering of an adapter→client message,
// shared by the runner's task-log tap and the CLI's stream attach so the two
// cannot drift. ok=false means the kind has no standalone display line here
// (hello, exit, and every client→adapter kind) — those are context-dependent
// and each surface words its own.
func RenderText(m Msg) (line string, ok bool) {
	switch m.Kind {
	case KindEvent:
		if m.Event == nil {
			return "", false
		}
		line := agentlog.Render(m.Event.ToAgentlog())
		if m.Event.Replay {
			line = ReplayPrefix + line
		}
		return line, true
	case KindRequest:
		if m.Request == nil {
			return "", false
		}
		return fmt.Sprintf("⏸ approval needed: %s (%s)", m.Request.Tool, m.Request.ID), true
	}
	return "", false
}
