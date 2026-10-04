package verb

import (
	"strings"
	"testing"
)

// --no-wake is the operator's: an agent that suppressed its own wake by
// misunderstanding would report the message as undelivered, so the agent
// verbs must not even list it.
func TestNoWakeIsOnlyOnTheOperatorSend(t *testing.T) {
	bs, ok := Lookup("board", "send")
	if !ok {
		t.Fatal("board send is not declared")
	}
	if !strings.Contains(strings.Join(bs.HelpBlock(100), "\n"), "--no-wake") {
		t.Error("board send help does not list --no-wake")
	}
	for _, p := range [][]string{{"agent", "send"}, {"agent", "dispatch"}} {
		v, _ := Lookup(p...)
		if strings.Contains(strings.Join(v.HelpBlock(100), "\n"), "no-wake") {
			t.Errorf("%s lists no-wake", strings.Join(p, " "))
		}
	}
	if _, ok := Lookup("board", "wake"); !ok {
		t.Error("board wake is not declared")
	}
}
