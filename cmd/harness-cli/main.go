//go:build !js

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/agent"
	"github.com/on-keyday/agent-harness/cli/cliopts"
	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func main() {
	serverCID := flag.String("server-cid", "",
		"server ConnectionID (env: HARNESS_SERVER_CID; default ws:127.0.0.1:8539-*)")
	wsPath := flag.String("ws-path", "", "WebSocket URL path (env: HARNESS_WS_PATH; default /ws)")
	flag.Usage = usage
	flag.Parse()
	resolvedWS := cliopts.ResolveString(*wsPath, "HARNESS_WS_PATH")
	if resolvedWS == "" {
		resolvedWS = "/ws"
	}
	cli.WebSocketPath = resolvedWS

	if flag.NArg() == 0 {
		usage()
		os.Exit(2)
	}
	sub := flag.Arg(0)
	args := flag.Args()[1:]
	ctx := context.Background()

	parseCID := func() objproto.ConnectionID {
		val := *serverCID
		if val == "" && os.Getenv("HARNESS_SERVER_CID") == "" {
			val = "ws:127.0.0.1:8539-*"
		}
		cid, err := cliopts.ResolveServerCID(val)
		if err != nil {
			die(err)
		}
		return cid
	}

	// addClaudeArgFlag registers --claude-arg as a repeatable flag and returns
	// the underlying slice (populated after fs.Parse). Each occurrence appends
	// one CLI argument forwarded verbatim to the spawned claude process; e.g.
	//   submit --claude-arg --resume --claude-arg <uuid>
	// works around the 2.1.123 /resume picker regression that requires the
	// caller to be in the original CWD by letting the user push --resume
	// through harness-cli without an interactive picker.
	addClaudeArgFlag := func(fs *flag.FlagSet) *[]string {
		var args repeatableStrings
		fs.Var(&args, "claude-arg", "extra CLI arg to forward to claude (repeatable; appended after runner-global --claude-args)")
		return (*[]string)(&args)
	}

	// addSelectorFlags registers --runner/--host/--ip on fs and returns a
	// function that resolves them to a RunnerSelector after fs.Parse.
	addSelectorFlags := func(fs *flag.FlagSet) func() protocol.RunnerSelector {
		runner := fs.String("runner", "", "pin to a specific runner by ConnectionID hex")
		host := fs.String("host", "", "pin to runner by hostname")
		ip := fs.String("ip", "", "pin to runner by IP address")
		return func() protocol.RunnerSelector {
			opts := cli.SelectorOpts{Runner: *runner, Host: *host, IP: *ip}
			if err := opts.ValidateSelector(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
			sel, err := cli.BuildSelector(opts)
			if err != nil {
				die(err)
			}
			return sel
		}
	}

	switch sub {
	case "submit":
		fs := flag.NewFlagSet("submit", flag.ExitOnError)
		repo := fs.String("repo", "", "repo identifier (env: HARNESS_REPO_PATH); must match a runner-registered RepoPath verbatim")
		task := fs.String("task", "", "prompt text")
		resume := fs.String("resume", "", "task id (32 hex) to resume — server reuses the id and worktree branch so claude's project key matches the previous run; --repo is ignored")
		resolveSelector := addSelectorFlags(fs)
		extraArgs := addClaudeArgFlag(fs)
		fs.Parse(args)
		if *task == "" {
			fmt.Fprintln(os.Stderr, "submit: --task is required")
			os.Exit(2)
		}
		repoVal := cliopts.ResolveString(*repo, "HARNESS_REPO_PATH")
		if repoVal == "" && *resume == "" {
			fmt.Fprintln(os.Stderr, "submit: --repo or HARNESS_REPO_PATH required (must match a runner's RepoPath verbatim) — except when --resume is set, which uses the existing task's repo")
			os.Exit(2)
		}
		sel := resolveSelector()
		// Hand-rolled Dial→SayHello→Submit→Close so the server records
		// kind=cli on this connection. Used by ii (origin tracking) so
		// the resulting task is attributed to "cli" in `harness-cli ls`.
		c, err := cli.Dial(ctx, parseCID())
		if err != nil {
			die(err)
		}
		defer c.Close()
		if err := c.SayHello(ctx, protocol.ClientKind_Cli); err != nil {
			die(err)
		}
		id, err := c.SubmitWithSelectorAndArgs(ctx, repoVal, *task, sel, *extraArgs, *resume)
		if err != nil {
			die(err)
		}
		fmt.Println(id)

	case "ls":
		if err := cli.List(ctx, parseCID(), os.Stdout); err != nil {
			die(err)
		}

	case "cancel":
		if len(args) == 0 {
			fmt.Fprintln(os.Stderr, "cancel: missing task id")
			os.Exit(2)
		}
		if err := cli.Cancel(ctx, parseCID(), args[0]); err != nil {
			die(err)
		}

	case "prune":
		fs := flag.NewFlagSet("prune", flag.ExitOnError)
		before := fs.Duration("before", 7*24*time.Hour, "forget terminal tasks older than this")
		fs.Parse(args)
		if err := cli.Prune(ctx, parseCID(), *before, os.Stdout); err != nil {
			die(err)
		}

	case "prune-local":
		fs := flag.NewFlagSet("prune-local", flag.ExitOnError)
		repo := fs.String("repo", ".", "repo to prune (env: HARNESS_REPO_PATH; default \".\")")
		before := fs.Duration("before", 7*24*time.Hour, "remove worktrees older than this")
		fs.Parse(args)
		repoVal := *repo
		if repoVal == "." {
			if env := os.Getenv("HARNESS_REPO_PATH"); env != "" {
				repoVal = env
			}
		}
		abs, err := filepath.Abs(repoVal)
		if err != nil {
			die(err)
		}
		if err := cli.PruneLocal(ctx, abs, *before, os.Stdout); err != nil {
			die(err)
		}

	case "logs":
		if len(args) == 0 {
			fmt.Fprintln(os.Stderr, "logs: missing task id")
			os.Exit(2)
		}
		if err := cli.Logs(ctx, parseCID(), args[0], os.Stdout); err != nil {
			die(err)
		}

	case "watch":
		if err := cli.Watch(ctx, parseCID(), os.Stdout); err != nil {
			die(err)
		}

	case "interactive":
		fs := flag.NewFlagSet("interactive", flag.ExitOnError)
		repo := fs.String("repo", "", "repo identifier (env: HARNESS_REPO_PATH); must match a runner-registered RepoPath verbatim")
		resume := fs.String("resume", "", "task id (32 hex) of a terminal interactive task to resume; --repo is ignored")
		resolveSelector := addSelectorFlags(fs)
		extraArgs := addClaudeArgFlag(fs)
		fs.Parse(args)
		repoVal := cliopts.ResolveString(*repo, "HARNESS_REPO_PATH")
		if repoVal == "" && *resume == "" {
			fmt.Fprintln(os.Stderr, "interactive: --repo or HARNESS_REPO_PATH required (must match a runner's RepoPath verbatim) — except when --resume is set, which uses the existing task's repo")
			os.Exit(2)
		}
		sel := resolveSelector()
		// Hand-rolled Dial→SayHello→Interactive→Close so the server
		// records kind=cli on this connection (origin attribution).
		c, err := cli.Dial(ctx, parseCID())
		if err != nil {
			die(err)
		}
		defer c.Close()
		if err := c.SayHello(ctx, protocol.ClientKind_Cli); err != nil {
			die(err)
		}
		if _, err := c.InteractiveWithSelectorAndArgs(ctx, repoVal, sel, *extraArgs, *resume, false); err != nil {
			die(err)
		}

	case "file":
		if len(args) == 0 {
			fmt.Fprintln(os.Stderr, "usage: harness-cli file {push|pull|ls} ...")
			os.Exit(2)
		}
		fsub := args[0]
		rest := args[1:]
		c, err := cli.Dial(ctx, parseCID())
		if err != nil {
			die(err)
		}
		defer c.Close()
		if err := c.SayHello(ctx, protocol.ClientKind_Cli); err != nil {
			die(err)
		}
		switch fsub {
		case "push":
			fs := flag.NewFlagSet("file push", flag.ExitOnError)
			recursive := fs.Bool("recursive", false, "transfer a directory tree")
			fs.BoolVar(recursive, "r", false, "alias for --recursive")
			force := fs.Bool("force", false, "overwrite existing destination")
			fs.BoolVar(force, "f", false, "alias for --force")
			fs.Parse(rest)
			pargs := fs.Args()
			if len(pargs) != 3 {
				fmt.Fprintln(os.Stderr, "usage: harness-cli file push [-r] [-f] <task-id> <local-src> <worktree-rel-dst>")
				os.Exit(2)
			}
			if *recursive {
				if err := c.FilePushDir(ctx, pargs[0], pargs[1], pargs[2], *force); err != nil {
					die(err)
				}
			} else {
				if err := c.FilePush(ctx, pargs[0], pargs[1], pargs[2], *force); err != nil {
					die(err)
				}
			}
		case "pull":
			fs := flag.NewFlagSet("file pull", flag.ExitOnError)
			recursive := fs.Bool("recursive", false, "transfer a directory tree")
			fs.BoolVar(recursive, "r", false, "alias for --recursive")
			force := fs.Bool("force", false, "overwrite existing destination")
			fs.BoolVar(force, "f", false, "alias for --force")
			fs.Parse(rest)
			pargs := fs.Args()
			if len(pargs) != 3 {
				fmt.Fprintln(os.Stderr, "usage: harness-cli file pull [-r] [-f] <task-id> <worktree-rel-src> <local-dst>")
				os.Exit(2)
			}
			if *recursive {
				if err := c.FilePullDir(ctx, pargs[0], pargs[1], pargs[2], *force); err != nil {
					die(err)
				}
			} else {
				if err := c.FilePull(ctx, pargs[0], pargs[1], pargs[2], *force); err != nil {
					die(err)
				}
			}
		case "ls":
			if len(rest) < 1 || len(rest) > 2 {
				fmt.Fprintln(os.Stderr, "usage: harness-cli file ls <task-id> [<worktree-rel-dir>]")
				os.Exit(2)
			}
			rel := ""
			if len(rest) == 2 {
				rel = rest[1]
			}
			if err := c.FileLs(ctx, rest[0], rel, os.Stdout); err != nil {
				die(err)
			}
		case "delete":
			fs := flag.NewFlagSet("file delete", flag.ExitOnError)
			recursive := fs.Bool("recursive", false, "target a directory tree instead of a single file (uses dir_delete)")
			fs.BoolVar(recursive, "r", false, "alias for --recursive")
			force := fs.Bool("force", false, "with -r: delete non-empty directory contents recursively (os.RemoveAll). Ignored without -r.")
			fs.BoolVar(force, "f", false, "alias for --force")
			fs.Parse(rest)
			pargs := fs.Args()
			if len(pargs) != 2 {
				fmt.Fprintln(os.Stderr, "usage: harness-cli file delete [-r [-f]] <task-id> <worktree-rel-path>")
				os.Exit(2)
			}
			if *recursive {
				if err := c.FileDeleteDir(ctx, pargs[0], pargs[1], *force); err != nil {
					die(err)
				}
			} else {
				if err := c.FileDelete(ctx, pargs[0], pargs[1]); err != nil {
					die(err)
				}
			}
		default:
			fmt.Fprintf(os.Stderr, "unknown file subcommand: %s\n", fsub)
			os.Exit(2)
		}

	case "session":
		if err := runSession(parseCID(), args); err != nil {
			die(err)
		}

	case "agent":
		if len(args) == 0 {
			agentUsage()
			os.Exit(2)
		}
		asub := args[0]
		rest := args[1:]
		var err error
		switch asub {
		case "send":
			err = agent.Send(ctx, rest, os.Stdin, os.Stdout)
		case "wait":
			err = agent.Wait(ctx, rest, os.Stdout)
		case "inbox":
			err = agent.Inbox(ctx, rest, os.Stdout)
		case "subscribe":
			err = agent.Subscribe(ctx, rest, os.Stdout)
		case "unsubscribe":
			err = agent.Unsubscribe(ctx, rest, os.Stdout)
		case "dispatch":
			err = agent.Dispatch(ctx, rest, os.Stdin, os.Stdout)
		case "topics":
			err = agent.Topics(ctx, rest, os.Stdout)
		case "subscriptions":
			err = agent.Subscriptions(ctx, rest, os.Stdout)
		default:
			agentUsage()
			os.Exit(2)
		}
		if err != nil {
			die(err)
		}

	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: harness-cli [--server-cid CID] [--ws-path PATH] <subcommand> [args]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Global flags fall back to env when omitted (flag > env > default):")
	fmt.Fprintln(os.Stderr, "  --server-cid  HARNESS_SERVER_CID  (default ws:127.0.0.1:8539-*)")
	fmt.Fprintln(os.Stderr, "  --ws-path     HARNESS_WS_PATH     (default /ws)")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Subcommands:")
	fmt.Fprintln(os.Stderr, "  submit --repo REPO --task TEXT [--runner HEX | --host NAME | --ip ADDR] [--claude-arg ARG ...] [--resume TASK_ID]")
	fmt.Fprintln(os.Stderr, "                                      enqueue a new task (--repo: HARNESS_REPO_PATH)")
	fmt.Fprintln(os.Stderr, "                                      --claude-arg is repeatable; appended after runner-global --claude-args")
	fmt.Fprintln(os.Stderr, "                                      --resume reuses an existing terminal task id + worktree branch (so `--claude-arg --resume <uuid>` finds claude's stored session)")
	fmt.Fprintln(os.Stderr, "  ls                                  list runners and recent tasks")
	fmt.Fprintln(os.Stderr, "  cancel TASK_ID                      cancel a queued/running task")
	fmt.Fprintln(os.Stderr, "  prune [--before DUR]                forget terminal tasks on the server")
	fmt.Fprintln(os.Stderr, "  prune-local [--repo PATH] [--before DUR]")
	fmt.Fprintln(os.Stderr, "                                      remove old worktrees in <repo>/.harness-worktrees/ (--repo: HARNESS_REPO_PATH)")
	fmt.Fprintln(os.Stderr, "  logs TASK_ID                        stream task log output")
	fmt.Fprintln(os.Stderr, "  watch                               stream task and runner status events")
	fmt.Fprintln(os.Stderr, "  interactive --repo REPO [--runner HEX | --host NAME | --ip ADDR] [--claude-arg ARG ...] [--resume TASK_ID]")
	fmt.Fprintln(os.Stderr, "                                      attach an interactive PTY claude (--repo: HARNESS_REPO_PATH)")
	fmt.Fprintln(os.Stderr, "                                      --claude-arg is repeatable; appended after runner-global --claude-args")
	fmt.Fprintln(os.Stderr, "                                      --resume reuses an existing terminal interactive task id + worktree branch")
	fmt.Fprintln(os.Stderr, "  session new --repo REPO [-d|--detach] [--runner HEX | --host NAME | --ip ADDR] [--claude-arg ARG ...] [--resume TASK_ID]")
	fmt.Fprintln(os.Stderr, "                                      open a detachable interactive PTY session (--repo: HARNESS_REPO_PATH)")
	fmt.Fprintln(os.Stderr, "                                      --claude-arg is repeatable; appended after runner-global --claude-args")
	fmt.Fprintln(os.Stderr, "                                      -d / --detach: start the session and exit immediately (don't attach the terminal)")
	fmt.Fprintln(os.Stderr, "  session attach TASK_ID              reattach to a detached/running session")
	fmt.Fprintln(os.Stderr, "  session ls                          JSON Lines: detachable interactive sessions only")
	fmt.Fprintln(os.Stderr, "  session kill TASK_ID                cancel a session (alias of cancel)")
	fmt.Fprintln(os.Stderr, "  agent {send|wait|inbox|subscribe|unsubscribe|dispatch|topics|subscriptions}")
	fmt.Fprintln(os.Stderr, "                                      agent-to-agent message ops (env-primary; HARNESS_AUTH_TICKET required)")
	fmt.Fprintln(os.Stderr, "  file push [-r|--recursive] [-f|--force] TASK_ID LOCAL_SRC WORKTREE_REL_DST")
	fmt.Fprintln(os.Stderr, "                                      copy a local file (or directory tree with -r) into the worktree")
	fmt.Fprintln(os.Stderr, "                                      default: O_EXCL refuses to overwrite; -f permits replacement")
	fmt.Fprintln(os.Stderr, "  file pull [-r|--recursive] [-f|--force] TASK_ID WORKTREE_REL_SRC LOCAL_DST")
	fmt.Fprintln(os.Stderr, "                                      copy a worktree file (or directory tree with -r) to a local path")
	fmt.Fprintln(os.Stderr, "                                      default: O_EXCL refuses to overwrite local; -f permits replacement")
	fmt.Fprintln(os.Stderr, "  file ls   TASK_ID [WORKTREE_REL_DIR]")
	fmt.Fprintln(os.Stderr, "                                      list a single directory under the worktree (default: worktree root)")
	fmt.Fprintln(os.Stderr, "  file delete TASK_ID WORKTREE_REL_PATH")
	fmt.Fprintln(os.Stderr, "                                      remove a file from the task's worktree (refuses directories)")
}

func agentUsage() {
	fmt.Fprintln(os.Stderr, "usage: harness-cli agent <subcommand> [flags]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Env-primary (HARNESS_*): SERVER_CID, TASK_ID, RUNNER_ID, HOSTNAME, WS_PATH, REPO_PATH")
	fmt.Fprintln(os.Stderr, "HARNESS_AUTH_TICKET is env-only (no flag accepted).")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Subcommands:")
	fmt.Fprintln(os.Stderr, "  send --topic T --data D|-           publish a message")
	fmt.Fprintln(os.Stderr, "  wait --topic T [--since-last] [--timeout DUR]")
	fmt.Fprintln(os.Stderr, "                                       block until next message")
	fmt.Fprintln(os.Stderr, "  inbox [--since-last]                 non-blocking dump (used by hook)")
	fmt.Fprintln(os.Stderr, "  subscribe --topic T                  register a subscription")
	fmt.Fprintln(os.Stderr, "  unsubscribe --topic T                remove a subscription")
	fmt.Fprintln(os.Stderr, "  dispatch --topic T --reply-topic R --data D|- [--timeout DUR]")
	fmt.Fprintln(os.Stderr, "                                       send + wait for reply (sugar)")
	fmt.Fprintln(os.Stderr, "  topics                              list every topic on the board (JSON Lines)")
	fmt.Fprintln(os.Stderr, "  subscriptions                       list this agent's registered patterns (JSON Lines)")
}

func die(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

// repeatableStrings is a flag.Value that accumulates one entry per occurrence.
// Used for --claude-arg so callers can write
//
//	harness-cli submit --claude-arg --resume --claude-arg <uuid> ...
//
// without shell-quoting concerns. The value is appended in the order the
// flags appear, which is the order forwarded to claude.
type repeatableStrings []string

func (r *repeatableStrings) String() string {
	if r == nil {
		return ""
	}
	return fmt.Sprint([]string(*r))
}

func (r *repeatableStrings) Set(v string) error {
	*r = append(*r, v)
	return nil
}
