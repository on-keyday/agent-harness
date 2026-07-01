package runner

import (
	"reflect"
	"testing"
)

func TestMergeExtraArgs(t *testing.T) {
	cases := []struct {
		name    string
		global  []string
		perTask []string
		want    []string
	}{
		{"both empty", nil, nil, nil},
		{"only global", []string{"-A", "-B"}, nil, []string{"-A", "-B"}},
		{"only per-task", nil, []string{"-X"}, []string{"-X"}},
		{
			"global first, per-task last (last-wins semantics)",
			[]string{"--allowedTools", "bash"},
			[]string{"--allowedTools", "ls"},
			[]string{"--allowedTools", "bash", "--allowedTools", "ls"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeExtraArgs(tc.global, tc.perTask)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestMergeExtraArgsReturnsFreshSlice(t *testing.T) {
	// Mutating the result must not leak back into the caller's stored config.
	global := []string{"-A"}
	perTask := []string{"-B"}
	merged := mergeExtraArgs(global, perTask)
	merged[0] = "MUTATED"
	if global[0] != "-A" {
		t.Errorf("global slice was mutated through merge result: got %q", global[0])
	}
}

func TestWithResumeConversationArgs(t *testing.T) {
	cases := []struct {
		name               string
		args               []string
		resumeConversation bool
		want               []string
	}{
		{"disabled", []string{"--foo"}, false, []string{"--foo"}},
		{"enabled appends continue", []string{"--foo"}, true, []string{"--foo", "--continue"}},
		{"enabled does not duplicate", []string{"--foo", "--continue"}, true, []string{"--foo", "--continue"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := withResumeConversationArgs(tc.args, tc.resumeConversation)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}
