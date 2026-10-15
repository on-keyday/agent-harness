package tui

import (
	"context"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/mattn/go-runewidth"

	"github.com/on-keyday/agent-harness/cli"
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
//
// Everything that drives the session — transcript, input, pending request, the
// key handler — is the embedded streamDriver, shared with the grid's
// StreamPane. What is this overlay's own is how it is entered and left.
type ChatModel struct {
	streamDriver

	open   bool
	taskID string
	cancel context.CancelFunc
}

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

func NewChatModel() ChatModel { return ChatModel{streamDriver: streamDriver{gutter: 2}} }

func (m ChatModel) IsOpen() bool { return m.open }

func (m *ChatModel) SetSize(w, h int) {
	m.width, m.height = w, h
	// The input sizes itself from the frame, so a resize has to reach it too.
	m.input.Width = m.inputWidth()
}

func (m ChatModel) TaskID() string { return m.taskID }

// Open attaches to taskID and starts the pump. The overlay opens immediately;
// the attach happens on the goroutine, so a slow or refused attach shows up as
// a status line rather than freezing the UI.
func (m *ChatModel) Open(ctx context.Context, c *cli.Client, program *tea.Program, taskID string) {
	m.Close()
	m.open = true
	m.taskID = taskID
	m.reset()
	m.status = "attaching…"

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

// chatBodyRows is how many transcript rows fit. View writes a blank, the
// title, another blank, then the body, then the status and the input: body + 5.
func (m ChatModel) chatBodyRows() int {
	if b := m.height - 5; b > 0 {
		return b
	}
	return 1
}

func (m ChatModel) Update(msg tea.Msg) (ChatModel, tea.Cmd) {
	switch v := msg.(type) {
	case chatAttachedMsg:
		// !m.open matters as much as the id: the attach can land AFTER the
		// operator left, and assigning it to a closed model would leak the
		// session with nothing left to close it.
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
		// The tick only repaints; elapsed is read off the clock.
		if !m.busy {
			return m, nil
		}
		return m, chatTickCmd()

	case tea.KeyMsg:
		return m.onKey(v)
	}
	return m, nil
}

func (m ChatModel) onKey(msg tea.KeyMsg) (ChatModel, tea.Cmd) {
	switch msg.String() {
	case "ctrl+]":
		// Leaving is Ctrl+], as it is from a PTY attach and from the grid's
		// input mode. It was Esc here alone, which is what an operator presses
		// to interrupt an agent — and lost the view instead. The task keeps
		// running.
		m.Close()
		return m, nil
	case "pgup":
		m.scrollBy(chatPage)
		return m, nil
	case "pgdown":
		m.scrollBy(-chatPage)
		return m, nil
	}
	cmd := m.key(msg)
	return m, cmd
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

	body := m.chatBodyRows()
	rows := m.bodyRows(body, body/2, m.emptyHint())
	for _, r := range rows {
		b.WriteString("  " + r.style.Render(r.text) + "\n")
	}
	for i := len(rows); i < body; i++ {
		b.WriteString("\n")
	}

	status := m.statusText()
	if m.scroll > 0 {
		status = "pgup/pgdn · " + status
	}
	if status == "" {
		status = "enter send · esc interrupt · ctrl+d finish · ctrl+] leave"
	}
	// Clipped like everything else: a status line wider than the frame wraps
	// and pushes the prompt off the bottom, which is exactly how this view
	// overflowed harness-tui — the body was wrapped and these two were not.
	b.WriteString("  " + MutedStyle.Render(runewidth.Truncate(status, m.rowWidth(), "…")) + "\n")
	b.WriteString("  " + m.inputView() + "\n")
	return b.String()
}
