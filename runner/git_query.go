package runner

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"os"

	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/hostcmd"
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
// hasWorktree distinguishes the two shapes: true when cwd IS the task's
// working tree (rows 1 and 2 of the table above), false when the task's
// working tree is gone and only its branch remains. In the false case cwd is
// the shared repository, whose HEAD and working tree belong to whoever checked
// it out — not to this task — so the caller must not let either stand in for
// the task's state. See gitArgv and buildGitResult.
func gitSourceFor(repoPath, taskIDHex string, noWorktree bool) (cwd, tip string, hasWorktree bool, status protocol.GitRunStatus) {
	if !isGitRepo(repoPath) {
		return "", "", false, protocol.GitRunStatus_NotAGitRepo
	}
	if noWorktree {
		return repoPath, "HEAD", true, protocol.GitRunStatus_Ok
	}
	wt := filepath.Join(repoPath, ".harness-worktrees", taskIDHex)
	// isWorktreeRoot, not a bare os.Stat: an orphan directory left behind by a
	// crashed cleanup still exists but holds no .git, and git run inside it
	// walks up to the parent repo and answers about the PARENT filtered to that
	// subdirectory — a wrong answer wearing a right answer's face.
	if isWorktreeRoot(wt) {
		return wt, "HEAD", true, protocol.GitRunStatus_Ok
	}
	// WorktreeManager.Remove deliberately keeps branch harness/<taskID> after
	// dropping the worktree, so the work stays reachable. This is the only
	// reader of that promise.
	ref := "refs/heads/harness/" + taskIDHex
	if refExists(repoPath, ref) {
		return repoPath, ref, false, protocol.GitRunStatus_Ok
	}
	return "", "", false, protocol.GitRunStatus_NoSource
}

// gitSubrepoMaxDepth bounds the discovery walk. Deep enough for the
// packages/<name>/<repo> shapes people actually have, shallow enough that the
// walk stays cheap on a large tree.
const gitSubrepoMaxDepth = 6

// gitSubrepoMaxCandidates bounds how many repo roots one walk reports. The
// body carries a truncated flag alongside it: a cap that is not reported reads
// as "there were none".
const gitSubrepoMaxCandidates = 64

// resolveSubrepo re-roots a query into a nested repository. root is the cwd
// the task would otherwise use; rel is the client's worktree-relative
// directory.
//
// Every failure is subrepo_invalid rather than a fallback to root: running one
// directory up and answering about the enclosing repository is precisely the
// wrong-answer-wearing-a-right-answer's-face this file keeps guarding against.
//
// The repo-root test is the same one used for worktrees, which is what makes
// this work for a plain nested repo (a directory with its own .git that the
// outer repo only ever sees as one untracked entry) as well as for a submodule.
func resolveSubrepo(root, rel string) (string, protocol.GitRunStatus) {
	if rel == "" {
		return root, protocol.GitRunStatus_Ok
	}
	full, err := ValidateRelPath(root, rel)
	if err != nil {
		return "", protocol.GitRunStatus_SubrepoInvalid
	}
	if err := rejectIfSymlinkInPath(root, full); err != nil {
		return "", protocol.GitRunStatus_SubrepoInvalid
	}
	if !isWorktreeRoot(full) {
		return "", protocol.GitRunStatus_SubrepoInvalid
	}
	return full, protocol.GitRunStatus_Ok
}

// findSubrepos walks root and returns the nested git repo roots under it, as
// paths relative to root. It does not descend into a repo it has found — a
// repository inside a nested repository is that repository's business, and the
// operator reaches it by re-rooting there first.
//
// A directory is a repo root when it holds a `.git` entry of EITHER kind: a
// submodule's .git is a file, not a directory. The check is one lstat per
// directory — it used to also spawn `git rev-parse` per candidate, which on a
// Windows runner cost more than the rest of the walk put together.
func findSubrepos(root string) ([]protocol.GitSubrepoEntry, bool) {
	var out []protocol.GitSubrepoEntry
	truncated := false

	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if truncated || depth > gitSubrepoMaxDepth {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, e := range entries {
			if truncated {
				return
			}
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			// .git is the repository's own storage; .harness-worktrees holds
			// OTHER tasks' checkouts, which are not this task's content.
			if name == ".git" || name == ".harness-worktrees" {
				continue
			}
			child := filepath.Join(dir, name)
			if isWorktreeRoot(child) {
				rel, err := filepath.Rel(root, child)
				if err != nil {
					continue
				}
				if len(out) >= gitSubrepoMaxCandidates {
					truncated = true
					return
				}
				var ent protocol.GitSubrepoEntry
				ent.SetPath([]byte(filepath.ToSlash(rel)))
				out = append(out, ent)
				continue // do not descend into a repo we have found
			}
			walk(child, depth+1)
		}
	}
	walk(root, 1)
	return out, truncated
}

// hasGitEntry reports whether dir holds a `.git` of either kind — a directory
// for a normal repository, a file for a `git worktree add` checkout or a
// submodule.
func hasGitEntry(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// rebaseHeadOnTip rewrites a leading HEAD to tip. Without it, a rev like
// "HEAD~1" against a task whose worktree is gone silently resolves against the
// SHARED repository's checkout: "HEAD" then means whatever branch the user
// happens to have out, so `diff HEAD` answers about main and reads as a
// correct empty diff. Everything after the leading HEAD (~1, ^, @{2}) is kept,
// because it is the same suffix grammar either way.
func rebaseHeadOnTip(rev, tip string, hasWorktree bool) string {
	if hasWorktree || rev == "" || tip == "" {
		return rev
	}
	if rev == "HEAD" {
		return tip
	}
	if strings.HasPrefix(rev, "HEAD~") || strings.HasPrefix(rev, "HEAD^") || strings.HasPrefix(rev, "HEAD@") {
		return tip + rev[len("HEAD"):]
	}
	return rev
}

// gitArgv builds the argv after "git". The returned slice always ends with the
// "--" pathspec separator (with the pathspec after it when non-empty) so a rev
// can never be reinterpreted as a path or vice versa.
//
// Every diff-producing form carries --no-ext-diff and --no-textconv: without
// them a .gitattributes in the worktree — which the agent can write — names an
// external diff driver, and reading a diff becomes command execution on the
// runner host.
func gitArgv(req *protocol.RunnerGitQueryRequest, tip string, hasWorktree bool) ([]string, protocol.GitRunStatus) {
	base := string(req.BaseRev)
	target := string(req.TargetRev)
	path := string(req.Path)

	for _, s := range []string{base, target, path} {
		if strings.HasPrefix(s, "-") {
			return nil, protocol.GitRunStatus_BadRev
		}
	}
	base = rebaseHeadOnTip(base, tip, hasWorktree)
	target = rebaseHeadOnTip(target, tip, hasWorktree)

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
		if req.SubmoduleDiff() {
			argv = append(argv, "--submodule=diff")
		}
		switch {
		case !hasWorktree && req.Target != protocol.GitDiffTarget_Rev:
			// With the task's working tree gone, "the working tree" and "the
			// index" name states that no longer exist — git would answer about
			// the SHARED repository's checkout instead, which belongs to
			// whoever has it out. The task's tip stands in for both. An empty
			// base then yields `diff <tip> <tip>`, i.e. nothing uncommitted,
			// which is the truth for a task with no working tree; inventing a
			// parent rev instead would break on a root commit and would answer
			// a question nobody asked.
			if base == "" {
				base = tip
			}
			argv = append(argv, base, tip)

		case req.Target == protocol.GitDiffTarget_Index:
			argv = append(argv, "--cached")
			if base != "" {
				argv = append(argv, base)
			}

		case req.Target == protocol.GitDiffTarget_Rev:
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
		argv = []string{"show", "--no-color", "--no-ext-diff", "--no-textconv"}
		if req.SubmoduleDiff() {
			argv = append(argv, "--submodule=diff")
		}
		argv = append(argv, rev)

	case protocol.GitQueryKind_Status:
		argv = []string{"status", "--porcelain", "--untracked-files=all", "-z"}

	case protocol.GitQueryKind_Subrepos:
		// Answered by a filesystem walk, not by git. buildGitResult
		// short-circuits before reaching here.
		return nil, protocol.GitRunStatus_Ok

	case protocol.GitQueryKind_File:
		if path == "" {
			// `rev:` with no path is the ROOT TREE, which would answer a
			// question nobody asked.
			return nil, protocol.GitRunStatus_FileNotFound
		}
		// The whole file from the side `target` names. The worktree side is
		// not a git command at all — it is the file on disk — and
		// buildGitResult reads it directly before reaching here.
		rev := ""
		switch req.Target {
		case protocol.GitDiffTarget_Index:
			// `git show :<path>` is the staged blob.
			rev = ""
		case protocol.GitDiffTarget_Rev:
			rev = target
			if rev == "" {
				rev = base
			}
			if rev == "" {
				return nil, protocol.GitRunStatus_BadRev
			}
		}
		// The pathspec separator is deliberately absent: `rev:path` is one
		// token, and a `--` after it would make git look for a second thing
		// to show.
		return []string{"show", "--no-color", "--no-ext-diff", "--no-textconv", rev + ":" + path}, protocol.GitRunStatus_Ok

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

// isGitRepo reports whether dir is inside a git working tree.
//
// The lstat is a fast path, not a shortcut: spawning a process costs ~300ms on
// the Windows runner in use here (measured), and this ran before EVERY query.
// A repository root always holds a .git entry — a directory for a normal repo,
// a file for a `git worktree add` checkout — so its presence settles the common
// case without a spawn. Only when it is absent do we ask git, which still
// covers a runner rooted at a SUBDIRECTORY of a repository (where .git lives
// further up) and any layout we do not create ourselves.
func isGitRepo(dir string) bool {
	if hasGitEntry(dir) {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitQueryTimeout)
	defer cancel()
	cmd := hostcmd.CommandContext(ctx, "git", "rev-parse", "--git-dir")
	cmd.Dir = dir
	return cmd.Run() == nil
}

// isWorktreeRoot reports whether dir is itself the top level of a git working
// tree, as opposed to merely sitting inside one.
//
// The test is the presence of a .git entry directly in dir, which is exactly
// what separates a real checkout from the case this guards against: a directory
// left behind by a crashed cleanup has no .git at all, and git run inside it
// walks UP to the enclosing repository and answers about that instead. It
// costs one lstat.
//
// This used to compare `git rev-parse --show-toplevel` against dir. That is one
// process spawn per call, and the walk in findSubrepos calls it once per
// candidate — ~300ms each on the Windows runner in use here (measured). The
// lstat decides the same question for every layout the harness creates: a
// `git worktree add` checkout has a .git file, a normal repo has a .git
// directory, and an orphan has neither.
func isWorktreeRoot(dir string) bool {
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return false
	}
	return hasGitEntry(dir)
}

func refExists(repoPath, ref string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), gitQueryTimeout)
	defer cancel()
	cmd := hostcmd.CommandContext(ctx, "git", "show-ref", "--verify", "--quiet", ref)
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

	cmd := hostcmd.CommandContext(ctx, "git", argv...)
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
	switch kind {
	case protocol.GitQueryKind_Show:
		res.SetShow(body)
	case protocol.GitQueryKind_File:
		res.SetFile(body)
	default:
		res.SetDiff(body)
	}
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
	case protocol.GitQueryKind_Subrepos:
		res.SetSubrepos(protocol.GitSubrepoBody{})
	case protocol.GitQueryKind_File:
		res.SetFile(protocol.GitTextBody{})
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
	cwd, tip, hasWorktree, st := gitSourceFor(repoPath, taskIDHex, s.NoWorktree)
	if st != protocol.GitRunStatus_Ok {
		return fail(st, "")
	}

	// Re-root into a nested repository, if one was named. This has to happen
	// before every other decision below: the tip, the working tree and the
	// status all become that repository's.
	if sub := string(req.Subrepo); sub != "" {
		if !hasWorktree {
			// A nested repo is an untracked directory inside the task's
			// worktree, so it went with the worktree. The harness/<taskID>
			// branch is in the OUTER repository and says nothing about it.
			return fail(protocol.GitRunStatus_NoSource,
				"the task's worktree is gone, and a nested repository lived inside it")
		}
		sr, st := resolveSubrepo(cwd, sub)
		if st != protocol.GitRunStatus_Ok {
			return fail(st, "not a git repository root inside this worktree: "+sub)
		}
		cwd = sr
		// The nested repository has its own HEAD, and harness/<taskID> does
		// not exist in it.
		tip = "HEAD"
	}

	// The worktree side of a file is not a git command — it is the file on
	// disk. Same containment guards as the file-transfer ops: the path must
	// resolve inside cwd and must not pass through a symlink.
	if req.Kind == protocol.GitQueryKind_File && req.Target == protocol.GitDiffTarget_Worktree {
		return readWorktreeFile(cwd, string(req.Path), clampMaxBytes(req.MaxBytes))
	}

	if req.Kind == protocol.GitQueryKind_Subrepos {
		entries, truncated := findSubrepos(cwd)
		var body protocol.GitSubrepoBody
		body.SetEntries(entries)
		body.SetTruncated(truncated)
		var res protocol.GitQueryResult
		res.Status = protocol.GitRunStatus_Ok
		res.SetStderr(nil)
		res.Kind = req.Kind
		res.SetSubrepos(body)
		return res
	}

	// With no working tree there is nothing for this task to be dirty in.
	// Running `git status` in the shared repo would report ITS state — other
	// tasks' worktree directories show up as untracked entries — which is a
	// different task's answer wearing this one's label.
	if req.Kind == protocol.GitQueryKind_Status && !hasWorktree {
		var res protocol.GitQueryResult
		res.Status = protocol.GitRunStatus_Ok
		res.SetStderr(nil)
		res.Kind = req.Kind
		res.SetStatusBody(protocol.GitStatusBody{})
		return res
	}

	argv, st := gitArgv(req, tip, hasWorktree)
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

// readWorktreeFile answers the file kind for the worktree side. It is bounded
// by the same max_bytes as a diff and reports its own truncation, so a huge
// file cannot be pulled whole into memory just to be cut client-side.
func readWorktreeFile(cwd, rel string, maxBytes uint32) protocol.GitQueryResult {
	fail := func(st protocol.GitRunStatus, msg string) protocol.GitQueryResult {
		var res protocol.GitQueryResult
		res.Status = st
		res.SetStderr([]byte(msg))
		res.Kind = protocol.GitQueryKind_File
		res.SetFile(protocol.GitTextBody{})
		return res
	}
	if rel == "" {
		return fail(protocol.GitRunStatus_FileNotFound, "no path given")
	}
	full, err := ValidateRelPath(cwd, rel)
	if err != nil {
		return fail(protocol.GitRunStatus_FileNotFound, "path is outside the repository: "+rel)
	}
	if err := rejectIfSymlinkInPath(cwd, full); err != nil {
		return fail(protocol.GitRunStatus_FileNotFound, "path passes through a symlink: "+rel)
	}
	st, err := os.Stat(full)
	if err != nil {
		return fail(protocol.GitRunStatus_FileNotFound, "no such file in the working tree: "+rel)
	}
	if st.IsDir() {
		return fail(protocol.GitRunStatus_FileNotFound, "that is a directory: "+rel)
	}
	f, err := os.Open(full)
	if err != nil {
		return fail(protocol.GitRunStatus_IoError, err.Error())
	}
	defer f.Close()
	buf, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
	if err != nil {
		return fail(protocol.GitRunStatus_IoError, err.Error())
	}
	truncated := false
	if uint32(len(buf)) > maxBytes {
		buf = buf[:maxBytes]
		truncated = true
	}
	var body protocol.GitTextBody
	body.SetText(buf)
	body.SetTruncated(truncated)

	var res protocol.GitQueryResult
	res.Status = protocol.GitRunStatus_Ok
	res.SetStderr(nil)
	res.Kind = protocol.GitQueryKind_File
	res.SetFile(body)
	return res
}
