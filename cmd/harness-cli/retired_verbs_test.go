package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli/verb"
)

// A retired verb's hint is printed only for a word the table no longer
// declares: an entry for a declared verb would never be reached, and one for
// a declared family would hide the family's usage.
func TestRetiredVerbsAreNotDeclared(t *testing.T) {
	for head := range retiredVerbs {
		if _, ok := verb.Lookup(head); ok {
			t.Errorf("%q is declared again; drop it from retiredVerbs", head)
		}
		if subs := familySubverbs(head); len(subs) > 0 {
			t.Errorf("%q heads a declared family %v; drop it from retiredVerbs", head, subs)
		}
	}
}

func TestRetiredVerbHint(t *testing.T) {
	for head, want := range map[string]string{
		"submit": "session new --oneshot",
		"logs":   "session snapshot",
	} {
		var b bytes.Buffer
		if !retiredVerbHintTo(&b, head) {
			t.Errorf("%q: no hint", head)
			continue
		}
		if !strings.Contains(b.String(), want) {
			t.Errorf("%q: hint %q does not name %q", head, b.String(), want)
		}
	}
	var b bytes.Buffer
	if retiredVerbHintTo(&b, "no-such-verb") || b.Len() != 0 {
		t.Errorf("an unknown word got a hint: %q", b.String())
	}
}
