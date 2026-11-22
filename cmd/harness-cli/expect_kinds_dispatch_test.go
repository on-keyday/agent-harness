//go:build !js

package main

import "testing"

// The command line installs the Sends of the verb it dispatches to, so it has
// to find the same verb the dispatcher does: the longest path, and git's form
// with the task id inside the path.
func TestCLISpecForFindsTheDispatchedVerb(t *testing.T) {
	for line, want := range map[string]string{
		"session await-idle ls --json":                        "session await-idle ls",
		"session await-idle 0123456789abcdef0123456789abcdef": "session await-idle",
		"git 0123456789abcdef0123456789abcdef log":            "git log",
		"ls --json": "ls",
	} {
		sp, ok := cliSpecFor(splitWords(line))
		if !ok || sp.FlagSetName() != want {
			t.Errorf("%q: got %q (%t), want %q", line, sp.FlagSetName(), ok, want)
		}
	}
	if _, ok := cliSpecFor([]string{"no-such-verb"}); ok {
		t.Error("an unknown verb resolved")
	}
}

func splitWords(s string) []string {
	var out []string
	w := ""
	for _, r := range s + " " {
		if r == ' ' {
			if w != "" {
				out = append(out, w)
			}
			w = ""
			continue
		}
		w += string(r)
	}
	return out
}
