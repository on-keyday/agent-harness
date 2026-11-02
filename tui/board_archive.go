package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/cli"
)

// Flush and archives in the board modal. See
// docs/superpowers/specs/2026-11-02-agentboard-flush-archive-design.md.

// BoardFlushMsg carries the result of DoBoardFlush.
type BoardFlushMsg struct {
	Topic   string
	Through uint64
	Dropped int
	Err     error
}

// BoardArchiveMsg carries the result of DoBoardArchive.
type BoardArchiveMsg struct {
	Topic string
	Row   cli.BoardArchiveRow
	Err   error
}

// BoardArchivesMsg carries the result of DoBoardArchives. Topic is the filter
// it was asked with, "" for every archive.
type BoardArchivesMsg struct {
	Topic string
	Rows  []cli.BoardArchiveRow
	Err   error
}

// BoardArchiveReadMsg carries one page of an archive, in ascending seq. Older
// is set when it was asked for below the oldest message shown.
type BoardArchiveReadMsg struct {
	ID, Topic string
	Msgs      []cli.BoardMessage
	Found     bool
	Older     bool
	Err       error
}

// BoardArchivePurgeMsg carries the result of DoBoardArchivePurge.
type BoardArchivePurgeMsg struct {
	ID     string
	Seq    uint64
	Purged int
	Found  bool
	Err    error
}

func boardCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}

// DoBoardFlush empties a retained topic's ring over the long-lived client.
func DoBoardFlush(c *cli.Client, topic string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := boardCtx()
		defer cancel()
		through, dropped, err := c.BoardFlush(ctx, topic)
		return BoardFlushMsg{Topic: topic, Through: through, Dropped: dropped, Err: err}
	}
}

// DoBoardArchive freezes a topic's history into an archive.
func DoBoardArchive(c *cli.Client, topic string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := boardCtx()
		defer cancel()
		row, err := c.BoardArchive(ctx, topic)
		return BoardArchiveMsg{Topic: topic, Row: row, Err: err}
	}
}

// DoBoardArchives lists the archives of topic, or all of them for "".
func DoBoardArchives(c *cli.Client, topic string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := boardCtx()
		defer cancel()
		rows, err := c.BoardArchives(ctx, topic)
		return BoardArchivesMsg{Topic: topic, Rows: rows, Err: err}
	}
}

// DoBoardArchiveRead reads the page of an archive below beforeSeq (0 = the
// newest), as many as O reads of a topic.
func DoBoardArchiveRead(c *cli.Client, id, topic string, beforeSeq uint64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := boardCtx()
		defer cancel()
		msgs, found, err := c.BoardArchiveRead(ctx, id, beforeSeq, boardOlderPage)
		// The page comes newest first; the view lists oldest first.
		for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
			msgs[i], msgs[j] = msgs[j], msgs[i]
		}
		return BoardArchiveReadMsg{ID: id, Topic: topic, Msgs: msgs, Found: found, Older: beforeSeq != 0, Err: err}
	}
}

// DoBoardArchivePurge deletes one message of an archive, or the archive for
// seq 0.
func DoBoardArchivePurge(c *cli.Client, id string, seq uint64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := boardCtx()
		defer cancel()
		purged, found, err := c.BoardArchivePurge(ctx, id, seq)
		return BoardArchivePurgeMsg{ID: id, Seq: seq, Purged: purged, Found: found, Err: err}
	}
}

// Flushable reports whether F should flush topic. On a topic that is not
// retained it says so on the status line instead: flushing one would destroy
// its messages, which is purge, not flush.
func (m *BoardModal) Flushable(topic string) bool {
	if topic == "" {
		return false
	}
	if m.retentionOf(topic) == nil {
		m.status = "flush: " + topic + " is not retained — only a retained topic keeps its messages on disk"
		return false
	}
	return true
}

// Archivable reports whether A should archive topic. A chat.* topic never
// can (direct messages stay temporary), so A says so instead of sending a
// request the server always refuses — the rule retain follows.
func (m *BoardModal) Archivable(topic string) bool {
	if topic == "" {
		return false
	}
	if !agentboard.Retainable(topic) {
		m.status = "archive: " + topic + " cannot be archived — chat.* topics stay temporary"
		return false
	}
	return true
}

// flushHint is the footer's "F: flush", present only on a retained topic.
func (m *BoardModal) flushHint(topic string) string {
	if m.retentionOf(topic) == nil {
		return ""
	}
	return modalKeys.BoardFlush + ": flush"
}

// ApplyArchives shows the archive list, of topic or of every topic for "".
func (m *BoardModal) ApplyArchives(topic string, rows []cli.BoardArchiveRow) {
	m.archTopic = topic
	m.archives = append([]cli.BoardArchiveRow(nil), rows...)
	if m.archCursor >= len(m.archives) {
		m.archCursor = max(len(m.archives)-1, 0)
	}
	m.curArchive = ""
	m.mode = boardArchives
	m.takePendingStatus()
}

// SelectedArchive is the archive under the list cursor.
func (m *BoardModal) SelectedArchive() (cli.BoardArchiveRow, bool) {
	if m.archCursor < 0 || m.archCursor >= len(m.archives) {
		return cli.BoardArchiveRow{}, false
	}
	return m.archives[m.archCursor], true
}

// ArchivesTopic is the filter the archive list was read with.
func (m *BoardModal) ArchivesTopic() string { return m.archTopic }

// CurArchive is the archive the message view shows, "" when it shows a topic.
func (m *BoardModal) CurArchive() string { return m.curArchive }

// ApplyArchiveMessages shows a page of an archive in the message view. The
// view is read-only there: what an archive holds changes only by purge.
func (m *BoardModal) ApplyArchiveMessages(id, topic string, msgs []cli.BoardMessage) {
	m.ApplyMessages(topic, msgs, nil, true)
	m.curArchive = id
}

// PrependArchiveOlder is PrependOlder for an archive's view.
func (m *BoardModal) PrependArchiveOlder(id string, older []cli.BoardMessage) {
	if id != m.curArchive || m.mode != boardMessages {
		return
	}
	if len(older) == 0 {
		m.status = "no older messages in archive " + id
		return
	}
	m.msgs = append(append([]cli.BoardMessage{}, older...), m.msgs...)
	m.msgCursor += len(older)
	m.status = fmt.Sprintf("loaded %d older message(s)", len(older))
	m.updateContentFromCursor()
}

// PopToArchives returns from an archive's messages to the archive list.
func (m *BoardModal) PopToArchives() {
	m.mode = boardArchives
	m.curArchive = ""
	m.msgs = nil
	m.msgCursor = 0
	m.status = ""
}

func (m *BoardModal) archivesView() string {
	scope := "all topics"
	if m.archTopic != "" {
		scope = m.archTopic
	}
	header := HeaderStyle.Render(fmt.Sprintf("archives of %s (%d)", scope, len(m.archives)))
	var b strings.Builder
	lo, hi := 0, len(m.archives)
	if room := m.height - 8; room > 0 && hi > room {
		lo = min(max(m.archCursor-room/2, 0), hi-room)
		hi = lo + room
	}
	for i := lo; i < hi; i++ {
		cursor := "  "
		if i == m.archCursor {
			cursor = "> "
		}
		b.WriteString(cursor + m.archives[i].Summary() + "\n")
	}
	if len(m.archives) == 0 {
		b.WriteString("  (no archives — " + modalKeys.BoardArchive + " on a topic makes one)\n")
	}
	status := ""
	if m.status != "" {
		status = "\n" + WarnStyle.Render(m.status)
	}
	footer := FooterStyle.Render(packItems([]string{"↑/↓ select", "Enter: read",
		modalKeys.BoardForgetContact + ": delete archive", "r: refresh", "Esc: back"}, m.content.Width))
	return PanelStyleFocused.Padding(0, 1).Render(header + "\n" + b.String() + status + "\n" + footer)
}

// MsgCount is how many messages the message view holds.
func (m *BoardModal) MsgCount() int { return len(m.msgs) }
