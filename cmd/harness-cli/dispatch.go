//go:build !js

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"strings"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/agent"
	"github.com/on-keyday/agent-harness/cli/sshgw"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/agentskills"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// cliVerbs implements verb.CLIDispatch: one method per verb the declaration
// gives this surface.
//
// It replaces main()'s 29-case switch over the first word plus, inside eight
// of those cases, a hand-written walk over the second and third. That shape is
// why `harness-cli` could carry a verb the table did not know (three of them
// did) and why the table could carry one the CLI never reached: nothing
// compared the two lists, and the completeness TESTS that did compare them
// only saw what they were told to look at.
//
// Go has no exhaustive switch. A type that misses a method does not build,
// which is what turns coverage from a test into a build property.
//
// What stays per-surface is the BODIES (D3): these return an error, print to
// stdout, and reach cli.Client directly, none of which the TUI does.
type cliVerbs struct {
	ctx context.Context
	// cid resolves the server ConnectionID, dying on a bad one. A function
	// rather than a value because resolution reads flags, env and the
	// workspace file, and a verb that never dials (version, caps, skill,
	// workspace) must not pay for it or fail on it.
	cid func() objproto.ConnectionID
}

var _ verb.CLIDispatch[error] = cliVerbs{}

// dial opens the one connection a verb's body needs. Callers close it.
func (h cliVerbs) dial() (*cli.Client, error) {
	return cli.Dial(h.ctx, h.cid(), protocol.ClientKind_Cli)
}

// withClient runs fn against a dialed client, closing it afterwards. Most
// bodies are exactly this, and the four that were written out by hand each
// spelled the deferred Close a little differently.
func (h cliVerbs) withClient(fn func(c *cli.Client) error) error {
	c, err := h.dial()
	if err != nil {
		return err
	}
	defer c.Close()
	return fn(c)
}

// --- file ---------------------------------------------------------------

func (h cliVerbs) FilePush(a verb.FilePushAction) error {
	return h.withClient(func(c *cli.Client) error {
		opts := cli.FilePushOpts{Force: a.Force, MkdirParents: a.Parents}
		if a.Recursive {
			return c.FilePushDir(h.ctx, a.TaskID, a.LocalSrc, a.RemoteDst, opts)
		}
		return c.FilePush(h.ctx, a.TaskID, a.LocalSrc, a.RemoteDst, opts)
	})
}

func (h cliVerbs) FilePull(a verb.FilePullAction) error {
	return h.withClient(func(c *cli.Client) error {
		if a.Recursive {
			// The --offset/--length combination is refused in Build, which
			// every surface goes through.
			return c.FilePullDir(h.ctx, a.TaskID, a.RemoteSrc, a.LocalDst, a.Force)
		}
		return c.FilePull(h.ctx, a.TaskID, a.RemoteSrc, a.LocalDst,
			cli.FileTransferRange{Offset: a.Offset, Length: a.Length}, a.Force)
	})
}

func (h cliVerbs) FileLs(a verb.FileLsAction) error {
	return h.withClient(func(c *cli.Client) error {
		return c.FileLs(h.ctx, a.TaskID, a.RelPath, os.Stdout)
	})
}

func (h cliVerbs) FileMkdir(a verb.FileMkdirAction) error {
	return h.withClient(func(c *cli.Client) error {
		return c.FileMkdir(h.ctx, a.TaskID, a.RelPath, a.Parents)
	})
}

func (h cliVerbs) FileDelete(a verb.FileDeleteAction) error {
	return h.withClient(func(c *cli.Client) error {
		if a.Recursive {
			return c.FileDeleteDir(h.ctx, a.TaskID, a.RelPath, a.Force)
		}
		return c.FileDelete(h.ctx, a.TaskID, a.RelPath)
	})
}

func (h cliVerbs) FileEdit(a verb.FileEditAction) error {
	return h.withClient(func(c *cli.Client) error {
		return runFileEdit(h.ctx, c, a.TaskID, a.RelPath)
	})
}

func (h cliVerbs) FileNew(a verb.FileNewAction) error {
	return h.withClient(func(c *cli.Client) error {
		return runFileNew(h.ctx, c, a.TaskID, a.RelPath)
	})
}

// --- the top-level verbs ------------------------------------------------

// requireRepo refuses a spawn whose repo the ladder could not fill.
//
// It cannot be a declared rule: the ladder's env and workspace tiers are
// applied AFTER Validate runs, so a table-side "required" would refuse a line
// that HARNESS_REPO_PATH was about to answer. It cannot be per-verb either --
// it was, inside parseSpawn, and moving the three spawn verbs onto the
// generated parser dropped it, so `submit hello` with no repo dialed the
// server and sent an empty one instead of exiting 2 without connecting.
//
// One implementation; the three call sites are the three methods the
// interface guarantees exist.
func requireRepo(name string, a verb.SpawnAction) error {
	if a.Repo != "" || a.ResumeTaskID != "" {
		return nil
	}
	return fmt.Errorf("%s: --repo or HARNESS_REPO_PATH required (must match a runner's RepoPath verbatim) — "+
		"except when --resume is set, which uses the existing task's repo", name)
}

func (h cliVerbs) Submit(a verb.SpawnAction) error {
	if err := requireRepo("submit", a); err != nil {
		return err
	}
	return h.withClient(func(c *cli.Client) error {
		id, err := c.Submit(h.ctx, a.Repo, a.Task, spawnOpts(a))
		if err != nil {
			return err
		}
		fmt.Println(id)
		return nil
	})
}

func (h cliVerbs) Interactive(a verb.SpawnAction) error {
	if err := requireRepo("interactive", a); err != nil {
		return err
	}
	return h.withClient(func(c *cli.Client) error {
		// The session survives a client disconnect (tmux-like) and any
		// operator client can take it over via reattach.
		_, err := c.Interactive(h.ctx, a.Repo, spawnOpts(a))
		return err
	})
}

func (h cliVerbs) Ls(a verb.ListAction) error {
	switch {
	case a.Tree:
		return cli.ListTree(h.ctx, h.cid(), os.Stdout)
	case a.JSON:
		return cli.ListJSON(h.ctx, h.cid(), os.Stdout)
	default:
		return cli.List(h.ctx, h.cid(), os.Stdout)
	}
}

func (h cliVerbs) Conns(a verb.ConnsAction) error {
	if a.Follow {
		var err error
		if a.JSON {
			err = cli.WatchConnsJSON(h.ctx, h.cid(), os.Stdout)
		} else {
			err = cli.WatchConns(h.ctx, h.cid(), os.Stdout)
		}
		if err != nil && err != context.Canceled {
			return err
		}
		return nil
	}
	conns, err := cli.ConnList(h.ctx, h.cid())
	if err != nil {
		return err
	}
	if a.JSON {
		for i := range conns {
			fmt.Fprintln(os.Stdout, cli.ConnInfoJSONLine(&conns[i]))
		}
		return nil
	}
	for _, line := range cli.ConnInfoLines(conns) {
		fmt.Fprintln(os.Stdout, line)
	}
	return nil
}

func (h cliVerbs) Cancel(a verb.CancelAction) error {
	return cli.Cancel(h.ctx, h.cid(), a.TaskID)
}

func (h cliVerbs) Notify(a verb.NotifyAction) error {
	return cli.Notify(h.ctx, h.cid(), a.Level, a.Title, a.Text)
}

func (h cliVerbs) Logs(a verb.LogsAction) error {
	return cli.Logs(h.ctx, h.cid(), a.TaskID, os.Stdout, a.Follow)
}

func (h cliVerbs) Watch(a verb.CatalogAction) error {
	return cli.Watch(h.ctx, h.cid(), os.Stdout)
}

func (h cliVerbs) NotifyWatch(a verb.CatalogAction) error {
	return cli.WatchNotificationsText(h.ctx, h.cid(), os.Stdout)
}

func (h cliVerbs) Prune(a verb.PruneAction) error {
	return cli.Prune(h.ctx, h.cid(), a.Before, a.TaskIDs, a.Force, os.Stdout)
}

func (h cliVerbs) Restore(a verb.RestoreAction) error {
	// --list and the bare form are the same request: listing is what the verb
	// does when it is not given ids to act on.
	ids := a.TaskIDs
	if a.List {
		ids = nil
	}
	return cli.Restore(h.ctx, h.cid(), ids, os.Stdout)
}

// --- the catalogs: no connection, so no cid() ---------------------------

func (h cliVerbs) Caps(a verb.CatalogAction) error {
	return cli.WriteCaps(os.Stdout, a.JSON)
}

func (h cliVerbs) Whoami(a verb.CatalogAction) error {
	resp, err := cli.WhoAmI(h.ctx, h.cid())
	if err != nil {
		return err
	}
	return cli.WriteWhoAmI(os.Stdout, resp, a.JSON)
}

func (h cliVerbs) Version(a verb.CatalogAction) error {
	return writeVersion(os.Stdout, a.JSON)
}

// SkillLs answers `skill ls`, the positional spelling of --list.
func (h cliVerbs) SkillLs(a verb.CatalogAction) error {
	a.List = true
	return h.Skill(a)
}

func (h cliVerbs) Skill(a verb.CatalogAction) error {
	if a.List {
		names, err := agentskills.List()
		if err != nil {
			return fmt.Errorf("skill list: %w", err)
		}
		for _, n := range names {
			fmt.Println(n)
			if d, derr := agentskills.Description(n); derr == nil && d != "" {
				fmt.Printf("    %s\n", d)
			}
		}
		return nil
	}
	name := a.Name
	if name == "" {
		name = "harness-cli"
	}
	md, err := agentskills.Skill(name)
	if err != nil {
		avail, lerr := agentskills.List()
		if lerr == nil {
			return fmt.Errorf("skill %q: %w (available: %s)", name, err, strings.Join(avail, ", "))
		}
		return fmt.Errorf("skill %q: %w", name, err)
	}
	os.Stdout.Write(md)
	return nil
}

// --- git ----------------------------------------------------------------
//
// One dial and one renderer for all six, which is what runGit's tail was.
// The task id is not in the action: `git log <task-id>` puts it in the MIDDLE
// of the path, so main peels it off and writes it in before dispatching.

func (h cliVerbs) gitQuery(a verb.GitAction, q func(*cli.Client, cli.GitQuery) (*cli.GitResult, error)) error {
	return h.withClient(func(c *cli.Client) error {
		res, err := q(c, cli.GitQuery{
			BaseRev: a.BaseRev, TargetRev: a.TargetRev, Path: a.Path, Subrepo: a.Subrepo,
			MaxCommits: a.Max, MaxBytes: a.MaxBytes, SubmoduleDiff: a.Submodule,
		})
		if err != nil {
			return err
		}
		if err := res.Err(); err != nil {
			return err
		}
		renderGitResult(os.Stdout, res, isTTY(os.Stdout))
		return nil
	})
}

func (h cliVerbs) GitLog(a verb.GitAction) error {
	return h.gitQuery(a, func(c *cli.Client, q cli.GitQuery) (*cli.GitResult, error) {
		return c.GitLog(h.ctx, a.TaskID, q)
	})
}

func (h cliVerbs) GitDiff(a verb.GitAction) error {
	return h.gitQuery(a, func(c *cli.Client, q cli.GitQuery) (*cli.GitResult, error) {
		if a.Staged {
			q.Target = protocol.GitDiffTarget_Index
		} else if a.TargetRev != "" {
			q.Target = protocol.GitDiffTarget_Rev
		}
		return c.GitDiff(h.ctx, a.TaskID, q)
	})
}

func (h cliVerbs) GitShow(a verb.GitAction) error {
	return h.gitQuery(a, func(c *cli.Client, q cli.GitQuery) (*cli.GitResult, error) {
		return c.GitShow(h.ctx, a.TaskID, q)
	})
}

func (h cliVerbs) GitStatus(a verb.GitAction) error {
	return h.gitQuery(a, func(c *cli.Client, q cli.GitQuery) (*cli.GitResult, error) {
		return c.GitStatus(h.ctx, a.TaskID, q)
	})
}

func (h cliVerbs) GitSubrepos(a verb.GitAction) error {
	return h.gitQuery(a, func(c *cli.Client, q cli.GitQuery) (*cli.GitResult, error) {
		return c.GitSubrepos(h.ctx, a.TaskID, q)
	})
}

func (h cliVerbs) GitFile(a verb.GitAction) error {
	return h.gitQuery(a, func(c *cli.Client, q cli.GitQuery) (*cli.GitResult, error) {
		if a.Staged {
			q.Target = protocol.GitDiffTarget_Index
		} else if a.TargetRev != "" {
			// `git file --rev X` reads the copy AT X, so the revision is the
			// query's base and the target names which side to read.
			q.Target = protocol.GitDiffTarget_Rev
			q.BaseRev = a.TargetRev
			q.TargetRev = ""
		}
		return c.GitFile(h.ctx, a.TaskID, q)
	})
}

// --- exec ---------------------------------------------------------------

func (h cliVerbs) Exec(a verb.ExecRunAction) error { return runExecAction(h.ctx, h.cid(), a) }

func (h cliVerbs) ExecLs(a verb.ExecRunAction) error {
	execs, err := cli.ExecRunList(h.ctx, h.cid(), a.TaskFilter)
	if err != nil {
		return err
	}
	if a.JSON {
		for i := range execs {
			fmt.Println(cli.ExecRunInfoJSONLine(&execs[i]))
		}
		return nil
	}
	for _, line := range cli.ExecRunInfoLines(execs) {
		fmt.Println(line)
	}
	return nil
}

func (h cliVerbs) ExecKill(a verb.ExecRunAction) error {
	if len(a.ExecIDs) == 0 {
		return fmt.Errorf("usage: harness-cli exec kill <exec-id> [<exec-id> ...]")
	}
	// Every id, even after one fails. Returning on the first error made
	// `exec kill 1 2 3` with a stale first id stop before touching 2 and 3 --
	// while the TUI, whose comment says "as on the CLI", killed them. One
	// declared verb, two meanings, decided by which id went away first.
	var failed error
	for _, id := range a.ExecIDs {
		if err := cli.ExecRunKill(h.ctx, h.cid(), id); err != nil {
			fmt.Fprintf(os.Stderr, "exec kill %d: %v\n", id, err)
			failed = err
			continue
		}
		fmt.Printf("killed exec %d\n", id)
	}
	return failed
}

// --- forward ------------------------------------------------------------

func (h cliVerbs) ForwardLs(a verb.ForwardLsAction) error {
	forwards, err := cli.PortForwardList(h.ctx, h.cid(), a.TaskFilter)
	if err != nil {
		return err
	}
	if a.JSON {
		for i := range forwards {
			fmt.Println(cli.PortForwardInfoJSONLine(&forwards[i]))
		}
		return nil
	}
	for _, line := range cli.PortForwardInfoLines(forwards) {
		fmt.Println(line)
	}
	return nil
}

func (h cliVerbs) ForwardKill(a verb.ForwardKillAction) error {
	// Every id, even after one fails -- see ExecKill.
	var failed error
	for _, id := range a.ForwardIDs {
		if err := cli.KillPortForward(h.ctx, h.cid(), id); err != nil {
			fmt.Fprintf(os.Stderr, "forward kill %d: %v\n", id, err)
			failed = err
			continue
		}
		fmt.Printf("killed forward %d\n", id)
	}
	return failed
}

func (h cliVerbs) ForwardTap(a verb.ForwardTapAction) error {
	filter, ferr := cli.ParseTapFilter(a.Dir)
	if ferr != nil {
		return ferr
	}
	tctx, cancel := interruptContext("forward tap", h.ctx)
	defer cancel()
	return cli.RunForwardTapDial(tctx, h.cid(), a.ForwardID, cli.ForwardTapOpts{
		Filter: filter, MaxRecordBytes: a.MaxRecordBytes, Mode: tapModeByName(a.Mode),
	}, os.Stdout)
}

// --- server / board / agent --------------------------------------------

func (h cliVerbs) ServerDialRunner(a verb.ServerDialRunnerAction) error {
	targetCID, err := objproto.ParseConnectionID(a.RunnerCID,
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		return fmt.Errorf("parse runner-cid: %w", err)
	}
	var viaCID objproto.ConnectionID
	if v := strings.TrimSpace(a.Via); v != "" {
		viaCID, err = objproto.ParseConnectionID(v,
			objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
		if err != nil {
			return fmt.Errorf("parse --via: %w", err)
		}
	}
	resp, err := cli.ServerDialRunner(h.ctx, h.cid(), targetCID, viaCID)
	if err != nil {
		return err
	}
	fmt.Println(resp.Status.String())
	if resp.Status != protocol.DialRunnerStatus_Ok {
		os.Exit(1)
	}
	return nil
}

// The five board verbs share one body: cli.RunBoardAction switches on the Sub
// the declaration fixed. Five methods rather than one because the interface is
// per-VERB -- that is what makes a new board verb a build error here.
func (h cliVerbs) BoardTopics(a verb.BoardAction) error      { return h.board(a) }
func (h cliVerbs) BoardRead(a verb.BoardAction) error        { return h.board(a) }
func (h cliVerbs) BoardSubscribers(a verb.BoardAction) error { return h.board(a) }
func (h cliVerbs) BoardRetract(a verb.BoardAction) error     { return h.board(a) }
func (h cliVerbs) BoardPurge(a verb.BoardAction) error       { return h.board(a) }

func (h cliVerbs) board(a verb.BoardAction) error {
	return cli.RunBoardAction(h.ctx, h.cid(), a, os.Stdout)
}

// The agent verbs run inside a task's Bash tool and read HARNESS_* env, so
// they resolve their own server CID and never call h.cid().
func (h cliVerbs) AgentSend(a verb.AgentSendAction) error {
	return agent.SendWith(h.ctx, a, os.Stdin, os.Stdout)
}
func (h cliVerbs) AgentDispatch(a verb.AgentSendAction) error {
	return agent.DispatchWith(h.ctx, a, os.Stdin, os.Stdout)
}
func (h cliVerbs) AgentInbox(a verb.AgentAction) error {
	return agent.InboxWith(h.ctx, a, os.Stdout)
}
func (h cliVerbs) AgentWait(a verb.AgentAction) error {
	return agent.WaitWith(h.ctx, a, os.Stdout)
}
func (h cliVerbs) AgentSubscribe(a verb.AgentAction) error {
	return agent.SubscribeWith(h.ctx, a, os.Stdout)
}
func (h cliVerbs) AgentUnsubscribe(a verb.AgentAction) error {
	return agent.UnsubscribeWith(h.ctx, a, os.Stdout)
}
func (h cliVerbs) AgentTopics(a verb.AgentAction) error {
	return agent.TopicsWith(h.ctx, a, os.Stdout)
}
func (h cliVerbs) AgentSubscriptions(a verb.AgentAction) error {
	return agent.SubscriptionsWith(h.ctx, a, os.Stdout)
}
func (h cliVerbs) AgentRetained(a verb.AgentAction) error {
	return agent.RetainedWith(h.ctx, a, os.Stdout)
}
func (h cliVerbs) AgentPurge(a verb.AgentAction) error {
	return agent.PurgeWith(h.ctx, a, os.Stdout)
}
func (h cliVerbs) AgentRead(a verb.AgentAction) error {
	return agent.ReadWith(h.ctx, a, os.Stdout)
}
func (h cliVerbs) AgentRetract(a verb.AgentAction) error {
	return agent.RetractWith(h.ctx, a, os.Stdout)
}

// --- caps set / set-parent ---------------------------------------------

func (h cliVerbs) CapsSet(a verb.SetCapsAction) error {
	opts := cli.SetCapsOpts{TaskID: a.TaskID, Cascade: a.Cascade, KeepConns: a.KeepConns}
	if a.Caps != nil {
		opts.Caps = cli.CapsPtr(*a.Caps)
	}
	if a.Scope != nil {
		opts.Scope = a.Scope
		opts.Overrides = a.Overrides
	}
	res, err := cli.SetCaps(h.ctx, h.cid(), opts)
	if err != nil {
		return err
	}
	for _, id := range res.Affected {
		fmt.Println(id)
	}
	fmt.Fprintf(os.Stderr, "changed %d task(s); closed %d connection(s)\n",
		len(res.Affected), res.ConnsClosed)
	return nil
}

func (h cliVerbs) CapsSetParent(a verb.SetParentAction) error {
	// An empty ParentID IS the detach request on the wire, which is why
	// `--parent ""` had to be refused at the flag rather than sorted out here:
	// the presence rule has already decided which of the three was picked, so
	// no condition on None or Swap can tell them apart.
	opts := cli.SetParentOpts{TaskID: a.TaskID, Swap: a.Swap, ParentID: a.ParentID}
	_ = a.None
	res, err := cli.SetParent(h.ctx, h.cid(), opts)
	if err != nil {
		return err
	}
	fmt.Println(cli.SetParentMessage(opts, res))
	return nil
}

// --- session ------------------------------------------------------------

func (h cliVerbs) SessionNew(a verb.SpawnAction) error {
	if err := requireRepo("session new", a); err != nil {
		return err
	}
	return runSessionNewWith(h.cid(), a)
}
func (h cliVerbs) SessionAttach(a verb.SessionAction) error {
	return runSessionAttachWith(h.cid(), a)
}
func (h cliVerbs) SessionSnapshot(a verb.SessionAction) error {
	return runSessionSnapshotWith(h.cid(), a)
}
func (h cliVerbs) SessionSend(a verb.SendAction) error { return runSessionSendWith(h.cid(), a) }
func (h cliVerbs) SessionExec(a verb.SessionExecAction) error {
	return runSessionExecWith(h.cid(), a)
}
func (h cliVerbs) SessionLs(a verb.SessionAction) error {
	return cli.SessionListJSON(h.ctx, h.cid(), os.Stdout)
}
func (h cliVerbs) SessionKill(a verb.SessionAction) error {
	return h.withClient(func(c *cli.Client) error { return c.Cancel(h.ctx, a.TaskID) })
}
func (h cliVerbs) SessionAwaitIdle(a verb.SessionAction) error {
	return runSessionAwaitIdleWith(h.cid(), a)
}
func (h cliVerbs) SessionResize(a verb.SessionAction) error {
	return runSessionResizeWith(h.cid(), a)
}
func (h cliVerbs) SessionStreamAttach(a verb.SessionAction) error {
	return runSessionStreamAttachWith(h.cid(), a)
}
func (h cliVerbs) SessionStreamTurn(a verb.SessionAction) error {
	return runSessionStreamTurnWith(h.cid(), a)
}
func (h cliVerbs) SessionStreamApprove(a verb.SessionAction) error {
	return runSessionStreamApproveWith(h.cid(), a)
}
func (h cliVerbs) SessionStreamInterrupt(a verb.SessionAction) error {
	return runSessionStreamSimpleWith(h.cid(), a)
}
func (h cliVerbs) SessionStreamFinish(a verb.SessionAction) error {
	return runSessionStreamSimpleWith(h.cid(), a)
}

// Forward is the OPEN form -- `forward <task-id> -L ... -R ... -W ...`. It
// runs until the forwards it started have all stopped, which is why it does
// not use withClient: the close has to happen after that wait, not on the way
// out of a closure.
func (h cliVerbs) Forward(a verb.ForwardOpenAction) error {
	var wHost string
	var wPort int
	if a.W != "" {
		hst, prt, werr := cli.ParseStdioForwardSpec(a.W)
		if werr != nil {
			return werr
		}
		wHost, wPort = hst, prt
	}
	parsed := make([]cli.ForwardSpec, 0, len(a.L))
	for _, spec := range a.L {
		p, err := cli.ParseForwardSpec(spec)
		if err != nil {
			return err
		}
		parsed = append(parsed, p)
	}
	parsedR := make([]cli.RemoteForwardSpec, 0, len(a.R))
	for _, spec := range a.R {
		p, err := cli.ParseRemoteForwardSpec(spec)
		if err != nil {
			return err
		}
		parsedR = append(parsedR, p)
	}
	c, err := h.dial()
	if err != nil {
		return err
	}
	defer c.Close()
	fctx, cancel := interruptContext("forward", h.ctx)
	defer cancel()
	logf := func(s string) { fmt.Fprintln(os.Stderr, s) }
	if a.W != "" {
		// stdout is the forward's payload channel, so status lines must go to
		// stderr (logf already does) and nothing may print to stdout.
		if a.HTTPPath != "" {
			body, berr := readFlagBody(a.HTTPBody)
			if berr != nil {
				return berr
			}
			return cli.RunHTTPRequestForward(fctx, c, a.TaskID, wHost, wPort, cli.HTTPRequestSpec{
				Method: a.HTTPMethod, Path: a.HTTPPath, Headers: a.HTTPHeaders, Body: body,
			}, os.Stdout, logf)
		}
		return cli.RunStdioForward(fctx, c, a.TaskID, wHost, wPort, logf)
	}
	// Both RunForward and RunRemoteForward return as soon as every forward
	// they started has stopped -- killed remotely, not just on Ctrl-C -- so
	// this must wait on that completion signal, not fctx.Done() alone: a -R
	// forward that outlives the -L side (or is the only side) must still let
	// the terminal return to its prompt once IT is killed, without requiring
	// Ctrl-C. rDone is closed once the -R goroutine (if any) has returned.
	var rDone chan struct{}
	if len(parsedR) > 0 {
		rDone = make(chan struct{})
		go func() {
			defer close(rDone)
			if err := cli.RunRemoteForward(fctx, c, a.TaskID, parsedR, logf); err != nil {
				logf("remote-forward: " + err.Error())
				cancel()
			}
		}()
	}
	var forwardErr error
	if len(parsed) > 0 {
		if err := cli.RunForward(fctx, c, a.TaskID, parsed, logf, nil); err != nil {
			// Not returned here: an early return would skip waiting for a live
			// -R forward below, tearing it down mid-flight with no graceful
			// signal. Same shape as the -R error path above -- log, cancel,
			// and let the wait for rDone run its course.
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
	return nil
}

// --- ssh-gateway --------------------------------------------------------

func (h cliVerbs) SshGateway(a verb.SSHGatewayAction) error {
	keyPath := a.HostKeyPath
	if keyPath == "" {
		keyPath = sshgw.DefaultHostKeyPath(workspaceCfgPath)
	}
	return h.withClient(func(c *cli.Client) error {
		fmt.Fprintf(os.Stderr, "harness-cli: ssh gateway on %s — `ssh -p %s <32-hex-task-id>[.control|.view|.sshd-parent]@%s` attaches; Ctrl-C stops it and every session it serves, and so does the server connection dropping\n",
			a.Listen, sshgw.PortOf(a.Listen), sshgw.HostOf(a.Listen))
		fmt.Fprintln(os.Stderr, "harness-cli: bare user name = cowrite (evicts nobody), .control takes the seat, .view watches; Ctrl+] detaches")
		gctx, cancel := interruptContext("ssh-gateway", h.ctx)
		defer cancel()
		return sshgw.Run(gctx, c, sshgw.Options{
			Listen:             a.Listen,
			HostKeyPath:        keyPath,
			AuthorizedKeysPath: a.AuthorizedKeys,
		})
	})
}

// --- workspace ----------------------------------------------------------

func (h cliVerbs) WorkspaceLs(a verb.WorkspaceAction) error {
	return runWorkspaceAction(h.ctx, a, h.cid)
}
func (h cliVerbs) WorkspaceRm(a verb.WorkspaceAction) error {
	return runWorkspaceAction(h.ctx, a, h.cid)
}
func (h cliVerbs) WorkspaceShow(a verb.WorkspaceAction) error {
	return runWorkspaceAction(h.ctx, a, h.cid)
}
func (h cliVerbs) WorkspaceSave(a verb.WorkspaceAction) error {
	return runWorkspaceAction(h.ctx, a, h.cid)
}

// PruneLocal removes worktrees on THIS machine. The repo comes off the action
// with its ladder already applied (env, then workspace config), which is a
// change: the ladder used to be re-implemented here keyed on the VALUE
// (`if repo == "."`), so an operator who typed `--repo .` got
// HARNESS_REPO_PATH instead -- silently, on the verb that removes worktrees.
func (h cliVerbs) PruneLocal(a verb.PruneLocalAction) error {
	abs, err := filepath.Abs(a.Repo)
	if err != nil {
		return err
	}
	if len(a.TaskIDs) == 0 {
		return cli.PruneLocal(h.ctx, abs, a.Before, nil, os.Stdout)
	}
	safe, err := classifyForLocalPrune(h.ctx, h.cid(), a.TaskIDs, a.Force, os.Stdout)
	if err != nil {
		return err
	}
	if len(safe) == 0 {
		fmt.Fprintln(os.Stdout, "prune-local: no removable task ids (use --force to override server-active state)")
		return nil
	}
	return cli.PruneLocal(h.ctx, abs, 0, safe, os.Stdout)
}

// unimplemented reports a verb the declaration gives this surface and this
// file has not carried over yet. It exists so the conversion can land family
// by family while the interface assertion still holds -- and it says which
// verb, rather than the surface silently answering nothing.
func (h cliVerbs) unimplemented(name string) error {
	return fmt.Errorf("%s: declared for the CLI and not wired here yet", name)
}
