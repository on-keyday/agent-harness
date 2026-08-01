package tui

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/on-keyday/agent-harness/cli"
)

// FileEditSaveMsg asks App to commit the popup's buffer. Name is the target
// worktree-relative path; it is editable, so it may differ from Doc.Rel —
// that is a save-as and has no baseline to check against.
type FileEditSaveMsg struct {
	TaskID string
	Name   string
	Text   string
	Create bool
	Doc    cli.FileEditDoc
}

// FileEditExternalMsg asks App to run $EDITOR over the popup's buffer. It is
// a message rather than a tea.Exec because tea.Exec must be returned from
// App.Update — the same two-stage shape tui/interactive.go uses.
type FileEditExternalMsg struct {
	Name string
	Text string
}

// fileEditExecDoneMsg lands after tea.Exec returns from the external editor.
type fileEditExecDoneMsg struct {
	path string
	err  error
}

type fileEditFocus int

const (
	fileEditFocusBody fileEditFocus = iota
	fileEditFocusName
)

// FileEditModel is the text-file editor popup: a name field and a body,
// modelled on the submit popup (tui/popup.go), which is the other bubbles
// textarea in this TUI.
//
// Key contract, matching that popup's (tui/app.go:924): the popup swallows
// every key while open. Ctrl+J saves (bubbletea reports Ctrl+Enter as Ctrl+J
// on most terminals), Esc cancels, Tab moves between fields, Ctrl+O hands the
// buffer to $EDITOR, and everything else goes to the focused widget.
//
// Ctrl+O rather than the mnemonic Ctrl+E because bubbles textarea binds
// Ctrl+E to LineEnd (textarea.go:88) and the submit popup has already taken
// it for focus cycling (tui/app.go:960).
type FileEditModel struct {
	open   bool
	create bool
	taskID string
	doc    cli.FileEditDoc

	name  textinput.Model
	body  textarea.Model
	focus fileEditFocus

	status    string
	statusErr bool

	width  int
	height int
}

// NewFileEdit constructs a closed popup.
func NewFileEdit() FileEditModel {
	n := textinput.New()
	n.Placeholder = "worktree-relative path"
	n.Prompt = ""
	b := textarea.New()
	b.Placeholder = "file contents…"
	b.ShowLineNumbers = false
	return FileEditModel{name: n, body: b}
}

// OpenEdit opens the popup over a loaded file.
func (m *FileEditModel) OpenEdit(taskID string, doc cli.FileEditDoc) {
	m.reset(taskID)
	m.create = false
	m.doc = doc
	m.name.SetValue(doc.Rel)
	m.name.CursorEnd()
	m.body.SetValue(doc.Text)
	m.body.Focus()
}

// OpenNew opens the popup for a file that does not exist yet. dir is the
// picker's current directory, seeded into the name field so the operator
// types only the file name.
func (m *FileEditModel) OpenNew(taskID, dir string) {
	m.reset(taskID)
	m.create = true
	m.doc = cli.FileEditDoc{}
	prefix := ""
	if dir != "" {
		prefix = strings.TrimSuffix(dir, "/") + "/"
	}
	m.name.SetValue(prefix)
	m.name.CursorEnd()
	m.body.SetValue("")
	m.body.Focus()
}

func (m *FileEditModel) reset(taskID string) {
	m.open = true
	m.taskID = taskID
	m.focus = fileEditFocusBody
	m.status = ""
	m.statusErr = false
	m.name.Blur()
	m.applySize()
}

// Close tears the popup down.
func (m *FileEditModel) Close() {
	m.open = false
	m.body.Blur()
	m.name.Blur()
}

func (m FileEditModel) IsOpen() bool         { return m.open }
func (m FileEditModel) IsCreate() bool       { return m.create }
func (m FileEditModel) TaskID() string       { return m.taskID }
func (m FileEditModel) Name() string         { return strings.TrimSpace(m.name.Value()) }
func (m FileEditModel) Text() string         { return m.body.Value() }
func (m FileEditModel) Doc() cli.FileEditDoc { return m.doc }

// SetStatus writes the popup's footer line — a commit result, or the reason
// an external editor could not start.
func (m *FileEditModel) SetStatus(msg string, isErr bool) {
	m.status = msg
	m.statusErr = isErr
}

// SetText replaces the body, used when an external editor round trip comes
// back with changed content.
func (m *FileEditModel) SetText(s string) { m.body.SetValue(s) }

// SetName replaces the target path, used when a caller knows the whole path
// rather than just the directory it lands in.
func (m *FileEditModel) SetName(s string) {
	m.name.SetValue(s)
	m.name.CursorEnd()
}

// SetSize records the host viewport and re-lays out the widgets.
func (m *FileEditModel) SetSize(w, h int) {
	m.width, m.height = w, h
	m.applySize()
}

func (m *FileEditModel) applySize() {
	w := m.width - 8
	if w < 20 {
		w = 20
	}
	h := m.height - 10
	if h < 4 {
		h = 4
	}
	m.name.Width = w
	m.body.SetWidth(w)
	m.body.SetHeight(h)
}

// Update handles the popup's own keys and delegates the rest to the focused
// widget. A closed popup is a no-op.
func (m FileEditModel) Update(msg tea.Msg) (FileEditModel, tea.Cmd) {
	if !m.open {
		return m, nil
	}
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		var cmd tea.Cmd
		m.body, cmd = m.body.Update(msg)
		return m, cmd
	}
	switch k.Type {
	case tea.KeyEsc:
		m.Close()
		return m, nil
	case tea.KeyCtrlJ:
		save := FileEditSaveMsg{
			TaskID: m.taskID,
			Name:   m.Name(),
			Text:   m.body.Value(),
			Create: m.create,
			Doc:    m.doc,
		}
		if save.Name == "" {
			m.SetStatus("file name required", true)
			m.focus = fileEditFocusName
			m.body.Blur()
			m.name.Focus()
			return m, nil
		}
		return m, func() tea.Msg { return save }
	case tea.KeyCtrlO:
		ext := FileEditExternalMsg{Name: m.Name(), Text: m.body.Value()}
		return m, func() tea.Msg { return ext }
	case tea.KeyTab:
		if m.focus == fileEditFocusBody {
			m.focus = fileEditFocusName
			m.body.Blur()
			m.name.Focus()
		} else {
			m.focus = fileEditFocusBody
			m.name.Blur()
			m.body.Focus()
		}
		return m, nil
	}
	var cmd tea.Cmd
	if m.focus == fileEditFocusName {
		m.name, cmd = m.name.Update(msg)
	} else {
		m.body, cmd = m.body.Update(msg)
	}
	return m, cmd
}

// View renders the popup box.
func (m FileEditModel) View() string {
	if !m.open {
		return ""
	}
	title := "Edit file"
	if m.create {
		title = "New file"
	}
	parts := []string{
		FocusedStyle.Render(title),
		"path: " + m.name.View(),
		m.body.View(),
	}
	if m.status != "" {
		if m.statusErr {
			parts = append(parts, ErrorStyle.Render(m.status))
		} else {
			parts = append(parts, OKStyle.Render(m.status))
		}
	}
	parts = append(parts, FooterStyle.Render("ctrl+j save · tab field · ctrl+o $EDITOR · esc cancel"))
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorFocused).
		Padding(1, 2)
	return box.Render(strings.Join(parts, "\n"))
}

// writeFileEditTemp spools an editor buffer to a temp file, keeping the
// original extension so an external editor picks its own highlighting.
func writeFileEditTemp(name, text string) (string, error) {
	ext := filepath.Ext(name)
	f, err := os.CreateTemp("", "harness-edit-*"+ext)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// editorExec runs an external editor under tea.Exec and announces the
// suspension on the terminal first.
//
// The announcement cannot live in the popup's View: Program.exec calls
// ReleaseTerminal before Run (bubbletea/exec.go:101), which leaves the alt
// screen, so the last TUI frame is gone by the time the editor starts. What
// stays visible is what the child writes to the primary screen — and
// bubbletea hands us that writer via SetStdout just before calling Run.
//
// For a terminal editor the banner is overpainted immediately and costs
// nothing. For a GUI editor (the Windows case) it stays on screen for as long
// as the window is open, which is the difference between "suspended, on
// purpose" and "hung".
type editorExec struct {
	cmd  *exec.Cmd
	name string // the resolved editor, for the banner
	path string // the temp file being edited
	out  io.Writer
}

func (e *editorExec) SetStdin(r io.Reader)  { e.cmd.Stdin = r }
func (e *editorExec) SetStdout(w io.Writer) { e.cmd.Stdout = w; e.out = w }
func (e *editorExec) SetStderr(w io.Writer) { e.cmd.Stderr = w }

func (e *editorExec) Run() error {
	if e.out != nil {
		// CRLF, not LF: the terminal is still in raw mode here, so a bare
		// newline steps down without returning to column 0.
		fmt.Fprintf(e.out,
			"\r\n  harness-tui: 外部エディタ %s を開いています\r\n"+
				"  編集中: %s\r\n"+
				"  エディタを終了すると TUI に戻ります。\r\n\r\n",
			e.name, e.path)
	}
	return e.cmd.Run()
}
