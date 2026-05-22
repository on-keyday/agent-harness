package tui

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/shlex"
	"github.com/on-keyday/agent-harness/cli"
)

// Action is the typed result of parsing one cmdline input.
// app.go switches on the concrete type.
type Action interface{ isAction() }

type SubmitAction struct {
	Repo         string
	Prompt       string
	ExtraArgs    []string
	ResumeTaskID string
}

type CancelAction struct {
	IDPrefix string
}

type PruneAction struct {
	Before time.Duration
}

type ClearAction struct{}
type QuitAction struct{}
type HelpAction struct{}

// SessionNewAction opens a new detachable interactive PTY session.
// When Detach is true the session is opened and the local stream is closed
// immediately (Docker-style background start); the task id is printed to
// cmdresult and the TUI is not suspended.
//
// Host / Runner / IP carry the runner pin selector, mutually exclusive and
// validated at parse time via cli.SelectorOpts.ValidateSelector.
type SessionNewAction struct {
	Repo         string
	ExtraArgs    []string
	ResumeTaskID string
	Detach       bool
	Host         string
	Runner       string
	IP           string
}

// SessionAttachAction re-attaches to an existing detachable session by ID.
type SessionAttachAction struct {
	TaskID string
}

// SessionLsAction lists interactive+detachable tasks in the cmdresult area.
type SessionLsAction struct{}

// SessionKillAction is an alias for CancelAction targeting a session.
// It reuses CancelAction so app.go's existing cancel dispatch handles it.
type SessionKillAction struct {
	IDPrefix string
}

// RepoAction switches the TUI session's default repo. Subsequent submit
// popups, interactive opens, and slash-command --repo defaults all use the
// new value. Per-action --repo overrides still win on a single call.
type RepoAction struct {
	Path string
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
// (dir_push).
type FilePushAction struct {
	TaskID    string
	LocalSrc  string
	RemoteDst string
	Recursive bool
	Force     bool
}

// FilePullAction copies from a task's worktree to a local destination.
// Recursive uses dir_pull. Force permits overwriting the local path.
type FilePullAction struct {
	TaskID    string
	RemoteSrc string
	LocalDst  string
	Recursive bool
	Force     bool
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

// InteractiveAction opens an interactive PTY claude session in Repo —
// the slash-command equivalent of the 'i' key, useful when chaining
// after /repo or when the user is already in cmdline focus.
type InteractiveAction struct {
	Repo         string
	ExtraArgs    []string
	ResumeTaskID string
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

func (SubmitAction) isAction()           {}
func (CancelAction) isAction()           {}
func (PruneAction) isAction()            {}
func (ClearAction) isAction()            {}
func (QuitAction) isAction()             {}
func (HelpAction) isAction()             {}
func (RepoAction) isAction()             {}
func (InteractiveAction) isAction()      {}
func (SessionNewAction) isAction()       {}
func (SessionAttachAction) isAction()    {}
func (SessionLsAction) isAction()        {}
func (SessionKillAction) isAction()      {}
func (FileLsAction) isAction()           {}
func (FilePushAction) isAction()         {}
func (FilePullAction) isAction()         {}
func (FileDeleteAction) isAction()       {}
func (ServerDialRunnerAction) isAction() {}

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
	case "server":
		return parseServer(tokens[1:])
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
	var extra repeatableStrings
	fs.Var(&extra, "claude-arg", "extra CLI arg forwarded to claude (repeatable)")
	if err := fs.Parse(args); err != nil {
		return nil, fmt.Errorf("interactive: %w", err)
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("interactive: unexpected positional argument %q", fs.Arg(0))
	}
	return InteractiveAction{Repo: *repo, ExtraArgs: []string(extra), ResumeTaskID: *resume}, nil
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
	var extra repeatableStrings
	fs.Var(&extra, "claude-arg", "extra CLI arg forwarded to claude (repeatable)")
	if err := fs.Parse(args); err != nil {
		return nil, fmt.Errorf("submit: %w", err)
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return nil, fmt.Errorf("submit: prompt is required")
	}
	return SubmitAction{Repo: *repo, Prompt: strings.Join(rest, " "), ExtraArgs: []string(extra), ResumeTaskID: *resume}, nil
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
	if err := fs.Parse(args); err != nil {
		return nil, fmt.Errorf("prune: %w", err)
	}
	return PruneAction{Before: *before}, nil
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
		detach := fs.Bool("detach", false, "start the session and immediately detach (run in background, print task id)")
		host := fs.String("host", "", "pin to a runner by reported hostname (mutually exclusive with --runner / --ip)")
		runner := fs.String("runner", "", "pin to a runner by 32-hex RunnerID (mutually exclusive with --host / --ip)")
		ip := fs.String("ip", "", "pin to a runner by IP address (mutually exclusive with --host / --runner)")
		var extra repeatableStrings
		fs.Var(&extra, "claude-arg", "extra CLI arg forwarded to claude (repeatable)")
		if err := fs.Parse(rest); err != nil {
			return nil, fmt.Errorf("session new: %w", err)
		}
		if fs.NArg() > 0 {
			return nil, fmt.Errorf("session new: unexpected argument %q", fs.Arg(0))
		}
		if err := (cli.SelectorOpts{Runner: *runner, Host: *host, IP: *ip}).ValidateSelector(); err != nil {
			return nil, fmt.Errorf("session new: %w", err)
		}
		return SessionNewAction{
			Repo:         defaultRepo,
			ExtraArgs:    []string(extra),
			ResumeTaskID: *resume,
			Detach:       *detach,
			Host:         *host,
			Runner:       *runner,
			IP:           *ip,
		}, nil
	case "attach":
		if len(rest) == 0 {
			return nil, fmt.Errorf("session attach: task id required")
		}
		if len(rest) > 1 {
			return nil, fmt.Errorf("session attach: too many arguments (got %d, want 1)", len(rest))
		}
		return SessionAttachAction{TaskID: rest[0]}, nil
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
	default:
		return nil, fmt.Errorf("session: unknown sub-verb %q (new | attach <id> | ls | kill <id>)", verb)
	}
}

// parseFile dispatches file sub-verbs: ls / push / pull / delete. All
// paths use the same -r / --recursive and -f / --force aliases as the
// CLI so the typing is interchangeable between `harness-cli file ...`
// and the TUI cmdline. Local paths are resolved on the host running
// the TUI; remote paths are interpreted relative to the task's
// worktree by the runner and confined to it (no `..` escape).
func parseFile(args []string) (Action, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("file: sub-verb required (ls | push | pull | delete)")
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
		if err := fs.Parse(rest); err != nil {
			return nil, fmt.Errorf("file push: %w", err)
		}
		pargs := fs.Args()
		if len(pargs) != 3 {
			return nil, fmt.Errorf("file push: usage: file push [-r] [-f] <task-id> <local-src> <worktree-rel-dst>")
		}
		return FilePushAction{
			TaskID: pargs[0], LocalSrc: pargs[1], RemoteDst: pargs[2],
			Recursive: *recursive, Force: *force,
		}, nil
	case "pull":
		fs := flag.NewFlagSet("file pull", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		recursive := fs.Bool("recursive", false, "")
		fs.BoolVar(recursive, "r", false, "")
		force := fs.Bool("force", false, "")
		fs.BoolVar(force, "f", false, "")
		if err := fs.Parse(rest); err != nil {
			return nil, fmt.Errorf("file pull: %w", err)
		}
		pargs := fs.Args()
		if len(pargs) != 3 {
			return nil, fmt.Errorf("file pull: usage: file pull [-r] [-f] <task-id> <worktree-rel-src> <local-dst>")
		}
		return FilePullAction{
			TaskID: pargs[0], RemoteSrc: pargs[1], LocalDst: pargs[2],
			Recursive: *recursive, Force: *force,
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
	default:
		return nil, fmt.Errorf("file: unknown sub-verb %q (ls | push | pull | delete)", verb)
	}
}
