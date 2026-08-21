package streamagent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"

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

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
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

	a := &claudeAdapter{w: w, agentIn: stdin, pending: map[string]string{}}

	_ = w.Hello(Hello{
		Protocol:     ProtocolVersion,
		Vendor:       "claude",
		Capabilities: []string{CapApprovals, CapUserTurns, CapInterrupt},
	})

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
		_ = stdin.Close()
		waitErr = <-waitCh
	case waitErr = <-waitCh:
		// The input pump is left blocked on a read that has no cancellable
		// form. That is fine here and only here: this process is on its way
		// out, so the goroutine dies with it.
		_ = stdin.Close()
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
	dec     agentlog.Decoder

	mu sync.Mutex
	// pending maps OUR request id to the vendor's. The runner never sees the
	// vendor id, so a vendor that changes its correlation scheme stops at this
	// map rather than reaching the runner.
	pending map[string]string
	seq     int
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
		if v.Type == "control_request" && a.handleControlRequest(v) {
			return
		}
	}
	for _, e := range a.decoder().Decode(line) {
		ev := toNeutral(e)
		addClaudeExtras(&ev, v, line)
		_ = a.w.Event(ev)
	}
}

// controlRequest is the can_use_tool payload. Fields beyond these are ignored
// here and preserved only where they are actionable (Input).
type controlRequest struct {
	Subtype     string          `json:"subtype"`
	ToolName    string          `json:"tool_name"`
	DisplayName string          `json:"display_name"`
	Description string          `json:"description"`
	Input       json.RawMessage `json:"input"`
	ToolUseID   string          `json:"tool_use_id"`
	Suggestions []struct {
		Type        string `json:"type"`
		Mode        string `json:"mode"`
		Destination string `json:"destination"`
	} `json:"permission_suggestions"`
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

	a.mu.Lock()
	a.seq++
	id := "req-" + strconv.Itoa(a.seq)
	a.pending[id] = v.RequestID
	a.mu.Unlock()

	req := Request{
		ID:          id,
		Tool:        cr.ToolName,
		DisplayName: cr.DisplayName,
		Description: cr.Description,
		Input:       cr.Input,
		ToolUseID:   cr.ToolUseID,
	}
	for _, s := range cr.Suggestions {
		req.Suggestions = append(req.Suggestions, Suggestion{
			Type: s.Type, Mode: s.Mode, Destination: s.Destination,
		})
	}
	_ = a.w.Request(req)
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
			_ = a.sendUserTurn(m.User.Text)
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
	a.mu.Lock()
	vendorID, ok := a.pending[r.ID]
	if ok {
		delete(a.pending, r.ID)
	}
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("no pending request %q (already answered, or never issued)", r.ID)
	}

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
		// Always send updatedInput. Omitting it is not "unchanged" — the
		// vendor reads a missing input as an empty one.
		if len(r.UpdatedInput) > 0 {
			inner["updatedInput"] = json.RawMessage(r.UpdatedInput)
		} else {
			inner["updatedInput"] = json.RawMessage(a.originalInput(r.ID))
		}
	}

	out := map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success", // reports the TRANSPORT, not the verdict
			"request_id": vendorID,
			"response":   inner,
		},
	}
	return a.writeVendor(out)
}

// originalInput is a placeholder for the skeleton: the adapter keeps only the
// id mapping so far, so an allow with no UpdatedInput echoes an empty object.
// Storing the original input alongside the id is the next step and is left
// visible rather than hidden behind a nil.
func (a *claudeAdapter) originalInput(string) []byte { return []byte(`{}`) }

func (a *claudeAdapter) sendUserTurn(text string) error {
	return a.writeVendor(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": text},
	})
}

func (a *claudeAdapter) writeVendor(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_, err = a.agentIn.Write(append(b, '\n'))
	return err
}

// toNeutral maps agentlog's Event onto the wire type. Kept as an explicit
// switch rather than an int cast so adding a Kind on either side is a compile
// error rather than a silently mislabelled event.
func toNeutral(e agentlog.Event) Event {
	out := Event{
		Text: e.Text, Tool: e.Tool, Args: e.Args, Result: e.Result,
		ExitCode: e.ExitCode, IsError: e.IsError, Warning: e.Warning,
	}
	switch e.Kind {
	case agentlog.KindRaw:
		out.Kind = EventRaw
	case agentlog.KindSessionStart:
		out.Kind = EventSessionStart
	case agentlog.KindThinking:
		out.Kind = EventThinking
	case agentlog.KindToolStart:
		out.Kind = EventToolStart
	case agentlog.KindToolEnd:
		out.Kind = EventToolEnd
	case agentlog.KindText:
		out.Kind = EventText
	case agentlog.KindFinish:
		out.Kind = EventFinish
	case agentlog.KindError:
		out.Kind = EventError
	default:
		out.Kind = EventRaw
	}
	if s := e.Stats; s != (agentlog.Stats{}) {
		out.Stats = &Stats{
			DurationMS: s.DurationMS, CostUSD: s.CostUSD,
			InputTokens: s.InputTokens, OutputTokens: s.OutputTokens,
		}
	}
	return out
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
