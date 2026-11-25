package cli

import (
	"context"
	"strings"
	"testing"
)

// A refused URL is refused before the request leaves, with the rule named:
// the server answers only invalid_candidates.
func TestRunnerDialAddWithRefusesBadURLLocally(t *testing.T) {
	f := &fakeTaskControlClient{}
	_, _, err := RunnerDialAddWith(context.Background(), f, []string{"wss://h/ws?token=x"}, "abcd1234", "")
	if err == nil || !strings.Contains(err.Error(), "query is not supported") {
		t.Fatalf("err = %v, want one naming the query rule", err)
	}
	if f.lastRequest != nil {
		t.Fatal("the request was sent")
	}
}

// Every candidate is checked before it is sent, so the operator reads why:
// the server answers a refusal with invalid_candidates alone.
func TestRunnerDialAddWithNamesEveryRefusal(t *testing.T) {
	for raw, want := range map[string]string{
		"wss:@3f2a9c0d11b7e604-*": "ws(s):// URL",
		"not-a-cid":               "not-a-cid",
	} {
		f := &fakeTaskControlClient{}
		_, _, err := RunnerDialAddWith(context.Background(), f, []string{raw}, "abcd1234", "")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want it to contain %q", raw, err, want)
		}
		if f.lastRequest != nil {
			t.Errorf("%s: the request was sent", raw)
		}
	}
}
