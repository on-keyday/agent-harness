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
	Repo   string
	Prompt string
}

type CancelAction struct {
	IDPrefix string
}

type PruneAction struct {
	Before  time.Duration
	Offline bool
}

type ClearAction struct{}
type QuitAction struct{}
type HelpAction struct{}

func (SubmitAction) isAction() {}
func (CancelAction) isAction() {}
func (PruneAction) isAction()  {}
func (ClearAction) isAction()  {}
func (QuitAction) isAction()   {}
func (HelpAction) isAction()   {}

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
	default:
		return nil, fmt.Errorf("unknown command: %q", tokens[0])
	}
}

func parseSubmit(args []string, defaultRepo string) (Action, error) {
	fs := flag.NewFlagSet("submit", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repo := fs.String("repo", defaultRepo, "")
	if err := fs.Parse(args); err != nil {
		return nil, fmt.Errorf("submit: %w", err)
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return nil, fmt.Errorf("submit: prompt is required")
	}
	return SubmitAction{Repo: *repo, Prompt: strings.Join(rest, " ")}, nil
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
	offline := fs.Bool("offline", false, "")
	if err := fs.Parse(args); err != nil {
		return nil, fmt.Errorf("prune: %w", err)
	}
	return PruneAction{Before: *before, Offline: *offline}, nil
}
