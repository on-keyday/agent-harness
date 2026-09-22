package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
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
	// Subs counts subscribers per topic NAME, including names that have no
	// topic yet. Nil when the caller lacks board_observe for BoardSubscribers;
	// the list still renders, with the Subs column blank.
	Subs map[string]int
	Err  error
}

// BoardReadMsg carries the result of DoBoardRead for a single topic.
// Found=false when the topic does not exist on the server.
type BoardReadMsg struct {
	Topic string
	Msgs  []cli.BoardMessage
	// Subs is the topic's subscriber set at read time, for the per-message
	// delivery marks. Nil when it could not be fetched.
	Subs  []cli.BoardSubscriberRow
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

// BoardRetractMsg carries the result of DoBoardRetract. Seq is always non-zero:
// there is no whole-topic retract, so unlike BoardPurgeMsg this message never
// stands for a topic-wide operation. Found=false when the topic, the seq, or a
// live message with that seq does not exist — the server does not tell those
// apart.
type BoardRetractMsg struct {
	Topic string
	Seq   uint64
	Found bool
	Err   error
}

// ---- tea.Cmd factories (mirroring DoConnSnapshot in conns.go) ----

// DoBoardTopics fetches all agentboard topics via the long-lived client.
// Mirrors DoConnSnapshot exactly (context.WithTimeout + method call + typed msg).
func DoBoardTopics(c *cli.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		rows, err := c.BoardTopics(ctx)
		if err != nil {
			return BoardTopicsMsg{Rows: rows, Err: err}
		}
		// One extra round trip yields both the per-topic subscriber count and
		// every subscribed NAME, which is what lets the list include topics
		// that do not exist yet. A failure here is not fatal to the listing.
		var subs map[string]int
		if srows, serr := c.BoardSubscribers(ctx, ""); serr == nil {
			subs = map[string]int{}
			for _, sr := range srows {
				for _, pat := range sr.Patterns {
					subs[pat.Name]++
				}
			}
		}
		return BoardTopicsMsg{Rows: rows, Subs: subs}
	}
}

// DoBoardRead fetches all retained messages for topic via the long-lived client.
func DoBoardRead(c *cli.Client, topic string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		msgs, found, err := c.BoardRead(ctx, topic)
		// Subscribers ride along so each message row can say who has actually
		// been handed it. A failure here is not fatal to the listing: the rows
		// print with shown_to=0/0 rather than not printing.
		subs, serr := c.BoardSubscribers(ctx, topic)
		if serr != nil {
			subs = nil
		}
		return BoardReadMsg{Topic: topic, Msgs: msgs, Subs: subs, Found: found, Err: err}
	}
}

// BoardSubscribersMsg carries the result of DoBoardSubscribers for one topic.
// BoardChainsMsg carries the assembled reply chains back to the App.
type BoardChainsMsg struct {
	Rows []cli.ThreadRow
	Err  error
}

// DoBoardChains collects every topic the operator can see and assembles the
// chains across them.
//
// It calls the *With variant against the App's long-lived client, like every
// other Do* here, and the collection itself is cli.CollectThreadsWith — the
// same function the WebUI's wasm bridge calls. The CLI reaches it through the
// fresh-dial wrapper. One collection, three surfaces.
func DoBoardChains(c *cli.Client) tea.Cmd {
	return func() tea.Msg {
		// Longer than the 15s its siblings use, deliberately: every other Do*
		// here is one round trip, while this is BoardTopics plus one BoardRead
		// per topic.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		rows, err := cli.CollectThreadsWith(ctx, c, cli.ThreadFilter{})
		return BoardChainsMsg{Rows: rows, Err: err}
	}
}

type BoardSubscribersMsg struct {
	Topic string
	Rows  []cli.BoardSubscriberRow
	Err   error
}

// DoBoardSubscribers fetches the tasks that would receive a publish to topic,
// via the long-lived client. Mirrors DoBoardRead.
func DoBoardSubscribers(c *cli.Client, topic string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		rows, err := c.BoardSubscribers(ctx, topic)
		return BoardSubscribersMsg{Topic: topic, Rows: rows, Err: err}
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

// DoBoardRetract withdraws one message via the long-lived client: it leaves
// every agent-facing path and stays in this view, marked, until the topic ages
// out. Mirrors DoBoardPurge — same client, same timeout; what differs is what
// survives.
func DoBoardRetract(c *cli.Client, topic string, seq uint64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		found, err := c.BoardRetract(ctx, topic, seq)
		return BoardRetractMsg{Topic: topic, Seq: seq, Found: found, Err: err}
	}
}

// BoardThreadOpMsg carries the result of a thread-scoped retract or purge.
// Verb is "retract" or "purge", for the status line.
type BoardThreadOpMsg struct {
	Verb   string
	Key    string
	Result cli.ThreadFanoutResult
	Err    error
}

// DoBoardThreadOp runs one thread-scoped destructive verb over the long-lived
// client.
//
// The work is cli.FanoutThread — the same function the CLI verbs and the wasm
// bridge call. A loop here would be free to disagree with it about the one
// thing that matters: an already-withdrawn message must not be asked about on
// the retract path, because the server's answer for it is the same not-found a
// missing message gets, and reporting a handled message as a failure is how
// this reads as broken when it worked.
//
// Longer than the 15s its per-message siblings use: this is a collection over
// every topic plus one call per message in the conversation.
func DoBoardThreadOp(c *cli.Client, op cli.ThreadOp, key string) tea.Cmd {
	verb := "retract"
	if op == cli.ThreadPurge {
		verb = "purge"
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		res, err := cli.FanoutThread(ctx, c, op, cli.ThreadFilter{Conversation: key})
		return BoardThreadOpMsg{Verb: verb, Key: key, Result: res, Err: err}
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
	// boardSubscribers shows which tasks would receive a publish to the
	// selected topic.
	boardSubscribers
	// boardChainList lists the CONVERSATIONS on the board, one row each. It is
	// the other view of the same data: topic mode answers "what is on this
	// topic", this one follows in_reply_to, which a topic-keyed view
	// structurally cannot — each agent receives on its own chat.<short-id>, so
	// an exchange is split across at least two topics.
	//
	// A list rather than the scrolled dump it used to be, because every other
	// view in this modal is list → detail (topics → Enter → messages) and this
	// one had no cursor at all — which is also why it had no way to act on ONE
	// conversation. The list supplies the selection the destructive actions
	// need, and removes the asymmetry, in the same move.
	boardChainList
	// boardChains shows the chains of ONE conversation, the detail half of
	// boardChainList.
	boardChains
)

// boardConv is one conversation in the chain list: the key rows are grouped by,
// the header spelling shared with the CLI, and the rows themselves.
type boardConv struct {
	Key    string
	Header string
	Rows   []cli.ThreadRow
}

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
	baseCols    []table.Column      // natural column sizing; see fitColumns
	rowTopics   []cli.BoardTopicRow // parallel slice: rowTopics[i] corresponds to table row i
	curTopic    string
	// curFound is BoardRead's found flag for curTopic: false means nothing is
	// retained under that name at all. Kept because the two empty states read
	// differently — "published to and emptied" vs "never published" — and the
	// message list and the content viewport must not disagree about which one
	// this is.
	curFound  bool
	msgs      []cli.BoardMessage
	subs      map[string]int           // per-topic subscriber count for the list
	subRows   []cli.BoardSubscriberRow // rows shown in boardSubscribers mode
	msgCursor int
	content   viewport.Model // payload of msgs[msgCursor], pretty-printed if valid JSON
	// msgSubs is the subscriber set for curTopic, captured with the messages so
	// each row can report how many subscribers have been handed it.
	msgSubs []cli.BoardSubscriberRow
	// chainRows is how many rows the last chain collection returned, across
	// every conversation — the number the list header reports.
	chainConvs  []boardConv
	chainCursor int
	chainRows   int
	status      string // one-line error / confirmation rendered below the table
	// pendingStatus is a status line that must SURVIVE the refresh an action
	// schedules.
	//
	// Every destructive action here sets its result and then re-reads from the
	// server, because only the server knows what the rows look like now — and
	// the Apply* that lands clears the status. The result the operator is meant
	// to read was therefore on screen for exactly one round trip. Held here and
	// consumed by the Apply*, rather than fixed in each handler, because both
	// the per-message and the thread-scoped paths had it.
	pendingStatus string
}

// takePendingStatus installs a status set before a refresh was scheduled.
func (m *BoardModal) takePendingStatus() {
	m.status = m.pendingStatus
	m.pendingStatus = ""
}

// Column positions in the topics table. boardTopicToRow builds its row in this
// order and tests index by these names, so inserting a column cannot silently
// shift an assertion onto its neighbour (which is exactly what adding Retr
// did to the Subs assertion).
const (
	boardColTopic = iota
	boardColMsgs
	boardColRetr
	boardColSubs
	boardColLastSeq
	boardColLastAt
)

// NewBoardModal constructs a BoardModal with fixed column widths for the topics
// table. Mirrors NewConnsModal.
func NewBoardModal() BoardModal {
	cols := []table.Column{
		{Title: "Topic", Width: 30},
		{Title: "Msgs", Width: 5},
		{Title: "Retr", Width: 5},
		{Title: "Subs", Width: 5},
		{Title: "LastSeq", Width: 9},
		{Title: "LastAt", Width: 22},
	}
	t := table.New(table.WithColumns(cols), table.WithFocused(true))
	baseCols := cols
	vp := viewport.New(80, 10)
	vp.SetContent("(select a topic and press Enter to read)")
	return BoardModal{
		topicsTable: t,
		baseCols:    baseCols,
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
	m.topicsTable.SetColumns(fitColumns(m.baseCols, w-4, flexColumn(m.baseCols, "Topic")))
	m.topicsTable.SetHeight(h/2 - 4)
	m.content.Width = w - 4
	m.content.Height = h/2 - 4
}

// ApplyTopics replaces all rows with the given slice and rebuilds the topics
// table. Mirrors ConnsModal.ApplySnapshot.
// ApplyTopics stores the listing as the UNION of topics that exist and names
// that are subscribed. A topic only comes into existence when something is
// published to it, so showing board topics alone hides the state an operator
// most wants to see: subscribed, nothing published yet. Those rows carry
// MsgCount 0 and no last-publish time, and are sorted in by name with the rest.
func (m *BoardModal) ApplyTopics(rows []cli.BoardTopicRow, subs map[string]int) {
	byName := make(map[string]cli.BoardTopicRow, len(rows)+len(subs))
	for _, r := range rows {
		byName[r.Name] = r
	}
	for pat := range subs {
		if _, ok := byName[pat]; !ok {
			byName[pat] = cli.BoardTopicRow{Name: pat}
		}
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)

	m.rowTopics = make([]cli.BoardTopicRow, 0, len(names))
	for _, n := range names {
		m.rowTopics = append(m.rowTopics, byName[n])
	}
	m.subs = subs
	m.rebuildTopicsRows()
	m.takePendingStatus()
}

// ApplyMessages populates message-drilldown mode with the given messages for
// topic. Sets mode to boardMessages on success. Called when DoBoardRead
// completes.
func (m *BoardModal) ApplyMessages(topic string, msgs []cli.BoardMessage, subs []cli.BoardSubscriberRow, found bool) {
	m.curTopic = topic
	m.curFound = found
	m.msgSubs = subs
	m.msgs = make([]cli.BoardMessage, len(msgs))
	copy(m.msgs, msgs)
	m.msgCursor = 0
	// Enter the message view even when nothing is retained. The listing now
	// includes names that are subscribed but never published to, so !found is
	// an ordinary destination rather than a race, and staying on the topic list
	// with only a status line left the reader with nowhere to look. It also
	// makes the content write below observable: the topic-list view does not
	// render the content viewport, so setting it and returning early wrote to
	// something nobody could see.
	m.mode = boardMessages
	m.takePendingStatus()
	m.updateContentFromCursor()
}

// ApplySubscribers stores the subscriber rows and switches to subscribers mode.
// An empty result is meaningful, not an error: the topic is retained on the
// board and nothing would receive a publish to it.
func (m *BoardModal) ApplySubscribers(topic string, rows []cli.BoardSubscriberRow) {
	m.curTopic = topic
	m.subRows = make([]cli.BoardSubscriberRow, len(rows))
	copy(m.subRows, rows)
	m.mode = boardSubscribers
	m.status = ""
}

// ApplyChains groups the collected rows into conversations and switches to the
// chain LIST.
//
// Grouping is not redone here: SelectThreads already stamped every row with its
// Conversation key and returned them in conversation order, so this only has to
// cut the slice where the key changes. Re-deriving it would be a second opinion
// about which exchange a message belongs to, which is exactly what stamping the
// key on the row exists to prevent.
func (m *BoardModal) ApplyChains(rows []cli.ThreadRow) {
	m.chainRows = len(rows)
	m.chainConvs = nil
	for i := 0; i < len(rows); {
		j := i
		for j < len(rows) && rows[j].Conversation == rows[i].Conversation {
			j++
		}
		seg := rows[i:j]
		m.chainConvs = append(m.chainConvs, boardConv{
			Key: seg[0].Conversation,
			// The CLI's own spelling, so the two surfaces cannot name the same
			// conversation differently.
			Header: cli.ConversationHeader(seg),
			Rows:   seg,
		})
		i = j
	}
	if m.chainCursor >= len(m.chainConvs) {
		m.chainCursor = 0
	}
	m.mode = boardChainList
	m.takePendingStatus()
}

// OpenSelectedConversation renders the highlighted conversation into the
// content viewport and descends into the detail view.
//
// The rendering is cli.RenderThreads — the CLI's own — rather than a second
// drawing routine here: the gutter, the header fields and the window statement
// then cannot drift between the two surfaces. BodyEscaped is stated rather than
// derived, because a viewport is not a file and a stray ESC repaints over the
// panel border (the reason sanitizeOutput exists in rawforward.go).
func (m *BoardModal) OpenSelectedConversation() {
	conv, ok := m.selectedConv()
	if !ok {
		return
	}
	var buf strings.Builder
	// Window is empty here and drawn by View instead. The viewport does not
	// wrap, so the statement was being cut at the panel border — and the half
	// it lost was the half that explains ORPHAN. Same sentence, same constant,
	// placed where it can be wrapped to the panel width.
	_ = cli.RenderThreads(&buf, conv.Rows, cli.ThreadRenderOptions{
		Body: cli.BodyEscaped,
	}, nil)
	m.content.SetContent(buf.String())
	m.content.GotoTop()
	m.mode = boardChains
	m.status = ""
}

func (m *BoardModal) selectedConv() (boardConv, bool) {
	if m.chainCursor < 0 || m.chainCursor >= len(m.chainConvs) {
		return boardConv{}, false
	}
	return m.chainConvs[m.chainCursor], true
}

// SelectedConversationKey is what the thread-scoped actions act on. Empty when
// nothing is highlighted, which the caller must treat as "do nothing": a
// destructive verb with no selector is the whole-board form the CLI refuses to
// have, and this surface must not be the one that offers it.
func (m *BoardModal) SelectedConversationKey() string {
	conv, ok := m.selectedConv()
	if !ok {
		return ""
	}
	return conv.Key
}

// PopToChainList returns from one conversation to the list of them.
func (m *BoardModal) PopToChainList() {
	m.mode = boardChainList
	m.status = ""
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

// SetStatusAfterRefresh is SetStatus for a caller that is about to schedule a
// re-read. The line is held until the Apply* lands and installed there, so an
// action's result outlives the refresh it triggers rather than being cleared
// by it.
func (m *BoardModal) SetStatusAfterRefresh(s string) { m.pendingStatus = s }

// rebuildTopicsRows translates rowTopics into bubbles/table rows.
// Mirrors ConnsModal.rebuildRows.
func (m *BoardModal) rebuildTopicsRows() {
	rows := make([]table.Row, 0, len(m.rowTopics))
	for i := range m.rowTopics {
		rows = append(rows, boardTopicToRow(&m.rowTopics[i], m.subs))
	}
	setTableRows(&m.topicsTable, rows)
}

// boardTopicToRow maps a BoardTopicRow to a table.Row (4 columns).
func boardTopicToRow(r *cli.BoardTopicRow, subs map[string]int) table.Row {
	// A real topic always has a last-publish time (topic.append sets it), so a
	// zero one marks a row that exists only because something subscribes to the
	// name. Dash out its seq too: "0 / 0" would read as a topic that was
	// published to and emptied, which is a different state.
	at, lastSeq := "-", "-"
	if r.LastPublishedAtMs > 0 {
		at = time.UnixMilli(int64(r.LastPublishedAtMs)).UTC().Format(time.RFC3339)
		lastSeq = fmt.Sprintf("%d", r.LastSeq)
	}
	// Blank rather than 0 when counts are unavailable (no board_observe), so an
	// absent count is never read as "nobody is listening".
	sub := ""
	if subs != nil {
		sub = fmt.Sprintf("%d", subs[r.Name])
	}
	// Retr counts withdrawn messages (agent retract), kept out of Msgs so that
	// column keeps answering "how much would a subscriber receive". Unlike Subs
	// it is printed even when zero: the count is always available, and a blank
	// here would collide with the meaning blank already carries next door.
	return table.Row{
		r.Name,
		fmt.Sprintf("%d", r.MsgCount),
		fmt.Sprintf("%d", r.RetractedCount),
		sub,
		lastSeq,
		at,
	}
}

// boardEmptyReason names which empty a topic is. Both print nothing, but they
// are different states: a topic that was published to and then emptied (purge,
// ring overflow) still exists on the board, while a subscribed name that was
// never published to has no topic at all. Same split as `harness-cli board
// read` reports on stderr.
func boardEmptyReason(found bool) string {
	if found {
		return "(on the board, but holds no messages)"
	}
	return "(nothing published to this topic)"
}

// updateContentFromCursor refreshes the content viewport from msgs[msgCursor].
// Pretty-prints the payload if json.Valid reports it is valid JSON; otherwise
// uses the raw string representation.
func (m *BoardModal) updateContentFromCursor() {
	if len(m.msgs) == 0 {
		// The reason is stated once, in the message list above; the content
		// viewport is the payload pane and there is no payload to show.
		m.content.SetContent("")
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
	agentName := msg.FromAgentProfile
	if agentName == "" {
		agentName = "-"
	}
	// in-reply-to shows only on replies: a re=0 on every header would be noise
	// on a board where nothing replies.
	inReplyTo := ""
	if msg.InReplyTo != 0 {
		inReplyTo = fmt.Sprintf("  re=%d", msg.InReplyTo)
	}
	// Same rule as re= : the marker prints only when it applies. The payload is
	// still shown in full — a withdrawn message is invisible to agents, not to
	// the operator auditing what was said.
	retracted := ""
	if msg.Retracted {
		// by= rides with the marker for the reason the CLI prints it too: two
		// verbs can withdraw a message now, and a bare RETRACTED would read as
		// the author having done it whoever did.
		retracted = fmt.Sprintf("  RETRACTED at=%s by=%s",
			time.UnixMilli(int64(msg.RetractedAtMs)).UTC().Format(time.RFC3339),
			cli.RetractedByLabel(msg))
	}
	// Same rule as re= : only a sender that declared a reply destination gets
	// this. It is the only view that shows where an answer to this message
	// goes -- the server resolves it off this row, not off the reply's text.
	replyTo := ""
	if msg.ReplyToTopic != "" {
		replyTo = fmt.Sprintf("  reply-to=%s", msg.ReplyToTopic)
	}
	header := fmt.Sprintf("seq=%d%s%s  from=%s  host=%s  agent=%s  at=%s%s", msg.Seq, inReplyTo, replyTo, fromShort, msg.FromHostname, agentName, at, retracted)
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

	case boardChainList:
		if k, ok := msg.(tea.KeyMsg); ok {
			switch k.Type {
			case tea.KeyUp:
				if m.chainCursor > 0 {
					m.chainCursor--
				}
				return m, nil
			case tea.KeyDown:
				if m.chainCursor < len(m.chainConvs)-1 {
					m.chainCursor++
				}
				return m, nil
			}
		}
		return m, nil

	case boardChains:
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
		footer := FooterStyle.Render("Enter: read  c: chains  s: subscribers  r: refresh  x: purge topic  Esc: close")
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
			agentName := msg.FromAgentProfile
			if agentName == "" {
				agentName = "-"
			}
			at := time.UnixMilli(int64(msg.ReceivedAtMs)).UTC().Format("15:04:05Z")
			// One spelling of the delivery mark, shared with the CLI: the
			// comparison is cli.ShownTo's, not a second copy here.
			line := fmt.Sprintf("%s[%d] seq=%-5d  from=%s  agent=%s  %s  %s",
				cursor, i+1, msg.Seq, fromShort, agentName, at,
				cli.ShownToLabel(m.msgSubs, m.curTopic, msg.Seq))
			// A withdrawn message reaches no agent any more; this view is the
			// only place it still exists. Marked and muted rather than hidden —
			// hiding it here would hand the operator the same blank the agents
			// see, which is the opposite of the point.
			if msg.Retracted {
				line = MutedStyle.Render(line + "  RETRACTED by=" + cli.RetractedByLabel(msg))
			}
			msgList.WriteString(line + "\n")
		}
		if len(m.msgs) == 0 {
			msgList.WriteString("  " + boardEmptyReason(m.curFound) + "\n")
		}
		header := HeaderStyle.Render(fmt.Sprintf("topic: %s  (%d msgs)", m.curTopic, len(m.msgs)))
		footer := FooterStyle.Render("↑/↓ select · " + scrollHint + " · w: retract msg  X: purge msg  r: re-read  Esc: back")
		return box.Render(header + "\n" + msgList.String() + "\n" + m.content.View() + statusLine + "\n" + footer)

	case boardSubscribers:
		var list strings.Builder
		for _, r := range m.subRows {
			short := r.TaskHex
			if len(short) > 8 {
				short = short[:8]
			}
			host := r.Hostname
			if host == "" {
				// Registered but never attached: it has a subscription and no
				// harness-cli invocation yet. Not missing data.
				host = "-"
			}
			agentName := r.AgentProfile
			if agentName == "" {
				agentName = "-"
			}
			// shown / pending per topic: how far the automatic injection
			// path has reached for this task, and how much sits above it.
			pats := "-"
			if len(r.Patterns) > 0 {
				parts := make([]string, 0, len(r.Patterns))
				for _, p := range r.Patterns {
					parts = append(parts, fmt.Sprintf("%s(shown=%d pending=%d)", p.Name, p.Shown, p.Pending))
				}
				pats = strings.Join(parts, ",")
			}
			list.WriteString(fmt.Sprintf("  %s  host=%s  agent=%s  topics=%s\n", short, host, agentName, pats))
		}
		if len(m.subRows) == 0 {
			list.WriteString("  (nobody subscribes — a publish here reaches no inbox)\n")
		}
		header := HeaderStyle.Render(fmt.Sprintf("subscribers of %s (%d)", m.curTopic, len(m.subRows)))
		footer := FooterStyle.Render("s: refresh  Esc: back")
		return box.Render(header + "\n" + list.String() + statusLine + "\n" + footer)
	case boardChainList:
		var list strings.Builder
		for i, conv := range m.chainConvs {
			cursor := "  "
			if i == m.chainCursor {
				cursor = "> "
			}
			list.WriteString(cursor + conv.Header + "\n")
		}
		if len(m.chainConvs) == 0 {
			list.WriteString("  (nothing on the board within that window)\n")
		}
		header := HeaderStyle.Render(fmt.Sprintf("conversations across every topic (%d, %d rows)",
			len(m.chainConvs), m.chainRows))
		// Wrapped, not truncated: an operator reading "nothing here" needs the
		// whole sentence to know whether that is the window or a fault.
		window := MutedStyle.Width(m.content.Width).Render(cli.ThreadWindowOperator)
		footer := FooterStyle.Render("↑/↓ select · Enter: open · w: retract thread  X: purge thread  c: refresh  Esc: back")
		return box.Render(header + "\n" + window + "\n" + list.String() + statusLine + "\n" + footer)

	case boardChains:
		// The KEY, not the header. RenderThreads already prints the
		// participant/count/time header as its section line inside the
		// viewport, so repeating it here was the same sentence twice — caught
		// on screen, not by a test. What is worth a fixed line instead is the
		// selector: this is the string `board thread --conversation` and both
		// thread-scoped verbs take, and it scrolls out of reach otherwise.
		key := ""
		if conv, ok := m.selectedConv(); ok {
			key = conv.Key
		}
		header := HeaderStyle.Render("conversation: " + key)
		footer := FooterStyle.Render(scrollHint + " · w: retract thread  X: purge thread  c: refresh  Esc: back")
		return box.Render(header + "\n" + m.content.View() + statusLine + "\n" + footer)
	}
	return box.Render("(unknown board mode)")
}
