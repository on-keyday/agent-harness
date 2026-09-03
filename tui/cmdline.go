package tui

import (
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/google/shlex"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/sshgw"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Action is the typed result of parsing one cmdline input; app.go switches on
// the concrete type. It is an alias of the shared interface (cli/verb), so a
// verb declared once is one type on every surface.
//
// The TUI keeps its own screen-state actions -- Clear, Quit, Help, Refresh,
// Repo, TrsfDebug, GridDiag, Grid -- which satisfy it by embedding
// verb.ActionMarker. That marker is exported for exactly this reason: an
// unexported method declared in cli/verb could not be implemented from here.
type Action = verb.Action

// PruneAction forgets tasks server-side. Two mutually-exclusive modes,
// mirroring `harness-cli prune`:
//   - time mode (TaskIDs empty): terminal tasks older than Before are removed;
//     Force is ignored.
//   - id mode (TaskIDs non-empty): only those ids are considered, Before is
//     ignored, and active (Queued/Running/Detached) tasks are skipped unless
//     Force. Ids must be full 32-hex — no prefix resolution, deliberately, so a
//     mistype misses (safe no-op) rather than resolving onto the wrong task.
type ClearAction struct{ verb.ActionMarker }
type QuitAction struct{ verb.ActionMarker }
type HelpAction struct{ verb.ActionMarker }

// RefreshAction forces a full snapshot re-sync (runners + tasks) right now,
// without waiting for the next event or resubscribe gap-fill.
type RefreshAction struct{ verb.ActionMarker }

// GridDiagAction turns the grid panes' per-pane diagnostic overlay on or off.
// On means every pane replaces its top body row with rx/rate/size/content-row
// state, which is how a black or apparently-stuck pane reports why.
//
// Set is nil for a bare `diag`, which TOGGLES. An explicit on/off is not the
// same request as a toggle: a script or a second operator saying `diag on`
// must not turn it OFF because someone already did.
type GridDiagAction struct {
	verb.ActionMarker
	Set *bool
}

// TrsfDebugAction dumps the client↔server trsf transport's internal state into
// the command-result panel (debug aid).
type TrsfDebugAction struct{ verb.ActionMarker }

// RepoAction switches the TUI session's default repo. Subsequent submit
// popups, interactive opens, and slash-command --repo defaults all use the
// new value. Per-action --repo overrides still win on a single call.
type RepoAction struct {
	verb.ActionMarker
	Path string
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
	verb.ActionMarker
	Caps protocol.Capability
	Show bool // true = display current set (no args), false = set to Caps
}

// ScopeAction is the target-set companion to CapsAction: caps say which verbs
// a spawned task may use, scope says which tasks it may point them at. Same
// show/set shape, same "does not apply on resume" rule.
type ScopeAction struct {
	verb.ActionMarker
	Scope     protocol.TaskScope
	Overrides []protocol.ScopeOverride
	Show      bool
}

// parseViaSpec routes one verb through the shared declaration (cli/verb).
//
// ContinueOnError with a discarded writer, because a typo here is a line in
// the results pane rather than an exit -- the CLI wants ExitOnError for the
// same input, which is why the error mode is a parameter of NewFlagSet.
func parseViaSpec(path string, args []string) (Action, error) {
	sp, ok := verb.Lookup(path)
	if !ok {
		return nil, fmt.Errorf("%s: not in the verb table", path)
	}
	// For(TUI) before anything else, like every sibling here: BuildFunc keys
	// on the surface the spec was narrowed to, and an un-narrowed spec falls
	// back to the FIRST declared surface -- the CLI for most of these. Latent
	// while no verb on this path narrows a positional away, and silent when it
	// stops being: a narrowed-away positional shifts every index after it.
	sp = sp.For(verb.TUI)
	fs := sp.NewFlagSet(flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	b, err := sp.Parse(fs, args)
	if err != nil {
		return nil, err // Parse names the verb itself
	}
	return sp.BuildFunc()(b)
}

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
		return parseViaSpec("prune", tokens[1:])
	case "restore":
		return parseViaSpec("restore", tokens[1:])
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
	case "exec":
		return parseExecRun(tokens[1:])
	case "ssh-gateway":
		return parseSSHGateway(tokens[1:])
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
	return parseViaSpec2("server", args[0], args[1:])
}

func parseInteractive(args []string, defaultRepo string) (Action, error) {
	return parseSpawnTUI("interactive", args, defaultRepo)
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
	return parseSpawnTUI("submit", args, defaultRepo)
}

func parseCancel(args []string) (Action, error) {
	return parseViaSpec("cancel", args)
}

// parseSession dispatches session sub-verbs: new / attach / ls / kill.
func parseSession(args []string, defaultRepo string) (Action, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("session: sub-verb required (new | attach <id> | ls | kill <id> | await-idle <id> | stream ...)")
	}
	sub := args[0]
	rest := args[1:]
	if sub == "new" {
		return parseSpawnTUI("session-new", rest, defaultRepo)
	}
	if sub == "stream" {
		// The event-stream namespace (design §3): one verb per inbound kind.
		// requests/snapshot are the two still unbuilt; naming one reports that
		// instead of "unknown".
		if len(rest) == 0 {
			return nil, fmt.Errorf("session stream: sub-verb required (attach|turn|approve|interrupt|finish <id>)")
		}
		if rest[0] == "requests" || rest[0] == "snapshot" {
			return nil, fmt.Errorf("session stream %s: specified (design §3) but not built yet", rest[0])
		}
		return parseViaPath([]string{"session", "stream", rest[0]}, rest[1:])
	}
	return parseViaPath([]string{"session", sub}, rest)
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

// parseNotify parses `notify` from the declaration.
//
// It used to be a positional grammar here -- `notify [info|warn|error] <title>
// [text...]` -- while the CLI took --level and --title. Same verb name, two
// grammars: the declaration's own example, `notify --level warn --title build
// the tree is red`, parsed on this surface as a notification TITLED "--level".
// D21 settles it on the CLI's spelling.
func parseNotify(args []string) (Action, error) {
	return parseViaPath([]string{"notify"}, args)
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
	return parseViaSpec2("file", args[0], args[1:])
}

// parseForward handles `forward ls` and `forward kill <id>`. Starting a
// forward has no cmdline verb at all — not for -L/-R (`p`/`b`, stopped by
// `P`/`B`) and not for the newer raw connect (`t`, tui/rawforward.go): all
// three are modal-only, matching the fact that `p`/`b` never had a `forward
// open`-shaped verb here either. This stays the list/kill surface only.
func parseForward(args []string) (Action, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("forward: sub-verb required (ls | kill | tap)")
	}
	// Opening a forward is not reachable from here: the TUI opens one from its
	// port-forward pane, which is where the local listener it binds belongs.
	return parseViaSpec2("forward", args[0], args[1:])
}

// parseExecRun handles `exec <task-id> [--] <cmd>...`, `exec ls [-task <id>]`
// and `exec kill <exec-id>`.
//
// The sub-verb is decided by the FIRST token only. `exec <id> ls -la` is a
// command named ls, not a listing, because a task id introduced it — the same
// rule harness-cli's splitExecArgv follows, and the reason `exec ls` can never
// be ambiguous with a task whose command happens to be `ls`.
//
// Everything after the task id is the argv VERBATIM. It is never re-joined and
// re-split: an argument containing a space is one argument, and splitting it
// would silently change the command. A bare `--` ends any option scanning, so
// a command that starts with a dash still reaches the runner intact.
func parseExecRun(args []string) (Action, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("exec: usage: exec [--shell] [--sshd-parent] <task-id> -- <command> [args...]")
	}
	if args[0] == "ls" || args[0] == "kill" {
		return parseViaSpec2("exec", args[0], args[1:])
	}
	return parseViaSpec("exec", args)
}

// parseSSHGateway routes the TUI's start/stop pair through the declaration.
// Bare `ssh-gateway` is the status report, which has no CLI counterpart and so
// no path of its own.
func parseSSHGateway(args []string) (Action, error) {
	if len(args) == 0 {
		args = []string{"status"}
	}
	act, err := parseViaPath([]string{"ssh-gateway", args[0]}, args[1:])
	if err != nil {
		return nil, err
	}
	g := act.(verb.SSHGatewayAction)
	if g.Sub == "start" && g.Listen == "" {
		g.Listen = sshgw.DefaultListen
	}
	return g, nil
}

// parseWorkspace handles the `workspace <sub> [name]` family.
//
// `workspace save` requires a name. Defaulting it to the installed workspace
// would let a slip overwrite one from the live client state; every other verb
// is read-only or re-runs what is already installed, so they may default.
func parseWorkspace(args []string) (Action, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("workspace: sub-verb required (save | rm | ls | show | apply | detach)")
	}
	return parseViaSpec2("workspace", args[0], args[1:])
}

// parseGit parses `git <task-id> {log|diff|show|status} ...` with the same
// grammar harness-cli uses, so a hand that learned one surface knows the other.
func parseGit(args []string) (Action, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("git: usage: git <task-id> {log | diff | show | status | subrepos | file} ...")
	}
	// The task id sits between the family word and the sub-verb, and the
	// pathspec is peeled before flags are read -- both are grammar, and both
	// now live in cli/verb rather than being spelled out again here.
	taskID, sub := args[0], args[1]
	act, err := parseViaSpec2("git", sub, args[2:])
	if err != nil {
		return nil, err
	}
	g := act.(verb.GitAction)
	g.TaskID = taskID
	return g, nil
}

// parseSetCaps and parseSetParent parse from the declaration.
//
// Both were hand-written token walks, and both diverged from it: `caps set
// --caps X --scope-for Y` was refused by the CLI (a narrowing needs a base to
// narrow) and ACCEPTED here, where cli/set_caps.go then writes Overrides only
// when Scope is non-nil -- so the operator typed --scope-for, saw no error,
// and nothing happened. `--caps=X` and `--parent=<id>` were refused here and
// accepted everywhere else.
func parseSetCaps(args []string) (Action, error) {
	return parseViaPath([]string{"caps", "set"}, args)
}

func parseSetParent(args []string) (Action, error) {
	return parseViaPath([]string{"caps", "set-parent"}, args)
}

func parseGrid(args []string) (Action, error) {
	return parseViaSpec("grid", args)
}

// parseViaSpec2 is parseViaSpec for a two-word verb path (`file push`,
// `git diff`, `session new`). Split from the one-word form only because a
// variadic path would make every call site pass a slice literal.
func parseViaSpec2(head, sub string, args []string) (Action, error) {
	return parseViaPath([]string{head, sub}, args)
}

// parseViaPath parses any declared path, however many words it is. The
// two-word wrapper above came first and `session stream turn` is three, which
// is how the whole stream namespace ended up hand-parsed.
func parseViaPath(path []string, args []string) (Action, error) {
	sp, ok := verb.Lookup(path...)
	if !ok {
		return nil, fmt.Errorf("%s: unknown sub-verb %q", path[0], strings.Join(path[1:], " "))
	}
	sp = sp.For(verb.TUI)
	fs := sp.NewFlagSet(flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	b, err := sp.Parse(fs, args)
	if err != nil {
		return nil, err // Parse names the verb itself
	}
	return sp.BuildFunc()(b)
}

// parseSpawnTUI parses one of the three spawn verbs from the declaration.
//
// defaultRepo is the surface-context tier of --repo's ladder: the TUI knows a
// repo from its session, the WebUI from a dropdown, and the CLI from nothing.
// One ladder, three injections, instead of three ladders.
func parseSpawnTUI(kind string, args []string, defaultRepo string) (Action, error) {
	path := []string{kind}
	if kind == "session-new" {
		path = []string{"session", "new"}
	}
	sp, ok := verb.Lookup(path...)
	if !ok {
		return nil, fmt.Errorf("%s: not in the verb table", kind)
	}
	sp = sp.For(verb.TUI)
	fs := sp.NewFlagSet(flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	b, err := sp.Parse(fs, args)
	if err != nil {
		return nil, err // Parse names the verb itself
	}
	act, err := sp.BuildFunc()(b)
	if err != nil {
		return nil, err
	}
	a := act.(verb.SpawnAction)
	a.Repo = sp.Resolve(b, "repo", nil, nil, map[string]string{"repo": defaultRepo})
	return a, nil
}
