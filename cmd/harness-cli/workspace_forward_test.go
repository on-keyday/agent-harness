//go:build !js

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/objtrsf/objproto"
)

const wfTask = "3f2a9c00000000000000000000000001"

// parseWorkspaceForward parses argv exactly as the binary does, so the
// assertion is about the spelling an operator types: `-L` / `-R` are FLAGS,
// which a quoted "-L 3000" positional could never have been.
func parseWorkspaceForward(t *testing.T, args ...string) verb.WorkspaceAction {
	t.Helper()
	act, handled, err := verb.ParseCLICommand(append([]string{"workspace", "forward"}, args...), nil)
	if !handled || err != nil {
		t.Fatalf("%v: handled=%t err=%v", args, handled, err)
	}
	wa, ok := act.(verb.WorkspaceAction)
	if !ok {
		t.Fatalf("%v: got %T", args, act)
	}
	return wa
}

func TestWorkspaceForwardParse(t *testing.T) {
	wa := parseWorkspaceForward(t, "add", "dev", wfTask, "-L", "3000", "-R", "8080")
	if wa.Sub != verb.SubForwardAdd || wa.Name != "dev" || wa.TaskID != wfTask ||
		strings.Join(wa.L, "|") != "3000" || strings.Join(wa.R, "|") != "8080" {
		t.Errorf("parsed %+v", wa)
	}
	if wa := parseWorkspaceForward(t, "rm", "dev", wfTask, "-L", "3000"); wa.Sub != verb.SubForwardRm {
		t.Errorf("rm parsed as %q", wa.Sub)
	}
	if _, _, err := verb.ParseCLICommand([]string{"workspace", "forward", "add", "dev", wfTask}, nil); err == nil {
		t.Error("add with neither -L nor -R parsed")
	}
}

func withConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	old := workspaceCfgPath
	workspaceCfgPath = path
	t.Cleanup(func() { workspaceCfgPath = old })
	return path
}

func noServer() objproto.ConnectionID {
	panic("workspace forward must not reach the server")
}

func TestWorkspaceForwardAddWritesTheFile(t *testing.T) {
	path := withConfig(t, "[workspace dev]\n")
	add := verb.WorkspaceAction{Sub: verb.SubForwardAdd, Name: "dev", TaskID: wfTask, L: []string{"3000"}}
	if err := runWorkspaceAction(context.Background(), add, noServer); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	got := string(b)
	if !strings.Contains(got, "[workspace dev task "+wfTask+"]") || !strings.Contains(got, "-L 3000") ||
		!strings.Contains(got, "resume   = continue") {
		t.Errorf("file:\n%s", got)
	}
	// A second add of the same forward is a no-op, under another spelling too.
	again := add
	again.L = []string{"127.0.0.1:3000:127.0.0.1:3000"}
	if err := runWorkspaceAction(context.Background(), again, noServer); err != nil {
		t.Fatal(err)
	}
	b2, _ := os.ReadFile(path)
	if strings.Count(string(b2), "forward") != 1 {
		t.Errorf("the same forward was declared twice:\n%s", b2)
	}
	rm := verb.WorkspaceAction{Sub: verb.SubForwardRm, Name: "dev", TaskID: wfTask, L: []string{"3000"}}
	if err := runWorkspaceAction(context.Background(), rm, noServer); err != nil {
		t.Fatal(err)
	}
	b3, _ := os.ReadFile(path)
	if strings.Contains(string(b3), "forward") || !strings.Contains(string(b3), wfTask) {
		t.Errorf("rm should drop the line and keep the block:\n%s", b3)
	}
}

func TestWorkspaceForwardAddRefusesAMissingWorkspace(t *testing.T) {
	path := withConfig(t, "[workspace dev]\n")
	add := verb.WorkspaceAction{Sub: verb.SubForwardAdd, Name: "nope", TaskID: wfTask, L: []string{"3000"}}
	err := runWorkspaceAction(context.Background(), add, noServer)
	if err == nil || !strings.Contains(err.Error(), "workspace save nope") {
		t.Errorf("err = %v, want a refusal naming `workspace save nope`", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "[workspace dev]\n" {
		t.Errorf("the file changed:\n%s", b)
	}
}
