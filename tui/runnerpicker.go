package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/on-keyday/agent-harness/cli"
)

// pickerRetry re-issues the request that came back ambiguous, pinned to what
// the operator picked. It travels with the failing request's own result
// message, so a picker can never re-issue some other request. c is the client
// the TUI holds AT PICK TIME: a reconnect between the answer and the keypress
// replaces it (BindClientMsg), and the one the request went out on is dead.
type pickerRetry func(c *cli.Client, sel cli.SelectorOpts, agent string) tea.Cmd

// RunnerPickerModel is shown when a spawn comes back ambiguous_runner. Step 1
// picks a runner, step 2 an agent on it; a step with one choice is skipped.
// Digits pick directly, j/k + Enter reach any row — a runner advertising every
// preset offers more agents than there are digits.
type RunnerPickerModel struct {
	open   bool
	cands  []cli.RunnerCandidate
	retry  pickerRetry
	runner int // -1 while in step 1
	cursor int
	// picked labels the last re-issue, for the caller's result line. Set
	// after Close so it survives it; Key hands it out once.
	picked string
}

func (p *RunnerPickerModel) IsOpen() bool { return p.open }

func (p *RunnerPickerModel) Close() { *p = RunnerPickerModel{} }

// Open starts a pick. It returns the retry's Cmd straight away when there is
// nothing to choose.
func (p *RunnerPickerModel) Open(cands []cli.RunnerCandidate, retry pickerRetry, c *cli.Client) tea.Cmd {
	*p = RunnerPickerModel{open: true, cands: cands, retry: retry, runner: -1}
	if len(cands) == 1 {
		return p.chooseRunner(0, c)
	}
	return nil
}

func (p *RunnerPickerModel) rows() int {
	if p.runner < 0 {
		return len(p.cands)
	}
	return len(p.cands[p.runner].Profiles)
}

func (p *RunnerPickerModel) chooseRunner(i int, client *cli.Client) tea.Cmd {
	p.runner, p.cursor = i, 0
	if len(p.cands[i].Profiles) == 1 {
		return p.chooseAgent(0, client)
	}
	return nil
}

func (p *RunnerPickerModel) chooseAgent(i int, client *cli.Client) tea.Cmd {
	c := p.cands[p.runner]
	cmd := p.retry(client, c.Pin(), c.Profiles[i])
	p.Close()
	p.picked = c.Hostname + "  " + c.Cid + "  (" + agentLabels(c.Profiles[i : i+1])[0] + ")"
	return cmd
}

func (p *RunnerPickerModel) choose(i int, client *cli.Client) tea.Cmd {
	if p.runner < 0 {
		return p.chooseRunner(i, client)
	}
	return p.chooseAgent(i, client)
}

// Key handles one key while open. client is the TUI's current one, which a
// pick re-issues over. The label is non-empty when the key re-issued the
// request, naming what was picked (the caller logs it).
func (p *RunnerPickerModel) Key(key string, client *cli.Client) (tea.Cmd, string) {
	cmd, picked := p.key(key, client)
	if !picked {
		return cmd, ""
	}
	label := p.picked
	p.picked = ""
	return cmd, label
}

func (p *RunnerPickerModel) key(key string, client *cli.Client) (tea.Cmd, bool) {
	switch key {
	case "esc":
		if p.runner >= 0 && len(p.cands) > 1 {
			p.runner, p.cursor = -1, 0
			return nil, false
		}
		p.Close()
		return nil, false
	case "j", "down":
		if p.cursor < p.rows()-1 {
			p.cursor++
		}
		return nil, false
	case "k", "up":
		if p.cursor > 0 {
			p.cursor--
		}
		return nil, false
	case "enter":
		cmd := p.choose(p.cursor, client)
		return cmd, !p.open
	}
	if len(key) == 1 && key[0] >= '1' && key[0] <= '9' {
		if i := int(key[0] - '1'); i < p.rows() {
			cmd := p.choose(i, client)
			return cmd, !p.open
		}
	}
	return nil, false
}

func (p *RunnerPickerModel) View() string {
	if !p.open {
		return ""
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorFocused).
		Padding(1, 2)
	var b strings.Builder
	mark := func(i int) string {
		if i == p.cursor {
			return "›"
		}
		return " "
	}
	if p.runner < 0 {
		b.WriteString("Ambiguous runner — pick one:\n\n")
		for i, c := range p.cands {
			fmt.Fprintf(&b, "%s %d) %-16s · %s · %s  [%d/%d]  %s\n", mark(i), i+1, c.Hostname, c.MatchedRoot,
				c.Cid, c.ActiveTasks, c.MaxTasks, strings.Join(agentLabels(c.Profiles), " "))
		}
	} else {
		c := p.cands[p.runner]
		fmt.Fprintf(&b, "Agent on %s — pick one:\n\n", c.Hostname)
		for i, a := range agentLabels(c.Profiles) {
			fmt.Fprintf(&b, "%s %d) %s\n", mark(i), i+1, a)
		}
	}
	b.WriteString("\n" + FooterStyle.Render("number or j/k+Enter to pick · Esc back/cancel"))
	return box.Render(b.String())
}

// agentLabels names each offered profile, showing a legacy runner's unnamed
// one as "(default)".
func agentLabels(profiles []string) []string {
	out := make([]string, len(profiles))
	for i, a := range profiles {
		if a == "" {
			a = "(default)"
		}
		out[i] = a
	}
	return out
}
