package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/mattn/go-runewidth"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/agentlog"
	"github.com/on-keyday/agent-harness/runner/streamagent"
)

// ChatModel is the event-stream kind's driving surface: a conversation view
// over one task's stream, entered with `r` on a live stream task.
//
// It reads the STREAM, which is the only place this kind's events are (it
// writes no task log), and it would have to even if there were a log.
// streamagent.RenderText renders a request as "⏸ approval needed:
// <tool> (<id>)" and drops Input entirely; agentlog.Render truncates a
// tool_start's args at 200 bytes. Both are right for a progress feed and wrong
// for deciding whether to allow a Write whose content is the thing at stake —
// the small ones survive the truncation, the large ones do not, and the large
// ones are exactly when it matters. Input rides the stream verbatim, so this
// holds its own cowrite attach (cli.StreamSession) and decodes NDJSON.
//
// Shape borrowed from kscale's cmd/katui/chat.go, which solved it first: a
// bounded transcript, activity lines rendered muted against a primary answer,
// an elapsed-seconds ticker so a minutes-long turn visibly advances, and an
// approval that freezes the transcript and offers its choices as keys.
//
// Not borrowed: its edit-the-arguments step (claude's tool input is routinely a
// whole file, which a one-line text input cannot edit), and its client-side
// tool execution (every tool here runs in the agent's own worktree).
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

// questionHint is the key row of a pending question.
const questionHint = "tab next · 1-9 pick · o other · r reply · enter send · d deny · esc leave"

// ChatLineMsg is one line off the stream, delivered by the pump goroutine.
type ChatLineMsg struct {
	TaskID string
	Line   cli.StreamLine
}

// ChatEndedMsg reports the pump stopping — the task finished, the observer was
// dropped, or the attach failed at open.
type ChatEndedMsg struct {
	TaskID string
	Err    error
}

type chatTickMsg struct{}

func chatTickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return chatTickMsg{} })
}

type ChatModel struct {
	open   bool
	taskID string
	width  int
	height int

	sess   *cli.StreamSession
	cancel context.CancelFunc

	input textinput.Model
	mode  chatMode

	lines   []chatLine
	scroll  int
	pending *streamagent.Request
	busy    bool
	elapsed int
	status  string

	// The answer being built for a pending QUESTION request (pending with
	// Questions): which question has focus, the options picked per question,
	// free text per question, and a freeform reply.
	qCur   int
	qSel   map[int]map[int]bool
	qText  map[int]string
	qReply string
}

func (m ChatModel) isQuestion() bool {
	return m.pending != nil && len(m.pending.Questions) > 0
}

func (m *ChatModel) resetQuestion() {
	m.qCur, m.qSel, m.qText, m.qReply = 0, map[int]map[int]bool{}, map[int]string{}, ""
}

// questionAnswers is the answer as the protocol carries it: question text →
// the picked labels in option order, then any free text.
func (m ChatModel) questionAnswers() map[string][]string {
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
func (m *ChatModel) pickOption(j int) {
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
func (m *ChatModel) buildQuestionAnswer() *streamagent.Msg {
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
	m.busy = true
	m.elapsed = 0
	m.status = "resuming…"
	return &streamagent.Msg{Kind: streamagent.KindResponse, Response: &resp}
}

// enterQuestionEditor swaps the input for the free-text or reply editor,
// prefilled with what was typed before.
func (m *ChatModel) enterQuestionEditor(mode chatMode) {
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

func NewChatModel() ChatModel { return ChatModel{} }

func (m ChatModel) IsOpen() bool { return m.open }

func (m *ChatModel) SetSize(w, h int) {
	m.width, m.height = w, h
	// The input sizes itself from the frame, so a resize has to reach it too.
	m.input.Width = m.inputWidth()
}

func (m ChatModel) TaskID() string { return m.taskID }

// restoreInput reinstalls the normal turn prompt. EVERY exit from a sub-mode
// must call it: kscale recorded the trap this closes — a sub-mode that swaps
// the input and does not put it back leaves the wrong prompt stuck forever.
func (m *ChatModel) restoreInput() {
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
func (m ChatModel) inputWidth() int {
	if w := m.rowWidth() - 8; w > 8 {
		return w
	}
	return 8
}

// Open attaches to taskID and starts the pump. The overlay opens immediately;
// the attach happens on the goroutine, so a slow or refused attach shows up as
// a status line rather than freezing the UI.
func (m *ChatModel) Open(ctx context.Context, c *cli.Client, program *tea.Program, taskID string) {
	m.Close()
	m.open = true
	m.taskID = taskID
	m.lines = nil
	m.scroll = 0
	m.pending = nil
	m.resetQuestion()
	m.busy = false
	m.elapsed = 0
	m.mode = chatModeNormal
	m.status = "attaching…"
	m.restoreInput()

	if c == nil || program == nil {
		m.status = "not connected"
		return
	}
	pumpCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	go func() {
		sess, err := c.OpenStreamSession(pumpCtx, taskID)
		if err != nil {
			program.Send(ChatEndedMsg{TaskID: taskID, Err: err})
			return
		}
		// Handing the session back through a message rather than assigning it
		// here: Update owns the model, and a goroutine writing m.sess would be
		// a data race with every keystroke.
		program.Send(chatAttachedMsg{TaskID: taskID, Sess: sess})
		// The agent's stderr rides its own frame type and is not NDJSON. It is
		// interleaved into the transcript: this kind writes no task log, so
		// the chat is where an agent's complaint is seen. Draining it is also
		// what keeps it from backpressuring the whole stream.
		go func() {
			for {
				line, err := sess.ReadStderrLine()
				if line.Raw != nil {
					program.Send(ChatLineMsg{TaskID: taskID, Line: line})
				}
				if err != nil || pumpCtx.Err() != nil {
					return
				}
			}
		}()
		for {
			line, err := sess.ReadLine()
			if line.Raw != nil || line.Decoded {
				program.Send(ChatLineMsg{TaskID: taskID, Line: line})
			}
			if err != nil {
				program.Send(ChatEndedMsg{TaskID: taskID, Err: err})
				return
			}
			if pumpCtx.Err() != nil {
				return
			}
		}
	}()
}

// chatAttachedMsg hands the opened session to the Update loop, which owns it.
type chatAttachedMsg struct {
	TaskID string
	Sess   *cli.StreamSession
}

// Close leaves the overlay. It does NOT end the task: detaching is not a kill,
// and for this kind Detached is the ordinary state.
func (m *ChatModel) Close() {
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	if m.sess != nil {
		_ = m.sess.Close()
		m.sess = nil
	}
	m.open = false
	m.pending = nil
	m.mode = chatModeNormal
}

// chatPage is how far PgUp/PgDn move, in physical rows.
const chatPage = 10

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

// rowWidth is the frame the transcript wraps into: the terminal less the
// two-space gutter each rendered row carries, on both sides.
func (m ChatModel) rowWidth() int {
	if m.width < 8 {
		return 1
	}
	return m.width - 4
}

// bodyRows is how many transcript rows fit. View writes a blank, the title,
// another blank, then the body, then the status and the input: body + 5.
func (m ChatModel) bodyRows() int {
	if b := m.height - 5; b > 0 {
		return b
	}
	return 1
}

func (m *ChatModel) appendLine(l string) { m.appendStyled(lipgloss.NewStyle(), l) }

func (m *ChatModel) appendStyled(st lipgloss.Style, l string) {
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
func (m *ChatModel) applyLine(line cli.StreamLine) {
	if line.Decoded && line.Msg.Kind == streamagent.KindRequest {
		if line.Msg.Request == nil {
			return
		}
		req := *line.Msg.Request
		m.pending = &req
		m.busy = false
		if len(req.Questions) > 0 {
			m.resetQuestion()
			m.appendStyled(WarnStyle, streamagent.QuestionSummary(req))
			m.status = questionHint
			return
		}
		m.appendStyled(WarnStyle, "⚑ approval needed: "+req.Tool+"  ("+req.ID+")")
		m.status = "a allow · d deny · esc leave"
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
func (m *ChatModel) buildApproval(b streamagent.Behavior, message string) *streamagent.Msg {
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
	m.busy = true
	m.elapsed = 0
	m.status = "resuming…"
	return &streamagent.Msg{Kind: streamagent.KindResponse, Response: &resp}
}

// buildSuggestion accepts the request's nth suggestion alongside allowing this
// call. A suggestion is a STANDING change ("stop asking"), which is why it is
// a separate act from a plain allow even though one message carries both.
func (m *ChatModel) buildSuggestion(n int) *streamagent.Msg {
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
	m.busy = true
	m.elapsed = 0
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
func (m *ChatModel) enterDenyReason() {
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
func (m *ChatModel) cancelSubMode() {
	m.mode = chatModeNormal
	m.restoreInput()
	if m.isQuestion() {
		m.status = questionHint
	} else if m.pending != nil {
		m.status = "a allow · d deny · esc leave"
	} else {
		m.status = ""
	}
}

// send writes a message on the held session and reports a failure into the
// transcript rather than swallowing it.
func (m *ChatModel) send(msg *streamagent.Msg) {
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

func (m ChatModel) Update(msg tea.Msg) (ChatModel, tea.Cmd) {
	switch v := msg.(type) {
	case chatAttachedMsg:
		// !m.open matters as much as the id: the attach can land AFTER the
		// operator pressed esc, and assigning it to a closed model would leak
		// the session with nothing left to close it.
		if !m.open || v.TaskID != m.taskID {
			_ = v.Sess.Close()
			return m, nil
		}
		m.sess = v.Sess
		return m, nil

	case ChatLineMsg:
		// A pump from a PREVIOUS chat can still be delivering; its lines must
		// not land in this transcript, and none of them belong in a closed one.
		if !m.open || v.TaskID != m.taskID {
			return m, nil
		}
		m.applyLine(v.Line)
		return m, nil

	case ChatEndedMsg:
		if !m.open || v.TaskID != m.taskID {
			return m, nil
		}
		m.busy = false
		if v.Err != nil {
			m.status = "stream ended: " + v.Err.Error()
		} else {
			m.status = "stream ended"
		}
		return m, nil

	case chatTickMsg:
		if !m.busy {
			return m, nil
		}
		m.elapsed++
		return m, chatTickCmd()

	case tea.KeyMsg:
		return m.onKey(v)
	}
	return m, nil
}

func (m ChatModel) onKey(msg tea.KeyMsg) (ChatModel, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		if m.mode != chatModeNormal {
			m.cancelSubMode()
			return m, nil
		}
		// Leaving is the most common thing to do here and must never be
		// ambiguous, so esc always closes. The task keeps running; interrupt
		// and finish have their own chords.
		m.Close()
		return m, nil

	case tea.KeyPgUp, tea.KeyPgDown:
		// The scrollback existed and was applied by the view from the first
		// version, but nothing ever changed m.scroll — declared, honoured, and
		// unreachable. Clamped against the CURRENT row count so it cannot run
		// off the top of a short transcript.
		total := len(chatRows(m.lines, m.rowWidth()))
		if msg.Type == tea.KeyPgUp {
			m.scroll += chatPage
		} else {
			m.scroll -= chatPage
		}
		if max := total - 1; m.scroll > max {
			m.scroll = max
		}
		if m.scroll < 0 {
			m.scroll = 0
		}
		return m, nil

	case tea.KeyCtrlX:
		m.send(&streamagent.Msg{Kind: streamagent.KindInterrupt, Interrupt: &streamagent.Interrupt{}})
		m.appendStyled(OKStyle, "▶ interrupt")
		return m, nil

	case tea.KeyCtrlD:
		m.send(&streamagent.Msg{Kind: streamagent.KindFinish, Finish: &streamagent.Finish{}})
		m.appendStyled(OKStyle, "▶ finish (the agent completes this turn and exits)")
		return m, nil

	case tea.KeyEnter:
		switch m.mode {
		case chatModeQuestionText:
			m.qText[m.qCur] = strings.TrimSpace(m.input.Value())
			if !m.pending.Questions[m.qCur].MultiSelect && m.qText[m.qCur] != "" {
				delete(m.qSel, m.qCur) // single-select: the text IS the choice
			}
			m.cancelSubMode()
			return m, nil
		case chatModeQuestionReply:
			m.qReply = strings.TrimSpace(m.input.Value())
			m.cancelSubMode()
			return m, nil
		case chatModeNormal:
			if m.isQuestion() {
				if out := m.buildQuestionAnswer(); out != nil {
					m.send(out)
					return m, chatTickCmd()
				}
				return m, nil
			}
		}
		if m.mode == chatModeDenyReason {
			reason := strings.TrimSpace(m.input.Value())
			out := m.buildApproval(streamagent.BehaviorDeny, reason)
			m.cancelSubMode()
			m.send(out)
			return m, chatTickCmd()
		}
		text := strings.TrimSpace(m.input.Value())
		if text == "" {
			return m, nil
		}
		m.input.SetValue("")
		// No local echo: the adapter puts the turn on the stream, so this chat
		// shows it the same way every other client and a later replay do.
		m.busy = true
		m.elapsed = 0
		m.status = ""
		m.send(&streamagent.Msg{Kind: streamagent.KindUser, User: &streamagent.UserTurn{Text: text}})
		return m, chatTickCmd()
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
		return m, nil
	}
	if m.pending != nil && m.mode == chatModeNormal {
		switch msg.String() {
		case "a":
			out := m.buildApproval(streamagent.BehaviorAllow, "")
			m.send(out)
			return m, chatTickCmd()
		case "d":
			m.enterDenyReason()
			return m, nil
		}
		if len(msg.String()) == 1 && msg.String()[0] >= '1' && msg.String()[0] <= '9' {
			if out := m.buildSuggestion(int(msg.String()[0] - '1')); out != nil {
				m.send(out)
				return m, chatTickCmd()
			}
			return m, nil
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// pendingBlock renders the approval: the tool, its input pretty-printed, and
// the keys. The input is the reason this view exists, so it is shown whole
// rather than summarised — bounded by the pane, not by a byte budget.
func (m ChatModel) pendingBlock(budget int) []chatLine {
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
	out = append(out, chatLine{text: "  a allow · d deny · esc leave", style: FocusedStyle})
	if budget > 0 && len(out) > budget {
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
func (m ChatModel) questionBlock(budget int) []chatLine {
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
	if budget > 0 && len(out) > budget {
		keep := out[:budget-2]
		out = append(keep,
			chatLine{text: "  … (questions truncated to fit; enlarge the terminal)", style: MutedStyle},
			out[len(out)-1])
	}
	return out
}

// emptyHint is what an empty transcript says. It is not decoration: a chat
// opened on a freshly RESUMED task shows nothing for a while, because the
// replay comes from the mux ring and a resume starts a new one, while the agent
// spends that time reading its OWN history before emitting anything. An
// operator who sees a blank screen there reasonably concludes it is broken —
// which is what happened the first time this was used in anger.
func (m ChatModel) emptyHint() string {
	if m.sess == nil {
		return "(attaching to the session…)"
	}
	return "(attached, nothing on the stream yet — a resumed agent reads its own history before its first event; type a message and press enter)"
}

func (m ChatModel) View() string {
	var b strings.Builder
	b.WriteString("\n  " + HeaderStyle.Render("CHAT — "+shortTaskID(m.taskID)) + "\n\n")

	body := m.bodyRows()
	logical := append([]chatLine{}, m.lines...)
	logical = append(logical, m.pendingBlock(body/2)...)
	if len(logical) == 0 {
		logical = []chatLine{{text: m.emptyHint(), style: MutedStyle}}
	}
	// Expand to physical rows BEFORE clipping: one logical line is many rows.
	rows := chatRows(logical, m.rowWidth())
	if m.scroll > 0 && m.scroll < len(rows) {
		rows = rows[:len(rows)-m.scroll]
	}
	if len(rows) > body {
		rows = rows[len(rows)-body:]
	}
	for _, r := range rows {
		b.WriteString("  " + r.style.Render(r.text) + "\n")
	}
	for i := len(rows); i < body; i++ {
		b.WriteString("\n")
	}

	status := m.status
	if m.scroll > 0 {
		status = fmt.Sprintf("scrolled ↑%d · pgup/pgdn · ", m.scroll) + status
	}
	if m.busy {
		if status == "" {
			status = "working"
		}
		status += fmt.Sprintf("  %ds", m.elapsed)
	}
	if status == "" {
		status = "enter send · ctrl+x interrupt · ctrl+d finish · esc leave"
	}
	// Clipped like everything else: a status line wider than the frame wraps
	// and pushes the prompt off the bottom, which is exactly how this view
	// overflowed harness-tui — the body was wrapped and these two were not.
	b.WriteString("  " + MutedStyle.Render(runewidth.Truncate(status, m.rowWidth(), "…")) + "\n")
	// Sized HERE rather than trusted from whenever it was last stored: the
	// width is a property of this render, and an input still carrying an older
	// frame's Width pads past the edge and wraps, which takes the prompt off
	// the bottom of the view.
	in := m.input
	in.Width = m.inputWidth()
	b.WriteString("  " + in.View() + "\n")
	return b.String()
}
