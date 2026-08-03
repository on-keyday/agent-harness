package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestClassifyGitLine(t *testing.T) {
	cases := []struct {
		line string
		want GitLineClass
	}{
		{"diff --git a/x b/x", GitLineFile},
		{"index 1234567..89abcde 100644", GitLineMeta},
		{"--- a/x", GitLineMeta},
		{"+++ b/x", GitLineMeta},
		{"@@ -1,3 +1,4 @@", GitLineHunk},
		{"+added", GitLineAdd},
		{"-removed", GitLineDel},
		{" context", GitLinePlain},
		{"", GitLinePlain},
		{"new file mode 100644", GitLineMeta},
		{"Binary files a/x and b/x differ", GitLineMeta},
	}
	for _, c := range cases {
		if got := ClassifyGitLine(c.line); got != c.want {
			t.Errorf("ClassifyGitLine(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

// "--- a/x" and "+++ b/x" must not be mistaken for a deletion and an addition;
// the header check has to run before the +/- check.
func TestClassifyGitLineHeadersBeatSigns(t *testing.T) {
	if ClassifyGitLine("--- a/tui/app.go") == GitLineDel {
		t.Error("--- header classified as a deletion")
	}
	if ClassifyGitLine("+++ b/tui/app.go") == GitLineAdd {
		t.Error("+++ header classified as an addition")
	}
}

// A removed line whose content itself starts with "--" is still a deletion,
// not a header: the header forms carry a trailing space.
func TestClassifyGitLineDeletedDashDashLine(t *testing.T) {
	if got := ClassifyGitLine("---x"); got != GitLineDel {
		t.Errorf("ClassifyGitLine(%q) = %v, want a deletion", "---x", got)
	}
}

func TestGitCommitShort(t *testing.T) {
	c := GitCommit{SHA: "0123456789abcdef"}
	if c.Short() != "0123456" {
		t.Fatalf("Short() = %q", c.Short())
	}
	short := GitCommit{SHA: "abc"}
	if short.Short() != "abc" {
		t.Fatalf("Short() on a short sha = %q", short.Short())
	}
}

func TestGitResultErr(t *testing.T) {
	ok := &GitResult{Status: protocol.GitRunStatus_Ok}
	if err := ok.Err(); err != nil {
		t.Fatalf("ok result returned %v", err)
	}
	bad := &GitResult{Status: protocol.GitRunStatus_BadRev, Stderr: "fatal: bad revision 'zzz'"}
	err := bad.Err()
	if err == nil {
		t.Fatal("bad_rev must produce an error")
	}
	// git's own words must survive to the operator, not be replaced by ours.
	if !strings.Contains(err.Error(), "bad revision 'zzz'") {
		t.Fatalf("error %q drops git's stderr", err.Error())
	}
}

func TestGitResultErrWithoutStderr(t *testing.T) {
	res := &GitResult{Status: protocol.GitRunStatus_NoSource}
	err := res.Err()
	if err == nil {
		t.Fatal("no_source must produce an error")
	}
	if !strings.Contains(err.Error(), "no_source") {
		t.Fatalf("error %q should name the status when git said nothing", err.Error())
	}
}

func TestDecodeGitResultLog(t *testing.T) {
	var c protocol.GitCommit
	c.SetSha([]byte("aaaaaaaaaaaa"))
	c.SetAuthor([]byte("claude"))
	c.When = 1700000000
	c.SetSubject([]byte("subject"))
	var body protocol.GitLogBody
	body.SetCommits([]protocol.GitCommit{c})
	body.SetTruncated(true)

	var res protocol.GitQueryResult
	res.Status = protocol.GitRunStatus_Ok
	res.Kind = protocol.GitQueryKind_Log
	res.SetLog(body)

	got := decodeGitResult(&res)
	if len(got.Commits) != 1 || !got.Truncated {
		t.Fatalf("decoded %+v", got)
	}
	if got.Commits[0].SHA != "aaaaaaaaaaaa" || got.Commits[0].Subject != "subject" {
		t.Fatalf("commit %+v", got.Commits[0])
	}
	if !got.Commits[0].When.Equal(time.Unix(1700000000, 0)) {
		t.Fatalf("when = %v", got.Commits[0].When)
	}
}

func TestDecodeGitResultStatus(t *testing.T) {
	var e protocol.GitStatusEntry
	e.Xy = [2]byte{'?', '?'}
	e.SetPath([]byte("new.txt"))
	var body protocol.GitStatusBody
	body.SetEntries([]protocol.GitStatusEntry{e})

	var res protocol.GitQueryResult
	res.Status = protocol.GitRunStatus_Ok
	res.Kind = protocol.GitQueryKind_Status
	res.SetStatusBody(body)

	got := decodeGitResult(&res)
	if len(got.Entries) != 1 || got.Entries[0].XY != "??" || got.Entries[0].Path != "new.txt" {
		t.Fatalf("decoded %+v", got.Entries)
	}
}

// show and diff share a body type but not an arm; reading the wrong one would
// silently yield empty text.
func TestDecodeGitResultShowReadsTheShowArm(t *testing.T) {
	var body protocol.GitTextBody
	body.SetText([]byte("commit aaa\n"))

	var res protocol.GitQueryResult
	res.Status = protocol.GitRunStatus_Ok
	res.Kind = protocol.GitQueryKind_Show
	res.SetShow(body)

	got := decodeGitResult(&res)
	if got.Text != "commit aaa\n" {
		t.Fatalf("text = %q", got.Text)
	}
}

// A failure still decodes: the runner always encodes an (empty) body arm, so
// the client reads status and stderr rather than erroring on the decode.
func TestDecodeGitResultFailureCarriesStderr(t *testing.T) {
	var res protocol.GitQueryResult
	res.Status = protocol.GitRunStatus_BadRev
	res.SetStderr([]byte("fatal: bad revision 'zzz'"))
	res.Kind = protocol.GitQueryKind_Diff
	res.SetDiff(protocol.GitTextBody{})

	got := decodeGitResult(&res)
	if got.Status != protocol.GitRunStatus_BadRev || got.Stderr == "" {
		t.Fatalf("decoded %+v", got)
	}
	if got.Err() == nil {
		t.Fatal("Err() must be non-nil for bad_rev")
	}
}
