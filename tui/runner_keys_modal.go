package tui

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// RunnerKeysModal is the TUI's Runner keys screen (K): `runner keys` and
// `runner dial ls` in one list, with x to unenroll a key or remove a dial
// record (y/n), n to enroll a key, a to add a dial record, o to dial a
// listening runner once, r to reload, esc to close. Writes go through the
// cli functions the command line calls, and their lines go to the results
// pane. Unenroll never forces on the first request: an in_use answer
// (cli.ForceOffer) is asked again with force, listing the runners on the key.
type RunnerKeysModal struct {
	open    bool
	mode    rkMode
	keys    []cli.RunnerKeyView
	dials   []cli.RunnerDialView
	loadErr error
	cur     int
	// The pending question: what x asked about, and whether the next yes
	// forces it.
	prompt  string
	pending rkRequest
	force   bool
	// The forms' fields. tasks / runners are the snapshot's, for the enroll
	// binding and the dial-once route.
	tasks   []protocol.TaskInfo
	runners []protocol.RunnerInfo
	row     int
	pubkey  textinput.Model
	label   textinput.Model
	cands   textinput.Model
	cid     textinput.Model
	bind    map[string]bool
	keyIx   int
	viaIx   int
	formErr string
	w, h    int
}

type rkMode int

const (
	rkList rkMode = iota
	rkConfirm
	rkEnroll
	rkDialAdd
	rkDialOnce
)

// rkRequest is what the modal asks the App to send.
type rkRequest struct {
	kind   string // "unenroll", "dial rm", "enroll", "dial add", "dial once", or ""
	ref    string // the key (hex), the dial record id, or the runner cid
	force  bool
	label  string
	cands  string
	via    string
	bound  []protocol.TaskID
	pubkey string
}

// RunnerKeyViewsMsg carries the two lists.
type RunnerKeyViewsMsg struct {
	Keys  []cli.RunnerKeyView
	Dials []cli.RunnerDialView
	Err   error
}

// RunnerOfferMsg is the answer to an unenroll or a kick (Kind), with Ref the
// key or runner it was about, for a forced retry.
type RunnerOfferMsg struct {
	Kind  string
	Ref   string
	Offer cli.ForceOffer
	Err   error
}

func DoRunnerKeyViews(c *cli.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		keys, err := cli.RunnerKeysWith(ctx, c)
		if err != nil {
			return RunnerKeyViewsMsg{Err: err}
		}
		dials, err := cli.RunnerDialListWith(ctx, c)
		return RunnerKeyViewsMsg{Keys: cli.RunnerKeyViews(keys), Dials: cli.RunnerDialViews(dials), Err: err}
	}
}

func DoRunnerUnenrollOffer(c *cli.Client, key string, force bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		st, res, err := cli.RunnerUnenrollWith(ctx, c, key, force)
		if err != nil {
			return RunnerOfferMsg{Kind: "unenroll", Ref: key, Err: err}
		}
		return RunnerOfferMsg{Kind: "unenroll", Ref: key, Offer: cli.UnenrollOffer(st, res)}
	}
}

func DoRunnerKickOffer(c *cli.Client, runner string, force bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		st, res, err := cli.RunnerKickWith(ctx, c, runner, force)
		if err != nil {
			return RunnerOfferMsg{Kind: "kick", Ref: runner, Err: err}
		}
		return RunnerOfferMsg{Kind: "kick", Ref: runner, Offer: cli.KickOffer(st, res)}
	}
}

func NewRunnerKeysModal() RunnerKeysModal {
	in := func(width, limit int, ph string) textinput.Model {
		t := textinput.New()
		t.Width, t.CharLimit, t.Placeholder = width, limit, ph
		return t
	}
	return RunnerKeysModal{
		pubkey: in(66, 64, "64 hex: agent-runner --print-pubkey"),
		label:  in(30, 64, "optional"),
		cands:  in(50, 512, "ws:host:port-*[,…] (transport:host:port-id)"),
		cid:    in(40, 128, "udp:host:7001"),
	}
}

func (m *RunnerKeysModal) IsOpen() bool { return m.open }
func (m *RunnerKeysModal) Close()       { m.open = false }
func (m *RunnerKeysModal) SetSize(w, h int) {
	m.w, m.h = w, h
}

// Open shows the list; tasks and runners are the snapshot's.
func (m *RunnerKeysModal) Open(tasks []protocol.TaskInfo, runners []protocol.RunnerInfo) {
	m.open, m.mode, m.tasks, m.runners, m.prompt, m.force = true, rkList, tasks, runners, "", false
}

func (m *RunnerKeysModal) SetViews(keys []cli.RunnerKeyView, dials []cli.RunnerDialView) {
	m.keys, m.dials, m.loadErr = keys, dials, nil
	if n := len(keys) + len(dials); m.cur >= n {
		m.cur = n - 1
	}
	if m.cur < 0 {
		m.cur = 0
	}
}

func (m *RunnerKeysModal) Move(d int) {
	if n := len(m.keys) + len(m.dials); n > 0 {
		m.cur = (m.cur + d + n) % n
	}
}

// BeginRemove asks about the selected row: unenroll a key, remove a record.
func (m *RunnerKeysModal) BeginRemove() {
	switch {
	case m.cur < len(m.keys):
		k := m.keys[m.cur]
		m.pending = rkRequest{kind: "unenroll", ref: k.Key}
		m.prompt = fmt.Sprintf("unenroll key %s (%s)? a runner holding it can no longer register; its dial records go with it (y/n)", k.Prefix, k.Label)
	case m.cur-len(m.keys) < len(m.dials):
		d := m.dials[m.cur-len(m.keys)]
		m.pending = rkRequest{kind: "dial rm", ref: d.ID}
		m.prompt = fmt.Sprintf("remove dial record %s? the server stops dialing %s (y/n)", d.ID, strings.Join(d.Candidates, ", "))
	default:
		return
	}
	m.force, m.mode = false, rkConfirm
}

// Confirm is the yes to the question: the request to send.
func (m *RunnerKeysModal) Confirm() rkRequest {
	r := m.pending
	r.force = m.force
	return r
}

// ApplyOffer takes an unenroll's answer: in_use asks again with force, naming
// the runners; anything else goes back to the list.
func (m *RunnerKeysModal) ApplyOffer(o cli.ForceOffer) {
	if o.NeedsForce {
		m.force, m.mode = true, rkConfirm
		m.prompt = strings.TrimSpace(o.Text) + " — force it? in the way: " + strings.Join(o.InTheWay, ", ") + " (y/n)"
		return
	}
	m.force, m.mode, m.prompt = false, rkList, ""
}

func (m *RunnerKeysModal) openForm(mode rkMode) {
	m.mode, m.row, m.formErr = mode, 0, ""
	m.pubkey.SetValue("")
	m.label.SetValue("")
	m.cands.SetValue("")
	m.cid.SetValue("")
	m.bind, m.keyIx, m.viaIx = map[string]bool{}, 0, 0
	m.focusRow()
}

func (m *RunnerKeysModal) OpenEnrollForm()   { m.openForm(rkEnroll) }
func (m *RunnerKeysModal) OpenDialForm()     { m.openForm(rkDialAdd) }
func (m *RunnerKeysModal) OpenDialOnceForm() { m.openForm(rkDialOnce) }

// formRows is how many rows the open form has, the last being submit.
func (m *RunnerKeysModal) formRows() int {
	switch m.mode {
	case rkEnroll:
		return 2 + len(m.tasks) + 1
	case rkDialAdd:
		return 4
	case rkDialOnce:
		return 3
	}
	return 0
}

// inputAt is the text field on the current form row, nil on other rows.
func (m *RunnerKeysModal) inputAt() *textinput.Model {
	switch m.mode {
	case rkEnroll:
		switch m.row {
		case 0:
			return &m.pubkey
		case 1:
			return &m.label
		}
	case rkDialAdd:
		switch m.row {
		case 0:
			return &m.cands
		case 2:
			return &m.label
		}
	case rkDialOnce:
		if m.row == 0 {
			return &m.cid
		}
	}
	return nil
}

func (m *RunnerKeysModal) focusRow() {
	for _, in := range []*textinput.Model{&m.pubkey, &m.label, &m.cands, &m.cid} {
		in.Blur()
	}
	if in := m.inputAt(); in != nil {
		in.Focus()
	}
}

// formProblem is why the form cannot be sent: what the server would refuse.
func (m *RunnerKeysModal) formProblem() string {
	switch m.mode {
	case rkEnroll:
		if b, err := hex.DecodeString(strings.TrimSpace(m.pubkey.Value())); err != nil || len(b) != 32 {
			return "public key: 64 hex characters (agent-runner --print-pubkey)"
		}
	case rkDialAdd:
		if strings.TrimSpace(m.cands.Value()) == "" {
			return "at least one candidate address"
		}
		if len(m.keys) == 0 {
			return "an enrolled key: none is enrolled — n enrolls one"
		}
	case rkDialOnce:
		if strings.TrimSpace(m.cid.Value()) == "" {
			return "the runner's address"
		}
	}
	return ""
}

// formRequest is the open form as a request.
func (m *RunnerKeysModal) formRequest() rkRequest {
	switch m.mode {
	case rkEnroll:
		r := rkRequest{kind: "enroll", pubkey: strings.TrimSpace(m.pubkey.Value()), label: m.label.Value()}
		for _, t := range m.tasks {
			if h := FormatTaskID(t.Id); m.bind[h] {
				r.bound = append(r.bound, t.Id)
			}
		}
		return r
	case rkDialAdd:
		r := rkRequest{kind: "dial add", cands: m.cands.Value(), label: m.label.Value()}
		if m.keyIx < len(m.keys) {
			r.ref = m.keys[m.keyIx].Key
		}
		return r
	case rkDialOnce:
		r := rkRequest{kind: "dial once", ref: strings.TrimSpace(m.cid.Value())}
		if m.viaIx > 0 && m.viaIx-1 < len(m.runners) {
			r.via = hex.EncodeToString(m.runners[m.viaIx-1].Id.Id[:])
		}
		return r
	}
	return rkRequest{}
}

// UpdateForm applies one key to the open form; a request when submitted.
func (m *RunnerKeysModal) UpdateForm(msg tea.KeyMsg) rkRequest {
	n := m.formRows()
	switch msg.String() {
	case "up", "shift+tab":
		if m.row > 0 {
			m.row--
		}
		m.focusRow()
		return rkRequest{}
	case "down", "tab":
		if m.row < n-1 {
			m.row++
		}
		m.focusRow()
		return rkRequest{}
	}
	if m.row == n-1 {
		if msg.Type == tea.KeyEnter {
			if p := m.formProblem(); p != "" {
				m.formErr = p
				return rkRequest{}
			}
			return m.formRequest()
		}
		return rkRequest{}
	}
	if in := m.inputAt(); in != nil {
		*in, _ = in.Update(msg)
		m.formErr = ""
		return rkRequest{}
	}
	cycle := func(at, size int) int {
		if size == 0 {
			return 0
		}
		switch msg.String() {
		case "right", " ":
			return (at + 1) % size
		case "left":
			return (at + size - 1) % size
		}
		return at
	}
	switch m.mode {
	case rkEnroll:
		if i := m.row - 2; i >= 0 && i < len(m.tasks) && msg.String() == " " {
			h := FormatTaskID(m.tasks[i].Id)
			m.bind[h] = !m.bind[h]
		}
	case rkDialAdd:
		if m.row == 1 {
			m.keyIx = cycle(m.keyIx, len(m.keys))
		}
	case rkDialOnce:
		if m.row == 1 {
			m.viaIx = cycle(m.viaIx, len(m.runners)+1)
		}
	}
	return rkRequest{}
}

func (m *RunnerKeysModal) View() string {
	var b strings.Builder
	mark := func(on bool) string {
		if on {
			return "▸ "
		}
		return "  "
	}
	switch m.mode {
	case rkEnroll, rkDialAdd, rkDialOnce:
		title := map[rkMode]string{rkEnroll: "runner enroll", rkDialAdd: "runner dial add", rkDialOnce: "server dial-runner"}[m.mode]
		b.WriteString(FocusedStyle.Render(title) + "\n\n")
		switch m.mode {
		case rkEnroll:
			b.WriteString(mark(m.row == 0) + "public key  " + m.pubkey.View() + "\n")
			b.WriteString(mark(m.row == 1) + "label       " + m.label.View() + "\n")
			b.WriteString(MutedStyle.Render("  bind (space): none checked = unbound, the runner takes any task") + "\n")
			for i, t := range m.tasks {
				h := FormatTaskID(t.Id)
				box := "[ ]"
				if m.bind[h] {
					box = "[x]"
				}
				b.WriteString(mark(m.row == i+2) + box + " " + h[:8] + " " + taskStatusStr(t.Status) + " " + taskAgentCell(t) + "\n")
			}
		case rkDialAdd:
			key := "(none enrolled — n enrolls one)"
			if m.keyIx < len(m.keys) {
				key = m.keys[m.keyIx].Prefix + " " + m.keys[m.keyIx].Label
			}
			b.WriteString(mark(m.row == 0) + "candidates  " + m.cands.View() + "\n")
			b.WriteString(mark(m.row == 1) + "key         ◂ " + key + " ▸\n")
			b.WriteString(mark(m.row == 2) + "label       " + m.label.View() + "\n")
		case rkDialOnce:
			via := "direct (the server dials it)"
			if m.viaIx > 0 && m.viaIx-1 < len(m.runners) {
				r := m.runners[m.viaIx-1]
				via = "via " + string(r.Hostname) + " (" + hex.EncodeToString(r.Id.Id[:4]) + ")"
			}
			b.WriteString(mark(m.row == 0) + "runner      " + m.cid.View() + "\n")
			b.WriteString(mark(m.row == 1) + "route       ◂ " + via + " ▸\n")
		}
		b.WriteString(mark(m.row == m.formRows()-1) + "[ send ]\n")
		if m.formErr != "" {
			b.WriteString(ErrorStyle.Render(m.formErr) + "\n")
		}
		b.WriteString(MutedStyle.Render("↑/↓ move · type to edit · ←/→ or space choose · enter on [ send ] · esc back"))
	default:
		b.WriteString(FocusedStyle.Render("Runner keys") + "\n\n")
		if m.loadErr != nil {
			b.WriteString(ErrorStyle.Render(m.loadErr.Error()) + "\n")
		}
		b.WriteString(fmt.Sprintf("keys=%d\n", len(m.keys)))
		for i, k := range m.keys {
			bound := "unbound"
			if len(k.Bound) > 0 {
				ids := make([]string, len(k.Bound))
				for j, id := range k.Bound {
					ids[j] = id[:8]
				}
				bound = "bound=" + strings.Join(ids, ",")
			}
			b.WriteString(fmt.Sprintf("%s%s label=%s by=%s %s runners=%d", mark(i == m.cur), k.Prefix, k.Label, k.EnrolledBy, bound, len(k.Runners)))
			if len(k.Runners) > 0 {
				b.WriteString(": " + strings.Join(k.Runners, ", "))
			}
			b.WriteString("\n")
		}
		b.WriteString(fmt.Sprintf("dial records=%d\n", len(m.dials)))
		for i, d := range m.dials {
			line := fmt.Sprintf("%s%s key=%s label=%s state=%s candidates=%s", mark(len(m.keys)+i == m.cur),
				d.ID, d.KeyPrefix, d.Label, d.State, strings.Join(d.Candidates, ","))
			if d.LastError != "" {
				line += " last_error=" + d.LastError
			}
			b.WriteString(line + "\n")
		}
		if m.mode == rkConfirm {
			b.WriteString(WarnStyle.Render(m.prompt) + "\n")
		}
		b.WriteString(MutedStyle.Render("↑/↓ select · x unenroll / remove · n enroll · a add dial record · o dial once · r reload · esc close"))
	}
	w := m.w - 4
	if w > 120 {
		w = 120
	}
	if w < 40 {
		w = 40
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1).Width(w).Render(b.String())
}

// kickQuestion is the runners table's pending kick: asked plainly first, and
// with force only after the server answered in_use.
type kickQuestion struct {
	ref    string // the runner's identity, hex
	prompt string
	force  bool
}

// askKick puts the plain question for r.
func (a *App) askKick(r protocol.RunnerInfo) {
	id := hex.EncodeToString(r.Id.Id[:])
	a.kickAsk = &kickQuestion{ref: id, prompt: fmt.Sprintf("kick runner %s (%s)? it ends its tasks and exits (y/n)", id[:8], r.Hostname)}
	a.cmdresult.Append(WarnStyle.Render(a.kickAsk.prompt))
}

// applyKickOffer takes a kick's answer: in_use asks again with force,
// listing the runner's tasks; anything else ends the question.
func (a *App) applyKickOffer(msg RunnerOfferMsg) {
	if msg.Err != nil {
		a.cmdresult.Append(ErrorStyle.Render("runner kick: " + msg.Err.Error()))
		a.kickAsk = nil
		return
	}
	text := strings.TrimSpace(msg.Offer.Text)
	if msg.Offer.NeedsForce {
		a.kickAsk = &kickQuestion{ref: msg.Ref, force: true,
			prompt: text + " — force it? its tasks: " + strings.Join(msg.Offer.InTheWay, ", ") + " (y/n)"}
		a.cmdresult.Append(WarnStyle.Render(a.kickAsk.prompt))
		return
	}
	a.kickAsk = nil
	if msg.Offer.OK {
		a.cmdresult.Append(OKStyle.Render(text))
	} else {
		a.cmdresult.Append(ErrorStyle.Render(text))
	}
}
