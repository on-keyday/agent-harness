package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/agentlog"
	"github.com/on-keyday/agent-harness/runner/streamagent"
)

const chatLineLimit = 400 // transcript ring: plenty of scrollback, bounded memory

// chatLine is one transcript entry: PLAIN text plus how to paint it.
//
// The text stays unstyled in the model because wrapping measures display
// width, and an ANSI escape counts as printable to runewidth — styling before
// wrapping would make every coloured line wrap short. kscale carries an
// ansiWrap for this; keeping the style beside the text instead means the wrap
// never sees an escape at all.
type chatLine struct {
	text  string
	style lipgloss.Style
}

type chatMode int

const (
	chatModeNormal chatMode = iota
	// chatModeDenyReason collects the deny message. It is a MODE rather than a
	// prompt because the reason reaches the agent verbatim as a failed tool
	// result, so it is composed deliberately, not typed into the turn box by
	// accident.
	chatModeDenyReason
	// chatModeQuestionText collects free text for the current question, and
	// chatModeQuestionReply a freeform reply that answers none in particular.
	chatModeQuestionText
	chatModeQuestionReply
)

// questionHint is the key row of a pending question; approvalHint an
// approval's.
const (
	questionHint = "tab next · 1-9 pick · o other · r reply · enter send · d deny · esc interrupt"
	approvalHint = "a allow · d deny · esc interrupt"
)

// streamDriver is what drives one event-stream session from the keyboard: the
// transcript, the turn input, the pending request and the answer being built
// for it, and the key handler that turns keys into protocol messages.
//
// It is shared by the two TUI surfaces that drive this kind — the chat overlay
// (ChatModel) and a grid pane (StreamPane) — so a key does the same thing in
// both. What each surface adds is only what is about the SURFACE: how it is
// entered and left, and how much room it has. Leaving is Ctrl+] on both and is
// handled by the surface; Esc is never "leave" here, it interrupts the agent,
// as Esc does in an agent's own UI behind a PTY pane.
type streamDriver struct {
	// width is the frame the driver renders into; gutter is the blank margin it
	// keeps on each side (the chat's two spaces; a pane has its own border).
	width  int
	height int
	gutter int

	sess *cli.StreamSession

	input textinput.Model
	mode  chatMode

	lines     []chatLine
	scroll    int
	pending   *streamagent.Request
	busy      bool
	busySince time.Time
	status    string

	// The answer being built for a pending QUESTION request (pending with
	// Questions): which question has focus, the options picked per question,
	// free text per question, and a freeform reply.
	qCur   int
	qSel   map[int]map[int]bool
	qText  map[int]string
	qReply string
}

// reset clears everything a fresh attach would rebuild.
func (m *streamDriver) reset() {
	m.lines = nil
	m.scroll = 0
	m.pending = nil
	m.resetQuestion()
	m.busy = false
	m.mode = chatModeNormal
	m.restoreInput()
}

func (m *streamDriver) setBusy() {
	m.busy = true
	m.busySince = time.Now()
}

// elapsed is how long the current turn has run, in whole seconds.
func (m streamDriver) elapsed() int {
	if !m.busy || m.busySince.IsZero() {
		return 0
	}
	return int(time.Since(m.busySince) / time.Second)
}

func (m streamDriver) isQuestion() bool {
	return m.pending != nil && len(m.pending.Questions) > 0
}

func (m *streamDriver) resetQuestion() {
	m.qCur, m.qSel, m.qText, m.qReply = 0, map[int]map[int]bool{}, map[int]string{}, ""
}

// questionAnswers is the answer as the protocol carries it: question text →
// the picked labels in option order, then any free text.
func (m streamDriver) questionAnswers() map[string][]string {
	out := map[string][]string{}
	if !m.isQuestion() {
		return out
	}
	for i, q := range m.pending.Questions {
		var vals []string
		for j, o := range q.Options {
			if m.qSel[i][j] {
				vals = append(vals, o.Label)
			}
		}
		if t := strings.TrimSpace(m.qText[i]); t != "" {
			vals = append(vals, t)
		}
		if len(vals) > 0 {
			out[q.Question] = vals
		}
	}
	return out
}

// pickOption toggles option j of the current question; on a single-select
// question it replaces the choice, free text included.
func (m *streamDriver) pickOption(j int) {
	q := m.pending.Questions[m.qCur]
	if j < 0 || j >= len(q.Options) {
		return
	}
	sel := m.qSel[m.qCur]
	if sel == nil {
		sel = map[int]bool{}
		m.qSel[m.qCur] = sel
	}
	if q.MultiSelect {
		sel[j] = !sel[j]
		return
	}
	m.qSel[m.qCur] = map[int]bool{j: true}
	delete(m.qText, m.qCur)
}

// buildQuestionAnswer answers the pending question, or returns nil (with the
// reason on the status line) while it is not yet complete.
func (m *streamDriver) buildQuestionAnswer() *streamagent.Msg {
	if !m.isQuestion() {
		return nil
	}
	answers := m.questionAnswers()
	if !streamagent.QuestionComplete(*m.pending, answers, m.qReply) {
		m.status = "answer every question (or r for a reply) · " + questionHint
		return nil
	}
	resp := streamagent.AnswerResponse(m.pending.ID, answers, m.qReply)
	var parts []string
	for _, q := range m.pending.Questions {
		if v := answers[q.Question]; len(v) > 0 {
			key := q.Header
			if key == "" {
				key = q.Question
			}
			parts = append(parts, key+"="+strings.Join(v, ", "))
		}
	}
	if m.qReply != "" {
		parts = append(parts, "reply: "+m.qReply)
	}
	m.appendStyled(OKStyle, "▶ answered: "+strings.Join(parts, "; "))
	m.pending = nil
	m.resetQuestion()
	m.setBusy()
	m.status = "resuming…"
	return &streamagent.Msg{Kind: streamagent.KindResponse, Response: &resp}
}

// enterQuestionEditor swaps the input for the free-text or reply editor,
// prefilled with what was typed before.
func (m *streamDriver) enterQuestionEditor(mode chatMode) {
	m.mode = mode
	ti := textinput.New()
	if mode == chatModeQuestionReply {
		ti.Prompt = "reply ▶ "
		ti.Placeholder = "a reply that answers none of the questions in particular"
		ti.SetValue(m.qReply)
	} else {
		q := m.pending.Questions[m.qCur]
		label := q.Header
		if label == "" {
			label = "other"
		}
		ti.Prompt = label + " ▶ "
		ti.Placeholder = "your own answer to: " + q.Question
		ti.SetValue(m.qText[m.qCur])
	}
	ti.Width = m.inputWidth()
	ti.Focus()
	m.input = ti
	m.status = "enter keeps it · esc goes back"
}

// restoreInput reinstalls the normal turn prompt. EVERY exit from a sub-mode
// must call it: kscale recorded the trap this closes — a sub-mode that swaps
// the input and does not put it back leaves the wrong prompt stuck forever.
func (m *streamDriver) restoreInput() {
	ti := textinput.New()
	ti.Prompt = agentlog.UserTurnPrefix
	ti.Placeholder = "message the agent…"
	ti.Width = m.inputWidth()
	ti.Focus()
	m.input = ti
}

// inputWidth keeps the prompt inside the frame. textinput scrolls its own
// content horizontally once it has a Width; without one it renders the whole
// value and wraps, taking the bottom of the view with it.
func (m streamDriver) inputWidth() int {
	if w := m.rowWidth() - 8; w > 8 {
		return w
	}
	return 8
}

// chatRows expands logical transcript lines into the PHYSICAL rows a terminal
// will show: embedded newlines become rows, and anything wider than the frame
// is soft-wrapped through wrapSegments (this package's own helper — east-asian
// aware, and it never splits a wide rune).
//
// The view MUST clip on these, not on m.lines. A transcript line holds an
// agent's whole multi-paragraph answer or a 200-byte tool result, so N logical
// lines are many more than N rows, and clipping on the logical count let the
// chat render straight past the bottom of harness-tui. kscale's chatView does
// this expansion before its own clip; borrowing the shape without it is what
// broke.
func chatRows(lines []chatLine, width int) []chatLine {
	if width < 1 {
		width = 1
	}
	var out []chatLine
	for _, l := range lines {
		for _, part := range strings.Split(l.text, "\n") {
			r := []rune(part)
			if len(r) == 0 {
				out = append(out, chatLine{style: l.style})
				continue
			}
			starts := wrapSegments(r, width)
			for i, st := range starts {
				end := len(r)
				if i+1 < len(starts) {
					end = starts[i+1]
				}
				out = append(out, chatLine{text: string(r[st:end]), style: l.style})
			}
		}
	}
	return out
}

// rowWidth is the frame the transcript wraps into: the width less the gutter on
// both sides.
func (m streamDriver) rowWidth() int {
	if w := m.width - 2*m.gutter; w > 0 {
		return w
	}
	return 1
}

// scrollBy moves the transcript view up (delta > 0) or back toward the tail,
// clamped against the CURRENT row count so it cannot run off the top of a
// short transcript.
func (m *streamDriver) scrollBy(delta int) {
	total := len(chatRows(m.lines, m.rowWidth()))
	m.scroll += delta
	if max := total - 1; m.scroll > max {
		m.scroll = max
	}
	if m.scroll < 0 {
		m.scroll = 0
	}
}

func (m *streamDriver) appendLine(l string) { m.appendStyled(lipgloss.NewStyle(), l) }

func (m *streamDriver) appendStyled(st lipgloss.Style, l string) {
	// Anchoring is in PHYSICAL rows because that is what scroll counts. A
	// wrapped arrival that bumped it by one would slide the window a little
	// every time, which reads as drift rather than as a bug.
	if m.scroll > 0 {
		m.scroll += len(chatRows([]chatLine{{text: l}}, m.rowWidth()))
	}
	m.lines = append(m.lines, chatLine{text: l, style: st})
	if len(m.lines) > chatLineLimit {
		m.lines = m.lines[len(m.lines)-chatLineLimit:]
	}
}

// applyLine folds one stream line into the view.
//
// Every line but a request goes through cli.LineDisplay, whose event text is
// streamagent.RenderText — the SAME renderer `session stream attach` uses. The REQUEST
// case is the one that deliberately differs: RenderText's one-liner is a
// notification, and this view needs the payload.
func (m *streamDriver) applyLine(line cli.StreamLine) {
	if line.Decoded && line.Msg.Kind == streamagent.KindRequest {
		if line.Msg.Request == nil {
			return
		}
		req := *line.Msg.Request
		// The same request again is a replay after a reattach, not a new one:
		// an answer half-built for it survives.
		same := m.pending != nil && m.pending.ID == req.ID
		m.pending = &req
		m.busy = false
		if len(req.Questions) > 0 {
			if !same {
				m.resetQuestion()
			}
			m.appendStyled(WarnStyle, streamagent.QuestionSummary(req))
			m.status = questionHint
			return
		}
		m.appendStyled(WarnStyle, "⚑ approval needed: "+req.Tool+"  ("+req.ID+")")
		m.status = approvalHint
		return
	}
	// Everything but a request is decided once, in cli.LineDisplay: the line
	// (a non-protocol one escaped), its tone, and what it does to the status
	// line. The WebUI chat reads the same Display over the wasm bridge, so the
	// two chats cannot classify one message two ways.
	d, ok := cli.LineDisplay(line)
	if !ok {
		return
	}
	if d.Text != "" {
		m.appendStyled(toneStyle(d.Tone), d.Text)
	}
	if d.SetStatus {
		m.status = d.Status
	}
	if d.Idle {
		m.busy = false
	}
	if d.Resolves != "" && m.pending != nil && m.pending.ID == d.Resolves {
		// Settled, wherever the answer came from. A deny reason still being
		// typed for it has nothing left to answer.
		if m.mode != chatModeNormal {
			m.cancelSubMode()
		}
		m.pending = nil
		m.resetQuestion()
		m.status = ""
	}
}

// toneStyle is this surface's paint for a streamagent.Tone. The WebUI's is the
// `c-<tone>` CSS class; the classification itself lives in streamagent.
func toneStyle(t streamagent.Tone) lipgloss.Style {
	switch t {
	case streamagent.ToneText:
		return lipgloss.NewStyle()
	case streamagent.ToneYou:
		return OKStyle
	case streamagent.ToneWarn:
		return WarnStyle
	case streamagent.ToneErr:
		return ErrorStyle
	case streamagent.ToneRaw:
		return WarnStyle
	}
	return MutedStyle
}

// buildApproval answers the pending request, clearing it. Returns nil when
// nothing is pending, so a stray keypress cannot send an answer with no id —
// the id is the staleness guard, and an answer without one is refused anyway.
func (m *streamDriver) buildApproval(b streamagent.Behavior, message string) *streamagent.Msg {
	if m.pending == nil {
		return nil
	}
	resp := streamagent.Response{ID: m.pending.ID, Behavior: b, Message: message}
	echo := approvalEcho(b, m.pending.Tool, message)
	if b == streamagent.BehaviorDeny {
		m.appendStyled(ErrorStyle, echo)
	} else {
		m.appendStyled(OKStyle, echo)
	}
	m.pending = nil
	m.setBusy()
	m.status = "resuming…"
	return &streamagent.Msg{Kind: streamagent.KindResponse, Response: &resp}
}

// buildSuggestion accepts the request's nth suggestion alongside allowing this
// call. A suggestion is a STANDING change ("stop asking"), which is why it is
// a separate act from a plain allow even though one message carries both.
func (m *streamDriver) buildSuggestion(n int) *streamagent.Msg {
	if m.pending == nil || n < 0 || n >= len(m.pending.Suggestions) {
		return nil
	}
	idx := n
	s := m.pending.Suggestions[n]
	resp := streamagent.Response{
		ID: m.pending.ID, Behavior: streamagent.BehaviorAllow, AcceptSuggestion: &idx,
	}
	label := s.Type
	if s.Mode != "" {
		label += " " + s.Mode
	}
	m.appendStyled(OKStyle, "▶ allow + accept suggestion: "+label)
	m.pending = nil
	m.setBusy()
	m.status = "resuming…"
	return &streamagent.Msg{Kind: streamagent.KindResponse, Response: &resp}
}

func approvalEcho(b streamagent.Behavior, tool, message string) string {
	if b == streamagent.BehaviorDeny {
		if message != "" {
			return "▶ deny " + tool + ": " + message
		}
		return "▶ deny " + tool
	}
	return "▶ allow " + tool
}

// enterDenyReason swaps the input for the reason editor.
func (m *streamDriver) enterDenyReason() {
	m.mode = chatModeDenyReason
	ti := textinput.New()
	ti.Prompt = "deny reason ▶ "
	ti.Placeholder = "what the agent should do instead (reaches it verbatim)"
	ti.Width = m.inputWidth()
	ti.Focus()
	m.input = ti
	m.status = "enter sends the denial · esc goes back"
}

// cancelSubMode returns to the turn prompt WITHOUT answering: leaving the
// reason editor must not decide the request.
func (m *streamDriver) cancelSubMode() {
	m.mode = chatModeNormal
	m.restoreInput()
	if m.isQuestion() {
		m.status = questionHint
	} else if m.pending != nil {
		m.status = approvalHint
	} else {
		m.status = ""
	}
}

// send writes a message on the held session and reports a failure into the
// transcript rather than swallowing it.
func (m *streamDriver) send(msg *streamagent.Msg) {
	if msg == nil {
		return
	}
	if m.sess == nil {
		m.appendStyled(ErrorStyle, "✗ not attached; nothing was sent")
		return
	}
	if err := m.sess.Send(*msg); err != nil {
		m.appendStyled(ErrorStyle, "✗ send failed: "+err.Error())
	}
}

// interrupt asks the agent to stop its turn.
func (m *streamDriver) interrupt() {
	m.send(&streamagent.Msg{Kind: streamagent.KindInterrupt, Interrupt: &streamagent.Interrupt{}})
	m.appendStyled(OKStyle, "▶ interrupt")
}

// key handles one key the surface did not claim. It returns a command for the
// surface to run (the chat's elapsed ticker, the input's own), which a surface
// that repaints on its own clock may drop.
func (m *streamDriver) key(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEsc:
		// Back out of a sub-editor; otherwise interrupt. Never "leave": that is
		// Ctrl+] on every surface, and an Esc meant as an interrupt that closed
		// the view instead was the accident this replaced.
		if m.mode != chatModeNormal {
			m.cancelSubMode()
			return nil
		}
		m.interrupt()
		return nil

	case tea.KeyCtrlX:
		m.interrupt()
		return nil

	case tea.KeyCtrlD:
		m.send(&streamagent.Msg{Kind: streamagent.KindFinish, Finish: &streamagent.Finish{}})
		m.appendStyled(OKStyle, "▶ finish (the agent completes this turn and exits)")
		return nil

	case tea.KeyEnter:
		switch m.mode {
		case chatModeQuestionText:
			m.qText[m.qCur] = strings.TrimSpace(m.input.Value())
			if !m.pending.Questions[m.qCur].MultiSelect && m.qText[m.qCur] != "" {
				delete(m.qSel, m.qCur) // single-select: the text IS the choice
			}
			m.cancelSubMode()
			return nil
		case chatModeQuestionReply:
			m.qReply = strings.TrimSpace(m.input.Value())
			m.cancelSubMode()
			return nil
		case chatModeNormal:
			if m.isQuestion() {
				if out := m.buildQuestionAnswer(); out != nil {
					m.send(out)
					return chatTickCmd()
				}
				return nil
			}
		}
		if m.mode == chatModeDenyReason {
			reason := strings.TrimSpace(m.input.Value())
			out := m.buildApproval(streamagent.BehaviorDeny, reason)
			m.cancelSubMode()
			m.send(out)
			return chatTickCmd()
		}
		text := strings.TrimSpace(m.input.Value())
		if text == "" {
			return nil
		}
		m.input.SetValue("")
		// No local echo: the adapter puts the turn on the stream, so this view
		// shows it the same way every other client and a later replay do.
		m.setBusy()
		m.status = ""
		m.send(&streamagent.Msg{Kind: streamagent.KindUser, User: &streamagent.UserTurn{Text: text}})
		return chatTickCmd()
	}

	// While a request is pending the letters are DECISIONS, not text: the
	// operator is answering, not composing. Only in the normal mode — the deny
	// editor needs its letters.
	if m.isQuestion() && m.mode == chatModeNormal {
		n := len(m.pending.Questions)
		switch {
		case msg.Type == tea.KeyTab:
			m.qCur = (m.qCur + 1) % n
		case msg.Type == tea.KeyShiftTab:
			m.qCur = (m.qCur + n - 1) % n
		case msg.String() == "o":
			m.enterQuestionEditor(chatModeQuestionText)
		case msg.String() == "r":
			m.enterQuestionEditor(chatModeQuestionReply)
		case msg.String() == "d":
			m.enterDenyReason()
		case len(msg.String()) == 1 && msg.String()[0] >= '1' && msg.String()[0] <= '9':
			m.pickOption(int(msg.String()[0] - '1'))
		}
		// Every other key is swallowed: a plain allow would answer the question
		// with nothing, which is the failure this view exists to prevent.
		return nil
	}
	if m.pending != nil && m.mode == chatModeNormal {
		switch msg.String() {
		case "a":
			out := m.buildApproval(streamagent.BehaviorAllow, "")
			m.send(out)
			return chatTickCmd()
		case "d":
			m.enterDenyReason()
			return nil
		}
		if len(msg.String()) == 1 && msg.String()[0] >= '1' && msg.String()[0] <= '9' {
			if out := m.buildSuggestion(int(msg.String()[0] - '1')); out != nil {
				m.send(out)
				return chatTickCmd()
			}
		}
		return nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return cmd
}

// pendingBlock renders the approval: the tool, its input pretty-printed, and
// the keys. The input is the reason this view exists, so it is shown whole
// rather than summarised — bounded by the pane, not by a byte budget.
func (m streamDriver) pendingBlock(budget int) []chatLine {
	if m.pending == nil {
		return nil
	}
	if m.isQuestion() {
		return m.questionBlock(budget)
	}
	out := []chatLine{
		{},
		{text: "⚑ " + m.pending.Tool + " wants to run  (" + m.pending.ID + ")", style: WarnStyle},
	}
	if d := strings.TrimSpace(m.pending.Description); d != "" {
		out = append(out, chatLine{text: "  " + d, style: MutedStyle})
	}
	if len(m.pending.Input) > 0 {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, m.pending.Input, "  ", "  "); err != nil {
			out = append(out, chatLine{text: "  " + string(m.pending.Input)})
		} else {
			for _, l := range strings.Split(pretty.String(), "\n") {
				out = append(out, chatLine{text: "  " + l})
			}
		}
	}
	for i, s := range m.pending.Suggestions {
		label := s.Type
		if s.Mode != "" {
			label += " " + s.Mode
		}
		if s.Destination != "" {
			label += " (" + s.Destination + ")"
		}
		out = append(out, chatLine{text: fmt.Sprintf("  [%d] also accept: %s", i+1, label), style: WarnStyle})
	}
	out = append(out, chatLine{text: "  " + approvalHint, style: FocusedStyle})
	if budget > 2 && len(out) > budget {
		// Clip the INPUT, never the key row: an approval whose choices scrolled
		// off is an approval the operator cannot answer.
		keep := out[:budget-2]
		out = append(keep,
			chatLine{text: "  … (input truncated to fit; `session snapshot --raw` shows it whole)", style: MutedStyle},
			out[len(out)-1])
	}
	return out
}

// questionBlock renders a pending question: every question with its options
// marked, the focused one highlighted, free text and a reply shown as typed,
// and the key row. Clipped like the approval block — never the key row.
func (m streamDriver) questionBlock(budget int) []chatLine {
	out := []chatLine{{}, {text: "❓ " + m.pending.ID, style: WarnStyle}}
	for i, q := range m.pending.Questions {
		head := q.Question
		if q.Header != "" {
			head = q.Header + ": " + q.Question
		}
		mark, st := "  ", lipgloss.NewStyle()
		if i == m.qCur {
			mark, st = "▸ ", FocusedStyle
		}
		kind := "pick one"
		if q.MultiSelect {
			kind = "pick any"
		}
		out = append(out, chatLine{text: mark + head + "  (" + kind + ")", style: st})
		for j, o := range q.Options {
			box := "( )"
			if q.MultiSelect {
				box = "[ ]"
			}
			if m.qSel[i][j] {
				box = "(•)"
				if q.MultiSelect {
					box = "[x]"
				}
			}
			line := fmt.Sprintf("    %d %s %s", j+1, box, o.Label)
			if o.Description != "" {
				line += " — " + o.Description
			}
			out = append(out, chatLine{text: line})
		}
		if t := m.qText[i]; t != "" {
			out = append(out, chatLine{text: "    other: " + t, style: OKStyle})
		}
	}
	if m.qReply != "" {
		out = append(out, chatLine{text: "  reply: " + m.qReply, style: OKStyle})
	}
	out = append(out, chatLine{text: "  " + questionHint, style: FocusedStyle})
	if budget > 2 && len(out) > budget {
		keep := out[:budget-2]
		out = append(keep,
			chatLine{text: "  … (questions truncated to fit; enlarge the terminal)", style: MutedStyle},
			out[len(out)-1])
	}
	return out
}

// bodyRows renders the transcript plus the pending block into exactly `rows`
// physical rows or fewer, tail-anchored and honouring the scroll offset.
// pendingBudget caps the pending block so the transcript keeps some room.
func (m streamDriver) bodyRows(rows, pendingBudget int, empty string) []chatLine {
	logical := append([]chatLine{}, m.lines...)
	logical = append(logical, m.pendingBlock(pendingBudget)...)
	if len(logical) == 0 && empty != "" {
		logical = []chatLine{{text: empty, style: MutedStyle}}
	}
	// Expand to physical rows BEFORE clipping: one logical line is many rows.
	out := chatRows(logical, m.rowWidth())
	if m.scroll > 0 && m.scroll < len(out) {
		out = out[:len(out)-m.scroll]
	}
	if len(out) > rows {
		out = out[len(out)-rows:]
	}
	return out
}

// statusText is the status line's content: scroll position, the server's or
// the driver's status, and a turn's elapsed seconds. Empty when there is
// nothing to say; each surface decides what an empty status shows.
func (m streamDriver) statusText() string {
	status := m.status
	if m.scroll > 0 {
		status = fmt.Sprintf("scrolled ↑%d · ", m.scroll) + status
	}
	if m.busy {
		if status == "" {
			status = "working"
		}
		status += fmt.Sprintf("  %ds", m.elapsed())
	}
	return status
}

// inputView is the input row, sized for this render: the width is a property
// of the render, and an input still carrying an older frame's Width pads past
// the edge and wraps, which takes the prompt off the bottom of the view.
func (m streamDriver) inputView() string {
	in := m.input
	in.Width = m.inputWidth()
	return in.View()
}
