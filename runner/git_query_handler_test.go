package runner

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
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
