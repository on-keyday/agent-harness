package protocol

import (
	"reflect"
	"testing"
)

func TestClaudeArgsRoundTripStrings(t *testing.T) {
	cases := [][]string{
		nil,
		{},
		{""}, // empty arg should round-trip as a single empty string
		{"--resume", "deadbeef"},
		{"--add-dir", "/path with spaces", "--allowedTools", "bash,ls"},
	}
	for i, in := range cases {
		ca := ClaudeArgsFromStrings(in)
		out := ca.AsStrings()
		// AsStrings() returns nil for empty input by contract; nil and []
		// are equivalent for our use, so normalize before comparing.
		if len(in) == 0 && len(out) == 0 {
			continue
		}
		if !reflect.DeepEqual(in, out) {
			t.Errorf("case %d: round-trip diverged\n  in:  %#v\n  out: %#v", i, in, out)
		}
	}
}

func TestAssignTaskRoundTripWithExtraArgs(t *testing.T) {
	orig := AssignTask{
		AuthTicket: [16]byte{0xab, 0xcd},
	}
	orig.SetRepoPath([]byte("/repo/foo"))
	orig.SetPrompt([]byte("multi\nline\nprompt"))
	orig.ExtraArgs = ClaudeArgsFromStrings([]string{"--resume", "uuid-123", "--add-dir", "/x"})

	wire := orig.MustAppend(nil)

	var got AssignTask
	if err := got.DecodeExact(wire); err != nil {
		t.Fatalf("DecodeExact: %v", err)
	}
	if string(got.RepoPath) != "/repo/foo" {
		t.Errorf("RepoPath: got %q", got.RepoPath)
	}
	if string(got.Prompt) != "multi\nline\nprompt" {
		t.Errorf("Prompt: got %q", got.Prompt)
	}
	gotArgs := got.ExtraArgs.AsStrings()
	wantArgs := []string{"--resume", "uuid-123", "--add-dir", "/x"}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Errorf("ExtraArgs: got %#v, want %#v", gotArgs, wantArgs)
	}
}

func TestSubmitRequestEmptyExtraArgs(t *testing.T) {
	// Verify that an empty extra-args list encodes to a single u16(0) length
	// prefix and round-trips cleanly — the common case for callers that
	// don't supply --claude-arg.
	orig := SubmitRequest{
		PromptLen: 4,
		Prompt:    []byte("test"),
	}
	orig.SetRepoPath([]byte("/r"))
	orig.Selector = RunnerSelector{Kind: RunnerSelectorKind_Any}

	wire := orig.MustAppend(nil)

	var got SubmitRequest
	if err := got.DecodeExact(wire); err != nil {
		t.Fatalf("DecodeExact: %v", err)
	}
	if got.ExtraArgs.ArgsLen != 0 {
		t.Errorf("ArgsLen: got %d, want 0", got.ExtraArgs.ArgsLen)
	}
	if len(got.ExtraArgs.AsStrings()) != 0 {
		t.Errorf("AsStrings: got %v, want empty", got.ExtraArgs.AsStrings())
	}
}
