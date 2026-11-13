//go:build !windows

package streamagent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A cancelled adapter passes SIGTERM on to its agent instead of SIGKILLing it
// (pillar A spec, F5): claude ends the tool processes it started in their own
// sessions when it gets a signal it can handle, and cannot when it is killed.
func TestCancelSendsTheAgentSIGTERM(t *testing.T) {
	dir := t.TempDir()
	mark := filepath.Join(dir, "mark")
	ready := filepath.Join(dir, "ready")
	agent := fakeAgent(t, filepath.Join(dir, "argv"),
		"trap 'echo term > "+mark+"; exit 0' TERM\n"+
			"echo ok > "+ready+"\n"+
			"while :; do sleep 0.1; done\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out, errOut bytes.Buffer
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pw.Close()
	done := make(chan error, 1)
	go func() {
		done <- RunClaude(ctx, ClaudeOpts{Argv: []string{agent}, Dir: dir, Out: &out, In: pr, ErrOut: &errOut})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the fake agent never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("RunClaude did not return after the cancel")
	}
	b, _ := os.ReadFile(mark)
	if strings.TrimSpace(string(b)) != "term" {
		t.Fatalf("the agent was not sent SIGTERM (mark %q): a bare SIGKILL strands its tool processes", b)
	}
}
