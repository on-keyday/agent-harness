package protocol

import (
	"testing"
)

func TestGitQueryRequestRoundTrip(t *testing.T) {
	var req GitQueryRequest
	req.TaskId = TaskID{Id: [16]byte{1, 2, 3}}
	req.Kind = GitQueryKind_Diff
	req.Target = GitDiffTarget_Worktree
	req.SetBaseRev([]byte("HEAD~3"))
	req.SetTargetRev([]byte(""))
	req.SetPath([]byte("tui/grid.go"))
	req.MaxCommits = 0
	req.MaxBytes = 2097152

	buf, err := req.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	var got GitQueryRequest
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Kind != GitQueryKind_Diff || got.Target != GitDiffTarget_Worktree {
		t.Fatalf("kind/target: %v %v", got.Kind, got.Target)
	}
	if string(got.BaseRev) != "HEAD~3" || string(got.Path) != "tui/grid.go" {
		t.Fatalf("strings: %q %q", got.BaseRev, got.Path)
	}
	if got.MaxBytes != 2097152 {
		t.Fatalf("max_bytes: %d", got.MaxBytes)
	}
}

func TestRunnerGitQueryRequestRoundTrip(t *testing.T) {
	var req RunnerGitQueryRequest
	req.TaskId = TaskID{Id: [16]byte{0xca, 0xfe}}
	req.StreamId = 42
	req.SetRepoPath([]byte("/srv/repo"))
	req.Kind = GitQueryKind_Log
	req.SetBaseRev([]byte(""))
	req.SetTargetRev([]byte(""))
	req.SetPath([]byte(""))
	req.MaxCommits = 7

	buf, err := req.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	var got RunnerGitQueryRequest
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.StreamId != 42 || string(got.RepoPath) != "/srv/repo" || got.MaxCommits != 7 {
		t.Fatalf("got %+v", got)
	}
}

func TestGitQueryResultLogRoundTrip(t *testing.T) {
	var c GitCommit
	c.SetSha([]byte("0123456789abcdef0123456789abcdef01234567"))
	c.SetAuthor([]byte("claude"))
	c.When = 1754179200
	c.SetSubject([]byte("feat(tui): grid paging"))

	var body GitLogBody
	body.SetCommits([]GitCommit{c})
	body.SetTruncated(true)

	var res GitQueryResult
	res.Status = GitRunStatus_Ok
	res.SetStderr(nil)
	res.Kind = GitQueryKind_Log
	if !res.SetLog(body) {
		t.Fatal("SetLog rejected")
	}

	buf, err := res.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	var got GitQueryResult
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	lg := got.Log()
	if lg == nil {
		t.Fatal("log arm missing")
	}
	if lg.Count != 1 || !lg.Truncated() {
		t.Fatalf("count/truncated: %d %v", lg.Count, lg.Truncated())
	}
	if string(lg.Commits[0].Sha) != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("sha: %q", lg.Commits[0].Sha)
	}
	if string(lg.Commits[0].Subject) != "feat(tui): grid paging" {
		t.Fatalf("subject: %q", lg.Commits[0].Subject)
	}
}

// A sha256 repository's 64-char object name must survive the wire; the length
// prefix exists precisely so this is not an encoder assertion.
func TestGitCommitAcceptsSha256Length(t *testing.T) {
	sha := make([]byte, 64)
	for i := range sha {
		sha[i] = 'a'
	}
	var c GitCommit
	if !c.SetSha(sha) {
		t.Fatal("SetSha rejected a 64-byte object name")
	}
	buf, err := c.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	var got GitCommit
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Sha) != 64 {
		t.Fatalf("sha len: %d", len(got.Sha))
	}
}

func TestGitQueryResultErrorArmIsEmpty(t *testing.T) {
	var res GitQueryResult
	res.Status = GitRunStatus_BadRev
	res.SetStderr([]byte("fatal: bad revision 'nope'"))
	res.Kind = GitQueryKind_Diff
	if !res.SetDiff(GitTextBody{}) {
		t.Fatal("SetDiff rejected")
	}
	buf, err := res.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	var got GitQueryResult
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Status != GitRunStatus_BadRev {
		t.Fatalf("status: %v", got.Status)
	}
	if string(got.Stderr) != "fatal: bad revision 'nope'" {
		t.Fatalf("stderr: %q", got.Stderr)
	}
	if d := got.Diff(); d == nil || d.Len != 0 {
		t.Fatalf("diff arm: %+v", d)
	}
}

func TestGitTextBodyRoundTrip(t *testing.T) {
	var body GitTextBody
	body.SetText([]byte("diff --git a/x b/x\n+added\n"))
	body.SetTruncated(true)

	var res GitQueryResult
	res.Status = GitRunStatus_Ok
	res.SetStderr(nil)
	res.Kind = GitQueryKind_Show
	if !res.SetShow(body) {
		t.Fatal("SetShow rejected")
	}
	buf, err := res.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	var got GitQueryResult
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	tb := got.Show()
	if tb == nil {
		t.Fatal("show arm missing")
	}
	if string(tb.Text) != "diff --git a/x b/x\n+added\n" || !tb.Truncated() {
		t.Fatalf("text %q truncated %v", tb.Text, tb.Truncated())
	}
}

func TestGitStatusBodyRoundTrip(t *testing.T) {
	var e1, e2 GitStatusEntry
	e1.Xy = [2]byte{' ', 'M'}
	e1.SetPath([]byte("tui/app.go"))
	e2.Xy = [2]byte{'?', '?'}
	e2.SetPath([]byte("scratch.txt"))

	var body GitStatusBody
	body.SetEntries([]GitStatusEntry{e1, e2})

	var res GitQueryResult
	res.Status = GitRunStatus_Ok
	res.SetStderr(nil)
	res.Kind = GitQueryKind_Status
	if !res.SetStatusBody(body) {
		t.Fatal("SetStatusBody rejected")
	}
	buf, err := res.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	var got GitQueryResult
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	st := got.StatusBody()
	if st == nil || st.Count != 2 {
		t.Fatalf("status arm: %+v", st)
	}
	if string(st.Entries[1].Path) != "scratch.txt" || st.Entries[1].Xy != [2]byte{'?', '?'} {
		t.Fatalf("entry 1: %+v", st.Entries[1])
	}
}

// The union arms must survive a trip through the TaskControl envelope, which
// is how they actually travel.
func TestGitQueryThroughTaskControlEnvelope(t *testing.T) {
	var body GitQueryRequest
	body.TaskId = TaskID{Id: [16]byte{7}}
	body.Kind = GitQueryKind_Status
	body.SetBaseRev(nil)
	body.SetTargetRev(nil)
	body.SetPath(nil)

	req := TaskControlRequest{Kind: TaskControlKind_GitQuery, RequestId: 99}
	if !req.SetGitQuery(body) {
		t.Fatal("SetGitQuery rejected")
	}
	buf, err := req.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	var got TaskControlRequest
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	gq := got.GitQuery()
	if gq == nil || gq.Kind != GitQueryKind_Status || got.RequestId != 99 {
		t.Fatalf("envelope: %+v", gq)
	}

	resp := TaskControlResponse{Kind: TaskControlKind_GitQuery, RequestId: 99}
	if !resp.SetGitQuery(GitQueryResponse{Status: GitQueryStatus_Ok, StreamId: 5}) {
		t.Fatal("SetGitQuery on response rejected")
	}
	rbuf, err := resp.Append(nil)
	if err != nil {
		t.Fatalf("append resp: %v", err)
	}
	var gotResp TaskControlResponse
	if _, err := gotResp.Decode(rbuf); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if r := gotResp.GitQuery(); r == nil || r.StreamId != 5 {
		t.Fatalf("response arm: %+v", r)
	}
}

func TestGitQueryRequestSubrepoRoundTrip(t *testing.T) {
	var req GitQueryRequest
	req.Kind = GitQueryKind_Diff
	req.Target = GitDiffTarget_Worktree
	req.SetBaseRev([]byte("HEAD"))
	req.SetTargetRev(nil)
	req.SetPath([]byte("src/x.go"))
	req.SetSubrepo([]byte("pkg/inner"))
	req.SetSubmoduleDiff(true)

	buf, err := req.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	var got GitQueryRequest
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// subrepo and path are different fields on purpose: one chooses the
	// repository, the other filters within it.
	if string(got.Subrepo) != "pkg/inner" {
		t.Fatalf("subrepo = %q", got.Subrepo)
	}
	if string(got.Path) != "src/x.go" {
		t.Fatalf("path = %q", got.Path)
	}
	if !got.SubmoduleDiff() {
		t.Fatal("submodule_diff lost")
	}
}

func TestGitQueryResultSubreposRoundTrip(t *testing.T) {
	var e1, e2 GitSubrepoEntry
	e1.SetPath([]byte("pkg/inner"))
	e2.SetPath([]byte("vendor/lib"))
	var body GitSubrepoBody
	body.SetEntries([]GitSubrepoEntry{e1, e2})
	body.SetTruncated(true)

	var res GitQueryResult
	res.Status = GitRunStatus_Ok
	res.SetStderr(nil)
	res.Kind = GitQueryKind_Subrepos
	if !res.SetSubrepos(body) {
		t.Fatal("SetSubrepos rejected")
	}
	buf, err := res.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	var got GitQueryResult
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	sr := got.Subrepos()
	if sr == nil || sr.Count != 2 || !sr.Truncated() {
		t.Fatalf("subrepos arm: %+v", sr)
	}
	if string(sr.Entries[1].Path) != "vendor/lib" {
		t.Fatalf("entry 1: %q", sr.Entries[1].Path)
	}
}

func TestGitRunStatusSubrepoInvalidExists(t *testing.T) {
	if GitRunStatus_SubrepoInvalid.String() != "subrepo_invalid" {
		t.Fatalf("name = %q", GitRunStatus_SubrepoInvalid.String())
	}
}

func TestRunnerGitQueryRequestCarriesSubrepo(t *testing.T) {
	var req RunnerGitQueryRequest
	req.StreamId = 7
	req.SetRepoPath([]byte("/srv/repo"))
	req.Kind = GitQueryKind_Subrepos
	req.SetBaseRev(nil)
	req.SetTargetRev(nil)
	req.SetPath(nil)
	req.SetSubrepo([]byte("pkg/inner"))
	req.SetSubmoduleDiff(true)

	buf, err := req.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	var got RunnerGitQueryRequest
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(got.Subrepo) != "pkg/inner" || !got.SubmoduleDiff() || got.StreamId != 7 {
		t.Fatalf("got %+v", got)
	}
}
