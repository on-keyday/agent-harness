//go:build !js

package main

import "testing"

// TestForwardSubcommandRouting: "ls" and "kill" are not hex, so they can
// never collide with a task id.
func TestForwardSubcommandRouting(t *testing.T) {
	for _, sub := range []string{"ls", "kill"} {
		if isTaskIDLike(sub) {
			t.Errorf("%q must not parse as a task id", sub)
		}
	}
	for _, id := range []string{"deadbeef", "0123456789abcdef0123456789abcdef"} {
		if !isTaskIDLike(id) {
			t.Errorf("%q should parse as a task id", id)
		}
	}
}
