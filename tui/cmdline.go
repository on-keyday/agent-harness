package tui

import (
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/google/shlex"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Action is the typed result of parsing one cmdline input.
// app.go switches on the concrete type.
type Action interface{ isAction() }

type SubmitAction struct {
	Repo               string
	Caps               *protocol.Capability
	Scope              *protocol.TaskScope
	Overrides          []protocol.ScopeOverride
	Prompt             string
	ExtraArgs          []string
	ResumeTaskID       string
	ResumeConversation bool
	AgentProfile       string
}

type CancelAction struct {
	IDPrefix string
}

// PruneAction forgets tasks server-side. Two mutually-exclusive modes,
// mirroring `harness-cli prune`:
//   - time mode (TaskIDs empty): terminal tasks older than Before are removed;
//     Force is ignored.
//   - id mode (TaskIDs non-empty): only those ids are considered, Before is
//     ignored, and active (Queued/Running/Detached) tasks are skipped unless
//     Force. Ids must be full 32-hex — no prefix resolution, deliberately, so a
//     mistype misses (safe no-op) rather than resolving onto the wrong task.
type PruneAction struct {
	Before  time.Duration
	TaskIDs []string
	Force   bool
}

type ClearAction struct{}
type QuitAction struct{}
type HelpAction struct{}

// RefreshAction forces a full snapshot re-sync (runners + tasks) right now,
// without waiting for the next event or resubscribe gap-fill.
type RefreshAction struct{}

// GridDiagAction turns the grid panes' per-pane diagnostic overlay on or off.
// On means every pane replaces its top body row with rx/rate/size/content-row
// state, which is how a black or apparently-stuck pane reports why.
//
// Set is nil for a bare `diag`, which TOGGLES. An explicit on/off is not the
// same request as a toggle: a script or a second operator saying `diag on`
// must not turn it OFF because someone already did.
type GridDiagAction struct{ Set *bool }

// TrsfDebugAction dumps the client↔server trsf transport's internal state into
// the command-result panel (debug aid).
type TrsfDebugAction struct{}

// SessionNewAction opens a new detachable interactive PTY session.
// When Detach is true the session is opened and the local stream is closed
// immediately (Docker-style background start); the task id is printed to
// cmdresult and the TUI is not suspended.
//
// Host / Runner / IP carry the runner pin selector, mutually exclusive and
// validated at parse time via cli.SelectorOpts.ValidateSelector.
type SessionNewAction struct {
	Repo               string
	Caps               *protocol.Capability
	Scope              *protocol.TaskScope
	Overrides          []protocol.ScopeOverride
	ExtraArgs          []string
	ResumeTaskID       string
	ResumeConversation bool
	Detach             bool
	Host               string
	Runner             string
	IP                 string
	X11                bool
	X11Display         int
	AgentProfile       string
	// Stream opens the session as an event-stream one (`--stream`). TUI-side
	// it requires Detach: the interactive handover is a terminal splice, and
	// this kind has no terminal — its events are followed in the logs pane.
	Stream bool
}

// SessionAttachAction re-attaches to an existing detachable session by ID.
type SessionAttachAction struct {
	TaskID string
}

// SessionStreamWriteAction is one write on an event-stream task's data plane —
// the cmdline route to what the chat view (r) does with keys. One action for
// all four verbs rather than four near-identical ones: they differ only in
// which message gets built, and that choice already lives in cli.
type SessionStreamWriteAction struct {
	Verb      string // turn | approve | interrupt | finish
	IDPrefix  string
	RequestID string // approve only
	Verdict   string // approve only: allow | deny
	Text      string // turn: the message; approve --deny: the reason
}

// SessionStreamAttachAction follows an event-stream task's events. In the TUI
// that is the logs pane — the runner renders this kind's events into the task
// log, so following the log IS following the stream; no second renderer.
type SessionStreamAttachAction struct {
	IDPrefix string
}

// SessionLsAction lists interactive+detachable tasks in the cmdresult area.
type SessionLsAction struct{}

// SessionKillAction is an alias for CancelAction targeting a session.
// It reuses CancelAction so app.go's existing cancel dispatch handles it.
type SessionKillAction struct {
	IDPrefix string
}

// SessionAwaitIdleAction arms a one-shot idle watcher on a live session.
// Default sink is reply: the long-poll runs in a tea.Cmd goroutine and the
// fire lands in cmdresult as an AwaitIdleResultMsg (non-blocking for the UI).
// Notify routes the fire through the operator-notification egress; Topic
// publishes it to an agentboard topic instead.
type SessionAwaitIdleAction struct {
	IDPrefix    string
	ThresholdMs uint32
	Notify      bool
	Topic       string
}

// RepoAction switches the TUI session's default repo. Subsequent submit
// popups, interactive opens, and slash-command --repo defaults all use the
// new value. Per-action --repo overrides still win on a single call.
type RepoAction struct {
	Path string
}

// GitAction opens the git modal on a task and runs one query in it. Sub is
// one of log / diff / show / status; the revision fields follow git's own
// positional counting (see parseGit).
type GitAction struct {
	TaskID    string
	Sub       string
	BaseRev   string
	TargetRev string
	// Path filters within a repository; Subrepo chooses which repository.
	Path      string
	Subrepo   string
	Staged    bool
	Submodule bool
	Max       uint32
}

// GridAction opens the live session viewer grid over a chosen set of tasks —
// the cmdline form of the g / z / Z keys, and the only way to name an arbitrary
// set. Mode and the two selectors are handed straight to cli.GridSet, so the
// TUI, the WebUI command line and the keys all agree on what each mode means.
//
// Anchor and IDs are id PREFIXES here; app.go resolves them against the task
// table before the set is built, the same as every other id-taking action.
type GridAction struct {
	Mode   cli.GridScopeMode
	Anchor string
	IDs    []string
}

// FileLsAction lists a directory under a task's worktree. RelPath empty
// means the worktree root.
type FileLsAction struct {
	TaskID  string
	RelPath string
}

// FilePushAction copies a local source into a task's worktree.
// Recursive=true uses dir_push (tar over the wire). Force overwrites an
// existing destination (push) or replaces an existing directory tree
// (dir_push). Parents creates missing parent directories of RemoteDst
// (mkdir -p semantics) before the push.
type FilePushAction struct {
	TaskID    string
	LocalSrc  string
	RemoteDst string
	Recursive bool
	Force     bool
	Parents   bool // create missing parent dirs of RemoteDst (mkdir -p)
}

// FileMkdirAction creates a directory under a task's worktree.
// Parents=false is strict mkdir (missing parent → error, existing dir
// → error); Parents=true is mkdir -p (parents created, idempotent).
type FileMkdirAction struct {
	TaskID  string
	RelPath string
	Parents bool
}

// FileEditAction opens a worktree file in the editor popup.
type FileEditAction struct {
	TaskID  string
	RelPath string
}

// FileNewAction opens the editor popup on a path that does not exist yet.
type FileNewAction struct {
	TaskID  string
	RelPath string
}

// FilePullAction copies from a task's worktree to a local destination.
// Recursive uses dir_pull. Force permits overwriting the local path.
type FilePullAction struct {
	TaskID    string
	RemoteSrc string
	LocalDst  string
	Recursive bool
	Force     bool
	Offset    uint64
	Length    uint64 // 0 = to end of file
}

// FileDeleteAction removes a path from a task's worktree. Recursive uses
// dir_delete; Force on Recursive removes a non-empty directory tree via
// os.RemoveAll on the runner side. Force without Recursive is a no-op
// (single-file delete has no force semantics).
type FileDeleteAction struct {
	TaskID    string
	RelPath   string
	Recursive bool
	Force     bool
}

// WorkspaceAction is the `workspace <sub> [name]` family: save the current
// client state into .harness/config, re-apply a workspace, or inspect what the
// file holds. There is no `workspace open`-shaped verb for starting one piece
// at a time; an apply is all-or-nothing by design.
type WorkspaceAction struct {
	Sub  string // "save" | "apply" | "ls" | "show"
	Name string // "" means the installed workspace, except for save
	// All is `save --all`: write every live session without opening the
	// picker. The picker is the default because which tasks belong in a
	// workspace is a statement, not something a rule can infer.
	All bool
}

// ForwardLsAction lists every port forward visible to this operator.
type ForwardLsAction struct{}

// ForwardKillAction closes one registered forward by id.
type ForwardKillAction struct{ ForwardID uint64 }

// InteractiveAction opens an interactive PTY claude session in Repo —
// the slash-command equivalent of the 'i' key, useful when chaining
// after /repo or when the user is already in cmdline focus.
type InteractiveAction struct {
	Repo               string
	Caps               *protocol.Capability
	Scope              *protocol.TaskScope
	Overrides          []protocol.ScopeOverride
	ExtraArgs          []string
	ResumeTaskID       string
	ResumeConversation bool
	AgentProfile       string
}

// ServerDialRunnerAction asks the server to dial out to a Listen-mode
// runner (Phase A reverse-dial / Phase B relayed-dial). Used in ACL
// environments where the runner cannot dial the server directly.
// Via, when non-empty, requests a relay through the named runner CID
// (Phase B: objproto EstablishRelay).
type ServerDialRunnerAction struct {
	RunnerCID string // e.g. "ws:192.168.3.10:8540-*"
	Via       string // empty = direct dial; non-empty = relay via this CID
}

// NotifyAction sends a notification via the server's notify hook.
// Level is one of "info", "warn", "error" (empty defaults to "info").
// Title is the first word of text when not using --level / explicit title
// syntax; see parseNotify for the full grammar.
type NotifyAction struct {
	Level string
	Title string
	Text  string
}

// CapsAction sets or shows the session-default capability mask applied to
// spawns that do not carry their own --caps.
// Show=true (no args): display current caps in the status line.
// Show=false: update sessionCaps to Caps.
//
// The default deliberately does not apply on resume — a resumed task keeps its
// persisted caps unless the resuming command names --caps explicitly. The old
// `caps --on-resume on` toggle re-granted from whatever the mode happened to
// hold at the time, silently, on every resume; it rewrote at least one live
// task's caps by accident and is gone.
type CapsAction struct {
	Caps protocol.Capability
	Show bool // true = display current set (no args), false = set to Caps
}

// ScopeAction is the target-set companion to CapsAction: caps say which verbs
// a spawned task may use, scope says which tasks it may point them at. Same
// show/set shape, same "does not apply on resume" rule.
type ScopeAction struct {
	Scope     protocol.TaskScope
	Overrides []protocol.ScopeOverride
	Show      bool
}

// SetCapsAction re-grants a LIVE task's authority. Operator-only, enforced by
// the server on the caller having no principal task — the TUI is always an
// operator connection, so there is no client-side gate to add here.
//
// Caps and Scope are pointers: omitting either keeps what the task has, and
// neither has a spare value to mean "unset" (Capability(0) is "none",
// TaskScope{} is "subtree").
type SetCapsAction struct {
	TaskID string
	Caps   *protocol.Capability
	Scope  *protocol.TaskScope
	// Overrides travels with Scope under the same presence, matching the CLI
	// and the wire: they are one half of the authority, so writing the scope
	// while keeping the old overrides would store something nobody described.
	Overrides []protocol.ScopeOverride
	Cascade   bool
	KeepConns bool
}

// SetParentAction re-points a LIVE task's parent link, the edge subtree
// scopes walk. Operator-only, enforced server-side. Exactly one of ParentID /
// Detach / Swap is set — the parser rejects zero or two.
type SetParentAction struct {
	TaskID   string
	ParentID string
	Detach   bool
	Swap     bool
}

func (SubmitAction) isAction()              {}
func (ScopeAction) isAction()               {}
func (SetCapsAction) isAction()             {}
func (SetParentAction) isAction()           {}
func (CancelAction) isAction()              {}
func (PruneAction) isAction()               {}
func (ClearAction) isAction()               {}
func (QuitAction) isAction()                {}
func (HelpAction) isAction()                {}
func (RefreshAction) isAction()             {}
func (TrsfDebugAction) isAction()           {}
func (GridDiagAction) isAction()            {}
func (RepoAction) isAction()                {}
func (InteractiveAction) isAction()         {}
func (SessionNewAction) isAction()          {}
func (SessionAttachAction) isAction()       {}
func (SessionStreamWriteAction) isAction()  {}
func (SessionStreamAttachAction) isAction() {}
func (SessionLsAction) isAction()           {}
func (SessionKillAction) isAction()         {}
func (SessionAwaitIdleAction) isAction()    {}
func (GitAction) isAction()                 {}
func (GridAction) isAction()                {}
func (FileLsAction) isAction()              {}
func (FilePushAction) isAction()            {}
func (FileMkdirAction) isAction()           {}
func (FilePullAction) isAction()            {}
func (FileDeleteAction) isAction()          {}
func (FileEditAction) isAction()            {}
func (FileNewAction) isAction()             {}
func (WorkspaceAction) isAction()           {}
func (ForwardLsAction) isAction()           {}
func (ForwardKillAction) isAction()         {}
func (ServerDialRunnerAction) isAction()    {}
func (NotifyAction) isAction()              {}
func (CapsAction) isAction()                {}

// ParseCommand tokenizes and parses one input line. defaultRepo is used when
// `submit` is invoked without --repo (typically the cwd).
// Returns (nil, nil) for empty / whitespace-only input.
func ParseCommand(input, defaultRepo string) (Action, error) {
	tokens, err := shlex.Split(input)
	if err != nil {
		return nil, fmt.Errorf("shlex: %w", err)
	}
	if len(tokens) == 0 {
		return nil, nil
	}
	switch tokens[0] {
	case "submit":
		return parseSubmit(tokens[1:], defaultRepo)
	case "cancel":
		return parseCancel(tokens[1:])
	case "prune":
		return parsePrune(tokens[1:])
	case "clear":
		return ClearAction{}, nil
	case "refresh", "sync":
		return RefreshAction{}, nil
	case "quit", "exit":
		return QuitAction{}, nil
	case "help":
		return HelpAction{}, nil
	case "repo":
		return parseRepo(tokens[1:])
	case "interactive":
		return parseInteractive(tokens[1:], defaultRepo)
	case "session":
		return parseSession(tokens[1:], defaultRepo)
	case "file":
		return parseFile(tokens[1:])
	case "git":
		return parseGit(tokens[1:])
	case "forward":
		return parseForward(tokens[1:])
	case "workspace":
		return parseWorkspace(tokens[1:])
	case "server":
		return parseServer(tokens[1:])
	case "trsf":
		return TrsfDebugAction{}, nil
	case "diag":
		if len(tokens) == 1 {
			return GridDiagAction{}, nil
		}
		switch tokens[1] {
		case "on":
			on := true
			return GridDiagAction{Set: &on}, nil
		case "off":
			off := false
			return GridDiagAction{Set: &off}, nil
		}
		return nil, fmt.Errorf("diag: want `diag` (toggle), `diag on` or `diag off`, got %q", tokens[1])
	case "notify":
		return parseNotify(tokens[1:])
	case "caps":
		if len(tokens) == 1 {
			return CapsAction{Show: true}, nil
		}
		if tokens[1] == "--on-resume" {
			return nil, fmt.Errorf("caps --on-resume was removed: pass --caps on the resuming command instead (e.g. `session new --resume <id> --caps all,-spawn`), which re-grants that mask for that one resume")
		}
		if tokens[1] == "set" {
			return parseSetCaps(tokens[2:])
		}
		if tokens[1] == "set-parent" {
			return parseSetParent(tokens[2:])
		}
		c, err := cli.ParseCaps(strings.Join(tokens[1:], ""))
		if err != nil {
			return nil, err
		}
		return CapsAction{Caps: c}, nil
	case "grid":
		return parseGrid(tokens[1:])
	case "scope":
		if len(tokens) == 1 {
			return ScopeAction{Show: true}, nil
		}
		sc, err := cli.ParseScope(strings.Join(tokens[1:], ""))
		if err != nil {
			return nil, err
		}
		return ScopeAction{Scope: sc}, nil
	default:
		return nil, fmt.Errorf("unknown command: %q", tokens[0])
	}
}

// parseServer handles the `server <sub>` family. Currently only
// `server dial-runner [--via <cid>] <runner-cid>` is supported.
func parseServer(args []string) (Action, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("server: usage: server dial-runner [--via <cid>] <runner-cid>")
	}
	switch args[0] {
	case "dial-runner":
		// Manually scan args to support --via both before and after the
		// positional CID argument. Go's flag.FlagSet stops at the first
		// non-flag token, so mixed order (cid --via cid) would not work
		// with flag.Parse alone.
		var via, runnerCID string
		rest := args[1:]
		for i := 0; i < len(rest); i++ {
			t := rest[i]
			if t == "--via" {
				i++
				if i >= len(rest) {
					return nil, fmt.Errorf("server dial-runner: --via: missing CID value")
				}
				via = rest[i]
			} else if strings.HasPrefix(t, "--via=") {
				via = t[len("--via="):]
			} else if t == "--" {
				// everything after -- is positional
				i++
				if i >= len(rest) {
					break
				}
				if runnerCID != "" {
					return nil, fmt.Errorf("server dial-runner: usage: server dial-runner [--via <cid>] <runner-cid>")
				}
				runnerCID = rest[i]
			} else if strings.HasPrefix(t, "-") {
				return nil, fmt.Errorf("server dial-runner: unknown flag %q", t)
			} else {
				if runnerCID != "" {
					return nil, fmt.Errorf("server dial-runner: usage: server dial-runner [--via <cid>] <runner-cid>")
				}
				runnerCID = t
			}
		}
		if runnerCID == "" {
			return nil, fmt.Errorf("server dial-runner: usage: server dial-runner [--via <cid>] <runner-cid>")
		}
		return ServerDialRunnerAction{RunnerCID: runnerCID, Via: via}, nil
	default:
		return nil, fmt.Errorf("server: unknown subcommand %q (try: dial-runner)", args[0])
	}
}

func parseInteractive(args []string, defaultRepo string) (Action, error) {
	fs := flag.NewFlagSet("interactive", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repo := fs.String("repo", defaultRepo, "")
	resume := fs.String("resume", "", "task id (32 hex) of a terminal interactive task to resume")
	resumeConversation := fs.Bool("resume-conversation", false, "with --resume, also ask the runner to resume the agent's own conversation state")
	agent := fs.String("agent", "", "agent profile name (empty = runner default)")
	var extra repeatableStrings
	fs.Var(&extra, "claude-arg", "extra CLI arg forwarded to claude (repeatable)")
	var caps capsFlag
	fs.Var(&caps, "caps", capsFlagUsage)
	var scope scopeFlag
	fs.Var(&scope, "scope", scopeFlagUsage)
	var scopeFor scopeForFlag
	fs.Var(&scopeFor, "scope-for", cli.ScopeForFlagUsage)
	if err := fs.Parse(args); err != nil {
		return nil, fmt.Errorf("interactive: %w", err)
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("interactive: unexpected positional argument %q", fs.Arg(0))
	}
	return InteractiveAction{Repo: *repo, ExtraArgs: []string(extra), ResumeTaskID: *resume, ResumeConversation: *resumeConversation, AgentProfile: *agent, Caps: caps.Value(), Scope: scope.Value(), Overrides: scopeFor.out}, nil
}

func parseRepo(args []string) (Action, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("repo: path required")
	}
	if len(args) > 1 {
		return nil, fmt.Errorf("repo: too many arguments (got %d, want 1)", len(args))
	}
	return RepoAction{Path: args[0]}, nil
}

func parseSubmit(args []string, defaultRepo string) (Action, error) {
	fs := flag.NewFlagSet("submit", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repo := fs.String("repo", defaultRepo, "")
	resume := fs.String("resume", "", "task id (32 hex) to resume — server reuses the id and worktree branch")
	resumeConversation := fs.Bool("resume-conversation", false, "with --resume, also ask the runner to resume the agent's own conversation state")
	agent := fs.String("agent", "", "agent profile name (empty = runner default)")
	var extra repeatableStrings
	fs.Var(&extra, "claude-arg", "extra CLI arg forwarded to claude (repeatable)")
	var caps capsFlag
	fs.Var(&caps, "caps", capsFlagUsage)
	var scope scopeFlag
	fs.Var(&scope, "scope", scopeFlagUsage)
	var scopeFor scopeForFlag
	fs.Var(&scopeFor, "scope-for", cli.ScopeForFlagUsage)
	if err := fs.Parse(args); err != nil {
		return nil, fmt.Errorf("submit: %w", err)
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return nil, fmt.Errorf("submit: prompt is required")
	}
	return SubmitAction{Repo: *repo, Prompt: strings.Join(rest, " "), ExtraArgs: []string(extra), ResumeTaskID: *resume, ResumeConversation: *resumeConversation, AgentProfile: *agent, Caps: caps.Value(), Scope: scope.Value(), Overrides: scopeFor.out}, nil
}

// repeatableStrings is a flag.Value that accumulates one entry per occurrence,
// mirroring the same idiom used by cmd/harness-cli for --claude-arg. Local
// definition because the cmdline parser uses its own flag.FlagSet and we
// don't want a cross-package dependency from tui → cmd/harness-cli.
type repeatableStrings []string

func (r *repeatableStrings) String() string {
	if r == nil {
		return ""
	}
	return strings.Join([]string(*r), " ")
}

func (r *repeatableStrings) Set(v string) error {
	*r = append(*r, v)
	return nil
}

// capsFlag is the optional --caps flag on submit / interactive / session new.
// It has to tell "not given" apart from "none": Capability_None is a real
// grantable value — a shell session with no capabilities is a normal thing to
// want — so a zero mask cannot stand in for absence. Value() returns nil when
// the flag never appeared and app.go falls back to the session default.
type capsFlag struct {
	set bool
	val protocol.Capability
}

func (c *capsFlag) String() string {
	if c == nil || !c.set {
		return ""
	}
	return cli.CapsLabel(c.val)
}

func (c *capsFlag) Set(v string) error {
	parsed, err := cli.ParseCaps(v)
	if err != nil {
		return err
	}
	c.set, c.val = true, parsed
	return nil
}

func (c *capsFlag) Value() *protocol.Capability {
	if c == nil || !c.set {
		return nil
	}
	v := c.val
	return &v
}

const capsFlagUsage = "capability mask for this spawn (overrides the `caps` default); " +
	"names are comma-separated and may be subtracted, e.g. all,-spawn"

// scopeForFlag collects repeatable --scope-for values, parsing and merging on
// each Set so an overlapping capability list is refused at the flag rather
// than a round trip later. Mirrors cmd/harness-cli's, deliberately: the two
// surfaces must reject the same input for the same reason.
type scopeForFlag struct{ out []protocol.ScopeOverride }

func (f *scopeForFlag) String() string { return cli.OverridesLabel(f.out) }

func (f *scopeForFlag) Set(v string) error {
	_, ov, err := cli.ParseScopeFor(v)
	if err != nil {
		return err
	}
	merged, err := cli.MergeScopeOverride(f.out, ov)
	if err != nil {
		return err
	}
	f.out = merged
	return nil
}

// scopeFlag is the optional --scope flag on submit / interactive / session
// new — the target-set half of capsFlag, with the same "not given" vs "given
// the zero value" distinction (base subtree is the zero TaskScope).
type scopeFlag struct {
	set bool
	val protocol.TaskScope
}

func (s *scopeFlag) String() string {
	if s == nil || !s.set {
		return ""
	}
	return cli.ScopeLabel(s.val)
}

func (s *scopeFlag) Set(v string) error {
	parsed, err := cli.ParseScope(v)
	if err != nil {
		return err
	}
	s.set, s.val = true, parsed
	return nil
}

func (s *scopeFlag) Value() *protocol.TaskScope {
	if s == nil || !s.set {
		return nil
	}
	v := s.val
	return &v
}

const scopeFlagUsage = "target scope for this spawn (overrides the `scope` default): " +
	cli.ScopeGrammar + "; on a resume it re-grants the scope (omitted = keep the task's), independently of --caps"

func parseCancel(args []string) (Action, error) {
	fs := flag.NewFlagSet("cancel", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return nil, fmt.Errorf("cancel: %w", err)
	}
	if fs.NArg() == 0 {
		return nil, fmt.Errorf("cancel: task id required")
	}
	return CancelAction{IDPrefix: fs.Arg(0)}, nil
}

func parsePrune(args []string) (Action, error) {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	before := fs.Duration("before", 7*24*time.Hour, "")
	force := fs.Bool("force", false, "")
	fs.BoolVar(force, "f", false, "")
	if err := fs.Parse(args); err != nil {
		return nil, fmt.Errorf("prune: %w", err)
	}
	return PruneAction{Before: *before, TaskIDs: fs.Args(), Force: *force}, nil
}

// parseSession dispatches session sub-verbs: new / attach / ls / kill.
func parseSession(args []string, defaultRepo string) (Action, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("session: sub-verb required (new | attach <id> | ls | kill <id>)")
	}
	verb := args[0]
	rest := args[1:]
	switch verb {
	case "new":
		fs := flag.NewFlagSet("session new", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		resume := fs.String("resume", "", "task id (32 hex) of a terminal interactive task to resume into a detachable session")
		resumeConversation := fs.Bool("resume-conversation", false, "with --resume, also ask the runner to resume the agent's own conversation state")
		detach := fs.Bool("detach", false, "start the session and immediately detach (run in background, print task id)")
		// The CLI registers -d as a shorthand (cmd/harness-cli/session.go); an
		// operator who learned the command there types it here and gets "flag
		// provided but not defined".
		fs.BoolVar(detach, "d", false, "shorthand for --detach")
		host := fs.String("host", "", "pin to a runner by reported hostname (mutually exclusive with --runner / --ip)")
		runner := fs.String("runner", "", "pin to a runner by 32-hex RunnerID (mutually exclusive with --host / --ip)")
		ip := fs.String("ip", "", "pin to a runner by IP address (mutually exclusive with --host / --runner)")
		x11 := fs.Bool("x11", false, "X11-forward GUI apps to your local X server")
		stream := fs.Bool("stream", false, "open an event-stream session (structured events, no PTY); requires --detach here — follow it in the logs pane")
		x11Display := fs.Int("x11-display", 10, "X11 display number N (runner binds 127.0.0.1:6000+N)")
		agent := fs.String("agent", "", "agent profile name (empty = runner default; on --resume, the resumed task's own profile)")
		var extra repeatableStrings
		fs.Var(&extra, "claude-arg", "extra CLI arg forwarded to claude (repeatable)")
		var caps capsFlag
		fs.Var(&caps, "caps", capsFlagUsage)
		var scope scopeFlag
		fs.Var(&scope, "scope", scopeFlagUsage)
		var scopeFor scopeForFlag
		fs.Var(&scopeFor, "scope-for", cli.ScopeForFlagUsage)
		if err := fs.Parse(rest); err != nil {
			return nil, fmt.Errorf("session new: %w", err)
		}
		if fs.NArg() > 0 {
			return nil, fmt.Errorf("session new: unexpected argument %q", fs.Arg(0))
		}
		if err := (cli.SelectorOpts{Runner: *runner, Host: *host, IP: *ip}).ValidateSelector(); err != nil {
			return nil, fmt.Errorf("session new: %w", err)
		}
		if *x11 && *detach {
			return nil, fmt.Errorf("session new: --x11 is incompatible with --detach")
		}
		if *stream && *x11 {
			return nil, fmt.Errorf("session new: --stream is incompatible with --x11 (X11 is a terminal-session concept)")
		}
		if *stream && !*detach {
			// The non-detach path hands the TERMINAL to the new session, and an
			// event-stream session has no terminal to hand it to. The CLI's
			// non-detach form follows events instead; the TUI's follower is the
			// logs pane, which needs no live handover.
			return nil, fmt.Errorf("session new: --stream needs -d in the TUI (then select the task; its events render in the logs pane)")
		}
		return SessionNewAction{
			Repo:               defaultRepo,
			ExtraArgs:          []string(extra),
			ResumeTaskID:       *resume,
			ResumeConversation: *resumeConversation,
			Detach:             *detach,
			Host:               *host,
			Runner:             *runner,
			IP:                 *ip,
			X11:                *x11,
			X11Display:         *x11Display,
			AgentProfile:       *agent,
			Caps:               caps.Value(),
			Scope:              scope.Value(),
			Overrides:          scopeFor.out,
			Stream:             *stream,
		}, nil
	case "attach":
		if len(rest) == 0 {
			return nil, fmt.Errorf("session attach: task id required")
		}
		if len(rest) > 1 {
			return nil, fmt.Errorf("session attach: too many arguments (got %d, want 1)", len(rest))
		}
		return SessionAttachAction{TaskID: rest[0]}, nil
	case "stream":
		// The event-stream namespace (design §3): one verb per inbound kind.
		// requests/snapshot are the two still unbuilt; naming one reports that
		// instead of "unknown".
		if len(rest) == 0 {
			return nil, fmt.Errorf("session stream: sub-verb required (attach|turn|approve|interrupt|finish <id>)")
		}
		switch rest[0] {
		case "attach":
			if len(rest) != 2 {
				return nil, fmt.Errorf("session stream attach: exactly one task id required")
			}
			return SessionStreamAttachAction{IDPrefix: rest[1]}, nil
		case "turn":
			if len(rest) < 3 {
				return nil, fmt.Errorf("session stream turn: <id> and the text (`session stream turn <id> hello there`)")
			}
			return SessionStreamWriteAction{Verb: "turn", IDPrefix: rest[1],
				Text: strings.Join(rest[2:], " ")}, nil
		case "approve":
			// <id> <request-id> allow|deny [reason...]. The verdict is a WORD
			// here rather than the CLI's --allow/--deny: this line is
			// whitespace-split with no flag parser, so a bare word cannot be
			// mistaken for one and cannot be silently dropped.
			if len(rest) < 4 {
				return nil, fmt.Errorf("session stream approve: <id> <request-id> allow|deny [reason...]")
			}
			verdict := rest[3]
			if verdict != "allow" && verdict != "deny" {
				return nil, fmt.Errorf("session stream approve: want allow or deny, got %q", verdict)
			}
			act := SessionStreamWriteAction{Verb: "approve", IDPrefix: rest[1], RequestID: rest[2], Verdict: verdict}
			if verdict == "deny" {
				act.Text = strings.Join(rest[4:], " ")
			} else if len(rest) > 4 {
				return nil, fmt.Errorf("session stream approve: an allow carries no message (the reason is a deny's)")
			}
			return act, nil
		case "interrupt", "finish":
			if len(rest) != 2 {
				return nil, fmt.Errorf("session stream %s: exactly one task id required", rest[0])
			}
			return SessionStreamWriteAction{Verb: rest[0], IDPrefix: rest[1]}, nil
		case "requests", "snapshot":
			return nil, fmt.Errorf("session stream %s: specified (design §3) but not built yet", rest[0])
		default:
			return nil, fmt.Errorf("unknown session stream verb %q", rest[0])
		}
	case "ls":
		return SessionLsAction{}, nil
	case "kill":
		if len(rest) == 0 {
			return nil, fmt.Errorf("session kill: task id required")
		}
		if len(rest) > 1 {
			return nil, fmt.Errorf("session kill: too many arguments (got %d, want 1)", len(rest))
		}
		return SessionKillAction{IDPrefix: rest[0]}, nil
	case "await-idle":
		fs := flag.NewFlagSet("session await-idle", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		thresholdMs := fs.Uint("threshold-ms", 0, "quiescence threshold in ms (0 = server default)")
		notify := fs.Bool("notify", false, "fire via the operator-notification egress instead of an in-TUI result line")
		topic := fs.String("topic", "", "fire via an agentboard publish to this topic")
		if err := fs.Parse(rest); err != nil {
			return nil, fmt.Errorf("session await-idle: %w", err)
		}
		if fs.NArg() == 0 {
			return nil, fmt.Errorf("session await-idle: task id required")
		}
		if fs.NArg() > 1 {
			return nil, fmt.Errorf("session await-idle: too many arguments (got %d, want 1)", fs.NArg())
		}
		if *notify && *topic != "" {
			return nil, fmt.Errorf("session await-idle: --notify and --topic are mutually exclusive")
		}
		return SessionAwaitIdleAction{
			IDPrefix:    fs.Arg(0),
			ThresholdMs: uint32(*thresholdMs),
			Notify:      *notify,
			Topic:       *topic,
		}, nil
	default:
		return nil, fmt.Errorf("session: unknown sub-verb %q (new | attach <id> | ls | kill <id> | await-idle <id>)", verb)
	}
}

// capsLabel formats a Capability bitmask for human display:
// "all", "none", or a comma-joined list of the enabled granular caps.
func capsLabel(c protocol.Capability) string {
	if c == protocol.Capability_All {
		return "all"
	}
	if c == protocol.Capability_None {
		return "none"
	}
	var names []string
	for _, g := range cli.GrantableCaps() {
		if g == protocol.Capability_None || g == protocol.Capability_All {
			continue
		}
		if c&g == g {
			names = append(names, g.String())
		}
	}
	return strings.Join(names, ",")
}

// parseNotify parses `notify [<level>] <text>`.
//
// Grammar:
//
//	notify <text>                 — level defaults to "info", title = first word, text = remainder
//	notify <level> <text>         — level ∈ {info,warn,error}, title = first word of text, text = remainder
//
// The first argument is treated as a level only when it is exactly one of
// "info", "warn", or "error". Otherwise the entire argument list is the text.
// The first whitespace-delimited token of the text becomes the title; the rest
// (if any) is the notification body. To include spaces in the title, quote the
// whole first argument (shlex splitting has already run).
func parseNotify(args []string) (Action, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("notify: usage: notify [info|warn|error] <title> [<text>...]")
	}
	level := ""
	rest := args
	switch args[0] {
	case "info", "warn", "error":
		level = args[0]
		rest = args[1:]
	}
	if len(rest) == 0 {
		return nil, fmt.Errorf("notify: title required")
	}
	title := rest[0]
	text := strings.Join(rest[1:], " ")
	return NotifyAction{Level: level, Title: title, Text: text}, nil
}

// parseFile dispatches file sub-verbs: ls / push / pull / mkdir /
// delete. All paths use the same -r / --recursive and -f / --force
// aliases as the CLI so the typing is interchangeable between
// `harness-cli file ...` and the TUI cmdline. Local paths are resolved
// on the host running the TUI; remote paths are interpreted relative
// to the task's worktree by the runner and confined to it (no `..`
// escape).
func parseFile(args []string) (Action, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("file: sub-verb required (ls | push | pull | mkdir | delete | edit | new)")
	}
	verb := args[0]
	rest := args[1:]
	switch verb {
	case "ls":
		if len(rest) < 1 || len(rest) > 2 {
			return nil, fmt.Errorf("file ls: usage: file ls <task-id> [<worktree-rel-dir>]")
		}
		rel := ""
		if len(rest) == 2 {
			rel = rest[1]
		}
		return FileLsAction{TaskID: rest[0], RelPath: rel}, nil
	case "push":
		fs := flag.NewFlagSet("file push", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		recursive := fs.Bool("recursive", false, "")
		fs.BoolVar(recursive, "r", false, "")
		force := fs.Bool("force", false, "")
		fs.BoolVar(force, "f", false, "")
		parents := fs.Bool("parents", false, "")
		fs.BoolVar(parents, "p", false, "")
		if err := fs.Parse(rest); err != nil {
			return nil, fmt.Errorf("file push: %w", err)
		}
		pargs := fs.Args()
		if len(pargs) != 3 {
			return nil, fmt.Errorf("file push: usage: file push [-r] [-f] [-p] <task-id> <local-src> <worktree-rel-dst>")
		}
		return FilePushAction{
			TaskID: pargs[0], LocalSrc: pargs[1], RemoteDst: pargs[2],
			Recursive: *recursive, Force: *force, Parents: *parents,
		}, nil
	case "pull":
		fs := flag.NewFlagSet("file pull", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		recursive := fs.Bool("recursive", false, "")
		fs.BoolVar(recursive, "r", false, "")
		force := fs.Bool("force", false, "")
		fs.BoolVar(force, "f", false, "")
		offset := fs.Uint64("offset", 0, "")
		fs.Uint64Var(offset, "o", 0, "")
		length := fs.Uint64("length", 0, "")
		fs.Uint64Var(length, "n", 0, "")
		if err := fs.Parse(rest); err != nil {
			return nil, fmt.Errorf("file pull: %w", err)
		}
		pargs := fs.Args()
		if len(pargs) != 3 {
			return nil, fmt.Errorf("file pull: usage: file pull [-r] [-f] [-o off] [-n len] <task-id> <worktree-rel-src> <local-dst>")
		}
		// A directory pull is a generated tar; its byte offsets are not a
		// stable thing to index into, so the combination is refused here
		// rather than sent for the runner to reject.
		if *recursive && (*offset != 0 || *length != 0) {
			return nil, fmt.Errorf("file pull: -o/-n cannot be combined with -r")
		}
		return FilePullAction{
			TaskID: pargs[0], RemoteSrc: pargs[1], LocalDst: pargs[2],
			Recursive: *recursive, Force: *force,
			Offset: *offset, Length: *length,
		}, nil
	case "delete":
		fs := flag.NewFlagSet("file delete", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		recursive := fs.Bool("recursive", false, "")
		fs.BoolVar(recursive, "r", false, "")
		force := fs.Bool("force", false, "")
		fs.BoolVar(force, "f", false, "")
		if err := fs.Parse(rest); err != nil {
			return nil, fmt.Errorf("file delete: %w", err)
		}
		pargs := fs.Args()
		if len(pargs) != 2 {
			return nil, fmt.Errorf("file delete: usage: file delete [-r [-f]] <task-id> <worktree-rel-path>")
		}
		return FileDeleteAction{
			TaskID: pargs[0], RelPath: pargs[1],
			Recursive: *recursive, Force: *force,
		}, nil
	case "mkdir":
		fs := flag.NewFlagSet("file mkdir", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		parents := fs.Bool("parents", false, "")
		fs.BoolVar(parents, "p", false, "")
		if err := fs.Parse(rest); err != nil {
			return nil, fmt.Errorf("file mkdir: %w", err)
		}
		pargs := fs.Args()
		if len(pargs) != 2 {
			return nil, fmt.Errorf("file mkdir: usage: file mkdir [-p] <task-id> <worktree-rel-dir>")
		}
		return FileMkdirAction{TaskID: pargs[0], RelPath: pargs[1], Parents: *parents}, nil
	case "edit":
		if len(rest) != 2 {
			return nil, fmt.Errorf("file edit: usage: file edit <task-id> <worktree-rel-path>")
		}
		return FileEditAction{TaskID: rest[0], RelPath: rest[1]}, nil
	case "new":
		if len(rest) != 2 {
			return nil, fmt.Errorf("file new: usage: file new <task-id> <worktree-rel-path>")
		}
		return FileNewAction{TaskID: rest[0], RelPath: rest[1]}, nil
	default:
		return nil, fmt.Errorf("file: unknown sub-verb %q (ls | push | pull | mkdir | delete | edit | new)", verb)
	}
}

// parseForward handles `forward ls` and `forward kill <id>`. Starting a
// forward has no cmdline verb at all — not for -L/-R (`p`/`b`, stopped by
// `P`/`B`) and not for the newer raw connect (`t`, tui/rawforward.go): all
// three are modal-only, matching the fact that `p`/`b` never had a `forward
// open`-shaped verb here either. This stays the list/kill surface only.
func parseForward(args []string) (Action, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("forward: sub-verb required (ls | kill)")
	}
	switch args[0] {
	case "ls":
		if len(args) != 1 {
			return nil, fmt.Errorf("forward ls: usage: forward ls")
		}
		return ForwardLsAction{}, nil
	case "kill":
		if len(args) != 2 {
			return nil, fmt.Errorf("forward kill: usage: forward kill <forward-id>")
		}
		id, err := strconv.ParseUint(args[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("forward kill: bad forward id %q", args[1])
		}
		return ForwardKillAction{ForwardID: id}, nil
	default:
		return nil, fmt.Errorf("forward: unknown sub-verb %q (want ls | kill)", args[0])
	}
}

// parseWorkspace handles the `workspace <sub> [name]` family.
//
// `workspace save` requires a name. Defaulting it to the installed workspace
// would let a slip overwrite one from the live client state; every other verb
// is read-only or re-runs what is already installed, so they may default.
func parseWorkspace(args []string) (Action, error) {
	const usage = "workspace: usage: workspace save <name> [--all] | workspace apply [name] | workspace rm <name> | workspace ls | workspace show [name]"
	if len(args) == 0 {
		return nil, fmt.Errorf("%s", usage)
	}
	sub := args[0]
	rest := args[1:]
	all := false
	var positional []string
	for _, a := range rest {
		if a == "--all" {
			all = true
			continue
		}
		if strings.HasPrefix(a, "-") {
			return nil, fmt.Errorf("workspace %s: unknown flag %q\n%s", sub, a, usage)
		}
		positional = append(positional, a)
	}
	var name string
	if len(positional) > 0 {
		name = positional[0]
	}
	switch sub {
	case "save":
		if name == "" {
			return nil, fmt.Errorf("workspace save: needs a name\n%s", usage)
		}
	case "rm":
		// A name is required, and there is no "the current one" shorthand:
		// deleting is the one verb here that cannot be undone by re-running it.
		if name == "" {
			return nil, fmt.Errorf("workspace rm: needs a name\n%s", usage)
		}
		if all {
			return nil, fmt.Errorf("workspace rm: --all applies to save only\n%s", usage)
		}
	case "apply", "ls", "show":
		if all {
			return nil, fmt.Errorf("workspace %s: --all applies to save only\n%s", sub, usage)
		}
	default:
		return nil, fmt.Errorf("workspace: unknown subcommand %q\n%s", sub, usage)
	}
	if len(positional) > 1 {
		return nil, fmt.Errorf("workspace %s: too many arguments\n%s", sub, usage)
	}
	return WorkspaceAction{Sub: sub, Name: name, All: all}, nil
}

// splitPathspecTokens peels "-- <path...>" off the tail. It runs BEFORE the
// flag parse for the same reason the CLI's splitPathspec does: Go's flag
// package consumes a bare "--" as its end-of-flags marker, so a pathspec left
// in the argv would silently vanish. Tokens after the separator are rejoined
// with a space so a path containing one survives.
func splitPathspecTokens(args []string) ([]string, string) {
	for i, a := range args {
		if a == "--" {
			return args[:i], strings.Join(args[i+1:], " ")
		}
	}
	return args, ""
}

// parsePermutedFlags parses fs while tolerating flags that appear after
// positional arguments, by peeling positionals one at a time and re-parsing
// the remainder. Go's flag package stops at the first non-flag token, so
// without this `git <id> diff HEAD --staged` would silently drop --staged.
//
// This is the same loop as parsePermuted in cmd/harness-cli/session.go; it is
// duplicated rather than imported because tui must not depend on cmd/. Safe
// only because every positional here is a revision, and a revision beginning
// with '-' is refused by the runner anyway.
func parsePermutedFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var positionals []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		positionals = append(positionals, rest[0])
		args = rest[1:]
	}
	return positionals, nil
}

// parseGit parses `git <task-id> {log|diff|show|status} ...` with the same
// grammar harness-cli uses, so a hand that learned one surface knows the other.
func parseGit(args []string) (Action, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("git: usage: git <task-id> {log | diff | show | status | subrepos | file} ...")
	}
	act := GitAction{TaskID: args[0], Sub: args[1]}
	rest, path := splitPathspecTokens(args[2:])
	act.Path = path

	switch act.Sub {
	case "log":
		fs := flag.NewFlagSet("git log", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		max := fs.Uint("max", 0, "")
		subrepo := fs.String("subrepo", "", "")
		pos, err := parsePermutedFlags(fs, rest)
		if err != nil {
			return nil, fmt.Errorf("git log: %w", err)
		}
		if len(pos) > 1 {
			return nil, fmt.Errorf("git log: at most one revision (got %d)", len(pos))
		}
		if len(pos) == 1 {
			act.BaseRev = pos[0]
		}
		act.Max = uint32(*max)
		act.Subrepo = *subrepo

	case "diff":
		fs := flag.NewFlagSet("git diff", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		staged := fs.Bool("staged", false, "")
		fs.BoolVar(staged, "cached", false, "")
		submodule := fs.Bool("submodule", false, "")
		subrepo := fs.String("subrepo", "", "")
		pos, err := parsePermutedFlags(fs, rest)
		if err != nil {
			return nil, fmt.Errorf("git diff: %w", err)
		}
		act.Staged = *staged
		act.Submodule = *submodule
		act.Subrepo = *subrepo
		// Positionals are counted the way git counts them: none = unstaged,
		// one = <base> against the working tree, two = commit against commit.
		switch len(pos) {
		case 0:
		case 1:
			act.BaseRev = pos[0]
		case 2:
			if *staged {
				return nil, fmt.Errorf("git diff: --staged names the index as the right-hand side, so a second revision has nowhere to go")
			}
			act.BaseRev, act.TargetRev = pos[0], pos[1]
		default:
			return nil, fmt.Errorf("git diff: at most two revisions (got %d)", len(pos))
		}

	case "show":
		fs := flag.NewFlagSet("git show", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		submodule := fs.Bool("submodule", false, "")
		subrepo := fs.String("subrepo", "", "")
		pos, err := parsePermutedFlags(fs, rest)
		if err != nil {
			return nil, fmt.Errorf("git show: %w", err)
		}
		if len(pos) > 1 {
			return nil, fmt.Errorf("git show: at most one revision (got %d)", len(pos))
		}
		if len(pos) == 1 {
			act.BaseRev = pos[0]
		}
		act.Submodule = *submodule
		act.Subrepo = *subrepo

	case "file":
		fs := flag.NewFlagSet("git file", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		staged := fs.Bool("staged", false, "")
		rev := fs.String("rev", "", "")
		subrepo := fs.String("subrepo", "", "")
		pos, err := parsePermutedFlags(fs, rest)
		if err != nil {
			return nil, fmt.Errorf("git file: %w", err)
		}
		// The path may arrive as a positional or after --, so one lifted
		// straight out of a diff header works either way.
		if len(pos) > 1 {
			return nil, fmt.Errorf("git file: one path (got %d)", len(pos))
		}
		if len(pos) == 1 {
			if act.Path != "" {
				return nil, fmt.Errorf("git file: path given twice (%q and %q)", pos[0], act.Path)
			}
			act.Path = pos[0]
		}
		if act.Path == "" {
			return nil, fmt.Errorf("git file: usage: git <task-id> file [--staged | --rev REV] <path>")
		}
		act.Staged = *staged
		act.TargetRev = *rev
		act.Subrepo = *subrepo

	case "status", "subrepos":
		fs := flag.NewFlagSet("git "+act.Sub, flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		subrepo := fs.String("subrepo", "", "")
		pos, err := parsePermutedFlags(fs, rest)
		if err != nil {
			return nil, fmt.Errorf("git %s: %w", act.Sub, err)
		}
		if len(pos) > 0 {
			return nil, fmt.Errorf("git %s: takes no revision (got %q)", act.Sub, pos[0])
		}
		act.Subrepo = *subrepo

	default:
		return nil, fmt.Errorf("git: unknown sub-verb %q (log | diff | show | status | subrepos | file)", act.Sub)
	}
	return act, nil
}

// parseSetCaps backs `caps set <task-id> [--caps NAMES] [--scope SPEC]
// [--cascade] [--keep-conns]`, the TUI form of harness-cli caps set.
func parseSetCaps(args []string) (Action, error) {
	usage := "caps set: usage: caps set <task-id> [--caps NAMES] [--scope SPEC] " +
		"[--scope-for CAPS=SCOPE ...] [--cascade] [--keep-conns]"
	act := SetCapsAction{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--cascade":
			act.Cascade = true
		case "--keep-conns":
			act.KeepConns = true
		case "--scope-for":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("caps set: --scope-for needs a value")
			}
			i++
			_, ov, err := cli.ParseScopeFor(args[i])
			if err != nil {
				return nil, fmt.Errorf("caps set: %w", err)
			}
			merged, err := cli.MergeScopeOverride(act.Overrides, ov)
			if err != nil {
				return nil, fmt.Errorf("caps set: %w", err)
			}
			act.Overrides = merged
		case "--caps":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("caps set: --caps needs a value")
			}
			i++
			c, err := cli.ParseCaps(args[i])
			if err != nil {
				return nil, fmt.Errorf("caps set: --caps: %w", err)
			}
			act.Caps = &c
		case "--scope":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("caps set: --scope needs a value")
			}
			i++
			sc, err := cli.ParseScope(args[i])
			if err != nil {
				return nil, fmt.Errorf("caps set: --scope: %w", err)
			}
			act.Scope = &sc
		default:
			if strings.HasPrefix(args[i], "-") {
				return nil, fmt.Errorf("caps set: unknown flag %q\n%s", args[i], usage)
			}
			if act.TaskID != "" {
				return nil, fmt.Errorf("caps set: more than one task id\n%s", usage)
			}
			act.TaskID = args[i]
		}
	}
	if act.TaskID == "" {
		return nil, fmt.Errorf("%s", usage)
	}
	if act.Caps == nil && act.Scope == nil {
		return nil, fmt.Errorf("caps set: pass --caps, --scope, or both — there is nothing to change otherwise")
	}
	return act, nil
}

// parseGrid backs the `grid` verb. The modes are cli.GridScopeMode's, spelled
// the same here as in the WebUI's grid command:
//
//	grid                                 every visible task (the g key)
//	grid <id>...                         exactly these, in this order
//	grid --under <id>                    id + its descendants (the z key)
//	grid --under <id> --descendants      its descendants only (the Z key)
//
// --under's set is the task's WORKING set: its subtree plus whatever its own
// scope names individually (cli.GridSubtree). --descendants without --under is
// rejected rather than silently ignored — there is no subtree to strip a root
// from.
// The grammar itself lives in cli.ParseGridArgs. The workspace config accepts
// the same argument string and cannot import this package, so a copy here would
// be a mirror with no way to fail loudly when the grammar grows.
func parseGrid(args []string) (Action, error) {
	mode, anchor, ids, err := cli.ParseGridArgs(args)
	if err != nil {
		return nil, err
	}
	return GridAction{Mode: mode, Anchor: anchor, IDs: ids}, nil
}

// parseSetParent backs `caps set-parent <task-id> (--parent <task-id> |
// --none | --swap)`, the TUI form of harness-cli caps set-parent.
func parseSetParent(args []string) (Action, error) {
	usage := "caps set-parent: usage: caps set-parent <task-id> (--parent <task-id> | --none | --swap)"
	act := SetParentAction{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--none":
			act.Detach = true
		case "--swap":
			act.Swap = true
		case "--parent":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("caps set-parent: --parent needs a value")
			}
			i++
			act.ParentID = args[i]
		default:
			if strings.HasPrefix(args[i], "-") {
				return nil, fmt.Errorf("caps set-parent: unknown flag %q\n%s", args[i], usage)
			}
			if act.TaskID != "" {
				return nil, fmt.Errorf("caps set-parent: more than one task id\n%s", usage)
			}
			act.TaskID = args[i]
		}
	}
	if act.TaskID == "" {
		return nil, fmt.Errorf("%s", usage)
	}
	picked := 0
	for _, on := range []bool{act.ParentID != "", act.Detach, act.Swap} {
		if on {
			picked++
		}
	}
	if picked != 1 {
		return nil, fmt.Errorf("caps set-parent: pass exactly one of --parent <task-id>, --none, --swap\n%s", usage)
	}
	return act, nil
}
