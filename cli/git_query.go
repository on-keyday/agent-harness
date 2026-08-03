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
	Kind      protocol.GitQueryKind
	Target    protocol.GitDiffTarget
	BaseRev   string
	TargetRev string
	// Path filters WITHIN a repository; Subrepo chooses WHICH repository.
	// They are not interchangeable and may be combined.
	Path    string
	Subrepo string
	// SubmoduleDiff inlines a submodule's own file-level changes under its
	// gitlink entry. Off by default: the combined output is no longer an
	// applyable patch.
	SubmoduleDiff bool
	MaxCommits    uint32
	MaxBytes      uint32
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
	Subrepos  []string // repo roots nested under the query's current root
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
	body.SetSubrepo([]byte(q.Subrepo))
	body.SetSubmoduleDiff(q.SubmoduleDiff)
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
	case protocol.GitQueryKind_Subrepos:
		if sr := res.Subrepos(); sr != nil {
			out.Truncated = sr.Truncated()
			for _, e := range sr.Entries {
				out.Subrepos = append(out.Subrepos, string(e.Path))
			}
		}
	case protocol.GitQueryKind_File:
		if tb := res.File(); tb != nil {
			out.Text = string(tb.Text)
			out.Truncated = tb.Truncated()
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

// The wrappers take a GitQuery for the optional knobs rather than growing a
// parameter each time one is added — subrepo and submodule are already two, and
// a positional list that long is where callers start passing them in the wrong
// order.

func (c *Client) GitLog(ctx context.Context, taskIDHex string, q GitQuery) (*GitResult, error) {
	q.Kind = protocol.GitQueryKind_Log
	return c.GitQuery(ctx, taskIDHex, q)
}

func (c *Client) GitDiff(ctx context.Context, taskIDHex string, q GitQuery) (*GitResult, error) {
	q.Kind = protocol.GitQueryKind_Diff
	return c.GitQuery(ctx, taskIDHex, q)
}

func (c *Client) GitShow(ctx context.Context, taskIDHex string, q GitQuery) (*GitResult, error) {
	q.Kind = protocol.GitQueryKind_Show
	return c.GitQuery(ctx, taskIDHex, q)
}

func (c *Client) GitStatus(ctx context.Context, taskIDHex string, q GitQuery) (*GitResult, error) {
	q.Kind = protocol.GitQueryKind_Status
	return c.GitQuery(ctx, taskIDHex, q)
}

// GitFile returns one file's whole content from the side q.Target names:
// worktree the file on disk, index the staged blob, rev the blob at
// q.TargetRev. q.Path is relative to the query's current root, so a caller
// reading a diff can pass the path exactly as the diff header spells it —
// including inside a subrepo, where the runner has already re-rooted.
func (c *Client) GitFile(ctx context.Context, taskIDHex string, q GitQuery) (*GitResult, error) {
	q.Kind = protocol.GitQueryKind_File
	return c.GitQuery(ctx, taskIDHex, q)
}

// GitSubrepos lists the git repo roots nested under the query's current root.
// With q.Subrepo set it lists what is nested one level further in.
func (c *Client) GitSubrepos(ctx context.Context, taskIDHex string, q GitQuery) (*GitResult, error) {
	q.Kind = protocol.GitQueryKind_Subrepos
	return c.GitQuery(ctx, taskIDHex, q)
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

// DiffFilePath returns the post-image path named by a unified-diff `+++` line,
// or "" for any other line.
//
// The `+++` line is used rather than the `diff --git a/x b/x` header because
// the header is genuinely ambiguous: it is `a/<p1> b/<p2>` with no delimiter
// that cannot also occur inside a path, so `diff --git a/odd b/name.txt b/odd
// b/name.txt` has no parse. Git itself does not parse it either — every tool
// reads the `---` / `+++` lines, which carry exactly one path each.
//
// The b-side is the right-hand side of the diff, which is what a reader asking
// "show me this whole file" means. A deletion writes `+++ /dev/null`, where
// there is no file to open, so that answers "" too.
func DiffFilePath(line string) string {
	const prefix = "+++ "
	if !strings.HasPrefix(line, prefix) {
		return ""
	}
	path := line[len(prefix):]
	// git prefixes the b-side with "b/" unless --no-prefix was used.
	path = strings.TrimPrefix(path, "b/")
	if path == "" || path == "/dev/null" || path == "dev/null" {
		return ""
	}
	return path
}

// DiffFilePathAt returns the file whose section of the diff contains line i.
//
// It resolves the SECTION first — the `diff --git` header at or above i, up to
// the next one — and then reads that section's `+++` line, which may be above
// OR below i: standing on the header itself is the common case, and the `+++`
// is three lines further down. Scanning only upward answered "" there.
//
// Returns "" when i sits above the first file, or when the section's `+++` is
// /dev/null (a deletion has no right-hand file to open).
//
// Both the TUI and the CLI resolve "the file I am looking at" through here so
// they cannot disagree about which section a line belongs to.
func DiffFilePathAt(lines []string, i int) string {
	if len(lines) == 0 {
		return ""
	}
	if i >= len(lines) {
		i = len(lines) - 1
	}
	if i < 0 {
		i = 0
	}
	// Walk up to this section's header.
	start := -1
	for j := i; j >= 0; j-- {
		if ClassifyGitLine(lines[j]) == GitLineFile {
			start = j
			break
		}
	}
	if start < 0 {
		return ""
	}
	// Read forward to the next header, taking the first +++ inside.
	for j := start + 1; j < len(lines); j++ {
		if ClassifyGitLine(lines[j]) == GitLineFile {
			return "" // section ended without one
		}
		if strings.HasPrefix(lines[j], "+++ ") {
			return DiffFilePath(lines[j])
		}
	}
	return ""
}
