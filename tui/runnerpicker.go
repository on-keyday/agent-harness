package tui

import (
	"fmt"

	"github.com/charmbracelet/lipgloss"
	"github.com/on-keyday/agent-harness/cli"
)

// RunnerPickerModel is a digit-select popup shown when an interactive open
// returns AmbiguousRunner: it lists the candidate (runner, profile) combos so
// the user picks one with a single keypress. Mirrors ForwardPicker. Per the
// multi-agent-profile design (§4a), the candidate unit is a (runner, profile)
// pair, not a bare runner — picking a row pins both the runner (Cid) and the
// agent (Profile) for the re-issued open.
type RunnerPickerModel struct {
	open   bool
	combos []cli.RunnerCombo
}

func (p *RunnerPickerModel) IsOpen() bool { return p.open }

func (p *RunnerPickerModel) Open(cands []cli.RunnerCandidate) {
	p.open = true
	p.combos = cli.FlattenCandidates(cands)
}

func (p *RunnerPickerModel) Close() { p.open = false; p.combos = nil }

// Pick maps a digit key ("1".."9") to a candidate, or nil if out of range /
// not a digit.
func (p *RunnerPickerModel) Pick(key string) *cli.RunnerCombo {
	if len(key) != 1 || key[0] < '1' || key[0] > '9' {
		return nil
	}
	idx := int(key[0] - '1')
	if idx >= len(p.combos) {
		return nil
	}
	return &p.combos[idx]
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
	for i, combo := range p.combos {
		c := combo.Candidate
		profile := combo.Profile
		if profile == "" {
			// Defensive fallback: every real combo from the server's
			// expandCombos already carries a resolved profile
			// (DefaultProfile/AgentBin), so this only fires for a
			// legacy/malformed response.
			profile = "(default)"
		}
		body += fmt.Sprintf("%d) %-16s · %-10s · %s · %s  [%d/%d]\n",
			i+1, c.Hostname, profile, c.MatchedRoot, c.Cid, c.ActiveTasks, c.MaxTasks)
	}
	body += "\n" + FooterStyle.Render("press number to pick · Esc to cancel")
	return box.Render(body)
}
