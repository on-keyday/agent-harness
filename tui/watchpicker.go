package tui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// watchChoice is one way to be told about a task: an await-idle signal, or
// the task's next progress line (await-progress). The two verbs stay separate
// on the command line; here they are one question — "wake me when what?".
type watchChoice struct {
	key      string // the stable name, remembered as the last choice
	label    string
	signal   protocol.AwaitIdleSignal
	progress bool
}

var allWatchChoices = []watchChoice{
	{key: "quiet", label: "output goes quiet", signal: protocol.AwaitIdleSignal_Quiet},
	{key: "lifecycle", label: "the agent hooks say the turn ended", signal: protocol.AwaitIdleSignal_Lifecycle},
	{key: "program", label: "the program's own status says so", signal: protocol.AwaitIdleSignal_Program},
	{key: "any", label: "whichever of those comes first", signal: protocol.AwaitIdleSignal_Any},
	{key: "progress", label: "the next progress line", progress: true},
}

// watchChoices is what a task can be watched for: every idle signal needs a
// live session; the progress line works on any task.
func watchChoices(live bool) []watchChoice {
	if live {
		return allWatchChoices
	}
	return allWatchChoices[len(allWatchChoices)-1:]
}

// watchLive reports whether the task has a session await-idle can watch.
func watchLive(t *protocol.TaskInfo) bool {
	return protocol.IsSessionKind(t.Kind) &&
		(t.Status == protocol.TaskStatus_Running || t.Status == protocol.TaskStatus_Detached)
}

// WatchPicker asks which watcher `w` / `W` arms. The cursor starts on the
// last choice, so repeating it is `w` Enter; a digit picks directly.
// ForwardPicker-shaped.
type WatchPicker struct {
	open       bool
	taskID     string
	sink       protocol.AwaitSink
	progressAt uint64 // await-progress baseline, read when the picker opened
	choices    []watchChoice
	cursor     int
}

func (p *WatchPicker) IsOpen() bool { return p.open }
func (p *WatchPicker) Close()       { p.open = false; p.choices = nil }

// Open shows choices for taskID; last is the previously picked key.
func (p *WatchPicker) Open(taskID string, sink protocol.AwaitSink, choices []watchChoice, last string) {
	*p = WatchPicker{open: true, taskID: taskID, sink: sink, choices: choices}
	for i, c := range choices {
		if c.key == last {
			p.cursor = i
		}
	}
}

// Key handles one key. It reports the picked choice and closes on a pick or
// Esc; any other key moves the cursor or is swallowed.
func (p *WatchPicker) Key(msg tea.KeyMsg) (watchChoice, bool) {
	switch msg.Type {
	case tea.KeyEsc:
		p.Close()
		return watchChoice{}, false
	case tea.KeyUp:
		if p.cursor > 0 {
			p.cursor--
		}
		return watchChoice{}, false
	case tea.KeyDown:
		if p.cursor < len(p.choices)-1 {
			p.cursor++
		}
		return watchChoice{}, false
	case tea.KeyEnter:
		c := p.choices[p.cursor]
		p.Close()
		return c, true
	}
	if s := msg.String(); len(s) == 1 && s[0] >= '1' && int(s[0]-'1') < len(p.choices) {
		c := p.choices[s[0]-'1']
		p.Close()
		return c, true
	}
	return watchChoice{}, false
}

func (p *WatchPicker) View() string {
	if !p.open {
		return ""
	}
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorFocused).Padding(1, 2)
	where := "here"
	if p.sink == protocol.AwaitSink_Notify {
		where = "as a notification"
	}
	body := fmt.Sprintf("Tell me (%s) about %s when…\n\n", where, shortTaskID(p.taskID))
	for i, c := range p.choices {
		mark := "  "
		if i == p.cursor {
			mark = "> "
		}
		body += fmt.Sprintf("%s%d) %-9s  %s\n", mark, i+1, c.key, c.label)
	}
	body += "\n" + FooterStyle.Render("number or ↑↓ Enter · Esc to cancel")
	return box.Render(body)
}
