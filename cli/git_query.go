package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// GitQuery names one read-only git question about a task's worktree.
// Zero values are meaningful: an empty BaseRev means the index for a diff,
// HEAD for a show, and the task's tip for a log — the same defaults git
// itself uses.
type GitQuery struct {
	Kind       protocol.GitQueryKind
	Target     protocol.GitDiffTarget
	BaseRev    string
	TargetRev  string
	Path       string
	MaxCommits uint32
	MaxBytes   uint32
}

type GitCommit struct {
	SHA     string
	Author  string
	When    time.Time
	Subject string
}

// Short returns the abbreviated object name the UIs display. Seven characters
// is git's own default abbreviation, taken from the front so a sha256 name
// abbreviates the same way a sha1 one does.
func (c GitCommit) Short() string {
	if len(c.SHA) <= 7 {
		return c.SHA
	}
	return c.SHA[:7]
}

type GitStatusEntry struct {
	XY   string
	Path string
}

// GitResult is one answer. Which of Commits / Entries / Text is populated
// follows Kind; the others stay nil or empty.
type GitResult struct {
	Status    protocol.GitRunStatus
	Stderr    string
	Kind      protocol.GitQueryKind
	Commits   []GitCommit
	Entries   []GitStatusEntry
	Text      string
	Truncated bool
}

// Err turns a non-ok runner status into an error carrying git's own stderr.
// When git said nothing — a status the runner decided before starting git —
// the status name stands in, so the message is never empty.
func (r *GitResult) Err() error {
	if r.Status == protocol.GitRunStatus_Ok {
		return nil
	}
	if r.Stderr != "" {
		return fmt.Errorf("git: %s", r.Stderr)
	}
	return fmt.Errorf("git: %s", r.Status.String())
}

// GitQuery round-trips a git_query and decodes the streamed result. It is a
// method on the long-lived *Client so the TUI and WebUI reuse the client they
// already hold, the same way ListFiles does — there is deliberately no
// dial-and-close variant for them to copy.
func (c *Client) GitQuery(ctx context.Context, taskIDHex string, q GitQuery) (*GitResult, error) {
	tid, err := parseTaskIDHex(taskIDHex)
	if err != nil {
		return nil, fmt.Errorf("git: parse task id: %w", err)
	}
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_GitQuery}
	body := protocol.GitQueryRequest{
		TaskId:     tid,
		Kind:       q.Kind,
		Target:     q.Target,
		MaxCommits: q.MaxCommits,
		MaxBytes:   q.MaxBytes,
	}
	body.SetBaseRev([]byte(q.BaseRev))
	body.SetTargetRev([]byte(q.TargetRev))
	body.SetPath([]byte(q.Path))
	req.SetGitQuery(body)

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.Kind != protocol.TaskControlKind_GitQuery {
		return nil, fmt.Errorf("git: unexpected response kind %v", resp.Kind)
	}
	r := resp.GitQuery()
	if r == nil {
		return nil, errors.New("git: response variant missing")
	}
	switch r.Status {
	case protocol.GitQueryStatus_Ok:
	case protocol.GitQueryStatus_NoSuchTask:
		return nil, errors.New("git: no such task")
	case protocol.GitQueryStatus_RunnerOffline:
		return nil, errors.New("git: runner offline")
	default:
		return nil, fmt.Errorf("git: %s", r.Status.String())
	}

	st := peer.WaitForBidirectionalStream(ctx, c.Transport(), trsf.StreamID(r.StreamId))
	if st == nil {
		return nil, fmt.Errorf("git: stream %d not visible", r.StreamId)
	}
	defer st.CloseBoth()
	if err := st.AppendData(true); err != nil {
		return nil, fmt.Errorf("git: half-close: %w", err)
	}
	raw, err := io.ReadAll(st)
	if err != nil {
		return nil, fmt.Errorf("git: read result: %w", err)
	}
	res := &protocol.GitQueryResult{}
	if _, err := res.Decode(raw); err != nil {
		return nil, fmt.Errorf("git: decode: %w", err)
	}
	return decodeGitResult(res), nil
}

func decodeGitResult(res *protocol.GitQueryResult) *GitResult {
	out := &GitResult{
		Status: res.Status,
		Stderr: string(res.Stderr),
		Kind:   res.Kind,
	}
	switch res.Kind {
	case protocol.GitQueryKind_Log:
		if lg := res.Log(); lg != nil {
			out.Truncated = lg.Truncated()
			for _, c := range lg.Commits {
				out.Commits = append(out.Commits, GitCommit{
					SHA:     string(c.Sha),
					Author:  string(c.Author),
					When:    time.Unix(int64(c.When), 0),
					Subject: string(c.Subject),
				})
			}
		}
	case protocol.GitQueryKind_Status:
		if sb := res.StatusBody(); sb != nil {
			for _, e := range sb.Entries {
				out.Entries = append(out.Entries, GitStatusEntry{
					XY:   string(e.Xy[:]),
					Path: string(e.Path),
				})
			}
		}
	case protocol.GitQueryKind_Show:
		if tb := res.Show(); tb != nil {
			out.Text = string(tb.Text)
			out.Truncated = tb.Truncated()
		}
	default:
		if tb := res.Diff(); tb != nil {
			out.Text = string(tb.Text)
			out.Truncated = tb.Truncated()
		}
	}
	return out
}

func (c *Client) GitLog(ctx context.Context, taskIDHex, baseRev, path string, max uint32) (*GitResult, error) {
	return c.GitQuery(ctx, taskIDHex, GitQuery{
		Kind: protocol.GitQueryKind_Log, BaseRev: baseRev, Path: path, MaxCommits: max,
	})
}

func (c *Client) GitDiff(ctx context.Context, taskIDHex, baseRev string, target protocol.GitDiffTarget, targetRev, path string, maxBytes uint32) (*GitResult, error) {
	return c.GitQuery(ctx, taskIDHex, GitQuery{
		Kind: protocol.GitQueryKind_Diff, Target: target,
		BaseRev: baseRev, TargetRev: targetRev, Path: path, MaxBytes: maxBytes,
	})
}

func (c *Client) GitShow(ctx context.Context, taskIDHex, rev, path string, maxBytes uint32) (*GitResult, error) {
	return c.GitQuery(ctx, taskIDHex, GitQuery{
		Kind: protocol.GitQueryKind_Show, BaseRev: rev, Path: path, MaxBytes: maxBytes,
	})
}

func (c *Client) GitStatus(ctx context.Context, taskIDHex, path string) (*GitResult, error) {
	return c.GitQuery(ctx, taskIDHex, GitQuery{Kind: protocol.GitQueryKind_Status, Path: path})
}

// GitLineClass tags a diff line for colouring. Each surface maps a class to
// its own styling; the classification lives here so the CLI, the TUI and the
// WebUI cannot disagree about what a line is.
type GitLineClass int

const (
	GitLinePlain GitLineClass = iota
	GitLineAdd
	GitLineDel
	GitLineHunk
	GitLineFile
	GitLineMeta
)

// ClassifyGitLine tags one line of unified diff output. The header checks run
// before the +/- checks because "--- a/x" and "+++ b/x" start with the same
// bytes as a deletion and an addition.
func ClassifyGitLine(line string) GitLineClass {
	switch {
	case strings.HasPrefix(line, "diff --git "), strings.HasPrefix(line, "diff --cc "):
		return GitLineFile
	case strings.HasPrefix(line, "--- "), strings.HasPrefix(line, "+++ "):
		return GitLineMeta
	case strings.HasPrefix(line, "index "),
		strings.HasPrefix(line, "new file mode "),
		strings.HasPrefix(line, "deleted file mode "),
		strings.HasPrefix(line, "old mode "),
		strings.HasPrefix(line, "new mode "),
		strings.HasPrefix(line, "similarity index "),
		strings.HasPrefix(line, "rename from "),
		strings.HasPrefix(line, "rename to "),
		strings.HasPrefix(line, "Binary files "):
		return GitLineMeta
	case strings.HasPrefix(line, "@@"):
		return GitLineHunk
	case strings.HasPrefix(line, "+"):
		return GitLineAdd
	case strings.HasPrefix(line, "-"):
		return GitLineDel
	default:
		return GitLinePlain
	}
}
