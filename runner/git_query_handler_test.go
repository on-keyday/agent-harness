package runner

import (
	"context"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestRunGitQueryCapturesStdout(t *testing.T) {
	repo := newTestRepo(t)
	req := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Log, MaxCommits: 10}
	argv, st := gitArgv(req, "HEAD", true)
	if st != protocol.GitRunStatus_Ok {
		t.Fatalf("argv status = %v", st)
	}
	out, stderr, truncated, status := runGitQuery(context.Background(), repo, argv, 1<<20)
	if status != protocol.GitRunStatus_Ok {
		t.Fatalf("status = %v, stderr = %q", status, stderr)
	}
	if truncated {
		t.Fatal("should not be truncated")
	}
	if !strings.Contains(string(out), "first") {
		t.Fatalf("stdout %q missing the commit subject", out)
	}
}

func TestRunGitQueryTruncatesAtMaxBytes(t *testing.T) {
	repo := newTestRepo(t)
	big := strings.Repeat("line of text\n", 5000)
	if err := os.WriteFile(filepath.Join(repo, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "big.txt")
	req := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Diff, Target: protocol.GitDiffTarget_Worktree}
	req.SetBaseRev([]byte("HEAD"))
	argv, _ := gitArgv(req, "HEAD", true)
	out, _, truncated, status := runGitQuery(context.Background(), repo, argv, 512)
	if status != protocol.GitRunStatus_Ok {
		t.Fatalf("status = %v", status)
	}
	if !truncated {
		t.Fatal("want truncated")
	}
	if len(out) > 512 {
		t.Fatalf("captured %d bytes, cap was 512", len(out))
	}
}

func TestRunGitQueryReportsBadRev(t *testing.T) {
	repo := newTestRepo(t)
	req := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Show}
	req.SetBaseRev([]byte("nosuchrev"))
	argv, _ := gitArgv(req, "HEAD", true)
	_, stderr, _, status := runGitQuery(context.Background(), repo, argv, 1<<20)
	if status != protocol.GitRunStatus_BadRev {
		t.Fatalf("status = %v, want bad_rev (stderr %q)", status, stderr)
	}
	if stderr == "" {
		t.Fatal("stderr should carry git's own message")
	}
}

func TestBuildGitResultRejectsDisallowedRepo(t *testing.T) {
	s := &Session{AllowedRoots: []string{"/definitely/not/here"}}
	req := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Status}
	req.SetRepoPath([]byte("/tmp"))
	res := s.buildGitResult(context.Background(), req)
	if res.Status != protocol.GitRunStatus_RepoNotAllowed {
		t.Fatalf("status = %v, want repo_not_allowed", res.Status)
	}
	// Even a refusal encodes its body arm, so the client's decode never fails.
	if res.StatusBody() == nil {
		t.Fatal("failure result must still carry an (empty) body arm")
	}
}

func TestBuildGitResultStatusKind(t *testing.T) {
	repo := newTestRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Session{AllowedRoots: []string{repo}, NoWorktree: true}
	req := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Status}
	req.SetRepoPath([]byte(repo))
	res := s.buildGitResult(context.Background(), req)
	if res.Status != protocol.GitRunStatus_Ok {
		t.Fatalf("status = %v, stderr %q", res.Status, res.Stderr)
	}
	body := res.StatusBody()
	if body == nil || body.Count == 0 {
		t.Fatalf("status body: %+v", body)
	}
	found := false
	for _, e := range body.Entries {
		if string(e.Path) == "untracked.txt" && e.Xy == [2]byte{'?', '?'} {
			found = true
		}
	}
	if !found {
		t.Fatalf("untracked file missing from %+v", body.Entries)
	}
}

// The whole point of the feature: an agent's uncommitted edit is visible while
// the task is still running.
func TestBuildGitResultDiffSeesUncommittedEdit(t *testing.T) {
	repo := newTestRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Session{AllowedRoots: []string{repo}, NoWorktree: true}
	req := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Diff, Target: protocol.GitDiffTarget_Worktree}
	req.SetBaseRev([]byte("HEAD"))
	req.SetRepoPath([]byte(repo))
	res := s.buildGitResult(context.Background(), req)
	if res.Status != protocol.GitRunStatus_Ok {
		t.Fatalf("status = %v stderr %q", res.Status, res.Stderr)
	}
	text := string(res.Diff().Text)
	if !strings.Contains(text, "+two") {
		t.Fatalf("diff missing the edit:\n%s", text)
	}
}

// A commit the agent already made must be reachable by naming a baseline
// before it — the case plain `git diff` answers with silence.
func TestBuildGitResultDiffFromEarlierBaseline(t *testing.T) {
	repo := newTestRepo(t)
	base := gitHead(t, repo)
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one\ncommitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "commit", "-q", "-am", "second")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one\ncommitted\ndirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Session{AllowedRoots: []string{repo}, NoWorktree: true}

	// Against HEAD only the uncommitted line shows.
	reqHead := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Diff, Target: protocol.GitDiffTarget_Worktree}
	reqHead.SetBaseRev([]byte("HEAD"))
	reqHead.SetRepoPath([]byte(repo))
	headRes := s.buildGitResult(context.Background(), reqHead)
	headText := string(headRes.Diff().Text)
	if strings.Contains(headText, "+committed") {
		t.Fatalf("HEAD baseline should not show the committed line:\n%s", headText)
	}

	// Against the earlier commit, both show.
	reqBase := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Diff, Target: protocol.GitDiffTarget_Worktree}
	reqBase.SetBaseRev([]byte(base))
	reqBase.SetRepoPath([]byte(repo))
	baseRes := s.buildGitResult(context.Background(), reqBase)
	baseText := string(baseRes.Diff().Text)
	if !strings.Contains(baseText, "+committed") || !strings.Contains(baseText, "+dirty") {
		t.Fatalf("earlier baseline should show both:\n%s", baseText)
	}
}

func TestBuildGitResultLogKind(t *testing.T) {
	repo := newTestRepo(t)
	s := &Session{AllowedRoots: []string{repo}, NoWorktree: true}
	req := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Log, MaxCommits: 10}
	req.SetRepoPath([]byte(repo))
	res := s.buildGitResult(context.Background(), req)
	if res.Status != protocol.GitRunStatus_Ok {
		t.Fatalf("status = %v stderr %q", res.Status, res.Stderr)
	}
	lg := res.Log()
	if lg == nil || lg.Count != 1 {
		t.Fatalf("log body: %+v", lg)
	}
	if string(lg.Commits[0].Subject) != "first" || string(lg.Commits[0].Author) != "tester" {
		t.Fatalf("commit: %+v", lg.Commits[0])
	}
}

// A finished task's worktree is gone, but its branch is retained; the log must
// still answer through it.
func TestBuildGitResultAfterWorktreeRemoved(t *testing.T) {
	repo := newTestRepo(t)
	taskID, taskIDHex := newTaskID(0xca)
	wt := filepath.Join(repo, ".harness-worktrees", taskIDHex)
	gitRun(t, repo, "worktree", "add", "-q", "-b", "harness/"+taskIDHex, wt)
	if err := os.WriteFile(filepath.Join(wt, "b.txt"), []byte("agent work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, wt, "add", "b.txt")
	gitRun(t, wt, "commit", "-q", "-m", "agent commit")
	gitRun(t, repo, "worktree", "remove", "--force", wt)

	s := &Session{AllowedRoots: []string{repo}}
	req := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Log, MaxCommits: 10}
	req.TaskId = taskID
	req.SetRepoPath([]byte(repo))
	res := s.buildGitResult(context.Background(), req)
	if res.Status != protocol.GitRunStatus_Ok {
		t.Fatalf("status = %v stderr %q", res.Status, res.Stderr)
	}
	lg := res.Log()
	if lg == nil || lg.Count != 2 {
		t.Fatalf("log body: %+v", lg)
	}
	if string(lg.Commits[0].Subject) != "agent commit" {
		t.Fatalf("newest commit: %+v", lg.Commits[0])
	}
}

func TestBuildGitResultNoSource(t *testing.T) {
	repo := newTestRepo(t)
	s := &Session{AllowedRoots: []string{repo}}
	taskID, _ := newTaskID(0xca)
	req := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Diff, Target: protocol.GitDiffTarget_Worktree}
	req.TaskId = taskID
	req.SetRepoPath([]byte(repo))
	res := s.buildGitResult(context.Background(), req)
	if res.Status != protocol.GitRunStatus_NoSource {
		t.Fatalf("status = %v, want no_source", res.Status)
	}
}

func TestBuildGitResultBadRevIsReported(t *testing.T) {
	repo := newTestRepo(t)
	s := &Session{AllowedRoots: []string{repo}, NoWorktree: true}
	req := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Diff, Target: protocol.GitDiffTarget_Worktree}
	req.SetBaseRev([]byte("--ext-diff"))
	req.SetRepoPath([]byte(repo))
	res := s.buildGitResult(context.Background(), req)
	if res.Status != protocol.GitRunStatus_BadRev {
		t.Fatalf("status = %v, want bad_rev", res.Status)
	}
	if len(res.Stderr) == 0 {
		t.Fatal("a refusal must say why")
	}
}

func gitHead(t *testing.T, dir string) string {
	t.Helper()
	req := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Log, MaxCommits: 1}
	argv, _ := gitArgv(req, "HEAD", true)
	out, stderr, _, st := runGitQuery(context.Background(), dir, argv, 1<<20)
	if st != protocol.GitRunStatus_Ok {
		t.Fatalf("gitHead: %v %s", st, stderr)
	}
	commits, _ := parseGitLog(out, 1)
	if len(commits) == 0 {
		t.Fatal("gitHead: no commits")
	}
	return string(commits[0].Sha)
}

// newTaskID returns a TaskID together with the hex string the runner derives
// from it. The two must be taken from the same place: a TaskID is 16 bytes, so
// its hex form is 32 characters, and hand-writing a short one would name a
// branch that gitSourceFor never looks for.
func newTaskID(seed byte) (protocol.TaskID, string) {
	var id protocol.TaskID
	for i := range id.Id {
		id.Id[i] = seed + byte(i)
	}
	return id, hex.EncodeToString(id.Id[:])
}

// Running `git status` in the shared repo for a task that has no working tree
// reports the REPO's state — other tasks' worktree directories show up as
// untracked entries. That is a different task's answer wearing this one's
// label, so the honest answer is an empty listing.
func TestBuildGitResultStatusIsEmptyWithoutWorktree(t *testing.T) {
	repo := newTestRepo(t)
	taskID, taskIDHex := newTaskID(0xbe)
	gitRun(t, repo, "branch", "harness/"+taskIDHex)
	// Noise in the shared repo that must NOT be reported as this task's.
	if err := os.MkdirAll(filepath.Join(repo, ".harness-worktrees", "someothertask"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "unrelated.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Session{AllowedRoots: []string{repo}}
	req := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Status}
	req.TaskId = taskID
	req.SetRepoPath([]byte(repo))
	res := s.buildGitResult(context.Background(), req)
	if res.Status != protocol.GitRunStatus_Ok {
		t.Fatalf("status = %v stderr %q", res.Status, res.Stderr)
	}
	body := res.StatusBody()
	if body == nil {
		t.Fatal("status body missing")
	}
	if body.Count != 0 {
		t.Fatalf("a task with no working tree reported %d dirty paths: %+v", body.Count, body.Entries)
	}
}

// HEAD must mean the TASK's tip once the worktree is gone, not the shared
// repo's checkout — otherwise `diff HEAD~1` errors and `diff HEAD` silently
// answers about whatever branch the user has out.
func TestBuildGitResultHeadMeansTheTaskTipWithoutWorktree(t *testing.T) {
	repo := newTestRepo(t)
	taskID, taskIDHex := newTaskID(0xde)
	wt := filepath.Join(repo, ".harness-worktrees", taskIDHex)
	gitRun(t, repo, "worktree", "add", "-q", "-b", "harness/"+taskIDHex, wt)
	if err := os.WriteFile(filepath.Join(wt, "only-in-task.txt"), []byte("agent work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, wt, "add", "only-in-task.txt")
	gitRun(t, wt, "commit", "-q", "-m", "agent commit")
	gitRun(t, repo, "worktree", "remove", "--force", wt)

	s := &Session{AllowedRoots: []string{repo}}
	req := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Diff, Target: protocol.GitDiffTarget_Worktree}
	req.TaskId = taskID
	req.SetBaseRev([]byte("HEAD~1"))
	req.SetRepoPath([]byte(repo))
	res := s.buildGitResult(context.Background(), req)
	if res.Status != protocol.GitRunStatus_Ok {
		t.Fatalf("status = %v stderr %q — HEAD~1 resolved against the shared repo", res.Status, res.Stderr)
	}
	if text := string(res.Diff().Text); !strings.Contains(text, "only-in-task.txt") {
		t.Fatalf("diff does not show the task's own commit:\n%s", text)
	}
}

// The whole point of re-rooting: a plain nested repo's own change is
// unreachable from outside it, and reachable once the query runs inside it.
func TestBuildGitResultSubrepoSeesWhatTheOuterRepoCannot(t *testing.T) {
	outer := newNestedFixture(t)
	s := &Session{AllowedRoots: []string{outer}, NoWorktree: true}

	// From the outer repo the nested repo is one untracked entry and nothing more.
	outerReq := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Diff, Target: protocol.GitDiffTarget_Worktree}
	outerReq.SetBaseRev([]byte("HEAD"))
	outerReq.SetRepoPath([]byte(outer))
	outerRes := s.buildGitResult(context.Background(), outerReq)
	if outerRes.Status != protocol.GitRunStatus_Ok {
		t.Fatalf("outer status = %v stderr %q", outerRes.Status, outerRes.Stderr)
	}
	if strings.Contains(string(outerRes.Diff().Text), "i2") {
		t.Fatalf("outer diff should not see inside a nested repo:\n%s", outerRes.Diff().Text)
	}

	// Re-rooted, the same change is right there.
	inReq := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Diff, Target: protocol.GitDiffTarget_Worktree}
	inReq.SetBaseRev([]byte("HEAD"))
	inReq.SetRepoPath([]byte(outer))
	inReq.SetSubrepo([]byte("pkg/inner"))
	inRes := s.buildGitResult(context.Background(), inReq)
	if inRes.Status != protocol.GitRunStatus_Ok {
		t.Fatalf("inner status = %v stderr %q", inRes.Status, inRes.Stderr)
	}
	if !strings.Contains(string(inRes.Diff().Text), "+i2") {
		t.Fatalf("re-rooted diff missing the nested change:\n%s", inRes.Diff().Text)
	}
}

// A re-rooted log is the nested repo's history, not the outer one's.
func TestBuildGitResultSubrepoLogIsTheInnerHistory(t *testing.T) {
	outer := newNestedFixture(t)
	s := &Session{AllowedRoots: []string{outer}, NoWorktree: true}
	req := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Log, MaxCommits: 10}
	req.SetRepoPath([]byte(outer))
	req.SetSubrepo([]byte("pkg/inner"))
	res := s.buildGitResult(context.Background(), req)
	if res.Status != protocol.GitRunStatus_Ok {
		t.Fatalf("status = %v stderr %q", res.Status, res.Stderr)
	}
	lg := res.Log()
	if lg == nil || lg.Count != 1 {
		t.Fatalf("log body: %+v", lg)
	}
	if string(lg.Commits[0].Subject) != "inner base" {
		t.Fatalf("subject %q — that is the outer repo's history", lg.Commits[0].Subject)
	}
}

func TestBuildGitResultSubreposKind(t *testing.T) {
	outer := newNestedFixture(t)
	s := &Session{AllowedRoots: []string{outer}, NoWorktree: true}
	req := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Subrepos}
	req.SetRepoPath([]byte(outer))
	res := s.buildGitResult(context.Background(), req)
	if res.Status != protocol.GitRunStatus_Ok {
		t.Fatalf("status = %v stderr %q", res.Status, res.Stderr)
	}
	body := res.Subrepos()
	if body == nil || body.Count != 2 {
		t.Fatalf("subrepos body: %+v", body)
	}
}

func TestBuildGitResultSubrepoInvalidIsReported(t *testing.T) {
	outer := newNestedFixture(t)
	s := &Session{AllowedRoots: []string{outer}, NoWorktree: true}
	req := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Status}
	req.SetRepoPath([]byte(outer))
	req.SetSubrepo([]byte("pkg")) // a real directory, but not a repo root
	res := s.buildGitResult(context.Background(), req)
	if res.Status != protocol.GitRunStatus_SubrepoInvalid {
		t.Fatalf("status = %v, want subrepo_invalid (it must not answer about the outer repo)", res.Status)
	}
	if len(res.Stderr) == 0 {
		t.Fatal("a refusal must say why")
	}
}

// A nested repo lives inside the worktree, so once the worktree is gone it is
// gone too — the harness/<taskID> branch says nothing about it.
func TestBuildGitResultSubrepoNeedsAWorktree(t *testing.T) {
	repo := newTestRepo(t)
	taskID, taskIDHex := newTaskID(0xaa)
	gitRun(t, repo, "branch", "harness/"+taskIDHex)

	s := &Session{AllowedRoots: []string{repo}}
	req := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Log}
	req.TaskId = taskID
	req.SetRepoPath([]byte(repo))
	req.SetSubrepo([]byte("pkg/inner"))
	res := s.buildGitResult(context.Background(), req)
	if res.Status != protocol.GitRunStatus_NoSource {
		t.Fatalf("status = %v, want no_source", res.Status)
	}
}

// --submodule is what makes the submodule's own file-level change visible from
// the superproject; without it only the gitlink moves.
func TestBuildGitResultSubmoduleDiffInlinesTheInnerChange(t *testing.T) {
	outer := newNestedFixture(t)
	s := &Session{AllowedRoots: []string{outer}, NoWorktree: true}

	mk := func(submodule bool) string {
		req := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Diff, Target: protocol.GitDiffTarget_Worktree}
		req.SetBaseRev([]byte("HEAD"))
		req.SetRepoPath([]byte(outer))
		req.SetSubmoduleDiff(submodule)
		res := s.buildGitResult(context.Background(), req)
		if res.Status != protocol.GitRunStatus_Ok {
			t.Fatalf("status = %v stderr %q", res.Status, res.Stderr)
		}
		return string(res.Diff().Text)
	}

	if got := mk(false); strings.Contains(got, "+s2") {
		t.Fatalf("default should show the gitlink only:\n%s", got)
	}
	if got := mk(true); !strings.Contains(got, "+s2") {
		t.Fatalf("--submodule should inline the inner change:\n%s", got)
	}
}

// countGitSpawns runs fn with a counting `git` shim ahead of the real one on
// PATH and reports how many times git was executed.
//
// Spawn count is the thing that actually hurts: on the Windows runner in use
// here a process start costs ~300ms (measured — every query took ~900ms and
// each ran three of them), so a probe that is "just one exec" is most of the
// latency. Counting it is the only way to keep that from creeping back in.
func countGitSpawns(t *testing.T, fn func()) int {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the counting shim is a shell script")
	}
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("no git on PATH: %v", err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "count.log")
	shim := "#!/bin/sh\necho x >> " + log + "\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	fn()
	b, err := os.ReadFile(log)
	if err != nil {
		return 0
	}
	return strings.Count(string(b), "\n")
}

// One query, one git. Locating the working directory used to cost two more
// spawns before the real command even started.
func TestGitQuerySpawnsGitExactlyOnce(t *testing.T) {
	repo := newTestRepo(t) // built before the shim goes on PATH
	s := &Session{AllowedRoots: []string{repo}, NoWorktree: true}

	for _, kind := range []protocol.GitQueryKind{
		protocol.GitQueryKind_Log,
		protocol.GitQueryKind_Status,
		protocol.GitQueryKind_Diff,
		protocol.GitQueryKind_Show,
	} {
		t.Run(kind.String(), func(t *testing.T) {
			n := countGitSpawns(t, func() {
				req := &protocol.RunnerGitQueryRequest{Kind: kind, Target: protocol.GitDiffTarget_Worktree}
				req.SetBaseRev([]byte("HEAD"))
				req.SetRepoPath([]byte(repo))
				res := s.buildGitResult(context.Background(), req)
				if res.Status != protocol.GitRunStatus_Ok {
					t.Fatalf("status = %v stderr %q", res.Status, res.Stderr)
				}
			})
			if n != 1 {
				t.Fatalf("%v spawned git %d times, want exactly 1", kind, n)
			}
		})
	}
}

// The discovery walk answers from the filesystem: it must not start git at all,
// not even once per candidate directory.
func TestSubreposSpawnsNoGit(t *testing.T) {
	outer := newNestedFixture(t)
	s := &Session{AllowedRoots: []string{outer}, NoWorktree: true}

	var count int
	n := countGitSpawns(t, func() {
		req := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Subrepos}
		req.SetRepoPath([]byte(outer))
		res := s.buildGitResult(context.Background(), req)
		if res.Status != protocol.GitRunStatus_Ok {
			t.Fatalf("status = %v stderr %q", res.Status, res.Stderr)
		}
		count = int(res.Subrepos().Count)
	})
	if count != 2 {
		t.Fatalf("found %d nested repos, want 2 — the walk must still work without git", count)
	}
	if n != 0 {
		t.Fatalf("the subrepos walk spawned git %d times; it answers from the filesystem", n)
	}
}

// Re-rooting is filesystem work too.
func TestSubrepoResolutionSpawnsNoExtraGit(t *testing.T) {
	outer := newNestedFixture(t)
	s := &Session{AllowedRoots: []string{outer}, NoWorktree: true}

	n := countGitSpawns(t, func() {
		req := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Log, MaxCommits: 5}
		req.SetRepoPath([]byte(outer))
		req.SetSubrepo([]byte("pkg/inner"))
		res := s.buildGitResult(context.Background(), req)
		if res.Status != protocol.GitRunStatus_Ok {
			t.Fatalf("status = %v stderr %q", res.Status, res.Stderr)
		}
	})
	if n != 1 {
		t.Fatalf("a re-rooted log spawned git %d times, want exactly 1", n)
	}
}
