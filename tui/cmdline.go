package tui

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/shlex"
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
type SessionNewAction struct {
	Repo         string
	ExtraArgs    []string
	ResumeTaskID string
	Detach       bool
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

// InteractiveAction opens an interactive PTY claude session in Repo —
// the slash-command equivalent of the 'i' key, useful when chaining
// after /repo or when the user is already in cmdline focus.
type InteractiveAction struct {
	Repo         string
	ExtraArgs    []string
	ResumeTaskID string
}

func (SubmitAction) isAction()        {}
func (CancelAction) isAction()        {}
func (PruneAction) isAction()         {}
func (ClearAction) isAction()         {}
func (QuitAction) isAction()          {}
func (HelpAction) isAction()          {}
func (RepoAction) isAction()          {}
func (InteractiveAction) isAction()   {}
func (SessionNewAction) isAction()    {}
func (SessionAttachAction) isAction() {}
func (SessionLsAction) isAction()     {}
func (SessionKillAction) isAction()   {}

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
	default:
		return nil, fmt.Errorf("unknown command: %q", tokens[0])
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
		var extra repeatableStrings
		fs.Var(&extra, "claude-arg", "extra CLI arg forwarded to claude (repeatable)")
		if err := fs.Parse(rest); err != nil {
			return nil, fmt.Errorf("session new: %w", err)
		}
		if fs.NArg() > 0 {
			return nil, fmt.Errorf("session new: unexpected argument %q", fs.Arg(0))
		}
		return SessionNewAction{Repo: defaultRepo, ExtraArgs: []string(extra), ResumeTaskID: *resume, Detach: *detach}, nil
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
