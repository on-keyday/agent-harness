//go:build !js

package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/agent"
	"github.com/on-keyday/agent-harness/cli/cliopts"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/agent-harness/runner/agentskills"
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
		runner := fs.String("runner", "", "pin to a specific runner by ConnectionID (the id= value from `harness-cli ls`)")
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
		capsFlag := fs.String("caps", "", "comma-separated capability names to grant the task (e.g. spawn,file_read / all / none); default: inherit all the spawner holds. With --resume, --caps re-grants caps to the task (else its persisted caps are kept)")
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
		caps, err := cli.ParseCaps(*capsFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, "submit: --caps:", err)
			os.Exit(2)
		}
		sel := resolveSelector()
		// Dial sends the merged PSK+identity handshake (kind=Cli or auto-upgrades
		// to Agent when in-task env is present); no separate SayHelloAuto needed.
		c, err := cli.Dial(ctx, parseCID(), protocol.ClientKind_Cli)
		if err != nil {
			die(err)
		}
		defer c.Close()
		id, err := c.SubmitWithSelectorArgsAndCaps(ctx, repoVal, *task, sel, *extraArgs, *resume, caps, *resume != "" && capsExplicitlySet(fs))
		if err != nil {
			die(err)
		}
		fmt.Println(id)

	case "ls":
		if err := cli.List(ctx, parseCID(), os.Stdout); err != nil {
			die(err)
		}

	case "conns":
		fs := flag.NewFlagSet("conns", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "output JSON lines instead of a human-readable table")
		follow := fs.Bool("follow", false, "stream live connection events (conns.status)")
		fs.BoolVar(follow, "f", false, "shorthand for --follow")
		fs.Parse(args)
		if *follow {
			var err error
			if *asJSON {
				err = cli.WatchConnsJSON(ctx, parseCID(), os.Stdout)
			} else {
				err = cli.WatchConns(ctx, parseCID(), os.Stdout)
			}
			if err != nil && err != context.Canceled {
				die(err)
			}
		} else {
			conns, err := cli.ConnList(ctx, parseCID())
			if err != nil {
				die(err)
			}
			if *asJSON {
				for i := range conns {
					fmt.Fprintln(os.Stdout, cli.ConnInfoJSONLine(&conns[i]))
				}
			} else {
				for _, line := range cli.ConnInfoLines(conns) {
					fmt.Fprintln(os.Stdout, line)
				}
			}
		}

	case "caps":
		fs := flag.NewFlagSet("caps", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "output the capability catalog as JSON")
		fs.Parse(args)
		if err := cli.WriteCaps(os.Stdout, *asJSON); err != nil {
			die(err)
		}

	case "whoami":
		fs := flag.NewFlagSet("whoami", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "output the identity as a JSON object")
		fs.Parse(args)
		resp, err := cli.WhoAmI(ctx, parseCID())
		if err != nil {
			die(err)
		}
		if err := cli.WriteWhoAmI(os.Stdout, resp, *asJSON); err != nil {
			die(err)
		}

	case "skill":
		name := "harness-cli"
		if len(args) > 0 {
			name = args[0]
		}
		md, err := agentskills.Skill(name)
		if err != nil {
			die(fmt.Errorf("skill %q: %w", name, err))
		}
		os.Stdout.Write(md)

	case "cancel":
		if len(args) == 0 {
			fmt.Fprintln(os.Stderr, "cancel: missing task id")
			os.Exit(2)
		}
		if err := cli.Cancel(ctx, parseCID(), args[0]); err != nil {
			die(err)
		}

	case "notify":
		fs := flag.NewFlagSet("notify", flag.ExitOnError)
		title := fs.String("title", "", "short heading for the notification")
		level := fs.String("level", "info", "severity: info|warn|error")
		_ = fs.Parse(args)
		rest := fs.Args()
		if len(rest) == 0 {
			fmt.Fprintln(os.Stderr, "notify: missing text")
			os.Exit(2)
		}
		text := strings.Join(rest, " ")
		if err := cli.Notify(ctx, parseCID(), *level, *title, text); err != nil {
			die(err)
		}

	case "prune":
		fs := flag.NewFlagSet("prune", flag.ExitOnError)
		before := fs.Duration("before", 7*24*time.Hour, "forget terminal tasks older than this (ignored when TASK_IDs are passed)")
		force := fs.Bool("force", false, "with TASK_IDs: also forget tasks the server still considers active (Queued/Running/Detached)")
		fs.BoolVar(force, "f", false, "shorthand for --force")
		fs.Parse(args)
		taskIDs := fs.Args()
		if err := cli.Prune(ctx, parseCID(), *before, taskIDs, *force, os.Stdout); err != nil {
			die(err)
		}

	case "prune-local":
		fs := flag.NewFlagSet("prune-local", flag.ExitOnError)
		repo := fs.String("repo", ".", "repo to prune (env: HARNESS_REPO_PATH; default \".\")")
		before := fs.Duration("before", 7*24*time.Hour, "remove worktrees older than this (ignored when TASK_IDs are passed)")
		force := fs.Bool("force", false, "with TASK_IDs: remove even when the server still considers the task active (Queued/Running/Detached)")
		fs.BoolVar(force, "f", false, "shorthand for --force")
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
		taskIDs := fs.Args()
		if len(taskIDs) == 0 {
			if err := cli.PruneLocal(ctx, abs, *before, nil, os.Stdout); err != nil {
				die(err)
			}
			break
		}
		safe, err := classifyForLocalPrune(ctx, parseCID(), taskIDs, *force, os.Stdout)
		if err != nil {
			die(err)
		}
		if len(safe) == 0 {
			fmt.Fprintln(os.Stdout, "prune-local: no removable task ids (use --force to override server-active state)")
			break
		}
		if err := cli.PruneLocal(ctx, abs, 0, safe, os.Stdout); err != nil {
			die(err)
		}

	case "logs":
		fs := flag.NewFlagSet("logs", flag.ExitOnError)
		follow := fs.Bool("follow", false, "after dumping history, keep streaming live log chunks (no-op when task is terminal)")
		fs.BoolVar(follow, "f", false, "shorthand for --follow")
		fs.Parse(args)
		rest := fs.Args()
		if len(rest) == 0 {
			fmt.Fprintln(os.Stderr, "logs: missing task id")
			os.Exit(2)
		}
		if err := cli.Logs(ctx, parseCID(), rest[0], os.Stdout, *follow); err != nil {
			die(err)
		}

	case "watch":
		if err := cli.Watch(ctx, parseCID(), os.Stdout); err != nil {
			die(err)
		}

	case "notify-watch":
		if err := cli.WatchNotificationsText(ctx, parseCID(), os.Stdout); err != nil {
			die(err)
		}

	case "interactive":
		fs := flag.NewFlagSet("interactive", flag.ExitOnError)
		repo := fs.String("repo", "", "repo identifier (env: HARNESS_REPO_PATH); must match a runner-registered RepoPath verbatim")
		resume := fs.String("resume", "", "task id (32 hex) of a terminal interactive task to resume; --repo is ignored")
		capsFlag := fs.String("caps", "", "comma-separated capability names to grant the task (e.g. spawn,file_read / all / none); default: inherit all the spawner holds. With --resume, --caps re-grants caps to the task (else its persisted caps are kept)")
		resolveSelector := addSelectorFlags(fs)
		extraArgs := addClaudeArgFlag(fs)
		fs.Parse(args)
		repoVal := cliopts.ResolveString(*repo, "HARNESS_REPO_PATH")
		if repoVal == "" && *resume == "" {
			fmt.Fprintln(os.Stderr, "interactive: --repo or HARNESS_REPO_PATH required (must match a runner's RepoPath verbatim) — except when --resume is set, which uses the existing task's repo")
			os.Exit(2)
		}
		caps, err := cli.ParseCaps(*capsFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, "interactive: --caps:", err)
			os.Exit(2)
		}
		sel := resolveSelector()
		// Dial sends the merged PSK+identity handshake; no separate SayHelloAuto needed.
		c, err := cli.Dial(ctx, parseCID(), protocol.ClientKind_Cli)
		if err != nil {
			die(err)
		}
		defer c.Close()
		if _, err := c.InteractiveWithSelectorArgsAndCaps(ctx, repoVal, sel, *extraArgs, *resume, false, caps, *resume != "" && capsExplicitlySet(fs)); err != nil {
			die(err)
		}

	case "file":
		if len(args) == 0 {
			fmt.Fprintln(os.Stderr, "usage: harness-cli file {push|pull|ls|delete} ...")
			os.Exit(2)
		}
		fsub := args[0]
		rest := args[1:]
		c, err := cli.Dial(ctx, parseCID(), protocol.ClientKind_Cli)
		if err != nil {
			die(err)
		}
		defer c.Close()
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

	case "forward":
		if len(args) < 1 {
			fmt.Fprintln(os.Stderr, "usage: harness-cli forward <task-id> -L [bind:]localport:remotehost:remoteport [-L ...]")
			os.Exit(2)
		}
		taskID := args[0]
		fs := flag.NewFlagSet("forward", flag.ExitOnError)
		var specs repeatableStrings
		var rspecs repeatableStrings
		fs.Var(&specs, "L", "local forward [bind:]localport:remotehost:remoteport (repeatable)")
		fs.Var(&rspecs, "R", "remote forward [bind:]runnerport:dialhost:dialport (repeatable)")
		fs.Parse(args[1:])
		if len(specs) == 0 && len(rspecs) == 0 {
			fmt.Fprintln(os.Stderr, "usage: harness-cli forward <task-id> [-L [bind:]localport:remotehost:remoteport] [-R [bind:]runnerport:dialhost:dialport] ...")
			os.Exit(2)
		}
		parsed := make([]cli.ForwardSpec, 0, len(specs))
		for _, s := range specs {
			sp, err := cli.ParseForwardSpec(s)
			if err != nil {
				die(err)
			}
			parsed = append(parsed, sp)
		}
		parsedR := make([]cli.RemoteForwardSpec, 0, len(rspecs))
		for _, s := range rspecs {
			sp, err := cli.ParseRemoteForwardSpec(s)
			if err != nil {
				die(err)
			}
			parsedR = append(parsedR, sp)
		}
		c, err := cli.Dial(ctx, parseCID(), protocol.ClientKind_Cli)
		if err != nil {
			die(err)
		}
		defer c.Close()
		fctx, cancel := signal.NotifyContext(ctx, os.Interrupt)
		defer cancel()
		logf := func(s string) { fmt.Fprintln(os.Stderr, s) }
		// Remote (-R) forwards run in the background; local (-L) forwards — or a
		// bare wait when only -R is given — hold the foreground until Ctrl-C.
		if len(parsedR) > 0 {
			go func() {
				if err := cli.RunRemoteForward(fctx, c, taskID, parsedR, logf); err != nil {
					logf("remote-forward: " + err.Error())
					cancel()
				}
			}()
		}
		if len(parsed) > 0 {
			if err := cli.RunForward(fctx, c, taskID, parsed, logf); err != nil {
				die(err)
			}
		} else {
			<-fctx.Done()
		}

	case "session":
		if err := runSession(parseCID(), args); err != nil {
			die(err)
		}

	case "server":
		if len(args) == 0 {
			serverUsage()
			os.Exit(2)
		}
		ssub := args[0]
		rest := args[1:]
		switch ssub {
		case "dial-runner":
			fs := flag.NewFlagSet("server dial-runner", flag.ExitOnError)
			viaCIDStr := fs.String("via", "", "relay through this registered runner CID (copy from `harness-cli ls` output)")
			if err := fs.Parse(rest); err != nil {
				die(err)
			}
			if fs.NArg() != 1 {
				fmt.Fprintln(os.Stderr, "usage: harness-cli server dial-runner [--via <runner-cid>] <runner-cid>")
				os.Exit(2)
			}
			targetCID, err := objproto.ParseConnectionID(fs.Arg(0),
				objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
			if err != nil {
				die(fmt.Errorf("parse runner-cid: %w", err))
			}
			var viaCID objproto.ConnectionID
			if v := strings.TrimSpace(*viaCIDStr); v != "" {
				viaCID, err = objproto.ParseConnectionID(v,
					objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
				if err != nil {
					die(fmt.Errorf("parse --via: %w", err))
				}
			}
			resp, err := cli.ServerDialRunner(ctx, parseCID(), targetCID, viaCID)
			if err != nil {
				die(err)
			}
			fmt.Println(resp.Status.String())
			if resp.Status != protocol.DialRunnerStatus_Ok {
				os.Exit(1)
			}
		default:
			serverUsage()
			os.Exit(2)
		}

	case "board":
		if len(args) == 0 {
			boardUsage()
			os.Exit(2)
		}
		bsub := args[0]
		rest := args[1:]
		if err := cli.RunBoardSubcmd(ctx, parseCID(), bsub, rest, os.Stdout); err != nil {
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
		case "purge":
			err = agent.Purge(ctx, rest, os.Stdout)
		case "retained":
			err = agent.Retained(ctx, rest, os.Stdout)
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
	fmt.Fprintln(os.Stderr, "  submit --repo REPO --task TEXT [--runner HEX | --host NAME | --ip ADDR] [--claude-arg ARG ...] [--resume TASK_ID] [--caps NAMES]")
	fmt.Fprintln(os.Stderr, "                                      enqueue a new task (--repo: HARNESS_REPO_PATH)")
	fmt.Fprintln(os.Stderr, "                                      --claude-arg is repeatable; appended after runner-global --claude-args")
	fmt.Fprintln(os.Stderr, "                                      --resume reuses an existing terminal task id + worktree branch (so `--claude-arg --resume <uuid>` finds claude's stored session)")
	fmt.Fprintln(os.Stderr, "                                      --caps: comma-separated capability names to grant (e.g. spawn,file_read / all / none); default all. On --resume, --caps re-grants caps to the task (else its persisted caps are kept)")
	fmt.Fprintln(os.Stderr, "  ls                                  list runners and recent tasks")
	fmt.Fprintln(os.Stderr, "  conns [-f|--follow] [--json]        snapshot live connections (requires info_global cap); -f streams live events; --json emits JSON lines")
	fmt.Fprintln(os.Stderr, "  caps [--json]                       list the grantable --caps capability names and what each authorizes")
	fmt.Fprintln(os.Stderr, "  whoami [--json]                     show THIS connection's own principal + server-enforced caps (no cap required)")
	fmt.Fprintln(os.Stderr, "  skill [NAME]                        print the embedded agent skill (default: harness-cli)")
	fmt.Fprintln(os.Stderr, "  cancel TASK_ID                      cancel a queued/running task")
	fmt.Fprintln(os.Stderr, "  notify [--title T] [--level info|warn|error] <text>")
	fmt.Fprintln(os.Stderr, "                                      send a notification (one short line; detail goes in the task log)")
	fmt.Fprintln(os.Stderr, "  prune [--before DUR] [-f|--force] [TASK_ID ...]")
	fmt.Fprintln(os.Stderr, "                                      ask the server to forget tasks")
	fmt.Fprintln(os.Stderr, "                                      no TASK_IDs: terminal tasks older than --before")
	fmt.Fprintln(os.Stderr, "                                      with TASK_IDs: only those (refuses active tasks unless --force)")
	fmt.Fprintln(os.Stderr, "  prune-local [--repo PATH] [--before DUR] [-f|--force] [TASK_ID ...]")
	fmt.Fprintln(os.Stderr, "                                      remove worktrees in <repo>/.harness-worktrees/ (--repo: HARNESS_REPO_PATH)")
	fmt.Fprintln(os.Stderr, "                                      with no TASK_IDs: time-based, removes entries older than --before")
	fmt.Fprintln(os.Stderr, "                                      with TASK_IDs: removes only those (refuses active tasks unless --force)")
	fmt.Fprintln(os.Stderr, "  logs [-f|--follow] TASK_ID          dump task log history; -f also streams live chunks until task terminal")
	fmt.Fprintln(os.Stderr, "  watch                               stream task and runner status events")
	fmt.Fprintln(os.Stderr, "  notify-watch                        stream notifications (backlog + live); one human-readable line each")
	fmt.Fprintln(os.Stderr, "  interactive --repo REPO [--runner HEX | --host NAME | --ip ADDR] [--claude-arg ARG ...] [--resume TASK_ID] [--caps NAMES]")
	fmt.Fprintln(os.Stderr, "                                      attach an interactive PTY claude (--repo: HARNESS_REPO_PATH)")
	fmt.Fprintln(os.Stderr, "                                      --claude-arg is repeatable; appended after runner-global --claude-args")
	fmt.Fprintln(os.Stderr, "                                      --resume reuses an existing terminal interactive task id + worktree branch")
	fmt.Fprintln(os.Stderr, "  session new --repo REPO [-d|--detach] [--runner HEX | --host NAME | --ip ADDR] [--claude-arg ARG ...] [--resume TASK_ID] [--caps NAMES]")
	fmt.Fprintln(os.Stderr, "                                      open a detachable interactive PTY session (--repo: HARNESS_REPO_PATH)")
	fmt.Fprintln(os.Stderr, "                                      --claude-arg is repeatable; appended after runner-global --claude-args")
	fmt.Fprintln(os.Stderr, "                                      -d / --detach: start the session and exit immediately (don't attach the terminal)")
	fmt.Fprintln(os.Stderr, "  session attach TASK_ID              reattach to a detached/running session")
	fmt.Fprintln(os.Stderr, "  session ls                          JSON Lines: detachable interactive sessions only")
	fmt.Fprintln(os.Stderr, "  session kill TASK_ID                cancel a session (alias of cancel)")
	fmt.Fprintln(os.Stderr, "  server dial-runner [--via CID] RUNNER_CID  ask the server to reverse-dial a Listen-mode runner")
	fmt.Fprintln(os.Stderr, "  board topics|read <topic>|purge <topic> [--seq N]")
	fmt.Fprintln(os.Stderr, "                                      inspect/purge the agentboard (cap: info_global; purge: purge)")
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
	fmt.Fprintln(os.Stderr, "  file delete [-r|--recursive] [-f|--force] TASK_ID WORKTREE_REL_PATH")
	fmt.Fprintln(os.Stderr, "                                      remove a file; -r a directory (dir_delete), -r -f a non-empty directory (RemoveAll); without -r a directory is refused")
	fmt.Fprintln(os.Stderr, "  forward <task-id> [-L [bind:]localport:remotehost:remoteport] [-R [bind:]runnerport:dialhost:dialport] ...")
	fmt.Fprintln(os.Stderr, "                                      -L: forward a local port through the runner to remote host:port (ssh -L)")
	fmt.Fprintln(os.Stderr, "                                      -R: runner listens, connections dial back to a client-side host:port (ssh -R)")
	fmt.Fprintln(os.Stderr, "                                      both repeatable; Ctrl-C to stop")
}

func serverUsage() {
	fmt.Fprintln(os.Stderr, "usage: harness-cli server <subcommand> [flags]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Subcommands:")
	fmt.Fprintln(os.Stderr, "  dial-runner [--via RUNNER_CID] RUNNER_CID")
	fmt.Fprintln(os.Stderr, "                                      ask the server to reverse-dial RUNNER_CID (Phase A/B)")
	fmt.Fprintln(os.Stderr, "                                      --via relays through an already-connected runner (Phase B)")
	fmt.Fprintln(os.Stderr, "                                      (runner must be running in --listen / --udp-listen mode)")
	fmt.Fprintln(os.Stderr, "                                      prints the DialRunnerStatus and exits non-zero on non-Ok")
}

func boardUsage() {
	fmt.Fprintln(os.Stderr, "usage: harness-cli board <subcommand> [flags]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Subcommands:")
	fmt.Fprintln(os.Stderr, "  topics                              list every topic on the board with metadata (cap: info_global)")
	fmt.Fprintln(os.Stderr, "  read <topic>                        print retained messages for <topic> (JSON pretty-printed; not found = exit 0)")
	fmt.Fprintln(os.Stderr, "  purge <topic> [--seq N]             drop the whole topic ring (seq=0) or one message by seq (cap: purge)")
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
	fmt.Fprintln(os.Stderr, "  retained --topic T | --self          list a topic's retained ring as metadata only, no payload (no cap)")
	fmt.Fprintln(os.Stderr, "  purge --topic T | --self [--seq N]   drop a topic's retained buffer, or one message by seq (cap: purge)")
}

func die(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

// classifyForLocalPrune dials the server, snapshots the task list, and
// returns the subset of taskIDs that are safe to remove locally. A task is
// safe when its server status is terminal (Succeeded/Failed/Cancelled) or
// when it is no longer in the snapshot at all (pruned/typo). Tasks the
// server still considers active (Queued/Running/Detached) are skipped with
// a warning unless force is set.
func classifyForLocalPrune(ctx context.Context, peerCID objproto.ConnectionID, taskIDs []string, force bool, out io.Writer) ([]string, error) {
	c, err := cli.Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	snap, err := c.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	statusByID := make(map[string]protocol.TaskStatus, len(snap.Tasks))
	for i := range snap.Tasks {
		statusByID[hex.EncodeToString(snap.Tasks[i].Id.Id[:])] = snap.Tasks[i].Status
	}
	safe := make([]string, 0, len(taskIDs))
	for _, id := range taskIDs {
		st, known := statusByID[id]
		if !known {
			safe = append(safe, id)
			continue
		}
		switch st {
		case protocol.TaskStatus_Succeeded,
			protocol.TaskStatus_Failed,
			protocol.TaskStatus_Cancelled:
			safe = append(safe, id)
		default:
			if force {
				fmt.Fprintf(out, "force-removing %s (status=%s on server)\n", id, st.String())
				safe = append(safe, id)
			} else {
				fmt.Fprintf(out, "skip %s: still active on server (status=%s); pass --force to override\n", id, st.String())
			}
		}
	}
	return safe, nil
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

// capsExplicitlySet reports whether the "caps" flag was explicitly provided on
// the command line (as opposed to taking its zero-value default). It uses
// flag.FlagSet.Visit which iterates only over flags that were actually set.
func capsExplicitlySet(fs *flag.FlagSet) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "caps" {
			found = true
		}
	})
	return found
}
