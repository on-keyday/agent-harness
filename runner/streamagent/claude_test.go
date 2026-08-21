package streamagent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// These drive a FAKE agent, not claude: the behaviours under test are the
// seam's, and a real model would make them non-deterministic and cost money to
// assert. The real binary is exercised by the probe scripts committed beside
// the spec, which is where "does claude actually do this" is answered.

// fakeAgent writes a script that speaks enough of claude's stream-json to
// exercise the adapter. It echoes the flags it was given to argvFile so a test
// can assert what the adapter appended, and reads its stdin line by line.
func fakeAgent(t *testing.T, argvFile, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-agent.sh")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" > " + argvFile + "\n" + body
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// runAdapter runs the adapter against a fake agent and returns the neutral
// messages it wrote. neutralIn is fed as the adapter's input.
func runAdapter(t *testing.T, agent string, opts ClaudeOpts, neutralIn string) ([]Msg, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	opts.Argv = append([]string{agent}, opts.Argv...)
	opts.Out = &out
	opts.In = strings.NewReader(neutralIn)
	opts.ErrOut = &errOut
	if opts.Dir == "" {
		opts.Dir = t.TempDir()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = RunClaude(ctx, opts)

	var msgs []Msg
	rd := NewReader(bytes.NewReader(out.Bytes()))
	for {
		m, err := rd.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("adapter wrote a line this package cannot read back: %v\nraw:\n%s", err, out.String())
		}
		msgs = append(msgs, m)
	}
	return msgs, errOut.String()
}

// driveAdapter is the interactive form: it reads the adapter's output as it
// arrives and lets the test answer, which is how the runner behaves. The
// canned form above cannot express an approval, because a response is only
// meaningful AFTER the request it names has been emitted -- feeding one up
// front races the stdout pump and is refused, correctly.
//
// onMsg returns true when the test is done; the neutral input then closes,
// which is how a session is asked to end.
func driveAdapter(t *testing.T, agent string, opts ClaudeOpts, onMsg func(Msg, *Writer) bool) ([]Msg, string) {
	t.Helper()
	outR, outW := io.Pipe()
	inR, inW := io.Pipe()
	var errOut bytes.Buffer

	opts.Argv = append([]string{agent}, opts.Argv...)
	opts.Out, opts.In, opts.ErrOut = outW, inR, &errOut
	if opts.Dir == "" {
		opts.Dir = t.TempDir()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() {
		_ = RunClaude(ctx, opts)
		_ = outW.Close()
	}()

	in := NewWriter(inW)
	var msgs []Msg
	rd := NewReader(outR)
	closed := false
	for {
		m, err := rd.Next()
		if err != nil {
			break
		}
		msgs = append(msgs, m)
		if !closed && onMsg != nil && onMsg(m, in) {
			closed = true
			_ = inW.Close()
		}
	}
	if !closed {
		_ = inW.Close()
	}
	return msgs, errOut.String()
}

func kinds(msgs []Msg) []MsgKind {
	out := make([]MsgKind, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Kind)
	}
	return out
}

func firstOfKind(msgs []Msg, k MsgKind) *Msg {
	for i := range msgs {
		if msgs[i].Kind == k {
			return &msgs[i]
		}
	}
	return nil
}

// The adapter owns the vendor protocol flags. If the runner had to supply
// them, the seam would not be a seam.
func TestAdapterAppendsTheVendorFlags(t *testing.T) {
	argvFile := filepath.Join(t.TempDir(), "argv")
	agent := fakeAgent(t, argvFile, "cat > /dev/null\n")
	runAdapter(t, agent, ClaudeOpts{}, "")

	got, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("the fake agent never ran: %v", err)
	}
	argv := string(got)
	for _, want := range []string{
		"-p", "--input-format stream-json", "--output-format stream-json",
		"--permission-prompt-tool stdio",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv is missing %q; got: %s", want, argv)
		}
	}
	if strings.Contains(argv, "--continue") {
		t.Errorf("--continue was appended without ResumeConversation; got: %s", argv)
	}
}

// --resume-conversation is an INTENT the adapter spells, so the runner names no
// vendor flag. --session-id must never appear: it is create-only, so building
// resume on it fails on every resume of a task.
func TestResumeConversationAppendsContinueAndNeverSessionID(t *testing.T) {
	argvFile := filepath.Join(t.TempDir(), "argv")
	agent := fakeAgent(t, argvFile, "cat > /dev/null\n")
	runAdapter(t, agent, ClaudeOpts{ResumeConversation: true}, "")

	argv := readFile(t, argvFile)
	if !strings.Contains(argv, "--continue") {
		t.Errorf("--continue missing; got: %s", argv)
	}
	if strings.Contains(argv, "--session-id") {
		t.Errorf("--session-id was passed. It is create-only (`already in use`, "+
			"exit 1), so a resume built on it dies on every resume; got: %s", argv)
	}
}

// The misconfiguration that otherwise exits `success`: an argv already naming
// a protocol flag must fail the run at start, loudly.
func TestConflictingFlagIsRefusedAtStart(t *testing.T) {
	argvFile := filepath.Join(t.TempDir(), "argv")
	agent := fakeAgent(t, argvFile, "cat > /dev/null\n")
	msgs, _ := runAdapter(t, agent, ClaudeOpts{Argv: []string{"--output-format", "text"}}, "")

	ex := firstOfKind(msgs, KindExit)
	if ex == nil || ex.Exit == nil {
		t.Fatalf("no exit message; got kinds %v", kinds(msgs))
	}
	if ex.Exit.Err == "" {
		t.Error("the run reported no error. This is the exact shape the vendor " +
			"has: every tool call fails and the process still exits success.")
	}
	if !strings.Contains(ex.Exit.Err, "--output-format") {
		t.Errorf("the error does not name the offending flag: %q", ex.Exit.Err)
	}
	if _, err := os.Stat(argvFile); err == nil {
		t.Error("the agent was started despite the refused flag set")
	}
}

// Vendor JSON must become neutral events, and must not leak through.
func TestVendorLinesBecomeNeutralEvents(t *testing.T) {
	argvFile := filepath.Join(t.TempDir(), "argv")
	agent := fakeAgent(t, argvFile,
		`printf '%s\n' '{"type":"system","subtype":"init","session_id":"s-1"}'`+"\n"+
			`printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}'`+"\n"+
			`printf '%s\n' '{"type":"result","subtype":"success","duration_ms":12,"total_cost_usd":0.5}'`+"\n"+
			"cat > /dev/null\n")
	msgs, _ := runAdapter(t, agent, ClaudeOpts{}, "")

	if h := firstOfKind(msgs, KindHello); h == nil || h.Hello.Protocol != ProtocolVersion {
		t.Fatalf("no hello, or the wrong protocol version; kinds %v", kinds(msgs))
	}

	var sawText, sawFinish bool
	for _, m := range msgs {
		if m.Kind != KindEvent {
			continue
		}
		switch m.Event.Kind {
		case EventText:
			sawText = m.Event.Text == "hello"
		case EventFinish:
			sawFinish = true
			if m.Event.Stats == nil || m.Event.Stats.CostUSD != 0.5 {
				t.Errorf("finish carried stats %+v, want cost 0.5", m.Event.Stats)
			}
		}
		if strings.Contains(m.Event.Text, `"type":"assistant"`) {
			t.Errorf("raw vendor JSON leaked into a neutral event: %q", m.Event.Text)
		}
	}
	if !sawText || !sawFinish {
		t.Errorf("text=%v finish=%v; kinds %v", sawText, sawFinish, kinds(msgs))
	}
}

// The approval round trip: a control_request becomes a neutral request, and a
// neutral response becomes a vendor control_response on the agent's stdin.
func TestApprovalRoundTrip(t *testing.T) {
	argvFile := filepath.Join(t.TempDir(), "argv")
	inFile := filepath.Join(t.TempDir(), "agent-stdin")
	agent := fakeAgent(t, argvFile,
		`printf '%s\n' '{"type":"control_request","request_id":"vendor-9","request":{"subtype":"can_use_tool","tool_name":"Write","display_name":"Write","input":{"file_path":"/x","content":"hi"},"tool_use_id":"toolu_1","permission_suggestions":[{"type":"setMode","mode":"acceptEdits","destination":"session"}]}}'`+"\n"+
			"cat > "+inFile+"\n")

	var req *Request
	msgs, _ := driveAdapter(t, agent, ClaudeOpts{}, func(m Msg, in *Writer) bool {
		if m.Kind != KindRequest {
			return false
		}
		req = m.Request
		_ = in.Response(Response{ID: m.Request.ID, Behavior: BehaviorDeny, Message: "no"})
		return true
	})

	if req == nil {
		t.Fatalf("no request surfaced; kinds %v", kinds(msgs))
	}
	if req.Tool != "Write" || req.ToolUseID != "toolu_1" {
		t.Errorf("request = %+v", req)
	}
	if len(req.Suggestions) != 1 || req.Suggestions[0].Mode != "acceptEdits" {
		t.Errorf("suggestions = %+v; the setMode suggestion must survive, it is "+
			"the button row", req.Suggestions)
	}
	var input map[string]any
	if err := json.Unmarshal(req.Input, &input); err != nil || input["file_path"] != "/x" {
		t.Errorf("input did not survive verbatim: %s (%v)", req.Input, err)
	}
	if req.ID == "vendor-9" {
		t.Error("the vendor's request id reached the runner; the adapter owns that mapping")
	}

	wrote := readFile(t, inFile)
	if !strings.Contains(wrote, `"type":"control_response"`) {
		t.Fatalf("no control_response reached the agent; stdin was:\n%s", wrote)
	}
	if !strings.Contains(wrote, `"request_id":"vendor-9"`) {
		t.Errorf("the answer did not carry the VENDOR id; stdin was:\n%s", wrote)
	}
	if !strings.Contains(wrote, `"behavior":"deny"`) || !strings.Contains(wrote, `"message":"no"`) {
		t.Errorf("deny and its reason did not survive; stdin was:\n%s", wrote)
	}
	// subtype reports the transport, not the verdict -- a deny still rides a
	// "success" envelope, and getting that wrong makes every deny look like a
	// protocol error to the agent.
	if !strings.Contains(wrote, `"subtype":"success"`) {
		t.Errorf("the deny was not wrapped in a success envelope; stdin was:\n%s", wrote)
	}
}

// A user turn becomes a vendor user message on the agent's stdin.
func TestUserTurnReachesTheAgent(t *testing.T) {
	argvFile := filepath.Join(t.TempDir(), "argv")
	inFile := filepath.Join(t.TempDir(), "agent-stdin")
	agent := fakeAgent(t, argvFile, "cat > "+inFile+"\n")

	in, _ := json.Marshal(Msg{V: ProtocolVersion, Kind: KindUser, User: &UserTurn{Text: "second turn"}})
	runAdapter(t, agent, ClaudeOpts{Prompt: "first turn"}, string(in)+"\n")

	wrote := readFile(t, inFile)
	for _, want := range []string{`"first turn"`, `"second turn"`} {
		if !strings.Contains(wrote, want) {
			t.Errorf("%s never reached the agent; stdin was:\n%s", want, wrote)
		}
	}
	if !strings.Contains(wrote, `"role":"user"`) {
		t.Errorf("turns were not wrapped as user messages; stdin was:\n%s", wrote)
	}
}

// An unknown control subtype must be REPORTED. Dropping it silently leaves the
// agent blocked on a request nobody knows exists, which reads as a hang with
// no cause -- and mcp_message is a real, untested subtype that could arrive.
func TestUnknownControlSubtypeIsReported(t *testing.T) {
	argvFile := filepath.Join(t.TempDir(), "argv")
	agent := fakeAgent(t, argvFile,
		`printf '%s\n' '{"type":"control_request","request_id":"v-1","request":{"subtype":"mcp_message"}}'`+"\n"+
			"cat > /dev/null\n")
	msgs, _ := runAdapter(t, agent, ClaudeOpts{}, "")

	if r := firstOfKind(msgs, KindRequest); r != nil {
		t.Errorf("an unknown subtype was surfaced as an answerable request: %+v", r.Request)
	}
	var reported bool
	for _, m := range msgs {
		if m.Kind == KindEvent && m.Event.Kind == EventError &&
			strings.Contains(m.Event.Text, "mcp_message") {
			reported = true
			if !m.Event.Warning {
				t.Error("an unhandled subtype ended the run; it should be a diagnostic")
			}
		}
	}
	if !reported {
		t.Errorf("mcp_message was swallowed; kinds %v", kinds(msgs))
	}
}

// Answering twice must not reach the agent twice: §3 derives "first answer
// wins" from cowrite being non-exclusive.
func TestSecondAnswerIsRefused(t *testing.T) {
	argvFile := filepath.Join(t.TempDir(), "argv")
	inFile := filepath.Join(t.TempDir(), "agent-stdin")
	agent := fakeAgent(t, argvFile,
		`printf '%s\n' '{"type":"control_request","request_id":"vendor-1","request":{"subtype":"can_use_tool","tool_name":"Write"}}'`+"\n"+
			"cat > "+inFile+"\n")

	msgs, _ := driveAdapter(t, agent, ClaudeOpts{}, func(m Msg, in *Writer) bool {
		if m.Kind != KindRequest {
			return false
		}
		r := Response{ID: m.Request.ID, Behavior: BehaviorAllow}
		_ = in.Response(r)
		_ = in.Response(r) // the same answer twice: first wins
		return true
	})

	if n := strings.Count(readFile(t, inFile), `"type":"control_response"`); n != 1 {
		t.Errorf("%d control_responses reached the agent, want exactly 1", n)
	}
	var complained bool
	for _, m := range msgs {
		if m.Kind == KindEvent && m.Event.Kind == EventError &&
			strings.Contains(m.Event.Text, "no pending request") {
			complained = true
		}
	}
	if !complained {
		t.Errorf("the duplicate answer was dropped without a word; kinds %v", kinds(msgs))
	}
}

// The agent's stderr goes to the task log verbatim, and never onto the neutral
// stream where it would be parsed as a message.
func TestAgentStderrStaysVerbatimAndOffTheNeutralStream(t *testing.T) {
	argvFile := filepath.Join(t.TempDir(), "argv")
	agent := fakeAgent(t, argvFile,
		"printf '%s\\n' 'boom {\"kind\":\"event\"}' >&2\n"+"cat > /dev/null\n")
	msgs, errOut := runAdapter(t, agent, ClaudeOpts{}, "")

	if !strings.Contains(errOut, `boom {"kind":"event"}`) {
		t.Errorf("stderr was not passed through verbatim: %q", errOut)
	}
	for _, m := range msgs {
		if m.Kind == KindEvent && strings.Contains(m.Event.Text, "boom") {
			t.Error("stderr was parsed onto the neutral stream")
		}
	}
}

// Every run ends with exactly one exit line, so the runner never has to guess
// whether an adapter died or is simply quiet.
func TestRunAlwaysEndsWithOneExit(t *testing.T) {
	argvFile := filepath.Join(t.TempDir(), "argv")
	agent := fakeAgent(t, argvFile, "cat > /dev/null\nexit 3\n")
	msgs, _ := runAdapter(t, agent, ClaudeOpts{}, "")

	var n int
	for _, m := range msgs {
		if m.Kind == KindExit {
			n++
			if m.Exit.Code != 3 {
				t.Errorf("exit code = %d, want 3", m.Exit.Code)
			}
		}
	}
	if n != 1 {
		t.Errorf("%d exit lines, want exactly 1; kinds %v", n, kinds(msgs))
	}
	if msgs[len(msgs)-1].Kind != KindExit {
		t.Errorf("exit was not the last line; kinds %v", kinds(msgs))
	}
}

// Concurrent writers must not interleave halves of a line. The adapter writes
// events from the stdout pump and exit from the run goroutine.
func TestWriterIsLineAtomic(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = w.Event(Event{Kind: EventText, Text: strings.Repeat("x", 500)})
		}(i)
	}
	wg.Wait()
	rd := NewReader(bytes.NewReader(buf.Bytes()))
	var n int
	for {
		m, err := rd.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("interleaved write produced an unreadable line: %v", err)
		}
		if len(m.Event.Text) != 500 {
			t.Fatalf("line was spliced: %d chars", len(m.Event.Text))
		}
		n++
	}
	if n != 50 {
		t.Errorf("read %d lines, wrote 50", n)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// A client writing something that is not the protocol must not END the
// session. `session send` writes RAW BYTES to the stream, so the natural
// operator action for the PTY kind -- typing text -- arrives here as a
// malformed line. Killing the task for it makes the same verb quietly
// destructive in one of the two kinds.
//
// The first version of this test deadlocked instead of failing, which is the
// defect showing itself: the follow-up write blocked forever because the
// adapter had stopped reading its input entirely. The writes happen off the
// read loop now so the failure is an assertion rather than a hang.
func TestAMalformedClientLineDoesNotEndTheSession(t *testing.T) {
	argvFile := filepath.Join(t.TempDir(), "argv")
	// Echoes one event per line it reads, so a turn that gets through is
	// visible ON THE STREAM rather than in a file written at teardown.
	agent := fakeAgent(t, argvFile,
		"while IFS= read -r line; do "+
			`printf '{"type":"assistant","message":{"content":[{"type":"text","text":"echoed"}]}}\n'`+
			"; done\n")

	var sawComplaint, sawEchoAfter bool
	var wrote sync.Once
	_, _ = driveAdapter(t, agent, ClaudeOpts{}, func(m Msg, in *Writer) bool {
		if m.Kind == KindHello {
			// Off the read loop: a blocked write here would stop the loop that
			// observes the result.
			wrote.Do(func() {
				go func() {
					_ = in.WriteRaw([]byte("hello there\n")) // what an operator types
					_ = in.User(UserTurn{Text: "after the garbage"})
				}()
			})
			return false
		}
		if m.Kind == KindEvent && m.Event.Kind == EventError {
			sawComplaint = true
		}
		if m.Kind == KindEvent && m.Event.Kind == EventText && m.Event.Text == "echoed" {
			sawEchoAfter = true
			return true // done: the session survived the garbage
		}
		return false
	})

	if !sawEchoAfter {
		t.Error("a user turn sent AFTER a malformed line never reached the agent: " +
			"one line of non-protocol input ended the session")
	}
	if !sawComplaint {
		t.Error("the malformed line was swallowed without a word")
	}
}
