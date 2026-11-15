package verb

import (
	"encoding/hex"
	"flag"
	"fmt"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The spawn grammar is built from one list. `submit` shared it with
// `session new` until A3 retired the oneshot kind; spawnKind is what is left
// of the per-verb differences.
type spawnKind int

const (
	spawnSessionNew spawnKind = iota
	// spawnSchedule is `schedule new` / `schedule edit` (pillar S): the run
	// and authority flags of a background `session new`, as the same Flag
	// rows, minus what a firing cannot carry.
	spawnSchedule
)

// stringList accumulates one entry per occurrence, in the order given, which
// is the order forwarded to the agent.
type stringList []string

func (l *stringList) String() string     { return fmt.Sprint([]string(*l)) }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// spawnFlags returns the flag set for one spawn verb.
func spawnFlags(k spawnKind) []Flag {
	repoHelp := "repo identifier; must match a runner-registered RepoPath verbatim"
	out := []Flag{
		{
			Name: "repo", Type: FlagString, Default: "", Field: "Repo", Help: repoHelp,
			// The ladder all three surfaces had their own version of. The
			// workspace tier reached only the CLI before this.
			Resolve: []Tier{
				{Env: "HARNESS_REPO_PATH"},
				{Workspace: "repo"},
				{SurfaceContext: true},
			},
		},
		{Name: "resume", Type: FlagString, Default: "", Field: "ResumeTaskID",
			Help: "task id (32 hex) to resume — the server reuses the id and worktree branch, so the agent's project key matches the previous run"},
		{Name: "resume-conversation", Type: FlagBool, Default: false, Field: "ResumeConversation",
			Help: "with --resume, also ask the runner to resume the agent's own conversation state"},
		{Name: "exact", Type: FlagBool, Default: false, Field: "Exact",
			Help: "with --resume: resume exactly as recorded — no new prompt, args, agent, caps or scope (needs only resume_exact)"},
		{Name: "caps", Type: FlagString, Default: "", Field: "Caps",
			FieldType: "*protocol.Capability", Convert: "parseCapsFlag", PresenceField: "CapsPresent",
			Help: "comma-separated capability names to grant (e.g. spawn,file_read / all / none)"},
		{Name: "scope", Type: FlagString, Default: "", Field: "Scope",
			FieldType: "*protocol.TaskScope", Convert: "parseScopeFlag",
			// The scope half is ONE unit: naming either --scope or --scope-for
			// makes it explicit, so a resume re-grants both together. Letting
			// --scope-for alone ride the session default's scope would write an
			// authority half typed and half inherited -- the TUI said so in
			// spawnAuthority's comment and derived presence that way, while the
			// CLI checked only "scope" and dropped a lone --scope-for on resume.
			PresenceField: "ScopePresent", PresenceAlso: "scope-for",
			Help: "which tasks those capabilities may target; default subtree (self + descendants)"},
		{Name: "agent", Type: FlagString, Default: "", Field: "Agent",
			Help: "agent profile name (empty = runner default; not to be confused with --agent-arg)"},
		{
			Name: "scope-for", Type: FlagString, Custom: scopeForValue, Field: "Overrides",
			FieldType: "[]protocol.ScopeOverride", Convert: "parseScopeForList",
			Help: "narrow ONE capability (or a comma-separated list of them) to a tighter scope than --scope",
		},
		// The delegation bound: what the new task may write onto the tasks IT
		// creates. Each half has its own presence, like --caps and --scope, so
		// a resume re-grants only the half it names.
		{Name: "delegate-caps", Type: FlagString, Default: "", Field: "DelegateCaps",
			FieldType: "*protocol.Capability", Convert: "parseCapsFlag", PresenceField: "DelegateCapsPresent",
			Help: "capabilities the new task may put into the tasks IT creates (default none)"},
		{Name: "delegate-scope", Type: FlagString, Default: "", Field: "DelegateScope",
			FieldType: "*protocol.TaskScope", Convert: "parseScopeFlag", PresenceField: "DelegateScopePresent",
			Help: "the most scope it may give them, in --scope's grammar (default subtree)"},
		// The spawn limits, each with its own presence like the bound.
		{Name: "spawn-depth", Type: FlagString, Default: "", Field: "SpawnDepth",
			FieldType: "*protocol.SpawnLimit", Convert: "parseSpawnLimitFlag", PresenceField: "SpawnDepthPresent",
			Help: "generations that may still be created below the new task: N or unbounded (default: one less than yours)"},
		{Name: "max-descendants", Type: FlagString, Default: "", Field: "MaxDescendants",
			FieldType: "*protocol.SpawnLimit", Convert: "parseSpawnLimitFlag", PresenceField: "MaxDescendantsPresent",
			Help: "tasks the new task's subtree may hold, ended ones included until pruned: N or unbounded (default unbounded)"},
		{
			Name: "agent-arg", Type: FlagString, Custom: argListValue, Field: "ExtraArgs",
			Help: "extra CLI arg to forward to the agent (repeatable; appended after the runner-global --agent-args)",
		},
		// Runner pinning. Absent from the TUI's submit before the migration.
		{Name: "runner", Type: FlagString, Default: "", Field: "Runner",
			Help: "pin to a specific runner by ConnectionID (the id= value from `ls`)"},
		{Name: "host", Type: FlagString, Default: "", Field: "Host", Help: "pin to a runner by hostname"},
		{Name: "ip", Type: FlagString, Default: "", Field: "IP", Help: "pin to a runner by IP address"},
	}
	// `session new`: a session can be detached, streamed, X11-forwarded and
	// sized.
	out = append(out,
		Flag{Name: "x11", Type: FlagBool, Default: false, Field: "X11",
			Help:            "forward X11: inject DISPLAY/XAUTHORITY so GUI apps render on your local X server",
			CmdlineSurfaces: CLI | TUI, SurfaceReason: x11SurfaceReason},
		// 10, not 0: the runner binds 127.0.0.1:6000+N, and ssh picks 10 by
		// convention precisely so a forward does not land on the runner's own
		// :0 X port.
		Flag{Name: "x11-display", Type: FlagUint, Default: uint(10), Field: "X11Display",
			Help:            "with --x11: the local display number (0..99)",
			CmdlineSurfaces: CLI | TUI, SurfaceReason: x11SurfaceReason},
		Flag{Name: "detach", Aliases: []string{"d"}, Type: FlagBool, Default: false, Field: "Detach",
			Help: "start the session and exit immediately (don't attach the terminal)"},
		Flag{Name: "stream", Type: FlagBool, Default: false, Field: "Stream",
			Help: "open an event-stream session (structured events, no PTY)"},
		// 40x120 by default (pillar A spec A-D7): a detached session nobody
		// attaches to is otherwise 0x0 for life, where some agents draw
		// nothing. 0 with --cols 0 still sends no size, for experiments.
		Flag{Name: "rows", Type: FlagUint, Default: uint(40), Field: "Rows", Help: "initial PTY rows of a detached/background open; --rows 0 --cols 0 sends no size (PTY 0x0)"},
		Flag{Name: "cols", Type: FlagUint, Default: uint(120), Field: "Cols", Help: "initial PTY columns of a detached/background open; --rows 0 --cols 0 sends no size (PTY 0x0)"},
		// Run mode (pillar A spec A-D1, A-D7, A-D15).
		Flag{Name: "background", Type: FlagBool, Default: false, Field: "Background",
			Help: "run unattended: queued until a runner is free, ended by its conditions (implies --detach; needs --prompt and --deadline, and --agent when new)"},
		Flag{Name: "foreground", Type: FlagBool, Default: false, Field: "Foreground",
			Help: "with --resume: resume a background task in foreground (without either, a resume keeps the task's run mode)"},
		Flag{Name: "prompt", Type: FlagString, Default: "", Field: "Prompt",
			Help: "the agent's first turn, in either run mode (at most 16384 bytes)"},
		Flag{Name: "deadline", Type: FlagDuration, Default: time.Duration(0), Field: "Deadline",
			Help: "background: the run is cancelled this long after it starts (required; no default)"},
		Flag{Name: "turn-end", Type: FlagString, Default: "", Field: "TurnEnd",
			Help: "background: which signal ends the run at a turn's end: off|quiet|lifecycle|program|any (default any; a stream session: lifecycle)"},
		Flag{Name: "quiet", Type: FlagDuration, Default: time.Duration(0), Field: "QuietFor",
			Help: "background: output silence that counts as a finished turn (default 5m)"},
		Flag{Name: "blocked", Type: FlagDuration, Default: time.Duration(0), Field: "BlockedFor",
			Help: "background: how long the agent may wait on a human before the run ends (default 5m)"},
		Flag{Name: "queue-timeout", Type: FlagDuration, Default: time.Duration(0), Field: "QueueTimeout",
			Help: "background: cancel the task if it is still queued this long after it was queued (default none)"},
		// The PTY record (pillar A spec, A-D22): neither = the server's
		// --record default; a resume does not reuse the previous run's.
		Flag{Name: "record", Type: FlagBool, Default: false, Field: "Record",
			Help: "record this run's PTY output on the server (`record get` reads it back; needs record_persist)"},
		Flag{Name: "no-record", Type: FlagBool, Default: false, Field: "NoRecord",
			Help: "do not record this run's PTY output, whatever the server's default"},
		// A3 (A-D29): the profile's print-mode argv in the PTY.
		Flag{Name: "oneshot", Type: FlagBool, Default: false, Field: "Oneshot",
			Help: "run the agent's one-shot (print-mode) argv in the session, which ends when it answers (needs --prompt; not with --stream)"},
		// A-D33: the PTY is its own axis. Neither = the mode's default, which
		// the server resolves: a PTY for an interactive open, none for
		// --oneshot; an exact resume keeps the task's recorded choice.
		Flag{Name: "pty", Type: FlagBool, Default: false, Field: "Pty",
			Help: "run the child in a PTY (an interactive open's default; with --oneshot, where the default is pipes: a pager env is set, and stdin is the terminal)"},
		Flag{Name: "no-pty", Type: FlagBool, Default: false, Field: "NoPty",
			Help: "run the child on pipes, no PTY (a --oneshot's default: stdin /dev/null, output byte for byte; interactive: stdin is your input, and nothing echoes it)"},
	)
	if k == spawnSchedule {
		return scheduleFlags(out)
	}
	return out
}

// scheduleOff are the spawn flags a schedule does not take, with why: a firing
// resumes (or creates) a background task nobody is attached to, so nothing that
// names a resume target, a terminal, a foreground run or a stream applies.
// --stream is absent because ScheduleSpec cannot carry it (Pillar S ruling
// R16); --ip and --host stay (a schedule can pin a runner).
var scheduleOff = map[string]string{
	"resume":      "a firing resumes the schedule's own task",
	"exact":       "a firing resumes the schedule's own task",
	"x11":         "a firing has no client to host the X tunnel",
	"x11-display": "a firing has no client to host the X tunnel",
	"detach":      "every firing is a background run, which is detached",
	"stream":      "a schedule cannot start an event-stream session",
	"foreground":  "every firing is a background run",
	"background":  "every firing is a background run",
}

// SchedulePresenceFlags are the rows that gain a PresenceField on a schedule:
// an edit must tell "not named" from the zero value (--oneshot=false,
// --resume-conversation=false, and --rows / --cols, whose Default is not 0).
// ScheduleFlagDifferences is the whole list of attributes in which a schedule's
// copy of a shared row differs from `session new`'s; a test holds it.
var schedulePresence = map[string]string{
	"resume-conversation": "ResumeConversationPresent",
	"oneshot":             "OneshotPresent",
	"rows":                "RowsPresent",
	"cols":                "ColsPresent",
	// The conditions travel as one presence bit each (R17): a present zero
	// is an answer (--queue-timeout 0 = none; --quiet 0 = the default).
	"deadline":      "DeadlinePresent",
	"quiet":         "QuietPresent",
	"blocked":       "BlockedPresent",
	"queue-timeout": "QueueTimeoutPresent",
	"turn-end":      "TurnEndPresent",
}

// scheduleFlags is spawnFlags minus scheduleOff, the same Flag rows with the
// differences schedulePresence and the two below name, plus nothing of its own
// (scheduleVerbFlags adds --cron, --tz, --task).
//
//   - --repo loses its ladder: HARNESS_REPO_PATH is set in every runner-spawned
//     task and would make `schedule new --task T` name a repo too (refused,
//     S-D14) and a bare `schedule edit` try to change a bound schedule's repo.
//   - --resume-conversation's help names --resume, which a schedule has not.
func scheduleFlags(all []Flag) []Flag {
	var out []Flag
	for _, f := range all {
		if _, off := scheduleOff[f.Name]; off {
			continue
		}
		if p, ok := schedulePresence[f.Name]; ok {
			f.PresenceField = p
		}
		switch f.Name {
		case "repo":
			f.Resolve = nil
			f.Help = "the repo the first firing creates the task in; must match a runner-registered RepoPath verbatim (needs this or --task; no HARNESS_REPO_PATH fallback)"
		case "resume-conversation":
			f.Help = "resume the agent's own conversation state at every firing after the first"
		case "turn-end":
			f.Help = "background: which signal ends the run at a turn's end: off|quiet|lifecycle|program|any|default (default = any; on an edit, naming it alone leaves the other conditions as they are)"
		case "runner":
			f.Help = "pin to a specific runner by ConnectionID (the id= value from `ls`); `any` (on an edit) sets the schedule back to any runner"
		}
		out = append(out, f)
	}
	return out
}

// scheduleExclusive are the cross-flag constraints of schedule new / edit: the
// spawn rules that name flags a schedule has, plus --repo against --task
// (S-D14: the repo is the task's).
func scheduleExclusive() []Rule {
	return []Rule{
		{Flags: []string{"runner", "host", "ip"},
			Reason: "they each name one runner, and two of them name two"},
		{Flags: []string{"record", "no-record"},
			Reason: "they are the two answers to one question"},
		{Flags: []string{"pty", "no-pty"},
			Reason: "they are the two answers to one question"},
		{Flags: []string{"repo", "task"},
			Reason: "a bound task brings its own repo"},
		{Flags: []string{"agent-arg", "no-agent-args"},
			Reason: "one adds to the stored list and the other empties it"},
	}
}

// scheduleValidate is spawnValidate's residue for a schedule. isNew: a create
// states its prompt; an edit names only what changes.
func scheduleValidate(b Bound, isNew bool) error {
	if te := b.Str("turn-end"); te != "" && te != TurnEndDefault {
		if _, err := ParseTurnEnd(te); err != nil {
			return fmt.Errorf("%w (or %s)", err, TurnEndDefault)
		}
	}
	if b.Bool("oneshot") && isNew && b.Str("prompt") == "" {
		return fmt.Errorf("--oneshot needs --prompt: it runs the agent's one-shot argv on that prompt")
	}
	if t := b.Str("task"); t != "" {
		if raw, err := hex.DecodeString(t); err != nil || len(raw) != 16 {
			return fmt.Errorf("--task: want 32 hex characters")
		}
	}
	return nil
}

// scheduleVerbFlags is a schedule new / edit row's flag list: the schedule's
// own three, then the shared spawn rows. --cron is required on a create and
// optional on an edit (an edit changes only what it names).
func scheduleVerbFlags(isNew bool) []Flag {
	own := []Flag{
		{Name: "cron", Type: FlagString, Default: "", Field: "Cron", Required: isNew,
			Help: "five-field cron expression or @hourly/@daily/@weekly/@monthly/@yearly, matched against the schedule's local clock (`*/30 * * * *` for every half hour; no @every)"},
		{Name: "tz", Type: FlagString, Default: "", Field: "Tz",
			Help: "IANA time zone the expression is read in (default: the server's local zone, stored by name)"},
		{Name: "task", Type: FlagString, Default: "", Field: "TaskID",
			Help: "bind this existing task (32 hex) instead of creating one at the first firing; it must not be running in foreground"},
	}
	if !isNew {
		// Edit-only spellings (R19): the wire can say "empty list" and "any
		// runner", the shared rows have no word for them.
		own = append(own, Flag{Name: "no-agent-args", Type: FlagBool, Default: false, Field: "NoAgentArgs",
			Help: "empty the stored agent args (--agent-arg replaces them; naming neither keeps them)"})
	}
	return append(own, spawnFlags(spawnSchedule)...)
}

// x11SurfaceReason is why --x11 and --x11-display stop at the WebUI: the
// forward ends at an X server on the machine the client runs on, and a page has
// no way to reach one.
const x11SurfaceReason = "a browser has no local X server for the tunnel to end at"

// spawnRules are the cross-flag constraints of one spawn verb. They were
// enforced in cmd/harness-cli/session.go alone before the migration, so the
// TUI accepted lines the CLI refused.
func spawnRules(k spawnKind) ([]Rule, []Requirement) {
	// The selector trio names ONE runner on every spawn verb; two of them
	// name two, which is not a narrowing but a contradiction.
	excl := []Rule{{
		Flags:  []string{"runner", "host", "ip"},
		Reason: "they each name one runner, and two of them name two",
	}}
	// --exact takes the task's recorded settings — the last run's prompt and
	// --oneshot among them — so every flag that sets one contradicts it.
	exactConflicts := []string{"prompt", "oneshot", "agent", "agent-arg", "caps", "scope", "scope-for", "delegate-caps", "delegate-scope", "spawn-depth", "max-descendants"}
	for _, f := range exactConflicts {
		excl = append(excl, Rule{Flags: []string{"exact", f}, Reason: "an exact resume takes the task's recorded settings"})
	}
	exactNeeds := Requirement{Flags: []string{"exact"}, Needs: "resume", Reason: "an exact resume names the task it restores"}
	excl = append(excl,
		Rule{Flags: []string{"x11", "detach"},
			Reason: "a detached session has no client to host the X tunnel"},
		Rule{Flags: []string{"stream", "x11"},
			Reason: "X11 is a terminal-session concept; the server refuses the pair too"},
		Rule{Flags: []string{"background", "foreground"},
			Reason: "they name the two run modes"},
		Rule{Flags: []string{"background", "x11"},
			Reason: "a background session has no client to host the X tunnel"},
		Rule{Flags: []string{"record", "no-record"},
			Reason: "they are the two answers to one question"},
		Rule{Flags: []string{"oneshot", "stream"},
			Reason: "a one-shot runs in a PTY; an event-stream session has none"},
		Rule{Flags: []string{"pty", "no-pty"},
			Reason: "they are the two answers to one question"},
		Rule{Flags: []string{"pty", "stream"},
			Reason: "an event-stream session has no PTY; the server refuses the pair too"},
	)
	// A stream session needs --detach or --background (which implies it):
	// checked in spawnValidate, since a Requirement names one flag.
	return excl, []Requirement{exactNeeds}
}

func spawnExclusive(k spawnKind) []Rule { r, _ := spawnRules(k); return r }

func spawnRequires(k spawnKind) []Requirement { _, q := spawnRules(k); return q }

// spawnValidate is the residue: a range on one flag's VALUE, which no
// attribute expresses.
func spawnValidate(b Bound) error {
	if b.Bool("x11") && uintOf(b.Flags["x11-display"]) > 99 {
		return fmt.Errorf("--x11-display must be 0..99")
	}
	// With --exact the declared rule refuses --oneshot itself, which says
	// more than asking for a prompt it would refuse next.
	if b.Bool("oneshot") && !b.Bool("exact") && b.Str("prompt") == "" {
		return fmt.Errorf("--oneshot needs --prompt: it runs the agent's one-shot argv on that prompt")
	}
	if b.Bool("stream") && !b.Bool("detach") && !b.Bool("background") {
		return fmt.Errorf("--stream needs --detach (or --background): an event-stream session has no terminal to attach; follow it with `session stream attach`")
	}
	if te := b.Str("turn-end"); te != "" {
		if _, err := ParseTurnEnd(te); err != nil {
			return err
		}
	}
	return nil
}

// TurnEndDefault is the schedule's spelling of "back to the server's default
// signal" (any): a present turn_end with the unset value.
const TurnEndDefault = "default"

// ParseTurnEnd reads --turn-end. "" is the server's default.
func ParseTurnEnd(s string) (protocol.TurnEndSignal, error) {
	switch s {
	case "":
		return protocol.TurnEndSignal_Unset, nil
	case "off":
		return protocol.TurnEndSignal_Off, nil
	case "quiet":
		return protocol.TurnEndSignal_Quiet, nil
	case "lifecycle":
		return protocol.TurnEndSignal_Lifecycle, nil
	case "program":
		return protocol.TurnEndSignal_Program, nil
	case "any":
		return protocol.TurnEndSignal_Any, nil
	}
	return 0, fmt.Errorf("--turn-end %q: want off|quiet|lifecycle|program|any", s)
}

// argListValue backs --agent-arg.
var argListValue = &CustomValue{
	New: func() flag.Value { return &stringList{} },
	Get: func(v flag.Value) any {
		if l, ok := v.(*stringList); ok {
			return []string(*l)
		}
		return []string(nil)
	},
}

// scopeForValue backs --scope-for. The parse and merge happen in the caller's
// package (they need protocol types this one deliberately does not build on),
// so here it accumulates the raw spellings in order.
var scopeForValue = &CustomValue{
	New: func() flag.Value { return &stringList{} },
	Get: func(v flag.Value) any {
		if l, ok := v.(*stringList); ok {
			return []string(*l)
		}
		return []string(nil)
	},
}

// ParseRunModeWord reads `session mode`'s mode positional: true = background.
func ParseRunModeWord(s string) (bool, error) {
	switch s {
	case "background":
		return true, nil
	case "foreground":
		return false, nil
	}
	return false, fmt.Errorf("session mode: want foreground or background, got %q", s)
}
