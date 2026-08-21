package streamagent

import (
	"fmt"

	"github.com/on-keyday/agent-harness/runner/agentlog"
)

// ToAgentlog is toNeutral inverted: a wire Event back into agentlog's, so a
// neutral event renders through the SAME agentlog.Render everywhere — the
// runner's task-log tap and the CLI's `session stream attach` both call it,
// and the oneshot kind's log lines are produced by that Render too, so the
// three surfaces cannot drift into three renderings of one event. The round
// trip (agentlog → neutral → agentlog) is asserted in this package's tests;
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
		return agentlog.Render(m.Event.ToAgentlog()), true
	case KindRequest:
		if m.Request == nil {
			return "", false
		}
		return fmt.Sprintf("⏸ approval needed: %s (%s)", m.Request.Tool, m.Request.ID), true
	}
	return "", false
}
