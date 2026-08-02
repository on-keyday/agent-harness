package tui

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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

func TestFileEditShiftTabMovesFocusToName(t *testing.T) {
	m := openedEditor(t)
	// Body is focused on open; shift+tab moves to the name field, so a typed
	// rune lands in the name, not in the body.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
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
	// A new file must be named and has nothing to edit yet, so typing right
	// after `n` belongs in the name — not in the contents.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("memo.txt")})
	if got := m.Name(); got != "sub/dir/memo.txt" {
		t.Errorf("Name()=%q, want the typed name appended", got)
	}
	if got := m.Text(); got != "" {
		t.Errorf("Text()=%q, want the body still empty", got)
	}
}

// Edit is the mirror image: the path is already right, so typing goes to the
// contents.
func TestFileEditOpenEditFocusesBody(t *testing.T) {
	m := openedEditor(t)
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Z")})
	if got := m.Name(); got != "notes.txt" {
		t.Errorf("Name()=%q, want it untouched", got)
	}
	if !strings.Contains(m.Text(), "Z") {
		t.Errorf("Text()=%q, want the rune in the body", m.Text())
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

// The popup is centred with lipgloss.Place at the viewport size, so a box
// wider than the viewport loses its right border and looks broken. The
// widget chrome constants are measured values; this is what catches them
// drifting.
func TestFileEditViewFitsViewport(t *testing.T) {
	long := "notes.txt は runner 側で変更されています — もう一度 ctrl+j で上書き, esc で破棄"
	for _, w := range []int{24, 40, 60, 80, 100, 120, 200} {
		m := NewFileEdit()
		m.SetSize(w, 40)
		m.OpenEdit("t", cli.FileEditDoc{Rel: "some/nested/notes.txt", Text: "alpha\nbeta\n"})
		if got := lipgloss.Width(m.View()); got > w {
			t.Errorf("viewport %d: View() width %d overflows", w, got)
		}
		m.SetStatus(long, true)
		if got := lipgloss.Width(m.View()); got > w {
			t.Errorf("viewport %d with a long status: View() width %d overflows", w, got)
		}
	}
}

// Fitting the box must not be achieved by silently truncating what the
// operator needs to read. clipLine is ANSI-unaware, so running it over a
// styled widget render would clip early and invisibly — this pins that the
// path survives at an ordinary width.
func TestFileEditViewKeepsFullPath(t *testing.T) {
	m := NewFileEdit()
	m.SetSize(80, 40)
	m.OpenEdit("t", cli.FileEditDoc{Rel: "some/nested/notes.txt", Text: "alpha\n"})
	if !strings.Contains(m.View(), "some/nested/notes.txt") {
		t.Errorf("View() lost the path:\n%s", m.View())
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

// The popup's key wiring must sanitize too, not just the buffer's own API:
// a bracketed paste arrives here as one KeyRunes event and can carry control
// bytes straight from the terminal.
func TestFileEditPasteWithControlCharsStaysText(t *testing.T) {
	m := openedEditor(t)
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x', 0x00, 'y', 0x1b, 'z'}, Paste: true})
	got := m.Text()
	if strings.ContainsRune(got, 0x00) || strings.ContainsRune(got, 0x1b) {
		t.Fatalf("Text()=%q still holds control characters", got)
	}
	if !strings.Contains(got, "xyz") {
		t.Errorf("Text()=%q, want the printable runes kept as xyz", got)
	}
	// And what we would push must load again.
	doc := m.Doc()
	if _, err := cli.NewFileEditDocForTest("memo.txt", doc.Encode(got)); err != nil {
		t.Errorf("saved bytes would not reopen: %v", err)
	}
}

// Tab types a tab in the body — it is a text-entry key, and a Makefile is
// only valid with real ones. Field switching moves to shift+tab, which the
// TUI already uses elsewhere (tui/app.go handles tea.KeyShiftTab).
func TestFileEditTabInsertsTabInBody(t *testing.T) {
	m := openedEditor(t)
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if !strings.Contains(m.Text(), "\t") {
		t.Errorf("Text()=%q, want a tab inserted", m.Text())
	}
	if got := m.Name(); got != "notes.txt" {
		t.Errorf("Name()=%q — tab must not have moved focus", got)
	}
}

func TestFileEditShiftTabSwitchesFields(t *testing.T) {
	m := openedEditor(t)
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'X'}})
	if got := m.Name(); got != "notes.txtX" {
		t.Errorf("Name()=%q, want the rune to land in the path field", got)
	}
	if strings.Contains(m.Text(), "X") {
		t.Errorf("Text()=%q, want the body untouched", m.Text())
	}
	// And back again.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'Y'}})
	if !strings.Contains(m.Text(), "Y") {
		t.Errorf("Text()=%q, want focus back on the body", m.Text())
	}
}

// The path field is single-line, so a tab there has no meaning — it keeps
// the familiar "tab moves to the next field" behaviour.
func TestFileEditTabInNameFieldSwitchesFields(t *testing.T) {
	m := NewFileEdit()
	m.SetSize(80, 24)
	m.OpenNew("abc123", "") // opens focused on the name
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'Z'}})
	if !strings.Contains(m.Text(), "Z") {
		t.Errorf("Text()=%q, want focus to have moved to the body", m.Text())
	}
}
