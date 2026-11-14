package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// RecordKeepForm changes one PTY record's retention from the d popup
// (`record keep`, A-D26/A-D28). The run is picked from the records the popup
// lists, so a run that has no record cannot be named; the retention is typed,
// "forever" or a duration, and read by cli.ParseRecordRetention — the same
// reading the WebUI's task sheet gets through the wasm bridge.
type RecordKeepForm struct {
	open    bool
	taskID  string
	runs    []uint32
	idx     int
	input   textinput.Model
	err     string
	pending bool
}

func NewRecordKeepForm() RecordKeepForm {
	in := textinput.New()
	in.Placeholder = "forever / 168h / 30m"
	in.CharLimit = 32
	return RecordKeepForm{input: in}
}

// Open starts the form on taskID's latest listed run.
func (f *RecordKeepForm) Open(taskID string, rs []protocol.RecordInfo) {
	f.open, f.taskID, f.err, f.pending = true, taskID, "", false
	f.runs = f.runs[:0]
	for _, r := range rs {
		f.runs = append(f.runs, r.Run)
	}
	f.idx = 0
	for i, r := range f.runs {
		if r > f.runs[f.idx] {
			f.idx = i
		}
	}
	f.input.SetValue("")
	f.input.Focus()
}

func (f *RecordKeepForm) IsOpen() bool  { return f.open }
func (f *RecordKeepForm) Close()        { f.open, f.pending = false, false }
func (f *RecordKeepForm) Pending() bool { return f.pending }

// Ref is the record the form would change, `<task-id>#<run>`.
func (f *RecordKeepForm) Ref() string {
	if len(f.runs) == 0 {
		return f.taskID
	}
	return fmt.Sprintf("%s#%d", f.taskID, f.runs[f.idx])
}

// Step moves to the previous (-1) or next (+1) listed run, in list order.
func (f *RecordKeepForm) Step(d int) {
	if n := len(f.runs); n > 0 {
		f.idx = (f.idx + d + n) % n
	}
}

// Values reads the typed retention.
func (f *RecordKeepForm) Values() (forever bool, ttl time.Duration, err error) {
	return cli.ParseRecordRetention(f.input.Value())
}

func (f *RecordKeepForm) Submitted() { f.pending, f.err = true, "" }

// Answered ends a send: a refusal stays on the form with what was typed.
func (f *RecordKeepForm) Answered(err error) {
	f.pending = false
	if err != nil {
		f.err = err.Error()
	}
}

func (f *RecordKeepForm) Update(msg tea.KeyMsg) tea.Cmd {
	var cmd tea.Cmd
	f.input, cmd = f.input.Update(msg)
	return cmd
}

func (f *RecordKeepForm) View() string {
	if !f.open {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(HeaderStyle.Render("Record retention") + "\n\n")
	sb.WriteString("record:     " + f.Ref() + "\n")
	sb.WriteString("keep for:   " + f.input.View() + "\n")
	sb.WriteString(FooterStyle.Render("counted from the run's end; shorter needs record_delete, longer or forever record_persist") + "\n")
	switch {
	case f.pending:
		sb.WriteString("\nsending…\n")
	case f.err != "":
		sb.WriteString("\n" + ErrorStyle.Render(f.err) + "\n")
	}
	sb.WriteString("\n" + FooterStyle.Render("↑/↓: run · Enter: apply · Esc: cancel"))
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorFocused).Padding(1, 2).Render(sb.String())
}

// RecordKeepResultMsg answers the keep form.
type RecordKeepResultMsg struct {
	Ref  string
	Text string
	Err  error
}

// recordKeepCmd sends one `record keep`; a variable so a test can see what
// the form sends without a server.
var recordKeepCmd = func(c *cli.Client, ref string, forever bool, ttl time.Duration) tea.Cmd {
	return func() tea.Msg {
		if c == nil {
			return RecordKeepResultMsg{Ref: ref, Err: errors.New("record keep: not connected")}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		var out strings.Builder
		err := cli.RecordKeepWith(ctx, c, ref, forever, ttl, &out)
		return RecordKeepResultMsg{Ref: ref, Text: strings.TrimSpace(out.String()), Err: err}
	}
}
