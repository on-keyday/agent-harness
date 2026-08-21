package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/streamagent"
)

// ChatModel is the event-stream kind's driving surface: a conversation view
// over one task's stream, entered with `r` on a live stream task.
//
// It reads the STREAM rather than the task log, and that is forced rather than
// stylistic. streamagent.RenderText renders a request as "⏸ approval needed:
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

type chatMode int

const (
	chatModeNormal chatMode = iota
	// chatModeDenyReason collects the deny message. It is a MODE rather than a
	// prompt because the reason reaches the agent verbatim as a failed tool
	// result, so it is composed deliberately, not typed into the turn box by
	// accident.
	chatModeDenyReason
)

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

	lines   []string
	scroll  int
	pending *streamagent.Request
	busy    bool
	elapsed int
	status  string
}

func NewChatModel() ChatModel { return ChatModel{} }

func (m ChatModel) IsOpen() bool { return m.open }

func (m *ChatModel) SetSize(w, h int) { m.width, m.height = w, h }

func (m ChatModel) TaskID() string { return m.taskID }

// restoreInput reinstalls the normal turn prompt. EVERY exit from a sub-mode
// must call it: kscale recorded the trap this closes — a sub-mode that swaps
// the input and does not put it back leaves the wrong prompt stuck forever.
func (m *ChatModel) restoreInput() {
	ti := textinput.New()
	ti.Prompt = "you ▶ "
	ti.Placeholder = "message the agent…"
	ti.Focus()
	m.input = ti
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
		// The agent's stderr rides its own frame type and is not NDJSON. An
		// undrained side backpressures the whole stream, so it is drained even
		// though this view does not show it — the task log carries it tagged.
		go func() { _, _ = io.Copy(io.Discard, sess.Stderr()) }()
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

func (m *ChatModel) appendLine(l string) {
	m.lines = append(m.lines, l)
	if m.scroll > 0 {
		m.scroll++
	}
	if len(m.lines) > chatLineLimit {
		m.lines = m.lines[len(m.lines)-chatLineLimit:]
	}
}

// applyLine folds one stream line into the view.
//
// Events go through streamagent.RenderText — the SAME renderer the task log
// uses, so the two cannot drift into two renderings of one event. The REQUEST
// case is the one that deliberately differs: RenderText's one-liner is a
// notification, and this view needs the payload.
func (m *ChatModel) applyLine(line cli.StreamLine) {
	if !line.Decoded {
		m.appendLine("(not the protocol) " + string(line.Raw))
		return
	}
	switch line.Msg.Kind {
	case streamagent.KindRequest:
		if line.Msg.Request == nil {
			return
		}
		req := *line.Msg.Request
		m.pending = &req
		m.busy = false
		m.appendLine("⚑ approval needed: " + req.Tool + "  (" + req.ID + ")")
		m.status = "a allow · d deny · esc leave"
		return
	case streamagent.KindExit:
		if line.Msg.Exit != nil {
			if line.Msg.Exit.Err != "" {
				m.appendLine(fmt.Sprintf("agent exited: code=%d err=%s", line.Msg.Exit.Code, line.Msg.Exit.Err))
			} else {
				m.appendLine(fmt.Sprintf("agent exited: code=%d", line.Msg.Exit.Code))
			}
		}
		m.busy = false
		m.status = "session ended"
		return
	case streamagent.KindHello:
		if h := line.Msg.Hello; h != nil {
			m.status = fmt.Sprintf("attached · %s protocol %d", h.Vendor, h.Protocol)
		}
		return
	}
	if text, ok := streamagent.RenderText(line.Msg); ok {
		m.appendLine(text)
	}
	if ev := line.Msg.Event; ev != nil {
		switch ev.Kind {
		case streamagent.EventFinish:
			m.busy = false
			m.status = ""
		case streamagent.EventThinking:
			m.status = "thinking…"
		case streamagent.EventToolStart:
			m.status = "running " + ev.Tool + "…"
		}
	}
}

// buildApproval answers the pending request, clearing it. Returns nil when
// nothing is pending, so a stray keypress cannot send an answer with no id —
// the id is the staleness guard, and an answer without one is refused anyway.
func (m *ChatModel) buildApproval(b streamagent.Behavior, message string) *streamagent.Msg {
	if m.pending == nil {
		return nil
	}
	resp := streamagent.Response{ID: m.pending.ID, Behavior: b, Message: message}
	m.appendLine(approvalEcho(b, m.pending.Tool, message))
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
	m.appendLine("▶ allow + accept suggestion: " + label)
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
	ti.Focus()
	m.input = ti
	m.status = "enter sends the denial · esc goes back"
}

// cancelSubMode returns to the turn prompt WITHOUT answering: leaving the
// reason editor must not decide the request.
func (m *ChatModel) cancelSubMode() {
	m.mode = chatModeNormal
	m.restoreInput()
	if m.pending != nil {
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
		m.appendLine("✗ not attached; nothing was sent")
		return
	}
	if err := m.sess.Send(*msg); err != nil {
		m.appendLine("✗ send failed: " + err.Error())
	}
}

func (m ChatModel) Update(msg tea.Msg) (ChatModel, tea.Cmd) {
	switch v := msg.(type) {
	case chatAttachedMsg:
		if v.TaskID != m.taskID {
			_ = v.Sess.Close()
			return m, nil
		}
		m.sess = v.Sess
		return m, nil

	case ChatLineMsg:
		// A pump from a PREVIOUS chat can still be delivering; its lines must
		// not land in this transcript.
		if v.TaskID != m.taskID {
			return m, nil
		}
		m.applyLine(v.Line)
		return m, nil

	case ChatEndedMsg:
		if v.TaskID != m.taskID {
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
		if m.mode == chatModeDenyReason {
			m.cancelSubMode()
			return m, nil
		}
		// Leaving is the most common thing to do here and must never be
		// ambiguous, so esc always closes. The task keeps running; interrupt
		// and finish have their own chords.
		m.Close()
		return m, nil

	case tea.KeyCtrlX:
		m.send(&streamagent.Msg{Kind: streamagent.KindInterrupt, Interrupt: &streamagent.Interrupt{}})
		m.appendLine("▶ interrupt")
		return m, nil

	case tea.KeyCtrlD:
		m.send(&streamagent.Msg{Kind: streamagent.KindFinish, Finish: &streamagent.Finish{}})
		m.appendLine("▶ finish (the agent completes this turn and exits)")
		return m, nil

	case tea.KeyEnter:
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
		m.appendLine("you ▶ " + text)
		m.busy = true
		m.elapsed = 0
		m.status = ""
		m.send(&streamagent.Msg{Kind: streamagent.KindUser, User: &streamagent.UserTurn{Text: text}})
		return m, chatTickCmd()
	}

	// While a request is pending the letters are DECISIONS, not text: the
	// operator is answering, not composing. Only in the normal mode — the deny
	// editor needs its letters.
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
func (m ChatModel) pendingBlock(width, budget int) []string {
	if m.pending == nil {
		return nil
	}
	out := []string{"", "⚑ " + m.pending.Tool + " wants to run  (" + m.pending.ID + ")"}
	if d := strings.TrimSpace(m.pending.Description); d != "" {
		out = append(out, "  "+d)
	}
	if len(m.pending.Input) > 0 {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, m.pending.Input, "  ", "  "); err != nil {
			out = append(out, "  "+string(m.pending.Input))
		} else {
			for _, l := range strings.Split(pretty.String(), "\n") {
				out = append(out, "  "+l)
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
		out = append(out, fmt.Sprintf("  [%d] also accept: %s", i+1, label))
	}
	out = append(out, "  a allow · d deny · esc leave")
	if budget > 0 && len(out) > budget {
		// Clip the INPUT, never the key row: an approval whose choices scrolled
		// off is an approval the operator cannot answer.
		keep := out[:budget-2]
		out = append(keep, "  … (input truncated to fit; `session snapshot --raw` shows it whole)", out[len(out)-1])
	}
	return out
}

func (m ChatModel) View() string {
	var b strings.Builder
	title := "CHAT — " + shortTaskID(m.taskID)
	b.WriteString("\n  " + title + "\n\n")

	reserved := 3 // status + input + a blank
	body := m.height - 4 - reserved
	if body < 1 {
		body = 1
	}

	rows := append([]string{}, m.lines...)
	rows = append(rows, m.pendingBlock(m.width, body/2)...)
	if len(rows) == 0 {
		rows = []string{"(no events yet — type a message and press enter)"}
	}
	if m.scroll > 0 && m.scroll < len(rows) {
		rows = rows[:len(rows)-m.scroll]
	}
	if len(rows) > body {
		rows = rows[len(rows)-body:]
	}
	for _, r := range rows {
		b.WriteString("  " + r + "\n")
	}
	for i := len(rows); i < body; i++ {
		b.WriteString("\n")
	}

	status := m.status
	if m.busy {
		if status == "" {
			status = "working"
		}
		status += fmt.Sprintf("  %ds", m.elapsed)
	}
	if status == "" {
		status = "enter send · ctrl+x interrupt · ctrl+d finish · esc leave"
	}
	b.WriteString("  " + status + "\n")
	b.WriteString("  " + m.input.View() + "\n")
	return b.String()
}
