package cli

import (
	"encoding/hex"
	"fmt"
	"reflect"
	"time"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// ScheduleForm is a schedule as the creation forms hold it (the WebUI's
// Compose schedule tab, the TUI's schedule form): strings in the spellings the
// schedule verbs take. It is what an edit opens with and what it compares
// against, so a screen edits a schedule with the form it was made with.
type ScheduleForm struct {
	Cron      string   `json:"cron"`
	Tz        string   `json:"tz"`
	Repo      string   `json:"repo"`
	Task      string   `json:"task"` // the bound task (32 hex), "" when none yet
	Host      string   `json:"host"` // the runner pinned by hostname, "" for any (or a pin the form cannot show)
	Agent     string   `json:"agent"`
	AgentArgs []string `json:"agentArgs"`
	Prompt    string   `json:"prompt"`
	// PromptHidden: the editor could not read the bound task's prompt; Prompt
	// is then "" and is sent only when the editor types a new one.
	PromptHidden       bool     `json:"promptHidden"`
	Kind               string   `json:"kind"`   // interactive | oneshot | stream
	Record             string   `json:"record"` // on | off | "" (the server's default)
	Pty                string   `json:"pty"`    // on | off | "" (the mode's default)
	ResumeConversation bool     `json:"resumeConversation"`
	Deadline           string   `json:"deadline"` // durations: "" means none
	TurnEnd            string   `json:"turnEnd"`
	Quiet              string   `json:"quiet"`
	Blocked            string   `json:"blocked"`
	QueueTimeout       string   `json:"queueTimeout"`
	Caps               string   `json:"caps"`
	Scope              string   `json:"scope"`
	ScopeFor           []string `json:"scopeFor"`
	GrantConditions    []string `json:"grantConditions"`
	DelegateCaps       string   `json:"delegateCaps"`
	DelegateScope      string   `json:"delegateScope"`
	SpawnDepth         string   `json:"spawnDepth"`
	MaxDescendants     string   `json:"maxDescendants"`
}

func formDuration(ms uint64) string {
	if ms == 0 {
		return ""
	}
	return (time.Duration(ms) * time.Millisecond).String()
}

func formTri(set, v bool) string {
	switch {
	case !set:
		return ""
	case v:
		return "on"
	}
	return "off"
}

// ScheduleFormOf is a stored schedule as its form.
func ScheduleFormOf(d protocol.ScheduleDetail) ScheduleForm {
	f := ScheduleForm{
		Cron: string(d.Row.Cron), Tz: string(d.Row.Tz), Repo: string(d.Row.Repo), Agent: string(d.Row.Agent),
		AgentArgs: append([]string{}, d.ExtraArgs.AsStrings()...),
		Prompt:    string(d.Prompt), PromptHidden: d.Row.PromptHidden(),
		Kind:   ScheduleKindWord(d.Kind),
		Record: formTri(d.RecordSet(), d.Record()), Pty: formTri(d.PtySet(), d.Pty()),
		ResumeConversation: d.ResumeConversation(),
		Deadline:           formDuration(d.Conditions.DeadlineMs), TurnEnd: EnumWireName(d.Conditions.TurnEnd.String()),
		Quiet: formDuration(d.Conditions.QuietMs), Blocked: formDuration(d.Conditions.BlockedMs),
		QueueTimeout: formDuration(d.Conditions.QueueTimeoutMs),
		Caps:         CapsLabel(d.Caps), Scope: ScopeLabel(d.Scope),
		ScopeFor:        []string{},
		GrantConditions: append([]string{}, GrantConditionSpecs(d.Overrides, d.GrantConditions)...),
		DelegateCaps:    CapsLabel(d.DelegateCaps), DelegateScope: ScopeLabel(d.DelegateScope),
		SpawnDepth: authority.LimitLabel(d.SpawnDepth), MaxDescendants: authority.LimitLabel(d.MaxDescendants),
	}
	if d.Row.TaskState == protocol.ScheduleTaskState_Bound {
		f.Task = hex.EncodeToString(d.Row.TaskId.Id[:])
	}
	if d.Selector.Kind == protocol.RunnerSelectorKind_ByHostname {
		if v := d.Selector.Hostname(); v != nil {
			f.Host = string(v.Name)
		}
	}
	for _, o := range d.Overrides {
		f.ScopeFor = append(f.ScopeFor, OverridesLabel([]protocol.ScopeOverride{o}))
	}
	return f
}

// ScheduleEditArgs is `schedule edit`'s argument line for the fields that
// differ between was (the form as opened) and now (as saved): an edit keeps
// whatever is not named, so the unchanged are left out and what was turned off
// or emptied is said explicitly. The line is parsed by the verb's own
// declaration, so the form adds no second request builder. What the verb has
// no spelling for is refused rather than silently kept.
func ScheduleEditArgs(id string, was, now ScheduleForm) ([]string, error) {
	args := []string{id}
	str := func(flag, a, b string) {
		if a != b {
			args = append(args, flag, b)
		}
	}
	str("--cron", was.Cron, now.Cron)
	str("--tz", was.Tz, now.Tz)
	str("--agent", was.Agent, now.Agent)
	str("--turn-end", was.TurnEnd, now.TurnEnd)
	if now.Prompt != was.Prompt && !(now.PromptHidden && now.Prompt == "") {
		args = append(args, "--prompt", now.Prompt)
	}
	if now.Task != was.Task {
		if was.Task != "" {
			return nil, fmt.Errorf("task: a schedule bound to a task stays bound to it; make a new schedule for another")
		}
		args = append(args, "--task", now.Task)
	}
	if now.Host != was.Host {
		if now.Host == "" {
			args = append(args, "--runner", "any")
		} else {
			args = append(args, "--host", now.Host)
		}
	}
	if !reflect.DeepEqual(nonNil(was.AgentArgs), nonNil(now.AgentArgs)) {
		if len(now.AgentArgs) == 0 {
			args = append(args, "--no-agent-args")
		}
		for _, a := range now.AgentArgs {
			args = append(args, "--agent-arg", a)
		}
	}
	for _, d := range []struct{ flag, a, b string }{
		{"--deadline", was.Deadline, now.Deadline}, {"--quiet", was.Quiet, now.Quiet},
		{"--blocked", was.Blocked, now.Blocked}, {"--queue-timeout", was.QueueTimeout, now.QueueTimeout},
	} {
		if d.a != d.b {
			v := d.b
			if v == "" {
				v = "0" // 0 means none
			}
			args = append(args, d.flag, v)
		}
	}
	if now.Kind != was.Kind {
		switch now.Kind {
		case "interactive":
			args = append(args, "--oneshot=false")
		case "oneshot":
			args = append(args, "--oneshot")
		case "stream":
			args = append(args, "--stream")
		default:
			return nil, fmt.Errorf("kind %q: want interactive, oneshot or stream", now.Kind)
		}
	}
	for _, t := range []struct{ name, on, off, a, b string }{
		{"record", "--record", "--no-record", was.Record, now.Record},
		{"pty", "--pty", "--no-pty", was.Pty, now.Pty},
	} {
		if t.a == t.b {
			continue
		}
		switch t.b {
		case "on":
			args = append(args, t.on)
		case "off":
			args = append(args, t.off)
		default:
			return nil, fmt.Errorf("%s: an edit cannot return it to the default; choose on or off", t.name)
		}
	}
	if now.ResumeConversation != was.ResumeConversation {
		args = append(args, fmt.Sprintf("--resume-conversation=%t", now.ResumeConversation))
	}
	str("--caps", was.Caps, now.Caps)
	// --scope replaces the overrides and their conditions together, so any
	// change to the three sends all three.
	if now.Scope != was.Scope || !reflect.DeepEqual(nonNil(was.ScopeFor), nonNil(now.ScopeFor)) ||
		!reflect.DeepEqual(nonNil(was.GrantConditions), nonNil(now.GrantConditions)) {
		args = append(args, "--scope", now.Scope)
		for _, s := range now.ScopeFor {
			args = append(args, "--scope-for", s)
		}
		for _, g := range now.GrantConditions {
			args = append(args, "--grant-condition", g)
		}
	}
	str("--delegate-caps", was.DelegateCaps, now.DelegateCaps)
	str("--delegate-scope", was.DelegateScope, now.DelegateScope)
	str("--spawn-depth", was.SpawnDepth, now.SpawnDepth)
	str("--max-descendants", was.MaxDescendants, now.MaxDescendants)
	return args, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
