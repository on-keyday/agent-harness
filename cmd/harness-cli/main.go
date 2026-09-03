//go:build !js

package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/cliopts"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/cli/workspace"
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

	// The two ladder tiers this SURFACE owns (D7), installed once instead of
	// passed at each call site -- three of which used to pass nil and lose
	// HARNESS_REPO_PATH on verbs that declare it.
	verb.EnvLookup = os.Getenv
	verb.WorkspaceLookup = func(k string) string {
		if k == "repo" {
			return workspaceRepo
		}
		return ""
	}
	workspaceCfgPath = *configPath
	workspaceServerCIDStr = cliopts.ResolveStringWith(*serverCID, "HARNESS_SERVER_CID", wsServerCID)

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

	// Everything the declaration gives this surface, routed by the GENERATED
	// dispatcher. It was a 29-case switch over the FIRST WORD plus, inside
	// eight of those cases, a hand-written walk over the second and third --
	// the shape that let `harness-cli` carry three verbs the table did not
	// know, and let the table carry verbs this binary never reached.
	//
	// `git <task-id> <sub>` is the one line the path match cannot find on its
	// own: the id sits in the MIDDLE of the verb, so no prefix of the tokens
	// is the path. It is peeled here exactly as the TUI and the WebUI peel it,
	// and goes through the same generated entry.
	tokens := append([]string{sub}, args...)
	h := cliVerbs{ctx: ctx, cid: parseCID}
	if sub == "git" {
		if len(args) < 2 {
			die(fmt.Errorf("usage: harness-cli git <task-id> {log|diff|show|status|subrepos|file} ..."))
		}
		act, handled, perr := verb.ParseCLICommand(append([]string{"git", args[1]}, args[2:]...), nil)
		if perr != nil {
			die(perr)
		}
		if !handled {
			die(fmt.Errorf("git: unknown sub-verb %q (log | diff | show | status | subrepos | file)", args[1]))
		}
		g := act.(verb.GitAction)
		g.TaskID = args[0]
		if err, ok := verb.DispatchCLIAction[error](h, g); ok {
			if err != nil {
				die(err)
			}
			return
		}
	}
	if err, handled, perr := verb.DispatchCLILine[error](h, tokens, nil); handled {
		if perr != nil {
			die(perr)
		}
		if err != nil {
			die(err)
		}
		return
	}
	// A family word with no sub-verb -- `file`, `session`, `board` -- reaches
	// here, and so does an outright unknown one. The four with a dedicated
	// usage keep it; every other family gets its sub-verb list FROM THE
	// TABLE, which is where the four hand-written ones (`file
	// {push|pull|ls|mkdir|delete|edit|new}` and its three siblings) each
	// drifted from: `file edit` and `file new` existed for a release without
	// appearing in that line.
	switch {
	case sub == "server":
		serverUsage()
	case sub == "board":
		boardUsage()
	case sub == "agent":
		agentUsage()
	case sub == "workspace":
		workspaceUsage()
	case len(familySubverbs(sub)) > 0:
		fmt.Fprintf(os.Stderr, "usage: harness-cli %s {%s} ...\n",
			sub, strings.Join(familySubverbs(sub), "|"))
	default:
		usage()
	}
	os.Exit(2)
}

// familySubverbs lists the declared second words under one first word, in
// table order and without repeats. Empty when the word heads no multi-word
// path -- which is what tells an unknown verb apart from a family named
// without a sub-verb.
//
// Deduplicated because a three-word path contributes its middle word once per
// leaf: `session stream {attach,turn,approve,interrupt,finish}` would
// otherwise print "stream" five times.
func familySubverbs(head string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range verb.PathsForSurface(verb.CLI) {
		f := strings.Fields(p)
		if len(f) < 2 || f[0] != head || seen[f[1]] {
			continue
		}
		seen[f[1]] = true
		out = append(out, f[1])
	}
	return out
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

func usage() { usageTo(os.Stderr) }

func usageTo(w io.Writer) {
	fmt.Fprintln(w, "usage: harness-cli [--server-cid CID] [--ws-path PATH] [--config PATH] [--workspace NAME] <subcommand> [args]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Global flags fall back to env, then to a --workspace (flag > env > workspace > default):")
	fmt.Fprintln(w, "  --server-cid  HARNESS_SERVER_CID  (workspace: server-cid; default ws:127.0.0.1:8539-*)")
	fmt.Fprintln(w, "  --ws-path     HARNESS_WS_PATH     (workspace: ws-path; default /ws)")
	fmt.Fprintln(w, "  --config      HARNESS_CONFIG      (default ./.harness/config; never read inside a task)")
	fmt.Fprintln(w, "  --workspace   NAME                which workspace in that file supplies server-cid / ws-path / repo")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Subcommands:")
	fmt.Fprintln(w, "  submit --repo REPO --task TEXT [--runner HEX | --host NAME | --ip ADDR] [--agent-arg ARG ...] [--agent NAME] [--resume TASK_ID] [--resume-conversation] [--caps NAMES] [--scope SPEC]")
	fmt.Fprintln(w, "                                      enqueue a new task (--repo: HARNESS_REPO_PATH)")
	fmt.Fprintln(w, "                                      --agent-arg is repeatable; appended after runner-global --agent-args; --claude-arg remains as a deprecated alias")
	fmt.Fprintln(w, "                                      --agent: agent profile name to run this task under (empty = runner default; not to be confused with --agent-arg)")
	fmt.Fprintln(w, "                                      --resume reuses an existing terminal task id + worktree branch (so `--agent-arg --resume <uuid>` forwards the agent's stored-session flag)")
	fmt.Fprintln(w, "                                      --caps: comma-separated capability names to grant (e.g. spawn,file_read / all / none); default all. On --resume, --caps re-grants caps to the task (else its persisted caps are kept)")
	fmt.Fprintln(w, "                                      --scope: which tasks those caps may target: "+cli.ScopeGrammar+"; default subtree")
	fmt.Fprintln(w, "  ls [--json]                         list runners and recent tasks; --json emits one {runners,tasks} object")
	fmt.Fprintln(w, "  conns [-f|--follow] [--json]        snapshot live connections (requires info_global cap); -f streams live events; --json emits JSON lines")
	fmt.Fprintln(w, "  caps [--json]                       list the grantable --caps capability names and --scope forms")
	fmt.Fprintln(w, "  caps set TASK_ID [--caps NAMES] [--scope SPEC] [--cascade] [--keep-conns]")
	fmt.Fprintln(w, "                                      OPERATOR ONLY: re-grant a LIVE task's caps and/or scope; effective on its next request, no restart")
	fmt.Fprintln(w, "  caps set-parent TASK_ID (--parent TASK_ID | --none | --swap)")
	fmt.Fprintln(w, "                                      OPERATOR ONLY: re-point a LIVE task's parent link — the edge subtree scopes walk. --none detaches it to the operator root; --swap inverts it with its current parent. Caps and scope are untouched")
	fmt.Fprintln(w, "  whoami [--json]                     show THIS connection's own principal + server-enforced caps and scope (no cap required)")
	fmt.Fprintln(w, "  skill [NAME | --list] | skill ls    print the embedded agent skill (default: harness-cli); --list, or the `ls` spelling, names them all")
	fmt.Fprintln(w, "  version [--json]                    the commit this binary — and the skills embedded in it — was built from")
	fmt.Fprintln(w, "  cancel TASK_ID                      cancel a queued/running task")
	fmt.Fprintln(w, "  notify [--title T] [--level info|warn|error] <text>")
	fmt.Fprintln(w, "                                      send a notification (one short line; detail goes in the task log)")
	fmt.Fprintln(w, "  prune [--before DUR] [-f|--force] [TASK_ID ...]")
	fmt.Fprintln(w, "                                      ask the server to forget tasks")
	fmt.Fprintln(w, "                                      no TASK_IDs: terminal tasks older than --before")
	fmt.Fprintln(w, "                                      with TASK_IDs: only those (refuses active tasks unless --force)")
	fmt.Fprintln(w, "  restore [--list]                    list what a prune forgot and could still be put back — ids, when they were pruned, and the repo/prompt that identify them. The ids live only in the server's WAL, so this is the only way to learn them")
	fmt.Fprintln(w, "  restore TASK_ID [TASK_ID ...]")
	fmt.Fprintln(w, "                                      put those back, rebuilt from the WAL. Requires the `prune` capability and the same scope: what you could forget, you can un-forget. The RECORD returns; the task log does not (prune removed the file) and the worktree was never touched. An id with no task_created cannot be rebuilt")
	fmt.Fprintln(w, "  prune-local [--repo PATH] [--before DUR] [-f|--force] [TASK_ID ...]")
	fmt.Fprintln(w, "                                      remove worktrees in <repo>/.harness-worktrees/ (--repo: HARNESS_REPO_PATH)")
	fmt.Fprintln(w, "                                      with no TASK_IDs: time-based, removes entries older than --before")
	fmt.Fprintln(w, "                                      with TASK_IDs: removes only those (refuses active tasks unless --force)")
	fmt.Fprintln(w, "  logs [-f|--follow] TASK_ID          dump task log history; -f also streams live chunks until task terminal")
	fmt.Fprintln(w, "  watch                               stream task and runner status events")
	fmt.Fprintln(w, "  notify-watch                        stream notifications (backlog + live); one human-readable line each")
	fmt.Fprintln(w, "  interactive --repo REPO [--runner HEX | --host NAME | --ip ADDR] [--agent-arg ARG ...] [--agent NAME] [--resume TASK_ID] [--resume-conversation] [--caps NAMES] [--scope SPEC]")
	fmt.Fprintln(w, "                                      attach an interactive PTY agent; the session is detachable (--repo: HARNESS_REPO_PATH)")
	fmt.Fprintln(w, "                                      --agent-arg is repeatable; appended after runner-global --agent-args; --claude-arg remains as a deprecated alias")
	fmt.Fprintln(w, "                                      --agent: agent profile name to run this session under (empty = runner default; not to be confused with --agent-arg)")
	fmt.Fprintln(w, "                                      --resume reuses an existing terminal interactive task id + worktree branch")
	fmt.Fprintln(w, "  session new --repo REPO [-d|--detach] [--runner HEX | --host NAME | --ip ADDR] [--agent-arg ARG ...] [--agent NAME] [--resume TASK_ID] [--resume-conversation] [--caps NAMES] [--scope SPEC]")
	fmt.Fprintln(w, "                                      open a detachable interactive PTY session (--repo: HARNESS_REPO_PATH)")
	fmt.Fprintln(w, "                                      --agent-arg is repeatable; appended after runner-global --agent-args; --claude-arg remains as a deprecated alias")
	fmt.Fprintln(w, "                                      --agent: agent profile name to run this session under (empty = runner default; not to be confused with --agent-arg)")
	fmt.Fprintln(w, "                                      -d / --detach: start the session and exit immediately (don't attach the terminal)")
	fmt.Fprintln(w, "  session attach TASK_ID              reattach to a detached/running session")
	fmt.Fprintln(w, "  session snapshot [--rows N] [--cols N] [--settle-ms MS] [--style] [--color] [--detect] [--json] [--raw] TASK_ID")
	fmt.Fprintln(w, "                                      print the session's current PTY screen as text (view attach; non-intrusive, works without a TTY)")
	fmt.Fprintln(w, "                                      --style/--color append attribute/color spans; --json emits {rows,cols,title,lines[],spans[]} instead of text")
	fmt.Fprintln(w, "                                      --detect judges the state: working / blocked (waiting on a HUMAN) / idle / unknown, naming the rule and the text it read")
	fmt.Fprintln(w, "                                      --raw writes the verbatim PTY bytes instead of the VT render (not combinable with --style/--color/--json/--detect)")
	fmt.Fprintln(w, "  session resize TASK_ID --size ROWSxCOLS [--wait-ms MS] [--quiet]")
	fmt.Fprintln(w, "                                      set a live session's PTY size; the server echoing the new size back IS the acknowledgement")
	fmt.Fprintln(w, "  session send [-enter] [-e] [-quiet] [--flush-ms MS] TASK_ID TEXT...")
	fmt.Fprintln(w, "                                      inject input into a session (co-writer attach, no takeover); pair with snapshot to drive it statelessly")
	fmt.Fprintln(w, "                                      -enter appends a CR (i.e. actually submits); -e interprets \\n \\r \\t \\e \\xHH")
	fmt.Fprintln(w, "                                      flags must precede TASK_ID; everything after it is joined with spaces and sent literally")
	fmt.Fprintln(w, "  session exec [--timeout D] [--json] [--exit-only] [--raw] TASK_ID CMD...")
	fmt.Fprintln(w, "                                      run one shell command line in the session's foreground shell and block until it finishes")
	fmt.Fprintln(w, "                                      exits with the command's own code (124 timeout, 125 error, 126 foreground shell exited); needs a POSIX shell")
	fmt.Fprintln(w, "                                      NOT `exec`, which runs its own process in the worktree with separate stdout/stderr")
	fmt.Fprintln(w, "                                      flags must precede TASK_ID; everything after it is joined with spaces as the command line")
	fmt.Fprintln(w, "  session ls                          JSON Lines: interactive sessions only")
	fmt.Fprintln(w, "  session kill TASK_ID                cancel a session (alias of cancel)")
	fmt.Fprintln(w, "  session stream turn TASK_ID TEXT...  send one user turn to an event-stream session")
	fmt.Fprintln(w, "  session stream approve TASK_ID REQUEST_ID (--allow | --deny [--message REASON]) [--suggestion N]")
	fmt.Fprintln(w, "                                      answer one pending tool request. The request id is the staleness guard: an answer aimed at a request that has gone is REFUSED, not applied to whatever is pending now")
	fmt.Fprintln(w, "                                      --message is the DENY reason and reaches the AGENT verbatim as a failed tool result; --suggestion accepts the request's Nth suggestion (a STANDING change, so it rides either verdict)")
	fmt.Fprintln(w, "  session stream attach TASK_ID       follow an event-stream session's events")
	fmt.Fprintln(w, "  session stream interrupt TASK_ID    abandon the running TURN; the agent survives to take the next one")
	fmt.Fprintln(w, "  session stream finish TASK_ID       close the agent's stdin so it completes the turn in flight and exits 0")
	fmt.Fprintln(w, "  session await-idle [--threshold-ms N] [--notify | --topic T] TASK_ID")
	fmt.Fprintln(w, "                                      one-shot: fire when the session's PTY output goes quiescent.")
	fmt.Fprintln(w, "                                      default long-polls; --notify/--topic arm a server-side sink and return")
	fmt.Fprintln(w, "  server dial-runner [--via CID] RUNNER_CID  ask the server to reverse-dial a Listen-mode runner")
	fmt.Fprintln(w, "  board topics|read <topic>|subscribers [topic]|retract <topic> --seq N|purge <topic> [--seq N]")
	fmt.Fprintln(w, "                                      inspect/withdraw/purge the agentboard (cap: info_global; retract and purge: purge)")
	fmt.Fprintln(w, "  agent {send|wait|inbox|subscribe|unsubscribe|dispatch|topics|subscriptions}")
	fmt.Fprintln(w, "                                      agent-to-agent message ops (env-primary; HARNESS_AUTH_TICKET required)")
	fmt.Fprintln(w, "  file push [-r|--recursive] [-f|--force] [-p|--parents] TASK_ID LOCAL_SRC WORKTREE_REL_DST")
	fmt.Fprintln(w, "                                      copy a local file (or directory tree with -r) into the worktree")
	fmt.Fprintln(w, "                                      default: O_EXCL refuses to overwrite; -f permits replacement")
	fmt.Fprintln(w, "  file mkdir [-p|--parents] TASK_ID WORKTREE_REL_DIR")
	fmt.Fprintln(w, "                                      create a directory in the worktree")
	fmt.Fprintln(w, "  file pull [-r|--recursive] [-f|--force] TASK_ID WORKTREE_REL_SRC LOCAL_DST")
	fmt.Fprintln(w, "                                      copy a worktree file (or directory tree with -r) to a local path")
	fmt.Fprintln(w, "                                      default: O_EXCL refuses to overwrite local; -f permits replacement")
	fmt.Fprintln(w, "  file ls   TASK_ID [WORKTREE_REL_DIR]")
	fmt.Fprintln(w, "                                      list a single directory under the worktree (default: worktree root)")
	fmt.Fprintln(w, "  file edit TASK_ID WORKTREE_REL_PATH  open the file in $EDITOR and write it back")
	fmt.Fprintln(w, "  file new  TASK_ID WORKTREE_REL_PATH  create an empty file (refused when it exists)")
	fmt.Fprintln(w, "  file delete [-r|--recursive] [-f|--force] TASK_ID WORKTREE_REL_PATH")
	fmt.Fprintln(w, "                                      remove a file; -r a directory (dir_delete), -r -f a non-empty directory (RemoveAll); without -r a directory is refused")
	fmt.Fprintln(w, "  git TASK_ID log    [--max N] [-- PATH]")
	fmt.Fprintln(w, "  git TASK_ID diff   [--staged] [BASE] [TARGET] [--max-bytes N] [-- PATH]")
	fmt.Fprintln(w, "  git TASK_ID show   [REV] [-- PATH]")
	fmt.Fprintln(w, "  git TASK_ID status [-- PATH]")
	fmt.Fprintln(w, "  git TASK_ID subrepos")
	fmt.Fprintln(w, "  git TASK_ID file   [--staged | --rev REV] PATH")
	fmt.Fprintln(w, "                                      read-only git view of a task's worktree (requires file_read)")
	fmt.Fprintln(w, "                                      runs in the worktree while the task lives, and against the retained")
	fmt.Fprintln(w, "                                      harness/<task-id> branch after it ends (committed work only)")
	fmt.Fprintln(w, "                                      diff counts revisions the way git does: none = unstaged, one = that")
	fmt.Fprintln(w, "                                      revision against the working tree, two = commit against commit")
	fmt.Fprintln(w, "                                      --subrepo DIR runs any of them inside a nested repository (a plain")
	fmt.Fprintln(w, "                                      nested repo is invisible from outside it); subrepos lists them")
	fmt.Fprintln(w, "                                      --submodule on diff/show inlines a submodule's own changes")
	fmt.Fprintln(w, "  workspace save <name> [--task <32-hex>] [--resume no|continue|fresh] [--runner assigned|any] [--repo PATH]")
	fmt.Fprintln(w, "                                      record the registered forwards into .harness/config as a named workspace;")
	fmt.Fprintln(w, "                                      every task with one unless --task narrows it. MERGES: blocks it did not")
	fmt.Fprintln(w, "                                      observe are kept and an existing block's resume/runner are never reset")
	fmt.Fprintln(w, "                                      (in-process forwards — a raw TUI pane, a WebUI preview pin — have no local")
	fmt.Fprintln(w, "                                      address to write down and are skipped, with a count)")
	fmt.Fprintln(w, "  workspace rm <name>                 delete one workspace from .harness/config (other workspaces and comments kept)")
	fmt.Fprintln(w, "  workspace ls | show [<name>]        list the workspaces in .harness/config, or print one")
	fmt.Fprintln(w, "                                      the TUI applies a workspace on start, on reconnect, and on `workspace apply`,")
	fmt.Fprintln(w, "                                      and `workspace detach [--stop]` there stops it re-applying;")
	fmt.Fprintln(w, "                                      neither exists here — a forward dies with the process that holds it")
	fmt.Fprintln(w, "  forward <task-id> [-L [bind:]localport:remotehost:remoteport] [-R [bind:]runnerport:dialhost:dialport] ...")
	fmt.Fprintln(w, "                                      -L: forward a local port through the runner to remote host:port (ssh -L)")
	fmt.Fprintln(w, "                                      -R: runner listens, connections dial back to a client-side host:port (ssh -R)")
	fmt.Fprintln(w, "                                      both repeatable; Ctrl-C to stop")
	fmt.Fprintln(w, "  forward <task-id> -W host:port")
	fmt.Fprintln(w, "                                      raw stdio forward (ssh -W): no local listener, this process's stdin/stdout is the client endpoint")
	fmt.Fprintln(w, "                                    [--http-path /p [--http-method M] [--http-header 'N: v'] [--http-body B|@file|-]]")
	fmt.Fprintln(w, "                                      with -W: send one built HTTP request and stream the response (stdin is not spliced)")
	fmt.Fprintln(w, "                                      mutually exclusive with -L / -R; not repeatable; exits with its peer")
	fmt.Fprintln(w, "  forward ls [--task TASK_ID] [--json]")
	fmt.Fprintln(w, "                                      list registered port forwards; --task filters, --json emits JSON lines")
	fmt.Fprintln(w, "  forward tap FORWARD_ID [--dir to-target|from-target|both] [--max-bytes N] [--hex | --text | --raw | --json]")
	fmt.Fprintln(w, "                                      stream the bytes crossing one forward. A tap sees only what crosses AFTER it opens; nothing is recorded server-side")
	fmt.Fprintln(w, "                                      --raw writes payload bytes with no headers, so it needs an explicit --dir: two directions on one stdout is not a stream any decoder can read")
	fmt.Fprintln(w, "  forward kill FORWARD_ID [FORWARD_ID ...]")
	fmt.Fprintln(w, "                                      kill one or more registered forwards by id (from `forward ls`)")
	fmt.Fprintln(w, "  exec [--shell] [--sshd-parent] <task-id> -- <command> [args...]")
	fmt.Fprintln(w, "                                      run a command in the task's WORKTREE as its own process:")
	fmt.Fprintln(w, "                                      stdout and stderr stay separate, and the command's own exit code becomes ours")
	fmt.Fprintln(w, "                                      works on a FINISHED task too, as long as its worktree is still there —")
	fmt.Fprintln(w, "                                      a task that ended with uncommitted work keeps one")
	fmt.Fprintln(w, "                                      dies with this process; for something to leave running, submit a task instead")
	fmt.Fprintln(w, "                                      NOT `session exec`, which types into the session's foreground shell")
	fmt.Fprintln(w, "                                      --shell: hand it to the RUNNER's shell as one line (sh -c / cmd /c by its platform)")
	fmt.Fprintln(w, "  exec ls [--task TASK_ID] [--json]   list running execs; --task filters, --json emits JSON lines")
	fmt.Fprintln(w, "  exec kill EXEC_ID [EXEC_ID ...]     stop one or more running execs by id (from `exec ls`)")
	fmt.Fprintln(w, "  ssh-gateway [--listen 127.0.0.1:2222] [--host-key PATH] [--authorized-keys PATH]")
	fmt.Fprintln(w, "                                      serve ssh: `ssh -p 2222 <32-hex-task-id>@127.0.0.1` attaches to that session,")
	fmt.Fprintln(w, "                                      so ssh config aliases, tmux and mosh reach a task with no harness binary there")
	fmt.Fprintln(w, "                                      the user name picks the mode: bare = cowrite (evicts nobody), .control takes")
	fmt.Fprintln(w, "                                      the seat and owns the PTY size, .view watches")
	fmt.Fprintln(w, "                                      Ctrl+] detaches. ssh's own ~. DISCONNECTS instead: the session survives either")
	fmt.Fprintln(w, "                                      way, but a disconnect leaves your terminal's modes unreset (`reset` fixes it)")
	fmt.Fprintln(w, "                                      no ssh auth on a loopback bind; --authorized-keys is REQUIRED off loopback")
	fmt.Fprintln(w, "                                      ssh -L / -W tunnel through it: the RUNNER dials the target, and each")
	fmt.Fprintln(w, "                                      forwarded connection is an ordinary `forward ls` row while it lasts")
	fmt.Fprintln(w, "                                      no scp/sftp and no ssh -R: use `file push`/`file pull` and `forward -R`")
	fmt.Fprintln(w, "                                      foreground; Ctrl-C stops it and every session it serves")
}

func serverUsage() { serverUsageTo(os.Stderr) }

func serverUsageTo(w io.Writer) {
	fmt.Fprintln(w, "usage: harness-cli server <subcommand> [flags]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Subcommands:")
	fmt.Fprintln(w, "  dial-runner [--via RUNNER_CID] RUNNER_CID")
	fmt.Fprintln(w, "                                      ask the server to reverse-dial RUNNER_CID (Phase A/B)")
	fmt.Fprintln(w, "                                      --via relays through an already-connected runner (Phase B)")
	fmt.Fprintln(w, "                                      (runner must be running in --listen / --udp-listen mode)")
	fmt.Fprintln(w, "                                      prints the DialRunnerStatus and exits non-zero on non-Ok")
}

func boardUsage() { boardUsageTo(os.Stderr) }

func boardUsageTo(w io.Writer) {
	fmt.Fprintln(w, "usage: harness-cli board <subcommand> [flags]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Subcommands:")
	fmt.Fprintln(w, "  topics                              list every topic on the board with metadata (cap: info_global)")
	fmt.Fprintln(w, "  read <topic> [--in-reply-to N] [--json]  print retained messages for <topic> (text: header + pretty payload; --json: JSON Lines, same record shape as agent inbox --json; not found = exit 0)")
	fmt.Fprintln(w, "  subscribers [topic]                 list each task's subscriptions; with <topic>, only the tasks a publish there reaches (cap: info_global)")
	fmt.Fprintln(w, "  retract <topic> --seq N             withdraw one message: gone from every agent path, still readable here")
	fmt.Fprintln(w, "                                      until the topic ages out. --seq is required — there is no whole-topic")
	fmt.Fprintln(w, "                                      retract (cap: purge)")
	fmt.Fprintln(w, "  purge <topic> [--seq N]             drop the whole topic ring (seq=0) or one message by seq. Unlike retract")
	fmt.Fprintln(w, "                                      this destroys the bytes, operator view included (cap: purge)")
}

func agentUsage() { agentUsageTo(os.Stderr) }

func agentUsageTo(w io.Writer) {
	fmt.Fprintln(w, "usage: harness-cli agent <subcommand> [flags]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Env-primary (HARNESS_*): SERVER_CID, TASK_ID, RUNNER_ID, HOSTNAME, WS_PATH, REPO_PATH")
	fmt.Fprintln(w, "HARNESS_AUTH_TICKET is env-only (no flag accepted).")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Subcommands:")
	fmt.Fprintln(w, "  send --topic T TEXT...              publish a message. The body is the trailing words, or")
	fmt.Fprintln(w, "                                       --data STRING, or --data - to read it from stdin. \"-\" is a")
	fmt.Fprintln(w, "                                       VALUE OF --data, never a positional: `send --topic T -`")
	fmt.Fprintln(w, "                                       publishes the one-byte body \"-\". The ok line reports bytes")
	fmt.Fprintln(w, "                                       and source, so a body that went out wrong says so at once")
	fmt.Fprintln(w, "  send --in-reply-to SEQ TEXT...      reply to SEQ; --topic optional (server routes it where that message asked)")
	fmt.Fprintln(w, "  send --reply-to R ...               route replies to THIS message to R instead of your own")
	fmt.Fprintln(w, "                                       chat.<short-id>; the peer answers with --in-reply-to alone")
	fmt.Fprintln(w, "  wait --topic T [--since N] [--in-reply-to SEQ] [--timeout DUR]")
	fmt.Fprintln(w, "                                       take everything after --since, blocking only if there is nothing;")
	fmt.Fprintln(w, "                                       omitting --since means cursor 0, so a non-empty ring returns AT ONCE")
	fmt.Fprintln(w, "                                       with old messages (scripting; NOT from an agent turn)")
	fmt.Fprintln(w, "  inbox [--since N] [--in-reply-to SEQ]  idempotent dump of subscribed topics; --since 0 (default) = whole ring")
	fmt.Fprintln(w, "  read SEQ                            fetch one retained message, whole; the hooks name it when they")
	fmt.Fprintln(w, "                                       decline to inline a large body. Limited to subscribed topics")
	fmt.Fprintln(w, "  subscribe --topic T                  register a subscription")
	fmt.Fprintln(w, "  unsubscribe --topic T                remove a subscription")
	fmt.Fprintln(w, "  dispatch --topic T TEXT... [--reply-to R] [--timeout DUR]")
	fmt.Fprintln(w, "                                       send, then block for the reply to THAT message. --reply-to R")
	fmt.Fprintln(w, "                                       declares R as the destination AND waits there; default is your")
	fmt.Fprintln(w, "                                       own chat.<short-id>. --timeout bounds the WHOLE call, publish")
	fmt.Fprintln(w, "                                       ack included (scripting; NOT from an agent turn)")
	fmt.Fprintln(w, "  topics                              list every topic on the board (JSON Lines) (cap: info_global)")
	fmt.Fprintln(w, "  subscriptions                       list this agent's registered patterns (JSON Lines)")
	fmt.Fprintln(w, "  retained --topic T | --self          list a topic's retained ring as metadata only, no payload (no cap)")
	fmt.Fprintln(w, "  purge --topic T | --self [--seq N]   drop a topic's retained buffer, or one message by seq (cap: purge)")
	fmt.Fprintln(w, "  retract SEQ                         withdraw a message YOU sent: gone from every agent path, still")
	fmt.Fprintln(w, "                                       visible to the operator as retracted (no cap; authorship-checked)")
	fmt.Fprintln(w, "                                       a reply to a message addressed to you retracts it automatically;")
	fmt.Fprintln(w, "                                       send --no-retire-on-reply to keep one alive past its answer")
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

// tapModeByName maps the declaration's mode word onto the renderer. The
// mutual exclusion between the four is enforced in the verb's Build, so by the
// time this runs exactly one was chosen.
func tapModeByName(name string) cli.TapRenderMode {
	switch name {
	case "text":
		return cli.TapText
	case "raw":
		return cli.TapRaw
	case "json":
		return cli.TapJSON
	default:
		return cli.TapHex
	}
}

// parseSpawn parses one of the three spawn verbs from the declaration and
// resolves the flags that have a fallback ladder.
//
// The ladder is flag > env > workspace, and it is applied HERE rather than in
// cli/verb because that package parses and does not read the environment or
// the config file.
func parseSpawn(kind string, args []string, _ func() objproto.ConnectionID) verb.SpawnAction {
	path := []string{kind}
	if kind == "session-new" {
		path = []string{"session", "new"}
	}
	sp, ok := verb.Lookup(path...)
	if !ok {
		die(fmt.Errorf("%s: not in the verb table", kind))
	}
	sp = sp.For(verb.CLI)
	fs := sp.NewFlagSet(flag.ExitOnError)
	b, perr := sp.Parse(fs, args)
	if perr != nil {
		die(perr)
	}
	act, berr := sp.BuildFunc()(b)
	if berr != nil {
		fmt.Fprintln(os.Stderr, berr)
		os.Exit(2)
	}
	a := act.(verb.SpawnAction)
	a.Repo = sp.Resolve(b, "repo", os.Getenv, func(string) string { return workspaceRepo }, nil)
	if a.Repo == "" && a.ResumeTaskID == "" {
		fmt.Fprintf(os.Stderr, "%s: --repo or HARNESS_REPO_PATH required (must match a runner's RepoPath verbatim) — except when --resume is set, which uses the existing task's repo\n", kind)
		os.Exit(2)
	}
	return a
}

// spawnOpts turns the shared action into the client's option bag.
//
// --caps / --scope / --scope-for are already parsed and merged by the verb's
// Build, which is where that grammar lives; the pointers carry "the operator
// said nothing" separately from "the operator said none", because both zero
// values are meaningful.
func spawnOpts(a verb.SpawnAction) cli.SessionOpts {
	var caps protocol.Capability
	if a.Caps != nil {
		caps = *a.Caps
	}
	var scope protocol.TaskScope
	if a.Scope != nil {
		scope = *a.Scope
	}
	sel, err := cli.BuildSelector(cli.SelectorOpts{Runner: a.Runner, Host: a.Host, IP: a.IP})
	if err != nil {
		die(err)
	}
	return cli.SessionOpts{
		Selector: sel, ExtraArgs: a.ExtraArgs, ResumeTaskID: a.ResumeTaskID,
		Caps: caps, Scope: scope, Overrides: a.Overrides,
		ResumeCapsOverride: a.ResumeTaskID != "" && a.CapsPresent,
		ScopePresent:       a.ResumeTaskID != "" && a.ScopePresent,
		ResumeConversation: a.ResumeConversation, AgentProfile: a.Agent,
	}
}
