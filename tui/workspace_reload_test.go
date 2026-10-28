package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/cli/workspace"
)

const reloadSample = "[workspace dev]\n\n[workspace dev task " + wsTaskA + "]\nresume = continue\n"

// newWorkspaceApp builds an App whose config loader reads path, with "dev"
// installed from it — the state `harness-tui --workspace dev` starts in.
func newWorkspaceApp(t *testing.T, content string) (*App, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	writeConfig(t, path, content)
	a := New(Config{})
	a.loadWorkspaceFile = func() (*workspace.File, string, error) {
		return workspace.LoadFrom(path, "", "", false)
	}
	if !a.reloadWorkspace("test") {
		t.Fatalf("initial load failed: %v", a.cmdresult.lines)
	}
	if ws, ok := a.workspaceFile.Workspace("dev"); ok {
		a.SetWorkspace(ws)
	}
	return a, path
}

func writeConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func resultText(a *App) string { return strings.Join(a.cmdresult.lines, "\n") }

// A hand edit made after start is what `workspace show` prints and what the
// installed workspace now says. The TUI used to read the file once, so the
// edit was invisible until restart.
func TestReloadSeesAHandEdit(t *testing.T) {
	a, path := newWorkspaceApp(t, reloadSample)
	writeConfig(t, path, reloadSample+"forward = -L 4000\n")
	a.runWorkspaceAction(verb.WorkspaceAction{Sub: verb.SubShow})
	if !strings.Contains(resultText(a), "-L 4000") {
		t.Errorf("show did not print the hand edit:\n%s", resultText(a))
	}
	if !a.workspace.Declares(wsTaskA, "-L 4000") {
		t.Error("the installed workspace does not carry the hand edit")
	}
}

// A file left unparsable refuses the one operation and changes nothing: the
// TUI keeps running and the installed workspace stays what it was.
func TestReloadRefusesABrokenFile(t *testing.T) {
	a, path := newWorkspaceApp(t, reloadSample)
	before := a.workspace
	writeConfig(t, path, "[workspace dev]\nnope = 1\n")
	a.runWorkspaceAction(verb.WorkspaceAction{Sub: verb.SubShow})
	if a.workspace != before {
		t.Error("a broken file replaced the installed workspace")
	}
	if !strings.Contains(resultText(a), path) {
		t.Errorf("the error does not name the file:\n%s", resultText(a))
	}
}

func TestReloadUninstallsAWorkspaceRemovedByHand(t *testing.T) {
	a, path := newWorkspaceApp(t, reloadSample)
	writeConfig(t, path, "[workspace other]\n")
	a.runWorkspaceAction(verb.WorkspaceAction{Sub: verb.SubLs})
	if a.workspace != nil {
		t.Errorf("workspace %q is still installed after it left the file", a.workspace.Name)
	}
	if !strings.Contains(resultText(a), "no longer in") {
		t.Errorf("the uninstall was not reported:\n%s", resultText(a))
	}
}

// A TUI write starts from the file as it is now, not as it was at start, so a
// hand edit survives it.
func TestSaveAfterHandEditKeepsIt(t *testing.T) {
	a, path := newWorkspaceApp(t, reloadSample)
	writeConfig(t, path, reloadSample+"\n[workspace dev task "+wsTaskB+"]\nresume = fresh\n")
	a.writeWorkspace(&workspace.Workspace{Name: "dev"}, workspace.MergeDecisions{}, 0, 0, workspaceDecided{})
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), wsTaskB) {
		t.Errorf("the hand-added block was overwritten:\n%s", got)
	}
}
