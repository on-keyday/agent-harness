//go:build !js

package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/agent"
	"github.com/on-keyday/agent-harness/cli/cliopts"
	"github.com/on-keyday/agent-harness/cli/sshgw"
	"github.com/on-keyday/agent-harness/cli/workspace"
	"github.com/on-keyday/agent-harness/runner/agentskills"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// scopeForFlag collects repeatable --scope-for values. It parses and merges on
// every Set, so an overlapping capability list is rejected at the flag rather
// than one round trip later — the server refuses it too, but a typo should not
// cost a spawn.
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

// workspaceRepo is the `repo` a --workspace supplied, consulted by the two
// places that fall back to HARNESS_REPO_PATH (the file subcommand here and
// `session new` in session.go). A package-level var rather than an os.Setenv:
// the workspace is a tier BELOW the environment, and writing it into the
// environment would erase that distinction for anything reading it later.
var workspaceRepo string

func main() {
	serverCID := flag.String("server-cid", "",
		"server ConnectionID (env: HARNESS_SERVER_CID; workspace: server-cid; default ws:127.0.0.1:8539-*)")
	wsPath := flag.String("ws-path", "", "WebSocket URL path (env: HARNESS_WS_PATH; workspace: ws-path; default /ws)")
	configPath := flag.String("config", "", "workspace config file (env: HARNESS_CONFIG; default ./.harness/config)")
	wsName := flag.String("workspace", "", "workspace whose server-cid / ws-path / repo to use (see `workspace ls`)")
	flag.Usage = usage
	flag.Parse()

	wsFile, _, werr := workspace.Load(*configPath)
	if werr != nil {
		die(fmt.Errorf("config: %w", werr))
	}
	var wsServerCID, wsWSPath string
	if *wsName != "" {
		w, ok := wsFile.Workspace(*wsName)
		if !ok {
			die(fmt.Errorf("config: no workspace named %q", *wsName))
		}
		wsServerCID, wsWSPath, workspaceRepo = w.ServerCID, w.WSPath, w.Repo
	}

	resolvedWS := cliopts.ResolveStringWith(*wsPath, "HARNESS_WS_PATH", wsWSPath)
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
		val := cliopts.ResolveStringWith(*serverCID, "HARNESS_SERVER_CID", wsServerCID)
		if val == "" {
			val = "ws:127.0.0.1:8539-*"
		}
		cid, err := cliopts.ResolveServerCID(val)
		if err != nil {
			die(err)
		}
		return cid
	}

	// addAgentArgFlags registers --agent-arg as a repeatable flag and returns
	// the underlying slice (populated after fs.Parse). Each occurrence appends
	// one CLI argument forwarded verbatim to the spawned agent process; e.g.
	//   submit --agent-arg --resume --agent-arg <uuid>
	// works around the 2.1.123 /resume picker regression that requires the
	// caller to be in the original CWD by letting the user push --resume
	// through harness-cli without an interactive picker.
	addAgentArgFlags := func(fs *flag.FlagSet) *[]string {
		var args repeatableStrings
		fs.Var(&args, "agent-arg", "extra CLI arg to forward to the agent (repeatable; appended after runner-global --agent-args)")
		fs.Var(&args, "claude-arg", "deprecated alias for --agent-arg")
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
		resumeConversation := fs.Bool("resume-conversation", false, "with --resume, also ask the runner to resume the agent's own conversation state")
		capsFlag := fs.String("caps", "", cli.CapsFlagUsage)
		scopeFlag := fs.String("scope", "", "which tasks this task's capabilities may target: "+cli.ScopeGrammar+"; default subtree (self + descendants). With --resume, --scope re-grants the scope (omitted = keep the task's), independently of --caps")
		agent := fs.String("agent", "", "agent profile name (empty = runner default)")
		var scopeFor scopeForFlag
		fs.Var(&scopeFor, "scope-for", cli.ScopeForFlagUsage)
		resolveSelector := addSelectorFlags(fs)
		extraArgs := addAgentArgFlags(fs)
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
		scope, err := cli.ParseScope(*scopeFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, "submit: --scope:", err)
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
		id, err := c.Submit(ctx, repoVal, *task, cli.SessionOpts{
			Selector: sel, ExtraArgs: *extraArgs, ResumeTaskID: *resume,
			Caps: caps, Scope: scope, Overrides: scopeFor.out,
			ResumeCapsOverride: *resume != "" && capsExplicitlySet(fs),
			ScopePresent:       *resume != "" && flagExplicitlySet(fs, "scope"),
			ResumeConversation: *resumeConversation, AgentProfile: *agent,
		})
		if err != nil {
			die(err)
		}
		fmt.Println(id)

	case "ls":
		fs := flag.NewFlagSet("ls", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "emit a single JSON object {\"runners\":[...],\"tasks\":[...]} instead of the human-readable table")
		asTree := fs.Bool("tree", false, "order tasks by their creator link and draw the hierarchy; shows every visible task, orphans included")
		fs.Parse(args)
		switch {
		case *asTree && *asJSON:
			// Not silently ignored: --json already carries created_by on every
			// row, so a consumer builds the tree itself. Nesting the JSON to
			// match would give the same data two shapes.
			die(errors.New("ls: --tree and --json are mutually exclusive (--json rows carry created_by; build the tree from those)"))
		case *asTree:
			if err := cli.ListTree(ctx, parseCID(), os.Stdout); err != nil {
				die(err)
			}
		case *asJSON:
			if err := cli.ListJSON(ctx, parseCID(), os.Stdout); err != nil {
				die(err)
			}
		default:
			if err := cli.List(ctx, parseCID(), os.Stdout); err != nil {
				die(err)
			}
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
		if len(args) > 0 && args[0] == "set" {
			runCapsSet(ctx, parseCID(), args[1:])
			return
		}
		if len(args) > 0 && args[0] == "set-parent" {
			runCapsSetParent(ctx, parseCID(), args[1:])
			return
		}
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

	case "version":
		fs := flag.NewFlagSet("version", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "output the build stamp as a JSON object")
		fs.Parse(args)
		if err := writeVersion(os.Stdout, *asJSON); err != nil {
			die(err)
		}

	case "skill":
		if len(args) > 0 && (args[0] == "--list" || args[0] == "-l" || args[0] == "ls") {
			names, err := agentskills.List()
			if err != nil {
				die(fmt.Errorf("skill list: %w", err))
			}
			for _, n := range names {
				fmt.Println(n)
				if d, derr := agentskills.Description(n); derr == nil && d != "" {
					fmt.Printf("    %s\n", d)
				}
			}
			break
		}
		name := "harness-cli"
		if len(args) > 0 {
			name = args[0]
		}
		md, err := agentskills.Skill(name)
		if err != nil {
			avail, lerr := agentskills.List()
			if lerr == nil {
				die(fmt.Errorf("skill %q: %w (available: %s)", name, err, strings.Join(avail, ", ")))
			}
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
		taskIDs, perr := cli.ParsePermuted(fs, args)
		if perr != nil {
			die(perr)
		}
		if err := cli.Prune(ctx, parseCID(), *before, taskIDs, *force, os.Stdout); err != nil {
			die(err)
		}

	case "prune-local":
		fs := flag.NewFlagSet("prune-local", flag.ExitOnError)
		repo := fs.String("repo", ".", "repo to prune (env: HARNESS_REPO_PATH; default \".\")")
		before := fs.Duration("before", 7*24*time.Hour, "remove worktrees older than this (ignored when TASK_IDs are passed)")
		force := fs.Bool("force", false, "with TASK_IDs: remove even when the server still considers the task active (Queued/Running/Detached)")
		fs.BoolVar(force, "f", false, "shorthand for --force")
		taskIDs, perr := cli.ParsePermuted(fs, args)
		if perr != nil {
			die(perr)
		}
		repoVal := *repo
		if repoVal == "." {
			if env := os.Getenv("HARNESS_REPO_PATH"); env != "" {
				repoVal = env
			} else if workspaceRepo != "" {
				repoVal = workspaceRepo
			}
		}
		abs, err := filepath.Abs(repoVal)
		if err != nil {
			die(err)
		}
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
		rest, perr := cli.ParsePermuted(fs, args)
		if perr != nil {
			die(perr)
		}
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
		resumeConversation := fs.Bool("resume-conversation", false, "with --resume, also ask the runner to resume the agent's own conversation state")
		capsFlag := fs.String("caps", "", cli.CapsFlagUsage)
		scopeFlag := fs.String("scope", "", "which tasks this task's capabilities may target: "+cli.ScopeGrammar+"; default subtree (self + descendants). With --resume, --scope re-grants the scope (omitted = keep the task's), independently of --caps")
		agent := fs.String("agent", "", "agent profile name (empty = runner default)")
		var scopeFor scopeForFlag
		fs.Var(&scopeFor, "scope-for", cli.ScopeForFlagUsage)
		resolveSelector := addSelectorFlags(fs)
		extraArgs := addAgentArgFlags(fs)
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
		scope, err := cli.ParseScope(*scopeFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, "interactive: --scope:", err)
			os.Exit(2)
		}
		sel := resolveSelector()
		// Dial sends the merged PSK+identity handshake; no separate SayHelloAuto needed.
		c, err := cli.Dial(ctx, parseCID(), protocol.ClientKind_Cli)
		if err != nil {
			die(err)
		}
		defer c.Close()
		// The session survives a client disconnect (tmux-like) and any
		// operator client can take it over via reattach.
		if _, err := c.Interactive(ctx, repoVal, cli.SessionOpts{
			Selector: sel, ExtraArgs: *extraArgs, ResumeTaskID: *resume,
			Caps: caps, Scope: scope, Overrides: scopeFor.out,
			ResumeCapsOverride: *resume != "" && capsExplicitlySet(fs),
			ScopePresent:       *resume != "" && flagExplicitlySet(fs, "scope"),
			ResumeConversation: *resumeConversation, AgentProfile: *agent,
		}); err != nil {
			die(err)
		}

	case "file":
		if len(args) == 0 {
			fmt.Fprintln(os.Stderr, "usage: harness-cli file {push|pull|ls|mkdir|delete|edit|new} ...")
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
			parents := fs.Bool("parents", false, "create missing parent directories of the destination (mkdir -p)")
			fs.BoolVar(parents, "p", false, "alias for --parents")
			pargs, perr := cli.ParsePermuted(fs, rest)
			if perr != nil {
				die(perr)
			}
			if len(pargs) != 3 {
				fmt.Fprintln(os.Stderr, "usage: harness-cli file push [-r] [-f] [-p] <task-id> <local-src> <worktree-rel-dst>")
				os.Exit(2)
			}
			opts := cli.FilePushOpts{Force: *force, MkdirParents: *parents}
			if *recursive {
				if err := c.FilePushDir(ctx, pargs[0], pargs[1], pargs[2], opts); err != nil {
					die(err)
				}
			} else {
				if err := c.FilePush(ctx, pargs[0], pargs[1], pargs[2], opts); err != nil {
					die(err)
				}
			}
		case "pull":
			fs := flag.NewFlagSet("file pull", flag.ExitOnError)
			recursive := fs.Bool("recursive", false, "transfer a directory tree")
			fs.BoolVar(recursive, "r", false, "alias for --recursive")
			force := fs.Bool("force", false, "overwrite existing destination")
			fs.BoolVar(force, "f", false, "alias for --force")
			offset := fs.Uint64("offset", 0, "first byte to pull (single-file pull only)")
			length := fs.Uint64("length", 0, "max bytes to pull; 0 = to end of file")
			pargs, perr := cli.ParsePermuted(fs, rest)
			if perr != nil {
				die(perr)
			}
			if len(pargs) != 3 {
				fmt.Fprintln(os.Stderr, "usage: harness-cli file pull [-r] [-f] [--offset N] [--length N] <task-id> <worktree-rel-src> <local-dst>")
				os.Exit(2)
			}
			if *recursive {
				// A directory pull is a generated tar, whose byte offsets are
				// not a stable thing to index into. Refused here rather than
				// sent, so the message names the combination.
				if *offset != 0 || *length != 0 {
					fmt.Fprintln(os.Stderr, "file pull: --offset/--length cannot be combined with --recursive")
					os.Exit(2)
				}
				if err := c.FilePullDir(ctx, pargs[0], pargs[1], pargs[2], *force); err != nil {
					die(err)
				}
			} else {
				rng := cli.FileTransferRange{Offset: *offset, Length: *length}
				if err := c.FilePull(ctx, pargs[0], pargs[1], pargs[2], rng, *force); err != nil {
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
		case "mkdir":
			fs := flag.NewFlagSet("file mkdir", flag.ExitOnError)
			parents := fs.Bool("parents", false, "create missing parent directories (mkdir -p); also makes an existing directory a success")
			fs.BoolVar(parents, "p", false, "alias for --parents")
			pargs, perr := cli.ParsePermuted(fs, rest)
			if perr != nil {
				die(perr)
			}
			if len(pargs) != 2 {
				fmt.Fprintln(os.Stderr, "usage: harness-cli file mkdir [-p] <task-id> <worktree-rel-dir>")
				os.Exit(2)
			}
			if err := c.FileMkdir(ctx, pargs[0], pargs[1], *parents); err != nil {
				die(err)
			}
		case "edit":
			if len(rest) != 2 {
				fmt.Fprintln(os.Stderr, "usage: harness-cli file edit <task-id> <worktree-rel-path>")
				os.Exit(2)
			}
			if err := runFileEdit(ctx, c, rest[0], rest[1]); err != nil {
				die(err)
			}
		case "new":
			if len(rest) != 2 {
				fmt.Fprintln(os.Stderr, "usage: harness-cli file new <task-id> <worktree-rel-path>")
				os.Exit(2)
			}
			if err := runFileNew(ctx, c, rest[0], rest[1]); err != nil {
				die(err)
			}
		case "delete":
			fs := flag.NewFlagSet("file delete", flag.ExitOnError)
			recursive := fs.Bool("recursive", false, "target a directory tree instead of a single file (uses dir_delete)")
			fs.BoolVar(recursive, "r", false, "alias for --recursive")
			force := fs.Bool("force", false, "with -r: delete non-empty directory contents recursively (os.RemoveAll). Ignored without -r.")
			fs.BoolVar(force, "f", false, "alias for --force")
			pargs, perr := cli.ParsePermuted(fs, rest)
			if perr != nil {
				die(perr)
			}
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

	case "git":
		if err := runGit(parseCID(), args); err != nil {
			die(err)
		}

	case "exec":
		if len(args) == 0 {
			fmt.Fprintln(os.Stderr, "usage: harness-cli exec [--shell] [--sshd-parent] <task-id> -- <command> [args...]")
			fmt.Fprintln(os.Stderr, "       harness-cli exec ls [--task <task-id>] [--json]")
			fmt.Fprintln(os.Stderr, "       harness-cli exec kill <exec-id> [<exec-id> ...]")
			os.Exit(2)
		}
		if err := runExec(ctx, parseCID(), args); err != nil {
			die(err)
		}

	case "workspace":
		if err := runWorkspace(ctx, args, parseCID(), *configPath,
			cliopts.ResolveStringWith(*serverCID, "HARNESS_SERVER_CID", wsServerCID)); err != nil {
			die(err)
		}

	case "forward":
		if len(args) < 1 {
			fmt.Fprintln(os.Stderr, "usage: harness-cli forward <task-id> -L [bind:]localport:remotehost:remoteport [-L ...]")
			fmt.Fprintln(os.Stderr, "       harness-cli forward <task-id> -W host:port")
			fmt.Fprintln(os.Stderr, "       harness-cli forward ls [--task <task-id>] [--json]")
			fmt.Fprintln(os.Stderr, "       harness-cli forward kill <forward-id> [<forward-id> ...]")
			fmt.Fprintln(os.Stderr, "       harness-cli forward tap <forward-id> [--dir to-target|from-target|both] [--max-bytes N] [--hex|--text|--raw|--json]")
			os.Exit(2)
		}
		switch args[0] {
		case "ls":
			fs := flag.NewFlagSet("forward ls", flag.ExitOnError)
			taskFilter := fs.String("task", "", "only forwards for this task id")
			asJSON := fs.Bool("json", false, "one JSON object per forward")
			if err := fs.Parse(args[1:]); err != nil {
				die(err)
			}
			forwards, err := cli.PortForwardList(ctx, parseCID(), *taskFilter)
			if err != nil {
				die(err)
			}
			if *asJSON {
				for i := range forwards {
					fmt.Println(cli.PortForwardInfoJSONLine(&forwards[i]))
				}
			} else {
				for _, line := range cli.PortForwardInfoLines(forwards) {
					fmt.Println(line)
				}
			}
			return
		case "tap":
			if len(args) < 2 {
				fmt.Fprintln(os.Stderr, "usage: harness-cli forward tap <forward-id> [--dir to-target|from-target|both] [--max-bytes N] [--hex|--text|--raw|--json]")
				os.Exit(2)
			}
			id, perr := strconv.ParseUint(args[1], 10, 64)
			if perr != nil {
				die(fmt.Errorf("forward tap: bad forward id %q", args[1]))
			}
			fs := flag.NewFlagSet("forward tap", flag.ExitOnError)
			dir := fs.String("dir", "both", "to-target, from-target or both")
			maxBytes := fs.Uint("max-bytes", 0, "cut each record's payload to this many bytes (0 = whole payload)")
			asHex := fs.Bool("hex", false, "hexdump body (default)")
			asText := fs.Bool("text", false, "printable body, no offset column")
			asRaw := fs.Bool("raw", false, "payload bytes only; requires an explicit --dir")
			asJSON := fs.Bool("json", false, "one JSON object per record")
			if err := fs.Parse(args[2:]); err != nil {
				die(err)
			}
			mode, merr := tapMode(*asHex, *asText, *asRaw, *asJSON)
			if merr != nil {
				fmt.Fprintln(os.Stderr, merr)
				os.Exit(2)
			}
			// --raw writes payloads with no headers, so two directions
			// concatenated onto one stdout interleave two conversations into a
			// byte soup no decoder can read. Refuse rather than produce it.
			if mode == cli.TapRaw && *dir == "both" {
				fmt.Fprintln(os.Stderr, "forward tap: --raw needs an explicit --dir (to-target or from-target); "+
					"both directions on one stdout is not a stream any decoder can read")
				os.Exit(2)
			}
			filter, ferr := cli.ParseTapFilter(*dir)
			if ferr != nil {
				die(ferr)
			}
			if *maxBytes > math.MaxUint32 {
				die(fmt.Errorf("forward tap: --max-bytes %d is out of range", *maxBytes))
			}
			tctx, cancel := interruptContext("forward tap", ctx)
			defer cancel()
			if err := cli.RunForwardTapDial(tctx, parseCID(), id, cli.ForwardTapOpts{
				Filter: filter, MaxRecordBytes: uint32(*maxBytes), Mode: mode,
			}, os.Stdout); err != nil {
				die(err)
			}
			return
		case "kill":
			if len(args) < 2 {
				fmt.Fprintln(os.Stderr, "usage: harness-cli forward kill <forward-id> [<forward-id> ...]")
				os.Exit(2)
			}
			for _, raw := range args[1:] {
				id, perr := strconv.ParseUint(raw, 10, 64)
				if perr != nil {
					die(fmt.Errorf("forward kill: bad forward id %q", raw))
				}
				if err := cli.KillPortForward(ctx, parseCID(), id); err != nil {
					die(err)
				}
				fmt.Printf("killed forward %d\n", id)
			}
			return
		}
		taskID := args[0]
		fs := flag.NewFlagSet("forward", flag.ExitOnError)
		var specs repeatableStrings
		var rspecs repeatableStrings
		fs.Var(&specs, "L", "local forward [bind:]localport:remotehost:remoteport (repeatable)")
		fs.Var(&rspecs, "R", "remote forward [bind:]runnerport:dialhost:dialport (repeatable)")
		// -W mirrors ssh -W: no local listener, this process's stdin/stdout is
		// the forward's client endpoint. ssh makes -W exclusive with -L/-R
		// (it implies ClearAllForwardings) for the same reason we do: -W owns
		// the foreground and exits with its peer, while -L/-R are long-lived
		// listeners. One invocation, one lifetime.
		wspec := fs.String("W", "", "raw stdio forward host:port (mutually exclusive with -L / -R)")
		// With --http-path, -W stops splicing stdin and sends one built request
		// instead, streaming the response to stdout. Ordinary flags rather than
		// a "GET /x" mini-syntax: that reads well until the first header or
		// body and then needs a parser and an escaping rule of its own.
		httpMethod := fs.String("http-method", "GET", "with -W --http-path: HTTP method")
		httpPath := fs.String("http-path", "", "with -W: send this HTTP request path instead of splicing stdin")
		httpBody := fs.String("http-body", "", "with --http-path: request body (literal, @file, or - for stdin)")
		var httpHeaders stringList
		fs.Var(&httpHeaders, "http-header", "with --http-path: \"Name: value\" (repeatable)")
		fs.Parse(args[1:])
		if *httpPath != "" && *wspec == "" {
			fmt.Fprintln(os.Stderr, "forward: --http-path needs -W host:port")
			os.Exit(2)
		}
		if forwardWConflictsWithLR(*wspec, len(specs), len(rspecs)) {
			fmt.Fprintln(os.Stderr, "forward: -W cannot be combined with -L / -R")
			os.Exit(2)
		}
		if len(specs) == 0 && len(rspecs) == 0 && *wspec == "" {
			fmt.Fprintln(os.Stderr, "usage: harness-cli forward <task-id> [-L [bind:]localport:remotehost:remoteport] [-R [bind:]runnerport:dialhost:dialport] [-W host:port] ...")
			os.Exit(2)
		}
		var wHost string
		var wPort int
		if *wspec != "" {
			h, p, werr := cli.ParseStdioForwardSpec(*wspec)
			if werr != nil {
				die(werr)
			}
			wHost, wPort = h, p
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
		fctx, cancel := interruptContext("forward", ctx)
		defer cancel()
		logf := func(s string) { fmt.Fprintln(os.Stderr, s) }
		if *wspec != "" {
			// stdout is the forward's payload channel, so status lines must go
			// to stderr (logf already does) and nothing may print to stdout.
			if *httpPath != "" {
				body, berr := readFlagBody(*httpBody)
				if berr != nil {
					die(berr)
				}
				spec := cli.HTTPRequestSpec{
					Method:  *httpMethod,
					Path:    *httpPath,
					Headers: httpHeaders,
					Body:    body,
				}
				if err := cli.RunHTTPRequestForward(fctx, c, taskID, wHost, wPort, spec, os.Stdout, logf); err != nil {
					die(err)
				}
				return
			}
			if err := cli.RunStdioForward(fctx, c, taskID, wHost, wPort, logf); err != nil {
				die(err)
			}
			return
		}
		// Both RunForward and RunRemoteForward now return as soon as every
		// forward they started has stopped — killed remotely, not just on
		// Ctrl-C — so the process must wait on that completion signal, not
		// fctx.Done() alone: a -R forward that outlives the -L side (or is
		// the only side) must still let the terminal return to its prompt
		// once IT is killed, without requiring Ctrl-C. rDone is closed once
		// the -R goroutine (if any) has returned.
		var rDone chan struct{}
		if len(parsedR) > 0 {
			rDone = make(chan struct{})
			go func() {
				defer close(rDone)
				if err := cli.RunRemoteForward(fctx, c, taskID, parsedR, logf); err != nil {
					logf("remote-forward: " + err.Error())
					cancel()
				}
			}()
		}
		var forwardErr error
		if len(parsed) > 0 {
			if err := cli.RunForward(fctx, c, taskID, parsed, logf, nil); err != nil {
				// Don't die(err) here: os.Exit would skip waiting for a live
				// -R forward below, tearing it down mid-flight with no
				// graceful signal. Same shape as the -R error path above —
				// log, cancel, and let the wait for rDone run its course
				// before the process actually exits.
				logf(err.Error())
				cancel()
				forwardErr = err
			}
		}
		if rDone != nil {
			<-rDone
		}
		if forwardErr != nil {
			os.Exit(1)
		}

	case "ssh-gateway":
		fs := flag.NewFlagSet("ssh-gateway", flag.ExitOnError)
		listen := fs.String("listen", sshgw.DefaultListen, "ssh listen host:port (no ssh auth on a loopback bind; --authorized-keys is required off loopback)")
		hostKey := fs.String("host-key", "", "ssh host key path (default: alongside the workspace config; generated on first run, then reused)")
		authKeys := fs.String("authorized-keys", "", "OpenSSH authorized_keys file; optional on a loopback bind, required otherwise")
		fs.Parse(args)
		keyPath := *hostKey
		if keyPath == "" {
			keyPath = sshgw.DefaultHostKeyPath(*configPath)
		}
		c, err := cli.Dial(ctx, parseCID(), protocol.ClientKind_Cli)
		if err != nil {
			die(err)
		}
		defer c.Close()
		fmt.Fprintf(os.Stderr, "harness-cli: ssh gateway on %s — `ssh -p %s <32-hex-task-id>[.control|.view|.sshd-parent]@%s` attaches; Ctrl-C stops it and every session it serves, and so does the server connection dropping\n",
			*listen, sshgw.PortOf(*listen), sshgw.HostOf(*listen))
		fmt.Fprintln(os.Stderr, "harness-cli: bare user name = cowrite (evicts nobody), .control takes the seat, .view watches; Ctrl+] detaches")
		gctx, cancel := interruptContext("ssh-gateway", ctx)
		defer cancel()
		if err := sshgw.Run(gctx, c, sshgw.Options{
			Listen:             *listen,
			HostKeyPath:        keyPath,
			AuthorizedKeysPath: *authKeys,
		}); err != nil {
			die(err)
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
			dpos, derr := cli.ParsePermuted(fs, rest)
			if derr != nil {
				die(derr)
			}
			if len(dpos) != 1 {
				fmt.Fprintln(os.Stderr, "usage: harness-cli server dial-runner [--via <runner-cid>] <runner-cid>")
				os.Exit(2)
			}
			targetCID, err := objproto.ParseConnectionID(dpos[0],
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
		case "read":
			err = agent.Read(ctx, rest, os.Stdout)
		case "retract":
			err = agent.Retract(ctx, rest, os.Stdout)
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

// isTaskIDLike reports whether s could be a task id (hex digits only). Used to
// keep `forward ls` / `forward kill` from being mistaken for a task id — neither
// word is hex.
func isTaskIDLike(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}

// forwardWConflictsWithLR reports whether -W was combined with -L or -R.
// They are mutually exclusive: -W owns the foreground and exits with its
// peer (like ssh -W, which implies ClearAllForwardings), while -L/-R are
// long-lived listeners started alongside each other.
func forwardWConflictsWithLR(wspec string, nLSpecs, nRSpecs int) bool {
	return wspec != "" && (nLSpecs > 0 || nRSpecs > 0)
}

// interruptStopGrace is how long a first interrupt gets to unwind cleanly
// before the process stops waiting for it.
const interruptStopGrace = 5 * time.Second

// interruptContext cancels ctx on the first interrupt and makes the second one
// authoritative, because a graceful stop that does not finish is
// indistinguishable from a hang at the keyboard.
//
// A forward's clean shutdown depends on the far side: listeners close, control
// streams unwind, registrations deregister. When any of that fails to return —
// which is what "Ctrl-C does not stop it" means — the operator is left with a
// process they cannot end from the terminal. So: the first interrupt cancels,
// and if the process is still alive interruptStopGrace later, or a second
// interrupt arrives, it prints where every goroutine is parked and exits 130.
//
// The dump is the point on Windows, where there is no SIGQUIT to ask for one:
// the stack of a hung shutdown is exactly the evidence needed to fix it, and
// it has to be captured on the machine that reproduces it.
func interruptContext(label string, parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt)
	stopped := make(chan struct{})
	go func() {
		select {
		case <-sig:
		case <-ctx.Done():
			return
		}
		fmt.Fprintln(os.Stderr, label+": stopping…")
		cancel()
		select {
		case <-stopped:
		case <-sig: // second interrupt: stop waiting
			forceExitWithStacks(label, "interrupted twice")
		case <-time.After(interruptStopGrace):
			forceExitWithStacks(label, fmt.Sprintf("did not stop within %s", interruptStopGrace))
		}
	}()
	return ctx, func() {
		signal.Stop(sig)
		close(stopped)
		cancel()
	}
}

// forceExitWithStacks reports why the process is leaving and dumps every
// goroutine, so a shutdown that hung leaves evidence rather than a mystery.
func forceExitWithStacks(label, reason string) {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	fmt.Fprintf(os.Stderr, "%s: %s — forcing exit. Goroutine dump follows.\n%s\n", label, reason, buf[:n])
	os.Exit(130)
}

// stringList collects a repeatable string flag, in the order given — header
// order is preserved all the way to the wire.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ", ") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// readFlagBody resolves a --http-body value: a literal, @file, or - for stdin.
func readFlagBody(v string) ([]byte, error) {
	switch {
	case v == "":
		return nil, nil
	case v == "-":
		return io.ReadAll(os.Stdin)
	case strings.HasPrefix(v, "@"):
		return os.ReadFile(v[1:])
	default:
		return []byte(v), nil
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: harness-cli [--server-cid CID] [--ws-path PATH] [--config PATH] [--workspace NAME] <subcommand> [args]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Global flags fall back to env, then to a --workspace (flag > env > workspace > default):")
	fmt.Fprintln(os.Stderr, "  --server-cid  HARNESS_SERVER_CID  (workspace: server-cid; default ws:127.0.0.1:8539-*)")
	fmt.Fprintln(os.Stderr, "  --ws-path     HARNESS_WS_PATH     (workspace: ws-path; default /ws)")
	fmt.Fprintln(os.Stderr, "  --config      HARNESS_CONFIG      (default ./.harness/config; never read inside a task)")
	fmt.Fprintln(os.Stderr, "  --workspace   NAME                which workspace in that file supplies server-cid / ws-path / repo")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Subcommands:")
	fmt.Fprintln(os.Stderr, "  submit --repo REPO --task TEXT [--runner HEX | --host NAME | --ip ADDR] [--agent-arg ARG ...] [--agent NAME] [--resume TASK_ID] [--resume-conversation] [--caps NAMES] [--scope SPEC]")
	fmt.Fprintln(os.Stderr, "                                      enqueue a new task (--repo: HARNESS_REPO_PATH)")
	fmt.Fprintln(os.Stderr, "                                      --agent-arg is repeatable; appended after runner-global --agent-args; --claude-arg remains as a deprecated alias")
	fmt.Fprintln(os.Stderr, "                                      --agent: agent profile name to run this task under (empty = runner default; not to be confused with --agent-arg)")
	fmt.Fprintln(os.Stderr, "                                      --resume reuses an existing terminal task id + worktree branch (so `--agent-arg --resume <uuid>` forwards the agent's stored-session flag)")
	fmt.Fprintln(os.Stderr, "                                      --caps: comma-separated capability names to grant (e.g. spawn,file_read / all / none); default all. On --resume, --caps re-grants caps to the task (else its persisted caps are kept)")
	fmt.Fprintln(os.Stderr, "                                      --scope: which tasks those caps may target: "+cli.ScopeGrammar+"; default subtree")
	fmt.Fprintln(os.Stderr, "  ls [--json]                         list runners and recent tasks; --json emits one {runners,tasks} object")
	fmt.Fprintln(os.Stderr, "  conns [-f|--follow] [--json]        snapshot live connections (requires info_global cap); -f streams live events; --json emits JSON lines")
	fmt.Fprintln(os.Stderr, "  caps [--json]                       list the grantable --caps capability names and --scope forms")
	fmt.Fprintln(os.Stderr, "  caps set TASK_ID [--caps NAMES] [--scope SPEC] [--cascade] [--keep-conns]")
	fmt.Fprintln(os.Stderr, "                                      OPERATOR ONLY: re-grant a LIVE task's caps and/or scope; effective on its next request, no restart")
	fmt.Fprintln(os.Stderr, "  whoami [--json]                     show THIS connection's own principal + server-enforced caps and scope (no cap required)")
	fmt.Fprintln(os.Stderr, "  skill [NAME | --list]               print the embedded agent skill (default: harness-cli); --list/ls names them all")
	fmt.Fprintln(os.Stderr, "  version [--json]                    the commit this binary — and the skills embedded in it — was built from")
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
	fmt.Fprintln(os.Stderr, "  interactive --repo REPO [--runner HEX | --host NAME | --ip ADDR] [--agent-arg ARG ...] [--agent NAME] [--resume TASK_ID] [--resume-conversation] [--caps NAMES] [--scope SPEC]")
	fmt.Fprintln(os.Stderr, "                                      attach an interactive PTY agent; the session is detachable (--repo: HARNESS_REPO_PATH)")
	fmt.Fprintln(os.Stderr, "                                      --agent-arg is repeatable; appended after runner-global --agent-args; --claude-arg remains as a deprecated alias")
	fmt.Fprintln(os.Stderr, "                                      --agent: agent profile name to run this session under (empty = runner default; not to be confused with --agent-arg)")
	fmt.Fprintln(os.Stderr, "                                      --resume reuses an existing terminal interactive task id + worktree branch")
	fmt.Fprintln(os.Stderr, "  session new --repo REPO [-d|--detach] [--runner HEX | --host NAME | --ip ADDR] [--agent-arg ARG ...] [--agent NAME] [--resume TASK_ID] [--resume-conversation] [--caps NAMES] [--scope SPEC]")
	fmt.Fprintln(os.Stderr, "                                      open a detachable interactive PTY session (--repo: HARNESS_REPO_PATH)")
	fmt.Fprintln(os.Stderr, "                                      --agent-arg is repeatable; appended after runner-global --agent-args; --claude-arg remains as a deprecated alias")
	fmt.Fprintln(os.Stderr, "                                      --agent: agent profile name to run this session under (empty = runner default; not to be confused with --agent-arg)")
	fmt.Fprintln(os.Stderr, "                                      -d / --detach: start the session and exit immediately (don't attach the terminal)")
	fmt.Fprintln(os.Stderr, "  session attach TASK_ID              reattach to a detached/running session")
	fmt.Fprintln(os.Stderr, "  session snapshot [--rows N] [--cols N] [--settle-ms MS] [--style] [--color] [--detect] [--json] [--raw] TASK_ID")
	fmt.Fprintln(os.Stderr, "                                      print the session's current PTY screen as text (view attach; non-intrusive, works without a TTY)")
	fmt.Fprintln(os.Stderr, "                                      --style/--color append attribute/color spans; --json emits {rows,cols,title,lines[],spans[]} instead of text")
	fmt.Fprintln(os.Stderr, "                                      --detect judges the state: working / blocked (waiting on a HUMAN) / idle / unknown, naming the rule and the text it read")
	fmt.Fprintln(os.Stderr, "                                      --raw writes the verbatim PTY bytes instead of the VT render (not combinable with --style/--color/--json/--detect)")
	fmt.Fprintln(os.Stderr, "  session send [-enter] [-e] [-quiet] [--flush-ms MS] TASK_ID TEXT...")
	fmt.Fprintln(os.Stderr, "                                      inject input into a session (co-writer attach, no takeover); pair with snapshot to drive it statelessly")
	fmt.Fprintln(os.Stderr, "                                      -enter appends a CR (i.e. actually submits); -e interprets \\n \\r \\t \\e \\xHH")
	fmt.Fprintln(os.Stderr, "                                      flags must precede TASK_ID; everything after it is joined with spaces and sent literally")
	fmt.Fprintln(os.Stderr, "  session exec [--timeout D] [--json] [--exit-only] [--raw] TASK_ID CMD...")
	fmt.Fprintln(os.Stderr, "                                      run one shell command line in the session's foreground shell and block until it finishes")
	fmt.Fprintln(os.Stderr, "                                      exits with the command's own code (124 timeout, 125 error, 126 foreground shell exited); needs a POSIX shell")
	fmt.Fprintln(os.Stderr, "                                      NOT `exec`, which runs its own process in the worktree with separate stdout/stderr")
	fmt.Fprintln(os.Stderr, "                                      flags must precede TASK_ID; everything after it is joined with spaces as the command line")
	fmt.Fprintln(os.Stderr, "  session ls                          JSON Lines: interactive sessions only")
	fmt.Fprintln(os.Stderr, "  session kill TASK_ID                cancel a session (alias of cancel)")
	fmt.Fprintln(os.Stderr, "  session await-idle [--threshold-ms N] [--notify | --topic T] TASK_ID")
	fmt.Fprintln(os.Stderr, "                                      one-shot: fire when the session's PTY output goes quiescent.")
	fmt.Fprintln(os.Stderr, "                                      default long-polls; --notify/--topic arm a server-side sink and return")
	fmt.Fprintln(os.Stderr, "  server dial-runner [--via CID] RUNNER_CID  ask the server to reverse-dial a Listen-mode runner")
	fmt.Fprintln(os.Stderr, "  board topics|read <topic>|subscribers [topic]|retract <topic> --seq N|purge <topic> [--seq N]")
	fmt.Fprintln(os.Stderr, "                                      inspect/withdraw/purge the agentboard (cap: info_global; retract and purge: purge)")
	fmt.Fprintln(os.Stderr, "  agent {send|wait|inbox|subscribe|unsubscribe|dispatch|topics|subscriptions}")
	fmt.Fprintln(os.Stderr, "                                      agent-to-agent message ops (env-primary; HARNESS_AUTH_TICKET required)")
	fmt.Fprintln(os.Stderr, "  file push [-r|--recursive] [-f|--force] [-p|--parents] TASK_ID LOCAL_SRC WORKTREE_REL_DST")
	fmt.Fprintln(os.Stderr, "                                      copy a local file (or directory tree with -r) into the worktree")
	fmt.Fprintln(os.Stderr, "                                      default: O_EXCL refuses to overwrite; -f permits replacement")
	fmt.Fprintln(os.Stderr, "  file mkdir [-p|--parents] TASK_ID WORKTREE_REL_DIR")
	fmt.Fprintln(os.Stderr, "                                      create a directory in the worktree")
	fmt.Fprintln(os.Stderr, "  file pull [-r|--recursive] [-f|--force] TASK_ID WORKTREE_REL_SRC LOCAL_DST")
	fmt.Fprintln(os.Stderr, "                                      copy a worktree file (or directory tree with -r) to a local path")
	fmt.Fprintln(os.Stderr, "                                      default: O_EXCL refuses to overwrite local; -f permits replacement")
	fmt.Fprintln(os.Stderr, "  file ls   TASK_ID [WORKTREE_REL_DIR]")
	fmt.Fprintln(os.Stderr, "                                      list a single directory under the worktree (default: worktree root)")
	fmt.Fprintln(os.Stderr, "  file delete [-r|--recursive] [-f|--force] TASK_ID WORKTREE_REL_PATH")
	fmt.Fprintln(os.Stderr, "                                      remove a file; -r a directory (dir_delete), -r -f a non-empty directory (RemoveAll); without -r a directory is refused")
	fmt.Fprintln(os.Stderr, "  git TASK_ID log    [--max N] [-- PATH]")
	fmt.Fprintln(os.Stderr, "  git TASK_ID diff   [--staged] [BASE] [TARGET] [--max-bytes N] [-- PATH]")
	fmt.Fprintln(os.Stderr, "  git TASK_ID show   [REV] [-- PATH]")
	fmt.Fprintln(os.Stderr, "  git TASK_ID status [-- PATH]")
	fmt.Fprintln(os.Stderr, "  git TASK_ID subrepos")
	fmt.Fprintln(os.Stderr, "  git TASK_ID file   [--staged | --rev REV] PATH")
	fmt.Fprintln(os.Stderr, "                                      read-only git view of a task's worktree (requires file_read)")
	fmt.Fprintln(os.Stderr, "                                      runs in the worktree while the task lives, and against the retained")
	fmt.Fprintln(os.Stderr, "                                      harness/<task-id> branch after it ends (committed work only)")
	fmt.Fprintln(os.Stderr, "                                      diff counts revisions the way git does: none = unstaged, one = that")
	fmt.Fprintln(os.Stderr, "                                      revision against the working tree, two = commit against commit")
	fmt.Fprintln(os.Stderr, "                                      --subrepo DIR runs any of them inside a nested repository (a plain")
	fmt.Fprintln(os.Stderr, "                                      nested repo is invisible from outside it); subrepos lists them")
	fmt.Fprintln(os.Stderr, "                                      --submodule on diff/show inlines a submodule's own changes")
	fmt.Fprintln(os.Stderr, "  workspace save <name> [--task <32-hex>] [--resume no|continue|fresh] [--runner assigned|any] [--repo PATH]")
	fmt.Fprintln(os.Stderr, "                                      record the registered forwards into .harness/config as a named workspace;")
	fmt.Fprintln(os.Stderr, "                                      every task with one unless --task narrows it. MERGES: blocks it did not")
	fmt.Fprintln(os.Stderr, "                                      observe are kept and an existing block's resume/runner are never reset")
	fmt.Fprintln(os.Stderr, "                                      (in-process forwards — a raw TUI pane, a WebUI preview pin — have no local")
	fmt.Fprintln(os.Stderr, "                                      address to write down and are skipped, with a count)")
	fmt.Fprintln(os.Stderr, "  workspace rm <name>                 delete one workspace from .harness/config (other workspaces and comments kept)")
	fmt.Fprintln(os.Stderr, "  workspace ls | show [<name>]        list the workspaces in .harness/config, or print one")
	fmt.Fprintln(os.Stderr, "                                      the TUI applies a workspace on start, on reconnect, and on `workspace apply`,")
	fmt.Fprintln(os.Stderr, "                                      and `workspace detach [--stop]` there stops it re-applying;")
	fmt.Fprintln(os.Stderr, "                                      neither exists here — a forward dies with the process that holds it")
	fmt.Fprintln(os.Stderr, "  forward <task-id> [-L [bind:]localport:remotehost:remoteport] [-R [bind:]runnerport:dialhost:dialport] ...")
	fmt.Fprintln(os.Stderr, "                                      -L: forward a local port through the runner to remote host:port (ssh -L)")
	fmt.Fprintln(os.Stderr, "                                      -R: runner listens, connections dial back to a client-side host:port (ssh -R)")
	fmt.Fprintln(os.Stderr, "                                      both repeatable; Ctrl-C to stop")
	fmt.Fprintln(os.Stderr, "  forward <task-id> -W host:port")
	fmt.Fprintln(os.Stderr, "                                      raw stdio forward (ssh -W): no local listener, this process's stdin/stdout is the client endpoint")
	fmt.Fprintln(os.Stderr, "                                    [--http-path /p [--http-method M] [--http-header 'N: v'] [--http-body B|@file|-]]")
	fmt.Fprintln(os.Stderr, "                                      with -W: send one built HTTP request and stream the response (stdin is not spliced)")
	fmt.Fprintln(os.Stderr, "                                      mutually exclusive with -L / -R; not repeatable; exits with its peer")
	fmt.Fprintln(os.Stderr, "  forward ls [--task TASK_ID] [--json]")
	fmt.Fprintln(os.Stderr, "                                      list registered port forwards; --task filters, --json emits JSON lines")
	fmt.Fprintln(os.Stderr, "  forward kill FORWARD_ID [FORWARD_ID ...]")
	fmt.Fprintln(os.Stderr, "                                      kill one or more registered forwards by id (from `forward ls`)")
	fmt.Fprintln(os.Stderr, "  exec [--shell] [--sshd-parent] <task-id> -- <command> [args...]")
	fmt.Fprintln(os.Stderr, "                                      run a command in the task's WORKTREE as its own process:")
	fmt.Fprintln(os.Stderr, "                                      stdout and stderr stay separate, and the command's own exit code becomes ours")
	fmt.Fprintln(os.Stderr, "                                      works on a FINISHED task too, as long as its worktree is still there —")
	fmt.Fprintln(os.Stderr, "                                      a task that ended with uncommitted work keeps one")
	fmt.Fprintln(os.Stderr, "                                      dies with this process; for something to leave running, submit a task instead")
	fmt.Fprintln(os.Stderr, "                                      NOT `session exec`, which types into the session's foreground shell")
	fmt.Fprintln(os.Stderr, "                                      --shell: hand it to the RUNNER's shell as one line (sh -c / cmd /c by its platform)")
	fmt.Fprintln(os.Stderr, "  exec ls [--task TASK_ID] [--json]   list running execs; --task filters, --json emits JSON lines")
	fmt.Fprintln(os.Stderr, "  exec kill EXEC_ID [EXEC_ID ...]     stop one or more running execs by id (from `exec ls`)")
	fmt.Fprintln(os.Stderr, "  ssh-gateway [--listen 127.0.0.1:2222] [--host-key PATH] [--authorized-keys PATH]")
	fmt.Fprintln(os.Stderr, "                                      serve ssh: `ssh -p 2222 <32-hex-task-id>@127.0.0.1` attaches to that session,")
	fmt.Fprintln(os.Stderr, "                                      so ssh config aliases, tmux and mosh reach a task with no harness binary there")
	fmt.Fprintln(os.Stderr, "                                      the user name picks the mode: bare = cowrite (evicts nobody), .control takes")
	fmt.Fprintln(os.Stderr, "                                      the seat and owns the PTY size, .view watches")
	fmt.Fprintln(os.Stderr, "                                      Ctrl+] detaches. ssh's own ~. DISCONNECTS instead: the session survives either")
	fmt.Fprintln(os.Stderr, "                                      way, but a disconnect leaves your terminal's modes unreset (`reset` fixes it)")
	fmt.Fprintln(os.Stderr, "                                      no ssh auth on a loopback bind; --authorized-keys is REQUIRED off loopback")
	fmt.Fprintln(os.Stderr, "                                      ssh -L / -W tunnel through it: the RUNNER dials the target, and each")
	fmt.Fprintln(os.Stderr, "                                      forwarded connection is an ordinary `forward ls` row while it lasts")
	fmt.Fprintln(os.Stderr, "                                      no scp/sftp and no ssh -R: use `file push`/`file pull` and `forward -R`")
	fmt.Fprintln(os.Stderr, "                                      foreground; Ctrl-C stops it and every session it serves")
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
	fmt.Fprintln(os.Stderr, "  read <topic> [--in-reply-to N] [--json]  print retained messages for <topic> (text: header + pretty payload; --json: JSON Lines, same record shape as agent inbox --json; not found = exit 0)")
	fmt.Fprintln(os.Stderr, "  subscribers [topic]                 list each task's subscriptions; with <topic>, only the tasks a publish there reaches (cap: info_global)")
	fmt.Fprintln(os.Stderr, "  retract <topic> --seq N             withdraw one message: gone from every agent path, still readable here")
	fmt.Fprintln(os.Stderr, "                                      until the topic ages out. --seq is required — there is no whole-topic")
	fmt.Fprintln(os.Stderr, "                                      retract (cap: purge)")
	fmt.Fprintln(os.Stderr, "  purge <topic> [--seq N]             drop the whole topic ring (seq=0) or one message by seq. Unlike retract")
	fmt.Fprintln(os.Stderr, "                                      this destroys the bytes, operator view included (cap: purge)")
}

func agentUsage() {
	fmt.Fprintln(os.Stderr, "usage: harness-cli agent <subcommand> [flags]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Env-primary (HARNESS_*): SERVER_CID, TASK_ID, RUNNER_ID, HOSTNAME, WS_PATH, REPO_PATH")
	fmt.Fprintln(os.Stderr, "HARNESS_AUTH_TICKET is env-only (no flag accepted).")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Subcommands:")
	fmt.Fprintln(os.Stderr, "  send --topic T TEXT...              publish a message. The body is the trailing words, or")
	fmt.Fprintln(os.Stderr, "                                       --data STRING, or --data - to read it from stdin. \"-\" is a")
	fmt.Fprintln(os.Stderr, "                                       VALUE OF --data, never a positional: `send --topic T -`")
	fmt.Fprintln(os.Stderr, "                                       publishes the one-byte body \"-\". The ok line reports bytes")
	fmt.Fprintln(os.Stderr, "                                       and source, so a body that went out wrong says so at once")
	fmt.Fprintln(os.Stderr, "  send --in-reply-to SEQ TEXT...      reply to SEQ; --topic optional (server routes it where that message asked)")
	fmt.Fprintln(os.Stderr, "  send --reply-to R ...               route replies to THIS message to R instead of your own")
	fmt.Fprintln(os.Stderr, "                                       chat.<short-id>; the peer answers with --in-reply-to alone")
	fmt.Fprintln(os.Stderr, "  wait --topic T [--since N] [--in-reply-to SEQ] [--timeout DUR]")
	fmt.Fprintln(os.Stderr, "                                       take everything after --since, blocking only if there is nothing;")
	fmt.Fprintln(os.Stderr, "                                       omitting --since means cursor 0, so a non-empty ring returns AT ONCE")
	fmt.Fprintln(os.Stderr, "                                       with old messages (scripting; NOT from an agent turn)")
	fmt.Fprintln(os.Stderr, "  inbox [--since N] [--in-reply-to SEQ]  idempotent dump of subscribed topics; --since 0 (default) = whole ring")
	fmt.Fprintln(os.Stderr, "  read SEQ                            fetch one retained message, whole; the hooks name it when they")
	fmt.Fprintln(os.Stderr, "                                       decline to inline a large body. Limited to subscribed topics")
	fmt.Fprintln(os.Stderr, "  subscribe --topic T                  register a subscription")
	fmt.Fprintln(os.Stderr, "  unsubscribe --topic T                remove a subscription")
	fmt.Fprintln(os.Stderr, "  dispatch --topic T TEXT... [--reply-to R] [--timeout DUR]")
	fmt.Fprintln(os.Stderr, "                                       send, then block for the reply to THAT message. --reply-to R")
	fmt.Fprintln(os.Stderr, "                                       declares R as the destination AND waits there; default is your")
	fmt.Fprintln(os.Stderr, "                                       own chat.<short-id>. --timeout bounds the WHOLE call, publish")
	fmt.Fprintln(os.Stderr, "                                       ack included (scripting; NOT from an agent turn)")
	fmt.Fprintln(os.Stderr, "  topics                              list every topic on the board (JSON Lines) (cap: info_global)")
	fmt.Fprintln(os.Stderr, "  subscriptions                       list this agent's registered patterns (JSON Lines)")
	fmt.Fprintln(os.Stderr, "  retained --topic T | --self          list a topic's retained ring as metadata only, no payload (no cap)")
	fmt.Fprintln(os.Stderr, "  purge --topic T | --self [--seq N]   drop a topic's retained buffer, or one message by seq (cap: purge)")
	fmt.Fprintln(os.Stderr, "  retract SEQ                         withdraw a message YOU sent: gone from every agent path, still")
	fmt.Fprintln(os.Stderr, "                                       visible to the operator as retracted (no cap; authorship-checked)")
	fmt.Fprintln(os.Stderr, "                                       a reply to a message addressed to you retracts it automatically;")
	fmt.Fprintln(os.Stderr, "                                       send --no-retire-on-reply to keep one alive past its answer")
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
// Used for --agent-arg so callers can write
//
//	harness-cli submit --agent-arg --resume --agent-arg <uuid> ...
//
// without shell-quoting concerns. The value is appended in the order the
// flags appear, which is the order forwarded to the agent.
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
func capsExplicitlySet(fs *flag.FlagSet) bool { return flagExplicitlySet(fs, "caps") }

// flagExplicitlySet reports whether the named flag was actually typed. Both
// --caps and --scope need this: their zero values are meaningful ("none" and
// "subtree"), so "was it given?" cannot be read off the value.
func flagExplicitlySet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// runCapsSet backs `harness-cli caps set <task-id>`: an operator-only live
// re-grant of a task's capabilities and/or scope. It takes effect on the
// target's next request — nothing restarts.
func runCapsSet(ctx context.Context, serverCID objproto.ConnectionID, args []string) {
	fs := flag.NewFlagSet("caps set", flag.ExitOnError)
	capsFlag := fs.String("caps", "", "new capability set (same syntax as --caps on submit); omitted = keep the task's current caps")
	scopeFlag := fs.String("scope", "", "new scope: "+cli.ScopeGrammar+"; omitted = keep the task's current scope")
	var scopeFor scopeForFlag
	fs.Var(&scopeFor, "scope-for", cli.ScopeForFlagUsage+" (written with --scope; they are one half of the authority)")
	cascade := fs.Bool("cascade", false, "also clamp every descendant to the new authority — without this a revoked task can still act through a child it spawned while it was wider")
	keepConns := fs.Bool("keep-conns", false, "on a narrowing, leave the affected tasks' connections open (default: close them, so in-flight attaches and transfers die with the grant)")
	// Interspersed parse: Go's flag package stops at the first non-flag
	// argument, so `caps set <id> --caps X` would silently leave --caps unset
	// with the id in front. Re-parsing after each positional accepts either
	// order, which is what anyone types.
	// One implementation of the peel, shared with every other verb that takes a
	// hex id and flags in either order — this used to be a copy of it.
	positional, perr := cli.ParsePermuted(fs, args)
	if perr != nil {
		os.Exit(2)
	}

	if len(positional) != 1 {
		fmt.Fprintln(os.Stderr, "caps set: exactly one task id required")
		fmt.Fprintln(os.Stderr, "  usage: harness-cli caps set <task-id> [--caps NAMES] [--scope SPEC] [--cascade] [--keep-conns]")
		os.Exit(2)
	}
	opts := cli.SetCapsOpts{TaskID: positional[0], Cascade: *cascade, KeepConns: *keepConns}
	if flagExplicitlySet(fs, "caps") {
		caps, err := cli.ParseCaps(*capsFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, "caps set: --caps:", err)
			os.Exit(2)
		}
		opts.Caps = cli.CapsPtr(caps)
	}
	if flagExplicitlySet(fs, "scope") {
		scope, err := cli.ParseScope(*scopeFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, "caps set: --scope:", err)
			os.Exit(2)
		}
		opts.Scope = &scope
		opts.Overrides = scopeFor.out
	}
	if opts.Caps == nil && opts.Scope == nil {
		fmt.Fprintln(os.Stderr, "caps set: pass --caps, --scope, or both — there is nothing to change otherwise")
		os.Exit(2)
	}

	res, err := cli.SetCaps(ctx, serverCID, opts)
	if err != nil {
		die(err)
	}
	for _, id := range res.Affected {
		fmt.Println(id)
	}
	fmt.Fprintf(os.Stderr, "changed %d task(s); closed %d connection(s)\n",
		len(res.Affected), res.ConnsClosed)
}

// runCapsSetParent backs `harness-cli caps set-parent <task-id>`: an
// operator-only re-point of a live task's parent link (the edge subtree
// scopes walk), or --swap to invert the task with its current parent. Caps
// and scope are untouched — `caps set` is the verb that changes authority.
func runCapsSetParent(ctx context.Context, serverCID objproto.ConnectionID, args []string) {
	fs := flag.NewFlagSet("caps set-parent", flag.ExitOnError)
	parentFlag := fs.String("parent", "", "new parent task id (32 hex); the target and its whole subtree move under it")
	noneFlag := fs.Bool("none", false, "detach the task to the operator root")
	swapFlag := fs.Bool("swap", false, "invert the task with its CURRENT parent: the task takes the parent's place and the parent becomes its child")
	// Interspersed parse, same as runCapsSet: Go's flag package stops at the
	// first non-flag argument, so `caps set-parent <id> --swap` would silently
	// leave --swap unset with the id in front.
	// One implementation of the peel, shared with every other verb that takes a
	// hex id and flags in either order — this used to be a copy of it.
	positional, perr := cli.ParsePermuted(fs, args)
	if perr != nil {
		os.Exit(2)
	}

	usage := func() {
		fmt.Fprintln(os.Stderr, "  usage: harness-cli caps set-parent <task-id> (--parent <task-id> | --none | --swap)")
		os.Exit(2)
	}
	if len(positional) != 1 {
		fmt.Fprintln(os.Stderr, "caps set-parent: exactly one task id required")
		usage()
	}
	picked := 0
	for _, on := range []bool{*parentFlag != "", *noneFlag, *swapFlag} {
		if on {
			picked++
		}
	}
	if picked != 1 {
		fmt.Fprintln(os.Stderr, "caps set-parent: pass exactly one of --parent <task-id>, --none, --swap")
		usage()
	}

	opts := cli.SetParentOpts{TaskID: positional[0], ParentID: *parentFlag, Swap: *swapFlag}
	res, err := cli.SetParent(ctx, serverCID, opts)
	if err != nil {
		die(err)
	}
	fmt.Println(cli.SetParentMessage(opts, res))
}

// runFileEdit pulls a worktree file, opens it in $EDITOR, and writes it back.
// A CLI has no terminal UI of its own to host an editor widget, so unlike the
// TUI this path always goes through an external editor.
func runFileEdit(ctx context.Context, c *cli.Client, taskID, rel string) error {
	doc, err := c.FileEditLoad(ctx, taskID, rel, nil)
	if err != nil {
		return err
	}
	edited, tmp, err := editViaExternalEditor(rel, doc.Text)
	if err != nil {
		return err
	}
	for force := false; ; force = true {
		st, cerr := c.FileEditCommit(ctx, taskID, doc, edited, force)
		if cerr != nil {
			return fmt.Errorf("%w (your edit is kept at %s)", cerr, tmp)
		}
		switch st {
		case cli.FileEditUnchanged:
			os.Remove(tmp)
			fmt.Printf("no change: %s\n", rel)
			return nil
		case cli.FileEditPushed:
			os.Remove(tmp)
			fmt.Printf("saved: %s\n", rel)
			return nil
		}
		fmt.Fprintf(os.Stderr, "%s changed on the runner since it was read. Overwrite? [y/N] ", rel)
		var answer string
		fmt.Fscanln(os.Stdin, &answer)
		if answer != "y" && answer != "Y" {
			fmt.Fprintf(os.Stderr, "not overwritten; your edit is kept at %s\n", tmp)
			return nil
		}
	}
}

// runFileNew opens an empty buffer in $EDITOR and pushes it to rel.
func runFileNew(ctx context.Context, c *cli.Client, taskID, rel string) error {
	text, tmp, err := editViaExternalEditor(rel, "")
	if err != nil {
		return err
	}
	if err := c.FilePushBytes(ctx, taskID, []byte(text), rel, cli.FilePushOpts{MkdirParents: true}, nil); err != nil {
		return fmt.Errorf("%w (your text is kept at %s)", err, tmp)
	}
	os.Remove(tmp)
	fmt.Printf("created: %s\n", rel)
	return nil
}

// editViaExternalEditor spools text to a temp file, runs $EDITOR on it with
// this process's stdio, and returns the result. The temp path comes back too
// so callers can name it when a later step fails and the edit would otherwise
// be lost.
func editViaExternalEditor(name, text string) (string, string, error) {
	f, err := os.CreateTemp("", "harness-edit-*"+filepath.Ext(name))
	if err != nil {
		return "", "", err
	}
	tmp := f.Name()
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", "", err
	}
	f.Close()
	cmd, err := cli.ExternalEditorCommand(tmp)
	if err != nil {
		os.Remove(tmp)
		return "", "", err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", tmp, fmt.Errorf("editor exited with an error: %w (your text is kept at %s)", err, tmp)
	}
	b, err := os.ReadFile(tmp)
	if err != nil {
		return "", tmp, err
	}
	return string(b), tmp, nil
}

// tapMode turns the four mutually exclusive output flags into one mode. Two at
// once is refused rather than silently ranked: a caller that asked for both
// --raw and --json meant something, and guessing which would be wrong half the
// time.
func tapMode(asHex, asText, asRaw, asJSON bool) (cli.TapRenderMode, error) {
	n := 0
	mode := cli.TapHex
	for _, c := range []struct {
		on bool
		m  cli.TapRenderMode
	}{{asHex, cli.TapHex}, {asText, cli.TapText}, {asRaw, cli.TapRaw}, {asJSON, cli.TapJSON}} {
		if c.on {
			n++
			mode = c.m
		}
	}
	if n > 1 {
		return 0, errors.New("forward tap: --hex, --text, --raw and --json are mutually exclusive")
	}
	return mode, nil
}
