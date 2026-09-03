package main

import (
	"bytes"
	"github.com/on-keyday/agent-harness/cli/verb"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestSplitPathspec(t *testing.T) {
	head, path := verb.SplitPathspec([]string{"abc", "--", "tui/app.go"})
	if len(head) != 1 || head[0] != "abc" {
		t.Fatalf("head = %v", head)
	}
	if path != "tui/app.go" {
		t.Fatalf("path = %q", path)
	}
}

func TestSplitPathspecNoSeparator(t *testing.T) {
	head, path := verb.SplitPathspec([]string{"abc", "def"})
	if len(head) != 2 || path != "" {
		t.Fatalf("head = %v path = %q", head, path)
	}
}

// The pathspec must be split off before the permuted flag parse: flag.Parse
// consumes a bare "--" itself, so a path left in the argv would vanish.
func TestSplitPathspecJoinsMultiWordPath(t *testing.T) {
	head, path := verb.SplitPathspec([]string{"--", "a b/c.go"})
	if len(head) != 0 {
		t.Fatalf("head = %v", head)
	}
	if path != "a b/c.go" {
		t.Fatalf("path = %q", path)
	}
}

func TestRenderGitLog(t *testing.T) {
	res := &cli.GitResult{
		Kind: protocol.GitQueryKind_Log,
		Commits: []cli.GitCommit{{
			SHA: "0123456789abcdef", Author: "claude",
			When: time.Unix(1700000000, 0), Subject: "first",
		}},
	}
	var buf bytes.Buffer
	renderGitResult(&buf, res, false)
	out := buf.String()
	if !strings.Contains(out, "0123456") {
		t.Fatalf("output %q missing the short sha", out)
	}
	if !strings.Contains(out, "first") {
		t.Fatalf("output %q missing the subject", out)
	}
}

func TestRenderGitLogTruncationIsAnnounced(t *testing.T) {
	res := &cli.GitResult{Kind: protocol.GitQueryKind_Log, Truncated: true}
	var buf bytes.Buffer
	renderGitResult(&buf, res, false)
	if !strings.Contains(buf.String(), "truncated") {
		t.Fatalf("truncation must be announced, got %q", buf.String())
	}
}

func TestRenderGitStatus(t *testing.T) {
	res := &cli.GitResult{
		Kind:    protocol.GitQueryKind_Status,
		Entries: []cli.GitStatusEntry{{XY: "??", Path: "new.txt"}},
	}
	var buf bytes.Buffer
	renderGitResult(&buf, res, false)
	if !strings.Contains(buf.String(), "?? new.txt") {
		t.Fatalf("output %q", buf.String())
	}
}

// A redirected diff has to stay byte-clean so `git apply` can read it.
func TestRenderGitDiffPlainHasNoEscapes(t *testing.T) {
	res := &cli.GitResult{Kind: protocol.GitQueryKind_Diff, Text: "+added\n-removed\n"}
	var buf bytes.Buffer
	renderGitResult(&buf, res, false)
	if strings.Contains(buf.String(), "\x1b[") {
		t.Fatalf("colour disabled but output carries escapes: %q", buf.String())
	}
	if buf.String() != "+added\n-removed\n" {
		t.Fatalf("plain render altered the bytes: %q", buf.String())
	}
}

func TestRenderGitDiffColored(t *testing.T) {
	res := &cli.GitResult{Kind: protocol.GitQueryKind_Diff, Text: "+added\n"}
	var buf bytes.Buffer
	renderGitResult(&buf, res, true)
	if !strings.Contains(buf.String(), "\x1b[") {
		t.Fatal("colour enabled but no escapes emitted")
	}
}

func TestRenderGitDiffEmptyTextPrintsNothing(t *testing.T) {
	res := &cli.GitResult{Kind: protocol.GitQueryKind_Diff, Text: ""}
	var buf bytes.Buffer
	renderGitResult(&buf, res, false)
	if buf.Len() != 0 {
		t.Fatalf("an empty diff must print nothing, got %q", buf.String())
	}
}

func TestRenderGitDiffTruncationIsAnnounced(t *testing.T) {
	res := &cli.GitResult{Kind: protocol.GitQueryKind_Diff, Text: "x\n", Truncated: true}
	var buf bytes.Buffer
	renderGitResult(&buf, res, false)
	if !strings.Contains(buf.String(), "truncated") {
		t.Fatalf("truncation must be announced, got %q", buf.String())
	}
}

func TestGitLineANSICoversEveryClass(t *testing.T) {
	for _, c := range []cli.GitLineClass{cli.GitLineAdd, cli.GitLineDel, cli.GitLineHunk, cli.GitLineFile, cli.GitLineMeta} {
		if gitLineANSI(c) == "" {
			t.Errorf("class %v has no colour", c)
		}
	}
	if gitLineANSI(cli.GitLinePlain) != "" {
		t.Error("a context line must not be coloured")
	}
}
