package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/objtrsf/objproto"
)

// -t makes this terminal the child's, so without one there is nothing to hand
// over. Refused BEFORE dialing: a zero connection id would fail to dial, so
// reaching the dial is itself the failure.
func TestExecTRefusesANonTerminalStdin(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	old := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = old }()

	err = runExecAction(context.Background(), objproto.ConnectionID{}, verb.ExecRunAction{
		TaskID: "0123456789abcdef0123456789abcdef", Argv: []string{"bash", "-l"}, Tty: true})
	if err == nil || !strings.Contains(err.Error(), "not a terminal") {
		t.Fatalf("exec -t with /dev/null stdin = %v, want a refusal saying stdin is not a terminal", err)
	}
}
