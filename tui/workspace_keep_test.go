package tui

import (
	"encoding/hex"
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

const longForm3000 = "-L 127.0.0.1:3000:127.0.0.1:3000"

func taskIDOf(t *testing.T, idHex string) protocol.TaskID {
	t.Helper()
	raw, err := hex.DecodeString(idHex)
	if err != nil || len(raw) != 16 {
		t.Fatalf("bad fixture id %q", idHex)
	}
	var id protocol.TaskID
	copy(id.Id[:], raw)
	return id
}

// fwdInfo is a registry row for a local tcp forward on port.
func fwdInfo(t *testing.T, id uint64, task string, port uint16) protocol.PortForwardInfo {
	return protocol.PortForwardInfo{
		ForwardId: id, TaskId: taskIDOf(t, task), Direction: protocol.PortForwardDirection_Local,
		ClientEndpoint: protocol.ClientEndpointKind_OsSocket,
		BindAddr:       []byte("127.0.0.1"), BindPort: port, TargetHost: []byte("127.0.0.1"), TargetPort: port,
	}
}

// keepApp is a workspace app with "dev" installed (an empty block for task A)
// and the forwards pane open on one row: forward 7, -L 3000 on A.
func keepApp(t *testing.T, session bool) (*App, string) {
	t.Helper()
	a, path := newWorkspaceApp(t, "[workspace dev]\n\n[workspace dev task "+wsTaskA+"]\nresume = continue\n")
	if session {
		a.activeForwards[1] = &PortForwardSession{ID: 1, TaskID: wsTaskA, Direction: ForwardLocal, Spec: "3000", ForwardID: 7}
	}
	a.forwardsModal.Open()
	a.forwardsModal.ApplySnapshot([]protocol.PortForwardInfo{fwdInfo(t, 7, wsTaskA, 3000)})
	return a, path
}

func pressW(a *App) { a.inForwardsModal(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'w'}}) }

func fileText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestKeepWritesAndAdopts(t *testing.T) {
	a, path := keepApp(t, true)
	pressW(a)
	if !strings.Contains(fileText(t, path), longForm3000) {
		t.Errorf("file does not declare the forward:\n%s", fileText(t, path))
	}
	if !a.activeForwards[1].FromWorkspace {
		t.Error("the local session was not adopted")
	}
	if want := "workspace dev: kept " + longForm3000 + " on " + pfShortID(wsTaskA); !strings.Contains(resultText(a), want) {
		t.Errorf("result does not say %q:\n%s", want, resultText(a))
	}
	if strings.Contains(resultText(a), "held by another client") {
		t.Error("a forward this TUI holds was reported as another client's")
	}
}

// Un-keep means "do not bring this back", not "stop it": the block stays for
// its policy, and the session is released so no apply stops it either.
func TestUnkeepReleasesAndKeepsTheBlock(t *testing.T) {
	a, path := keepApp(t, true)
	pressW(a)
	pressW(a)
	text := fileText(t, path)
	if strings.Contains(text, "forward") || !strings.Contains(text, "[workspace dev task "+wsTaskA+"]") {
		t.Errorf("want the block kept with no forward:\n%s", text)
	}
	if a.activeForwards[1].FromWorkspace {
		t.Error("the session is still workspace-owned after un-keep")
	}
	if !strings.Contains(resultText(a), "(still running; x stops it)") {
		t.Errorf("un-keep did not say the forward keeps running:\n%s", resultText(a))
	}
}

func TestKeepWithoutALocalSessionSaysSo(t *testing.T) {
	a, _ := keepApp(t, false)
	pressW(a)
	if !strings.Contains(resultText(a), "held by another client") {
		t.Errorf("result does not warn about the other holder:\n%s", resultText(a))
	}
}

func TestKeepRefusesAnInProcessRow(t *testing.T) {
	a, path := keepApp(t, false)
	before := fileText(t, path)
	fi := fwdInfo(t, 7, wsTaskA, 3000)
	fi.ClientEndpoint = protocol.ClientEndpointKind_InProcess
	a.forwardsModal.ApplySnapshot([]protocol.PortForwardInfo{fi})
	pressW(a)
	if fileText(t, path) != before || !strings.Contains(resultText(a), "cannot be written down") {
		t.Errorf("an in-process row was kept or not refused:\n%s", resultText(a))
	}
}

func TestKeepRefusesALinkRow(t *testing.T) {
	a, path := keepApp(t, false)
	before := fileText(t, path)
	fi := fwdInfo(t, 7, wsTaskA, 3000)
	fi.Direction = protocol.PortForwardDirection_Link
	a.forwardsModal.ApplySnapshot([]protocol.PortForwardInfo{fi})
	pressW(a)
	if fileText(t, path) != before || !strings.Contains(resultText(a), "cannot be written down") {
		t.Errorf("a link row was kept or not refused:\n%s", resultText(a))
	}
}

func TestKeepWithNoWorkspaceInstalled(t *testing.T) {
	a, path := keepApp(t, true)
	a.SetWorkspace(nil)
	before := fileText(t, path)
	pressW(a)
	if fileText(t, path) != before || !strings.Contains(resultText(a), "no workspace installed") {
		t.Errorf("keep with nothing installed wrote or did not refuse:\n%s", resultText(a))
	}
}

func TestForwardMarksArePerTask(t *testing.T) {
	a, _ := newWorkspaceApp(t, "[workspace dev]\n\n[workspace dev task "+wsTaskA+"]\nforward = -L 3000\n")
	fs := []protocol.PortForwardInfo{fwdInfo(t, 1, wsTaskA, 3000), fwdInfo(t, 2, wsTaskB, 3000)}
	marks := a.forwardMarks(fs)
	if marks[1] != "kept" || marks[2] != "-" {
		t.Errorf("marks = %v, want {1: kept, 2: -}", marks)
	}
	a.SetWorkspace(nil)
	if m := a.forwardMarks(fs); m[1] != "" || m[2] != "" {
		t.Errorf("with nothing installed marks = %v, want empty cells", m)
	}
}

// The direction of `w` comes from the file as it is at the keypress, not from
// the column the pane last drew: a hand edit in between must not invert it.
func TestKeepDecidesFromTheFileNotTheColumn(t *testing.T) {
	a, path := keepApp(t, true)
	if got := a.forwardMarks(a.forwardsModal.forwards)[7]; got != "-" {
		t.Fatalf("mark before = %q, want -", got)
	}
	writeConfig(t, path, fileText(t, path)+"forward = -L 3000\n")
	pressW(a)
	if strings.Contains(fileText(t, path), "forward") {
		t.Errorf("w kept instead of un-keeping the hand-declared forward:\n%s", fileText(t, path))
	}
	if !strings.Contains(resultText(a), "no longer keeping") {
		t.Errorf("result does not say which way it went:\n%s", resultText(a))
	}
}
