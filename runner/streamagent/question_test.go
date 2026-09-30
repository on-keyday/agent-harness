package streamagent

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

const askInput = `{"questions":[` +
	`{"question":"How should I format the output?","header":"Format","multiSelect":false,"options":[{"label":"Summary","description":"brief"},{"label":"Detailed","description":"full"}]},` +
	`{"question":"Which sections?","header":"Sections","multiSelect":true,"options":[{"label":"Intro","description":""},{"label":"Outro","description":""}]}]}`

// askAgent emits one control_request for tool with input, then writes every
// line it is sent to file so the test can read the control_response.
func askAgent(t *testing.T, tool, input, extra string) (agent, inFile string) {
	t.Helper()
	inFile = filepath.Join(t.TempDir(), "agent-stdin")
	req := `{"type":"control_request","request_id":"vendor-9","request":{"subtype":"can_use_tool","tool_name":"` + tool + `","input":` + input + extra + `}}`
	return fakeAgent(t, filepath.Join(t.TempDir(), "argv"),
		"printf '%s\\n' '"+req+"'\n"+"cat > "+inFile+"\n"), inFile
}

// vendorResponse reads the control_response the adapter wrote, if any.
func vendorResponse(t *testing.T, inFile string) (map[string]any, bool) {
	t.Helper()
	for _, l := range strings.Split(readFile(t, inFile), "\n") {
		if !strings.Contains(l, `"control_response"`) {
			continue
		}
		var m struct {
			Response struct {
				Response map[string]any `json:"response"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("unreadable control_response %q: %v", l, err)
		}
		return m.Response.Response, true
	}
	return nil, false
}

// answerOnce drives the adapter: on the request it sends resp (built from the
// request) off the reading goroutine, and stops at the resolved line or at the
// refusal warning.
func answerOnce(t *testing.T, agent string, build func(Request) Response) (Request, []Msg) {
	t.Helper()
	var got Request
	msgs, _ := driveAdapter(t, agent, ClaudeOpts{}, func(m Msg, in *Writer) bool {
		switch {
		case m.Kind == KindRequest:
			got = *m.Request
			r := build(got)
			go func() { _ = in.Response(r) }()
		case m.Kind == KindResolved:
			return true
		case m.Kind == KindEvent && m.Event.Kind == EventError && m.Event.Warning:
			return true
		}
		return false
	})
	return got, msgs
}

func TestAskUserQuestionBecomesANeutralQuestion(t *testing.T) {
	agent, _ := askAgent(t, "AskUserQuestion", askInput, "")
	req, _ := answerOnce(t, agent, func(r Request) Response { return Response{ID: r.ID, Behavior: BehaviorDeny} })
	if len(req.Questions) != 2 || req.Questions[0].Header != "Format" || !req.Questions[1].MultiSelect ||
		req.Questions[0].Options[1].Label != "Detailed" || req.Questions[0].Options[0].Description != "brief" {
		t.Fatalf("questions not carried: %+v", req.Questions)
	}
}

func TestAnAnswerBecomesTheDocumentedUpdatedInput(t *testing.T) {
	agent, inFile := askAgent(t, "AskUserQuestion", askInput, "")
	answerOnce(t, agent, func(r Request) Response {
		// by header, by text, a multi-select with free text, and a reply
		return AnswerResponse(r.ID, map[string][]string{
			"Format":          {"Summary"},
			"Which sections?": {"Intro", "my own section"},
		}, "and keep it short")
	})
	resp, ok := vendorResponse(t, inFile)
	if !ok {
		t.Fatal("no control_response reached the agent")
	}
	if resp["behavior"] != "allow" {
		t.Fatalf("behavior %v", resp["behavior"])
	}
	in := resp["updatedInput"].(map[string]any)
	if qs, _ := in["questions"].([]any); len(qs) != 2 {
		t.Errorf("the original questions did not ride along: %v", in["questions"])
	}
	ans := in["answers"].(map[string]any)
	if ans["How should I format the output?"] != "Summary" {
		t.Errorf("single-select answer %v, want the label keyed by question TEXT", ans["How should I format the output?"])
	}
	multi, _ := ans["Which sections?"].([]any)
	if len(multi) != 2 || multi[0] != "Intro" || multi[1] != "my own section" {
		t.Errorf("multi-select answer %v", ans["Which sections?"])
	}
	if in["response"] != "and keep it short" {
		t.Errorf("reply %v", in["response"])
	}
}

func TestAnAnswerNamingNoQuestionIsRefusedAndStaysPending(t *testing.T) {
	agent, inFile := askAgent(t, "AskUserQuestion", askInput, "")
	_, msgs := answerOnce(t, agent, func(r Request) Response {
		return AnswerResponse(r.ID, map[string][]string{"Colour": {"red"}}, "")
	})
	if _, ok := vendorResponse(t, inFile); ok {
		t.Fatal("a refused answer reached the agent")
	}
	var refused bool
	for _, m := range msgs {
		if m.Kind == KindResolved {
			t.Fatal("a refused answer resolved the request")
		}
		if m.Kind == KindEvent && strings.Contains(m.Event.Text, "names no question") {
			refused = true
		}
	}
	if !refused {
		t.Fatalf("no refusal reported; kinds %v", kinds(msgs))
	}
}

func TestAnswersOnAToolApprovalAreRefused(t *testing.T) {
	agent, inFile := askAgent(t, "Write", `{"file_path":"/x"}`, "")
	_, msgs := answerOnce(t, agent, func(r Request) Response {
		return AnswerResponse(r.ID, nil, "sure")
	})
	if _, ok := vendorResponse(t, inFile); ok {
		t.Fatal("answers on an approval reached the agent")
	}
	var refused bool
	for _, m := range msgs {
		if m.Kind == KindEvent && strings.Contains(m.Event.Text, "asks no question") {
			refused = true
		}
	}
	if !refused {
		t.Fatal("no refusal reported")
	}
}

// A shape the adapter cannot read stays an ordinary approval, as before.
func TestAMalformedQuestionFallsBackToAnApproval(t *testing.T) {
	agent, _ := askAgent(t, "AskUserQuestion", `{"questions":"not a list"}`, "")
	req, _ := answerOnce(t, agent, func(r Request) Response { return Response{ID: r.ID, Behavior: BehaviorDeny} })
	if len(req.Questions) != 0 || req.Tool != "AskUserQuestion" {
		t.Fatalf("got %+v", req)
	}
}

// The placeholder this replaced echoed {} on every plain allow.
func TestAPlainAllowEchoesTheOriginalInput(t *testing.T) {
	agent, inFile := askAgent(t, "Write", `{"file_path":"/x","content":"hi"}`, "")
	answerOnce(t, agent, func(r Request) Response { return Response{ID: r.ID, Behavior: BehaviorAllow} })
	resp, _ := vendorResponse(t, inFile)
	in, _ := resp["updatedInput"].(map[string]any)
	if in["file_path"] != "/x" || in["content"] != "hi" {
		t.Fatalf("updatedInput %v, want the request's own input", resp["updatedInput"])
	}
}

// Accepting a suggestion echoes it back verbatim in updatedPermissions; it was
// accepted on the wire and dropped here before.
func TestAnAcceptedSuggestionIsEchoedAsUpdatedPermissions(t *testing.T) {
	sugg := `,"permission_suggestions":[{"type":"setMode","mode":"acceptEdits","destination":"session"}]`
	agent, inFile := askAgent(t, "Write", `{"file_path":"/x"}`, sugg)
	answerOnce(t, agent, func(r Request) Response {
		n := 0
		return Response{ID: r.ID, Behavior: BehaviorAllow, AcceptSuggestion: &n}
	})
	resp, _ := vendorResponse(t, inFile)
	perms, _ := resp["updatedPermissions"].([]any)
	if len(perms) != 1 {
		t.Fatalf("updatedPermissions %v", resp["updatedPermissions"])
	}
	p := perms[0].(map[string]any)
	if p["type"] != "setMode" || p["mode"] != "acceptEdits" || p["destination"] != "session" {
		t.Fatalf("suggestion not echoed verbatim: %v", p)
	}
}

func TestQuestionHelpers(t *testing.T) {
	r := Request{ID: "req-1", Questions: []Question{
		{Question: "A?", Header: "A", Options: []Option{{Label: "x"}}},
		{Question: "B?", Options: []Option{{Label: "y"}}},
	}}
	if got := QuestionSummary(r); got != "❓ question: A: A? (+1 more) (req-1)" {
		t.Errorf("summary %q", got)
	}
	if QuestionComplete(r, map[string][]string{"A?": {"x"}}, "") {
		t.Error("complete with one of two answered")
	}
	if !QuestionComplete(r, map[string][]string{"A?": {"x"}, "B?": {"y"}}, "") {
		t.Error("not complete with both answered")
	}
	if !QuestionComplete(r, nil, "a reply") {
		t.Error("a reply does not stand in for the answers")
	}
	if m, ok := RenderText(Msg{Kind: KindRequest, Request: &r}); !ok || !strings.HasPrefix(m, "❓ question: ") {
		t.Errorf("task log line %q", m)
	}
}
