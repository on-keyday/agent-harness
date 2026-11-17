package cli

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func nowTask(kind protocol.TaskKind, progress string, at uint64, live bool, title string) protocol.TaskInfo {
	var t protocol.TaskInfo
	t.Kind = kind
	t.Status = protocol.TaskStatus_Running
	t.SetPrompt([]byte("the original ask"))
	t.SetProgress([]byte(progress))
	t.ProgressAt = at
	t.ProgressAgeMs = 720000
	t.SetLiveTerminal(live)
	t.SetTitle([]byte(title))
	return t
}

// Progress first, then the program's title (marked), then the prompt cell —
// the TUI's About column and the overview read this one function.
func TestTaskNowTextPrecedence(t *testing.T) {
	cases := []struct {
		name string
		t    protocol.TaskInfo
		want string
	}{
		{"progress", nowTask(protocol.TaskKind_Interactive, "stage 2", 1, true, "✳ topic"), "stage 2 (12m ago)"},
		{"title", nowTask(protocol.TaskKind_Interactive, "", 0, true, "✳ topic"), "(title) ✳ topic"},
		{"cleared falls to title", nowTask(protocol.TaskKind_Interactive, "", 9, true, "✳ topic"), "(title) ✳ topic"},
		{"prompt", nowTask(protocol.TaskKind_Oneshot, "", 0, false, ""), "the original ask"},
		{"interactive", nowTask(protocol.TaskKind_Interactive, "", 0, false, ""), "<interactive>"},
		{"stream", nowTask(protocol.TaskKind_Stream, "", 0, false, ""), "<stream>"},
	}
	for _, c := range cases {
		if got := TaskNowText(c.t); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	one := nowTask(protocol.TaskKind_Interactive, "", 0, false, "")
	one.SetOneshot(true)
	one.SetNoPty(true)
	if got := TaskNowText(one); got != "(oneshot) the original ask" {
		t.Errorf("oneshot: got %q", got)
	}
	one.SetNoPty(false)
	if got := TaskNowText(one); got != "(oneshot+pty) the original ask" {
		t.Errorf("oneshot+pty: got %q", got)
	}
	pipe := nowTask(protocol.TaskKind_Interactive, "", 0, false, "")
	pipe.SetNoPty(true)
	if got := TaskNowText(pipe); got != "<interactive+pipe>" {
		t.Errorf("pipe: got %q", got)
	}
}

func TestTaskNowTextStripsControlsInProgressAndTitle(t *testing.T) {
	if got := TaskNowText(nowTask(protocol.TaskKind_Interactive, "\x1b[2Jx\u009b", 1, true, "")); got != "[2Jx (12m ago)" {
		t.Errorf("progress: %q", got)
	}
	if got := TaskNowText(nowTask(protocol.TaskKind_Interactive, "", 0, true, "\x1b]0;t\x07")); got != "(title) ]0;t" {
		t.Errorf("title: %q", got)
	}
}

// The prompt is agent-written too; before this it passed ESC and C1 through.
func TestTaskNowTextSanitisesPrompt(t *testing.T) {
	ti := nowTask(protocol.TaskKind_Oneshot, "", 0, false, "")
	ti.SetPrompt([]byte("a\x1b[2Jb\u009bc\nd"))
	if got := TaskNowText(ti); got != "a[2Jbc d" {
		t.Fatalf("got %q", got)
	}
}

func TestTaskNowTextPromptCutOnRune(t *testing.T) {
	ti := nowTask(protocol.TaskKind_Oneshot, "", 0, false, "")
	ti.SetPrompt([]byte(strings.Repeat("あ", 100))) // 300 bytes
	got := TaskNowText(ti)
	if !utf8.ValidString(got) || len(got) > 140 || len(got) < 138 {
		t.Fatalf("len %d valid %v", len(got), utf8.ValidString(got))
	}
}
