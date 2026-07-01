package tui

import (
	"fmt"

	"github.com/charmbracelet/lipgloss"
	"github.com/on-keyday/agent-harness/cli"
)

// RunnerPickerModel is a digit-select popup shown when an interactive open
// returns AmbiguousRunner: it lists the candidate runners so the user picks one
// with a single keypress. Mirrors ForwardPicker.
type RunnerPickerModel struct {
	open       bool
	candidates []cli.RunnerCandidate
}

func (p *RunnerPickerModel) IsOpen() bool { return p.open }

func (p *RunnerPickerModel) Open(cands []cli.RunnerCandidate) {
	p.open = true
	p.candidates = cands
}

func (p *RunnerPickerModel) Close() { p.open = false; p.candidates = nil }

// Pick maps a digit key ("1".."9") to a candidate, or nil if out of range /
// not a digit.
func (p *RunnerPickerModel) Pick(key string) *cli.RunnerCandidate {
	if len(key) != 1 || key[0] < '1' || key[0] > '9' {
		return nil
	}
	idx := int(key[0] - '1')
	if idx >= len(p.candidates) {
		return nil
	}
	return &p.candidates[idx]
}

func (p *RunnerPickerModel) View() string {
	if !p.open {
		return ""
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorFocused).
		Padding(1, 2)
	body := "Ambiguous runner — pick one:\n\n"
	for i, c := range p.candidates {
		body += fmt.Sprintf("%d) %-16s [%d/%d]  %s  %s\n",
			i+1, c.Hostname, c.ActiveTasks, c.MaxTasks, c.MatchedRoot, c.Cid)
	}
	body += "\n" + FooterStyle.Render("press number to pick · Esc to cancel")
	return box.Render(body)
}
