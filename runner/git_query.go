package runner

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// gitQueryTimeout bounds a single git invocation. A diff of a large tree is
// the slow case; 30s is well past it and short enough that a wedged git does
// not hold a stream open indefinitely.
const gitQueryTimeout = 30 * time.Second

const (
	gitDefaultMaxCommits = 100
	gitMaxMaxCommits     = 1000
	gitDefaultMaxBytes   = 2 * 1024 * 1024
	gitMaxMaxBytes       = 8 * 1024 * 1024
)

// gitLogFormat separates fields with NUL and records with newline. A commit
// subject can contain neither (%s stops at the first newline), so the field
// walk in parseGitLog is unambiguous.
const gitLogFormat = "--format=%H%x00%an%x00%at%x00%s"

func clampMaxCommits(v uint32) uint32 {
	if v == 0 {
		return gitDefaultMaxCommits
	}
	if v > gitMaxMaxCommits {
		return gitMaxMaxCommits
	}
	return v
}

func clampMaxBytes(v uint32) uint32 {
	if v == 0 {
		return gitDefaultMaxBytes
	}
	if v > gitMaxMaxBytes {
		return gitMaxMaxBytes
	}
	return v
}

// gitSourceFor decides where git runs for taskIDHex and what names the task's
// tip.
//
// Worktree present (the normal case for a Running / Detached task): run there,
// HEAD is the tip, and uncommitted plus untracked state is visible. This is
// the case the whole feature exists for — reading a running agent's work from
// the side.
//
// Worktree gone (a terminal task whose dir was removed after a clean finish):
// run in the repo and name the retained branch explicitly, because the repo's
// HEAD points at whatever the user has checked out, not at this task's work.
//
// noWorktree mirrors Session.NoWorktree: the agent ran directly in the repo,
// so the repo IS the task's working directory.
func gitSourceFor(repoPath, taskIDHex string, noWorktree bool) (cwd, tip string, status protocol.GitRunStatus) {
	if !isGitRepo(repoPath) {
		return "", "", protocol.GitRunStatus_NotAGitRepo
	}
	if noWorktree {
		return repoPath, "HEAD", protocol.GitRunStatus_Ok
	}
	wt := filepath.Join(repoPath, ".harness-worktrees", taskIDHex)
	// isWorktreeRoot, not a bare os.Stat: an orphan directory left behind by a
	// crashed cleanup still exists, and git run inside it walks up to the
	// parent repo's .git and answers about the PARENT filtered to that
	// subdirectory — a wrong answer wearing a right answer's face.
	if isWorktreeRoot(wt) {
		return wt, "HEAD", protocol.GitRunStatus_Ok
	}
	// WorktreeManager.Remove deliberately keeps branch harness/<taskID> after
	// dropping the worktree, so the work stays reachable. This is the only
	// reader of that promise.
	ref := "refs/heads/harness/" + taskIDHex
	if refExists(repoPath, ref) {
		return repoPath, ref, protocol.GitRunStatus_Ok
	}
	return "", "", protocol.GitRunStatus_NoSource
}

// gitArgv builds the argv after "git". The returned slice always ends with the
// "--" pathspec separator (with the pathspec after it when non-empty) so a rev
// can never be reinterpreted as a path or vice versa.
//
// Every diff-producing form carries --no-ext-diff and --no-textconv: without
// them a .gitattributes in the worktree — which the agent can write — names an
// external diff driver, and reading a diff becomes command execution on the
// runner host.
func gitArgv(req *protocol.RunnerGitQueryRequest, tip string) ([]string, protocol.GitRunStatus) {
	base := string(req.BaseRev)
	target := string(req.TargetRev)
	path := string(req.Path)

	for _, s := range []string{base, target, path} {
		if strings.HasPrefix(s, "-") {
			return nil, protocol.GitRunStatus_BadRev
		}
	}

	var argv []string
	switch req.Kind {
	case protocol.GitQueryKind_Log:
		start := base
		if start == "" {
			start = tip
		}
		n := clampMaxCommits(req.MaxCommits)
		argv = []string{"log", "--no-color", gitLogFormat,
			"--max-count=" + strconv.FormatUint(uint64(n)+1, 10), start}

	case protocol.GitQueryKind_Diff:
		argv = []string{"diff", "--no-color", "--no-ext-diff", "--no-textconv"}
		switch req.Target {
		case protocol.GitDiffTarget_Index:
			argv = append(argv, "--cached")
			if base != "" {
				argv = append(argv, base)
			}
		case protocol.GitDiffTarget_Rev:
			if base == "" {
				// `git diff <nothing> <target>` is not a thing. Refuse rather
				// than build an argv with a hole in it.
				return nil, protocol.GitRunStatus_BadRev
			}
			argv = append(argv, base, target)
		default: // worktree
			if base != "" {
				argv = append(argv, base)
			}
		}

	case protocol.GitQueryKind_Show:
		rev := base
		if rev == "" {
			rev = tip
		}
		argv = []string{"show", "--no-color", "--no-ext-diff", "--no-textconv", rev}

	case protocol.GitQueryKind_Status:
		argv = []string{"status", "--porcelain", "--untracked-files=all", "-z"}

	default:
		return nil, protocol.GitRunStatus_BadRev
	}

	argv = append(argv, "--")
	if path != "" {
		argv = append(argv, path)
	}
	return argv, protocol.GitRunStatus_Ok
}

// parseGitLog walks gitLogFormat output. Records are newline-terminated and
// fields are NUL-separated; a record with fewer than four fields is skipped
// rather than half-decoded. max is the client's requested count — git was
// asked for one more, so a longer result means the list was cut short.
func parseGitLog(out []byte, max uint32) ([]protocol.GitCommit, bool) {
	var commits []protocol.GitCommit
	truncated := false
	for _, rec := range strings.Split(string(out), "\n") {
		if rec == "" {
			continue
		}
		fields := strings.Split(rec, "\x00")
		if len(fields) < 4 {
			continue
		}
		if uint32(len(commits)) >= max {
			truncated = true
			break
		}
		when, _ := strconv.ParseUint(fields[2], 10, 64)
		var c protocol.GitCommit
		c.SetSha([]byte(fields[0]))
		c.SetAuthor([]byte(fields[1]))
		c.When = when
		c.SetSubject([]byte(fields[3]))
		commits = append(commits, c)
	}
	return commits, truncated
}

// parseGitStatusZ walks `git status --porcelain -z` output: NUL-terminated
// records of "XY <path>". A rename or copy entry (X is 'R' or 'C') carries the
// source path in a following record; it is consumed and discarded, exactly as
// filterDirtyPaths does in worktree.go.
func parseGitStatusZ(out []byte) []protocol.GitStatusEntry {
	var entries []protocol.GitStatusEntry
	records := strings.Split(string(out), "\x00")
	for i := 0; i < len(records); i++ {
		rec := records[i]
		if len(rec) < 4 {
			continue
		}
		status := rec[:2]
		path := rec[3:]
		if status[0] == 'R' || status[0] == 'C' {
			i++
		}
		var e protocol.GitStatusEntry
		e.Xy = [2]byte{status[0], status[1]}
		e.SetPath([]byte(path))
		entries = append(entries, e)
	}
	return entries
}

// isGitRepo reports whether dir is inside a git working tree. It shells out
// rather than looking for a ".git" entry because a worktree's .git is a file,
// not a directory, and reimplementing that discovery is not worth it.
func isGitRepo(dir string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), gitQueryTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-dir")
	cmd.Dir = dir
	return cmd.Run() == nil
}

// isWorktreeRoot reports whether dir is itself the top level of a git working
// tree, as opposed to merely sitting inside one. Symlinks are resolved on both
// sides because git reports the real path and the caller may hold a symlinked
// one (a temp dir under /tmp on macOS, for instance).
func isWorktreeRoot(dir string) bool {
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitQueryTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	top, err := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
	if err != nil {
		return false
	}
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false
	}
	return top == want
}

func refExists(repoPath, ref string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), gitQueryTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "show-ref", "--verify", "--quiet", ref)
	cmd.Dir = repoPath
	return cmd.Run() == nil
}

// runGitQuery executes argv in cwd and captures at most maxBytes of stdout.
// Over the cap the process is killed rather than drained: a diff of a huge
// tree should not stream gigabytes into memory just to be discarded.
//
// A non-zero exit is classified from git's own stderr. "unknown revision",
// "bad revision" and "ambiguous argument" all mean the caller named something
// that does not resolve, which is a user-correctable bad_rev; anything else is
// git_failed and carries stderr verbatim so the operator sees git's words
// rather than ours.
func runGitQuery(ctx context.Context, cwd string, argv []string, maxBytes uint32) ([]byte, string, bool, protocol.GitRunStatus) {
	ctx, cancel := context.WithTimeout(ctx, gitQueryTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", argv...)
	cmd.Dir = cwd
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err.Error(), false, protocol.GitRunStatus_IoError
	}
	if err := cmd.Start(); err != nil {
		return nil, err.Error(), false, protocol.GitRunStatus_IoError
	}

	// One byte past the cap is enough to know the output was longer.
	out, readErr := io.ReadAll(io.LimitReader(stdout, int64(maxBytes)+1))
	truncated := false
	if uint32(len(out)) > maxBytes {
		out = out[:maxBytes]
		truncated = true
		cancel() // stop git rather than drain the rest
	}
	waitErr := cmd.Wait()
	stderr := strings.TrimSpace(errBuf.String())

	if truncated {
		// The kill above makes Wait report failure; what we captured is still
		// exactly what the caller asked for.
		return out, "", true, protocol.GitRunStatus_Ok
	}
	if readErr != nil {
		return nil, readErr.Error(), false, protocol.GitRunStatus_IoError
	}
	if waitErr != nil {
		low := strings.ToLower(stderr)
		switch {
		case strings.Contains(low, "unknown revision"),
			strings.Contains(low, "bad revision"),
			strings.Contains(low, "ambiguous argument"),
			strings.Contains(low, "bad object"):
			return nil, stderr, false, protocol.GitRunStatus_BadRev
		default:
			return nil, stderr, false, protocol.GitRunStatus_GitFailed
		}
	}
	return out, stderr, false, protocol.GitRunStatus_Ok
}

// setGitBody puts body on the union arm matching kind. The diff and show arms
// carry the same type, so the only difference is which setter the encoder
// needs.
func setGitTextBody(res *protocol.GitQueryResult, kind protocol.GitQueryKind, body protocol.GitTextBody) {
	if kind == protocol.GitQueryKind_Show {
		res.SetShow(body)
		return
	}
	res.SetDiff(body)
}

// emptyGitBody fills the union arm for kind with a zero body. Every result
// encodes an arm, including the failures — see the GitQueryResult comment in
// message.bgn.
func emptyGitBody(res *protocol.GitQueryResult, kind protocol.GitQueryKind) {
	switch kind {
	case protocol.GitQueryKind_Log:
		res.SetLog(protocol.GitLogBody{})
	case protocol.GitQueryKind_Status:
		res.SetStatusBody(protocol.GitStatusBody{})
	default:
		setGitTextBody(res, kind, protocol.GitTextBody{})
	}
}

// buildGitResult is the whole runner-side answer for one request, with no
// stream involvement, so it is directly testable.
func (s *Session) buildGitResult(ctx context.Context, req *protocol.RunnerGitQueryRequest) protocol.GitQueryResult {
	fail := func(st protocol.GitRunStatus, stderr string) protocol.GitQueryResult {
		var res protocol.GitQueryResult
		res.Status = st
		res.SetStderr([]byte(stderr))
		res.Kind = req.Kind
		emptyGitBody(&res, req.Kind)
		return res
	}

	repoPath := string(req.RepoPath)
	// AllowedRoots empty means an unconfigured Session (tests); the assign /
	// open-exec paths guard the same way.
	if len(s.AllowedRoots) > 0 && !s.repoAllowed(repoPath) {
		return fail(protocol.GitRunStatus_RepoNotAllowed, "repo is not under this runner's --roots")
	}

	taskIDHex := hex.EncodeToString(req.TaskId.Id[:])
	cwd, tip, st := gitSourceFor(repoPath, taskIDHex, s.NoWorktree)
	if st != protocol.GitRunStatus_Ok {
		return fail(st, "")
	}

	argv, st := gitArgv(req, tip)
	if st != protocol.GitRunStatus_Ok {
		return fail(st, "a revision or pathspec began with '-'")
	}

	out, stderr, truncated, st := runGitQuery(ctx, cwd, argv, clampMaxBytes(req.MaxBytes))
	if st != protocol.GitRunStatus_Ok {
		return fail(st, stderr)
	}

	var res protocol.GitQueryResult
	res.Status = protocol.GitRunStatus_Ok
	res.SetStderr(nil)
	res.Kind = req.Kind
	switch req.Kind {
	case protocol.GitQueryKind_Log:
		commits, logTruncated := parseGitLog(out, clampMaxCommits(req.MaxCommits))
		var body protocol.GitLogBody
		body.SetCommits(commits)
		body.SetTruncated(logTruncated)
		res.SetLog(body)
	case protocol.GitQueryKind_Status:
		var body protocol.GitStatusBody
		body.SetEntries(parseGitStatusZ(out))
		res.SetStatusBody(body)
	default:
		var body protocol.GitTextBody
		body.SetText(out)
		body.SetTruncated(truncated)
		setGitTextBody(&res, req.Kind, body)
	}
	return res
}

// handleGitQuery answers one git_query on the stream the server allocated.
// Shaped exactly like handleListFiles: wait for the stream, always write
// something, always close.
func (s *Session) handleGitQuery(ctx context.Context, req *protocol.RunnerGitQueryRequest) {
	log := s.logger()
	stream := peer.WaitForBidirectionalStream(ctx, s.Streams, trsf.StreamID(req.StreamId))
	if stream == nil {
		log.Error("git_query: stream not visible", "stream_id", req.StreamId)
		return
	}
	defer stream.CloseBoth()

	res := s.buildGitResult(ctx, req)
	body, err := res.Append(nil)
	if err != nil {
		log.Error("git_query: encode result", "err", err)
		return
	}
	if _, err := stream.Write(body); err != nil {
		log.Error("git_query: write result", "err", err)
		return
	}
	if err := stream.AppendData(true); err != nil {
		log.Error("git_query: half-close", "err", err)
	}
}
