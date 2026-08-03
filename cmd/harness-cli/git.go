package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// splitPathspec peels "-- <path...>" off the tail. It must run BEFORE the
// permuted flag parse: Go's flag package consumes a bare "--" as its
// end-of-flags marker, so a pathspec left in the argv would silently vanish.
// Everything after the separator is joined with a space so an unquoted path
// containing spaces still arrives whole.
func splitPathspec(args []string) ([]string, string) {
	for i, a := range args {
		if a == "--" {
			return args[:i], strings.Join(args[i+1:], " ")
		}
	}
	return args, ""
}

// gitCommonFlags registers the knobs every sub-verb accepts and returns a
// closure that folds them into a GitQuery. Registering them per sub-verb (not
// once globally) keeps each FlagSet's own -h honest about what it takes.
func gitCommonFlags(fs *flag.FlagSet, submodule bool) func(cli.GitQuery) cli.GitQuery {
	subrepo := fs.String("subrepo", "", "run the query inside this worktree-relative directory, which must itself be a git repo root")
	var sub *bool
	if submodule {
		sub = fs.Bool("submodule", false, "inline a submodule's own file-level changes (the output is then not an applyable patch)")
	}
	return func(q cli.GitQuery) cli.GitQuery {
		q.Subrepo = *subrepo
		if sub != nil {
			q.SubmoduleDiff = *sub
		}
		return q
	}
}

func runGit(cid objproto.ConnectionID, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: harness-cli git <task-id> {log|diff|show|status|subrepos|file} ...")
	}
	taskID := args[0]
	sub := args[1]
	rest, path := splitPathspec(args[2:])

	ctx := context.Background()
	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()

	color := isTTY(os.Stdout)
	emit := func(res *cli.GitResult, err error) error {
		if err != nil {
			return err
		}
		if err := res.Err(); err != nil {
			return err
		}
		renderGitResult(os.Stdout, res, color)
		return nil
	}

	switch sub {
	case "log":
		fs := flag.NewFlagSet("git log", flag.ExitOnError)
		max := fs.Uint("max", 0, "maximum commits (0 = 100, capped at 1000)")
		common := gitCommonFlags(fs, false)
		pos, err := parsePermuted(fs, rest)
		if err != nil {
			return err
		}
		if len(pos) > 1 {
			return fmt.Errorf("git log: at most one revision (got %d)", len(pos))
		}
		base := ""
		if len(pos) == 1 {
			base = pos[0]
		}
		return emit(c.GitLog(ctx, taskID, common(cli.GitQuery{
			BaseRev: base, Path: path, MaxCommits: uint32(*max),
		})))

	case "diff":
		fs := flag.NewFlagSet("git diff", flag.ExitOnError)
		staged := fs.Bool("staged", false, "diff the index instead of the working tree")
		fs.BoolVar(staged, "cached", false, "alias for --staged")
		maxBytes := fs.Uint("max-bytes", 0, "maximum diff bytes (0 = 2MiB, capped at 8MiB)")
		common := gitCommonFlags(fs, true)
		pos, err := parsePermuted(fs, rest)
		if err != nil {
			return err
		}
		// Positionals are counted the way git counts them: none = unstaged,
		// one = <base> against the working tree, two = commit against commit.
		base, targetRev := "", ""
		target := protocol.GitDiffTarget_Worktree
		if *staged {
			target = protocol.GitDiffTarget_Index
		}
		switch len(pos) {
		case 0:
		case 1:
			base = pos[0]
		case 2:
			if *staged {
				return fmt.Errorf("git diff: --staged names the index as the right-hand side, so a second revision has nowhere to go")
			}
			base, targetRev = pos[0], pos[1]
			target = protocol.GitDiffTarget_Rev
		default:
			return fmt.Errorf("git diff: at most two revisions (got %d)", len(pos))
		}
		return emit(c.GitDiff(ctx, taskID, common(cli.GitQuery{
			Target: target, BaseRev: base, TargetRev: targetRev,
			Path: path, MaxBytes: uint32(*maxBytes),
		})))

	case "show":
		fs := flag.NewFlagSet("git show", flag.ExitOnError)
		maxBytes := fs.Uint("max-bytes", 0, "maximum bytes (0 = 2MiB, capped at 8MiB)")
		common := gitCommonFlags(fs, true)
		pos, err := parsePermuted(fs, rest)
		if err != nil {
			return err
		}
		if len(pos) > 1 {
			return fmt.Errorf("git show: at most one revision (got %d)", len(pos))
		}
		rev := ""
		if len(pos) == 1 {
			rev = pos[0]
		}
		return emit(c.GitShow(ctx, taskID, common(cli.GitQuery{
			BaseRev: rev, Path: path, MaxBytes: uint32(*maxBytes),
		})))

	case "status":
		fs := flag.NewFlagSet("git status", flag.ExitOnError)
		common := gitCommonFlags(fs, false)
		pos, err := parsePermuted(fs, rest)
		if err != nil {
			return err
		}
		if len(pos) > 0 {
			return fmt.Errorf("git status: takes no revision (got %q)", pos[0])
		}
		return emit(c.GitStatus(ctx, taskID, common(cli.GitQuery{Path: path})))

	case "file":
		fs := flag.NewFlagSet("git file", flag.ExitOnError)
		staged := fs.Bool("staged", false, "the staged blob instead of the file on disk")
		rev := fs.String("rev", "", "the blob at this revision instead of the file on disk")
		maxBytes := fs.Uint("max-bytes", 0, "maximum bytes (0 = 2MiB, capped at 8MiB)")
		common := gitCommonFlags(fs, false)
		pos, err := parsePermuted(fs, rest)
		if err != nil {
			return err
		}
		// The path may come as a positional or after --, so a path lifted
		// straight out of a diff header works either way.
		target := protocol.GitDiffTarget_Worktree
		switch {
		case *rev != "":
			target = protocol.GitDiffTarget_Rev
		case *staged:
			target = protocol.GitDiffTarget_Index
		}
		if len(pos) > 1 {
			return fmt.Errorf("git file: one path (got %d)", len(pos))
		}
		if len(pos) == 1 {
			if path != "" {
				return fmt.Errorf("git file: path given twice (%q and %q)", pos[0], path)
			}
			path = pos[0]
		}
		if path == "" {
			return fmt.Errorf("usage: harness-cli git <task-id> file [--staged | --rev REV] <path>")
		}
		return emit(c.GitFile(ctx, taskID, common(cli.GitQuery{
			Target: target, TargetRev: *rev, Path: path, MaxBytes: uint32(*maxBytes),
		})))

	case "subrepos":
		fs := flag.NewFlagSet("git subrepos", flag.ExitOnError)
		common := gitCommonFlags(fs, false)
		pos, err := parsePermuted(fs, rest)
		if err != nil {
			return err
		}
		if len(pos) > 0 {
			return fmt.Errorf("git subrepos: takes no revision (got %q)", pos[0])
		}
		return emit(c.GitSubrepos(ctx, taskID, common(cli.GitQuery{})))

	default:
		return fmt.Errorf("git: unknown sub-verb %q (log | diff | show | status | subrepos | file)", sub)
	}
}

const (
	ansiReset = "\x1b[0m"
	ansiGreen = "\x1b[32m"
	ansiRed   = "\x1b[31m"
	ansiCyan  = "\x1b[36m"
	ansiBold  = "\x1b[1m"
	ansiDim   = "\x1b[2m"
)

func gitLineANSI(class cli.GitLineClass) string {
	switch class {
	case cli.GitLineAdd:
		return ansiGreen
	case cli.GitLineDel:
		return ansiRed
	case cli.GitLineHunk:
		return ansiCyan
	case cli.GitLineFile:
		return ansiBold
	case cli.GitLineMeta:
		return ansiDim
	default:
		return ""
	}
}

func renderGitResult(w io.Writer, res *cli.GitResult, color bool) {
	switch res.Kind {
	case protocol.GitQueryKind_Log:
		for _, c := range res.Commits {
			fmt.Fprintf(w, "%s  %s  %-12s  %s\n",
				c.Short(), c.When.Format("2006-01-02 15:04"), c.Author, c.Subject)
		}
		if res.Truncated {
			fmt.Fprintln(w, "(truncated: more commits exist; raise --max)")
		}
	case protocol.GitQueryKind_Status:
		for _, e := range res.Entries {
			fmt.Fprintf(w, "%s %s\n", e.XY, e.Path)
		}
	case protocol.GitQueryKind_File:
		// Whole-file content: no diff colouring, and no trailing-newline
		// fiddling — it goes out as the bytes it is.
		fmt.Fprint(w, res.Text)
		if res.Truncated {
			fmt.Fprintln(w, "\n(truncated: raise --max-bytes to see more)")
		}
	case protocol.GitQueryKind_Subrepos:
		for _, p := range res.Subrepos {
			fmt.Fprintln(w, p)
		}
		if len(res.Subrepos) == 0 {
			fmt.Fprintln(w, "(no nested repositories)")
		}
		if res.Truncated {
			fmt.Fprintln(w, "(truncated: more nested repositories exist than the walk reports)")
		}
	default:
		// A trailing newline in the text would otherwise print as a blank
		// line, which reads as content that is not there.
		text := strings.TrimSuffix(res.Text, "\n")
		if text != "" {
			for _, line := range strings.Split(text, "\n") {
				if !color {
					fmt.Fprintln(w, line)
					continue
				}
				if prefix := gitLineANSI(cli.ClassifyGitLine(line)); prefix != "" {
					fmt.Fprintf(w, "%s%s%s\n", prefix, line, ansiReset)
				} else {
					fmt.Fprintln(w, line)
				}
			}
		}
		if res.Truncated {
			fmt.Fprintln(w, "(truncated: raise --max-bytes to see more)")
		}
	}
}

// isTTY reports whether f is a terminal, which is the only condition under
// which colour escapes belong in the output — a redirected diff has to stay
// byte-clean for `git apply`.
func isTTY(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}
