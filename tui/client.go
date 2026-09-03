package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/runner/streamagent"
)

// --- tea.Cmd factories using the persistent cli.Client ---
//
// These wrap (*cli.Client) methods with the tea.Msg shapes the TUI's update
// loop expects (echo strings, prefix/resolved IDs for cancel, structured
// snapshots for List). The methods themselves are defined in package cli;
// we only own the result-message wrapping here.

type SubmitResultMsg struct {
	TaskID string
	Err    error
	Echo   string // human-readable echo of the request, e.g. "submit --repo /r \"prompt\""
}

type CancelResultMsg struct {
	IDPrefix string
	Resolved string
	Err      error
}

type PruneResultMsg struct {
	Removed uint32
	// IDMode is true when the prune targeted explicit task ids; only then are
	// the Skipped* counts meaningful (time mode only ever touches terminal
	// tasks, so nothing is skipped).
	IDMode         bool
	SkippedActive  uint32
	SkippedMissing uint32
	Forced         bool
	Err            error
}

// LogHistoryMsg carries the historical content of a task log fetched from the
// server's on-disk store. app.go appends it into the LogsModel when the task
// id matches the currently-followed one (a switch can happen between fetch
// and arrival).
type LogHistoryMsg struct {
	TaskID  string
	Content []byte
	Found   bool
	Err     error
	// Gen is the followTask generation this fetch was issued for. The app
	// drops a response whose Gen is not the current one, so a superseded
	// in-flight fetch cannot fold a second copy of the log into the pane.
	Gen int
}

// DoSubmit issues a Submit RPC over the existing persistent client.
// Authority is what a spawn grants the new task: the capability mask AND the
// scope bounding which tasks those capabilities may target. The two always
// travel together, so they are one value rather than two adjacent positional
// arguments — the ladder cli.SessionOpts exists to avoid.
//
// Zero value = no capabilities, subtree scope.
type Authority struct {
	Caps  protocol.Capability
	Scope protocol.TaskScope
	// Overrides narrows individual capabilities below Scope. It lives HERE
	// rather than in each Do* helper's argument list for the same reason
	// SessionOpts carries it: a field threaded through some callers and not
	// others is the failure mode these routes have already had.
	Overrides []protocol.ScopeOverride
	// ScopePresent, on a resume, re-grants Scope onto the task instead of
	// keeping its persisted one (SessionOpts.ScopePresent). Ignored on
	// create. Set only when the command line named --scope explicitly —
	// the session default must never silently rewrite a resumed task.
	ScopePresent bool
}

// sessionRequest is the half of a session or submit request that is NOT the
// authority: which runner, what to resume, how the PTY starts. It exists so
// Authority.opts can be the one place in this package that builds a
// cli.SessionOpts.
//
// Every call site names its fields. The two resume booleans are adjacent and
// interchangeable to the compiler, and the Do* helpers already pass them
// positionally through argument lists long enough to hide a swap.
type sessionRequest struct {
	Selector           protocol.RunnerSelector
	ExtraArgs          []string
	ResumeTaskID       string // hex; "" = fresh task
	ResumeCapsOverride bool
	ResumeConversation bool
	AgentProfile       string
	// InitialRows / InitialCols size a detached session's PTY at open time;
	// both must be non-zero to take effect (cli.SessionOpts.InitialRows).
	InitialRows uint16
	InitialCols uint16
	// EventStream opens the session as TaskKind_Stream (`session new --stream`):
	// structured events instead of a PTY. In the TUI this only rides the
	// detached open — the interactive handover splices a terminal, which this
	// kind does not have.
	EventStream bool
}

// opts folds an Authority and a sessionRequest into the cli.SessionOpts the
// client takes. Every session and submit path in this package goes through
// here, and TestSessionOptsIsBuiltInOnePlace fails if a second cli.SessionOpts
// literal appears in the package.
//
// It exists because the comment on cli.SessionOpts.Overrides — "a field set in
// one caller and missed in the others is the established failure mode on these
// routes" — described this package while six literals in interactive.go were
// dropping that exact field. `--scope-for` parsed, rode SessionNewAction and
// spawnAuthority, and died at the request build, so every operator surface
// reported the flag as supported while `session new` granted the bare scope.
// A warning comment is not a mechanism.
func (a Authority) opts(r sessionRequest) cli.SessionOpts {
	return cli.SessionOpts{
		Selector:           r.Selector,
		ExtraArgs:          r.ExtraArgs,
		ResumeTaskID:       r.ResumeTaskID,
		ResumeCapsOverride: r.ResumeCapsOverride,
		ResumeConversation: r.ResumeConversation,
		AgentProfile:       r.AgentProfile,
		InitialRows:        r.InitialRows,
		InitialCols:        r.InitialCols,
		EventStream:        r.EventStream,
		Caps:               a.Caps,
		Scope:              a.Scope,
		Overrides:          a.Overrides,
		ScopePresent:       a.ScopePresent,
	}
}

func DoSubmit(c *cli.Client, repo, prompt string, auth Authority) tea.Cmd {
	return DoSubmitWithOpts(c, repo, prompt, "", nil, "", auth, false, false, "")
}

// DoSubmitWithOpts issues a Submit RPC with an optional hostname pin,
// optional per-task extra claude args, and optional resume target id. When
// host is non-empty a ByHostname selector is built; otherwise Any is used.
// extraArgs are forwarded verbatim to the runner and appended to its
// --claude-args baseline at exec time. resumeTaskID, when non-empty, asks
// the server to reuse that terminal task's id and worktree branch.
// auth carries RequestedCaps and the target scope; pass
// Authority{Caps: protocol.Capability_All} for the inherit-all behaviour.
// resumeCapsOverride, when true, signals the server to re-grant caps from
// the resumer's caps rather than keeping the persisted caps of the resumed task.
// Ignored (no-op) when resumeTaskID is empty (fresh submit).
// agentProfile selects a named agent profile (e.g. "codex"); "" defers to the
// bound runner's default (or, on resume with an empty profile, the resumed
// task's own recorded profile — resolved server-side). Submit has no picker
// (§4a scope boundary): a profile no runner serves comes back as the flat
// SubmitStatus.profile_unavailable, surfaced as an error here.
func DoSubmitWithOpts(c *cli.Client, repo, prompt, host string, extraArgs []string, resumeTaskID string, auth Authority, resumeCapsOverride bool, resumeConversation bool, agentProfile string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		echo := buildSubmitEcho(repo, prompt, host, extraArgs, resumeTaskID, agentProfile)
		sel, err := cli.BuildSelector(cli.SelectorOpts{Host: host})
		if err != nil {
			return SubmitResultMsg{Err: fmt.Errorf("selector: %w", err), Echo: echo}
		}
		id, err := c.Submit(ctx, repo, prompt, auth.opts(sessionRequest{
			Selector: sel, ExtraArgs: extraArgs, ResumeTaskID: resumeTaskID,
			ResumeCapsOverride: resumeCapsOverride,
			ResumeConversation: resumeConversation, AgentProfile: agentProfile,
		}))
		if err != nil {
			return SubmitResultMsg{Err: err, Echo: echo}
		}
		return SubmitResultMsg{TaskID: id, Echo: echo}
	}
}

// buildSubmitEcho formats the human-readable echo string for the cmdline
// result panel. Annotations are added only when the corresponding option is
// set so the common case (just repo + prompt) stays readable.
func buildSubmitEcho(repo, prompt, host string, extraArgs []string, resumeTaskID, agentProfile string) string {
	annot := ""
	if host != "" {
		annot += fmt.Sprintf(" --host %q", host)
	}
	if agentProfile != "" {
		annot += fmt.Sprintf(" --agent %q", agentProfile)
	}
	if len(extraArgs) > 0 {
		annot += fmt.Sprintf(" (+%d claude-args)", len(extraArgs))
	}
	if resumeTaskID != "" {
		annot += fmt.Sprintf(" --resume %s", shortTaskID(resumeTaskID))
	}
	return fmt.Sprintf("submit --repo %q%s %q", repo, annot, prompt)
}

// shortTaskID truncates a 32-char hex task id to its first 12 for display.
// Falls back to the original string when the input is shorter than expected.
func shortTaskID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}

// DoCancel issues a Cancel RPC over the existing persistent client.
// resolved is the full hex id (callers resolve prefixes against tasksByID).
// SetCapsResultMsg carries the outcome of a live re-grant. Summary is the
// human echo of what was written ("caps=… scope=…", omitting kept fields) —
// the result line names the target and the change, because "1 task changed"
// carries no information on the usual single-target call.
type SetCapsResultMsg struct {
	TaskID      string
	Summary     string
	Affected    []string
	ConnsClosed uint32
	Err         error
}

// DoSetCaps re-grants a live task's caps and/or scope. It uses the long-lived
// client the TUI already holds (cli.SetCapsWith), not the dial-and-close
// cli.SetCaps — a fresh connection would throw away the handshake for one RPC.
func DoSetCaps(c *cli.Client, opts cli.SetCapsOpts) tea.Cmd {
	var parts []string
	if opts.Caps != nil {
		parts = append(parts, "caps="+capsLabel(*opts.Caps))
	}
	if opts.Scope != nil {
		parts = append(parts, "scope="+cli.ScopeLabel(*opts.Scope))
	}
	summary := strings.Join(parts, "  ")
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		res, err := cli.SetCapsWith(ctx, c, opts)
		return SetCapsResultMsg{
			TaskID: opts.TaskID, Summary: summary, Affected: res.Affected,
			ConnsClosed: res.ConnsClosed, Err: err,
		}
	}
}

// SetParentResultMsg carries the outcome of a parent-link change. Opts rides
// along so the result line can name the target and the form that was applied
// (cli.SetParentMessage).
type SetParentResultMsg struct {
	Opts cli.SetParentOpts
	Res  cli.SetParentResult
	Err  error
}

// DoSetParent re-points a live task's parent link (or swaps it with its
// parent). Uses the long-lived client the TUI already holds — see DoSetCaps.
func DoSetParent(c *cli.Client, opts cli.SetParentOpts) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		res, err := cli.SetParentWith(ctx, c, opts)
		return SetParentResultMsg{Opts: opts, Res: res, Err: err}
	}
}

// StreamWriteResultMsg reports one `session stream <verb>` write.
type StreamWriteResultMsg struct {
	Verb     string
	Resolved string
	Err      error
}

// DoStreamWrite performs one write on an event-stream task's data plane.
//
// It threads the TUI's long-lived client rather than dialing (the pattern every
// other Do* here follows), and each verb routes to the cli helper that builds
// its message — the TUI never assembles a streamagent.Msg itself, so it cannot
// drift from what the CLI sends.
func DoStreamWrite(c *cli.Client, v verb.SessionAction, resolved string) tea.Cmd {
	return func() tea.Msg {
		short := strings.TrimPrefix(v.Sub, "stream-")
		out := StreamWriteResultMsg{Verb: short, Resolved: resolved}
		if resolved == "" {
			out.Err = fmt.Errorf("no task matching prefix %q", v.TaskID)
			return out
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		flush := time.Duration(v.FlushMs) * time.Millisecond
		switch v.Sub {
		case "stream-turn":
			out.Err = c.StreamTurn(ctx, resolved, v.Text, flush)
		case "stream-approve":
			resp := streamagent.Response{ID: v.RequestID, Behavior: streamagent.BehaviorAllow}
			if v.Deny {
				resp.Behavior, resp.Message = streamagent.BehaviorDeny, v.Message
			}
			// A suggestion is a STANDING change, so it rides either verdict.
			if v.SuggestionSet {
				n := int(v.Suggestion)
				resp.AcceptSuggestion = &n
			}
			out.Err = c.StreamApprove(ctx, resolved, resp, flush)
		case "stream-interrupt":
			out.Err = c.StreamInterrupt(ctx, resolved, flush)
		case "stream-finish":
			out.Err = c.StreamFinish(ctx, resolved, flush)
		default:
			out.Err = fmt.Errorf("unknown stream verb %q", v.Sub)
		}
		return out
	}
}

// DoRestore puts back pruned task records from the server's WAL.
func DoRestore(c *cli.Client, ids []string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		var buf strings.Builder
		err := cli.RestoreWith(ctx, c, ids, &buf)
		return RestoreResultMsg{Text: strings.TrimSpace(buf.String()), Err: err}
	}
}

// RestoreResultMsg carries a restore's outcome to the results pane.
type RestoreResultMsg struct {
	Text string
	Err  error
}

func DoCancel(c *cli.Client, idPrefix, resolved string) tea.Cmd {
	return func() tea.Msg {
		if resolved == "" {
			return CancelResultMsg{IDPrefix: idPrefix, Err: fmt.Errorf("no task matching prefix %q", idPrefix)}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := c.Cancel(ctx, resolved)
		return CancelResultMsg{IDPrefix: idPrefix, Resolved: resolved, Err: err}
	}
}

// DoGetTaskLogGen fetches the historical log via the persistent client. The
// stream-pointer response is read off the same trsf transport the client
// already runs. gen is the followTask generation this fetch was issued for
// and is stamped onto the reply so a superseded fetch can be discarded (see
// LogHistoryMsg).
func DoGetTaskLogGen(c *cli.Client, taskID string, gen int) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		content, found, err := c.GetTaskLog(ctx, taskID)
		return LogHistoryMsg{TaskID: taskID, Content: content, Found: found, Err: err, Gen: gen}
	}
}

// DoPruneTasks asks the server to forget tasks. With taskIDs empty it runs in
// time mode (terminal tasks older than `before` are removed; force ignored).
// With taskIDs non-empty it runs in id mode (`before` ignored; each id must be
// full 32-hex; active tasks are skipped unless force). Mirrors
// (*cli.Client).Prune's two modes.
func DoPruneTasks(c *cli.Client, before time.Duration, taskIDs []string, force bool) tea.Cmd {
	idMode := len(taskIDs) > 0
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cutoff := time.Time{}
		if !idMode {
			cutoff = time.Now().Add(-before)
			force = false
		}
		res, err := c.PruneTasks(ctx, cutoff, taskIDs, force)
		if err != nil {
			return PruneResultMsg{IDMode: idMode, Err: err}
		}
		return PruneResultMsg{
			Removed:        res.Removed,
			IDMode:         idMode,
			SkippedActive:  res.SkippedActive,
			SkippedMissing: res.SkippedMissing,
			Forced:         force,
		}
	}
}

// AwaitIdleResultMsg carries the outcome of a session await-idle command.
// For the reply sink this arrives when the watcher FIRES (or the session
// stops) — potentially minutes after the command. For notify/board sinks it
// arrives immediately with Status=Armed.
type AwaitIdleResultMsg struct {
	TaskID       string
	Status       protocol.AwaitIdleStatus
	LastOutputAt uint64
	Err          error
}

// DoAwaitIdle arms a one-shot idle watcher via the persistent client. ctx
// must be the long-lived app context, NOT a 10s round-trip timeout: the
// reply sink's response is deferred until the session actually goes idle.
func DoAwaitIdle(ctx context.Context, c *cli.Client, taskID string, thresholdMs uint32, sink protocol.AwaitIdleSink, topic string) tea.Cmd {
	return func() tea.Msg {
		resp, err := c.AwaitIdle(ctx, taskID, thresholdMs, sink, topic)
		if err != nil {
			return AwaitIdleResultMsg{TaskID: taskID, Err: err}
		}
		return AwaitIdleResultMsg{TaskID: taskID, Status: resp.Status, LastOutputAt: resp.LastOutputAt}
	}
}

// RefreshSnapshot wraps (*cli.Client).Snapshot with the SnapshotMsg envelope
// the TUI's update loop expects. The RoundTripTaskControl + decode lives in
// the cli package so the wasm bridge and other consumers can share it.
func RefreshSnapshot(c *cli.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		lr, err := c.Snapshot(ctx)
		if err != nil {
			return SnapshotMsg{Err: err}
		}
		return SnapshotMsg{Runners: lr.Runners, Tasks: lr.Tasks}
	}
}

// SessionListMsg carries the result of a session ls command: a slice of
// interactive+detachable tasks, or an error if the snapshot failed.
type SessionListMsg struct {
	Tasks []protocol.TaskInfo
	Err   error
}

// DoSessionList fetches a snapshot and returns only interactive+detachable
// tasks in a SessionListMsg.
func DoSessionList(c *cli.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		lr, err := c.Snapshot(ctx)
		if err != nil {
			return SessionListMsg{Err: err}
		}
		var sessions []protocol.TaskInfo
		for _, t := range lr.Tasks {
			if protocol.IsSessionKind(t.Kind) {
				sessions = append(sessions, t)
			}
		}
		return SessionListMsg{Tasks: sessions}
	}
}
