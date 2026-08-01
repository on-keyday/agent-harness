package tui

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
)

func openedEditor(t *testing.T) FileEditModel {
	t.Helper()
	m := NewFileEdit()
	m.SetSize(80, 24)
	m.OpenEdit("abc123", cli.FileEditDoc{Rel: "notes.txt", Text: "alpha\n"})
	if !m.IsOpen() {
		t.Fatal("OpenEdit did not open the popup")
	}
	return m
}

func TestFileEditCtrlJEmitsSave(t *testing.T) {
	m := openedEditor(t)
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlJ})
	if cmd == nil {
		t.Fatal("ctrl+j produced no command")
	}
	msg := cmd()
	save, ok := msg.(FileEditSaveMsg)
	if !ok {
		t.Fatalf("ctrl+j produced %T, want FileEditSaveMsg", msg)
	}
	if save.Name != "notes.txt" || save.Text != "alpha\n" {
		t.Errorf("save=%+v, want notes.txt / alpha", save)
	}
	if !m.IsOpen() {
		t.Error("popup closed on save; it must stay open until the commit result lands")
	}
}

func TestFileEditEscCloses(t *testing.T) {
	m := openedEditor(t)
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.IsOpen() {
		t.Error("esc did not close the popup")
	}
}

func TestFileEditTabMovesFocusToName(t *testing.T) {
	m := openedEditor(t)
	// Body is focused on open; Tab moves to the name field, so a typed rune
	// lands in the name, not in the body.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'X'}})
	if got := m.Name(); got != "notes.txtX" {
		t.Errorf("Name()=%q, want notes.txtX", got)
	}
	if got := m.Text(); got != "alpha\n" {
		t.Errorf("Text()=%q, want the body untouched", got)
	}
}

func TestFileEditUnboundKeyReachesBody(t *testing.T) {
	m := openedEditor(t)
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'Z'}})
	if got := m.Text(); got != "alpha\nZ" && got != "Zalpha\n" {
		t.Errorf("Text()=%q, want the rune inserted in the body", got)
	}
}

func TestFileEditCtrlOEmitsExternal(t *testing.T) {
	m := openedEditor(t)
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	if cmd == nil {
		t.Fatal("ctrl+o produced no command")
	}
	ext, ok := cmd().(FileEditExternalMsg)
	if !ok {
		t.Fatalf("ctrl+o produced %T, want FileEditExternalMsg", cmd())
	}
	if ext.Text != "alpha\n" || ext.Name != "notes.txt" {
		t.Errorf("external=%+v, want the current buffer", ext)
	}
}

func TestFileEditOpenNewSeedsDirectory(t *testing.T) {
	m := NewFileEdit()
	m.SetSize(80, 24)
	m.OpenNew("abc123", "sub/dir")
	if !m.IsCreate() {
		t.Error("IsCreate()=false after OpenNew")
	}
	if got := m.Name(); got != "sub/dir/" {
		t.Errorf("Name()=%q, want the current directory seeded", got)
	}
	if got := m.Text(); got != "" {
		t.Errorf("Text()=%q, want empty", got)
	}
}

func TestFileEditSaveWithoutNameStaysOpen(t *testing.T) {
	m := NewFileEdit()
	m.SetSize(80, 24)
	m.OpenNew("abc123", "")
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlJ})
	if cmd != nil {
		t.Error("ctrl+j with an empty name produced a save command")
	}
	if !m.IsOpen() {
		t.Error("popup closed on an empty-name save")
	}
}

func TestFileEditTempFileKeepsExtension(t *testing.T) {
	p, err := writeFileEditTemp("sub/dir/notes.md", "alpha\n")
	if err != nil {
		t.Fatalf("writeFileEditTemp: %v", err)
	}
	defer os.Remove(p)
	if !strings.HasSuffix(p, ".md") {
		t.Errorf("temp path %q lost the .md extension", p)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(b) != "alpha\n" {
		t.Errorf("temp contents=%q, want alpha", b)
	}
}

func TestFileEditTempFileNoExtension(t *testing.T) {
	p, err := writeFileEditTemp("Makefile", "all:\n")
	if err != nil {
		t.Fatalf("writeFileEditTemp: %v", err)
	}
	defer os.Remove(p)
	if filepath.Ext(p) != "" {
		t.Errorf("temp path %q invented an extension", p)
	}
}

// The suspension banner has to reach the terminal, because the alt screen is
// already gone by the time the editor runs and the popup's own View is not
// visible. Assert it is written before the child runs, and with CRLF — the
// terminal is still in raw mode here.
func TestEditorExecAnnouncesBeforeRunning(t *testing.T) {
	var buf bytes.Buffer
	e := &editorExec{cmd: exec.Command("true"), name: "nano", path: "/tmp/harness-edit-1.md"}
	e.SetStdout(&buf)
	if err := e.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"nano", "/tmp/harness-edit-1.md", "TUI"} {
		if !strings.Contains(out, want) {
			t.Errorf("banner %q does not mention %q", out, want)
		}
	}
	if strings.Contains(strings.ReplaceAll(out, "\r\n", ""), "\n") {
		t.Errorf("banner %q has a bare LF; raw mode needs CRLF", out)
	}
}

func TestFileEditClosedUpdateIsNoop(t *testing.T) {
	m := NewFileEdit()
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlJ})
	if cmd != nil {
		t.Error("a closed popup produced a command")
	}
	if m.IsOpen() {
		t.Error("a closed popup opened itself")
	}
}
