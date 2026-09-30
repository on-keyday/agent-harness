package streamagent

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/on-keyday/agent-harness/runner/hostcmd"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/on-keyday/agent-harness/runner/agentlog"
)

// vendorFlags are the flags the ADAPTER appends, never the runner's argv
// template. Putting them in the template would return vendor knowledge to the
// runner, which is the one thing this seam exists to prevent (§2).
//
// The set is not a preference. Measured 2026-08-20 against claude 2.1.237:
// --input-format is documented "only works with --print", stream-json is the
// only realtime streaming input the CLI has, and the permission channel exists
// ONLY under it — with --permission-prompt-tool but no --input-format the CLI
// closes stdin after 3s, every approval fails with "AbortError: Stream
// closed", and the process still exits result: success. A run that
// accomplished nothing reporting success is why refuseConflicts below is not
// defensive clutter.
var vendorFlags = []string{
	"-p",
	"--input-format", "stream-json",
	"--output-format", "stream-json",
	"--verbose",
	"--permission-prompt-tool", "stdio",
	// The deltas the chats' progress heartbeat is made from. Without it
	// nothing arrives until a whole block is done, and a long thinking phase
	// cannot be told from a hung process. Measured 2026-09-30 with -p and
	// stream-json. Not in `conflicting`: a duplicate boolean disables nothing.
	"--include-partial-messages",
	// Opus 4.7 and later return EMPTY thinking blocks unless the request asks
	// for a summary (the Agent SDK's ThinkingConfig.display, whose API default
	// is "omitted"). Measured 2026-09-30 on claude 2.1.285 / Opus 5.5 with -p
	// and stream-json: 0 chars without it, a 283-char summary and 25 non-empty
	// thinking deltas with it. The flag is NOT in `claude --help` or the CLI
	// reference -- it was found in the binary -- so a claude that predates it
	// may refuse to start; that is a loud failure at spawn, not a silent one.
	"--thinking-display", "summarized",
}

// conflicting flags a caller must not have set: each one either duplicates or
// silently disables part of vendorFlags.
var conflicting = []string{
	"-p", "--print",
	"--input-format",
	"--output-format",
	"--permission-prompt-tool",
}

// ClaudeOpts configures one adapter run.
type ClaudeOpts struct {
	// Argv is the agent command the runner resolved, already including any
	// sandbox wrapper. The adapter execs it and appends vendorFlags; when the
	// runner resolved --agent-bin to agent-in-podman.sh this leaves the
	// adapter OUTSIDE the container and the agent inside, which is the correct
	// side for harness-side glue.
	Argv []string
	// Dir is the task worktree.
	Dir string
	// ResumeConversation appends --continue. It is an INTENT, not a flag, so
	// the runner never names a vendor flag: measured to work under the framed
	// stdin, staying on the same session id rather than forking. Note the
	// vendor's --session-id is create-only ("already in use", exit 1), so
	// resume must not be built on a minted id.
	ResumeConversation bool
	// Prompt, when non-empty, is sent as the first user turn once the agent is
	// up. Empty leaves the agent idle until the runner sends one.
	Prompt string
	// Getenv reads the environment the agent will run in, for locating its
	// transcript on a resume. Nil means os.Getenv; a test points it at a
	// scratch config dir so it never reads the real home directory.
	Getenv func(string) string

	Out    io.Writer // neutral NDJSON out (the runner's pipe)
	In     io.Reader // neutral NDJSON in
	ErrOut io.Writer // agent stderr, verbatim, for the task log
}

// RunClaude runs one event-stream session end to end. It returns when the
// agent exits or the neutral input closes.
func RunClaude(ctx context.Context, o ClaudeOpts) error {
	w := NewWriter(o.Out)

	if len(o.Argv) == 0 {
		_ = w.Exit(Exit{Code: -1, Err: "no agent argv"})
		return errors.New("no agent argv")
	}
	if err := refuseConflicts(o.Argv); err != nil {
		// §2's failure discipline: fail the task at start, loudly. A warn here
		// would hand every task a silently agent-less run — and this specific
		// misconfiguration is the one that otherwise exits `success`.
		_ = w.Exit(Exit{Code: -1, Err: err.Error()})
		return err
	}

	argv := append([]string{}, o.Argv...)
	argv = append(argv, vendorFlags...)
	if o.ResumeConversation {
		argv = append(argv, "--continue")
	}

	// hostcmd, not exec: stdin/stdout are pipes the harness reads, so there is
	// no console for anyone to look at -- but Windows allocates one anyway, and
	// it appears on the desktop.
	cmd := hostcmd.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = o.Dir
	cmd.Env = os.Environ()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = w.Exit(Exit{Code: -1, Err: "stdin pipe: " + err.Error()})
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = w.Exit(Exit{Code: -1, Err: "stdout pipe: " + err.Error()})
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = w.Exit(Exit{Code: -1, Err: "stderr pipe: " + err.Error()})
		return err
	}
	if err := cmd.Start(); err != nil {
		_ = w.Exit(Exit{Code: -1, Err: "start " + argv[0] + ": " + err.Error()})
		return err
	}

	var closeOnce sync.Once
	closeAgentIn := func() { closeOnce.Do(func() { _ = stdin.Close() }) }
	a := &claudeAdapter{w: w, agentIn: stdin, agentInClose: closeAgentIn,
		nonce:   newRunNonce(),
		pending: map[string]pendingRequest{}, interrupts: map[string]struct{}{}}

	_ = w.Hello(Hello{
		Protocol:     ProtocolVersion,
		Vendor:       "claude",
		Capabilities: []string{CapApprovals, CapUserTurns, CapInterrupt},
	})

	// The replay is written BEFORE the stdout pump starts, so no live event
	// can interleave with it. The agent's own output waits in its pipe.
	if o.ResumeConversation {
		getenv := o.Getenv
		if getenv == nil {
			getenv = os.Getenv
		}
		a.replayedSession = a.replayTranscript(getenv, o.Dir)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.pumpAgentStdout(stdout) }()
	go func() { defer wg.Done(); copyLines(stderr, o.ErrOut) }()

	if o.Prompt != "" {
		_ = a.sendUserTurn(o.Prompt)
	}

	// Both endings have to be watched, because either can come first and
	// waiting for the wrong one hangs. Measured as a 45-second test hang when
	// this pumped stdin on the calling goroutine: an agent that exits on its
	// own leaves the adapter blocked on an input nobody will ever close.
	//
	//   - the neutral input ends  → close the agent's stdin, let it finish
	//   - the AGENT exits first   → stop; there is nothing left to feed
	inDone := make(chan error, 1)
	go func() { inDone <- a.pumpNeutralIn(o.In) }()
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	var inErr, waitErr error
	select {
	case inErr = <-inDone:
		closeAgentIn()
		waitErr = <-waitCh
	case waitErr = <-waitCh:
		// The input pump is left blocked on a read that has no cancellable
		// form. That is fine here and only here: this process is on its way
		// out, so the goroutine dies with it.
		closeAgentIn()
	}
	wg.Wait()

	ex := Exit{Code: exitCode(waitErr)}
	if inErr != nil && !errors.Is(inErr, io.EOF) {
		ex.Err = inErr.Error()
	}
	_ = w.Exit(ex)
	return waitErr
}

// refuseConflicts rejects an argv that already names a flag the adapter owns.
func refuseConflicts(argv []string) error {
	for _, arg := range argv[1:] {
		name := arg
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		for _, bad := range conflicting {
			if name == bad {
				return fmt.Errorf("agent argv names %s, which this adapter sets itself; "+
					"the runner's argv template must not carry vendor protocol flags", arg)
			}
		}
	}
	return nil
}

type claudeAdapter struct {
	w       *Writer
	agentIn io.Writer
	// agentInClose ends the session: closing the agent's stdin lets it finish
	// the turn in flight and exit 0. Guarded because the run loop closes it too
	// on its own teardown paths.
	agentInClose func()
	dec          agentlog.Decoder

	mu sync.Mutex
	// pending maps OUR request id to what answering it needs. The runner never
	// sees the vendor id, so a vendor that changes its correlation scheme stops
	// at this map rather than reaching the runner.
	pending map[string]pendingRequest
	// interrupts are the ids of control_requests WE sent, so their receipts can
	// be recognised. Without this the receipt falls through to the agentlog
	// decoder and surfaces as a raw event carrying vendor JSON — which is
	// exactly the leak the seam exists to prevent, and which a test asserts
	// against for the other direction.
	interrupts map[string]struct{}
	seq        int
	// nonce makes a request id unique across RUNS, not just within one. seq is
	// per-process, so a resumed task restarts at 1 — and the id is precisely
	// the staleness guard (design §3): without this, an `approve req-1` an
	// operator was holding from before a resume answers a DIFFERENT request.
	// Observed live on 2026-08-21: a fresh session's first approval really is
	// `req-1`, so the collision needs no imagination.
	nonce    string
	finished bool

	// replayedSession is the transcript a resume replayed, checked once
	// against the session the agent actually resumed (design §3.6). The file
	// was chosen by the adapter's reading of `--continue`, so a disagreement is
	// reported rather than left to show the wrong history silently.
	replayedSession string
	sessionChecked  bool

	// prog is the phase in flight, for the progress heartbeat (design:
	// docs/superpowers/specs/2026-09-30-stream-progress-design.md §2). Only
	// the stdout pump touches it, so it needs no lock.
	prog     Progress
	progOn   bool      // a phase is in flight
	progLast time.Time // when the last heartbeat went out
	now      func() time.Time
}

// progressInterval bounds the heartbeat: one per phase start, then at most one
// a second while the phase keeps producing.
const progressInterval = time.Second

func (a *claudeAdapter) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

// progressStart begins a phase and announces it at once.
func (a *claudeAdapter) progressStart(phase, tool string) {
	a.prog = Progress{Phase: phase, Tool: tool, Tokens: a.prog.Tokens}
	if phase != PhaseThinking {
		a.prog.Tokens = 0 // a token estimate belongs to the thinking phase
	}
	a.progOn = true
	a.progLast = a.clock()
	_ = a.w.Progress(a.prog)
}

// progressTick announces the running totals, at most once per interval.
func (a *claudeAdapter) progressTick() {
	if !a.progOn {
		return
	}
	if now := a.clock(); now.Sub(a.progLast) >= progressInterval {
		a.progLast = now
		_ = a.w.Progress(a.prog)
	}
}

// streamEvent is the part of a partial-message line the heartbeat reads.
type streamEvent struct {
	Event struct {
		Type         string `json:"type"`
		ContentBlock struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"content_block"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			PartialJSON string `json:"partial_json"`
		} `json:"delta"`
	} `json:"event"`
}

// handleStreamEvent consumes one partial-message line. It never reaches the
// agentlog decoder: the complete block arrives as its own assistant line, and
// that is what becomes an event.
func (a *claudeAdapter) handleStreamEvent(line []byte) {
	var se streamEvent
	if json.Unmarshal(line, &se) != nil {
		return
	}
	e := se.Event
	switch e.Type {
	case "content_block_start":
		switch e.ContentBlock.Type {
		case "thinking", "redacted_thinking":
			a.progressStart(PhaseThinking, "")
		case "text":
			a.progressStart(PhaseText, "")
		case "tool_use", "server_tool_use":
			a.progressStart(PhaseToolInput, e.ContentBlock.Name)
		}
	case "content_block_delta":
		n := utf8.RuneCountInString(e.Delta.Text) +
			utf8.RuneCountInString(e.Delta.Thinking) +
			utf8.RuneCountInString(e.Delta.PartialJSON)
		if n == 0 {
			return // signature_delta and the like carry nothing to count
		}
		a.prog.Chars += n
		a.progressTick()
	case "content_block_stop":
		a.progOn = false
	}
}

// thinkingTokens takes claude's running estimate for the thinking phase. It
// arrives during thinking with or without partial messages, and used to be
// dropped because the decoder makes no event of a non-init system line.
func (a *claudeAdapter) thinkingTokens(line []byte) {
	var t struct {
		Estimated int `json:"estimated_tokens"`
	}
	if json.Unmarshal(line, &t) != nil || t.Estimated <= 0 {
		return
	}
	if !a.progOn || a.prog.Phase != PhaseThinking {
		a.prog.Tokens = t.Estimated
		a.progressStart(PhaseThinking, "")
		return
	}
	a.prog.Tokens = t.Estimated
	a.progressTick()
}

// newRunNonce is the per-run half of a request id. Random rather than a
// timestamp: two adapters can start inside one clock tick.
func newRunNonce() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A degenerate nonce is worse than a random one and better than none:
		// it still separates this run from every run that got randomness, and
		// it says in the id itself that this one did not.
		return "norand"
	}
	return hex.EncodeToString(b[:])
}

// mintRequestID returns the next approval id for this run. It takes a.mu
// itself; callers must not hold it.
func (a *claudeAdapter) mintRequestID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seq++
	return "req-" + a.nonce + "-" + strconv.Itoa(a.seq)
}

func (a *claudeAdapter) decoder() agentlog.Decoder {
	if a.dec == nil {
		a.dec = agentlog.NewDecoder("claude-stream-json")
	}
	return a.dec
}

// pumpAgentStdout is the vendor→neutral direction.
func (a *claudeAdapter) pumpAgentStdout(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	for sc.Scan() {
		a.handleAgentLine(sc.Bytes())
	}
}

// vendorLine is the discriminating envelope, decoded loosely on purpose:
// anything this build does not recognise falls through to agentlog, which
// never fails a line.
type vendorLine struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	RequestID string          `json:"request_id"`
	Request   json.RawMessage `json:"request"`
	SessionID string          `json:"session_id"`
}

func (a *claudeAdapter) handleAgentLine(line []byte) {
	if len(trimSpace(line)) == 0 {
		return
	}
	var v vendorLine
	if err := json.Unmarshal(line, &v); err == nil {
		if v.Type == "system" && v.Subtype == "init" {
			a.checkReplayedSession(v.SessionID)
		}
		if v.Type == "stream_event" {
			a.handleStreamEvent(line)
			return
		}
		if v.Type == "system" && v.Subtype == "thinking_tokens" {
			a.thinkingTokens(line)
		}
		if v.Type == "control_request" && a.handleControlRequest(v) {
			return
		}
		if v.Type == "control_response" && a.handleControlResponse(line) {
			return
		}
	}
	for _, e := range a.decoder().Decode(line) {
		ev := FromAgentlog(e)
		addClaudeExtras(&ev, v, line)
		_ = a.w.Event(ev)
	}
}

// checkReplayedSession warns when the live session is not the one replayed.
// Only the first init counts: an interrupt makes the agent emit another.
func (a *claudeAdapter) checkReplayedSession(live string) {
	if a.sessionChecked || a.replayedSession == "" || live == "" {
		return
	}
	a.sessionChecked = true
	if live != a.replayedSession {
		_ = a.w.Event(Event{Kind: EventError, Warning: true,
			Text: "the conversation shown above is session " + a.replayedSession +
				", but the agent resumed session " + live})
	}
}

// controlRequest is the can_use_tool payload. Fields beyond these are ignored
// here and preserved only where they are actionable (Input).
type controlRequest struct {
	Subtype     string            `json:"subtype"`
	ToolName    string            `json:"tool_name"`
	DisplayName string            `json:"display_name"`
	Description string            `json:"description"`
	Input       json.RawMessage   `json:"input"`
	ToolUseID   string            `json:"tool_use_id"`
	Suggestions []json.RawMessage `json:"permission_suggestions"`
}

// pendingRequest is what answering one request needs: the vendor's id, the
// tool's input (echoed on an allow — the vendor reads a missing input as an
// empty one), the questions it asks (for a question tool) and its
// suggestions verbatim (a suggestion is accepted by echoing it back).
type pendingRequest struct {
	vendorID    string
	input       json.RawMessage
	questions   []Question
	suggestions []json.RawMessage
}

// askUserQuestionTool is claude's clarifying-question tool. It is recognised
// HERE and nowhere else: the neutral request carries its questions as
// Request.Questions, so no client needs this name or its input shape.
const askUserQuestionTool = "AskUserQuestion"

// questionsOf decodes AskUserQuestion's input. A shape it cannot read yields
// nil, and the request stays an ordinary approval — the pre-existing behaviour
// — rather than a question no surface can render.
func questionsOf(input json.RawMessage) []Question {
	var in struct {
		Questions []struct {
			Question    string `json:"question"`
			Header      string `json:"header"`
			MultiSelect bool   `json:"multiSelect"`
			Options     []struct {
				Label       string `json:"label"`
				Description string `json:"description"`
			} `json:"options"`
		} `json:"questions"`
	}
	if json.Unmarshal(input, &in) != nil || len(in.Questions) == 0 {
		return nil
	}
	var out []Question
	for _, q := range in.Questions {
		if q.Question == "" || len(q.Options) == 0 {
			return nil
		}
		nq := Question{Question: q.Question, Header: q.Header, MultiSelect: q.MultiSelect}
		for _, o := range q.Options {
			nq.Options = append(nq.Options, Option{Label: o.Label, Description: o.Description})
		}
		out = append(out, nq)
	}
	return out
}

// resolveAnswers maps each answer key — a question's text, or its header — to
// the question's text, and shapes the value the way the vendor documents: a
// multi-select question takes the label list, a single-select one the label
// (several are joined with ", "). A key that names no question is an error:
// an answer aimed at something that is not there must not be applied to
// something that is.
func resolveAnswers(qs []Question, answers map[string][]string) (map[string]any, error) {
	out := map[string]any{}
	for key, vals := range answers {
		var q *Question
		for i := range qs {
			if qs[i].Question == key {
				q = &qs[i]
				break
			}
		}
		if q == nil {
			for i := range qs {
				if qs[i].Header != "" && qs[i].Header == key {
					q = &qs[i]
					break
				}
			}
		}
		if q == nil {
			return nil, fmt.Errorf("answer names no question of this request: %q", key)
		}
		if len(vals) == 0 {
			continue
		}
		if q.MultiSelect {
			out[q.Question] = append([]string(nil), vals...)
		} else {
			out[q.Question] = strings.Join(vals, ", ")
		}
	}
	return out, nil
}

// handleControlRequest reports whether the line was consumed as a request.
func (a *claudeAdapter) handleControlRequest(v vendorLine) bool {
	var cr controlRequest
	if err := json.Unmarshal(v.Request, &cr); err != nil {
		return false
	}
	if cr.Subtype != "can_use_tool" {
		// hook_callback and mcp_message exist as subtypes. The probe showed
		// hooks do NOT arrive here (they surface as system/hook_* events) and
		// mcp_message was never exercised, so an unknown subtype is reported
		// rather than answered or dropped: an approval channel that silently
		// ignores a request the agent is blocked on is a hang with no cause.
		_ = a.w.Event(Event{
			Kind:    EventError,
			Text:    "unhandled control_request subtype " + cr.Subtype,
			Warning: true,
			Extras:  map[string]string{"claude.control_subtype": cr.Subtype},
		})
		return true
	}

	id := a.mintRequestID()
	var questions []Question
	if cr.ToolName == askUserQuestionTool {
		questions = questionsOf(cr.Input)
	}
	a.mu.Lock()
	a.pending[id] = pendingRequest{vendorID: v.RequestID, input: cr.Input,
		questions: questions, suggestions: cr.Suggestions}
	a.mu.Unlock()

	req := Request{
		ID:          id,
		Tool:        cr.ToolName,
		DisplayName: cr.DisplayName,
		Description: cr.Description,
		Input:       cr.Input,
		ToolUseID:   cr.ToolUseID,
		Questions:   questions,
	}
	for _, raw := range cr.Suggestions {
		var s struct {
			Type        string `json:"type"`
			Mode        string `json:"mode"`
			Destination string `json:"destination"`
		}
		_ = json.Unmarshal(raw, &s)
		req.Suggestions = append(req.Suggestions, Suggestion{
			Type: s.Type, Mode: s.Mode, Destination: s.Destination,
		})
	}
	_ = a.w.Request(req)
	return true
}

// handleControlResponse consumes a receipt for a control_request WE sent, so it
// does not fall through to the log decoder as raw vendor JSON. Reports whether
// the line was consumed.
func (a *claudeAdapter) handleControlResponse(line []byte) bool {
	var r struct {
		Response struct {
			Subtype   string `json:"subtype"`
			RequestID string `json:"request_id"`
			Error     string `json:"error"`
			Response  struct {
				StillQueued []any `json:"still_queued"`
			} `json:"response"`
		} `json:"response"`
	}
	if err := json.Unmarshal(line, &r); err != nil {
		return false
	}
	id := r.Response.RequestID
	a.mu.Lock()
	_, mine := a.interrupts[id]
	delete(a.interrupts, id)
	a.mu.Unlock()
	if !mine {
		return false // not ours; let the decoder have it
	}
	ev := Event{Kind: EventError, Warning: true, Text: "interrupt acknowledged"}
	if r.Response.Subtype != "success" {
		ev.Text = "interrupt refused: " + r.Response.Error
	} else if n := len(r.Response.Response.StillQueued); n > 0 {
		ev.Extras = map[string]string{"claude.still_queued": strconv.Itoa(n)}
	}
	_ = a.w.Event(ev)
	return true
}

// pumpNeutralIn is the neutral→vendor direction.
func (a *claudeAdapter) pumpNeutralIn(r io.Reader) error {
	rd := NewReader(r)
	for {
		m, err := rd.Next()
		if errors.Is(err, ErrBadLine) {
			// One bad line is one bad line. Reported so a client is not left
			// wondering, and then we keep reading: `session send` puts raw
			// bytes on this stream, so anything at all can arrive, and ending
			// the task for it would make that verb destructive.
			_ = a.w.Event(Event{Kind: EventError, Warning: true,
				Text: "ignored a line that is " + err.Error()})
			continue
		}
		if err != nil {
			return err
		}
		switch m.Kind {
		case KindResponse:
			if m.Response == nil {
				continue
			}
			if err := a.answer(*m.Response); err != nil {
				_ = a.w.Event(Event{Kind: EventError, Text: err.Error(), Warning: true})
			}
		case KindUser:
			if m.User == nil {
				continue
			}
			// Reported, not discarded. The `_ =` this replaced swallowed the
			// one error that matters here: a turn written after a finish goes
			// to a closed pipe, and the client would never learn its message
			// went nowhere.
			if err := a.sendUserTurn(m.User.Text); err != nil {
				_ = a.w.Event(Event{Kind: EventError, Warning: true,
					Text: "user turn not delivered: " + err.Error()})
			}
		case KindFinish:
			// The clean end: the agent completes its turn and exits. Nothing
			// can follow, and the pump keeps reading only so that a client
			// writing after it gets told rather than ignored.
			if a.agentInClose != nil {
				a.agentInClose()
			}
			a.mu.Lock()
			a.finished = true
			a.mu.Unlock()
		case KindInterrupt:
			if err := a.sendInterrupt(); err != nil {
				_ = a.w.Event(Event{Kind: EventError, Text: err.Error(), Warning: true})
			}
		default:
			// Unknown kinds are reported, not dropped: the runner believing it
			// sent something the adapter ignored is the failure this seam is
			// most likely to have.
			_ = a.w.Event(Event{
				Kind: EventError, Warning: true,
				Text: "adapter ignored an unknown message kind " + string(m.Kind),
			})
		}
	}
}

func (a *claudeAdapter) answer(r Response) error {
	// Validated BEFORE the request is taken out of the table: a refused answer
	// leaves it pending, answerable by a corrected one.
	a.mu.Lock()
	p, ok := a.pending[r.ID]
	if !ok {
		a.mu.Unlock()
		return fmt.Errorf("no pending request %q (already answered, or never issued)", r.ID)
	}
	answering := len(r.Answers) > 0 || r.Reply != ""
	var answers map[string]any
	switch {
	case answering && len(p.questions) == 0:
		a.mu.Unlock()
		return fmt.Errorf("request %q asks no question; answers and a reply mean nothing on a tool approval", r.ID)
	case answering && r.Behavior == BehaviorDeny:
		a.mu.Unlock()
		return fmt.Errorf("request %q: answers ride an allow, not a deny", r.ID)
	case answering:
		var err error
		if answers, err = resolveAnswers(p.questions, r.Answers); err != nil {
			a.mu.Unlock()
			return fmt.Errorf("request %q: %w", r.ID, err)
		}
	}
	if r.AcceptSuggestion != nil && (*r.AcceptSuggestion < 0 || *r.AcceptSuggestion >= len(p.suggestions)) {
		a.mu.Unlock()
		return fmt.Errorf("request %q has no suggestion %d", r.ID, *r.AcceptSuggestion)
	}
	delete(a.pending, r.ID)
	a.mu.Unlock()

	inner := map[string]any{}
	switch r.Behavior {
	case BehaviorDeny:
		inner["behavior"] = "deny"
		if r.Message != "" {
			// Measured: this reaches the agent as a tool_result with is_error.
			inner["message"] = r.Message
		}
	default:
		inner["behavior"] = "allow"
		// Always send updatedInput. Omitting it is not "unchanged" -- the
		// vendor reads a missing input as an empty one -- so an allow with no
		// rewrite echoes the request's own input.
		switch {
		case len(r.UpdatedInput) > 0:
			inner["updatedInput"] = json.RawMessage(r.UpdatedInput)
		case answering:
			inner["updatedInput"] = answeredInput(p.input, answers, r.Reply)
		case len(p.input) > 0:
			inner["updatedInput"] = p.input
		default:
			inner["updatedInput"] = json.RawMessage(`{}`)
		}
		// Accepting a suggestion is echoing it back verbatim in
		// updatedPermissions (code.claude.com/docs/en/agent-sdk/user-input,
		// "Approve and remember"). This was accepted on the wire and silently
		// dropped here until 2026-09-30.
		if r.AcceptSuggestion != nil {
			inner["updatedPermissions"] = []json.RawMessage{p.suggestions[*r.AcceptSuggestion]}
		}
	}

	out := map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success", // reports the TRANSPORT, not the verdict
			"request_id": p.vendorID,
			"response":   inner,
		},
	}
	if err := a.writeVendor(out); err != nil {
		return err
	}
	// Only now is the request settled, and the line says so on the stream the
	// server keeps: a follower replaying it sees the request AND its end.
	verdict := BehaviorAllow
	if r.Behavior == BehaviorDeny {
		verdict = BehaviorDeny
	}
	_ = a.w.Resolved(Resolved{ID: r.ID, Behavior: verdict})
	return nil
}

// answeredInput is AskUserQuestion's updatedInput: the tool's own input with
// `answers` (and `response`, for a freeform reply) added, which is the shape
// the vendor documents -- the original questions ride along unchanged.
func answeredInput(input json.RawMessage, answers map[string]any, reply string) json.RawMessage {
	obj := map[string]json.RawMessage{}
	_ = json.Unmarshal(input, &obj)
	if len(answers) > 0 {
		b, _ := json.Marshal(answers)
		obj["answers"] = b
	}
	if reply != "" {
		b, _ := json.Marshal(reply)
		obj["response"] = b
	}
	b, _ := json.Marshal(obj)
	return b
}

// sendInterrupt asks the agent to abandon its running turn. Measured shape:
// a control_request of subtype "interrupt", answered with a receipt carrying
// still_queued. The id is minted here for the same reason approval ids are —
// the vendor's correlation scheme stops at this seam.
func (a *claudeAdapter) sendInterrupt() error {
	a.mu.Lock()
	a.seq++
	id := "int-" + strconv.Itoa(a.seq)
	a.interrupts[id] = struct{}{}
	a.mu.Unlock()
	return a.writeVendor(map[string]any{
		"type":       "control_request",
		"request_id": id,
		"request":    map[string]any{"subtype": "interrupt"},
	})
}

// sendUserTurn feeds a turn to the agent and then puts it on the stream the
// server keeps. The agent does not echo a turn back under stream-json, so
// without this line a turn existed only in the chat that sent it: another
// client, a later reattach and the task log never saw who asked what.
func (a *claudeAdapter) sendUserTurn(text string) error {
	if err := a.writeVendor(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": text},
	}); err != nil {
		return err
	}
	_ = a.w.Event(Event{Kind: EventUserText, Text: text})
	return nil
}

func (a *claudeAdapter) writeVendor(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.finished {
		return fmt.Errorf("the session was finished; the agent's input is closed")
	}
	_, err = a.agentIn.Write(append(b, '\n'))
	return err
}

// addClaudeExtras carries the vendor-specific detail §1 assigns to extras.
// Keys are namespaced; values are strings. Only what the spec's table names is
// carried — an extras map that grows by reflex is the dumping ground the rules
// exist to prevent.
func addClaudeExtras(ev *Event, v vendorLine, line []byte) {
	set := func(k, val string) {
		if val == "" {
			return
		}
		if ev.Extras == nil {
			ev.Extras = map[string]string{}
		}
		ev.Extras[k] = val
	}
	if v.Type == "system" {
		set("claude.subtype", v.Subtype)
	}
	set("claude.session_id", v.SessionID)

	switch {
	case v.Type == "system" && v.Subtype == "thinking_tokens":
		var t struct {
			Estimated int64 `json:"estimated_tokens"`
		}
		if json.Unmarshal(line, &t) == nil && t.Estimated > 0 {
			set("claude.thinking_tokens", strconv.FormatInt(t.Estimated, 10))
		}
	case v.Type == "rate_limit_event":
		var t struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(line, &t) == nil {
			set("claude.rate_limit.status", t.Status)
		}
	case v.Type == "system" && v.Subtype == "informational":
		var t struct {
			Content string `json:"content"`
			Level   string `json:"level"`
		}
		if json.Unmarshal(line, &t) == nil {
			set("claude.informational", t.Content)
			set("claude.level", t.Level)
		}
	}
}

func copyLines(r io.Reader, w io.Writer) {
	if w == nil {
		w = io.Discard
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	for sc.Scan() {
		fmt.Fprintf(w, "%s\n", sc.Bytes())
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}
