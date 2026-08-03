package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// gitRun runs git in dir with a deterministic identity, failing the test on
// error. Tests that build fixture repos share it.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=tester", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=tester", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// newTestRepo builds a git repo with one commit and returns its path.
func newTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "a.txt")
	gitRun(t, dir, "commit", "-q", "-m", "first")
	return dir
}

func TestClampMaxCommits(t *testing.T) {
	if got := clampMaxCommits(0); got != 100 {
		t.Fatalf("0 -> %d, want 100", got)
	}
	if got := clampMaxCommits(5); got != 5 {
		t.Fatalf("5 -> %d", got)
	}
	if got := clampMaxCommits(99999); got != 1000 {
		t.Fatalf("99999 -> %d, want 1000", got)
	}
}

func TestClampMaxBytes(t *testing.T) {
	if got := clampMaxBytes(0); got != 2097152 {
		t.Fatalf("0 -> %d", got)
	}
	if got := clampMaxBytes(1 << 30); got != 8388608 {
		t.Fatalf("1GiB -> %d", got)
	}
}

func TestGitArgvRejectsDashLeadingRev(t *testing.T) {
	req := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Diff, Target: protocol.GitDiffTarget_Worktree}
	req.SetBaseRev([]byte("--ext-diff"))
	if _, st := gitArgv(req, "HEAD", true); st != protocol.GitRunStatus_BadRev {
		t.Fatalf("status = %v, want bad_rev", st)
	}
}

func TestGitArgvRejectsDashLeadingPath(t *testing.T) {
	req := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Diff, Target: protocol.GitDiffTarget_Worktree}
	req.SetPath([]byte("--output=/tmp/x"))
	if _, st := gitArgv(req, "HEAD", true); st != protocol.GitRunStatus_BadRev {
		t.Fatalf("status = %v, want bad_rev", st)
	}
}

func TestGitArgvRevTargetNeedsBase(t *testing.T) {
	req := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Diff, Target: protocol.GitDiffTarget_Rev}
	req.SetTargetRev([]byte("abc123"))
	if _, st := gitArgv(req, "HEAD", true); st != protocol.GitRunStatus_BadRev {
		t.Fatalf("status = %v, want bad_rev for empty base with target=rev", st)
	}
}

func TestGitArgvDiffAlwaysDisablesExternalDrivers(t *testing.T) {
	req := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Diff, Target: protocol.GitDiffTarget_Worktree}
	req.SetBaseRev([]byte("HEAD"))
	argv, st := gitArgv(req, "HEAD", true)
	if st != protocol.GitRunStatus_Ok {
		t.Fatalf("status = %v", st)
	}
	joined := strings.Join(argv, " ")
	for _, want := range []string{"--no-color", "--no-ext-diff", "--no-textconv"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv %q missing %s", joined, want)
		}
	}
	// The pathspec separator is present even with an empty pathspec, so a rev
	// that happens to look like a path can never be reinterpreted.
	if argv[len(argv)-1] != "--" {
		t.Fatalf("argv %q does not end with the -- separator", joined)
	}
}

func TestGitArgvShowDisablesExternalDrivers(t *testing.T) {
	req := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Show}
	req.SetBaseRev([]byte("abc"))
	argv, st := gitArgv(req, "HEAD", true)
	if st != protocol.GitRunStatus_Ok {
		t.Fatalf("status = %v", st)
	}
	joined := strings.Join(argv, " ")
	for _, want := range []string{"--no-ext-diff", "--no-textconv"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("show argv %q missing %s", joined, want)
		}
	}
}

func TestGitArgvIndexTargetUsesCached(t *testing.T) {
	req := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Diff, Target: protocol.GitDiffTarget_Index}
	req.SetBaseRev([]byte("HEAD"))
	argv, st := gitArgv(req, "HEAD", true)
	if st != protocol.GitRunStatus_Ok {
		t.Fatalf("status = %v", st)
	}
	if !strings.Contains(strings.Join(argv, " "), "--cached") {
		t.Fatalf("argv %v missing --cached", argv)
	}
}

func TestGitArgvPathspecGoesAfterSeparator(t *testing.T) {
	req := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Diff, Target: protocol.GitDiffTarget_Worktree}
	req.SetPath([]byte("tui/app.go"))
	argv, st := gitArgv(req, "HEAD", true)
	if st != protocol.GitRunStatus_Ok {
		t.Fatalf("status = %v", st)
	}
	if len(argv) < 2 || argv[len(argv)-2] != "--" || argv[len(argv)-1] != "tui/app.go" {
		t.Fatalf("argv tail = %v", argv[max(0, len(argv)-3):])
	}
}

func TestGitArgvLogFallsBackToTip(t *testing.T) {
	req := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Log, MaxCommits: 5}
	argv, st := gitArgv(req, "refs/heads/harness/deadbeef", true)
	if st != protocol.GitRunStatus_Ok {
		t.Fatalf("status = %v", st)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "refs/heads/harness/deadbeef") {
		t.Fatalf("argv %q does not name the tip", joined)
	}
	// max_count is asked one higher than requested so truncation is detectable.
	if !strings.Contains(joined, "--max-count=6") {
		t.Fatalf("argv %q should over-fetch by one", joined)
	}
}

func TestParseGitLog(t *testing.T) {
	out := []byte("aaa\x00alice\x001700000000\x00first subject\n" +
		"bbb\x00bob\x001700000100\x00second subject\n")
	commits, truncated := parseGitLog(out, 5)
	if truncated {
		t.Fatal("should not be truncated")
	}
	if len(commits) != 2 {
		t.Fatalf("got %d commits", len(commits))
	}
	if string(commits[0].Sha) != "aaa" || string(commits[0].Author) != "alice" {
		t.Fatalf("commit 0: %+v", commits[0])
	}
	if commits[1].When != 1700000100 || string(commits[1].Subject) != "second subject" {
		t.Fatalf("commit 1: %+v", commits[1])
	}
}

func TestParseGitLogTruncates(t *testing.T) {
	out := []byte("aaa\x00a\x001\x00s1\nbbb\x00b\x002\x00s2\nccc\x00c\x003\x00s3\n")
	commits, truncated := parseGitLog(out, 2)
	if !truncated {
		t.Fatal("want truncated")
	}
	if len(commits) != 2 {
		t.Fatalf("got %d commits, want 2", len(commits))
	}
}

// A record carrying more fields than expected must not shift the subject.
func TestParseGitLogIgnoresExtraFields(t *testing.T) {
	out := []byte("aaa\x00a\x001\x00sub\x00ject\n")
	commits, _ := parseGitLog(out, 5)
	if len(commits) != 1 || string(commits[0].Subject) != "sub" {
		t.Fatalf("commits: %+v", commits)
	}
}

func TestParseGitStatusZ(t *testing.T) {
	// " M a.txt", "?? new.txt", and a rename which carries a second record.
	out := []byte(" M a.txt\x00?? new.txt\x00R  dst.txt\x00src.txt\x00")
	entries := parseGitStatusZ(out)
	if len(entries) != 3 {
		t.Fatalf("got %d entries: %+v", len(entries), entries)
	}
	if entries[0].Xy != [2]byte{' ', 'M'} || string(entries[0].Path) != "a.txt" {
		t.Fatalf("entry 0: %+v", entries[0])
	}
	if entries[1].Xy != [2]byte{'?', '?'} || string(entries[1].Path) != "new.txt" {
		t.Fatalf("entry 1: %+v", entries[1])
	}
	// The rename's source record must be consumed, not parsed as its own entry.
	if entries[2].Xy != [2]byte{'R', ' '} || string(entries[2].Path) != "dst.txt" {
		t.Fatalf("entry 2: %+v", entries[2])
	}
}

func TestGitSourceForPrefersWorktree(t *testing.T) {
	repo := newTestRepo(t)
	wt := filepath.Join(repo, ".harness-worktrees", "cafe1234")
	gitRun(t, repo, "worktree", "add", "-q", "-b", "harness/cafe1234", wt)
	cwd, tip, _, st := gitSourceFor(repo, "cafe1234", false)
	if st != protocol.GitRunStatus_Ok {
		t.Fatalf("status = %v", st)
	}
	if cwd != wt {
		t.Fatalf("cwd = %q, want %q", cwd, wt)
	}
	if tip != "HEAD" {
		t.Fatalf("tip = %q, want HEAD", tip)
	}
}

// A directory left behind by a crashed cleanup is not a worktree. Running git
// inside it would silently answer about the parent repo scoped to that
// subdirectory, so it must not be mistaken for the task's checkout.
func TestGitSourceForRejectsOrphanDirectory(t *testing.T) {
	repo := newTestRepo(t)
	orphan := filepath.Join(repo, ".harness-worktrees", "cafe1234")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	// No branch either, so the only honest answer is no_source.
	if _, _, _, st := gitSourceFor(repo, "cafe1234", false); st != protocol.GitRunStatus_NoSource {
		t.Fatalf("status = %v, want no_source", st)
	}
}

// With the worktree gone but the branch retained, the orphan directory must
// not shadow the branch fallback.
func TestGitSourceForOrphanDirectoryFallsBackToBranch(t *testing.T) {
	repo := newTestRepo(t)
	orphan := filepath.Join(repo, ".harness-worktrees", "cafe1234")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "branch", "harness/cafe1234")
	cwd, tip, _, st := gitSourceFor(repo, "cafe1234", false)
	if st != protocol.GitRunStatus_Ok || cwd != repo || tip != "refs/heads/harness/cafe1234" {
		t.Fatalf("cwd=%q tip=%q st=%v", cwd, tip, st)
	}
}

func TestGitSourceForFallsBackToBranchRef(t *testing.T) {
	repo := newTestRepo(t)
	gitRun(t, repo, "branch", "harness/cafe1234")
	cwd, tip, _, st := gitSourceFor(repo, "cafe1234", false)
	if st != protocol.GitRunStatus_Ok {
		t.Fatalf("status = %v", st)
	}
	if cwd != repo {
		t.Fatalf("cwd = %q, want repo %q", cwd, repo)
	}
	if tip != "refs/heads/harness/cafe1234" {
		t.Fatalf("tip = %q", tip)
	}
}

func TestGitSourceForNoWorktreeAndNoBranch(t *testing.T) {
	repo := newTestRepo(t)
	if _, _, _, st := gitSourceFor(repo, "cafe1234", false); st != protocol.GitRunStatus_NoSource {
		t.Fatalf("status = %v, want no_source", st)
	}
}

func TestGitSourceForNoWorktreeMode(t *testing.T) {
	repo := newTestRepo(t)
	cwd, tip, _, st := gitSourceFor(repo, "cafe1234", true)
	if st != protocol.GitRunStatus_Ok || cwd != repo || tip != "HEAD" {
		t.Fatalf("cwd=%q tip=%q st=%v", cwd, tip, st)
	}
}

func TestGitSourceForNotAGitRepo(t *testing.T) {
	dir := t.TempDir()
	if _, _, _, st := gitSourceFor(dir, "cafe1234", true); st != protocol.GitRunStatus_NotAGitRepo {
		t.Fatalf("status = %v, want not_a_git_repo", st)
	}
}

func TestRebaseHeadOnTip(t *testing.T) {
	const tip = "refs/heads/harness/cafe"
	// With a working tree, HEAD already means the task's tip — leave it alone.
	if got := rebaseHeadOnTip("HEAD", tip, true); got != "HEAD" {
		t.Fatalf("live worktree: %q", got)
	}
	// Without one, HEAD would resolve against the SHARED repo's checkout.
	if got := rebaseHeadOnTip("HEAD", tip, false); got != tip {
		t.Fatalf("HEAD -> %q, want the tip", got)
	}
	if got := rebaseHeadOnTip("HEAD~2", tip, false); got != tip+"~2" {
		t.Fatalf("HEAD~2 -> %q", got)
	}
	if got := rebaseHeadOnTip("HEAD^", tip, false); got != tip+"^" {
		t.Fatalf("HEAD^ -> %q", got)
	}
	// A sha is already absolute, and a branch named HEADLESS is not HEAD.
	if got := rebaseHeadOnTip("abc123", tip, false); got != "abc123" {
		t.Fatalf("sha -> %q", got)
	}
	if got := rebaseHeadOnTip("HEADLESS", tip, false); got != "HEADLESS" {
		t.Fatalf("HEADLESS -> %q; only HEAD and HEAD<suffix> are the ref", got)
	}
	if got := rebaseHeadOnTip("", tip, false); got != "" {
		t.Fatalf("empty -> %q", got)
	}
}

// With the task's working tree gone there is no working tree to diff against,
// so the tip stands in for it. Without this the argv would ask git about the
// SHARED repo's checkout, which belongs to whoever has it out.
func TestGitArgvWithoutWorktreeDiffsAgainstTheTip(t *testing.T) {
	const tip = "refs/heads/harness/cafe"
	req := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Diff, Target: protocol.GitDiffTarget_Worktree}
	req.SetBaseRev([]byte("abc123"))
	argv, st := gitArgv(req, tip, false)
	if st != protocol.GitRunStatus_Ok {
		t.Fatalf("status = %v", st)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "abc123 "+tip) {
		t.Fatalf("argv %q should diff base against the tip", joined)
	}
	if argv[len(argv)-1] != "--" {
		t.Fatalf("argv %q lost the pathspec separator", joined)
	}
}

// An empty base with no working tree yields tip..tip — nothing uncommitted,
// which is the truth. Inventing tip^ instead would break on a root commit.
func TestGitArgvWithoutWorktreeEmptyBaseIsEmptyDiff(t *testing.T) {
	const tip = "refs/heads/harness/cafe"
	req := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Diff, Target: protocol.GitDiffTarget_Worktree}
	argv, st := gitArgv(req, tip, false)
	if st != protocol.GitRunStatus_Ok {
		t.Fatalf("status = %v", st)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, tip+" "+tip) {
		t.Fatalf("argv %q should be tip..tip", joined)
	}
	if strings.Contains(joined, "^") {
		t.Fatalf("argv %q invented a parent revision", joined)
	}
}

// The index does not survive its working tree either.
func TestGitArgvWithoutWorktreeIndexTargetAlsoUsesTheTip(t *testing.T) {
	const tip = "refs/heads/harness/cafe"
	req := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Diff, Target: protocol.GitDiffTarget_Index}
	req.SetBaseRev([]byte("abc123"))
	argv, st := gitArgv(req, tip, false)
	if st != protocol.GitRunStatus_Ok {
		t.Fatalf("status = %v", st)
	}
	if joined := strings.Join(argv, " "); strings.Contains(joined, "--cached") {
		t.Fatalf("argv %q asks about an index that no longer exists", joined)
	}
}

// Two explicit revisions are commits on both sides, so the fallback must not
// rewrite them.
func TestGitArgvWithoutWorktreeRevTargetIsUntouched(t *testing.T) {
	const tip = "refs/heads/harness/cafe"
	req := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Diff, Target: protocol.GitDiffTarget_Rev}
	req.SetBaseRev([]byte("aaa"))
	req.SetTargetRev([]byte("bbb"))
	argv, st := gitArgv(req, tip, false)
	if st != protocol.GitRunStatus_Ok {
		t.Fatalf("status = %v", st)
	}
	if joined := strings.Join(argv, " "); !strings.Contains(joined, "aaa bbb") {
		t.Fatalf("argv %q", joined)
	}
}

// newNestedFixture builds an outer repo containing BOTH shapes: a plain nested
// repo (its own .git, untracked by the outer repo — invisible to it) and a
// submodule (a gitlink the outer repo tracks). Returns the outer repo path.
func newNestedFixture(t *testing.T) string {
	t.Helper()
	base := t.TempDir()

	// The submodule's upstream has to exist before it can be added.
	subUp := filepath.Join(base, "subupstream")
	if err := os.MkdirAll(subUp, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, subUp, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(subUp, "s.txt"), []byte("s1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, subUp, "add", "s.txt")
	gitRun(t, subUp, "commit", "-q", "-m", "sub base")

	outer := filepath.Join(base, "outer")
	if err := os.MkdirAll(outer, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, outer, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(outer, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, outer, "add", "a.txt")
	gitRun(t, outer, "commit", "-q", "-m", "outer base")

	// Plain nested repo, two levels down so the walk has to descend.
	inner := filepath.Join(outer, "pkg", "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, inner, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(inner, "i.txt"), []byte("i1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, inner, "add", "i.txt")
	gitRun(t, inner, "commit", "-q", "-m", "inner base")
	// An uncommitted change that ONLY the inner repo can see.
	if err := os.WriteFile(filepath.Join(inner, "i.txt"), []byte("i1\ni2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	gitRun(t, outer, "-c", "protocol.file.allow=always", "submodule", "add", "-q", subUp, "sub")
	gitRun(t, outer, "commit", "-q", "-m", "add submodule")
	if err := os.WriteFile(filepath.Join(outer, "sub", "s.txt"), []byte("s1\ns2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	return outer
}

func TestFindSubreposFindsBothShapes(t *testing.T) {
	outer := newNestedFixture(t)
	entries, truncated := findSubrepos(outer)
	if truncated {
		t.Fatal("two repos should not trip the cap")
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[string(e.Path)] = true
	}
	// The plain nested repo is the one the outer repo cannot see any other way.
	if !got["pkg/inner"] {
		t.Fatalf("plain nested repo missing from %v", got)
	}
	// A submodule is a repo root too, so one picker covers both cases.
	if !got["sub"] {
		t.Fatalf("submodule missing from %v", got)
	}
}

// A repo we have found is not walked into: what is inside it is that repo's
// business, reached by re-rooting there first.
func TestFindSubreposDoesNotDescendIntoAFoundRepo(t *testing.T) {
	outer := newNestedFixture(t)
	deeper := filepath.Join(outer, "pkg", "inner", "deeper")
	if err := os.MkdirAll(deeper, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, deeper, "init", "-q", "-b", "main")

	entries, _ := findSubrepos(outer)
	for _, e := range entries {
		if strings.Contains(string(e.Path), "deeper") {
			t.Fatalf("walk descended into a found repo: %q", e.Path)
		}
	}
}

func TestFindSubreposSkipsHarnessWorktrees(t *testing.T) {
	outer := newNestedFixture(t)
	wt := filepath.Join(outer, ".harness-worktrees", "deadbeef")
	gitRun(t, outer, "worktree", "add", "-q", "-b", "harness/deadbeef", wt)
	entries, _ := findSubrepos(outer)
	for _, e := range entries {
		if strings.HasPrefix(string(e.Path), ".harness-worktrees") {
			t.Fatalf("another task's worktree reported as this task's content: %q", e.Path)
		}
	}
}

func TestResolveSubrepoAcceptsAPlainNestedRepo(t *testing.T) {
	outer := newNestedFixture(t)
	got, st := resolveSubrepo(outer, "pkg/inner")
	if st != protocol.GitRunStatus_Ok {
		t.Fatalf("status = %v", st)
	}
	if got != filepath.Join(outer, "pkg", "inner") {
		t.Fatalf("cwd = %q", got)
	}
}

func TestResolveSubrepoEmptyIsTheRoot(t *testing.T) {
	outer := newNestedFixture(t)
	got, st := resolveSubrepo(outer, "")
	if st != protocol.GitRunStatus_Ok || got != outer {
		t.Fatalf("got %q %v", got, st)
	}
}

// Every rejection is subrepo_invalid, never a silent fallback to the enclosing
// repository.
func TestResolveSubrepoRejections(t *testing.T) {
	outer := newNestedFixture(t)
	if err := os.MkdirAll(filepath.Join(outer, "plaindir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outer, "pkg", "inner"), filepath.Join(outer, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	for _, rel := range []string{
		"../outside", // escapes the worktree
		"/etc",       // absolute
		"nosuchdir",  // missing
		"plaindir",   // exists but is not a repo root
		"a.txt",      // a file, not a directory
		"link",       // reaches a repo, but through a symlink
	} {
		if _, st := resolveSubrepo(outer, rel); st != protocol.GitRunStatus_SubrepoInvalid {
			t.Errorf("resolveSubrepo(%q) = %v, want subrepo_invalid", rel, st)
		}
	}
}

func TestGitArgvSubmoduleDiffOptIn(t *testing.T) {
	req := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Diff, Target: protocol.GitDiffTarget_Worktree}
	req.SetBaseRev([]byte("HEAD"))

	argv, _ := gitArgv(req, "HEAD", true)
	if strings.Contains(strings.Join(argv, " "), "--submodule") {
		t.Fatalf("off by default, but argv has it: %v", argv)
	}

	req.SetSubmoduleDiff(true)
	argv, _ = gitArgv(req, "HEAD", true)
	if !strings.Contains(strings.Join(argv, " "), "--submodule=diff") {
		t.Fatalf("opted in, but argv lacks it: %v", argv)
	}
}

func TestGitArgvShowHonoursSubmoduleDiff(t *testing.T) {
	req := &protocol.RunnerGitQueryRequest{Kind: protocol.GitQueryKind_Show}
	req.SetBaseRev([]byte("abc"))
	req.SetSubmoduleDiff(true)
	argv, st := gitArgv(req, "HEAD", true)
	if st != protocol.GitRunStatus_Ok {
		t.Fatalf("status = %v", st)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--submodule=diff") {
		t.Fatalf("argv %q", joined)
	}
	// The revision must still land after the flags, not before them.
	if !strings.Contains(joined, "--submodule=diff abc") {
		t.Fatalf("revision misplaced in %q", joined)
	}
}

// A `git worktree add` checkout stores its .git as a FILE, not a directory.
// The root test has to accept both — an os.Stat().IsDir() check would reject
// every task worktree the harness creates.
func TestIsWorktreeRootAcceptsAGitFile(t *testing.T) {
	repo := newTestRepo(t)
	wt := filepath.Join(repo, "wt")
	gitRun(t, repo, "worktree", "add", "-q", "-b", "wtbranch", wt)

	st, err := os.Lstat(filepath.Join(wt, ".git"))
	if err != nil {
		t.Fatalf("worktree has no .git entry: %v", err)
	}
	if st.IsDir() {
		t.Skip("this git stores a worktree's .git as a directory; the file case is what needs pinning")
	}
	if !isWorktreeRoot(wt) {
		t.Fatal("a worktree whose .git is a file was not recognised as a root")
	}
}

func TestIsWorktreeRootRejectsAPlainDirectory(t *testing.T) {
	repo := newTestRepo(t)
	plain := filepath.Join(repo, "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	// It sits INSIDE a repository, so git run there would happily answer about
	// the enclosing one. That is the answer this must not produce.
	if isWorktreeRoot(plain) {
		t.Fatal("a plain directory inside a repo was taken for a repo root")
	}
}

func TestIsWorktreeRootRejectsAFile(t *testing.T) {
	repo := newTestRepo(t)
	if isWorktreeRoot(filepath.Join(repo, "a.txt")) {
		t.Fatal("a regular file was taken for a repo root")
	}
}

// isGitRepo settles the common case without spawning a process; the fallback
// still has to answer for a directory whose .git lives further up.
func TestIsGitRepoAcceptsASubdirectory(t *testing.T) {
	repo := newTestRepo(t)
	sub := filepath.Join(repo, "deep", "er")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if !isGitRepo(sub) {
		t.Fatal("a subdirectory of a repository is inside a working tree")
	}
}

func TestIsGitRepoRejectsANonRepo(t *testing.T) {
	if isGitRepo(t.TempDir()) {
		t.Fatal("a bare temp dir is not a repository")
	}
}
