package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
)

// ---- msg types (co-located with the modal, mirroring ConnSnapshotMsg in conns.go) ----

// BoardTopicsMsg carries the result of DoBoardTopics: the full topic listing
// or an error.
type BoardTopicsMsg struct {
	Rows []cli.BoardTopicRow
	Err  error
}

// BoardReadMsg carries the result of DoBoardRead for a single topic.
// Found=false when the topic does not exist on the server.
type BoardReadMsg struct {
	Topic string
	Msgs  []cli.BoardMessage
	Found bool
	Err   error
}

// BoardPurgeMsg carries the result of DoBoardPurge.
// Seq==0 means whole-topic purge; Seq!=0 means single-message purge.
// Found=false when the topic (or specific seq) does not exist.
type BoardPurgeMsg struct {
	Topic  string
	Seq    uint64
	Purged int
	Found  bool
	Err    error
}

// ---- tea.Cmd factories (mirroring DoConnSnapshot in conns.go) ----

// DoBoardTopics fetches all agentboard topics via the long-lived client.
// Mirrors DoConnSnapshot exactly (context.WithTimeout + method call + typed msg).
func DoBoardTopics(c *cli.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		rows, err := c.BoardTopics(ctx)
		return BoardTopicsMsg{Rows: rows, Err: err}
	}
}

// DoBoardRead fetches all retained messages for topic via the long-lived client.
func DoBoardRead(c *cli.Client, topic string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		msgs, found, err := c.BoardRead(ctx, topic)
		return BoardReadMsg{Topic: topic, Msgs: msgs, Found: found, Err: err}
	}
}

// DoBoardPurge purges one message (seq!=0) or the whole topic ring (seq==0)
// via the long-lived client.
func DoBoardPurge(c *cli.Client, topic string, seq uint64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		purged, found, err := c.BoardPurge(ctx, topic, seq)
		return BoardPurgeMsg{Topic: topic, Seq: seq, Purged: purged, Found: found, Err: err}
	}
}

// ---- BoardModal ----

// boardMode is the internal mode of the BoardModal.
type boardMode int

const (
	// boardTopics shows a scrollable table of all agentboard topics.
	boardTopics boardMode = iota
	// boardMessages shows the message list + content viewport for a selected topic.
	boardMessages
)

// BoardModal is a two-mode overlay that mirrors ConnsModal's structure for the
// table-based list (topic mode) and uses a viewport.Model (mirroring LogsModel)
// to show selected-message payload content (message mode).
//
// Key dispatch follows the ConnsModal convention: the App intercepts
// Enter / r / x / X / Esc before calling Update, so Do* commands can
// reference a.client. Update handles only table navigation (topic mode) and
// message cursor + viewport scroll (message mode).
type BoardModal struct {
	open        bool
	mode        boardMode
	topicsTable table.Model
	rowTopics   []cli.BoardTopicRow // parallel slice: rowTopics[i] corresponds to table row i
	curTopic    string
	msgs        []cli.BoardMessage
	msgCursor   int
	content     viewport.Model // payload of msgs[msgCursor], pretty-printed if valid JSON
	status      string         // one-line error / confirmation rendered below the table
}

// NewBoardModal constructs a BoardModal with fixed column widths for the topics
// table. Mirrors NewConnsModal.
func NewBoardModal() BoardModal {
	cols := []table.Column{
		{Title: "Topic", Width: 30},
		{Title: "Msgs", Width: 5},
		{Title: "LastSeq", Width: 9},
		{Title: "LastAt", Width: 22},
	}
	t := table.New(table.WithColumns(cols), table.WithFocused(true))
	vp := viewport.New(80, 10)
	vp.SetContent("(select a topic and press Enter to read)")
	return BoardModal{
		topicsTable: t,
		content:     vp,
	}
}

func (m *BoardModal) IsOpen() bool { return m.open }

// Open opens the modal and resets to topic-list mode.
func (m *BoardModal) Open() {
	m.open = true
	m.mode = boardTopics
}

// Close closes the modal and resets mode to boardTopics.
func (m *BoardModal) Close() {
	m.open = false
	m.mode = boardTopics
}

// Mode returns the current internal display mode.
func (m *BoardModal) Mode() boardMode { return m.mode }

// PopToTopics returns from message-drilldown mode back to topic-list mode.
// Used by the App's Esc handler when mode==boardMessages.
func (m *BoardModal) PopToTopics() {
	m.mode = boardTopics
	m.msgs = nil
	m.msgCursor = 0
	m.status = ""
}

// SetSize propagates terminal dimensions into both the topics table and the
// content viewport. Mirrors ConnsModal.SetSize.
func (m *BoardModal) SetSize(w, h int) {
	// Reserve 4 rows for border/header/footer in both halves.
	m.topicsTable.SetWidth(w - 4)
	m.topicsTable.SetHeight(h/2 - 4)
	m.content.Width = w - 4
	m.content.Height = h/2 - 4
}

// ApplyTopics replaces all rows with the given slice and rebuilds the topics
// table. Mirrors ConnsModal.ApplySnapshot.
func (m *BoardModal) ApplyTopics(rows []cli.BoardTopicRow) {
	m.rowTopics = make([]cli.BoardTopicRow, len(rows))
	copy(m.rowTopics, rows)
	m.rebuildTopicsRows()
	m.status = ""
}

// ApplyMessages populates message-drilldown mode with the given messages for
// topic. Sets mode to boardMessages on success. Called when DoBoardRead
// completes.
func (m *BoardModal) ApplyMessages(topic string, msgs []cli.BoardMessage, found bool) {
	m.curTopic = topic
	if !found {
		m.msgs = nil
		m.msgCursor = 0
		m.content.SetContent("(topic not found)")
		m.status = "topic not found"
		return
	}
	m.msgs = make([]cli.BoardMessage, len(msgs))
	copy(m.msgs, msgs)
	m.msgCursor = 0
	m.mode = boardMessages
	m.status = ""
	m.updateContentFromCursor()
}

// SelectedTopicName returns the topic name under the table cursor, or "" when
// the table is empty or the cursor is out of range.
func (m *BoardModal) SelectedTopicName() string {
	idx := m.topicsTable.Cursor()
	if idx < 0 || idx >= len(m.rowTopics) {
		return ""
	}
	return m.rowTopics[idx].Name
}

// CurTopic returns the topic currently shown in message mode.
func (m *BoardModal) CurTopic() string { return m.curTopic }

// SelectedMsgSeq returns the Seq of the message under msgCursor, or 0 when
// there are no messages.
func (m *BoardModal) SelectedMsgSeq() uint64 {
	if m.msgCursor < 0 || m.msgCursor >= len(m.msgs) {
		return 0
	}
	return m.msgs[m.msgCursor].Seq
}

// SetStatus sets the status line text. Used by the App to relay RPC errors or
// purge confirmations.
func (m *BoardModal) SetStatus(s string) { m.status = s }

// rebuildTopicsRows translates rowTopics into bubbles/table rows.
// Mirrors ConnsModal.rebuildRows.
func (m *BoardModal) rebuildTopicsRows() {
	rows := make([]table.Row, 0, len(m.rowTopics))
	for i := range m.rowTopics {
		rows = append(rows, boardTopicToRow(&m.rowTopics[i]))
	}
	m.topicsTable.SetRows(rows)
}

// boardTopicToRow maps a BoardTopicRow to a table.Row (4 columns).
func boardTopicToRow(r *cli.BoardTopicRow) table.Row {
	at := "-"
	if r.LastPublishedAtMs > 0 {
		at = time.UnixMilli(int64(r.LastPublishedAtMs)).UTC().Format(time.RFC3339)
	}
	return table.Row{
		r.Name,
		fmt.Sprintf("%d", r.MsgCount),
		fmt.Sprintf("%d", r.LastSeq),
		at,
	}
}

// updateContentFromCursor refreshes the content viewport from msgs[msgCursor].
// Pretty-prints the payload if json.Valid reports it is valid JSON; otherwise
// uses the raw string representation.
func (m *BoardModal) updateContentFromCursor() {
	if len(m.msgs) == 0 {
		m.content.SetContent("(no messages)")
		return
	}
	if m.msgCursor < 0 {
		m.msgCursor = 0
	}
	if m.msgCursor >= len(m.msgs) {
		m.msgCursor = len(m.msgs) - 1
	}
	msg := m.msgs[m.msgCursor]

	// Pretty-print if valid JSON.
	var payloadStr string
	if json.Valid(msg.Payload) {
		var v interface{}
		if err := json.Unmarshal(msg.Payload, &v); err == nil {
			if b, err := json.MarshalIndent(v, "", "  "); err == nil {
				payloadStr = string(b)
			}
		}
	}
	if payloadStr == "" {
		payloadStr = string(msg.Payload)
	}

	fromShort := msg.FromTaskHex
	if len(fromShort) > 8 {
		fromShort = fromShort[:8]
	}
	at := time.UnixMilli(int64(msg.ReceivedAtMs)).UTC().Format(time.RFC3339)
	header := fmt.Sprintf("seq=%d  from=%s  host=%s  at=%s", msg.Seq, fromShort, msg.FromHostname, at)
	m.content.SetContent(header + "\n\n" + payloadStr)
}

// Update handles navigation within the modal. In topic mode it forwards all
// events to the underlying table (for ↑/↓ row selection). In message mode it
// intercepts ↑/↓ to move msgCursor and forwards everything else to the
// content viewport (PgUp/PgDn etc.). The App intercepts Enter/r/x/X/Esc
// before calling this, so none of those reach Update.
// Mirrors ConnsModal.Update.
func (m BoardModal) Update(msg tea.Msg) (BoardModal, tea.Cmd) {
	if !m.open {
		return m, nil
	}
	switch m.mode {
	case boardTopics:
		var cmd tea.Cmd
		m.topicsTable, cmd = m.topicsTable.Update(msg)
		return m, cmd

	case boardMessages:
		if k, ok := msg.(tea.KeyMsg); ok {
			switch k.Type {
			case tea.KeyUp:
				if m.msgCursor > 0 {
					m.msgCursor--
					m.updateContentFromCursor()
				}
				return m, nil
			case tea.KeyDown:
				if m.msgCursor < len(m.msgs)-1 {
					m.msgCursor++
					m.updateContentFromCursor()
				}
				return m, nil
			}
		}
		var cmd tea.Cmd
		m.content, cmd = m.content.Update(msg)
		return m, cmd
	}
	return m, nil
}

// View renders the board modal. In topic mode it shows the topics table with a
// status line and key hints. In message mode it shows a message list with a
// cursor indicator above the content viewport. Mirrors ConnsModal.View.
func (m BoardModal) View() string {
	box := PanelStyleFocused.Padding(0, 1)
	statusLine := ""
	if m.status != "" {
		statusLine = "\n" + WarnStyle.Render(m.status)
	}

	switch m.mode {
	case boardTopics:
		header := HeaderStyle.Render(fmt.Sprintf("agentboard topics (%d)", len(m.rowTopics)))
		footer := FooterStyle.Render("Enter: read  r: refresh  x: purge topic  Esc: close")
		return box.Render(header + "\n" + m.topicsTable.View() + statusLine + "\n" + footer)

	case boardMessages:
		// Build a mini-list with a cursor indicator showing which message is
		// selected; the full payload is shown in the content viewport below.
		var msgList strings.Builder
		for i := range m.msgs {
			cursor := "  "
			if i == m.msgCursor {
				cursor = "> "
			}
			msg := m.msgs[i]
			fromShort := msg.FromTaskHex
			if len(fromShort) > 8 {
				fromShort = fromShort[:8]
			}
			at := time.UnixMilli(int64(msg.ReceivedAtMs)).UTC().Format("15:04:05Z")
			msgList.WriteString(fmt.Sprintf("%s[%d] seq=%-5d  from=%s  %s\n",
				cursor, i+1, msg.Seq, fromShort, at))
		}
		if len(m.msgs) == 0 {
			msgList.WriteString("  (no messages)\n")
		}
		header := HeaderStyle.Render(fmt.Sprintf("topic: %s  (%d msgs)", m.curTopic, len(m.msgs)))
		footer := FooterStyle.Render("↑/↓ select · PgUp/PgDn scroll · X: purge msg  r: re-read  Esc: back")
		return box.Render(header + "\n" + msgList.String() + "\n" + m.content.View() + statusLine + "\n" + footer)
	}
	return box.Render("(unknown board mode)")
}
